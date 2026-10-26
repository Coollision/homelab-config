---
paths:
  - "workload/**"
  - "system/**"
  - "tests/**"
  - "lib/**"
---

# Helm app conventions

- Top-level `values.yaml` key is `deployment:` or `statefulset:` (stateful data → statefulset). Ports map `name: number` and auto-create the Service.
- Ingress blocks: `ingress_internal`, `ingress_internal_secure`, `ingress_external_secure`; hosts use `<secret:kv/data/shared/domains~local|domain>`.
- Storage list: `nfs-client` (`storagePath`) or Longhorn via `longhorn-storage-lib`. Longhorn Volume CRs are rendered by the libs; set `replicas:` in values, never patch live.
- Always set `resources.requests/limits`; memory limit must cover runtime heap (Node/JVM size off host RAM, not cgroup; cap via env).
- Comments in values explain **why** (gotchas, links to wiki), not what; keep that density.
- Changing `lib/*` fans out to every dependent chart: run `scripts/validate-charts.sh` (auto fan-out) and say how many charts it covered.
- Never edit `Chart.lock`, `charts/*.tgz` (gitignored); run `helm dependency update` instead.
- A sync-wave (`syncWave:`) only matters for ordering across apps; don't add one without a dependency reason.
- New or renamed app/port/path/behaviour → update the matching `wiki/` page in the same change.
