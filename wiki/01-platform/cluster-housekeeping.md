# Cluster housekeeping

[← Back to Home](../Home.md) · Platform: [Observability](observability.md) · [Storage](storage.md)

Five small controllers that keep the cluster tidy or make it cheaper to run idle.

## Keel — automated image updates

`system/kube-system/keel` runs a community fork image (`fardjad/keel:latest`, not the
official image — no in-repo explanation why, worth confirming next time it's touched)
behind basic auth. It's the mechanism behind any chart using a `latest`/floating image
tag with a `keel:` policy block (e.g. Jackett, Tautulli in the media stack — see
[Media stack](../03-media-stack/overview.md)) — those get force-updated on a schedule
rather than waiting for a Renovate PR.

## kube-fledged — image pre-caching

`system/kube-system/kube-fledged` pre-pulls/caches container images on nodes via
`ImageCache` CRDs (defined per-workload, not centrally). Useful for large images
(ESPHome's build toolchain, monitoring stack images) where a cold pull would otherwise
delay a pod's first start noticeably.

## Reloader — status unclear

`system/kube-system/reloader` is listed here for completeness, but as of this writing
its `Chart.yaml`/`values.yaml` don't exist on disk *or* in git, and no workload anywhere
in the repo carries a `reloader.stakater.com/...` annotation. **Don't assume this is
active** — verify against the live ArgoCD UI before relying on config-change-triggered
rollouts here.

## Node Feature Discovery + descheduler

One umbrella chart, two upstream projects bundled together because NFD's labels drive
the descheduler's placement logic:

- **NFD** runs custom USB-device detection rules that label nodes:
  `feature.node.kubernetes.io/iot-zigbee-coordinator`,
  `feature.node.kubernetes.io/iot-thread-border-router`,
  `feature.node.kubernetes.io/nut-ups-apc` — these are what let the Zigbee coordinator,
  Thread border router, and UPS server pods pin themselves to whichever physical node
  actually has that USB device plugged in, via required `nodeAffinity`.
- **Descheduler** runs three profiles (`mini-nodes`, `vm-nodes`, `hw-enforcement`) —
  the last one actively evicts any pod violating a required node affinity, which is the
  enforcement mechanism behind the USB-device pinning above. Storage pods
  (`evictableNamespaces.exclude: [storage]`) are explicitly protected from eviction.

**Non-obvious tuning:** the descheduler balances on **actual metrics-server CPU/memory
usage**, not pod *requests* — a deliberate fix after the default requests-based algorithm
caused endless, futile eviction churn of 0-request BestEffort pods while a control-plane
node sat at 94% CPU by *requested* (not actual) load.

## Sablier — scale-to-zero for idle apps

`system/kube-system/sablier` + a Traefik plugin (present in both Traefik instances) scale
idle web-UI Deployments to zero replicas and wake them on the next incoming request.
One shared Sablier controller serves every enrolled app.

**Currently enrolled:** echo (test app, 2m), CloudBeaver (30m), PeaNUT (30m), ESPHome
(3h — deliberately long to avoid interrupting batch firmware updates), ha-mcp (30m,
blocking wait), kubernetes-mcp-server, Longhorn UI, and the ArgoCD server itself (the
last three via hand-wired label patches since their upstream charts have no
`scaleToZero` knob). **BabyBuddy was tried and explicitly reverted** — used too often for
the wake delay to be worth it.

**The single most important operational fact about Sablier here:** its periodic
reconciliation loop lists Deployments, StatefulSets, *and* CNPG `Cluster` CRs in one
combined call, and a permissions error on **any** of the three silently breaks discovery
for **every** enrolled app, every cycle — not just CNPG-related ones. `rbac.cnpg: true`
must stay set even though this cluster never uses CNPG hibernation. See
[Known Issues](../05-operations/known-issues.md#sablier-scale-to-zero-gotchas) for the
full list of trade-offs this rollout surfaced (MCP session drops, Longhorn RWX detach
races, ArgoCD selfHeal fighting a manual scale).

## Practical tools

- `scripts/sablier-status.sh` — shows asleep/awake state, time-awake, and time-to-sleep
  per enrolled app.
- `scripts/gen-scale-to-zero-ignores.sh` — regenerates the ArgoCD `ignoreDifferences`
  block in `applications/set-*.yaml` for every `shared-lib` app with `scaleToZero.enabled`
  — hand-wired bespoke apps get a manually-maintained entry *outside* the autogen markers
  so the script doesn't delete them.
