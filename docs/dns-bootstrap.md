# Technitium DNS — bootstrap & key settings

**Version:** 15.4
**Topology:** 3-node cluster, all comms over **VLAN 30 MACVLAN IPs** — primary pod `<VIP-A>`,
secondary pod `<VIP-B>`, tertiary (NAS Docker) `<NAS-IP>`. Cluster nodes advertise these
MACVLAN IPs; **never ClusterIPs or node IPs**. This is the only model that lets the
off-overlay NAS node join (it can't route to k8s ClusterIPs).

The advertised address is set at init/join but is **no longer immutable** — v15.4 added
`POST /api/admin/cluster/updateIpAddress` (see §2.0) and
`POST /api/admin/cluster/secondary/updatePrimary`. Advertise the **ULA** alongside the
IPv4 MACVLAN IP and **never a delegated GUA** — see §2.0.

**Cert model — DANE/TLSA (important):** cluster TLS uses each node's **self-signed** cert,
validated via **DANE**: each node publishes `_53443._tcp.technitium-<role>.mylocaldns TLSA`
records in the `mylocaldns` zone. A node trusts a peer only if it can resolve that TLSA
record, so **`mylocaldns` must transfer first** or every heartbeat/config-sync fails with
`UntrustedRoot`. `ignoreCertificateErrors=true` (below) bypasses validation **only for the
one-time join**; ongoing comms always DANE-validate.

All API calls: `https://<VIP>:53443/api/<endpoint>?token=<T>` (TLS, self-signed → `curl -k`).
Token: `GET /api/user/login?user=admin&pass=<pass>`.

---

## 1. Cluster setup (all-VLAN30)

### Init (primary, once)

```http
POST /api/admin/cluster/init
  clusterDomain=mylocaldns
  primaryNodeIpAddresses=<VIP-A>          # the primary's VLAN 30 MACVLAN IP, NOT a ClusterIP
```

### Join each secondary — run ON the joining node

```http
POST /api/admin/cluster/initJoin          # on the secondary/tertiary itself
  primaryNodeUrl=https://technitium-primary.mylocaldns:53443/
  primaryNodeIpAddress=<VIP-A>            # REQUIRED manual IP — see gotcha below
  primaryNodeUsername=admin  primaryNodePassword=<pass>
  secondaryNodeIpAddresses=<this node's VLAN 30 IP>   # <VIP-B> or <NAS-IP>
  ignoreCertificateErrors=true            # REQUIRED: self-signed certs (join-time only)
```

> **Gotcha:** the join resolves the primary's name through the joining node's resolver
> (CoreDNS for pods) which doesn't host `mylocaldns` → "could not be resolved". `nslookup`
> also bypasses `/etc/hosts`, so it looks broken even when `hostAliases` are set. Always
> pass `primaryNodeIpAddress=<VIP-A>` explicitly.

### net1 host routes on the pods — REQUIRED

The `sbr` chained plugin moves net1's subnet route into a separate table, so a pod's
cluster traffic to a VLAN 30 peer would otherwise egress via the overlay → node → SNAT to
the (changing) node IP, which the ACLs refuse. The chart's initContainer
([`system/dns/technitium/values.yaml`](../system/dns/technitium/values.yaml)) adds `/32`
routes to every peer (`<VIP-A>`/`<VIP-B>`/`<NAS-IP>`) out of net1 so each pod sources from
its own MACVLAN IP. Without it: AXFR/NOTIFY refused → `mylocaldns` never transfers → DANE
fails → `UntrustedRoot`. The NAS reaches VLAN 30 natively, so it needs no routes.

---

## 2. Key settings applied post-init

### 2.0 Cluster node addresses — the source of truth for catalog NOTIFY

**Read this before debugging any NOTIFY failure.** The catalog zone's `notifyNameServers`
**and** `zoneTransferNetworkACL` are **auto-derived from `settings.clusterNodes[].ipAddresses`**,
and every catalog **member** zone *inherits the catalog's notify list*. So one stale address
in the cluster node list produces notify failures across every member zone whose serial
changes. Editing the catalog's notify list by hand appears to work but is **regenerated**
the moment anything touches cluster config — always fix the node list instead:

```http
POST /api/admin/cluster/updateIpAddress
  node=technitium-<role>.mylocaldns
  ipAddresses=<VIP-x>,<ULA-x>              # IPv4 MACVLAN + ULA; NEVER a delegated GUA
```

> **A node may only update its OWN entry** — run this against the node being changed
> (the NAS's entry must be set through the NAS's own API, not the primary's).

Each secondary's `primaryNameServerAddresses` derives from the same list, which is why the
primary's ULA must be present there or IPv6 NOTIFY is answered with `RCODE=Refused`.

**Never record an ISP-delegated GUA anywhere.** The delegation rotates; every GUA recorded
before a rotation becomes a black hole that presents as a generic NOTIFY timeout
(`SocketException 110`). Use the ULA `<ULA-PREFIX>::/48` per-VLAN `/64`s — both halves are
renumber-proof. See the long comment in
[`system/dns/technitium/values.yaml`](../system/dns/technitium/values.yaml).

### Zone-transfer ACL — every primary zone, keep CONSISTENT

AXFR from a secondary arrives sourced from that node's VLAN 30 MACVLAN IP (via the net1
routes). Allow the specific MACVLAN IPs on **every** zone the cluster replicates — that is
`pi`, `declerck.dev`, `mylocaldns`, `thewizardofoz.win`, `cluster-catalog.mylocaldns`
**and the reverse zones** (`<vlan>.168.192.in-addr.arpa`). A per-zone omission silently
breaks just that zone's transfer (and if it's `mylocaldns`, DANE breaks too):

