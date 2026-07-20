#!/usr/bin/env bash
# Shows which Sablier-managed Deployments/StatefulSets are currently asleep
# (scaled to 0) vs awake, across the whole cluster, plus how long each awake
# app has been up and how long until it sleeps again. Read-only — queries
# live cluster state and Sablier's own /metrics; unrelated to the git-side
# ignoreDifferences generator.
#
# "Sleeps in" needs Sablier >= 1.15.0 with --server.metrics.enabled (the
# sablier_session_expires_at_timestamp_seconds gauge). If that's not scraped
# successfully (older version, or the port-forward fails), it just shows "-"
# for every row rather than erroring.
set -euo pipefail

METRICS_FILE=$(mktemp)
PF_PID=""
cleanup() {
  rm -f "$METRICS_FILE"
  [ -n "$PF_PID" ] && kill "$PF_PID" 2>/dev/null || true
}
trap cleanup EXIT

kubectl -n kube-system port-forward svc/sablier 18080:10000 >/dev/null 2>&1 &
PF_PID=$!
for _ in $(seq 1 20); do
  curl -sS -o /dev/null "http://localhost:18080/health" 2>/dev/null && break
  sleep 0.2
done
curl -sS "http://localhost:18080/metrics" -o "$METRICS_FILE" 2>/dev/null || true

kubectl get deployment,statefulset -A -l 'sablier.enable=true' -o json | python3 -c "
import json, re, subprocess, sys, time
from datetime import datetime, timezone

d = json.load(sys.stdin)

expires = {}
try:
    with open('$METRICS_FILE') as f:
        for line in f:
            m = re.match(r'sablier_session_expires_at_timestamp_seconds\{([^}]*)\}\s+([0-9.eE+-]+)', line)
            if not m:
                continue
            labels, value = m.groups()
            gm = re.search(r'group=\"([^\"]*)\"', labels)
            if gm:
                expires[gm.group(1)] = float(value)
except FileNotFoundError:
    pass

def fmt_duration(seconds):
    seconds = max(0, int(seconds))
    h, rem = divmod(seconds, 3600)
    m, s = divmod(rem, 60)
    if h:
        return f'{h}h{m:02d}m'
    if m:
        return f'{m}m{s:02d}s'
    return f'{s}s'

now = time.time()
rows = []
for item in d['items']:
    ns, name = item['metadata']['namespace'], item['metadata']['name']
    group = item['metadata']['labels'].get('sablier.group', '?')
    desired = item['spec'].get('replicas', 1)
    ready = item['status'].get('readyReplicas', 0)
    state = 'ASLEEP' if desired == 0 else ('WAKING' if ready != desired else 'AWAKE')

    awake_for = '-'
    if state == 'AWAKE':
        sel = item['spec']['selector']['matchLabels']
        sel_str = ','.join(f'{k}={v}' for k, v in sel.items())
        out = subprocess.run(
            ['kubectl', 'get', 'pods', '-n', ns, '-l', sel_str,
             '-o', 'jsonpath={.items[0].metadata.creationTimestamp}'],
            capture_output=True, text=True,
        ).stdout.strip()
        if out:
            created = datetime.strptime(out, '%Y-%m-%dT%H:%M:%SZ').replace(tzinfo=timezone.utc)
            awake_for = fmt_duration((datetime.now(timezone.utc) - created).total_seconds())

    sleeps_in = '-'
    if group in expires:
        remaining = expires[group] - now
        sleeps_in = fmt_duration(remaining) if remaining > 0 else 'expiring'

    rows.append((state, ns, name, group, awake_for, sleeps_in))

rows.sort(key=lambda r: (r[0] != 'ASLEEP', r[1], r[2]))
print(f'{\"STATE\":8} {\"NAMESPACE\":12} {\"NAME\":24} {\"AWAKE FOR\":10} {\"SLEEPS IN\":10} GROUP')
for r in rows:
    print(f'{r[0]:8} {r[1]:12} {r[2]:24} {r[4]:10} {r[5]:10} {r[3]}')
"
