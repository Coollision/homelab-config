# Matter & Thread

[← Back to Home](../Home.md) · Smarthome: [Home Assistant](home-assistant.md) · [Zigbee](zigbee.md)

This is the most heavily-documented and most incident-prone corner of the whole repo —
worth reading in full before touching any of these three pods. A dedicated
`matter-thread-debug` Claude Code skill and an in-repo `MATTER_INTEGRATION.md` design doc
exist specifically because of how many layers can fail independently here.

## The three pods

| Pod | Path | Role |
|---|---|---|
| **matter-server** | `workload/smarthome/matter-server` | The Matter controller (OHF matter.js server) that HA talks to |
| **thread-border-router** | `workload/smarthome/thread-border-router` | OpenThread Border Router — bridges the Thread mesh to IP, backed by a USB radio stick |
| **mdns-reflector** | `workload/smarthome/mdns-reflector` | Bridges mDNS announcements between the IoT and Intern VLANs |

### matter-server
Two MACVLAN legs: one on the IoT VLAN (mDNS discovery + direct device traffic; this is
what its primary interface binds mDNS to) and one on the Intern VLAN (so phones/laptops
can reach it for commissioning without inter-VLAN routing). Talks to Home Assistant only
over the in-cluster Service DNS name — never over its VLAN IP, to keep VLAN separation
clean.

### thread-border-router
Same two-VLAN pattern, plus a physical USB Thread radio stick passed through via a
privileged hostPath mount. Runs a `postStart` hook that clamps the Thread SRP (Service
Registration Protocol) lease to 15 minutes — this single change is the fix for the
single worst historical failure mode of this stack, explained below. Also runs an init
container that enables IPv6 forwarding in a specific two-step order (per-interface RA
acceptance *before* global forwarding) — reversing that order silently drops the pod's
IPv6 global address.

### mdns-reflector
A **dumb, verbatim L2 packet repeater** — not a re-publishing mDNS responder. That
distinction is the fix for the single worst *root cause* behind mesh outages, also
explained below.

## Why this mesh is fragile, and the two structural fixes that mattered

The Matter fabric hangs almost entirely on a handful of mains-powered Thread router
devices; every other device is a battery-powered sleepy end device that can never route
for others. When a router device dies or restarts, the mesh partitions or forces sleepy
devices to re-parent under new mesh addresses — and here's where it gets non-obvious:

**Fix #1 — the SRP lease clamp.** OpenThread's SRP registry (which is how the border
router knows "device X is at address Y") lives **entirely in RAM**. Every restart of the
border router pod wipes it, and devices only re-register when their SRP lease expires —
by default up to 2 hours. So restarting the border router as a "fix" for an outage used
to *extend* the outage instead of shortening it. Clamping the max lease to 900 seconds
(15 minutes) via a `postStart` hook bounds the worst-case blackout after any border
router restart to 15 minutes.

**Fix #2 — replacing the mDNS reflector implementation.** The original reflector
implementation (a real Avahi responder configured to "reflect") **re-published** every
record under its own identity. This caused the border router's own SRP advertisements to
echo back to it over the network, which it then interpreted as a name conflict and
rejected — meaning that after *any* address churn, devices could never successfully
re-register in SRP again, no matter what was restarted. This was silently causing mass
"unavailable" cascades in Home Assistant. Replacing the Avahi-based reflector with a pure
L2 verbatim repeater (which cannot generate a conflicting response, because it never
generates a response — it only forwards packets byte-for-byte) fixed the root cause
permanently.

**A third, subtler failure mode remains only partially understood:** sleepy end devices
sometimes drop in a *rolling* wave — not simultaneously, not tied to any single router,
and with the backbone routers themselves staying perfectly healthy throughout. The
leading hypothesis after extensive log analysis was 2.4GHz interference on the original
Thread channel, which overlapped a WiFi channel; the mesh was migrated to a
non-overlapping Thread channel as a mitigation. If this rolling-dropout pattern recurs
post-migration, the interference hypothesis is likely wrong and the cause is still open.

## Diagnostic playbook (in order)

1. **Don't touch the border router first.**
2. Check the Thread router table and EID cache for stale/partitioned entries.
3. If devices are attached but Home Assistant shows them unavailable, restart the
   **matter-server** first — it forces a fresh CASE session and re-resolve, and usually
   fixes the problem faster than anything else.
4. Restart the **border router** only for a genuinely wedged partition that doesn't merge
   on its own after a reasonable wait.
5. Physically power-cycle any router device whose mesh entry looks stale, even if it
   otherwise looks fine.
6. Battery-powered stragglers (especially button/remote devices) re-register on their own
   poll cycle — a manual button press or battery reseat forces it immediately.

Full log-query recipes, historical incident timelines, and channel-migration steps live
in the `matter-thread-debug` skill and are intentionally not duplicated here — this page
is the "why," the skill is the "how, right now."
