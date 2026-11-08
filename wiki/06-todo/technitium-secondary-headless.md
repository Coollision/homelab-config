# TODO — Technitium secondary without a web UI

[← Back to Home](../Home.md) · Related: [DNS](../01-platform/networking.md)

> **Status (2026-11-09):** idea only, not researched yet. Parked on purpose; nothing here blocks anything.

Question: does the Technitium **secondary** need its web UI at all, and would running it without one save anything?
Today the secondary is exposed like the primary (own ingress, `dns-b`), although all admin work happens on the primary.

- [ ] **Is "no UI" possible at all?** Find out whether Technitium can disable its web service per node, or whether the
      web port is also the API and cluster-sync endpoint that the secondary needs (the primary talks to it over that
      HTTPS port). If the UI and API are one listener, the realistic option is only "stop exposing it".
- [ ] **Would it save resources?** Measure first: memory/CPU of the secondary vs the primary (`kubectl top`), and what the
      web UI adds on top of the DNS core. Expect little if the UI is static files served by the same process.
- [ ] **Cheaper variant:** drop the secondary's ingress (and the `serversTransport` it needs) so there is no UI route
      to it, while keeping the pod, the cluster sync and DNS service untouched. Check that nothing (Homepage, monitoring,
      scripts) uses `dns-b`.
- [ ] **Decide** and, if worth it, change `system/dns/technitium/values.yaml`; update the DNS wiki page in the same change.
