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

## Outstanding known issues / TODOs

- `aws sso login` stdout parsing is tied to current CLI output format. Consider switching to the AWS SDK SSO device-auth flow directly to avoid the subprocess dependency.
- The SSO wait page polls `/login-poll.json` every 3 seconds regardless of whether the user has already clicked the URL. No WebSocket/SSE is used; HTTP polling is intentional for simplicity.
- Tunnel Deployment restarts after login are done by patching a pod template annotation (`proxies.homelab.io/restartedAt`). This triggers a rolling restart. If the Deployment is at `replicas=0` (creds were invalid at last reconcile), the annotation patch has no immediate effect; the next reconcile cycle will scale it back up.
