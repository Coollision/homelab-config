# TODO — UPS: staged load shedding, graceful shutdown and auto-recovery

[← Back to Home](../Home.md) · Related: [Power / NUT](../01-platform/power-nut.md) · [k3s tuning](k3s-tuning.md) (kubelet graceful shutdown) · [Storage](../01-platform/storage.md) · [Databases](../01-platform/databases.md)

> **Status (2026-11-08):** reviewed design, nothing built; next step is the live dry-run in §7b. Replaces the scratch plan and the sketch in
> `system/nut/disabled-automation/`. Hard requirement: **a power outage must never cost data.** Runtime is a
> secondary goal. Delete this page once implemented and move the result into [Power / NUT](../01-platform/power-nut.md).

## 1. What the shedding actually buys (read this first)

Baseline from the 2026-10 audit and live NUT: ~155 W at the wall, UPS output ~108–124 W, `battery.runtime` ≈ 2500 s
(~42 min) at ~12 % load. The UPS's own ~31 W is not sheddable.

Source: [rack power audit 2026-10-14](../01-platform/power-nut.md) (155 W at the wall, UPS output ≈ 124 W, UPS self ≈ 31 W).

| Rung | Cuts | ≈ W saved | UPS-output load | Runtime (≈) |
|---|---|---|---|---|
| mains | — | — | ~124 W | ~42 min |
| 1: NAS (the VM worker is *inside* its 28.5 W) | NAS | −28 | ~96 W | ~55 min |
| 1b: PoE ports | technical-room AP + living-room switch (§4) | −6 (≈ −7 with PSU loss) | ~89 W | ~59 min |
| 2: cluster down (3 minis/Pi ≈ 23 W) | workloads → DBs → nodes | −23 | ~73 W | clean off, then the UPS is cycled |
| floor (only until the cluster stops) | gateway + Protect HDD 22, switch 16, kept PoE devices ≈ 17, ISP box 9 | — | ~67 W | — |

Honest ceiling: with the whole ladder the load drops to ~67 W, so at best ~1.8× the baseline; shedding is worth
~+17 min. More importantly **the network has no value once the cluster is down** (DNS, Home Assistant and the rest live
there), so there is no "keep the network and cameras alive on the leftover battery" phase: the cluster is the thing
worth keeping up, and it stops with battery to spare. The design target is **a clean shutdown with margin**, and the
UPS is cycled straight after, not run flat (also kinder to an ageing lead-acid battery). Everything below is ordered by
data safety first. The UPS's own 31 W is paid in every rung.

## 2. Review of the scratch plan — what had to change

