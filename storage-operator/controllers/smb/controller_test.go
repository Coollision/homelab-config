package smb

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"homelab/storage-operator/internal/config"
	"homelab/storage-operator/internal/render"
	"homelab/storage-operator/internal/shares"
)

var volGVK = schema.GroupVersionKind{Group: "longhorn.io", Version: "v1beta2", Kind: "Volume"}

const testdata = "../../internal/render/testdata"

func newScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	s.AddKnownTypeWithName(volGVK, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(volGVK.GroupVersion().WithKind("VolumeList"), &unstructured.UnstructuredList{})
	return s
}

func newMapper(s *runtime.Scheme) meta.RESTMapper {
	m := meta.NewDefaultRESTMapper(nil)
	for gvk := range s.AllKnownTypes() {
		scope := meta.RESTScopeNamespace
		if gvk.Kind == "PersistentVolume" || gvk.Kind == "Namespace" || gvk.Kind == "Node" {
			scope = meta.RESTScopeRoot
		}
		m.Add(gvk, scope)
	}
	return m
}

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	c, err := config.Parse("operator:\n  namespace: storage\nsmb:\n  image: crazymax/samba:latest\n",
		config.Env{LoadBalancerIP: "192.0.2.10", Username: "smbuser", Password: "pw"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func volume(name, ns string, spec, status map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{"spec": spec, "status": status}}
	u.SetGroupVersionKind(volGVK)
	u.SetName(name)
	u.SetNamespace(ns)
	return u
}

func operatorDeployment() *appsv1.Deployment {
	return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name: "smb-operator", Namespace: "storage", UID: "op-uid",
		Labels: map[string]string{"app": "smb-operator", "app.kubernetes.io/part-of": "longhorn-smb-operator"},
	}}
}

func newReconciler(t *testing.T, cfg *config.Config, objs ...client.Object) (*Reconciler, client.Client) {
	t.Helper()
	s := newScheme()
	c := fake.NewClientBuilder().WithScheme(s).WithRESTMapper(newMapper(s)).WithObjects(objs...).Build()
	return &Reconciler{
		Reader: c, Writer: c, Metrics: NewMetrics(nil), VolumeGVK: volGVK,
		Opts: Options{Config: cfg, OwnerDeployment: "smb-operator"},
		Now:  func() time.Time { return time.Unix(1700000000, 0) },
	}, c
}

// ---------------------------------------------------------------------------------------------
// Discovery parity with the Python capture (discovery__*.json).

type discoveryInput struct {
	PVCs []struct {
		Namespace  string            `json:"namespace"`
		Name       string            `json:"name"`
		Labels     map[string]string `json:"labels"`
		VolumeName string            `json:"volumeName"`
	} `json:"pvcs"`
	Volumes map[string]struct {
		Namespace string         `json:"namespace"`
		Spec      map[string]any `json:"spec"`
		Status    map[string]any `json:"status"`
	} `json:"volumes"`
	Migration any `json:"migration"`
}

type pyShare struct {
	shares.Share
	StructuralKey string `json:"structural_key"`
	Endpoint      string `json:"endpoint"`
}

type pyEvent struct {
	Namespace, Kind, Name, Reason, Message, Type, Component string
}

type discoveryGolden struct {
	Shares      []pyShare                    `json:"shares"`
	Annotations map[string]map[string]string `json:"annotations"`
	Events      []pyEvent                    `json:"events"`
}

