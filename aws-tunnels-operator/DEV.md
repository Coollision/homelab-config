# aws-tunnels-operator — Developer Notes

Running log of caveats, bugs, design decisions, and how they were resolved. Kept here so future work doesn't re-investigate the same ground.

---

## Architectural decisions

### No CRDs — single ConfigMap as desired state

The operator does not use a CRD. Desired state is read from a single ConfigMap (`stackName` + `spec.json` keys). This was intentional:

- Avoids CRD install/upgrade lifecycle in ArgoCD.
- Keeps the operator self-contained: one ConfigMap drives the whole stack.
- Helm renders `spec.json` from `values.yaml` using `toRawJson` (see issue below).

### Operator + auth server in one binary/pod

There is no separate auth sidecar. The `AuthServer` HTTP server is registered as a `manager.Runnable` alongside `SingleStackRunner`. Both share the same `client.Client` and are started/stopped by controller-runtime's signal handler. This simplifies RBAC and deployment.

### Credentials in Secrets, not PVCs

The old stack used a PVC to persist AWS credentials across pod restarts. The operator instead writes credentials into a Kubernetes Secret (`<stack>-creds-<profile>`). This allows:

- The reconcile loop to check credential validity without a running process.
- Standard Kubernetes tooling to inspect/rotate credentials.
- ArgoCD `ignoreDifferences` to suppress false diffs on Secret data.

### Tunnel Deployment scaling (0 or 1)

The operator does not delete tunnel Deployments when credentials expire — it scales them to `replicas=0`. This preserves the Deployment resource (and its history/events) and avoids the create/delete churn on every login cycle.

---

## Issues and fixes

### `spec.json` AVP placeholders getting JSON-encoded by Helm

**Problem:** The Helm chart renders `spec.json` into a ConfigMap using `{{ ... | toJson }}`. When ArgoCD Vault Plugin (AVP) secret placeholders like `<secret:path#key>` were present in values, `toJson` HTML-escaped the angle brackets (`<` → `\u003c`, `>` → `\u003e`). AVP then couldn't match the placeholder pattern and left the literal escaped string in the rendered manifest.

**Fix:** Replace `toJson` with `toRawJson` in `chart/templates/stack-configmap.yaml`. `toRawJson` skips HTML escaping and preserves `<...>` characters verbatim.

```yaml
# stack-configmap.yaml
spec.json: |
  {{ dict "aws" .Values.stack.aws ... | toRawJson }}
```

**Commit:** `fix(chart): use toRawJson to preserve AVP secret placeholders in spec.json`

---

### ArgoCD continuously pruning operator-managed resources

**Problem (attempt 1 — label only):** Added `app.kubernetes.io/instance: <argoAppName>` label to all operator resources. ArgoCD in `label` tracking mode picked these up and started trying to reconcile/prune them.

**Problem (attempt 2 — self-referencing annotation):** Added `argocd.argoproj.io/tracking-id: <app>:apps/Deployment:<ns>/<name>` annotations pointing each resource at itself. ArgoCD interpreted this as owning the resource. Since operator-created Deployments/Services/Secrets/IngressRoutes are not in Git, ArgoCD flagged them for pruning on every sync.

