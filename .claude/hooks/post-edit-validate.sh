#!/usr/bin/env bash
# PostToolUse(Edit|Write) hook. Silent on success (zero tokens); on failure exits 2 with a
# trimmed error so Claude fixes it immediately.
#  1. values.yaml / Chart.yaml / templates / lib edits -> scripts/validate-charts.sh for that chart
#  2. markdown edits outside .claude/ -> block private identifiers (public repo)
f=$(jq -r '.tool_input.file_path // empty' 2>/dev/null)
[ -z "$f" ] && exit 0
root=$(git rev-parse --show-toplevel 2>/dev/null) || exit 0
rel="${f#"$root"/}"

case "$rel" in
  workload/*|system/*|tests/*|lib/*|argocd/*)
    case "$rel" in */charts/*|*/disabled-*|*.sh|*.md|*.txt) exit 0 ;; esac
    case "$rel" in
      lib/*) out=$(QUIET=1 "$root/scripts/validate-charts.sh" 2>&1) ;;
      *)
        d=$(dirname "$rel")
        while [ "$d" != "." ] && [ ! -f "$root/$d/Chart.yaml" ]; do d=$(dirname "$d"); done
        [ "$d" = "." ] && exit 0
        out=$(QUIET=1 "$root/scripts/validate-charts.sh" "$d" 2>&1) ;;
    esac
    rc=$?
    if [ $rc -ne 0 ]; then
      echo "validate-charts failed after editing $rel:" >&2
      echo "$out" | head -25 >&2
      exit 2
    fi
    ;;
  *.md)
    case "$rel" in .claude/*|CLAUDE.md) exit 0 ;; esac   # internal agent docs may use real names
    extra=$(grep -v '^#' "$root/.claude/private-patterns.txt" 2>/dev/null | paste -sd'|' -)   # local, gitignored: one ERE per line (names, domains, emails)
    hits=$(sed -E 's/\b10\.4[23]\.[0-9.\/]*//g' "$root/$rel" | grep -nEi '\b(192\.168|10|172\.(1[6-9]|2[0-9]|3[01]))\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}\b|\bfd[0-9a-f]{2}:[0-9a-f:]{6,}|\b2a02:[0-9a-f:]+|\b[a-z0-9-]+\.pi[0-9]?\b'"${extra:+|$extra}" | head -5)   # 10.42/43 = k3s default CIDRs, not private
    if [ -n "$hits" ]; then
      echo "Public repo: $rel contains private identifiers. Replace with placeholders (<internal-domain>, <node-name>, <vlan-ip>):" >&2
      echo "$hits" >&2
      exit 2
    fi
    ;;
esac
exit 0
