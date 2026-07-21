# Home Assistant recorder — MariaDB → CNPG Postgres migration (Path B, full history)

**Goal:** move the HA recorder off the bundled, hand-tuned MariaDB onto the shared
CNPG cluster `smarthome-db-pg`, keeping **all** history (states + statistics), with
**downtime limited to a single HA restart** (~3–8 min).

**Two-phase, evidence-driven approach (deliberate):**
- **Phase 1 — migrate on _stock_ Postgres tuning.** Do NOT port the MariaDB tweaks yet. Run on
  CNPG's default parameters so we get a real baseline of how PG behaves under this workload.
- **Phase 2 — tune against measurements.** Only change a parameter once a metric shows we need
  it. Each candidate is tied to the MariaDB tweak it mirrors *and* the thing to observe first.

> **Capacity ≠ tuning.** Two things are NOT deferrable perf knobs — they're floors the migration
> can't run without: **storage** (a ~10 GB DB won't fit in the current 5 Gi) and the **memory
> limit** (index builds + HA write load OOM a 256 Mi pod). Phase 1 raises those. It leaves the
> Postgres *parameters* (`shared_buffers`, `work_mem`, …) at CNPG stock — `shared_buffers` stays
> 128 MB; the extra RAM just becomes OS page cache, which is the Postgres way anyway.

**Driver (honest framing):** the win is **operational consolidation** (one GitOps CNPG for
n8n + HA + future apps, unified Longhorn backups, free Prometheus metrics), **not** raw
perf/RAM — see "Reality check". Full history is chosen so a full 30-day window
(`purge_keep_days: 30`) is available immediately rather than rebuilt from zero.

---

## 0. Live baseline (captured pre-migration)

| Item | Value |
|---|---|
| HA version | `2026.7.2` |
| MariaDB | `12.3.2`, bundled in the `homeassistant` chart, real RAM ~789 Mi |
| MariaDB data dir | **7.7 GB** |
| `states` | **5.65 GB** (2.4 GB data + 3.2 GB idx), ~22.4 M rows |
| `state_attributes` | 141 MB, ~259 k rows |
| `statistics` | 450 MB, ~3.52 M rows |
| `statistics_short_term` | 423 MB, ~3.19 M rows |
| `statistics_meta` / `states_meta` | 488 / 1 945 rows |
| `events` / `event_data` | ~232 k / ~19 k rows |
| Recorder | `db_url: mysql://…@homeassistant-db-service:3306/HomeAssistant?charset=utf8mb4`, `purge_keep_days: 30`, no `commit_interval` override, excludes 4 entities |
| Target cluster | `smarthome-db-pg`, CNPG, **PG 18.3**, `instances: 1`, `5Gi`, limit `256Mi`, stock params (`shared_buffers 128MB`, `work_mem 4MB`) |

> ⚠️ The recorder `db_url` lives in HA's `/config/secrets.yaml` on the Longhorn config
> volume — **not in git**. That single line is the entire cutover switch and the entire
> rollback. MariaDB is left fully intact (`protect: true`) until decommission.

## Reality check

- **No native cross-engine sync exists.** CNPG `initdb.import` and Postgres logical replication
  are Postgres-source-only; `mysql_fdw` needs a custom CNPG image; Debezium needs Kafka. So the
  method is **pgloader bulk pre-copy (HA stays up) + monotonic-PK delta at cutover** — the delta
  is a few thousand rows, so effective downtime = the delta + the HA restart.
- **Perf is parity-at-best from the engine swap.** Path B carries the 22 M-row `states` table
  over, so don't expect faster history loads or lower RAM than the tuned MariaDB. Phase 2 aims
  to *match* MariaDB, guided by evidence.
- **Energy dashboard reads `statistics` / `statistics_short_term`, never `states`** — its speed
  is a `work_mem` question (Phase 2), not a `states`-table question.

---

# PHASE 1 — Migrate on stock Postgres tuning

## 1.1 Capacity changes (required — git, no HA impact)

