package pvreclaimer

import (
	"context"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

type pvOpt func(*corev1.PersistentVolume)

func phase(p corev1.PersistentVolumePhase) pvOpt {
	return func(pv *corev1.PersistentVolume) { pv.Status.Phase = p }
}
func policy(p corev1.PersistentVolumeReclaimPolicy) pvOpt {
	return func(pv *corev1.PersistentVolume) { pv.Spec.PersistentVolumeReclaimPolicy = p }
}
func class(c string) pvOpt { return func(pv *corev1.PersistentVolume) { pv.Spec.StorageClassName = c } }
func annotate(k, v string) pvOpt {
	return func(pv *corev1.PersistentVolume) { pv.Annotations = map[string]string{k: v} }
}
func noClaim() pvOpt { return func(pv *corev1.PersistentVolume) { pv.Spec.ClaimRef = nil } }
func uidCleared() pvOpt {
	return func(pv *corev1.PersistentVolume) { pv.Spec.ClaimRef.UID, pv.Spec.ClaimRef.ResourceVersion = "", "" }
}

func newPV(opts ...pvOpt) *corev1.PersistentVolume {
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-1"},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
			StorageClassName:              "longhorn",
			ClaimRef:                      &corev1.ObjectReference{Kind: "PersistentVolumeClaim", Namespace: "apps", Name: "data", UID: "u-1", ResourceVersion: "42"},
		},
		Status: corev1.PersistentVolumeStatus{Phase: corev1.VolumeReleased},
	}
	for _, o := range opts {
		o(pv)
	}
	return pv
}

func TestReconcile(t *testing.T) {
	cleared := func(t *testing.T, pv *corev1.PersistentVolume) {
		t.Helper()
		if pv.Spec.ClaimRef != nil {
			t.Fatalf("claimRef = %+v, want nil", pv.Spec.ClaimRef)
		}
	}
	untouched := func(t *testing.T, pv *corev1.PersistentVolume) {
		t.Helper()
		if pv.Spec.ClaimRef == nil || pv.Spec.ClaimRef.UID != "u-1" {
			t.Fatalf("claimRef = %+v, want unchanged", pv.Spec.ClaimRef)
		}
	}
	tests := []struct {
		name    string
		pv      *corev1.PersistentVolume
		opts    Options
		check   func(*testing.T, *corev1.PersistentVolume)
		patches int
		result  string // metric label expected to be 1 ("" = none)
		event   string // expected event reason prefix ("" = none)
	}{
		{name: "released retain cleared (parity)", pv: newPV(), check: cleared, patches: 1, result: ResultReclaimed, event: ReasonReclaimed},
		{name: "released delete cleared by default", pv: newPV(policy(corev1.PersistentVolumeReclaimDelete)), check: cleared, patches: 1, result: ResultReclaimed, event: ReasonReclaimed},
		{name: "any class cleared by default", pv: newPV(class("nfs")), check: cleared, patches: 1, result: ResultReclaimed, event: ReasonReclaimed},
		{name: "skip annotation ignored by default", pv: newPV(annotate(SkipAnnotation, "true")), check: cleared, patches: 1, result: ResultReclaimed, event: ReasonReclaimed},
		{name: "bound untouched", pv: newPV(phase(corev1.VolumeBound)), check: untouched},
		{name: "available untouched", pv: newPV(phase(corev1.VolumeAvailable)), check: untouched},
		{name: "already clear is a no-op", pv: newPV(noClaim()), check: cleared},
		{name: "retain-only skips delete", pv: newPV(policy(corev1.PersistentVolumeReclaimDelete)), opts: Options{RetainOnly: true}, check: untouched, result: ResultSkipped},
		{name: "retain-only reclaims retain", pv: newPV(), opts: Options{RetainOnly: true}, check: cleared, patches: 1, result: ResultReclaimed, event: ReasonReclaimed},
		{name: "allowlist skips other class", pv: newPV(class("nfs")), opts: Options{StorageClasses: []string{"longhorn"}}, check: untouched, result: ResultSkipped},
		{name: "allowlist reclaims listed class", pv: newPV(), opts: Options{StorageClasses: []string{"x", "longhorn"}}, check: cleared, patches: 1, result: ResultReclaimed, event: ReasonReclaimed},
		{name: "skip annotation honoured", pv: newPV(annotate(SkipAnnotation, "true")), opts: Options{HonorSkipAnnotation: true}, check: untouched, result: ResultSkipped},
		{name: "skip annotation false reclaims", pv: newPV(annotate(SkipAnnotation, "false")), opts: Options{HonorSkipAnnotation: true}, check: cleared, patches: 1, result: ResultReclaimed, event: ReasonReclaimed},
		{
			name: "keep-claim drops uid only", pv: newPV(), opts: Options{Mode: PatchModeKeepClaim}, patches: 1, result: ResultReclaimed, event: ReasonReclaimed,
			check: func(t *testing.T, pv *corev1.PersistentVolume) {
				c := pv.Spec.ClaimRef
				if c == nil || c.Namespace != "apps" || c.Name != "data" || c.UID != "" || c.ResourceVersion != "" {
					t.Fatalf("claimRef = %+v, want apps/data without uid/resourceVersion", c)
				}
			},
		},
		{name: "keep-claim already clear is a no-op", pv: newPV(uidCleared()), opts: Options{Mode: PatchModeKeepClaim}, check: func(*testing.T, *corev1.PersistentVolume) {}},
		{name: "dry-run sends dry-run patch only", pv: newPV(), opts: Options{DryRun: true}, check: untouched, patches: 1, result: ResultDryRun, event: ReasonWouldReclaim},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			patches := 0
			c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(tt.pv).
				WithInterceptorFuncs(interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
					patches++
					po := &client.PatchOptions{}
					po.ApplyOptions(opts)
					if len(po.DryRun) > 0 {
						return nil // the fake client does not implement dryRun=All
					}
					return c.Patch(ctx, obj, p, opts...)
				}}).Build()
			rec := events.NewFakeRecorder(10)
			r := &Reconciler{Reader: c, Client: c, Recorder: rec, Metrics: NewMetrics(nil), Opts: tt.opts}

			if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "pv-1"}}); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			var got corev1.PersistentVolume
			if err := c.Get(context.Background(), types.NamespacedName{Name: "pv-1"}, &got); err != nil {
				t.Fatal(err)
			}
			tt.check(t, &got)
			if patches != tt.patches {
				t.Errorf("patches = %d, want %d", patches, tt.patches)
			}
			for _, res := range []string{ResultReclaimed, ResultDryRun, ResultSkipped, ResultError} {
				want := 0.0
				if res == tt.result {
					want = 1
				}
				if v := testutil.ToFloat64(r.Metrics.Reclaimed.WithLabelValues(res)); v != want {
					t.Errorf("metric %s = %v, want %v", res, v, want)
				}
			}
			select {
			case e := <-rec.Events:
				if tt.event == "" || !strings.Contains(e, " "+tt.event+" ") {
					t.Errorf("event %q, want reason %q", e, tt.event)
				}
			default:
				if tt.event != "" {
					t.Errorf("no event, want %q", tt.event)
				}
			}
		})
	}
}