func TestDiscoveryParityWithPython(t *testing.T) {
	var inputs map[string]discoveryInput
	b, err := os.ReadFile(filepath.Join(testdata, "discovery_input.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &inputs); err != nil {
		t.Fatal(err)
	}
	for name, in := range inputs {
		t.Run(name, func(t *testing.T) {
			var want discoveryGolden
			gb, err := os.ReadFile(filepath.Join(testdata, "discovery__"+name+".json"))
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(gb, &want); err != nil {
				t.Fatal(err)
			}
			var objs []client.Object
			for _, p := range in.PVCs {
				objs = append(objs, &corev1.PersistentVolumeClaim{
					ObjectMeta: metav1.ObjectMeta{Namespace: p.Namespace, Name: p.Name, Labels: p.Labels},
					Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: p.VolumeName},
				})
			}
			for vn, v := range in.Volumes {
				ns := v.Namespace
				if ns == "" {
					ns = "storage"
				}
				objs = append(objs, volume(vn, ns, v.Spec, v.Status))
			}
			if in.Migration != nil {
				raw, _ := json.Marshal(in.Migration)
				objs = append(objs, &corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{Namespace: "storage", Name: MigrationConfigMap},
					Data:       map[string]string{MigrationKey: string(raw)},
				})
			}
			r, c := newReconciler(t, testConfig(t), objs...)
			ctx := context.Background()
			got, err := r.DiscoverShares(ctx, logr.Discard())
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, r.DiscoverMigrationShares(ctx, logr.Discard())...)

			// Shares (order: the API returns PVCs sorted, the Python fake used input order).
			var gotPy []pyShare
			for _, s := range got {
				gotPy = append(gotPy, pyShare{s, s.StructuralKey(), s.Endpoint()})
			}
			byID := func(l []pyShare) {
				sort.Slice(l, func(i, j int) bool { return l[i].StructuralKey < l[j].StructuralKey })
			}
			byID(gotPy)
			byID(want.Shares)
			if (len(gotPy) > 0 || len(want.Shares) > 0) && !reflect.DeepEqual(gotPy, want.Shares) {
				t.Errorf("shares:\n got %+v\nwant %+v", gotPy, want.Shares)
			}

			// Annotations.
			gotAnn := map[string]map[string]string{}
			var pvcs corev1.PersistentVolumeClaimList
			_ = c.List(ctx, &pvcs)
			for _, p := range pvcs.Items {
				if a := p.Annotations; a != nil {
					if a[render.AnnotPVCLastUpdate] != "2023-11-14 22:13:20 UTC" {
						t.Errorf("last-update = %q", a[render.AnnotPVCLastUpdate])
					}
					gotAnn[p.Namespace+"/"+p.Name] = map[string]string{
						render.AnnotPVCStatus: a[render.AnnotPVCStatus], render.AnnotPVCSharePath: a[render.AnnotPVCSharePath],
					}
				}
			}
			if want.Annotations == nil {
				want.Annotations = map[string]map[string]string{}
			}
			if !reflect.DeepEqual(gotAnn, want.Annotations) {
				t.Errorf("annotations:\n got %v\nwant %v", gotAnn, want.Annotations)
			}

			// Events.
			var evs corev1.EventList
			_ = c.List(ctx, &evs)
			var gotEv []pyEvent
			for _, e := range evs.Items {
				if !strings.HasSuffix(e.Name, ".1700000000") || e.Count != 1 {
					t.Errorf("event name/count %s %d", e.Name, e.Count)
				}
				gotEv = append(gotEv, pyEvent{e.Namespace, e.InvolvedObject.Kind, e.InvolvedObject.Name, e.Reason, e.Message, e.Type, e.Source.Component})
			}
			sortEv := func(l []pyEvent) {
				sort.Slice(l, func(i, j int) bool { return l[i].Name+l[i].Reason < l[j].Name+l[j].Reason })
			}
			sortEv(gotEv)
			sortEv(want.Events)
			if len(gotEv) != 0 || len(want.Events) != 0 {
				if !reflect.DeepEqual(gotEv, want.Events) {
					t.Errorf("events:\n got %+v\nwant %+v", gotEv, want.Events)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------------------------
// Full reconcile flows.

func rwxPVC(ns, name, vol string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: map[string]string{"smb-access": "shared"}},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: vol},
	}
}

func getCM(t *testing.T, c client.Client, name string) *corev1.ConfigMap {
	t.Helper()
	var cm corev1.ConfigMap
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "storage", Name: name}, &cm); err != nil {
		t.Fatalf("get %s: %v", name, err)
	}
	return &cm
}

func getDep(t *testing.T, c client.Client) *appsv1.Deployment {
	t.Helper()
	var d appsv1.Deployment
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "storage", Name: "smb-server"}, &d); err != nil {
		t.Fatal(err)
	}
	return &d
}

func TestReconcileZeroSharesFreshStartIsNoop(t *testing.T) {
	// Python: current_structure starts as an empty set, so zero shares == no change.
	r, c := newReconciler(t, testConfig(t), operatorDeployment())
	if !r.ReconcileOnce(context.Background()) {
		t.Fatal("reconcile failed")
	}
	var d appsv1.Deployment
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "storage", Name: "smb-server"}, &d); err == nil {
		t.Fatal("no shares on a fresh start must not create the Deployment")
	}
	getCM(t, c, render.MountTableConfigMap) // the mount table is always written
}