Edits in `workload/smarthome/smarthome-db-cnpg/`, mirroring the n8n pattern.
**No `postgresql.parameters` block yet** — that's Phase 2.

### `cluster.yaml`
```yaml
spec:
  instances: 1
  storage:
    pvcTemplate:
      resources:
        requests:
          storage: 25Gi              # capacity floor — a ~10GB DB + WAL + index temp won't fit in 5Gi
  resources:
    requests: { cpu: 250m, memory: 512Mi }
    limits:   { memory: 1Gi }        # capacity floor — 256Mi OOMs on load + HA write path
  managed:
    roles:
      - name: n8n_user               # existing
        ensure: present
        login: true
        passwordSecret: { name: pgrole-n8n }
      - name: homeassistant_user     # NEW
        ensure: present
        login: true
        passwordSecret: { name: pgrole-homeassistant }
  # NOTE: intentionally NO spec.postgresql.parameters — running CNPG stock tuning for the baseline
```

### `databases.yaml`
```yaml
apiVersion: postgresql.cnpg.io/v1
kind: Database
metadata: { name: db-homeassistant, namespace: smarthome }
spec:
  name: homeassistant
  owner: homeassistant_user
  cluster: { name: smarthome-db-pg }
  databaseReclaimPolicy: retain
```

### `secrets.yaml`
```yaml
apiVersion: v1
kind: Secret
metadata: { name: pgrole-homeassistant, namespace: smarthome }
type: kubernetes.io/basic-auth
stringData:
  username: homeassistant_user
  password: <secret:kv/data/smarthome/homeassistant-db~pg-password>   # add to Vault first
---
# add to the existing `smarthome-db` ConfigMap data:  homeassistant_database: homeassistant
```

### Storage resize — **GitOps 3-place edit** (self-heal caution)
The `smarthome-db-pg-data` Longhorn Volume + PV are GitOps-managed and self-heal in ~4 s —
**never `kubectl patch`**. Same commit:
1. `storage.yaml` → Longhorn `Volume.spec.size` (`5368709120` → `26843545600`)
2. `storage.yaml` → `PersistentVolume.spec.capacity.storage` (`5Gi` → `25Gi`)
3. `cluster.yaml` → `pvcTemplate.resources.requests.storage` (`25Gi`)

**Sync ArgoCD.** CNPG creates the `homeassistant` DB + role. Zero HA impact so far.

## 1.2 Tooling — pgloader Job

Run in namespace `smarthome` for cluster DNS + low latency to both DBs.
```bash
kubectl -n smarthome run pgloader --rm -it --restart=Never \
  --image=ghcr.io/dimitri/pgloader:latest -- bash
```

## 1.3 Stage the schema (no downtime)

**Let HA own the schema** so its version matches `2026.7.2` and **no schema migration runs at
cutover** (a migration over 22 M `states` rows would wreck the downtime budget).
1. Throwaway HA pod, image `ghcr.io/home-assistant/home-assistant:2026.7.2`, minimal `/config`:
   ```yaml
   default_config:
   recorder:
     db_url: postgresql://homeassistant_user:<PW>@smarthome-db-pg-rw:5432/homeassistant
   ```
2. Wait for the recorder "schema created" log line → **delete the pod.** Tables now exist, empty.

**Speed the bulk `states` load** — capture then drop the big secondary indexes; recreate after §1.4:
```sql
-- capture first:  pg_dump -s -t states -t state_attributes homeassistant | grep 'CREATE INDEX'
-- then DROP the non-PK indexes on states / state_attributes / statistics / statistics_short_term
```

## 1.4 Bulk pre-copy (no downtime — HA keeps running on MariaDB)

