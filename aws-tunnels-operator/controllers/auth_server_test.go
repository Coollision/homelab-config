package controllers

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestBuildAWSCLIEnv_EmptyProfile(t *testing.T) {
	_, _, err := buildAWSCLIEnv("", "content", []string{"dev"})
	if err == nil || !strings.Contains(err.Error(), "aws profile is required") {
		t.Fatalf("expected profile required error, got: %v", err)
	}
}

func TestBuildAWSCLIEnv_EmptyConfig(t *testing.T) {
	_, _, err := buildAWSCLIEnv("dev", "", []string{"dev"})
	if err == nil || !strings.Contains(err.Error(), "aws config is empty") {
		t.Fatalf("expected config empty error, got: %v", err)
	}
}

func TestBuildAWSCLIEnv_ProfileNotAvailable(t *testing.T) {
	_, _, err := buildAWSCLIEnv("missing-profile", "[profile dev]\nregion = eu-west-1\n", []string{"dev"})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not found error, got: %v", err)
	}
}

func TestBuildAWSCLIEnv_Success(t *testing.T) {
	env, cleanup, err := buildAWSCLIEnv("dev", "[profile dev]\nregion = eu-west-1\n", []string{"dev"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cleanup == nil {
		t.Fatal("expected cleanup function")
	}
	defer cleanup()

	found := false
	for _, e := range env {
		if strings.HasPrefix(e, "AWS_CONFIG_FILE=") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected AWS_CONFIG_FILE in env, got: %v", env)
	}
}

func TestDiscoverTargets_ReturnsTargets(t *testing.T) {
	spec := minimalSpec()
	cm := specConfigMap(t, "default", "aws-tunnels-operator-stack", "aws-tunnels", spec)
	// Pre-create a credentials Secret so the target appears with expiration data.
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "aws-tunnels-creds-dev", Namespace: "default"},
		Data: map[string][]byte{
			"expiration": []byte(time.Now().UTC().Add(1 * time.Hour).Format(time.RFC3339)),
		},
	}
	c := newFakeClient(t, cm, secret)

	s := &AuthServer{
		Client:          c,
		StackNamespace:  "default",
		StackConfigName: "aws-tunnels-operator-stack",
	}

	targets, err := s.discoverTargets(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(targets) == 0 {
		t.Fatal("expected at least one target")
	}
	if targets[0].Profile != "dev" {
		t.Errorf("expected profile dev, got %q", targets[0].Profile)
	}
	if !targets[0].Valid {
		t.Error("expected target to be valid (credential secret has future expiration)")
	}
}

func TestDiscoverTargets_MissingSecret(t *testing.T) {
	cm := specConfigMap(t, "default", "aws-tunnels-operator-stack", "aws-tunnels", minimalSpec())
	c := newFakeClient(t, cm)

	s := &AuthServer{
		Client:          c,
		StackNamespace:  "default",
		StackConfigName: "aws-tunnels-operator-stack",
	}

	targets, err := s.discoverTargets(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(targets) == 0 {
		t.Fatal("expected at least one target")
	}
	// Without a Secret the target should exist but be invalid.
	if targets[0].Valid {
		t.Error("expected target to be invalid when no secret exists")
	}
}

func TestRestartStackTunnels_RestartsTunnels(t *testing.T) {
	cm := specConfigMap(t, "default", "aws-tunnels-operator-stack", "aws-tunnels", minimalSpec())

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "aws-tunnels-db", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "aws-tunnels-db"}},
			},
		},
	}
	c := newFakeClient(t, cm, dep)

	s := &AuthServer{
		Client:          c,
		StackNamespace:  "default",
		StackConfigName: "aws-tunnels-operator-stack",
	}

	restarted, err := s.restartStackTunnels(context.Background(), "default", "aws-tunnels", "dev")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if restarted != 1 {
		t.Errorf("expected 1 restarted tunnel, got %d", restarted)
	}

	// Verify the restartedAt annotation was written.
	updated := &appsv1.Deployment{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "aws-tunnels-db"}, updated); err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	if updated.Spec.Template.Annotations["proxies.homelab.io/restartedAt"] == "" {
		t.Error("expected proxies.homelab.io/restartedAt annotation to be set")
	}
}

