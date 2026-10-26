#!/usr/bin/env bash
# Local mirror of the CI "Validate Charts" job: yamllint + helm lint + helm template.
#
# Usage:
#   scripts/validate-charts.sh                 # every chart changed vs HEAD (staged, unstaged, untracked)
#   scripts/validate-charts.sh <chart-dir>...  # specific chart dirs
#   QUIET=1 scripts/validate-charts.sh ...     # print only failures (used by the post-edit hook)
#
# A change under lib/<name>/ fans out to every chart depending on that library.
# Exit 0 = all good, 1 = at least one failure (output holds the first lines of each error).
set -uo pipefail
cd "$(git rev-parse --show-toplevel)"

QUIET="${QUIET:-}"
say() { [ -n "$QUIET" ] || echo "$@"; }

chart_of() { # walk up from a file to the nearest Chart.yaml dir (skips vendored charts/)
  local d
  d=$(dirname "$1")
  while [ "$d" != "." ] && [ "$d" != "/" ]; do
    case "$d" in */charts/*|*/disabled-*|disabled-*) return 0 ;; esac
    [ -f "$d/Chart.yaml" ] && { echo "$d"; return 0; }
    d=$(dirname "$d")
  done
}

if [ $# -gt 0 ]; then
  dirs=$(printf '%s\n' "$@" | sed 's|/$||')
else
  files=$( { git diff --name-only HEAD; git ls-files --others --exclude-standard; } | sort -u)
  dirs=$(for f in $files; do [ -e "$f" ] && chart_of "$f"; done | sort -u)
  for lib in $(echo "$files" | grep -oE '^lib/[^/]+' | sort -u); do
    dirs=$(printf '%s\n' "$dirs"
      grep -lE "repository:[[:space:]]*file://.*${lib}\b" $(find . -name Chart.yaml -not -path '*/charts/*' -not -path '*/disabled-*' | sed 's|^\./||') 2>/dev/null | xargs -n1 dirname 2>/dev/null)
  done
  dirs=$(printf '%s\n' "$dirs" | grep -v '^$' | sort -u)
fi

[ -z "$dirs" ] && { say "validate: no charts affected"; exit 0; }

fail=0
for d in $dirs; do
  [ -f "$d/Chart.yaml" ] || { echo "FAIL $d: no Chart.yaml"; fail=1; continue; }
  say "== $d"
  out=$(yamllint -c .yamllint.yml -f parsable "$d/values.yaml" "$d/Chart.yaml" 2>&1 | grep -E '\[error\]' | head -5)
  [ -n "$out" ] && { echo "FAIL yamllint $d"; echo "$out"; fail=1; }

  if grep -q '^dependencies:' "$d/Chart.yaml" && [ ! -d "$d/charts" ]; then
    (cd "$d" && helm dependency update >/dev/null 2>&1) || { echo "FAIL $d: helm dependency update"; fail=1; continue; }
  fi
  out=$(helm lint "$d" 2>&1) || { echo "FAIL helm lint $d"; echo "$out" | grep -E 'ERROR|Error' | head -8; fail=1; }
  if ! grep -qE '^type:[[:space:]]*library' "$d/Chart.yaml"; then
    out=$(helm template "$(basename "$d")" "$d" 2>&1 >/dev/null) || { echo "FAIL helm template $d"; echo "$out" | head -8; fail=1; }
  fi
done

[ $fail -eq 0 ] && say "validate: OK"
exit $fail