`/tmp/bulk.load`:
```
LOAD DATABASE
     FROM     mysql://<mysql_user>:<PW>@homeassistant-db-service:3306/HomeAssistant
     INTO postgresql://homeassistant_user:<PW>@smarthome-db-pg-rw:5432/homeassistant
 WITH data only, truncate, workers = 4, concurrency = 1, max parallel create index = 4
  SET maintenance_work_mem to '256MB', work_mem to '128MB'   -- one-time LOAD session only, not persistent tuning
 CAST type tinyint to boolean using tinyint-to-boolean,
      type datetime to timestamptz drop default drop not null
 BEFORE LOAD DO $$ SET session_replication_role = 'replica'; $$
 AFTER  LOAD DO $$ SET session_replication_role = 'origin';  $$ ;
```
```bash
pgloader /tmp/bulk.load        # ~20–60 min on Longhorn; HA unaffected
```
> The `SET maintenance_work_mem/work_mem` above are **load-time session settings** for this
> one-shot copy (so index rebuilds don't crawl) — they do NOT persist and are not the Phase-2
> tuning. At a 1 Gi pod limit index builds may spill to temp files; that's fine (slower, no OOM).

Then, still **no downtime**:
- Recreate the dropped secondary indexes (heavy build happens now, not at cutover).
- `ANALYZE;`
- Record high-water marks for the delta:
  ```sql
  SELECT max(state_id) FROM states;  SELECT max(attributes_id) FROM state_attributes;
  SELECT max(event_id) FROM events;  SELECT max(data_id) FROM event_data;
  SELECT max(id) FROM statistics;    SELECT max(id) FROM statistics_short_term;
  SELECT max(metadata_id) FROM states_meta; SELECT max(id) FROM statistics_meta;
  SELECT max(event_type_id) FROM event_types;
  ```

## 1.5 Cutover (DOWNTIME window: delta + restart)

**a. Stop HA**
```bash
kubectl -n smarthome scale statefulset homeassistant --replicas=0
```

**b. Delta copy through pgloader** — build a MariaDB view-schema whose views are named like the
target tables and select only rows above the high-water marks; pgloader appends them with
correct casts (`data only`, **no `truncate`**).
```sql
-- MariaDB, using the §1.4 high-water marks:
CREATE DATABASE IF NOT EXISTS hadelta;
CREATE OR REPLACE VIEW hadelta.states               AS SELECT * FROM HomeAssistant.states               WHERE state_id      > :hw;
CREATE OR REPLACE VIEW hadelta.state_attributes     AS SELECT * FROM HomeAssistant.state_attributes     WHERE attributes_id > :hw;
CREATE OR REPLACE VIEW hadelta.events               AS SELECT * FROM HomeAssistant.events               WHERE event_id      > :hw;
CREATE OR REPLACE VIEW hadelta.event_data           AS SELECT * FROM HomeAssistant.event_data           WHERE data_id       > :hw;
CREATE OR REPLACE VIEW hadelta.statistics           AS SELECT * FROM HomeAssistant.statistics           WHERE id            > :hw;
CREATE OR REPLACE VIEW hadelta.statistics_short_term AS SELECT * FROM HomeAssistant.statistics_short_term WHERE id          > :hw;
CREATE OR REPLACE VIEW hadelta.states_meta          AS SELECT * FROM HomeAssistant.states_meta          WHERE metadata_id   > :hw;
CREATE OR REPLACE VIEW hadelta.statistics_meta      AS SELECT * FROM HomeAssistant.statistics_meta      WHERE id            > :hw;
CREATE OR REPLACE VIEW hadelta.event_types          AS SELECT * FROM HomeAssistant.event_types          WHERE event_type_id > :hw;
```
`/tmp/delta.load` (`data only`, **no truncate**):
```
LOAD DATABASE
     FROM     mysql://<mysql_user>:<PW>@homeassistant-db-service:3306/hadelta
     INTO postgresql://homeassistant_user:<PW>@smarthome-db-pg-rw:5432/homeassistant
 WITH data only
 CAST type tinyint to boolean using tinyint-to-boolean,
      type datetime to timestamptz drop default drop not null
 BEFORE LOAD DO $$ SET session_replication_role = 'replica'; $$
 AFTER  LOAD DO $$ SET session_replication_role = 'origin';  $$ ;
```
```bash
pgloader /tmp/delta.load       # a few thousand rows → seconds
```
> Skip `recorder_runs` / `statistics_runs` — HA writes fresh run rows on boot.

**c. Reset every sequence** (mandatory — else the recorder's first insert collides on PK):
```sql
SELECT setval(pg_get_serial_sequence('states','state_id'),                (SELECT max(state_id)      FROM states));
SELECT setval(pg_get_serial_sequence('state_attributes','attributes_id'), (SELECT max(attributes_id) FROM state_attributes));
SELECT setval(pg_get_serial_sequence('states_meta','metadata_id'),        (SELECT max(metadata_id)   FROM states_meta));
SELECT setval(pg_get_serial_sequence('events','event_id'),                (SELECT max(event_id)      FROM events));
SELECT setval(pg_get_serial_sequence('event_data','data_id'),             (SELECT max(data_id)       FROM event_data));
SELECT setval(pg_get_serial_sequence('event_types','event_type_id'),      (SELECT max(event_type_id) FROM event_types));
SELECT setval(pg_get_serial_sequence('statistics','id'),                  (SELECT max(id)            FROM statistics));
SELECT setval(pg_get_serial_sequence('statistics_short_term','id'),       (SELECT max(id)            FROM statistics_short_term));
SELECT setval(pg_get_serial_sequence('statistics_meta','id'),             (SELECT max(id)            FROM statistics_meta));
```

**d. Analyze**
```sql
VACUUM ANALYZE states, state_attributes, statistics, statistics_short_term;
```

**e. Flip `db_url` and start HA** — in HA `/config/secrets.yaml`:
```
db_url: postgresql://homeassistant_user:<PW>@smarthome-db-pg-rw:5432/homeassistant?sslmode=require
```
Drop the MySQL-only `?charset=utf8mb4`. `psycopg2` is bundled in the HA image.
```bash
kubectl -n smarthome scale statefulset homeassistant --replicas=1   # downtime ends
```

## 1.6 Validate migration

- HA log: recorder started, **no schema migration**, no `IntegrityError`/duplicate-key.
- Row-count parity (MariaDB vs PG) on `states`, `statistics`, `statistics_short_term`.
- Energy dashboard renders full history; History/Logbook work; new states keep landing.

**Do not tune yet.** Run on stock for a few days through a normal usage cycle (incl. an
overnight `purge` run) so Phase 2 has real numbers.

---

# PHASE 2 — Tune against measurements

Principle: **change one knob, re-measure, keep only what the numbers justify.** The harness
`scripts/ha-pg-perf-bench.sh` does the measuring; the checklist below decides.

## 2.0 Observability (turn on, then let it soak a day)

Reload-only, safe. Run through a normal cycle incl. an Energy-dashboard session and an overnight
`purge`:
```sql
ALTER SYSTEM SET log_temp_files = 0;              -- log every on-disk sort/hash spill
ALTER SYSTEM SET log_min_duration_statement = '500ms';
SELECT pg_reload_conf();
```
`pg_stat_statements` is the best per-query lens but needs `shared_preload_libraries` (a restart)
— add only if the harness isn't enough.

## 2.1 The evaluation harness

`scripts/ha-pg-perf-bench.sh` runs `EXPLAIN (ANALYZE, BUFFERS, SETTINGS)` over four
representative HA queries and prints, per query: **exec_ms**, **sort** (`in-mem` vs
`DISK-SPILL`), **blk_reads** (cache misses), plus the DB **cache-hit %**. All read-only.

| Query | Represents | Sensitive to |
|---|---|---|
| Q1 | Energy 1y hourly statistics fetch | `shared_buffers`, `work_mem` (final sort) |
| Q2 | Short-term statistics, 2 days | baseline / warm cache |
| Q3 | Energy 1y daily-bucket `GROUP BY` | **`work_mem`** (hash/sort spill) |
| Q4 | History states, 5 busiest entities, 7d | `shared_buffers`, `random_page_cost` |

## 2.2 Protocol

```bash
# 1. Stock baseline (defaults from Phase 1) — record it
scripts/ha-pg-perf-bench.sh --label baseline

# 2. A/B the session-testable knobs instantly (no restart), one at a time
scripts/ha-pg-perf-bench.sh --label wm32  --set work_mem=32MB
scripts/ha-pg-perf-bench.sh --label rpc11 --set random_page_cost=1.1

# 3. Keep a knob ONLY if a metric moved (see gate table). Stack survivors:
scripts/ha-pg-perf-bench.sh --label combo --set work_mem=32MB --set random_page_cost=1.1
```
For `shared_buffers` (can't be SET per-session): edit `cluster.yaml`, let CNPG roll-restart,
re-run `--label sb512`, compare `blk_reads`/cache-hit. For `synchronous_commit` (a **write**
knob the read harness won't show): compare recorder write latency in HA logs, or run a quick
`pgbench -c4 -T30` insert test with it `on` vs `off`.

## 2.3 Decision gate — keep the change only if…

| Candidate | KEEP it when the harness/metrics show… | MariaDB analog | Apply |
|---|---|---|---|
| `work_mem` 4MB → 32MB | Q3 (and/or Q1) flips `DISK-SPILL` → `in-mem` and exec_ms drops | `tmp-table-size=32M` | reload |
| `shared_buffers` 128MB → 512MB | cache-hit < ~99% **and** Q1/Q4 `blk_reads` high; drops after bump (also raise mem limit ~1.5Gi) | `innodb-buffer-pool-size=512M` | **restart** |
| `random_page_cost` → 1.1 | Q4 `EXPLAIN` switched a seq scan → index scan / exec_ms drops | `innodb-io-capacity` hints | reload |
| `synchronous_commit=off` | recorder write latency / commit stalls in logs; pgbench TPS up | `flush-log-at-trx-commit=2` | reload |
| `max_wal_size=2GB`, `checkpoint_completion_target=0.9` | `pg_stat_checkpointer.num_requested` ≫ `num_timed` | `innodb-log-file-size=128M` | reload |
| per-table `autovacuum_vacuum_scale_factor=0.02` on `states`/`statistics_short_term` | high `n_dead_tup` + stale `last_autovacuum` after purge; scans slow | *(none — new on PG)* | reload |

Supporting one-liners:
```sql
SELECT num_timed, num_requested FROM pg_stat_checkpointer;                 -- checkpoint pressure
SELECT relname, n_dead_tup, last_autovacuum FROM pg_stat_user_tables       -- bloat after purge
  ORDER BY n_dead_tup DESC LIMIT 10;
```

## 2.4 Commit the survivors

Only the knobs that passed their gate go into `spec.postgresql.parameters` in `cluster.yaml`
(GitOps). `shared_buffers`/`max_connections` trigger a rolling restart; the rest are reload-only.
Raise the pod memory `limit` in the same commit if `shared_buffers` survived. Re-run
`--label final` and keep the output as the record of what each knob bought.

> **Expected end state if everything fires:** ~MariaDB's footprint (1.25–1.5 Gi) and comparable
> render speed — parity, as framed. If most knobs *don't* fire on stock, better still: you
> consolidated without paying the RAM, and you have the numbers to prove it.

---

## Multiple readers / writers (design note)
- HA recorder is a **single writer** — one `db_url` → `-rw`. **Stay at `instances: 1`** (matches
  n8n). Read replicas add a `-ro` endpoint HA won't use; only worth it if Grafana/analytics query
  it. With async replicas `synchronous_commit=off` stays correct. No PgBouncer now (few conns).

## Rollback / decommission
- **Rollback:** revert the `db_url` line to `mysql://…`, restart HA. MariaDB untouched.
- **Decommission** (after ~1 week clean): remove the `homeassistantdb:` block + DB affinity term
  from `workload/smarthome/homeassistant/values.yaml`; keep the `homeassistant-db` volume
  `protect: true` a while, then delete.

## Downtime budget
| Step | Time |
|---|---|
| Scale HA to 0 | ~10 s |
| Delta pgloader | seconds–1 min |
| Sequence reset + `VACUUM ANALYZE` | seconds |
| `db_url` edit + HA start to healthy | ~1–2 min |
| **Total** | **~3–8 min** |
