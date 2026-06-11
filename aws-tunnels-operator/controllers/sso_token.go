package controllers

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// credsRefreshAhead is how long before STS-cred expiry the reconcile loop proactively refreshes, so
// a running tunnel can be rolled onto fresh creds before the old ones lapse.
const credsRefreshAhead = 10 * time.Minute

// ssoCacheMu serializes access to the on-disk SSO token cache between the interactive login flow
// (which writes a freshly-minted cache, incl. a rotated refresh token) and the reconcile loop's
// seed/refresh/persist. Without it, a reconcile could re-seed the dir from the (older) Secret in the
// window after a login writes the new token but before it persists, silently dropping the rotation.
// The login flow takes it for its whole duration; the reconcile loop TryLocks and skips the refresh
// for that tick if a login holds it (it retries next tick).
var ssoCacheMu sync.Mutex

// tokenSecretName is the Secret holding the captured AWS SSO token cache (access token + refresh
// token + OIDC client registration). One per stack; captured by the in-cluster `aws sso login` (the
// auth-server login flow) and then kept rotated by the operator. This is an AWS-scoped, revocable
// token — NOT the corporate password.
func tokenSecretName(stackName string) string {
	return fmt.Sprintf("%s-sso-token", strings.ToLower(ProfileKey(stackName)))
}

// ssoCacheDir is the AWS CLI's SSO token cache directory. The CLI derives it from $HOME and it is
// NOT relocatable via env, so the operator pod sets HOME to a writable path (see chart deployment).
func ssoCacheDir() string {
	home := os.Getenv("HOME")
	if home == "" {
		home = "/root"
	}
	return filepath.Join(home, ".aws", "sso", "cache")
}

// seedTokenCache writes the captured token cache from the <stack>-sso-token Secret onto disk so the
// AWS CLI can read (and silently refresh) it. Returns seeded=false (no error) when the Secret is
// absent/empty — meaning there is nothing to refresh from yet (run a login). The returned version is
// the Secret's ResourceVersion, which changes on a fresh login; callers use it to detect a re-login.
func seedTokenCache(ctx context.Context, c client.Client, namespace, stackName string) (seeded bool, version string, err error) {
	secret := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: tokenSecretName(stackName)}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return false, "", nil
		}
		return false, "", fmt.Errorf("get sso token secret: %w", err)
	}
	if len(secret.Data) == 0 {
		return false, "", nil
	}
	dir := ssoCacheDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, "", fmt.Errorf("create sso cache dir: %w", err)
	}
	for name, content := range secret.Data {
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o600); err != nil {
			return false, "", fmt.Errorf("write sso cache file %s: %w", name, err)
		}
	}
	return true, secret.ResourceVersion, nil
}

// persistTokenCache writes the on-disk token cache back into the <stack>-sso-token Secret, but only
// when it differs. The AWS CLI rotates the refresh token on use, and that rotation must survive pod
// restarts or the long-lived session is lost. The Secret is intentionally NOT labelled with the
// stack label, so pruneStaleResources never considers it for deletion.
func persistTokenCache(ctx context.Context, c client.Client, namespace, stackName string) error {
	dir := ssoCacheDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read sso cache dir: %w", err)
	}
	data := map[string][]byte{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		content, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return fmt.Errorf("read sso cache file %s: %w", e.Name(), err)
		}
		data[e.Name()] = content
	}
	if len(data) == 0 {
		return nil
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: tokenSecretName(stackName), Namespace: namespace}}
	_, err = controllerutil.CreateOrUpdate(ctx, c, secret, func() error {
		secret.Type = corev1.SecretTypeOpaque
		if secretDataEqual(secret.Data, data) {
			return nil
		}
		secret.Data = data
		return nil
	})
	return err
}

func secretDataEqual(a, b map[string][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for k, va := range a {
		vb, ok := b[k]
		if !ok || !bytes.Equal(va, vb) {
			return false
		}
	}
	return true
}

// credsNeedRefresh reports whether a creds Secret is missing, expired, unparseable, or within the
// refresh-ahead window of expiry.
func credsNeedRefresh(secret *corev1.Secret, ahead time.Duration) bool {
	if secret == nil || secret.Data == nil {
		return true
	}
	expRaw, ok := secret.Data["expiration"]
	if !ok {
		return true
	}
	exp, err := time.Parse(time.RFC3339, string(expRaw))
	if err != nil {
		return true
	}
	return time.Now().UTC().Add(ahead).After(exp)
}

// refreshProfileCredentials mints fresh STS credentials for a profile from the cached SSO token
// (the AWS CLI refreshes the access token silently via the refresh token — no interactive login)
// and writes them into the profile's creds Secret. It returns true when the STS credentials
// actually changed, so the caller can roll that profile's tunnels onto the new creds.
//
// The token cache must already be seeded on disk (see seedTokenCache).
func (r *SingleStackRunner) refreshProfileCredentials(ctx context.Context, cfg StackConfig, profile, awsConfigText string, awsProfiles []string) (bool, error) {
	awsEnv, cleanup, err := buildAWSCLIEnv(profile, awsConfigText, awsProfiles)
	if err != nil {
		return false, err
	}
	defer cleanup()

	creds, err := exportProcessCredentials(ctx, profile, awsEnv)
	if err != nil {
		return false, err
	}

	secretName := credsSecretName(cfg.Name, profile)
	prevKey := ""
	existing := &corev1.Secret{}
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: cfg.Namespace, Name: secretName}, existing); err == nil && existing.Data != nil {
		prevKey = string(existing.Data["AWS_ACCESS_KEY_ID"])
	}
	if err := writeCredsSecretData(ctx, r.Client, cfg.Namespace, secretName, creds); err != nil {
		return false, err
	}
	return prevKey != creds.AccessKeyID, nil
}
