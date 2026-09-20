# Auto-slicing architecture — living plan

Status: **draft, under discussion**. This file is the working copy — edit it
directly as decisions change, rather than only tracking discussion in chat.
Also published as a read-only page for easy sharing:
https://claude.ai/artifact/Jh1NgFYSvxEJQxshK3a3DQ (keep both in sync, or drop
the external page once this file is the source of truth).

Goal, in the homelab owner's own words: *"the orca engine used for slicing
anything that hits printstash (or at least after selecting filament/printer)
... simple to use for karen, a basic upload/print for her, and not much work
for me."*

---

## The finding that reshapes everything

PrintStash already ships a Spoolman integration and a per-printer material
model. It's just switched off (`spoolman_enabled=0` in `system_config`).
This means we don't need to build printer/material matching — PrintStash's
own `compatibility_for_printer()` already does it, once material slots are
populated.

---

## Live inventory (verified against the real cluster/printers)

| What | State |
|---|---|
| PrintStash DB | 5 models, 1 gcode file, 0 tags, 0 filament profiles. 4 printer rows — **2 are stale duplicate "K1C" entries (id 1, 2), delete them.** |
| Spoolman | `0.26.1`, deployed, **completely empty**, **no auth at all**. |
| Kobra S1 ACE | 4 slots live over plain HTTP (`filament_hub` Klipper object) — material, RGB color, RFID-sourced SKU. Slot 0 is Anycubic-RFID (`sku: HABVO-103`), slots 1–3 are hand-entered. |
| Kobra S1 constraints | GoKlipper + Rinkhals, **not** Klipper, **not** Happy Hare. 217MB RAM, ~105MB free. `mmu_ace.py` contains a *disabled* Spoolman emulation — comment reads `# Spoolman emulation removed - causes system freeze`. **Do not add `[spoolman]` to the Kobra — hard constraint, not a judgment call.** |
| K1C Moonraker | Has a real `spoolman.py` component available, not yet enabled — one `moonraker.conf` stanza away. |
| OrcaSlicer pod | 19 hand-tuned presets, 136KB, on a **ReadWriteOnce Longhorn PVC** (`/config`, 5Gi). A **plaintext PrintStash API key is exposed in `0.20mm Standard S1 (slice flush).json`'s `post_process` field — rotate it.** |
| Headless CLI slicing | **Verified working**, real cluster test: `lscr.io/linuxserver/orcaslicer:v2.4.2-ls39`, no Xvfb needed, `orca-slicer --slice` produces correct Klipper gcode with `DISPLAY`/`WAYLAND_DISPLAY` unset. |
| MakerWorld import | **Built and working** (this session) — see below. |

### Bug found and fixed during verification: `filament_type` silently defaults to PLA

`--load-filaments <path>` does **not** resolve the `inherits` chain. A vendor
leaf file only carries deltas; `filament_type` lives in the parent
(`fdm_filament_abs.json`). Unresolved, Orca silently falls back to its
compiled-in default (PLA) — and `filament_type` is exactly the field
PrintStash's `compatibility_for_printer()` matches on. **Fix, verified
working:** flatten each preset's `inherits` chain into a single
self-contained file at ConfigMap-build time. Must keep a `"from": "User"`
key or Orca rejects the file (`file's from unsupported`, exit -5). This also
fixes the separate "filament weight reads 0g" gap for free (flattening
guarantees `filament_density` is present).

---

## Decisions made so far

### 1. Selkies / interactive OrcaSlicer GUI → **retire it**

Reversed from an earlier "keep it" recommendation. That rested on the GUI
being the only place to author presets — with the Mac available for manual
slicing/fiddly work, it isn't. Retiring it kills: ~1GB standing RAM, a
broken Sablier scale-to-zero config (orcaslicer was missing from the
auto-generated ignore-list, so it silently never scaled to zero), an
unauthenticated port 8082 reachable from any pod in the cluster, and the
RWO-PVC storage conflict with a slicing Job wanting the same volume, all in
one move.

**Before retiring:** export the 19 presets to git (flattened, per the bug
above, and with `post_process` stripped — that's where the leaked API key
lives). The PVC is their only copy.

**Manual/fiddly slicing moves to:** OrcaSlicer on the Mac, printer profiles
pointed at the Moonraker URLs directly.

### 2. Trigger model → slice-everything, scoped smart, not tag-driven (for now)

"Generate for all printers/presets" is fine as a default, but "everything"
should mean **one slice per printer per currently-loaded material**, not
one per printer × process-preset × color combination. Reasoning:
- PrintStash's schema only allows **one recommended gcode per model**
  (unique index), and the send-to-printer picker is a flat dropdown — a
  naive full combinatorial fan-out is ~24 revisions/model, unusable.
- Color doesn't change the gcode structurally for single-material prints —
  it's header metadata. Four ABS spools in four colors don't need four
  slices.
- Result: **2 revisions per model** today (1 per printer), color riding
  along as `--filament-colour` metadata, not a fan-out axis.

