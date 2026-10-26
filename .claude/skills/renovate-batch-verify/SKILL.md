---
name: renovate-batch-verify
description: Check all open Renovate/Dependabot bump branches for breaking changes and successful render, then batch the safe ones onto one local branch for a single combined commit. Use for "check the renovate branches", "batch the dependency updates", "review the bump PRs", "put all updates on one branch".
argument-hint: [branch-filter]
disable-model-invocation: true
---

# Renovate batch verification $ARGUMENTS

Goal: per bump branch, a verdict (breaking changes that hit **our** config, plus what we gain) and a
proven render; then one local branch with the safe ones. Never push unless asked.

Context: `renovate.json` automerges most patch/minor bumps (system/argocd) overnight, so the open
`renovate/*` branches are the ones needing eyes (majors, workload minors, grouped weekly PRs such as
`renovate/master-platform-charts`). Branches come in three kinds: image tag, Helm chart `version:`
(file://, https:// or oci:// dependency), GitHub Action ref.

## 1. Discover

```bash
git fetch --all --prune
git branch -r | grep -E 'renovate/|dependabot/'
git log --oneline origin/master..origin/<branch>
git diff origin/master...origin/<branch>
```

- A branch with **no merge base** (e.g. `orphan/*` lineage) errors on `...`: exclude it, report it separately, ask before touching.
- Build a table: dependency · type · old→new. Flag **majors** as highest risk.

## 2. Check CI before rendering

`helm-validate.yml` already ran `helm dependency update` + `lint` + `template` on exactly the changed charts of every pushed branch. A green run **is** the render verification (CI is also more authoritative than local Helm, whose version differs).

```bash
gh pr checks <pr-or-branch>        # or: gh run list --branch <branch> --limit 3
```

Record `RENDER: PASS (CI <run-url>)` and skip the local render, **unless**:

- CI is red, missing, skipped or didn't cover the chart → `scripts/validate-charts.sh <chart-dir>` with that branch's files applied (`git checkout origin/<branch> -- <files>`).
- The release notes show a breaking change that plausibly touches our values/templates, or it is a **major chart bump** → render to prove our config still works and diff old vs new (`helm template` both, `diff`) for RBAC/CRD/default/image changes. A passing render only proves it *renders*, not that a renamed values key isn't now silently ignored.
- `oci://` pulls failing in a sandbox are an environment limit: report `BLOCKED-network`, never fake success.
- Local Helm 4 renders subchart null-overrides and `grafana-dashboards`/`grafana-alerts` differently from ArgoCD/CI (empty locally); don't chase that diff.

## 3. Dispatch Sonnet workers, 2 at a time

One `general-purpose`, `model: sonnet`, `isolation: worktree` agent per branch that needs research. **Max 2 concurrent**: run waves of 2. Patch image bumps with green CI can be judged inline from the diff + release notes.

Each prompt must carry: file, old→new, branch name, and demand:

- **Breaking-change research**: fetch upstream release notes *between* the two versions, read *this repo's* values for that app, judge whether our config is actually hit. Cite URLs; don't paste changelogs.
- **What we gain**: security fixes (name the CVE/GHSA), bugs we've hit, features worth enabling. Skip UI/i18n churn.
- **Render** per step 2 (only when required).
- GitHub Action bumps: no render; check the workflow YAML still parses and that removed inputs, runner/Node requirements or trigger changes don't affect it.
- Structured, concise output: `VERDICT: SAFE | NEEDS-ATTENTION | BLOCKED` + one line, breaking changes (do they hit us?), gains, render result, pre-merge action.

Scale effort to risk: light for patch bumps, old-vs-new render diff and values audit for majors.

## 4. Build the combined branch (SAFE only)

```bash
git checkout -b chore/renovate-batch-<YYYY-MM-DD> master
git checkout origin/<branch> -- <files that branch changes>   # per branch; NOT git merge
git status --short && git diff --cached --stat
```

- File-by-file checkout gives one clean staged set without merge commits and never stages pre-existing untracked work-in-progress; still confirm with `git status --short`.
- Grep the new version strings in the staged files.
- Run `scripts/validate-charts.sh` once on the staged set (includes `lib/` fan-out).
- If a bump changes a version documented in `wiki/`, update that page.
- **One commit**, message `chore(deps): batch renovate updates <date>`, body per update: verdict, whether a breaking change hits us, gains (security first, with ids), pre-merge actions. Add the attribution trailer. Commit only if the user asked for it.

## 5. Clean up worktrees

Agent worktrees survive when they applied files. After collecting reports:

```bash
git worktree list                                          # only .claude/worktrees/agent-* entries
git worktree remove --force .claude/worktrees/agent-<id>
git branch -D worktree-agent-<id>
git worktree prune
```

Never touch non-`agent-*` worktrees (e.g. a long-lived `orphan/*` checkout).

## 6. Report

1. **Verdict table**: dependency · bump · render (local or "CI green") · verdict + note.
2. **What you're getting**: per update, useful fixes/features, security first with CVE/GHSA. Mandatory; a risk-only report reads as pure danger.
3. New branch name + commit.
4. **Excluded**: no-merge-base branches, BLOCKED renders.
5. **Pre-merge / post-rollout actions** collected in one place.

Offer to delete the redundant remote bump branches after the merge.

## Gotchas

- **GitOps self-heal**: ArgoCD reverts live `kubectl patch` in seconds; verification means render (or CI), not live edits.
- **DB-bearing apps** (n8n, Postgres operators, etc.): a minor bump can carry an irreversible migration; flag a pre-rollout DB snapshot even when render is clean.
- **Major chart bumps**: breaking changes are often in values keys we don't use (check our values), but confirm the rendered output is structurally identical apart from the intended version.
- **kube-prometheus-stack**: Thanos is not used here (verify: `kubectl get thanosrulers -A` empty, Prometheus `.spec.thanos` empty), so ignore Thanos notes. CRD changes across operator minors are normally additive optional fields (check `storedVersions` with `kubectl get crd <kind>.monitoring.coreos.com -o custom-columns=...`); report as low-risk housekeeping (`crds.upgradeJob.enabled` or one-time server-side apply), not a blocker.
- **Dir renames** recreate the ArgoCD app; a bump branch that moves directories needs extra care.
