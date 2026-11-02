# TODO — Plex: move to the cluster, and fix the missing iGPU on the second mini

[← Back to Home](../Home.md) · Related: [Media stack](../03-media-stack/overview.md) · [Ansible](../00-setup/ansible.md) · [Storage](../01-platform/storage.md)

> **Status (2026-10-30):** phase 1 (the GPU fix) is **done on both minis**. Plex itself still runs as the DSM package on
> the NAS and nothing about it changed. Phase 3 (moving Plex) is optional and the case for it is weak. Still open: the
> transcode benchmark on the minis and the Phase 2 decision. Delete this page, folding the outcome into the media-stack
> overview, once the phases you chose are done.
>
> **Update (2026-11-01):** the Intel GPU device plugin is installed (`system/inteldeviceplugins-system`) and advertises
> `gpu.intel.com/i915` with 4 sharing slots per mini. A Plex pod should request that resource instead of mounting
> `/dev/dri`; it shares the iGPU with the `whisper-cpp` speech-to-text server.

>
> **Update (2026-11-02):** scale-out ideas (ClusterPlex, an own Go shim and operator, splitting one stream across
> transcoders) were researched and parked without a decision; see "Scaling beyond 3 streams". Mini iGPU benchmark
> results are recorded there.

## TL;DR

- Plex on the NAS already does **hardware transcoding** (hw decode + hw encode on every video transcode) and the
  NAS is not short of CPU for it. Moving Plex is a GitOps and tidiness decision, not a performance fix.
- **Fixed (verified 2026-11-01):** both minis are on Debian 13 (kernel 6.12) with `firmware-misc-nonfree`. Both have
  `/dev/dri/card0` + `renderD128`, and i915 reports GuC submission/SLPC/RC enabled, and `gpu_check.yml` (container `vainfo`, H.264 + HEVC decode and encode) passes. The N150 mini originally had no
  `/dev/dri` because the Debian 12 kernel (6.1) did not know its PCI ID, and the N100 mini lacked the GuC/HuC firmware.
  The text below keeps that history.
- The NAS benchmark shows plenty of headroom (3 concurrent 1080p HEVC streams at ~2.5x realtime each), and most
  transcodes come from client quality caps that can be changed for free (see "Cheaper wins").
- **If Plex moves, run one plain PMS pod pinned to a GPU node.** Distributed transcoding (ClusterPlex) is feasible but
  is not worth its complexity at this load. kube-plex is dead and cannot work with Plex on the NAS anyway.

## Second-round findings (2026-10-22)

1. **The N100 mini's iGPU does not work for transcoding today either.** `/dev/dri/renderD128` exists, but the Intel
   media driver (`iHD`) fails to initialise there ("init failed"). i915 reports `GuC firmware i915/tgl_guc_70.bin:
   MISSING` and `HuC firmware i915/tgl_huc.bin: ERROR`: `/lib/firmware/i915` is not installed on either mini.
   Gen12 needs it for the media driver (well-known with Debian 12 and 12th-gen Intel; install
   `firmware-misc-nonfree`). Not yet proven end to end: confirmed after the install plus a reboot (phase 1).
   So **both** minis need the firmware, and only the N150 mini also needs the kernel.
2. **Why Plex transcodes at all (video transcodes only, 124 plays):** almost all are *client quality caps*, not
   incompatibility. "4 Mbps 720p" / "2 Mbps 720p" profiles dominate: Android TV remote (~50), Plex Web **on the LAN**
   (~30 at 4 Mbps 720p, ~18 at "Original" that still transcode), plus the Plex setting `WanPerStreamMaxUploadRate`
   = 4000 kbps and `WanTotalMaxUploadRate` = 27000 kbps. A further ~36 plays only transcode the *audio*
   (EAC3/AC3 → AAC for Plex Web and mobile) with video copied, which is cheap. Only ~13 plays are 4K sources.
   `TranscodeCountLimit` is **3**, equal to the measured peak concurrency: a 4th simultaneous transcode would be
   refused, and nothing here can show whether that ever happened.
