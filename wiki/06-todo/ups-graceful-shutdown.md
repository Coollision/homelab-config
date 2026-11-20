# TODO — UPS: staged load shedding, graceful shutdown and auto-recovery

[← Back to Home](../Home.md) · **[One-page plan](ups-shutdown-overview.md)** · Related: [Power / NUT](../01-platform/power-nut.md) · [k3s tuning](k3s-tuning.md) (kubelet graceful shutdown) · [Storage](../01-platform/storage.md) · [Databases](../01-platform/databases.md)

> **Status (2026-11-18): current plan = "Layer A only"** ([§0](#0-current-plan-layer-a-only-2026-11-18)), built against the
> existing APC. The full design below (Layer B orchestrator, shed groups, PoE, recovery phases; §1–§10) is **paused** until
> the UPS is replaced: the battery measured past end-of-life (~18 min real runtime, §2d R11) and no NUT-compatible
> replacement is buyable yet. The §2c/§2d decisions stay valid as input for that redesign. 2026-11-20: manual clean
> shutdown by the owner. Hard requirement unchanged: **a power outage must never cost data.** Replaces the scratch plan
> and the sketch in `system/nut/disabled-automation/`. Delete this page once implemented and move the result into
> [Power / NUT](../01-platform/power-nut.md).

## 0. Current plan: Layer A only (2026-11-18)

Goal: when the UPS has been on battery long enough that it might give up, every host shuts down cleanly by itself. No
orchestrator, nothing changed in git or the cluster, so recovery is simply "power returns, hosts boot, everything starts
as usual".

**Trigger (NUT primary in the `nut` pod):**

- `upssched`: **8 min on battery → FSD** (`upsmon -c fsd`). A plain timer, because the UPS's charge % and runtime are
  badly optimistic on this battery (knee after ~13–15 min, empty at ~18 min; §2d R11). Mains back before 8 min cancels
  the timer. Verify the image ships `upssched`.
- Backstop: the UPS's own LB flag (leave `battery.*.low` as is; the timer is the real trigger).
- `DEADTIME 120` in a mounted `upsmon.conf`: the ~20 s driver wedge at every switch to battery (seen in the 2026-11-17
  tests) and the hourly stall never cause FSD. `SHUTDOWNCMD` stays a no-op (it would only stop the pod).

**Hosts:**

| Host | Mechanism |
|---|---|
| 4 k3s nodes | `nut-client` / `upsmon` secondary (`monuser`, `DEADTIME 120`) → on FSD a normal `poweroff` (ansible `node_common`) |
| kubelet (all nodes) | graceful node shutdown by **existing** pod priority, no app labels needed (table below); logind `InhibitDelayMaxSec` ≥ 210 s; post-check `systemd-inhibit --list` shows the kubelet delay lock |
| NAS | already a NUT secondary of the cluster's UPS server: re-enable UPS support in DSM (it was off for the tests); FSD → DSM safe mode (~2–3 min) |
| CNPG (`smarthome-db-pg`, `service-db-pg`) | `smartShutdownTimeout: 30` in git (R2), so Postgres stops cleanly inside step 1 |

| Step | Pods (by live priority) | Waits up to |
|---|---|---|
| 1 | priority 0: all apps **and** the databases | 150 s |
| 2 | ≥ 1 000 000 000: Longhorn (`longhorn-critical`) and `system-*-critical` | 60 s |

Storage outlives the databases on the same node; across nodes the order is best effort (accepted, §2d R1). Kubelet
config via the k3s kubelet config drop-in or `kubelet-arg` (`shutdownGracePeriodByPodPriority`); work out what k3s
v1.36 supports first ([k3s tuning](k3s-tuning.md) §2) and re-check `KillMode`.

**Timing:** FSD at 8 min on battery → nodes off after ~3.5–4 min (NAS ~3 min in parallel) → all off at ~12 min, ~5 min
before the measured empty point. The network gear keeps running until the UPS is flat (accepted, R6).

**Remaining gap (accepted):** mains returns after the hosts are off but before the UPS runs flat → AC never drops, so
BIOS "always on" does not trigger and the hosts stay off; power them on by hand (buttons, or WoL for the minis and NAS).
Closing it needs the UPS power-cycle command (Test 0 b/c, deferred).

**Not needed for this plan:** Layer B, shed groups and the `ups` → `apc` rename, PoE, WoL orchestration, ArgoCD freeze,
`original-replicas`, the 4-tier priority classes.

**Build and test (each drain/restart/poweroff needs the owner's OK at that moment):**

1. NUT pod: `upssched` timer + mounted `upsmon.conf` (`DEADTIME 120`), CNPG `smartShutdownTimeout: 30`; validate charts.
2. Ansible: NUT client, logind drop-in, kubelet shutdown config; apply **one node at a time** with the existing drain
   pattern.
3. Test on one drained node: simulated FSD with mains on → node powers off, pods stopped in the two steps (logs).
4. Full test with the smart plug feeding the UPS (`switch.bureau_plug_servers`, as in the 2026-11-17 discharge test): pull
   mains, watch FSD at 8 min, every host down cleanly, then restore mains and power on; check Longhorn and CNPG are clean.
5. Move the result into [Power / NUT](../01-platform/power-nut.md).

### UPS replacement status

- The APC's battery (APCRBC176, 24 V 7.0 Ah) is at ~35–50 % of new capacity (validated against the CSB datasheet, §2d
  R11). The BX manual says it is not user-replaceable (Schneider: live parts; DIY is possible with precautions). The
  owner decided **not** to buy a bridge (new blocks or cartridge).
- **Waiting for the EcoFlow River 520 Gen4** (512 Wh, 500 W, ~139 × 177 × 229 mm, ≈ 3 h here, EU expected Q1 2027): buy
  only once a review confirms a USB data port with NUT support, and that 229 mm is not its height (rack slot: ≤ 190 mm
  high, ≤ 390 mm deep, may be a bit wider than 140 mm). The River 3 Plus (NUT-proven, fits) is being phased out and is
  no longer buyable near €299 in Belgium.
- On a swap: NFD USB-ID rule for the `nut` pod, `ups.conf` (drop the APC workarounds), the 8 min timer (rerun the
  smart-plug discharge test), and the exporter `instance` label.

## 1. What the shedding actually buys (read this first)

Baseline from the 2026-10 audit and live NUT: ~155 W at the wall, UPS output ~108–124 W, `battery.runtime` ≈ 2500 s
(~42 min) at ~12 % load. The UPS's own ~31 W is not sheddable.

Source: [rack power audit 2026-10-14](../01-platform/power-nut.md) (155 W at the wall, UPS output ≈ 124 W, UPS self ≈ 31 W).

| Rung | Cuts | ≈ W saved | UPS-output load | Runtime (≈) |
|---|---|---|---|---|
| mains | — | — | ~124 W | ~42 min |
| 1: NAS (the VM worker is *inside* its 28.5 W) | NAS | −28 | ~96 W | ~55 min |
| 1b: PoE ports | technical-room AP + living-room switch (§4) | −6 (≈ −7 with PSU loss) | ~89 W | ~59 min |
| 2: cluster down (2 minis + Pi ≈ 23 W) | workloads → DBs → nodes | −23 | ~66 W | clean off, then the UPS is cycled |
| floor (only until the cluster stops) | gateway + Protect HDD 22, switch 16, kept PoE devices ≈ 17, ISP box 9 | — | ~64 W | — |

Honest ceiling: with the whole ladder the load drops to ~64–66 W, so at best ~1.9× the baseline; shedding is worth
~+17 min. More importantly **the network has no value once the cluster is down** (DNS, Home Assistant and the rest live
there), so there is no "keep the network and cameras alive on the leftover battery" phase: the cluster is the thing
worth keeping up, and it stops with battery to spare. The design target is **a clean shutdown with margin**, and the
UPS is cycled straight after, not run flat (also kinder to an ageing lead-acid battery). Everything below is ordered by
data safety first. The UPS's own 31 W is paid in every rung. All figures are wall-side, measured on mains: treat them as
order of magnitude until the real discharge test (§8), because the UPS self-draw and inverter loss on battery scale differently.

## 2. Review of the scratch plan — what had to change

| # | Problem in the scratch plan | Fix in this plan |
|---|---|---|
| 1 | n8n is the only executor, but it lives on the UPS node, depends on its own CNPG DB + Longhorn + k3s. If the cluster is already degraded, **nothing shuts the NAS down.** | Two layers (§3): a dumb cluster-independent safety net (NUT clients + kubelet graceful shutdown) under the smart orchestrator. |
| 2 | "Drain `<ups-node>` and `<control-plane-node>`" then n8n SSH-poweroffs. Drain evicts n8n itself mid-run. | No end-of-run drain. Scale down by tier, wait for volumes to detach, then rely on kubelet graceful shutdown + scheduled poweroff. n8n/orchestrator pod is the last tier-0 workload. |
| 3 | `load.off` only as a backstop. If mains returns *during* the shutdown the nodes stay off (BIOS "power on after AC loss" never triggers because AC never dropped). | The UPS must be power-cycled **after** the hosts are off. The live driver offers `shutdown.reboot`, `shutdown.default`, `load.off.delay`, `shutdown.stop` (**no** `shutdown.return`/`stayoff`). Delay ≈ 480 s (measured worst case, §2c #1). In the NUT source `shutdown.reboot` has a **fixed 10 s** delay, so `load.off.delay <s>` is the candidate; whether either restarts the outlets with mains **absent** is **Test 0** (§8.1). |
| 4 | `LB` flag fires with only 120 s left (`battery.runtime.low=120`, `battery.charge.low=10`). | Both are **writable** on this UPS: raise them declaratively via `override.battery.runtime.low` / `override.battery.charge.low` in `ups.conf` **plus `ignorelb`** (overrides alone do not move the firmware LB flag, §2c #6), starting at ≈ 300 s / 15 % and raised after Test 1 (§2c issue A). NOTE the NAS is already a NUT secondary and shuts itself down on `LB`/FSD (§2b), so raising `LB` also moves the DSM-native shutdown earlier. Intended. |
| 5 | 5-min Slack warning + Wait burns ~12 % of the battery at full load and blocks on **internet reachability** (street cabinet may be dead too). | No blocking wait. Notifications are best-effort (`continueOnFail`). "Abort" = automatic: re-check `OL` before every irreversible step. |
| 6 | State kept in an n8n Data Table (Postgres on Longhorn) — not readable until the thing being shut down is back. | State lives in a ConfigMap (`ups-state`, ns `nut`) + per-object annotation (§5). Survives n8n loss; readable by anything with kubectl. |
| 7 | UniFi writes (PoE off) would use the cloud proxy, which dies with the internet. | Go through Home Assistant, which already has local (non-cloud) UniFi access (§4). |
| 8 | Only `arr-stack` hard-coded as the scale-down target. | Label-driven tiers (§5): git is the source of truth, nothing hard-coded in the workflow. |
| 9 | CNPG shutdown not addressed. Data-loss hotspot (HA recorder, service DBs, n8n). | Explicit DB step: CNPG hibernation annotation (clean checkpoint) after dependants are down, before nodes stop. |
| 10 | Longhorn replicas on the VM worker: powering it off degrades volumes. | Gate = every attached volume keeps ≥ 1 healthy replica elsewhere. HA, Z2M and the DB are 3-replica volumes (2 left, fine). Mosquitto is 2-replica (blue + VM): since it now stays up through the outage, **raise it to 3 replicas** (no longer optional). Loki / Prometheus end up single-replica for the outage; accepted. |
| 11 | Mount hazards: NAS-backed pods stuck `Terminating` when the NAS disappears (hard mounts). | NAS-dependent workloads are a tier that is scaled down and **confirmed gone** *before* the NAS is powered off. |
| 12 | Plan says "all three nodes" for BIOS/AC-restore. | Four k3s nodes (3 minis/VM + the Pi). Check BIOS on the two minis; the Pi powers on by default; the VM needs **VMM autostart** (currently *disabled*, §2b). |
| 13 | Recovery depends on n8n finishing. If it doesn't, apps stay at 0 replicas with ArgoCD frozen — silently. | The orchestrator itself is a CronJob state machine (§3) that resumes from `ups-state`, so there is no separate dead-man. Alert if still frozen after 15 min. |
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
| Kubelet | `shutdownGracePeriod` = `shutdownGracePeriodCriticalPods` = **0 s on all 4 nodes**; logind `InhibitDelayMaxSec` = 5 s on all 4 (Pi checked 2026-11-07); k3s unit `KillMode`: read as `control-group` on the minis/VM on 2026-11-07, but [k3s tuning](k3s-tuning.md) records the template fix to `process` everywhere; re-check live (§2c #18) | Today a plain `poweroff` kills pods. Layer A needs both: kubelet args (via `k3s_pre` config) **and** a logind drop-in `InhibitDelayMaxSec` ≥ grace period. |
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

## 2c. Adversarial review: decisions (2026-11-17)

An Opus review of this page found 23 issues (5 critical). Each was discussed and decided; **where this section
conflicts with the rest of the page, this section wins** (the body is updated as the design is built).

| # | Finding | Decision |
|---|---|---|
| 1 | UPS cycle delay (≥ 120 s) is shorter than a worst-case host shutdown; outlets could drop mid-shutdown | One fixed command, delay ≈ **480 s** (worst-case tier grace 360 s + poweroff scheduling + OS), re-derived from measurements in the dry-run. In Layer A the command is sent only after secondaries have finished or a fixed wait, not at FSD time. |
| 2 | `shutdown.reboot` may restart outlets on battery | Command choice is **validated against the NUT docs/source first** (see "Command semantics" below), then bench-tested with mains absent. One fixed command, no per-state logic. |
| 3 | A routine driver stall (~1/h, 15–40 s stale) makes upsmon see comm loss while on battery and set FSD | `DEADTIME` **120 s** on the primary (mounted `upsmon.conf`), on node clients, and verified on DSM. Layer B detects FSD and stands down. |
| 4 | Layer A depends on the `nut` pod (k3s, MetalLB) | **Accepted risk.** The pod keeps following the UPS via NFD; no host-level NUT. A broken k3s on the UPS node during an outage means no coordinated shutdown. |
| 5 | "Plain poweroff is safe" is unproven; Longhorn has no priorityClass | Four-tier kubelet shutdown (below), tested on one drained node first. |
| 6 | `override.battery.*.low` does not move the firmware LB flag | `ignorelb` **plus** the overrides; proven in bench test 8.1a. |
| 7 | Cycle command can be lost during a driver stall/restart | `upscmd -w`, read back the UPS countdown timer, **no node poweroff without a confirmed countdown**; retry after a driver restart; watchdog paused during the command window. |
| 8 | Power-on after SSH poweroff / mains back before the cycle is unproven | NAS stays **undoable**: Wake-on-LAN + VM start (DSM CLI over SSH) in the undo path and recovery. Both tested in dry-run Test 1. |
| 9 | Several actors (orchestrator, dead-man, Layer A) can overlap | **One CronJob state machine** (`concurrencyPolicy: Forbid`) driven by `ups-state`; no separate dead-man, no lease. Reaction latency up to ~1 min; Layer A covers anything faster. |
| 10 | Recovery on a half-empty battery | Hold the heavy steps (wake DBs, unfreeze ArgoCD, start NAS-dependent apps) until **charge ≥ 50 % and OL stable 10 min**. Triggers stay armed from boot. |
| 11 | Orchestrator on the VM dies mid-run; n8n's DB is hibernated under it | Orchestrator = the CronJob in `nut`, node affinity away from the VM, no DB, state in the `ups-state` ConfigMap. n8n is notifications only. Must carry `power.homelab/shed=keep`. |
| 12 | `original-replicas` can be overwritten with 0; ArgoCD/Sablier outside the mechanism | **Write-once** (only if absent and > 0), covers ArgoCD controllers and Sablier, cleared when recovery completes. |
| 13 | Dry-run gaps (agent dies mid-test, internet drops at the cycle, DNS) | Agent runs both tests **with safeguards**: dead-man alert, phone hotspot, kubeconfig by IP, plug read locally, runbook says "poll until the API answers" after the cycle. The UPS bench test becomes **Test 0** and runs first. |
| 14 | Powering off the VM relocates pods and starts Longhorn rebuilds | Follow the existing ansible pattern (Longhorn-healthy gate, cordon, `drain --ignore-daemonsets --delete-emptydir-data --disable-eviction`, CNPG PVC reuse). **No** Longhorn scheduling pause; a rebuild after 300 s is accepted and measured in Test 1. |
| 15 | Other controllers keep acting while ArgoCD is frozen | **Label-driven freeze**: `power.homelab/shed=freeze` (Deployments/StatefulSets scaled to 0, CronJobs `suspend: true`), covering ArgoCD, Sablier, Keel, descheduler, Reloader. Verify the Longhorn `allow-recurring-job-while-volume-detached` setting once. |
| 16 | Label contract and CI guard miss charts outside shared-lib, `system/`, inline NAS volumes | Guard runs on **rendered manifests across `system/` and `workload/`**; NAS detection includes inline `nfs`/CIFS volumes; non-mount NAS dependencies (DNS container, image mirror, Longhorn backup target) documented. Non-shared-lib charts get the label via their own values or a kustomize patch. |
| 17 | Watt arithmetic in §1 is off (89 − 23 = 66, floor adds to 64, "3 minis" is 2 + Pi) | Correct the table; mark §1 order-of-magnitude until the discharge test (the 31 W UPS self-draw was measured on mains). |
| 18 | `KillMode` facts contradict [k3s tuning](k3s-tuning.md); kubelet inhibitor lock needs `InhibitDelayMaxSec` ≥ grace *at kubelet start* | Re-check `KillMode`; ansible post-check that `systemd-inhibit --list` shows the kubelet delay lock with the right duration on all four nodes. |
| 19 | One-off LB blips would trigger the no-gates panic path | **Superseded by issue B below:** there is no Layer B panic path; low battery is Layer A's job (the kubelet tiers keep the order). `ignorelb` computes LB from charge/runtime, proven in Test 0. |
| 20 | Stale data during a stall has no defined meaning | Tri-state `OL` / `OB` / `STALE`. STALE never counts as OL (no undo, no reset of "OL stable"); the on-battery clock runs from a persisted first-OB timestamp. **No escalation on staleness alone**; `maxOnBatterySec` and Layer A are the backstops. |
| 21 | DSM secondary behaviour on repeated stale/NOCOMM is unverified | Check DSM UPS settings and logs (incl. the 2026-06-02 / 2026-07-20 safe modes: real outage or comm loss?) before relying on them. |
| 22 | `nut` pod lifecycle: normal priority, `SHUTDOWNCMD` can end upsmon and re-send the cycle | Tier-4 priority; `SHUTDOWNCMD` a no-op; cycle script idempotent (skip if a countdown is already running). |
| 23 | PoE restore depends on HA, which has no NUT integration | Superseded 2026-11-17: shed **and** restore both go through Home Assistant (HA is recovery phase 1), no new UniFi credential. IPs, not DNS names, for controller and NAS SSH targets. |

### Kubelet shutdown tiers (finding 5)

Importance decides who goes first; dependency stops anything going down before what it depends on. Lowest priority
stops first. Grace values are ceilings (pods only wait as long as they need), but their sum sets the worst case that the
cycle delay must cover when Layer A has to do everything alone.

| Tier | Stops | Who | Grace |
|---|---|---|---|
| 1 disposable | first | arr stack, music-assistant, slskd, cloudbeaver, ha-mcp, esphome, vscode-server, jellyseerr, tautulli, babybuddy, aws-tunnels, Vaultwarden + its MySQL, `service-db-pg` (only the arr stack uses it), piper/whisper, other `stage1` apps; DBs of this tier get a priority just above their clients | 60 s |
| 2 core | second | Home Assistant, Zigbee2MQTT, Mosquitto, Matter/Thread, monitoring (Prometheus, Alertmanager, Grafana, Loki), n8n | 60 s |
| 3 databases | third | `smarthome-db-pg`, Vault | 180 s |
| 4 plumbing | last | Longhorn (manager, instance-manager, CSI), NUT, Technitium (DNS, kept for the SSH calls) | 60 s |

Worst case 60 + 60 + 180 + 60 = 360 s, hence the ≈ 480 s cycle delay. When Layer B ran first the real time is far shorter.
Layer B's label tiers map onto these (§5); the one-page plan has the same table.

### Command semantics (finding 2: from the NUT source, not yet bench-confirmed)

From `apc-hid.c` / `usbhid-ups.c` (NUT master): `shutdown.reboot` writes a **fixed 10 s** `DelayBeforeReboot` (so it cannot give
the long delay §2 row 3 assumed, unless the command accepts an argument: unconfirmed); `load.off.delay` takes its delay from
`ups.delay.shutdown` / an argument; `shutdown.return` is a generic alias that needs `load.on.delay`, which this UPS
does not offer; `upsdrv_shutdown` (the `upsdrvctl shutdown` / killpower path) tries
`shutdown.return, shutdown.reboot, load.off.delay, shutdown.stayoff` in that order, so on this UPS it would pick
`shutdown.reboot` with the 10 s delay, another reason to leave `allow_killpower` at 0. **Not documented anywhere I found:**
whether either command restarts the outlets while mains is *absent*. That is the Test 0 bench question, with `load.off.delay <s>`
the likely candidate for the long delay.

### Consolidation follow-ups (2026-11-17)

Turning the decisions above into one flow exposed seven inconsistencies, labelled A–G. Status:

| | Topic | Status |
|---|---|---|
| A | Layer A (low battery, 300 s / 15 %) fires too late for a worst case of 360 s tier grace + OS + NAS 2–3 min and a ≈ 480 s cycle delay | **Settled by tests.** Test 1 measures the real shutdown time; thresholds follow (proposal: Layer A ≈ 900 s / 35 %, stage 2 ≈ 1200 s / 50 %, only if the measurement says so) |
| B | "Layer B yields to FSD" overlaps a Layer B panic path; upsmon sets FSD on the first LB reading | **Agreed:** drop the Layer B panic path. Low battery *is* the panic and belongs to Layer A (the safety net) |
| C | Old `stage2` label (Vaultwarden, Vault, n8n) disagrees with the tiers | **Agreed:** `stage2` retired. `service-db-pg` (only the arr stack uses it) and Vaultwarden + MySQL are tier 1, n8n tier 2, Vault tier 3 |
| D | Home Assistant would wait for the 50 % recovery gate | **Agreed:** home control (`smarthome-db-pg`, Mosquitto, Z2M, HA, Matter, Thread) comes back first, as soon as mains is stable 2 min; the rest waits for 50 % + 10 min |
| E | No fallback if the cycle countdown is never confirmed | **Agreed, other form:** the UPS cycle stays. No countdown → nodes stay idle, Layer A shuts them down at low battery; no "power off anyway" branch |
| F | Layer A sends the cycle command at FSD, before the secondaries have shut down | **Adopted, applies when `cycleEnabled` is on** (the cycle is built behind `cycleEnabled: false`, off for 2026-11-20): wait until nodes and NAS have logged out of upsd (max ≈ 180 s), then send with the remaining measured delay |
| G | Stage 1 delay 90 s vs 3–5 min | **Agreed:** 3 min |

Also decided in this round: the **UPS cycle stays** (no "let the UPS run dry" variant), and an on-site **ESPHome Wake-on-LAN node** is
the backup wake path (see the [one-page plan](ups-shutdown-overview.md)); both minis' NICs report magic-packet WoL enabled (read-only
check 2026-11-17), BIOS-side WoL and AC-restore still need a live test. Layer B form: **reopened in §2d** (was "CronJob script first").

## 2d. Second adversarial review (2026-11-17/18): decided

A second pass over both pages, checked against the live cluster (k3s v1.36 on all four nodes; pod placement and priority classes read
with kubectl). Every row is decided (2026-11-17/18); where a row conflicts with §2c, the row wins. The current plan (§0) only uses R2, R5 (priorities as they exist), R6, R10 and R11; the rest applies to the paused full design.

### Data-safety gaps

| # | Finding | Evidence | Proposal |
|---|---|---|---|
| R1 | **Kubelet tiers only order pods on one node.** A priority group ends as soon as *its* pods are gone, so a node without tier 1–3 pods kills its Longhorn instance-manager (tier 4) within seconds of FSD, while a DB on another node is still checkpointing. Layer A's "tier order" does not hold across nodes, so T-C cannot pass as written. | `service-db-pg` runs on the Pi, which holds **no** Longhorn replicas: every replica of its volume is on another node. `smarthome-db-pg` and Vault share the control-plane mini. | **Decided 2026-11-17: accept.** Layer A is a *crash-safe best-effort* net, not a clean shutdown: tiers 1–2 empty within seconds on every node, so the race is seconds wide; Longhorn acks a write only after all replicas have it and Postgres recovers from WAL, so the realistic worst case is crash recovery, not lost data. The clean path is Layer B (DBs hibernated before any node goes down). Barrier DaemonSets were considered and rejected (no hard guarantee either, every maintenance reboot would take ~6 min). T-C pass criterion becomes "no lost data, all volumes back; Postgres crash recovery allowed". |
| R2 | Tier 3 grace (180 s) **equals** CNPG `smartShutdownTimeout` (180 s). If a client is still connected (R1, or a client on another node), the smart phase uses the whole window and the kubelet SIGKILLs Postgres before the fast shutdown and checkpoint. | §2b CNPG row; live: both clusters run the defaults `smartShutdownTimeout: 180`, `stopDelay: 1800` (not set in git) | **Decided 2026-11-17:** `smartShutdownTimeout: 30` in git on both CNPG clusters; tier 3 grace stays 180 s (30 s polite wait + fast shutdown/checkpoint fits easily). Side effect: restarts/drains cut lingering clients after 30 s instead of 3 min. |
| R3 | **BIOS "last state" trap.** With `cycleEnabled: false` (the 20th) the nodes are powered off *by software* while AC is still present. If AC-restore is "last state" (a common default), they stay off when mains returns after the UPS runs flat. The same question applies to DSM after the stage-1 SSH `poweroff` (Power Recovery may only cover *unexpected* loss). | §5 of the one-page plan, cycle off | **Decided 2026-11-17:** owner reports minis, NAS (DSM Power Recovery) and the rest are already set to "always on". Verified once with the real sequence (software poweroff → unplug → replug → boots) on one mini during T-B; the NAS is observed on the day, Wake-on-LAN stays its fallback (R8). |
| R4 | **VM inside the NAS under Layer A.** FSD reaches the NAS and the VM worker at the same time; DSM safe mode takes about 2–3 min while the VM's kubelet tiers (plus R1 barriers) take up to 360 s. VMM may force the guest off when DSM shuts down. | §2b NAS rows; VM is a NUT secondary only after WS1 | **Decided 2026-11-17: read VMM's guest-shutdown timeout only** (Test 0 e, Wednesday's DSM checks); no scheduling rule for now. Live 2026-11-17: on the VM run only stateless pods, Longhorn copies and one stateful app (BabyBuddy); no DB or Vault, by scheduler luck. Revisit (node affinity for DBs/Vault) if the timeout turns out to be short. |
| R5 | **Unlabelled is not harmless.** Pods without a priority class get priority 0 and stop **before** tier 1. That includes things tier 4 needs. | MetalLB speaker (carries the `nut` LoadBalancer IP the secondaries and issue F's logout check use) and Technitium both run with **no** priorityClass. CoreDNS and Multus are already system-critical. | **Decided 2026-11-17:** tier 4 = Longhorn (reuse the existing `longhorn-critical` class, no new one) + `nut` pod + MetalLB speaker + Technitium (kept so LAN DNS, and with it internet use, lasts as long as possible). Everything else stateless stays unlabelled. |
| R6 | **The gateway is hard-cut.** "A power outage must never cost data" does not hold for the gateway: UniFi OS and the Protect disk run until the UPS is flat (cycle off) or cuts (cycle on). | §1 floor row | **Decided 2026-11-17: accept for the automation**, written exception to the hard rule: network gear are crash-tolerant appliances (realistic loss: the last moments of footage, a disk check on boot). **On the 20th** the runbook adds a manual step: once the cluster is down and the battery is at ~20 %, shut the gateway down by hand from the UniFi UI. |

### Mechanism gaps

| # | Finding | Proposal |
|---|---|---|
| R7 | **How stage 2 powers the nodes off is unspecified** ("schedule poweroff on all nodes in 30 s" needs an SSH key to every node from a pod, or a privileged DaemonSet). | **Decided 2026-11-17: shed groups as virtual UPSes in NUT** (one general mechanism, no SSH, no Home Assistant). The real UPS is renamed `ups` → `apc` (the k3s nodes watch it); a `dummy-ups` repeater of `apc` named `ups` is **shed group 1** (the NAS, because DSM only monitors the name `ups`; later e.g. an AI box). Stage 1: drain the VM worker, then the Go binary raises **FSD on `ups`** over the upsd protocol → only group-1 hosts shut down (the NAS's VMM takes the drained VM down with it). End of stage 2: FSD on `apc` (and `ups`) → everything left, through the same path as Layer A. New hosts join a group via config, no code change. **Undo must reset NUT** (the FSD flag is sticky until restart) before Wake-on-LAN. Rename ripple: nut pod upsmon, PeaNUT, and the exporter: `params.ups` in `system/nut/nut/templates/servicemonitor.yaml` must become `[apc]` (otherwise it silently scrapes the virtual copy). No metric duplication: the exporter scrapes one named UPS only, and alerts, the Grafana UPS dashboard and Homepage select on the fixed `instance="apc-bx1600mi"` label, not the UPS name (checked 2026-11-18). On a UPS swap, keep that label for history or rename it everywhere in one go. Test 0 (Wednesday) proves: repeater mirrors OB + computed LB; FSD on `ups` reaches only its clients; what clears FSD; DSM reacts as before. Fallback if it fails: no early NAS shed, the NAS goes via normal Layer A. |
| R8 | **Wake-on-LAN from a pod does not work.** A magic packet is an L2 broadcast; from the pod network it never reaches the NAS's VLAN. | **Decided 2026-11-17: WoL goes through Home Assistant** (`wake_on_lan`): it is a recovery/undo action, HA is up in both cases (undo happens before tier 2 stops; recovery phase 1 starts HA first), and if HA does not come back something died and manual work is needed anyway. Principle: **no HA in the shutdown path; recovery and small extras may use it.** PoE shedding also stays on HA (#23), **owned by the orchestrator in both directions** (decided 2026-11-17): it calls an HA script "PoE off" in stage 1 and "PoE on" in the **undo** path and in recovery phase 1 once HA is healthy; HA only executes, it never decides "mains is back" (no NUT integration in HA, so no second state machine). The "on at HA start" automation stays as a free fallback for a dead orchestrator. Reason: after a *short* outage (undo before stage 2) HA never restarts, so the start automation alone would leave the ports off. Build check: HA reaches the NAS's L2 segment. Known limit: with everything off and mains back before the UPS ran flat, nothing sends WoL (deferred ESPHome node; on the 20th: power button). |
| R9 | The orchestrator would hold an SSH key for a NAS user with **full sudo**: compromise of the `nut` namespace = root on the NAS. | **Obsolete 2026-11-17:** R7's shed groups remove SSH from the design entirely (no NAS or node keys). |
| R10 | **Test 0 is not in the schedule.** Layer A relies on `ignorelb` + overrides being "proven in Test 0 (a)", but Tue–Fri has no slot for it, and this page's status line still says "next step is Test 0, then the dry-run". | **Decided 2026-11-17:** Wednesday morning, **before** Layer A goes on the nodes, software-only on mains (no power-down, no re-cabling): (a) LB fires at our thresholds, (r1) the virtual `ups` mirrors status and LB, (r2) FSD on `ups` reaches only its clients (throwaway test client that only logs), (r3) what clears FSD, (e) read DSM UPS/VMM settings and the safe-mode logs. During (a) the owner **unticks UPS support in DSM** (DSM may go to safe mode on `LOWBATT` even on mains) and re-enables it afterwards. (b)–(c) (outlet cycle) after the 20th; a real on-battery event can then be made by switching off the smart plug that feeds the UPS (via HA), with a hard time cap and close monitoring. |
| R11 | Battery age unknown; charge % on an old lead-acid battery can collapse under load; `maxOnBatterySec` has no value. | For the 20th the wall-clock cap is the primary stage 2 trigger; charge and runtime are backups. **Measured 2026-11-17** (smart plug feeding the UPS switched off, NAS UPS client disabled): battery is APCRBC176-type, **24 V 7 Ah (168 VAh)**, 3–5 y life; on battery the load reads 19–21 % (12 % on mains, inverter losses) ≈ 0.85 C; charge falls a steady **~2.3 %/min** (100→79 % in 9.5 min), runtime estimate tracks the clock (~91 %), voltage 24.4 → 23.2 V by 73 %: the first ~10 min look linear. The **switch to battery wedges the NUT driver** for ~20 s (watchdog restart; STALE/WAIT), confirming the tri-state design. **Deeper runs (stitched, three runs with short recharges, which flatter the result):** 0 min 100 % 24.4 V · 5 min 90 % 24.1 V · 9.5 min 79 % 23.7 V · 13 min 73 % 23.2 V · 15 min 68 % 22.7 V · 16.5 min 65 % **21.7 V** (stopped by the 22.0 V guard). The voltage knee starts after ~13–15 min while the UPS still claims 65 % and ~26 min runtime: **real usable runtime ≈ 18–20 min, and charge %/runtime are badly optimistic in the lower half.** **Decided 2026-11-17:** stage 2 at **10 min** on battery **or** battery voltage **≤ 23.2 V** (1-min average, ignore the first 2 min after the switch) **or** charge < 30 %; Layer A NUT thresholds raised to **~60 %** charge (calibrate in Test 0 (a); verify whether NUT can also use a voltage threshold); **battery replacement:** APCRBC176 (24 V 7.0 Ah, confirmed by the APC datasheet), but the BX manual states the battery is **not user-replaceable** (authorised service centre; UPS off and unplugged), so the owner prefers replacing the whole UPS. Battery voltage: scale verified (27.4 V float = 13.7 V/block, inside CSB.s 13.5–13.8 V window); absolute accuracy unverifiable without opening the unit. **Validated against the CSB HR1234W datasheet** (constant power per 12 V block, new: ~85 W for 45 min to 1.75–1.80 V/cell): at our ~72–90 W per block a new pack lasts ~35–60 min; ours reached 1.81 V/cell at ~16.5 min → **~35–50 % of new capacity, past end-of-life** (UPS norm 80 %); real empty ≈ 18 min. So the thresholds above were **tightened until the new battery is in: stage 2 at 8 min or ≤ 23.5 V, Layer A at ~75 % reported** (60 % would fire after cut-off). Longer term: an EcoFlow River 3 Plus (LiFePO4, ~286 Wh ≈ 1.5–2 h here) shows up as a USB HID UPS with a NUT `ecoflow-hid` subdriver (NUT ≥ 2.8.4; the pod runs 2.8.3) — separate evaluation, not a Friday dependency. |
| R12 | Cold boot without the NAS: if the live inventory points `k3s_registries` at the NAS registry container, or node DNS at the NAS DNS container, image pulls stall until timeouts. | **Checked + decided 2026-11-17.** Docker Hub pulls go via the NAS mirror first, and containerd falls back to upstream when it is down (slower, not broken). Node DNS = the two Technitium pods + one more resolver; all can be down at boot, but every plumbing/home-control pod uses cached images (`IfNotPresent`), so Technitium starts and DNS heals itself. 14 containers used `Always` and refuse to start offline even with the image on disk: the 5 **pinned-tag** arr apps (radarr, sonarr, lidarr, bazarr, prowlarr; `Always` came in with the 2026-07-19 migration, no stated reason) are switched to `IfNotPresent`; the `:latest` ones (tautulli, music-intake, smb-server, Keel, exporters) keep `Always` and wait for internet in recovery phase 2 (`ErrImagePull` expected). **Spegel: reopen candidate after the 20th** (a pod landing on a different node after a full restart while the internet and NAS mirror are down is the exact "reopen if" case; check that it resolves *tags* offline). |
| R13 | Housekeeping: graceful node shutdown leaves pods in `Failed`/`Terminated` after boot; `ups-state` must not be in git (ArgoCD would prune or revert it); HA's "PoE on at start" also runs on every normal HA restart (fine while "on" is the normal state). | **Decided 2026-11-17:** (1) the verify step deletes only `Failed` pods with reason `NodeShutdown`/`Terminated` before judging health (Kubernetes' own GC only starts at 12,500); (2) `ups-state` is created by the binary at runtime and is not in the chart, so ArgoCD never resets the orchestrator's memory (`ups-automation-config` stays in git, read-only for the binary); (3) PoE-on-at-HA-start also firing on normal HA restarts is intentional. |

### The 20th is a planned outage

> **Decided 2026-11-17 (revised the same evening): planned mode.** First "let the automation run", then reversed after
> the discharge tests (R11): with ~18 min of real runtime an on-battery shutdown only gains minutes, so on the 20th the
> owner runs a **clean shutdown on mains shortly before the announced cut** (the binary's `run --planned` if rehearsed,
> otherwise the manual runbook), then the gateway by hand. The full on-battery automation is **no longer a Friday
> dependency** and can be built and tested properly after the 20th.

The date and rough time are known and the owner is present. The safest procedure for a *planned* outage is a deliberate shutdown on
mains **before** the cut, not a first-ever run of freshly built on-battery automation against a battery of unknown age. Proposal: on the
20th run stage 1 + stage 2 on purpose ("planned mode", same code as the automation) shortly before the announced start, end with FSD,
and let the UPS carry only the network until it runs flat (a free discharge measurement of the network load). The on-battery triggers
stay armed as a backup in case the cut comes early. This lowers Thursday's go/no-go bar: Layer A + planned mode, not the full on-battery
state machine.

### Orchestrator form: Go now, but not an operator now

The question was whether "script first, Go operator later" is right. Verdict, in two parts:

- **A shell/kubectl script is the wrong tool for this state machine.** Tri-state status, persisted timers that run through STALE,
  undo before stage 2 but not after, write-once annotations, retries, two recovery gates and a second outage during recovery are
  exactly where untested bash fails, and the one rehearsal on Thursday exercises only the happy path. Testability matters more than
  usual here.
- **An operator (CRD, controller-runtime, leader election) is the wrong thing to build in one day**, and it adds nothing to safety:
  the testability comes from the *shape* of the code, not from being an operator. "More complex logic" is not a goal either; the logic
  should stay as small as it is now. Go is useful here for *checking* that logic, not for making it bigger.

Proposal:

1. One Go binary on its own orphan branch, reusing the `storage-operator` image/release/CI pattern (Go, table tests already there).
2. A **pure core** `Step(state, observation, now, config) → (state, actions)` with table tests for STALE windows, flaps, undo vs
   no-undo, FSD stand-down, write-once annotations, second outage during recovery; a fake UPS that replays recorded NUT logs including
   the hourly stall.
3. A thin executor: client-go (scale, annotate, CNPG hibernate, wait detached), a direct upsd TCP client (no Prometheus), HA calls for
   PoE and WoL (R8), FSD for the end of stage 2 (R7).
4. The same binary as a **laptop CLI**: `plan` (read-only dry-run: lists exactly what would be scaled, in which order) and
   `run --planned` (the 20th). The rehearsal, the planned outage and the automation then share one code path.
5. In-cluster as a 1-replica Deployment loop (`Recreate`, ~10 s poll) or the CronJob; either way the state stays in `ups-state`,
   written with `resourceVersion` checks so two copies cannot both act.
6. CRD + status, leader election: after the 20th, only if wanted.

Fallback if the binary and its tests are not green by Wednesday evening: the manual runbook (plan B), which the 20th mostly needs anyway.

## 3. Architecture: two layers

The flows (phases, stage 1, stage 2, Layer A, recovery) are drawn in the [one-page plan](ups-shutdown-overview.md); this page
keeps the evidence and the details behind them.

| Layer | What | Decisions |
|---|---|---|
| **A** safety net | NUT primary in the `nut` pod; secondaries on the 4 nodes and the NAS (DSM); FSD → kubelet tiers → poweroff | `DEADTIME` 120 s (mounted `upsmon.conf`, same on the node clients and checked on DSM); `ignorelb` + overrides; fires at *computed* low battery (thresholds from Test 1, issue A); needs no orchestrator, ArgoCD or n8n; accepted risk: needs k3s on the UPS node |
| **B** orchestrator | staged shedding by label tier, hibernate DBs, UPS cycle, recovery | one CronJob in `nut` (every minute, `concurrencyPolicy: Forbid`), node affinity away from the VM worker, no database, state in the `ups-state` ConfigMap; reads NUT directly, never Prometheus; Slack/n8n notifications are best-effort |

- **Layer B yields to FSD.** The two layers never run at the same time. Low battery is Layer A's job (issue B).
- **Orchestrator form: reopened (§2d).** Proposal: a Go binary now (pure, table-tested core + laptop CLI), no CRD/operator yet.
  The text below was the earlier "script first" decision. The dry-run needs measured numbers anyway, so the first
  version is a script run as a CronJob. A Go operator on its own orphan branch (the pattern of `storage-operator`,
  `aws-tunnels-operator` and `cert-push`) is the natural second step: a CRD declares tiers (label selectors), triggers, thresholds and
  hooks (SSH poweroff, Wake-on-LAN, PoE); status replaces the ConfigMap; leader election replaces `concurrencyPolicy`; a fake UPS source makes
  it unit-testable; reusable for any NUT UPS. Layer A stays plain NUT either way.
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

**How to switch the ports.** Shed: stage 1 calls an HA script (`switch.turn_off` on the two PoE ports); HA is up at that point (tier 2 stops later). Restore also goes through HA (decided 2026-11-17): HA is in recovery phase 1, so an HA automation on HA start / mains return switches the ports back on; no new UniFi credential. If HA fails to come back the two ports stay off (low impact). HA currently exposes PoE *power-cycle buttons* but no PoE on/off switch entities and no NUT sensors, so the per-port PoE switches probably need enabling in the UniFi integration (verify). Raw API fallback: `PUT .../rest/device/<id>` with the *full* `port_overrides` array (partial arrays wipe overrides). Use IPs, not DNS names, for the controller and the NAS SSH targets. The bedroom switch and AP are not on the UPS.

## 5. Label / annotation contract (git is the source of truth)

Only workloads with **state or a dependency order** need a tier. Everything stateless (cert-manager, MetalLB, Multus, reloader, NFD,
kube-fledged, exporters, ArgoCD's redis/dex/repo-server, Traefik, CoreDNS) stays **unlabelled and is stopped by the nodes' graceful
shutdown** (Layer A). The four tiers and their app lists are in the [one-page plan](ups-shutdown-overview.md) (section 3).

Set in an app's `values.yaml`; `shared-lib` renders it as a label on the Deployment/StatefulSet/CronJob:

```yaml
power:
  shed: stage1      # stage1 (tier 1) | final (tier 2) | keep (tier 3/4, stopped by the node) | freeze (controllers, CronJobs)
  needs: [nas]      # optional: stop BEFORE the NAS is touched (auto-detected too, see below)
```

Rendered as `power.homelab/shed=<value>` / `power.homelab/needs=<dep>`. The old `stage2` value is retired (issue C). Databases of
tier 3 are not labels but explicit steps (`smarthome-db-pg` is hibernated after tier 2).

**Maintenance-free guard.** A CI/pre-commit check (same idea as `gen-scale-to-zero-ignores.sh`) runs on the **rendered manifests across
`system/` and `workload/`** and fails when anything with persistence, a database or a NAS mount has no `power:` key. Charts that do
not use shared-lib (ArgoCD's own chart, Longhorn, upstream charts, plain CNPG manifests) get the label through their own values or a
kustomize patch. Unlabelled-and-stateless is allowed. The orchestrator itself must carry `shed=keep`; the guard enforces it.

At runtime the orchestrator:

1. Selects with `kubectl get deploy,sts,cronjob -A -l power.homelab/shed=<value>`; additionally **auto-detects** NAS users (pods mounting
   a PV whose NFS server is the NAS, **including inline `nfs`/CIFS volumes**) and treats them as `needs: nas` even if unlabelled.
   Non-mount NAS dependencies (its DNS container, the image mirror, the Longhorn backup target) are documented, not detected.
2. Writes `power.homelab/original-replicas=<n>` on the object **before** scaling: **write-once** (only if absent and > 0), covers
   ArgoCD controllers and Sablier, cleared when recovery completes (extra live annotations are ignored by ArgoCD's diff).
3. Scales to 0 (CronJobs: `suspend: true`), waits for pods gone **and** Longhorn volumes detached, then continues.
4. Freezes everything labelled `freeze` first, so nothing wakes or reverts the shed mid-outage: ArgoCD controller +
   applicationset-controller, Sablier, Keel, descheduler, Reloader. Also verify once that Longhorn's
   `allow-recurring-job-while-volume-detached` is off.

App-specific notes (the tier table itself lives in the one-page plan):

- **Tier 1 + NAS** (stop and confirm *gone* before the NAS poweroff; hard mounts wedge pods in `Terminating`): radarr, sonarr, bazarr,
  lidarr, slskd, music-intake, music-assistant, the NFS provisioner. Within tier 1 the DBs go last (`service-db-pg`, Vaultwarden's
  MySQL).
- **SMB:** smb-operator → smb-server → smb-announcer, operator first so it does not recreate the server.
- **Tier 2 (core):** Home Assistant, Zigbee2MQTT, Mosquitto, Matter server, Thread border router, monitoring, n8n. Stopped together
  with a short grace (60–120 s), then `smarthome-db-pg` is hibernated. Mosquitto goes to 3 Longhorn replicas.
- **Vault** must be unsealed again before the ArgoCD unfreeze in recovery (auto-unseal sidecar; OnDelete, so bumps need a manual pod delete).
- **Matter recovery gotcha:** the Thread border router's SRP registry is RAM-only, so a restart makes Thread devices re-register. Start
  the Matter server first, the border router last, then expect up to ~15 min of churn (SRP lease clamped to 15 min); not a failure.
- Voice (piper/whisper) and Vaultwarden are tier 1 (decided 2026-11-17).

Tunables live in one ConfigMap `ups-automation-config` (git-managed): `stage1DelaySec` (180), `stage2RuntimeSec`, `stage2ChargePct`,
`maxOnBatterySec`, `recoverStableSec`, `recoveryHomeStableSec` (120), `recoveryChargePct` (50), the UPS cycle delay, PoE port list,
per-stage enable flags.

## 6. Stage sequence and thresholds

The step-by-step flows for stage 1 and stage 2 are drawn in the [one-page plan](ups-shutdown-overview.md) (sections 4 and 5).

**Preconditions checked every step.** Status is tri-state: `OL` / `OB` / `STALE` (a driver stall, 15–40 s about once an hour). `STALE`
never counts as `OL`: it cannot undo a shed or reset "OL stable"; the on-battery clock runs from a persisted first-`OB` timestamp and
keeps counting through stale periods; **no escalation on staleness alone**. If `OL` is stable for `recoverStableSec` **before** stage 2
starts, undo cleanly (including Wake-on-LAN and VM start if the NAS/VM are already off). After stage 2 starts it always runs to the UPS
cycle and then recovers. Layer B yields as soon as FSD is set.

| Trigger (all gated on `OB`) | Layer | Action |
|---|---|---|
| `OB` seen | B | `ups-state=ARMED`, best-effort notify, no cluster change |
| `OB` ≥ `stage1DelaySec` (3 min) | B | stage 1 |
| `charge < stage2ChargePct` **or** `runtime < stage2RuntimeSec` **or** on battery > `maxOnBatterySec` | B | stage 2 |
| computed low battery (`ignorelb`) | A | FSD → kubelet tiers → poweroff |
| no UPS data > `DEADTIME` (120 s) | A | FSD, same |

Time budget (**measure, don't guess**): scale-down + detach X min, CNPG hibernate Y min, kubelet tiers Z min (worst case 360 s), NAS
~2–3 min. Set the thresholds from the measured total with ≥ 2× margin. Starting values, to be replaced by the Test 1 numbers (issue A):

| Threshold | Start value | Why |
|---|---|---|
| stage 2 (Layer B) | on battery **8 min** **or** battery voltage **≤ 23.5 V** (until the new battery; then 10 min / 23.2 V) **or** `charge < 30 %` | decided 2026-11-17 from the discharge test (§2d R11): the knee starts after ~13–15 min while charge %/runtime stay optimistic |
| computed low battery (Layer A) | ≈ `charge < 75 %` until the new battery (then ~60 %) (calibrated in Test 0 (a)) | the UPS still reported 65 % at 21.7 V, close to cut-off; 15 % would never be reached alive |
| UPS cycle delay | ≈ 480 s | 30 s poweroff scheduling + 360 s tier grace + OS shutdown + margin; measured, not guessed |
| expected stage-2 duration | ~3–4 min | stage 1 already stopped the NAS, the VM and the tier-1 apps; remaining: tier 2 in parallel (≤ 60 s) + detach (~1 min) + CNPG hibernate (seconds once clients are gone; 180 s `smartShutdownTimeout` and 1800 s `stopDelay` are ceilings) + kubelet tiers + poweroff |

## 7. Recovery

The flow is drawn in the [one-page plan](ups-shutdown-overview.md) (section 7). Decisions:

1. **Hosts boot:** minis (BIOS AC-restore), NAS (DSM Power Recovery), VM worker (VMM autostart, currently *disabled*), Pi (powers on with
   AC). If the NAS or VM did not boot (mains returned before the cycle, or after an SSH poweroff) the orchestrator sends
   **Wake-on-LAN** and starts the VM over SSH; an on-site ESPHome WoL node is the backup (one-page plan, "Backup wake").
2. Wait for nodes `Ready`; uncordon any cordoned node (the VM worker was drained).
3. **Phase 1, home control, once mains has been stable `recoveryHomeStableSec` (2 min):** remove the hibernation annotation on
   `smarthome-db-pg` and wait healthy, then Mosquitto, Zigbee2MQTT, Home Assistant, Matter server, Thread border router **last**,
   scaled by the orchestrator from `original-replicas` (ArgoCD is still frozen).
4. **Gate for phase 2:** charge ≥ `recoveryChargePct` (50 %) **and** mains stable 10 min. Shedding triggers stay armed from boot, so a
   second outage during recovery is handled like a first one.
5. **Phase 2:** (the PoE ports were already restored by HA's start automation in phase 1) wake `service-db-pg`, wait for Vault to be unsealed, scale Sablier back up, then unfreeze
   ArgoCD and the rest of the freeze list; selfHeal restores everything else (monitoring, n8n, tier 1). After ~5 min anything still at
   0 with an `original-replicas` annotation is scaled from it, tier 2 first, then tier 1 (fallback only).
6. Verify (no Pending/CrashLoop, Longhorn attached/healthy, NAS shares mounted), clear `ups-state` and the annotations, notify with
   the downtime.

NAS must be reachable before ArgoCD is unfrozen, or NAS-mounted workloads start against a missing share.

**ArgoCD is the primary restorer for phase 2, not a dependency-free one.** Freezing it first (so nothing reverts the scale-down) and
unfreezing it at the end (so selfHeal restores every replica from git) is the intended path. What it does **not** do, and the
recovery steps must:

- remove the CNPG hibernation annotation (live-only annotations are not in git, so ArgoCD never clears them);
- uncordon nodes, restore PoE ports, start the VM and the NAS;
- restore Sablier-managed apps: their replicas are ignored by design, so they stay at 0 until woken (the desired idle state);
- enforce order across apps: unfreezing flips ~40 apps at once (the small node has only ~6 GB, and ArgoCD's reconcile concurrency is
  already throttled for that reason). Hence the gate and the prerequisites *before* the unfreeze.

ArgoCD's repo-server needs **GitHub reachable and Vault unsealed** to render (lovely plugin) and its cache is not persistent, so after
a cold boot with the internet still down it cannot restore anything. That is why phase 1 is done by the orchestrator itself and why
the `original-replicas` fallback stays. Also expect ArgoCD to roll out any commits merged during the outage (e.g. Renovate bumps) once
unfrozen; harmless, but one more reason to unfreeze only when everything is healthy.

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
- **Safeguards (Test 2 cuts the LAN and the internet at the cycle):** dead-man alert, phone hotspot for the agent, kubeconfig by IP, plug read locally; after the cycle the runbook says *poll until the API answers*. **Test 0** (§8.1) runs before any of this.
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
| tier 2 (`final`) stopped (HA, Z2M, Mosquitto, Matter/Thread, monitoring, n8n) + `smarthome-db-pg` hibernated | 2 |
| all nodes off, just before the UPS cycle | 2 |

### Before you start (gates)

- [ ] Quiet window, **not** 02:00–05:00 (Plex Butler / Hyper Backup); household warned (DNS and Home Assistant go down in test 2).
- [ ] Laptop has a non-cluster DNS fallback, so GitHub/docs stay reachable while Technitium is down.
- [ ] Longhorn all volumes healthy; recent CNPG backups verified; Vault unseal keys at hand (manual Secret).
- [ ] Physical access to the rack in case an auto-power-on assumption fails.
- [ ] Enable VMM autostart on the NAS VM; check BIOS AC-restore on both minis, the DSM Power Recovery toggle and a Wake-on-LAN live test on one drained mini (§9).
- [ ] Enable the PoE switch entities in HA's UniFi integration (§4), or skip the PoE step and test it separately.
- [ ] Measurement running with ≥ 30 min baseline: plug power (Tuya plug, 1 W), NUT `ups.load` / `battery.runtime` /
      `battery.charge` (Prometheus is fine, mains is up), per-node CPU, and **temperature near the UPS**: NUT `battery.temperature` if this UPS reports it (not seen so far), otherwise any sensor you already own, e.g. a Zigbee2MQTT or Thread temperature sensor temporarily placed next to the battery compartment, plus the rack switch's own temperature already in HA as a free proxy for rack heat. Heat is what ages lead-acid, and a discharge test should show how much the battery warms.

### Test 1 — stage 1 (≈ 1.5 h incl. settling and recovery)

1. Freeze list (`shed=freeze`: ArgoCD controller + applicationset-controller, Sablier, Keel, descheduler to 0; CronJobs suspended).
2. Scale the `stage1 + NAS` apps to 0, write `original-replicas`, wait until pods are gone **and** volumes detached.
3. Scale the other `stage1` apps to 0, hibernate `service-db-pg`.
4. PoE: switch off the technical-room AP and the living-room switch.
5. Check Longhorn: volumes with a replica on the VM, replica states once the VM is down.
6. *Gate*, then cordon + drain the VM worker (existing ansible pattern), SSH `poweroff` it, wait until off, then `poweroff` the NAS.
7. Hold ~10 min and read the meters. Then test **both** recovery paths: the undo (Wake-on-LAN + VM start) and the normal recovery of §7.

### Test 2 — stage 2 + the UPS cycle (≈ 2 h incl. settling and recovery, only after test 1 passed and you said go)

1. Test 1, then stop tier 2 (`final`) with a 60–120 s grace, then hibernate `smarthome-db-pg`.
2. *Gate*, then the UPS cycle command (the command proven in Test 0, §2 row 3),
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
| Layer A alone shuts everything down in tier order | disable Layer B, pull mains, read timestamps | no faulted volume, no Postgres recovery mode, total ≤ the measured worst case |
| Home control (DB, MQTT, Z2M, HA, Matter, Thread) is back first | time from stable mains to HA healthy | well before phase 2 starts |

### Output

Per-step durations, anything that needed a manual push, surprises (stuck mounts, DNS gaps, Vault, HA entities not
returning) and the measured watts, all written into this page. Then decide: build the full orchestrator, build only
Layer A + the NAS/PoE shed, or stop at the safety net if the gains are not worth it.

## 8. Test plan (nothing here touches prod until the previous step passes)

1. **Test 0, bench facts** (the 40 W bulb as the only load; the rack re-cabled to the wall while the UPS node stays on USB):
   (a) set `ignorelb` + `override.battery.*.low`, confirm `LB` flips at the new values (safe on mains: upsmon only acts on LB when on
   battery); (b) try `load.off.delay <s>` and `shutdown.reboot` with mains **present and absent**: which one restarts the outlets, the
   maximum delay accepted, and that `ups.timer.*` counts down; (c) confirm a driver restart does not cancel a running countdown;
   (d) confirm DSM *Power Recovery* and BIOS "power on after AC loss" on both minis, plus a **Wake-on-LAN live test** from a powered-off
   mini (drain it, power off, send a packet; separately unplug and replug the power); (e) read DSM's UPS settings (time before safe mode,
   comm-loss behaviour) and the 2026-06-02 / 2026-07-20 safe-mode events.
2. **Dry-run orchestrator** with a mock UPS status (including injected `STALE` windows) and the actions pointed at the `testing`
   namespace + a label-selected scratch app. Abort path included.
3. **Layer A alone:** disable the orchestrator, pull mains (or simulate FSD), confirm nodes + NAS shut down cleanly in tier order and
   the UPS cycles. Check Longhorn/CNPG are clean on boot (no rebuild storms, no recovery-mode Postgres). Try it first on one drained
   node (kubelet `shutdownGracePeriodByPodPriority` + logind inhibitor, §2c #5/#18).
4. **Real discharge test** at a time you are home: measure actual runtime vs `battery.runtime` and the time budget in §6. Log battery
   temperature too (§7b). Tune thresholds from that, not from the estimate (issue A).
5. **Full run** end to end, including recovery, with `maxOnBatterySec` short.

## 9. Decisions

- [x] **Phase 0 dry-run by an agent (§7b)** before any implementation, with safeguards; Test 0 first.
- [x] **End state:** cycle the UPS right after the nodes are off, leaving battery in reserve (decided 2026-11-07, **kept** 2026-11-17).
      Backup wake path: on-site ESPHome WoL node (idea, one-page plan).
- [x] Monitoring (Prometheus, Alertmanager, Grafana, Loki) stays up to the end, in tier 2 (decided 2026-11-07).
- [x] **Tiers** (2026-11-17): `service-db-pg` and Vaultwarden (+ MySQL) tier 1; n8n tier 2; Vault tier 3; piper/whisper tier 1.
- [x] Stage 1 delay **3 min**; low battery is Layer A's panic path, no Layer B panic path; home control recovers first (§2c A–G).
- [x] Orchestrator form (**decided 2026-11-17, §2d**): a Go binary now (pure, table-tested core, executor, laptop CLI), following the
      orphan-branch conventions; no CRD/operator before the 20th. Replaces "CronJob script first".
- [x] §2d R1–R13 and planned mode: all decided (2026-11-17/18).
- [x] **Current plan: Layer A only** (2026-11-18, §0); full design paused until the UPS is replaced.
- [ ] Thresholds and cycle delay: measured in the rehearsal (issue A).
- [x] UPS cycle built behind `cycleEnabled: false`, off for 2026-11-20 until Test 0 proves the command; Layer A sends it only after the secondaries have finished (issue F).
- [x] PoE 1b (§4): shed and restore via Home Assistant. Still to do: enable the PoE switch entities in HA's UniFi integration (skip PoE shedding if that is not possible by the Wednesday rehearsal).
- [ ] ESPHome WoL node: which VLAN/port (must share L2 with the nodes and NAS), how the Pi is powered, first-packet delay vs the cycle delay.
- [ ] Replace the battery (RBC176) *before* trusting any threshold; see [rack power audit](../01-platform/power-nut.md).
- [ ] **Manual checks only you can do:** BIOS AC-restore on both minis; DSM Power Recovery toggle; enable VMM autostart; did the NAS
      restart on its own after the 2026-07-20 safe shutdown?
- [ ] Rotate the NUT `admin` password at some point: it was printed once in an audit transcript (not in git).

## 10. Repo changes (when built)

| Path | Change |
|---|---|
| `lib/shared-lib` | new `power:` values key (`stage1` / `final` / `keep` / `freeze`, `needs`) → label + validation; PriorityClasses for the shutdown tiers |
| `scripts/` + pre-commit/CI | guard on **rendered manifests across `system/` and `workload/`**: persistence, DB or NAS mount (incl. inline `nfs`/CIFS) must declare `power:`; orchestrator must be `keep` (modelled on `gen-scale-to-zero-ignores.sh`) |
| `system/nut/automation/` (from `disabled-automation/`) | orchestrator CronJob (`concurrencyPolicy: Forbid`, affinity away from the VM worker), RBAC (nodes get/list/patch + drain, deployments/statefulsets/cronjobs patch + scale, CNPG patch, ArgoCD controller scale), `ups-automation-config` + `ups-state` ConfigMaps |
| `system/nut/nut/` | mounted `upsmon.conf` with `DEADTIME 120`; shed groups: real UPS renamed `ups` → `apc` + `dummy-ups` repeater `ups` (§2d R7), and the ServiceMonitor `params.ups` → `[apc]` (alerts/dashboard/Homepage key on `instance="apc-bx1600mi"`, unchanged); `ups.conf` `ignorelb` + `override.battery.runtime.low` / `charge.low`; `NOTIFYCMD` on FSD → idempotent UPS cycle script (tracked command, countdown read-back); `SHUTDOWNCMD` a no-op; tier-4 priority; do not touch `allow_killpower` until tested |
| `ansible/roles/node_common/` | `nut-client`/upsmon (`monuser`, `DEADTIME 120`), logind drop-in `InhibitDelayMaxSec`, `WakeOnLan=magic` link config, SSH key (sudo is already NOPASSWD), post-check that the kubelet delay inhibitor lock has the right duration |
| `ansible/roles/k3s_pre/` | kubelet `shutdownGracePeriodByPodPriority` tiers (see [k3s tuning](k3s-tuning.md) §2); needs the logind drop-in above; re-check `KillMode` |
| `system/storage/longhorn` | priority class for manager / instance manager / CSI so storage outlives the DBs |
| `workload/smarthome/mosquitto` | Longhorn replicas 2 → 3 |
| ESPHome | Wake-on-LAN node on a non-UPS socket (idea) |
| `wiki/01-platform/power-nut.md` | replace "not built yet" with the final design |

