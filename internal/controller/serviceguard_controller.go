// Package controller holds the ServiceGuard reconcile loop:
// probe -> count -> decide (policy) -> record -> act -> requeue.
package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	guardv1 "github.com/Other-Now/serviceguard-operator/api/v1alpha1"
	"github.com/Other-Now/serviceguard-operator/internal/metrics"
	"github.com/Other-Now/serviceguard-operator/internal/policy"
	"github.com/Other-Now/serviceguard-operator/internal/probe"
)

const (
	// Same mechanism as `kubectl rollout restart`: changing a pod-template
	// annotation makes the Deployment controller do a normal rolling update,
	// honouring maxUnavailable and readiness. Deleting pods directly would not.
	RestartAnnotation = "guard.other-now.dev/restartedAt"
	// Written on the Service at failover so a human can fail back.
	OriginalSelectorAnnotation = "guard.other-now.dev/original-selector"

	CondHealthy          = "Healthy"
	CondDegraded         = "Degraded"
	CondCertExpiringSoon = "CertExpiringSoon"
)

type ServiceGuardReconciler struct {
	client.Client
	Recorder record.EventRecorder
	Prober   probe.Prober
	Now      func() time.Time // injectable clock for tests
	// The probe runs inside Reconcile, so with the default of 1 worker one
	// slow endpoint (up to TimeoutSeconds) delays every other guard.
	MaxConcurrentReconciles int
}

