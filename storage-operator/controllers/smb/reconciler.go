// Package smb is the SMB server controller: a Go port of smb_operator_v2.py with identical
// inputs (PVC labels, smb-operator-config, smb-operator-migration) and identical outputs
// (smb-config, smb-mounts, smb-server Deployment/Service, migrate-* PV/PVCs, PVC annotations,
// core/v1 Events). See README.md for the parity notes.
package smb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	"github.com/google/go-cmp/cmp"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"homelab/storage-operator/internal/config"
	"homelab/storage-operator/internal/render"
	"homelab/storage-operator/internal/shares"
)

// MigrationConfigMap is the optional ConfigMap listing extra Longhorn volumes to expose.
const (
	MigrationConfigMap = "smb-operator-migration"
	MigrationKey       = "volumes.yaml"
	EventComponent     = "smb-operator"
	// LegacyLonghornNamespace is the second namespace Volume lookups fall back to.
	LegacyLonghornNamespace = "longhorn-system"
)

// ErrOwnerUnresolved means the operator's own Deployment could not be read, so the smb-server
// Service cannot get its ownerReference. Creating it anyway would leave an orphan, so the
// reconcile fails and is requeued with backoff instead.
var ErrOwnerUnresolved = errors.New("operator Deployment UID unresolved")

// Options configure the controller.
type Options struct {
	Config *config.Config
	// OwnerDeployment is the operator's own Deployment: source of the inherited child labels and
	// owner of the smb-server Service. Python hardcoded "smb-operator".
	OwnerDeployment string
	// DryRun sends every write with dryRun=All and logs what would change.
	DryRun bool
	// VolumeGVK is the Longhorn Volume kind; resolved from the REST mapper when empty.
	VolumeGVK schema.GroupVersionKind
}

// Reconciler renders one SMB server from the global set of shares. Every watch maps onto one
// fixed request, so the workqueue serialises reconciles.
type Reconciler struct {
	// Reader does the live (uncached) reads, like the Python operator's direct API calls.
	Reader client.Reader
	// Writer performs writes; a dry-run client in --dry-run mode.
	Writer    client.Client
	Opts      Options
	Metrics   *Metrics
	VolumeGVK schema.GroupVersionKind
	Now       func() time.Time

	mu               sync.Mutex
	currentShares    map[string]struct{}
	currentStructure map[string]struct{}
	operatorUID      string
	operatorLabels   map[string]string
	ready            atomic.Bool
}

// Ready reports whether a reconcile has succeeded (readyz parity).
func (r *Reconciler) Ready() bool { return r.ready.Load() }

func (r *Reconciler) cfg() *config.Config { return r.Opts.Config }

