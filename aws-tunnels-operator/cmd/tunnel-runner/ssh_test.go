package main

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ec2instanceconnect"
)

func TestValidateSSHConfig(t *testing.T) {
	// Disabled mode never validates SSH settings.
	if err := validateSSHConfig(&Config{SSHEnabled: false, LocalPort: "8080", SSHLocalPort: "8080"}); err != nil {
		t.Fatalf("expected no error when SSH is disabled, got %v", err)
	}

	// The two ports must differ or ssh and the SSM forward fight over the same listener.
	err := validateSSHConfig(&Config{SSHEnabled: true, LocalPort: "8080", SSHLocalPort: "8080", SSHUser: "ec2-user"})
	if err == nil || !strings.Contains(err.Error(), "must differ") {
		t.Fatalf("expected a port-collision error, got %v", err)
	}

	if err := validateSSHConfig(&Config{SSHEnabled: true, LocalPort: "8080", SSHLocalPort: "2222", SSHUser: "ec2-user"}); err != nil {
		t.Fatalf("expected valid config, got %v", err)
	}

	if err := validateSSHConfig(&Config{SSHEnabled: true, LocalPort: "8080", SSHLocalPort: "2222"}); err == nil {
		t.Fatalf("expected an error for an empty SSH user")
	}

	if err := validateSSHConfig(&Config{SSHEnabled: true, LocalPort: "0", SSHLocalPort: "2222", SSHUser: "ec2-user"}); err == nil {
		t.Fatalf("expected an error for an out-of-range port")
	}
}

func TestSSHArgs(t *testing.T) {
	cfg := &Config{
		LocalPort: "15432", RemotePort: "5432",
		SSHLocalPort: "2222", SSHUser: "ec2-user", SSHCompression: true,
	}
	args := strings.Join(sshArgs(cfg, "db.internal", "/tmp/k"), " ")

	if !strings.Contains(args, "-C") {
		t.Fatalf("expected compression flag, got: %s", args)
	}
	// The forward must bind loopback only — socat is what exposes the tunnel on the pod IP.
	if !strings.Contains(args, "-L 127.0.0.1:15432:db.internal:5432") {
		t.Fatalf("expected loopback-bound -L forward, got: %s", args)
	}
	if !strings.Contains(args, "ec2-user@127.0.0.1") || !strings.Contains(args, "-p 2222") {
		t.Fatalf("expected ssh to dial the local SSM forward, got: %s", args)
	}
	// Non-interactive: must never block waiting on a prompt.
	if !strings.Contains(args, "BatchMode=yes") || !strings.Contains(args, "-N") {
		t.Fatalf("expected non-interactive flags, got: %s", args)
	}

	cfg.SSHCompression = false
	if strings.Contains(strings.Join(sshArgs(cfg, "db.internal", "/tmp/k"), " "), " -C ") {
		t.Fatalf("expected no compression flag when disabled")
	}
}

func TestSSMSSHForwardArgs(t *testing.T) {
	cfg := &Config{AWSRegion: "eu-central-1", SSHLocalPort: "2222"}
	args := strings.Join(ssmSSHForwardArgs(cfg, "i-abc"), " ")

	// Must forward the bastion's own :22, not the tunnel target.
	if !strings.Contains(args, "AWS-StartPortForwardingSession") {
		t.Fatalf("expected the local port-forward document, got: %s", args)
	}
	if strings.Contains(args, "ToRemoteHost") {
		t.Fatalf("SSH mode must not use the remote-host document, got: %s", args)
	}
	if !strings.Contains(args, `"portNumber":["22"]`) || !strings.Contains(args, `"localPortNumber":["2222"]`) {
		t.Fatalf("expected bastion :22 -> 2222 mapping, got: %s", args)
	}
}

func TestNewSSHKeyPair(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen not available")
	}
	key, err := newSSHKeyPair()
	if err != nil {
		t.Fatalf("generating keypair: %v", err)
	}
	defer key.cleanup()

	if !strings.HasPrefix(key.publicKey, "ssh-ed25519 ") {
		t.Fatalf("expected an ed25519 public key, got %q", key.publicKey)
	}
	info, err := os.Stat(key.privatePath)
	if err != nil {
		t.Fatalf("stat private key: %v", err)
	}
	// ssh refuses keys that are group/world readable.
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("private key is too permissive: %v", perm)
	}

	dir := key.dir
	key.cleanup()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("expected key dir to be removed, got err=%v", err)
	}
}

func TestWaitForPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	_, port, _ := net.SplitHostPort(ln.Addr().String())

	if err := waitForPort(context.Background(), port, 5*time.Second); err != nil {
		t.Fatalf("expected the open port to be detected, got %v", err)
	}

	// A closed port must time out rather than hang forever.
	ln.Close()
	if err := waitForPort(context.Background(), port, 600*time.Millisecond); err == nil {
		t.Fatalf("expected a timeout for a closed port")
	}

	// Cancellation must be honoured promptly.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitForPort(ctx, port, 5*time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestClippedBuffer(t *testing.T) {
	var b clippedBuffer
	// Writing far more than the cap must retain only the tail, bounding memory.
	for i := 0; i < 100; i++ {
		_, _ = b.Write([]byte(strings.Repeat("x", 1024)))
	}
	if len(b.String()) > clippedBufferMax {
		t.Fatalf("buffer grew past the cap: %d", len(b.String()))
	}
	b = clippedBuffer{}
	_, _ = b.Write([]byte("hello"))
	if b.String() != "hello" {
		t.Fatalf("expected short writes preserved, got %q", b.String())
	}
}

// stubEIC records the key push so the auth path can be asserted without AWS.
type stubEIC struct {
	called bool
	err    error
	user   string
	pubKey string
}

func (s *stubEIC) SendSSHPublicKey(_ context.Context, in *ec2instanceconnect.SendSSHPublicKeyInput, _ ...func(*ec2instanceconnect.Options)) (*ec2instanceconnect.SendSSHPublicKeyOutput, error) {
	s.called = true
	if in.InstanceOSUser != nil {
		s.user = *in.InstanceOSUser
	}
	if in.SSHPublicKey != nil {
		s.pubKey = *in.SSHPublicKey
	}
	return &ec2instanceconnect.SendSSHPublicKeyOutput{}, s.err
}

// When the SSM forward never comes up, the session must fail fast rather than hang, and must not
// push a key for a tunnel it cannot reach.
func TestRunSSHSessionFailsWhenForwardNeverOpens(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen not available")
	}
	key, err := newSSHKeyPair()
	if err != nil {
		t.Fatalf("keypair: %v", err)
	}
	defer key.cleanup()

	cfg := &Config{
		AWSRegion: "eu-central-1", LocalPort: "18080", RemotePort: "5432",
		SSHLocalPort: "59999", SSHUser: "ec2-user", SSHCompression: true, SSHEnabled: true,
	}
	eic := &stubEIC{}

	// Short deadline: `aws` is absent or fails immediately in tests, so the port never opens.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, err = runSSHSession(ctx, "i-abc", "db.internal", cfg, key, eic, nil)
	if err == nil {
		t.Fatalf("expected an error when the SSM forward never opens")
	}
	if eic.called {
		t.Fatalf("must not push an SSH key when the forward is not ready")
	}
}
