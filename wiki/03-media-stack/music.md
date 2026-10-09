# Music — Music Assistant, slskd and the legacy library

[← Back to Home](../Home.md) · Related: [Media stack overview](overview.md) · [Storage](../01-platform/storage.md) · [Networking](../01-platform/networking.md) · [Remaining work](../06-todo/music-stack.md)

A music library that plays on Sonos, Home Assistant and phones through **Music Assistant**, with new
music fetched by **slskd** (Soulseek). The pieces:

```
                       ┌───────────── NAS (NFS, read-only mounts) ─────────────┐
                       │  Music share (legacy library, flat, ~100 albums)      │
                       │  Downloads share ── slskd/  (complete downloads)      │
                       │                  └─ slskd-incomplete/ (never scanned) │
                       └───────┬───────────────────────────────▲───────────────┘
                               │ ro                            │ rw (subPath)
   Sonos (VLAN 5) ◀──HTTP 8097─┤                          ┌────┴────┐
        ▲  control (websocket)  │  Music Assistant         │  slskd  │  Soulseek network
        └───────────────────────┤  smarthome ns,           │ arr-    │  (outbound only,
   phone / HA ── 8095 ingress ──┘  macvlan leg on VLAN 5   │ stack   │   no inbound port)
                                                           └─────────┘
```