**Open question, explicitly not settled:** if you'd rather have all 4 ACE
colors as separately-selectable revisions instead of 1-per-printer with
color as metadata, say so — it's a small change to the slice-set
computation, it just makes the revision dropdown busier.

Tag-driven slicing (add a tag → slice a specific combo, remove it → drop
the file) was your own idea and isn't rejected — it's **deferred**: there's
no clean event hook for tag changes in PrintStash (mutation is inlined
across 6 call sites, no shared event/webhook system), so it would cost far
more to build than the one clean `ingest_mesh()` hook already used. A
periodic reconciler (see below) can support tags as a *filter*, read at
reconcile time, without needing an event hook at all.

### 3. Spoolman wiring

- **Enable what already exists**: point PrintStash at Spoolman (`base_url`,
  `enabled=true`), leave `write_enabled=false` initially.
- **"Active spool per printer" is a client-side convention** — Spoolman has
  no printer entity and only a single *global* active-spool setting, useless
  with two printers. Use Spoolman's `location` field instead:
  `Kobra S1 / ACE 1` … `Kobra S1 / ACE 4`, `Creality K1C`.
- **RFID-vs-manual spool bridge**: a Spoolman extra field `filament.ace_sku`
  holding the Anycubic SKU. Match an ACE slot to a Spoolman filament by
  `ace_sku` when RFID supplies one, fall back to `(material, colour)`
  otherwise — this mirrors how the printer itself stores the two kinds of
  spool differently (RFID slot 0 is read live off the tag every time and
  never persisted to disk; hand-entered slots 1-3 persist to
  `ams_config.cfg`).
- **Push material state into PrintStash** via the existing, documented
  `PUT /api/v1/printers/{id}/material-state/manual`. Set
  `provider_material_sync_enabled: false` on both real printers first, so
  the reconciler's manual writes always win over stale provider rows.
- **K1C's native Moonraker `[spoolman]`**: enable later, only for
  gram-accurate usage decrementing. Keep PrintStash's own
  `spoolman_write_enabled=false` to avoid double-counting.
- **Kobra's native Moonraker `[spoolman]`**: never — see the hard
  constraint above.

### 4. Custom RFID tags on spools — confirmed feasible

