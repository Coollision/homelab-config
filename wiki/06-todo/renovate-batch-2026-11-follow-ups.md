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

## Second batch (2026-11-17): rolled out, Radarr held back

Rolled out and verified: Traefik chart 42.4.1 (both instances, Traefik v3.7.19, five GHSA fixes), Mosquitto 2.0.23,
Alloy chart 1.16.0, code-server 4.145.2, Prowlarr 3.0.2 (forward Postgres migration ran on Postgres 18).

- [ ] **Radarr 6.4.4 → 7.0.3** (open Renovate branch, deliberately not merged). 7.0.1 runs a one-way media-info
      migration over every movie file. Before merging: `pg_dump` of `radarr_main` and `radarr_log` (database `service-db-pg`
      in `arr-stack`), Longhorn snapshot of the `arr-stack-radarr-config` volume, and raise the memory limit (640Mi) and
      the probe delay for the first start. Merge it a day or more after Prowlarr 3, and check the Prowlarr indexer sync,
      the download client and the Homepage widget afterwards. Rollback means restoring the dump.
- [ ] Radarr restarted several times in the days before the bump; look at why before adding a migration on top.
- [ ] Drop the second-batch safety copies after about a week: Longhorn snapshots `mosquitto-pre-batch2` and
      `arr-stack-prowlarr-config-pre-batch2` (namespace `storage`), the Prowlarr `pg_dump`s and the copy of
      `dynamic-security.json` in the local backup folder.
- [ ] Lesson: build a batch from the bump diff only. Copying a file wholesale from an old Renovate branch reverted the
      homepage registry card (fixed in `1494e50f`).

## Verify

- [ ] Matter: open a Thread device in Home Assistant and confirm it responds, then compare the unavailable-entity
      count against the 140 seen after the rollout (no pre-upgrade baseline exists; some of those were already
      unavailable, e.g. the vacuum buttons and a camera sensor).
- [ ] Cloudbeaver: no pod was running after the sync (scale to zero), so the 26.2.5 image was never seen live.
      Wake it once and check the tag.
- [ ] `aws-tunnels` showed Progressing in ArgoCD after the rollout. It was not part of the batch; find out why.

## Open observations

- [ ] ArgoCD took about 13 minutes to start applying the new `monitoring` revision (operation Running with no
      per-resource progress, repo-server healthy). Cause not found. Note it if it repeats, together with the
      repo-server memory limits (see the CMP concurrency entry in known issues).
- [ ] n8n logs a deprecation: `N8N_RUNNERS_MODE=internal` (internal task runner mode) will be removed in a future
      version. Plan a switch to external runners before that release.
- [ ] matterjs-server 1.6.0 no longer logs Thread/Wi-Fi credentials, passcodes or keys. The debug logs written by
      earlier versions may still contain them.
- [ ] Matter: new pairings are refused when the device's certification is not signed by the CSA (DIY/test devices);
      OTA upload through the server now needs `--enable-test-net-dcl`. Neither applies today.
