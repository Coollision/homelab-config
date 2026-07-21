#!/usr/bin/env bash
# Perf-knob evaluation harness for the Home Assistant recorder on smarthome-db-pg (CNPG).
#
# Read-only: runs EXPLAIN (ANALYZE, BUFFERS, SETTINGS) over a representative HA query set
# (Energy-dashboard statistics aggregation + History states scan) so each Postgres tuning
# knob can be A/B'd against real execution time, disk-spill, and cache behaviour BEFORE it
# is committed to cluster.yaml. The queries only SELECT; nothing is written.
#
# Session-testable knobs (work_mem, random_page_cost, effective_io_concurrency, ...) can be
# compared instantly with --set, no restart. shared_buffers cannot be SET per-session — test
# it by editing cluster.yaml, letting CNPG restart, then re-running with the same --label and
# reading the cache-hit line. synchronous_commit is a WRITE knob — see the doc, not this script.
#
# Usage:
#   scripts/ha-pg-perf-bench.sh                                        # stock baseline
#   scripts/ha-pg-perf-bench.sh --label wm32   --set work_mem=32MB
#   scripts/ha-pg-perf-bench.sh --label rpc11  --set random_page_cost=1.1
#   scripts/ha-pg-perf-bench.sh --label combo  --set work_mem=32MB --set random_page_cost=1.1
#
# Compare the "Execution Time" + "Sort Method" lines across labels. External-merge/disk sort
# on Q1/Q3 disappearing when you raise work_mem = evidence to keep work_mem=32MB. Read> shared
# buffers shrinking after a shared_buffers bump = evidence to keep it. No change = don't tune.
set -euo pipefail

NS=smarthome
POD=smarthome-db-pg-1
DB=homeassistant
LABEL="baseline"
RUNS=3
SETS=()

while [[ $# -gt 0 ]]; do
  case "$1" in
    --set)   SETS+=("$2"); shift 2;;
    --label) LABEL="$2";   shift 2;;
    --runs)  RUNS="$2";    shift 2;;
    --db)    DB="$2";      shift 2;;
    --pod)   POD="$2";     shift 2;;
    -h|--help) grep '^#' "$0" | sed 's/^# \?//'; exit 0;;
    *) echo "unknown arg: $1" >&2; exit 1;;
  esac
done

OUTDIR="${TMPDIR:-/tmp}/ha-pg-bench"
mkdir -p "$OUTDIR"
OUT="$OUTDIR/${LABEL}.txt"

setblock=""
for s in "${SETS[@]:-}"; do
  [[ -z "$s" ]] && continue
  setblock+="SET ${s%%=*} = '${s#*=}';"$'\n'
done

# Representative HA queries. Energy-set is auto-discovered via has_sum (cumulative
# energy/gas/water sensors); History-set via the 5 busiest state metadata_ids.
read -r -d '' QUERIES <<'SQL' || true
\echo :::Q1 Energy statistics — 1y hourly fetch (scan+sort, the Energy-dashboard core)
EXPLAIN (ANALYZE, BUFFERS, SETTINGS)
SELECT s.metadata_id, s.start_ts, s.state, s.sum
FROM statistics s
WHERE s.metadata_id IN (SELECT id FROM statistics_meta WHERE has_sum)
  AND s.start_ts >= extract(epoch FROM now() - interval '365 days')
ORDER BY s.metadata_id, s.start_ts;

\echo :::Q2 Short-term statistics — last 2 days
EXPLAIN (ANALYZE, BUFFERS, SETTINGS)
SELECT metadata_id, start_ts, mean, sum
FROM statistics_short_term
WHERE start_ts >= extract(epoch FROM now() - interval '2 days')
ORDER BY metadata_id, start_ts;

\echo :::Q3 Energy daily-bucket delta — 1y GROUP BY (work_mem stress: hash/sort spill)
EXPLAIN (ANALYZE, BUFFERS, SETTINGS)
SELECT s.metadata_id, floor(s.start_ts/86400) AS day, max(s.sum) - min(s.sum) AS delta
FROM statistics s
WHERE s.metadata_id IN (SELECT id FROM statistics_meta WHERE has_sum)
  AND s.start_ts >= extract(epoch FROM now() - interval '365 days')
GROUP BY s.metadata_id, floor(s.start_ts/86400)
ORDER BY s.metadata_id, day;

\echo :::Q4 History states — 5 busiest entities, 7 days (big-table index scan + cache)
EXPLAIN (ANALYZE, BUFFERS, SETTINGS)
SELECT st.metadata_id, st.last_updated_ts, st.state
FROM states st
WHERE st.metadata_id IN (
        SELECT metadata_id FROM states s2
        WHERE s2.last_updated_ts >= extract(epoch FROM now() - interval '7 days')
        GROUP BY metadata_id ORDER BY count(*) DESC LIMIT 5)
  AND st.last_updated_ts >= extract(epoch FROM now() - interval '7 days')
ORDER BY st.metadata_id, st.last_updated_ts;
SQL

echo "== label=$LABEL  runs=$RUNS  sets=[${SETS[*]:-none}]  db=$DB =="
: > "$OUT"
for i in $(seq 1 "$RUNS"); do
  echo "--- run $i/$RUNS ---" | tee -a "$OUT"
  # -qtAX keeps it quiet; \timing shows per-statement wall time; setblock applies the knobs.
  kubectl exec -n "$NS" "$POD" -c postgres -i -- \
    psql -d "$DB" -X -v ON_ERROR_STOP=1 <<PSQL 2>&1 | tee -a "$OUT"
\timing on
$setblock
$QUERIES
PSQL
done

echo
echo "==================== SUMMARY ($LABEL) ===================="
# cache-hit ratio for the DB right now (shared_buffers effectiveness proxy)
kubectl exec -n "$NS" "$POD" -c postgres -- psql -d "$DB" -qtAX -c \
  "SELECT 'cache_hit_pct='||round(sum(blks_hit)*100.0/nullif(sum(blks_hit+blks_read),0),2)
   FROM pg_stat_database WHERE datname='$DB';" || true

python3 - "$OUT" <<'PY'
import re, sys
txt = open(sys.argv[1]).read()
# group per query marker, keep last run's numbers (warmest)
blocks = re.split(r':::(Q\d[^\n]*)', txt)
seen = {}
for i in range(1, len(blocks), 2):
    label = blocks[i].strip()
    body  = blocks[i+1]
    et = re.findall(r'Execution Time:\s*([\d.]+) ms', body)
    disk = 'external merge Disk' in body or 'external sort  Disk' in body or 'Disk:' in body
    reads = re.findall(r'read=(\d+)', body)
    seen[label] = (et[-1] if et else '?', 'DISK-SPILL' if disk else 'in-mem', sum(int(x) for x in reads) if reads else 0)
print(f"{'query':40} {'exec_ms':>10} {'sort':>11} {'blk_reads':>10}")
for k,(ms,sort,reads) in seen.items():
    print(f"{k[:40]:40} {ms:>10} {sort:>11} {reads:>10}")
PY
echo "raw EXPLAIN output: $OUT"
