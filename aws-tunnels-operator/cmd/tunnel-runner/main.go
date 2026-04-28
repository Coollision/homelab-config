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

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/rds"
)

const (
	defaultLocalPort   = "8080"
	defaultTunnelName  = "aws-tunnel"
	defaultRegion      = "eu-west-1"
	stateDir           = "/tmp/tunnel-state"
	retryCredDuration  = 30 * time.Second
	retryErrorDuration = 10 * time.Second
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
	state *StateWriter
	log   *slog.Logger
}

// Run is the main loop; it blocks until ctx is cancelled.
func (r *Runner) Run(ctx context.Context) {
	r.state.Set(StateStarting, "tunnel process started")

	for ctx.Err() == nil {
		// Credentials are injected via EnvFrom from the K8s Secret.
		// An empty key means the Secret is absent or expired; the operator
		// will scale the pod to zero once it detects expiry.
		if os.Getenv("AWS_ACCESS_KEY_ID") == "" {
			r.log.Info("no credentials found, waiting")
			r.state.Set(StateAuthRequired, "AWS_ACCESS_KEY_ID is empty — waiting for credential Secret")
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

		r.log.Info("starting SSM session",
			"tunnel", r.cfg.TunnelName,
			"bastion", r.cfg.BastionName,
			"instanceID", instanceID,
			"remoteHost", remoteHost,
			"remotePort", r.cfg.RemotePort,
			"localPort", r.cfg.LocalPort,
		)
		r.state.Set(StateRunning, fmt.Sprintf(
			"forwarding 0.0.0.0:%s → %s:%s", r.cfg.LocalPort, remoteHost, r.cfg.RemotePort,
		))

		isAuth, err := runSSMSession(ctx, instanceID, remoteHost, r.cfg)
		if err != nil {
			r.handleError(ctx, "SSM session", forceAuth(err, isAuth))
			continue
		}

		// Clean exit (context cancelled or graceful close) — loop to reconnect.
		r.state.Set(StateReconnecting, "SSM session ended cleanly")
	}
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

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		log.Error("failed to load AWS config", "err", err)
		os.Exit(1)
	}

	runner := &Runner{
		cfg:   cfg,
		ec2:   ec2.NewFromConfig(awsCfg),
		rds:   rds.NewFromConfig(awsCfg),
		state: sw,
		log:   log,
	}

	runner.Run(ctx)
	log.Info("tunnel-runner stopped")
}