| # | Problem in the scratch plan | Fix in this plan |
|---|---|---|
| 1 | n8n is the only executor, but it lives on the UPS node, depends on its own CNPG DB + Longhorn + k3s. If the cluster is already degraded, **nothing shuts the NAS down.** | Two layers (§3): a dumb cluster-independent safety net (NUT clients + kubelet graceful shutdown) under the smart orchestrator. |
| 2 | "Drain `<ups-node>` and `<control-plane-node>`" then n8n SSH-poweroffs. Drain evicts n8n itself mid-run. | No end-of-run drain. Scale down by tier, wait for volumes to detach, then rely on kubelet graceful shutdown + scheduled poweroff. n8n/orchestrator pod is the last tier-0 workload. |
| 3 | `load.off` only as a backstop. If mains returns *during* the shutdown the nodes stay off (BIOS "power on after AC loss" never triggers because AC never dropped). | The UPS must be power-cycled **after** the hosts are off. The live driver offers `shutdown.reboot`, `shutdown.default`, `load.off.delay`, `shutdown.stop` (**no** `shutdown.return`/`stayoff`). Candidate: `shutdown.reboot` or `load.off.delay <s>` with ≥ 120 s delay; which one restarts the outlets with mains present is a **bench test** (§8.1). |
| 4 | `LB` flag fires with only 120 s left (`battery.runtime.low=120`, `battery.charge.low=10`). | Both are **writable** on this UPS: raise them declaratively via `override.battery.runtime.low` / `override.battery.charge.low` in `ups.conf` (no `ignorelb` needed, none set today), target ≈ 300 s / 15 %. NOTE the NAS is already a NUT secondary and shuts itself down on `LB`/FSD (§2b), so raising `LB` also moves the DSM-native shutdown earlier. Intended. |
| 5 | 5-min Slack warning + Wait burns ~12 % of the battery at full load and blocks on **internet reachability** (street cabinet may be dead too). | No blocking wait. Notifications are best-effort (`continueOnFail`). "Abort" = automatic: re-check `OL` before every irreversible step. |
| 6 | State kept in an n8n Data Table (Postgres on Longhorn) — not readable until the thing being shut down is back. | State lives in a ConfigMap (`ups-state`, ns `nut`) + per-object annotation (§5). Survives n8n loss; readable by anything with kubectl. |
| 7 | UniFi writes (PoE off) would use the cloud proxy, which dies with the internet. | Go through Home Assistant, which already has local (non-cloud) UniFi access (§4). |
| 8 | Only `arr-stack` hard-coded as the scale-down target. | Label-driven tiers (§5): git is the source of truth, nothing hard-coded in the workflow. |
| 9 | CNPG shutdown not addressed. Data-loss hotspot (HA recorder, service DBs, n8n). | Explicit DB step: CNPG hibernation annotation (clean checkpoint) after dependants are down, before nodes stop. |
| 10 | Longhorn replicas on the VM worker: powering it off degrades volumes. | Gate = every attached volume keeps ≥ 1 healthy replica elsewhere. HA, Z2M and the DB are 3-replica volumes (2 left, fine). Mosquitto is 2-replica (blue + VM): since it now stays up through the outage, **raise it to 3 replicas** (no longer optional). Loki / Prometheus end up single-replica for the outage; accepted. |
| 11 | Mount hazards: NAS-backed pods stuck `Terminating` when the NAS disappears (hard mounts). | NAS-dependent workloads are a tier that is scaled down and **confirmed gone** *before* the NAS is powered off. |
| 12 | Plan says "all three nodes" for BIOS/AC-restore. | Four k3s nodes (3 minis/VM + the Pi). Check BIOS on the two minis; the Pi powers on by default; the VM needs **VMM autostart** (currently *disabled*, §2b). |
| 13 | Recovery depends on n8n finishing. If it doesn't, apps stay at 0 replicas with ArgoCD frozen — silently. | A boot-time dead-man: CronJob `ups-recover` (every 5 min) that completes recovery if state ≠ `NORMAL` and mains has been stable. Alert if still frozen after 15 min. |
| 14 | `battery.runtime` is the UPS's *estimate* from load only; the battery is old (RBC176 swap pending). | Trigger on `charge` **or** `runtime` **or** a hard wall-clock cap, and calibrate with one real discharge test (§8). |

## 2b. Verified facts (2026-11-07, read-only)