func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Reconcile runs one full pass and requeues after reconcileInterval as the periodic resync (the
// Python watches restarted every reconcileInterval and re-listed everything).
func (r *Reconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ok, err := r.reconcileOnce(ctx)
	if ok {
		r.Metrics.ReconcileTotal.Inc()
		r.Metrics.LastReconcile.Set(float64(r.now().UnixNano()) / 1e9)
		r.Metrics.SharesManaged.Set(float64(len(r.currentShares)))
		r.ready.Store(true)
	}
	if errors.Is(err, ErrOwnerUnresolved) {
		// Workqueue rate-limited backoff instead of waiting a full reconcileInterval.
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: time.Duration(r.cfg().ReconcileInterval) * time.Second}, nil
}

// ReconcileOnce is reconcile() from the Python operator. It returns success.
func (r *Reconciler) ReconcileOnce(ctx context.Context) bool {
	ok, _ := r.reconcileOnce(ctx)
	return ok
}

// reconcileOnce is ReconcileOnce that also returns the (already logged and counted) error.
func (r *Reconciler) reconcileOnce(ctx context.Context) (ok bool, rerr error) {
	logger := log.FromContext(ctx)
	defer func() {
		if p := recover(); p != nil {
			logger.Error(fmt.Errorf("%v", p), "Reconciliation error")
			r.Metrics.ReconcileErrors.Inc()
			ok = false
		}
	}()
	ok, err := r.reconcile(ctx, logger)
	if err != nil {
		logger.Error(err, "Reconciliation error")
		r.Metrics.ReconcileErrors.Inc()
		return false, err
	}
	return ok, nil
}

func (r *Reconciler) reconcile(ctx context.Context, logger logr.Logger) (bool, error) {
	logger.Info("Starting reconciliation...")
	pvcShares, err := r.DiscoverShares(ctx, logger)
	if err != nil {
		return false, err
	}
	migrationShares := r.DiscoverMigrationShares(ctx, logger)
	all := append(slices.Clone(pvcShares), migrationShares...)
	if len(migrationShares) > 0 {
		logger.Info("Including migration share(s) in SMB server", "count", len(migrationShares), "shares", names(migrationShares))
	}

	changed, err := r.withRetry(ctx, "update_mount_table", func() (bool, error) {
		return r.updateMountTable(ctx, logger, render.MountTable(all))
	})
	if err != nil {
		return false, err
	}
	if changed {
		var detached []string
		for _, s := range all {
			if s.MountType == shares.MountNFS && !s.Attached {
				detached = append(detached, s.Name)
			}
		}
		if len(detached) > 0 {
			sort.Strings(detached)
			logger.Info("  Detached (unmounted, share still published): " + strings.Join(detached, ", "))
		}
	}

	newShares := map[string]struct{}{}
	newStructure := map[string]struct{}{}
	for _, s := range all {
		newShares[s.UniqueID()] = struct{}{}
		newStructure[s.StructuralKey()] = struct{}{}
	}
	// Starts empty, like Python: zero shares on a fresh start is "no change".
	if setEqual(newStructure, r.currentStructure) {
		logger.V(1).Info("No structural changes", "shares", len(all))
		return true, nil
	}

	added := setDiff(newShares, r.currentShares)
	removed := setDiff(r.currentShares, newShares)
	logger.Info("Share configuration changed", "previous", len(r.currentShares), "current", len(newShares),
		"added", added, "removed", removed)

	if _, err := r.withRetry(ctx, "update_smb_config", func() (bool, error) {
		return true, r.updateSMBConfig(ctx, logger, render.SMBConf(r.cfg(), all))
	}); err != nil {
		return false, err
	}

	r.EnsureMigrationPVCs(ctx, logger, migrationShares)

	dep, err := render.Deployment(r.cfg(), all, r.childBaseLabels(ctx, logger), r.now())
	if err != nil {
		return false, err
	}
	success, err := r.withRetry(ctx, "apply_deployment", func() (bool, error) {
		return r.applyDeployment(ctx, logger, dep)
	})
	if err != nil {
		return false, err
	}
	if !success {
		return false, nil
	}
	if _, err := r.withRetry(ctx, "ensure_service", func() (bool, error) {
		return true, r.ensureService(ctx, logger)
	}); err != nil {
		return false, err
	}
	r.currentShares = newShares
	r.currentStructure = newStructure
	logger.Info("Reconciliation completed successfully")
	for _, id := range added {
		ns, pvc, _ := strings.Cut(id, "/")
		r.recordEvent(ctx, "PersistentVolumeClaim", pvc, ns, "ShareAdded", "SMB share added: "+pvc, corev1.EventTypeNormal)
	}
	for _, id := range removed {
		ns, pvc, _ := strings.Cut(id, "/")
		r.recordEvent(ctx, "PersistentVolumeClaim", pvc, ns, "ShareRemoved", "SMB share removed", corev1.EventTypeNormal)
	}
	return true, nil
}

// ---------------------------------------------------------------------------------------------
// Discovery

// DiscoverShares lists every PVC labelled smb-access (all namespaces) and resolves it.
func (r *Reconciler) DiscoverShares(ctx context.Context, logger logr.Logger) ([]shares.Share, error) {
	var list corev1.PersistentVolumeClaimList
	if err := r.Reader.List(ctx, &list, client.HasLabels{"smb-access"}); err != nil {
		return nil, fmt.Errorf("listing PVCs: %w", err)
	}
	var out []shares.Share
	for i := range list.Items {
		pvc := &list.Items[i]
		in := shares.PVCInput{Namespace: pvc.Namespace, Name: pvc.Name, VolumeName: pvc.Spec.VolumeName, Labels: pvc.Labels}
		var res shares.PVCResult
		if in.VolumeName == "" {
			res = shares.UnboundResult(in)
		} else {
			if mode, ok := shares.NormalizeAccess(in.Labels); !ok {
				logger.Info(fmt.Sprintf("Invalid access mode '%s' for %s/%s, defaulting to '%s'",
					in.Labels["smb-access"], in.Namespace, in.Name, mode))
			}
			logger.Info(fmt.Sprintf("Processing PVC: %s/%s -> %s", in.Namespace, in.Name, shares.ShareName(in)))
			res = shares.ResolvePVC(in, r.getLonghornVolume(ctx, in.VolumeName), r.cfg().SMBLoadBalancerIP)
		}
		logger.Info(res.Log, "warning", res.Warn)
		r.updatePVCAnnotations(ctx, logger, in.Namespace, in.Name, res.Status, res.SharePath)
		if res.Event != nil {
			r.recordEvent(ctx, "PersistentVolumeClaim", in.Name, in.Namespace, res.Event.Reason, res.Event.Message, res.Event.Type)
		}
		if res.Share != nil {
			out = append(out, *res.Share)
		}
	}
	logger.Info(fmt.Sprintf("Discovered %d shares from %d PVCs", len(out), len(list.Items)))
	return out, nil
}

// DiscoverMigrationShares reads smb-operator-migration. Problems never fail the reconcile.
func (r *Reconciler) DiscoverMigrationShares(ctx context.Context, logger logr.Logger) []shares.Share {
	var cm corev1.ConfigMap
	if err := r.Reader.Get(ctx, types.NamespacedName{Namespace: r.cfg().Namespace, Name: MigrationConfigMap}, &cm); err != nil {
		if !apierrors.IsNotFound(err) {
			logger.Info("Failed to read migration ConfigMap", "error", err.Error())
		}
		return nil
	}
	entries, warnings, err := shares.ParseMigration(cm.Data[MigrationKey])
	if err != nil {
		logger.Error(err, "migration ConfigMap")
		return nil
	}
	for _, w := range warnings {
		logger.Info(w, "warning", true)
	}
	if len(entries) == 0 {
		return nil
	}
	var out []shares.Share
	for _, e := range entries {
		vol := r.getLonghornVolume(ctx, e.VolumeName)
		if vol == nil {
			logger.Info(fmt.Sprintf("Migration: Longhorn volume '%s' not found, skipping", e.VolumeName), "warning", true)
			continue
		}
		s, ok := shares.ResolveMigration(e, vol, r.cfg().Namespace)
		if !ok {
			logger.Info(fmt.Sprintf("Migration: failed to parse NFS endpoint for '%s'", e.VolumeName), "warning", true)
			continue
		}
		out = append(out, s)
	}
	logger.Info(fmt.Sprintf("Migration: resolved %d share(s)", len(out)))
	return out
}

// getLonghornVolume tries longhorn.namespace then longhorn-system. Any error counts as "not
// found" (Python swallowed every ApiException here).
func (r *Reconciler) getLonghornVolume(ctx context.Context, name string) map[string]any {
	for _, ns := range []string{r.cfg().LonghornNamespace, LegacyLonghornNamespace} {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(r.VolumeGVK)
		if err := r.Reader.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, u); err == nil {
			return u.Object
		}
	}
	return nil
}

