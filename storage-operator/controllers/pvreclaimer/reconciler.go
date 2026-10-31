package pvreclaimer

import (
	"context"
	"fmt"
	"slices"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// conflictRequeue is the delay before retrying a PV that changed between read and patch.
const conflictRequeue = time.Second

// Event reasons emitted on the PV.
const (
	// ReasonReclaimed: claimRef was reset; the PV is Available again.
	ReasonReclaimed = "Reclaimed"
	// ReasonWouldReclaim: dry-run; repeats on every update/resync of a still-Released PV.
	ReasonWouldReclaim = "WouldReclaim"
	// ReasonFailed: the patch failed (Warning); repeats on each retry.
	ReasonFailed = "ReclaimFailed"
)

// Reconciler clears spec.claimRef on Released PVs.
type Reconciler struct {
	// Reader is used for the uncached re-read at the start of Reconcile.
	Reader client.Reader
	// Client sends the patches.
	Client client.Client
	// Recorder emits the Reclaimed/WouldReclaim/ReclaimFailed events on the PV.
	Recorder events.EventRecorder
	// Metrics counts handled PVs by result.
	Metrics *Metrics
	// Opts.Mode is normalised by SetupWithManager; an empty Mode patches like PatchModeClear.
	Opts Options

	ready atomic.Bool
}

// Ready reports whether the PV informer has synced (latched by the readiness check).
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

// patch builds the merge patch for the configured mode. It carries the resourceVersion that was
// read, so the apiserver rejects it with a conflict if the PV changed (e.g. got Bound) since.
func (r *Reconciler) patch(pv *corev1.PersistentVolume) client.Patch {
	claimRef := `null`
	if r.Opts.Mode == PatchModeKeepClaim {
		claimRef = `{"uid":null,"resourceVersion":null}`
	}
	return client.RawPatch(types.MergePatchType, fmt.Appendf(nil,
		`{"metadata":{"resourceVersion":%q},"spec":{"claimRef":%s}}`, pv.ResourceVersion, claimRef))
}

// Reconcile re-reads the PV from the API and patches it only while it is still Released. A
// conflict (the PV changed in between) is requeued without an error event.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("pv", req.Name)
	var pv corev1.PersistentVolume
	if err := r.Reader.Get(ctx, req.NamespacedName, &pv); err != nil {
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
	if err := r.Client.Patch(ctx, &pv, r.patch(&pv), opts...); err != nil {
		switch {
		case apierrors.IsNotFound(err):
			return ctrl.Result{}, nil
		case apierrors.IsConflict(err):
			logger.V(1).Info("PV changed since it was read; requeueing")
			return ctrl.Result{RequeueAfter: conflictRequeue}, nil
		}
		r.Metrics.Reclaimed.WithLabelValues(ResultError).Inc()
		r.Recorder.Eventf(&pv, nil, corev1.EventTypeWarning, ReasonFailed, "Reclaim", "clearing claimRef (was %s) failed: %v", claim, err)
		return ctrl.Result{}, fmt.Errorf("patching PV %s: %w", pv.Name, err)
	}
	if r.Opts.DryRun {
		r.Metrics.Reclaimed.WithLabelValues(ResultDryRun).Inc()
		r.Recorder.Eventf(&pv, nil, corev1.EventTypeNormal, ReasonWouldReclaim, "Reclaim", "dry-run: would reset claimRef (%s, was %s)", r.Opts.Mode, claim)
		logger.Info("dry-run: would reclaim PV", "claim", claim, "mode", r.Opts.Mode)
		return ctrl.Result{}, nil
	}
	r.Metrics.Reclaimed.WithLabelValues(ResultReclaimed).Inc()
	r.Recorder.Eventf(&pv, nil, corev1.EventTypeNormal, ReasonReclaimed, "Reclaim", "reset claimRef (%s, was %s); PV is Available again", r.Opts.Mode, claim)
	logger.Info("reclaimed PV", "claim", claim, "mode", r.Opts.Mode)
	return ctrl.Result{}, nil
}
