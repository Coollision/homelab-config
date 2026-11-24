# TODO — Zigbee coordinator: USB Sonoff dongle → PoE Sonoff Dongle Max (no mass re-pairing)

[← Back to Home](../Home.md) · Related: [Zigbee](../02-smarthome/zigbee.md) · [Networking](../01-platform/networking.md)

> **Status (2026-11-24): cutover done.** Zigbee2MQTT runs on the Max (EmberZNet 9.1.0.0, EZSP v19) over TCP with the
> original network; 56 devices online, 2 needed a manual re-pair. Stability is being watched. Keep the old stick untouched
> as the rollback and delete this page once the Max has run stable for ~2 weeks.

## Outcome in short

- Same network restored from a converted backup: PAN ID, ext PAN ID, network key, channel 25 and coordinator IEEE are unchanged.
- Only **19 per-device link keys** fit: the Max's key table has 20 entries, the stick's backup carried 58. The restore used a
  backup trimmed to 19 keys (lights, shutters, bedroom buttons, outdoor battery sensors); the other devices rejoined on the
  network key. Two devices (one plug, one ceiling light module) had to be re-paired by hand.
- Link quality improved: median LQI 51 → 96 over the 53 devices measured on both coordinators (one snapshot each, so
  re-check after a few days).

## Goals

- Keep **every** Zigbee device paired: same network, restored from a *converted* backup.
- Be able to roll back to the old USB stick at any time (Max dies, flaky link, ...).
- Drop the Z2M node pin: no USB passthrough, the pod can run anywhere.

## Decisions

| Topic | Decision | Why |
|---|---|---|
| VLAN | Same VLAN as the k3s nodes (server VLAN), static DHCP lease | Serial-over-TCP is jitter-sensitive; no routed hop/firewall. Zigbee RF is unaffected by VLAN. |
| Adapter | Max = EFR32MG24 → `ember`; the old stick was Z-Stack (`zstack`) | **Z2M does not officially support zstack→ember.** A plain Z-Stack backup is rejected, so it was converted with the community converter (`--ezsp-version 19`). |
| Firmware | EmberZNet 9.1.0.0 / EZSP v19 (shipped with 7.4.5.0, updated before the cutover) | Z2M supports 7.4.x–9.1.x; no larger-key-table build exists for this adapter (see the key table entry in [Known Issues](../05-operations/known-issues.md#ember-key-table-too-small)). |
| Channel | Unchanged (25) | Changing it forces sleepy devices to re-pair. |
| Transport | TCP server mode on the Max (port 6638, WiFi off) | `serial.port: tcp://<coordinator-ip>:6638`, `adapter: ember`. |
| Placement | Soft `type=mini` node affinity + `preventDeschedule` pod label | The VM node has known disk-I/O hangs; the descheduler must not bounce this singleton. |
| Repo change timing | Pushed **at cutover**, not before | Pushing early reschedules the pod without the stick. |

## What happened on 2026-11-24, and what differed from the plan

1. **Firmware.** The Max shipped with EmberZNet 7.4.5.0 (EZSP v13) and was updated to 9.1.0.0 (EZSP v19) before the cutover. The
   converter's default EZSP version is 13, so it was run with `--ezsp-version 19`.
2. **IEEE.** The old coordinator IEEE was written through the Max web UI (it has a restore-factory-IEEE button, so it is
   reversible) and verified on the NCP itself over TCP 6638 (EZSP `getEui64`), not only in the UI.
3. **No dry run.** The Max sits in the rack within RF range of the live mesh, so a dry run would have put the production
   PAN/key/IEEE on a second coordinator next to the stick. The first real start was the test (abort rule + rollback).
4. **Longhorn RWX hang.** After Z2M was stopped and the share-manager pod was recreated, the node NFS clients kept a dead
   connection and the Z2M pod sat in `ContainerCreating` for about an hour (see the Longhorn RWX share-manager restart hang
   entry in Known Issues). Fixed at node level; no data was touched.
5. **Key table too small.** The first start with the full converted backup failed before any network was formed:
   `Current key table of 20 is too small to import backup of 58`. Fix: trim the backup (Known Issues entry above), then the log
   showed `Imported 19 keys` and `restored`.
6. **One ASH crash.** About 2 s after that first successful start the NCP gave up (`ERROR_EXCEEDED_MAXIMUM_ACK_TIMEOUT_COUNT`). The
   pod restarted and has been stable since (see the Known Issues entry on the ack timeout). If it recurs, see the fallback
   ladder below.

## Still open

- [ ] **Watch stability** for a few days (Z2M log, pod restarts, availability). Fallback ladder: 1) pin Z2M to 2.14.2 (one upstream
      report saw disconnects with this adapter over TCP on 2.14.3), 2) check the Max serial-over-network settings, 3) roll back
      to the stick.
