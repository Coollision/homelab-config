# Rejected ideas

[← Back to Home](../Home.md)

Ideas that were researched or discussed and deliberately **not** built. Each page records what the idea was,
why it lost, and what would have to change to reopen it, so the same question is not re-derived later.

| Page | Idea | Why not |
|---|---|---|
| [Spegel embedded registry](spegel-embedded-registry.md) | k3s' peer-to-peer image mirror, possibly replacing kube-fledged | Only copies images some node already holds; does nothing for failover or an offline internet |
| [Single sign-on](sso.md) | One identity provider (Authelia, Pocket ID, Authentik) in front of the apps | Per-app permissions stay per-app, key apps keep native login, LAN migration cost; an external-ingress-only gate is parked, not rejected |
| [Technitium secondary headless](technitium-secondary-headless.md) | Run the DNS secondary without its web UI | The UI port is also the cluster sync API, and the UI costs nothing measurable |
