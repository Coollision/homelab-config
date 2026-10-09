# TODO — Music stack: rollout and remaining work

[← Back to Home](../Home.md) · How it works: [Music](../03-media-stack/music.md) · Related: [Storage](../01-platform/storage.md) · [Networking](../01-platform/networking.md)

> **Status (2026-10-09):** deployed and verified: both apps are healthy, Sonos and the web player work
> (including grouped radio), the legacy library is cleaned and scanned, Radio Browser is added. Still to do:
> the ReplayGain pass, Spotify, and the checks below. This page is the checklist for what is left and for
> what is deliberately not built. Delete it (and fold the leftovers into [Music](../03-media-stack/music.md))
> once the rollout checks below are done.

## Rollout — what is verified and what is left

Verified on 2026-10-09: both ArgoCD apps healthy; slskd logged into Soulseek with its folders created; Music
Assistant on its VLAN 5 leg with fixed IP, Published IP set, read-only NFS mounts, Sonos players discovered,
radio playing across all devices, legacy library scanned (116 albums, 31 album artists, no fake artists).

- [ ] Confirm the permanent VLAN 5 route survives a restart (`setup-vlan5-route` init container): after the
      pod restarts, grouping a Sonos with the web player on a radio stream still holds.
- [ ] ReplayGain pass (`cleanup.py --replaygain --resume`), then a manual Music Assistant sync so it picks up the tags.
- [ ] Spotify (browser login, paste back the dead redirect URL). Add it **after** the local providers have scanned.
- [ ] Home Assistant integration connects (ClusterIP service on 8095); phone playback works.
- [ ] Turn off Music Assistant's cloud remote access unless wanted.
- [ ] Check the Music Assistant version still matches the research it was built on (the stable line was 2.10.x).
- [ ] Six files still carry an ID3v1 block after the rewrite: find them (`tail -c 128` starts with `TAG`).
- [ ] Optional: Picard on the ~40 single-artist albums for MusicBrainz IDs.

## Slskd trial (one week)

- [ ] Judge whether searches for the kind of music in this library (Dutch/Belgian dance compilations)
      actually complete with no inbound port. If not: a forwarded port or a VPN with port forwarding
      (neither built, both bigger decisions).
- [ ] Decide whether to put a few files in the (currently empty) share.

## Deferred on purpose

- **Intake tagging (beets or Lidarr)**: decide once slskd has proven itself. beets could reuse the cleanup
  rules on the downloads folder; Lidarr is weak for compilations and its dependencies (Prowlarr, the
  download client) live on the NAS, so it first needs a decision on where it runs and a remote path mapping.
- **Daily backup question** is closed: the Longhorn storage lib already puts protected volumes in the
  2-hourly snapshot and daily backup jobs.
