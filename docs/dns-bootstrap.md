# Technitium DNS — bootstrap & key settings

**Version:** 15.2.0  
**Known v15 quirks:** TSIG API rejects non-empty secrets (configure keys via UI); clustering
heartbeat logs `InvalidTokenHttpApiClientException` but sync still works (GitHub #1848).

All API calls: `http://<VIP>:5380/api/<endpoint>?token=<T>`. Get a token via
`GET /api/user/login?user=admin&pass=<pass>`.

---

## 1. Cluster setup

### Init (primary, once)

```http
POST /api/admin/cluster/init
  clusterDomain=mylocaldns
  primaryNodeIpAddresses=<VIP-A>
```

### Join secondary

```http
POST /api/admin/cluster/primary/join   (on primary)
  secondaryNodeId=<integer>
  secondaryNodeUrl=https://technitium-secondary.mylocaldns:53443/
  secondaryNodeIpAddresses=<VIP-B>
  secondaryNodeCertificate=<base64url-DER-cert>
  user=admin  pass=<pass>
```

Fetch the secondary cert first:

```bash
openssl s_client -connect <VIP-B>:53443 -showcerts </dev/null 2>/dev/null \
  | openssl x509 -outform DER | base64 | tr '+/' '-_' | tr -d '=\n'
```

### ⚠ Patch cluster.config after init (critical)

Technitium stores node IPs in `/etc/dns/cluster.config` and restores them into the
`mylocaldns` A records on every restart. Without patching, those IPs are VLAN 30 IPs;
NOTIFY/AXFR traffic gets SNAT'd through node IPs and refused by the overlay ACL.

```bash
# On both pods — replace VLAN 30 IPs (hex) with ClusterIPs (hex).
# Convert: python3 -c "import ipaddress; print(ipaddress.ip_address('<IP>').packed.hex())"
kubectl exec -n dns technitium-primary-0 -- \
  perl -pi -e 's/<VIP-A-4bytes>/<PRIMARY-CLUSTERIP-4bytes>/g;
               s/<VIP-B-4bytes>/<SECONDARY-CLUSTERIP-4bytes>/g' /etc/dns/cluster.config
# same on secondary-0, then:
kubectl delete pod -n dns technitium-primary-0 technitium-secondary-0 \
  --force --grace-period=0
```

Verify: `mylocaldns` A records should resolve to ClusterIPs after restart.

---

## 2. Key settings applied post-init

These are the non-default settings that make the cluster work correctly. Apply on
the **primary** (most sync to secondary via clustering).

### Secondary server — `notifyAllowedNetworks`

NOTIFY arrives from the primary's **pod IP** (flannel overlay source), not the
ClusterIP (ClusterIPs are destination-only VIPs). Set this so the secondary accepts
NOTIFYs from the pod CIDR:

```http
POST /api/settings/set   (on secondary)
  notifyAllowedNetworks=10.42.0.0/16
```

### All primary zones — zone transfer ACL

AXFR requests from the secondary arrive with source = secondary pod IP (`10.42.x.x`).
Set on every primary zone (`pi`, `declerck.dev`, `mylocaldns`, `cluster-catalog.mylocaldns`):

```http
POST /api/zones/options/set
  zone=<zone>
  zoneTransfer=UseSpecifiedNetworkACL
  zoneTransferNetworkACL=10.42.0.0/16       ← pod overlay (cluster secondary)
  zoneTransferNetworkACL=<SYNO-IP>          ← Synology slave
```

### Synology NOTIFY

Add `notifyNameServers=<SYNO-IP>` to each zone you want the Synology to slave:

```http
POST /api/zones/options/set
  zone=<zone>  notify=ZoneNameServersAndSpecifiedNameServers
  notifyNameServers=<SYNO-IP>
  zoneTransferTsigKeyNames=secundaryZoneUpdate
```

---

## 3. TSIG keys (web UI only — API bug in v15)

Settings → TSIG Keys → Add on the **primary** (syncs to secondary automatically):

| Key name              | Algorithm | Secret source                           |
|-----------------------|-----------|-----------------------------------------|
| `dnsUpdateAnsible`    | HMAC-MD5  | `tmp/zonefile/dnsUpdateAnsible`         |
| `secundaryZoneUpdate` | HMAC-MD5  | `tmp/zonefile/secundaryZoneUpdate`      |

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

POST /api/blocklist/update/start   ← force immediate download
```

Syncs to secondary via clustering. See `system/dns/technitium/README.md` for how to
manage the allowlist and check blocked queries.

---

## 5. Zone seeding

```http
# Create primary zone and add to cluster catalog:
POST /api/zones/create
  zone=<zone>  type=Primary  catalog=cluster-catalog.mylocaldns

# Import records from BIND zone file:
POST /api/zones/import
  zone=<zone>  overwrite=true   (body: BIND zone file text)

# Create secondary zone (e.g. remote-mastered pi1, declerck.cool):
POST /api/zones/create
  zone=<zone>  type=Secondary
  primaryNameServerAddresses=<REMOTE-MASTER-IP>
  zoneTransferProtocol=Tcp  tsigKeyName=secundaryZoneUpdate
  catalog=cluster-catalog.mylocaldns
```

---

## 6. Synology slave (UI)

Flip `pi` and `declerck.dev` from master → slave on Synology, master = `<VIP-A>`,
TSIG = `secundaryZoneUpdate`. Leave `pi1`/`declerck.cool` as-is.

---

## 7. Ansible cutover

Update `dns_server_ip` in `ansible/inventory/group_vars/` to `<VIP-A>`, then run:

```bash
ansible-playbook -i inventory playbooks/dns.yml
```

---

## 8. UniFi DHCP

DNS servers: **VIP-A, VIP-B only** — no Synology (it can't ad-block; see README).

---

## Rollback

Before step 7: revert DHCP to Synology, remove ArgoCD app.  
After step 7: flip Synology back to master, restore DHCP, re-point Ansible.
