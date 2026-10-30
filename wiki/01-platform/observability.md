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
`query.max-samples`/`query.max-concurrency`, and storage is a 20Gi Longhorn PVC. Retention is configured as 90d, but
`retentionSize: 18GiB` is the cap that actually bites, so effective retention is shorter
(roughly three weeks at the time of writing).

Prometheus is kept off the Raspberry Pi worker by a required `nodeAffinity` (`type NotIn rpi`):
long-range queries plus scrape load are too much for it, and a drain of another node must not
reschedule it there.

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

Loki is single-binary with filesystem storage on a 10Gi Longhorn PVC and
`replication_factor: 1` — homelab-scale by design, not accidental. `retention_period` is
90d, but the PVC size is the practical limit. **For anything Matter/Thread-related, Loki is the first place to look**, not
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

## Synology NAS metrics — `system/monitoring/home-metrics`

Plain manifests (no Helm chart): one `snmp-exporter` Deployment scraping **every NAS** over
SNMPv3 with a large custom `synology` module (interfaces, disks, RAID/volumes, temperatures,
fans, CPU/memory, model/version). Each NAS is one `endpoints[]` entry in the `ServiceMonitor`
(`deploy.yaml`): the target IP comes from Vault (`<secret:kv/data/shared/network~<nas>-ip>`) and a
`relabelings` rule renames `instance` to the NAS name. **To add a NAS:** put its IP in Vault,
copy an endpoint block, change the Vault key and the `replacement:` name — dashboards and alert
rules pick the new `instance` up automatically (they never hard-code NAS names).

- The NAS needs SNMPv3 enabled with the same user as the others (DSM → Control Panel → Terminal
  & SNMP), and must be reachable from the cluster's Servers VLAN. For a NAS at another site that
  means the site-to-site link + the UniFi zone policy must allow Servers → that site (SNMP is UDP/161).
