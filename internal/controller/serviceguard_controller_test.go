package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	guardv1 "github.com/Other-Now/serviceguard-operator/api/v1alpha1"
	"github.com/Other-Now/serviceguard-operator/internal/probe"
)

// scripted returns the next result from a fixed sequence, then repeats the last.
type scripted struct {
	seq   []probe.Result
	calls int
}

func (s *scripted) Probe(context.Context, string, time.Duration) probe.Result {
	i := min(s.calls, len(s.seq)-1)
	s.calls++
	return s.seq[i]
}

var (
	up   = probe.Result{Healthy: true, StatusCode: 200, Latency: 3 * time.Millisecond}
	down = probe.Result{StatusCode: 503, Latency: 2 * time.Millisecond, Err: errors.New("status 503")}
)

type harness struct {
	t   *testing.T
	c   client.Client
	r   *ServiceGuardReconciler
	rec *record.FakeRecorder
	now time.Time
	key types.NamespacedName
}

func newHarness(t *testing.T, sg *guardv1.ServiceGuard, p probe.Prober, objs ...client.Object) *harness {
	t.Helper()
	sch := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	if err := guardv1.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(sch).
		WithObjects(append(objs, sg)...).
		WithStatusSubresource(&guardv1.ServiceGuard{}).
		Build()
	h := &harness{t: t, c: c, rec: record.NewFakeRecorder(100), now: time.Unix(1_700_000_000, 0), key: client.ObjectKeyFromObject(sg)}
	h.r = &ServiceGuardReconciler{Client: c, Recorder: h.rec, Prober: p, Now: func() time.Time { return h.now }}
	return h
}

// tick advances the clock by one interval and reconciles once.
func (h *harness) tick() ctrl.Result {
	h.t.Helper()
	res, err := h.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: h.key})
	if err != nil {
		h.t.Fatalf("reconcile: %v", err)
	}
	h.now = h.now.Add(10 * time.Second)
	return res
}

func (h *harness) guard() *guardv1.ServiceGuard {
	var sg guardv1.ServiceGuard
	if err := h.c.Get(context.Background(), h.key, &sg); err != nil {
		h.t.Fatal(err)
	}
	return &sg
}

func (h *harness) restartStamp() string {
	var d appsv1.Deployment
	if err := h.c.Get(context.Background(), types.NamespacedName{Namespace: "demo", Name: "web"}, &d); err != nil {
		h.t.Fatal(err)
	}
	return d.Spec.Template.Annotations[RestartAnnotation]
}

func (h *harness) events() []string {
	var out []string
	for {
		select {
		case e := <-h.rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

func guardFixture(failover bool) *guardv1.ServiceGuard {
	sg := &guardv1.ServiceGuard{
		ObjectMeta: metav1.ObjectMeta{Namespace: "demo", Name: "web", Generation: 1},
		Spec: guardv1.ServiceGuardSpec{
			Deployment: "web", Endpoint: "http://web.demo:8080/healthz",
			IntervalSeconds: 10, TimeoutSeconds: 2, FailureThreshold: 3,
			Remediation: guardv1.RemediationSpec{CooldownSeconds: 30, MaxRestarts: 2, WindowSeconds: 600},
		},
	}
	if failover {
		sg.Spec.Remediation.Failover = &guardv1.FailoverSpec{Service: "web", Selector: map[string]string{"app": "web", "track": "standby"}}
	}
	return sg
}

func deployment() *appsv1.Deployment {
	return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "demo", Name: "web"}}
}

func service() *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "demo", Name: "web"},
		Spec:       corev1.ServiceSpec{Selector: map[string]string{"app": "web", "track": "primary"}},
	}
}

func TestHealthyNeverTouchesDeployment(t *testing.T) {
	h := newHarness(t, guardFixture(false), &scripted{seq: []probe.Result{up}}, deployment())
	for range 20 {
		if res := h.tick(); res.RequeueAfter != 10*time.Second {
			t.Fatalf("requeue %v, want interval", res.RequeueAfter)
		}
	}
	sg := h.guard()
	if h.restartStamp() != "" || sg.Status.TotalRemediations != 0 {
		t.Fatal("healthy endpoint must not be restarted")
	}
	if !meta.IsStatusConditionTrue(sg.Status.Conditions, CondHealthy) {
		t.Fatal("Healthy condition should be True")
	}
}