- [ ] Check the Max socket/flow-control settings (serial-over-network); `rtscts` stays unset.
- [ ] Check that the Thread channel is not on or adjacent to Zigbee channel 25.
- [ ] Confirm the rack PoE switch is on the UPS.
- [ ] Check the HA side: entities kept their ids (`button.zigbee2mqtt_bridge_*` must not have become `_2`), button automations
      still fire, no replayed events.
- [ ] One ceiling light module has a weak link (LQI ≈ 12, but it responds): consider a router nearby.
- [ ] **Backup safety net:** scheduled copy of `coordinator_backup.json` off the PVC (CronJob or existing backup tooling) so a dead
      Max can be restored with a recent frame counter.
- [ ] Add a monitoring alert: Z2M bridge offline / coordinator connection lost.
- [ ] After a few stable days: remove the leftover full-converted backup copy (`coordinator_backup.full-ember.json.bak`) from the
      Z2M PVC (it contains the network key).

## Rollback (any time)

The Max now holds the live network, so its frame counter advances. The old stick must not start while the Max is powered.

- [ ] Disable ArgoCD auto-sync (`kubectl -n argocd patch app zigbee2mqtt --type merge -p '{"spec":{"syncPolicy":{"automated":null}}}'`) and
      scale Z2M to 0 (`kubectl -n smarthome scale sts zigbee2mqtt --replicas=0`). **Take a fresh backup from the Max first** (frame counter).
- [ ] **PoE-disable the Max's switch port** before the stick starts (never two coordinators on one PAN).
- [ ] Build the rollback backup from the *original zstack backup* (kept off-cluster): set `network_key.frame_counter` well above the Max's current
      counter plus a large margin. An ember-written backup has no `stack_specific.zstack`/tclk_seed, so it cannot be used directly.
- [ ] The stick still holds the network in NVRAM with a stale frame counter and Z2M would skip the restore: wipe the stick's NV so the restore runs.
- [ ] Set `serial.port: /dev/ttyUSB0`, `adapter: zstack` in `configuration.yaml` on the PVC and put the rollback backup in place (Z2M still stopped).
- [ ] Plug the stick into a node, `git revert` the chart part of the cutover commit (affinity + hostPath patch; NFD label + udev rule stay), push,
      re-enable auto-sync (`... '{"spec":{"syncPolicy":{"automated":{"prune":true,"selfHeal":true}}}}'`; the AppSets do not restore it for you).
- [ ] Verify: mains devices reconnect within minutes, battery devices at their next check-in (up to ~1 h), toggle a light, press a button.

## Cleanup (after ~2 weeks stable)

- [ ] Remove the NFD `zigbee-coordinator` rule + udev rule (ansible) if no longer wanted; update `wiki/01-platform/cluster-housekeeping.md`,
      `docs/cluster-update-playbook.md` and `system/nut/README.md` mentions.
- [ ] Delete this page and its nav entry.
- [ ] Update the HA-side memory/docs (coordinator type, channel, adapter) in the Home Assistant config repo.

## Risks

- Z2M over a TCP serial bridge can drop the NCP (ack timeout seen once). Mitigation: watch, fallback ladder above, UPS on the PoE switch.
- A Longhorn RWX share-manager restart stalls the Z2M pod (stale NFS client connection, node-level fix); consider moving single-node Z2M data off RWX.
- The Max's key table is 20: new devices that request a unique link key may not get a table slot. One slot was left free on purpose.
- Frame counter mismatch after restore/rollback → devices silently ignore the coordinator. Mitigation: fresh backup before each swap, Z2M's margin.
- Stale `configuration.yaml` overwritten on shutdown → edit only while Z2M is stopped, re-verify.
- Two coordinators on one PAN → never power the old stick and the Max on the same network at the same time.
