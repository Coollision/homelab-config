package render

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"sigs.k8s.io/yaml"

	"homelab/storage-operator/internal/config"
	"homelab/storage-operator/internal/shares"
)

// Fixture inputs shared with hack/golden/capture.py.
type cfgFixture struct {
	Namespace string `json:"namespace"`
	LBIP      string `json:"lbIP"`
	Username  string `json:"username"`
	YAML      string `json:"yaml"`
}

// Labels the Python harness pretends the operator Deployment carries.
var testOperatorLabels = map[string]string{
	"app":                       "smb-operator",
	"app.kubernetes.io/part-of": "longhorn-smb-operator",
}

const testOwnerUID = "00000000-0000-0000-0000-00000000beef"

func loadJSON(t *testing.T, name string, v any) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
}

// updateGolden (UPDATE_GOLDEN=1) rewrites the golden files this package owns. The Python capture
// harness is frozen: startup/prestop scripts and Deployments diverged from it on purpose (no
// share-specific pod template, see TestShareChangeKeepsPodTemplate), so Go is now their source.
var updateGolden = os.Getenv("UPDATE_GOLDEN") != ""

func golden(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func writeGolden(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join("testdata", name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func loadConfigs(t *testing.T) map[string]*config.Config {
	t.Helper()
	var raw map[string]cfgFixture
	loadJSON(t, "configs.json", &raw)
	out := map[string]*config.Config{}
	for name, f := range raw {
		c, err := config.Parse(f.YAML, config.Env{LoadBalancerIP: f.LBIP, Username: f.Username, Password: "unused"})
		if err != nil {
			t.Fatalf("config %s: %v", name, err)
		}
		if c.Namespace != f.Namespace {
			t.Fatalf("config %s: namespace %q", name, c.Namespace)
		}
		out[name] = c
	}
	return out
}

func loadShares(t *testing.T) map[string][]shares.Share {
	t.Helper()
	var raw map[string][]map[string]any
	loadJSON(t, "share_fixtures.json", &raw)
	out := map[string][]shares.Share{}
	for name, list := range raw {
		ss := []shares.Share{}
		for _, m := range list {
			s := shares.Share{MountType: shares.MountNFS, Attached: true}
			b, _ := json.Marshal(m)
			if err := json.Unmarshal(b, &s); err != nil {
				t.Fatal(err)
			}
			ss = append(ss, s)
		}
		out[name] = ss
	}
	return out
}

func diffText(t *testing.T, name, got, want string) {
	t.Helper()
	if got == want {
		return
	}
	gl, wl := strings.Split(got, "\n"), strings.Split(want, "\n")
	for i := 0; i < len(gl) || i < len(wl); i++ {
		var g, w string
		if i < len(gl) {
			g = gl[i]
		}
		if i < len(wl) {
			w = wl[i]
		}
		if g != w {
			t.Errorf("%s: first difference at line %d:\n got: %q\nwant: %q", name, i+1, g, w)
			return
		}
	}
	t.Errorf("%s: differs (trailing bytes?) got %d bytes want %d", name, len(got), len(want))
}

func TestGoldenText(t *testing.T) {
	cfgs := loadConfigs(t)
	fixtures := loadShares(t)
	for cn, cfg := range cfgs {
		for fn, ss := range fixtures {
			base := cn + "__" + fn
			t.Run(base, func(t *testing.T) {
				diffText(t, base+".smb.conf", SMBConf(cfg, ss), golden(t, base+".smb.conf"))
				diffText(t, base+".mounts.tsv", MountTable(ss), golden(t, base+".mounts.tsv"))
			})
		}
		// Share-independent since the pod template stopped listing shares: one golden per config.
		t.Run(cn+"/startup", func(t *testing.T) {
			if updateGolden {
				writeGolden(t, cn+".startup.sh", StartupScript(cfg))
			}
			diffText(t, cn+".startup.sh", StartupScript(cfg), golden(t, cn+".startup.sh"))
		})
	}
	if updateGolden {
		writeGolden(t, "prestop.sh", PreStopScript())
	}
	diffText(t, "prestop.sh", PreStopScript(), golden(t, "prestop.sh"))
}

// Deployment goldens only for the fixtures that exercise the pod spec (no shares; NFS plus direct
// volumes). The other fixtures differ only in Deployment-level annotations, which
// TestShareChangeKeepsPodTemplate covers without a file per fixture.
var deploymentGoldenFixtures = map[string]bool{"none": true, "mixed": true}

func TestGoldenDeployment(t *testing.T) {
	cfgs := loadConfigs(t)
	fixtures := loadShares(t)
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for cn, cfg := range cfgs {
		for fn, ss := range fixtures {
			if !deploymentGoldenFixtures[fn] {
				continue
			}
			base := cn + "__" + fn
			t.Run(base, func(t *testing.T) {
				got, err := Deployment(cfg, ss, testOperatorLabels, now)
				if err != nil {
					t.Fatal(err)
				}
				if updateGolden {
					d := got.DeepCopy()
					delete(d.Annotations, AnnotLastUpdated)
					b, _ := json.MarshalIndent(d, "", "  ")
					writeGolden(t, base+".deployment.json", string(b))
				}
				var want appsv1.Deployment
				loadJSON(t, base+".deployment.json", &want)
				if v := got.Annotations[AnnotLastUpdated]; v != "2026-01-02 03:04:05 UTC" {
					t.Errorf("last-updated = %q", v)
				}
				delete(got.Annotations, AnnotLastUpdated)
				if !equality.Semantic.DeepEqual(got, &want) {
					gy, _ := yaml.Marshal(got)
					wy, _ := yaml.Marshal(&want)
					diffText(t, base+".deployment", string(gy), string(wy))
				}
			})
		}
	}
}

func TestGoldenCreatedObjects(t *testing.T) {
	cfgs := loadConfigs(t)
	for cn, cfg := range cfgs {
		t.Run(cn, func(t *testing.T) {
			var svc corev1.Service
			loadJSON(t, cn+"__create_00_service.json", &svc)
			got := Service(cfg, testOperatorLabels, "smb-operator", testOwnerUID)
			if !equality.Semantic.DeepEqual(got, &svc) {
				gy, _ := yaml.Marshal(got)
				wy, _ := yaml.Marshal(&svc)
				diffText(t, "service", string(gy), string(wy))
			}

			labels := ChildLabels(testOperatorLabels, map[string]string{LabelMigration: "true"})
			cases := []struct {
				share   shares.Share
				size    string
				pv, pvc string
			}{
				{shares.Share{Name: "d", LonghornVolumeName: "vol-direct", MountType: shares.MountDirect},
					shares.SizeMi(map[string]any{"spec": map[string]any{"size": "5368709120"}}), "01", "02"},
				{shares.Share{Name: "b", LonghornVolumeName: "vol-badsize", MountType: shares.MountDirect},
					shares.SizeMi(map[string]any{"spec": map[string]any{"size": "abc"}}), "03", "04"},
			}
			for _, c := range cases {
				var pv corev1.PersistentVolume
				loadJSON(t, cn+"__create_"+c.pv+"_pv.json", &pv)
				if g := MigrationPV(c.share, c.size, labels); !equality.Semantic.DeepEqual(g, &pv) {
					gy, _ := yaml.Marshal(g)
					wy, _ := yaml.Marshal(&pv)
					diffText(t, "pv", string(gy), string(wy))
				}
				var pvc corev1.PersistentVolumeClaim
				loadJSON(t, cn+"__create_"+c.pvc+"_pvc.json", &pvc)
				if g := MigrationPVC(c.share, cfg.Namespace, c.size, labels); !equality.Semantic.DeepEqual(g, &pvc) {
					gy, _ := yaml.Marshal(g)
					wy, _ := yaml.Marshal(&pvc)
					diffText(t, "pvc", string(gy), string(wy))
				}
			}
		})
	}
}

// Python sent the config strings verbatim ("2000m"); the apiserver stores the canonical form
// ("2"). Go sends the canonical form directly, so the stored object is identical either way.
func TestResourceQuantityFormatting(t *testing.T) {
	cfgs := loadConfigs(t)
	d, err := Deployment(cfgs["live"], nil, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	r := d.Spec.Template.Spec.Containers[0].Resources
	for k, want := range map[string]string{
		"rc": "200m", "rm": "256Mi", "lc": "2", "lm": "512Mi",
	} {
		var q string
		switch k {
		case "rc":
			q = r.Requests.Cpu().String()
		case "rm":
			q = r.Requests.Memory().String()
		case "lc":
			q = r.Limits.Cpu().String()
		case "lm":
			q = r.Limits.Memory().String()
		}
		if q != want {
			t.Errorf("%s = %s, want %s (apiserver canonical form)", k, q, want)
		}
	}
}

// Adding or removing a share must not change the pod template, or smbd restarts and every SMB
// client loses its session. Only direct (pod-volume) shares may, since kubelet needs the volume.
func TestShareChangeKeepsPodTemplate(t *testing.T) {
	cfg := loadConfigs(t)["live"]
	nfs := func(names ...string) []shares.Share {
		var out []shares.Share
		for _, n := range names {
			out = append(out, shares.Share{Name: n, Path: "/shares/" + n, MountType: shares.MountNFS, Attached: true})
		}
		return out
	}
	now := time.Now()
	base, err := Deployment(cfg, nfs("a", "b"), testOperatorLabels, now)
	if err != nil {
		t.Fatal(err)
	}
	for name, ss := range map[string][]shares.Share{
		"add":    nfs("a", "b", "c"),
		"remove": nfs("a"),
		"empty":  nil,
	} {
		got, err := Deployment(cfg, ss, testOperatorLabels, now)
		if err != nil {
			t.Fatal(err)
		}
		if !equality.Semantic.DeepEqual(base.Spec.Template, got.Spec.Template) {
			gy, _ := yaml.Marshal(got.Spec.Template)
			wy, _ := yaml.Marshal(base.Spec.Template)
			diffText(t, name+" pod template", string(gy), string(wy))
		}
		if got.Annotations[AnnotShareList] == base.Annotations[AnnotShareList] && name != "empty" {
			t.Errorf("%s: Deployment share-list annotation did not change", name)
		}
	}
}

func TestPodTemplateHasNoSubPathConfig(t *testing.T) {
	d, err := Deployment(loadConfigs(t)["live"], nil, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range d.Spec.Template.Spec.Containers[0].VolumeMounts {
		if m.SubPath != "" {
			t.Errorf("mount %s uses subPath %q: ConfigMap updates would never reach the pod", m.Name, m.SubPath)
		}
	}
}
