# aws-tunnels-operator

Go-based Kubernetes operator replacing the old Helm/script-based aws-tunnels stack.

This folder is a standalone project in the monorepo with:

- Go operator source code
- Helm chart at `chart/`
- Operator/auth API docs in `API.md`

## What changed

- No PVC for credentials.
- Operator and auth API run in one pod.
- Auth endpoint writes short-lived AWS credentials into per-profile Kubernetes Secrets.
- Tunnel Deployments scale to `0` when creds are missing/expired, and to `1` when valid.
- Single-stack desired state from ConfigMap (`stackName` + `spec.json`) instead of CRDs.

## Login flow

Use the operator auth endpoint:

```bash
curl -X POST https://aws-auth.<domain>/login \
  -H 'content-type: application/json' \
  -d '{"namespace":"proxies","stack":"aws-tunnels","profile":"awsprofile001"}'
```

The operator executes:

- `aws sso login --profile <profile> --no-browser`
- `aws configure export-credentials --profile <profile> --format process`

Then stores exported values in secret:

- `<stack>-creds-<profile>`
- keys: `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_SESSION_TOKEN`, `expiration`

## Argo CD note

If you want Argo to show auth/credential secrets but ignore runtime data drift, set ignore differences on secret `data` for label `proxies.homelab.io/stack=aws-tunnels`.

## Deploy

```bash
helm upgrade --install aws-tunnels-operator tests/proxies/aws-tunnels-operator/chart -n proxies --create-namespace
```

## API

See `API.md` for:

- ConfigMap stack spec shape (`stackName` + `spec.json`)
- Auth API/UI (`GET /`, `POST /login`, `GET /login-wait`, `GET /login-poll.json`, `POST /restart`, `GET /healthz`)

## Tests

```bash
go test ./...
```

## Build Multi-Arch Image

The container image now copies prebuilt Go binaries from `dist/` for each target architecture.

Local example:

```bash
mkdir -p dist
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o dist/manager-linux-amd64 ./
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o dist/manager-linux-arm64 ./
docker buildx build --platform linux/amd64,linux/arm64 -t ghcr.io/coollision/aws-tunnels-operator:dev --load .
```

## Versioning And Releases

- CI enforces Conventional Commit messages for operator-related changes.
- CI automatically bumps chart `version` and `appVersion` based on Conventional Commit messages on branch pushes.
- `values.yaml` image tag is kept in sync as `vX.Y.Z`.
- Container images are published with immutable tags:
  - `vX.Y.Z`
  - `sha-<commit>`
- `latest` is intentionally not published.
- CI automatically creates and pushes release tags as `aws-tunnels-operator/vX.Y.Z`.

## Renovate Automation

- Dependency updates are configured in `.github/renovate.json` with `baseBranches` set to `orphan/aws-tunnels-operator-standalone`.
- For fully automatic scheduled runs, the Renovate workflow file must exist on the repository default branch (for example `master`) because GitHub Actions schedules only run from the default branch.
- Keep/copy `.github/workflows/renovate.yaml` on the default branch, while leaving `baseBranches` targeting this orphan branch.
