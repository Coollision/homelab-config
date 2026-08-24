# ESPHome

[← Back to Home](../Home.md) · Smarthome: [Home Assistant](home-assistant.md) · See also: the `esphome-device-ops` skill

`workload/smarthome/esphome` runs the ESPHome dashboard — the firmware build/flash/manage
UI for ESP32/ESP8266-based devices. There's deliberately **no ESPHome MCP server** for
Claude Code to drive; all device operations go through `kubectl exec` into this pod plus
the dashboard's own JSON API. See the `esphome-device-ops` skill for the actual commands.

## Networking: mDNS, not ping

The dashboard is configured to discover devices via mDNS rather than ping, because ping
can't cross VLANs and multicast otherwise wouldn't reach IoT-VLAN devices from a
cluster-internal pod. This means the pod carries a MACVLAN leg on the IoT VLAN — and,
critically, that leg **must** use the `sbr` chained CNI plugin (see
[Networking](../01-platform/networking.md#multus--systemkube-systemmultus)). Without it,
the IoT VLAN's DHCP-assigned default route hijacks the pod's egress, and firmware
compiles start failing with DNS resolution errors for the exact packages they need to
download — a genuinely confusing failure mode if you don't already know to check routing
first.

## Resource sizing

The dashboard's device-catalog rewrite in a past ESPHome release roughly doubled its idle
memory footprint and made firmware-compile memory spikes much larger; a memory limit that
was fine for the old dashboard will OOMKill the new one on startup, presenting as a
"WebSocket not connected" error in the browser with no obvious cause. If you ever see
that symptom after an ESPHome version bump, check for an OOMKill before debugging
anything else.

## Scale-to-zero

Enrolled in Sablier with a deliberately long session duration (hours, not minutes) — so a
batch of firmware compiles and OTA pushes can run to completion without the pod scaling
down mid-job.
