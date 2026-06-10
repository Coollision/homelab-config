# Technitium DNS — bootstrap & key settings

**Version:** 15.2.0
**Topology:** 3-node cluster, all comms over **VLAN 30 MACVLAN IPs** — primary pod `<VIP-A>`,
secondary pod `<VIP-B>`, tertiary (NAS Docker) `<NAS-IP>`. Cluster nodes advertise these
MACVLAN IPs; **never ClusterIPs or node IPs**. This is the only model that lets the
off-overlay NAS node join (it can't route to k8s ClusterIPs, and a node's advertised IP
is fixed at init/join — there is no `update-primary` API).

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

### Zone-transfer ACL — every primary zone, keep CONSISTENT

AXFR from a secondary arrives sourced from that node's VLAN 30 MACVLAN IP (via the net1
routes). Allow the specific MACVLAN IPs on **every** primary zone (`pi`, `declerck.dev`,
`mylocaldns`, `thewizardofoz.win`, `cluster-catalog.mylocaldns`). A per-zone omission
silently breaks just that zone's transfer (and if it's `mylocaldns`, DANE breaks too):

```http
POST /api/zones/options/set
  zone=<zone>  zoneTransfer=UseSpecifiedNetworkACL
  zoneTransferNetworkACL=<VIP-A>          # primary MACVLAN (promotion/self)
  zoneTransferNetworkACL=<VIP-B>          # secondary pod
  zoneTransferNetworkACL=<NAS-IP>         # tertiary (NAS)
  zoneTransferNetworkACL=10.42.0.0/16     # pod overlay (harmless fallback)
  # + <SYNO-IP> (Synology slave) / <REMOTE-MASTER-IP> per zone as needed
```

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
  zone=pi  updateSecurityPolicies=dnsUpdateAnsible|pi|any
```

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
`recursion=UseSpecifiedNetworkACL` and list the RFC-1918 ranges **plus** each `/64`
explicitly in `recursionNetworkACL`.

---

## 6. Synology slave (UI)

Flip the local master zones from master → slave on Synology, master = `<VIP-A>`,
TSIG = `secundaryZoneUpdate`. Leave the remote-mastered zones as-is.

## 7. Ansible cutover

Update `dns_server_ip` in `ansible/inventory/group_vars/` to `<VIP-A>`, then run
`ansible-playbook -i inventory playbooks/dns.yml`.

## 8. UniFi DHCP

DNS servers: **`<VIP-A>`, `<VIP-B>` only** — no Synology (it can't ad-block; see README).

---

## Gotchas

- **k3s node-name NXDOMAIN noise:** the k3s node names (`worker-<x>`/`master-<x>`) differ
  from the DNS records (`node-<x>.<zone>`) and the OS hostname (`node-<x>`). kubelet resolves its
  own k3s node name every ~10 s → NXDOMAIN against these resolvers. Fixed by mapping each
  k3s node name to its IP in `/etc/hosts` via the Ansible `node-common` role
  (`hostname-update.yaml`); apply with `ansible-playbook playbooks/node_setup.yml --tags hosts`.
- **`primaryNameServerAddresses` empty on a catalog member** → it resolves the primary via
  DNS (→ ClusterIP/unreachable). Drive it from the catalog (§2), not per-zone.

## Rollback

Before the authority flip: revert DHCP to Synology, remove the ArgoCD app. After: flip
Synology back to master, restore DHCP, re-point Ansible. A full pre-rebuild snapshot of the
live config (cluster state, zone options + records, certs) can be captured to
`tmp/cluster-rebuild-backup/` via the API before any destructive cluster operation.
