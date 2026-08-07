package controllers

import (
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	if got := *desiredReplicas(true, 1); got != 1 {
		t.Fatalf("expected 1 replica when creds are valid, got %d", got)
	}
	if got := *desiredReplicas(false, 1); got != 0 {
		t.Fatalf("expected 0 replicas when creds are invalid, got %d", got)
	}
	if got := *desiredReplicas(true, 4); got != 4 {
		t.Fatalf("expected the requested 4 replicas, got %d", got)
	}
	// Invalid creds win over any requested count — the runner cannot connect regardless.
	if got := *desiredReplicas(false, 4); got != 0 {
		t.Fatalf("expected 0 replicas when creds are invalid regardless of want, got %d", got)
	}
	if got := *desiredReplicas(true, maxTunnelReplicas+5); got != maxTunnelReplicas {
		t.Fatalf("expected clamp to %d, got %d", maxTunnelReplicas, got)
	}
}

func TestClampReplicas(t *testing.T) {
	cases := []struct{ in, want int32 }{
		{-3, 1}, {0, 1}, {1, 1}, {5, 5},
		{maxTunnelReplicas, maxTunnelReplicas},
		{maxTunnelReplicas + 1, maxTunnelReplicas},
	}
	for _, c := range cases {
		if got := clampReplicas(c.in); got != c.want {
			t.Fatalf("clampReplicas(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestResolveReplicas(t *testing.T) {
	n := func(v int32) *int32 { return &v }

	// Nothing configured anywhere -> 1.
	if got := resolveReplicas(TunnelSpec{}, TunnelDefaultSpec{}); got != 1 {
		t.Fatalf("expected default of 1, got %d", got)
	}
	// Stack default applies when the tunnel says nothing.
	if got := resolveReplicas(TunnelSpec{}, TunnelDefaultSpec{Replicas: n(3)}); got != 3 {
		t.Fatalf("expected stack default 3, got %d", got)
	}
	// Per-tunnel wins over the stack default.
	if got := resolveReplicas(TunnelSpec{Replicas: n(2)}, TunnelDefaultSpec{Replicas: n(3)}); got != 2 {
		t.Fatalf("expected per-tunnel 2 to win, got %d", got)
	}
	// Out-of-range configured values are clamped, not rejected.
	if got := resolveReplicas(TunnelSpec{Replicas: n(99)}, TunnelDefaultSpec{}); got != maxTunnelReplicas {
		t.Fatalf("expected clamp to %d, got %d", maxTunnelReplicas, got)
	}
}

func TestResolveSSH(t *testing.T) {
	b := func(v bool) *bool { return &v }

	// Off by default — compression is a loss on incompressible traffic, so it must be opt-in.
	if got := resolveSSH(TunnelSpec{}, TunnelDefaultSpec{}); got.Enabled {
		t.Fatalf("expected SSH disabled by default")
	}

	// Stack default enables it, and the blanks get sensible values.
	got := resolveSSH(TunnelSpec{}, TunnelDefaultSpec{SSH: SSHSpec{Enabled: true}})
	if !got.Enabled {
		t.Fatalf("expected stack default to enable SSH")
	}
	if got.User != defaultSSHUser || got.LocalSSHPort != defaultLocalSSHPort {
		t.Fatalf("expected defaults filled in, got user=%q port=%d", got.User, got.LocalSSHPort)
	}
	if got.Compression == nil || !*got.Compression {
		t.Fatalf("expected compression on by default — it is the whole point of the mode")
	}

	// A tunnel can opt in on its own.
	if got := resolveSSH(TunnelSpec{SSH: SSHSpec{Enabled: true}}, TunnelDefaultSpec{}); !got.Enabled {
		t.Fatalf("expected per-tunnel opt-in to enable SSH")
	}

	// Per-tunnel values override the stack defaults.
	got = resolveSSH(
		TunnelSpec{SSH: SSHSpec{Enabled: true, User: "admin", LocalSSHPort: 2323, Compression: b(false)}},
		TunnelDefaultSpec{SSH: SSHSpec{Enabled: true, User: "ec2-user", LocalSSHPort: 2222}},
	)
	if got.User != "admin" || got.LocalSSHPort != 2323 {
		t.Fatalf("expected per-tunnel overrides, got user=%q port=%d", got.User, got.LocalSSHPort)
	}
	if got.Compression == nil || *got.Compression {
		t.Fatalf("expected compression explicitly disabled")
	}
}

func TestTCPTLSSpec(t *testing.T) {
	// Passthrough must never carry a certificate — Traefik cannot do both.
	got := tcpTLSSpec(TLSSpec{Passthrough: true, SecretName: "ignored"})
	if got["passthrough"] != true {
		t.Fatalf("expected passthrough true, got %v", got)
	}
	if _, ok := got["secretName"]; ok {
		t.Fatalf("passthrough route must not carry a secretName: %v", got)
	}

	// Termination with an explicit secret keeps SNI routing working for clients.
	got = tcpTLSSpec(TLSSpec{SecretName: "db-tls"})
	if got["passthrough"] != false || got["secretName"] != "db-tls" {
		t.Fatalf("expected terminating route with secret, got %v", got)
	}

	got = tcpTLSSpec(TLSSpec{CertResolver: "le"})
	if got["certResolver"] != "le" {
		t.Fatalf("expected certResolver passed through, got %v", got)
	}

	// Neither set: still a valid terminating route using Traefik's default cert.
	got = tcpTLSSpec(TLSSpec{})
	if got["passthrough"] != false {
		t.Fatalf("expected non-passthrough default, got %v", got)
	}
}

func TestManualReplicaOverride(t *testing.T) {
	withAnnot := func(v map[string]string) *appsv1.Deployment {
		return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Annotations: v}}
	}

	if _, ok := manualReplicaOverride(withAnnot(nil)); ok {
		t.Fatalf("expected no override when annotations are absent")
	}
	if _, ok := manualReplicaOverride(withAnnot(map[string]string{annotManualReplicas: "  "})); ok {
		t.Fatalf("expected no override for a blank value")
	}
	// A hand-edited bad value must fall back to config rather than wedge the tunnel.
	if _, ok := manualReplicaOverride(withAnnot(map[string]string{annotManualReplicas: "three"})); ok {
		t.Fatalf("expected no override for an unparseable value")
	}
	got, ok := manualReplicaOverride(withAnnot(map[string]string{annotManualReplicas: "4"}))
	if !ok || got != 4 {
		t.Fatalf("expected override 4, got %d (ok=%v)", got, ok)
	}
	// Overrides are clamped like configured values.
	got, ok = manualReplicaOverride(withAnnot(map[string]string{annotManualReplicas: "99"}))
	if !ok || got != maxTunnelReplicas {
		t.Fatalf("expected override clamped to %d, got %d", maxTunnelReplicas, got)
	}
}

func TestCredsSecretName(t *testing.T) {
	cases := []struct {
		stack, profile, want string
	}{
		{"my-stack", "dev", "my-stack-creds-dev"},
		{"my-stack", "My Profile", "my-stack-creds-my_profile"},
		{"my-stack", "AWS/SSO:prod@eu", "my-stack-creds-aws_sso_prod_eu"},
		{"my-stack", "UPPER", "my-stack-creds-upper"},
	}
	for _, tc := range cases {
		got := credsSecretName(tc.stack, tc.profile)
		if got != tc.want {
			t.Errorf("credsSecretName(%q, %q) = %q, want %q", tc.stack, tc.profile, got, tc.want)
		}
	}
}