3. **NAS iGPU benchmark** (same jellyfin-ffmpeg build in a container, VAAPI decode → scale → H.264 encode at 4 Mbps
   720p, 60 s clips, NAS also running its normal VM load): 1080p H.264 → 6.9x realtime, 1080p HEVC 10-bit → 5.6x,
   4K HEVC 10-bit → 3.8x. **Three concurrent 1080p HEVC streams: ~2.5x each**, still far above realtime. Software
   x264 for comparison: ~0.8x, so without a working GPU the NAS could not keep up. Conclusion: the NAS has
   comfortable headroom for this workload; startup latency is the only thing a faster GPU could help.
4. **The N100 side could not be benchmarked** (point 1). Re-run the same test after phase 1 so the "faster GPU"
   claim rests on a number. The test is cheap: temporary pod pinned to the node with `/dev/dri` mounted,
   clips copied to `/tmp`, then delete pod, image and clips.
5. **Container Manager is already installed on the NAS** and runs a few containers, so "Plex in Docker on the NAS"
   (needed for any ClusterPlex variant) is available without installing anything.

## Cheaper wins that need no migration

Ranked by effort versus effect; none of these require moving Plex.

**Local versus remote are separate knobs (checked 2026-10-22).** Plex decides "local" from the client address, and the
caps differ:

| Knob | Where | Applies to | Current value / finding |
|---|---|---|---|
| What counts as local | server → Network → LAN Networks (`LanNetworksBandwidth`) | decides local vs remote | the two client VLANs, comma-separated **without spaces** (a space makes Plex silently ignore the entry). Your own plays are all classified local, none remote, so it works. A client on any other VLAN or a VPN subnet would be treated as remote and capped. |
| Remote per-stream cap | server → remote bitrate limit (`WanPerStreamMaxUploadRate`) | **remote only**, never LAN | 4000 kbps, total cap 27000 kbps |
| Home-streaming quality | each client app, separate from its Internet-streaming quality | local playback | per device; the server cannot change it |
| Internet-streaming quality | each client app | remote playback | set by each remote viewer on their own device |

What the data says per group:

- **Your own devices (all local, 60 video transcodes).** Every one of the 49 Plex Web transcodes is from Chrome. 28 of
  them ran at "4 Mbps 720p" with sources averaging ~7.8 Mbps, so the browser's *home* streaming quality is capped
  (growing: 3 in Aug, 11 in Sep, 14 in Oct, so a stale per-browser setting, not a server limit). The fix is on that
  browser: Plex Web settings → Quality → home streaming to Maximum/Original, and turn off automatic quality adjustment.
  Menu labels vary by Plex Web version; the setting lives in the browser, so redo it per browser profile.
  Another 18 Plex Web transcodes already run at Original; 15 of those carry subtitle *conversion*, so subtitles
  (not quality) force the video transcode there. Text subtitles (SRT) the browser can render avoid it; image
  subtitles (PGS) force a burn-in.
- **Samsung TV and Android phone (local).** A handful at 2 Mbps / 4 Mbps profiles: same fix, in each app's home quality.
- **Remote viewers (7 other accounts, all remote, ~64 video transcodes).** Their own apps often sit at "2 Mbps 720p"
  or "4 Mbps 720p", and the server cap is 4 Mbps while the sources average ~6.6 Mbps. So these transcodes are
  partly intentional (protecting the uplink). Not a defect, and it costs the NAS little (hardware transcode).
  Raising the cap only helps viewers whose own app is also at Original.

- [ ] Plex Web on the LAN: set home streaming quality to Maximum on the browser(s) you use. ~23% of all video transcodes.
- [ ] Optional: preferred text subtitles over image subtitles for the titles you watch in the browser.
- [x] **Superseded 2026-11-03:** `WanPerStreamMaxUploadRate` is now 27000 (equal to the total cap), because Plex treats the
      LAN Chrome client as remote and applies the cap to it; see "Why LAN Plex Web got the remote cap". The 4 Mbps
      decision below was made 2026-10-23.
- [x] `WanPerStreamMaxUploadRate` stays at 4 Mbps (decided 2026-10-23). Raising it to ~8–10 Mbps would let more remote
      viewers direct-play 1080p, at the cost of up to 3 × that on the uplink (total cap 27 Mbps, which would then
      need raising too). Revisit if remote viewers complain about quality, if the uplink gets faster, or if remote
      transcodes ever start to strain whatever machine runs Plex. Re-check with the Tautulli queries in the
      measurements section (remote video transcodes and their client profiles) before changing it.
