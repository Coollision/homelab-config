# aws-tunnels

Exposes private AWS services to the homelab through AWS SSM port forwarding, now packaged as a Helm chart.

## Layout

- `Chart.yaml` — chart metadata
- `values.yaml` — shared config plus the tunnel list
- `templates/` — rendered Kubernetes resources
- `source/server.py` — auth server source
- `source/entrypoint.sh` — tunnel entrypoint source
- `source/aws-config` — AWS CLI profile source

## Values-driven configuration

The chart supports:

- shared AWS SSO profile configuration
- one shared auth server
- one shared PVC
- multiple tunnel pods defined in `values.yaml`

Example tunnel entry:

```yaml
tunnels:
  - name: gitlab-dev
    bastionName: bastion-dev
    remoteHost: <secret:kv/data/proxies/aws-tunnel~gitlab-dev-host>
    remotePort: 81
    localPort: 18080
    host: gitlab-work.<secret:kv/data/domains~domain>

  - name: dev-db
    bastionName: bastion-dev
    remoteHost: ""
    remotePort: 5432
    localPort: 15432
    host: dev-db-work.<secret:kv/data/domains~domain>
    ingressMode: tcp
    tls:
      passthrough: true
    rds:
      clusterPrefix: my-dev-db-cluster
```

### DB tunnels on port 443

- Use `ingressMode: tcp` to publish database tunnels via Traefik `websecure` (443) with `IngressRouteTCP`.
- This avoids opening new external ports; routing is based on SNI host (e.g. `dev-db-work.<domain>`).
- Client must use TLS and send SNI for the requested host.
- For RDS end-to-end TLS, keep `tls.passthrough: true` on DB tunnels (default).

### TLS settings to avoid DB connection errors (no AWS changes)

- With `tls.passthrough: true`, TLS is terminated by RDS (not Traefik). This keeps encryption all the way to AWS.
- Recommended client setting: `sslmode=require` for quick connectivity with no certificate mismatch errors.
- If you need certificate validation, use `sslmode=verify-ca` and provide the AWS RDS CA bundle (`global-bundle.pem`).
- Avoid `sslmode=verify-full` on `*-db-work` hostnames in passthrough mode, because the server certificate hostname is the RDS endpoint.

### Endpoint stability recommendation

- For Aurora, prefer `rds.clusterPrefix` over `rds.instancePrefix` to target the cluster endpoint and reduce failover/churn issues.
- The tunnel resolves the latest `available` cluster endpoint matching the prefix before opening SSM forwarding.

Example (`psql`):

```bash
psql "host=dev-db-work.example.com port=443 dbname=<db> user=<user> sslmode=require"
# or with CA validation:
psql "host=dev-db-work.example.com port=443 dbname=<db> user=<user> sslmode=verify-ca sslrootcert=/path/to/global-bundle.pem"
```

### Handling changing DB identifiers

- Set `rds.instancePrefix` or `rds.clusterPrefix` on a tunnel.
- Tunnel resolves latest `available` RDS endpoint matching the prefix before creating the SSM session.
- This tolerates instance renames with numeric suffix changes.

### Multiple AWS profiles (prod-ready)

- Configure `aws.extraProfiles` in `values.yaml` for additional account/role pairs.
- Set `tunnel.awsProfile` (and optional `tunnel.awsRegion`) on tunnels that should use a non-default profile.
- Auth server exposes per-profile login buttons, and a successful login only restarts tunnels that use that same profile.
- Global restart remains available via the restart button and touches the shared `.last-login` signal.

## Rendering

```bash
helm template aws-tunnels tests/proxies/aws-tunnels
```

## Notes

- Scripts were moved into real files with proper extensions under `source/`.
- ConfigMaps now import those files instead of embedding large inline blobs in root-level manifests.
- Add new tunnels by editing `values.yaml`, not by copying YAML manifests.
