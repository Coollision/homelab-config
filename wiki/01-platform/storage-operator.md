# Storage operator {#storage-operator}

[← Back to Home](../Home.md) · Platform: [Storage](storage.md) · [Databases](databases.md)

`storage-operator` is one Go binary (controller-runtime manager) hosting two controllers for
the storage stack: the **SMB server controller** (Longhorn RWO volume to RWX SMB share) and the
**PV reclaimer** (frees `Released` PVs). It replaces the old in-repo Python operator and, once
the (now removed) shell-based `system/storage/pv-reclaimer` app. The wiki
[Storage](storage.md) page explains where it fits; this page is the operator itself.

| Controller | Flag | Chart toggle | State |
|---|---|---|---|
| `smb` | `--enable-smb` (default `true`) | `smb.enabled` | live, parity port of the Python operator |
| `pv-reclaimer` | `--enable-pv-reclaimer` (default `false`) | `pvReclaimer.enabled` | live (replaced the shell `pv-reclaimer` app) |

Both share one leader-election lease (`storage-operator-leader`), one metrics endpoint
(`:8080/metrics`) and one probe endpoint (`:8081/healthz`, `:8081/readyz` with a named check
per controller).

## Why Go, why a standalone branch {#storage-operator-why}

The Python operator needed a Kustomize setup, an inline script in a ConfigMap and an image
built on the fly. The Go port is a single static binary with golden-file tests captured from
the Python operator (byte-for-byte rendered `smb.conf`, mount table, scripts and objects), so
behaviour parity is tested, not assumed. The code lives on an **orphan branch**
(`orphan/storage-operator-standalone`) of the same GitHub repo, with no shared history with
this GitOps repo: its own CI, its own versions, and no Renovate/ArgoCD noise from this repo's
pushes. It is published as image `ghcr.io/coollision/storage-operator` and Helm chart
`oci://ghcr.io/coollision/charts/storage-operator`. Reference docs on that branch:
`storage-operator/README.md` (overview, parity table), `API.md` (config, labels, events,
metrics, flags) and `DEV.md` (layout, tests, dry run).

## Build and release {#storage-operator-release}

One workflow on the orphan branch (`.github/workflows/storage-operator.yaml`) runs on every push
touching `storage-operator/**`:

1. lint commit subjects (Conventional Commits);
2. `go vet`, `go test -race` (golden parity + shellcheck of rendered scripts), `helm lint`,
   `helm template`, and chart invariant checks (strategy `Recreate`, `--owner-deployment=smb-operator`);
3. semver bump from the commit types (`BREAKING CHANGE`/`!` = major, `feat` = minor, else patch),
   written into the chart's `version`, `appVersion` and image tag, committed back as
   `chore(release): ...`;
4. build `linux/amd64` + `linux/arm64` binaries, push the multi-arch image (`vX.Y.Z` and
   `sha-<short>` tags), push the chart to the OCI registry;
5. create the release tag `storage-operator/vX.Y.Z`.

Renovate (default helm manager; the orphan branch is also a Renovate base branch, like aws-tunnels) bumps the `storage-operator` dependency version in the wrapper chart's
`Chart.yaml`, like any other chart. Don't hand-bump it.

## Deployment {#storage-operator-deployment}

