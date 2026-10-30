# TODO — Node networking: ifupdown to systemd-networkd

[← Back to Home](../Home.md) · Related: [Networking](../01-platform/networking.md) · [Ansible](../00-setup/ansible.md) · [Plex / GPU](plex-migration.md)

> **Status (2026-10-30):** investigated, nothing changed. The Debian 12 → 13 upgrade deliberately **keeps ifupdown**
> (DHCP client pinned, IP-continuity gate). This page is the separate follow-up: do it **after** the nodes have settled
> on Debian 13 and **before** Debian 14. Delete the page, folding the outcome into [Networking](../01-platform/networking.md),
> once it is done.

## TL;DR

- Small job: about **8–14 person-hours** including a VM rehearsal and docs. Risk is moderate on the control-plane node
  (etcd, single API server) and low on the VM node (snapshot).
- The x86 nodes are as simple as it gets: one DHCP uplink, plus two IP-less VLAN sub-interfaces on the nodes that carry
  the Multus macvlans. No bridges, bonds, pre/post-up hooks or static host routes. The macvlan routes live inside the
  pods, not on the nodes, so they are unaffected.
- The `vlan_trunk` role already has a networkd `vlan.netdev.j2` template and detects the active network manager. Missing:
  the uplink DHCP `.network`, the VLAN `.network` files, a networkd.conf drop-in, resolv.conf handling and a cutover task
  with a revert timer.
- The real reason to do it is the **DHCP client**, not ifupdown (see the last section).

## Current state (verified 2026-10-30, read-only)

| | Nodes with VLAN trunk | VM node |
|---|---|---|
| OS / tools | Debian 12, ifupdown 0.8.41, isc-dhcp-client | same |
| Uplink | `<uplink-nic>` via `inet dhcp` | same, different NIC name |
| VLANs | `vlan40`, `vlan5` as `inet manual`, no IPv4, IPv6 from router adverts | none |
| networkd | running, every link `unmanaged`, `/etc/systemd/network/` empty | not running |
| resolved / NetworkManager | not running | same |
| resolv.conf | regular file written by dhclient | same |
| dhclient | Debian default config: sends **no** client identifier (reservation keys on the MAC); hooks for classless routes and timesyncd | same |

The Raspberry Pi node uses NetworkManager and is out of scope; the role already skips or fails correctly for it.

## Target config

Uplink (`/etc/systemd/network/10-uplink.network`):

```ini
[Match]
Name=<uplink-nic>
[Network]
DHCP=yes                   # or DHCP=ipv4 to mirror today exactly
IPv6AcceptRA=yes
VLAN=vlan40
VLAN=vlan5
KeepConfiguration=dhcp-on-stop
[DHCPv4]
ClientIdentifier=mac       # critical: the default (duid) may get a different lease
UseDNS=yes
UseNTP=yes
UseRoutes=yes
```

VLAN links: the existing `vlan.netdev.j2`, plus a new `.network` per VLAN with `LinkLocalAddressing=ipv6`,
`IPv6AcceptRA=yes` (Matter, Thread and mDNS paths rely on it), `DHCP=no`, `ConfigureWithoutCarrier=yes`.

Keep networkd away from the Kubernetes links with a `networkd.conf.d` drop-in: `ManageForeignRoutes=no` and
`ManageForeignRoutingPolicyRules=no`, and never a catch-all `[Match] Name=*`. `cni0`, `flannel.1`, `veth*` and the
macvlan children must stay `unmanaged`.

DNS: networkd does not write `/etc/resolv.conf`. Recommended: install `systemd-resolved` and symlink resolv.conf to its
stub (k3s then hands the kubelet the real upstream file; confirm on a test node). Turn resolved's own mDNS and LLMNR
**off** (`MulticastDNS=no`, `LLMNR=no`) so it cannot clash with the mDNS reflector or Matter on UDP 5353.
NTP from DHCP is handled natively by `UseNTP=yes`, replacing the dhclient hook.

