# TODO — k3s tuning: image pulls, graceful shutdown, automated upgrades

[← Back to Home](../Home.md) · Related: [Cluster housekeeping](../01-platform/cluster-housekeeping.md) · [Ansible](../00-setup/ansible.md) · [Registry mirror](registry-mirror.md)

> **Status (2026-11-03):** ideas collected while auditing kube-fledged; nothing applied or verified on the
> cluster yet. Kubelet args go through `ansible/roles/k3s_pre/templates/config.yaml.j2`. Delete this page once done.

## 1. Faster image pulls

Two separate knobs, do not mix them up:

- **Kubelet `serializeImagePulls: false` + `maxParallelImagePulls`:** several *images* at once (one slow pull no
  longer blocks the others on a node).
- **Containerd `max_concurrent_downloads`:** parallel *layers within one image*. Believed default is 3 (verify for the
  installed containerd). Needs a k3s containerd config template, since k3s regenerates `config.toml`.

Reality check: Home Assistant's ~660 MB image took ~85 s (~8 MB/s) from ghcr, which looks upstream- or unpack-limited,
not layer-concurrency-limited. Benchmark before keeping any change; combine with the [mirror test](registry-mirror.md).

- [ ] Measure a cold pull of a multi-layer image with the defaults, then with a higher layer limit.
- [ ] Decide whether parallel image pulls matter (only when several images land on a node at once, e.g. after a drain).

## 2. Graceful node shutdown

Kubelet `shutdownGracePeriod` / `shutdownGracePeriodCriticalPods` let pods terminate cleanly on a reboot or poweroff.

- [ ] Add the kubelet args via `config.yaml.j2`; check logind inhibitor delay allows the chosen period.
- [ ] Find a safe way to test (one drained node, reboot, watch Longhorn engines and pod terminations).

## 3. system-upgrade-controller, paired with Longhorn safety

Automated k3s upgrades (cordon + drain one node at a time, upgrade in order) instead of manual node-by-node work.
Pair it with a Longhorn-health gate, otherwise it automates the risky "quick restart" case.

- [x] **`KillMode` investigated.** The `control-group` came from a template bug (see
      [Known Issues](../05-operations/known-issues.md#node-stack-gotchas)); fixed, units now `KillMode=process`, so plain k3s
      restarts leave pods running and need no drain. The Plan's drain is then only needed for OS/reboot work.
- [ ] Decide on upgrade windows and plans (servers first, one node at a time); use a drain config that respects
      PodDisruptionBudgets and Longhorn.
- [ ] Add a gate: do not upgrade the next node until all Longhorn volumes are healthy (pre-check Job or a `prepare`
      step in the Plan).
- [ ] Renovate/ArgoCD interaction: how the `Plan` CRs and the k3s version pin are kept in Git.

## 4. Follow up: `NodeMemoryMajorPagesFaults` on the smallest worker

Alert fired 2026-11-08 ~22:30-22:55 CET (~1100 major faults/s, threshold `> 500` over 5m). Investigated: the node was fine
(swap off, `MemAvailable` 31-38 %), but **a single container was thrashing inside its own limit**: `flaresolverr`
(headless Chromium) sat at ~99 % of its 512Mi limit, so the kernel kept evicting its file pages and it kept re-reading
them (~220 MB/s). It is the only container cluster-wide above 50 faults/s in 7 days. The alert was right, just worded
as a node problem. Raising the alert threshold was rejected: it would hide this and, with swap off, would likely never fire.

- [x] Raised flaresolverr memory (limit 512Mi to 1Gi, request 352Mi to 512Mi).
- [ ] Optional: disable the stock rule and add `ContainerMemoryThrashing` (container pgmajfault rate vs. % of memory limit)
      plus a node rule that also requires `MemAvailable < 10 %`; needs a live backtest first.
- [ ] Re-check around 2026-11-22 that the alert stays quiet (2026-11-09: no alerts firing, flaresolverr ~340Mi of 1Gi, only the old pod thrashed).
- [x] Side finding: `node_exporter` returned two series for the same node. Gone by 2026-11-09 (4 series, one per node, all up).