func TestReconcileCreateThenDetachOnlyTouchesMountTable(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t)
	vol := volume("pvc-a", "storage", map[string]any{"accessMode": "rwx"}, map[string]any{"shareEndpoint": "nfs://192.0.2.31/pvc-a"})
	r, c := newReconciler(t, cfg, operatorDeployment(), rwxPVC("apps", "media", "pvc-a"), vol)

	if !r.ReconcileOnce(ctx) {
		t.Fatal("first reconcile failed")
	}
	d := getDep(t, c)
	if d.Labels["app"] != "smb-server" || d.Labels["managed-by"] != "smb-operator" || d.Labels["app.kubernetes.io/part-of"] != "longhorn-smb-operator" {
		t.Errorf("deployment labels %v", d.Labels)
	}
	tmpl := d.Spec.Template.DeepCopy()
	if !strings.Contains(getCM(t, c, render.MountTableConfigMap).Data[render.MountTableKey], "media\t/shares/media\t192.0.2.31:/pvc-a\n") {
		t.Error("mount table missing endpoint")
	}
	if !strings.Contains(getCM(t, c, render.SMBConfigMap).Data[render.SMBConfKey], "[media]") {
		t.Error("smb.conf missing share")
	}
	var svc corev1.Service
	if err := c.Get(ctx, types.NamespacedName{Namespace: "storage", Name: "smb-server"}, &svc); err != nil {
		t.Fatal(err)
	}
	if len(svc.OwnerReferences) != 1 || svc.OwnerReferences[0].UID != "op-uid" || svc.OwnerReferences[0].Name != "smb-operator" ||
		svc.Spec.LoadBalancerIP != "192.0.2.10" {
		t.Errorf("service %+v", svc)
	}

	// Detach: the share-manager goes away.
	var u unstructured.Unstructured
	u.SetGroupVersionKind(volGVK)
	_ = c.Get(ctx, types.NamespacedName{Namespace: "storage", Name: "pvc-a"}, &u)
	_ = unstructured.SetNestedField(u.Object, "", "status", "shareEndpoint")
	if err := c.Update(ctx, &u); err != nil {
		t.Fatal(err)
	}
	rv := getDep(t, c).ResourceVersion
	if !r.ReconcileOnce(ctx) {
		t.Fatal("second reconcile failed")
	}
	if got := getDep(t, c); got.ResourceVersion != rv || !reflect.DeepEqual(got.Spec.Template, *tmpl) {
		t.Error("detach must not touch the Deployment")
	}
	if !strings.Contains(getCM(t, c, render.MountTableConfigMap).Data[render.MountTableKey], "media\t/shares/media\t\n") {
		t.Error("mount table should hold an empty endpoint while detached")
	}
	var pvc corev1.PersistentVolumeClaim
	_ = c.Get(ctx, types.NamespacedName{Namespace: "apps", Name: "media"}, &pvc)
	if pvc.Annotations[render.AnnotPVCStatus] != "detached" {
		t.Errorf("status annotation %q", pvc.Annotations[render.AnnotPVCStatus])
	}
	if v := metricValue(r.Metrics.ReconcileErrors); v != 0 {
		t.Errorf("errors = %v", v)
	}
}

func TestReconcileAfterRestartReappliesWithoutTemplateChange(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t)
	vol := volume("pvc-a", "storage", map[string]any{"accessMode": "rwx"}, map[string]any{"shareEndpoint": "nfs://192.0.2.31/pvc-a"})
	r, c := newReconciler(t, cfg, operatorDeployment(), rwxPVC("apps", "media", "pvc-a"), vol)
	if !r.ReconcileOnce(ctx) {
		t.Fatal()
	}
	before := getDep(t, c).Spec.Template
	// New process: in-memory structure is empty again.
	r2 := &Reconciler{Reader: c, Writer: c, Metrics: NewMetrics(nil), VolumeGVK: volGVK, Opts: r.Opts,
		Now: func() time.Time { return time.Unix(1800000000, 0) }}
	if !r2.ReconcileOnce(ctx) {
		t.Fatal()
	}
	after := getDep(t, c)
	if !reflect.DeepEqual(before, after.Spec.Template) {
		t.Error("restart re-apply must not change the pod template")
	}
	if after.Annotations[render.AnnotLastUpdated] != "2027-01-15 08:00:00 UTC" {
		t.Errorf("deployment-level last-updated = %q", after.Annotations[render.AnnotLastUpdated])
	}
}

func TestMigrationDirectCreateAndGC(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t)
	mig := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "storage", Name: MigrationConfigMap},
		Data:       map[string]string{MigrationKey: "volumes:\n  - name: vol-rwo\n    smb: ro\n"},
	}
	orphanLabels := map[string]string{render.LabelMigration: "true"}
	r, c := newReconciler(t, cfg, operatorDeployment(), mig,
		volume("vol-rwo", "storage", map[string]any{"accessMode": "rwo", "size": "2147483648"}, map[string]any{}),
		&corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "migrate-old", Labels: orphanLabels}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "storage", Name: "migrate-old", Labels: orphanLabels}},
	)
	if !r.ReconcileOnce(ctx) {
		t.Fatal()
	}
	var pv corev1.PersistentVolume
	if err := c.Get(ctx, types.NamespacedName{Name: "migrate-vol-rwo"}, &pv); err != nil {
		t.Fatal(err)
	}
	if pv.Spec.Capacity.Storage().String() != "2Gi" || pv.Spec.CSI.VolumeHandle != "vol-rwo" {
		t.Errorf("pv %+v", pv.Spec)
	}
	var pvc corev1.PersistentVolumeClaim
	if err := c.Get(ctx, types.NamespacedName{Namespace: "storage", Name: "migrate-vol-rwo"}, &pvc); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, types.NamespacedName{Name: "migrate-old"}, &pv); err == nil {
		t.Error("orphan PV not deleted")
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "storage", Name: "migrate-old"}, &pvc); err == nil {
		t.Error("orphan PVC not deleted")
	}
	d := getDep(t, c)
	vols := d.Spec.Template.Spec.Volumes
	if len(vols) != 3 || vols[2].PersistentVolumeClaim == nil || !vols[2].PersistentVolumeClaim.ReadOnly {
		t.Errorf("volumes %+v", vols)
	}
	if strings.Contains(getCM(t, c, render.MountTableConfigMap).Data[render.MountTableKey], "vol-rwo") {
		t.Error("direct shares must not be in the mount table")
	}
}

