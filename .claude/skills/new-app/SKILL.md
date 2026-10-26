---
name: new-app
description: Scaffold a new shared-lib Helm app under workload/<namespace>/<name> (or system/) following repo conventions, validate the render, and add the wiki stub. Use when the user wants to deploy a new service/container on the cluster.
argument-hint: <namespace>/<name> [image]
disable-model-invocation: true
---

# New app: $ARGUMENTS

1. Parse `<namespace>/<name>`; choose tree (`workload/` default, `system/` for infra). Ensure `<tree>/<ns>/<name>` does not exist (also check `disabled-<name>`).
2. If image, ports, storage, ingress, secrets or DB are unclear → run the `grill-me` flow first (max 5 questions).
3. Copy the closest existing app as the template (`workload/smarthome/whisper` for a stateful single container; check `ls <tree>/<ns>` for a closer neighbour). Files: `Chart.yaml` (name, `shared-lib` dependency via `file://../../../lib/shared-lib`), `templates/app.yaml` (`{{ include "shared-lib.all" . }}`), `values.yaml` (`fullnameOverride`, `deployment|statefulset`, resources, storage, ingress, TZ `Europe/Brussels`).
4. Pin the image tag (no `latest`) so Renovate can bump it; secrets as `<secret:kv/data/...~key>` and tell the user which Vault keys they must create (never invent values).
5. `cd <dir> && helm dependency update`, then `scripts/validate-charts.sh <dir>`; inspect `helm template` for service, ports, PVCs, ingress host.
6. If `scaleToZero.enabled: true` → run `scripts/gen-scale-to-zero-ignores.sh`.
7. Add/extend the relevant `wiki/` page (placeholders only, no private identifiers).
8. Report: files created, Vault keys needed, validation result. Do not commit or push unless asked.
