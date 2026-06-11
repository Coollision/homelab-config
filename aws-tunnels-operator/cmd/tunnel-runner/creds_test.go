package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeCredFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}

func TestSecretFileCredentials_Retrieve(t *testing.T) {
	dir := t.TempDir()
	exp := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	writeCredFiles(t, dir, map[string]string{
		"AWS_ACCESS_KEY_ID":     "AKIAEXAMPLE\n",
		"AWS_SECRET_ACCESS_KEY": "secret",
		"AWS_SESSION_TOKEN":     "token",
		"expiration":            exp.Format(time.RFC3339),
	})

	creds, err := secretFileCredentials{dir: dir}.Retrieve(context.Background())
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if creds.AccessKeyID != "AKIAEXAMPLE" { // trailing newline trimmed
		t.Errorf("AccessKeyID = %q, want AKIAEXAMPLE", creds.AccessKeyID)
	}
	if creds.SecretAccessKey != "secret" || creds.SessionToken != "token" {
		t.Errorf("unexpected secret/token: %q / %q", creds.SecretAccessKey, creds.SessionToken)
	}
	if !creds.CanExpire {
		t.Fatal("expected CanExpire=true when expiration present")
	}
	if want := exp.Add(-renewEarly); !creds.Expires.Equal(want) {
		t.Errorf("Expires = %v, want %v (renewed %v early)", creds.Expires, want, renewEarly)
	}
}

func TestSecretFileCredentials_MissingAccessKey(t *testing.T) {
	dir := t.TempDir()
	// No AWS_ACCESS_KEY_ID file at all — creds not provisioned yet.
	if _, err := (secretFileCredentials{dir: dir}).Retrieve(context.Background()); err == nil {
		t.Fatal("expected error when access key is absent")
	}
}

func TestSecretFileCredentials_NoExpiration(t *testing.T) {
	dir := t.TempDir()
	writeCredFiles(t, dir, map[string]string{
		"AWS_ACCESS_KEY_ID":     "AKIA",
		"AWS_SECRET_ACCESS_KEY": "secret",
		"AWS_SESSION_TOKEN":     "token",
	})
	creds, err := secretFileCredentials{dir: dir}.Retrieve(context.Background())
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if creds.CanExpire {
		t.Error("expected CanExpire=false when no expiration file")
	}
}

func TestReadCredFile_Absent(t *testing.T) {
	v, err := readCredFile(t.TempDir(), "does-not-exist")
	if err != nil {
		t.Fatalf("absent file should not error: %v", err)
	}
	if v != "" {
		t.Errorf("absent file should read as empty, got %q", v)
	}
}
