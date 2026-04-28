# aws-tunnels-operator

Go-based Kubernetes operator that manages AWS SSO tunnel stacks: it authenticates against AWS SSO, stores short-lived credentials in Kubernetes Secrets, and keeps per-tunnel Deployments scaled up or down based on whether credentials are valid.

- Go operator source code lives here
- Helm chart: `chart/`
- Auth/API reference: `API.md`
- Developer notes (caveats, issues, fixes): `DEV.md`

---

## How it works

### Architecture

The operator and its built-in auth HTTP server run in a **single pod**. There are no CRDs and no PVCs.

```
┌──────────────────────────────────────────────────────┐
│  aws-tunnels-operator pod                            │
│                                                      │
│  ┌──────────────────┐   ┌───────────────────────┐   │
│  │  SingleStackRunner│   │  AuthServer (:8090)   │   │
│  │  (reconcile loop) │   │  GET /                │   │
│  │  every 30 s       │   │  POST /login          │   │
│  └────────┬─────────┘   │  GET /login-wait       │   │
│           │              │  GET /login-poll.json  │   │
│           │              │  POST /restart         │   │
│           │              │  GET /healthz          │   │
│           │              └───────────────────────┘   │
└───────────┼──────────────────────────────────────────┘
            │ reads
            ▼
    ConfigMap: aws-tunnels-operator-stack
      stackName: aws-tunnels
      spec.json: { aws, tunnels, tunnelDefaults, … }
            │
            │ creates/updates
            ▼
    ┌─────────────────────────────────────────────┐
    │  Per-profile Secret  <stack>-creds-<profile>│
    │    AWS_ACCESS_KEY_ID / SECRET / TOKEN        │
    │    expiration (RFC3339)                      │
    └──────────────────────┬──────────────────────┘
                           │ drives replicas (0 or 1)
                           ▼
    Per-tunnel Deployment  <stack>-<tunnel>
    Per-tunnel Service     <stack>-<tunnel>
    Per-tunnel IngressRoute(TCP)  <stack>-<tunnel>
```

### Reconcile loop

`SingleStackRunner` runs on a 30-second ticker. On each tick it:

