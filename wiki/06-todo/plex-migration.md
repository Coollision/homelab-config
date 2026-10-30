# TODO — Plex: move to the cluster, and fix the missing iGPU on the second mini

[← Back to Home](../Home.md) · Related: [Media stack](../03-media-stack/overview.md) · [Ansible](../00-setup/ansible.md) · [Storage](../01-platform/storage.md)

> **Status (2026-10-30):** phase 1 (the GPU fix) is **done on both minis**. Plex itself still runs as the DSM package on
> the NAS and nothing about it changed. Phase 3 (moving Plex) is optional and the case for it is weak. Still open: the
> transcode benchmark on the minis and the Phase 2 decision. Delete this page, folding the outcome into the media-stack
> overview, once the phases you chose are done.

## TL;DR

- Plex on the NAS already does **hardware transcoding** (hw decode + hw encode on every video transcode) and the
  NAS is not short of CPU for it. Moving Plex is a GitOps and tidiness decision, not a performance fix.
- The second mini (N150) has **no `/dev/dri`**: the Debian 12 kernel (6.1) does not know that GPU's PCI ID. Fix:
  kernel from `bookworm-backports` (6.12). The first mini (N100) has `/dev/dri` but **hardware transcoding does not
  work there either** until the missing i915 GuC/HuC firmware (`firmware-misc-nonfree`) is installed. Both fixes are
  independent of Plex and are the real prerequisite for any move.
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

## Open questions

- [x] Why did the N150 mini's i915 get no probe error in `dmesg`? Answered: the 6.1 kernel simply had no matching ID;
      on 6.12 it probes, loads the firmware and creates the render node. The gate in 1a keeps it from regressing.
- [ ] Is the NAS VM that dominates its CPU needed? If not, that is the real headroom win on the NAS.
- [ ] Does the N100 actually beat the NAS iGPU? Firmware is now installed on both minis; unknown until the benchmark is re-run.
      Expect the gap to matter for 4K HDR tone-mapping and AV1, not for 1080p to 720p.
