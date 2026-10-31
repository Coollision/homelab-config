# storage-operator chart

```bash
helm install storage-operator oci://ghcr.io/coollision/charts/storage-operator -n storage
```

| Value | Default | Notes |
|---|---|---|
| `fullnameOverride` | `smb-operator` | keep it: Deployment selector `app: <fullname>` and the smb-server Service ownerReference depend on it |
| `image.{repository,tag,pullPolicy}` | `ghcr.io/coollision/storage-operator`, release tag | |
| `extraLabels` | `{}` | operator Deployment labels; copied by the SMB controller onto every child object |
| `existingSecret` | `smb-credentials` | keys `ip`, `username`, `password` |
| `logLevel` | `INFO` | |
| `dryRun` | `false` | `--dry-run` |
| `ownerDeploymentName` | fullname | `--owner-deployment` |
| `smb.enabled` | `true` | SMB controller + its RBAC + config/migration ConfigMaps |
| `pvReclaimer.enabled` | `false` | renders `<fullname>-pv-reclaimer` ClusterRole/Binding and passes `--enable-pv-reclaimer` plus the flags below |
| `pvReclaimer.dryRun` | `false` | `--pv-reclaimer-dry-run` (first cutover run: `true`) |
| `pvReclaimer.retainOnly` | `false` | `--pv-reclaimer-retain-only` |
| `pvReclaimer.storageClasses` | `[]` (all) | `--pv-reclaimer-storage-classes` |
| `pvReclaimer.honorSkipAnnotation` | `false` | `--pv-reclaimer-honor-skip-annotation` |
| `pvReclaimer.patchMode` | `clear` | `--pv-reclaimer-patch-mode` (`clear` or `keep-claim`) |
| `config.{create,name,data}` | today's live config | `data` is rendered to `config.yaml` |
| `migration.{create,name,annotations,volumes}` | empty list, ArgoCD SSA/ignore annotations | |
| `operatorArgs.*` | `:8080`, `:8081`, `leaderElect: true`, `extraArgs: []` | |
| `rbac.create`, `serviceAccount.*`, `resources`, `probes`, `affinity`, `tolerations`, `nodeSelector`, `podAnnotations` | | |

RBAC: a namespaced Role (leases, ConfigMaps, Services create-only, Deployments) and a ClusterRole
(PVCs, PVs, Longhorn volumes read-only, events). The pv-reclaimer rules live in their own
ClusterRole so turning the toggle off removes its write access to PVs.