func TestReconcileIdempotent(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(newPV()).Build()
	r := &Reconciler{Reader: c, Client: c, Recorder: events.NewFakeRecorder(10), Metrics: NewMetrics(nil)}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "pv-1"}}
	for range 2 {
		if _, err := r.Reconcile(context.Background(), req); err != nil {
			t.Fatal(err)
		}
	}
	if v := testutil.ToFloat64(r.Metrics.Reclaimed.WithLabelValues(ResultReclaimed)); v != 1 {
		t.Fatalf("reclaimed = %v, want 1", v)
	}
}

func TestReconcileConflictRequeues(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(newPV()).Build()
	// Reader returns a stale copy, as if the PV was rebound after it was read.
	stale := newPV()
	stale.ResourceVersion = "1"
	reader := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(stale).Build()
	var cur corev1.PersistentVolume
	if err := c.Get(context.Background(), types.NamespacedName{Name: "pv-1"}, &cur); err != nil {
		t.Fatal(err)
	}
	cur.Status.Phase = corev1.VolumeBound
	if err := c.Status().Update(context.Background(), &cur); err != nil {
		t.Fatal(err)
	}
	r := &Reconciler{Reader: reader, Client: c, Recorder: events.NewFakeRecorder(1), Metrics: NewMetrics(nil)}
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "pv-1"}})
	if err != nil || res.RequeueAfter == 0 {
		t.Fatalf("Reconcile = %+v, %v; want requeue without error", res, err)
	}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "pv-1"}, &cur); err != nil {
		t.Fatal(err)
	}
	if cur.Spec.ClaimRef == nil {
		t.Fatal("claimRef cleared on a PV that changed since it was read")
	}
}

func TestReconcileMissingPV(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(scheme(t)).Build()
	r := &Reconciler{Reader: c, Client: c, Recorder: events.NewFakeRecorder(1), Metrics: NewMetrics(nil)}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "gone"}}); err != nil {
		t.Fatalf("Reconcile on missing PV: %v", err)
	}
}

func TestReleasedPredicate(t *testing.T) {
	p := ReleasedPredicate()
	for _, tt := range []struct {
		phase corev1.PersistentVolumePhase
		want  bool
	}{{corev1.VolumeReleased, true}, {corev1.VolumeBound, false}, {corev1.VolumeAvailable, false}, {corev1.VolumeFailed, false}} {
		pv := newPV(phase(tt.phase))
		if got := p.Create(event.CreateEvent{Object: pv}); got != tt.want {
			t.Errorf("create %s = %v, want %v", tt.phase, got, tt.want)
		}
		if got := p.Update(event.UpdateEvent{ObjectOld: newPV(phase(corev1.VolumeBound)), ObjectNew: pv}); got != tt.want {
			t.Errorf("update to %s = %v, want %v", tt.phase, got, tt.want)
		}
	}
}

func TestParsePatchMode(t *testing.T) {
	for in, want := range map[string]PatchMode{"": PatchModeClear, "clear": PatchModeClear, "keep-claim": PatchModeKeepClaim} {
		if got, err := ParsePatchMode(in); err != nil || got != want {
			t.Errorf("ParsePatchMode(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := ParsePatchMode("bogus"); err == nil {
		t.Error("ParsePatchMode(bogus): want error")
	}
}

func TestParseStorageClasses(t *testing.T) {
	got := ParseStorageClasses(" a, ,b,")
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("got %q", got)
	}
	if ParseStorageClasses("") != nil {
		t.Fatal("empty input: want nil")
	}
}

func scheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}
