# Storage

[← Back to Home](../Home.md) · Platform: [Networking](networking.md) · [Databases](databases.md)

Four pieces, each solving a different access pattern:

| Component | Path | Access pattern |
|---|---|---|
| Longhorn | `system/storage/longhorn` | Default RWO block storage for almost every app's config/data volume |
| SMB operator | `system/storage/longhorn-smb-operator` | Turns a Longhorn RWO volume into an RWX SMB share |
| NFS provisioner | `system/storage/nfs` | RWX storage backed by the Synology NAS (media libraries, some configs) |
| PV reclaimer | `system/storage/pv-reclaimer` | Auto-reclaims `Released` PVs so they don't pile up |

## Longhorn

Wraps the upstream chart with `replicaSoftAntiAffinity: false` — on a 3-node cluster this
means a 3-replica volume puts **exactly one replica per node**, maximizing redundancy but
also meaning storage load is spread evenly regardless of which node is "supposed to" be
the storage node (see [Known Issues → memory topology](../05-operations/known-issues.md#node-memory-asymmetry)
for the consequence of this on the control-plane node). Recurring jobs are declared in the
chart, not left as upstream defaults: 2-hourly snapshots (24 kept), daily backups (7
kept), weekly backups (4 kept), plus daily system-backup and filesystem-trim jobs.
`defaultBackupStore` points at an NFS target on the Synology NAS — so Longhorn backups
land off-cluster, on the same NAS that also backs the NFS provisioner below.

**Two StorageClasses exist:**
- the default (`Retain` reclaim, 3 replicas) — plain RWO block volumes.
- `longhorn-shared` — RWX, implemented by the SMB operator (below), used wherever an app
  needs a shared-write volume without going all the way to NFS.

**`volumeProtection.enabled: true`** blocks deletion of any volume labelled `protect=true`
— this is the safety net behind Vault's data volume, Technitium's config, and most
database PVCs. If a volume delete is ever refused, check for this label first (and see
[Known Issues → Longhorn volumes are GitOps-managed](../05-operations/known-issues.md#longhorn-volumes-are-gitops-managed)
before reaching for `kubectl patch` at all).

## The SMB operator: bridging Longhorn RWO to RWX

`system/storage/longhorn-smb-operator` is **not a Helm chart** — a hand-rolled Python
controller (deployed via plain Kustomize) that watches Longhorn `Volume` CRs, and for any
volume needing shared access, mounts its NFS export into a shared Samba gateway pod and
reconciles the share list live. Its single most important operating constraint —
learned the hard way in a 30-hour outage — is documented in
[Known Issues → the SMB operator's hard-mount trap](../05-operations/known-issues.md#smb-operator-hard-mount-trap):
**NFS endpoints must never be baked into the pod spec**, only into a ConfigMap the
in-pod reconciler reads live, because a Longhorn RWX volume detaches (and its backing
NFS server vanishes) every time its last consumer scales to zero via Sablier.

## NFS provisioner

Wraps `nfs-subdir-external-provisioner`, pointed at the Synology NAS, `StorageClass:
nfs-client`, `onDelete: retain`. This is what backs the arr-stack's shared media library
volumes (`arr-stack-video`, `-series`, `-downloads`, `-books` — see
[Media Stack](../03-media-stack/overview.md)) and a couple of legacy WordPress sites —
the Synology NAS is a hard, load-bearing dependency shared across Longhorn's backup
target, this provisioner, and the SMB operator's re-export path.

## PV reclaimer

A tiny custom controller that watches for PVs stuck in `Released` phase and reclaims them
automatically — deploys very early (`syncWave: -5`) so it's watching from the start of a
bootstrap. A webhook-notification hook exists in the config but is currently unwired
(empty URL) — flagged as a real TODO, not a bug, if you're looking for why you never got
a "PV reclaimed" alert.

## Practical notes

- **You cannot shrink a Longhorn PVC in place.** Downsizing means: pause the app, scale
  to zero, `rsync` data to a scratch PVC, delete the old PVC/PV/Volume, recreate smaller,
  `rsync` back. This exact procedure — and its sharp edges (a finished copy Job's PVC
  keeps a protection finalizer; the storage class is Retain-reclaim so a deleted scratch
  PVC leaves an orphan PV behind) — is written up in
  [Known Issues → resizing a Longhorn volume](../05-operations/known-issues.md#resizing-a-longhorn-pvc).
- **Replica count is a `values.yaml` field, not a `kubectl patch` target** — selfHeal
  reverts a live patch in seconds. See
  [Known Issues → Longhorn volumes are GitOps-managed](../05-operations/known-issues.md#longhorn-volumes-are-gitops-managed).