**Fix (attempt 3 — non-self-referencing annotation):** Per [ArgoCD docs on non-self-referencing annotations](https://argo-cd.readthedocs.io/en/stable/user-guide/resource_tracking/#non-self-referencing-annotations):

> "If the tracking annotation does not reference the resource it is applied to, the resource will neither affect the application's sync status nor be marked for pruning. Copied resources will be visible on the UI at top level."

All operator resources now have their `tracking-id` point at the **stack ConfigMap** (which ArgoCD already manages from Git):

```
argocd.argoproj.io/tracking-id: aws-tunnels:/ConfigMap:proxies/aws-tunnel-config
```

ArgoCD shows them in the app tree as "copied resources" with no sync status — visible but not managed/pruned.

**Commit:** `fix(operator): use non-self-referencing tracking-id to prevent argocd pruning operator resources`

**Key insight:** The annotation value format is `<appName>:<group>/<kind>:<namespace>/<name>`. The "non-self-referencing" trick works because ArgoCD compares the annotation's `(kind, ns, name)` tuple against the resource it's applied to. If they don't match, ArgoCD treats it as a viewer annotation only.

---

### `copyCode is not defined` on the SSO wait page

**Problem:** The SSO wait page (`/login-wait`) has a "Copy" button that calls `onclick="copyCode()"`. The `copyCode` function was defined inside an IIFE:

```js
(function() {
  // ...
  function copyCode() { ... }  // not reachable from onclick=""
})();
```

`onclick` attribute handlers resolve names in the global scope (`window`). Functions declared with `function` inside an IIFE are scoped to that IIFE and are invisible globally.

**Fix:** Assign the function to `window`:

```js
window.copyCode = function copyCode() { ... }
```

This exposes it globally while keeping the named function reference (useful in stack traces) and leaving the rest of the IIFE private.

**Commit:** `fix(auth-server): expose copyCode to global scope so onclick handler can reach it`

---

### `toRawJson` vs `toJson` in Helm — general rule

Helm's `toJson` uses Go's `encoding/json` with HTML-safe encoding enabled, which escapes `<`, `>`, and `&`. `toRawJson` disables HTML escaping. Always use `toRawJson` when the JSON output will be processed by another tool that does its own string matching (AVP, Vault, etc.).

---

### Multi-arch image build — binaries must be pre-built outside Docker

**Problem:** `docker buildx` cross-compilation inside a Go Dockerfile is slow (emulated QEMU) and requires a heavy build image.

**Fix:** Build Go binaries for `linux/amd64` and `linux/arm64` on the CI host first, then `COPY` them into a thin runtime image:

```dockerfile
FROM amazon/aws-cli:2.27.9
ARG TARGETARCH
COPY dist/manager-linux-${TARGETARCH} /manager
RUN chmod +x /manager
ENTRYPOINT ["/manager"]
```

The base image is `amazon/aws-cli` (not `scratch` or `alpine`) because `aws` CLI must be available at runtime to run `aws sso login` and `aws configure export-credentials`.

---

### `aws configure export-credentials` output format

The auth server calls `aws configure export-credentials --profile <profile> --format process`. The JSON output follows the [credential process format](https://docs.aws.amazon.com/cli/latest/userguide/cli-configure-sourcing-external.html):

```json
{
  "Version": 1,
  "AccessKeyId": "...",
  "SecretAccessKey": "...",
  "SessionToken": "...",
  "Expiration": "2026-04-28T12:00:00Z"
}
```

Note capital-case keys (`AccessKeyId`, not `accessKeyId`). The Go struct `exportCredentials` matches these exactly. The `Expiration` field is stored as-is into the Secret's `expiration` key and parsed by `IsCredentialValid()` using `time.RFC3339`.

---

### SSO login stdout parsing

`aws sso login --no-browser` writes the verification URL and user code to stdout, not stderr. The auth server reads stdout line by line using a `bufio.Scanner` in a goroutine, looking for:

- Lines containing `https://` → parsed as the verification URL
- Lines matching an 8-character alphanumeric pattern → parsed as the user code

This is brittle against AWS CLI output format changes. If the SSO wait page shows a spinner but no URL or code, check whether the AWS CLI version has changed its output format.

---

### Profile key sanitisation

AWS profile names can contain `/`, `:`, `@`, spaces, and backslashes (e.g., SSO profile names). These characters are not valid in Kubernetes resource names. `ProfileKey()` replaces all of them with `_` before using the profile name as a Secret name suffix:

```go
func ProfileKey(profile string) string {
    replacer := strings.NewReplacer("/", "_", ":", "_", " ", "_", "@", "_", "\\", "_")
    return replacer.Replace(profile)
}
```

Secret names also go through `strings.ToLower` to ensure DNS-label compliance.

---

### Tunnel pod AWS config injection

Each tunnel Deployment needs an `~/.aws/config` file for the profile. Rather than mounting a large shared ConfigMap, the operator generates a per-profile config inline and writes it into an `aws-config` volume (projected from a ConfigMap). The config hash is added as a pod template annotation (`proxies.homelab.io/awsConfigHash`) so pod restarts happen automatically when the AWS config changes.

---

### Helm orphan branch + Renovate schedule

GitHub Actions scheduled workflows only run from the **default branch** of the repository. Since this operator lives on an orphan branch (`orphan/aws-tunnels-operator-standalone`), the `renovate.yaml` workflow file must also exist on `master` (or the repo default branch). The `renovate.json` config targets the orphan branch via `baseBranches`. Both files must be kept in sync.

---

### `sso-session` config is required for refresh tokens (silent auto-refresh)

The operator renders `~/.aws/config` in the **`sso-session`** format, not the legacy inline format:

```ini
[profile dev]
sso_session = aws-tunnels
sso_account_id = ...
sso_role_name  = ...

[sso-session aws-tunnels]
sso_start_url = ...
sso_region    = ...
sso_registration_scopes = sso:account:access
```

**Why it matters:** only the `sso-session` format with `sso_registration_scopes =
sso:account:access` makes the AWS CLI register an OIDC client that is issued a **refresh token**,
which it stores in the token cache. The legacy inline form (`sso_start_url` directly in `[profile]`)
yields a short-lived access token with no refresh token — so `aws configure export-credentials`
can't silently renew, and unattended refresh is impossible. This switch (in `RenderAWSConfig`) is
the single change that the whole captured-token model depends on.

The token cache file is named `sha1hex(<sso-session name>).json`. The operator names the session
after the stack and runs its own in-cluster `aws sso login` under that same name, so the cache key
always matches — no external coordination needed.

**Gated behind `stack.aws.useRefresh` (default false).** When false, `RenderAWSConfig` emits the
legacy inline format, the reconcile auto-refresh block is skipped, and the login path behaves as
before — i.e. exactly the prior behavior. When true, it emits sso-session, enables the
seed/refresh/persist loop, and the login adds `--use-device-code`.

**`--use-device-code` is mandatory for the in-cluster login under sso-session.** With sso-session
config, `aws sso login --no-browser` defaults to the **authorization-code + PKCE** flow, whose
`redirect_uri` is `http://127.0.0.1:<port>/oauth/callback` — a listener on *the machine running the
CLI*. That works on a laptop (browser + CLI co-located) but not in-cluster (the user's browser would
redirect to its own localhost, not the operator pod). `--use-device-code` forces the RFC 8628
device-authorization grant (verification URL + user code, approved on any device, operator polls for
completion) — and it still yields a refresh token, because the refresh token is tied to the client
registration (`sso_registration_scopes`), independent of grant type. `ssoLoginArgs()` adds the flag
only when UseRefresh is set.
After a successful in-cluster login, the login handler calls `persistTokenCache` so the pod's fresh
cache (with refresh token) survives restarts.

### SSO token cache dir is `$HOME`-derived, not relocatable

The AWS CLI keeps the SSO token cache at `~/.aws/sso/cache` and derives it from `$HOME` — there is
no env var to relocate it (unlike `AWS_CONFIG_FILE`). So the operator pod sets `HOME=/operator-home`
and mounts a writable `emptyDir` at `/operator-home/.aws/sso/cache`. The operator seeds that dir
from the `<stack>-sso-token` Secret each reconcile and persists rotations back — durable state lives
in the Secret, not a PVC (consistent with the "Secrets, not PVCs" decision above). The refresh
token rotates on use, so `persistTokenCache` must write the rotated cache back or the long-lived
session is lost on the next pod restart.

### How refreshed creds reach the tunnel — in-place (refresh mode) vs roll (legacy)

How STS creds get from the operator-managed `<stack>-<profile>` Secret into the tunnel pod depends on
`stack.aws.useRefresh`:

**Refresh mode (`useRefresh: true`) — in-place, no roll.** The creds Secret is mounted read-only at
`/var/run/aws-creds` and the operator sets `AWS_CREDS_DIR` to it (no `envFrom`, no SSO `aws-config` in
the pod — those would shadow/short-circuit file-based resolution). The tunnel-runner uses
`secretFileCredentials` (an `aws.CredentialsProvider` wrapped in `aws.NewCredentialsCache`) for the Go
SDK, and hands the same freshly-resolved creds to each `aws ssm start-session` subprocess via its env.
When the operator rotates the Secret, Kubernetes propagates the change into the mounted volume and the
provider re-reads it on the next expiry — so the pod is **never rolled** for a creds rotation. This is
deliberate: an already-running SSM session rides an SSM-issued session token (not the STS creds) and
survives STS expiry, and reconnects resolve fresh creds with no operator involvement. The provider
renews `renewEarly` (2 min) before the real expiry so a reconnect never grabs about-to-lapse creds.

Critically, **only the operator ever refreshes from the SSO cache** — SSO refresh tokens rotate on
use, so multiple independent refreshers would clobber each other's token. Pods only ever receive
short-lived STS creds, never the SSO token cache.

**Legacy mode (`useRefresh: false`) — roll.** Creds reach the pod via `envFrom`, and Kubernetes does
not hot-reload env vars, so a refresh that changes the creds rolls the affected profile's tunnel
Deployments (stamping `proxies.homelab.io/restartedAt`); the roll only fires when the access key id
actually changed, to avoid a restart loop every tick.

Both modes pin `RollingUpdate` `maxSurge=1/maxUnavailable=0` with a readiness probe gated on the SSM
tunnel being up, so any roll (a config change, or a creds roll in legacy mode) brings the replacement
up before retiring the old pod — gap-free for new connections; existing TCP streams reset once at the
swap.

### Manual stop/start annotation

`proxies.homelab.io/manuallyStopped: "true"` on a tunnel Deployment forces `replicas=0` even when
creds are valid. The reconcile mutate reads it (it persists across reconciles because CreateOrUpdate
fetches the existing object first) and the `/tunnel-toggle` endpoint sets/clears it. The
`<stack>-sso-token` Secret and the manual-stop annotation are both deliberately kept out of the
pruner's deletion path (the token Secret is added to `desiredSecrets`).

## Outstanding known issues / TODOs

- `aws sso login` stdout parsing is tied to current CLI output format. Consider switching to the AWS SDK SSO device-auth flow directly to avoid the subprocess dependency. (Less pressing now that the captured-token + auto-refresh path means interactive login is rare — usually only on the first login and when the SSO session finally expires.)
- Unattended lifetime is bounded by the AWS IAM Identity Center session duration (admin-set). There is no operator-side control over it; when the refresh token is rejected, tunnels scale to 0 and the UI flags a needed re-login.
- The SSO wait page polls `/login-poll.json` every 3 seconds regardless of whether the user has already clicked the URL. No WebSocket/SSE is used; HTTP polling is intentional for simplicity.
- Tunnel Deployment restarts after login are done by patching a pod template annotation (`proxies.homelab.io/restartedAt`). This triggers a rolling restart. If the Deployment is at `replicas=0` (creds were invalid at last reconcile), the annotation patch has no immediate effect; the next reconcile cycle will scale it back up.
