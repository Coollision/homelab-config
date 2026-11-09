# TODO — Mosquitto: per-client ACLs, dynamic users, TLS route, real client IPs, 2.1 upgrade

[← Back to Home](../Home.md) · Related: [Zigbee and Mosquitto](../02-smarthome/zigbee.md) · [Networking](../01-platform/networking.md)

> **Status (2026-11-09):** phases 0–4 are **live in production** (ACL enforcement since 2026-11-08, after the 6-day
> usage capture). Still open: the pre-auth alerting item in phase 0, the alert-threshold revisit, and phases 5–7.
> Everything was rehearsed first on the `tests` branch (reference app, kept disabled at
> `tests/mosquitto-test/disabled-mosquitto-test`). Phases 5–6 wait for Mosquitto 2.1.3 (see [below](#waiting-for-213)).
> Delete this page once the migration is done.

## Why not a clustered broker

One Mosquitto pod uses ~6 Mi RAM and ~1 m CPU for a handful of clients. A clustered broker (EMQX, VerneMQ, HiveMQ) costs
100× the memory, boots slowly, and does not remove the real single point of failure (Zigbee2MQTT and its USB dongle sit
on one node). Mosquitto is also what Zigbee2MQTT, Home Assistant and ESPHome docs assume. The wins are elsewhere:
authentication/ACLs, TLS, and visibility of who connects.

## Target design

- **Auth and ACLs: dynamic-security plugin.** Default-deny, one role per client (`r-<user>`), topic prefixes in git. An
  init container (`reconcile.sh`, data in `clients.conf`) reapplies roles and the managed users (Vault `<name>-password` keys, passwords rotate with
  Vault) on every start. Users/roles created at runtime with `mosquitto_ctrl` survive restarts, including their role
  bindings (roles are never deleted).
- **TLS: terminated by Traefik, no certificate in Mosquitto.** An `IngressRouteTCP` matches `HostSNI(mqtt.<internal-domain>)`
  on `websecure` (443) and uses the default wildcard store. No new certificate is issued, so nothing new appears in
  public CT logs (hard requirement). The internal wildcard DNS record already resolves any new name to Traefik.
- **Plaintext stays on 1883 / LB 1880.** Shelly Gen1, EMS-ESP and the Moonraker printer cannot (all) do TLS; in-cluster
  Home Assistant and Zigbee2MQTT use the service directly. Scope it with the firewall at the end (phase 7).
- **`externalTrafficPolicy: Local`** on the Mosquitto Service so the broker sees real client IPs instead of the node's
  flannel address (traefik and the Cloudflare tunnel service already use `Local`).
- **After 2.1.3:** a second listener (`1884`, `enable_proxy_protocol 2`) reachable only through a ClusterIP Service and a
  NetworkPolicy for the Traefik pods, with `proxyProtocol: {version: 2}` on the route, so TLS clients also show their real IP.

## Rehearsal results (tests branch, fake credentials)

Passed, on both 2.0.22 and 2.1.2-alpine unless noted:

- Reconcile on a fresh volume, after a plain pod kill and after a config roll: 0 errors, idempotent. Learning on → off
  removes the full-access rule. Password rotation works (old rejected, new accepted).
- ACL matrix of 41 cases (6 roles, no-role user, bad password, `$CONTROL`) in learning and strict mode.
- Real client IP visible with `Local` (exact match from a LAN client on another VLAN). Same-node pod restart: ~17 s
  client-visible gap; forced cross-node move: ~40 s. That is the pod's own start/reattach time, no extra ARP cost.
- TLS route: Let's Encrypt wildcard verifies (TLSv1.3), `mosquitto_ctrl`/pub/sub work over 443 with ACLs, a silent client
  survived 434 s idle through Traefik.
- 2.1.2 in place on the same volume: init and broker fine, no warnings, last-will works on allowed topics and is dropped
  on denied ones, largest retained message (459 KB) is under the new 2 MB packet default.
- PROXY v2 listener: the TLS client's exact IP and port reach the broker log; the listener is unreachable from other pods
  (NetworkPolicy enforcement verified with a control, blocked → open without policy → blocked again).

Found and fixed during the rehearsal (the fixes are in the reference app):

- dynsec ACL types are `publishClientSend` / `publishClientReceive` (not `...ToBroker`). With a quiet error helper the wrong
  names would have logged "done" while every publish rule was missing: an outage for Home Assistant and Zigbee2MQTT.
- A client holding `#` could read admin responses on `$CONTROL/#`. Every role now carries an explicit high-priority deny.
- `addClientRole` on an existing binding answers "Internal error" on 2.0.22; check `getClient` first.
- `mosquitto_ctrl` prints a plaintext warning on stderr, which must not be counted as an error.
- Kustomize only rewrites the hashed ConfigMap name in the pod volume when the generator has an explicit `namespace`.
  Hashing is wanted: with `disableNameSuffixHash` a config change never rolls the pod.
- **Rollback hazard:** after 2.1.x has written `dynamic-security.json`, 2.0.22 rejects every password, admin included.
  `reconcile.sh` now moves such a file aside (`.bak-<timestamp>`) and rebuilds the managed users/roles; runtime-created
  users must then be recreated from the backup. Always snapshot the Longhorn volume before the upgrade.
- A 5-minute read-only capture of production found `printers/<printer>/power`, published (retained) by a Home Assistant
  automation, which none of the guessed roles covered. Guessed ACLs miss things, hence the full-access-first phase.

## Plan

One variable at a time, one commit per phase, observe at least 24 h before the next.

### Phase 0: prerequisites

- [x] Vault: create `kv/workload/smarthome/mosquitto` key `admin-credential` (`user:password`). Without it the init fails. **Done 2026-11-03** (`kv patch`; a second key `admin-source-range` holds the TLS-route allow-list).
- [x] Node-RED credential is no longer referenced; the unused Vault key `credential-3` is left in place (any `kv delete` needs explicit approval).
- [x] Confirm the Moonraker printer's MQTT base topic and who consumes `printers/<printer>/power`. Confirmed 2026-11-08 by the 6-day capture: the Moonraker login reads it (read-only), the K1C also reads its own Zigbee2MQTT plug, and a second printer publishes under its own base topic. Both are in the `moonraker` role.
- [x] Confirm the Longhorn volume `mosquitto` is covered by snapshot/backup jobs; take a manual snapshot before phases 2 and 5. Hourly snapshots and a nightly backup exist; a snapshot from 90 minutes before phase 2 was `readyToUse`.
- [x] Broker-up probe. Covered (checked 2026-11-09) by `MosquittoMetricsMissing` plus the client-count, no-publish, drop and churn alerts in `prometheusrules.yaml` (`mosquitto.alerts`). The pod-level kube-prometheus defaults (`KubePodCrashLooping`, `KubePodNotReady`, `KubeStatefulSetReplicasMismatch`) are enabled and cover a crashed or unready Zigbee2MQTT or Home Assistant pod.
- [x] Application-level availability alert (Zigbee2MQTT bridge offline while the pod is Ready): not built, moved to [Future ideas](future-ideas.md). Checked 2026-11-09: no Home Assistant automation uses the bridge connection entity, and ~174 entities were `unavailable`, so an unavailable-count alert needs a clean baseline first.

### Phase 1: `externalTrafficPolicy: Local` (own commit, quiet hour)

- [x] Add `/spec/externalTrafficPolicy: Local` to the Service patch in the Mosquitto `kustomization.yaml`.
- Expected: the L2 announcer moves to the pod's node and every LAN connection (Shellies, EMS-ESP, printer) resets once;
  they reconnect within seconds to about a minute (keepalive 60), worst case minutes for a device that ignores gratuitous ARP.
- [x] Verify real source IPs in the broker log; Home Assistant Shelly/EMS entities recover. Verified 2026-11-03: the announcer moved to the pod's node, 9 clients were back within seconds, Shellies and EMS-ESP now show their IoT-VLAN addresses.
- [x] Collect source IP/VLAN per user: input for phase 7. Done 2026-11-08 (6 days of connect lines): Shelly, EMS-ESP and Moonraker connect from the IoT VLAN, the debug client from the admin VLAN, Home Assistant, Zigbee2MQTT and the admin logins from inside the cluster. The plaintext port only needs the IoT VLAN plus the admin VLAN.
- Rollback: revert the commit.

### Phase 2: dynamic security, everybody has full access

- [x] Port the reference app to `workload/smarthome/mosquitto` (the draft files exist uncommitted in a working tree, but
      the reference app on the `tests` branch is the canonical version). Keep `acl-learning=true`: every role gets `#`
      read/write, authentication stays on. The `credential*` list must match Vault.
- [x] Verify the init log ends with `reconcile: done`, every user authenticates, retained messages are intact, clients healthy. Verified 2026-11-03: `reconcile: done (6 roles, learning=true)`, 0 auth failures, admin login works, 9/9 clients back, retained state intact.
- Rollback: revert the commit (the old init rebuilds the password file; the dynsec file on the volume is inert).

### Phase 2b: capture real usage, then trim

- [x] Raise broker `log_type` (debug/subscribe) for 24–72 h covering a full day, automations, a Zigbee2MQTT restart and a On since 2026-11-03 (the pod rolled in 29 s). Report: `scripts/mosquitto-acl-usage.sh --since 24h` (reads Loki, flags topics the roles do not cover).
      Home Assistant restart; ship to Loki; aggregate per username: published topics, subscribed filters, will and retained topics.
- Early finding (15 min of data): Home Assistant's MQTT integration subscribes to the discovery topics of many native integrations (e.g. `tasmota/discovery/#`, `esphome/discover/#`, `dsmr/#`). The final `homeAssistant` role needs read-only access to the ones the full report shows, otherwise HA logs denied subscriptions after phase 4.
- Capture result 2026-11-08 (6 days, 1.6M log lines): the zigbee2mqtt, emsChip, shelly and debug roles match observed use. `moonraker` was missing its second base topic and the plug read (fixed). `homeAssistant` still needs read-only access to the discovery filters it subscribes to on every connect: `drop_connect/discovery/#`, `dsmr/#`, `esphome/discover/#`, `fully/deviceInfo/+`, `inels/status/#`, `pglab/discovery/#`, `cloudapp/QBUSMQTTGW/#`, `tasmota/discovery/#`, `/greencell/broadcast`, `prism/hello`.
- [x] Replace the guessed roles with the observed table. Done 2026-11-08, now data in `clients.conf` (see [Zigbee/MQTT](../02-smarthome/zigbee.md)); migration rehearsed in the production image: grants revoked by diff, runtime users untouched, special-character passwords log in.
- [x] Extra logging: `log_type all` replaced by default levels + `debug` + `subscribe` (needed for denial alerts, see above). Revisit if Loki volume becomes a problem.

### Phase 3: TLS route

- [x] Add the `IngressRouteTCP` (copy from the reference app). Verify with `openssl s_client -servername` and `mosquitto_ctrl` Verified 2026-11-03: wildcard cert, TLSv1.3, admin login over 443; no `Certificate` object was created.
      over 443; confirm no new `Certificate` object appeared.
- [x] Add a `MiddlewareTCP` `ipAllowList` so the admin login is not reachable from every VLAN. The range comes from Vault `admin-source-range` (currently only the admin subnet); a pod outside it is dropped by Traefik ("matched none of the trusted IPs").

### Phase 4: enforce the ACLs

- [x] Follow-up tooling: Grafana dashboard `Mosquitto` (`system/monitoring/monitoring/grafana-dashboards/mosquitto.json`: rejected logins, connects per user and subnet, disconnect reasons, security log lines; the topic-capture row empties once `log_type all` is removed) and the alert `MQTT unauthorised connection` (`grafana-alerts/log-alerts.yaml`, fires on any `not authorised`).
- [x] Set `acl-learning=false` (the pod rolls). Enforced 2026-11-08 after a final usage check against `clients.conf` (found and fixed one real gap: HA subscribes to `/greencell/broadcast/device`). Denial visibility: a denied publish is logged only at `log_type debug` (kept on) and alerts via `MQTT publish denied`; denied subscribes are never logged by Mosquitto at any level, so a read-side gap shows up as a silent entity. Break-glass: set `acl-learning=true` in `kustomization.yaml`. Watch for denied publishes (silent on MQTT 3.1.1) and entity availability for 24–48 h.
- Rollback: set it back to `true`.

### Phase 5: Mosquitto 2.1.x (needs 2.1.3, see below)

- [ ] Image tag in **both** `values.yaml` and the init container in `kustomization.yaml` (Renovate tracks neither for this
      app; 2.1 images exist only as `-alpine`, there is no `-openssl` variant).
- [ ] Longhorn snapshot first; rollback = revert the tag (the init rebuilds the state file) or restore the snapshot.

### Phase 6: PROXY v2 listener

- [ ] Listener `1884` with `enable_proxy_protocol 2`, ClusterIP-only Service (never on the LoadBalancer), NetworkPolicy that
      allows `1883` from anywhere and `1884` only from the Traefik pods in `kube-system`; `proxyProtocol.version: 2` on the route.
- [ ] Verify a TLS client's real IP in the log and that `1884` is unreachable from another pod.

### Phase 7: retire and scope the old flows

- [ ] Scope access to the LoadBalancer's plaintext port in UniFi to the VLANs/devices seen in phase 1.
- [ ] Optional: per-device Shelly users (`mosquitto_ctrl createClient` + `addClientRole`), EMS-ESP to TLS on 443.
- [ ] Credentials: move from the ConfigMap into a Secret mounted only in the init container.
- [ ] Vault cleanup (needs an explicit OK, never delete unasked): the old `credential-1..8` keys in `workload/smarthome/mosquitto` are unused since the `clients.conf` switch (values were copied to `<name>-password`); keep them until the new setup has run for a while as the rollback path.
- Plaintext cannot be retired completely (Shelly Gen1, Moonraker); the end state is firewall-scoped plaintext plus TLS for the rest.

## Metrics exporter (optional follow-up) {#metrics-exporter}

The dashboard and the `MQTT unauthorised connection` alert are log-based. Broker health (clients connected, message
rate, dropped messages, uptime) has no metric source: Mosquitto has no Prometheus endpoint, only the `$SYS/#` topics.

Scope:

- [x] Exporter: `sapcc/mosquitto-exporter` 0.8.0 (last release 2021, repo still maintained; Apache-2.0, 148 stars). Chosen because the alternatives are smaller and no more active; Renovate tracks the tag.
- [x] `$SYS`-only `exporter` role + user, Vault keys written 2026-11-08 (password never in git).
- [x] Small app + `ServiceMonitor` scaffolded (credentials via Secret).
- [x] Verified live 2026-11-08 (`broker_clients_connected` ~10, ~100 publishes/min, 0 dropped). Alerts in `prometheusrules.yaml` group `mosquitto.alerts` (metrics missing, clients dropped, no publishes, dropped messages, connection churn); thresholds come from the first readings, so revisit after a week of data. The image is amd64-only (pinned off the Pi).
- [x] Panels added to the `Mosquitto` dashboard.
- [ ] Revisit the alert thresholds after a week of data (they come from the first readings).
- Not covered by metrics, stays on logs: per-user activity, rejected logins, ACL denials (`$SYS` has none of them).

## Waiting for 2.1.3 {#waiting-for-213}

Why wait: 2.1.2 (2026-02-09) is the latest release, and the unreleased `2.1.3` section of the upstream ChangeLog fixes
incorrect max-packet-size checks for incoming and outgoing packets (#3503, #3515), a will/ACL ordering bug on disconnect (#3487),
and wrongly delayed will messages (#3505). The will test passed on 2.1.2 and the largest retained message is far below the
limit, so 2.1.2 is usable, just not ideal.

**There is no release date to rely on.** Facts as of 2026-11-03:

| Signal | Value |
|---|---|
| Upstream ChangeLog heading | `2.1.3 - 2026-02-xx` (a placeholder since February, 13 fixes queued) |
| Git tag / milestone for 2.1.3 | none |
| Last 2.1.x releases | 2.1.0 on 2026-01-29, 2.1.1 on 2026-02-04, 2.1.2 on 2026-02-09 |
| Upstream activity | about one commit in the last 30 days, mostly fuzzing and refactoring |
| Past point-release gaps | irregular: days, 4–6 months, once over a year |

A guess, not a prediction: no sooner than the first quarter of 2027, possibly much later. So **do not block on it**:

- Do phases 0–4 and 7 on 2.0.22 now; they do not need 2.1.
- Re-check on **2027-01-15**. If 2.1.3 is out, do phases 5 and 6 with it. If not, either stay on 2.0.22 and skip phase 6
  (only admin connections over TLS lose their real IP), or accept 2.1.2 after re-running the reference app.
- Where to look: the upstream ChangeLog (`2.1.3` heading gets a date), the GitHub tags, and Docker Hub tags for
  `eclipse-mosquitto` (`2.1.3-alpine`).

## Warnings

- While `acl-learning=true`, every role has full access (authentication is on). Do not forget phase 4 after the capture.
- The plaintext listener still reaches the dynamic-security admin login until it is firewall-scoped (phase 7); use the TLS
  route or `kubectl exec` for admin work.
- The reference app uses fake credentials and its own namespace/volume. To rerun it, rename the directory back (drop
  `disabled-`) on the `tests` branch; delete it again afterwards.
