# Technitium DNS (clustered) — authoritative DNS + ad-blocking

Authoritative DNS and network-wide ad-blocking for the homelab, running on the
cluster instead of the NAS so it survives a node going down and keeps serving
while the NAS is offline.

## Topology

```text
                      ┌──────────────────────────┐
   clients  ── DNS1 ─▶│ Technitium PRIMARY  (VIP-A)│  authoritative + ad-block
  (via DHCP)          │  node X                   │  ← only node that accepts
                      └────────────┬──────────────┘    config/zone changes
                                   │ cluster sync (block lists, settings,
                                   │ users, tokens) + catalog zones
                      ┌────────────▼──────────────┐
   clients  ── DNS2 ─▶│ Technitium SECONDARY (VIP-B)│ authoritative + ad-block
                      │  node Y                   │  (read-only for config)
                      └────────────┬──────────────┘
                                   │ AXFR + NOTIFY (TSIG, overlay only)
                      ┌────────────▼──────────────┐
                      │ Synology  (BIND slave)     │ disaster break-glass
                      │  NOT in the DHCP list      │ (no ad-block — see below)
                      └────────────────────────────┘
```

- **Two instances, one chart.** `values.yaml` has `primary:` and `secondary:` sub-trees,
  each rendered as its own StatefulSet + ClusterIP Service.
- **Each pod gets a MACVLAN interface** (`net1`, static DHCP lease in UniFi) for the
  VLAN 30 IP clients use as DNS. ClusterIPs (the Service) handle pod-to-pod cluster
  traffic so it stays on the flannel overlay. No MetalLB.
- **Clients are handed only the two VIPs** via DHCP — both ad-block.

## Clustering model

- **No automatic promotion.** Kubernetes handles recovery: primary pod reschedules,
  Longhorn volume re-attaches, same primary is back in ~1 min. Manually promote the
  secondary only if the primary's volume is unrecoverable.
- **Config edits require the primary online.** Secondary is read-only for config but
  keeps answering queries. Ansible `nsupdate` targets the primary VIP.
- **What syncs automatically:** block/allow lists, settings, users, tokens, catalog
  zones. Cache and logs are per-node.
- **Overlay routing:** cluster sync (port 53443), NOTIFY, and AXFR all route via the
  flannel overlay using ClusterIPs. The `mylocaldns` zone A records must equal the
  ClusterIPs — this is enforced by a binary patch to `/etc/dns/cluster.config` done
  once after cluster init (see `docs/dns-bootstrap.md`).

## Ad-blocking

**Active blocklist:** [OISD Big](https://big.oisd.nl/) — updates every 24 h, synced
to both instances via clustering. Blocked domains return `NXDOMAIN`.

### Checking a blocked query

In the Technitium web UI → **Logs** → filter by the client IP or domain. Blocked
queries show `BLOCKED` in the Action column. Or query directly:

```bash
# Should return NXDOMAIN if blocked, an answer if not:
dig @<VIP-A> <suspect-domain> A
```

To see why a domain is blocked, check **Blocking → Blocked Zone** in the UI —
it shows which list matched.

### Allowing a domain (permanent whitelist)

**Web UI (primary only — syncs to secondary automatically):**
Settings → Blocking → Allowed List → paste the domain(s), one per line → Save.

Or via the allow-list file directly: Settings → Blocking → toggle to
"Allowed Domains" view, add entries.

**API:**

```bash
# Add to allowed list
POST /api/allowed/add?token=<T>&domain=<domain>

# Remove from allowed list
POST /api/allowed/remove?token=<T>&domain=<domain>

# List current entries
GET  /api/allowed/list?token=<T>
```

Changes on the primary sync to the secondary within seconds.

### Temporarily disabling ad-blocking

Settings → Blocking → uncheck "Enable Blocking" → Save. Re-enable the same way.
Only do this on the primary; secondary follows via cluster sync.

### Adding a stronger blocklist

OISD Big covers ~90% of ad/tracker domains. For a stricter setup, add Hagezi Pro
in Settings → Blocking → Block List URLs:
`https://raw.githubusercontent.com/hagezi/dns-blocklists/main/adblock/pro.txt`

Keep in mind a stronger list increases false positives. Whitelist individual domains
as needed (see above).

## Ad-blocking and the Synology fallback

Ad-blocking is a resolver-side filter — not zone data, so it never transfers to BIND.
Because client resolver selection is unreliable, **every advertised resolver must
enforce the same policy**. The Synology stays a synced slave but is a **manual
break-glass** for a true total-cluster outage, not a normal resolver.

**Future improvement:** run a third Technitium on the NAS joined to the cluster — it
would ad-block too and survive total cluster failure. Deferred until port 53 is free
on the NAS (retire the NAS DNS Server package first).

## Post-deploy bootstrap

The chart only deploys the servers. Once pods are up, an operator configures the rest
via the API — see `docs/dns-bootstrap.md` for the step-by-step.

## Cutover & rollback

Cutover keeps the NAS authoritative until the end: bring Technitium up as a secondary
of the NAS, validate, switch DHCP, then flip authority (Technitium → primary,
NAS → slave) and re-point Ansible. Fully reversible until that flip.

> Private domains, IPs and node names are omitted per repo convention.
> Concrete values live in the (gitignored) Ansible config and Vault.
