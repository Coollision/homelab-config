# Matter Integration Guide — Homelab with Unifi & VLANs

This document is the complete reference for running the **OHF Matter.js Server** with
**Home Assistant** in a Kubernetes homelab on a **full Unifi network stack** with
**VLAN segmentation**.

---

## Table of Contents

1. [What is Matter and why does networking matter?](#1-what-is-matter)
2. [The VLAN Problem — Why a standard K8s deployment breaks Matter](#2-the-vlan-problem)
3. [The Solution — Multus CNI + MACVLAN](#3-the-solution)
4. [How it works end-to-end](#4-how-it-works)
5. [Unifi Configuration — Step by Step](#5-unifi-configuration)
   - 5.1 [Switch port: add VLAN 40 as a tagged VLAN on one node](#51-switch-port-add-vlan-40-tagged-on-one-node)
   - 5.2 [Firewall rules: VLAN 30 ↔ VLAN 40](#52-firewall-rules)
   - 5.3 [Firewall rules: commissioning from phones (VLAN 5)](#53-commissioning-from-intern-vlan)
   - 5.4 [Settings you MUST disable](#54-settings-to-disable)
   - 5.5 [WiFi IoT SSID setup](#55-wifi-iot-ssid)
6. [Node preparation](#6-node-preparation)
   - 6.1 [Create the VLAN 40 sub-interface (no IP)](#61-create-the-vlan-40-sub-interface)
   - 6.2 [Label the trunk-capable node in Kubernetes](#62-label-the-trunk-capable-node)
   - 6.3 [Persist the sub-interface via systemd-networkd](#63-persist-via-systemd-networkd)
   - 6.4 [Ansible integration](#64-ansible-integration)
7. [Vault secrets to create](#7-vault-secrets)
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

| Protocol | Purpose | Network behaviour |
|---|---|---|
| **mDNS** (RFC 6762) | Discover Matter devices | IPv4 multicast `224.0.0.251` / IPv6 `ff02::fb`, **link-local** |
| **IPv6 link-local** | Device addressing | `fe80::/10`, **link-local** |
| **ICMPv6 / NDP** | IPv6 neighbour discovery, Thread routing | Link-local |
| **BLE** | Initial commissioning | Bluetooth, no network needed |
| **TCP/UDP unicast** | Ongoing device control after pairing | Routable, crosses VLANs |

**Link-local protocols do not cross router boundaries.** An mDNS packet on VLAN 40 never
reaches VLAN 30. This single fact is the root cause of every Matter + VLAN problem.

---

## 2. The VLAN Problem

Your homelab VLANs:

| VLAN | ID | Subnet | Residents |
|---|---|---|---|
| Servers | 30 | 192.168.30.0/24 | K8s nodes, Traefik, Home Assistant |
| IoT | 40 | 192.168.40.0/24 | Smart home devices, Thread border routers |
| Intern | 5 | 192.168.5.0/24 | Computers, phones, tablets |

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

> *"Also do not enable any mdns forwarders on the network (the option is called mDNS on Unifi for
> example) as they tend to corrupt or severely hinder the Matter packets on the network."*

The Unifi proxy modifies packet timing and TTLs in ways that break Matter's commissioning flow.

---

## 3. The Solution

**Multus CNI + MACVLAN** — the Kubernetes-native way to give a pod a real interface on a
different VLAN, without moving the node or adding a second IP to the node.

### What changes and what does not

| | Before | After |
|---|---|---|
| Node VLAN | VLAN 30 (servers) | **VLAN 30 — unchanged** |
| Node IP | 192.168.30.x | **192.168.30.x — unchanged** |
| Switch port | VLAN 30 untagged | VLAN 30 untagged **+ VLAN 40 tagged** |
| Pod `eth0` | Flannel overlay | Flannel overlay (K8s cluster traffic) |
| Pod `net1` | — | **MACVLAN on VLAN 40 (new)** |
| Pod VLAN 40 IP | — | 192.168.40.x (from Vault secret) |
| mDNS reaches IoT | ✗ | **✅ direct on `net1`** |
| `hostNetwork` needed | yes | **no** |

The node keeps a single IP on VLAN 30. Only the **pod** gets a second VLAN 40 address.
No node migration, no IP re-addressing, no routing complexity.

---

## 4. How it Works

```
┌──────────────────────────────────────────────────────────┐
│  K8s node (e.g. node-gn)   — VLAN 30: 192.168.30.10     │
│                                                          │
│  Physical NIC: enp1s0  (untagged, VLAN 30 native)       │
│  VLAN sub-iface: enp1s0.40  (tagged, NO IP on node)     │
│                                                          │
│  ┌─────────────────────────────────────────────┐        │
│  │  matter-server pod                          │        │
│  │  eth0: 10.42.x.x (Flannel/K8s cluster)     │        │
│  │  net1: 192.168.40.51 (MACVLAN on enp1s0.40) │        │
│  │                                             │        │
│  │  PRIMARY_INTERFACE=net1                     │        │
│  │  → mDNS binds to net1                       │        │
│  │  → discovers IoT devices on VLAN 40         │        │
│  └─────────────────────────────────────────────┘        │
└────────────────────────────┬─────────────────────────────┘
                             │ enp1s0.40 (VLAN 40 tagged)
                             │
                    ┌────────▼────────┐
                    │  Unifi switch   │
                    │  Port: trunk    │
                    │  VLAN30 native  │
                    │  VLAN40 tagged  │
                    └────────┬────────┘
                             │
              ┌──────────────▼──────────────┐
              │      VLAN 40 — IoT          │
              │  Matter devices: 192.168.40.x│
              │  Thread border routers       │
              └─────────────────────────────┘
```

**Traffic flows:**

| Path | Interface | Protocol |
|---|---|---|
| HA → matter-server WebSocket | `eth0` / K8s service DNS | TCP 5580 (Flannel overlay) |
| matter-server ↔ IoT devices | `net1` (MACVLAN) | mDNS multicast + TCP unicast |
| Traefik → matter-server dashboard | `eth0` / K8s service | HTTP via Flannel |
| Phone → matter-server commissioning | `net1` (VLAN 40 IP) | TCP 5540 (inter-VLAN routed) |

---

## 5. Unifi Configuration

Log into **Unifi Network** (not UniFi OS). All steps are in the **Network** application.

### 5.1 Switch port: add VLAN 40 tagged on one node

You need to add VLAN 40 as a **tagged VLAN** on the switch port of the node that will run
matter-server (typically `node-gn`, the mini PC). The node's primary VLAN 30 stays as the
native (untagged) VLAN — its IP does not change.

**Create a new switch port profile (or edit the existing one):**

1. **Settings → Profiles → Switch Port Profiles → + Create New** (or edit the existing VLAN-30 profile you use for server nodes).
2. Name it e.g. `servers-iot-trunk`.
3. **Native Network:** VLAN 30 (Servers) — this stays the node's primary VLAN.
4. **Tagged Networks:** Add **VLAN 40 (IoT)** — this allows the VLAN 40 sub-interface to exist on the host.
5. Save.
6. Go to **Devices → [your switch] → Ports** and apply the new profile to the port connected to the matter-server node.

The node's DHCP-assigned IP on VLAN 30 is unaffected. The node gets no new IP — only the pod will have a VLAN 40 address.

> **Only one switch port change needed.** Only the node that runs matter-server requires this.
> All other nodes stay on their single-VLAN access port.

### 5.2 Firewall rules

Go to **Settings → Firewall & Security → Rules → LAN**.

#### Rule 1 — Flannel overlay (bidirectional VLAN 30 ↔ VLAN 40)

Flannel uses VXLAN (UDP 8472) for the pod overlay. Both directions are needed so pods can
communicate across the node/VLAN boundary.

| Field | Value |
|---|---|
| Name | `Allow Flannel VXLAN VLAN30↔VLAN40` |
| Rule type | LAN In (create once per direction, or use bidirectional if supported) |
| Protocol | UDP |
| Port | `8472` |
| Action | Accept |

> If your K3s uses WireGuard for the Flannel backend (`--flannel-backend=wireguard-native`),
> also allow UDP `51820`.

#### Rule 2 — kubelet health checks (VLAN 30 control plane → VLAN 40 node)

The K3s masters need to reach the kubelet on the trunk node to verify pod health.

| Field | Value |
|---|---|
| Name | `Allow kubelet from Servers` |
| Source | `192.168.30.0/24` |
| Destination | trunk node IP (e.g. `192.168.30.10`) |
| Protocol | TCP |
| Port | `10250` |
| Action | Accept |

#### Rule 3 — Matter WebSocket: VLAN 30 → VLAN 40 pod IP

Home Assistant (VLAN 30) must reach matter-server's VLAN 40 pod IP on port 5580.
This is needed if you configure HA to use the pod's VLAN 40 IP directly instead of the
in-cluster K8s service DNS (see §8.3).

| Field | Value |
|---|---|
| Name | `Allow Matter WebSocket from Servers` |
| Source | `192.168.30.0/24` |
| Destination | matter-server VLAN 40 pod IP (e.g. `192.168.40.51`) |
| Protocol | TCP |
| Port | `5580` |
| Action | Accept |

> This rule is **optional** if HA uses the K8s in-cluster DNS name
> `ws://matter-server-service.smarthome.svc.cluster.local:5580/ws`, because that traffic
> stays entirely within the Flannel overlay (VLAN 30 → VLAN 30 via eth0).

#### Rule 4 — Longhorn storage replication (VLAN 40 pod → VLAN 30 nodes)

If Longhorn replicates the matter-server data volume to VLAN 30 nodes:

| Field | Value |
|---|---|
| Name | `Allow Longhorn from matter-server pod` |
| Source | `192.168.40.51` (matter-server VLAN 40 IP) |
| Destination | `192.168.30.0/24` |
| Protocol | TCP |
| Ports | `9500`, `9501-9502` |
| Action | Accept |

### 5.3 Commissioning from Intern VLAN

When commissioning (pairing) a new Matter device, your phone (VLAN 5) needs to reach
matter-server. The Matter commissioning protocol uses TCP 5540.

| Field | Value |
|---|---|
| Name | `Allow Matter commissioning from Intern` |
| Source | `192.168.5.0/24` |
| Destination | matter-server VLAN 40 pod IP (`192.168.40.51`) |
| Protocol | TCP |
| Port | `5540` |
| Action | Accept |

Also allow mDNS from VLAN 5 so the Home Assistant Companion App can find matter-server:

| Field | Value |
|---|---|
| Name | `Allow mDNS from Intern to IoT` |
| Source | `192.168.5.0/24` |
| Destination | `192.168.40.0/24` |
| Protocol | UDP |
| Port | `5353` |
| Action | Accept |

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

### 6.1 Create the VLAN 40 sub-interface

SSH into the target node and create the VLAN sub-interface. It gets **no IP** — the pod
takes the VLAN 40 address via MACVLAN.

```bash
# First, find the actual physical interface name on the node:
ip link show
# Look for the interface with the VLAN 30 IP — e.g. enp1s0, ens3, eth0.
# For node-gn this is enp1s0 (from Ansible inventory k3s_flannel_iface).

# Create the sub-interface (VLAN ID 40)
# Replace enp1s0 with your actual interface name
sudo ip link add link enp1s0 name enp1s0.40 type vlan id 40
sudo ip link set enp1s0.40 up

# Verify
ip link show enp1s0.40
# Should show: enp1s0.40@enp1s0: <BROADCAST,MULTICAST,UP,LOWER_UP>
```

> The sub-interface has no IP address on the host. `ip addr show enp1s0.40` will show no
> `inet` lines. This is correct — the pod takes the VLAN 40 IP via MACVLAN.

### 6.2 Label the trunk-capable node

The matter-server chart uses a **required** node affinity for `vlan40-trunk=true`. Apply this
label to the node where you created the sub-interface:

```bash
# Use the K3s node name (check with: kubectl get nodes)
kubectl label node master-gn vlan40-trunk=true

# Verify
kubectl get node master-gn --show-labels | grep vlan40-trunk
```

Without this label the pod will stay `Pending` with:
```
0/3 nodes are available: 3 node(s) didn't match Pod's node affinity/selector.
```

### 6.3 Persist via systemd-networkd

The sub-interface created in §6.1 is lost on reboot. Make it persistent using
systemd-networkd (or NetworkManager — adapt to your OS setup):

```ini
# /etc/systemd/network/40-vlan40.netdev
[NetDev]
Name=enp1s0.40
Kind=vlan

[VLAN]
Id=40
```

```ini
# /etc/systemd/network/41-vlan40.network
[Match]
Name=enp1s0.40

[Network]
# No IP on the node — MACVLAN takes it.
LinkLocalAddressing=no
```

```bash
sudo systemctl restart systemd-networkd
ip link show enp1s0.40   # should be UP after restart
```

Also set the required IPv6 sysctl for Thread support:

```ini
# /etc/sysctl.d/99-matter-thread.conf
net.ipv6.conf.all.forwarding = 0
net.ipv6.conf.enp1s0.40.accept_ra = 1
net.ipv6.conf.enp1s0.40.accept_ra_rt_info_max_plen = 64
```

```bash
sudo sysctl --system
```

### 6.4 Ansible integration

Add the following to `ansible/inventory/host_vars/<your-node>.yml` (the node you picked):

```yaml
# VLAN 40 sub-interface for Matter (no IP — only the pod gets VLAN 40)
k3s_node_label:
  - kubefledged.io/cache=true
  - type=mini
  - node.longhorn.io/create-default-disk=config
  - vlan40-trunk=true          # ← ADD THIS
```

Add a task to the `node-common` Ansible role (or a new `matter-vlan` role) to create the
systemd-networkd files and apply the sysctl config from §6.3.

---

## 7. Vault Secrets

Before deploying, create these secrets in Vault. The matter-server chart references them
with `<secret:kv/data/smarthome/matter-server~key>`.

```bash
# Vault KV v2: write with `vault kv put kv/<path>` (no /data/ when writing —
# Vault adds it automatically. The <secret:kv/data/...> syntax in values.yaml
# is the API read path, which does include /data/).
vault kv put kv/smarthome/matter-server \
  vlan40-parent-interface="enp1s0.40" \
  vlan40-ip="192.168.40.51" \
  vlan40-gateway="192.168.40.1"
# enp1s0.40  = VLAN 40 sub-interface name on the node (use your actual interface)
# 192.168.40.51 = free IP in VLAN 40 for the pod
# 192.168.40.1  = VLAN 40 default gateway (Unifi UDM/USG IP on VLAN 40)
```

> Adjust the IP values to match your network. Add a DHCP reservation in Unifi for
> `192.168.40.51` so the IP is reserved (even though we set it statically in the CNI config,
> it avoids future conflicts).

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

# Verify the pod has TWO interfaces (eth0 + net1)
kubectl exec -n smarthome <matter-server-pod> -- ip addr show
# You should see:
#   eth0: 10.42.x.x/x  (Flannel)
#   net1: 192.168.40.51/24  (MACVLAN VLAN 40)

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
ws://192.168.40.51:5580/ws
```

This routes through Unifi inter-VLAN (VLAN 30 → VLAN 40), requires firewall Rule 3 from §5.2,
but gives you a stable external endpoint independent of K8s internals.

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
9. Commissioning completes; device appears in HA
```

### Network paths during commissioning

| Step | Protocol | Path |
|---|---|---|
| Phone ↔ Device (BLE pairing) | Bluetooth | Direct, no network |
| HA → matter-server | WebSocket | In-cluster (Flannel) or VLAN 30→40 |
| Phone → matter-server | TCP 5540 | VLAN 5 → VLAN 40 (§5.3 firewall rule) |
| matter-server ↔ Device | mDNS + TCP | VLAN 40 on `net1` (direct, same VLAN) |

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
sysctl net.ipv6.conf.enp1s0.40.accept_ra  # expect: 1

# MUST be 64 — accept route info from border routers for Thread prefixes
sysctl net.ipv6.conf.enp1s0.40.accept_ra_rt_info_max_plen  # expect: 64
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

- **"didn't match node affinity"** → Run `kubectl label node <node> vlan40-trunk=true`
- **PVC not bound** → Check Longhorn is healthy: `kubectl get pvc -n smarthome`
- **Image pull error** → Requires internet access to `ghcr.io`

### Pod starts but net1 is missing

```bash
kubectl exec -n smarthome <pod> -- ip addr show
# If only eth0 is shown:
```

- Verify Multus DaemonSet is running: `kubectl get pods -n kube-system -l app=multus -o wide`
- Verify the NAD exists: `kubectl get net-attach-def -n smarthome`
- Verify the pod annotation is set: `kubectl get pod -n smarthome <pod> -o jsonpath='{.metadata.annotations}'`
- Check Multus logs on the node: `kubectl logs -n kube-system <multus-pod-on-that-node>`

### net1 has no IP / wrong IP

```bash
kubectl exec -n smarthome <pod> -- ip addr show net1
```

- Verify `parentInterface` in Vault matches the actual sub-interface name on the node
- Verify the sub-interface is UP: `ssh node-gn -- ip link show enp1s0.40`
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
- If using VLAN 40 IP: confirm firewall rule §5.2 Rule 3 allows TCP 5580

### Commissioning from phone fails

- Confirm phone (VLAN 5) can reach matter-server TCP 5540 on `192.168.40.51` (§5.3 firewall)
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

- [ ] Switch port for the matter-server node updated to trunk profile (VLAN 30 native + VLAN 40 tagged)
- [ ] DHCP reservation in VLAN 40 for `192.168.40.51` (avoids IP conflicts)
- [ ] Firewall: VLAN 30 ↔ VLAN 40 UDP 8472 (Flannel VXLAN)
- [ ] Firewall: VLAN 30 → trunk node TCP 10250 (kubelet)
- [ ] Firewall: VLAN 5 → `192.168.40.51` TCP 5540 (Matter commissioning from phones)
- [ ] Firewall: VLAN 5 → VLAN 40 UDP 5353 (mDNS from phones)
- [ ] **mDNS repeater DISABLED** on all networks
- [ ] **Multicast Enhancement DISABLED** on IoT WiFi SSID
- [ ] IGMP Snooping reviewed on VLAN 40
- [ ] IoT WiFi SSID: 2.4 GHz, VLAN 40, client isolation OFF, Band Steering OFF

### Node (one-time setup)

- [ ] VLAN 40 sub-interface created: `ip link add link enp1s0 name enp1s0.40 type vlan id 40`
- [ ] systemd-networkd files created for persistence (§6.3)
- [ ] sysctl `99-matter-thread.conf` created and applied (§6.3)
- [ ] Node labelled: `kubectl label node master-gn vlan40-trunk=true`
- [ ] Ansible inventory updated with `vlan40-trunk=true` label (§6.4)

### Vault

- [ ] `kv/data/smarthome/matter-server` with keys: `vlan40-parent-interface`, `vlan40-ip`, `vlan40-gateway`

### Kubernetes / ArgoCD

- [ ] PR merged to `tests` branch
- [ ] Multus Application synced and DaemonSet healthy
- [ ] matter-server Application synced
- [ ] Pod running on trunk node with `net1` interface showing `192.168.40.51`
- [ ] `kubectl exec` confirms `PRIMARY_INTERFACE=net1`
- [ ] Longhorn PVC bound

### Home Assistant

- [ ] Matter integration added with WebSocket URL:
      `ws://matter-server-service.smarthome.svc.cluster.local:5580/ws`
- [ ] Matter integration shows as connected
- [ ] First device commissioned and visible


