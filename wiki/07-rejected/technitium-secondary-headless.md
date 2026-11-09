# Rejected — Technitium secondary without a web UI

[← Back to Rejected ideas](README.md) · Related: [Networking](../01-platform/networking.md)

> **Status (2026-11-09):** rejected after a read-only look at the repo, the live pods and the Technitium clustering docs.

## The idea

The secondary is exposed like the primary (own ingress route and `serversTransport`), although all admin work happens
on the primary. Could the secondary run without a web UI, and would that save anything?

## Why it lost

- **The UI port is also the cluster port.** The admin web console and the cluster sync API share one listener (port
  `53443` in this chart). The primary reaches the secondary on that port for joins and resyncs, so the listener
  cannot go away. The Technitium docs describe node-to-node traffic as running over the web service, and no source
  documents a switch to turn it off per node.
- **Nothing to save.** The secondary uses roughly the same memory as the primary (~300 Mi, a few millicores). That is
  the .NET runtime plus the DNS cache and block list, so the console adds almost nothing.
- **Dropping only the ingress is not worth a change.** It would remove two Traefik objects and one hostname, with no
  effect on the pod, and lose the browser route to the secondary for emergencies (`kubectl port-forward` still works).

## What would reopen it

A Technitium release that lets a cluster node run its cluster API without the console, or a real need to shrink
the exposed surface (then drop the secondary's ingress block in `system/dns/technitium/values.yaml`).