// ---------------------------------------------------------------------------------------------
// Writes

// updatePVCAnnotations writes the smb-operator status annotations; failures are only logged.
func (r *Reconciler) updatePVCAnnotations(ctx context.Context, logger logr.Logger, ns, name, status, sharePath string) {
	patch := map[string]any{"metadata": map[string]any{"annotations": map[string]string{
		render.AnnotPVCStatus:     status,
		render.AnnotPVCSharePath:  sharePath,
		render.AnnotPVCLastUpdate: r.now().UTC().Format(render.TimeFormat),
	}}}
	body, err := json.Marshal(patch)
	if err != nil {
		logger.Error(err, "Failed to encode PVC annotation patch", "pvc", ns+"/"+name)
		return
	}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
	if err := r.Writer.Patch(ctx, pvc, client.RawPatch(types.MergePatchType, body)); err != nil {
		logger.Info("Failed to update PVC annotations", "pvc", ns+"/"+name, "error", err.Error())
	}
}

// recordEvent creates a raw core/v1 Event named <obj>.<unix seconds>, as Python did. Failures
// (including a same-second name clash) are only logged at debug level.
func (r *Reconciler) recordEvent(ctx context.Context, kind, name, ns, reason, message, typ string) {
	now := metav1.NewTime(r.now())
	ev := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: fmt.Sprintf("%s.%d", name, now.Unix()), Namespace: ns},
		InvolvedObject: corev1.ObjectReference{Kind: kind, Name: name, Namespace: ns},
		Reason:         reason,
		Message:        message,
		Type:           typ,
		FirstTimestamp: now,
		LastTimestamp:  now,
		Count:          1,
		Source:         corev1.EventSource{Component: EventComponent},
	}
	if err := r.Writer.Create(ctx, ev); err != nil {
		log.FromContext(ctx).V(1).Info("Failed to record event", "error", err.Error())
		return
	}
	r.Metrics.EventsSent.Inc()
}