```http
POST /api/zones/options/set
  zone=<zone>  zoneTransfer=UseSpecifiedNetworkACL
  zoneTransferNetworkACL=<VIP-A>          # primary MACVLAN (promotion/self)
  zoneTransferNetworkACL=<VIP-B>          # secondary pod
  zoneTransferNetworkACL=<NAS-IP>         # tertiary (NAS)
  zoneTransferNetworkACL=<ULA-A>          # ULA of each node — REQUIRED, see below
  zoneTransferNetworkACL=<ULA-B>
  zoneTransferNetworkACL=<ULA-NAS>
  zoneTransferNetworkACL=10.42.0.0/16     # pod overlay (harmless fallback)
  # + <SYNO-IP> (Synology slave) / <REMOTE-MASTER-IP> per zone as needed
```

> **The ULAs are not optional.** Because the cluster node list (§2.0) advertises each node's
> ULA, NOTIFY is sent to it — and a secondary notified at its ULA replies with an AXFR
> **sourced from that ULA**. An IPv4-only ACL then logs
> `refused a zone transfer request since the request IP address is not allowed by the zone`.
> Keep the notify targets and the transfer ACL on the same address families.

**Reverse (`in-addr.arpa`) zones are `Forwarder` type and default to `zoneTransfer=Deny`,**
so they silently never replicate — the `SecondaryForwarder` copies just freeze at whatever
they last held. Apply the ACL above to them explicitly.

### Catalog primary address — on each secondary

Catalog member zones resolve their primary via the catalog. Per-zone
`overrideCatalogPrimaryNameServers` does **not** persist on a catalog member; instead set
it once on the `SecondaryCatalog` zone on each secondary — it sticks and drives AXFR for
all member zones:

```http
POST /api/zones/options/set   (on each secondary / tertiary)
  zone=cluster-catalog.mylocaldns
  primaryNameServerAddresses=<VIP-A>  primaryZoneTransferProtocol=Tcp
```

### Synology NOTIFY

```http
POST /api/zones/options/set
  zone=<zone>  notify=BothZoneAndSpecifiedNameServers
  notifyNameServers=<SYNO-IP>
  zoneTransferTsigKeyNames=secundaryZoneUpdate
```

