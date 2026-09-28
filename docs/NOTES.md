# Interview notes: serviceguard-operator

These are short notes for a 1–2 day read. Each point names where it lives in the code.

## 1. What an operator is, in one breath
A controller is a loop that reads **desired state** (spec), looks at **actual state**, and acts to close the gap. It writes what it saw to **status**. An operator is a controller for your own resource type (the CRD).

Reconcile must be **level-triggered and idempotent**. It gets a key (`namespace/name`), not the event that caused it, and has to work out what to do from the current state alone. That is why the restart budget sits in `.status` and not in a Go variable (`serviceguard_controller.go`).

## 2. The reconcile loop here
`Reconcile` in `internal/controller/serviceguard_controller.go`:
1. Get the ServiceGuard. If it's NotFound, forget its metrics and return.
2. If it was probed less than one interval ago, return `RequeueAfter` for the remaining time.
3. Probe (`internal/probe`), update the failure counter and the `Healthy` condition, and check the certificate.
4. `policy.Decide(...)`, a pure function.
5. No action: write status and requeue after one interval.
6. Action: **write status first**, then patch the Deployment or Service, emit an event and a metric, and requeue.

Requeue: `ctrl.Result{RequeueAfter: interval}` is the timer. Returning an error gives exponential backoff instead, which is used only for a failed status write.

## 3. Safety rules (the part to talk about)
| Failure mode | Guard |
|---|---|
| One bad probe triggers a restart | `failureThreshold` of consecutive failures |
| Restart storm (restarting every interval while the rollout is still running) | `cooldownSeconds` after every action |
| A restart that never fixes it, looping for ever | `maxRestarts` per sliding `windowSeconds`, then escalate |
| Failover/failback oscillation | Failover is one-way; the original selector is saved in an annotation |
| Two operator replicas both acting | Leader election (a Lease object); only the leader runs reconciles |
| New leader forgets history and restarts again | Budget and last action stored in `.status` |
| Crash between acting and recording | Record first, then act; the worst case is a missed action, never a doubled one |
| Taking down all replicas | Rollout restart through the Deployment controller, which honours `maxUnavailable` and readiness |
| Blast radius | RBAC can only `patch` Deployments and Services |

Proof: `policy_test.go` has `TestFlappingNeverRemediates`, `TestHardDownIsBounded` and `TestHardDownNoFailoverRespectsWindow`. The e2e scenarios 2 and 3 run the same checks on a real cluster.

## 4. Things that commonly go wrong with controllers (and the fix used)
- **Status-update hot loop.** Writing status changes `resourceVersion` and fires a watch event. `GenerationChangedPredicate` filters it out, because `metadata.generation` only changes when the spec changes.
- **Conflicts.** `Status().Update` sends the resourceVersion I read. If someone else wrote in between, I get a 409 and return the error. The requeue re-reads and decides again, which is optimistic concurrency.
- **Slow probes block other guards.** Reconcile is synchronous, so `MaxConcurrentReconciles` is set to 8. The workqueue guarantees the same key is never processed by two workers at once.
- **Timestamps.** `metav1.Time` serialises to whole seconds. The stored `lastProbeTime` is rounded down, so the "probed too recently" check can only err toward probing, never toward skipping.

## 5. Leader election
`LeaderElection: true` in `cmd/manager/main.go`. Replicas compete for a `coordination.k8s.io/Lease`. The holder renews it, and the others take over if it isn't renewed within the lease duration (15 s by default).

`LeaderElectionReleaseOnCancel` makes a pod that gets SIGTERM give up the lease immediately, so a rolling update of the operator hands over in about a second instead of 15. E2e scenario 4 measures this.

## 6. Why Terraform + Helm
- **Terraform** owns *infrastructure*: the cluster, loading the image, installing the release. It is declarative and has a plan/apply/destroy lifecycle and state.
- **Helm** packages the *application*: CRD, RBAC and Deployment, with values.
- The Terraform `helm` provider joins the two, so `terraform apply` goes from nothing to a running operator, and CI uses exactly that path.

The provider reads the cluster's endpoint and certificates from the `kind_cluster` resource, so ordering follows from those references plus an explicit `depends_on` for the image load.

## 7. Questions I should expect
- *Why not a liveness probe?* Kubelet liveness restarts one container based on that pod's own view. This checks the **service** as a client sees it, through the Service. It escalates across Deployments, rate-limits cluster-wide actions, and reports through events and metrics.
- *What if the operator itself is down?* Nothing is remediated, but nothing breaks either: it's a fail-static design. Two replicas with leader election cover a single pod loss.
- *Scaling to 10k guards?* The per-guard cost is one timer-driven reconcile per interval. The limits are:
  - worker count against probe latency;
  - the status write rate against the API server (10k guards at 10 s is 1k writes per second, too many). I would write status only on change, or batch it.
  - The cache also holds every Deployment and Service.
- *How do you test a controller?* Pure policy with table tests, the reconciler against the fake client with an injected clock, and the real thing on kind in CI.
