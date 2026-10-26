# Homelab GitOps repo

K3s cluster managed by ArgoCD from this repo. **Public repo**: never write private domains, IPs,
node hostnames or emails into any tracked file. Use `<secret:kv/...>` (configs) or placeholders
like `<internal-domain>`, `<node-name>` (docs). Details live in `wiki/` — grep it before asking.

## Layout

| Path | What |
|---|---|
| `system/<namespace>/<app>/` | infrastructure apps (project `system`, set `system-config`) |
| `workload/<namespace>/<app>/` | workloads (project `workloads`, set `workloads`); namespace = parent dir |
| `tests/<ns>/<app>/` | scratch apps deployed from the **`tests` branch** (see `tests/README.md`) |
| `lib/shared-lib` | Helm library: `{{ include "shared-lib.all" . }}`; also `arr-lib`, `longhorn-storage-lib` |
| `applications/` | ApplicationSets + AppProjects (auto-discover dirs; app name = dir basename) |
| `argocd/` | self-managing ArgoCD chart · `ansible/`, `node-setup/` host provisioning |
| `wiki/` | MkDocs site (`properdocs.yml`); `docs/` = long-form runbooks |
| `.claude/` | skills, agents, rules, hooks (see below) |

- A `disabled-<name>` dir is excluded from discovery. Renaming a dir = delete + recreate of the ArgoCD app.
- Apps are one Helm chart each: `Chart.yaml` (depends on `shared-lib` via `file://`) + `values.yaml`
  (+ `templates/app.yaml` containing just the include). Copy a small neighbour (e.g. `workload/smarthome/whisper`) rather than inventing structure.
- Rendering goes through `argocd-lovely-plugin`, which resolves `<secret:kv/data/<path>~<key>>` from Vault at sync time. Locally those stay literal strings, so `helm template` works without Vault.
- `scaleToZero.enabled` in values → Sablier; `applications/set-*.yaml` ignoreDifferences are **generated** by `scripts/gen-scale-to-zero-ignores.sh` (pre-commit + CI). Never hand-edit the autogen region.
- Renovate owns image/chart version bumps (`renovate.json`); don't hand-bump unless asked.

## Workflow

1. **Understand before editing**: read the app's `values.yaml` + matching `wiki/` page. Non-trivial or ambiguous work → `/grill-me` first, then plan.
2. **Edit** (path rules in `.claude/rules/` load automatically per directory).
3. **Verify locally before saying "done"**: `scripts/validate-charts.sh` (yamllint + helm lint + helm template on changed charts, mirrors CI). A hook runs it after every chart edit and only speaks on failure. For behaviour changes also diff the render: `helm template x <dir> | grep -n <thing>`.
4. **Ship**: Conventional Commits (`chore(deps):`, `feat(<app>):`, `fix(<app>):`, `docs(wiki):`). Plain `git push`; **never force-push** unless explicitly told. Commit only when asked.
5. **Observe** ArgoCD/cluster through `kubectl`/`argocd` from the laptop (nodes have no kubeconfig). Delegate noisy investigation to the `cluster-investigator` agent so logs stay out of the main context.

## Hard rules

- GitOps wins: ArgoCD `selfHeal` reverts live `kubectl patch`/`scale` in seconds. Change `values.yaml`, not the cluster (use `kubectl` for read/diagnose, or deliberate temporary maintenance mode in the ArgoCD UI).
- Don't assume where a service runs (K3s vs NAS vs HA host): verify the backing host first.
- Vault: never `kv delete/undelete/destroy` without asking. Secrets never go in git.
- Destructive cluster actions (`delete pvc|pv|ns`, Longhorn volumes, DB drops) need explicit user OK each time.
- ArgoCD manifest cache can serve stale renders: if a synced change isn't live, see skill `gitops-debug`.
- Changing apps/ports/paths/behaviour documented in `wiki/` → update the wiki page too (a pre-commit hook also tries).

## Skills & agents (project-level, `.claude/`)

| Invoke | Use for |
|---|---|
| `/grill-me <topic>` | interview me one question at a time (with a recommended answer) before a design/migration |
| `/new-app <ns>/<name>` | scaffold a shared-lib app, validate render, add wiki stub |
| `/validate [dir...]` | run `scripts/validate-charts.sh` and summarise failures only |
| `/gitops-debug <app>` | ArgoCD out-of-sync / degraded / stale-render triage |
| `/promote-test-app <name>` | move an app from `tests/` to `system/` or `workload/` without downtime |
| `/renovate-batch-verify` | check open Renovate bump branches for breaking changes + render, batch the safe ones on one branch |
| `/wiki-sync` | reconcile `wiki/` with the working-tree diff |
| agent `chart-reviewer` | independent read-only review of a chart diff against repo conventions |
| agent `cluster-investigator` | read-only kubectl/argocd digging, returns a short conclusion |

Domain skills (zigbee, matter/thread, esphome) are local-only and trigger by description.

## Token hygiene

- Prefer `Grep`/targeted `Read` (offset/limit) over reading whole values files; `kubectl ... -o jsonpath`/`--no-headers`/`| head` over full YAML dumps; `kubectl logs --tail=100`.
- Use `model: sonnet` agents for exploration and CLI-heavy digging; max 2 agents concurrently.
- Keep this file short: put path-specific guidance in `.claude/rules/`, procedures in skills, facts in `wiki/`.

## Compact instructions

When compacting, keep: the task goal, files changed + why, failing validation output, pending user decisions, and which ArgoCD apps/namespaces are involved. Drop raw kubectl/log dumps and file contents already committed.
