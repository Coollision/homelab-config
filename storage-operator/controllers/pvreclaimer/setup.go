package pvreclaimer

import (
	"errors"
	"fmt"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// ControllerName is the controller-runtime controller name (metrics label controller="pv-reclaimer").
const ControllerName = "pv-reclaimer"

// ReleasedPredicate passes PVs whose status.phase is Released. The informer's initial list emits a
// create event per PV, so PVs that were already Released at start-up are reclaimed too (parity
// with the shell loop's start-up pass).
func ReleasedPredicate() predicate.Predicate {
	return predicate.NewPredicateFuncs(func(o client.Object) bool {
		pv, ok := o.(*corev1.PersistentVolume)
		return ok && pv.Status.Phase == corev1.VolumeReleased
	})
}

// SetupWithManager registers the pv-reclaimer controller, its metrics and its readiness check.
// o.Mode is validated and normalised once here (empty becomes PatchModeClear).
func SetupWithManager(mgr manager.Manager, o Options) (*Reconciler, error) {
	mode, err := ParsePatchMode(string(o.Mode))
	if err != nil {
		return nil, err
	}
	o.Mode = mode
	r := &Reconciler{
		Reader:   mgr.GetAPIReader(),
		Client:   mgr.GetClient(),
		Recorder: mgr.GetEventRecorder(ControllerName),
		Metrics:  NewMetrics(metrics.Registry),
		Opts:     o,
	}
	err = ctrl.NewControllerManagedBy(mgr).
		Named(ControllerName).
		For(&corev1.PersistentVolume{}, builder.WithPredicates(ReleasedPredicate())).
		Complete(r)
	if err != nil {
		return nil, fmt.Errorf("building %s controller: %w", ControllerName, err)
	}
	if err := mgr.AddReadyzCheck(ControllerName, r.readyzCheck(mgr.GetCache())); err != nil {
		return nil, err
	}
	return r, nil
}

// readyzCheck passes once the PV informer has synced (i.e. the start-up pass has been queued).
// It does not block: an unsynced informer fails the probe until the next attempt.
func (r *Reconciler) readyzCheck(informers cache.Informers) healthz.Checker {
	return func(req *http.Request) error {
		if r.Ready() {
			return nil
		}
		inf, err := informers.GetInformer(req.Context(), &corev1.PersistentVolume{}, cache.BlockUntilSynced(false))
		if err != nil {
			return fmt.Errorf("pv-reclaimer: getting PV informer: %w", err)
		}
		if !inf.HasSynced() {
			return errors.New("pv-reclaimer: PV informer not synced")
		}
		r.ready.Store(true)
		return nil
	}
}
