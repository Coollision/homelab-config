# Zigbee

[← Back to Home](../Home.md) · Smarthome: [Home Assistant](home-assistant.md) · [Matter & Thread](matter-thread.md)

Two pieces: Zigbee2MQTT (the Zigbee coordinator software) and Mosquitto (the MQTT broker
it publishes to, which Home Assistant's MQTT integration then subscribes to).

## Zigbee2MQTT — `workload/smarthome/zigbee2mqtt`

Runs pinned to whichever node has the USB Zigbee coordinator stick attached (via the same
[Node Feature Discovery](../01-platform/cluster-housekeeping.md#node-feature-discovery--descheduler)
pattern used for the Thread border router and UPS). No Multus/VLAN legs — it only needs
the USB device and a path to the MQTT broker.

**Z2M has no REST API.** Everything — config, device list, network map — goes over its
WebSocket endpoint. If you're scripting against it, that's the only door in; see the
`zigbee-mesh-debug` skill for ready-made WebSocket tooling. Its `configuration.yaml`
lives on its Longhorn PVC, **not in Git** — Zigbee settings are not GitOps-managed and a
`selfHeal` sync will never touch or revert them.

**Channel plan:** Zigbee's RF channel was deliberately kept fixed even while Thread's
channel was migrated to avoid WiFi overlap (see [Matter & Thread](matter-thread.md)) —
unlike Thread, Zigbee2MQTT has no delay-timer channel migration, and moving it would
force every sleepy end device to re-pair. The two channels ended up numerically adjacent
after Thread's migration; since the two radios run on different physical hosts, this is
expected to be fine, but it's the first thing to suspect if Zigbee degrades unexpectedly.

**Mesh health, in short:** an audit found the network stable overall, with a couple of
long-dead ghost devices that were being pointlessly polled every ~90 seconds — each
failed poll triggered a network-wide route-discovery broadcast, so removing two dead
devices cut scan time by roughly 7x. A handful of energy-monitoring plugs dominate
airtime disproportionately, and a couple of light fixtures have historically weak link
quality. See the `zigbee-mesh-debug` skill for current per-device detail — it changes
over time and isn't worth freezing into this wiki page.

## Mosquitto — `workload/smarthome/mosquitto`

The MQTT broker Zigbee2MQTT (and other MQTT-speaking devices) publish to. Runs with
anonymous access disabled — credentials are assembled at pod startup from several Vault
secrets concatenated into a password file, then hashed in place.

**Reachability note:** unlike matter-server, the border router, or the mDNS reflector,
Mosquitto has **no dedicated IoT-VLAN MACVLAN leg** — it's exposed via a plain
LoadBalancer Service instead, relying on the network's own inter-VLAN routing for any
IoT-side MQTT client that isn't already cluster-internal. If a new IoT-VLAN-only MQTT
client can't reach it, that routing path — not the pod itself — is the first thing to
check.
