---
name: gitops-debug
description: Triage an ArgoCD application that is OutOfSync, Degraded, Progressing forever, or whose pushed change is not live (stale render). Use when the user says an app won't sync, a change didn't apply, or an ApplicationSet isn't picking up a directory.
argument-hint: <app-name>
context: fork
agent: cluster-investigator
---

Investigate ArgoCD app `$ARGUMENTS` (read-only; do not sync/patch/delete). Report a short verdict + proposed fix.

Order of checks (stop at the first root cause):

1. `argocd app get $ARGUMENTS` or `kubectl -n argocd get app $ARGUMENTS -o jsonpath='{.status.sync.status} {.status.health.status} {.status.conditions}'`: sync/health/conditions and the synced revision vs `git rev-parse origin/master` (or `origin/tests` for tests/ apps).
2. **App missing?** Dir must match the ApplicationSet glob (`workload/*/*`, `system/*/*`, `tests/*/*`) and must not be `disabled-*`. Name collision: app name is the dir basename, unique across all sets; an app owned by `tests` cannot be adopted (ownerReferences).
3. **Render error?** `kubectl -n argocd get app $ARGUMENTS -o jsonpath='{.status.conditions[*].message}'`; reproduce with `helm template` locally. Plugin timeouts / repo-server OOMKilled → `kubectl -n argocd get pods`, `kubectl -n argocd logs deploy/argocd-repo-server -c <plugin sidecar> --tail=80`.
4. **Synced but old values live?** The lovely-plugin manifest cache can survive `refresh=hard`; compare `kubectl get <kind> <name> -o yaml` to the local render. Suggested remedy: restart repo-server (needs user OK).
5. **Reverted live edit?** `selfHeal` undoes patches in seconds. Fix belongs in `values.yaml`.
6. **Stuck resources**: pods Pending (PVC/Longhorn attach, node affinity, Multus NAD), `Terminating` with hard NFS/SMB mounts, scale-to-zero apps (Sablier) showing 0 replicas = expected, not Degraded.
7. **Vault sealed?** `<secret:...>` render failures across many apps at once → check Vault pod status first.

Return: root cause (1-2 lines), evidence (commands + key output, trimmed), recommended fix, and what needs the user's approval.
