// storage-operator hosts the homelab storage controllers on one controller-runtime manager:
//
//   - smb (--enable-smb, default on): Go port of the Python smb-operator.
//   - pv-reclaimer (--enable-pv-reclaimer, default off): Go port of the pv-reclaimer shell loop.
//
// All controllers share one leader-election lease, one metrics registry and one /healthz,
// /readyz endpoint (with a named readiness check per enabled controller).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"go.uber.org/zap/zapcore"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"homelab/storage-operator/controllers/pvreclaimer"
	"homelab/storage-operator/controllers/smb"
	"homelab/storage-operator/internal/config"
)

// LeaderElectionID is shared by every controller in this binary.
const LeaderElectionID = "storage-operator-leader"

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// logLevelFromEnv maps the Python LOG_LEVEL names onto zap levels.
func logLevelFromEnv() zapcore.Level {
	switch strings.ToUpper(os.Getenv("LOG_LEVEL")) {
	case "DEBUG":
		return zapcore.DebugLevel
	case "WARNING", "WARN":
		return zapcore.WarnLevel
	case "ERROR", "CRITICAL":
		return zapcore.ErrorLevel
	}
	return zapcore.InfoLevel
}

func main() {
	var (
		metricsAddr       string
		probeAddr         string
		leaderElect       bool
		enableSMB         bool
		enablePVReclaimer bool
		dryRun            bool
		ownerDeployment   string
		namespace         string
		configMapName     string
		pvrDryRun         bool
		pvrRetainOnly     bool
		pvrClasses        string
		pvrHonorSkip      bool
		pvrPatchMode      string
	)
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to")
	flag.BoolVar(&leaderElect, "leader-elect", true, "Enable leader election (lease "+LeaderElectionID+"); forced off with --dry-run")
	flag.BoolVar(&enableSMB, "enable-smb", true, "Run the SMB server controller")
	flag.BoolVar(&enablePVReclaimer, "enable-pv-reclaimer", false, "Run the PV reclaimer controller (clears spec.claimRef on Released PVs)")
	flag.BoolVar(&pvrDryRun, "pv-reclaimer-dry-run", false, "pv-reclaimer: send patches with dryRun=All and emit WouldReclaim events (leader election stays on)")
	flag.BoolVar(&pvrRetainOnly, "pv-reclaimer-retain-only", false, "pv-reclaimer: only reclaim PVs with reclaim policy Retain (default: any policy)")
	flag.StringVar(&pvrClasses, "pv-reclaimer-storage-classes", "", "pv-reclaimer: comma-separated StorageClass allowlist (default empty: every class)")
	flag.BoolVar(&pvrHonorSkip, "pv-reclaimer-honor-skip-annotation", false, "pv-reclaimer: skip PVs annotated "+pvreclaimer.SkipAnnotation+"=true")
	flag.StringVar(&pvrPatchMode, "pv-reclaimer-patch-mode", string(pvreclaimer.PatchModeClear), "pv-reclaimer: 'clear' removes the whole claimRef, 'keep-claim' drops only its uid/resourceVersion")
	flag.BoolVar(&dryRun, "dry-run", false, "Send all writes with dryRun=All and log what would change (pod-template diff)")
	flag.StringVar(&ownerDeployment, "owner-deployment", "smb-operator", "Name of the operator's own Deployment (child labels + smb-server Service owner)")
	flag.StringVar(&namespace, "namespace", envOr("OPERATOR_NAMESPACE", "storage"), "Namespace of the config ConfigMap (env OPERATOR_NAMESPACE)")
	flag.StringVar(&configMapName, "config-map", envOr("CONFIG_MAP_NAME", "smb-operator-config"), "Name of the config ConfigMap (env CONFIG_MAP_NAME)")
	zopts := zap.Options{Level: logLevelFromEnv()}
	zopts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zopts)))
	setupLog := ctrl.Log.WithName("setup")

	mode, err := pvreclaimer.ParsePatchMode(pvrPatchMode)
	if err != nil {
		setupLog.Error(err, "invalid flag")
		os.Exit(2)
	}

	if err := run(setupLog, runOpts{
		metricsAddr: metricsAddr, probeAddr: probeAddr, leaderElect: leaderElect && !dryRun,
		enableSMB: enableSMB, enablePVReclaimer: enablePVReclaimer, dryRun: dryRun,
		ownerDeployment: ownerDeployment, namespace: namespace, configMapName: configMapName,
		pvReclaimer: pvreclaimer.Options{
			DryRun: pvrDryRun, RetainOnly: pvrRetainOnly, StorageClasses: pvreclaimer.ParseStorageClasses(pvrClasses),
			HonorSkipAnnotation: pvrHonorSkip, Mode: mode,
		},
	}); err != nil {
		setupLog.Error(err, "fatal")
		os.Exit(1)
	}
}

