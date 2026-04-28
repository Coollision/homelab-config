# aws-tunnels-operator Helm Chart

This chart deploys the operator and writes one stack config ConfigMap consumed by the operator runtime.

`stack.auth.host` is the single source of truth for auth hostname. If `ingress.enabled=true`, the IngressRoute uses `stack.auth.host`.

## Install

```bash
helm upgrade --install aws-tunnels-operator \
  tests/proxies/aws-tunnels-operator/chart \
  -n proxies --create-namespace
```

## Minimal Single-Stack Values (Recommended)

For a single instance, use `stack` values:

```yaml
stack:
  name: aws-tunnels
  aws:
    profile: awsprofile001
    region: eu-west-1
    ssoStartUrl: <secret:kv/data/aws~sso-start-url>
    accountId: <secret:kv/data/aws~account-id>
    roleName: Admin
  auth:
    enabled: true
    host: aws-auth.<secret:kv/data/domains~domain>
    port: 8090
  tunnels:
    - name: gitlab-dev
      host: gitlab-dev.<secret:kv/data/domains~domain>
      bastionName: bhnonprod
      remoteHost: localhost
      remotePort: "8181"
      localPort: 8181
      ingressMode: http
```

## Auth API/UI Endpoints

- `GET /` - login/status UI
- `GET /status.json` - machine-readable status
- `POST /login` - login and restart matching profile tunnels
- `POST /restart` - restart all tunnels in a stack

Body:

```json
{"namespace":"proxies","stack":"aws-tunnels","profile":"awsprofile001"}
```
