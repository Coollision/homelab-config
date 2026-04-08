# Matter Integration Guide — Homelab with Unifi & VLANs

This document is the complete reference for running the **OHF Matter.js Server** with
**Home Assistant** in a Kubernetes homelab on a **full Unifi network stack** with
**VLAN segmentation**.

---

## Table of Contents

1. [What is Matter and why does networking matter?](#1-what-is-matter)
2. [The VLAN Problem — Why a standard K8s deployment breaks Matter](#2-the-vlan-problem)
3. [The Solution — Multus CNI + dual MACVLAN interfaces](#3-the-solution)
4. [How it works end-to-end](#4-how-it-works)
5. [Unifi Configuration — Step by Step](#5-unifi-configuration)

- 5.1 [Switch port: add VLAN 40 and VLAN 5 as tagged VLANs on one node](#51-switch-port-add-vlan-40-and-vlan-5-tagged-on-one-node)
- 5.2 [Firewall rules: VLAN 30 ↔ VLAN 40](#52-firewall-rules)
- 5.3 [Firewall rules: commissioning from phones (VLAN 5)](#53-commissioning-from-intern-vlan)
- 5.4 [Settings you MUST disable](#54-settings-to-disable)
- 5.5 [WiFi IoT SSID setup](#55-wifi-iot-ssid)

6. [Node preparation](#6-node-preparation)

- 6.1 [Create the VLAN 40 and VLAN 5 sub-interfaces (no IPs)](#61-create-the-vlan-40-and-vlan-5-sub-interfaces)
- 6.2 [Label the trunk-capable node in Kubernetes](#62-label-the-trunk-capable-node)
- 6.3 [Persist the sub-interface via systemd-networkd](#63-persist-via-systemd-networkd)
- 6.4 [Ansible integration](#64-ansible-integration)

7. [Chart values to set](#7-chart-values)
8. [Kubernetes deployment](#8-kubernetes-deployment)
   - 8.1 [Deploy Multus first](#81-deploy-multus-first)
   - 8.2 [Deploy matter-server](#82-deploy-matter-server)
   - 8.3 [Home Assistant configuration](#83-home-assistant-configuration)
9. [Commissioning a Matter device](#9-commissioning-a-matter-device)
10. [Thread devices and Thread Border Routers](#10-thread-devices-and-border-routers)
11. [Troubleshooting](#11-troubleshooting)
12. [Moving to production (workload branch)](#12-moving-to-production)

---

## 1. What is Matter?

**Matter** is an open-source smart home connectivity standard (backed by Apple, Google, Amazon,
CSA). It runs over IP so all ecosystems can talk to the same device.

The transport is IP-based, but **discovery** is link-local:

| Protocol            | Purpose                                  | Network behaviour                                              |
| ------------------- | ---------------------------------------- | -------------------------------------------------------------- |
| **mDNS** (RFC 6762) | Discover Matter devices                  | IPv4 multicast `224.0.0.251` / IPv6 `ff02::fb`, **link-local** |
| **IPv6 link-local** | Device addressing                        | `fe80::/10`, **link-local**                                    |
| **ICMPv6 / NDP**    | IPv6 neighbour discovery, Thread routing | Link-local                                                     |
| **BLE**             | Initial commissioning                    | Bluetooth, no network needed                                   |
| **TCP/UDP unicast** | Ongoing device control after pairing     | Routable, crosses VLANs                                        |

**Link-local protocols do not cross router boundaries.** An mDNS packet on VLAN 40 never
reaches VLAN 30. This single fact is the root cause of every Matter + VLAN problem.

---

## 2. The VLAN Problem

Your homelab VLANs:

| VLAN    | ID  | Subnet          | Residents                                 |
| ------- | --- | --------------- | ----------------------------------------- |
| Servers | 30  | 192.168.30.0/24 | K8s nodes, Traefik, Home Assistant        |
| IoT     | 40  | 192.168.40.0/24 | Smart home devices, Thread border routers |
| Intern  | 5   | 192.168.5.0/24  | Computers, phones, tablets                |

A standard K8s pod deployment puts the pod on the cluster overlay network — effectively VLAN 30.

```
[IoT device — VLAN 40]  --mDNS multicast-->  [VLAN 40 switch]
                                                     |
                                            (router drops multicast)
                                                     |
                                   [K8s pod — VLAN 30]  ← never arrives
```

Matter devices are invisible to matter-server. Commissioning fails.

### Why Unifi's built-in mDNS repeater does NOT help

Unifi has a built-in mDNS feature (Settings → Networks → [network] → Advanced → **mDNS**).
This is a Layer-7 proxy that re-broadcasts mDNS across VLANs.

**Do NOT enable this for Matter.** The official Matter.js documentation states:

> _"Also do not enable any mdns forwarders on the network (the option is called mDNS on Unifi for
> example) as they tend to corrupt or severely hinder the Matter packets on the network."_

The Unifi proxy modifies packet timing and TTLs in ways that break Matter's commissioning flow.

---

## 3. The Solution

**Multus CNI + MACVLAN** — the Kubernetes-native way to give a pod real interfaces on
multiple VLANs, without moving the node or adding extra IPs to the node itself.

### What changes and what does not

|                      | Before            | After                                                 |
| -------------------- | ----------------- | ----------------------------------------------------- |
| Node VLAN            | VLAN 30 (servers) | **VLAN 30 — unchanged**                               |
| Node IP              | 192.168.30.x      | **192.168.30.x — unchanged**                          |
| Switch port          | VLAN 30 untagged  | VLAN 30 untagged **+ VLAN 40 tagged + VLAN 5 tagged** |
| Pod `eth0`           | Flannel overlay   | Flannel overlay (K8s cluster traffic)                 |
| Pod `net1`           | —                 | **MACVLAN on VLAN 40 (new)**                          |
| Pod `net2`           | —                 | **MACVLAN on VLAN 5 (new)**                           |
| Pod VLAN 40 IP       | —                 | DHCP lease on VLAN 40                                 |
| Pod VLAN 5 IP        | —                 | DHCP lease on VLAN 5                                  |
| mDNS reaches IoT     | ✗                 | **✅ direct on `net1`**                               |
| `hostNetwork` needed | yes               | **no**                                                |

The node keeps a single IP on VLAN 30. Only the **pod** gets additional VLAN 40 and VLAN 5 addresses.
No node migration, no IP re-addressing, no routing complexity.

---

## 4. How it Works

```
┌──────────────────────────────────────────────────────────┐
│  K8s node (e.g. node-gn)   — VLAN 30: 192.168.30.10     │
│                                                          │
│  Physical NIC: enp1s0  (untagged, VLAN 30 native)       │
│  VLAN sub-ifaces: vlan40 + vlan5 (NO IPs on node)│
│                                                          │
│  ┌─────────────────────────────────────────────┐        │
│  │  matter-server pod                          │        │
│  │  eth0: 10.42.x.x (Flannel/K8s cluster)     │        │
│  │  net1: DHCP lease on VLAN 40 (vlan40)    │        │
│  │  net2: DHCP lease on VLAN 5 (vlan5)      │        │
│  │                                             │        │
│  │  PRIMARY_INTERFACE=net1                     │        │
│  │  → mDNS binds to net1                       │        │
│  │  → discovers IoT devices on VLAN 40         │        │
│  │  → accepts direct client traffic on VLAN 5  │        │
│  └─────────────────────────────────────────────┘        │
└────────────────────────────┬─────────────────────────────┘
                             │ vlan40 + vlan5 (VLAN 40 + VLAN 5 tagged)
                             │
                    ┌────────▼────────┐
                    │  Unifi switch   │
                    │  Port: trunk    │
                    │  VLAN30 native  │
                    │  VLAN40 + VLAN5 tagged │
                    └────────┬────────┘
                             │
              ┌──────────────▼──────────────┐
              │      VLAN 40 — IoT          │
              │  Matter devices: 192.168.40.x│
              │  Thread border routers       │
              └─────────────────────────────┘
```

**Traffic flows:**

| Path                                | Interface                                 | Protocol                     |
| ----------------------------------- | ----------------------------------------- | ---------------------------- |
| HA → matter-server WebSocket        | `eth0` / K8s service DNS                  | TCP 5580 (Flannel overlay)   |
| matter-server ↔ IoT devices         | `net1` (MACVLAN)                          | mDNS multicast + TCP unicast |
| Phones / laptops ↔ matter-server    | `net2` (MACVLAN)                          | Direct VLAN 5 client traffic |
| Traefik → matter-server dashboard   | `eth0` / K8s service                      | HTTP via Flannel             |
| Phone → matter-server commissioning | `net2` (VLAN 5 IP) or `net1` (VLAN 40 IP) | TCP 5540                     |

---

## 5. Unifi Configuration

Log into **Unifi Network** (not UniFi OS). All steps are in the **Network** application.

### 5.1 Switch port: add VLAN 40 and VLAN 5 tagged on one node

You need to add VLAN 40 and VLAN 5 as **tagged VLANs** on the switch port of the node that will run
matter-server (typically `node-gn`, the mini PC). The node's primary VLAN 30 stays as the
native (untagged) VLAN — its IP does not change.

**Create a new switch port profile (or edit the existing one):**

1. **Settings → Profiles → Switch Port Profiles → + Create New** (or edit the existing VLAN-30 profile you use for server nodes).
2. Name it e.g. `servers-iot-trunk`.
3. **Native Network:** VLAN 30 (Servers) — this stays the node's primary VLAN.
4. **Tagged Networks:** Add **VLAN 40 (IoT)** and **VLAN 5 (Intern)** — this allows both VLAN sub-interfaces to exist on the host.
5. Save.
6. Go to **Devices → [your switch] → Ports** and apply the new profile to the port connected to the matter-server node.

The node's DHCP-assigned IP on VLAN 30 is unaffected. The node gets no new IP — only the pod will have VLAN 40 and VLAN 5 addresses.

> **Only one switch port change needed.** Only the node that runs matter-server requires this.
> All other nodes stay on their single-VLAN access port.

### 5.2 Firewall rules

Go to **Settings → Firewall & Security → Rules → LAN**.

For this Multus design, you should **not** add dedicated cross-VLAN firewall exceptions for
Flannel, kubelet, Home Assistant, or Longhorn just because matter-server has VLAN 40 and VLAN 5
interfaces.

- K3s node-to-node traffic stays on the node IPs in **VLAN 30**.
- Flannel overlay traffic stays on the normal cluster path via `eth0`.
- Home Assistant should connect to matter-server over the in-cluster service on `eth0`, not the
  VLAN 40 address.
- Longhorn replication is handled by the storage/node plane, not by the pod's VLAN 40 interface.

If your baseline firewall policy already allows normal cluster traffic inside the servers VLAN,
no extra Matter-specific rule is needed here.

### 5.3 Commissioning from Intern VLAN

When commissioning (pairing) a new Matter device, your phone (VLAN 5) needs to reach
matter-server. The Matter commissioning protocol uses TCP 5540.

Because matter-server now has a dedicated `net2` interface on VLAN 5, commissioning clients can
reach it directly on the same VLAN. In the normal case, this does **not** require an inter-VLAN
firewall rule.

Recommended approach:

- Keep phones/tablets on VLAN 5.
- Use the current matter-server VLAN 5 DHCP IP for direct client access when needed.
- Do not expose the VLAN 40 address to user devices unless you intentionally want routed client → IoT access.

If you enforce client isolation or host-level filtering inside VLAN 5, allow TCP `5540` from your
client devices to the matter-server VLAN 5 IP.

### 5.4 Settings to DISABLE

These Unifi settings break or degrade Matter. Check and disable them.

#### ❌ Disable Unifi mDNS repeater

`Settings → Networks → [each network] → Advanced → mDNS` → **OFF**

Disable on ALL networks. The Unifi mDNS proxy corrupts Matter commissioning packets.

#### ❌ Disable Multicast Enhancement on the IoT WiFi SSID

`WiFi → [IoT SSID] → Advanced → Multicast Enhancement` (also called "Multicast Optimization")
→ **OFF**

This feature converts multicast to unicast at the AP level. It destroys mDNS and Matter device
discovery. It must be OFF on the SSID used by IoT devices.

#### ⚠️ Review IGMP Snooping on VLAN 40

`Settings → Networks → VLAN 40 → Advanced → IGMP Snooping`

If IGMP Snooping is ON, some Matter devices that do not send proper IGMP joins may not receive
multicast. Options:

- Enable **IGMP Proxy** alongside snooping (handles the join issue).
- Or turn IGMP Snooping **OFF** on VLAN 40 — acceptable for a home IoT network.

#### ❌ Do NOT enable IPv6 RA Guard on VLAN 40

Thread devices require ICMPv6 Router Advertisements to propagate routes from Thread Border
Routers. Ensure RA Guard is not blocking ICMPv6 on VLAN 40.

### 5.5 WiFi IoT SSID

Matter over WiFi devices join your IoT WiFi network. Configure the SSID correctly:

1. Create (or use) an IoT SSID (e.g. `HomeIoT`).
2. Set its **Network** to **VLAN 40**.
3. Enable on **2.4 GHz** — many Matter devices are 2.4 GHz only.
4. **Band Steering: OFF** — prevents devices being forced to 5 GHz.
5. **Client Isolation: OFF** — devices need to talk to the VLAN gateway and each other.
6. **Multicast Enhancement: OFF** (see §5.4).

---

## 6. Node Preparation

Pick the node that will run matter-server (typically `node-gn`, the mini PC, since Home
Assistant prefers `type=mini` nodes and they benefit from co-location). Apply the switch
port change (§5.1) to this node's port.

> **Node hostname vs K3s node name:** Throughout this guide, `node-gn` is the OS/Ansible
> hostname and `master-gn` is the K3s node name (set via `k3s_node_name` in Ansible). Use
> the hostname when SSH-ing to the machine and the K3s node name in `kubectl` commands.
> Check your K3s node name with: `kubectl get nodes`

### 6.1 Create the VLAN 40 and VLAN 5 sub-interfaces

SSH into the target node and create the VLAN sub-interfaces. They get **no IPs** — the pod
takes the VLAN 40 and VLAN 5 addresses via MACVLAN.

```bash
# First, find the actual physical interface name on the node:
ip link show
# Look for the interface with the VLAN 30 IP — e.g. enp1s0, ens3, eth0.
# For node-gn this is enp1s0 (from Ansible inventory k3s_flannel_iface).

# Create the sub-interfaces (VLAN IDs 40 and 5)
# Replace enp1s0 with your actual interface name
sudo ip link add link enp1s0 name vlan40 type vlan id 40
sudo ip link add link enp1s0 name vlan5 type vlan id 5
sudo ip link set vlan40 up
sudo ip link set vlan5 up

# Verify
ip link show vlan40
ip link show vlan5
```

> The sub-interfaces have no IP address on the host. `ip addr show vlan40` and
> `ip addr show vlan5` will show no `inet` lines. This is correct — the pod takes
> both IPs via MACVLAN.

### 6.2 Label the trunk-capable node

The matter-server chart uses a **required** node affinity for `vlan40-trunk=true` and
`vlan5-trunk=true`. Apply both labels to the node where you created the sub-interfaces:

```bash
# Use the K3s node name (check with: kubectl get nodes)
kubectl label node master-gn vlan40-trunk=true
kubectl label node master-gn vlan5-trunk=true

# Verify
kubectl get node master-gn --show-labels | grep -E "vlan40-trunk|vlan5-trunk"
```

Without this label the pod will stay `Pending` with:

```
0/3 nodes are available: 3 node(s) didn't match Pod's node affinity/selector.
```

### 6.3 Persist via systemd-networkd

The sub-interfaces created in §6.1 are lost on reboot. Make them persistent using
systemd-networkd (or NetworkManager — adapt to your OS setup):

```ini
# /etc/systemd/network/40-vlan40.netdev
[NetDev]
Name=vlan40
Kind=vlan

[VLAN]
Id=40
```

```ini
# /etc/systemd/network/40-vlan5.netdev
[NetDev]
Name=vlan5
Kind=vlan

[VLAN]
Id=5
```

```ini
# /etc/systemd/network/41-vlan40.network
[Match]
Name=vlan40

[Network]
# No IP on the node — MACVLAN takes it.
LinkLocalAddressing=no
```

```bash
sudo systemctl restart systemd-networkd
ip link show vlan40   # should be UP after restart
```

```ini
# /etc/systemd/network/41-vlan5.network
[Match]
Name=vlan5

[Network]
# No IP on the node — MACVLAN takes it.
LinkLocalAddressing=no
```

```bash
sudo systemctl restart systemd-networkd
ip link show vlan40
ip link show vlan5
```

Also set the required IPv6 sysctl for Thread support:

```ini
# /etc/sysctl.d/99-matter-thread.conf
net.ipv6.conf.all.forwarding = 0
net.ipv6.conf.vlan40.accept_ra = 1
net.ipv6.conf.vlan40.accept_ra_rt_info_max_plen = 64
```

```bash
sudo sysctl --system
```

### 6.4 Ansible integration

Add the following to `ansible/inventory/host_vars/<your-node>.yml` (the node you picked):

```yaml
# VLAN sub-interfaces for Matter (no IPs — only the pod gets VLAN IPs)
k3s_node_label:
  - kubefledged.io/cache=true
  - type=mini
  - node.longhorn.io/create-default-disk=config
  - vlan40-trunk=true
  - vlan5-trunk=true

vlan_trunk_interfaces:
  - id: 40
    parent_interface: enp1s0
    interface_name: vlan40
  - id: 5
    parent_interface: enp1s0
    interface_name: vlan5
```

The `node_setup.yml` playbook now includes the `vlan-trunk` role. Any host with
`vlan_trunk_interfaces` defined will get the matching systemd-networkd files automatically.

---

## 7. Chart Values

Before deploying, set these values directly in `tests/smarthome/matter-server/values.yaml`
(or in your own override file):

```bash
# Multus MACVLAN network list
multusNetworks[0].name=matter-server-vlan40
multusNetworks[0].parentInterface=vlan40
multusNetworks[0].nodeLabelKey=vlan40-trunk
multusNetworks[1].name=matter-server-vlan5
multusNetworks[1].parentInterface=vlan5
multusNetworks[1].nodeLabelKey=vlan5-trunk

# Optional ingress hosts
ingress_internal.host=matter-server.local-domain
ingress_internal_secure.host=matter-server.example.com
```

> In DHCP mode, the pod receives dynamic addresses on both VLAN interfaces. You can check
> current leases with `kubectl exec -n smarthome <matter-server-pod> -- ip addr show`.

---

## 8. Kubernetes Deployment

### 8.1 Deploy Multus first

Multus must be running and its CRD (`network-attachment-definitions.k8s.cni.cncf.io`) must
exist before matter-server's `NetworkAttachmentDefinition` resource is created.

The `tests` ApplicationSet will auto-sync both apps. If matter-server fails on first sync
because the CRD is not yet installed, ArgoCD will auto-retry and succeed once Multus is
healthy. To deploy manually in order:

```bash
# 1. Trigger Multus sync via ArgoCD UI, or wait for auto-sync (~3 min).
#    Do NOT use `kubectl apply -f` on the chart templates directly —
#    they are Helm templates and will fail without `helm template` first.
argocd app sync multus   # or click Sync in ArgoCD UI

# 2. Wait for DaemonSet to be ready
kubectl rollout status daemonset/multus -n kube-system

# 3. Verify CRD exists
kubectl get crd network-attachment-definitions.k8s.cni.cncf.io

# 4. Sync matter-server (ArgoCD will auto-retry if it ran before CRD was ready)
argocd app sync matter-server
```

Verify Multus is working on the target node:

```bash
# The DaemonSet pod on the trunk node should be Running
kubectl get pods -n kube-system -l app=multus -o wide

# Multus creates a new CNI conf wrapping Flannel in K3s's CNI dir:
# (SSH to the node)
ls /var/lib/rancher/k3s/agent/etc/cni/net.d/
# You should see a 00-multus.conf file alongside the flannel conf
```

### 8.2 Deploy matter-server

```bash
# Check ArgoCD picked up the Application
kubectl get application matter-server -n argocd

# Watch sync
kubectl get application matter-server -n argocd -w

# Verify pod is running on the trunk node
kubectl get pods -n smarthome -o wide | grep matter-server

# Verify the pod has THREE interfaces (eth0 + net1 + net2)
kubectl exec -n smarthome <matter-server-pod> -- ip addr show
# You should see:
#   eth0: 10.42.x.x/x  (Flannel)
#   net1: DHCP lease on VLAN 40
#   net2: DHCP lease on VLAN 5

# Check matter-server started and is using net1
kubectl logs -n smarthome <matter-server-pod> | grep -i "primary\|interface\|mdns"
```

### 8.3 Home Assistant configuration

Go to **HA → Settings → Devices & Services → Add Integration → Matter (BETA)**.

**Recommended: Use the K8s in-cluster service DNS**

```
ws://matter-server-service.smarthome.svc.cluster.local:5580/ws
```

This keeps traffic entirely within the Flannel overlay network (VLAN 30 → VLAN 30). No
inter-VLAN firewall rules needed for the HA → matter-server connection. This works because
HA and matter-server are both in the same K8s cluster.

**Alternative: Use the VLAN 40 pod IP directly**

```
ws://<current-net1-dhcp-ip>:5580/ws
```

This routes through Unifi inter-VLAN (VLAN 30 → VLAN 40) and weakens the separation between the
servers and IoT networks. Prefer the in-cluster service DNS unless you explicitly want direct
routed access to the pod's VLAN 40 interface.

---

## 9. Commissioning a Matter device

Commissioning = the initial pairing of a new device to your Matter controller.

### Flow

```
1. Power on the new Matter device (factory reset or first boot)
2. HA → Settings → Devices & Services → Matter (BETA) → + Add Device
3. HA generates a QR code / setup code via matter-server
4. Scan with your phone (VLAN 5) or enter the code manually
5. Phone communicates with device via BLE
6. Phone sends IoT WiFi credentials (VLAN 40 SSID) to the device
7. Device joins VLAN 40 WiFi and gets a 192.168.40.x IP
8. matter-server (with net1 on VLAN 40) discovers the device via mDNS on net1
9. Clients on VLAN 5 can also reach the pod directly on net2 when needed
10. Commissioning completes; device appears in HA
```

### Network paths during commissioning

| Step                         | Protocol   | Path                                                                   |
| ---------------------------- | ---------- | ---------------------------------------------------------------------- |
| Phone ↔ Device (BLE pairing) | Bluetooth  | Direct, no network                                                     |
| HA → matter-server           | WebSocket  | In-cluster (Flannel) or VLAN 30→40                                     |
| Phone → matter-server        | TCP 5540   | VLAN 5 → net2 directly, or VLAN 5 → VLAN 40 if using net1 DHCP address |
| matter-server ↔ Device       | mDNS + TCP | VLAN 40 on `net1` (direct, same VLAN)                                  |

---

## 10. Thread Devices and Border Routers

**Thread** is a low-power IPv6 mesh used by many Matter sensors and bulbs. Thread devices
don't connect to WiFi — they join a Thread mesh via a **Thread Border Router (TBR)**.

Common TBRs: Apple HomePod mini, Apple TV 4K (3rd gen), Google Nest Hub 2nd gen.

TBRs sit on your **VLAN 40 WiFi**. The matter-server pod communicates with TBRs via its
`net1` interface (VLAN 40), which is exactly right.

### IPv6 requirements on the node

For Thread routing to work (ICMPv6 RIOs from border routers), these sysctl values must be
set on the node (see §6.3 for persistence):

```bash
# MUST be 0 — IPv6 forwarding enabled breaks RIO processing (RFC 4191)
sysctl net.ipv6.conf.all.forwarding       # expect: 0

# MUST be 1 — accept Router Advertisements from Thread Border Routers
sysctl net.ipv6.conf.vlan40.accept_ra  # expect: 1

# MUST be 64 — accept route info from border routers for Thread prefixes
sysctl net.ipv6.conf.vlan40.accept_ra_rt_info_max_plen  # expect: 64
```

### Unifi IPv6 notes for Thread

- Do NOT enable **RA Guard** or block ICMPv6 on VLAN 40.
- Ensure **Multicast Enhancement is OFF** on the IoT SSID (Thread uses multicast IPv6).
- If using a separate Thread SSID, ensure it is also tagged to VLAN 40.

---

## 11. Troubleshooting

### Pod stays Pending

```bash
kubectl describe pod -n smarthome <matter-server-pod>
```

- **"didn't match node affinity"** → Run `kubectl label node <node> vlan40-trunk=true vlan5-trunk=true`
- **PVC not bound** → Check Longhorn is healthy: `kubectl get pvc -n smarthome`
- **Image pull error** → Requires internet access to `ghcr.io`

### Pod starts but net1 or net2 is missing

```bash
kubectl exec -n smarthome <pod> -- ip addr show
# If only eth0 is shown:
```

- Verify Multus DaemonSet is running: `kubectl get pods -n kube-system -l app=multus -o wide`
- Verify the NAD exists: `kubectl get net-attach-def -n smarthome`
- Verify the pod annotation is set: `kubectl get pod -n smarthome <pod> -o jsonpath='{.metadata.annotations}'`
- Check Multus logs on the node: `kubectl logs -n kube-system <multus-pod-on-that-node>`

### net1 or net2 has no IP / wrong IP

```bash
kubectl exec -n smarthome <pod> -- ip addr show net1
```

- Verify `parentInterface` in values matches the actual sub-interface name on the node
- Verify the sub-interface is UP: `ssh node-gn -- ip link show vlan40`
- Verify the VLAN 5 sub-interface is UP when checking `net2`: `ssh node-gn -- ip link show vlan5`
- Verify the switch port has VLAN 40 tagged (Unifi → Devices → switch port)

### IoT devices not discovered

```bash
kubectl logs -n smarthome <matter-server-pod> | grep -iE "mdns|discover|interface"
```

- Confirm `PRIMARY_INTERFACE=net1` is set in pod env: `kubectl exec -n smarthome <pod> -- env | grep PRIMARY`
- Verify IoT device is on VLAN 40: check Unifi client list for `192.168.40.x` address
- Disable **Multicast Enhancement** on the IoT WiFi SSID in Unifi
- Check mDNS multicast membership: `kubectl exec -n smarthome <pod> -- cat /proc/net/igmp`

### HA cannot connect to matter-server

```bash
# Test from within the HA pod
kubectl exec -n smarthome <homeassistant-pod> -- nc -zv matter-server-service 5580
```

- If using in-cluster DNS: confirm the K8s service exists: `kubectl get svc -n smarthome`
- If using VLAN 40 IP: confirm inter-VLAN policy allows TCP 5580 from HA to the current net1 DHCP IP

### Commissioning from phone fails

- Confirm phone (VLAN 5) can reach matter-server TCP 5540 on the current net2 DHCP IP (or net1 DHCP IP if using that path)
- Disable VPN on the phone during commissioning
- Confirm the IoT device joined the correct WiFi SSID (VLAN 40) in Unifi client list
- Try factory resetting the device and recommissioning

### MACVLAN limitation: host cannot ping pod's VLAN 40 IP

This is a known Linux MACVLAN limitation in bridge mode: **the host and its MACVLAN
child cannot communicate directly**. This is expected and harmless for our setup because:

- HA connects to matter-server via K8s service DNS (Flannel, not MACVLAN)
- The host node does not need to reach the pod's VLAN 40 IP
- Other VLAN 40 devices (IoT) CAN reach the pod normally

---

## 12. Moving to Production

When matter-server works correctly from the `tests` branch, migrate to `workload/smarthome/`
on `master` following the instructions in `tests/README.md`.

```bash
# 1. Copy chart to production location on master branch
cp -r tests/smarthome/matter-server workload/smarthome/matter-server
git add workload/smarthome/matter-server
git commit -m "feat: promote matter-server to production"
git push origin master

# 2. Release ArgoCD ownership from tests ApplicationSet
kubectl patch application matter-server -n argocd --type=json \
  -p='[{"op": "remove", "path": "/metadata/ownerReferences"}]'

# 3. workloads ApplicationSet adopts it automatically (~3 minutes)
kubectl get application matter-server -n argocd \
  -o jsonpath='{.metadata.ownerReferences[0].name}'
# Should output: workloads

# 4. Disable in tests branch (don't delete yet — keep as fallback)
git checkout tests
git mv tests/smarthome/matter-server tests/smarthome/disabled-matter-server
git push origin tests
```

Also promote the Multus chart to system:

```bash
cp -r tests/kube-system/multus system/kube-system/multus
# Move to production following same ownership-transfer steps
```

---

## Summary Checklist

### Unifi (one-time setup)

- [ ] Switch port for the matter-server node updated to trunk profile (VLAN 30 native + VLAN 40 tagged + VLAN 5 tagged)
- [ ] DHCP scope active on VLAN 40 for Matter net1 lease
- [ ] DHCP scope active on VLAN 5 for Matter net2 lease
- [ ] **mDNS repeater DISABLED** on all networks
- [ ] **Multicast Enhancement DISABLED** on IoT WiFi SSID
- [ ] IGMP Snooping reviewed on VLAN 40
- [ ] IoT WiFi SSID: 2.4 GHz, VLAN 40, client isolation OFF, Band Steering OFF

### Node (one-time setup)

- [ ] VLAN 40 sub-interface created: `ip link add link enp1s0 name vlan40 type vlan id 40`
- [ ] VLAN 5 sub-interface created: `ip link add link enp1s0 name vlan5 type vlan id 5`
- [ ] systemd-networkd files created for persistence (§6.3)
- [ ] sysctl `99-matter-thread.conf` created and applied (§6.3)
- [ ] Node labelled: `kubectl label node master-gn vlan40-trunk=true vlan5-trunk=true`
- [ ] Ansible inventory updated with `vlan40-trunk=true`, `vlan5-trunk=true`, and `vlan_trunk_interfaces` (§6.4)

### Chart Values

- [ ] `multusNetworks` list includes `matter-server-vlan40` and `matter-server-vlan5` with the right `parentInterface` values

### Kubernetes / ArgoCD

- [ ] PR merged to `tests` branch
- [ ] Multus Application synced and DaemonSet healthy
- [ ] matter-server Application synced
- [ ] Pod running on trunk node with DHCP-assigned `net1` and `net2` addresses
- [ ] `kubectl exec` confirms `PRIMARY_INTERFACE=net1`
- [ ] Longhorn PVC bound

### Home Assistant

- [ ] Matter integration added with WebSocket URL:
      `ws://matter-server-service.smarthome.svc.cluster.local:5580/ws`
- [ ] Matter integration shows as connected
- [ ] First device commissioned and visible
