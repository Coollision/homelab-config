# Debian release upgrade (distro_upgrade.yml) {#distro-upgrade}

In-place Debian major upgrade (today: 12 bookworm → 13 trixie) of the K3s nodes, one node per run,
inside the same cordon → drain → reboot → verify → uncordon cycle as `cluster_update.yml`
(`roles/node_update` with `node_update_distro_upgrade=true`). See also [Ansible](ansible.md) and
`docs/cluster-update-playbook.md`.

## What it does {#what-it-does}

1. **Preflight** (also in dry run): classify the node as *done*, *fresh* or *resume*; refuse
   release skipping, unclean dpkg, pending old-release updates, holds, a VM without a snapshot ack,
   and the control plane before the others. It detects the network stack (`networkd` when
   `10-uplink.network` exists, `systemd-networkd` is active and `networking.service` is not enabled;
   `ifupdown` when `networking.service` is active; anything else is refused) and then refuses an
   ifupdown DHCP primary NIC without `isc-dhcp-client`, or a networkd uplink that is not `configured`
   / not the default-route interface / without `systemd-resolved` and the stub `resolv.conf`.
   Dry run also simulates the upgrade against the target sources in a scratch apt dir under `/tmp`
   (`-e node_update_distro_simulate=false` for strictly read-only).
2. **Cordon + drain** (unchanged). Drain comes *before* apt on purpose: open-iscsi/iscsid restart
   during the upgrade, which is only safe with no Longhorn volumes attached.
3. Backups on the node (`/var/backups/distro-upgrade`: `/etc` tarball, dpkg selections, network
   snapshot, old sources). Control plane: `k3s etcd-snapshot save`, then the snapshot,
   `server/token` and `/etc/rancher/k3s/config.yaml` are fetched to `node_update_distro_offnode_backup_dir`
   (controller-side, outside this repo).
4. `apt-mark manual` for the network stack (ifupdown: `ifupdown`, `isc-dhcp-client`; networkd:
   `systemd-networkd`, `systemd-resolved`), Longhorn deps and timesyncd; deb822
   `debian.sources` for the target; old source files moved aside.
5. **Point of no return**, then minimal upgrade (`upgrade --without-new-pkgs`) and `full-upgrade`,
   both `--no-install-recommends`, confdef/confold + `UCF_FORCE_CONFFOLD`, async with a hard timeout.
6. Backports kernels (`~bpo`) purged and the `linux-image-*` meta package moved to the target release
   before reboot (a bpo kernel can otherwise out-rank the new kernel and boot again).
7. Pre-reboot gates: no newly installed `multipath-tools`/`dhcpcd-base`/host `iptables` (networkd nodes:
   also not `ifupdown`); ifupdown: `isc-dhcp-client` still installed; networkd: `systemd-resolved` installed
   and both units enabled, `10-uplink.network` present, `resolv.conf` still the resolved stub symlink,
   `networking.service` not enabled; newest kernel (dpkg version order) is `+debNN` with an
   initramfs and a GRUB entry plus a fallback kernel, `findmnt --verify`, iGPU PCI ID known.
8. Reboot, post-boot gates (release + kernel, systemd failed units (`systemd-networkd-wait-online` is
   allowlisted on ifupdown nodes only, where networkd sees every link as unmanaged; on networkd nodes it
   must pass), interface set / primary IP / gateway unchanged, DNS resolves; ifupdown: `networking.service`
   active and `dhclient` running; networkd: `systemd-networkd` and `systemd-resolved` active, uplink
   `configured` in `networkctl`, `resolv.conf` stub symlink intact, k3s node `InternalIP` equals the uplink
   IPv4; k3s + `flannel.1`, Longhorn host deps, NTP sync, `/tmp`),
   iGPU check, uncordon, pod + Longhorn heal barrier, soak (`node_update_distro_soak_minutes`, default 10),
   barrier again.

The k3s binary is never touched (`node_update_upgrade_k3s=false` is enforced). Whenever a k3s bump is done,
run it as its own `cluster_update.yml` pass, before or after the distro upgrade, never inside the
same drain (k3s wants server-first, this play wants control-plane-last).

"Done" means all of: major version on target, `dpkg --audit` empty, nothing pending, running a
`+debNN` kernel, node schedulable. A node that reports 13 but fails any of these is resumed, not
skipped (base-files is upgraded early, so a crash mid-upgrade already reports 13). A resumed run
starts with `dpkg --configure -a`. A node that is clean but still **cordoned** (e.g. left that way
by `node_update_keep_cordoned`) gets an uncordon-only pass: no apt, no reboot.
The escape-hatch apt holds (`node_update_distro_hold_packages`) are released again after a successful upgrade.

`-e node_update_keep_cordoned=true` is for a single node (`--limit`), only as the hand-off to this
follow-up: the node is left drained and cordoned, and the settle wait, soak and image prune are skipped.

## Prerequisites checklist {#prerequisites}

- [ ] No other maintenance run active (the shared lock enforces it; `cluster_update.yml` finished).
- [ ] Old release fully patched: `cluster_update.yml -e node_update_upgrade_k3s=false` (preflight refuses otherwise).
- [ ] Longhorn: no degraded/faulted volumes.
- [ ] Dry run of the node read and understood (simulation summary: kernel, removals, watch-list).
- [ ] VM node: hypervisor snapshot taken → `-e node_update_distro_snapshot_taken=true`.
- [ ] Control plane: off-node directory (e.g. a NAS mount) → `-e node_update_distro_offnode_backup_dir=/abs/path`.
- [ ] DHCP, ifupdown nodes only: primary NICs keep `isc-dhcp-client` (enforced). If you ever drop that, first confirm
      MAC-based reservations for every node and pass `-e node_update_distro_allow_dhcp_client_change=true`.
