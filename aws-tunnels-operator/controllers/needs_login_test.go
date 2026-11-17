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

	if err := markRefreshOK(ctx, c, "ns", "s", time.Now()); err != nil {
		t.Fatal(err)
	}
	_ = c.Get(ctx, clientKey("ns", "s"), got)
	if got.Annotations[annotLastRefreshOK] == "" {
		t.Fatal("expected last-refresh-ok stamp")
	}

	if err := markLoggedIn(ctx, c, "ns", "s", time.Now()); err != nil {
		t.Fatal(err)
	}
	_ = c.Get(ctx, clientKey("ns", "s"), got)
	if profileNeedsLogin(got) || got.Annotations[annotLastLogin] == "" || got.Annotations[annotLastRefreshOK] != "" {
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