| Piece | Where | Notes |
|---|---|---|
| Music Assistant | `workload/smarthome/music-assistant/` | StatefulSet, Longhorn `/data`, Multus leg on VLAN 5, internal ingress only |
| slskd | `workload/arr-stack/slskd/` | StatefulSet, Longhorn `/app`, internal ingress only, no inbound port |
| Tag cleanup scripts | `scripts/music/` | One-off transform for the legacy library, see [below](#the-legacy-library-and-its-tag-cleanup) |
| NFS paths | Vault `kv/storage/nfs` (`music-path`, `downloads-path`) | Same secret as the other NFS paths |

Nothing here has external ingress. Music Assistant and slskd are only reachable on the internal secure
ingress, and Music Assistant is **not** scale-to-zero (Sonos and Home Assistant hold long-lived
connections that a sleeping pod cannot answer).

## Storage

- **Legacy library:** the Music share, mounted read-only into Music Assistant at `/media/music`.
  Music Assistant only ever writes `.m3u` playlists into a library, so a read-only mount only costs it
  local-playlist editing. Covers, metadata and its database live in its own `/data`.
- **New downloads:** slskd writes to a `slskd` subfolder of the existing Downloads share (the same
  `arr-stack-downloads` claim radarr and sonarr use, selected with a `subPath`). Music Assistant mounts
  the same share read-only, again with `subPath: slskd`, so it never sees the video downloads.
  slskd's incomplete folder is a *sibling* (`slskd-incomplete`), outside what Music Assistant scans.
- **Why Music Assistant has its own PV/PVC pairs:** a PV binds to exactly one PVC and PVCs are namespaced,
  so the ones in `workload/arr-stack/shared/_storage.yaml` cannot be reused from `smarthome`. They are
  in the chart's `templates/storage.yaml` and point at the same two NFS paths.
- The two shares are on different NAS volumes, so there are no hardlinks between them. Nothing here
  needs them.
- `shared-lib` mounts gained two optional fields for this, `subPath` and `readOnly`, on any `storage`
  entry. Charts only pick the change up after `helm dependency update`.

## Music Assistant

Image `ghcr.io/music-assistant/server`, pinned to a stable 2.10.x tag (beta 2.11.0b2 had a Sonos
regression). Ports: **8095** (UI, API, websocket) and **8097** (stream server).

### The network leg, and why it is mandatory

Music Assistant officially requires host networking and lists Kubernetes as unsupported: it relies on
mDNS and direct reachability to find and drive players. The three Sonos players are on the Intern VLAN
(5), so the pod gets a Multus macvlan leg there (the same pattern as matter-server and ESPHome,
including the `sbr` chained plugin so the leg never takes the default route).

How a stream reaches a speaker:

1. Music Assistant controls the player with a websocket to it (native S2 provider; all players here are S2).
2. The player then **pulls the audio over HTTP from Music Assistant's stream server on port 8097**. That
   server has no auth and no TLS by design, so it is never routed through an ingress.

Music Assistant advertises its own address in the stream URL, and its "auto" detection picks the source
address of the *default route*, which in this pod is `eth0`, the cluster IP. Sonos cannot reach that.
Hence the one manual setting after the first start:

> **Settings → Core → Streams (advanced) → Published IP address** = the address of the `net1` leg.

Give that stub a UniFi alias and a fixed IP first, otherwise the published address drifts with the DHCP
lease. The stub's MAC follows the scheme in `lib/shared-lib/templates/_multus.yaml`
(`02:05:67:9d:64:67`, workload id 67). The Sonos Roam is portable: it withdraws its mDNS announcement
when it sleeps and Music Assistant reconnects when it announces again, so it depends on mDNS working
on that VLAN (mDNS is on for the VLAN; IGMP snooping is deliberately off, see the TV note in
[Networking](../01-platform/networking.md)).

### Providers

- **Filesystem (local), twice:** `/media/music` (legacy library) and `/media/downloads` (new downloads).
  There is no folder-layout setting: **tags are always primary**, folders are only a secondary signal.
  Add these **before** Spotify and let each scan finish, because Spotify artwork can otherwise win.
- **Sonos:** discovered over mDNS (`_sonos._tcp`). Manual IPs exist as a fallback only.
- **Home Assistant:** the HA integration connects to Music Assistant on 8095; a ClusterIP service is
  enough, and Music Assistant controls Sonos directly and does not need Home Assistant.
- **Spotify:** needs Premium. The account here is older than December 2024, so the librespot backend works
  (newer accounts need the official Soloist backend). The "Use the Spotify app" pairing advertises the
  wrong IP in a two-interface pod, so use the **browser login**, which redirects to a loopback address:
  let it time out, then paste the dead URL back into Music Assistant. There is **no "prefer local over
  Spotify" setting**; duplicates are linked into one item and can be linked by hand.
- Built-in **cloud remote access** exists; keep it off unless wanted.

### What a scan does

- Full scan of every Filesystem source every 12 hours, plus manual. No file watching.
- A file is re-read when its mtime (in whole seconds) changes. A tag rewrite therefore reprocesses the
  files in place; a tag edit that preserves the mtime would be missed.
- A scan that finds nothing, or fails part way, never deletes anything, so a broken mount cannot wipe
  the library.
- Ignored automatically: files and folders starting with `.` or `_` (so `_Doorbel` and `_Nieuw` in the
  share are invisible), `#recycle`, `@eaDir`, non-audio files, non-UTF-8 names. Other Synology internals
  starting with `@` are **not** special-cased.
- Two sources with overlapping albums merge by name/artist/year, or by MusicBrainz ID, which this library
  mostly lacks, so expect a few duplicates between the legacy library and new downloads.

### State, backup and upgrades

`/data` holds the database, settings and the Spotify credentials. A rescan rebuilds the library, but
**playlists, favourites and play counts live only in `/data`**. The volume is `protect: true`, which the
Longhorn storage lib also opts into the 2-hourly snapshots and the daily backup. Database migrations run at
startup and a downgrade after a migration is unsafe: **snapshot `/data` before bumping the image tag**, and
read the release notes first (breaking changes between 2.x versions are not enumerated anywhere).
Memory has no official figure: 1Gi request / 2Gi limit is a starting guess, and the first library sync
(16 parallel tag reads) is the heavy part.

## slskd

Image `slskd/slskd`, pinned to the newest stable tag (ignore `canary` and the rolling
`0.26.0.<build>-<sha>` tags). Runs non-root (uid 1000, `fsGroup` for `/app`); do not combine that with
`PUID`/`PGID`, the entrypoint exits.

- **Credentials** come from Vault `kv/apps/slskd` (web UI user/password, API key, JWT key, Soulseek
  account). The web UI defaults to `slskd`/`slskd`, so they are always overridden. Soulseek has no
  signup: the first login with an unused username creates the account.
- **No inbound port, no VPN.** The Soulseek listen port is a container port only; it is not forwarded
  on the router and not exposed with a LoadBalancer. Consequence: two peers that are both unreachable
  cannot connect to each other, so some search results will never start, and some peers refuse users that
  share nothing. The share is an empty folder on purpose, so nothing from the personal library is exposed;
  adding a few files is a decision for after the one-week trial. Egress is the normal home connection,
  and the legal side of what is downloaded is the owner's responsibility.
- **If results are too thin**, the options are a forwarded port (TCP 50300 to a LoadBalancer service) or a
  VPN with port forwarding; neither is built.
- **Directories:** slskd does not create overridden directories itself; the kubelet creates the `subPath`
  folders on the Downloads share on first start. Music Assistant mounts `slskd` read-only with a
  `subPath`, which **fails until that folder exists** (`CreateContainerConfigError`, retries by itself),
  so slskd should come up first.
- **Health:** `/health` is unauthenticated and is the probe.
- Downloaded files are played as delivered. They are **not** tagged: a file without an artist tag gets the
  part of its filename before the first ` - ` as artist, which for `NN - Title - Artist.mp3` is the track
  number. Tag new downloads (Picard, or the cleanup script) before they are scanned; the intake tooling
  (beets or Lidarr) is deliberately undecided, see [Remaining work](../06-todo/music-stack.md).

## The legacy library and its tag cleanup

The library was audited read-only (tags parsed on the NAS itself, nothing copied over the network):

- ~100 album folders, strictly flat, ~4000 MP3 files, no lossless. About 60 folders are compilations
  (Serious Beats, Tomorrowland, Q-Base, …); most files are 128 kbps. Audio files untouched since ~2017.
- **Album artist is the album title** on ~88% of the files. That was deliberate (an old tagging guide:
  "give album artist and album the same name"), and it is exactly wrong for Music Assistant: the album
  artist is used verbatim, so `Rammstein (Mutter)` becomes an artist, and ~150 fake artists would appear.
  The compilation flag is set on no file; multi-disc sets are split into separate albums; ID3v2.3 with an
  ID3v1 block; 131 artist names differ only by letter case; some guest artists are glued on without a
  separator (`Black Eyed PeasJustin Timberlake`).

What Music Assistant expects, which the cleanup produces:

| Tag | Rule |
|---|---|
| `ALBUMARTIST` | The real artist, or **exactly `Various Artists`** for compilations. No other spelling (`Various`, `VA`, `Verschillende artiesten`) is recognised and would become its own artist |
| `TCMP` | `1` on compilations. It only marks the album type, it does **not** set the album artist |
| Disc | `TPOS` = `n/total` on one album; a `CD1`/`CD2` suffix in the album name is *not* understood, so the script strips it and merges the discs |
| Multiple artists | Only `;` (or a multi-value ID3v2.4 frame) splits. `/` does not (AC/DC). Free text is also split on ` feat. `, ` ft. `, ` vs. `, ` presents ` |
| Encoding | ID3v2.4, UTF-8, ID3v1 removed |
| ReplayGain | `REPLAYGAIN_*` tags are read and preferred over Music Assistant's own measurement (reference −18 LUFS) |

`scripts/music/cleanup.py` does this (see its README): **dry-run by default**, per-folder overrides, a
review flag for ambiguous folders, `--apply` to write, `--replaygain` to add loudness tags in the same pass
(so every file is modified once), `--folder-covers` to write `cover.jpg` from embedded art. The rules
(compilation = no artist above 60% of the tracks and at least 6 artists; dominant artist otherwise) were
trialled on a header-only copy of 12 folders before being committed. The first real run still needs:

1. the dry-run over the whole library, reviewed by hand (the review list, `KnuffelRock` with its 15 albums
   in one folder, `Z_Singels` with 400 loose singles);
2. the trial on a copy in a throwaway Music Assistant;
3. the real run **before** the real provider is added, so the first scan sees clean tags. The files are
   backed up on the NAS; no snapshot was taken because nothing else depends on the old convention.

Run it on the NAS itself (ReplayGain reads every file, ~19 GB), not over SMB.

## Decisions and why

- **Music Assistant in `smarthome` with a VLAN 5 leg**, not host networking or a firewall hole: it matches
  the existing Multus pattern and needs no firewall rule. Option rejected: leave it on the server VLAN and
  open Intern↔Servers, because mDNS discovery does not cross VLANs and the Roam moves around.
- **Fix the tags in place** instead of keeping an untouched copy: backups exist, nothing else relies on the
  old convention (Plex and the NAS indexer only read it, Audio Station is not in use).
- **No inbound port for Soulseek** for a one-week trial: reversible and lowest exposure.
- **New downloads on the Downloads volume** (subPath), not a second share: no extra NFS path to manage.
- **Intake tooling and Lidarr deferred** until slskd has proven itself. Lidarr is weak for compilations and
  its dependencies (Prowlarr, the download client) live on the NAS, not in this cluster.

## First rollout checklist

See [Remaining work](../06-todo/music-stack.md): it lists exactly what to confirm after the first sync.
