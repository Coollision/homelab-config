# TODO — Browser-only remote access to internal sites (forward proxy) and a cluster-first front door

[← Back to Home](../Home.md) · Related: [Networking](../01-platform/networking.md), [DNS](../01-platform/dns.md), [cert-push](../01-platform/cert-push.md), [Rejected: SSO](../07-rejected/sso.md)

> **Status (2026-11-19):** design discussion only, nothing built. Everything marked *verify* is untested on this
> cluster. The first step is a read-only experiment on the `tests` branch (below); no live change is needed for it.

## Goal

Reach internal sites and LAN IPs from a device where nothing can be installed (no VPN client), with **native
browsing** (a browser proxy setting or PAC file, not a remote-desktop gateway), authenticating once. Private traffic
must not be proxied through Cloudflare.

A second, separate wish: have the cluster become the first entrypoint for public traffic (bans handled there) **while
the NAS still sees real client IPs**, because the NAS's own logs and auto-block need them.

## Decisions so far

| Question | Outcome |
| --- | --- |
| Put the proxy behind the Cloudflare tunnel? | **No.** Browser proxies speak `CONNECT`; the proxied tunnel only carries HTTP/WebSocket. The Cloudflare options that carry raw TCP (WARP, `cloudflared access`) need a client. Cloudflare Access was also already rejected ([SSO](../07-rejected/sso.md): local first). |
| Open forward proxy with a password only? | **No.** It is an SSRF-by-design service; one auth failure is a pivot into the LAN. |
| Auth | **Client certificate (mTLS)**. A scanner without the cert gets a TLS alert and cannot tell it is a proxy. Cert per device from a private CA (CA key in Vault, `.p12` in the password manager). Basic auth + `probe_resistance` was considered and set aside. |
| Who is the proxy | Traefik **cannot** forward-proxy. It can only be the mTLS front door at TCP level (`IngressRouteTCP`, TLS terminate, `TLSOption` with `RequireAndVerifyClientCert`); the proxy itself is Squid (or tinyproxy) behind it. |
| DNS | Free: the proxy resolves hostnames through Technitium, so split-horizon names, wildcard certs and raw LAN IPs all work ([DNS](../01-platform/dns.md)). |
| Hostname | A non-proxied (grey-cloud) record on a dedicated name; the real name stays in Vault (`<proxy-host>` here). |
| Extra gate | Slack approve button (see below). |

## Target design

```text
browser (PAC / proxy setting, client cert)
   │  TLS + client cert, <proxy-host>:<port>
   ▼
UniFi port-forward ──▶ dedicated Traefik (own MetalLB IP, "traefik-proxy")
                         IngressRouteTCP, TLS terminate, mTLS required
                         ▼
                       Squid (NetworkPolicy, destination allowlist,
                              external-ACL helper = Slack approval)
                         ▼
                       internal hostnames / allowed RFC1918 ranges only
```

- **Dedicated Traefik instance**, not a new entrypoint on the internal one: keeps the repo's isolation rule (one
  instance per exposure level, see [Networking](../01-platform/networking.md)).
- **Squid hardening:** allowlist of internal domains and chosen ranges only; deny the proxy's own admin ports, the
  cluster API and everything else; NetworkPolicy and ideally a firewall rule limiting what the pod can reach; rate
  limits; failure-only logs to Loki (same pattern as the external Traefik).
- **Browser setup:** PAC file or the browser's own proxy settings; a portable Firefox avoids a locked system-wide
  proxy setting. The client cert must be importable in that browser (*verify per device*).

### Slack approval gate (later phase)

- A cert alone is no longer enough: Squid's `external_acl_type` helper asks a small approval service whether the
  client is currently approved; if not it denies and posts a Slack message (time, source IP, GeoIP guess) with
  buttons for 1 h / 4 h / session and Deny.
- **Use Slack Socket Mode** for the buttons: the service connects outbound, so no public callback URL is needed.
- State is an in-memory or Redis TTL entry keyed by source IP (and cert CN if one cert per device).
- Rate-limit the notifications (for example once per 10 minutes per IP). Fail closed if Slack or the service is down;
  LAN access is unaffected.
- Caveat: IP-keyed approval is shared by anyone behind the same address (CGNAT, hotel Wi-Fi); keep windows short.
  Behind Traefik, Squid only sees the real source IP if Traefik sends PROXY protocol v2 to it (*verify*, Squid supports it).

## Which port: the 443 question

Today public 443, 5001 and 6690 are forwarded to the NAS. UniFi cannot split by hostname, so the proxy either gets
its own port or takes over 443.

| Option | What changes | Cost |
| --- | --- | --- |
| **A. Separate port (for example 8443)** | One new UniFi forward to the new LB IP. NAS untouched. | Some work or guest networks only allow outbound 80/443. |
| **B. Traefik takes 443 and splits by SNI** | New LB IP; `IngressRouteTCP` with the proxy hostname terminating mTLS and a `HostSNI(*)` **passthrough** to the NAS; UniFi 443 forward moves to the new LB IP (5001 and 6690 stay on the NAS). | The NAS loses real client IPs (everything arrives from a node IP), and its external 443 now depends on the cluster. |