---

## 3. TSIG keys (web UI only — API rejects non-empty secrets in v15)

Settings → TSIG Keys → Add on the **primary** (syncs to secondaries automatically):

| Key name              | Algorithm | Secret source                      |
|-----------------------|-----------|------------------------------------|
| `dnsUpdateAnsible`    | HMAC-MD5  | `tmp/zonefile/dnsUpdateAnsible`    |
| `secundaryZoneUpdate` | HMAC-MD5  | `tmp/zonefile/secundaryZoneUpdate` |

Scope `dnsUpdateAnsible` to the `pi` zone for RFC-2136 updates:

```http
POST /api/zones/options/set
  zone=pi
  updateSecurityPolicies=dnsUpdateAnsible|*.pi|ANY|dnsUpdateAnsible|pi|ANY
  update=UseSpecifiedNetworkACL
  updateNetworkACL=<ansible-controller-subnet>,<server-vlan-subnet>,<pod-cidr>
```

Both parts are required, otherwise `nsupdate` returns `REFUSED`:

- The policy domain must be `*.pi` — a bare `pi` only matches the zone apex, not hosts under it.
- The zone's `update` ACL is checked **in addition to** the TSIG policy (`update=Deny` logs
  "request IP address is not allowed by the zone"). Allow the subnet Ansible runs from.

Dynamic updates are only accepted by the cluster **primary**, so `dns_server_ip` must point at it.

---

## 4. Ad-blocking

```http
POST /api/settings/set
  enableBlocking=true  blockingType=NxDomain
  blockListUrls=https://big.oisd.nl/
  blockListUpdateIntervalHours=24
```

The list downloads asynchronously on first `settings/set` (`/api/blocklist/update/start`
returns 404 in v15). Syncs to secondaries via clustering. See
[`system/dns/technitium/README.md`](../system/dns/technitium/README.md) for managing the
allowlist and checking blocked queries.

---

## 5. Zone seeding

```http
# Primary zone added to the cluster catalog:
POST /api/zones/create
  zone=<zone>  type=Primary  catalog=cluster-catalog.mylocaldns

# Import records from a BIND zone file (catalog members reject NS/glue + SOA MNAME
# mismatches — strip NS/glue and set SOA MNAME = technitium-primary.mylocaldns):
POST /api/zones/import
  zone=<zone>  overwrite=true                     # body: BIND zone file text

# Secondary zone from a remote master (e.g. pi1, declerck.cool):
POST /api/zones/create
  zone=<zone>  type=Secondary
  primaryNameServerAddresses=<REMOTE-MASTER-IP>
  zoneTransferProtocol=Tcp  tsigKeyName=secundaryzoneupdate   # lowercased internally
  catalog=cluster-catalog.mylocaldns
```

IPv6 recursion: there is no combined "private + specified" mode in v15 — use
`recursion=UseSpecifiedNetworkACL` and list the RFC-1918 ranges **plus** the ULA prefix
explicitly in `recursionNetworkACL`:

```http
POST /api/settings/set     # run on EVERY node — this setting does NOT cluster-sync
  recursion=UseSpecifiedNetworkACL
  recursionNetworkACL=10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,127.0.0.0/8,::1,<ULA-PREFIX>::/48
```

> **Two traps here.** (1) `recursionNetworkACL` is **per-node and does not cluster-sync** —
> set it on the primary *and* every secondary or IPv6 clients get inconsistent answers.
> (2) List the **ULA prefix**, never delegated GUA `/64`s. UniFi advertises the ULA as an RA
> DNS server, so IPv6 clients query from a ULA source; if only GUA ranges are listed, every
> recursive query is answered `REFUSED` while *authoritative* lookups still return `NOERROR`.
> That asymmetry hides the fault: internal names resolve fine, so it looks healthy, while
> dual-stack clients burn a DNS timeout per external lookup before falling back to IPv4.

---

## 6. Synology slave (UI)

