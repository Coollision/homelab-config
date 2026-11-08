# Homepage

[← Back to Home](../Home.md) · See also: [Misc apps](misc-apps.md)

Landing page for the lab: [gethomepage.dev](https://gethomepage.dev) in `workload/apps/homepage`, reachable on the
internal secure ingress only (it lists every service and has no login of its own).

## How apps get on it

Stateless, **no volume**: the config files are one ConfigMap in `values.yaml`, mounted file by file (`subPath`) with an
`emptyDir` over `/app/config/logs`.

- **Auto-discovery.** `shared-lib` renders `gethomepage.dev/*` annotations on the IngressRoute of every app that has
  `ingress_internal_secure` (or only `ingress_internal`). Homepage reads them through its Traefik integration
  (`kubernetes.yaml`: `traefik: true`). A new app shows up without touching Homepage.
- **Tuning per app** with a top-level `homepage:` block in that app's `values.yaml`:
  `name`, `group` (default: the namespace), `icon`, `description`, `href`, `widget` (map, see below), `hidden: true` (opt out: MCP endpoints, scratch apps).
- **Always rendered:** `href` (Homepage never derives it from `Host()`) and a `pod-selector` so the status dot works.
- **Not discoverable** (hand-written IngressRoutes: Grafana, Prometheus, Alertmanager, Longhorn, Vault, ArgoCD, Traefik;
  anything off-cluster) lives in `services.yaml` inside the ConfigMap.

## Off-cluster cards

Things on the NAS (Gobelijn) are hand-written in `services.yaml` under `NAS`: DSM, Netboot.xyz
(console :3000) and the docker.io registry mirror (:7777, API only), all Docker; plus Plex (DSM package, :32400, link and
status only: its token is account-wide, so no widget). Status is a plain `siteMonitor` from the pod;
the NAS address comes from the `HOMEPAGE_VAR_GOBELIJN_IP` env var (Vault `kv/shared/network~gobelijn-ip`), never git.

aws-tunnels (a plain subchart, so not discoverable) has two hand-written cards in `Tools`: the SSO login page (status
via the in-cluster service) and the work GitLab (no status check, every poll would cross a rate-limited SSM session).

## Scale-to-zero apps

Sablier is a Traefik middleware, so nothing Homepage does through the Kubernetes API wakes an app. Homepage cannot
turn pod status off per service, so the lib always renders the real `pod-selector`: a gated app shows running while
awake and "not found" while asleep (the pod count is the only signal; one error line per poll in Homepage's log).
**Never give a gated app a widget or `siteMonitor`**: widget URLs point at the in-cluster Service, which is empty
while asleep (ArgoCD, Longhorn UI, peanut, cloudbeaver, esphome, ha-mcp, vscode-server, kubernetes-mcp-server).

## Widgets and secrets

The repo is public, so annotations and `services.yaml` never carry keys. A widget references an env var
(`{{HOMEPAGE_VAR_SONARR_KEY}}`); the value comes from the `homepage-keys` Secret, filled from Vault
(`kv/workload/arr-stack/<app>~api-key`). Widgets are set per app in its `homepage.widget` block (type, in-cluster
`url`, `key`) and exist for the arr apps, Jellyseerr, Tautulli and slskd. Metric tiles use the native
`prometheusmetric` widget against Prometheus: the tiles in `services.yaml` (cluster health/load, UPS, Longhorn,
CNPG, Mosquitto, Sablier, NAS; every tile links to its dashboard or UI) reuse the queries from the starred Grafana dashboards, and the
`Dashboards` bookmarks link to those dashboards. ArgoCD, Longhorn and peanut (UPS) are one card each in Infra (link and live numbers together; peanut's discovered
card is hidden so the hand-written one with the numbers is the only one). The ArgoCD numbers read `argocd_app_info` from the application
controller (ServiceMonitor in `argocd/values.yaml`), so it works while argocd-server is asleep. Technitium uses a dedicated read-only user (`homepage`, own API token in `kv/workload/apps/homepage~technitium-token`).
Technitium's implicit `Everyone` group (every user is in it) was stripped of View on every section except Dashboard
(2026-11-09), so the token can read dashboard stats and nothing else: Zones, Logs, DhcpServer, Settings and
Administration are all denied. That state lives in Technitium's own data, **not in git**: a rebuilt primary starts with
the defaults again (Everyone can view Zones/Cache/Allowed/Blocked/Apps/DnsClient/DhcpServer/Logs), so redo it with
`admin/permissions/set` (`groupPermissions=Everyone|false|false|false` per section).
UniFi: the key (`unifi-key`) belongs to a dedicated local admin so it can be revoked on its own; the console offers
no role choice, so treat it as read-write. The console's certificate is self-signed with no IP SAN, so Homepage reaches
it through `templates/unifi-proxy.yaml` (Traefik, wildcard cert, `unifi.<internal-domain>`). The gateway address is in
Vault (`unifi-gateway`), not git.
Home Assistant has no widget on purpose: it keeps its own dashboard and there is nothing worth surfacing here, so
no long-lived token exists for Homepage (HA tokens cannot be scoped read-only anyway). Its card is just the link and
pod status from auto-discovery.

## Auth

Homepage's own login is off: the page is internal-only and LAN access is the control. Before ever exposing it
externally, enable Homepage v2 auth (`HOMEPAGE_AUTH_ENABLED`, password or OIDC).

## Gotchas

- `HOMEPAGE_ALLOWED_HOSTS` must list the served host; probes send `Host: localhost:3000` for that reason.
- RBAC (`templates/rbac.yaml`) is read-only; the pod uses the `homepage` ServiceAccount via the lib's `serviceAccountName`.
- The ConfigMap checksum is taken before Vault placeholders resolve, so rotating a Vault value does not restart the pod.
- No ArgoCD widget: argocd-server is scale-to-zero. Use Prometheus `argocd_app_info` (needs a controller ServiceMonitor).
