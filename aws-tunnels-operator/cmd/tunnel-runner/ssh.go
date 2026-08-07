package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2instanceconnect"
)

// The compressed SSH transport.
//
// AWS rate-limits a single SSM session to ~0.7 MB/s and that cap applies to bytes on the wire, so
// the only way to move more data is to send less of it. Instead of forwarding the target port
// directly over SSM, this mode forwards the bastion's :22 and runs `ssh -C` through it, which
// gzips payloads ON THE BASTION before they reach the bottleneck. Measured 3.8x on a Postgres
// bulk read (0.62 -> 2.36 MB/s); the SSH layer itself costs nothing (0.65 MB/s uncompressed).
//
// Only compressible traffic benefits. Encrypted or already-compressed payloads get slightly
// slower, so this must stay off for TLS-passthrough tunnels.
//
// Authentication uses EC2 Instance Connect: a freshly generated key is pushed to the instance and
// is valid for 60 seconds, so nothing long-lived is stored and there is no key to rotate. The key
// is re-pushed on every reconnect because of that expiry.

const (
	// sshConnectTimeout bounds the wait for the SSM forward to start accepting connections.
	sshConnectTimeout = 60 * time.Second
	// sshDialInterval is the poll period while waiting for that local port.
	sshDialInterval = 250 * time.Millisecond
	// eicKeyGraceWindow is how long an EC2 Instance Connect key stays valid. AWS fixes this at
	// 60s; we push immediately before dialling so the whole window is available for the handshake.
	eicKeyGraceWindow = 60 * time.Second
)

// EICClient is the subset of the EC2 Instance Connect API the runner uses.
type EICClient interface {
	SendSSHPublicKey(ctx context.Context, params *ec2instanceconnect.SendSSHPublicKeyInput, optFns ...func(*ec2instanceconnect.Options)) (*ec2instanceconnect.SendSSHPublicKeyOutput, error)
}

// sshKeyPair is an ephemeral keypair used for a single tunnel process lifetime.
type sshKeyPair struct {
	privatePath string
	publicKey   string // OpenSSH authorized_keys format
	dir         string
}

// newSSHKeyPair generates an ed25519 keypair in a private temp dir. ssh-keygen is used rather
// than crypto/ed25519 + x/crypto/ssh so the runner keeps its existing dependency surface — it
// already shells out to the AWS CLI, and openssh-clients is installed in the image for `ssh`.
func newSSHKeyPair() (*sshKeyPair, error) {
	dir, err := os.MkdirTemp("", "tunnel-ssh-")
	if err != nil {
		return nil, fmt.Errorf("create key dir: %w", err)
	}
	// 0700: the private key must not be group/world readable or ssh refuses to use it.
	if err := os.Chmod(dir, 0o700); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("chmod key dir: %w", err)
	}

	priv := filepath.Join(dir, "id_ed25519")
	cmd := exec.Command("ssh-keygen", "-t", "ed25519", "-N", "", "-f", priv, "-q", "-C", "aws-tunnel-runner")
	if out, err := cmd.CombinedOutput(); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("ssh-keygen: %w: %s", err, strings.TrimSpace(string(out)))
	}

	pub, err := os.ReadFile(priv + ".pub")
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("read public key: %w", err)
	}
	return &sshKeyPair{privatePath: priv, publicKey: strings.TrimSpace(string(pub)), dir: dir}, nil
}

func (k *sshKeyPair) cleanup() {
	if k != nil && k.dir != "" {
		_ = os.RemoveAll(k.dir)
	}
}

// startDetached launches cmd in its own process group.
//
// This matters: `aws ssm start-session` forks session-manager-plugin as a child, and killing only
// the aws wrapper orphans the plugin — which keeps the SSM session and the local port alive,
// so the next reconnect attempt fails with "address already in use". Killing the whole group
// reaps both.
func startDetached(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd.Start()
}

// killGroup terminates the process group led by cmd, tolerating an already-dead process.
func killGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		_ = cmd.Process.Kill()
		return
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
}

// waitForPort blocks until something accepts TCP on 127.0.0.1:port, or the deadline passes.
func waitForPort(ctx context.Context, port string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", port), 2*time.Second)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for 127.0.0.1:%s", timeout, port)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(sshDialInterval):
		}
	}
}

// sshArgs builds the ssh command line for the -L forward through the local SSM forward.
// remoteHost is resolved per-attempt (RDS endpoints move), so it is passed in rather than read
// from Config.
func sshArgs(cfg *Config, remoteHost, keyPath string) []string {
	args := []string{}
	if cfg.SSHCompression {
		args = append(args, "-C")
	}
	args = append(args,
		"-i", keyPath,
		"-p", cfg.SSHLocalPort,
		// The bastion host key is not known ahead of time and the instance may be replaced, so
		// pinning it would break reconnects. The hop is already authenticated and encrypted by
		// SSM underneath (and reaches only 127.0.0.1 inside this pod), so host-key trust adds
		// nothing here.
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "BatchMode=yes",
		"-o", "ExitOnForwardFailure=yes",
		// Detect a silently dead session so the supervisor can reconnect instead of hanging.
		"-o", "ServerAliveInterval=30",
		"-o", "ServerAliveCountMax=3",
		"-N",
		// Bind the forward on loopback only: the socat sidecar is what exposes the tunnel on the
		// pod IP, exactly as in the plain-SSM path, so nothing changes for the Service.
		"-L", fmt.Sprintf("127.0.0.1:%s:%s:%s", cfg.LocalPort, remoteHost, cfg.RemotePort),
		fmt.Sprintf("%s@127.0.0.1", cfg.SSHUser),
	)
	return args
}

