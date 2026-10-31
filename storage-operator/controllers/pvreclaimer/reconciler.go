package pvreclaimer

import (
	"context"
	"fmt"
	"slices"
	"sync/atomic"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// Event reasons emitted on the PV.
const (
	ReasonReclaimed    = "Reclaimed"
	ReasonWouldReclaim = "WouldReclaim"
	ReasonFailed       = "ReclaimFailed"
)

// Reconciler clears spec.claimRef on Released PVs.
type Reconciler struct {
	Client   client.Client
	Recorder events.EventRecorder
	Metrics  *Metrics
	Opts     Options

	ready atomic.Bool
}

// Ready reports whether the PV informer cache has synced (set by the readiness check).
func (r *Reconciler) Ready() bool { return r.ready.Load() }

// needsReclaim reports whether pv is Released and still carries the claim binding this mode removes.
func (r *Reconciler) needsReclaim(pv *corev1.PersistentVolume) bool {
	switch {
	case pv.Status.Phase != corev1.VolumeReleased, pv.Spec.ClaimRef == nil:
		return false
	case r.Opts.Mode == PatchModeKeepClaim:
		return pv.Spec.ClaimRef.UID != "" || pv.Spec.ClaimRef.ResourceVersion != ""
	}
	return true
}

// skipReason returns why an otherwise reclaimable pv is excluded by the opt-in filters, or "".
func (r *Reconciler) skipReason(pv *corev1.PersistentVolume) string {
	switch {
	case r.Opts.RetainOnly && pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimRetain:
		return "reclaim policy is not Retain"
	case len(r.Opts.StorageClasses) > 0 && !slices.Contains(r.Opts.StorageClasses, pv.Spec.StorageClassName):
		return "storage class not in allowlist"
	case r.Opts.HonorSkipAnnotation && pv.Annotations[SkipAnnotation] == "true":
		return "skip annotation set"
	}
	return ""
}

// patch builds the merge patch for the configured mode.
func (r *Reconciler) patch() client.Patch {
	if r.Opts.Mode == PatchModeKeepClaim {
		return client.RawPatch(types.MergePatchType, []byte(`{"spec":{"claimRef":{"uid":null,"resourceVersion":null}}}`))
	}
	return client.RawPatch(types.MergePatchType, []byte(`{"spec":{"claimRef":null}}`))
}

// Reconcile re-reads the PV and patches it only while it is still Released.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("pv", req.Name)
	var pv corev1.PersistentVolume
	if err := r.Client.Get(ctx, req.NamespacedName, &pv); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !r.needsReclaim(&pv) {
		return ctrl.Result{}, nil // not (or no longer) Released, or already reclaimed
	}
	if reason := r.skipReason(&pv); reason != "" {
		r.Metrics.Reclaimed.WithLabelValues(ResultSkipped).Inc()
		logger.V(1).Info("skipping Released PV", "reason", reason)
		return ctrl.Result{}, nil
	}

	claim := pv.Spec.ClaimRef.Namespace + "/" + pv.Spec.ClaimRef.Name
	var opts []client.PatchOption
	if r.Opts.DryRun {
		opts = append(opts, client.DryRunAll)
	}
	if err := r.Client.Patch(ctx, &pv, r.patch(), opts...); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		r.Metrics.Reclaimed.WithLabelValues(ResultError).Inc()
		r.Recorder.Eventf(&pv, nil, corev1.EventTypeWarning, ReasonFailed, "Reclaim", "clearing claimRef (was %s) failed: %v", claim, err)
		return ctrl.Result{}, fmt.Errorf("patching PV %s: %w", pv.Name, err)
	}
	if r.Opts.DryRun {
		r.Metrics.Reclaimed.WithLabelValues(ResultDryRun).Inc()
		r.Recorder.Eventf(&pv, nil, corev1.EventTypeNormal, ReasonWouldReclaim, "Reclaim", "dry-run: would reset claimRef (%s, was %s)", r.mode(), claim)
		logger.Info("dry-run: would reclaim PV", "claim", claim, "mode", r.mode())
		return ctrl.Result{}, nil
	}
	r.Metrics.Reclaimed.WithLabelValues(ResultReclaimed).Inc()
	r.Recorder.Eventf(&pv, nil, corev1.EventTypeNormal, ReasonReclaimed, "Reclaim", "reset claimRef (%s, was %s); PV is Available again", r.mode(), claim)
	logger.Info("reclaimed PV", "claim", claim, "mode", r.mode())
	return ctrl.Result{}, nil
}

func (r *Reconciler) mode() PatchMode {
	if r.Opts.Mode == "" {
		return PatchModeClear
	}
	return r.Opts.Mode
}
