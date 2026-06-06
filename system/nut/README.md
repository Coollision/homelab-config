# NUT — Network UPS server (APC Back-UPS BX1600MI)

This namespace turns the APC **BX1600MI** plugged into one of the worker nodes into a network-wide UPS
using [Network UPS Tools (NUT)](https://networkupstools.org/). Instead of one machine owning the UPS
over USB, every consumer (the NAS, Prometheus, Home Assistant, future shutdown automation) talks to a
single NUT server over the standard NUT protocol on TCP **3493**.

```
            USB                          NUT proto :3493 (LoadBalancer, Vault kv/nut/nut~lb-ip)
  APC UPS ───────► UPS node   ┌─────────────────┬──────────────┬────────────────┐
                   (worker)   │                 │              │                │
                        ┌─────▼────┐      ┌──────▼────┐   ┌─────▼────┐    ┌──────▼─────┐
                        │  upsd +  │      │    NAS    │   │  PeaNUT  │    │  Home      │
                        │ usbhid   │      │  client   │   │  web UI  │    │  Assistant │
                        │  driver  │      │           │   │          │    │  (later)   │
                        └────┬─────┘      └───────────┘   └──────────┘    └────────────┘
                             │ 127.0.0.1
                        ┌────▼────────┐
                        │ nut_exporter│──► Prometheus ──► Grafana "ups" dashboard
                        │   :9199     │
                        └─────────────┘
```

## What is what

| App | Dir | What it does |
|-----|-----|--------------|
| **nut** | [`nut/`](nut/) | The NUT server. `usbhid-ups` driver reads the APC over USB; `upsd` serves it on `:3493`; a `nut_exporter` sidecar exposes Prometheus metrics on `:9199`. StatefulSet pinned to the UPS node. |
| **peanut** | [`peanut/`](peanut/) | [PeaNUT](https://github.com/Brandawg93/PeaNUT) read-only web dashboard, exposed via an internal Traefik ingress (`ups.<domain>`). Just a viewer — talks to `nut-service:3493`. |

### How the pod follows the hardware
- An **NFD `NodeFeatureRule`** (`feature.node.kubernetes.io/nut-ups-apc`, defined in
  [`../kube-system/node-feature-discovery-descheduler/values.yaml`](../kube-system/node-feature-discovery-descheduler/values.yaml))
  labels whichever node has USB `051d:0002` serial `9B2328A03879` plugged in.
- The `nut` StatefulSet has a **required nodeAffinity** on that label + a hostname pin. Move the UPS to
  another node and (after updating the hostname pin in [`nut/values.yaml`](nut/values.yaml)) the pod
  follows it; the descheduler's `hw-enforcement` profile evicts it if it ever lands on the wrong node.

### USB passthrough — why the whole `/dev/bus/usb` is mounted
The Zigbee/Thread sticks are serial (tty) devices with a stable `/dev/...` node, so they use a udev
`SYMLINK` + a single-node `CharDevice` hostPath. The APC is a **USB-HID** device driven via
**libusb**, which opens `/dev/bus/usb/<bus>/<devnum>` — and the BX1600MI **re-enumerates** (its
devnum changes) whenever USB hiccups. `usbhid-ups` then re-opens the device by scanning the bus.

A scoped single-node approach (init container `mknod`-ing just the APC node into an `emptyDir`) was
tried first and **fails**: the node is frozen at pod-start, so after the first re-enumeration the
driver can't find the new node and the UPS is permanently `Data stale`. So we mount the host's live
`/dev/bus/usb` directly (privileged hostPath). This exposes node-blue's other USB devices (Zigbee /
Thread) to the pod, which is an accepted trade-off for a trusted, in-repo pod. The udev rule (in
[`../../ansible/roles/node-common/templates/50-usb.rules`](../../ansible/roles/node-common/templates/50-usb.rules))
just sets perms on the host.

## Config & secrets

The `instantlinux/nut-upsd` image generates `ups.conf` / `upsd.conf` / `upsmon.conf` from env vars
(see [`nut/values.yaml`](nut/values.yaml)). We override **only `upsd.users`** (mounted at
`/etc/nut/local/upsd.users`) to define two accounts:

| User | Password (Vault `kv/nut/nut`) | Role | Used by |
|------|-------------------------------|------|---------|
| `admin` | `admin-pass` | `upsmon primary` + `instcmds ALL` + `actions SET` | the container's own `upsmon`, `nut_exporter`, PeaNUT, future HA controls |
| `monuser` | `monuser-pass` (= `secret`) | `upsmon secondary` | the NAS (see below) |

The LoadBalancer IP and both passwords live in Vault at `kv/nut/nut`.

## NAS as a client

The NAS no longer owns the UPS over USB — it becomes a NUT **client**:

1. DSM → **Control Panel → Hardware & Power → UPS**
2. Enable UPS support, type = **"Synology UPS server"** (this is DSM's name for a NUT secondary)
3. **NUT server IP** = the LoadBalancer IP (Vault `kv/nut/nut~lb-ip`)
4. DSM hardcodes the UPS name to `ups`, user `monuser`, password `secret` — which is exactly why our
   `upsd.users` defines `monuser`/`secret` as `upsmon secondary`.

DSM will then show online / on-battery / low-battery and shut itself down on low battery.

> The legacy NAS **SNMP** UPS scrape (`synology_ups*` in
> [`../monitoring/home-metrics`](../monitoring/home-metrics)) and its Grafana panels are kept for now.
> Once the NUT path is confirmed healthy, drop that scrape and the SNMP panels.

## Monitoring

`nut_exporter` (sidecar) → Prometheus (via the ServiceMonitor in
[`nut/templates/servicemonitor.yaml`](nut/templates/servicemonitor.yaml), scraped at
`/ups_metrics?ups=ups`, labelled `instance="apc-bx1600mi"`) → the **`ups`** Grafana dashboard, which
now has an "APC BX1600MI — NUT" row (`network_ups_tools_*`) next to the legacy SNMP row.

Useful PromQL: `network_ups_tools_battery_charge`, `network_ups_tools_battery_runtime`,
`network_ups_tools_ups_load`, `network_ups_tools_input_voltage`,
`network_ups_tools_ups_status{flag="OB"}` (1 = on battery).

## Future: graceful shutdown automation

The endgame: as the battery drains during an outage, progressively shed load so the rest of the
homelab survives longer — drain the VM worker and power down the NAS first, then everything else as
the battery gets critical.

This is **not built yet**. The design notes and where the code will live are in
[`disabled-automation/README.md`](disabled-automation/README.md) (named `disabled-` so the
ApplicationSet ignores it until it becomes a real app). The current config is forward-compatible:
`admin` already has `instcmds ALL` + `actions SET`, and `upsmon` is already running in the pod (its
`NOTIFYCMD` is the natural hook for triggering the drain sequence).
