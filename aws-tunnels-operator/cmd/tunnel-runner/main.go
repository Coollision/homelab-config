package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2instanceconnect"
	"github.com/aws/aws-sdk-go-v2/service/rds"
)

const (
	defaultLocalPort  = "8080"
	defaultTunnelName = "aws-tunnel"
	defaultRegion     = "eu-west-1"
	defaultSSHUser    = "ec2-user"
	// defaultSSHLocalPort is a loopback-only port inside the pod used to reach the bastion's :22
	// through the SSM forward. It must differ from LOCAL_PORT.
	defaultSSHLocalPort = "2222"
	stateDir            = "/tmp/tunnel-state"
	retryCredDuration   = 30 * time.Second
	retryErrorDuration  = 10 * time.Second
	// minLoopInterval floors the reconnect loop period so no failure path can busy-spin the CPU.
	minLoopInterval = 2 * time.Second
)

// Config holds all runtime configuration for the tunnel-runner, sourced from env vars.
type Config struct {
	BastionName       string
	RemoteHost        string
	RemotePort        string
	LocalPort         string
	TunnelName        string
	AWSRegion         string
	RDSClusterPrefix  string
	RDSInstancePrefix string

	// Compressed SSH transport. When SSHEnabled, the runner forwards the bastion's :22 over SSM
	// and runs `ssh -C -L` through it instead of forwarding the target port directly, so payloads
	// are gzipped on the bastion before crossing AWS's ~0.7 MB/s per-session cap. See ssh.go.
	SSHEnabled     bool
	SSHCompression bool
	SSHUser        string
	SSHLocalPort   string
}