| Area | Fact | Consequence |
|---|---|---|
| NUT driver | `usbhid-ups`, NUT 2.8.3, `pollonly`, no `ignorelb`/`override.*`; `battery.charge.low`/`battery.runtime.low` writable; `battery.mfr.date` is bogus (2001); runtime ≈ 2400 s at 14 % load | Thresholds via `ups.conf` overrides. Write the real battery swap date into `battery.mfr.date` to track age. |
| NUT commands | `load.off`, `load.off.delay`, `shutdown.default`, `shutdown.reboot`, `shutdown.stop`, `driver.killpower` (needs `driver.flag.allow_killpower`, currently 0). No `shutdown.return`. | See §2 row 3. Leave `allow_killpower` at 0 until tested. |
| upsmon in the `nut` pod | primary, `SHUTDOWNCMD "/sbin/shutdown -h +0"` (only affects the pod), no `NOTIFYCMD`, `FINALDELAY 5`; `admin` has SET + `instcmds ALL`; `monuser` secondary exists | Add a `NOTIFYCMD` (FSD → cycle UPS). Nodes can use `monuser` as NUT clients. |
| NAS | DSM 7.4.1; **already a NUT secondary of the cluster's UPS server**, `upssched` handles ONBATT / LOWBATT / FSD via `synoups`; WoL enabled; DS supports Power Recovery (the on/off value is UI-only) | NAS already shuts down at `LB`. Layer A for the NAS exists; only the earlier stage-1 poweroff is new. Verify the DSM Power Recovery toggle in the UI. |
| NAS history | DSM "Safe Mode" fired 2026-06-02 and 2026-07-20 (safe mode → `synopoweroff` in ≈ 2 min 10 s) | The native chain has worked. Real data point: NAS shutdown ≈ 2–3 min. Check whether it restarted by itself after 2026-07-20 (next log line is 8 days later). |
| NAS VM | one guest, 3 vCPU / 6 GB, **autostart: disabled**, `on_poweroff=destroy`; NAS also runs `dns-server`, `netbootxyz`, `k3s_postgres-registry` containers | Recovery must start the VM (enable VMM autostart: manual DSM step). Stop the guest *gracefully from inside* (SSH `poweroff`) before the NAS; don't rely on `destroy`. |
| NAS access | `youri` has full sudo; `poweroff`/`shutdown` exist | SSH `poweroff` is viable; no VMM API needed. |
| Kubelet | `shutdownGracePeriod` = `shutdownGracePeriodCriticalPods` = **0 s on all 4 nodes**; logind `InhibitDelayMaxSec` = 5 s on all 4 (Pi checked 2026-11-07); k3s unit `KillMode=control-group` on the minis/VM but **`process` on the Pi** | Today a plain `poweroff` kills pods. Layer A needs both: kubelet args (via `k3s_pre` config) **and** a logind drop-in `InhibitDelayMaxSec` ≥ grace period. |
| Node tooling | `nut-client`/`upsmon` not installed anywhere; `homelab` has NOPASSWD sudo on the three reachable nodes; the Pi is reachable as its own user (local SSH config) and has NOPASSWD sudo too | Ansible role for NUT client + logind drop-in; sort out Pi SSH first (§9). |
| Longhorn | replicas only on the two minis + the VM (none on the Pi); `node-down-pod-deletion-policy=delete-both-statefulset-and-deployment-pod`, `replica-replenishment-wait-interval=300`. 3-replica volumes: all app configs, HA, n8n, Vault, both DB volumes. 2-replica incl. VM: mosquitto, loki, prometheus | Shutting the VM leaves those three single-replica. Rebuild window starts after 300 s on boot. |
| CNPG | `service-db-pg` (Pi) and `smarthome-db-pg` (control-plane), **1 instance each**, `stopDelay=1800`, `smartShutdownTimeout=180`, pod grace 1800 s | Hibernate them only after dependants (HA, n8n, arr) are down; budget ≤ 3 min each (smart shutdown), in parallel. |
| NAS-mounting workloads | radarr, sonarr, bazarr, lidarr, slskd, music-intake (arr-stack), music-assistant (smarthome), nfs provisioner (storage, 2 replicas) | `needs: nas` tier. Prefer **auto-detecting** them at runtime (PVs whose NFS server is the NAS) over trusting labels alone. |
| ArgoCD | controller is a StatefulSet; ArgoCD manages itself; scaling the controller to 0 sticks because nothing else reconciles it | Freeze = scale controller + applicationset-controller to 0, as planned. |
| Slow terminations | prometheus 600 s, alertmanager 120 s, CNPG 1800 s, rest ≤ 60 s | Monitoring stays up to the end (the outage is the thing you want metrics for) and is stopped last with a short grace override (60–120 s): Prometheus replays its WAL on start, so a short grace costs nothing. CNPG is bounded by `smartShutdownTimeout` (180 s) once clients are gone. |

Design consequence: the orchestrator reads UPS state **directly from NUT**, never from Prometheus, so it keeps working
if monitoring is gone or broken. Prometheus, Alertmanager, Grafana and Loki are the `final` tier: kept running until the
last moment so the whole outage is on record (the NUT exporter keeps scraping). The orchestrator also writes its own
timeline (stage start/end times, thresholds hit) to `ups-state` and Slack for the post-mortem.

## 3. Architecture: two layers

```
Layer A  SAFETY NET (dumb, no n8n, no ArgoCD)          Layer B  ORCHESTRATOR (smart, tunable)
 - NUT clients: each node (ansible) + DSM on the NAS     - staged shedding by label tier
 - kubelet shutdownGracePeriod → plain `poweroff`        - PoE shedding, notifications
   is safe for pods/Longhorn                            - ArgoCD freeze, DB hibernate, recovery
 - upsmon NOTIFYCMD in the nut pod on FSD →             - fires EARLIER than Layer A, so A only
   the UPS cycle command (§2 row 3)                                 acts when B is broken or absent
 - fires at native LB (≈5 min / 15 %)                  - fires at charge < 30 % or runtime < 8 min
```

- **Orchestrator host: decision pending (§9).** Candidates: n8n (existing `UPS` workflow, needs a community node + SSH +
  Synology creds), or a small script run as Jobs/CronJobs in `nut` (versioned in git, no extra credentials store).
  For a safety path the second is my recommendation; n8n can still do notifications.
- Layer A must be testable on its own (§8 test 3).

## 4. PoE / network rung

Measured via the UniFi API (2026-11-07). Only 7 ports draw PoE power, ~33 W in total (the audit's 26 W includes PSU
loss); the rest are `auto` at 0 W, so switching them off saves nothing.

