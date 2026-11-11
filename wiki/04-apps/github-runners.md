# GitHub Actions runners (ARC)

Autoscaling self-hosted runners for the GitHub org, using Actions Runner Controller in scale-set mode.

| Part | Path | Namespace |
|---|---|---|
| Controller | `system/arc-systems/arc-controller` | `arc-systems` |
| Org runner scale set | `workload/arc-runners/original-group` | `arc-runners` |

- Workflows use `runs-on: original-group`. Scale 0 to 3 runners (dind mode, amd64 only).
- Auth is a GitHub App installed on the org; credentials live in Vault at
  `kv/workload/arc-runners/original-group` (keys `app-id`, `installation-id`, `private-key`).
- Runner group must allow the target repos (org Settings > Actions > Runner groups).
- Another org later: copy the scale-set dir to `workload/arc-runners/<org>` (the controller is shared, one scale set per org) and change `githubConfigUrl` + Vault path.
