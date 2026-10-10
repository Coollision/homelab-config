#!/usr/bin/env bash
# Audit Vault KV (mount `kv`) against the repo's <secret:...> / <path:...> placeholders.
#
# Reports, by NAME only (values are never printed):
#   MISSING  - referenced in the repo but absent from Vault
#   UNUSED   - present in Vault but referenced nowhere in the repo
#   NAMING   - path outside shared/|system/|workload/, or key not kebab-case
#
# Needs VAULT_ADDR + a token (`vault login`). Run from anywhere inside the repo.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
exec python3 -I - <<'PY'
import json, re, subprocess, sys

def vault(*a):
    r = subprocess.run(["vault", *a], capture_output=True, text=True)
    return r.stdout if r.returncode == 0 else None

def walk(prefix=""):
    out = vault("kv", "list", "-format=json", "-mount=kv", prefix)
    for k in json.loads(out) if out else []:
        if k.endswith("/"): yield from walk(prefix + k)
        else: yield prefix + k

have = set()
for p in walk():
    d = json.loads(vault("kv", "get", "-format=json", "-mount=kv", p) or '{"data":{"data":{}}}')["data"]["data"] or {}
    have |= {(p, k) for k in d}

grep = subprocess.run(["git", "grep", "-hoE", r"<(secret|path):kv/data/[^~#>|]+[~#][^>| ]+"],
                      capture_output=True, text=True).stdout.split()
refs = set()
for s in grep:
    m = re.match(r"<(?:secret|path):kv/data/([^~#]+)[~#](.+)", s)
    refs.add(m.groups())

for p, k in sorted(refs - have): print("MISSING", f"{p}~{k}")
for p, k in sorted(have - refs): print("UNUSED ", f"{p}~{k}")
for p, k in sorted(have):
    if not re.match(r"(shared|system|workload)/", p) or not re.fullmatch(r"[a-z0-9][a-z0-9.-]*", k):
        print("NAMING ", f"{p}~{k}")
PY