## Procedure (per node)

1. Stage the networkd files with Ansible; leave `/etc/network/interfaces` in place but unused. Install resolved, do not
   start it yet.
2. Arm a revert that **survives a reboot** (a persistent timer unit): it removes the networkd files, re-enables
   `networking.service` and reboots. Cancel it only after every gate passes.
3. Cut over with a **reboot**, not a live swap: disable `networking`, move `interfaces` aside (keep `lo`), enable
   networkd and resolved, reboot. A live `ifdown` + reload leaves a window where nothing owns the NIC on a remote host.
4. Gates, in order: same IPv4 as before · default route and DNS resolve · `networkctl` shows uplink and VLANs
   `configured` and everything else `unmanaged` · IPv6 on the VLANs · node Ready · flannel pod-to-pod across nodes ·
   Multus pods (DNS, Home Assistant, Matter, OTBR, ESPHome) restart with their extra interfaces · Longhorn replicas
   healthy · on the control-plane node, etcd and `/readyz` healthy.
5. Order: VM node first (snapshot, no VLANs) → the Multus/VLAN node (drain first) → control-plane node last (about
   5 minutes without an API server; take the etcd snapshot first).

Lock-out fallbacks: VM node = hypervisor console and snapshot. Bare metal = the revert timer, then a local keyboard and
monitor, then the netboot node restore (stated as available, **not verified**: test it once before relying on it).

## Effort and risk

| Work item | Size | Hours |
|---|---|---|
| Ansible: networkd role (uplink + VLAN `.network`, drop-in, resolved), hook into vlan_trunk and its teardown, cutover task with revert timer | M | 3–5 |
| Rehearsal: throwaway Debian VM on a trunk port carrying the VLANs, with a real DHCP reservation for its MAC (only this catches client-id and VLAN problems; a container cannot) | M | 2–4 |
| Rollout of three nodes with the gates | M | 2–3 |
| Wiki: networking, bootstrap, ansible cheatsheet, known issues | S | 1 |

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| Different DHCP lease (client identifier) | medium with defaults, low with `ClientIdentifier=mac` | high (flannel, etcd, DNS records) | `ClientIdentifier=mac`, VM rehearsal, IP gate, optional fixed lease |
| Lock-out of a bare-metal node | low | high | revert timer, console, netboot restore |
| networkd flushes CNI routes or links | low with the drop-in | high | `ManageForeignRoutes=no`, narrow `[Match]` |
| resolved mDNS/LLMNR clashes with reflector or Matter | medium | medium | disable both in resolved.conf |
| VLAN IPv6 adverts lost, Matter/Thread break | medium | medium | `IPv6AcceptRA=yes` on VLANs, run the Matter debug checks afterwards |
| Control-plane reboot = API outage | certain | low (about 5 min) | quiet window, etcd snapshot first |

## Is staying on ifupdown viable?

ifupdown itself is fine for years. The pressure is the DHCP client:

- ISC DHCP (including dhclient) went end-of-life upstream in 2022 (not re-verified in this investigation).
- Debian 12 and 13 still ship `isc-dhcp-client`; on 13, ifupdown prefers `dhcpcd`, which sends a different client
  identifier. That is why the 12 → 13 upgrade pins the old client.
- Whether Debian 14 still ships it is **unverified**: check the `isc-dhcp` package tracker before the Debian 14 freeze.
  If it goes, the choice is dhcpcd under ifupdown or this migration; networkd settles the identifier with one line.

## Open items

- [ ] Confirm in the router's lease table how the reservation is keyed today (MAC versus option 61), before choosing
      `ClientIdentifier`.
- [ ] Verify the k3s behaviour with resolved's stub resolver on a test node.
- [ ] Test the netboot restore of a bare-metal node once, before the control-plane cutover.
- [ ] Check the `isc-dhcp` package status for Debian 14 before its freeze.
