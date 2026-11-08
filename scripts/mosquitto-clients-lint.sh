#!/usr/bin/env bash
# Lint workload/smarthome/mosquitto/clients.conf with the same parser the broker's init container
# uses (clients.awk): known keywords, valid access verbs, unique users, a Vault placeholder (never a
# literal password) for every user, no bare '#' for writing clients, nothing under $CONTROL.
# Wired into pre-commit and CI (helm-validate.yml). Optional: --vault also checks that every referenced
# Vault key exists (needs VAULT_ADDR + a token, so it is not run in CI).
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
D=workload/smarthome/mosquitto
awk -v lint=1 -f "$D/clients.awk" "$D/clients.conf" > /dev/null
echo "mosquitto clients.conf: OK ($(grep -c '^user ' "$D/clients.conf") users)"
if [ "${1:-}" = "--vault" ]; then
  grep -o '<secret:kv/data/[^>]*>' "$D/clients.conf" | sort -u | while read -r ph; do
    path="${ph#<secret:kv/data/}"; key="${path#*~}"; path="${path%%~*}"; key="${key%>}"
    vault kv get -mount=kv -field="$key" "$path" > /dev/null 2>&1 || { echo "MISSING Vault key $path ~ $key"; exit 1; }
  done
  echo "all Vault keys present"
fi
