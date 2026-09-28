// Package metrics registers the operator's Prometheus series on
// controller-runtime's registry, served at the manager's /metrics endpoint.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Label "guard" is namespace/name: one series per ServiceGuard, so cardinality
// is bounded by the number of guards, not by endpoints or errors.
var (
	ProbeDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "serviceguard_probe_duration_seconds",
		Help:    "Latency of health probes, including failed ones.",
		Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5},
	}, []string{"guard"})

	ProbeFailures = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "serviceguard_probe_failures_total",
		Help: "Probes that returned an error, a bad status or timed out.",
	}, []string{"guard"})

	Healthy = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "serviceguard_healthy",
		Help: "1 if the last probe succeeded, else 0.",
	}, []string{"guard"})

	Remediations = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "serviceguard_remediations_total",
		Help: "Actions taken, by action (Restart, Failover).",
	}, []string{"guard", "action"})

	// Suppressed counts the times the threshold was met but a safety rule
	// (cooldown, budget, already failed over) held the operator back. A
	// rising value here with flat Remediations is the rate limiter working.
	Suppressed = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "serviceguard_remediations_suppressed_total",
		Help: "Actions withheld by cooldown, restart budget or prior failover.",
	}, []string{"guard", "reason"})

	CertExpiry = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "serviceguard_cert_expiry_timestamp_seconds",
		Help: "NotAfter of the certificate served by an https endpoint (unix seconds).",
	}, []string{"guard"})
)

func init() {
	ctrlmetrics.Registry.MustRegister(ProbeDuration, ProbeFailures, Healthy, Remediations, Suppressed, CertExpiry)
}

// Forget drops a deleted guard's series so they don't linger as stale values.
func Forget(guard string) {
	l := prometheus.Labels{"guard": guard}
	ProbeDuration.DeletePartialMatch(l)
	ProbeFailures.DeletePartialMatch(l)
	Healthy.DeletePartialMatch(l)
	Remediations.DeletePartialMatch(l)
	Suppressed.DeletePartialMatch(l)
	CertExpiry.DeletePartialMatch(l)
}
