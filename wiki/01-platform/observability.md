# Observability

[← Back to Home](../Home.md) · Platform: [Cluster housekeeping](cluster-housekeeping.md) · [Power — NUT](power-nut.md)

Three pieces feed one Grafana: metrics, logs, and a couple of SNMP/UPS exporters.

## Metrics — `system/monitoring/monitoring`

Wraps `kube-prometheus-stack`, aliased `promstack`, plus two hand-added exporters
(`armexporter` for ARM-board metrics, `speedtestExporter` on a 5-minute interval). This
cluster deliberately does **not** run Thanos — if you see Thanos-related release notes on
a version bump, they're not relevant here; see
[Known Issues → no Thanos in use](../05-operations/known-issues.md#no-thanos-in-use).

Heavily tuned for a resource-constrained homelab rather than left at chart defaults:
aggressive `metricRelabelings` drop high-cardinality Go-runtime and process metrics from
nearly every scrape target (kube-apiserver, kube-scheduler, node-exporter, kubelet/cAdvisor,
etc.), Prometheus itself runs with `native-histograms` enabled and a capped
`query.max-samples`/`query.max-concurrency`, and storage is a 20Gi Longhorn PVC with
90-day retention.

**Generic scrape-in mechanism:** any pod/service annotated `prometheus.io/scrape: "true"`
(plus `~port`/`~path`/`~scheme`) gets auto-discovered across all namespaces except
`kube-system`/`prom` — this is how most workload apps opt into being scraped without a
dedicated `ServiceMonitor` per app.

## Logs — `system/monitoring/logging`

Loki (single-binary mode) + Grafana Alloy as the shipping agent, running as a DaemonSet
that tails both the systemd journal (`/var/log/journal`) and **every pod's logs**
cluster-wide. Two extra LoadBalancer-exposed syslog listeners feed the same Loki instance
from outside Kubernetes entirely: TCP :10514 for the Synology NAS, UDP :10555 for the
UniFi controller.

**A real incident shaped the current label config:** CloudNativePG's pod labels once
pushed a log stream past Loki's default 15-label-per-stream limit, silently dropping
*all* Postgres logs with a 400 the retention/monitoring layer never surfaced as an
alert. Fixed with an explicit `labeldrop` rule plus raising the limit to 20 for headroom
— worth remembering if logs from a *new* CNPG cluster ever seem to just vanish.

Loki retention is ~7 days (`retention_period: 90d` is configured but storage is
filesystem-only, single `replication_factor: 1` — homelab-scale by design, not
accidental). **For anything Matter/Thread-related, Loki is the first place to look**, not
in-pod logs — the matter-server/thread-border-router pods get restarted often enough
during mesh troubleshooting that in-pod logs are frequently already gone; see
[Known Issues → the Matter mesh](../05-operations/known-issues.md#the-matter-thread-mesh-is-fragile-by-design).

## Grafana

Grafana lives inside the `monitoring` chart, wired to both Prometheus and Loki
(`additionalDataSources` points at the Loki gateway Service). Dashboard/alert
ConfigMap sidecars scan **all namespaces**, so a dashboard shipped from any app's chart
gets picked up automatically — in theory. In practice there's a live gap here: see
[Known Issues → dashboards that render nothing](../05-operations/known-issues.md#grafana-dashboards-silently-dont-render)
for why the bundled dashboards in `system/monitoring/monitoring/grafana-dashboards/`
haven't actually been rendering, and
[Known Issues → the Grafana v2 export format](../05-operations/known-issues.md#grafana-v2-dashboard-export-format)
for a gotcha if you ever export a new one from Grafana 13's UI.

## SNMP / UPS metrics — `system/monitoring/home-metrics`

Plain manifests (no Helm chart) running `snmp-exporter` against the Synology NAS over
SNMPv3, with a large custom module covering interfaces, disk, CPU, temperature, and
UPS-adjacent OIDs. This is explicitly **legacy and scheduled for retirement** — see
[Power — NUT](power-nut.md) for the NUT-based replacement that's superseding it.