func TestRestartAfterThreshold(t *testing.T) {
	h := newHarness(t, guardFixture(false), &scripted{seq: []probe.Result{down}}, deployment())
	h.tick()
	h.tick()
	if h.restartStamp() != "" {
		t.Fatal("restarted before threshold")
	}
	h.tick() // third consecutive failure
	if h.restartStamp() == "" {
		t.Fatal("expected rollout restart annotation after 3 failures")
	}
	sg := h.guard()
	if sg.Status.TotalRemediations != 1 || sg.Status.ConsecutiveFailures != 0 || len(sg.Status.RecentRestarts) != 1 {
		t.Fatalf("status after restart: %+v", sg.Status)
	}
	if ev := strings.Join(h.events(), "|"); !strings.Contains(ev, "Restarted") {
		t.Fatalf("expected Restarted event, got %q", ev)
	}
}

func TestFlappingEndpointNoAction(t *testing.T) {
	seq := []probe.Result{}
	for range 30 {
		seq = append(seq, down, down, up) // always one short of the threshold
	}
	h := newHarness(t, guardFixture(true), &scripted{seq: seq}, deployment(), service())
	for range len(seq) {
		h.tick()
	}
	if h.restartStamp() != "" || h.guard().Status.TotalRemediations != 0 {
		t.Fatal("flapping endpoint triggered a remediation")
	}
}

// Hard down: restart, restart, then budget spent -> failover, then nothing more.
func TestEscalatesToFailoverThenStops(t *testing.T) {
	h := newHarness(t, guardFixture(true), &scripted{seq: []probe.Result{down}}, deployment(), service())
	for range 60 { // 10 minutes of failed probes
		h.tick()
	}
	sg := h.guard()
	if !sg.Status.FailedOver || sg.Status.TotalRemediations != 3 {
		t.Fatalf("want 2 restarts + 1 failover, status %+v", sg.Status)
	}
	var svc corev1.Service
	if err := h.c.Get(context.Background(), types.NamespacedName{Namespace: "demo", Name: "web"}, &svc); err != nil {
		t.Fatal(err)
	}
	if svc.Spec.Selector["track"] != "standby" {
		t.Fatalf("selector not switched: %v", svc.Spec.Selector)
	}
	if svc.Annotations[OriginalSelectorAnnotation] != `{"app":"web","track":"primary"}` {
		t.Fatalf("original selector not recorded: %q", svc.Annotations[OriginalSelectorAnnotation])
	}
	if c := meta.FindStatusCondition(sg.Status.Conditions, CondDegraded); c == nil || c.Reason != "AlreadyFailedOver" {
		t.Fatalf("Degraded condition should say AlreadyFailedOver, got %+v", c)
	}
}

func TestProbesAtMostOncePerInterval(t *testing.T) {
	p := &scripted{seq: []probe.Result{up}}
	h := newHarness(t, guardFixture(false), p, deployment())
	h.tick()                            // probes, clock +10s
	h.now = h.now.Add(-7 * time.Second) // only 3s after the probe
	res, err := h.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: h.key})
	if err != nil {
		t.Fatal(err)
	}
	if p.calls != 1 {
		t.Fatalf("early reconcile probed again (%d calls)", p.calls)
	}
	if res.RequeueAfter != 7*time.Second {
		t.Fatalf("requeue %v, want the remaining 7s", res.RequeueAfter)
	}
}

func TestMissingDeploymentReportsNotPanics(t *testing.T) {
	h := newHarness(t, guardFixture(false), &scripted{seq: []probe.Result{down}}) // no Deployment object
	for range 3 {
		h.tick()
	}
	if ev := strings.Join(h.events(), "|"); !strings.Contains(ev, "RemediationFailed") {
		t.Fatalf("expected RemediationFailed event, got %q", ev)
	}
}

func TestCertExpiryWarnsOnceAndNeverRemediates(t *testing.T) {
	soon := time.Unix(1_700_000_000, 0).Add(24 * time.Hour)
	tlsUp := up
	tlsUp.CertNotAfter = &soon
	sg := guardFixture(false)
	sg.Spec.MinCertValidityHours = 72
	h := newHarness(t, sg, &scripted{seq: []probe.Result{tlsUp}}, deployment())
	for range 5 {
		h.tick()
	}
	got := h.guard()
	if !meta.IsStatusConditionTrue(got.Status.Conditions, CondCertExpiringSoon) {
		t.Fatal("CertExpiringSoon should be True")
	}
	n := strings.Count(strings.Join(h.events(), "|"), CondCertExpiringSoon)
	if n != 1 {
		t.Fatalf("want exactly one expiry event across 5 probes, got %d", n)
	}
	if h.restartStamp() != "" {
		t.Fatal("cert expiry must not restart anything")
	}
}
