# DNS — split-horizon, Technitium and Cloudflare {#dns}

[← Back to Home](../Home.md) · Platform: [Networking](networking.md) · [Certificates](certificates.md)

Two DNS "views" of the same domain exist on purpose: **Cloudflare** answers the internet,
**Technitium** answers the LAN. This page is the map; the config details live in
[`system/dns/technitium/README.md`](https://github.com/Coollision/homelab-config/blob/master/system/dns/technitium/README.md),
the step-by-step API runbook in `docs/dns-bootstrap.md`, and the non-obvious cluster rules in
[Known Issues → Technitium](../05-operations/known-issues.md#technitium-dns-cluster).

## Architecture {#dns-architecture}

```text
 Internet client ──▶ Cloudflare DNS (public view, DNSSEC) ──▶ proxied: Cloudflare Tunnel ──▶ external Traefik
                                                           └▶ unproxied: WAN IP (Plex only)

 LAN client ──▶ Technitium (internal view)  ◀── UniFi DNAT: any port-53 packet is rewritten to the primary
                 primary + secondary (pods, VLAN 30 MACVLAN) + tertiary (Docker on the NAS)
```

- **Technitium** is a 3-node cluster: primary and secondary as StatefulSets, a tertiary as
  a Docker container on the NAS (survives a total k3s outage). All three ad-block and all
  three are handed to clients. Cluster internals: [Known Issues](../05-operations/known-issues.md#technitium-dns-cluster).
- **Authoritative internal zones:** the personal domain(s), the private service zone
  (RFC 2136 updates from Ansible via TSIG), the cluster zone (DNSSEC-signed, carries the
  DANE/TLSA records the cluster trusts), reverse zones, plus read-only secondaries of a few
  legacy zones.
- **Everything else** is forwarded over DoT (TLS) to two public resolvers, with DNSSEC
  validation done by Technitium itself. There is no plain-UDP fallback.
- **Cloudflare** hosts the public view of the same domains: a wildcard CNAME to the tunnel
  (proxied), a handful of explicit names, and mail (MX/SPF/DKIM/DMARC). DNSSEC is on.

## How the two views line up {#dns-split-horizon}

| Name pattern | Internal (Technitium) | Public (Cloudflare) |
|---|---|---|
| `*.<domain>` | wildcard CNAME → internal Traefik (`ingress.<service-zone>`) | wildcard CNAME → tunnel (proxied) |
| apps with `ingress_external_secure` | same hostname, served by internal Traefik too | tunnel → external Traefik |
| `<domain>` apex / photo / NAS names | NAS LAN address | proxied WAN IP |
| `np.<domain>`, `plex.<domain>` | → NAS LAN address | unproxied WAN IP (Plex needs the raw IP) |
| MX / SPF / DKIM / DMARC | **absent** (see [gaps](#dns-gaps)) | present |

Rules of thumb:

- A new `<app>.<domain>` resolves internally to Traefik with **no DNS change** (wildcard).
- Anything exposed externally must also have an internal route, otherwise LAN clients hit the
  internal wildcard and get a Traefik 404. Today every externally exposed app has its internal twin.
- Internal-only names (`*.<service-zone>`, the cluster zone, NAS helpers) must never be created in Cloudflare.
- Because the internal zone is authoritative, **a record that exists only in Cloudflare is
  invisible on the LAN** (and an unknown subdomain is never NXDOMAIN, the wildcard answers it).

### Certificates and the split {#dns-acme}

cert-manager solves DNS-01 against Cloudflare and pins its recursive self-check to public
resolvers so it sees the *public* view, not the internal wildcard. That only works because
the cluster nodes sit on VLAN 30, which is **excluded** from the DNAT redirect. If a node ever
moves to a redirected VLAN, the self-check lands on Technitium, `_acme-challenge` resolves to
the internal wildcard (no TXT) and renewals stall silently.

## Record review (2026-11-05) {#dns-record-review}

A record-by-record pass over the public zones and the internal Technitium zones, plus live tests.

**Public (Cloudflare) side: healthy**

- The home zone's apex and `np` `A` records equal the gateway's current WAN address and were
  updated within a second of each other on the last IP change. DDNS is done by the UniFi gateway
  through a small Cloudflare Worker, so the records follow IP changes automatically.
- End-to-end through the tunnel (resolved at Cloudflare's edge, bypassing the internal view): all
  five externally exposed apps answer correctly, an unknown subdomain gets the catch-all `404`,
  and the edge certificate is valid. DNSSEC is active.
- CAA is Cloudflare-managed and already allows the CAs in use. SPF uses 3 of 10 lookups.

**Internal (Technitium) side: coherent**

- The private service zone holds the cluster nodes, the Traefik address (wildcard CNAME), the
  resolver names and the broker address. Two **spare nodes** have records but are not cluster
  members, and one broker record has a **misspelled name** with no known users. Both are harmless
  cleanup candidates, not defects.
- The two legacy secondary zones replicate from a **family member's NAS**, which hosts its own
  domains outside Cloudflare (with its own DDNS provider). Only one of them is actually queried.
- A small forwarder zone exists so one UniFi-related hostname resolves **publicly** instead of
  through the internal wildcard. It forwards over plain UDP, so it does not benefit from the DoT
  upstream change. Its only queries are SOA checks from the resolver nodes themselves.
- The domain that belongs to another person is hosted in the same Cloudflare account. Its
  apex/np records point at that person's public address; the owner confirms it still works.

## Enforcement: making clients use it {#dns-enforcement}

Three layers (full detail and API traps in `docs/dns-bootstrap.md` §9): a per-VLAN UniFi DNAT
that redirects any port-53 packet to the primary (UDP and TCP, IPv4), firewall blocks for
DoT/DoQ and for known public resolver IPs (v4 and v6), and canary/DoH-bootstrap names blocked
at the resolver. IPv6 is blocked, not redirected, on purpose.

Verified on the gateway dataplane (2026-11-05): rules are live and unshadowed on Intern, IoT,
Camera, TV, Default, guest and both remote-user VPNs; no NAT/masquerade is involved, so
Technitium logs the real client IP; the DoT and public-resolver ipsets are populated.
Quick functional test from any redirected VLAN: `dig @1.1.1.1 use-application-dns.net`
returns `NXDOMAIN` in about a millisecond.

Not covered by design: the resolver VLAN itself (VLAN 30), site-to-site traffic, and the **guest** network (public DNS on purpose, so visitors never see internal names). The **Default** network is management-only (UniFi switches and APs, no clients) and uses the gateway's resolver.

## Known gaps and improvements {#dns-gaps}

Findings of the 2026-11-05 audit, most important first (the allow list was checked too: it holds three specific domains, not TLDs, so it is not a leak). Status column is what was done
about it; "open" means a decision or change is still pending.

| # | Finding | Impact | Status |
|---|---|---|---|
| 1 | **The UniFi gateway's own resolver forwards to public DNS**, and the Default (management) and guest networks are handed the gateway as DNS. | Only UniFi gear and guests use it, so nothing of yours bypasses the filter. Moving it to Technitium would couple UniFi cloud access to the cluster and show guests internal names. | accepted, by design |
| 2 | **Guest DNAT rules sent hardcoded-DNS guests into the Servers VLAN**, where the guest zone is blocked, so their DNS would die silently. | Latent break for any guest device with a hardcoded resolver. | fixed 2026-11-05 — rules removed; guests now reach their resolver directly |
| 3 | **Internal zone serves no mail/TXT/CAA records** (MX, SPF, DKIM, DMARC, site-verification); they exist only in Cloudflare. | None: those records are looked up by the other side of a mail exchange or by a CA, always against public DNS, and the only mail sender here (Vaultwarden) uses an external relay. Mirroring would just drift when a provider rotates a key. | accepted, by design |
| 4 | **Upstream forwarders used plain UDP** to two public resolvers. | Upstream queries left in clear text (DNSSEC validation protects integrity, not privacy). | fixed 2026-11-05 — forwarders now DoT (TLS, certificate validated by hostname); cold lookups ~25-60 ms on all three nodes |
| 5 | **No DNS monitoring.** Only CoreDNS is scraped; nothing probes each Technitium node. Pod readiness and volume usage are already covered by the kube defaults. | An answering failure (or the NAS tertiary, which has no Kubernetes object) goes unnoticed until clients timeout. | parked — a design exists in `system/monitoring/disabled-blackbox-exporter`, see [Monitoring](#dns-monitoring) |
| 6 | **~7 % of queries were REFUSED**: an hourly burst of ~250 PTR lookups for the pod subnet. Diagnosed: Home Assistant's `dhcp` integration swept its own pod `/24` (see [Known Issues](../05-operations/known-issues.md#ha-dhcp-discovery-ptr-burst)). | Log and metrics noise only. | fixed 2026-11-06 in the HA chart (the probe address is routed over its VLAN 5 interface, so the sweep now covers the LAN `/24`); **confirmed** on the first sweep after HA restarted: the LAN range was queried and answered `NXDOMAIN`/`NOERROR`, no `REFUSED` |
| 7 | **Cloudflare token**: no expiry, shared with cert-manager, and its DNS permission appears to cover every zone in the account, not only the one cert-manager needs. | Wider blast radius than necessary if it leaks (cert-manager only needs `Zone:Zone:Read` plus `Zone:DNS:Edit` on its own zone). | accepted by the owner 2026-11-05 |
| 8 | **Public mail/CA hygiene.** Checked 2026-11-05 against the zone's own nameserver: CAA is present (managed automatically by Cloudflare for Universal SSL, hidden from the dashboard record list, and already allows Let's Encrypt); SPF uses 3 of 10 lookups with a soft-fail; DMARC is `p=none` (monitoring only). | Only the DMARC policy is weak: spoofed mail from the domain is not quarantined. | mostly fine — optional: raise DMARC to `quarantine` (ramp with `pct`) after reviewing the aggregate reports at the report provider; verify public records via the zone's own nameserver, not `@1.1.1.1` (redirected on the LAN) |
| 9 | **Servers VLAN (30) has no redirect or block**; other hosts there can use public/DoT resolvers. | Residual bypass, known and accepted since the resolvers live there. | accepted |
| 10 | **DoH to arbitrary hosts** cannot be blocked without TLS inspection. | Known limitation. | accepted |
| 11 | Single `admin` account plus long-lived API tokens. | No separation between interactive and automation access. | accepted by the owner 2026-11-05 (idea if revisited: a no-group user is already view-only on most sections, and a small group can add Settings/Cluster view) |
| 12 | **The descheduler evicts pods from the Pi worker when its pod count crosses 80 % of capacity** (`LowNodeUtilization`, capacity 40, so 33+ pods), including the Technitium primary (observed once; volume re-attach each time). | While the node is over the threshold the evicted pods come straight back and the loop repeats every 2 minutes (93 evictions in 6 h at its worst). It went quiet at 31 pods (78 %) with the policy unchanged, so it is latent, not fixed. | accepted by the owner 2026-11-06 (not urgent). If it returns: raise the pod-count target for that profile, exclude the Pi node from `LowNodeUtilization`, or disable that plugin |

Docs fixed in the same pass: README (still described ClusterIP/binary-patch clustering and a
BIND slave), blocklist list, TV VLAN redirect behaviour, version.

## Monitoring (parked) {#dns-monitoring}

Not live. A blackbox-exporter design sits in `system/monitoring/disabled-blackbox-exporter`
(a `disabled-` directory is ignored by ArgoCD): three probes per resolver (internal name,
public name, a block-listed host that must return `NXDOMAIN`) plus alerts through the normal
Slack severity routing. It was parked because of one constraint:

- A prober pod on the **same node** as a Technitium pod cannot reach that pod's MACVLAN
  address (macvlan parent to child is not routable). The only node Technitium can never run on
  is the single VM node, so the prober had to be pinned there, and then **every planned
  maintenance of that node raises alerts**. A false alert is worse than none.

Ways out when it is picked up again: probe the in-cluster pods through their ClusterIP Services
(no pinning, but this skips the MACVLAN path clients use) and probe only the NAS container by
address; or give the prober its own VLAN 30 MACVLAN leg. A Technitium stats exporter
(community projects polling its API with a read-only token) is a separate, optional step for
dashboards and trends.

## Day-2 cheat sheet {#dns-day2}

- **Checking public DNS from the LAN:** `dig @1.1.1.1` is redirected to Technitium and returns the *internal* view. Query a Cloudflare nameserver for the zone directly (`dig @<cloudflare-ns> <domain> MX`), or use the Cloudflare API. `dig MX` against Technitium legitimately returns nothing (see gap 3).
- **Reach the API:** `kubectl -n dns port-forward pod/technitium-primary-0 <port>:53443`, token
  from the gitignored `.env`. Read-only calls only unless you mean it.
- **Health = every replicated zone present on all nodes with an identical SOA serial** and no
  notify failures, not the zone count.
- **Blocked something you need?** Allow the *specific domain* (not the TLD); blocked queries
  show as `BLOCKED` in the query log.
- **Backups:** each pod's config volume has Longhorn snapshots + a daily backup; the cluster
  also keeps three live copies of the synced config.
