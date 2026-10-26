---
name: validate
description: Run the local equivalent of the CI chart validation (yamllint, helm lint, helm template with library fan-out) on changed or named charts and report only failures. Use before claiming a values/chart/lib change works, or when the user says validate/check/render.
argument-hint: [chart-dir...]
allowed-tools: Bash(scripts/validate-charts.sh:*), Bash(scripts/gen-scale-to-zero-ignores.sh:*), Bash(git diff:*), Bash(git status:*)
---

Run `scripts/validate-charts.sh $ARGUMENTS` (no args = every chart changed vs HEAD, including `lib/` fan-out).

- Report: number of charts checked, then **only** failures with the 1-3 relevant error lines each.
- Fix obvious causes (YAML syntax, missing key) and re-run until green; for anything ambiguous, show the error and ask.
- If `values.yaml` files changed, also run `scripts/gen-scale-to-zero-ignores.sh`; exit 1 + a diff in `applications/set-*.yaml` means the generated region was stale (stage it).
- For a behaviour change, also grep the rendered output for the intended effect (`helm template x <dir> | grep -n ...`) before saying it is done.
