#!/usr/bin/env bash
# Chaos / e2e suite against a kind cluster that already runs the operator
# (terraform apply). Writes Markdown results to $OUT (default e2e-results.md).
#
#   1. restart:  break the primary N times -> time to detect, time to recover
#   2. flap:     toggle broken/healthy faster than the threshold -> expect 0 actions
#   3. budget:   make the primary unfixable -> expect exactly MaxRestarts restarts, then failover
#   4. leader:   delete the leader pod -> time until the lease has a new holder
#   5. scale:    200 guards on one controller -> probes keep up, operator memory
set -euo pipefail

RUNS=${RUNS:-10}
OUT=${OUT:-e2e-results.md}
NS=demo
OPNS=serviceguard-system
LEASE=serviceguard.guard.other-now.dev
HERE=$(cd "$(dirname "$0")/.." && pwd)

ms()    { date +%s%3N; }
k()     { kubectl "$@"; }
curlc() { k -n $NS exec chaos -- curl -s -m 1 "$@"; }
total() { local v; v=$(k -n $NS get sg "${1:-web}" -o jsonpath='{.status.totalRemediations}'); echo "${v:-0}"; }
healthy_body() { curlc http://web.$NS.svc:8080/healthz 2>/dev/null || true; }

# pct <p> <values...>: nearest-rank percentile
pct() { local p=$1; shift; printf '%s\n' "$@" | sort -n | awk -v p="$p" '{a[NR]=$1} END{i=int((p/100)*NR+0.999); if(i<1)i=1; print a[i]}'; }

wait_ok() { # until 3 consecutive 200s from the service
  local want=${1:-ok} n=0 deadline=$(( $(date +%s) + 120 ))
  while (( n < 3 )); do
    if [[ $(healthy_body) == "$want"* ]]; then n=$((n+1)); else n=0; fi
    (( $(date +%s) < deadline )) || { echo "timeout waiting for '$want'"; return 1; }
    sleep 0.3
  done
}

echo "== setup"
k apply -f "$HERE/deploy/demo.yaml"
k -n $NS rollout status deploy/web-primary --timeout=120s
k -n $NS rollout status deploy/web-standby --timeout=120s
k -n $NS wait --for=condition=Ready pod/chaos --timeout=120s
wait_ok "ok primary"

{
  echo "# e2e results"
  echo
  echo "kind, 1 node, operator replicas=2 with leader election. Guard: interval 2s, timeout 1s, threshold 3, cooldown 15s."
  echo "Times are wall-clock from the test script, polled every ~0.3s through a curl pod, so they include kubectl exec overhead."
  echo
} > "$OUT"

# ---------------------------------------------------------------- 1. restart
echo "== restart x$RUNS"
ttd=(); ttr=()
for i in $(seq 1 "$RUNS"); do
  before=$(total)
  t0=$(ms)
  curlc -X POST http://web.$NS.svc:8080/break >/dev/null
  until [[ $(total) -gt $before ]]; do sleep 0.2; done
  t1=$(ms)
  wait_ok "ok primary"
  t2=$(ms)
  ttd+=($((t1 - t0))); ttr+=($((t2 - t0)))
  echo "run $i: detect $((t1 - t0)) ms, recover $((t2 - t0)) ms"
  k -n $NS rollout status deploy/web-primary --timeout=120s >/dev/null
  sleep 16 # past the cooldown so the next run is independent
done
{
  echo "## 1. Break primary, operator rollout-restarts it ($RUNS runs)"
  echo
  echo "| | p50 | p95 | max |"
  echo "|---|---|---|---|"
  echo "| time to detect (break -> restart issued) | $(pct 50 "${ttd[@]}") ms | $(pct 95 "${ttd[@]}") ms | $(pct 100 "${ttd[@]}") ms |"
  echo "| time to recover (break -> 3 healthy probes via Service) | $(pct 50 "${ttr[@]}") ms | $(pct 95 "${ttr[@]}") ms | $(pct 100 "${ttr[@]}") ms |"
  echo
  echo "Expected detect time is between (threshold-1) x interval = 4 s and threshold x interval = 6 s, plus reconcile latency."
  echo
} >> "$OUT"

# ---------------------------------------------------------------- 2. flap
echo "== flap"
sleep 16
before=$(total)
end=$(( $(date +%s) + 90 ))
flips=0
while (( $(date +%s) < end )); do
  curlc -X POST http://web.$NS.svc:8080/break >/dev/null; sleep 3
  curlc -X POST http://web.$NS.svc:8080/heal  >/dev/null; sleep 3
  flips=$((flips + 1))
done
after=$(total)
{
  echo "## 2. Flapping endpoint (3 s broken / 3 s healthy for 90 s, $flips cycles)"
  echo
  echo "Remediations triggered: **$((after - before))** (expected 0: 3 s broken is at most 2 failed 2 s probes, threshold is 3)."
  echo
} >> "$OUT"
curlc -X POST http://web.$NS.svc:8080/heal >/dev/null

# ---------------------------------------------------------------- 3. budget
echo "== budget -> failover"
# Fresh guard so restarts from scenario 1 don't count against the window.
k -n $NS delete sg web --wait
k apply -f "$HERE/deploy/demo.yaml"
k -n $NS patch sg web --type=merge -p '{"spec":{"remediation":{"maxRestarts":2,"cooldownSeconds":10}}}'
sleep 5
t0=$(ms)
k -n $NS set env deploy/web-primary FORCE_FAIL=1 >/dev/null
wait_ok "ok standby"
t1=$(ms)
fo=$(k -n $NS get sg web -o jsonpath='{.status.failedOver}')
rem=$(total)
sleep 30 # keep failing: nothing more should happen
rem_after=$(total)
{
  echo "## 3. Unfixable primary: bounded restarts, then failover"
  echo
  echo "| | value |"
  echo "|---|---|"
  echo "| remediations until failover (2 restarts + 1 failover expected) | $rem |"
  echo "| failedOver | $fo |"
  echo "| break -> Service serving standby | $((t1 - t0)) ms |"
  echo "| extra actions in the 30 s after failover | $((rem_after - rem)) |"
  echo
  echo '```'
  k -n $NS get events --field-selector involvedObject.kind=ServiceGuard --sort-by=.lastTimestamp -o custom-columns=REASON:.reason,MESSAGE:.message | tail -n 8
  echo '```'
  echo
} >> "$OUT"

# ---------------------------------------------------------------- 4. leader
echo "== leader handover"
leader=$(k -n $OPNS get lease $LEASE -o jsonpath='{.spec.holderIdentity}')
pod=${leader%%_*}
t0=$(ms)
k -n $OPNS delete pod "$pod" --wait=false >/dev/null
while true; do
  h=$(k -n $OPNS get lease $LEASE -o jsonpath='{.spec.holderIdentity}' 2>/dev/null || true)
  [[ -n $h && ${h%%_*} != "$pod" ]] && break
  sleep 0.2
done
t1=$(ms)
{
  echo "## 4. Leader election"
  echo
  echo "Deleted leader pod \`$pod\`; standby replica took the lease after **$((t1 - t0)) ms** (graceful release on SIGTERM)."
  echo
} >> "$OUT"
k -n $OPNS rollout status deploy/serviceguard --timeout=120s >/dev/null

# ---------------------------------------------------------------- 5. scale
echo "== scale"
# The metrics Service balances across both replicas; only the leader runs
# reconciles, so read RSS from the leader pod directly.
leader_ip() { local p; p=$(k -n $OPNS get lease $LEASE -o jsonpath='{.spec.holderIdentity}'); k -n $OPNS get pod "${p%%_*}" -o jsonpath='{.status.podIP}'; }
rss_leader() { curlc "http://$(leader_ip):8080/metrics" | awk '/^process_resident_memory_bytes/ {printf "%.1f", $2/1048576}'; }
m0=$(rss_leader)
N=200
for i in $(seq 1 $N); do
  cat <<EOF
apiVersion: guard.other-now.dev/v1alpha1
kind: ServiceGuard
metadata: {name: load-$i, namespace: $NS}
spec:
  deployment: web-standby
  endpoint: http://web-standby-probe.$NS.svc:8080/healthz
  intervalSeconds: 10
  timeoutSeconds: 1
  failureThreshold: 3
  remediation: {maxRestarts: 0}
---
EOF
done > /tmp/load.yaml
k apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Service
metadata: {name: web-standby-probe, namespace: $NS}
spec:
  selector: {app: web, track: standby}
  ports: [{port: 8080, targetPort: 8080}]
EOF
k apply -f /tmp/load.yaml >/dev/null
sleep 60
m1=$(rss_leader)
now=$(date +%s)
fresh=$(k -n $NS get sg -o json | jq --argjson now "$now" '[.items[] | select(.metadata.name|startswith("load-")) | select(.status.lastProbeTime != null) | select(($now - (.status.lastProbeTime|fromdateiso8601)) <= 20)] | length')
p95_ms=$(k -n $NS get sg -o json | jq '[.items[] | select(.metadata.name|startswith("load-")) | .status.lastProbeLatencyMs // 0] | sort | .[(length*0.95|floor)]')
{
  echo "## 5. Scale: $N guards on one controller (interval 10 s)"
  echo
  echo "| | value |"
  echo "|---|---|"
  echo "| guards probed within the last 2 intervals, 60 s after creation | $fresh / $N |"
  echo "| p95 last probe latency across guards | ${p95_ms} ms |"
  echo "| leader RSS before | ${m0} MiB |"
  echo "| leader RSS with $N guards | ${m1} MiB |"
  echo
} >> "$OUT"
k -n $NS delete -f /tmp/load.yaml --wait=false >/dev/null

echo "== done"
cat "$OUT"