type runOpts struct {
	metricsAddr, probeAddr                    string
	leaderElect, enableSMB, enablePVReclaimer bool
	dryRun                                    bool
	ownerDeployment, namespace, configMapName string
	pvReclaimer                               pvreclaimer.Options
}

func run(setupLog interface {
	Info(string, ...any)
	Error(error, string, ...any)
}, o runOpts) error {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	restCfg, err := ctrl.GetConfig()
	if err != nil {
		return err
	}

	var smbOpts smb.Options
	byObject := map[client.Object]cache.ByObject{}
	if o.enableSMB {
		c, err := client.New(restCfg, client.Options{Scheme: scheme})
		if err != nil {
			return err
		}
		cfg, err := loadConfig(c, o.namespace, o.configMapName)
		if err != nil {
			return err
		}
		probe := smb.Options{Config: cfg}
		gvk, err := c.RESTMapper().KindFor(smb.VolumeGVR(probe))
		if err != nil {
			return fmt.Errorf("resolving Longhorn volume kind: %w", err)
		}
		for _, k := range cfg.Deprecated {
			setupLog.Info("config key is accepted but ignored (deprecated)", "key", k)
		}
		if !cfg.UseWatchAPI {
			setupLog.Info("operator.useWatchAPI=false: watches stay on; the periodic resync every reconcileInterval gives the polling behaviour")
		}
		setupLog.Info("SMB controller configuration",
			"namespace", cfg.Namespace, "reconcileInterval", cfg.ReconcileInterval,
			"longhornNamespace", cfg.LonghornNamespace, "maxRetries", cfg.MaxRetries,
			"ownerDeployment", o.ownerDeployment, "dryRun", o.dryRun)
		smbOpts = smb.Options{Config: cfg, OwnerDeployment: o.ownerDeployment, DryRun: o.dryRun, VolumeGVK: gvk}

		// Keep the informer caches small: only labelled PVCs, ConfigMaps in the operator namespace
		// and Volumes in the Longhorn namespace. Reconcile reads go to the API directly.
		hasLabel, _ := labels.NewRequirement("smb-access", selection.Exists, nil)
		byObject[&corev1.PersistentVolumeClaim{}] = cache.ByObject{Label: labels.NewSelector().Add(*hasLabel)}
		byObject[&corev1.ConfigMap{}] = cache.ByObject{Namespaces: map[string]cache.Config{cfg.Namespace: {}}}
		vol := &unstructured.Unstructured{}
		vol.SetGroupVersionKind(gvk)
		byObject[vol] = cache.ByObject{Namespaces: map[string]cache.Config{cfg.LonghornNamespace: {}}}
	}

	mgr, err := ctrl.NewManager(restCfg, ctrl.Options{
		Scheme:                  scheme,
		Metrics:                 server.Options{BindAddress: o.metricsAddr},
		HealthProbeBindAddress:  o.probeAddr,
		LeaderElection:          o.leaderElect,
		LeaderElectionID:        LeaderElectionID,
		LeaderElectionNamespace: o.namespace,
		Cache:                   cache.Options{ByObject: byObject},
	})
	if err != nil {
		return err
	}

	if o.enableSMB {
		if _, err := smb.SetupWithManager(mgr, smbOpts); err != nil {
			return fmt.Errorf("setting up smb controller: %w", err)
		}
	}
	if o.enablePVReclaimer {
		p := o.pvReclaimer
		setupLog.Info("pv-reclaimer controller configuration",
			"dryRun", p.DryRun, "retainOnly", p.RetainOnly, "storageClasses", p.StorageClasses,
			"honorSkipAnnotation", p.HonorSkipAnnotation, "patchMode", p.Mode)
		if _, err := pvreclaimer.SetupWithManager(mgr, p); err != nil {
			return fmt.Errorf("setting up pv-reclaimer controller: %w", err)
		}
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return err
	}
	if err := mgr.AddReadyzCheck("ping", healthz.Ping); err != nil {
		return err
	}
	return mgr.Start(ctrl.SetupSignalHandler())
}

// loadConfig reads config.yaml from the ConfigMap (fatal when missing/empty/invalid, as in Python).
func loadConfig(c client.Client, ns, name string) (*config.Config, error) {
	var cm corev1.ConfigMap
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &cm); err != nil {
		return nil, fmt.Errorf("ConfigMap '%s' in namespace '%s' is required: %w", name, ns, err)
	}
	cfg, err := config.Parse(cm.Data["config.yaml"], config.Env{
		LoadBalancerIP: os.Getenv("SMB_LOADBALANCER_IP"),
		Username:       os.Getenv("SMB_USERNAME"),
		Password:       os.Getenv("SMB_PASSWORD"),
	})
	if err != nil {
		return nil, fmt.Errorf("ConfigMap '%s': %w", name, err)
	}
	return cfg, nil
}