`system/storage/longhorn-smb-operator` is a thin **wrapper chart**: `Chart.yaml` depends on the
OCI chart, `values.yaml` only sets `smb.enabled: true` and `pvReclaimer.enabled`
(chart defaults reproduce the Python era's ConfigMaps), and `templates/smb-credentials-secret.yaml`
renders the credentials Secret from Vault (`<secret:kv/...>`, keys `ip`, `username`,
`password`).

Things that look odd but are deliberate:

- **The Deployment is still named `smb-operator`.** The `smb-server` Service has an
  `ownerReference` to it, and an ownerReference is immutable in practice; renaming would
  orphan or garbage-collect the Service. The binary takes `--owner-deployment` (default
  `smb-operator`) and CI asserts the match.
- **Strategy `Recreate`.** One replica plus a leader lease plus "ready after reconcile" deadlocks
  a `RollingUpdate`. The lease is released on context cancel, so the new pod acquires it
  immediately instead of waiting for expiry. (The cluster-wide `OnDelete` pattern used for
  Vault is not used here.)

## Configuration {#storage-operator-config}

| Input | What |
|---|---|
| PVC label `smb-access` = `shared` / `read-only` | opts a PVC into SMB; optional `smb-share-name` (default PVC name) |
| ConfigMap `smb-operator-config` / `config.yaml` | unchanged from the Python era: reconcile interval, SMB image/workgroup, NFS mount options (keep `soft`), Longhorn namespace/group, retry policy, SMB server resources |
| ConfigMap `smb-operator-migration` / `volumes.yaml` (optional) | explicit `name` / `shareName` / `smb: rw\|ro` entries; volumes without a `shareEndpoint` get a temp `migrate-<volume>` PV/PVC pair (Retain, RWO) |
| Secret `smb-credentials` | `ip` (LoadBalancer IP), `username`, `password`; from Vault |

Config is read at start (no hot reload): restart the operator after changing it. Dead keys
(`smb.security`, `resources.operator`, `operator.logLevel`, `operator.healthPort`) are ignored
with one deprecation log line. Full key list and defaults: `API.md`.

## SMB controller behaviour {#storage-operator-smb}

- **Discovery.** Labelled PVCs, the migration ConfigMap and Longhorn `Volume` status changes all
  map to a single reconcile key (serialised, storms collapse); `reconcileInterval` is the
  periodic resync. It renders `smb.conf` (ConfigMap `smb-config`), the mount table
  (`smb-mounts`, `mounts.tsv`) and the `smb-server` Deployment + LoadBalancer Service, and
  writes `smb-operator/status` (`active`, `detached`, `pending`, `error`), `share-path` and
  `last-update` annotations on each PVC, plus Events.
- **Mount table, not pod spec.** RWX NFS endpoints never appear in the pod spec. An in-pod
  reconciler mounts them from `mounts.tsv`, polling every `mountReconcileInterval`. This is the
  fix for the [hard-mount trap](../05-operations/known-issues.md#smb-operator-hard-mount-trap).
- **Detach / Sablier.** When a volume detaches (last consumer scaled to zero), its share is
  **kept** in `smb.conf` but unmounted until the volume reattaches; the PVC shows `detached`
  and a `ShareDetached` event each reconcile. The pod is never rolled for this.
- **Restarts.** The structural state is in memory, so the first non-empty reconcile after an
  operator restart re-applies the Deployment. The pod template is unchanged (no pod roll), but
  the Deployment `generation` bumps because the `smb-operator/last-updated` annotation changes.

## PV reclaimer {#storage-operator-pv-reclaimer}

Watches PVs with `status.phase == Released` (including those already Released at start-up) and
merge-patches `claimRef` so the PV becomes `Available` again. Each reconcile re-reads the PV and
patches only if it is still Released, guarded by `resourceVersion` (conflict: requeue).

Defaults are **shell parity**: every Released PV, any reclaim policy, any StorageClass, whole
`claimRef` cleared. Opt-in switches, all off by default:

| Value (flag) | Effect |
|---|---|
| `pvReclaimer.dryRun` (`--pv-reclaimer-dry-run`) | patch with `dryRun=All`, emit `WouldReclaim` events |
| `pvReclaimer.retainOnly` | only PVs with reclaim policy `Retain` |
| `pvReclaimer.storageClasses` | comma-separated StorageClass allowlist |
| `pvReclaimer.honorSkipAnnotation` | skip PVs annotated `storage-operator.homelab/pv-reclaimer-skip: "true"` |
| `pvReclaimer.patchMode` | `clear` (drop whole `claimRef`) or `keep-claim` (drop only `uid`/`resourceVersion`) |

**Cutover is done.** It runs live (`dryRun` off) and the legacy shell app
`system/storage/pv-reclaimer` has been removed. Next hardening step: `retainOnly: true`. The
shell app's unwired webhook was not ported.

## Metrics, events, probes {#storage-operator-observability}

- Metrics (`:8080/metrics`): `smb_operator_reconcile_total`, `_reconcile_errors_total`,
  `_shares_managed`, `_last_reconcile_timestamp_seconds`, `_retry_count_total`,
  `_events_sent_total`; `storage_operator_pv_reclaimed_total{result=reclaimed|dry_run|skipped|error}`;
  plus `controller_runtime_*{controller=...}`.
- Events: PVC `ShareAdded`/`ShareRemoved`/`ShareDetached`/`NotBound`/`VolumeNotFound`/`NoShareEndpoint`;
  Deployment/Service `Created`/`Updated`; PV `Reclaimed`/`WouldReclaim`/`ReclaimFailed`.
- Probes: `:8081/healthz`, `:8081/readyz`. SMB is ready after any successful reconcile;
  `pv-reclaimer` once the PV informer has synced.

## Runbook {#storage-operator-runbook}

```bash
# Health
kubectl -n storage get deploy smb-operator
kubectl -n storage logs deploy/smb-operator --tail=100
kubectl -n storage get pvc -l smb-access -o custom-columns=N:.metadata.name,S:.metadata.annotations.smb-operator/status
kubectl get events -A --field-selector reason=WouldReclaim | tail

# Force a reconcile: restart (Recreate; lease is released, so the swap is quick)
kubectl -n storage rollout restart deploy/smb-operator
```

- **Roll back** to the Python operator: `git revert` the commit that switched
  `system/storage/longhorn-smb-operator` to the wrapper chart; ArgoCD does the rest.
- **Local dry run** (every write sent with `dryRun=All`, leader election off, safe next to the
  live operator; logs smb.conf / mount-table / pod-template diffs). Run from the orphan branch checkout:

  ```bash
  SMB_LOADBALANCER_IP=... SMB_USERNAME=... SMB_PASSWORD=... \
    go run . --kubeconfig ~/.kube/config --dry-run --zap-log-level=debug
  ```

  Expected: pod template unchanged; only the Deployment `last-updated` annotation differs.
  For the reclaimer alone: add `--enable-smb=false --enable-pv-reclaimer --pv-reclaimer-dry-run --leader-elect=false`.

## Gotchas {#storage-operator-gotchas}

- **Delete-policy race.** With parity defaults the reclaimer also clears `claimRef` on Released
  PVs with `reclaimPolicy: Delete` and PVs already being deleted. That races the PV controller /
  CSI deleter (leaked volume, or a new PVC binding a PV whose volume is gone). Set
  `retainOnly: true` right after cutover. Longhorn's default class is `Retain`, so this mostly
  matters for other classes.
- **Dry-run events repeat.** The PV stays Released, so `WouldReclaim` repeats; compare names.
- **Restart re-applies the Deployment** (generation bump, no pod roll) as described above.
- **Config is not hot-reloaded**; restart after editing `smb-operator-config`.
- **Never `kubectl patch` the SMB Deployment or volumes**: ArgoCD selfHeal and the operator both
  revert it.
