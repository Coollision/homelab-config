package controllers

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
)

func TestProfileKey(t *testing.T) {
	got := ProfileKey("aws/profile@prod")
	want := "aws_profile_prod"
	if got != want {
		t.Fatalf("expected %s, got %s", want, got)
	}
}

func TestIsCredentialValid(t *testing.T) {
	valid := &corev1.Secret{Data: map[string][]byte{"expiration": []byte(time.Now().UTC().Add(10 * time.Minute).Format(time.RFC3339))}}
	if !IsCredentialValid(valid) {
		t.Fatalf("expected valid secret")
	}

	expired := &corev1.Secret{Data: map[string][]byte{"expiration": []byte(time.Now().UTC().Add(-10 * time.Minute).Format(time.RFC3339))}}
	if IsCredentialValid(expired) {
		t.Fatalf("expected expired secret to be invalid")
	}

	bad := &corev1.Secret{Data: map[string][]byte{"expiration": []byte("n/a")}}
	if IsCredentialValid(bad) {
		t.Fatalf("expected malformed secret to be invalid")
	}
}

func TestDesiredReplicas(t *testing.T) {
	if got := *desiredReplicas(true); got != 1 {
		t.Fatalf("expected 1 replica when creds are valid, got %d", got)
	}
	if got := *desiredReplicas(false); got != 0 {
		t.Fatalf("expected 0 replicas when creds are invalid, got %d", got)
	}
}
