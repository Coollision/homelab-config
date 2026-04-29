package main

import (
	"context"
	"embed"
	"flag"
	"net/http"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"homelab/aws-tunnels-operator/controllers"
)

//go:embed templates
var templateDir embed.FS

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

	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to")
	flag.Parse()

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                server.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         true,
		LeaderElectionID:       "aws-tunnels-operator-leader",
	})
	if err != nil {
		os.Exit(1)
	}

	stackNamespace := os.Getenv("POD_NAMESPACE")
	if stackNamespace == "" {
		stackNamespace = "default"
	}
	stackConfigName := os.Getenv("STACK_CONFIGMAP_NAME")
	if stackConfigName == "" {
		stackConfigName = "aws-tunnels-operator-stack"
	}
	argoAppName := os.Getenv("ARGOCD_APP_NAME")

	if err := mgr.Add(&controllers.SingleStackRunner{
		Client:        mgr.GetClient(),
		Namespace:     stackNamespace,
		ConfigMapName: stackConfigName,
		ArgoAppName:   argoAppName,
	}); err != nil {
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		os.Exit(1)
	}

	authMux := http.NewServeMux()
	authSrv := &controllers.AuthServer{Client: mgr.GetClient(), TemplateFS: templateDir, StackNamespace: stackNamespace, StackConfigName: stackConfigName}
	authSrv.Register(authMux)
	if err := mgr.Add(&httpRunnable{addr: ":8090", handler: authMux}); err != nil {
		os.Exit(1)
	}

	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		os.Exit(1)
	}
}
