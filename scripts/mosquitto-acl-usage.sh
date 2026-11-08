#!/usr/bin/env bash
# Per-user MQTT usage report for trimming the Mosquitto ACL roles (phase 2b of
# wiki/06-todo/mosquitto-modernization.md).
#
# Needs the broker to run with `log_type all` (temporary, see mosquitto.conf) so Loki holds
# "Received PUBLISH/SUBSCRIBE" lines, and kubectl access to the cluster. Topic names only:
# the broker never logs payloads.
#
# Usage: scripts/mosquitto-acl-usage.sh [--since 24h]   (units s/m/h/d)
#
# For every username it prints what was published and subscribed (device ids normalised) and
# marks anything NOT covered by the rw/ro lines of that user's role in
# workload/smarthome/mosquitto/clients.conf. While acl-learning=true nothing is denied, so the
# "NOT COVERED" rows are exactly what would break when learning is switched off.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
SINCE=24h
[ "${1:-}" = "--since" ] && SINCE="${2:?--since needs a value like 24h}"

kubectl -n monitoring port-forward svc/logging-loki 13100:3100 >/dev/null 2>&1 &
PF=$!
trap 'kill $PF 2>/dev/null' EXIT
for _ in $(seq 1 20); do curl -s -o /dev/null http://localhost:13100/ready && break; sleep 0.5; done

SINCE="$SINCE" exec python3 -I - <<'PY'
import collections, json, os, re, subprocess, sys, time, urllib.parse, urllib.request

since = os.environ["SINCE"]
unit = {"s": 1, "m": 60, "h": 3600, "d": 86400}[since[-1]]
end = time.time_ns()
start = end - int(since[:-1]) * unit * 10**9

# --- roles from clients.conf (parsed by the same clients.awk the broker's init uses) ------
roles = {}
parsed = subprocess.run(["awk", "-v", "lint=1", "-f", "workload/smarthome/mosquitto/clients.awk",
                         "workload/smarthome/mosquitto/clients.conf"], capture_output=True, text=True, check=True).stdout
for rec in parsed.splitlines():
    kind, *f = rec.split("\x1f")
    if kind == "U":
        roles[f[0]] = []
    elif kind == "A":
        roles[f[0]].append((f[1], f[2]))

def covers(role_filter, f):
    r, t = role_filter.split("/"), f.split("/")
    for i, rl in enumerate(r):
        if rl == "#":
            return True
        if i >= len(t):
            return False
        if rl == "+":
            if t[i] == "#":
                return False
            continue
        if rl != t[i]:
            return False
    return len(r) == len(t)

def covered(user, topic, write):
    allowed = ("rw", "send") if write else ("rw", "ro")
    return any(covers(f, topic) and kind in allowed for kind, f in roles.get(user, []))

# --- pull logs from Loki ---------------------------------------------------------------
query = ('{namespace="smarthome",pod="mosquitto-0",container="mosquito"} '
         '|~ "New client connected|Received PUBLISH from|Received SUBSCRIBE from|\\\\(QoS [0-2]\\\\)"')
lines, cursor = [], start
while True:
    qs = urllib.parse.urlencode({"query": query, "start": cursor, "end": end, "limit": 5000, "direction": "forward"})
    req = urllib.request.Request("http://localhost:13100/loki/api/v1/query_range?" + qs, headers={"X-Scope-OrgID": "1"})
    res = json.load(urllib.request.urlopen(req, timeout=120))["data"]["result"]
    batch = sorted((int(v[0]), v[1]) for s in res for v in s["values"])
    if not batch:
        break
    lines += batch
    if len(batch) < 5000:
        break
    cursor = batch[-1][0] + 1
print(f"# window {since}, {len(lines)} relevant log lines\n", file=sys.stderr)

# --- attribute lines to users ----------------------------------------------------------
user_of, pub, sub = {}, collections.defaultdict(collections.Counter), collections.defaultdict(collections.Counter)
pending = None
for _, text in lines:
    body = text.split(": ", 1)[1] if ": " in text else text
    m = re.search(r"New client connected from \S+ as (\S+) \(.*u'([^']*)'", body)
    if m:
        user_of[m.group(1)] = m.group(2); continue
    m = re.match(r"Received PUBLISH from (\S+) \(.*?, '(.*)', \.\.\. \(\d+ bytes\)\)", body)
    if m:
        pub[user_of.get(m.group(1), "?")][m.group(2)] += 1; continue
    m = re.match(r"Received SUBSCRIBE from (\S+)", body)
    if m:
        pending = m.group(1); continue
    m = re.match(r"\s*(\S+) \(QoS \d\)", body)
    if m and pending:
        sub[user_of.get(pending, "?")][m.group(1)] += 1

def norm(t):
    t = re.sub(r"-[0-9A-Fa-f]{12}\b", "-<id>", t)
    t = re.sub(r"0x[0-9a-f]{16}", "<ieee>", t)
    p = t.split("/")
    if p[0] == "zigbee2mqtt" and len(p) > 1 and p[1] != "bridge":
        p[1] = "<device>"
    return "/".join(p[:3])

def show(title, counters, write):
    print(f"\n### {title}\n\n| user | topic (normalised) | count | role covers it |\n|---|---|---|---|")
    for user in sorted(counters):
        agg = collections.Counter(); ok = {}
        for topic, n in counters[user].items():
            k = norm(topic); agg[k] += n
            ok[k] = ok.get(k, True) and covered(user, topic, write)
        for k, n in agg.most_common():
            print(f"| {user} | `{k}` | {n} | {'yes' if ok[k] else '**NOT COVERED**'} |")

show("Published", pub, True)
show("Subscribed", sub, False)
print("\nUsers seen: " + ", ".join(sorted(set(user_of.values()))))
print("Roles without any traffic: " + (", ".join(sorted(set(roles) - set(pub) - set(sub))) or "none"))
PY