The ACE's tag format (NTAG213/215/216, 13.56MHz ISO14443A) is **plaintext,
unencrypted, no signature, no UID binding** — unlike Bambu's AMS tags. NTAG215
stickers have plenty of headroom (format needs ≥144 bytes; NTAG215 gives you
more than NTAG213's minimum). Verified against the Kobra's own firmware
source (`mmu_ace.py`'s `parse_anycubic_sku()`) that the community-documented
byte layout matches exactly what this printer expects.

**Android app for writing tags: "ACE RFID" by DnG-Crafts, on the Play
Store — the zero-cost route, no extra hardware needed beyond an NFC-capable
phone.** (Repo: `DnG-Crafts/ACE-RFID`.) Alternative if you'd rather do it
from a desktop: an ACR122U USB NFC reader/writer + `Molodos/anycubic-nfc-filament`
(Python).

**Two operational gotchas, not optional to know before writing tags:**
1. **Once a tag is present on a gate, Rinkhals locks it against software
   override** (`mmu_ace.py:1516`, confirmed in source) — you cannot fix a
   wrong tag from Mainsail, only by rewriting the tag or disabling RFID
   entirely (`[filament_hub] enable_rfid: 1` in `printer.custom.cfg`, your
   own off-switch). Get the tag right the first time.
2. **Color is stored ABGR on the tag** (byte-swapped from the usual RGBA/RGB
   you'd expect) — write it in the wrong order and get a visibly wrong color.

**Proposal, not yet committed, needs a one-tag test before trusting it
broadly:** since Rinkhals derives `gate.spool_id = int(sku_suffix)` from the
tag's own SKU field, writing custom tags whose SKU suffix *is* the Spoolman
spool ID would make the gate-to-spool mapping self-describing, no extra
lookup table needed. Fallback if this doesn't hold up: match by `location`
as described in §3 instead.

**Correction, worth remembering:** `gate_spool_id` in the ACE's own status
object looks like it might be a Spoolman spool ID — it isn't. It's literally
the numeric suffix of the SKU (`mmu_ace.py:1229`), which happens to often be
a small integer that looks plausible. Never treat it as a real Spoolman ID.

### 5. Triggers, concretely: two, not one

**(a) Event trigger — extend the same `ingest_mesh()` hook already used for
nothing yet, added fresh.** `ingest_mesh()` is the single chokepoint for
every mesh entering PrintStash (upload, URL import, browser-extension
upload). On a new mesh, build and POST a Job manifest, same pattern as the
MakerWorld importer (see RUNBOOK.md) — reuse the same explicit SA-token
path, same RBAC-scoping discipline. Slice for the default printer only, to
keep this trigger's RBAC create-only and bound the bulk-import fan-out risk.

**(b) Reconcile trigger — a CronJob, every ~15 min.** Subsumes tags,
Spoolman, and spool changes in one loop:
1. Read live material state from both printers (Kobra's `filament_hub`,
   K1C's Spoolman-by-location).
2. Push it into PrintStash via `material-state/manual`.
3. Compute the desired slice set (every model × every printer's loaded
   material, minus any `noslice`-tagged model).
4. Diff against existing revisions; create Jobs for what's missing,
   soft-delete revisions for combos no longer loaded.

This needs its own ServiceAccount with `create` + `list` + `delete` on
`jobs.batch` — broader than the strictly create-only role the event-trigger
path uses, so keep them separate rather than widening the existing one.

---

## Build order

**Phase 0 — settings/data only, no code: ✅ DONE (2026-09-21)**
1. ✅ PrintStash → Spoolman: `PUT /api/v1/spoolman` with the in-cluster
   `base_url`, `enabled=true`, `write_enabled=false`. Confirmed connected,
   version `0.26.1`.
2. ✅ Stale duplicate "K1C" printer rows (id 1, 2) — already soft-deleted
   from earlier session cleanup, verified via direct DB check.
3. ✅ `provider_material_sync_enabled: false` set on both real printers
   (Kobra S1 id 3, Creality K1C id 4).
4. ✅ Spoolman seeded: `filament.ace_sku` extra field defined; vendor
   "Anycubic" + 4 ABS filaments (one per ACE color, `density=1.04`,
   `diameter=1.75`) + 4 spools bound via `location` to `Kobra S1 / ACE 1..4`.
   The RFID slot (ACE 1, orange) carries `ace_sku="HABVO-103"`; the other 3
   are hand-entered-spool placeholders (real color hex captured from
   `filament_hub`, but no known SKU). **K1C has no live spool data yet**
   (native Moonraker Spoolman integration not enabled — Phase 4) — seeded
   a clearly-labeled `PLACEHOLDER` vendor/filament/spool at
   `location="Creality K1C"` instead of guessing real material; replace
   this once the K1C's actual loaded filament is trackable.
5. ✅ `POST /api/v1/spoolman/sync-filaments` → 5 filament_profiles created
   in PrintStash.
6. ✅ Leaked API key rotated: new key minted (`orca-auto-push-rotated`),
   swapped into all 9 OrcaSlicer process presets on the `/config` PVC
   (`sed` replace of the old key string), old key revoked
   (`DELETE /api/v1/auth/api-keys/1`), Vault's `kv/apps/printstash-orca-push`
   updated to match. Also revoked `kv/apps/printstash-makerworld-importer`'s
   key/secret — unused since the MakerWorld Job redesign no longer calls
   PrintStash's API at all.

**Phase 1 — presets to git + retire the GUI (one unit — export must precede deletion):**
7. Export the 19 presets, flattened (fixes the PLA bug), `post_process` stripped.
8. Ship as a ConfigMap.
9. Retire the Selkies Deployment/PVC.

**Phase 2 — the slicing Job:**
10. Build the Job entrypoint (command override on the existing
    `lscr.io/linuxserver/orcaslicer` image — no custom image needed).
11. Wire the `ingest_mesh()` hook → one Job per upload for the default printer.
12. New RUNBOOK.md section: this is bespoke-forever (like the MakerWorld
    importer), no upstream PR to track.

**Phase 3 — the reconciler CronJob:**
13. Write one test RFID tag before tagging spools broadly (validates the
    SKU-as-spool-ID proposal in §4).
14. Material-state push + slice-set diff + backfill + retirement logic.

**Phase 4 — hardening:**
15. NetworkPolicy restricting the (now-retired, or if kept, reconsidered)
    orcaslicer pod's port 8082 to Traefik only.
16. K1C native `[spoolman]` for gram-accurate usage tracking.

**Explicitly deferred, with reasons:**
- Tag-driven slicing as a *trigger* — subsumed by the Phase 3 reconciler
  whenever wanted; not worth 6 patch sites at 5 models / 0 tags today.
- Multi-material/per-tool ACE slicing — needs per-object tool assignment,
  inherently a GUI job, out of scope for auto-slicing.
- Headless gcode thumbnails — not worth chasing, cosmetic only.

---

## Open questions (need your call before proceeding past Phase 0)

1. **Color fan-out** — 1 slice per printer with color as metadata (current
   plan), or 1 per color/slot (busier picker, ~4-6 revisions/model)?
2. **Rotate the leaked API key now?**
3. **Confirm retiring Selkies** — any manual-slicing workflow this would
   break that hasn't been accounted for?
4. **RFID SKU-as-spool-ID proposal** (§4) — worth the one-tag test, or go
   straight to `location`-based matching?

---

## Changelog

- **Rev 1** — initial plan from live investigation (printer/Spoolman/OrcaSlicer state, trigger model, build order).
- **Rev 2** — added §4 (RFID tag feasibility + Android app), reversed §1 (retire Selkies, not keep), added the `filament_type`/PLA bug + fix, corrected `gate_spool_id` (derived, not a stale ID, worse than assumed).
- **This file** — mirrors the published page's Rev 2 content; MakerWorld import section above reflects the *actual, already-shipped* architecture (single-item resolve via k8s Job + poll, not the originally-designed fire-and-forget push), documented in full in `RUNBOOK.md`.
