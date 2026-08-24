# Certificates

[← Back to Home](../Home.md) · Platform: [Networking](networking.md) · [Secrets — Vault](secrets-vault.md)

`system/cert-manager/cert-manager` wraps the upstream cert-manager chart with one
specific job: issue a single wildcard TLS certificate for the internal/external domain,
via Let's Encrypt + a Cloudflare DNS-01 challenge, and hand it to Traefik as the default
cert for everything.

- **DNS-01, not HTTP-01**, using a Cloudflare API token sourced from Vault — this is what
  lets a single wildcard cert cover every subdomain without a separate challenge per
  ingress.
- **Recursive nameservers are pinned to public resolvers** (not the cluster's own
  internal DNS) for the ACME challenge lookup — deliberately routes around the
  split-horizon/ad-blocking internal DNS so a challenge can't be silently swallowed by an
  internal-only answer.
- The resulting wildcard `Certificate`'s Secret is consumed directly by the internal
  Traefik's `TLSStore` as the cluster-wide default — issue once here, serve everywhere.

That's the whole design: one `ClusterIssuer`, one wildcard `Certificate`, one consumer.
