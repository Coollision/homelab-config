package controllers

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestIsTokenExpiredError(t *testing.T) {
	if !isTokenExpiredError(errors.New("aws configure export-credentials failed: Token has expired and refresh failed")) {
		t.Fatal("expected expired-token error to match")
	}
	if isTokenExpiredError(errors.New("dial tcp: i/o timeout")) || isTokenExpiredError(nil) {
		t.Fatal("transient/nil errors must not match")
	}
}

func TestSSOAccessTokenExpiry(t *testing.T) {
	sec := &corev1.Secret{Data: map[string][]byte{
		"reg.json": []byte(`{"clientId":"x","expiresAt":"2030-01-01T00:00:00Z"}`),
		"old.json": []byte(`{"startUrl":"https://example.awsapps.com/start","accessToken":"a","expiresAt":"2026-11-17T08:00:00Z"}`),
		"new.json": []byte(`{"startUrl":"https://example.awsapps.com/start/","accessToken":"b","expiresAt":"2026-11-17T09:00:00Z"}`),
		"other":    []byte(`{"startUrl":"https://other.awsapps.com/start","accessToken":"c","expiresAt":"2031-01-01T00:00:00Z"}`),
		"garbage":  []byte(`not json`),
	}}
	got, ok := ssoAccessTokenExpiry(sec, "https://example.awsapps.com/start")
	if !ok || !got.Equal(time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)) {
		t.Fatalf("got %v %v", got, ok)
	}
	if _, ok := ssoAccessTokenExpiry(sec, "https://missing.example/start"); ok {
		t.Fatal("unknown start url must not match")
	}
	if _, ok := ssoAccessTokenExpiry(nil, "x"); ok {
		t.Fatal("nil secret must not match")
	}
}

func TestNeedsLoginAnnotationLifecycle(t *testing.T) {
	ctx := context.Background()
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "ns"}}
	c := fake.NewClientBuilder().WithObjects(sec).Build()

	if err := markNeedsLogin(ctx, c, "ns", "s", time.Now()); err != nil {
		t.Fatal(err)
	}
	got := &corev1.Secret{}
	_ = c.Get(ctx, clientKey("ns", "s"), got)
	if !profileNeedsLogin(got) {
		t.Fatal("expected parked after markNeedsLogin")
	}

	if err := markLoggedIn(ctx, c, "ns", "s", time.Now()); err != nil {
		t.Fatal(err)
	}
	_ = c.Get(ctx, clientKey("ns", "s"), got)
	if profileNeedsLogin(got) || got.Annotations[annotLastLogin] == "" {
		t.Fatalf("login must unpark and stamp last-login: %v", got.Annotations)
	}
	// missing secret is a no-op, not an error
	if err := markLoggedIn(ctx, c, "ns", "absent", time.Now()); err != nil {
		t.Fatal(err)
	}
}

func clientKey(ns, name string) types.NamespacedName {
	return types.NamespacedName{Namespace: ns, Name: name}
}
