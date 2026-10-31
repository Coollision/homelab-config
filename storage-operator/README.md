# storage-operator

One controller-runtime manager hosting the homelab storage controllers:

| Controller | Flag | Chart toggle | Status |
|---|---|---|---|
| `smb` | `--enable-smb` (default `true`) | `smb.enabled` | Go port of the Python `smb_operator_v2.py` |
| `pv-reclaimer` | `--enable-pv-reclaimer` (default `false`) | `pvReclaimer.enabled` | Go port of the pv-reclaimer shell loop (see below) |

All controllers share one leader-election lease (`storage-operator-leader`), one metrics endpoint
(`:8080/metrics`) and one probe endpoint (`:8081/healthz`, `:8081/readyz` with a named check per
controller).

## pv-reclaimer controller

Watches PersistentVolumes (predicate: `status.phase == Released`) and merge-patches
`{"spec":{"claimRef":null}}` so the PV becomes Available again. The informer's initial list also
handles PVs that were already Released at start-up, like the shell loop did. Each reconcile
re-reads the PV from the API and patches only if it is still Released, guarded by its
resourceVersion (a conflict is requeued).

Defaults are shell parity: every Released PV, any reclaim policy, any StorageClass, whole
`claimRef` cleared. Opt-in switches (all off): `--pv-reclaimer-dry-run`,
`--pv-reclaimer-retain-only`, `--pv-reclaimer-storage-classes`,
`--pv-reclaimer-honor-skip-annotation`, `--pv-reclaimer-patch-mode=keep-claim`. See
[API.md](API.md#pv-reclaimer).

> **Caveat (parity):** the defaults also reclaim Released PVs with `reclaimPolicy: Delete` and PVs
> being deleted (`deletionTimestamp` set). That races the PV controller / CSI deleter (leaked
> volume, or a new PVC binding a PV whose volume is gone). After cutover, set
> `pvReclaimer.retainOnly: true` first.

## SMB controller

Exposes Longhorn volumes over SMB from a single `smb-server` Deployment.

- **Inputs** (unchanged from the Python operator): PVCs labelled `smb-access` (`shared` |
  `read-only`, optional `smb-share-name`), ConfigMap `smb-operator-config` (`config.yaml`),
  optional ConfigMap `smb-operator-migration` (`volumes.yaml`), Secret `smb-credentials`
  (`ip`, `username`, `password`).
- **Outputs**: ConfigMaps `smb-config` (`smb.conf`) and `smb-mounts` (`mounts.tsv`), Deployment
  and LoadBalancer Service `smb-server`, temp `migrate-<volume>` PV/PVC pairs, `smb-operator/*`
  annotations on the PVCs, core/v1 Events.
- RWX NFS endpoints never appear in the pod spec; an in-pod bash reconciler mounts them from the
  `smb-mounts` table, so a Longhorn detach (e.g. scale-to-zero) does not restart SMB.

Full reference: [API.md](API.md). Development, golden files and the dry run: [DEV.md](DEV.md).

### Parity with the Python operator

Same config keys/defaults, labels, annotations, rendered files, object names/selectors, events,
metric names, replace-vs-patch logic, create-only Service with an ownerReference to the operator
Deployment, migration PV/PVC GC, in-memory structural state (first reconcile after a restart
re-applies the Deployment with an unchanged pod template). Known, deliberate differences:

| Area | Python | Go | Why |
|---|---|---|---|
| Concurrency | 3 watch threads could reconcile at once | one workqueue key, serialised | framework default (I1) |
| Probes / metrics port | all on `healthPort` (8080) | metrics `:8080`, probes `:8081` | controller-runtime layout; chart wires it |
| Readiness | only after the *initial* reconcile succeeded | after *any* successful reconcile | never stuck unready |
| Watch restarts | watches re-listed every `reconcileInterval` | `RequeueAfter: reconcileInterval` resync | same effect, one reconcile instead of one per object |
| `useWatchAPI: false` | polling only | accepted; watches stay on, resync does the polling | trivially equivalent |
| Owner Deployment | hardcoded `smb-operator` | `--owner-deployment` (default `smb-operator`) | plan |
| Config hot reload | none (read at start) | none (read at start) | parity |
| Dead keys (`smb.security`, `resources.operator`, `operator.logLevel/healthPort`) | ignored | ignored + one deprecation log line | plan decision 6 |
| Resource quantities | sent verbatim (`2000m`) | sent canonical (`2`) | apiserver stores the canonical form either way |
| PVC annotation write | full-object strategic patch | merge patch of the three annotations | same result, no resourceVersion conflicts |

## Install

```bash
helm install storage-operator oci://ghcr.io/coollision/charts/storage-operator \
  --namespace storage --set existingSecret=smb-credentials
```

Chart values: see [chart/values.yaml](chart/values.yaml) and [chart/README.md](chart/README.md).

## Adding a controller

1. New package `controllers/<name>/` exposing `SetupWithManager(mgr manager.Manager, opts Options)`.
   Register metrics on `sigs.k8s.io/controller-runtime/pkg/metrics.Registry` and add a named
   readiness check with `mgr.AddReadyzCheck("<name>", ...)`.
2. In `main.go`, add an `--enable-<name>` flag and call the setup function behind it. If the controller
   needs its own informer restrictions, add them to the `byObject` map before `ctrl.NewManager`
   (note: the cache is shared; PVCs are already restricted to the `smb-access` label when the
   SMB controller is on).
3. Chart: gate RBAC on the controller's toggle (see `clusterrole-pv-reclaimer.yaml`).
