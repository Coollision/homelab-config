# TODO — Music stack: rollout and remaining work

[← Back to Home](../Home.md) · How it works: [Music](../03-media-stack/music.md) · Related: [Storage](../01-platform/storage.md) · [Networking](../01-platform/networking.md)

> **Status:** Music Assistant, slskd, the NFS storage and the tag-cleanup scripts are in the repo.
> Nothing has been verified live yet. This page is the checklist for the first rollout and for what is
> deliberately not built. Delete it (and fold the leftovers into [Music](../03-media-stack/music.md))
> once the rollout checks below are done.

## First rollout — confirm it starts cleanly

Order matters: slskd first, because it creates the `slskd` folder that Music Assistant mounts read-only.

- [ ] Both ArgoCD apps (`slskd`, `music-assistant`) are Synced and Healthy.
- [ ] **slskd:** pod Ready, `/health` answers, web UI login works with the Vault credentials, the Soulseek
      login succeeded (connected, not "logged out") in the UI, and `slskd` and `slskd-incomplete` exist on the
      Downloads share.
- [ ] **Music Assistant pod:** Ready, no `CreateContainerConfigError` (that means the `slskd` folder is missing),
      memory well under the 2Gi limit during the first scan.
- [ ] **VLAN 5 leg:** `net1` has an address and **no default route** (the default stays on `eth0`),
      the NFS mounts are read-only and readable.
- [ ] Give the `net1` stub a UniFi alias and a fixed IP.
- [ ] Create the first admin at `/setup`; set **Published IP address** (Streams, advanced) to the `net1` address.
- [ ] Add the two Filesystem providers, let each scan finish. Check artists and compilations against the
      [tag rules](../03-media-stack/music.md#the-legacy-library-and-its-tag-cleanup) (with untouched tags
      expect the fake album-artist entries, which is why the cleanup comes first).
- [ ] Sonos: all three players discovered (including the Roam after leaving and rejoining the network) and a
      test stream plays. If not: check mDNS reaches the pod on `net1`, then fall back to manual player IPs.
- [ ] Home Assistant integration connects (the ClusterIP service on 8095 is enough); phone playback works.
- [ ] Spotify last (browser login, paste back the dead redirect URL).
- [ ] Turn off Music Assistant's cloud remote access unless wanted.
- [ ] Check the Music Assistant version still matches the research it was built on (the stable line was 2.10.x).

## Library cleanup (not run yet)

- [ ] Dry-run `scripts/music/cleanup.py` over the whole library, on the NAS, and review the review list.
- [ ] Trial on a copy of three folders (artist album, compilation, multi-disc) in a throwaway Music Assistant.
- [ ] Real run with `--apply --replaygain` **before** the real provider is added.
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
