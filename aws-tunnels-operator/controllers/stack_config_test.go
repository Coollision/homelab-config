package controllers

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// newFakeClient builds an in-memory controller-runtime client with all standard
// Kubernetes types registered.
func newFakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

// specConfigMap builds a ConfigMap containing a serialised AWSTunnelStackSpec.
func specConfigMap(t *testing.T, ns, name, stackName string, spec AWSTunnelStackSpec) *corev1.ConfigMap {
	t.Helper()
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Data: map[string]string{
			stackNameKey:     stackName,
			stackSpecJSONKey: string(raw),
		},
	}
}

// minimalSpec returns an AWSTunnelStackSpec that satisfies all validation checks.
func minimalSpec() AWSTunnelStackSpec {
	return AWSTunnelStackSpec{
		AWS: AWSSpec{
			Profile:     "dev",
			Region:      "eu-west-1",
			SSOStartURL: "https://sso.example.com",
			AccountID:   "123456789012",
			RoleName:    "DevRole",
		},
		Tunnels: []TunnelSpec{
			{Name: "db", RemotePort: "5432"},
		},
	}
}

func TestLoadStackConfig_Success(t *testing.T) {
	cm := specConfigMap(t, "default", "aws-tunnels-operator-stack", "aws-tunnels", minimalSpec())
	c := newFakeClient(t, cm)

	cfg, err := LoadStackConfig(context.Background(), c, "default", "aws-tunnels-operator-stack")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Name != "aws-tunnels" {
		t.Errorf("expected name aws-tunnels, got %q", cfg.Name)
	}
	if cfg.Namespace != "default" {
		t.Errorf("expected namespace default, got %q", cfg.Namespace)
	}
}

func TestLoadStackConfig_MissingConfigMap(t *testing.T) {
	c := newFakeClient(t)
	_, err := LoadStackConfig(context.Background(), c, "default", "missing")
	if err == nil {
		t.Fatal("expected error for missing configmap")
	}
}

func TestLoadStackConfig_EmptyNamespace(t *testing.T) {
	c := newFakeClient(t)
	_, err := LoadStackConfig(context.Background(), c, "", "any")
	if err == nil || !strings.Contains(err.Error(), "namespace is required") {
		t.Fatalf("expected namespace error, got: %v", err)
	}
}

func TestLoadStackConfig_NoTunnels(t *testing.T) {
	spec := minimalSpec()
	spec.Tunnels = nil
	cm := specConfigMap(t, "default", "aws-tunnels-operator-stack", "aws-tunnels", spec)
	c := newFakeClient(t, cm)

	_, err := LoadStackConfig(context.Background(), c, "default", "aws-tunnels-operator-stack")
	if err == nil || !strings.Contains(err.Error(), "no tunnels") {
		t.Fatalf("expected 'no tunnels' error, got: %v", err)
	}
}

func TestAWSConfigMapName_Custom(t *testing.T) {
	cfg := StackConfig{
		Name: "my-stack",
		Spec: AWSTunnelStackSpec{
			Shared: SharedNamesSpec{AWSConfigMapName: "custom-aws-config"},
		},
	}
	if got := cfg.AWSConfigMapName(); got != "custom-aws-config" {
		t.Errorf("expected custom-aws-config, got %q", got)
	}
}

func TestAWSConfigMapName_Default(t *testing.T) {
	cfg := StackConfig{Name: "my-stack"}
	if got := cfg.AWSConfigMapName(); got != "my-stack-aws-config" {
		t.Errorf("expected my-stack-aws-config, got %q", got)
	}
}

func TestDefinedAWSProfiles(t *testing.T) {
	cfg := StackConfig{Spec: AWSTunnelStackSpec{
		AWS: AWSSpec{
			Profile: "main",
			ExtraProfile: []AWSProfileSpec{
				{Name: "staging"},
			},
		},
	}}
	profiles := cfg.DefinedAWSProfiles()
	if len(profiles) != 2 {
		t.Fatalf("expected 2 profiles, got %d: %v", len(profiles), profiles)
	}
}