// +kubebuilder:rbac:groups=guard.other-now.dev,resources=serviceguards,verbs=get;list;watch
// +kubebuilder:rbac:groups=guard.other-now.dev,resources=serviceguards/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;patch
// +kubebuilder:rbac:groups="",resources=services,verbs=get;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *ServiceGuardReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	key := req.String()

	var sg guardv1.ServiceGuard
	if err := r.Get(ctx, req.NamespacedName, &sg); err != nil {
		if apierrors.IsNotFound(err) {
			metrics.Forget(key)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	now := r.Now()
	interval := time.Duration(max(sg.Spec.IntervalSeconds, 1)) * time.Second

	// Probe at most once per interval. Extra reconciles (spec edits, error
	// retries, a resync) would otherwise probe early and count failures faster
	// than the threshold assumes.
	if lp := sg.Status.LastProbeTime; lp != nil && sg.Status.ObservedGeneration == sg.Generation {
		if wait := interval - now.Sub(lp.Time); wait > 0 {
			return ctrl.Result{RequeueAfter: wait}, nil
		}
	}

	timeout := time.Duration(max(sg.Spec.TimeoutSeconds, 1)) * time.Second
	res := r.Prober.Probe(ctx, sg.Spec.Endpoint, timeout)

	metrics.ProbeDuration.WithLabelValues(key).Observe(res.Latency.Seconds())
	st := &sg.Status
	st.LastProbeTime = &metav1.Time{Time: now}
	st.LastProbeLatencyMs = res.Latency.Milliseconds()
	st.ObservedGeneration = sg.Generation
	if res.Healthy {
		st.ConsecutiveFailures = 0
		metrics.Healthy.WithLabelValues(key).Set(1)
		setCond(st, CondHealthy, metav1.ConditionTrue, "ProbeSucceeded", fmt.Sprintf("HTTP %d in %dms", res.StatusCode, res.Latency.Milliseconds()), sg.Generation)
	} else {
		st.ConsecutiveFailures++
		metrics.Healthy.WithLabelValues(key).Set(0)
		metrics.ProbeFailures.WithLabelValues(key).Inc()
		setCond(st, CondHealthy, metav1.ConditionFalse, "ProbeFailed", errString(res.Err), sg.Generation)
	}

	r.checkCert(&sg, res, now)

	d := policy.Decide(res.Healthy, stateFrom(st), configFrom(&sg), now)
	st.RecentRestarts = toMeta(d.RecentRestarts)
	if d.Action == policy.None && !res.Healthy && st.ConsecutiveFailures >= sg.Spec.FailureThreshold {
		metrics.Suppressed.WithLabelValues(key, string(d.Reason)).Inc()
		setCond(st, CondDegraded, metav1.ConditionTrue, string(d.Reason), "threshold met but action withheld", sg.Generation)
		if d.Reason == policy.ReasonBudgetExhausted {
			r.Recorder.Event(&sg, corev1.EventTypeWarning, string(d.Reason), "restart budget spent and no failover configured; needs a human")
		}
	}
	if d.Action == policy.None {
		if res.Healthy {
			meta.RemoveStatusCondition(&st.Conditions, CondDegraded)
		}
		return ctrl.Result{RequeueAfter: interval}, r.Status().Update(ctx, &sg)
	}

	// Record the action BEFORE doing it. If the status write fails (conflict,
	// API blip) we never act, and the next reconcile decides again. The other
	// order could act, lose the record, and act a second time without
	// cooldown. Under partial failure this errs toward doing less.
	st.LastRemediationTime = &metav1.Time{Time: now}
	st.TotalRemediations++
	st.ConsecutiveFailures = 0
	if d.Action == policy.Failover {
		st.FailedOver = true
	}
	setCond(st, CondDegraded, metav1.ConditionTrue, string(d.Action), string(d.Reason), sg.Generation)
	if err := r.Status().Update(ctx, &sg); err != nil {
		return ctrl.Result{}, err
	}

	var actErr error
	switch d.Action {
	case policy.Restart:
		actErr = r.restart(ctx, &sg, now)
	case policy.Failover:
		actErr = r.failover(ctx, &sg)
	}
	if actErr != nil {
		logger.Error(actErr, "remediation failed", "action", d.Action)
		r.Recorder.Eventf(&sg, corev1.EventTypeWarning, "RemediationFailed", "%s failed: %v", d.Action, actErr)
		return ctrl.Result{RequeueAfter: interval}, nil
	}
	metrics.Remediations.WithLabelValues(key, string(d.Action)).Inc()
	logger.Info("remediated", "action", d.Action, "reason", d.Reason, "recentRestarts", len(d.RecentRestarts))
	return ctrl.Result{RequeueAfter: interval}, nil
}

func (r *ServiceGuardReconciler) restart(ctx context.Context, sg *guardv1.ServiceGuard, now time.Time) error {
	var dep appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{Namespace: sg.Namespace, Name: sg.Spec.Deployment}, &dep); err != nil {
		return fmt.Errorf("get deployment %s: %w", sg.Spec.Deployment, err)
	}
	patch := client.MergeFrom(dep.DeepCopy())
	if dep.Spec.Template.Annotations == nil {
		dep.Spec.Template.Annotations = map[string]string{}
	}
	dep.Spec.Template.Annotations[RestartAnnotation] = now.UTC().Format(time.RFC3339)
	if err := r.Patch(ctx, &dep, patch); err != nil {
		return err
	}
	r.Recorder.Eventf(sg, corev1.EventTypeWarning, "Restarted", "rollout restart of deployment/%s after %d failed probes", dep.Name, sg.Spec.FailureThreshold)
	return nil
}

func (r *ServiceGuardReconciler) failover(ctx context.Context, sg *guardv1.ServiceGuard) error {
	fo := sg.Spec.Remediation.Failover
	var svc corev1.Service
	if err := r.Get(ctx, types.NamespacedName{Namespace: sg.Namespace, Name: fo.Service}, &svc); err != nil {
		return fmt.Errorf("get service %s: %w", fo.Service, err)
	}
	orig, _ := json.Marshal(svc.Spec.Selector)
	patch := client.MergeFrom(svc.DeepCopy())
	if svc.Annotations == nil {
		svc.Annotations = map[string]string{}
	}
	svc.Annotations[OriginalSelectorAnnotation] = string(orig)
	// Replace, not merge: a merge would keep primary-only labels and match nothing.
	svc.Spec.Selector = fo.Selector
	if err := r.Patch(ctx, &svc, patch); err != nil {
		return err
	}
	r.Recorder.Eventf(sg, corev1.EventTypeWarning, "FailedOver", "service/%s selector %s -> %v; restart budget exhausted. Fail back manually.", svc.Name, orig, fo.Selector)
	return nil
}

