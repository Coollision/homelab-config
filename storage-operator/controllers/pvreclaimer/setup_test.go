package pvreclaimer

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	toolscache "k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache/informertest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllertest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

func TestReconcilePatchErrorIsWrappedCountedAndEvented(t *testing.T) {
	boom := apierrors.NewForbidden(schema.GroupResource{Resource: "persistentvolumes"}, "pv-1", errors.New("denied"))
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(newPV()).
		WithInterceptorFuncs(interceptor.Funcs{Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
			return boom
		}}).Build()
	rec := events.NewFakeRecorder(10)
	r := &Reconciler{Reader: c, Client: c, Recorder: rec, Metrics: NewMetrics(nil), Opts: Options{Mode: PatchModeClear}}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "pv-1"}})
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "pv-1") {
		t.Fatalf("err = %v, want wrapped %v naming the PV", err, boom)
	}
	if v := testutil.ToFloat64(r.Metrics.Reclaimed.WithLabelValues(ResultError)); v != 1 {
		t.Errorf("error metric = %v, want 1", v)
	}
	select {
	case e := <-rec.Events:
		if !strings.HasPrefix(e, corev1.EventTypeWarning+" "+ReasonFailed) {
			t.Errorf("event = %q, want Warning %s", e, ReasonFailed)
		}
	default:
		t.Error("no ReclaimFailed event")
	}
}

func TestSetupWithManager(t *testing.T) {
	// No API server is contacted until the manager starts.
	mgr, err := ctrl.NewManager(&rest.Config{Host: "http://127.0.0.1:1"}, ctrl.Options{
		Scheme: scheme(t), Metrics: metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := SetupWithManager(mgr, Options{Mode: "bogus"}); err == nil {
		t.Fatal("invalid patch mode: want error")
	}
	r, err := SetupWithManager(mgr, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Opts.Mode != PatchModeClear {
		t.Errorf("Mode = %q, want normalised %q", r.Opts.Mode, PatchModeClear)
	}
}

func TestReadyzCheckLatchesOncePVInformerSynced(t *testing.T) {
	inf := controllertest.NewFakeInformer()
	informers := &informertest.FakeInformers{
		Scheme:         scheme(t),
		InformersByGVK: map[schema.GroupVersionKind]toolscache.SharedIndexInformer{corev1.SchemeGroupVersion.WithKind("PersistentVolume"): inf},
	}
	r := &Reconciler{}
	check := r.readyzCheck(informers)
	req := httptest.NewRequest("GET", "/readyz", nil)

	if err := check(req); err == nil || r.Ready() {
		t.Fatalf("unsynced informer: err = %v, ready = %v; want error, not ready", err, r.Ready())
	}
	inf.Synced()
	if err := check(req); err != nil || !r.Ready() {
		t.Fatalf("synced informer: err = %v, ready = %v", err, r.Ready())
	}
	informers.Error = errors.New("gone")
	if err := check(req); err != nil {
		t.Fatalf("readiness must stay latched: %v", err)
	}
}
