package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	rdstypes "github.com/aws/aws-sdk-go-v2/service/rds/types"
	"github.com/aws/smithy-go"
	"log/slog"
)

// --- mock smithy.APIError ---

type mockAPIError struct{ code, message string }

func (e mockAPIError) Error() string           { return e.message }
func (e mockAPIError) ErrorCode() string       { return e.code }
func (e mockAPIError) ErrorMessage() string    { return e.message }
func (e mockAPIError) ErrorFault() smithy.ErrorFault { return smithy.FaultUnknown }

// --- mock AWS clients ---

type mockRDSClient struct {
	clusters  []rdstypes.DBCluster
	instances []rdstypes.DBInstance
	err       error
}

func (m *mockRDSClient) DescribeDBClusters(_ context.Context, _ *rds.DescribeDBClustersInput, _ ...func(*rds.Options)) (*rds.DescribeDBClustersOutput, error) {
	if m.err != nil {
		return nil, m.err
	}
	return &rds.DescribeDBClustersOutput{DBClusters: m.clusters}, nil
}

func (m *mockRDSClient) DescribeDBInstances(_ context.Context, _ *rds.DescribeDBInstancesInput, _ ...func(*rds.Options)) (*rds.DescribeDBInstancesOutput, error) {
	if m.err != nil {
		return nil, m.err
	}
	return &rds.DescribeDBInstancesOutput{DBInstances: m.instances}, nil
}

type mockEC2Client struct {
	reservations []ec2types.Reservation
	err          error
}

func (m *mockEC2Client) DescribeInstances(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	if m.err != nil {
		return nil, m.err
	}
	return &ec2.DescribeInstancesOutput{Reservations: m.reservations}, nil
}

// --- configFromEnv tests ---