- The module sets `max_repetitions: 10`. With the default (25) the remote NAS answers the
  hrStorage walk with UDP replies larger than the link MTU; the fragments are dropped and the
  scrape just times out. See
  [Known Issues → SNMP scrape of a remote NAS times out](../05-operations/known-issues.md#snmp-remote-nas-timeout).
- NAS syslog goes to Loki (see Logs above); the syslog `hostname` equals the SNMP `instance` name,
  which is what lets one dashboard variable filter both metrics and logs.

The UPS side of this used to live here; it has been replaced by NUT — see [Power — NUT](power-nut.md).

### Dashboard: Synology SNMP Dashboard

`grafana-dashboards/synology-snmp.json` (uid `qfGRDGZgz`) is the **only** Synology dashboard. The
`NAS` variable lists every `instance` of `job="snmp-exporter"` (multi-select, default *All*) and the
dashboard repeats every panel horizontally per selected NAS (two per row, so NASes sit side by side for comparison): status, disks, RAID, volume usage, temperatures,
network traffic (the `Interface` variable hides loopback/docker/veth/tap/ovs noise), load, and
failed sign-ins from Loki. Edit the JSON in git; UI edits are overwritten by the sidecar.

## Alerting

There are two rule engines but **one Slack channel and one severity convention**:

| What | Engine | Where it lives | Routing |
|---|---|---|---|
| Everything metric-based (kube defaults, Longhorn, UPS, NAS, certificates) | Prometheus rules | `values.yaml` (defaults) + `templates/prometheusrules.yaml` | Alertmanager (`promstack.alertmanager.config`) |
| Log-based (NAS failed logins, error surge, log-volume surge) | Grafana-managed rules on Loki | `grafana-alerts/log-alerts.yaml` | Grafana policy in `grafana-alerts/notifications.yaml` |

Metric alerts are PrometheusRules rather than Grafana rules on purpose: they are reviewable in git,
unit-testable with `promtool`, and Alertmanager already provides grouping and inhibition. Grafana is
only used where it has to be — evaluating LogQL. (Forwarding Grafana alerts into Alertmanager would
need a non-GitOps admin-API toggle, so the log rules notify Slack directly with the same routing.)

**Severity convention** (every rule must set `labels.severity`):

| severity | Slack behaviour |
|---|---|
| `critical` | posted within ~30 s, re-posted every 4 h while firing, a "resolved" message follows |
| `warning` | grouped digest: first post after 2 min, new members batched every 30 min, re-posted every 24 h, **no** "resolved" message |
| `info` | dropped — dashboards, not notifications |

`Watchdog` and `InfoInhibitor` go to a `null` receiver. Inhibition: critical mutes the same alert's
warning/info in a namespace, and **`UpsOnBattery` mutes all warnings** (during a power cut everything
else is a consequence). The Slack webhook is in Vault at `kv/system/monitoring/monitoring~slack-webhook-url`
and is used by both Alertmanager and Grafana provisioning.

**Rules added/owned here** (thresholds are in the files):

- *NAS* (`synology.alerts`, per `instance`): unreachable 10 min, system/power failure, fan failure,
  RAID degraded/crashed, disk failure, disk temperature (>50 °C 30 min warn / >60 °C critical),
  system temperature >70 °C, volume <10 % / <5 % free. The long `for:` windows replaced hand-made
  Grafana temperature rules that flapped ~90 times a week.
- *Certificates*: cert-manager not ready 30 min, expiry <14 d (renewal normally happens at 30 d, so
  this means it is stuck), <3 d critical.
- *Longhorn*, *UPS*: unchanged (`longhorn.alerts`, `nut.alerts`).
- *Logs*: failed NAS sign-ins (>4 in 10 min), error surge (>400 error lines/15 min **and** ≥5× the same
  window yesterday), volume surge (>40 k lines/15 min **and** ≥4× yesterday). Relative thresholds
  because several healthy workloads log ~100 errors per 15 min permanently.
- `CPUThrottlingHigh` is disabled (`defaultRules.disabled`): info-level and constantly firing for
  scale-to-zero apps; it never required action.

**Restarts must not page you.** Three layers, all deliberate:

- *Grafana log rules* use `noDataState: OK` and `execErrState: KeepLast`, so a Loki/Grafana restart or a
  quiet log stream never produces "DatasourceNoData"/"DatasourceError" notifications. Keep that on any
  new Grafana rule. (The old hand-made rules on Prometheus used `NoData`/`Error`, which is exactly what
  fired on every Prometheus restart — they were replaced by PrometheusRules, which have no "no data" state.)
- *Prometheus restart* takes ~3.5 min and stops sending alerts. `prometheusSpec.rules.alert.resendDelay: 5m`
  makes Alertmanager keep each alert alive for ~20 min without a refresh, so firing alerts are not
  resolved-and-refired (which would repost every critical). `forOutageTolerance: 1h` restores `for:`
  timers, so a restart does not delay or reset pending alerts either.
- *Alertmanager restart*: 2 replicas with a persistent notification log, so it does not re-notify what it
  already sent. Warnings additionally never post "resolved", so a flap inside their 24 h repeat window is
  silent.
- Rules that must tolerate missing data (`absent()`, `up == 0`) always carry a `for:` ≥ 10 min.

**Not covered yet** (no metrics are scraped today): Vault sealed, ArgoCD app health, CloudNativePG
replication/backup. Enable their ServiceMonitors first, then add rules next to the existing groups.
There is also no dead-man's-switch: Watchdog is dropped, so if Alertmanager itself dies nothing
tells you — wiring Watchdog to an external heartbeat service (healthchecks.io style) is the fix.

**Adding a rule:** metric → add a group/rule in `templates/prometheusrules.yaml` (wrap `{{ }}` in
`{{` + backtick escapes like the existing ones), set `severity` + `category`, check with
`promtool check rules`. Log → add to `grafana-alerts/log-alerts.yaml` (plain Grafana provisioning
YAML — **not** wrapped in a ConfigMap; the chart template wraps it, and a double wrap is silently
ignored, which is how the old Longhorn Grafana rules never loaded). Test a log rule against live
Loki with `POST /api/v1/eval` on Grafana before committing.

**Silencing / debugging:** silences and the live alert list are in the Alertmanager UI; Grafana's
log rules are under *Alerting → Alert rules → folder Homelab*.
