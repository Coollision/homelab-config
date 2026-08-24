# Networking

[← Back to Home](../Home.md) · Platform: [Storage](storage.md) · [Secrets](secrets-vault.md) · [Cluster housekeeping](cluster-housekeeping.md)

Networking is the most-tuned, most-incident-prone part of this platform — see
[Known Issues](../05-operations/known-issues.md) for the long tail of things that have
gone wrong here. This page covers the steady-state design.

## The pieces, and how traffic actually flows

```
Internet ──▶ Cloudflare Tunnel ──▶ "external" Traefik ──▶ ingress objects opting into
                (cloudflared)         (2nd instance)        ingressClass: traefik-external

LAN/VLAN5 ──▶ MetalLB LB IP ──▶ "internal" Traefik ──▶ every normal IngressRoute
                                  (the main one)

IoT devices (VLAN 40) ──▶ Multus MACVLAN leg on the relevant pod ──▶ e.g. matter-server,
                                                                        ESPHome, mdns-reflector
```

### MetalLB — `system/metallb-system/metallb`
Plain **L2 mode** (FRR/BGP explicitly disabled) — one `IPAddressPool` + `L2Advertisement`.
This is what hands out LoadBalancer IPs to the internal Traefik, the NUT UPS server, and
anything else needing a real LAN IP.

### Multus — `system/kube-system/multus`
The enabling layer for **secondary network interfaces per pod**. Deployed as a thick-plugin
DaemonSet with delegate plugins `macvlan`, `dhcp`, `ipvlan`, `sbr` (source-based routing),
`tuning`. Any pod that needs to be L2-adjacent to a VLAN — to do mDNS discovery, to talk
Thread, to reach IoT devices directly — gets a `net1`/`net2` MACVLAN leg defined in its
`shared-lib` `multusNetworks` values block.

**The MAC-addressing convention** (`lib/shared-lib/templates/_multus.yaml`): every stub MAC
follows `02:{40|05}:67:9d:64:XX` — second octet `40` = VLAN 40 (IoT), `05` = VLAN 5
(Intern) — so you can tell which VLAN a pod's stub belongs to at a glance in the UniFi
client list. See [Matter & Thread](../02-smarthome/matter-thread.md) and
[ESPHome](../02-smarthome/esphome.md) for concrete users of this pattern, and
[Known Issues](../05-operations/known-issues.md#multus-dhcphostname-is-a-dead-end) for why
naming these stubs needs a UniFi alias, not the chart's `dhcpHostname` field.

**The `sbr` chained plugin is load-bearing, not optional.** Without it, a MACVLAN's DHCP
lease installs a *competing default route*, hijacking the pod's egress and breaking access
to cluster DNS/the internet. `sbr` confines that VLAN's routes to a separate source-keyed
routing table so `eth0` stays the default route. This bit ESPHome specifically (see
[Known Issues](../05-operations/known-issues.md#esphome-vlan40-mdns-oom)).

### Traefik — `system/kube-system/traefik` (internal) and `system/kube-system/cloudflare-tunnel` (external)
Two separate Traefik instances exist on purpose:

- **Internal Traefik** gets a MetalLB LB IP and serves every normal in-cluster Ingress.
  It carries the default TLS cert (the cert-manager wildcard, see
  [Certificates](certificates.md)) and the Sablier scale-to-zero Traefik plugin (see
  [Cluster housekeeping](cluster-housekeeping.md)).
- **Cloudflare Tunnel** (`cloudflared`, 2 replicas) terminates the tunnel from Cloudflare's
  edge and forwards everything to a **second, dedicated "external" Traefik instance**
  (aliased `traefik-external`). Only ingress objects that explicitly opt into
  `ingressClass: traefik-external` are internet-reachable at all — everything else is
  internal-only by default. This second Traefik also carries the Sablier plugin, so
  externally-exposed scale-to-zero apps (e.g. BabyBuddy) still wake correctly through this
  extra hop.

### Technitium DNS — `system/dns/technitium`
A clustered, ad-blocking, authoritative DNS server (primary + secondary in-cluster, plus
a tertiary Docker container on the NAS as a manual break-glass). The full non-obvious
config — MACVLAN-only communication, DANE/TLSA cert validation, catalog-zone notify
inheritance, IPv6 recursion ACLs — is substantial enough to live entirely in
[Known Issues → Technitium](../05-operations/known-issues.md#technitium-dns-cluster). The
short version: **every DNS-cluster node talks over its VLAN 30 MACVLAN IP, never a
ClusterIP or node IP**, because the off-cluster NAS node can't route to Kubernetes
ClusterIPs and the API won't let a primary's advertised IP change after init.

## VLAN map (as encoded in the repo)

| VLAN | Role | Who's on it |
|---|---|---|
| 5 | Intern | Home Assistant, matter-server (2nd leg), thread-border-router (2nd leg, mDNS only), mdns-reflector |
| 30 | Servers | Technitium DNS cluster, Mosquitto's LoadBalancer IP |
| 40 | IoT | matter-server (primary), thread-border-router (primary/Thread backbone), mdns-reflector, ESPHome |

The **mdns-reflector** (`workload/smarthome/mdns-reflector`) exists purely to bridge
mDNS announcements between VLAN 40 and VLAN 5, so phones on the LAN can discover IoT
devices. Its implementation choice (a dumb L2 repeater, not a re-publishing responder) is
the single most important lesson from the Matter mesh's history — see
[Known Issues → the mDNS reflector conflict loop](../05-operations/known-issues.md#mdns-reflector-must-not-re-publish).

## Design rationale worth keeping in mind

- **Why a second Traefik instance for the tunnel, instead of one Traefik with two
  entrypoints?** Isolation: the external path (internet-facing) and internal path
  (LAN-only) are fully separate Deployments, so an internal-only app can never
  accidentally become internet-reachable just by a routing mistake — it has to explicitly
  opt into the external ingress class.
- **Why MACVLAN and not a CNI overlay for IoT traffic?** mDNS and Thread border-router
  discovery are fundamentally L2 protocols; an overlay network (Flannel) can't participate
  in VLAN-scoped multicast the way a MACVLAN leg — a real, VLAN-tagged NIC as far as the
  switch is concerned — can.
