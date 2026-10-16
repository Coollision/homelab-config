# Routed IPTV (Proximus TV)

[← Back to Home](../Home.md) · Platform: [Networking](networking.md) · Related: [Known Issues](../05-operations/known-issues.md#iptv-multicast-never-crosses-the-isp-box-bridge)

The Proximus TV decoders do **not** sit on the ISP box's own LAN anymore. They live on a
UniFi-managed **TV VLAN (120)**, routed by the UDM, in their own firewall zone — so they're
ordinary, controllable members of the home network (Home Assistant control, casting from
phones, Plex from the NAS, filtered DNS) while live TV still works.

Getting there took one long debugging session (2026-10-01 → 10-11). This page is the
steady-state design **plus** the story of why it ended up this shape, because almost every
"obvious" approach was tried and measured first — and most of them don't work.

> **Not in Git.** None of this is Kubernetes: it's UniFi controller config, a small watchdog
> script on the UDM, and a Home Assistant automation. This page is the only place where the
> whole picture is written down.

## The final design

```text
VDSL ─▶ ISP Internet Box ──(bridged internet: public IP)────────▶ UDM WAN1  (DHCP)
                 │
                 └──(box LAN port: TV multicast lives HERE)──────▶ UDM WAN2  (DHCP, failover-only,
                                                                       IGMP proxy upstream)
UDM ──IGMP proxy──▶ TV VLAN 120 (own firewall zone) ──▶ living-room decoder (wired)
                         │
                         └─▶ bedroom switch ─▶ bedroom AP ~~WiFi mesh~~ root AP ─▶ bedroom decoder
                                                 (multicast→unicast on the UDM bridge, see below)
```

| Piece | Setting | Why |
|---|---|---|
| WAN1 | DHCP (was PPPoE), the box bridges the internet session | Normal internet; failover never goes via WAN2 |
| **WAN2** | A spare UDM LAN port re-assigned as WAN2, cabled to an Internet Box LAN port; DHCP from the box; **load-balance: failover-only** | The box only hands TV multicast to devices on **its own LAN side** — this is the only way the UDM can receive it |
| IGMP proxy | Upstream **WAN2**, downstream the TV network; **off** on WAN1 | UniFi allows exactly one IGMP-proxy upstream |
| TV network | VLAN 120, DHCP DNS → the in-house Technitium cluster, DNS-redirect NAT like every other VLAN | Decoders resolve fine via Technitium (no ISP forwarders needed so far) |
| IGMP snooping | **On** for TV, but **no switch querier** (empty querier list), fast-leave off | With a switch querier it broke live TV — see [Known Issues](../05-operations/known-issues.md#igmp-snooping-competing-querier) |
| mDNS | UniFi mDNS proxy in **custom** mode: only `_googlecast._tcp` + `_androidtvremote2._tcp`, between the LAN and TV VLANs | Casting/remote from phones and HA — and never touching the Matter VLAN (see [mDNS reflector lesson](../05-operations/known-issues.md#mdns-reflector-must-not-re-publish)) |
| Decoders | Fixed IPs, listed in a UniFi IP group "Proximus decoders" | Every rule targets the group: a new decoder = fixed IP + add to group |
| **multicast→unicast** | Linux bridge `mcast_to_unicast` on the UDM's TV bridge ports, kept on by a watchdog | Makes live TV survive the bedroom's WiFi-mesh hop — see below |

### Firewall: the TV zone

The TV VLAN has its **own zone** (custom UniFi zones default to *block* in both directions,
except TV → internet and → gateway). Explicit rules:

| Allow | For |
|---|---|
| TV → DNS cluster (port 53, via the DNS IP + port groups) | DHCP + NAT both send decoders to Technitium |
| TV → NAS (Plex ports) and → the parents' site Plex | Plex app on the decoders |
| TV → Home Assistant (direct + via the internal ingress) | HA TTS/media URLs are fetched *by the decoder* |
| TV → LAN, TCP | Casting **local media** from a phone (the decoder fetches from the phone's own HTTP server) |
| LAN → decoders and Servers → decoders, Cast + Android-TV ports (port group) | Phones and HA control the boxes |

Plus copies of the LAN zone's protections, scoped to TV: block public DNS resolvers (v4+v6),
block DoT/DoQ, block the gateway's management ports. UniFi auto-adds "Allow IPTV Streaming
IGMP/Multicast" rules once an IGMP proxy upstream is set — no manual IGMP rule is needed.

Rollback for the zone: move the TV network back into the LAN ("Internal") zone.

### Home Assistant

- **Google Cast** (decoder as media player — TTS works but *takes over the screen*, see the
  overlay plan in HA's own docs) and **Android TV Remote** (power, keys, apps; pairing needs a
  code from the TV screen).
- `automation.infra_network_tv_multicast_watchdog` — the dead man's switch described below.
- Some decoder telemetry endpoints are blocked globally in Technitium (ISP log-shipping and
  Firebase logging); the big trackers were already caught by the general blocklists.

## The bedroom problem: multicast over a WiFi mesh

The bedroom decoder is wired to a small switch that hangs off an AP **without a cable** —
it uplinks via UniFi **wireless meshing**. Live TV there lost quality / stuttered; replay and
recordings (unicast) were fine.

**Measured cause:** WiFi sends multicast as broadcast at the **lowest basic rate** (6 Mbit/s on
5 GHz), without retries. One ~7 Mbit/s TV stream therefore used **~90% of the 5 GHz airtime**
on both mesh APs (the root AP itself transmitting ~85% of the time), packets got lost, the
decoder masked it by degrading the picture.

What was tried, in order — and what it showed:

| Attempt | Result |
|---|---|
| Different mesh parent (better AP, −70 → −66 dBm) | Airtime still ~89% — not a signal problem |
| Raise the SSID's 5 GHz minimum data rate to 24 Mbit/s | **No effect** — the mesh backhaul isn't governed by SSID settings (reverted) |
| IGMP snooping (without switch querier) | Stops *flooding* (the other TV's channel no longer crosses the mesh), but one stream still saturates it |
| Research (Ubiquiti docs/community, ISP forums) | No UniFi setting for multicast on the mesh link; everyone else fixed it with a cable |
| **`mcast_to_unicast` on the UDM's TV bridge** | **Airtime 89% → 22%.** Fixed. |

**What `mcast_to_unicast` does:** the UDM's Linux bridge for the TV VLAN already does IGMP
snooping and knows which decoder (by MAC) joined which channel. With the per-port flag on, it
sends each listener **its own copy with the decoder's unicast MAC** as destination (the IP
packet stays multicast, so the decoder accepts it unchanged). To the WiFi mesh this is normal
unicast: full PHY rate (~400 Mbit/s), with retries — a quarter of the airtime.

Verification trick: `tcpdump -eni <tv-trunk-port> "udp and dst net 239.0.0.0/8"` should show the
decoders' own MACs as destination, not `01:00:5e:…` multicast MACs.

### Keeping it on: the watchdog

UniFi provisioning resets the flag on every reprovision and reboot. So the UDM runs a tiny
watchdog from `/data` (which survives firmware updates):

| File (`/data/custom/tv-m2u/` on the UDM) | Role |
|---|---|
| `tv-m2u.sh` | Every minute: turn `mcast_to_unicast` on for every port of the TV bridge if it's off; log it; POST a heartbeat to an HA webhook every 5 min (and immediately after a re-apply) |
| `tv-m2u.service` / `tv-m2u.timer` | systemd oneshot + 1-minute timer (copied into `/etc/systemd/system`) |
| `install.sh` | Re-installs the units — **run after a UniFi OS firmware update** |
| `uninstall.sh` | Removes the watchdog and turns the flag off again |

Home Assistant (`automation.infra_network_tv_multicast_watchdog`) records each heartbeat in an
`input_datetime`. **No heartbeat for 20 minutes** (typically: the systemd units vanished with a
firmware update) → a persistent **Spook repair** with the one-line fix; the existing
Spook-repair notifier pushes exactly once. Self-repairs after a reprovision are normal and only
go to the logbook. Full details in HA's `docs/automations/infra_network_tv_multicast_watchdog.md`.

## How we got here (the short history)

1. **Start:** decoder on a plain passthrough VLAN to the Internet Box's LAN — worked, but the
   decoder was an island (no HA, no casting, ISP DNS). An earlier attempt at a routed TV VLAN
   "stuttered" and was parked.
2. **Routed attempt, WAN as PPPoE:** nothing usable; the decoder only got a unicast fallback.
3. **WAN switched to DHCP** (the box bridges, the UDM gets the public IP): live TV *started* but
   stuttered — still the unicast fallback. Packet captures on the UDM proved it: the UDM's IGMP
   joins went out (v3, and forced v2), **but not a single multicast packet came back over the
   bridged WAN**. The decoder kept joining and leaving every 1–3 s, then fell back to the ISP's
   unicast fast-channel-change/retransmit service (a few seconds' worth of stream, stretched
   over the whole programme).
4. **Proof of concept on the box LAN side:** a temporary IP on the passthrough bridge + one
   join → the box streamed immediately. Multicast exists only on the box's LAN side.
5. **WAN2 on a spare port** to that LAN side + IGMP proxy upstream there → routed live TV works.
   The `udm-iptv` community package was evaluated and **not** needed (and would have been
   fragile across firmware updates).
6. **Cleanup by one-at-a-time reverts:** QoS and IPS on the TV VLAN were harmless; IGMP snooping
   with the old switch-querier config was the one that broke it.
7. **Hardening:** own firewall zone, DNS via Technitium, telemetry blocks, decoder IP group.
8. **Bedroom over WiFi mesh** → `mcast_to_unicast` + watchdog (above).

## Operating it

- **New decoder:** cable it to a port with native VLAN 120 → give it a fixed IP → add it to the
  "Proximus decoders" IP group. Casting/HA rules apply automatically.
- **After a UniFi OS firmware update:** if HA shows the "watchdog meldt zich niet meer" repair,
  SSH to the UDM and run `sh /data/custom/tv-m2u/install.sh`.
- **Live TV stutters somewhere:** (1) is the decoder getting *multicast*? (switch port counters:
  hundreds of multicast frames/s, not a few) — if not, check WAN2/IGMP proxy; (2) is that path
  wireless? check 5 GHz airtime on the APs in the path and whether `mcast_to_unicast` is still
  on; (3) don't re-enable a switch IGMP querier on the TV VLAN.
- **Moving to fiber some day:** the documented setup applies instead (WAN on the ISP's TV VLAN
  directly, IGMP proxy on WAN1, WAN2 no longer needed).