// operatorInfo returns (uid, labels) of the operator Deployment, cached after the first success.
// On failure it logs, returns the last known labels and an error wrapping ErrOwnerUnresolved.
func (r *Reconciler) operatorInfo(ctx context.Context, logger logr.Logger) (string, map[string]string, error) {
	if r.operatorUID != "" {
		return r.operatorUID, r.operatorLabels, nil
	}
	var d appsv1.Deployment
	if err := r.Reader.Get(ctx, types.NamespacedName{Namespace: r.cfg().Namespace, Name: r.Opts.OwnerDeployment}, &d); err != nil {
		logger.Info("Failed to get operator UID", "error", err.Error())
		return "", r.operatorLabels, fmt.Errorf("%w: deployment %s/%s: %w", ErrOwnerUnresolved, r.cfg().Namespace, r.Opts.OwnerDeployment, err)
	}
	if d.UID == "" {
		return "", r.operatorLabels, fmt.Errorf("%w: deployment %s/%s has no UID", ErrOwnerUnresolved, r.cfg().Namespace, r.Opts.OwnerDeployment)
	}
	r.operatorUID = string(d.UID)
	r.operatorLabels = map[string]string{}
	for k, v := range d.Labels {
		r.operatorLabels[k] = v
	}
	return r.operatorUID, r.operatorLabels, nil
}

// childBaseLabels returns the operator Deployment's labels (empty when unresolved, as in Python).
func (r *Reconciler) childBaseLabels(ctx context.Context, logger logr.Logger) map[string]string {
	_, l, _ := r.operatorInfo(ctx, logger)
	return l
}

// childLabels merges extra onto childBaseLabels.
func (r *Reconciler) childLabels(ctx context.Context, logger logr.Logger, extra map[string]string) map[string]string {
	return render.ChildLabels(r.childBaseLabels(ctx, logger), extra)
}

func (r *Reconciler) updateMountTable(ctx context.Context, logger logr.Logger, table string) (bool, error) {
	ns := r.cfg().Namespace
	var cm corev1.ConfigMap
	err := r.Reader.Get(ctx, types.NamespacedName{Namespace: ns, Name: render.MountTableConfigMap}, &cm)
	if apierrors.IsNotFound(err) {
		cm = corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: render.MountTableConfigMap, Namespace: ns, Labels: r.childLabels(ctx, logger, nil)},
			Data:       map[string]string{render.MountTableKey: table},
		}
		if err := r.Writer.Create(ctx, &cm); err != nil {
			return false, err
		}
		logger.Info("Created mount table ConfigMap", "dryRun", r.Opts.DryRun)
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if v, ok := cm.Data[render.MountTableKey]; ok && v == table {
		return false, nil
	}
	if r.Opts.DryRun {
		logger.Info("DRY-RUN: mount table would change", "diff", cmp.Diff(cm.Data[render.MountTableKey], table))
	}
	if err := r.patchData(ctx, &cm, render.MountTableKey, table); err != nil {
		return false, err
	}
	logger.Info("Updated mount table ConfigMap (no pod restart required)", "dryRun", r.Opts.DryRun)
	return true, nil
}