func TestConfigFromEnv_MissingRequired(t *testing.T) {
	t.Setenv("BASTION_NAME", "")
	t.Setenv("REMOTE_PORT", "")
	_, err := configFromEnv()
	if err == nil {
		t.Fatal("expected error for missing required vars")
	}
	if !strings.Contains(err.Error(), "BASTION_NAME") {
		t.Errorf("expected BASTION_NAME in error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "REMOTE_PORT") {
		t.Errorf("expected REMOTE_PORT in error, got: %v", err)
	}
}

func TestConfigFromEnv_Defaults(t *testing.T) {
	t.Setenv("BASTION_NAME", "my-bastion")
	t.Setenv("REMOTE_PORT", "5432")
	t.Setenv("LOCAL_PORT", "")
	t.Setenv("TUNNEL_NAME", "")
	t.Setenv("AWS_REGION", "")

	cfg, err := configFromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.LocalPort != defaultLocalPort {
		t.Errorf("expected default LOCAL_PORT %q, got %q", defaultLocalPort, cfg.LocalPort)
	}
	if cfg.TunnelName != defaultTunnelName {
		t.Errorf("expected default TUNNEL_NAME %q, got %q", defaultTunnelName, cfg.TunnelName)
	}
	if cfg.AWSRegion != defaultRegion {
		t.Errorf("expected default AWS_REGION %q, got %q", defaultRegion, cfg.AWSRegion)
	}
}

func TestConfigFromEnv_Full(t *testing.T) {
	t.Setenv("BASTION_NAME", "bastion-prod")
	t.Setenv("REMOTE_PORT", "3306")
	t.Setenv("LOCAL_PORT", "13306")
	t.Setenv("TUNNEL_NAME", "mysql")
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("RDS_CLUSTER_PREFIX", "cluster-")
	t.Setenv("RDS_INSTANCE_PREFIX", "inst-")
	t.Setenv("REMOTE_HOST", "db.example.com")

	cfg, err := configFromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.BastionName != "bastion-prod" {
		t.Errorf("unexpected BastionName: %q", cfg.BastionName)
	}
	if cfg.AWSRegion != "us-east-1" {
		t.Errorf("unexpected AWSRegion: %q", cfg.AWSRegion)
	}
}

// --- outputContainsAuthKeyword tests ---

func TestOutputContainsAuthKeyword(t *testing.T) {
	cases := []struct {
		input string
		want  bool
	}{
		{"An error occurred (ExpiredToken) ...", true},
		{"Error: AuthFailure from service", true},
		{"InvalidClientToken provided", true},
		{"Unauthorized access", true},
		{"AccessDenied for this action", true},
		{"Connection reset by peer", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := outputContainsAuthKeyword(tc.input); got != tc.want {
			t.Errorf("outputContainsAuthKeyword(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

// --- isAuthError tests ---

func TestIsAuthError_AuthOutputErr(t *testing.T) {
	err := forceAuth(errors.New("process output auth failure"), true)
	if !isAuthError(err) {
		t.Error("expected isAuthError to return true for authOutputErr")
	}
}

func TestIsAuthError_SmithyAuthCodes(t *testing.T) {
	authCodes := []string{
		"ExpiredTokenException",
		"InvalidClientTokenId",
		"AuthFailure",
		"AccessDeniedException",
		"UnauthorizedException",
	}
	for _, code := range authCodes {
		err := mockAPIError{code: code, message: "test error"}
		if !isAuthError(err) {
			t.Errorf("isAuthError(%q) = false, want true", code)
		}
	}
}

func TestIsAuthError_NonAuthSmithyCode(t *testing.T) {
	err := mockAPIError{code: "InvalidParameterValue", message: "bad param"}
	if isAuthError(err) {
		t.Error("expected isAuthError to return false for non-auth error code")
	}
}

func TestIsAuthError_PlainError(t *testing.T) {
	if isAuthError(errors.New("generic error")) {
		t.Error("expected isAuthError to return false for plain error")
	}
}

func TestIsAuthError_Nil(t *testing.T) {
	if isAuthError(nil) {
		t.Error("expected isAuthError to return false for nil")
	}
}

// --- forceAuth tests ---

func TestForceAuth_Forced(t *testing.T) {
	base := errors.New("base error")
	wrapped := forceAuth(base, true)
	if !isAuthError(wrapped) {
		t.Error("expected wrapped error to be detected as auth error")
	}
}

func TestForceAuth_NotForced(t *testing.T) {
	base := errors.New("base error")
	result := forceAuth(base, false)
	if isAuthError(result) {
		t.Error("expected unforced error not to be detected as auth error")
	}
	if result != base {
		t.Error("expected forceAuth(err, false) to return original error unchanged")
	}
}

// --- filterClusters / filterInstances tests ---

func makeCluster(id, status string, createTime time.Time) rdstypes.DBCluster {
	return rdstypes.DBCluster{
		DBClusterIdentifier: aws.String(id),
		Status:              aws.String(status),
		Endpoint:            aws.String(id + ".endpoint.rds.amazonaws.com"),
		ClusterCreateTime:   &createTime,
	}
}

func makeInstance(id, status string, createTime time.Time) rdstypes.DBInstance {
	return rdstypes.DBInstance{
		DBInstanceIdentifier: aws.String(id),
		DBInstanceStatus:     aws.String(status),
		Endpoint:             &rdstypes.Endpoint{Address: aws.String(id + ".endpoint.rds.amazonaws.com")},
		InstanceCreateTime:   &createTime,
	}
}

func TestFilterClusters(t *testing.T) {
	now := time.Now()
	clusters := []rdstypes.DBCluster{
		makeCluster("prod-cluster-1", "available", now.Add(-2*time.Hour)),
		makeCluster("prod-cluster-2", "stopped", now.Add(-1*time.Hour)),
		makeCluster("staging-cluster", "available", now),
	}
	got := filterClusters(clusters, "prod-")
	if len(got) != 1 {
		t.Fatalf("expected 1 filtered cluster, got %d", len(got))
	}
	if aws.ToString(got[0].DBClusterIdentifier) != "prod-cluster-1" {
		t.Errorf("unexpected cluster: %q", aws.ToString(got[0].DBClusterIdentifier))
	}
}

func TestFilterInstances(t *testing.T) {
	now := time.Now()
	instances := []rdstypes.DBInstance{
		makeInstance("db-primary", "available", now.Add(-1*time.Hour)),
		makeInstance("db-replica", "available", now),
		makeInstance("other-db", "available", now),
	}
	got := filterInstances(instances, "db-")
	if len(got) != 2 {
		t.Fatalf("expected 2 filtered instances, got %d", len(got))
	}
}

// --- resolveRemoteHost tests ---

func TestResolveRemoteHost_StaticHost(t *testing.T) {
	cfg := &Config{RemoteHost: "static.db.example.com", RemotePort: "5432"}
	host, err := resolveRemoteHost(context.Background(), &mockRDSClient{}, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if host != "static.db.example.com" {
		t.Errorf("expected static host, got %q", host)
	}
}

func TestResolveRemoteHost_ClusterPrefix(t *testing.T) {
	now := time.Now()
	rdsClient := &mockRDSClient{
		clusters: []rdstypes.DBCluster{
			makeCluster("prod-cluster-old", "available", now.Add(-2*time.Hour)),
			makeCluster("prod-cluster-new", "available", now.Add(-1*time.Hour)),
		},
	}
	cfg := &Config{RDSClusterPrefix: "prod-", AWSRegion: "eu-west-1"}
	host, err := resolveRemoteHost(context.Background(), rdsClient, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The newest cluster should be selected.
	if !strings.Contains(host, "prod-cluster-new") {
		t.Errorf("expected newest cluster endpoint, got %q", host)
	}
}

func TestResolveRemoteHost_InstancePrefix(t *testing.T) {
	now := time.Now()
	rdsClient := &mockRDSClient{
		instances: []rdstypes.DBInstance{
			makeInstance("db-instance", "available", now),
		},
	}
	cfg := &Config{RDSInstancePrefix: "db-", AWSRegion: "eu-west-1"}
	host, err := resolveRemoteHost(context.Background(), rdsClient, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(host, "db-instance") {
		t.Errorf("expected instance endpoint, got %q", host)
	}
}

func TestResolveRemoteHost_NoConfig(t *testing.T) {
	cfg := &Config{}
	_, err := resolveRemoteHost(context.Background(), &mockRDSClient{}, cfg)
	if err == nil {
		t.Fatal("expected error when no host config is provided")
	}
}

func TestResolveRemoteHost_NoMatchingCluster(t *testing.T) {
	rdsClient := &mockRDSClient{clusters: []rdstypes.DBCluster{
		makeCluster("other-cluster", "available", time.Now()),
	}}
	cfg := &Config{RDSClusterPrefix: "prod-", AWSRegion: "eu-west-1"}
	_, err := resolveRemoteHost(context.Background(), rdsClient, cfg)
	if err == nil || !strings.Contains(err.Error(), "no available RDS cluster") {
		t.Fatalf("expected 'no available RDS cluster' error, got: %v", err)
	}
}

// --- resolveBastion tests ---

func TestResolveBastion_Found(t *testing.T) {
	instanceID := "i-0abc123def456"
	ec2Client := &mockEC2Client{
		reservations: []ec2types.Reservation{
			{Instances: []ec2types.Instance{{InstanceId: aws.String(instanceID)}}},
		},
	}
	cfg := &Config{BastionName: "prod-bastion"}
	id, err := resolveBastion(context.Background(), ec2Client, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != instanceID {
		t.Errorf("expected %q, got %q", instanceID, id)
	}
}

func TestResolveBastion_NoInstances(t *testing.T) {
	ec2Client := &mockEC2Client{reservations: []ec2types.Reservation{}}
	cfg := &Config{BastionName: "prod-bastion"}
	_, err := resolveBastion(context.Background(), ec2Client, cfg)
	if err == nil {
		t.Fatal("expected error for empty reservations")
	}
}

func TestResolveBastion_APIError(t *testing.T) {
	ec2Client := &mockEC2Client{err: errors.New("connection refused")}
	cfg := &Config{BastionName: "prod-bastion"}
	_, err := resolveBastion(context.Background(), ec2Client, cfg)
	if err == nil {
		t.Fatal("expected error for API failure")
	}
}

// --- StateWriter tests ---

func TestStateWriter_Set(t *testing.T) {
	dir := t.TempDir()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	sw, err := newStateWriter(dir, log)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	sw.Set(StateRunning, "test detail")

	content, err := os.ReadFile(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	if !strings.Contains(string(content), string(StateRunning)) {
		t.Errorf("expected state %q in file, got %q", StateRunning, string(content))
	}
}

func TestStateWriter_CreatesDir(t *testing.T) {
	// Use a non-existent nested dir to verify MkdirAll behaviour.
	dir := filepath.Join(t.TempDir(), "a", "b", "c")
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	_, err := newStateWriter(dir, log)
	if err != nil {
		t.Fatalf("expected newStateWriter to create dir, got: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("expected dir to exist: %v", err)
	}
}