| Port | Device (type) | ≈ W | Verdict |
|---|---|---|---|
| rack switch p9 | technical-room switch (feeds the AP, doorbell, chime below) | 11.6 | keep |
| rack switch p10 | office AP (also anchors the bedroom AP's wireless mesh) | 6.4 | keep (29 Wi-Fi clients, IoT) |
| rack switch p2 | driveway camera | 3.3 | only marginal candidate |
| rack switch p1 | living-room mini switch (only a Sonos behind it, no mains = no use) | 1.5 | **shed (1b)** |
| technical-room switch p1 | technical-room AP | 4.7 | **shed (1b)**: the office AP still covers 2.4 GHz |
| technical-room switch p3 / p2 | doorbell camera / chime | 4.5 / 1.3 | keep (security) |

Conclusion: **small PoE rung 1b: two port-offs ≈ 6 W (~5 % of the load, ≈ +4 min).** Shed the technical-room AP
(port 1 of the technical-room switch; do **not** cut that switch's uplink, it also carries the doorbell and chime) and
the living-room switch (rack p1). Wi-Fi coverage during an outage is not a goal; the office AP stays for 2.4 GHz and the
bedroom mesh. Driveway camera stays (3 W isn't worth losing coverage when it matters). Keep the gateway (its disk
records the cameras), the rack switch and the ISP box until the cluster stops.

**How to switch the ports: through Home Assistant.** HA already has local (non-cloud) access to the UniFi controller, so
the orchestrator needs no new UniFi credential: stage 1 calls an HA script (`switch.turn_off` on the two PoE ports),
and HA is up at that point (it is a `stage2` workload). Restore must not depend on the orchestrator: an HA automation
re-enables the ports on HA start and whenever the UPS is back on mains. HA currently exposes PoE *power-cycle buttons*
for these switches but no PoE on/off switch entities and no NUT sensors, so the per-port PoE switches probably need
enabling in the UniFi integration (verify). Raw API fallback: `PUT .../rest/device/<id>` with the *full* `port_overrides`
array (partial arrays wipe overrides). The bedroom switch and AP are not on the UPS.

## 5. Label / annotation contract (git is the source of truth)

Only workloads with **state or a dependency order** need a tier. Everything stateless (cert-manager, MetalLB, Multus,
Keel, reloader, descheduler, NFD, kube-fledged, exporters, ArgoCD's redis/dex/repo-server, Traefik, CoreDNS) stays
**unlabelled and is simply stopped by the nodes' graceful shutdown** (Layer A). Scaling it down earlier saves nothing and
only adds risk.

Set in an app's `values.yaml`; `shared-lib` renders it as a label on the Deployment/StatefulSet:

```yaml
power:
  shed: stage1      # stage1 | stage2 | final | keep
  needs: [nas]      # optional: stop BEFORE the NAS is touched (auto-detected too, see below)
```

Rendered as `power.homelab/shed=<stage>` / `power.homelab/needs=<dep>`. **Maintenance-free guard:** a CI/pre-commit
check (same idea as `gen-scale-to-zero-ignores.sh`) fails when a `workload/` app has persistence, a DB, or a NAS mount but
no `power:` key, so a new app can never silently miss its slot. Unlabelled-and-stateless is allowed.

At runtime the orchestrator:

1. Selects with `kubectl get deploy,sts -A -l power.homelab/shed=<stage>`; additionally **auto-detects** NAS users (pods
   mounting a PV whose NFS server is the NAS) and treats them as `needs: nas` even if unlabelled.
2. Writes `power.homelab/original-replicas=<n>` on the object **before** scaling (extra live annotations are ignored by
   ArgoCD's diff). Stateless fallback for recovery, no DB needed.
3. Scales to 0, waits for pods gone **and** Longhorn volumes detached, then continues.
4. Scales **Sablier** to 0 together with the ArgoCD freeze (otherwise an ingress hit wakes a scaled-to-zero app mid-shutdown).

### App-by-app tiers (proposal from the live cluster, 2026-11-07)

| Tier | Apps | Notes |
|---|---|---|
| `stage1` + NAS | radarr, sonarr, bazarr, lidarr, slskd, music-intake, music-assistant, NFS provisioner | stop and confirm **gone** before the NAS poweroff (hard mounts wedge pods in `Terminating`) |
| `stage1` | prowlarr, jackett, jellyseerr, prefetcharr, flaresolverr, tautulli, `service-db-pg` (hibernate after the arr apps), babybuddy, geocache app, mobileraker-companion, piper, whisper, whisper-cpp, aws-tunnels, ha-mcp / vscode-server / esphome / cloudbeaver / kubernetes-mcp-server (Sablier apps) | nobody needs these in an outage; cuts load and silver's replica churn |
| `stage1` (ordered) | smb-operator → smb-server → smb-announcer | operator first so it doesn't recreate the server; file shares may have open clients |
| `stage2` | Vaultwarden (+ its MySQL), Vault, n8n (if the orchestrator is not n8n) | stopped when stage 2 starts; Vault must be unsealed again before the ArgoCD unfreeze in recovery. |
| after `final` | `smarthome-db-pg` hibernation | explicit step, not a label: runs only after HA (its client) is gone, so the DB shuts down cleanly |
| `final` | **Home Assistant, Zigbee2MQTT, Mosquitto, Matter server, Thread border router** (they stay up as long as DNS: Z2M → MQTT → HA is one chain, Matter/Thread devices keep working in an outage), then Prometheus, Alertmanager, Grafana, Loki (+ caches) | kept to the last moment, stopped together with a short grace (60–120 s), **then** the DB is hibernated. Not plain `keep`: if the node's graceful shutdown killed HA and the DB in parallel, the DB could be mid smart-shutdown when the grace period ends (crash-safe, but not clean). |
| `keep` | NUT (+ PeaNUT), Technitium primary/secondary, Longhorn (+ CSI), CNPG operator, orchestrator, ArgoCD controller until frozen | stopped by the node's graceful shutdown. DNS is last on purpose: the orchestrator and the NAS SSH calls need name resolution |

Open question for you: anything above that you'd actually want alive in an outage (voice satellites on
piper/whisper? Vaultwarden for passwords?). Everything on the `stage1` list is stopped within ~2 min of battery.

Recovery gotcha from the Matter notes: the Thread border router's SRP registry is RAM-only, so a restart makes Thread
devices re-register. Start order on recovery: Matter server first, border router last, then expect up to ~15 min of
churn (SRP lease is clamped to 15 min); don't treat that as a failure.

Tunables live in one ConfigMap `ups-automation-config` (git-managed): `stage1DelaySec`, `stage2RuntimeSec`,
`stage2ChargePct`, `maxOnBatterySec`, `recoverStableSec`, PoE port list, per-stage enable flags.

## 6. Stage sequence

Preconditions checked every step: still `OB`? (If `OL` is stable for `recoverStableSec` **before** the point of no
return, undo cleanly. After stage 2 starts it always runs to the UPS cycle and then recovers.)

| Stage | Trigger (all gated on `OB`) | Actions, in order |
|---|---|---|
| **armed** | `OB` seen | write `ups-state=ARMED`, best-effort notify. No cluster changes. |
| **1 — shed** | `OB` ≥ `stage1DelaySec` (~90 s) | freeze ArgoCD + scale Sablier to 0 → scale `shed=stage1,needs=nas` → wait gone → scale other `stage1` → hibernate `service-db-pg` → PoE-off list (§4, via HA) → gate Longhorn replica health → SSH `poweroff` the VM worker → wait down → `poweroff` the NAS |
| **2 — full** | `runtime < stage2RuntimeSec` **or** `charge < stage2ChargePct` **or** on-battery > `maxOnBatterySec` | (do stage 1 if not yet done) → scale `stage2` → stop `final` (HA, Z2M, Mosquitto, Matter/Thread, monitoring; grace 60–120 s) → wait gone/detached → hibernate `smarthome-db-pg` → UPS cycle command with a ≥ 120 s delay → scheduled `poweroff` (`systemd-run --on-active=30`) on all remaining nodes in parallel → final notify |
| **panic** | native `LB` | straight into the stage-2 actions, no gates |

Time budget (**measure, don't guess**): scale-down + detach X min, CNPG hibernate Y min, kubelet grace Z min, NAS ~2 min.
Set the thresholds from the measured total, with ≥ 2× margin. Starting values (tunables, to be confirmed by the discharge test):

| Threshold | Value | Why |
|---|---|---|
| stage 2 (orchestrator) | `charge < 30 %` **or** `runtime < 480 s` | charge is load-independent and is the primary trigger: 30 % of ~40 min ≈ 12 min left at full load, ≈ 17 min after stage 1 shed the load. The runtime guard (not 5 min: the estimate sags on an old battery) only matters if the battery is weaker than assumed. |
| native `LB` (Layer A, last resort) | `charge < 15 %` / `runtime < 300 s` | must still fit kubelet grace (~60 s) + poweroff, and DSM's own safe-mode chain (~2–3 min); only acts if the orchestrator is broken. |
| expected stage-2 duration | ~3–4 min | stage 1 already stopped the NAS, the VM and half the apps. Remaining: stop core apps in parallel (≤ 60 s) + detach (~1 min) + CNPG hibernate (seconds once clients are gone; the 180 s `smartShutdownTimeout` and 1800 s `stopDelay` are ceilings, not typical) + kubelet grace + poweroff. |

## 7. Recovery

Mains returns → UPS cycles outlets (§2 row 3) → minis power on (BIOS), NAS powers on (DSM *Power Recovery*), the
VM worker autostarts (**VMM autostart must be switched on**, it is off today) → k3s restores pods from its own state → `ups-recover` (CronJob
dead-man, or the orchestrator) sees `ups-state != NORMAL` and `OL` stable:

1. Wait nodes `Ready`; uncordon any cordoned node.
2. Restore PoE ports.
3. Wake CNPG (remove hibernation annotation), wait healthy.
4. Wait for Vault to be unsealed, scale Sablier back up, then unfreeze ArgoCD; let selfHeal restore replicas. After ~5 min anything still at 0 with an
   `original-replicas` annotation is scaled from it (fallback only).
5. Verify (no Pending/CrashLoop, Longhorn attached/healthy, NAS shares mounted), clear `ups-state`, notify with downtime.

NAS must be reachable before ArgoCD is unfrozen, or NAS-mounted workloads start against a missing share.

**ArgoCD is the primary restorer, not a dependency-free one.** Freezing it first (so nothing reverts the scale-down) and
unfreezing it at the end (so selfHeal restores every replica from git) is the intended path. What it does **not** do, and
the recovery step must:

- remove the CNPG hibernation annotation (live-only annotations aren't in git, so ArgoCD never clears them);
- uncordon nodes, restore PoE ports, start the VM and the NAS;
- restore Sablier-managed apps: their replicas are ignored by design, so they stay at 0 until woken (that is the desired
  idle state);
- enforce order across apps: unfreezing flips ~40 apps at once (silver has only ~6 GB, and ArgoCD's reconcile concurrency
  is already throttled for that reason). Hence the prerequisites in the list above *before* the unfreeze.

ArgoCD's repo-server needs **GitHub reachable and Vault unsealed** to render (lovely plugin) and its cache is not
persistent, so after a cold boot with the internet still down it cannot restore anything. That is why the
`original-replicas` annotation fallback stays: after ~5 min without convergence the recovery scales labelled workloads
back up itself, in reverse tier order (`stage2` first, then `stage1`). Also expect ArgoCD to roll out any commits merged
during the outage (e.g. Renovate bumps) once unfrozen; harmless, but one more reason to unfreeze only when everything is
healthy.

## 7b. Phase 0 — self-triggered live dry-run by an agent (before building anything)

Cheaper than writing the orchestrator only to find the gains are small or the plan has holes: run stage 1 and stage 2
**with mains still connected**, driven by an agent that follows a fixed runbook, timestamps every step
and watches the meters. It costs some planned downtime but validates the claims and will probably expose bugs in this
plan.

**What it can and cannot prove.** On mains the UPS never discharges, so this validates *watts saved*, *durations*,
*ordering*, *what breaks* and *recovery*. It does **not** validate real battery runtime or the `LB`/charge thresholds
(`battery.runtime` is only a load-based estimate); that is the discharge test in §8.

### How the agent runs it (guardrails)

- **Model split.** A **Sonnet** agent executes the runbook (the cost of a stronger model is small next to the cost of a
  mis-judged shutdown, and "is this state unexpected?" is exactly where a small model is weakest). A **Haiku** agent is
  fine only for the passive part: logging the meters every minute. The main session (Opus level) is the escalation point at
  every gate and on every deviation, and judges pass/fail for test 2. At most 2 agents at a time (runner + logger).
- **Fixed runbook, no improvisation.** The runbook is this section turned into an ordered command list; the runner executes
  and logs, it does not design. Any unexpected state (pod stuck `Terminating`, volume degraded, command error) means
  **stop and report**, not retry or fix.
- **Human gates** (agent pauses and asks): before powering off the NAS, before the UPS cycle command, before the
  ArgoCD unfreeze, and before test 2 at all. Repo rule stays: destructive cluster actions need an explicit OK each time.
- **Deterministic actions.** The destructive steps are plain `kubectl`/`ssh` lines from the runbook; the runner's own
  judgement is limited to reading the measurements against the table below and stopping on any deviation.
- **One log file** (`tmp/`, not committed): `timestamp | step | result | plug W | ups.load | battery.runtime`.
- Rollback at any point = the recovery order in §7; physical power buttons as the last resort.

### Settling time and measure points

Wait **5 min after every power-affecting step** before reading the meters, so each rung gets a clean delta. The Tuya
plug is averaged over 5 min and `battery.runtime` is smoothed; `ups.load` only has 1 % (= 9 W) resolution, so use the
plug as the primary reading and `ups.load` / runtime only as a cross-check. Read the **last 2–3 min** of each settle
window, not the first. Operational steps (freeze ArgoCD, write annotations, wait for pods gone / volumes detached) need no
extra wait beyond their own condition.

| Measure point (after settling 5 min) | Test |
|---|---|
| baseline (≥ 30 min before starting) | 1 |
| stage 1 apps scaled down + `service-db-pg` hibernated | 1 |
| PoE ports off | 1 |
| VM worker powered off | 1 |
| NAS powered off | 1 |
| `stage2` apps down | 2 |
| `final` stopped (HA, Z2M, Mosquitto, Matter/Thread, monitoring) + `smarthome-db-pg` hibernated | 2 |
| all nodes off, just before the UPS cycle | 2 |

### Before you start (gates)

- [ ] Quiet window, **not** 02:00–05:00 (Plex Butler / Hyper Backup); household warned (DNS and Home Assistant go down in test 2).
- [ ] Laptop has a non-cluster DNS fallback, so GitHub/docs stay reachable while Technitium is down.
- [ ] Longhorn all volumes healthy; recent CNPG backups verified; Vault unseal keys at hand (manual Secret).
- [ ] Physical access to the rack in case an auto-power-on assumption fails.
- [ ] Enable VMM autostart on the NAS VM; check BIOS AC-restore on both minis and the DSM Power Recovery toggle (§9).
- [ ] Enable the PoE switch entities in HA's UniFi integration (§4), or skip the PoE step and test it separately.
- [ ] Measurement running with ≥ 30 min baseline: plug power (Tuya plug, 1 W), NUT `ups.load` / `battery.runtime` /
      `battery.charge` (Prometheus is fine, mains is up), per-node CPU, and **temperature near the UPS**: NUT `battery.temperature` if this UPS reports it (not seen so far), otherwise any sensor you already own, e.g. a Zigbee2MQTT or Thread temperature sensor temporarily placed next to the battery compartment, plus the rack switch's own temperature already in HA as a free proxy for rack heat. Heat is what ages lead-acid, and a discharge test should show how much the battery warms.

### Test 1 — stage 1 (≈ 1.5 h incl. settling and recovery)

1. Freeze ArgoCD (scale the controller + applicationset-controller to 0), scale Sablier to 0.
2. Scale the `stage1 + NAS` apps to 0, write `original-replicas`, wait until pods are gone **and** volumes detached.
3. Scale the other `stage1` apps to 0, hibernate `service-db-pg`.
4. PoE: switch off the technical-room AP and the living-room switch.
5. Check Longhorn: volumes with a replica on the VM, replica states once the VM is down.
6. *Gate*, then SSH `poweroff` the VM worker, wait until off, then `poweroff` the NAS.
7. Hold ~10 min and read the meters. Then recover in the order of §7 (NAS first, VM, nodes Ready, wake CNPG, unfreeze).

### Test 2 — stage 2 + the UPS cycle (≈ 2 h incl. settling and recovery, only after test 1 passed and you said go)

1. Test 1, then scale `stage2`, stop `final` with a 60–120 s grace, then hibernate `smarthome-db-pg`.
2. *Gate*, then the UPS cycle command (candidates in §2 row 3; the 40 W bulb is the test load for the first dry attempt),
   then the scheduled `poweroff` on all nodes. The UPS drops and restores the outlets with mains present, which tests the
   whole recovery chain: BIOS AC-restore, NAS Power Recovery, VMM autostart, k3s cold boot, Vault unseal, ArgoCD unfreeze.
3. Record every duration and everything that did not come back by itself.

### Claims to validate

| Claim in this plan | How measured | Pass if |
|---|---|---|
| NAS (+ VM) shed saves ≈ 28 W | plug watts, before/after | −24…−32 W |
| PoE rung saves ≈ 6 W | plug watts after the two port-offs | −4…−8 W |
| Cluster down saves ≈ 23 W | plug watts, all nodes off | −19…−27 W |
| Runtime estimate rises ≈ 1.4× after stage 1 | `battery.runtime` before/after | ×1.2…×1.5 (estimate only) |
| Stage 1 takes ≈ 5–8 min | step timestamps | within 2× |
| Stage 2 takes ≈ 3–4 min | step timestamps | ≤ 6 min (feeds §6 thresholds) |
| CNPG hibernation takes seconds once clients are gone | timestamp | < 60 s each |
| NAS-mounting pods leave cleanly before the NAS stops | pods stuck `Terminating` | none |
| Longhorn copes with the VM going away | replica/robustness states, rebuild time | no faulted volume, rebuild < 10 min |
| One ArgoCD unfreeze restores everything | Pending/OOM on the 6 GB node, time to healthy | no OOM, healthy < 15 min |
| The UPS cycle restarts all outlets with mains present | outlets drop and return | yes (else try another command) |
| BIOS AC-restore, DSM Power Recovery, VMM autostart work | what boots by itself | all three |
| Matter/Thread churn after recovery is tolerable | HA entity availability | back within ~15 min |

### Output

Per-step durations, anything that needed a manual push, surprises (stuck mounts, DNS gaps, Vault, HA entities not
returning) and the measured watts, all written into this page. Then decide: build the full orchestrator, build only
Layer A + the NAS/PoE shed, or stop at the safety net if the gains are not worth it.

## 8. Test plan (nothing here touches prod until the previous step passes)

1. **Bench facts**: (a) set `override.battery.*.low`, confirm `LB` flips at the new values (safe while on mains: upsmon only acts on LB when on battery); (b) with the 40 W bulb as the only load and mains present, try `shutdown.reboot` / `load.off.delay` and confirm the outlets drop and come back; (c) confirm DSM *Power Recovery* (UI) and BIOS "power on after AC loss" on both minis.
2. **Dry-run orchestrator** with a mock UPS status and the actions pointed at the `testing` namespace + a label-selected
   scratch app. Abort path included.
3. **Layer A alone:** disable the orchestrator, pull mains (or simulate FSD), confirm nodes + NAS shut down cleanly and
   the UPS cycles. Check Longhorn/CNPG are clean on boot (no rebuild storms, no recovery-mode Postgres).
4. **Real discharge test** at a time you're home: measure actual runtime vs `battery.runtime` and the time budget
   in §6. Log battery temperature too (see §7b). Tune thresholds from that, not from the estimate.
5. **Full run** end to end, including recovery, with `maxOnBatterySec` short.

## 9. Decisions still open

- [ ] **Next step: Phase 0 dry-run by an agent (§7b) before any implementation.** Its result decides how much gets built.
- [ ] Orchestrator: n8n workflow vs Jobs/CronJobs in `nut`.
- [x] **End state after the cluster is down:** decided 2026-11-07, cycle the UPS right after the nodes are off
      (UPS cycle command, §2 row 3), leaving battery in reserve. No "leave network up" phase: without cluster DNS and services it
      is useless, and it would need a Wake-on-LAN step in recovery. (Protect cameras keep recording to the gateway disk
      until the cut; accepted.)
- [ ] PoE 1b (§4): enable the PoE switch entities in HA's UniFi integration, add the HA shed script + the restore-on-start/OL automation.
- [ ] Confirm the app-by-app tiers (§5 proposal); in particular piper/whisper (voice) and Vaultwarden: alive in an outage or not.
- [ ] Do we want stage 1 to fire at all for short outages (`stage1DelaySec` 90 s vs 3–5 min)? The NAS cold-boot cost is
      ~minutes of RAID/DSM startup for ~10 min of runtime gained.
- [ ] Replace the battery (RBC176) *before* trusting any threshold; see [rack power audit](../01-platform/power-nut.md).
- [x] Monitoring (Prometheus, Alertmanager, Grafana, Loki) stays up to the end (`final` tier): decided 2026-11-07.
- [ ] **Manual checks only you can do:** BIOS AC-restore on both minis; DSM Power Recovery toggle; enable VMM autostart; did the NAS restart on its own after the 2026-07-20 safe shutdown?
- [ ] Rotate the NUT `admin` password at some point: it was printed once in an audit transcript (not in git).

## 10. Repo changes (when built)

| Path | Change |
|---|---|
| `lib/shared-lib` | new `power:` values key → label + validation |
| `scripts/` + pre-commit/CI | lint: `workload/` apps with persistence, DB or NAS mount must declare `power:` (modelled on `gen-scale-to-zero-ignores.sh`) |
| `system/nut/automation/` (from `disabled-automation/`) | RBAC ServiceAccount (nodes get/list/patch, deployments/statefulsets patch + scale, CNPG patch, ArgoCD controller scale, jobs), `ups-automation-config` + `ups-state` ConfigMaps, recovery CronJob |
| `system/nut/nut/` | `ups.conf` `override.battery.runtime.low` / `charge.low`; `NOTIFYCMD` on FSD → UPS cycle command (§2 row 3); do not touch `allow_killpower` until tested |
| `ansible/roles/node_common/` | `nut-client`/upsmon (`monuser`), logind drop-in `InhibitDelayMaxSec`, SSH key (sudo is already NOPASSWD) |
| `ansible/roles/k3s_pre/` | kubelet `shutdown-grace-period` / `-critical-pods` args (see [k3s tuning](k3s-tuning.md) §2); needs the logind drop-in above |
| `workload/smarthome/mosquitto` | Longhorn replicas 2 → 3 |
| `wiki/01-platform/power-nut.md` | replace "not built yet" with the final design |