func (r *Reconciler) updateSMBConfig(ctx context.Context, logger logr.Logger, conf string) error {
	ns := r.cfg().Namespace
	var cm corev1.ConfigMap
	err := r.Reader.Get(ctx, types.NamespacedName{Namespace: ns, Name: render.SMBConfigMap}, &cm)
	if apierrors.IsNotFound(err) {
		cm = corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: render.SMBConfigMap, Namespace: ns, Labels: r.childLabels(ctx, logger, nil)},
			Data:       map[string]string{render.SMBConfKey: conf},
		}
		if err := r.Writer.Create(ctx, &cm); err != nil {
			return err
		}
		logger.Info("Created SMB configuration ConfigMap", "dryRun", r.Opts.DryRun)
		return nil
	}
	if err != nil {
		return err
	}
	if r.Opts.DryRun && cm.Data[render.SMBConfKey] != conf {
		logger.Info("DRY-RUN: smb.conf would change", "diff", cmp.Diff(cm.Data[render.SMBConfKey], conf))
	}
	if err := r.patchData(ctx, &cm, render.SMBConfKey, conf); err != nil {
		return err
	}
	logger.Info("Updated SMB configuration ConfigMap", "dryRun", r.Opts.DryRun)
	return nil
}

// patchData merge-patches one data key of cm.
func (r *Reconciler) patchData(ctx context.Context, cm *corev1.ConfigMap, key, value string) error {
	body, err := json.Marshal(map[string]any{"data": map[string]string{key: value}})
	if err != nil {
		return fmt.Errorf("encoding ConfigMap %s data patch: %w", cm.Name, err)
	}
	target := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: cm.Namespace, Name: cm.Name}}
	return r.Writer.Patch(ctx, target, client.RawPatch(types.MergePatchType, body))
}

// applyDeployment mirrors apply_deployment: create on 404; replace when the strategy, the set of
// volume names or the set of mount paths differ (strategic merge cannot drop list entries);
// otherwise a strategic-merge patch with the full desired object. Other read errors return
// (false, nil) like Python (logged, not counted as a reconcile error).
func (r *Reconciler) applyDeployment(ctx context.Context, logger logr.Logger, desired *appsv1.Deployment) (bool, error) {
	cfg := r.cfg()
	key := types.NamespacedName{Namespace: cfg.Namespace, Name: cfg.SMBDeploymentName}
	var existing appsv1.Deployment
	if err := r.Reader.Get(ctx, key, &existing); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("Creating new SMB deployment...", "dryRun", r.Opts.DryRun)
			if err := r.Writer.Create(ctx, desired.DeepCopy()); err != nil {
				return false, err
			}
			r.recordEvent(ctx, "Deployment", cfg.SMBDeploymentName, cfg.Namespace, "Created", "SMB deployment created", corev1.EventTypeNormal)
			return true, nil
		}
		logger.Error(err, "Failed to apply deployment")
		return false, nil
	}
	logger.Info("Updating existing SMB deployment...")

	var result *appsv1.Deployment
	var err error
	if NeedsReplace(&existing, desired) {
		logger.Info("Detected significant deployment change (strategy/volumes/volumeMounts); using replace operation")
		result = desired.DeepCopy()
		err = r.Writer.Update(ctx, result)
	} else {
		var body []byte
		body, err = StrategicPatchBody(desired)
		if err == nil {
			result = &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name}}
			err = r.Writer.Patch(ctx, result, client.RawPatch(types.StrategicMergePatchType, body))
		}
	}
	if err != nil {
		// Python caught every ApiException from replace/patch inside apply_deployment.
		logger.Error(err, "Failed to apply deployment")
		return false, nil
	}
	if r.Opts.DryRun {
		if equality.Semantic.DeepEqual(existing.Spec.Template, result.Spec.Template) {
			logger.Info("DRY-RUN: smb-server pod template unchanged (no pod roll)")
		} else {
			logger.Info("DRY-RUN: smb-server POD TEMPLATE WOULD CHANGE (pod roll)",
				"diff", cmp.Diff(existing.Spec.Template, result.Spec.Template))
		}
	}
	logger.Info("SMB deployment updated successfully", "dryRun", r.Opts.DryRun)
	r.recordEvent(ctx, "Deployment", cfg.SMBDeploymentName, cfg.Namespace, "Updated", "SMB deployment updated with new configuration", corev1.EventTypeNormal)
	return true, nil
}

