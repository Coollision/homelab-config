#!/usr/bin/env bash
# Token-cheap read-only Loki log fetcher for the homelab cluster (works for pods that no longer exist).
#
# Why a script: Loki is only reachable in-cluster, wants an X-Scope-OrgID header, and raw LogQL output is
# huge. This opens a short-lived port-forward, queries, then strips noise, collapses repeats and caps output.
#
# Usage:
#   loki-logs.sh query [filters] [window]    compact log lines, oldest first
#   loki-logs.sh count [filters] [window]    only the number of matching lines per pod (cheapest first probe)
#   loki-logs.sh pods  [filters] [window]    pod names that logged in the window
#   loki-logs.sh raw '<LogQL>' [window]      full control; same compaction
#
# Filters (combine freely; all are regexes, case-sensitive):
#   -n|--ns <ns>        namespace              -p|--pod <re>     pod name (prefix match, e.g. snapshot-2h-2992)
#   -a|--app <re>       app/container label    -g|--grep <re>    line must match
#   -v|--exclude <re>   line must NOT match    -e|--errors       shortcut: grep 'level=(error|fatal|warn)|(?i)error|fail|timeout'
# Window (UTC; default --since 1h):
#   --since 90m|6h|2d   or   --from 2026-11-27T01:00 --to 2026-11-27T03:00
# Output:
#   -l|--limit N        max lines fetched, default 40 (hard cap 300); identical lines (ids/numbers ignored) collapse to 1 + count
#   --width N           truncate each line, default 300
#   --full              keep the func=/file=/key=value tail instead of stripping it
#
# Env: LOKI_ORG (tenant header, default 1), LOKI_NS (default monitoring), LOKI_SVC (default logging-loki).
# Read-only. Needs kubectl, curl, jq.
set -euo pipefail

ORG="${LOKI_ORG:-1}"; LNS="${LOKI_NS:-monitoring}"; LSVC="${LOKI_SVC:-logging-loki}"
mode="${1:-}"; [ -n "$mode" ] || { sed -n '2,24p' "$0" | sed 's/^# \{0,1\}//'; exit 1; }
shift
ns=""; pod=""; app=""; grepre=""; excl=""; since="1h"; from=""; to=""; limit=40; width=300; full=0; raw=""
[ "$mode" = raw ] && { raw="${1:?raw needs a LogQL string}"; shift; }
while [ $# -gt 0 ]; do
  case "$1" in
    -n|--ns) ns="$2"; shift 2;;       -p|--pod) pod="$2"; shift 2;;
    -a|--app) app="$2"; shift 2;;     -g|--grep) grepre="$2"; shift 2;;
    -v|--exclude) excl="$2"; shift 2;; -e|--errors) grepre='level=(error|fatal|warn)|(?i)(error|fail|timeout)'; shift;;
    --since) since="$2"; shift 2;;    --from) from="$2"; shift 2;;  --to) to="$2"; shift 2;;
    -l|--limit) limit="$2"; shift 2;; --width) width="$2"; shift 2;; --full) full=1; shift;;
    *) echo "unknown arg: $1" >&2; exit 1;;
  esac
done
[ "$limit" -gt 300 ] && limit=300

epoch() { date -u -j -f '%Y-%m-%dT%H:%M' "$1" +%s 2>/dev/null || date -u -d "$1" +%s; }
if [ -n "$from" ]; then s=$(epoch "$from"); e=$([ -n "$to" ] && epoch "$to" || date -u +%s)
else
  n="${since%[mhd]}"; case "$since" in *m) m=60;; *h) m=3600;; *d) m=86400;; *) echo "bad --since: $since" >&2; exit 1;; esac
  e=$(date -u +%s); s=$((e - n * m))
fi

