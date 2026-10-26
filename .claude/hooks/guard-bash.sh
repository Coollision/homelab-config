#!/usr/bin/env bash
# PreToolUse(Bash) hook: hard-block commands the repo's rules forbid. Exit 2 = blocked,
# stderr is shown to Claude. Everything else passes untouched.
cmd=$(jq -r '.tool_input.command // empty' 2>/dev/null)
[ -z "$cmd" ] && exit 0

if echo "$cmd" | grep -qE 'git[[:space:]]+(-C[[:space:]]+[^[:space:]]+[[:space:]]+)?push([[:space:]].*)?[[:space:]](-f|--force|--force-with-lease(=[^[:space:]]*)?|\+[^[:space:]]+)([[:space:]]|$)'; then
  echo "Blocked: never force-push unless the user explicitly asks. Use a plain 'git push'." >&2
  exit 2
fi
if echo "$cmd" | grep -qE 'git[[:space:]]+(commit|push)[[:space:]].*--no-verify'; then
  echo "Blocked: --no-verify skips the repo's pre-commit checks. Ask the user first." >&2
  exit 2
fi
if echo "$cmd" | grep -qE '(^|[;&|[:space:]])vault[[:space:]]+kv[[:space:]]+(delete|destroy|undelete|metadata[[:space:]]+delete)'; then
  echo "Blocked: Vault deletes/undeletes need explicit user approval (read wiki secrets page + scripts/vault-audit.sh first)." >&2
  exit 2
fi
exit 0