func TestTemplatesRenderWithNewFields(t *testing.T) {
	// os.DirFS("..") roots at the project dir, which holds templates/. Mirrors how main.go embeds it.
	s := &AuthServer{TemplateFS: os.DirFS("..")}
	if err := s.loadTemplates(); err != nil {
		t.Fatalf("load templates: %v", err)
	}

	var buf bytes.Buffer
	rootData := authRootPageData{
		Targets:       []loginTarget{{Namespace: "n", Stack: "s", Profile: "dev", Valid: true}},
		Tunnels:       []tunnelStatus{{Name: "db", Desired: 1, Ready: 1}, {Name: "prod", Stopped: true}},
		TokenImported: true,
	}
	if err := s.templates["auth-root"].Execute(&buf, rootData); err != nil {
		t.Fatalf("render auth-root: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "/tunnel-toggle") {
		t.Error("expected a stop/start toggle form in the rendered root page")
	}
	if !strings.Contains(out, "imported") {
		t.Error("expected token-imported status line in the rendered root page")
	}

	buf.Reset()
	waitData := authSSOWaitPageData{SessionID: "sid", Stack: "s", Profile: "dev"}
	if err := s.templates["auth-sso-wait"].Execute(&buf, waitData); err != nil {
		t.Fatalf("render auth-sso-wait: %v", err)
	}
}

func TestHandleTunnelToggle_StopThenStart(t *testing.T) {
	cm := specConfigMap(t, "default", "aws-tunnels-operator-stack", "aws-tunnels", minimalSpec())
	one := int32(1)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "aws-tunnels-db", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &one,
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "aws-tunnels-db"}}},
		},
	}
	c := newFakeClient(t, cm, dep)
	s := &AuthServer{Client: c, StackNamespace: "default", StackConfigName: "aws-tunnels-operator-stack"}

	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/tunnel-toggle", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		s.handleTunnelToggle(rec, req)
		return rec
	}
	get := func() *appsv1.Deployment {
		d := &appsv1.Deployment{}
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "aws-tunnels-db"}, d); err != nil {
			t.Fatalf("get deployment: %v", err)
		}
		return d
	}

	// Stop: pins replicas to 0 and sets the manuallyStopped annotation.
	if rec := post("tunnel=db&action=stop"); rec.Code != http.StatusSeeOther {
		t.Fatalf("stop: expected redirect, got %d: %s", rec.Code, rec.Body.String())
	}
	stopped := get()
	if stopped.Annotations[annotManuallyStopped] != "true" {
		t.Errorf("expected manuallyStopped=true, got %q", stopped.Annotations[annotManuallyStopped])
	}
	if stopped.Spec.Replicas == nil || *stopped.Spec.Replicas != 0 {
		t.Errorf("expected replicas=0 after stop")
	}

	// Start: clears the annotation. No valid creds Secret exists, so it stays at 0 and lets the
	// reconcile loop bring it up once creds land.
	if rec := post("tunnel=db&action=start"); rec.Code != http.StatusSeeOther {
		t.Fatalf("start: expected redirect, got %d: %s", rec.Code, rec.Body.String())
	}
	started := get()
	if _, ok := started.Annotations[annotManuallyStopped]; ok {
		t.Errorf("expected manuallyStopped annotation cleared after start")
	}
}

func TestHandleTunnelToggle_UnknownTunnel(t *testing.T) {
	cm := specConfigMap(t, "default", "aws-tunnels-operator-stack", "aws-tunnels", minimalSpec())
	c := newFakeClient(t, cm)
	s := &AuthServer{Client: c, StackNamespace: "default", StackConfigName: "aws-tunnels-operator-stack"}

	req := httptest.NewRequest(http.MethodPost, "/tunnel-toggle", strings.NewReader("tunnel=ghost&action=stop"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handleTunnelToggle(rec, req)
	// Form-mode errors redirect to /?err=...
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected redirect for unknown tunnel, got %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "err=") {
		t.Errorf("expected error redirect, got %q", loc)
	}
}

func TestRestartStackTunnels_ProfileFilter(t *testing.T) {
	// Tunnel uses the default profile "dev"; filtering by "other" should restart nothing.
	cm := specConfigMap(t, "default", "aws-tunnels-operator-stack", "aws-tunnels", minimalSpec())
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "aws-tunnels-db", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "aws-tunnels-db"}},
			},
		},
	}
	c := newFakeClient(t, cm, dep)

	s := &AuthServer{
		Client:          c,
		StackNamespace:  "default",
		StackConfigName: "aws-tunnels-operator-stack",
	}

	restarted, err := s.restartStackTunnels(context.Background(), "default", "aws-tunnels", "other")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if restarted != 0 {
		t.Errorf("expected 0 restarted tunnels for non-matching profile, got %d", restarted)
	}
}
