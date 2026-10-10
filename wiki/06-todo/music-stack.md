# TODO — Music stack: rollout and remaining work

[← Back to Home](../Home.md) · How it works: [Music](../03-media-stack/music.md) · Related: [Storage](../01-platform/storage.md) · [Networking](../01-platform/networking.md)

> **Status (2026-10-09):** deployed and verified: Music Assistant and slskd are healthy, Sonos works (group
> Sonos players natively, see the Roam entry in known issues), the legacy library is cleaned and scanned, Radio
> Browser and Spotify are added, Music Assistant has no access to Downloads any more. The intake is deployed
> and verified with two real downloads. Still to do: the review flow with a messy compilation, the ReplayGain pass
> on the legacy library (runs on its own) and the checks below. This page is the checklist for what is left and for
> what is deliberately not built. Delete it (and fold the leftovers into [Music](../03-media-stack/music.md))
> once the rollout checks below are done.

## Rollout — what is verified and what is left

Verified on 2026-10-09: both ArgoCD apps healthy; slskd logged into Soulseek with its folders created; Music
Assistant on its VLAN 5 leg with fixed IP, Published IP set, read-only NFS mounts, Sonos players discovered,
radio playing across all devices, legacy library scanned (116 albums, 31 album artists, no fake artists).

- [ ] Confirm the permanent VLAN 5 route survives a restart (`setup-vlan5-route` init container): after the
      pod restarts, grouping a Sonos with the web player on a radio stream still holds.
- [x] ReplayGain pass finished (2026-10-10): all 4014 files are ID3v2.4 with ReplayGain tags, 31 album artists, a library
      sync was triggered afterwards. Only 3 files still carry an ID3v1 block.
- [x] (old entry) ReplayGain pass, then a manual Music Assistant sync so it picks up the tags.
- [x] Spotify added (2026-10-09, librespot backend). Its first library sync imported the followed artists and
      saved albums (the album-artist list grew by ~60 entries and one album); not a tag problem.
- [ ] Home Assistant integration connects (ClusterIP service on 8095); phone playback works.
- [ ] Turn off Music Assistant's cloud remote access unless wanted.
- [ ] Check the Music Assistant version still matches the research it was built on (the stable line was 2.10.x).
- [ ] Six files still carry an ID3v1 block after the rewrite: find them (`tail -c 128` starts with `TAG`).
- [ ] Optional: Picard on the ~40 single-artist albums for MusicBrainz IDs.

## Intake (music-intake)

- [x] Image pulled, pod Ready, slskd reachable, Music Assistant configured, inbox and library writable (2026-10-09).
- [x] First real run: two slskd downloads were filed automatically with the right tags, the inbox ended empty and
      Music Assistant showed both albums (see the music page).
- [ ] Review flow with a compilation from an uploader with poor tags (edit, approve).
- [ ] Decide whether the Python tagger should be ported to Go later.

## Lidarr (artist albums, torrents; deployed 2026-10-10)

- [x] Pod Ready on PostgreSQL (after the one extra restart), the databases created, the UI reachable (2026-10-10).
- [x] Configured via the API and verified: root folder, quality and metadata profiles, Recycle Bin, Download Station
      client and remote path mapping, Prowlarr link (Knaben, The Pirate Bay synced), a test search returned releases.
- [ ] Look at the two Prowlarr indexers that fail (`BT.etree`, `Torrent Downloads`) and decide whether to fix or drop them.
- [ ] Add one artist you do not have yet and follow it to a filed album: grabbed, imported into `Music/Lidarr`, visible
      in Music Assistant with sensible tags.
- [ ] Decide later whether to connect Soulseek (Soularr) and how to keep it apart from the intake.

## Slskd trial (one week, started 2026-10-09)

Day 1: searches and downloads work with the port closed; one peer banned us and one stalled, both handled by
picking another source (details in [Music](../03-media-stack/music.md#slskd)).

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
