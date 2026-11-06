# Technitium DNS (clustered) — authoritative DNS + ad-blocking

Authoritative DNS and network-wide ad-blocking for the homelab, running on the
cluster instead of the NAS so it survives a node going down and keeps serving
while the NAS is offline.

## Topology

```text
                      ┌──────────────────────────┐
   clients  ── DNS1 ─▶│ Technitium PRIMARY  (VIP-A)│  authoritative + ad-block
  (via DHCP)          │  pod on node X             │  ← only node that accepts
                      └────────────┬──────────────┘    config/zone changes
                                   │ cluster sync (block lists, settings,
                                   │ users, tokens) + catalog zones
                      ┌────────────▼──────────────┐
   clients  ── DNS2 ─▶│ Technitium SECONDARY (VIP-B)│ authoritative + ad-block
                      │  pod on node Y             │  (read-only for config)
                      └────────────┬──────────────┘
                                   │ same cluster, over the VLAN 30 IPs
                      ┌────────────▼──────────────┐
   clients  ── DNS3 ─▶│ Technitium TERTIARY (NAS)  │ Docker on the NAS, joined to
                      │  outside Kubernetes        │ the cluster: survives a total
                      └────────────────────────────┘ k3s outage, ad-blocks too
```

- **Two in-cluster instances, one chart.** `values.yaml` has `primary:` and `secondary:`
  sub-trees, each rendered as its own StatefulSet + ClusterIP Service. The tertiary is a
  plain Docker container on the NAS, not part of this chart.
- **Each pod gets a MACVLAN interface** (`net1`, static DHCP lease in UniFi) for the
  VLAN 30 IP clients use as DNS. No MetalLB.
- **All three nodes talk to each other over those VLAN 30 IPs** (IPv4 + a stable ULA),
  never ClusterIPs or node IPs; an initContainer adds host routes so pods source from
  the MACVLAN address. Why: [Known Issues → Technitium](../../../wiki/05-operations/known-issues.md#technitium-dns-cluster).
- **Clients are handed all resolvers that ad-block** via DHCP/RA, and a UniFi DNAT rule
  redirects stray port-53 traffic to the primary (`docs/dns-bootstrap.md` §9).

## Clustering model

- **No automatic promotion.** Kubernetes handles recovery: primary pod reschedules,
  Longhorn volume re-attaches, same primary is back in ~1 min. Manually promote a
  secondary only if the primary's volume is unrecoverable.
- **Config edits require the primary online.** Secondaries are read-only for config but
  keep answering queries. Ansible `nsupdate` targets the primary VIP.
- **What syncs automatically:** block/allow lists, settings, users, tokens, catalog
  zones. Cache, logs and `recursionNetworkACL` are per-node.
- **Cluster TLS** uses each node's self-signed cert, validated via DANE/TLSA records in
  the signed internal cluster zone, so that zone must always transfer first.
  See `docs/dns-bootstrap.md` for init/join and the non-obvious ACL rules.

## Ad-blocking

**Active blocklists:** [OISD Big](https://big.oisd.nl/) and Hagezi `multi` (wildcard,
domains-only) — update every 24 h, synced to every node via clustering. Blocked domains
return `NXDOMAIN`.

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

## Ad-blocking and the tertiary

Ad-blocking is a resolver-side filter, not zone data. Because client resolver selection is
unreliable, **every advertised resolver must enforce the same policy** — which is why the
NAS runs a Technitium that is *joined to the cluster* (settings and block lists sync) rather
than a plain DNS server.

## Post-deploy bootstrap

The chart only deploys the servers. Once pods are up, an operator configures the rest
via the API — see `docs/dns-bootstrap.md` for the step-by-step.

> Private domains, IPs and node names are omitted per repo convention.
> Concrete values live in the (gitignored) Ansible config and Vault.
> Architecture overview, split-horizon design and Cloudflare side: [DNS](../../../wiki/01-platform/dns.md).