Flip the local master zones from master → slave on Synology, master = `<VIP-A>`,
TSIG = `secundaryZoneUpdate`. Leave the remote-mastered zones as-is.

## 7. Ansible cutover

Update `dns_server_ip` in `ansible/inventory/group_vars/` to `<VIP-A>`, then run
`ansible-playbook -i inventory playbooks/dns.yml`.

## 8. UniFi DHCP

DNS servers: **`<VIP-A>`, `<VIP-B>` only** — no Synology (it can't ad-block; see README).

### Multus stubs in the client list

Most of the unrecognised locally-administered MACs on the IoT and LAN VLANs are not real
devices — they are the **Multus MACVLAN interfaces** of cluster pods (the DNS pods, and the
smart-home pods that need to sit on an IoT/LAN segment). Each attachment is a separate NIC
on a real VLAN, so each gets its own DHCP lease and its own UniFi client entry.

They are pinned rather than random precisely so they can be named and reserved. Each
attachment declares a `mac:` (locally-administered `02:` prefix, second octet = VLAN tag,
last octet = workload id) and a `dhcpHostname:` of the form `_stub_<workload>-<vlan-role>`.
The scheme and the full allocation table live in
[`lib/shared-lib/templates/_multus.yaml`](../lib/shared-lib/templates/_multus.yaml); the
values are in each workload's `values.yaml`. **That is the source of truth — the UniFi
client list mirrors it, never the other way round.**

> **`dhcpHostname` does not actually name anything in UniFi.** It renders correctly into the
> NAD as a `provide` of DHCP option 12, and the bundled CNI `dhcp` plugin supports the
> option, but no stub has ever registered a hostname on the controller — verified on a stub
> whose UniFi record was created the same day the option was added to its config. UniFi
> therefore falls back to fingerprinting, which confidently mislabels pod interfaces as
> consumer hardware (a reflector stub was listed as a printer at 99% confidence). Treat
> `dhcpHostname` as documentation-in-config only; **the UniFi alias is what actually names
> the client**, and it outranks both fingerprint and hostname in the UI.

For each attachment, set the UniFi alias to exactly the `dhcpHostname` string and give it a
fixed IP. Two things to know before touching one:

- **A MAC must be unique across the whole homelab, not merely per-VLAN.** UniFi keys clients
  on the MAC alone, so two stubs sharing one on different VLANs collapse into a single entry
  that flips between segments and can be neither named nor reserved. It looks fine on the
  wire, which is why this survived unnoticed.
- **Changing a `mac:` changes the IPv6 link-local (EUI-64) too,** and several IPv6 routes are
  written against those by hand. `grep` for the EUI-64 form before renumbering, and re-point
  the fixed reservation in the same change or the pod silently picks up a new address.
- **Never age out a `_stub_` entry on `last_seen`.** UniFi does not maintain that field for
  these clients — stubs that are connected *right now* report a `last_seen` months in the
  past, and the two DNS stubs report `never`. A "delete anything not seen in 30 days" sweep
  would therefore wipe live infrastructure. Check membership in `/stat/sta` instead, and
  exclude the `_stub_` prefix from any such cleanup.

### Client alias prefixes

The controller's client list uses a prefix convention that makes it sort and scan usefully.
Keep new entries inside it:

| Prefix | Scope | Form |
|---|---|---|
| `_stub_` | Multus pod interfaces | `_stub_<workload>-<iot\|lan>`, verbatim from `dhcpHostname` |
| `_` | cluster nodes and infra hosts | `_<Hostname>`, capitalised |
| `Z_` | IoT / smart-home devices | `Z_<domain>.<room>.<detail>`, lowercase, dot-separated |
| *(none)* | people's devices, AV, misc | free text, `<Type> <Person>` |

The two underscore prefixes exist to sort infrastructure above everything else; `Z_` sorts
the large IoT population to the bottom. Within `Z_`, prefer the dotted form that mirrors the
Home Assistant entity id — spaces are the outlier, not the norm.

### Editing clients over the API

The Network **integration** API (`/proxy/network/integration/v1`) is read-only for clients —
it can list them and run actions, but has no field for an alias or a fixed IP. Renaming and
reserving must go through the **legacy** endpoint, which accepts the same API key:

```http
PUT  /proxy/network/api/s/default/rest/user/<_id>
  {"name": "...", "use_fixedip": true, "fixed_ip": "...", "network_id": "..."}
POST /proxy/network/api/s/default/rest/user          # create a record for a MAC not yet seen
```

`network_id` is required whenever `fixed_ip` is set. A reservation for an address another
client currently holds is rejected with `api.err.FixedIpAlreadyUsedByClient` — the old client
has to be forgotten first, which matters when a pod is being renumbered onto a new MAC while
its old interface still holds the lease.

---

## 9. DNS enforcement — making clients actually use these resolvers

DHCP-supplied DNS is **advisory**. A device is free to ignore it, and plenty do: smart TVs
and IoT firmware routinely hardcode a public resolver, and browsers ship DNS-over-HTTPS on
by default. Without enforcement the ad-blocking in §4 and every internal zone above are
simply bypassed by whichever client feels like it. Three layers close that, in descending
order of usefulness.

### 9.1 Layer 1 — transparent redirect (UniFi Destination NAT)

The important layer. A DNAT rule rewrites the destination of any port-53 packet to the
primary resolver, so a device hardcoded to a public resolver is **silently answered by the
cluster** and never notices. This is strictly better than blocking: nothing breaks, it just
stops working the way the device intended.

Configured under Settings → Policy Engine → NAT. The API is the internal v2 endpoint —
**not** the documented integration API, which has no NAT paths at any version:

```http
POST /proxy/network/v2/api/site/default/nat
{
  "type": "DNAT",  "protocol": "udp",  "ip_version": "IPV4",
  "in_interface": "<network _id of the client VLAN>",
  "ip_address":   "<primary resolver VLAN30 MACVLAN IP>",   # the translated destination
  "rule_index": <unique int>,  "enabled": true, "exclude": false,
  "is_predefined": false, "logging": false, "pppoe_use_base_interface": false,
  "setting_preference": "manual",
  "source_filter":      { "filter_type": "NONE", "firewall_group_ids": [],
                          "invert_address": false, "invert_port": false },
  "destination_filter": { "filter_type": "ADDRESS_AND_PORT", "firewall_group_ids": [],
                          "invert_address": false, "invert_port": false, "port": "53" }
}
```

Field names are not guessable and the endpoint returns a bare **HTTP 500 with no message**
for anything malformed, so do not brute-force it — create one rule in the UI and `GET` the
endpoint to read the exact shape. Notable traps:

- The translated address is **`ip_address`**, not `translated_ip`. There is no translated-port
  field; the destination port is preserved.
- It is **`rule_index`** (not `index`) and it must be **unique across all NAT rules** —
  a collision is rejected with `api.err.NatRuleInvalidParameters: duplicate rule_index`.
- It is **`is_predefined`**, not `predefined` as on firewall policies.
- `in_interface` takes a **network `_id`**, so rules are per-**network**, not per-zone.
  Grouping VLANs into one firewall zone does *not* give them a shared NAT rule — each
  network needs its own.
- **UDP and TCP need separate rules.** DNS falls back to TCP for large responses; a
  UDP-only rule leaks those silently.

Because each rule is bound to one client network, **no exclusion rule is needed** for the
resolver VLAN — a rule scoped to a client interface can never capture the resolvers' own
upstream queries, so the self-redirect loop that guides warn about cannot occur. Only add an
`exclude: true` rule if a rule is ever applied network-wide.

No SNAT/masquerade is required either: clients and resolvers sit on different VLANs, so
replies already transit the gateway and are un-NATted back to the address the client
expected. Adding masquerade here would only hide the real client IP from the query log.

**Deliberately excluded:** the resolver VLAN itself, and the **IPTV networks** — carrier IPTV
commonly depends on the ISP's own resolver, and redirecting it is a well-known way to break
live TV. Those VLANs keep their ISP-supplied DNS on purpose.

### 9.2 Layer 2 — block what cannot be redirected

Encrypted DNS can't be transparently rewritten, so it is blocked outright with zone-based
firewall policies (`BLOCK`, `logging: true`, source = each client zone, destination =
external):

| Target | Mechanism |
|---|---|
| DoT / DoQ | port group `853, 784, 8853` |
| Hardcoded-IP DoH | address group of well-known public resolver IPs, **all ports** |

Blocking the resolver IPs on every port (rather than just 443) also catches plain port-53
traffic to them, which is a useful backstop wherever a DNAT rule does not apply.

> **DoH cannot be fully solved.** It is port 443 to ordinary web addresses and is
> indistinguishable from browsing without TLS interception. Blocking known providers raises
> the cost; it is not a guarantee. Treat this layer as defence in depth, not a boundary.

### 9.3 Layer 3 — resolver-side (the cheapest win)

Because `blockingType=NxDomain` is already set (§4), the manual block zone doubles as DoH
suppression. Add via `POST /api/blocked/add?domain=<domain>`; entries propagate to every
cluster node:

- **`use-application-dns.net`** — the Firefox canary. NXDOMAIN here makes Firefox **disable
  DoH by itself**, with no client configuration. Highest value single entry on the list.
- The **DoH bootstrap hostnames** of the major providers (Google, Cloudflare incl. their
  `security`/`family`/`chrome` variants and `one.one.one.one`, Quad9, OpenDNS, AdGuard,
  NextDNS, CleanBrowsing, dns.sb). Kills any DoH profile configured by hostname rather than
  by IP, including Android Private DNS in strict mode.

Chromium-family browsers only auto-upgrade to DoH when the configured resolver appears on a
known-provider list; a self-hosted resolver does not, so they stay on plaintext by default.

### 9.4 Verifying it works

Do **not** verify by checking that a public resolver is unreachable — that only proves the
firewall layer. Prove the *redirect* with a query whose answer differs between resolvers:

```bash
# from a client VLAN, aimed at a public resolver:
dig @<any public resolver> use-application-dns.net     # expect NXDOMAIN, in ~1ms
```

A public resolver returns `NOERROR` for that name; only a resolver with the canary blocked
returns `NXDOMAIN`. LAN-scale latency on a query supposedly crossing the internet is the
second tell. Run the same query from the excluded resolver VLAN as a control — it should
return `NOERROR` from the real public resolver, which confirms the rules are scoped and not
global.

Test **both** transports (`dig +tcp`), and confirm ordinary browsing and internal zones are
unaffected afterwards.

### 9.5 IPv6 — every layer needs its own rules

**Nothing above is dual-stack by default.** NAT rules carry `ip_version: IPV4`, and an
address-based firewall rule only matches the family of the literals in it. A dual-stack
client that reaches a public resolver over IPv6 bypasses all three layers while the IPv4
side looks perfectly enforced. Verify over IPv6 explicitly — this is the single easiest
thing to miss.

**IPv6 is blocked, not redirected — deliberately.** DNAT rewrites only the *destination*.
A client picks a **GUA source** when the destination is a public GUA, so a redirected packet
arrives at the resolver with a GUA source and is answered `REFUSED` by the ULA-only
`recursionNetworkACL` (§5). Making DNAT work on IPv6 would mean re-adding a delegated GUA
prefix to that ACL — the rotation hazard that caused a previous outage. Blocking instead
costs nothing: clients fall back to the RA-supplied ULA resolver, which is already correct.

| Layer | IPv4 | IPv6 |
|---|---|---|
| Redirect (DNAT) | ✅ per network, UDP + TCP | ❌ **by design** — see above |
| Public resolver block | address group, all ports | separate rule, `group_type: ipv6-address-group` |
| DoT / DoQ port block | — | one rule, `ip_version: BOTH`, covers both families |

Only the port-based rule can serve both families from a single policy; anything matching on
addresses needs a per-family rule with `ip_version` set accordingly.

> **Trap when testing IPv6.** Binding the probe to a **ULA** source and aiming at a public
> resolver always times out — ULAs are not globally routable, so the result says nothing
> about your rules. Bind to the **GUA**, or test from a real dual-stack client. A pod whose
> default route is the IPv4 overlay is equally useless as a probe. Confirm the harness works
> *before* the rules exist, or a false "already blocked" reading will hide the gap.

---

## Gotchas

- **k3s node-name NXDOMAIN noise:** the k3s node names (`worker-<x>`/`master-<x>`) differ
  from the DNS records (`node-<x>.<zone>`) and the OS hostname (`node-<x>`). kubelet resolves its
  own k3s node name every ~10 s → NXDOMAIN against these resolvers. Fixed by mapping each
  k3s node name to its IP in `/etc/hosts` via the Ansible `node-common` role
  (`hostname-update.yaml`); apply with `ansible-playbook playbooks/node_setup.yml --tags hosts`.
- **`primaryNameServerAddresses` empty on a catalog member** → it resolves the primary via
  DNS (→ ClusterIP/unreachable). Drive it from the catalog (§2), not per-zone.
- **NOTIFY failing on "some zones" but not others** → almost always one stale address in the
  cluster node list (§2.0), inherited by every catalog member. Zones that set their *own*
  `notifyNameServers` override the inheritance and stay healthy, which makes the failure look
  arbitrary. Zones that merely look healthy may just not have had a serial bump yet.
- **`notifyFailedFor` is persisted pending state**, not a live symptom. It keeps retrying the
  recorded address even after the config is corrected; re-`POST /api/zones/options/set` on
  each affected zone to reset it.
- **Serial-regression stall:** a secondary never transfers while its serial is **≥** the
  primary's. If a zone is recreated on the primary (e.g. a cluster rebuild resets it to
  serial 1) while the secondaries hold higher serials, it can never converge and there is no
  error — only silent divergence. Fix by raising the primary's SOA serial above every
  secondary via `POST /api/zones/records/update` (type=SOA), then resync.
