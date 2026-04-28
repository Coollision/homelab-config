package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	rdstypes "github.com/aws/aws-sdk-go-v2/service/rds/types"
	"github.com/aws/smithy-go"
)

// EC2Client is the minimal ec2.Client surface required by the resolver.
// Defined as an interface to allow substitution in tests.
type EC2Client interface {
	DescribeInstances(ctx context.Context, input *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
}

// RDSClient is the minimal rds.Client surface required by the resolver.
type RDSClient interface {
	DescribeDBClusters(ctx context.Context, input *rds.DescribeDBClustersInput, optFns ...func(*rds.Options)) (*rds.DescribeDBClustersOutput, error)
	DescribeDBInstances(ctx context.Context, input *rds.DescribeDBInstancesInput, optFns ...func(*rds.Options)) (*rds.DescribeDBInstancesOutput, error)
}

// authOutputErr wraps an error originating from process-output scanning rather
// than an SDK call, so the caller can still route it through isAuthError.
type authOutputErr struct{ cause error }

func (e authOutputErr) Error() string { return e.cause.Error() }
func (e authOutputErr) Unwrap() error { return e.cause }

// forceAuth wraps err as an auth error when forced is true, otherwise returns it unchanged.
func forceAuth(err error, forced bool) error {
	if forced {
		return authOutputErr{err}
	}
	return err
}

// isAuthError reports whether err represents an AWS authentication or
// authorisation failure, whether detected via SDK error codes or via
// process-output scanning (authOutputErr).
func isAuthError(err error) bool {
	if err == nil {
		return false
	}
	var out authOutputErr
	if errors.As(err, &out) {
		return true
	}
	var ae smithy.APIError
	if !errors.As(err, &ae) {
		return false
	}
	switch ae.ErrorCode() {
	case "ExpiredTokenException", "InvalidClientTokenId",
		"AuthFailure", "AccessDeniedException", "UnauthorizedException":
		return true
	}
	return false
}

// resolveRemoteHost determines the tunnel target host in priority order:
//  1. RDS cluster prefix  → endpoint of the newest available cluster
//  2. RDS instance prefix → endpoint of the newest available instance
//  3. Static REMOTE_HOST env var
func resolveRemoteHost(ctx context.Context, c RDSClient, cfg *Config) (string, error) {
	switch {
	case cfg.RDSClusterPrefix != "":
		return clusterEndpoint(ctx, c, cfg.AWSRegion, cfg.RDSClusterPrefix)
	case cfg.RDSInstancePrefix != "":
		return instanceEndpoint(ctx, c, cfg.AWSRegion, cfg.RDSInstancePrefix)
	case cfg.RemoteHost != "":
		return cfg.RemoteHost, nil
	default:
		return "", fmt.Errorf("one of REMOTE_HOST, RDS_CLUSTER_PREFIX, or RDS_INSTANCE_PREFIX is required")
	}
}

func clusterEndpoint(ctx context.Context, c RDSClient, region, prefix string) (string, error) {
	out, err := c.DescribeDBClusters(ctx, &rds.DescribeDBClustersInput{})
	if err != nil {
		return "", fmt.Errorf("describe db clusters: %w", err)
	}

	available := filterClusters(out.DBClusters, prefix)
	if len(available) == 0 {
		return "", fmt.Errorf("no available RDS cluster with prefix %q in region %s", prefix, region)
	}

	// Sort ascending by create time; take the last (newest) — mirrors sort_by(@, &ClusterCreateTime)[-1].
	sort.Slice(available, func(i, j int) bool {
		ti, tj := available[i].ClusterCreateTime, available[j].ClusterCreateTime
		return ti != nil && tj != nil && ti.Before(*tj)
	})

	ep := aws.ToString(available[len(available)-1].Endpoint)
	if ep == "" {
		return "", fmt.Errorf("RDS cluster with prefix %q has no endpoint", prefix)
	}
	return ep, nil
}

func filterClusters(clusters []rdstypes.DBCluster, prefix string) []rdstypes.DBCluster {
	out := make([]rdstypes.DBCluster, 0, len(clusters))
	for _, cl := range clusters {
		if strings.HasPrefix(aws.ToString(cl.DBClusterIdentifier), prefix) &&
			aws.ToString(cl.Status) == "available" {
			out = append(out, cl)
		}
	}
	return out
}

func instanceEndpoint(ctx context.Context, c RDSClient, region, prefix string) (string, error) {
	out, err := c.DescribeDBInstances(ctx, &rds.DescribeDBInstancesInput{})
	if err != nil {
		return "", fmt.Errorf("describe db instances: %w", err)
	}

	available := filterInstances(out.DBInstances, prefix)
	if len(available) == 0 {
		return "", fmt.Errorf("no available RDS instance with prefix %q in region %s", prefix, region)
	}

	// Sort ascending; take newest — mirrors sort_by(@, &InstanceCreateTime)[-1].
	sort.Slice(available, func(i, j int) bool {
		ti, tj := available[i].InstanceCreateTime, available[j].InstanceCreateTime
		return ti != nil && tj != nil && ti.Before(*tj)
	})

	ep := available[len(available)-1].Endpoint
	if ep == nil || aws.ToString(ep.Address) == "" {
		return "", fmt.Errorf("RDS instance with prefix %q has no endpoint address", prefix)
	}
	return aws.ToString(ep.Address), nil
}

func filterInstances(instances []rdstypes.DBInstance, prefix string) []rdstypes.DBInstance {
	out := make([]rdstypes.DBInstance, 0, len(instances))
	for _, inst := range instances {
		if strings.HasPrefix(aws.ToString(inst.DBInstanceIdentifier), prefix) &&
			aws.ToString(inst.DBInstanceStatus) == "available" {
			out = append(out, inst)
		}
	}
	return out
}

// resolveBastion returns the EC2 instance ID of the running bastion with the given Name tag.
func resolveBastion(ctx context.Context, c EC2Client, cfg *Config) (string, error) {
	out, err := c.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		Filters: []ec2types.Filter{
			{Name: aws.String("tag:Name"), Values: []string{cfg.BastionName}},
			{Name: aws.String("instance-state-name"), Values: []string{"running"}},
		},
	})
	if err != nil {
		return "", fmt.Errorf("describe instances for bastion %q: %w", cfg.BastionName, err)
	}

	if len(out.Reservations) == 0 || len(out.Reservations[0].Instances) == 0 {
		return "", fmt.Errorf("no running EC2 instance with Name=%q", cfg.BastionName)
	}

	id := aws.ToString(out.Reservations[0].Instances[0].InstanceId)
	if id == "" {
		return "", fmt.Errorf("bastion %q returned an empty instance ID", cfg.BastionName)
	}
	return id, nil
}
