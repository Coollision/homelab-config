package controllers

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestTokenSecretName(t *testing.T) {
	cases := map[string]string{
		"aws-tunnels": "aws-tunnels-sso-token",
		"My Stack":    "my_stack-sso-token",
	}
	for in, want := range cases {
		if got := tokenSecretName(in); got != want {
			t.Errorf("tokenSecretName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCredsNeedRefresh(t *testing.T) {
	mk := func(d time.Duration) *corev1.Secret {
		return &corev1.Secret{Data: map[string][]byte{"expiration": []byte(time.Now().UTC().Add(d).Format(time.RFC3339))}}
	}
	if !credsNeedRefresh(nil, time.Minute) {
		t.Error("nil secret should need refresh")
	}
	if credsNeedRefresh(mk(1*time.Hour), 10*time.Minute) {
		t.Error("far-future creds should not need refresh")
	}
	if !credsNeedRefresh(mk(5*time.Minute), 10*time.Minute) {
		t.Error("creds within the ahead window should need refresh")
	}
	if !credsNeedRefresh(mk(-1*time.Minute), time.Minute) {
		t.Error("expired creds should need refresh")
	}
	if !credsNeedRefresh(&corev1.Secret{Data: map[string][]byte{"expiration": []byte("nope")}}, time.Minute) {
		t.Error("unparseable expiration should need refresh")
	}
}

func TestSecretDataEqual(t *testing.T) {
	a := map[string][]byte{"x": []byte("1"), "y": []byte("2")}
	if !secretDataEqual(a, map[string][]byte{"x": []byte("1"), "y": []byte("2")}) {
		t.Error("identical maps should be equal")
	}
	if secretDataEqual(a, map[string][]byte{"x": []byte("1")}) {
		t.Error("different lengths should not be equal")
	}
	if secretDataEqual(a, map[string][]byte{"x": []byte("1"), "y": []byte("3")}) {
		t.Error("different values should not be equal")
	}
}

func TestSeedAndPersistTokenCache(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: tokenSecretName("aws-tunnels"), Namespace: "default"},
		Data:       map[string][]byte{"abc.json": []byte(`{"accessToken":"x"}`)},
	}
	c := newFakeClient(t, secret)

	seeded, err := seedTokenCache(context.Background(), c, "default", "aws-tunnels")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if !seeded {
		t.Fatal("expected seeded=true")
	}
	got, err := os.ReadFile(filepath.Join(ssoCacheDir(), "abc.json"))
	if err != nil {
		t.Fatalf("read seeded file: %v", err)
	}
	if string(got) != `{"accessToken":"x"}` {
		t.Errorf("unexpected seeded content: %s", got)
	}

	// Simulate the CLI rotating the refresh token on disk, then persist it back to the Secret.
	if err := os.WriteFile(filepath.Join(ssoCacheDir(), "abc.json"), []byte(`{"accessToken":"rotated"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := persistTokenCache(context.Background(), c, "default", "aws-tunnels"); err != nil {
		t.Fatalf("persist: %v", err)
	}
	updated := &corev1.Secret{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: tokenSecretName("aws-tunnels")}, updated); err != nil {
		t.Fatalf("get updated secret: %v", err)
	}
	if string(updated.Data["abc.json"]) != `{"accessToken":"rotated"}` {
		t.Errorf("expected rotated content persisted, got %s", updated.Data["abc.json"])
	}
}

func TestSeedTokenCache_NoSecret(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c := newFakeClient(t)
	seeded, err := seedTokenCache(context.Background(), c, "default", "aws-tunnels")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if seeded {
		t.Error("expected seeded=false when no token secret exists")
	}
}
