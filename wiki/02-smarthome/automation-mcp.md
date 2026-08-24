# Automation & MCP

[← Back to Home](../Home.md) · Smarthome: [Home Assistant](home-assistant.md) · [Database](database.md)

## n8n — `workload/smarthome/n8n`

A general workflow-automation engine, backed by the shared smarthome Postgres cluster
(see [Database](database.md)). A number of its defaults are pinned deliberately rather
than left at chart defaults, mostly to keep behavior stable across upgrades and to fit
its memory limit — for example, a compression-node size cap is set well below the
upstream default specifically because the pod's memory limit is much smaller than that
default assumes. One real production workflow lives here: a daily-daycare-report → baby
tracker automation, which is documented in detail (email trigger gotchas, dedup logic,
parsing rules) in the assistant's own memory rather than in this repo, since it concerns
household process rather than infrastructure.

**Community node policy:** any n8n community node added here should be checked for
maintenance/popularity first (recent releases, download counts) — built-in nodes (HTTP
Request, Code, Switch) are preferred whenever they can do the job, to avoid pulling in
low-trust unmaintained packages.

## ha-mcp — `workload/smarthome/ha-mcp`

An MCP (Model Context Protocol) bridge that exposes Home Assistant control to LLM/agent
tooling — this is what lets a tool like Claude Code query and control Home Assistant
directly. Runs OAuth 2.1 with Dynamic Client Registration, so each connecting MCP client
gets its own revocable login rather than sharing one static credential.

**A real, easy-to-miss bug this app had:** an encryption-key environment variable that
looked like it should control OAuth persistence had no effect at all — the app doesn't
read it. The actual persistence knob is a config-directory path that must point at
writable, persistent storage; without it, both the registered-client list and the
signing secret are regenerated on every restart, forcing every MCP client to
re-authenticate each time the pod restarts. Fixed by giving it a small persistent volume
at the right path.

**Scale-to-zero trade-off:** ha-mcp holds a long-lived MCP session, which doesn't mix
cleanly with Sablier's idle-timeout scale-to-zero — see
[Known Issues → MCP servers vs. scale-to-zero](../05-operations/known-issues.md#mcp-servers-vs-scale-to-zero).

## kubernetes-mcp-server — `workload/apps/kubernetes-mcp-server`

Not smarthome-specific, but the same "agent tooling via MCP" pattern — it's what lets
Claude Code operate this very cluster. Deliberately runs with **OAuth disabled** on the
server side: if the server advertised OAuth metadata, MCP clients using a static
Authorization header would ignore that header and try to start an interactive OAuth flow
instead — a known client-side limitation, not a design choice on this end. Access is
instead gated entirely by a Traefik Basic Auth middleware in front of it.

**Security note worth being explicit about:** the server authenticates to the Kubernetes
API using its own ServiceAccount, bound to `cluster-admin` — it does **not** forward the
caller's Basic Auth credential as a Kubernetes token. This means anyone who passes the
single Traefik Basic Auth gate gets full cluster-admin access through this server. That's
an accepted trade-off for a single-user homelab, not an oversight — but it's the reason
this page calls it out rather than glossing over it. Narrowing the blast radius later
(a scoped ClusterRole instead of cluster-admin, plus a read-only/no-destructive-actions
config flag) is possible without touching the auth-gating layer at all, if ever needed.
