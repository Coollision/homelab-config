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
```

## Rendering

```bash
helm template aws-tunnels tests/proxies/aws-tunnels
```

## Notes

- Scripts were moved into real files with proper extensions under `source/`.
- ConfigMaps now import those files instead of embedding large inline blobs in root-level manifests.
- Add new tunnels by editing `values.yaml`, not by copying YAML manifests.
