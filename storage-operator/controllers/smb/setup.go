package smb

import (
	"context"
	"errors"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

// ControllerName is the controller-runtime controller name (metrics label controller="smb").
const ControllerName = "smb"

// VolumeGVR returns the Longhorn Volume resource from the config.
func VolumeGVR(o Options) schema.GroupVersionResource {
	c := o.Config
	return schema.GroupVersionResource{Group: c.LonghornGroup, Version: c.LonghornVersion, Resource: c.LonghornPlural}
}

// ResolveVolumeGVK maps the configured group/version/plural to a Kind via the REST mapper.
func ResolveVolumeGVK(mgr manager.Manager, o Options) (schema.GroupVersionKind, error) {
	return mgr.GetRESTMapper().KindFor(VolumeGVR(o))
}

// SetupWithManager registers the SMB controller, its metrics and its readiness check.
func SetupWithManager(mgr manager.Manager, o Options) (*Reconciler, error) {
	if o.Config == nil {
		return nil, errors.New("smb: config is required")
	}
	gvk := o.VolumeGVK
	if gvk.Empty() {
		var err error
		if gvk, err = ResolveVolumeGVK(mgr, o); err != nil {
			return nil, err
		}
	}
	writer := mgr.GetClient()
	if o.DryRun {
		writer = client.NewDryRunClient(writer)
	}
	r := &Reconciler{
		Reader:    mgr.GetAPIReader(),
		Writer:    writer,
		Opts:      o,
		Metrics:   NewMetrics(metrics.Registry),
		VolumeGVK: gvk,
	}

	key := types.NamespacedName{Namespace: o.Config.Namespace, Name: o.Config.SMBDeploymentName}
	toKey := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []ctrl.Request {
		return []ctrl.Request{{NamespacedName: key}}
	})

	vol := &unstructured.Unstructured{}
	vol.SetGroupVersionKind(gvk)

	// One event at start-up: the Python operator always reconciled once before watching.
	kick := make(chan event.GenericEvent, 1)
	kick <- event.GenericEvent{Object: &corev1.ConfigMap{}}

	err := ctrl.NewControllerManagedBy(mgr).
		Named(ControllerName).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Watches(&corev1.PersistentVolumeClaim{}, toKey, builder.WithPredicates(SMBPVCPredicate())).
		Watches(&corev1.ConfigMap{}, toKey, builder.WithPredicates(MigrationConfigMapPredicate(o.Config.Namespace))).
		Watches(vol, toKey, builder.WithPredicates(VolumeStatusPredicate(o.Config.LonghornNamespace))).
		WatchesRawSource(source.Channel(kick, toKey)).
		Complete(r)
	if err != nil {
		return nil, err
	}
	if err := mgr.AddReadyzCheck(ControllerName, func(*http.Request) error {
		if !r.Ready() {
			return errors.New("smb: no successful reconcile yet")
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return r, nil
}