func TestNeedsReplace(t *testing.T) {
	cfg := testConfig(t)
	nfs := shares.New("a", "apps", "a", shares.AccessShared)
	direct := shares.New("b", "storage", "migration-b", shares.AccessShared)
	direct.MountType, direct.LonghornVolumeName = shares.MountDirect, "b"
	d1, _ := render.Deployment(cfg, []shares.Share{nfs}, nil, time.Now())
	d2, _ := render.Deployment(cfg, []shares.Share{nfs, direct}, nil, time.Now())
	d3, _ := render.Deployment(cfg, []shares.Share{nfs, shares.New("c", "x", "c", shares.AccessReadOnly)}, nil, time.Now())
	if NeedsReplace(d1, d1) || NeedsReplace(d1, d3) {
		t.Error("nfs-only changes are patches")
	}
	if !NeedsReplace(d1, d2) || !NeedsReplace(d2, d1) {
		t.Error("direct volume add/remove needs replace")
	}
	rolling := d1.DeepCopy()
	rolling.Spec.Strategy.Type = appsv1.RollingUpdateDeploymentStrategyType
	if !NeedsReplace(rolling, d1) {
		t.Error("strategy change needs replace")
	}
}

func TestStrategicPatchBodyDropsZeroNoise(t *testing.T) {
	d, _ := render.Deployment(testConfig(t), nil, nil, time.Now())
	b, err := StrategicPatchBody(d)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if strings.Contains(s, "creationTimestamp") || strings.Contains(s, `"status"`) {
		t.Errorf("patch body has zero-value noise: %s", s)
	}
}

func TestPredicates(t *testing.T) {
	p := SMBPVCPredicate()
	if !p.Create(event.CreateEvent{Object: rwxPVC("a", "b", "c")}) {
		t.Error("labelled PVC")
	}
	if p.Create(event.CreateEvent{Object: &corev1.PersistentVolumeClaim{}}) {
		t.Error("unlabelled PVC")
	}
	m := MigrationConfigMapPredicate("storage")
	if !m.Update(event.UpdateEvent{ObjectNew: &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "storage", Name: MigrationConfigMap}}}) ||
		m.Update(event.UpdateEvent{ObjectNew: &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "storage", Name: "other"}}}) {
		t.Error("migration cm predicate")
	}
	v := VolumeStatusPredicate("storage")
	old := volume("v", "storage", nil, map[string]any{"state": "attached", "robustness": "healthy"})
	noise := volume("v", "storage", nil, map[string]any{"state": "attached", "robustness": "degraded"})
	det := volume("v", "storage", nil, map[string]any{"state": "detached"})
	other := volume("v", "elsewhere", nil, map[string]any{"state": "detached"})
	if v.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: noise}) {
		t.Error("irrelevant status change must not trigger")
	}
	if !v.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: det}) {
		t.Error("state change must trigger")
	}
	if v.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: other}) || v.Create(event.CreateEvent{Object: old}) {
		t.Error("other namespace / create must not trigger")
	}
}

func TestReconcileMetricsAndReadiness(t *testing.T) {
	r, _ := newReconciler(t, testConfig(t), operatorDeployment())
	if r.Ready() {
		t.Fatal("not ready before first reconcile")
	}
	res, err := r.Reconcile(context.Background(), reconcileRequest())
	if err != nil || res.RequeueAfter != 30*time.Second {
		t.Fatalf("res=%v err=%v", res, err)
	}
	if !r.Ready() || metricValue(r.Metrics.ReconcileTotal) != 1 {
		t.Error("ready + reconcile_total after success")
	}
}

func reconcileRequest() reconcile.Request {
	return reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "storage", Name: "smb-server"}}
}

func metricValue(c prometheus.Metric) float64 {
	var m dto.Metric
	_ = c.Write(&m)
	if m.Counter != nil {
		return m.Counter.GetValue()
	}
	return m.Gauge.GetValue()
}