func TestReferencedAWSProfiles_IncludesTunnelOverride(t *testing.T) {
	cfg := StackConfig{Spec: AWSTunnelStackSpec{
		AWS:     AWSSpec{Profile: "main"},
		Tunnels: []TunnelSpec{{Name: "db", AWSProfile: "svc-account"}},
	}}
	refs := cfg.ReferencedAWSProfiles()
	found := false
	for _, r := range refs {
		if r == "svc-account" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected svc-account in referenced profiles, got: %v", refs)
	}
}

func TestRenderAWSConfig_Success(t *testing.T) {
	cfg := StackConfig{Spec: AWSTunnelStackSpec{
		AWS: AWSSpec{
			Profile:     "dev",
			Region:      "eu-west-1",
			SSOStartURL: "https://sso.example.com",
			AccountID:   "123456789012",
			RoleName:    "DevRole",
		},
	}}
	text, profiles, err := cfg.RenderAWSConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(text, "[profile dev]") {
		t.Errorf("expected [profile dev] in config text, got: %s", text)
	}
	if len(profiles) != 1 || profiles[0] != "dev" {
		t.Errorf("unexpected profiles: %v", profiles)
	}
}

func TestRenderAWSConfig_DefaultRegion(t *testing.T) {
	cfg := StackConfig{Spec: AWSTunnelStackSpec{
		AWS: AWSSpec{
			Profile:     "dev",
			SSOStartURL: "https://sso.example.com",
			AccountID:   "123456789012",
			RoleName:    "DevRole",
			// Region intentionally empty — should default to eu-west-1.
		},
	}}
	text, _, err := cfg.RenderAWSConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(text, "region = eu-west-1") {
		t.Errorf("expected default region in config text, got: %s", text)
	}
}

func TestRenderAWSConfig_MissingFields(t *testing.T) {
	cfg := StackConfig{Spec: AWSTunnelStackSpec{
		AWS: AWSSpec{Profile: "dev"},
	}}
	_, _, err := cfg.RenderAWSConfig()
	if err == nil {
		t.Fatal("expected error for missing SSO fields")
	}
}

func TestRenderAWSConfig_NoProfile(t *testing.T) {
	cfg := StackConfig{}
	_, _, err := cfg.RenderAWSConfig()
	if err == nil || !strings.Contains(err.Error(), "no aws profiles") {
		t.Fatalf("expected 'no aws profiles' error, got: %v", err)
	}
}

func TestSyncAWSConfigMap_Creates(t *testing.T) {
	cm := specConfigMap(t, "default", "aws-tunnels-operator-stack", "aws-tunnels", minimalSpec())
	c := newFakeClient(t, cm)

	cfg, err := LoadStackConfig(context.Background(), c, "default", "aws-tunnels-operator-stack")
	if err != nil {
		t.Fatal(err)
	}

	mapName, text, _, err := SyncAWSConfigMap(context.Background(), c, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mapName == "" {
		t.Error("expected non-empty configmap name")
	}
	if !strings.Contains(text, "[profile dev]") {
		t.Errorf("expected config text to contain profile block, got: %s", text)
	}
}

func TestSyncAWSConfigMap_Updates(t *testing.T) {
	cm := specConfigMap(t, "default", "aws-tunnels-operator-stack", "aws-tunnels", minimalSpec())
	// Pre-create the AWS config ConfigMap so SyncAWSConfigMap takes the update path.
	existingAwsCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "aws-tunnels-aws-config", Namespace: "default"},
		Data:       map[string]string{awsConfigDataKey: "old-content"},
	}
	c := newFakeClient(t, cm, existingAwsCM)

	cfg, err := LoadStackConfig(context.Background(), c, "default", "aws-tunnels-operator-stack")
	if err != nil {
		t.Fatal(err)
	}

	_, text, _, err := SyncAWSConfigMap(context.Background(), c, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(text, "[profile dev]") {
		t.Errorf("expected updated config text, got: %s", text)
	}
}
