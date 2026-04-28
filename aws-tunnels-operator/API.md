# AWS Tunnels Operator API

## Operator Config API

The operator now consumes a single ConfigMap as desired state source.

- ConfigMap name: `aws-tunnels-operator-stack` (configurable)
- Keys:
  - `stackName`: logical stack name prefix
  - `spec.json`: JSON object with stack spec

`spec.json` top-level keys:

- `aws`
- `auth`
- `nodeAffinity`
- `tunnelDefaults`
- `tunnels`

## Auth API

The operator exposes an auth service on port `8090`.

### `GET /`

Returns a lightweight login/status UI with profile-scoped login buttons and stack restart form.

### `GET /status.json`

Returns discovered stack/profile targets and credential status.

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

On success, matching tunnel Deployments (same stack/profile) are restarted by patching pod template annotation.

### `POST /restart`

Restarts all tunnel Deployments in a stack.

Request body:

```json
{
  "namespace": "proxies",
  "stack": "aws-tunnels"
}
```

### `GET /login-wait`

HTML wait page used by form login to display AWS SSO URL while login is in progress.

### `GET /login-poll.json`

Polling endpoint for async form login progress and completion redirect.

### `GET /healthz`

Returns health status of the auth endpoint.
