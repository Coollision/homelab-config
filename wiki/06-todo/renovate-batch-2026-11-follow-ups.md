# TODO — Renovate batch 2026-11-17: follow-ups

[← Back to Home](../Home.md) · Related: [Matter / Thread](../02-smarthome/matter-thread.md) · [Known issues](../05-operations/known-issues.md)

> **Status (2026-11-17):** the batch is rolled out and verified: kube-prometheus-stack 92.22.0 → 93.3.1,
> n8n 2.46.4 → 2.49.0, matterjs-server 1.5.5 → 1.6.0, homepage v2.8.0, ha-mcp 9.2.0, cloudnative-pg chart 0.29.2,
> cloudbeaver 26.2.5, cloudflared 2026.11.1 and the Helm CI action 4.3.2. Prometheus had no targets down, both
> Postgres clusters were healthy and n8n ran its migrations. This page holds what is left. Delete it once the boxes
> are ticked.

## Safety copies (clean up after about a week)

- [ ] Drop the Longhorn snapshot `matter-server-pre-1-6-0` (namespace `storage`) once the Thread mesh has run on
      matterjs-server 1.6.0 for a week without trouble. Deleting a snapshot needs an explicit OK.
- [ ] Remove the `pg_dump` of the n8n database taken before the 2.49.0 migrations (kept in a local backup folder
      on the laptop, outside the repo) once n8n has been stable for a week.
- [ ] Rollback note while both exist: revert the tag and, only if that alone does not bring the nodes back,
      restore the snapshot. Whether 1.5.5 can read data written by 1.6.0 was never verified.

## Second batch (2026-11-17): rolled out, including Radarr 7.0.3

Rolled out and verified: Traefik chart 42.4.1 (both instances, Traefik v3.7.19, five GHSA fixes), Mosquitto 2.0.23,
Alloy chart 1.16.0, code-server 4.145.2, Prowlarr 3.0.2 (forward Postgres migration ran on Postgres 18).

- [x] **Radarr 6.4.4 → 7.0.3**: rolled out 2026-11-17 (commit `aaa8cea3`) with the `pg_dump` and Longhorn snapshot taken first; running healthy, 0 restarts.
- [x] Radarr restarted several times in the days before the bump. Cause found 2026-11-20: Radarr exits when PostgreSQL (`service-db-pg-1`) is unreachable and has no startup retry. Restarts on 11-10 (~21:54 UTC, no commit; same minutes as the UniFi gateway update, link unproven) and 11-17 (~19:56 UTC, the CloudNativePG operator bump in the first batch restarted the DB instance). Not memory: peak ~507Mi of 640Mi.
- [ ] Drop the second-batch safety copies after about a week: Longhorn snapshots `mosquitto-pre-batch2` and
      `arr-stack-prowlarr-config-pre-batch2` (namespace `storage`), the Prowlarr `pg_dump`s and the copy of
      `dynamic-security.json` in the local backup folder.
- [ ] Lesson: build a batch from the bump diff only. Copying a file wholesale from an old Renovate branch reverted the
      homepage registry card (fixed in `1494e50f`).

## Verify

- [x] Matter: open a Thread device in Home Assistant and confirm it responds, then compare the unavailable-entity
      count against the 140 seen after the rollout (no pre-upgrade baseline exists; some of those were already
      unavailable, e.g. the vacuum buttons and a camera sensor). Done 2026-11-20: 152 Matter entities across 12 devices in HA, 0 unavailable.
- [x] `aws-tunnels` showed Progressing in ArgoCD after the rollout. It was not part of the batch; find out why.
      Checked 2026-11-20: Healthy and Synced; the pod had 0 restarts. The operator logged an expired AWS SSO session (refresh parked until the next interactive login), which is the likely cause of a brief non-Healthy state. Not a chart problem.

## Open observations

- [ ] ArgoCD took about 13 minutes to start applying the new `monitoring` revision (operation Running with no
      per-resource progress, repo-server healthy). Cause not found. Note it if it repeats, together with the
      repo-server memory limits (see the CMP concurrency entry in known issues).
- [ ] n8n logs a deprecation: `N8N_RUNNERS_MODE=internal` (internal task runner mode) will be removed in a future
      version. Plan a switch to external runners before that release (`n8nio/runners` sidecar, version matching n8n; measure its RAM,
      see [Windmill pilot](windmill-pilot.md#n8n-side-verified-2026-11-20-why-staying-is-the-default)).
      Checked 2026-11-20: still logged at every start (internal mode; also 'Python 3 is missing' for the internal Python runner). Fix = `N8N_RUNNERS_MODE=external` + `N8N_RUNNERS_AUTH_TOKEN` (Vault) + `N8N_RUNNERS_BROKER_LISTEN_ADDRESS` on n8n, and an `n8nio/runners` sidecar whose tag must always equal the n8n tag (Renovate would need to bump both together). No NODE_FUNCTION_ALLOW_* vars are set today. Pod limit is 1Gi, so size the sidecar (and recheck n8n's OOM history) before rolling out.
- [ ] Matter: new pairings are refused when the device's certification is not signed by the CSA (DIY/test devices);
      OTA upload through the server now needs `--enable-test-net-dcl`. Neither applies today.