- [ ] Console access to the bare-metal nodes in case the network does not come back.

## Order and commands {#order}

Canary VM worker → bare-metal worker (longer soak, it has the iGPU/VLAN special cases) →
control plane last. One node per run:

```bash
cd ansible
ansible-playbook playbooks/distro_upgrade.yml --limit <node-name>          # dry run, every node
ansible-playbook playbooks/distro_upgrade.yml --limit <vm-node> -e node_update_dry_run=false -e node_update_distro_snapshot_taken=true
ansible-playbook playbooks/distro_upgrade.yml --limit <worker-node> -e node_update_dry_run=false -e node_update_distro_soak_minutes=60
ansible-playbook playbooks/distro_upgrade.yml --limit <control-plane-node> -e node_update_dry_run=false \
  -e node_update_distro_offnode_backup_dir=/abs/path/outside/repo
```

`--check` is refused (apt results in check mode are meaningless); the default dry run is the preview.
Raspberry Pi OS nodes already on the target are skipped; older Pi OS is refused (reflash instead).

## Failure and rollback {#rollback}

| Failure point | State | What happens / what to do |
|---|---|---|
| Preflight | untouched | play stops, node never cordoned |
| Before the point of no return (backups, sources) | sources rewritten, nothing installed | rescue restores the old sources, `apt-get update`, uncordons (only if this run cordoned the node) |
| After the point of no return | mixed userspace or new release | rescue runs `dpkg --configure -a` once, prints dpkg/apt processes, leaves the node **cordoned**; fix over SSH, re-run the same command (it resumes) |
| Reboot timeout | unknown | console; pick the previous kernel in GRUB "Advanced"; check `/etc/network/interfaces` |
| Post-boot gate | new release, booted | stays cordoned; workloads already run elsewhere |
| Unrecoverable | — | VM: revert the hypervisor snapshot. Bare-metal minis: the existing network-boot node restore (user's procedure, not verified here), then `node_setup.yml` + `k3s_setup.yml` rejoin. Pis: re-image the SD card |

A failed run keeps the maintenance lock on purpose; check the node, then re-run with
`-e maint_lock_break=true`.

**Control-plane cluster state**: a node restore does not bring back etcd. Restore from the fetched
snapshot on the restored node (token + config from the same off-node directory):

```bash
systemctl stop k3s
k3s server --cluster-reset --cluster-reset-restore-path=/path/to/pre-trixie-<node>-<ts>[.zip]
systemctl start k3s
```

## Decisions {#decisions}

- **Network stack**: ifupdown + `isc-dhcp-client` were kept for the 12 → 13 upgrade (decided 2026-10-30), with the
  DHCP-client pin and IP-continuity gate mandatory. The nodes have since moved to systemd-networkd
  ([Networking](../01-platform/networking.md#network-stack)).
- **/tmp**: stays on disk, `tmp.mount` masked (decided 2026-10-30). Override with
  `node_update_distro_upgrade_tmp_mode=tmpfs_capped` (+ `node_update_distro_tmpfs_size`) or `tmpfs_default`.
- **No full-disk image** of the control-plane node; the off-node etcd snapshot + token/config is the
  required cluster-state backup, node restore uses the netboot procedure.

## Adding the next release {#next-release}

1. Add an entry to `node_update_distro_releases` in `roles/node_update/defaults/main.yml` (codename, security suite,
   components) and set `node_update_distro_to_major`.
2. Read the release notes ("Issues to be aware of", "Upgrading"); add any quirks to
   `tasks/distro_release_upgrade.yml` guarded by `node_update_distro_to_major`.
3. Re-check by hand: DHCP/network stack, iptables/nftables default, kernel vs iGPU PCI IDs, Python vs
   controller ansible-core, open-iscsi/multipath vs Longhorn, k3s support for the kernel/cgroups.
4. Test on a throwaway VM (DHCP NIC, ideally a backports kernel installed), then the canary order above.

## Known limits {#limits}

- **Network stack:** both ifupdown + `isc-dhcp-client` and systemd-networkd (`networkd_migrate`) nodes are handled, the
  gates branch on the detected stack. NetworkManager (Raspberry Pi OS) is out of scope; the networkd path has not run
  against a real node yet.
- **Open questions for 13 → 14 (forky release notes are still a draft and say nothing on networking):** whether
  `systemd-networkd` is split into its own package or `ifupdown` / `dhcpcd-base` packaging changes (the `apt-mark` loop
  only marks packages that exist, so a split is harmless but unverified); whether new `systemd-resolved` defaults touch
  `/etc/resolv.conf` or the drop-ins from `networkd_migrate`; whether a new systemd renames or re-orders interfaces. Re-read the
  final release notes ("Issues to be aware of") and the dry-run simulation watch-list before the 13 → 14 run.
- The netboot restore of a bare-metal node has never been tested.

- Not exercised against a real node yet; only syntax/lint/template rendering were checked locally.
- Raspberry Pi OS in-place upgrade is not implemented (preflight refuses).
- No post-uncordon Multus `net1` check; macvlan pods are verified only by their own health.
- `node_update_intel_gpu_check_image` (trixie-slim) is Renovate-managed; consider pausing that bump during the upgrade window.
