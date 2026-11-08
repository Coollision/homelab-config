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

The MQTT broker Zigbee2MQTT, Home Assistant, Shelly Gen1, EMS-ESP and the Moonraker printer talk to. Anonymous
access is off. Auth and ACLs come from Mosquitto's **dynamic-security plugin**. Who may log in and which topics they may touch
is one data file, `workload/smarthome/mosquitto/clients.conf`: an init container (`reconcile.sh`) reapplies the deny-by-default
policy, one role per client (`r-<user>`) with exactly the listed grants, and the users with their Vault passwords on every
start. Grants removed from the file are revoked on the broker; users created at runtime with `mosquitto_ctrl` survive restarts.
ACLs are **enforced** (`acl-learning=false` in `kustomization.yaml`; setting it to `true` gives every role full access and is the
break-glass). The roles were trimmed to a captured usage report (`scripts/mosquitto-acl-usage.sh`, which reads `clients.conf`).
A denied publish is logged (`log_type debug`) and raises the alert `MQTT publish denied`; denied subscribes are not logged by
Mosquitto at all, so a missing read grant shows up as a silent entity: check `clients.conf` first.

**Adding an MQTT client** (two edits, no code):

1. `vault kv patch -mount=kv workload/smarthome/mosquitto <name>-password=<value>` (no spaces in the value).
2. Add a block to `clients.conf`:

   ```
   user <name> <secret:kv/data/workload/smarthome/mosquitto~<name>-password> [readonly]
     rw   <topic>      # read + write
     ro   <topic>      # read only
     send <topic>      # publish only
   ```

   `scripts/mosquitto-clients-lint.sh` (pre-commit + CI) checks the format: known keywords, a Vault placeholder instead of a
   literal password, no bare `#` for writing clients, nothing under `$CONTROL`, at least one grant per user. Retiring a user
   means adding a `retired-user <name>` line (removing a block alone does not delete the user). If the file is invalid or a
   placeholder is unresolved, the init keeps the last applied state and logs an ERROR instead of changing anything.

One plaintext listener (`1883`, exposed as `1880` on the LoadBalancer, `externalTrafficPolicy: Local` so the broker sees
real client IPs). Clients that can do TLS use `mqtt.<internal-domain>:443`: an `IngressRouteTCP` makes Traefik terminate
TLS with its wildcard cert (no dedicated certificate), limited to the admin subnet by a `MiddlewareTCP` allow-list.
Zigbee2MQTT and Home Assistant run in-cluster and use the plaintext service directly.

**Reachability note:** unlike matter-server, the border router, or the mDNS reflector,
Mosquitto has **no dedicated IoT-VLAN MACVLAN leg** — it's exposed via a plain
LoadBalancer Service instead, relying on the network's own inter-VLAN routing for any
IoT-side MQTT client that isn't already cluster-internal. If a new IoT-VLAN-only MQTT
client can't reach it, that routing path — not the pod itself — is the first thing to
check.

**Monitoring:** the Grafana dashboard `Mosquitto` (logs: rejected logins, connects per user and subnet) and the alert
`MQTT unauthorised connection` (any refused login) are log-based. Broker health comes from `mosquitto-exporter`
(`workload/smarthome/mosquitto-exporter`, `sapcc/mosquitto-exporter`): it logs in as the `exporter` user, whose role may
only read `$SYS/#`, and a `ServiceMonitor` scrapes it. Its login lives in Vault (`workload/smarthome/mosquitto-exporter`
`user`/`password`; the broker's `clients.conf` points at the same key) and reaches the pod through a Secret, not env values.

Remaining steps (usage capture, ACL trim, 2.1 upgrade): [TODO — Mosquitto modernization](../06-todo/mosquitto-modernization.md).