- **Port 80 is not a blocker:** cert-manager issues the certificate and [cert-push](../01-platform/cert-push.md) pushes
  it to the NAS, so the NAS needs no inbound 80 for renewals. Re-check that the push job targets the NAS LAN address,
  not the public name (*verify*).
- **DSM allow list (UI only):** Control Panel > Security > Protection > Allow/Block List > Create > Allow list.
  Menu names vary by DSM version (*verify*). Entries must be the **node IPs** (or the subnet covering them), because
  pod traffic to the LAN is source-NATed to the node IP; or give Traefik a fixed source via a Multus MACVLAN leg.
  This only prevents lockout; per-client auto-block is blind for everything arriving through Traefik. Keep DSM
  account lockout and 2FA on.
- **Plan:** prove the proxy on a separate port first. Move 443 only as its own reversible change (UniFi rule rollback),
  and only if the NAS real-IP question below has a good answer.

## Requirement: the NAS must keep seeing real client IPs

A passthrough hides the client address and Traefik cannot add headers to it, so Option B only works if DSM can use
a forwarded address. **Not established:** an attempt to find whether DSM has a "trust proxy" setting for
`X-Forwarded-For`, or accepts PROXY protocol on 443/5001, returned nothing sourced (search engines blocked, Synology
KB pages not readable). Treat it as unknown.

### Experiment (read-only for the NAS, run on a machine with cluster access)

Goal: learn which IP DSM records when the request comes through an HTTP-terminating Traefik route that adds
`X-Forwarded-For`.

1. On the `tests` branch, add a scratch app under `tests/<ns>/<name>/` (copy a small neighbour; see
   [tests/README.md](https://github.com/Coollision/homelab-config/blob/master/tests/README.md)). It needs a Service
   that points at the NAS LAN address (ExternalName or Endpoints) and an `IngressRoute` on **`traefik-external`** or the
   internal Traefik with a test hostname that resolves only for you, forwarding HTTPS to the NAS with
   `insecureSkipVerify` (same trick as `serversTransport` in `lib/shared-lib`). Use `<secret:kv/...>` placeholders for
   hostnames and IPs; nothing private in git.
2. Run `scripts/validate-charts.sh`, push the `tests` branch, wait for the `tests` ApplicationSet to sync.
3. From a network **outside** the LAN (phone on mobile data), open the NAS login through the test route and enter a
   wrong password once or twice. Do not trigger a lockout on a real account.
4. In DSM, read Log Center (connection/login logs) and the Auto Block list (only read, do not change anything) and
   note the client IP it recorded: the real one or the Traefik node IP.
5. Record the result here, then remove the scratch app (rename to `disabled-<name>` or delete it on the `tests` branch).

| Result | Meaning |
| --- | --- |
| Real client IP logged | DSM honours the header. Option B is open; document the exact DSM setting. |
| Traefik node IP logged | DSM cannot be made to read forwarded IPs. Option B is out for the NAS. |

Also worth checking on the NAS itself (read-only): whether the DSM nginx config mentions `real_ip` or
`proxy_protocol`, and Control Panel > Login Portal > Advanced / Reverse Proxy for any trusted-proxy option.

### If DSM cannot read the forwarded IP

- Keep public 443 forwarded straight to the NAS and run the cluster's ban logic **beside** the NAS, not in front of it
  (for example CrowdSec reading logs or UniFi events and pushing bans to the UniFi firewall, which sits in front of
  both).
- IPv6, where clients have it: no NAT, so the NAS and the cluster can each have a public address and see real client
  IPs. IPv4 keeps the single-address problem.
- A source-preserving path (routing/DNAT without source NAT through a node, with the NAS replying via it) is not a
  Traefik feature and is fragile; not recommended.

## Steps

- [ ] Run the NAS real-IP experiment above and record the result here.
- [ ] Decide A (separate port) or B (443 via Traefik) from the result and from which outbound ports the target
      networks allow.
- [ ] Private CA and per-device client certs (CA key in Vault); confirm the target browser can import a cert.
- [ ] Scratch build on the `tests` branch: Squid with allowlist and NetworkPolicy, reachable from the LAN only.
- [ ] Add the dedicated Traefik with the mTLS `TLSOption`, LAN-only test.
- [ ] Add the UniFi port-forward and test from outside (phone on mobile data) with and without a client cert.
- [ ] Slack approval gate (Socket Mode), notification rate limit, fail-closed test.
- [ ] Promote with `/promote-test-app`, add the real wiki page under `01-platform` and move the decisions there.
- [ ] If Option B: add the DSM allow list entries and write the UniFi forward rollback down first.