func (r *ServiceGuardReconciler) checkCert(sg *guardv1.ServiceGuard, res probe.Result, now time.Time) {
	st := &sg.Status
	if res.CertNotAfter == nil {
		return
	}
	st.CertNotAfter = &metav1.Time{Time: *res.CertNotAfter}
	metrics.CertExpiry.WithLabelValues(sg.Namespace + "/" + sg.Name).Set(float64(res.CertNotAfter.Unix()))
	minValid := time.Duration(sg.Spec.MinCertValidityHours) * time.Hour
	if probe.CertExpiringSoon(res.CertNotAfter, now, minValid) {
		// Only emit the event on the transition, not on every probe.
		if !meta.IsStatusConditionTrue(st.Conditions, CondCertExpiringSoon) {
			r.Recorder.Eventf(sg, corev1.EventTypeWarning, CondCertExpiringSoon, "certificate expires %s (in %s)", res.CertNotAfter.UTC().Format(time.RFC3339), res.CertNotAfter.Sub(now).Round(time.Minute))
		}
		setCond(st, CondCertExpiringSoon, metav1.ConditionTrue, "BelowMinValidity", res.CertNotAfter.UTC().Format(time.RFC3339), sg.Generation)
	} else if minValid > 0 {
		setCond(st, CondCertExpiringSoon, metav1.ConditionFalse, "Valid", res.CertNotAfter.UTC().Format(time.RFC3339), sg.Generation)
	}
}

func (r *ServiceGuardReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Now == nil {
		r.Now = time.Now
	}
	return ctrl.NewControllerManagedBy(mgr).
		// Our own status writes bump resourceVersion but not generation; without
		// this predicate every status update would trigger an immediate reconcile.
		For(&guardv1.ServiceGuard{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		WithOptions(crcontroller.Options{MaxConcurrentReconciles: max(r.MaxConcurrentReconciles, 1)}).
		Named("serviceguard").
		Complete(r)
}

func stateFrom(st *guardv1.ServiceGuardStatus) policy.State {
	s := policy.State{ConsecutiveFailures: int(st.ConsecutiveFailures), FailedOver: st.FailedOver}
	if st.LastRemediationTime != nil {
		t := st.LastRemediationTime.Time
		s.LastRemediation = &t
	}
	for _, t := range st.RecentRestarts {
		s.RecentRestarts = append(s.RecentRestarts, t.Time)
	}
	return s
}

func configFrom(sg *guardv1.ServiceGuard) policy.Config {
	rm := sg.Spec.Remediation
	return policy.Config{
		FailureThreshold:   int(max(sg.Spec.FailureThreshold, 1)),
		Cooldown:           time.Duration(rm.CooldownSeconds) * time.Second,
		MaxRestarts:        int(rm.MaxRestarts),
		Window:             time.Duration(max(rm.WindowSeconds, 1)) * time.Second,
		FailoverConfigured: rm.Failover != nil,
	}
}

func toMeta(ts []time.Time) []metav1.Time {
	if len(ts) == 0 {
		return nil
	}
	out := make([]metav1.Time, len(ts))
	for i, t := range ts {
		out[i] = metav1.Time{Time: t}
	}
	return out
}

func setCond(st *guardv1.ServiceGuardStatus, typ string, s metav1.ConditionStatus, reason, msg string, gen int64) {
	meta.SetStatusCondition(&st.Conditions, metav1.Condition{Type: typ, Status: s, Reason: reason, Message: msg, ObservedGeneration: gen})
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