// NeedsReplace is the replace-vs-patch decision from apply_deployment.
func NeedsReplace(existing, desired *appsv1.Deployment) bool {
	if a, b := existing.Spec.Strategy.Type, desired.Spec.Strategy.Type; a != "" && b != "" && a != b {
		return true
	}
	volNames := func(d *appsv1.Deployment) map[string]struct{} {
		m := map[string]struct{}{}
		for _, v := range d.Spec.Template.Spec.Volumes {
			m[v.Name] = struct{}{}
		}
		return m
	}
	mountPaths := func(d *appsv1.Deployment) map[string]struct{} {
		m := map[string]struct{}{}
		if len(d.Spec.Template.Spec.Containers) > 0 {
			for _, vm := range d.Spec.Template.Spec.Containers[0].VolumeMounts {
				m[vm.MountPath] = struct{}{}
			}
		}
		return m
	}
	return !setEqual(volNames(existing), volNames(desired)) || !setEqual(mountPaths(existing), mountPaths(desired))
}

// StrategicPatchBody serialises the full desired Deployment as a strategic-merge patch, the
// way the Python client sent it, minus Go zero-value noise (creationTimestamp: null, status).
func StrategicPatchBody(d *appsv1.Deployment) ([]byte, error) {
	raw, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	delete(m, "status")
	if md, ok := m["metadata"].(map[string]any); ok {
		delete(md, "creationTimestamp")
	}
	if spec, ok := m["spec"].(map[string]any); ok {
		if tpl, ok := spec["template"].(map[string]any); ok {
			if md, ok := tpl["metadata"].(map[string]any); ok {
				delete(md, "creationTimestamp")
			}
		}
	}
	return json.Marshal(m)
}

// ensureService is create-only (ensure_service). The Service gets an ownerReference to the
// operator Deployment so it is garbage-collected with it.
func (r *Reconciler) ensureService(ctx context.Context, logger logr.Logger) error {
	cfg := r.cfg()
	var svc corev1.Service
	err := r.Reader.Get(ctx, types.NamespacedName{Namespace: cfg.Namespace, Name: cfg.SMBServiceName}, &svc)
	if err == nil {
		logger.V(1).Info("SMB service already exists")
		return nil
	}
	if !apierrors.IsNotFound(err) {
		logger.Error(err, "Failed to check service")
		return nil
	}
	logger.Info("Creating SMB service with owner reference...", "dryRun", r.Opts.DryRun)
	uid, labels, err := r.operatorInfo(ctx, logger)
	if err != nil {
		return fmt.Errorf("refusing to create Service %s/%s without ownerReference: %w", cfg.Namespace, cfg.SMBServiceName, err)
	}
	if err := r.Writer.Create(ctx, render.Service(cfg, labels, r.Opts.OwnerDeployment, uid)); err != nil {
		return err
	}
	logger.Info("SMB service created", "loadBalancerIP", cfg.SMBLoadBalancerIP)
	r.recordEvent(ctx, "Service", cfg.SMBServiceName, cfg.Namespace, "Created",
		"SMB service created with LoadBalancer IP "+cfg.SMBLoadBalancerIP, corev1.EventTypeNormal)
	return nil
}

