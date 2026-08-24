# Smarthome database

[← Back to Home](../Home.md) · Smarthome: [Home Assistant](home-assistant.md) · [Automation & MCP](automation-mcp.md) · Platform: [Databases](../01-platform/databases.md)

`workload/smarthome/smarthome-db-cnpg` is a single-instance CloudNativePG Postgres
cluster shared by two consumers: **n8n** and **Home Assistant's recorder**. See
[Platform → Databases](../01-platform/databases.md) for the general CNPG pattern this
follows (declarative roles, `Database` CRs with a retain-on-delete policy, a pre-created
named Longhorn volume).

Postgres parameters here are tuned from actual query-plan evidence rather than carried
over from assumptions about the previous database engine — a `work_mem` increase that
fixed a specific slow `GROUP BY` query in Home Assistant's energy dashboard was kept, a
larger buffer-pool-style setting that showed no reproducible benefit on this workload was
tried and then reverted. The lesson, if you're ever tuning this cluster further: trust a
deterministic query-plan change over noisy before/after timing on a live, contended
cluster — see [Known Issues → HA recorder migration](../05-operations/known-issues.md#ha-recorder-to-cnpg-migration)
for the full tuning narrative.
