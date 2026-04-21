# aws-tunnels-operator

Go-based Kubernetes operator replacing the old Helm/script-based aws-tunnels stack.

This folder is a standalone project in the monorepo with:

- Go operator source code
- Helm chart at `chart/`
- Kubernetes CRD/API docs in `API.md`

## What changed

- No PVC for credentials.
- Operator and auth API run in one pod.
- Auth endpoint writes short-lived AWS credentials into per-profile Kubernetes Secrets.
- Tunnel Deployments scale to `0` when creds are missing/expired, and to `1` when valid.
- Argo-friendly structure via `config/default/kustomization.yaml` and a single CR (`AWSTunnelStack`).

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
kubectl apply -k tests/proxies/aws-tunnels-operator/config/default
kubectl apply -f tests/proxies/aws-tunnels-operator/config/samples/proxies_v1alpha1_awstunnelstack.yaml
```

## Deploy with Helm

```bash
helm upgrade --install aws-tunnels-operator tests/proxies/aws-tunnels-operator/chart -n proxies --create-namespace
```

Use `values.yaml` (`stacks`) to define one or more `AWSTunnelStack` resources as part of the same release.

## API

See `API.md` for:

- CRD API (`proxies.homelab.io/v1alpha1`, `AWSTunnelStack`)
- Auth API (`POST /login`, `GET /healthz`)

## Tests

```bash
go test ./...
```