- [ ] Consider raising `TranscodeCountLimit` from 3 to 4–5: the benchmark says the NAS can sustain 3 concurrent at ~2.5x.
- [ ] Audio-only transcodes are expected for browsers; if a given client matters, an AAC/AC3 track on the title avoids
      them. Not worth bulk work.
- [ ] Set the transcoder temp directory off the SSD volume only if I/O ever shows as the bottleneck: the NAS has
      ~5.8 GB tmpfs but its VM already uses ~6.5 GB of its 11.8 GB RAM, so measure first.
- [ ] Pre-made 720p/4 Mbps copies (Plex Optimized Versions or an arr-side profile) only for titles that remote
      viewers keep requesting; trades disk for CPU and is probably not needed.

## Measurements behind this (Tautulli, 2026-07-22 → 2026-10-22)

| Fact | Value |
|---|---|
| Plays | 245 in 3 months, peak 3 concurrent |
| Transcodes | ~64% of plays; mostly 1080p → 720p (Chrome, Android), **all hardware decode + encode** |
| 4K → 1080p transcodes | 7 plays in 3 months (the only case where a newer iGPU would matter) |
| NAS | 4-core Celeron J4125 with working `/dev/dri`; load avg ~2, dominated by a VM, not Plex (Plex ~0.3%) |
| Plex data | ~14 GB on the NAS SSD volume (9.2 GB metadata, 2.7 GB plug-in support, 1.3 GB media/thumbs) |
| Libraries | ~755 films, ~162 series, ~153 albums, media on the NAS HDD volumes |
| Minis | both N-series, 16 GB RAM, ~23–29 GB free root disk; ~4–5 GB RAM free in practice |

Power is a wash: NAS disks never spin down regardless, the minis never reach deep package C-states.

## Phase 0 — Backups (before anything)

- [ ] Stop the Plex package briefly and copy the whole AppData folder aside (or take a snapshot of that volume).
- [ ] Note the exact Plex version and the library paths as Plex stores them (they are DSM paths).
- [ ] Optional trim to leave disk headroom on a mini: empty Cache, prune old Plug-in Support logs, run Plex's
      "clean bundles" and "optimize database".

## Phase 1 — Fix the GPU on the N150 mini (kernel from backports)

**Root cause (verified 2026-10-22):** PCI device `8086:46d4` is present and healthy (BARs mapped, no BIOS problem),
but the running kernel's `modules.alias` only lists `46D0`, `46D1`, `46D2` for i915. A manual bind fails with an I/O
error. The N100 mini is `46d1`, which is why it works. Debian 12's 6.1 stable kernel never received the extra
ADL-N IDs; `bookworm-backports` ships 6.12 (6.12.95 at the time of writing), which does.

Also missing on **both** minis: i915 firmware (`/lib/firmware/i915`). This is not optional: with
`tgl_guc_70.bin` MISSING and `tgl_huc.bin` ERROR, the `iHD` media driver fails to initialise on the N100 mini
(tested 2026-10-22 inside a container), so its `/dev/dri` is currently unusable for transcoding. It is in
`firmware-misc-nonfree`, available from the `non-free-firmware` component already enabled in the sources. The N100
mini therefore also needs the package plus a reboot (one `cluster_update` pass), but not the backports kernel.

### 1a. Ansible changes (done 2026-10-30)

Implemented as **auto-detection inside `roles/node_update`** (no per-host flags), so a future kernel or hardware change
fixes itself during the normal update run:

1. **Detect:** read the Intel VGA PCI IDs with `lspci`; nodes without an Intel iGPU (VM, Raspberry Pi) skip everything.
2. **Decide:** if the newest installed kernel's `modules.alias` lacks the PCI ID, pull `linux-image-amd64` from
   `<release>-backports` (bookworm on x86 only); install `firmware-misc-nonfree` when missing.
3. **Pre-reboot gate:** never reboot into a kernel that does not know the GPU.
4. **Post-reboot check (before uncordon):** the render node exists, i915 loaded and authenticated the GuC/HuC firmware,
   and a throwaway container with a current VA driver lists H.264 and HEVC decode and encode on the render node.
