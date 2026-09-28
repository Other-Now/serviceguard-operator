// Command manager runs the ServiceGuard operator.
package main

import (
	"flag"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	guardv1 "github.com/Other-Now/serviceguard-operator/api/v1alpha1"
	"github.com/Other-Now/serviceguard-operator/internal/controller"
	"github.com/Other-Now/serviceguard-operator/internal/probe"
)

func main() {
	var metricsAddr, probeAddr string
	var leaderElect, insecureTLS bool
	var workers int
	flag.IntVar(&workers, "max-concurrent-reconciles", 8, "Guards probed in parallel.")
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Prometheus /metrics address.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Liveness/readiness address.")
	flag.BoolVar(&leaderElect, "leader-elect", false, "Enable leader election so only one replica acts.")
	flag.BoolVar(&insecureTLS, "probe-insecure-tls", true, "Skip certificate verification when probing (needed to read expiry of self-signed certs).")
	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	setupLog := ctrl.Log.WithName("setup")

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = guardv1.AddToScheme(scheme)

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         leaderElect,
		LeaderElectionID:       "serviceguard.guard.other-now.dev",
		// Step down immediately on SIGTERM so a rolling update of the operator
		// hands over in seconds instead of waiting out the lease.
		LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "unable to create manager")
		os.Exit(1)
	}

	if err := (&controller.ServiceGuardReconciler{
		Client:   mgr.GetClient(),
		Recorder: mgr.GetEventRecorderFor("serviceguard"), //nolint:staticcheck // core/v1 events; fake recorder in tests
		Prober:   probe.NewHTTPProber(insecureTLS),

		MaxConcurrentReconciles: workers,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller")
		os.Exit(1)
	}

	_ = mgr.AddHealthzCheck("healthz", healthz.Ping)
	_ = mgr.AddReadyzCheck("readyz", healthz.Ping)

	setupLog.Info("starting manager", "leaderElect", leaderElect)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "manager exited")
		os.Exit(1)
	}
}
