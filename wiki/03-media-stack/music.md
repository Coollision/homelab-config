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
| music-intake | `workload/arr-stack/music-intake/` (code in its own repository) | Tags and files finished downloads into the library, review UI on an internal ingress, see [Intake](#intake-music-intake) |
| Tag cleanup scripts | `scripts/music/` | One-off transform for the legacy library, see [below](#the-legacy-library-and-its-tag-cleanup) |
| NFS paths | Vault `kv/shared/nfs` (`music-path`, `downloads-path`) | Same secret as the other NFS paths |

Nothing here has external ingress. Music Assistant and slskd are only reachable on the internal secure
ingress, and Music Assistant is **not** scale-to-zero (Sonos and Home Assistant hold long-lived
connections that a sleeping pod cannot answer).

## Storage

- **Legacy library:** the Music share, mounted read-only into Music Assistant at `/media/music`.
  Music Assistant only ever writes `.m3u` playlists into a library, so a read-only mount only costs it
  local-playlist editing. Covers, metadata and its database live in its own `/data`.
- **New downloads are an inbox, not part of the library.** slskd writes to a `slskd` subfolder of the existing
  Downloads share (the same `arr-stack-downloads` claim radarr and sonarr use, selected with a `subPath`), its
  incomplete files to a sibling `slskd-incomplete`. **Music Assistant has no access to the Downloads share at
  all**: a finished download only reaches the library after the intake step has tagged it and moved it into the
  Music share.
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
lease. (Done for this stub: alias `_stub_music-assistant-lan` and a reservation on the Intern VLAN.)

**The URL it hands out for itself.** Music Assistant has two *separate* addresses, and both default to the
primary IP, which in this pod is the cluster IP:

| Setting (Settings → System → Webserver / Core → Streams, both "advanced") | Used for | Value here |
|---|---|---|
| **Published IP address** (Streams, `publish_ip`) | The audio stream the players pull on port 8097 | the `net1` address |
| **Internal URL** (Webserver, `base_url`, default "auto") | The address in its server info (what Home Assistant and apps see), cover-art/proxy URLs given to clients, OAuth and setup-flow callbacks, guest links | the internal secure ingress hostname |
| External URL (Webserver, `external_url`) | A reachable-from-outside address | left empty (no external ingress) |

The Internal URL is the "running behind a reverse proxy" setting the docs describe. Both take effect
immediately (no reload). **Neither lives in git**: they are stored in `/data`, so they have to be set again
(Settings, or the `config/core/save` API) if that volume is ever recreated, along with the admin user,
the providers and the Spotify login.

**A second, easy-to-miss routing problem.** The `sbr` plugin keeps `net1`'s routes out of the main table, so
the pod by default talks *to* the players from its cluster IP, NAT'd out through the node. That is enough
for native Sonos control and for the stream the players pull from the published IP, but not for grouping a
Sonos with a non-Sonos player (for example the web player): Music Assistant then bridges the Sonos over
AirPlay/Sendspin, and the speaker has to connect **back** to the address Music Assistant announces, which is
the unreachable cluster IP. Symptom: `cliairplay did not connect to <player>`, and the Sonos is dropped from
the group after 30 seconds. The chart therefore has an init container (`setup-vlan5-route`, same pattern
as matter-server) that adds an on-link route for the VLAN 5 subnet via `net1`, so every connection to the
players originates from the `net1` address. If you ever add that route to a *running* pod, expect every
existing Sonos connection to drop once and Music Assistant to reconnect (the pod's old connections used the
cluster address). The stub's MAC follows the scheme in `lib/shared-lib/templates/_multus.yaml`
(`02:05:67:9d:64:67`, workload id 67). The Sonos Roam is portable: it withdraws its mDNS announcement
when it sleeps and Music Assistant reconnects when it announces again, so it depends on mDNS working
on that VLAN (mDNS is on for the VLAN; IGMP snooping is deliberately off, see the TV note in
[Networking](../01-platform/networking.md)).

### Providers

- **Filesystem (local):** one provider on `/media/music` (the legacy folders plus everything the intake files in).
  There is no folder-layout setting: **tags are always primary**, folders are only a secondary signal.
  Add it **before** Spotify and let the scan finish, because Spotify artwork can otherwise win.
- **Sonos:** discovered over mDNS (`_sonos._tcp`). Manual IPs exist as a fallback only.
- **Home Assistant:** the HA integration connects to Music Assistant on 8095; a ClusterIP service is
  enough, and Music Assistant controls Sonos directly and does not need Home Assistant.
- **Radio:** the **Radio Browser** provider (a free community directory, no account) is enabled, with
  *Qmusic Belgium* in the library. Radio Browser lists several Qmusic entries, mostly Dutch; pick the one with
  country BE. The Belgian station only publishes **HE-AAC 96 kbps** and **MP3 128 kbps** (probed with ffprobe;
  other mount names just fall back to the MP3 stream), so there is no higher-quality stream to switch to and
  the "LQ" badge is simply the bitrate. Both variants are in the library to compare by ear.
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
- Albums merge by name/artist/year, or by MusicBrainz ID, which the legacy library mostly lacks, so an album
  imported twice under slightly different tags can show up as two entries.

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

- **Credentials** come from Vault `kv/workload/arr-stack/slskd` (web UI user/password, API key, JWT key, Soulseek
  account). The web UI defaults to `slskd`/`slskd`, so they are always overridden. Soulseek has no
  signup: the first login with an unused username creates the account.
- **No inbound port, no VPN.** The Soulseek listen port is a container port only; it is not forwarded
  on the router and not exposed with a LoadBalancer. Consequence: two peers that are both unreachable
  cannot connect to each other, so some search results will never start, and some peers refuse users that
  share nothing. The share is an empty folder on purpose, so nothing from the personal library is exposed;
  adding a few files is a decision for after the one-week trial. Egress is the normal home connection,
  and the legal side of what is downloaded is the owner's responsibility.
- **Trial findings (day 1, 2026-10-09).** With no inbound port the setup works: searches return plenty of
  sources (a 2006 compilation: 14 users; a popular album: 150, nearly all with a free upload slot and an empty
  queue) and transfers run at about 2.5–3 MB/s. Two real album downloads were done through the API as a test
  (an artist album at 320 kbps and a 2-disc compilation). Observed failure modes, all per peer:
  - **`Banned`**: the peer refuses us (a peer that shares nothing is a common ban target). All of that peer's
    files are rejected immediately; just pick another source.
  - **A stalled transfer**: one file sits at 0 KB/s (ETA hours) and holds that peer's single upload slot, so the
    rest stay "Queued, Remotely". Cancel the user's transfers and re-queue from another source.
  - Practical source choice: free upload slot, queue length 0, a complete folder for every disc.
  Finished albums land in `Downloads/slskd/<uploader's folder name>`; after a sync Music Assistant showed them
  as proper albums (the artist album under its artist, the compilation under `Various Artists`), because
  these uploaders had tagged them correctly. Compilation downloads are often m4a or FLAC, which the ID3-only
  cleanup script does not touch.
- **If results are too thin**, the options are a forwarded port (TCP 50300 to a LoadBalancer service) or a
  VPN with port forwarding; neither is built.
- **Directories:** slskd does not create overridden directories itself; the kubelet creates the `subPath`
  folders on the Downloads share on first start.
- **Health:** `/health` is unauthenticated and is the probe.
- Downloads are **not** played as delivered: they sit in the inbox until the [intake](#intake-music-intake) tags
  and files them (a file without an artist tag would otherwise get the part of its filename before the first
  ` - ` as artist, which for `NN - Title - Artist.mp3` is the track number).

## Intake (music-intake)

slskd's finished albums are an **inbox**; nothing reaches the library, and so Music Assistant, until the intake
has tagged and filed it. It is a small Go + React service in its own (private) repository, built into a
multi-arch image on GHCR (Keel polls `latest`, like the other self-built apps), with the Python tagger from
the cleanup inside the image. The chart here only wires it up.

```
slskd -> Downloads/slskd (inbox) -> settle + slskd finished? -> analyse -> confident -> tag, file, clean ----> Music/<album>
                                                              \-> unsure -> review UI (edit, approve) -/        (Music Assistant sync triggered)
```

- **Settling.** A folder only counts as finished when it has been unchanged for a few minutes **and** slskd
  reports no unfinished transfers for it (a stalled peer leaves a half album with an unchanged timestamp).
- **Auto vs review.** Auto-filed: every track has artist, title, album and track number, at least three tracks,
  nothing had to be guessed from file names, and the target folder name is free. Everything else, notably
  compilations from uploaders with sloppy tags, an existing folder with that name, or untagged files, waits in
  the UI: edit album, album artist (one click sets `Various Artists` and the compilation flag), year, folder
  name, per-track artist/title/number, then approve. Editing an item always means it waits for a click.
- **Layout.** Flat like the legacy library: `Music/<Artist - Album>` for artist albums, `Music/<Album>` for
  compilations. Multi-disc albums keep their disc subfolders. An existing folder is never overwritten or merged.
- **What it writes.** The same tag rules as the cleanup (`Various Artists` + compilation flag, ID3v2.4/UTF-8
  without ID3v1, `CD1`/`CD2` merged with disc numbers, ReplayGain, `cover.jpg`), now for mp3, m4a and flac.
- **Safety.** Tagged in the inbox, copied into a hidden `.<name>.partial` folder in the library, renamed into place
  (the library never shows half an album), then the inbox files are removed and leftovers (nfo, scans) deleted so
  the inbox ends up empty. If tagging fails for any file nothing moves. A restart in the middle (Keel restarts the
  pod on every new image) is resumed automatically: not yet in the library -> the import runs again, already
  filed -> only the bookkeeping is finished, ambiguous -> `failed` with an explanation for a person to look at.
- **Speed.** ReplayGain dominates the import time: about a minute and a half for a 16-track mp3 album, about ten
  minutes for a 28-track m4a compilation on the 2-core pod. Set `INTAKE_REPLAYGAIN=false` if speed matters more
  (Music Assistant measures loudness itself when the tags are missing).
- **Verified 2026-10-09** with two real slskd downloads: an mp3 artist album (auto-filed as `Artist - Album`, ID3v2.4,
  ReplayGain, no ID3v1) and an m4a compilation (filed as `Album`, `Various Artists`, compilation flag, ReplayGain),
  the inbox left empty and both albums visible in Music Assistant after the sync the intake triggered itself.
- **Storage.** `/inbox` is the claim radarr and sonarr also use, with `subPath: slskd` (read-write, because it is
  cleaned out). `/library` is the Music share through its own PV/PVC in `arr-stack` (read-write here, while Music
  Assistant mounts the same path read-only). The SQLite job state is on a small Longhorn volume.
- **Wiring.** It asks slskd (same namespace) which transfers are unfinished, using slskd's API key, and triggers
  a library sync in Music Assistant after each import with a token from Vault (`kv/workload/arr-stack/music-intake`).
- **Env names** follow the config library: the config path without dots, upper-cased
  (`INTAKE_INBOXDIR`, `INTAKE_SLSKD_URL`, `DB_PATH`).
- **No login of its own** and no external ingress: internal secure ingress only.
- The tagger is Python (mutagen) on purpose for now, because it is the one mature library that writes all three
  formats including ReplayGain; a Go port (ffprobe/ffmpeg) is a possible later step.

## Lidarr (artist albums, torrents)

`workload/arr-stack/lidarr/`: Lidarr **v3.1.0 on the stable branch** (linuxserver image), PostgreSQL through the shared
CNPG cluster (role `lidarr_user`, databases `lidarr_main` and `lidarr_log`, wired like Radarr with the `arr-lib`
helper), config on a Longhorn volume, internal ingress only.

- **Scope.** Artist albums only: monitor an artist and it finds and files missing albums. **Compilations stay with
  the intake.** Lidarr deliberately has no "Various Artists" artist, the Servarr wiki says DJ-mix and compilation
  libraries "won't import well", and a compilation has to be added album by album.
- **Library root.** A separate, initially empty `Lidarr` folder inside the Music share (mounted at `/music` with a
  `subPath`, so Lidarr cannot see or touch anything else). The legacy folders and the intake's output are never
  scanned by it, because its import expects `Artist/Album` folders and the flat layout would mismatch. Music Assistant
  scans the whole share, so Lidarr's albums show up there too, as ordinary albums.
- **Acquisition.** Torrents only for now: the cluster's Prowlarr (public torrent indexers with an Audio category, no
  Usenet) and the Synology's Download Station as the download client. Lidarr mounts the Downloads share at
  `/data/downloads`, with a remote path mapping in the Lidarr UI from the NAS path to that.
- **Soulseek is deliberately not connected.** Soularr (the maintained bridge) would download through slskd into the
  same folder the intake treats as its inbox, so the intake would also pick those albums up, and Soularr has an open
  bug where its cleanup touches other slskd downloads. Torrents avoid this: they land in `Downloads`, the intake
  only watches `Downloads/slskd`. Revisit once there is a design for separating them.
- **Settings to apply in the UI** (they live in the database, not in this repo): root folder `/music`, **Recycle Bin on**
  (when the community metadata server returns nothing for an artist Lidarr can delete the artist's files), write
  metadata off, the Prowlarr application link, the Download Station client and its remote path mapping. A metadata
  profile with only studio albums is the default.
- **Metadata source.** Lidarr depends on a community-hosted mirror of MusicBrainz (the Servarr metadata server). It had a
  long outage in 2025 and looks healthy now. If it misbehaves, the symptoms are artists that cannot be added or refreshed.
- Version bumps: stay on a plain stable tag. `develop`/`nightly` only add plugin support (Tubifarry) and going back
  to stable afterwards needs a database restore.

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

Result of the real run (2026-10-09): 119 albums in 3998 files rewritten to ID3v2.4/UTF-8, ID3v1 removed;
77 compilations (3260 files) under `Various Artists` with the compilation flag; the album artists dropped
from ~150 fake entries to 31 real ones. After the first scan Music Assistant shows 116 albums (a few merge,
because they differ only in case), 31 album artists, 74 compilations, no fake artists and no case duplicates.
The run was done in two passes from a throwaway read-write pod on a quiet node (not over SMB): **pass 1** the
tag fixes (a few minutes), **pass 2** the ReplayGain loudness tags (hours, because every file is decoded
twice; run at the lowest priority and resumable, so it does not starve the node's other workloads).

What Music Assistant expects, which the cleanup produces:

| Tag | Rule |
|---|---|
| `ALBUMARTIST` | The real artist, or **exactly `Various Artists`** for compilations. No other spelling (`Various`, `VA`, `Verschillende artiesten`) is recognised and would become its own artist |
| `TCMP` | `1` on compilations. It only marks the album type, it does **not** set the album artist |
| Disc | `TPOS` = `n/total` on one album; a `CD1`/`CD2` suffix in the album name is *not* understood, so the script strips it and merges the discs |
| Multiple artists | Only `;` (or a multi-value ID3v2.4 frame) splits. `/` does not (AC/DC). Free text is also split on ` feat. `, ` ft. `, ` vs. `, ` presents ` |
| Encoding | ID3v2.4, UTF-8, ID3v1 removed |
| ReplayGain | `REPLAYGAIN_*` tags are read and preferred over Music Assistant's own measurement (reference −18 LUFS) |

`scripts/music/cleanup.py` does this (see its README): **dry-run by default**, per-folder and per-artist
overrides, a review flag for ambiguous folders, `--apply` to write, `--replaygain` for loudness tags (with
`--jobs` for parallel decodes and `--resume` to skip finished albums), `--folder-covers` to write `cover.jpg`
from embedded art. The rules (compilation = no artist above 60% of the tracks and at least 6 artists;
dominant artist otherwise) were trialled on a header-only copy of 12 folders, then dry-run over the whole
library with nothing flagged for review, before the real run. The files are backed up on the NAS; no
snapshot was taken because nothing else depends on the old convention.

Run it next to the data, not over SMB: a pod that mounts the Music share read-write (the export allows it).
ReplayGain peaks above +12 dBFS are clamped, because a single corrupt MP3 frame can report +27 dBFS and would
permanently cap that album's gain in players (loud, clipped 128 kbps MP3s normally decode to +2..+4 dBFS).

## Decisions and why

- **Music Assistant in `smarthome` with a VLAN 5 leg**, not host networking or a firewall hole: it matches
  the existing Multus pattern and needs no firewall rule. Option rejected: leave it on the server VLAN and
  open Intern↔Servers, because mDNS discovery does not cross VLANs and the Roam moves around.
- **Fix the tags in place** instead of keeping an untouched copy: backups exist, nothing else relies on the
  old convention (Plex and the NAS indexer only read it, Audio Station is not in use).
- **No inbound port for Soulseek** for a one-week trial: reversible and lowest exposure.
- **New downloads are an inbox on the Downloads volume** (subPath), and Music Assistant cannot see it: the
  library Music Assistant plays from only ever contains tagged, filed music.
- **Lidarr deferred.** It is weak for compilations and its dependencies (Prowlarr, the download client) live
  on the NAS, not in this cluster.

## First rollout checklist

See [Remaining work](../06-todo/music-stack.md): it lists exactly what to confirm after the first sync.
