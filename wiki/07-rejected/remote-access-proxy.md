# Rejected — Browser-only remote access through an mTLS forward proxy

[← Back to Rejected ideas](README.md) · Related: [Networking](../01-platform/networking.md), [DNS](../01-platform/dns.md), [Single sign-on](sso.md)

> **Status (2026-11-23):** rejected after a working spike. The server side is fine; the **client device** is the
> problem. The spike manifests stay on the `tests` branch as `tests/rp-spike/disabled-rp-spike` for reference.
> The NAS real-client-IP question that came out of the same discussion is **parked, not rejected** (last section).

## The idea

Reach internal sites and LAN IPs from a device where nothing can be installed (no VPN client), with native browsing
(a PAC file or proxy setting), authenticated once, and without sending private traffic through Cloudflare.
Design: a TLS endpoint on a public port that demands a **client certificate** (private CA, one cert per device),
in front of a Squid `CONNECT` proxy limited to an allowlist of internal names, with an optional Slack approval gate.

## What the spike proved

Built on the `tests` branch (nginx stream + Squid in one pod, own LoadBalancer IP, public port forward), tested
from the LAN with a real Let's Encrypt server cert:

| Request | Result |
|---|---|
| No client cert | refused at the TLS handshake |
| Cert from a different CA | refused, logged as `self-signed certificate` |
| Valid device cert, allowed host | tunnel opens, HTTP 200 |
| Valid device cert, host not on the allowlist | 403 from Squid |

So the mechanism works. Findings worth keeping if a similar proxy is ever built:

- **`ubuntu/squid` is built with GnuTLS and does not enforce client certs.** With `tls-cafile=` set, clients with no
  cert and clients with a cert from the wrong CA were both let through (an open proxy). The `user_cert` ACL does not
  exist in that build. Enforce mTLS in front of Squid instead.
- **nginx `stream` with `ssl_verify_client on` enforces it at the handshake** and also supports `ssl_crl`
  (revocation) and logs the cert subject and source IP per connection. Traefik's `TLSOption` has no CRL/OCSP check
  and cannot pass the cert identity through a TCP route.
- **Squid OOMKills in a container** unless `max_filedescriptors` is capped (the container reports about 1M fds and
  Squid sizes memory off that). 4096 and a 256Mi limit were fine.
- Squid options differ in v6: `clientca=` is `tls-cafile=`; logging to `/dev/stdout` fails for the `proxy` user.
- Chrome and Firefox support an HTTPS proxy only through a PAC file (`HTTPS host:port`). Safari/iOS effectively do
  not. Firefox has an open bug (1129521) where CORS requests are sent without the client cert.
- A new app on the `tests` ApplicationSet can come up with auto-sync disabled; a one-time sync operation needs
  `CreateNamespace=true` repeated in its `syncOptions`.

## Why it lost

- **The target device is a locked-down corporate PC** (TLS-inspecting secure web gateway, custom root CA, managed
  browser policy). On such a machine the proxy setting and PAC file are visible to the endpoint agent and policy,
  every connection to the proxy name and port is logged by the gateway, and the proxy setting or certificate import
  may be locked. Pointing a managed work browser at a private tunnel to a home network is also the kind of thing
  an employer's acceptable-use policy prohibits, and it is not worth putting the user's job at risk for convenience.
- The remaining design cost (private CA and revocation runbook, port forward on the public internet, Squid hardening
  against SSRF into the LAN, Slack approval gate) is only justified if the client side works.

## What would reopen it

A personal, unmanaged device where a PAC file and a client certificate can be installed. At that point a normal VPN
client (WireGuard) is simpler and safer than this proxy, so this idea only matters for a device where a VPN app is
impossible but browser settings are free, which is rare.

## Parked: let the cluster be the public front door while the NAS keeps real client IPs

Separate wish from the same discussion, not built, not rejected:

- DSM has a **Trusted Proxies** setting (Control Panel > Security) where the Cloudflare ranges are already listed.
  Adding the node IPs would make DSM honour a forwarded client IP, but **only if Traefik terminates TLS for the NAS
  name and re-encrypts** (a TLS passthrough cannot add `X-Forwarded-For`).
- Cheap test when this is revisited: add the node IPs to the DSM Protection **allow list first** (failed logins
  through Traefik count against the node IP, and Auto Block could cut the cluster's SMB/NFS), then add them as
  trusted proxies, send one request through an HTTP-terminating Traefik route from a LAN client and read which IP
  Log Center records.
- IPv6 (no NAT) is the other way to keep real IPs on both the NAS and the cluster.
