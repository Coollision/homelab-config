# Matter Integration Guide — Homelab with Unifi & VLANs

This document explains everything you need to know to run the **OHF Matter.js Server** alongside
**Home Assistant** in a Kubernetes homelab that uses a **full Unifi network stack** with
**separate VLANs per network segment**.

---

## Table of Contents

1. [What is Matter and why does networking matter?](#1-what-is-matter-and-why-does-networking-matter)
2. [The VLAN Problem — Why a standard K8s deployment breaks Matter](#2-the-vlan-problem)
3. [The Solution — Dedicated IoT-VLAN node in K8s (no trunking)](#3-the-solution)
4. [Network Architecture Overview](#4-network-architecture-overview)
5. [Unifi Configuration — Step by Step](#5-unifi-configuration)
   - 5.1 [Switch port profile for node-r](#51-switch-port-profile-for-node-r)
   - 5.2 [Firewall rules — VLAN 40 ↔ VLAN 30](#52-firewall-rules)
   - 5.3 [Firewall rules — VLAN 5 (Intern) → Matter commissioning](#53-firewall-rules-for-commissioning-from-intern-vlan)
   - 5.4 [Settings to DISABLE for Matter to work](#54-settings-to-disable)
   - 5.5 [WiFi IoT SSID / device onboarding](#55-wifi-iot-ssid--device-onboarding)
6. [Node Migration — Moving node-r to VLAN 40](#6-node-migration)
   - 6.1 [Update Ansible inventory](#61-update-ansible-inventory)
   - 6.2 [Label the IoT node in Kubernetes](#62-label-the-iot-node-in-kubernetes)
7. [Kubernetes Deployment](#7-kubernetes-deployment)
   - 7.1 [Vault secrets to create](#71-vault-secrets-to-create)
   - 7.2 [Deploy from tests branch](#72-deploy-from-tests-branch)
   - 7.3 [Home Assistant configuration](#73-home-assistant-configuration)
8. [Commissioning a Matter device](#8-commissioning-a-matter-device)
9. [Thread devices and Thread Border Routers](#9-thread-devices-and-thread-border-routers)
10. [MetalLB — IP pool for VLAN 40 (optional)](#10-metallb-ip-pool-for-vlan-40)
11. [Troubleshooting](#11-troubleshooting)
12. [Moving to production (workload branch)](#12-moving-to-production)

---

## 1. What is Matter and why does networking matter?

**Matter** (formerly Project CHIP) is an open-source smart home connectivity standard backed by
Apple, Google, Amazon, and the CSA. It is designed to make IoT devices interoperable across
ecosystems.

Underneath Matter, the transport protocol is **IP-based** — both IPv4 and IPv6. However, device
**discovery** relies on:

| Protocol | Purpose | Network behaviour |
|---|---|---|
| **mDNS** (multicast DNS, RFC 6762) | Discover Matter devices on the network | IPv4 multicast `224.0.0.251` / IPv6 `ff02::fb`, **link-local only** |
| **IPv6 link-local** | Device addressing and commissioning | `fe80::/10`, **link-local only** |
| **ICMPv6 / NDP** | IPv6 neighbour discovery, Thread routing | Link-local |
| **BLE** | Initial commissioning (pairing) | Bluetooth range, no network needed |
| **TCP/UDP unicast** | Ongoing device control after commissioning | Routable across VLANs |

The critical property of **link-local** protocols is that **they do not cross router boundaries**.
An mDNS packet sent on VLAN 40 will never reach VLAN 30 on its own — routers (including your
Unifi gateway) drop link-local multicast by design.

This means:

> **The machine running matter-server must be on the same Layer-2 network (VLAN) as the Matter
> devices it controls.**

Once a device is *commissioned* (paired), Matter control traffic uses regular unicast TCP/UDP and
**can** cross VLANs via your Unifi router — but discovery and re-commissioning cannot.

---

## 2. The VLAN Problem

Your homelab uses three VLANs:

| VLAN | ID | Subnet | Residents |
|---|---|---|---|
| Servers | 30 | 192.168.30.0/24 | K8s nodes (node-gn, node-s), Traefik, HA |
| IoT | 40 | 192.168.40.0/24 | Smart home devices, Thread border routers |
| Intern | 5 | 192.168.5.0/24 | Computers, phones, tablets |

**Without any changes**, deploying matter-server as a regular K8s pod means:

```
[Matter device — VLAN 40]  --mDNS multicast--> [VLAN 40 switch]
                                                       |
                                              (router drops multicast)
                                                       |
                                        [VLAN 30 — K8s pod] ← never arrives
```

The pod never sees the mDNS announcements from devices on VLAN 40, so commissioning fails and
devices cannot be discovered.

### Why you cannot just use Unifi's built-in mDNS repeater

Unifi Network has a built-in "mDNS" feature (Settings → Networks → [network] → Advanced →
**mDNS**). This acts as a Layer-7 proxy that re-broadcasts mDNS packets across VLANs.

**Do NOT enable this for Matter.** The official Matter.js documentation explicitly states:

> *"Also do not enable any mdns forwarders on the network (the option is called mDNS on Unifi for
> example) as they tend to corrupt or severely hinder the Matter packets on the network."*

The Unifi mDNS proxy modifies packet timing and TTLs in ways that break the Matter protocol's
commissioning flow.

### Why not add a VLAN trunk to a K8s node?

VLAN trunking (tagging multiple VLANs on one switch port so the node has one interface per VLAN)
is technically possible but adds significant complexity:

- The Linux host needs sub-interfaces (e.g. `enp1s0.40`) for each tagged VLAN
- Kubernetes networking (Flannel/CNI) needs to be aware of the extra interfaces
- Security posture is reduced: a single compromised node can reach multiple network segments
- Multus CNI could attach a VLAN-40 interface to the pod directly, but still requires trunk config

This approach is **not recommended** for a clean homelab architecture.

---

## 3. The Solution

**One node per VLAN. No trunking. No mDNS proxy.**

The approach used in this repository is:

1. **Move `node-r` (Raspberry Pi) from VLAN 30 to VLAN 40 (IoT).**
   The RPi becomes the dedicated IoT-network Kubernetes worker. It has exactly ONE network
   interface on exactly ONE VLAN — the IoT VLAN. No trunking required.

2. **Deploy `matter-server` exclusively to `node-r` using a node affinity label.**
   With `hostNetwork: true`, the container uses the node's real network stack. It can see all
   VLAN 40 multicast traffic natively — no proxying needed.

3. **K3s cluster connectivity is maintained via Unifi inter-VLAN routing.**
   The K3s API server (VLAN 30) and the worker node (VLAN 40) communicate through the Unifi
   gateway via unicast TCP. Flannel overlay tunnels (VXLAN/WireGuard) work over routed unicast.

4. **Home Assistant (VLAN 30) connects to matter-server (VLAN 40) via inter-VLAN routing.**
   The WebSocket endpoint (`ws://192.168.40.x:5580/ws`) is a plain TCP connection that the
   Unifi gateway forwards from VLAN 30 to VLAN 40 — no special protocol treatment needed.

### Why this is clean

| Property | Result |
|---|---|
| Each server on exactly one VLAN | ✅ No trunking anywhere |
| mDNS works natively | ✅ node-r is on VLAN 40 with all IoT devices |
| No Unifi mDNS proxy needed | ✅ No packet corruption |
| K8s cluster stays intact | ✅ Flannel/unicast works across VLANs |
| HA → matter-server | ✅ Plain TCP via inter-VLAN routing |

---

## 4. Network Architecture Overview

```
┌─────────────────────────────────────────────────────────────────────────────┐
│  Unifi Gateway (UDM / USG)                                                  │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐                       │
│  │  VLAN 5      │  │  VLAN 30     │  │  VLAN 40     │                       │
│  │  Intern      │  │  Servers     │  │  IoT         │                       │
│  │  192.168.5.x │  │  192.168.30.x│  │  192.168.40.x│                       │
│  └──────┬───────┘  └──────┬───────┘  └──────┬───────┘                       │
│         │                 │                  │                               │
│         └──── inter-VLAN routing (unicast TCP/UDP) ────────────────────────┘
│
│  Firewall rules (see §5.2):                                                 │
│  VLAN 40 → VLAN 30: allow TCP 6443 (K3s API)                               │
│  VLAN 30 → VLAN 40: allow TCP 5580 (Matter WebSocket)                      │
│  VLAN  5 → VLAN 40: allow TCP 5540 (Matter protocol / commissioning)       │
│  VLAN  5 → VLAN 40: allow UDP 5353 (mDNS — for phone commissioning)        │
└─────────────────────────────────────────────────────────────────────────────┘

┌──────────────────────────┐     ┌──────────────────────────┐
│  VLAN 30 — Servers       │     │  VLAN 40 — IoT           │
│                          │     │                          │
│  node-gn (mini)          │     │  node-r (RPi)            │
│  ├── homeassistant       │     │  └── matter-server       │
│  └── homeassistant-db    │     │      hostNetwork: true   │
│                          │     │      port 5580 (WS)      │
│  node-s (vm/master)      │     │                          │
│  └── K3s control plane   │     │  IoT devices             │
│                          │     │  ├── Matter over WiFi    │
│  Traefik (LoadBalancer)  │     │  ├── Matter over Thread  │
│  Vault, ArgoCD, etc.     │     │  └── Thread Border Router│
└──────────┬───────────────┘     └──────────┬───────────────┘
           │                                │
           │  HA → matter-server            │
           │  ws://192.168.40.x:5580/ws     │
           │  (TCP via Unifi routing)       │
           └────────────────────────────────┘
```

---

## 5. Unifi Configuration

Log in to **Unifi Network** (not UniFi OS). All steps are in the **Network** application.

### 5.1 Switch port profile for node-r

node-r is currently connected to a switch port configured as **VLAN 30 (untagged)**. You need to
change it to **VLAN 40 (untagged)**.

**Steps:**
1. Go to **Devices** → select your Unifi switch.
2. Click the port that `node-r` / Raspberry Pi is connected to.
3. Under **Port Profile**, change from your VLAN-30 profile to your **VLAN-40 (IoT)** profile.
4. Apply. The RPi will lose its current IP and request a new one from the VLAN 40 DHCP pool.

> **Important:** Have a DHCP reservation ready in VLAN 40 for the RPi's MAC address so it always
> gets the same IP (e.g. `192.168.40.50`). You'll need this IP later for HA configuration and
> for Ansible inventory updates.

> **Note:** The RPi will briefly drop out of the K8s cluster when its IP changes. K3s will
> automatically reconnect once it gets its new VLAN 40 IP. Flannel (overlay) adapts to the new
> IP automatically. Expect 1–3 minutes of node `NotReady` status.

### 5.2 Firewall rules

Go to **Settings → Firewall & Security → Rules**.

Create the following rules. Unifi rule direction is always from the perspective of the **source
network**.

#### Rule 1 — Allow K3s API (VLAN 40 worker → VLAN 30 control plane)

| Field | Value |
|---|---|
| Name | `Allow K3s API from IoT` |
| Rule type | LAN In |
| Source network | VLAN 40 (IoT) |
| Source address | `192.168.40.0/24` |
| Destination network | VLAN 30 (Servers) |
| Destination address | K3s master IPs (e.g. `192.168.30.10, 192.168.30.11`) |
| Protocol | TCP |
| Port | `6443` |
| Action | Accept |

#### Rule 2 — Allow Flannel overlay (bidirectional VLAN 30 ↔ VLAN 40)

Flannel uses VXLAN (UDP 8472) or WireGuard (UDP 51820) for the overlay network. Both directions
are needed.

| Field | Value |
|---|---|
| Name | `Allow Flannel Overlay VLAN30↔VLAN40` |
| Rule type | LAN In (create two rules, one per direction) |
| Protocol | UDP |
| Port | `8472` (VXLAN) **and** `51820` (WireGuard, if enabled) |
| Action | Accept |

To find which Flannel backend K3s is using:

```bash
kubectl get cm -n kube-system k3s -o yaml | grep backend
# or
kubectl get nodes -o wide  # check flannel annotations
```

#### Rule 3 — Allow Matter WebSocket (VLAN 30 → VLAN 40)

Home Assistant (VLAN 30) needs to reach matter-server (VLAN 40) on port 5580.

| Field | Value |
|---|---|
| Name | `Allow Matter WebSocket from Servers` |
| Rule type | LAN In |
| Source network | VLAN 30 (Servers) |
| Destination address | `192.168.40.x` (node-r VLAN 40 IP) |
| Protocol | TCP |
| Port | `5580` |
| Action | Accept |

#### Rule 4 — Allow Longhorn storage replication (VLAN 30 ↔ VLAN 40)

Longhorn replicates data between nodes. If matter-server's Longhorn volume needs replicas
accessible from VLAN 30 nodes:

| Field | Value |
|---|---|
| Name | `Allow Longhorn from IoT node` |
| Protocol | TCP |
| Ports | `9500` (Longhorn manager), `9501-9502` (replica sync) |
| Source | `192.168.40.0/24` |
| Destination | `192.168.30.0/24` |
| Action | Accept |

#### Rule 5 — Allow kubelet and metrics (VLAN 30 → VLAN 40)

The K3s control plane needs to reach worker kubelet for health checks and metrics.

| Field | Value |
|---|---|
| Name | `Allow kubelet from Servers to IoT node` |
| Protocol | TCP |
| Port | `10250` (kubelet), `10255` (read-only metrics) |
| Source | `192.168.30.0/24` |
| Destination | `192.168.40.x` (node-r) |
| Action | Accept |

### 5.3 Firewall rules for commissioning from Intern VLAN

When you commission (pair) a new Matter device using your phone (VLAN 5), the phone and the
matter-server need to communicate. The commissioning flow is:

```
Phone (VLAN 5) --BLE--> IoT device (pairing starts)
Phone (VLAN 5) --TCP 5540--> matter-server (VLAN 40) ← commissioning handshake
```

| Field | Value |
|---|---|
| Name | `Allow Matter commissioning from Intern` |
| Rule type | LAN In |
| Source network | VLAN 5 (Intern) |
| Destination | `192.168.40.x` (node-r / matter-server IP) |
| Protocol | TCP |
| Port | `5540` (Matter protocol default) |
| Action | Accept |

Also allow mDNS from VLAN 5 so the Home Assistant Companion App and Apple Home can discover
the Matter server:

| Field | Value |
|---|---|
| Name | `Allow mDNS from Intern to IoT` |
| Rule type | LAN In |
| Source network | VLAN 5 |
| Destination | VLAN 40 |
| Protocol | UDP |
| Port | `5353` |
| Action | Accept |

> **Note:** This is different from enabling Unifi's mDNS *repeater*. We are allowing direct UDP
> port 5353 unicast/directed traffic, not enabling a proxy that rewrites multicast packets.

### 5.4 Settings to DISABLE

These Unifi settings will break Matter or degrade it significantly. Disable them.

#### ❌ Disable mDNS (Unifi built-in repeater)

`Settings → Networks → [each network] → Advanced → mDNS`

Set to **OFF** on ALL networks, especially VLAN 40 (IoT). The Unifi mDNS proxy corrupts Matter
packets and interferes with commissioning.

#### ❌ Disable Multicast Enhancement / Multicast Optimization

This setting is typically found under:
`WiFi → [your IoT SSID] → Advanced → Multicast Enhancement` (also called
"Multicast Optimization" in older firmware)

Set to **OFF**. This feature converts multicast to unicast at the AP level, which destroys mDNS
and Matter's multicast discovery.

#### ❌ Check IGMP Snooping

`Settings → Networks → [VLAN 40] → Advanced → IGMP Snooping`

If IGMP Snooping is ON, it may cause mDNS to only reach devices that have explicitly subscribed
to the multicast group. Matter devices often do not send proper IGMP joins. Options:

- **Preferred:** Leave IGMP Snooping **ON** but also enable **IGMP Proxy** if your switch
  supports it (this handles the subscription issue).
- **Alternatively:** Turn IGMP Snooping **OFF** on VLAN 40. This floods multicast to all ports
  which is acceptable for a home IoT network.

#### ❌ Disable "Block LAN to WAN multicast and broadcast" (if shown)

Some Unifi firmware versions show this under Advanced Gateway Settings. Ensure it does not block
intra-VLAN multicast on VLAN 40.

### 5.5 WiFi IoT SSID / device onboarding

Matter over WiFi devices need to join a WiFi network. They should join a 2.4 GHz SSID that is
**tagged to VLAN 40**:

1. In **WiFi**, create (or use) an IoT SSID (e.g. `HomeIoT`).
2. Set the **Network** for this SSID to your **VLAN 40** network.
3. Ensure it broadcasts on **2.4 GHz** (many Matter devices are 2.4 GHz only).
4. Keep **Band Steering** OFF for the IoT SSID to prevent devices being pushed to 5 GHz.
5. Keep **Client Isolation** OFF (devices on the same SSID need to talk to each other and to the
   VLAN gateway).

When commissioning, your phone (VLAN 5) uses BLE to tell the device which SSID/password to use.
The device then connects to the IoT SSID and appears on VLAN 40 where matter-server can see it.

---

## 6. Node Migration

### 6.1 Update Ansible inventory

Your actual inventory is in `ansible/inventory/` (gitignored). Reflect these changes in both
your real inventory and in `ansible/inventory_example/host_vars/node-r.yml` as documentation.

**Changes to `host_vars/node-r.yml`:**

```yaml
# Before:
host_ip_address: 192.168.30.XX   # old VLAN 30 IP

# After:
host_ip_address: 192.168.40.50   # new VLAN 40 IP (use your DHCP reservation)
dns_record_name: node-r.pi
dns_record_value: "{{ host_ip_address }}"

k3s_node_name: worker-r
k3s_node_label:
  - kubefledged.io/cache=true
  - type=rpi
  - iot-network=true              # ← ADD THIS LABEL
```

The `iot-network=true` label is what the `matter-server` chart uses in its **required** node
affinity to ensure the pod only schedules on the IoT VLAN node.

**After updating the switch port (§5.1) and the node gets a new IP:**

```bash
# SSH to the RPi using its new VLAN 40 IP
ssh homelab@192.168.40.50

# Confirm the node joined the cluster
kubectl get nodes -o wide
# You should see worker-r with an address in 192.168.40.x

# Add the iot-network label (one-time, or via Ansible on next run)
kubectl label node worker-r iot-network=true
```

### 6.2 Label the IoT node in Kubernetes

The matter-server chart has a **required** node affinity for `iot-network=true`. Without this
label on the target node, the pod will stay in `Pending` forever with:

```
0/3 nodes are available: 3 node(s) didn't match Pod's node affinity/selector.
```

Apply the label before or immediately after deploying:

```bash
kubectl label node worker-r iot-network=true
```

Verify:
```bash
kubectl get node worker-r --show-labels | grep iot-network
```

---

## 7. Kubernetes Deployment

### 7.1 Vault secrets to create

Before deploying, create the following secrets in Vault. The chart references them using the
`<secret:kv/data/...~key>` syntax.

```bash
# Set the Matter server hostname (used for Traefik ingress)
# These reference secrets already defined for other services and need no new entries.

# No new Vault secrets are strictly required for the tests deployment.
# The ingress uses the existing domains secrets already in Vault.
# If you want to pin the WebSocket endpoint to a MetalLB IP, add:

vault kv patch kv/data/smarthome/matter-server \
  ip=192.168.40.51     # choose a free IP in your VLAN 40 range for MetalLB
```

> If you do NOT set a MetalLB `serviceIp`, matter-server will use a ClusterIP service only.
> Home Assistant can still reach it directly via the node's VLAN 40 IP (`192.168.40.50:5580`)
> since `hostNetwork: true` binds port 5580 to the node's real IP.

### 7.2 Deploy from tests branch

The `tests` ApplicationSet (in `applications/set-tests.yaml`) reads from the **`tests` git
branch**. This PR targets the current branch — when merged to the `tests` branch, ArgoCD will
automatically pick up the `tests/smarthome/matter-server/` chart and deploy it.

```bash
# Verify ArgoCD picked up the new application
kubectl get application matter-server -n argocd

# Watch the sync
kubectl get application matter-server -n argocd -w

# Check pod status
kubectl get pods -n smarthome -l app.kubernetes.io/name=matter-server

# Make sure it scheduled on the IoT node
kubectl get pod -n smarthome -o wide | grep matter-server
# Expected: VLAN 40 IP in the NODE column (worker-r / 192.168.40.x)

# Check logs
kubectl logs -n smarthome -l app.kubernetes.io/name=matter-server --tail=50
```

### 7.3 Home Assistant configuration

Once matter-server is running on node-r (VLAN 40), configure HA to use it.

**Option A — Use the node's direct IP (simplest for tests)**

In HA → Settings → Devices & Services → Add Integration → **Matter (BETA)**:
- Set the WebSocket URL to: `ws://192.168.40.50:5580/ws`
  (replace with actual node-r VLAN 40 IP)

**Option B — Use the Kubernetes service hostname (more stable)**

Within the cluster, the service is reachable via:
`ws://matter-server-service.smarthome.svc.cluster.local:5580/ws`

Since HA and matter-server are both in Kubernetes, this in-cluster DNS name works perfectly and
is independent of node IPs. **This is the recommended approach for production.**

**Option C — Use a MetalLB LoadBalancer IP on VLAN 40**

If you added a `serviceIp` in `values.yaml`, HA connects to:
`ws://192.168.40.51:5580/ws` (the MetalLB VIP)

This is reachable from VLAN 30 via Unifi inter-VLAN routing (requires firewall rule from §5.2
Rule 3).

> **After HA connects to matter-server**, the Matter integration will appear in HA. Devices will
> initially show as unavailable until they are commissioned. See §8 for commissioning.

---

## 8. Commissioning a Matter device

Commissioning is the process of pairing a new Matter device to your controller.

### Flow

```
1. Power on the new Matter device (factory reset or first boot)
2. Open HA → Settings → Devices & Services → Matter (BETA) → + Add Device
3. HA generates a commissioning invitation and QR code
4. Scan the QR code with your phone OR enter the setup code
5. Phone communicates with device via BLE (or WiFi AP mode for some devices)
6. Phone sends WiFi credentials (IoT SSID / VLAN 40) to the device
7. Device joins VLAN 40 WiFi
8. matter-server (on VLAN 40 node) discovers device via mDNS and completes commissioning
9. Device appears in HA
```

### Network requirements during commissioning

| Step | Protocol | Path |
|---|---|---|
| Phone → Device (BLE) | Bluetooth | Direct, no VLAN involved |
| HA → matter-server | TCP WebSocket | VLAN 30 → VLAN 40 (Rule 3) |
| Phone → matter-server | TCP 5540 | VLAN 5 → VLAN 40 (§5.3) |
| matter-server → Device | mDNS + TCP | VLAN 40 (same VLAN, direct) |

### Troubleshooting commissioning

If commissioning stalls:
- Confirm matter-server pod is running on the correct node (`kubectl get pod -n smarthome -o wide`)
- Confirm the device joined the IoT WiFi SSID (check Unifi client list for VLAN 40)
- Check matter-server logs: `kubectl logs -n smarthome <matter-server-pod>`
- Ensure `Multicast Enhancement` is OFF on the IoT SSID

---

## 9. Thread devices and Thread Border Routers

**Thread** is a low-power IPv6 mesh network used by many Matter devices (sensors, smart bulbs,
etc.). Thread devices do not connect to WiFi directly — they join a Thread mesh and communicate
via a **Thread Border Router (TBR)**.

### What is a Thread Border Router?

A Thread Border Router bridges the Thread mesh network to your IP network. Common examples:
- **Apple HomePod** (mini or full-size)
- **Apple TV 4K** (3rd gen)
- **Google Nest Hub** (2nd gen)
- **Nanoleaf Thread** device with border router capability

These devices sit on your **VLAN 40 WiFi** and maintain the Thread mesh.

### How Thread affects matter-server

For matter-server to control Thread-based Matter devices, it needs:

1. **IPv6 enabled on node-r** (check: `ip -6 addr show`)
2. **Kernel option `CONFIG_IPV6_ROUTER_PREF` enabled** (check: `zcat /proc/config.gz | grep ROUTER_PREF`)
3. **IPv6 forwarding DISABLED** on the node:
   ```bash
   sysctl net.ipv6.conf.all.forwarding
   # Must be 0 (disabled). IPv6 forwarding prevents RIO processing.
   ```
4. **ICMPv6 Router Advertisements accepted** (for Thread route propagation):
   ```bash
   # Replace eth0 with the actual VLAN 40 interface name on node-r
   sysctl net.ipv6.conf.eth0.accept_ra         # should be 1
   sysctl net.ipv6.conf.eth0.accept_ra_rt_info_max_plen  # should be 64
   ```

### Persistent sysctl settings on node-r

Add to `/etc/sysctl.d/99-matter-thread.conf` on the RPi:

```ini
# Required for Thread Border Router communication via IPv6 NDP RIOs
net.ipv6.conf.all.forwarding = 0
net.ipv6.conf.eth0.accept_ra = 1
net.ipv6.conf.eth0.accept_ra_rt_info_max_plen = 64
```

> Replace `eth0` with the actual interface name. Check with `ip link show` on node-r.

You can add this as a file in an Ansible role (`roles/node-common/`) to apply it automatically
during node provisioning.

### Unifi and Thread

Thread Border Routers on your VLAN 40 WiFi will handle Thread mesh traffic internally. The
main requirement on the Unifi side is:

- **Multicast Enhancement OFF** on the IoT SSID (critical — Thread uses multicast IPv6)
- **IPv6 allowed** from VLAN 40 to pass through (do not block ICMPv6 on VLAN 40)
- If using a Thread device for commissioning, ensure the TBR is on the same VLAN 40 as matter-server

---

## 10. MetalLB — IP pool for VLAN 40

By default, MetalLB in L2 mode announces LoadBalancer IPs via ARP. The ARP announcement is
sent by the MetalLB speaker pod on the **same node as the pod holding the IP**. If
matter-server is on node-r (VLAN 40), MetalLB will announce on VLAN 40 — which means the VIP
will only be reachable from VLAN 40.

To make the matter-server service reachable from VLAN 30 (where HA lives), you have two options:

### Option A — Use the in-cluster DNS name (recommended)

No MetalLB IP needed. HA connects via:
`ws://matter-server-service.smarthome.svc.cluster.local:5580/ws`

Both HA and matter-server are in the same K8s cluster. The Flannel overlay handles routing
between pods regardless of which VLAN the underlying nodes are on. **This just works.**

### Option B — Dedicated MetalLB pool for VLAN 40

If you want a stable external VLAN 40 IP for debugging or external access, create a separate
IP pool scoped to node-r. Add a file at `system/metallb-system/metallb/iot-pool.yaml`:

```yaml
apiVersion: metallb.io/v1beta1
kind: IPAddressPool
metadata:
  name: iot
  namespace: metallb-system
spec:
  addresses:
    - 192.168.40.50-192.168.40.60   # adjust range to fit your network
---
apiVersion: metallb.io/v1beta1
kind: L2Advertisement
metadata:
  name: iot
  namespace: metallb-system
spec:
  ipAddressPools:
    - iot
  nodeSelectors:
    - matchLabels:
        iot-network: "true"          # only announce from node-r
```

Then set `serviceIp: 192.168.40.51` (or use a Vault secret) in `values.yaml`.

---

## 11. Troubleshooting

### Pod stays Pending

```bash
kubectl describe pod -n smarthome <matter-server-pod>
```

Common causes:
- **Node affinity not met**: Ensure `kubectl label node worker-r iot-network=true` was run
- **Longhorn storage**: Ensure Longhorn can provision volumes. Check with `kubectl get pvc -n smarthome`
- **Image pull**: The image `ghcr.io/matter-js/matterjs-server:stable` requires internet access

### Devices not discovered

1. Confirm pod is on node-r: `kubectl get pod -n smarthome -o wide`
2. Confirm node-r is on VLAN 40: `kubectl get node worker-r -o jsonpath='{.status.addresses}'`
3. Check mDNS is working from the pod:
   ```bash
   kubectl exec -n smarthome <matter-server-pod> -- cat /proc/net/dev
   # Look for the host's eth0 (not a virtual interface) — confirms hostNetwork
   ```
4. Check matter-server logs for mDNS scan output:
   ```bash
   kubectl logs -n smarthome <matter-server-pod> | grep -i mdns
   kubectl logs -n smarthome <matter-server-pod> | grep -i discover
   ```
5. Verify `Multicast Enhancement` is OFF on the IoT WiFi SSID in Unifi

### HA cannot connect to matter-server

1. Test TCP connectivity from within the HA pod:
   ```bash
   kubectl exec -n smarthome <homeassistant-pod> -- nc -zv 192.168.40.50 5580
   ```
2. Check Unifi firewall rule (§5.2 Rule 3) allows TCP 5580 from VLAN 30 to VLAN 40
3. Verify matter-server is listening:
   ```bash
   kubectl exec -n smarthome <matter-server-pod> -- ss -tlnp | grep 5580
   ```

### K3s node NotReady after IP change

After moving node-r to VLAN 40, the node needs to re-announce its new IP:

```bash
# On node-r (new VLAN 40 IP):
sudo systemctl restart k3s-agent

# On a master node:
kubectl get nodes -o wide
# Should show worker-r with 192.168.40.x IP within 1-2 minutes
```

If the node stays NotReady, check:
```bash
# On node-r:
sudo journalctl -u k3s-agent -f

# Common issue: K3s API server unreachable
# Check firewall rule §5.2 Rule 1 allows TCP 6443 from VLAN 40 → VLAN 30
```

### Commissioning fails / times out

- Ensure phone (VLAN 5) can reach matter-server TCP 5540 on VLAN 40 (§5.3)
- Disable any VPN on the phone during commissioning
- Check the device is on the correct IoT WiFi SSID (VLAN 40)
- Try resetting the device to factory defaults and retry

### IPv6 / Thread issues

```bash
# On node-r, verify IPv6 is enabled and properly configured:
ip -6 addr show
sysctl net.ipv6.conf.all.forwarding      # expect 0
sysctl net.ipv6.conf.eth0.accept_ra      # expect 1

# Check ICMPv6 is not blocked on VLAN 40 in Unifi firewall
```

---

## 12. Moving to production

Once matter-server works correctly from the `tests` branch, follow the instructions in
`tests/README.md` to migrate it to `workload/smarthome/matter-server/` on the `master` branch
without downtime.

**Quick steps:**

```bash
# 1. Copy tests/smarthome/matter-server → workload/smarthome/matter-server on master

# 2. Remove ArgoCD ownership from tests ApplicationSet:
kubectl patch application matter-server -n argocd --type=json \
  -p='[{"op": "remove", "path": "/metadata/ownerReferences"}]'

# 3. workloads ApplicationSet adopts it automatically (~3 minutes)

# 4. Verify:
kubectl get application matter-server -n argocd \
  -o jsonpath='{.metadata.ownerReferences[0].name}'
# Should output: workloads

# 5. Rename or delete tests/smarthome/matter-server on the tests branch:
git checkout tests
git mv tests/smarthome/matter-server tests/smarthome/disabled-matter-server
git push origin tests
```

---

## Summary Checklist

### Unifi

- [ ] DHCP reservation for node-r MAC address in VLAN 40 (e.g. `192.168.40.50`)
- [ ] Switch port for node-r changed from VLAN 30 → VLAN 40 profile
- [ ] Firewall rule: VLAN 40 → VLAN 30 TCP 6443 (K3s API)
- [ ] Firewall rule: VLAN 30 → VLAN 40 TCP 5580 (Matter WebSocket from HA)
- [ ] Firewall rule: VLAN 5 → VLAN 40 TCP 5540 (Matter commissioning from phones)
- [ ] Firewall rule: VLAN 5 → VLAN 40 UDP 5353 (mDNS for phone discovery)
- [ ] Firewall rule: bidirectional VLAN 30 ↔ VLAN 40 UDP 8472 (Flannel overlay)
- [ ] Firewall rule: VLAN 30 → VLAN 40 TCP 10250 (kubelet healthchecks)
- [ ] **mDNS repeater DISABLED** on all networks
- [ ] **Multicast Enhancement DISABLED** on IoT WiFi SSID
- [ ] IGMP Snooping reviewed on VLAN 40 (OFF or proxy enabled)
- [ ] IoT WiFi SSID is 2.4 GHz, tagged to VLAN 40, client isolation OFF

### Node / Ansible

- [ ] Ansible inventory updated: node-r IP → `192.168.40.50` (or your reserved IP)
- [ ] Label `iot-network=true` added to `k3s_node_label` in node-r host_vars
- [ ] `kubectl label node worker-r iot-network=true` applied
- [ ] IPv6 sysctl settings applied on node-r (`/etc/sysctl.d/99-matter-thread.conf`)

### Kubernetes / ArgoCD

- [ ] Vault secrets ready (domains secret already exists)
- [ ] PR merged to `tests` branch
- [ ] `matter-server` Application appears in ArgoCD and syncs
- [ ] Pod is `Running` on `worker-r` (VLAN 40 node)
- [ ] Storage PVC bound (Longhorn)

### Home Assistant

- [ ] Matter integration added in HA with WebSocket URL:
      `ws://matter-server-service.smarthome.svc.cluster.local:5580/ws`
- [ ] HA shows Matter integration as connected
- [ ] First device commissioned and visible in HA
