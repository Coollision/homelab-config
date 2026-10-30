# TODO — One stable uplink name on every node

[← Back to Home](../Home.md) · Related: [Networking](../01-platform/networking.md) · [Ansible](../00-setup/ansible.md) · [Networkd migration](networkd-migration.md)

> **Status (2026-10-30):** rolled out on all four nodes (Pi, VM, both mini PCs) and verified. Left: after a soak,
> run the `cleanup` phase per node to drop the old-name fallback config, then delete this page, folding the outcome
> into [Networking](../01-platform/networking.md).

## Why

The VLAN sub-interfaces already have the same name on every trunk node (`vlan40`, `vlan5`, from
`vlan_trunk_interfaces`). The physical uplink does not: it is `enp1s0` on the two mini PCs, `enp3s0` on the VM and `eth0`
on the Raspberry Pi. Things that sit directly on the uplink therefore only work where the name happens to match:

- the Technitium macvlan attachments (VLAN 30 is untagged, so there is no sub-interface to attach to). Both pods are
  eligible on the Pi by label, but the attachment hardcodes the mini-PC name, so the secondary was stuck on it with
  `Link not found` (2026-10-30);
- the per-node `parent_interface` of the VLAN trunk and `k3s_flannel_iface`, which are separate per-node values today.

Goal: every node calls its primary NIC `uplink0`. One value everywhere, and a NIC swap or a network-stack change
(networkd migration, Debian upgrade) cannot change it. The name avoids the kernel's `ethN` and `enpNsN` families, which
systemd warns can collide with a pinned name at boot.

## What changes

| Where | Change |
|---|---|
| Host, all nodes | `/etc/systemd/network/10-uplink.link`: match the NIC by its permanent MAC, `Name=uplink0`. Applied by udev at boot, independent of ifupdown, networkd or NetworkManager. Sorts before the Pi OS and default `.link` files |
| Host, ifupdown nodes | stanza renamed to `uplink0`; the **old stanza stays** until the node is verified, so a failed rename still brings the old name up and the node stays reachable |
| Host, Pi (NetworkManager) | the wired profile is not bound to a name; the VLAN profiles get `vlan.parent` set to `uplink0` |
| Inventory | `k3s_flannel_iface` and every `parent_interface` become `uplink0` (private inventory, plus the example inventory) |
| Cluster | Technitium `parentInterface` becomes `uplink0` (GitOps, flipped at a fixed point of the rollout) |
| Docs | networking and Ansible pages, Matter integration examples |

MAC and DHCP lease are untouched, so the IP and the router reservation stay the same.

## Safety gates (before any reboot)

1. Exactly one primary NIC (the default-route device) and a permanent MAC for it.
2. `udevadm test-builtin net_setup_link` on the uplink says `uplink0` is applied, and on every VLAN sub-interface says
   the file is **not** applied (they share the parent's MAC, so a wrong match key would rename them).
3. The inventory already says `uplink0` for flannel and the trunk parents (no half-edited node).
4. On ifupdown nodes the old stanza is still present as the fallback.

## Rollout (one node per run, reboot cutover)

Staging writes the files and flags a reboot; the rolling update then does the normal cordon, drain, reboot, wait,
uncordon, Longhorn heal. A verify step asserts: `uplink0` exists and the old name does not, same IPv4 and default
route, VLAN sub-interfaces on `uplink0`, node Ready, flannel on `uplink0`.

1. Raspberry Pi (canary, nothing else depends on it; proves the Technitium secondary starts there).
2. VM node (snapshot first).
3. Flip Technitium `parentInterface` to `uplink0` in Git.
4. Trunk node without the control plane.
5. Control-plane node last (about 5 minutes without an API server; etcd snapshot first). Do this right after step 3,
   because a restart of the primary on the control-plane node would fail in between.
6. Remove the old ifupdown stanzas, update the docs, delete this page.

Prometheus network series change their `device` label (`enp1s0` to `uplink0`), so graphs split at the cutover.

## Open items

- [ ] Run `uplink_name_phase=cleanup` (`-e uplink_legacy_name=<old name>`) on each node once you trust the rename.
- [ ] A fresh node must get the `uplink0` pin at bootstrap (`uplink_name.yml` stage, reboot) before `k3s_setup.yml`:
      the k3s template defaults to `uplink0`. The `uplink_name` role is meant to be retired after the cleanup phase, so
      by then this belongs in the bootstrap steps (or `node_setup`).
- [ ] Gotchas found on the way, now handled in the roles: the initramfs does not need the `.link` file (udev renames in
      the real root), and the staged rename is applied with `cluster_update.yml -e node_update_force_reboot=true`
      (staging assumes the reboot follows immediately). The `reboot-required` flag alone now also
      triggers a reboot in `node_update` (precedence fix), so the flag staged by the role is enough; the force flag still works.
