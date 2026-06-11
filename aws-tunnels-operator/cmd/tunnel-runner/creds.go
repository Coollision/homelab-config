package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// credsDirEnv names the env var that switches the runner into refresh mode. When set, STS creds are
// read on demand from this directory — a mounted, operator-refreshed Secret — instead of from static
// AWS_* env vars baked into the pod at start. The operator is the sole holder of the SSO refresh
// token and keeps the directory's files current, so the pod picks up rotated creds in place without
// a restart, and the tunnel is not dropped every time the ~hourly STS creds roll over.
const credsDirEnv = "AWS_CREDS_DIR"

// renewEarly pulls each credential's effective expiry forward so the SDK's credential cache renews
// before the real STS expiry — reconnects then never resolve creds that are about to lapse. The
// operator refreshes the Secret well ahead of this, so fresh creds are already on disk by then.
const renewEarly = 2 * time.Minute

// secretFileCredentials is an aws.CredentialsProvider that reads STS creds from the files of a
// mounted Kubernetes Secret (keys AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY / AWS_SESSION_TOKEN /
// expiration). Wrapped in aws.NewCredentialsCache, Retrieve is re-invoked once the creds reach
// Expires, at which point it re-reads the (by then operator-refreshed) files.
type secretFileCredentials struct{ dir string }

func (p secretFileCredentials) Retrieve(_ context.Context) (aws.Credentials, error) {
	id, err := readCredFile(p.dir, "AWS_ACCESS_KEY_ID")
	if err != nil {
		return aws.Credentials{}, err
	}
	if id == "" {
		return aws.Credentials{}, fmt.Errorf("no AWS_ACCESS_KEY_ID in %s (credentials not provisioned yet)", p.dir)
	}
	secret, err := readCredFile(p.dir, "AWS_SECRET_ACCESS_KEY")
	if err != nil {
		return aws.Credentials{}, err
	}
	token, err := readCredFile(p.dir, "AWS_SESSION_TOKEN")
	if err != nil {
		return aws.Credentials{}, err
	}

	creds := aws.Credentials{
		AccessKeyID:     id,
		SecretAccessKey: secret,
		SessionToken:    token,
		Source:          "SecretFile",
	}
	if expRaw, _ := readCredFile(p.dir, "expiration"); expRaw != "" {
		if exp, perr := time.Parse(time.RFC3339, expRaw); perr == nil {
			creds.CanExpire = true
			creds.Expires = exp.Add(-renewEarly)
		}
	}
	return creds, nil
}

// readCredFile reads one credential file, returning "" (no error) when it is absent — a Secret whose
// keys have not been written yet reads as "not provisioned" rather than a hard failure.
func readCredFile(dir, name string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("read credential file %s: %w", name, err)
	}
	return strings.TrimSpace(string(b)), nil
}
