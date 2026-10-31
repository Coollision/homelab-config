# storage-operator — SMB controller reference

Everything here is identical to the Python `smb_operator_v2.py` unless marked **(Go)**.

## Environment

| Variable | Source | Required |
|---|---|---|
| `OPERATOR_NAMESPACE` | downward API (default `storage`) | no |
| `CONFIG_MAP_NAME` | default `smb-operator-config` | no |
| `SMB_LOADBALANCER_IP` | Secret `smb-credentials` key `ip` | yes |
| `SMB_USERNAME` | Secret key `username` | yes |
| `SMB_PASSWORD` | Secret key `password` | yes |
| `LOG_LEVEL` | `DEBUG` / `INFO` / `WARNING` / `ERROR` | no |

## Flags **(Go)**

| Flag | Default | |
|---|---|---|
| `--enable-smb` | `true` | run the SMB controller |
| `--enable-pv-reclaimer` | `false` | placeholder, logs "not implemented" |
| `--owner-deployment` | `smb-operator` | operator Deployment (child labels, Service owner) |
| `--dry-run` | `false` | all writes with `dryRun=All`; logs smb.conf / mount-table / pod-template diffs; disables leader election |
| `--leader-elect` | `true` | lease `storage-operator-leader` in `OPERATOR_NAMESPACE` |
| `--namespace`, `--config-map` | env | override the env values |
| `--metrics-bind-address` / `--health-probe-bind-address` | `:8080` / `:8081` | |
| `--kubeconfig`, `--zap-*` | | controller-runtime defaults |

## config.yaml

| Key | Default |
|---|---|
| `operator.namespace` | **required** |
| `operator.reconcileInterval` | `30` (resync period, seconds) |
| `operator.useWatchAPI` | `true` (accepted; see README) |
| `smb.deploymentName` / `smb.serviceName` | `smb-server` |
| `smb.image` | `dperson/samba:latest` |
| `smb.workgroup` | `WORKGROUP` |
| `smb.serverString` | `Homelab Storage` |
| `smb.nfsMountOptions` | `vers=4.2,noresvport,soft,timeo=50,retrans=2` (keep `soft`) |
| `smb.mountReconcileInterval` | `15` |
| `smb.initialMountWait` | `30` |
| `longhorn.{namespace,group,version,plural}` | `storage`, `longhorn.io`, `v1beta2`, `volumes` |
| `retry.{maxRetries,initialDelay,backoffMultiplier,retryableStatusCodes}` | `3`, `1`, `2`, `[409,429,500,503,504]` |
| `resources.smbServer.{requests,limits}.{cpu,memory}` | `100m/128Mi`, `500m/512Mi` |
| `smb.security`, `resources.operator`, `operator.logLevel`, `operator.healthPort` | ignored |

Volume lookups try `longhorn.namespace`, then `longhorn-system`.

## Migration ConfigMap `smb-operator-migration` / `volumes.yaml`

```yaml
volumes:
  - name: <longhorn volume>   # required
    shareName: <share>        # default: name
    smb: rw                   # rw (default) | ro; anything else -> rw
```

Volume with `status.shareEndpoint` → NFS share; without → direct CSI share via temp PV+PVC
`migrate-<volume>` (label `smb-operator/migration=true`, RWO, Retain, class `longhorn`, size from
`spec.size` in Mi, `1Gi` if unparseable). Orphans are deleted on structural changes.

## Labels and annotations

| Where | Key | Value |
|---|---|---|
| PVC (input) | `smb-access` | `shared` \| `read-only` |
| PVC (input) | `smb-share-name` | share name (default PVC name) |
| PVC (output) | `smb-operator/status` | `active` \| `detached` \| `pending` \| `error` |
| PVC (output) | `smb-operator/share-path` | `//<LB IP>/<share>` when active |
| PVC (output) | `smb-operator/last-update` | `YYYY-MM-DD HH:MM:SS UTC`, every reconcile |
| children | operator Deployment labels + `managed-by: smb-operator` | |
| smb-server Deployment + pod template | `smb-operator/version: "2.1"`, `shares-count`, `share-list` | |
| smb-server Deployment only | `smb-operator/last-updated` | |
| migrate-* PV/PVC | `smb-operator/migration: "true"` | |

## Events (core/v1, component `smb-operator`, name `<object>.<unix seconds>`)

| Object | Reason | Type |
|---|---|---|
| PVC | `NotBound`, `VolumeNotFound`, `NoShareEndpoint` | Warning |
| PVC | `ShareDetached` (every reconcile while detached) | Normal |
| PVC | `ShareAdded`, `ShareRemoved` | Normal |
| Deployment | `Created`, `Updated` | Normal |
| Service | `Created` | Normal |

## Metrics (`:8080/metrics`)

`smb_operator_reconcile_total`, `smb_operator_reconcile_errors_total`,
`smb_operator_shares_managed`, `smb_operator_last_reconcile_timestamp_seconds`,
`smb_operator_retry_count_total`, `smb_operator_events_sent_total`, plus controller-runtime's
`controller_runtime_*{controller="smb"}`.