5. **Standalone:** `playbooks/gpu_check.yml` runs the same assertions with no changes (smoke test after any kernel update).

**Why the VA check runs in a container:** Debian 12's host `intel-media-va-driver` (23.1.1) is too old for the N150's
PCI ID (`vainfo` fails with "init failed" even though the hardware and kernel are fine); a current driver (25.x, as
used by workloads and by Debian 13) works. So the host keeps only the kernel and firmware, and the old host VA packages
were purged. Use a container with a recent driver for Plex.

### 1b. Rollout (done 2026-10-30)

- [x] Dry run, then a real run per node (single cordon, drain, reboot, uncordon pass, Longhorn healed in between).
- [x] N150 mini: kernel 6.12 from backports, firmware installed, `/dev/dri/renderD128` now exists, GuC and HuC
      authenticated, container `vainfo` lists H.264 and HEVC decode plus low-power encode.
- [x] N100 mini: firmware installed on its 6.1 kernel, render node present, container check passes.
- [ ] Re-run the benchmark from "Second-round findings" on both minis and fill the numbers in here. The NAS reference is
      6.9x / 5.6x / 3.8x for 1080p H.264 / 1080p HEVC / 4K HEVC, ~2.5x each with three concurrent.
- [ ] If good, decide whether to move the N100 mini to 6.12 as well for consistency (it works on 6.1; kernels differing
      between minis is acceptable). The planned Debian 13 upgrade (a separate playbook, not merged yet)
      brings 6.12 and a current VA driver natively and makes the backports kernel unnecessary.
- **Rollback:** the old 6.1 kernel stays installed on the N150 mini; pick it in GRUB.

### 1c. The full update/upgrade run (done 2026-10-30)

A full `cluster_update.yml` run (master first, then workers, one node at a time) patched all four nodes, took the
N150 mini to the 6.12 kernel and moved the Pi node to k3s v1.36.3. The GPU check ran inside it.

## Phase 2 — Decide where Plex lives

Three realistic shapes. Option A is the status quo and is a valid answer.

| | A. Stay on the NAS | B. One PMS pod in the cluster | C. PMS on the NAS + ClusterPlex workers in the cluster |
|---|---|---|---|
| Transcoding | NAS iGPU (works today) | mini iGPU (N100 today, either after phase 1) | worker minis, NAS idle |
| Complexity | none | medium: one workload, one data volume | high: Plex in Docker on the NAS, orchestrator, workers, shared transcode dir |
| Failure modes | NAS = single point (already true) | node = single point unless GPU plugin is added | adds orchestrator/relay bugs (below) |
| Worth it at 3 concurrent 1080p→720p streams | yes | marginal | no |

### Can ClusterPlex or kube-plex work with Plex on the NAS and the workers in the cluster?

Investigated against the upstream repositories and issue trackers on 2026-10-22.

**ClusterPlex** (`pabloromeo/clusterplex`): a modified PMS (a shim replaces Plex's transcoder binary, plus an
nginx relay) talks to an orchestrator, which dispatches jobs to worker containers over websockets.

- **The DSM Plex package: not possible.** The shim must replace the transcoder inside the Plex install. The
  supported ways are a docker mod on the LinuxServer Plex image, or the project's own PMS image. A package update
  would overwrite a hand-patched DSM install. "Native OS support" was requested upstream (issue closed as stale
  in 2025; a contributor offered to build it in January 2026; nothing merged as far as I could see).
- **Plex in Container Manager (Docker) on the NAS with the ClusterPlex docker mod: feasible.** The DS920+ has
  Container Manager and `/dev/dri` can be passed through. Costs: migrate the DSM package's AppData into the
  container's config volume; keep library paths identical (the DB stores them), so the media mount point inside
  the container must equal the DSM path; workers mount the same NFS exports at the same paths.
- **Requirements:** media and the `/transcode` directory shared at **identical paths** between PMS and every
  worker; workers also need a `/codecs` volume. The Plex config is not shared. With PMS on the NAS, the NAS
  exports both the media and a transcode directory on its SSD volume over NFS.
