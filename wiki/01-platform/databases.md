# Databases

[← Back to Home](../Home.md) · Platform: [Secrets — Vault](secrets-vault.md) · See also: [Smarthome database](../02-smarthome/database.md) · [Media stack DB](../03-media-stack/overview.md#the-shared-postgres-service-db-cnpg)

## The operator: CloudNativePG

`system/databases/cloudnative-pg` installs just the **CloudNativePG operator** — a small,
lean deployment (operator only; monitoring/dashboards explicitly left disabled for now).
Every actual Postgres instance is a separate `Cluster` custom resource defined by the
*consuming* workload, not centrally here. There are currently two:

| Cluster | Namespace | Backs |
|---|---|---|
| `smarthome-db-pg` | `smarthome` | Home Assistant recorder, n8n |
| `service-db-pg` | `arr-stack` | Sonarr, Radarr, Prowlarr, Bazarr, Jellyseerr |

## The declarative pattern every CNPG cluster follows

Each cluster is defined by plain manifests (no Helm chart), consistently structured as:

- `cluster.yaml` — the `Cluster` CR itself: instance count (currently 1 everywhere — no
  HA replica), a PVC template bound to a **pre-created, named** Longhorn volume (not
  dynamic provisioning — this makes disaster recovery and resizing deterministic),
  Postgres parameter tuning, and `managed.roles` — every consuming app's database role,
  declared right here with its password sourced from a Vault-backed Secret.
- `databases.yaml` — one `Database` CR per app database, each carrying
  `databaseReclaimPolicy: retain` — deleting the CR (e.g. during a decommission) never
  deletes the actual data.
- `storage.yaml` — the static Longhorn `Volume` + `PersistentVolume`, pre-bound via
  `claimRef` for deterministic binding, carrying its own snapshot/backup recurring-job
  labels.
- `secrets.yaml` — the Vault-backed role-credential Secrets.

**Why this shape, not a Helm chart:** roles and databases are genuinely declarative data
(a list of who exists and what they own), and CNPG's own CRDs already express that
cleanly — wrapping them in a templating layer would add nothing.

## Why Postgres, and why CloudNativePG specifically

The shared services Postgres (arr-stack) and Home Assistant's recorder both used to run
on other engines — a Bitnami-chart Postgres and a bundled MariaDB, respectively. Both
were migrated onto CloudNativePG in mid-2026:

- **service-db** (2026-06-03): migrated off the Bitnami Postgres chart. The trigger
  wasn't a services problem — it was **memory**: parsing the classic Bitnami Helm repo
  index cost the ArgoCD repo-server sidecar over 500Mi of RAM per render, causing
  cluster-wide OOMKills unrelated to Postgres itself. Moving to CNPG (and switching other
  Bitnami consumers to OCI registries) fixed it. Full story in
  [Known Issues → the ArgoCD repo-server OOM](../05-operations/known-issues.md#argocd-repo-server-oom).
- **Home Assistant recorder** (2026-07-21/22): migrated off the bundled MariaDB
  StatefulSet onto the smarthome CNPG cluster. The driver here was explicitly
  **consolidation, not performance** — fewer bespoke database engines to operate, not a
  MariaDB problem being fixed. Full migration mechanics (pgloader gotchas, sequence
  resets, tuning decisions that were tried and reverted) are in
  [Known Issues → HA recorder migration](../05-operations/known-issues.md#ha-recorder-to-cnpg-migration).

Both migrations followed the same rough shape: bulk-load history via `pgloader`/`pg_dump`,
verify row counts match exactly, run for a validation period, then fully decommission the
old engine (including deleting its storage) once confidence was high — with backups kept
as the only rollback path after that point.

## Consumer pattern for apps that don't natively speak Postgres

Radarr, Sonarr, and Prowlarr default to SQLite and don't take a `DATABASE_URL`-style env
var — so `lib/arr-lib` (a thin Helm library wrapping `shared-lib`) injects an init
container that waits for Postgres to be reachable, then rewrites the app's
`config.xml` in place via `xmlstarlet` to point at Postgres instead. Apps that *do*
support Postgres natively (Bazarr, Jellyseerr, n8n) just get plain env vars pointing at
the CNPG cluster's `-rw` Service — no XML rewriting needed. See
[Media stack](../03-media-stack/overview.md) for the full picture.