1. Reads the stack ConfigMap (`stackName` + `spec.json`).
2. For each unique AWS profile, upserts a creds Secret (creating it empty if it doesn't exist).
3. For each tunnel, reads the profile creds Secret and calls `IsCredentialValid()` (checks `expiration` field).
4. Creates or updates the tunnel Deployment with `replicas=1` if creds are valid, `replicas=0` otherwise.
5. Creates or updates the tunnel Service and IngressRoute/IngressRouteTCP.

### Login flow

The auth server handles AWS SSO login out-of-band:

1. User hits `POST /login` (JSON body or HTML form).
2. The server spawns `aws sso login --profile <profile> --no-browser` in a goroutine.
3. It parses stdout for the verification URL and user code.
4. The user is redirected to `GET /login-wait?sid=<id>`, which polls `GET /login-poll.json?sid=<id>`.
5. When login completes, the server runs `aws configure export-credentials --profile <profile> --format process`.
6. Exported credentials are written into the creds Secret.
7. Tunnel Deployments for that profile are restarted by patching the pod template annotation.
8. The wait page redirects to `/` on success.

### Credential storage

Per-profile credentials are stored in Secrets named `<stack>-creds-<profile>`:

| Key | Value |
|-----|-------|
| `AWS_ACCESS_KEY_ID` | from `aws configure export-credentials` |
| `AWS_SECRET_ACCESS_KEY` | from `aws configure export-credentials` |
| `AWS_SESSION_TOKEN` | from `aws configure export-credentials` |
| `expiration` | RFC3339 timestamp |

When the expiration timestamp is in the past (or missing), `IsCredentialValid()` returns `false` and the tunnel Deployment is scaled to 0.

### Tunnel pod structure

Each tunnel Deployment runs two containers:

- **aws-cli** (`amazon/aws-cli`): establishes the SSM port-forwarding session.
- **socat** (`alpine/socat`): proxies the local port to the tunnel endpoint.

Credentials are injected via `envFrom` referencing the profile Secret. An AWS config file is mounted from a per-profile ConfigMap at `/aws-config/config`.

---

## Configuration

### Helm values

```yaml
# values.yaml key reference

image:
  repository: ghcr.io/coollision/aws-tunnels-operator
  tag: v0.7.1           # always a pinned semver — never "latest"
  pullPolicy: IfNotPresent

# Stack config ConfigMap name (must match stackConfig.name)
stackConfig:
  name: aws-tunnels-operator-stack

# ArgoCD integration — see "ArgoCD" section below
argoApp:
  name: ""              # defaults to .Release.Name

# Stack desired state (rendered into the ConfigMap as spec.json)
stack:
  name: aws-tunnels
  aws:
    profile: awsprofile001
    region: eu-west-1
    ssoStartUrl: ""
    accountId: ""
    roleName: ""
  # tunnelDefaults, tunnels, auth, nodeAffinity, etc. — see below
```

### Stack spec (`spec.json`)

The full stack shape is defined in `api/v1alpha1/awstunnelstack_types.go`. Key sections:

#### `aws` — default AWS profile

```json
{
  "aws": {
    "profile": "awsprofile001",
    "region": "eu-west-1",
    "ssoStartUrl": "https://my-sso.awsapps.com/start",
    "accountId": "123456789012",
    "roleName": "MyRole",
    "extraProfiles": []
  }
}
```

Use `extraProfiles` to define additional named AWS profiles for multi-account setups.

#### `tunnelDefaults` — shared defaults for all tunnels

```json
{
  "tunnelDefaults": {
    "image": "amazon/aws-cli:2.27.9",
    "proxyImage": "alpine/socat:1.8.0.3",
    "servicePort": 8080,
    "resources": {
      "requests": { "cpu": "50m", "memory": "64Mi" }
    }
  }
}
```

#### `tunnels` — list of tunnel definitions

```json
{
  "tunnels": [
    {
      "name": "rds",
      "host": "rds.internal.example.com",
      "bastionName": "my-bastion",
      "remoteHost": "my-db.cluster-xyz.eu-west-1.rds.amazonaws.com",
      "remotePort": "5432",
      "localPort": 5432,
      "awsProfile": "",        // overrides stack-level aws.profile if set
      "awsRegion": "",         // overrides stack-level aws.region if set
      "ingressMode": "http",   // "http" (IngressRoute) or "tcp" (IngressRouteTCP)
      "tls": { "passthrough": false }
    }
  ]
}
```

`ingressMode: "tcp"` creates a Traefik `IngressRouteTCP` with SNI matching; `"http"` (default) creates an `IngressRoute` with `Host()` matching.

### Environment variables

The following environment variables are set automatically by the Helm chart:

| Variable | Source | Purpose |
|----------|--------|---------|
| `POD_NAMESPACE` | `fieldRef: metadata.namespace` | Namespace where tunnel resources are managed |
| `STACK_CONFIGMAP_NAME` | `values.stackConfig.name` | Name of the stack spec ConfigMap |
| `ARGOCD_APP_NAME` | `values.argoApp.name` or `.Release.Name` | ArgoCD app name for resource tracking annotations |

---

## ArgoCD integration

The operator labels and annotates every resource it manages so they appear in the ArgoCD application tree.

### Labels applied

```
app.kubernetes.io/managed-by: aws-tunnels-operator
app.kubernetes.io/instance: <argoAppName>    # same as ArgoCD app name
proxies.homelab.io/stack: <stackName>
```

### Tracking annotation

```
argocd.argoproj.io/tracking-id: <appName>:/ConfigMap:<namespace>/<configMapName>
```

This is a **non-self-referencing** annotation — it points at the stack ConfigMap rather than at the resource itself. ArgoCD shows the tunnel resources in the UI tree but **does not diff or prune them** (they are not in Git and are managed exclusively by the operator). See [ArgoCD resource tracking docs](https://argo-cd.readthedocs.io/en/stable/user-guide/resource_tracking/#non-self-referencing-annotations).

Set `argoApp.name: ""` (the default) to derive the app name from `.Release.Name`, or set it explicitly if your ArgoCD app name differs from the Helm release name. Set it to a non-empty value if you need a fixed override; the operator skips all ArgoCD labels/annotations when `ARGOCD_APP_NAME` is empty.

### ArgoCD ignoreDifferences

The creds Secrets are written by the operator at runtime, so ArgoCD will see them as drifted. Add `ignoreDifferences` to the Application:

```yaml
ignoreDifferences:
  - group: ""
    kind: Secret
    matchLabels:
      proxies.homelab.io/stack: aws-tunnels
    jsonPointers:
      - /data
```

---

## Deploy

```bash
helm upgrade --install aws-tunnels \
  oci://ghcr.io/coollision/charts/aws-tunnels-operator \
  --version 0.7.1 \
  -n proxies --create-namespace \
  -f my-values.yaml
```

Or from local chart:

```bash
helm upgrade --install aws-tunnels ./chart -n proxies --create-namespace -f my-values.yaml
```

---

## Auth UI

`GET /` renders a dashboard with:

- Per-profile login buttons (shows credential expiry and validity)
- Tunnel status table: name, profile, bastion, host URL (clickable), ready replicas, restart count
- Stack restart form

The SSO wait page (`/login-wait`) shows the verification URL and user code with a one-click copy button and a direct login link (URL with `user_code` pre-filled).

---

## API reference

See `API.md` for the full HTTP API reference.

---

## Tests

```bash
cd aws-tunnels-operator
go test ./...
```

---

## Build

### Multi-arch image (CI approach — build outside Docker)

```bash
mkdir -p dist
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o dist/manager-linux-amd64 ./
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o dist/manager-linux-arm64 ./

docker buildx build \
  --platform linux/amd64,linux/arm64 \
  -t ghcr.io/coollision/aws-tunnels-operator:dev \
  --push .
```

The `Dockerfile` copies the pre-built binary from `dist/manager-linux-${TARGETARCH}` into the `amazon/aws-cli` base image (so `aws` CLI is available at runtime).

### Local single-arch

```bash
go build -o manager ./
```

---

## Versioning and releases

- CI enforces [Conventional Commits](https://www.conventionalcommits.org/) for operator-related changes.
- `feat:` → minor bump; `fix:`/`chore:`/`refactor:` → patch bump; `!`/`BREAKING CHANGE:` → major bump.
- `chart/Chart.yaml` `version`, `appVersion`, and `values.yaml` `image.tag` are kept in sync automatically by CI.
- Images are published with immutable tags: `vX.Y.Z` and `sha-<commit>`. `latest` is never published.
- CI creates and pushes release tags as `aws-tunnels-operator/vX.Y.Z` after a successful pipeline run.

---

## Renovate

Dependency updates run via Renovate with `baseBranches: [orphan/aws-tunnels-operator-standalone]`.

> **Note:** GitHub Actions schedules only trigger from the repository default branch. The `.github/workflows/renovate.yaml` workflow file must also exist on the default branch (`master`), while `renovate.json` keeps `baseBranches` targeting this orphan branch.
