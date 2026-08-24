# Misc apps

[← Back to Home](../Home.md) · See also: [AWS proxies](aws-proxies.md)

Everything that doesn't fit the smarthome or media-stack families.

## BabyBuddy — `workload/apps/babybuddy`

A baby-tracking web app (feeding/sleep/diaper logs), backed by a plain SQLite file on its
own volume — no external database. Runs an init container that patches the container
image's own web-server config (to enable compression/caching) and tunes SQLite's
`PRAGMA`s directly against the database file on every pod start. This is idempotent but
fragile in principle — it depends on exact text matching inside the upstream image's
config files, so a future image update could make it silently stop applying. Exposed
both internally and externally.

**Deliberately excluded from Sablier scale-to-zero** — it was tried, but the app is used
often enough that the wake delay wasn't worth the memory savings, so it was reverted.

## CloudBeaver — `workload/databases/cloudbeaver`

A web-based SQL admin UI. Internal-only, enrolled in scale-to-zero. It has **no
pre-wired database connections in this repo** — every connection it actually uses is
configured by hand through its own UI and stored on its workspace volume, invisible to
Git. It can, in principle, reach any Postgres/MariaDB cluster on the cluster network (no
network policy restricts it), so "what databases CloudBeaver can currently see" is
operational knowledge that lives only in its own workspace state, not something this
wiki can describe as a fixed list.

## Vaultwarden — `workload/secrets/vaultwarden`

A self-hosted password manager (Bitwarden-compatible), backed by its own dedicated
MariaDB instance (kept on MariaDB deliberately — it was not part of the CNPG
consolidation described in [Databases](../01-platform/databases.md)). Both its data and
database volumes carry Longhorn's daily-backup + 2-hourly-snapshot labels, so recovery
doesn't depend on an application-level dump.

**A real config gotcha already fixed here, worth knowing if you ever touch this app
again:** Vaultwarden persists its admin-panel settings (signup toggle, admin token, SMTP
config) to a JSON file on its data volume, and **that file always wins over environment
variables** for those specific settings — env vars for them are dead configuration and
have been removed rather than left as misleading no-ops. Only settings Vaultwarden never
exposes in its admin UI (timezone, the database connection string, mobile push-relay
URLs) are still set via environment variables here.

Exposed both internally and externally, as you'd expect for a password manager that also
needs to serve mobile apps and browser extensions from outside the LAN.

## WordPress — `workload/wordpress`

Both entries under this folder are disabled (excluded from deployment by the
`disabled-*` naming convention — see [GitOps structure](../00-setup/gitops-structure.md)).
They're small, unrelated side-project client sites hosted on this cluster as a
convenience, each a standalone WordPress + MariaDB pair backed by NFS storage rather than
Longhorn (unlike everything else in this repo) — parked rather than torn down. If either
is ever re-enabled, double-check its Vault secret path actually matches its own site
before assuming the credentials are wired correctly; a copy-paste-from-sibling mistake
has been spotted in this pair before and was never confirmed fixed.
