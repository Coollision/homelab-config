# TODO: Windmill pilot (possible n8n replacement)

[← Back to Home](../Home.md) · Related: [UniFi Matter sync (n8n)](../02-smarthome/unifi-matter-sync.md) · [Smarthome database](../02-smarthome/database.md) · [UPS graceful shutdown](ups-graceful-shutdown.md)

> **Status (2026-11-20): evaluated, no preference yet, nothing deployed.** **Goal: if Windmill can cleanly replace n8n, decommission n8n.**
> Test first, migrate second: the pilot is **only the Matter → UniFi sync** (steps 1-5 and 7 of the plan), next to n8n, which stays
> untouched and keeps running. The full migration waits for the pilot's go/no-go. Nothing forces a move: n8n works, and its two
> workarounds are already built (custom `WebSocket Request` node, community `n8n-nodes-imap` for Denderkind's backup poll).
>
> **Revisit triggers for a full move:** a real Go need, Python packages n8n's runners image cannot carry, or n8n instability returning
> (n8n 3.0, 2026-10-27, had breaking changes). Keep new flow logic in plain portable code (Python/JS functions) with a thin n8n wrapper,
> so a later move is mostly copy-paste.

## Decommission criteria (all must hold)

- All 9 workflows are ported and have run in production on schedule for at least a week, with output matching n8n (dry-run diff).
- Windmill's steady-state RAM (server + workers) is at or below n8n's footprint (1Gi cap today, **plus the runners sidecar once n8n moves to external task runners**; measure that figure for a like-for-like comparison), with no worker growth over several days.
- Failure alerting works without the Enterprise-only error handlers.
- State from the 8 n8n Data Tables (contracts, regulated tariffs, indices, build status, ...) is copied to plain Postgres tables, the ported flows read and write those, and nothing reads the n8n copies any more.
- Windmill workers run with an acceptable isolation setting (not plain `privileged` unless knowingly accepted).
- Jobs run correctly on both amd64 and the arm64 node (a Python and a Go job on each), so scheduling is not restricted by architecture.

If any criterion fails, n8n stays and Windmill is removed instead. Nothing is deleted before then.

## Why

n8n's free tier has no project separation, and its two community nodes (IMAP, WebSocket) are a maintenance burden. The workflows are
mostly Code nodes around HTTP calls, so a code-first tool fits better. Wanted: runs as a service, flows **not** in git (UI/DB held),
own database connection, n8n-Data-Table-like storage, low RAM, and **Go + Python** support (neither n8n nor Node-RED has them).
Windmill is the only candidate found that combines a UI, Go/Python and DB-backed storage; alternatives (Node-RED: JS only; Activepieces:
TS only; Kestra: JVM, heavy; Hatchet/Temporal: code-first, no UI) lose on at least one point.

## What the n8n estate looks like (what has to be ported)

| Workflow | Nodes that matter | Windmill equivalent |
|---|---|---|
| Energiekosten (5 flows), Waterkosten (2) | schedule, HTTP to Home Assistant, Data Tables, large Code nodes | scheduled scripts + a data table; Code nodes become scripts almost 1:1 |
| Matter → UniFi sync | HA node, UniFi HTTP, custom `WebSocket Request` node (ConfigMap + `N8N_CUSTOM_EXTENSIONS`, pod restart per change), Slack | one Python script (`websockets`, ~15 lines; the node's allow-list/size/timeout guards become a few lines); **the pilot** |
| Denderkind → BabyBuddy | built-in IMAP Email Trigger (push) + community `n8n-nodes-imap` (30-min backup poll, PDF download, move mail), Code, HTTP, Slack | `imaplib` script on a short schedule (no IMAP trigger exists, see below); push only if worth a long-running listener |

## Findings (verified against Windmill docs, 2026-11-16)

| Question | Answer |
|---|---|
| Workspaces on free edition | **3** (Enterprise: unlimited; re-checked 2026-11-20), 50 users, 10 SSO users. Forks and dev workspaces count toward the 3. Credentials/variables are per workspace and we want to share them across flows, so the plan is **one workspace** with folders for grouping (free: max 4 groups; folder limits **to verify**). The cap is a non-issue. |
| Own DB | `DATABASE_URL` to CNPG. On non-superuser Postgres create roles `windmill_admin` + `windmill_user` **before first start**, grant `USAGE` on `public` (owning the DB is simplest). Bundled Postgres is demo-only; disable it. |
| n8n tables | n8n Data Tables are **plain Postgres tables** in the `n8n` database (`data_table_user_<id>`, names in the `data_table` / `data_table_column` metadata tables). 8 exist: `energiekosten_contracten`, `_regulier`, `_indices`, `_prijzen`, `_tariefwacht`, `_rlp_2026`, `kosten_bouwstatus`, `waterkosten_tarieven`. They can be copied into properly named tables (e.g. a `costs` schema) with `INSERT ... SELECT`, independent of which tool reads them. Row counts and column types (JSON-as-text, dates) not yet checked. |
| Data tables | Supported: internal managed Postgres, instance-level external Postgres, or a **bring-your-own Postgres resource** (preferred: dedicated DB, plain SQL, readable from Grafana/HA). Datatable/DuckLake DBs live beside the main DB, so back up with `pg_dumpall`. |
| Error handlers | The **schedule error handler** and **workspace error handler** (one alert hook for all schedules/the workspace) are **Enterprise only**; schedules themselves are free. The error-handling docs attach no edition limit to the **flow failure module**, the **instance error handler** (`GLOBAL_ERROR_HANDLER_PATH_IN_ADMINS_WORKSPACE`) or try/except in a script, so free alerting exists but is set up per flow. Critical alerts, Prometheus/OTel metrics and the instance events webhook are Enterprise only. Replace the `Energiekosten: foutmelding` pattern with one of these (**verify on the pilot**). |
| Email / IMAP | Only SMTP-based inbound email triggers (100 emails/day free). **No IMAP polling** → Denderkind becomes a scheduled script. |
| Git sync, audit logs, worker-group UI | Enterprise (git sync free for ≤ 2 users); irrelevant here. Job-run history is kept ≤ 30 days on free. |
| RAM | No official per-component figure. Docs: "1 worker per 1 vCPU and 1-2 GB"; chart defaults are a 2Gi **limit** (no requests) per app/worker. One third-party test: ~633 MiB idle across 8 containers incl. Postgres, LSP, Caddy. Some users report workers growing to ~2 GB over a week → hard limits mandatory. **Must be measured here.** |
| Hardening | Chart default: root, **workers `privileged: true`**. Options: `isolationSecurity: capabilities` (root, only SYS_ADMIN/SETPCAP/SYS_RESOURCE) or `userNamespaces` (non-root, no caps, needs Kubernetes ≥ 1.33; cluster is on 1.36, **node kernel support unchecked**). Both hardened modes also need the `windmill-nsjail` **seccomp and AppArmor profiles installed on every worker node** (otherwise the docs fall back to `seccompProfile: Unconfined`); AppArmor on the arm64 Raspberry Pi OS node is **unverified**. On Kubernetes ≥ 1.32 a job over its memory limit can OOM-kill the whole worker pod unless the kubelet sets `singleProcessOOMKill: true`. A forum thread shows a plain non-root securityContext breaking Python (`/.cache` permission) — needs writable cache/tmp paths. Namespace has no Pod Security Admission labels, so privileged is technically allowed. |
| Multi-arch | Cluster is mixed: 3 amd64 nodes and 1 arm64 node (64-bit Raspberry Pi OS, kernel 6.18). Checked with `docker buildx imagetools inspect`: `windmill`, `windmill-ee` and `windmill-extra` all publish `linux/amd64` + `linux/arm64`, so the manifest list schedules anywhere. The cluster's arm64 node is 64-bit, which the upstream forum says is required (no armv7). **To verify on the cluster:** Python/Go/Bash job runtimes and any PowerShell/nsjail extras on arm64, and that the Go and Python dependency caches work there. Pin the first pilot to amd64 (`nodeSelector: kubernetes.io/arch`) and test an arm64 worker separately. |
| MCP | Built-in server at `<base_url>/api/mcp` (streamable HTTP; OAuth, bearer header or token in URL). Access levels: build and run (write/test/diff/deploy drafts), read and run, read only, or **scripts and flows exposed as typed tools** scoped by folder/path. Own-IdP auth is Enterprise; free-edition limits not documented. Comparable to n8n's MCP (already used here), plus folder-scoped tool exposure. **To verify:** works on the free edition, behind the Traefik token gate with a bearer header. |
| Security | CVE-2026-47107 (nsjail mode on Kubernetes): use Windmill ≥ 1.703.2. |

## Plan

1. **Database**: CNPG database + roles (`windmill_admin`, `windmill_user`) on the smarthome cluster; Vault paths under the repo-mirrored KV convention; a second database for the data-table resource.
2. **Chart**: `workload/smarthome/windmill` via shared-lib/Helm dependency. Server + **1 worker + 1 native worker**; no LSP, no multiplayer, no Caddy (Traefik is the ingress); bundled Postgres off; memory limits ~512Mi–1Gi per worker, requests set (chart has none). One workspace.
3. **Isolation**: try `userNamespaces`; fall back to `capabilities`; privileged only as a last resort. Record the outcome here.
4. **Multi-arch check**: run a Python and a Go job on an amd64 worker and on an arm64 worker (separate worker pods pinned by `kubernetes.io/arch`); confirm the runtimes and caches behave the same.
5. **Measure**: `kubectl top` idle, then while running a Python job and a Go job (first Go run compiles); compare to n8n's 1Gi cap. Watch for growth over a few days.
6. **Copy the n8n tables to plain Postgres** (read-only on the n8n side): check row counts and column types per table, create named tables in a `costs` schema, `INSERT ... SELECT`, register the database as a Windmill Postgres resource. **Copy, don't move**: the n8n flows keep reading their own tables during the parallel run, so keep the two copies in sync (or freeze edits) until cutover.
7. **Pilot**: port the Matter → UniFi sync; keep it dry-run until the output matches n8n's.
8. **Decide**: go/no-go on RAM + isolation. If go: port Energie/Water (on the copied tables), then Denderkind, running both tools in parallel for a week.
9. **Decommission n8n** once the criteria above hold: export the n8n workflows and data tables as a last backup, remove `workload/smarthome/n8n` (plus its Vault paths, CNPG database/role and PVC, each destructive step with explicit OK), and update the wiki pages that mention n8n (databases, Home, Matter/UniFi sync, future ideas, UPS notes).

## n8n side, verified 2026-11-20 (why staying is the default)

- n8n has first-class Agents (GA in 2.40) and a built-in MCP server, both in use; Windmill only has MCP.
- Community edition has no Projects, but **folders are free after registering for a licence key**, and tags are core.
- Data Tables are available on Community with a **200 MiB instance-wide cap** (`N8N_DATA_TABLES_MAX_SIZE_BYTES`): check the 8 tables against it.
- Whichever tool runs the flows, copying the tables to plain Postgres helps Grafana/HA access.
- **n8n internal task-runner mode is deprecated** (docs: removal planned, no version published; one page ties it to n8n 3.0; n8n logs the
  warning; external runners were rolled out on 2026-11-22). n8n recommends **external runners**: the `n8nio/runners`
  image as a sidecar with a version matching n8n. This makes n8n **two containers with its own sidecar memory**, so "n8n is one app with
  no extras" is weaker than it looked, and the Windmill RAM comparison should be **n8n + runners sidecar vs Windmill server + worker**.
  Affected: the Code nodes (Denderkind, Energie/Water); **not** the custom `WebSocket Request` node (runs in the main process).
  Python packages are added by building a custom runners image. A reported bug has an internal runner still spawning with
  `N8N_RUNNERS_MODE=external` (n8n 1.120.4 / 2.0.3, [issue 22798](https://github.com/n8n-io/n8n/issues/22798)): check before switching.

## Open decisions (for `/grill-me`)

- **Denderkind push vs poll in Windmill:** n8n's built-in IMAP trigger gives push (IDLE) for free; in Windmill both variants are custom code.
  Poll is plain `imaplib` on a 1-5 min schedule with the existing dedup (the daycare report arrives about once a day, so latency barely matters).
  Preferred shape: the same poll script on a slow schedule (the existing 30 min) **plus an on-demand webhook** (docs: one script can have
  schedules, a webhook and a manual Run button), so a poll can be fired instantly without a constant 1-min schedule. Push (IMAP IDLE)
  needs a long-running process; the docs list "perpetual scripts" (restart on exit) with no Enterprise tag, so it may be possible on free
  (**to verify on the pilot**, not needed for the default).

## Verified 2026-11-20 (docs + cluster)

- **Docs, confirmed:** webhooks (Bearer, per-script scoped tokens), several schedules per script, flow failure module and the instance
  error handler (no Enterprise tag; the workspace-level handler is Enterprise), built-in MCP server at `/api/mcp` on self-hosted with
  Bearer auth (only own-IdP sign-in is Enterprise), `userNamespaces` requirements (K8s ≥ 1.33, `max_user_namespaces` > 0, ID-mapped-mount volumes).
- **Docs silent (test on the pilot):** outbound 993 for `imaplib`, default/max job timeout, HTTP-route edition, folder cap (pricing says
  "maximum 4 groups" on one row), Python/Go on arm64, writable cache env (`HOME`, `UV_CACHE_DIR`) for a non-root worker (a forum report shows `/.cache` permission errors).
- **Cluster:** AppArmor is enabled in the kernel on the three amd64 nodes and `max_user_namespaces` is high, but **`apparmor_parser` is not installed**
  (the nsjail AppArmor profile cannot be loaded without it); no `singleProcessOOMKill` kubelet setting found. The arm64 node could not be
  checked over SSH (key refused).
- Hostname/ingress and whether to enable SSO (10 free SSO users).
- Which database hosts Windmill (smarthome cluster vs a new one) and who may reach it.
- Alerting replacement for the Enterprise-only error handlers.
- Whether n8n is retired entirely or kept for AI/agent flows.
- Do **not** make Windmill the UPS-shutdown orchestrator (see [UPS graceful shutdown](ups-graceful-shutdown.md): the executor must not depend on the cluster it shuts down).

## Sources

[Self-host](https://www.windmill.dev/docs/advanced/self_host) · [Pricing](https://www.windmill.dev/pricing) · [Data tables](https://www.windmill.dev/docs/core_concepts/persistent_storage/data_tables) · [Email triggers](https://www.windmill.dev/docs/triggers/email_triggers) · [Security isolation](https://www.windmill.dev/docs/advanced/security_isolation) · [Helm chart](https://github.com/windmill-labs/windmill-helm-charts)
