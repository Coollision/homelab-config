---
name: promote-test-app
description: Move an app from tests/<ns>/<app> (tests branch) to workload/ or system/ (master) without downtime by transferring ApplicationSet ownership. Use when a tests-branch experiment is ready for production.
argument-hint: <app-name>
disable-model-invocation: true
---

# Promote `$ARGUMENTS` from tests → production

Authoritative procedure: `tests/README.md` ("Moving an app from tests to system or workload"). Follow it; this skill adds guard rails.

1. Confirm target (`workload/<ns>/` or `system/<ns>/`) and that the app name does not already exist there.
2. Copy `tests/<ns>/$ARGUMENTS` to the target on `master`; drop test-only values (test hostnames, debug flags). `scripts/validate-charts.sh <target>`; run `scripts/gen-scale-to-zero-ignores.sh` if scaleToZero is used.
3. Show the diff and **ask for OK**, then commit + plain `git push` to master (never force).
4. Only after the push is visible to ArgoCD: remove the ownerReference so the target set adopts the Application (command in `tests/README.md`); wait ~3 min; verify `ownerReferences[0].name` is `workloads` or `system-config`.
5. Clean up: delete or `disabled-` rename the folder on the `tests` branch.
6. Update the wiki page for the app.