// EnsureMigrationPVCs mirrors ensure_migration_pvcs: GC orphaned migrate-* PVCs then PVs, then
// create the PV+PVC for each desired direct share. Errors are logged, never returned.
func (r *Reconciler) EnsureMigrationPVCs(ctx context.Context, logger logr.Logger, migration []shares.Share) {
	ns := r.cfg().Namespace
	labels := r.childLabels(ctx, logger, map[string]string{render.LabelMigration: "true"})
	desired := map[string]shares.Share{}
	var order []string
	for _, s := range migration {
		if s.MountType == shares.MountDirect {
			if _, dup := desired[s.MigrationPVCName()]; !dup {
				order = append(order, s.MigrationPVCName())
			}
			desired[s.MigrationPVCName()] = s
		}
	}
	sel := client.MatchingLabels{render.LabelMigration: "true"}

	var pvcs corev1.PersistentVolumeClaimList
	if err := r.Reader.List(ctx, &pvcs, client.InNamespace(ns), sel); err != nil {
		logger.Info("Failed to list migration PVCs for cleanup", "error", err.Error())
	} else {
		for i := range pvcs.Items {
			if _, ok := desired[pvcs.Items[i].Name]; !ok {
				logger.Info("Migration cleanup: deleting orphaned PVC", "pvc", ns+"/"+pvcs.Items[i].Name)
				if err := r.Writer.Delete(ctx, &pvcs.Items[i]); err != nil && !apierrors.IsNotFound(err) {
					logger.Info("Failed to delete orphaned PVC", "error", err.Error())
				}
			}
		}
	}
	var pvs corev1.PersistentVolumeList
	if err := r.Reader.List(ctx, &pvs, sel); err != nil {
		logger.Info("Failed to list migration PVs for cleanup", "error", err.Error())
	} else {
		for i := range pvs.Items {
			if _, ok := desired[pvs.Items[i].Name]; !ok {
				logger.Info("Migration cleanup: deleting orphaned PV", "pv", pvs.Items[i].Name)
				if err := r.Writer.Delete(ctx, &pvs.Items[i]); err != nil && !apierrors.IsNotFound(err) {
					logger.Info("Failed to delete orphaned PV", "error", err.Error())
				}
			}
		}
	}

	for _, pvcName := range order {
		s := desired[pvcName]
		vol := r.getLonghornVolume(ctx, s.LonghornVolumeName)
		if vol == nil {
			logger.Info("Migration: cannot create PV — volume not found", "volume", s.LonghornVolumeName)
			continue
		}
		size := shares.SizeMi(vol)
		var pv corev1.PersistentVolume
		err := r.Reader.Get(ctx, types.NamespacedName{Name: s.MigrationPVName()}, &pv)
		switch {
		case apierrors.IsNotFound(err):
			logger.Info("Migration: creating PV", "pv", s.MigrationPVName(), "size", size)
			if err := r.Writer.Create(ctx, render.MigrationPV(s, size, labels)); err != nil {
				logger.Error(err, "Migration: failed to create PV", "pv", s.MigrationPVName())
				continue
			}
		case err != nil:
			logger.Info("Migration: failed to read PV", "pv", s.MigrationPVName(), "error", err.Error())
			continue
		}
		var pvc corev1.PersistentVolumeClaim
		err = r.Reader.Get(ctx, types.NamespacedName{Namespace: ns, Name: pvcName}, &pvc)
		switch {
		case apierrors.IsNotFound(err):
			logger.Info("Migration: creating PVC", "pvc", ns+"/"+pvcName)
			if err := r.Writer.Create(ctx, render.MigrationPVC(s, ns, size, labels)); err != nil {
				logger.Error(err, "Migration: failed to create PVC", "pvc", pvcName)
			}
		case err != nil:
			logger.Info("Migration: failed to read PVC", "pvc", pvcName, "error", err.Error())
		}
	}
}

// ---------------------------------------------------------------------------------------------
// Retry (retry_on_error)

func isRetryable(err error, codes []int) bool {
	var se apierrors.APIStatus
	if !errors.As(err, &se) {
		return false
	}
	return slices.Contains(codes, int(se.Status().Code))
}

func (r *Reconciler) withRetry(ctx context.Context, name string, fn func() (bool, error)) (bool, error) {
	cfg := r.cfg()
	delay := time.Duration(cfg.InitialDelay) * time.Second
	attempts := max(cfg.MaxRetries, 1)
	for attempt := 0; attempt < attempts; attempt++ {
		ok, err := fn()
		if err == nil {
			return ok, nil
		}
		if !isRetryable(err, cfg.RetryableStatusCodes) || attempt == attempts-1 {
			return false, fmt.Errorf("%s: %w", name, err)
		}
		log.FromContext(ctx).Info("API error, retrying", "func", name, "delay", delay, "attempt", attempt+1, "max", attempts)
		r.Metrics.RetryCount.Inc()
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(delay):
		}
		delay *= time.Duration(max(cfg.BackoffMultiplier, 1))
	}
	return false, nil
}

// ---------------------------------------------------------------------------------------------
// helpers

func names(ss []shares.Share) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = s.Name
	}
	return out
}

func setEqual(a, b map[string]struct{}) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			return false
		}
	}
	return true
}

func setDiff(a, b map[string]struct{}) []string {
	var out []string
	for k := range a {
		if _, ok := b[k]; !ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