// configFromEnv builds a Config from the process environment.
// Returns an error if any required variable is absent.
func configFromEnv() (*Config, error) {
	cfg := &Config{
		BastionName:       os.Getenv("BASTION_NAME"),
		RemoteHost:        os.Getenv("REMOTE_HOST"),
		RemotePort:        os.Getenv("REMOTE_PORT"),
		LocalPort:         envOrDefault("LOCAL_PORT", defaultLocalPort),
		TunnelName:        envOrDefault("TUNNEL_NAME", defaultTunnelName),
		AWSRegion:         envOrDefault("AWS_REGION", defaultRegion),
		RDSClusterPrefix:  os.Getenv("RDS_CLUSTER_PREFIX"),
		RDSInstancePrefix: os.Getenv("RDS_INSTANCE_PREFIX"),

		SSHEnabled: os.Getenv("SSH_ENABLED") == "true",
		// Compression is the whole reason for SSH mode, so it defaults on and must be turned off
		// explicitly (useful to confirm the SSH layer itself is free).
		SSHCompression: envOrDefault("SSH_COMPRESSION", "true") == "true",
		SSHUser:        envOrDefault("SSH_USER", defaultSSHUser),
		SSHLocalPort:   envOrDefault("SSH_LOCAL_PORT", defaultSSHLocalPort),
	}

	var missing []string
	if cfg.BastionName == "" {
		missing = append(missing, "BASTION_NAME")
	}
	if cfg.RemotePort == "" {
		missing = append(missing, "REMOTE_PORT")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("required env vars not set: %s", strings.Join(missing, ", "))
	}
	if err := validateSSHConfig(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Runner orchestrates the tunnel lifecycle via a state-machine retry loop.
type Runner struct {
	cfg   *Config
	ec2   EC2Client
	rds   RDSClient
	eic   EICClient
	state *StateWriter
	log   *slog.Logger
	// sshKey is the ephemeral keypair used in SSH mode. It is generated once per process; the
	// public half is re-pushed to the bastion on every reconnect because EC2 Instance Connect
	// keys expire after 60s.
	sshKey *sshKeyPair
	// creds is non-nil only in refresh mode (AWS_CREDS_DIR set). When set, STS creds are resolved
	// from it on demand (and re-resolved when they expire) rather than from static env vars, and
	// the resolved creds are handed to each `aws ssm` subprocess so it uses the rotated values.
	creds aws.CredentialsProvider
}

// Run is the main loop; it blocks until ctx is cancelled.
func (r *Runner) Run(ctx context.Context) {
	r.state.Set(StateStarting, "tunnel process started")

	var lastLoopStart time.Time
	for ctx.Err() == nil {
		// Floor the loop period so no path can spin the CPU — in particular a clean-but-immediate SSM
		// exit, which (unlike the error paths) doesn't otherwise sleep before reconnecting. Long-lived
		// sessions make `since` far exceed the floor, so normal reconnects aren't delayed.
		if since := time.Since(lastLoopStart); since < minLoopInterval {
			r.sleep(ctx, minLoopInterval-since)
		}
		lastLoopStart = time.Now()

		// Resolve the creds to hand the SSM subprocess. In legacy mode they arrive via EnvFrom and
		// ssmEnv is nil (the subprocess inherits them); in refresh mode they are read from the
		// mounted, operator-refreshed Secret. Either way an empty result means creds aren't present
		// yet — the operator scales the pod to zero once it detects expiry, so we just wait.
		ssmEnv, ok := r.ssmCredentialsEnv(ctx)
		if !ok {
			r.log.Info("no credentials found, waiting")
			r.state.Set(StateAuthRequired, "AWS credentials unavailable — waiting for the operator to provision them")
			r.sleep(ctx, retryCredDuration)
			continue
		}

		remoteHost, err := resolveRemoteHost(ctx, r.rds, r.cfg)
		if err != nil {
			r.handleError(ctx, "resolve remote host", err)
			continue
		}

		instanceID, err := resolveBastion(ctx, r.ec2, r.cfg)
		if err != nil {
			r.handleError(ctx, fmt.Sprintf("resolve bastion %q", r.cfg.BastionName), err)
			continue
		}

		transport := "SSM"
		if r.cfg.SSHEnabled {
			transport = "SSH-over-SSM"
			if r.cfg.SSHCompression {
				transport += " (compressed)"
			}
		}
		r.log.Info("starting session",
			"transport", transport,
			"tunnel", r.cfg.TunnelName,
			"bastion", r.cfg.BastionName,
			"instanceID", instanceID,
			"remoteHost", remoteHost,
			"remotePort", r.cfg.RemotePort,
			"localPort", r.cfg.LocalPort,
		)
		r.state.Set(StateRunning, fmt.Sprintf(
			"forwarding 0.0.0.0:%s → %s:%s via %s", r.cfg.LocalPort, remoteHost, r.cfg.RemotePort, transport,
		))

		var isAuth bool
		if r.cfg.SSHEnabled {
			isAuth, err = runSSHSession(ctx, instanceID, remoteHost, r.cfg, r.sshKey, r.eic, ssmEnv)
		} else {
			isAuth, err = runSSMSession(ctx, instanceID, remoteHost, r.cfg, ssmEnv)
		}
		if err != nil {
			r.handleError(ctx, transport+" session", forceAuth(err, isAuth))
			continue
		}

		// Clean exit (context cancelled or graceful close) — loop to reconnect.
		r.state.Set(StateReconnecting, "session ended cleanly")
	}
}

// ssmCredentialsEnv reports whether usable creds are available and, in refresh mode, the extra env
// vars to hand the `aws ssm` subprocess so it uses the operator-refreshed creds. In legacy mode the
// subprocess inherits the static AWS_* vars from the pod env, so the returned slice is nil.
func (r *Runner) ssmCredentialsEnv(ctx context.Context) ([]string, bool) {
	if r.creds == nil {
		return nil, os.Getenv("AWS_ACCESS_KEY_ID") != ""
	}
	c, err := r.creds.Retrieve(ctx)
	if err != nil || c.AccessKeyID == "" {
		if err != nil {
			r.log.Info("credentials not available yet", "err", err)
		}
		return nil, false
	}
	if c.Expired() {
		// The operator hasn't refreshed the mounted Secret (e.g. the SSO session has ended). Don't
		// fire doomed EC2/RDS/SSM calls with stale creds — wait for the operator to provide fresh
		// ones (it scales this pod to zero anyway once it sees the creds Secret expire).
		r.log.Info("mounted credentials are expired, waiting for refresh")
		return nil, false
	}
	return []string{
		"AWS_ACCESS_KEY_ID=" + c.AccessKeyID,
		"AWS_SECRET_ACCESS_KEY=" + c.SecretAccessKey,
		"AWS_SESSION_TOKEN=" + c.SessionToken,
	}, true
}

// handleError routes err to the correct retry state and sleep duration.
func (r *Runner) handleError(ctx context.Context, op string, err error) {
	if isAuthError(err) {
		r.log.Warn("auth error — waiting for credential refresh", "op", op, "err", err)
		r.state.Set(StateAuthRequired, err.Error())
		r.sleep(ctx, retryCredDuration)
	} else {
		r.log.Error("transient error — will retry", "op", op, "err", err)
		r.state.Set(StateError, err.Error())
		r.sleep(ctx, retryErrorDuration)
	}
}

// sleep pauses for d, returning early on context cancellation.
func (r *Runner) sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := configFromEnv()
	if err != nil {
		log.Error("invalid configuration", "err", err)
		os.Exit(1)
	}

	sw, err := newStateWriter(stateDir, log)
	if err != nil {
		log.Error("failed to initialise state writer", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// In refresh mode (AWS_CREDS_DIR set) the SDK resolves creds from the mounted Secret via a
	// caching provider that re-reads the files when they expire, so EC2/RDS calls keep working
	// across creds rotation without a restart. In legacy mode the SDK uses its default chain
	// (the static AWS_* env vars from EnvFrom).
	var credsProvider aws.CredentialsProvider
	loadOpts := []func(*awsconfig.LoadOptions) error{}
	if dir := os.Getenv(credsDirEnv); dir != "" {
		credsProvider = aws.NewCredentialsCache(secretFileCredentials{dir: dir})
		loadOpts = append(loadOpts, awsconfig.WithCredentialsProvider(credsProvider))
		log.Info("refresh mode: resolving STS credentials from mounted Secret", "dir", dir)
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		log.Error("failed to load AWS config", "err", err)
		os.Exit(1)
	}

	runner := &Runner{
		cfg:   cfg,
		ec2:   ec2.NewFromConfig(awsCfg),
		rds:   rds.NewFromConfig(awsCfg),
		eic:   ec2instanceconnect.NewFromConfig(awsCfg),
		state: sw,
		log:   log,
		creds: credsProvider,
	}

	// SSH mode needs a keypair for the whole process lifetime; only the 60s-lived public half
	// ever reaches the bastion.
	if cfg.SSHEnabled {
		key, keyErr := newSSHKeyPair()
		if keyErr != nil {
			log.Error("failed to generate ephemeral SSH key", "err", keyErr)
			os.Exit(1)
		}
		defer key.cleanup()
		runner.sshKey = key
		log.Info("compressed SSH transport enabled",
			"user", cfg.SSHUser, "sshLocalPort", cfg.SSHLocalPort, "compression", cfg.SSHCompression)
	}

	runner.Run(ctx)
	log.Info("tunnel-runner stopped")
}