- **Known problems:**
  - An open issue (#377, October 2026) reports **5–8 s extra startup/seek latency** with a shared transcode
    volume; the maintainer's suggestion is an NFS server local to PMS, which is what PMS-on-the-NAS would give,
    but whether 1 GbE NFS is fast enough is untested.
  - An open issue (#328, open since 2024, still reported in 2026) reports repeated **502 errors from the relay**
    (duplicate `Content-Length` header), often "works eventually, slow to start".
  - Hardware acceleration on workers is **not a documented feature**. The upstream threads on Intel QuickSync are
    from Docker Swarm users, one of whom gave up. On Kubernetes it is plausible (a worker is a normal Plex
    container with `/dev/dri` mounted) but unproven by the project, so it would need testing here.
- **Health:** actively used (~590 stars, Helm chart, tested on Kubernetes), but a single maintainer, the last
  release was October 2025 and the last commit February 2026, with most open PRs being dependency bumps.

**kube-plex**: the original (`munnerz/kube-plex`) has had no commits since 2023, and the maintained fork
(`ressu/kube-plex`) was **archived in October 2025** (last release January 2024). It also launches transcode
pods through the Kubernetes API from inside PMS, so PMS has to run in the cluster; it cannot work with Plex on
the NAS. Not an option.

**Verdict:** for Plex on the NAS plus cluster workers, only ClusterPlex-in-Docker is possible, and it adds an
orchestrator, a fork-style Plex image and two open bugs to fix a problem that does not exist. If Plex moves into
the cluster, a single PMS pod beats both projects. Revisit ClusterPlex only if concurrent 4K transcodes become
common or the single iGPU saturates.

## Phase 3 — Move Plex into the cluster (option B, only if chosen)

Not started. Design notes so the decision is informed:

- **Workload:** one PMS StatefulSet/Deployment (LinuxServer image keeps the config layout familiar), pinned to a
  node with a working render node. Initially the N100 mini; after phase 1 either mini via a node label.
- **GPU access:** mount `/dev/dri` (hostPath) and add the `render` supplemental group, or use the Intel GPU
  device plugin with node feature discovery so the scheduler follows the GPU (gives failover once both minis work).
- **Config volume:** Longhorn RWO, not NFS (SQLite and file locks). Size it for ~14 GB plus growth, after the phase 0
  trim; minis only have 23–29 GB free, so watch the replica count and Longhorn reserve.
- **Media:** read-only NFS mounts from the NAS at the **same paths Plex already has in its DB**, with `soft`
  mount options. This is the Synology-to-cluster pattern that has not wedged before; the earlier SMB-operator
  wedge involved Longhorn RWX share-managers disappearing on scale-to-zero, a different situation.
- **Transcode dir:** local `emptyDir` on the node (fast, no network hop).
- **Sablier / scale-to-zero:** do not enrol it; remote access, Tautulli and prefetcharr need it reachable.
- **Dependents to repoint:** Tautulli, prefetcharr (Plex token and URL), Radarr/Sonarr connections, port 32400
  forwarding and the custom access URL, DNS, ingress, the Plex Pass claim, Home Assistant integration.
- **Cutover:** copy AppData with Plex stopped, start the pod, verify libraries, users, watch state and a
  hardware transcode (Tautulli shows hw decode/encode), then repoint dependents. Keep the DSM package stopped but
  installed as rollback for a couple of weeks.
- **Acceptance:** same watch history in Tautulli, hardware transcodes still `hw decode/encode = 1`, a 4K →
  1080p transcode works, nightly Butler window does not push either mini over its memory limit.

## Scaling beyond 3 streams: ideas explored (2026-11-02, parked)

Goal that prompted this: grow past the cap of 3 concurrent transcodes, and transcode heavier material on low-power
hardware, while watching at 2x–3x playback speed. **Nothing was built or decided.** This section records what was
researched so it can be picked up later. Findings marked *(inferred)* come from agent reasoning, not from a
measurement or a fetched source; re-check them against a real transcoder command line in the PMS logs before relying on them.

### Facts that frame the decision

- **The cap of 3 is a setting, not hardware.** `TranscodeCountLimit` = 3 equals the measured peak concurrency. The NAS
  benchmark sustained 3 concurrent 1080p HEVC at ~2.5x each, so the limit can likely rise before any scale-out.
- **One stream is one process on one iGPU.** More nodes add parallel *streams*, not speed for a single stream.
- **2x–3x playback** *(inferred)*: Plex clients speed up playback themselves and just fetch segments sooner, so a
  transcode must sustain `speed × 1.0` realtime. Single-stream rates (NAS: 6.9x / 5.6x / 3.8x) cover 3x except
  4K HEVC 10-bit, which is marginal. The real constraint is concurrency on one iGPU.

### ClusterPlex as the off-the-shelf option

- Upstream chart (1.1.10) has **no GPU support** (no `gpu.intel.com/i915` request, no device mounts, no nodeSelector)
  and **no autoscaling** (`worker.config.replicas` is fixed); KEDA is not installed in this cluster.
- **Slow update path:** last release v1.4.16 on 2025-10-12, last commit 2026-02-12, 19 open issues, one maintainer.
  It tracks Plex through the LinuxServer image (`latest` / `VERSION=docker`), so PMS updates fast but the shim does not.
  Pin PMS, worker and `clusterplexVersion` to the same Plex build if ever tried.
- **History of breakage** (issue tracker, checked): most were codec/EAE version discovery after a Plex update
  (2021, twice in 2022, 2024 with PMS 1.40.3+). Fixes mostly took 1–7 days, the slowest 4–11 weeks. Others: EAE
  watchfolder permissions, a transcoder that does not exit, HDR hardware transcode, LinuxServer mod not loading.

### The "own Go build" gamble (design only)

Stock LinuxServer Plex plus a Go shim as an LSIO docker mod (renames `Plex Transcoder` to `Plex Transcoder.real`,
forwards argv/env to a worker), plus an operator.

- **Architecture:** a warm worker per GPU node scaled to 0 on idle; a Job per session is ruled out (every seek would
  mean a 3–8 s cold start). **Local fallback** (run the real transcoder if no worker answers in ~1.5 s) makes it safe.
- **Paths:** identical `/media`, `/transcode`, `/codecs` mounts on PMS and workers, no argument rewriting.
- **Operator:** one `PlexServer` CRD; rolls Plex updates through a canary (config copy, codec pre-warm, synthetic
  transcode, then roll PMS) and keeps the previous digest. Rollback after a Plex DB migration is **not** reliably
  reversible, so back up the DB before every upgrade.
- **Plex hooks worth using:** polling `/status/sessions`, `/transcode/sessions`, `/identity`. Webhooks need Plex Pass
  and are lossy. Do not write to the Plex DB, parse logs or depend on plex.tv.
- **Breakage surface** (likelihood per Plex release): codec dir layout and EAE **high**; localhost callback flags
  medium (a worker-local proxy avoids rewriting them); argv shape medium-high but passed through untouched; binary
  path low. Expect roughly 2–4 shim fixes a year, about one of them real work *(inferred)*. A nightly canary that runs a
  real SDR, HDR and EAC3 transcode against each new LSIO tag, gating the Renovate bump, is the mitigation.
- **Effort:** about 3,800 lines of Go across three milestones, roughly 3–4 weeks of evenings, plus upkeep.
- **Biggest unverified assumption:** that PMS accepts progress/manifest callbacks from a non-loopback pod. One `curl`
  from another pod against the session progress URL settles it; if rejected, remote workers do not work.
- **Verdict at today's load:** no-go on distributed transcoding. A worthwhile subset is a single stock LSIO pod
  requesting `gpu.intel.com/i915: 1` with `/transcode` on a node-local `emptyDir`, plus a small controller or
  CronJob for the nightly canary, a pre-upgrade DB backup and metrics from `/status/sessions`.

### Two transcoders on one stream

Judged **feasible on paper, not worth it** *(no live sources were fetched)*. Mechanism: the shim splits the timeline
into keyframe-aligned chunks and has different workers produce alternate segments, re-serialising progress for PMS.

- **Blockers:** audio encoder priming at every chunk join (clicks or drift); Plex's DASH/fMP4 needs byte-compatible
  init data and continuous timestamps across workers (undocumented, changes between releases); PMS assumes one
  in-order, throttled producer; rate control resets per chunk.
- **Gain is small:** with 2 workers and 6 s chunks, 4K HEVC 10-bit goes from 3.8x to about 5.0x (not 7.6x) once
  per-chunk startup is counted; 1080p H.264 gains almost nothing. Break-even is lost when startup exceeds chunk
  length divided by single-worker speed. No known project does this for live playback; Tdarr and similar are offline.
- **Cheap kill-or-confirm test** (stock jellyfin-ffmpeg, only proves the Jellyfin path): two ffmpeg processes making
  alternate 30 s HLS chunks (`-force_key_frames`, `-copyts`, `-segment_start_number`), a hand-written playlist, played
  in hls.js at 2x and 3x. Pass: at least 1.6x speedup and clean joins. Run it only when the GPUs are idle.

### Jellyfin / Seerr, asides

- **Seerr** is the merged Overseerr + Jellyseerr (merged February 2026, from third-party sources); supports Plex,
  Jellyfin and Emby and talks to Radarr/Sonarr by URL and key. It works with Plex today and commits to nothing.
- **Jellyfin** is an easier remote-transcode target (plain ffmpeg argv, stable interface, rffmpeg exists; expect 0–1
  fixes a year) but gives up the Plex client apps, remote-user experience and watch history. Not a transcoding reason
  to switch for this load.

### Benchmark results (both minis)

Measured 2026-11-02 with jellyfin-ffmpeg 7.1.2 (container image 10.11.3), `iHD` 25.3.4 on `/dev/dri/renderD128`
on both minis (H.264 and HEVC Main10 decode and encode). Method: VAAPI decode, scale to 720p, H.264 encode at 4 Mbps,
real 60 s clips cut from the library, median of 3 runs, `whisper-cpp` idle. The N100 mini and the N150 mini are 4 cores each.

Single stream, x realtime:

| Clip | N150 mini | N100 mini | NAS baseline |
|---|---|---|---|
| 1080p H.264 | 11.2x | 10.2x | 6.9x |
| 1080p HEVC 10-bit | 9.8x | 8.8x | 5.6x |
| 4K HEVC 10-bit SDR | 7.3x | 5.4x | 3.8x |
| 4K HDR10, `tonemap_vaapi` to SDR | 2.94x | 2.58x | not measured |

Concurrent streams, x realtime **per stream** (aggregate in brackets):

| Test | N150 mini | N100 mini |
|---|---|---|
| 1080p HEVC, 3 streams | 4.0x (12.0x) | 3.04x (9.1x) |
| 1080p HEVC, 5 streams | 2.41x (12.1x) | 1.80x (9.0x) |
| 4K SDR, 3 streams | 2.70x (8.1x) | 1.78x (5.3x) |
| 4K SDR, 5 streams | 1.62x (8.1x) | 1.06x (5.3x) |

What it says:

- **Both minis beat the NAS iGPU by 1.5–2x** on every clip, so moving Plex there would be faster even before any scale-out.
- **Aggregate throughput is flat as streams are added**: the media engine is the ceiling and work is shared evenly, so
  more streams on one iGPU just divides the same capacity. Two nodes roughly add their aggregates (about 20 x for 1080p HEVC).
- **Against the 2x–3x playback goal** (a transcode must sustain at least the playback speed): single streams are fine
  everywhere. 3 concurrent 1080p HEVC streams still clear 3x on both minis (the N100 only just). Missing 3x: 4K HDR
  tone-mapping on both minis (2.9x and 2.6x), 3 concurrent 4K streams, and 5 concurrent streams of anything.
- So raising `TranscodeCountLimit` to 5 is safe for 1080p at normal speed, but heavy 4K HDR at 2x–3x is the case a
  single iGPU does not cover. That is the only scenario where a second node would matter.
- The N100 mini looked partly CPU-bound under load (host CPU 83–97%, versus 46–56% on the N150), *inferred*: prefer
  the N150 mini for Plex. Software x264 reached only 3.28x on the N150 mini and 1.17x on the N100 mini at 95–98% CPU,
  so the GPU path is clearly doing the work.
- **Not measured:** GPU engine utilisation (`intel_gpu_top` is not installed on the nodes), Jellyfin's OpenCL
  tone-map filter (Plex's tone-mapping path may differ from `tonemap_vaapi`), and Plex itself (these are ffmpeg runs).

### Why LAN Plex Web got the remote cap (2026-11-03)

Symptom: a 4K HEVC HDR10 title (about 3.5 Mbps) played in Plex Web in Chrome from the client VLAN was re-encoded to
1080p HEVC at about 3.7 Mbps and lagged on the NAS iGPU, although quality was "Original" and Tautulli/the dashboard said
`location=lan`.

**Cause chain (measured from the debug log and session data):** Plex Web sends `maxVideoBitrate=<WanPerStreamMaxUploadRate>`
in the transcode start request (`autoAdjustQuality=1`, `location=lan`); PMS logs "Clipped max bitrate ... based on
client-requested limit", and a `video.width limitation applies: 3840 > 1920` line appears with the capped bitrate; the
video is then transcoded. With the pref at 0 (or high), the client sends no/high `maxVideoBitrate` and the same file
direct-streams (only the 7.1 HE-AAC audio is transcoded). The subtitle (external SRT) is converted to ASS but **not**
burned in (`decision=transcode`, `location=segments-subs`); it was never the cause.

**Why Plex thinks the client is remote is unresolved.** PMS tags every request from the routed client VLAN `(WAN)` (the NAS
has a single interface on the server VLAN, so only that subnet is `(Subnet)`), even though the client subnet is in
LAN Networks. Same symptom is reported on the Plex forum since 2020 without a documented fix.

Tried, no effect: bypassing the DSM reverse proxy (direct `IP:32400`); LAN Networks as CIDR and as netmask form (clean value,
no parse errors); restarting PMS; `IPNetworkType` `dualstack` to `ipv4` (plex.tv still lists the server's IPv6 address as its
public address and reports `publicAddressMatches=false`, while the client, NAS and cluster share one public IPv4);
Plex Web quality set to Original. DNS is not blocking `*.plex.direct` (the local name resolves to the NAS).
Not tried: removing the custom server access URL (would break remote friends), `allowedNetworks` (drops sign-in for the
network), or giving the NAS an interface on the client VLAN.

**Decision:** `WanPerStreamMaxUploadRate` = 27000 kbps. The total remote cap (27 Mbps) and one stream per user still protect
the uplink. Everything else touched during the investigation was reverted (`LanNetworksBandwidth`, `IPNetworkType`,
`TranscoderHEVCEncodingMode`, debug logging). Worth a Plex Web bug report if it bothers again: attach the PMS version
(1.43.5), the `(WAN)` tag lines, the start request params and the plex.tv resources data.

**Benchmark caveat:** Plex actually encodes HEVC 10-bit (`hevc_vaapi`, p010) because `TranscoderHEVCEncodingMode` is `always`,
while the benchmark above used H.264 output, so absolute transcode speeds are optimistic. A re-run with HEVC output
would give the realistic numbers; switching the mode to `hevc-sources` would keep H.264 sources on the cheaper H.264 path.

### If picking this up again (cheapest first)

- [x] Benchmark both minis (done 2026-11-02, see above). 4K HDR tone-mapping is 2.9x / 2.6x, below the 4x bar, so heavy
      4K HDR at 2x–3x is the one case where scale-out would still help; everything else fits on one iGPU.
- [ ] Raise `TranscodeCountLimit` (3 → 5) based on the concurrency numbers; watch Tautulli for buffering.
- [ ] Run one stock LSIO Plex pod on a GPU node (node-local transcode `emptyDir`, `gpu.intel.com/i915: 1`, Longhorn RWO config).
- [ ] Build the canary + backup controller only if updates start to hurt; it is useful with or without workers.
- [ ] Only if one iGPU saturates: verify the loopback-callback assumption, then consider the shim and warm-worker design.

## Open questions

- [x] Why did the N150 mini's i915 get no probe error in `dmesg`? Answered: the 6.1 kernel simply had no matching ID;
      on 6.12 it probes, loads the firmware and creates the render node. The gate in 1a keeps it from regressing.
- [ ] Is the NAS VM that dominates its CPU needed? If not, that is the real headroom win on the NAS.
- [ ] Does the N100 actually beat the NAS iGPU? Firmware is now installed on both minis; unknown until the benchmark is re-run.
      Expect the gap to matter for 4K HDR tone-mapping and AV1, not for 1080p to 720p.
