# Rejected — Single sign-on (SSO)

[← Back to Rejected ideas](README.md) · Related: [Future ideas](../06-todo/future-ideas.md), [Networking](../01-platform/networking.md)

> **Status (2026-11-09):** full SSO rejected after a read-only inventory of every app's auth setup and a comparison
> of the candidates. A narrower option, an auth gate on the external ingress only, is not rejected; it is parked
> below with what it would cost and cover.

## Requirements any auth setup must meet

- **Local first.** Everything on the LAN keeps working without internet access. That rules out hosted gates such as
  Cloudflare Access, even though they would cost nothing to run.
- **Users managed in a UI.** Users rarely change, but if there is an identity provider, adding a user on the fly must
  be possible without a commit. YAML-only users (plain Authelia) do not qualify on their own.
- **No VPN-only access.** Some people get access to a single app, not to the home network, so external access stays
  on a public ingress.

## The idea

One identity provider in front of the self-hosted apps: one login, central user management, and a second factor
in a single place. Candidates looked at: Authelia (+ LLDAP for the user UI), Pocket ID, Authentik, Casdoor (tried
before), Kanidm.

## Why full SSO lost

- **Per-app permissions stay per-app.** Immich's OIDC can auto-create accounts and set role and storage quota from
  claims (only at creation), but the docs describe no mapping of IdP groups to what a user can see, so album and
  library access would still be set inside the app. SSO saves the account step, not the access setup.
- **The apps that matter most keep their native login anyway.** Vault, Vaultwarden, Home Assistant, DSM and ArgoCD
  all support OIDC (verified in their docs), but SSO adds convenience, not security: Vaultwarden still requires the
  master password, Home Assistant has built-in MFA, and the native logins must stay as break-glass because
  forward-auth on every app makes the IdP a single point of failure for the whole LAN.
- **The user-UI requirement makes it two components.** Authelia alone has no user UI, so it means Authelia + LLDAP.
  The single-component alternatives are either heavy (Authentik, too much for the memory-tight nodes) or only do
  OIDC (Pocket ID, which needs a separate proxy for apps without OIDC).
- **Several apps have no usable OIDC.** n8n's OIDC is Enterprise-only (community add-ons exist, licensing unclear),
  Seerr's OIDC is experimental on a preview image only, and Babybuddy's reverse-proxy header login is
  community-documented with staff-status and mobile-app caveats. Forward-auth can still sit in front of these as an
  extra gate, but then it is a second login, not a single one.
- **Real migration cost on the LAN.** The HTTP-only internal ingresses (Grafana, Vault, Prometheus, Alertmanager,
  Traefik dashboard) would have to move to HTTPS because the session cookie is `Secure`. API paths used by phone apps
  and integrations would need bypass rules, and Sablier ordering would need care.
- **The gain is mostly the admin's own convenience.** With 4–5 stable users, central user management solves nothing.
  What SSO would really save is one admin logging into many UIs, which a password manager already handles.

## Parked: an auth gate on the external ingress only

The review that led to this page found that the reasoning above applies to SSO *everywhere*, not to the apps that
are already reachable from the internet. Five apps have an `ingress_external_secure` on `traefik-external` and rely
only on their own login: Home Assistant, Vaultwarden, Jellyseerr, Babybuddy and the wedding geocache.

A self-hosted gate (Authelia + LLDAP) on the external ingress only would meet all three requirements, and most
objections to full SSO fall away:

- **No LAN dependency.** Internal ingresses are untouched, so an IdP outage only breaks remote access and no
  break-glass is needed.
- **No HTTPS migration.** The external ingress is already HTTPS; the HTTP-only ingresses are all internal.
- **No OIDC needed.** Forward-auth works in front of any app, on top of the app's own login.

What it would actually protect is small:

| App | Behind the gate? | Why |
| --- | --- | --- |
| Home Assistant | No | The companion app breaks behind forward-auth; its API paths would need a bypass, which removes most of the protection. Native login + MFA stays. |
| Vaultwarden | No | Same problem with the Bitwarden apps. Master password + 2FA stays. |
| Wedding geocache | No | Guest-facing, must stay open. |
| Jellyseerr | Yes | Web UI only. |
| Babybuddy | Probably | Web UI yes; the mobile app may need an API bypass (unverified). |

So today the gate covers about two apps, at the cost of running and upgrading two extra components. That is why it
is parked rather than planned. Each newly exposed app makes it more worthwhile.

## What would reopen it

- **Edge gate:** a third or fourth app without strong native auth goes on the external ingress, or someone
  outside the household needs access to an app whose own login is weak. Then the pick is Authelia + LLDAP on
  `traefik-external` only (YAML config in this repo, LLDAP for the user UI, small footprint; Authelia's OIDC is
  certified but still labelled beta).
- **Full SSO:** many users or users that change often, or a shared login across many apps becoming a daily
  annoyance. Same pick, extended to internal ingresses with the migration work above.

Memory figures for these candidates are estimates, not measured. Home Assistant stays on its native login either way
(the mobile app's OIDC support is unverified).
