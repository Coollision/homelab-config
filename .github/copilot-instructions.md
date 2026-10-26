# Homelab GitOps repo: agent guide

Single source of truth: [`CLAUDE.md`](../CLAUDE.md) (layout, workflow, hard rules) plus path-scoped rules in `.claude/rules/`. Read those first.

Essentials:

- GitOps: ArgoCD ApplicationSets auto-discover `system/<ns>/<app>` and `workload/<ns>/<app>` (one Helm chart each, usually via `lib/shared-lib`). `disabled-*` dirs are skipped.
- Secrets are `<secret:kv/data/...~key>` placeholders resolved by `argocd-lovely-plugin`; never commit real secrets or private domains/IPs/hostnames (public repo).
- Validate before finishing: `scripts/validate-charts.sh` (yamllint + helm lint + helm template).
- Don't hand-edit the generated scale-to-zero region in `applications/set-*.yaml`; Renovate owns version bumps.
