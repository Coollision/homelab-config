# AWS Tunnels Operator API

## Kubernetes API

- Group: `proxies.homelab.io`
- Version: `v1alpha1`
- Kind: `AWSTunnelStack`
- Resource: `awstunnelstacks`

Top-level spec keys:

- `aws`
- `auth`
- `nodeAffinity`
- `tunnelDefaults`
- `tunnels`

## Auth API

The operator exposes an auth service on port `8090`.

### `POST /login`

Starts AWS SSO login flow for a profile and persists exported credentials into a Kubernetes Secret.

Request body:

```json
{
  "namespace": "proxies",
  "stack": "aws-tunnels",
  "profile": "awsprofile001"
}
```

Response:

- `200 OK` on success
- `4xx/5xx` on validation or execution failures

### `GET /healthz`

Returns health status of the auth endpoint.