- **`zones/options/set` bumps the SOA serial but does NOT send a NOTIFY,** so an options
  change alone leaves secondaries stale until their refresh timer (900 s). Force it with
  `POST /api/zones/resync` (per-zone, run on the secondary) or
  `POST /api/admin/cluster/secondary/resync?node=<name>` (full config pull, run on the primary).
- **Forwarder zones forward SOA queries,** so `dig SOA <zone>` returns nothing for the
  reverse zones — read `soaSerial` from `/api/zones/list` when comparing nodes.
- **Zone counts can differ per node — compare serials, not counts.** Only catalog members
  replicate. Disabling a zone drops it from the catalog, so secondaries *correctly* delete
  their copy while the primary keeps the disabled definition. Health check = every
  replicated zone present on all nodes at an identical `soaSerial`, and `notifyFailed=false`
  everywhere — not the zone count.
- **Legacy `"internal": true` zones on an out-of-date node.** Older builds shipped
  `localhost`, `0/127/255.in-addr.arpa` and `1.0.0…ip6.arpa` as protected internal zones that
  the API refuses to remove (`Access was denied to manage internal DNS Server zone`). They
  never replicate (`catalog: null`) and are harmless, and **updating that node to current
  Technitium removes them** — which is the fix, not deleting `.zone` files by hand. A node
  showing them is simply behind; all three nodes answer `localhost`/`127.in-addr.arpa`
  identically either way.

## Rollback

Before the authority flip: revert DHCP to Synology, remove the ArgoCD app. After: flip
Synology back to master, restore DHCP, re-point Ansible. A full pre-rebuild snapshot of the
live config (cluster state, zone options + records, certs) can be captured to
`tmp/cluster-rebuild-backup/` via the API before any destructive cluster operation.
