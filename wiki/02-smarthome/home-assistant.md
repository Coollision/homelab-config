# Home Assistant

[← Back to Home](../Home.md) · Smarthome: [Matter & Thread](matter-thread.md) · [Zigbee](zigbee.md) · [Voice](voice.md) · [Automation & MCP](automation-mcp.md) · [Database](database.md)

`workload/smarthome/homeassistant` is the hub everything else in this section connects
into. Runs as a StatefulSet with a single VLAN-5 (Intern) MACVLAN leg — it does **not**
have a VLAN 40 (IoT) leg itself; instead it reaches Matter/Thread devices through the
in-cluster matter-server Service and a manually-added IPv6 route (see
[Matter & Thread](matter-thread.md)).

## Networking quirks worth knowing

- An init container adds an IPv6 route to the Thread mesh's OMR prefix via the border
  router's **link-local** address — deliberately not its global address, because the
  global address broke silently across an ISP prefix rotation in production. See
  [Known Issues → surviving prefix rotation](../05-operations/known-issues.md#thread-and-dns-surviving-isp-prefix-rotation).
- The same init container also installs `/32` host routes for a few Sonos speakers on
  VLAN 5, so Sonos's UPnP event listener binds a reachable source address instead of the
  pod's cluster IP — a workaround for the `sbr` chained-CNI-plugin routing table not
  being consulted for non-MACVLAN-sourced traffic.

## Database

The recorder is being migrated off a previously-bundled MariaDB onto the shared
CloudNativePG cluster — see [Smarthome database](database.md) and
[Known Issues → HA recorder migration](../05-operations/known-issues.md#ha-recorder-to-cnpg-migration)
for the full story, including a rendered-manifest leftover file that still shows the old
MariaDB shape and should not be mistaken for current config.

## The `http:` YAML deprecation

Home Assistant 2026.8 deprecated the `http:` block in `configuration.yaml` and
auto-imported it into `.storage` on first boot. That block is what carried
`use_x_forwarded_for` and the `trusted_proxies` CIDRs needed for HA to correctly identify
clients behind Traefik — losing that config would make HA treat Traefik's own IP as
"the client" and ban the reverse proxy after a few failed logins, locking everyone out.
The migration captured it correctly, but the stale YAML block still needs manual removal
before HA 2027.2 makes it a hard error. When debugging proxy/CORS/IP-ban behavior on this
app, check `.storage/http` on the PVC — it's no longer in the YAML you'd grep.

## Voice, Zigbee, Matter/Thread

Home Assistant is the consumer for all of these — see their dedicated pages:
[Voice](voice.md), [Zigbee](zigbee.md), [Matter & Thread](matter-thread.md).
