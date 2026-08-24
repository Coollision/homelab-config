# Power — NUT (UPS monitoring)

[← Back to Home](../Home.md) · Platform: [Observability](observability.md) · [Cluster housekeeping](cluster-housekeeping.md)

## NUT server — `system/nut/nut`

Network UPS Tools running against a USB-attached UPS. Runs privileged with direct
`/dev/bus/usb` access (a scoped single-device hostPath was tried first and failed — the
UPS re-enumerates on USB hiccups, so it needs full bus access, not a fixed device path).
**Pinned to whichever physical node has the UPS's USB cable plugged in** via a required
node-affinity label set by [Node Feature Discovery](cluster-housekeeping.md#node-feature-discovery--descheduler),
and enforced against accidental drift by the descheduler's hardware-enforcement profile.

Exposes the NUT protocol on a LoadBalancer IP so other clients (e.g. the NAS) can act as
NUT secondaries, plus a `nut_exporter` sidecar feeding Prometheus.

## PeaNUT — `system/nut/peanut`

A simple read-only web dashboard for the NUT server above, internal-only, enrolled in
Sablier scale-to-zero.

## What's *not* built yet

`system/nut/disabled-automation/` documents a **designed-but-not-implemented**
progressive-shutdown plan: on battery, drain the VM worker node and shut down the NAS; on
low battery, drain the remaining nodes and issue the UPS's `load.off` command. Three
trigger mechanisms are sketched (NUT's own `upsmon` notify hook, an Alertmanager webhook,
or a Home Assistant automation) but none are wired up — the NUT server's admin account
already has the permissions this would need ("forward-compatible" provisioning), but
nothing acts on them yet.

## Why NUT is replacing the old SNMP-based UPS metrics

[Observability's home-metrics exporter](observability.md#snmp--ups-metrics--systemmonitoringhome-metrics)
also scrapes UPS-adjacent data via SNMP against the NAS. That path is being retired in
favor of NUT's native metrics once the NUT path is confirmed stable — if you're chasing
a UPS metric, prefer the NUT dashboard over the SNMP one going forward.
