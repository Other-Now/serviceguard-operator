# serviceguard-operator

A small **Kubernetes operator in Go** that watches a service's health endpoint and heals it, with the **remediation safety rules** as the main feature.

It is deployed with **Terraform** (kind cluster, image load, and a Helm release), and CI runs chaos tests against a real cluster.

```
ServiceGuard (CRD) ──► reconcile every intervalSeconds
                          │
                          ├─ probe: HTTP GET, timeout = failure, TLS cert expiry read
                          ├─ count consecutive failures            (flap filter)
                          ├─ policy.Decide(state, config, now)     (pure function)
                          │     healthy / below threshold ........ nothing
                          │     already failed over ............... nothing (a human fails back)
                          │     inside cooldown ................... nothing (let the rollout finish)
                          │     restart budget left in window ..... rollout restart
                          │     budget spent ...................... fail over Service to standby, or stop and report
                          ├─ write status FIRST, then act          (errs toward doing less)
                          └─ events + conditions + Prometheus metrics
```

## Example

```yaml
apiVersion: guard.other-now.dev/v1alpha1
kind: ServiceGuard
metadata: {name: web, namespace: demo}
spec:
  deployment: web-primary                 # what gets rollout-restarted
  endpoint: http://web.demo.svc:8080/healthz
  intervalSeconds: 2
  timeoutSeconds: 1
  failureThreshold: 3                     # consecutive failures before any action
  minCertValidityHours: 72                # https only: warn, never remediate
  remediation:
    cooldownSeconds: 15
    maxRestarts: 2                        # per sliding window
    windowSeconds: 600
    failover: {service: web, selector: {app: web, track: standby}}
```

```
$ kubectl get sg -n demo
NAME   HEALTHY   FAILURES   REMEDIATIONS   FAILEDOVER   AGE
web    True      0          3              true         4m
```

## Results

All results come from the CI run on a GitHub-hosted runner: kind, 1 node, operator ×2 with leader election. The full table is in the job summary and in the `e2e-results` artifact.

RESULTS_PLACEHOLDER

## Design decisions

| Decision | Why |
|---|---|
| **Restart = pod-template annotation** (`kubectl rollout restart`), not deleting pods | The Deployment controller then does a normal rolling update that respects `maxUnavailable` and readiness. Deleting pods can take every replica down at once. |
| **Failure threshold** | A single failed probe is noise. With threshold 3 and interval 2 s, a service has to be down for 4–6 s continuously before anything happens. |
| **Cooldown + sliding-window budget** | Stops a restart storm. If a restart doesn't fix the problem, restarting again every interval makes it worse. After `maxRestarts` the operator escalates to failover or stops. |
| **Failover is one-way** | Automatic failback on a flapping primary is the classic oscillation bug. The original selector is saved in an annotation so a human can fail back. |
| **Record, then act** | Status (last action, budget) is written before the Deployment or Service is patched. If the write fails, nothing happens and the next reconcile decides again. The opposite order could act, lose the record, and act again without cooldown. |
| **Budget lives in `.status`, not memory** | After a leader change, the new leader inherits the restart history instead of starting with a fresh budget. |
| **`GenerationChangedPredicate`** | The operator writes status on every probe. Without the predicate, each write would trigger an immediate re-reconcile, which becomes a hot loop. |
| **Probe at most once per interval** | Extra reconciles (spec edits, retries) would otherwise probe early and reach the threshold faster than configured. |
| **Cert expiry never remediates** | Restarting doesn't renew a certificate. Expiry raises an event, a condition and a `…cert_expiry_timestamp_seconds` metric. |
| **`MaxConcurrentReconciles` = 8** | The probe runs inside Reconcile. With controller-runtime's default of 1 worker, one endpoint that times out delays every other guard. The workqueue still never runs the same guard twice at once. |
| **Least-privilege RBAC** | The operator can only `patch` Deployments and Services. It cannot create or delete workloads. |

## Metrics

| Series | Meaning |
|---|---|
| `serviceguard_probe_duration_seconds` (histogram) | probe latency, including failures |
| `serviceguard_probe_failures_total` | failed probes |
| `serviceguard_healthy` | 1 or 0, from the last probe |
| `serviceguard_remediations_total{action}` | `Restart` / `Failover` |
| `serviceguard_remediations_suppressed_total{reason}` | threshold met, but cooldown, budget or an earlier failover held the operator back |
| `serviceguard_cert_expiry_timestamp_seconds` | NotAfter of the served certificate |

Every series has one `guard` label (`namespace/name`), so cardinality is bounded by the number of guards. A deleted guard's series are removed.

## Tests

- `internal/policy`: table tests for every rule, plus simulations over one hour or more. They check that a flapping endpoint never triggers an action, that a hard-down endpoint gets exactly `maxRestarts` restarts then one failover, and that no sliding window ever contains more than `maxRestarts`.
- `internal/probe`: status codes, timeout, connection refused, and a TLS server with a generated certificate to check the expiry read.
- `internal/controller`: the real reconciler against controller-runtime's fake client, with a scripted prober and an injected clock:
  - restart after the threshold
  - flapping produces no action
  - escalation to failover (the selector is replaced and the original saved)
  - probing is rate-limited
  - a missing Deployment is reported
  - the cert-expiry event fires once
- `hack/e2e.sh` (CI, kind): real chaos. It breaks the pod, flaps it, makes the primary unfixable, kills the leader, and runs 200 guards.

## Run it

```bash
docker build -t serviceguard:dev .
cd terraform && terraform init && terraform apply     # kind + image load + helm install
kind export kubeconfig --name serviceguard
RUNS=10 bash hack/e2e.sh                              # needs bash, kubectl, jq
terraform destroy
```

Unit tests only: `go test ./...`

## Layout

```
api/v1alpha1/         CRD types (+ generated deepcopy)
internal/policy/      the decision function — all safety rules live here
internal/probe/       HTTP/TLS probe
internal/controller/  reconcile loop
internal/metrics/     Prometheus series
cmd/manager/          operator main (leader election, metrics, health)
cmd/demoapp/          chaos target: /healthz, POST /break, POST /heal, FORCE_FAIL=1
charts/               Helm chart (CRD, RBAC, Deployment)
terraform/            kind cluster + Helm release
deploy/demo.yaml      primary/standby app, Service, ServiceGuard, curl pod
hack/e2e.sh           chaos suite -> e2e-results.md
docs/NOTES.md         interview notes
```

## Not done (on purpose)

- **Webhooks:** there is no admission webhook. Defaults and ranges are handled by CRD OpenAPI validation.
- **Informer cache scope:** the cache watches all Deployments and Services cluster-wide. On a large cluster I would scope it by namespace or label, or read uncached.
- **Latency SLO:** probe latency is only a failure when it exceeds the timeout. A separate p95 threshold, with scale-up as the remediation, is the obvious next step.