esc() { printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g'; }
if [ -z "$raw" ]; then
  sel=""
  [ -n "$ns" ]  && sel+="namespace=\"$ns\","
  [ -n "$pod" ] && sel+="pod=~\"$(esc "$pod").*\","
  [ -n "$app" ] && sel+="app=~\"$(esc "$app")\","
  [ -n "$sel" ] || { echo "need at least one of --ns/--pod/--app (or use raw)" >&2; exit 1; }
  raw="{${sel%,}}"
  [ -n "$grepre" ] && raw+=" |~ \"$(esc "$grepre")\""
  [ -n "$excl" ]   && raw+=" !~ \"$(esc "$excl")\""
fi

port=$((31000 + RANDOM % 2000))
kubectl -n "$LNS" port-forward "svc/$LSVC" "$port:3100" >/dev/null 2>&1 &
pf=$!; trap 'kill $pf 2>/dev/null || true' EXIT
for _ in 1 2 3 4 5 6 7 8; do curl -s -m 2 -o /dev/null "http://localhost:$port/ready" && break; sleep 1; done

api() { curl -s -m 90 -G -H "X-Scope-OrgID: $ORG" "http://localhost:$port/loki/api/v1/$1" "${@:2}"; }
range=(--data-urlencode "start=${s}000000000" --data-urlencode "end=${e}000000000")

case "$mode" in
  pods)
    api series "${range[@]}" --data-urlencode "match[]=${raw}" | jq -r '[.data[]?.pod]|unique|.[]';;
  count)
    api query_range "${range[@]}" --data-urlencode "query=sum by (pod) (count_over_time(${raw}[$((e - s))s]))" \
      --data-urlencode "limit=1" -d "step=$((e - s))" 2>/dev/null | jq -r '.data.result[]? | "\(.values[-1][1])\t\(.metric.pod)"' | sort -rn | head -20;;
  query|raw)
    api query_range "${range[@]}" --data-urlencode "query=$raw" --data-urlencode "limit=$limit" \
      --data-urlencode "direction=forward" > "${TMPDIR:-/tmp}/loki-logs.$$.json"
    jq -e '.status=="success"' "${TMPDIR:-/tmp}/loki-logs.$$.json" >/dev/null || { head -c 300 "${TMPDIR:-/tmp}/loki-logs.$$.json"; echo; rm -f "${TMPDIR:-/tmp}/loki-logs.$$.json"; exit 1; }
    [ "$(jq '[.data.result[]?.values[]]|length' "${TMPDIR:-/tmp}/loki-logs.$$.json")" -gt 0 ] || { echo "NO LINES matched (window $((e - s))s). Check pod prefix, window, LOKI_ORG tenant; empty != no logs." >&2; rm -f "${TMPDIR:-/tmp}/loki-logs.$$.json"; exit 2; }
    jq -r '[.data.result[] | .stream.pod as $p | .values[] | [.[0], ($p // "-"), .[1]]] | sort_by(.[0])[]
           | "\(.[0][0:10] | tonumber | strftime("%d %H:%M:%S")) \(.[1]) \(.[2] | gsub("\n";" "))"' \
      "${TMPDIR:-/tmp}/loki-logs.$$.json" |
    awk -v full="$full" -v w="$width" '
      { line=$0
        if (!full) { gsub(/ func=[^ ]+/,"",line); gsub(/ file="[^"]+"/,"",line)
                     gsub(/ (concurrent|executionCount|groups|parameters|retainCount|specLabels|task)=("[^"]*"|[^ ]*)/,"",line)
                     sub(/time="[^"]+" /,"",line) }
        sub(/[ \t]+$/,"",line)
        key=line; sub(/^[0-9]+ [0-9:]+ /,"",key); gsub(/[0-9a-f]{8}-[0-9a-f-]{27}/,"<id>",key); gsub(/[0-9]+/,"#",key)
        if (!(key in cnt)) { order[++k]=key; first[key]=substr(line,1,w) }
        cnt[key]++; split(line,t," "); last[key]=t[2] }
      END { for (i=1;i<=k;i++) { key=order[i]; print first[key] (cnt[key]>1 ? "  (x" cnt[key] ", last " last[key] ")" : "") } }'
    rm -f "${TMPDIR:-/tmp}/loki-logs.$$.json";;
  *) echo "unknown mode: $mode" >&2; exit 1;;
esac