// ssmSSHForwardArgs builds the `aws ssm start-session` invocation that exposes the bastion's :22
// on a loopback port inside this pod.
func ssmSSHForwardArgs(cfg *Config, instanceID string) []string {
	params := fmt.Sprintf(`{"portNumber":["22"],"localPortNumber":["%s"]}`, cfg.SSHLocalPort)
	return []string{
		"ssm", "start-session",
		"--region", cfg.AWSRegion,
		"--target", instanceID,
		"--document-name", "AWS-StartPortForwardingSession",
		"--parameters", params,
	}
}

// runSSHSession brings up the compressed transport and blocks until it ends.
//
// Two subprocesses are supervised: the SSM forward to the bastion's :22, and the ssh client doing
// the -L forward. If either exits the other is killed, so the caller's reconnect loop always
// restarts from a clean state rather than reusing a half-dead pair.
func runSSHSession(ctx context.Context, instanceID, remoteHost string, cfg *Config, key *sshKeyPair, eic EICClient, extraEnv []string) (isAuthErr bool, _ error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	env := os.Environ()
	if len(extraEnv) > 0 {
		env = append(env, extraEnv...)
	}

	ssmCmd := exec.CommandContext(ctx, "aws", ssmSSHForwardArgs(cfg, instanceID)...)
	ssmCmd.Env = env
	ssmOut := &clippedBuffer{}
	ssmCmd.Stdout, ssmCmd.Stderr = ssmOut, ssmOut
	if err := startDetached(ssmCmd); err != nil {
		return false, fmt.Errorf("start SSM ssh-forward: %w", err)
	}
	defer killGroup(ssmCmd)

	ssmDone := make(chan error, 1)
	go func() { ssmDone <- ssmCmd.Wait() }()

	// The forward must be listening before the ssh client dials it.
	if err := waitForPort(ctx, cfg.SSHLocalPort, sshConnectTimeout); err != nil {
		out := ssmOut.String()
		if outputContainsAuthKeyword(out) {
			return true, fmt.Errorf("SSM ssh-forward auth failure: %s", strings.TrimSpace(out))
		}
		return false, fmt.Errorf("SSM ssh-forward not ready: %w: %s", err, strings.TrimSpace(out))
	}

	// EC2 Instance Connect keys live for 60s, so push immediately before connecting. This runs on
	// every reconnect by design — there is deliberately no long-lived key on the bastion.
	pushCtx, pushCancel := context.WithTimeout(ctx, 30*time.Second)
	_, err := eic.SendSSHPublicKey(pushCtx, &ec2instanceconnect.SendSSHPublicKeyInput{
		InstanceId:     aws.String(instanceID),
		InstanceOSUser: aws.String(cfg.SSHUser),
		SSHPublicKey:   aws.String(key.publicKey),
	})
	pushCancel()
	if err != nil {
		return isAuthError(err), fmt.Errorf("send ephemeral SSH key: %w", err)
	}

	sshCmd := exec.CommandContext(ctx, "ssh", sshArgs(cfg, remoteHost, key.privatePath)...)
	sshCmd.Env = env
	sshOut := &clippedBuffer{}
	sshCmd.Stdout, sshCmd.Stderr = sshOut, sshOut
	if err := startDetached(sshCmd); err != nil {
		return false, fmt.Errorf("start ssh: %w", err)
	}
	defer killGroup(sshCmd)

	sshDone := make(chan error, 1)
	go func() { sshDone <- sshCmd.Wait() }()

	// Whichever half dies first ends the session; the deferred killGroup calls reap the other.
	select {
	case <-ctx.Done():
		return false, nil
	case err := <-ssmDone:
		out := ssmOut.String()
		if outputContainsAuthKeyword(out) {
			return true, fmt.Errorf("SSM ssh-forward auth failure: %w: %s", err, strings.TrimSpace(out))
		}
		if err == nil {
			return false, nil
		}
		return false, fmt.Errorf("SSM ssh-forward exited: %w: %s", err, strings.TrimSpace(out))
	case err := <-sshDone:
		out := sshOut.String()
		if err == nil {
			return false, nil
		}
		return false, fmt.Errorf("ssh exited: %w: %s", err, strings.TrimSpace(out))
	}
}

// clippedBuffer collects subprocess output but keeps only the tail, so a chatty or looping
// subprocess cannot grow the runner's memory without bound.
type clippedBuffer struct {
	buf []byte
}

const clippedBufferMax = 8 << 10

func (c *clippedBuffer) Write(p []byte) (int, error) {
	c.buf = append(c.buf, p...)
	if len(c.buf) > clippedBufferMax {
		c.buf = c.buf[len(c.buf)-clippedBufferMax:]
	}
	return len(p), nil
}

func (c *clippedBuffer) String() string { return string(c.buf) }

// validateSSHConfig rejects settings that would produce a broken tunnel at runtime.
func validateSSHConfig(cfg *Config) error {
	if !cfg.SSHEnabled {
		return nil
	}
	if cfg.SSHLocalPort == cfg.LocalPort {
		return fmt.Errorf("SSH_LOCAL_PORT (%s) must differ from LOCAL_PORT (%s)", cfg.SSHLocalPort, cfg.LocalPort)
	}
	for _, p := range []string{cfg.SSHLocalPort, cfg.LocalPort} {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("invalid port %q", p)
		}
	}
	if cfg.SSHUser == "" {
		return errors.New("SSH_USER must not be empty when SSH mode is enabled")
	}
	return nil
}
