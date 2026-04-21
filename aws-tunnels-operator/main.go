package main

import (
	"context"
	"flag"
	"net/http"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"

	proxyv1alpha1 "homelab/aws-tunnels-operator/api/v1alpha1"
	"homelab/aws-tunnels-operator/controllers"
)

// httpRunnable wraps an http.Server so it satisfies manager.Runnable.
type httpRunnable struct {
	addr    string
	handler http.Handler
}

func (h *httpRunnable) Start(ctx context.Context) error {
	srv := &http.Server{Addr: h.addr, Handler: h.handler}
	go func() {
		<-ctx.Done()
		_ = srv.Shutdown(context.Background())
	}()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func main() {
	var metricsAddr string
	var probeAddr string

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to")
	flag.Parse()

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(proxyv1alpha1.AddToScheme(scheme))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                server.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
	})
	if err != nil {
		os.Exit(1)
	}

	if err := (&controllers.AWSTunnelStackReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		os.Exit(1)
	}

	authMux := http.NewServeMux()
	authSrv := &controllers.AuthServer{Client: mgr.GetClient()}
	authSrv.Register(authMux)
	if err := mgr.Add(&httpRunnable{addr: ":8090", handler: authMux}); err != nil {
		os.Exit(1)
	}

	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		os.Exit(1)
	}
}
