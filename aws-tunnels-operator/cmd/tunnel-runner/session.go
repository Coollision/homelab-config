package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// ssmParams is the parameter document for AWS-StartPortForwardingSessionToRemoteHost.
type ssmParams struct {
	Host            []string `json:"host"`
	PortNumber      []string `json:"portNumber"`
	LocalPortNumber []string `json:"localPortNumber"`
}

// authOutputKeywords are substrings in session-manager-plugin output that indicate
// an authentication failure. These are detected via string matching because the
// plugin is an external process, not an SDK call; SDK-level auth errors are
// handled separately by isAuthError via smithy.APIError.
var authOutputKeywords = []string{
	"ExpiredToken", "AuthFailure", "InvalidClientToken", "Unauthorized", "AccessDenied",
}

// runSSMSession starts an AWS SSM port-forwarding session and blocks until it exits.
// It returns (isAuthErr, error); error is nil on a clean exit.
func runSSMSession(ctx context.Context, instanceID, remoteHost string, cfg *Config) (isAuthErr bool, _ error) {
	params, err := json.Marshal(ssmParams{
		Host:            []string{remoteHost},
		PortNumber:      []string{cfg.RemotePort},
		LocalPortNumber: []string{cfg.LocalPort},
	})
	if err != nil {
		return false, fmt.Errorf("marshal ssm params: %w", err)
	}

	cmd := exec.CommandContext(ctx, "aws", "ssm", "start-session",
		"--region", cfg.AWSRegion,
		"--target", instanceID,
		"--document-name", "AWS-StartPortForwardingSessionToRemoteHost",
		"--parameters", string(params),
	)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	if err := cmd.Run(); err != nil {
		output := out.String()
		if outputContainsAuthKeyword(output) {
			return true, fmt.Errorf("SSM session auth failure: %w\n%s", err, output)
		}
		return false, fmt.Errorf("SSM session exited: %w\n%s", err, output)
	}
	return false, nil
}

func outputContainsAuthKeyword(s string) bool {
	lower := strings.ToLower(s)
	for _, kw := range authOutputKeywords {
		if strings.Contains(lower, strings.ToLower(kw)) {
			return true
		}
	}
	return false
}
