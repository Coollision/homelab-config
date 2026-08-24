# Media stack — overview & topology

[← Back to Home](../Home.md) · Platform: [Databases](../01-platform/databases.md) · [Storage](../01-platform/storage.md)

`workload/arr-stack/` is a media-automation pipeline (Radarr/Sonarr/Prowlarr family).
**Note:** this cluster's arr-stack is a separate, independent instance from any other
Radarr/Sonarr/Prowlarr deployment the household might also use elsewhere (e.g. on a NAS)
— never assume a matching service name or a working API key means it's the same library;
verify by content (item counts, a known title) before acting on it. See
[Known Issues → verify a service's actual location](../05-operations/known-issues.md#verify-where-a-service-actually-runs-before-acting-on-it).

## Request flow

```
User ──▶ Jellyseerr (request UI)
              │
              ▼
        Radarr (movies) / Sonarr (TV)
              │  queries indexers synced from Prowlarr
              ▼
        Prowlarr (indexer manager) ──▶ Jackett (meta-indexer) ──▶ FlareSolverr
              │                                                    (Cloudflare bypass)
              ▼
        download client (NOT part of this stack — lives outside it;
              Radarr/Sonarr only mount the shared downloads volume)
              ▼
        shared NFS media volumes ──▶ Radarr/Sonarr import ──▶ library
              ▼
        Bazarr (subtitles) ──reads the same library volumes
              ▼
        Tautulli (usage stats) ── monitors the media server, read-only
              ▼
        Prefetcharr — watches viewing sessions and pre-triggers a Sonarr
              search for the next season before the viewer catches up
```

Everything in this chain that isn't infrastructure (API keys between Prowlarr and
Radarr/Sonarr, indexer settings, download-client wiring) is configured through each app's
own web UI at runtime and lives in that app's own database — it is **not** expressed in
this repo's Helm values, deliberately. Grep for `api_key`/`apikey` in this repo and
you'll find almost nothing — that's expected, not a gap.

## The apps

| App | Role | Storage | DB |
|---|---|---|---|
| **Jellyseerr** | Request UI | config PVC | native Postgres |
| **Radarr** | Movie library manager | config PVC + shared downloads/video NFS | Postgres via `arr-lib` XML rewrite |
| **Sonarr** | TV library manager | config PVC + shared downloads/series NFS | Postgres via `arr-lib` XML rewrite |
| **Prowlarr** | Indexer aggregator, syncs into Radarr/Sonarr | config PVC only | Postgres via `arr-lib` XML rewrite |
| **Bazarr** | Subtitle fetcher | config PVC + shared video/series NFS (read) | native Postgres |
| **Jackett** | Secondary indexer proxy | config PVC only | — |
| **FlareSolverr** | Cloudflare-challenge solver for indexers | — | — |
| **Tautulli** | Media-server usage stats | config PVC | — |
| **Prefetcharr** | Pre-fetches upcoming episodes based on viewing progress | — (no Service, outbound-only) | — |

## The shared Postgres — `service-db-cnpg`

A single CloudNativePG cluster (`service-db-pg`) backs Radarr, Sonarr, Prowlarr, Bazarr,
and Jellyseerr, following the same declarative pattern described in
[Platform → Databases](../01-platform/databases.md). Radarr/Sonarr/Prowlarr don't support
Postgres natively, so `lib/arr-lib` (a thin wrapper around `lib/shared-lib`) injects an
init container per pod that waits for Postgres, then rewrites the app's XML config file
in place to point at it instead of SQLite. Bazarr and Jellyseerr support Postgres
natively and just get plain environment variables.

This cluster was itself migrated onto CloudNativePG from an older, deprecated database
chart — see [Platform → Databases](../01-platform/databases.md#why-postgres-and-why-cloudnativepg-specifically)
for why, and [Known Issues](../05-operations/known-issues.md#argocd-repo-server-oom) for
the memory-pressure incident that triggered it.

## The shared media volumes

Four NFS-backed `PersistentVolumeClaim`s, all `ReadWriteMany`, defined once in
`workload/arr-stack/shared/` and mounted by whichever app needs that library:
video, series, downloads, and a books volume currently unused by any active app (kept
around for a disabled book-management app). NFS keeps media zero-copy on the NAS — only
config/database data lives on Longhorn.

## History: the namespace migration

This stack used to live in a flat `workload/services/` layout with a self-managed
Postgres. In mid-2026 it was moved wholesale into its own `arr-stack` namespace on
Longhorn-backed config storage, with service names changing from a `-web` suffix to a
`-service` suffix in the process. Because [directory moves are in-place Application
mutations, not delete-and-recreate](../00-setup/gitops-structure.md), this required a
careful pause-then-cutover sequence rather than a simple git mv — the full mechanics
(and the runtime config each app needed hand-repointing afterward, since inter-app
hostnames aren't GitOps-managed) are written up in
[Known Issues → moving a workload between namespaces](../05-operations/known-issues.md#moving-a-workload-between-namespaces).
