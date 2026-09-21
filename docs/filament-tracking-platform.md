# Filament tracking platform — requirements and investigation status

Investigation date: 2026-09-17. Discovery phase — **nothing decided, nothing built.**
Every claim below is tagged with how well it is verified. Do not treat `CLAIMED` rows as facts.

## Goal

One platform that knows every spool I own. Buy whatever filament I want, tag it myself,
enter the details once, and have both printers and my phone all agree about what is loaded
and how much is left — without it feeling like four disconnected apps stitched together.

## Requirements

| # | Requirement | Priority |
| --- | --- | --- |
| R1 | Single source of truth for all spools (inventory, cost, remaining weight, location) | Must |
| R2 | Vendor-neutral — any filament brand, no ecosystem lock-in | Must |
| R3 | Self-tagged with cheap generic NFC tags (NTAG213/215), no proprietary chips | Must |
| R4 | Details entered **once**, not re-keyed per system | Must |
| R5 | Android phone scan → full spool details | Must |
| R6 | Kobra S1 + ACE Pro 2: auto-recognise spool on insert, zero manual steps | Must |
| R7 | K1C: manual spool selection is acceptable (spools change rarely there) | Accepted |
| R8 | Feels like **one** platform — one primary UI, not a toolchain | Must |
| R9 | Self-hosted, GitOps-managed, runs on the existing cluster. No cloud SaaS | Must |
| R10 | Automatic consumption tracking / remaining weight, per spool | Should |
| R11 | Migration path to an open tag standard (OpenTag3D) without re-tagging spools | Should |

### Non-goals

- Building custom NFC reader hardware (ESP32 + PN532/PN5180). Evaluated and rejected — R7
  means the K1C does not need a reader, and the ACE has one built in.
- Cloud inventory services (SimplyPrint et al.) — violates R9. Referenced below only as
  evidence of what is technically possible.
- Multi-colour on the K1C. It stays single-colour.

## Current state

- `workload/apps/spoolman` — Spoolman 0.26.1, SQLite on Longhorn, internal HTTPS ingress.
  Deployed, essentially empty, no printer wired to it.
- Kobra S1 Combo + ACE Pro 2 — stock firmware.
- K1C — rooted, **Creality Helper Script already installed**. Moonraker/Fluidd presence and
  whether `[spoolman]` is configured still to be confirmed on the box.
- Home Assistant — running on cluster, no Spoolman integration.

## Proposed architecture

The insight that makes this one platform instead of four: **both printers can speak Moonraker,
and Moonraker has native Spoolman support.** So Spoolman becomes the hub rather than a
side-car inventory that has to be reconciled by hand.

```
                        ┌──────────────────────────┐
   Android phone ──NFC──│  Spoolman  (SSOT)        │──REST──> Home Assistant
   (read/write tags)    │  cluster, GitOps         │          (dashboard, low-filament
                        └────────────┬─────────────┘           notifications)
                                     │ Moonraker [spoolman]
                     ┌───────────────┴────────────────┐
                     │                                │
          Kobra S1 + ACE Pro 2                     K1C
          Rinkhals firmware                Helper Script (done)
          per-gate auto tracking           manual spool select in Fluidd
          NFC tag read by ACE              (R7: acceptable)
          [blocked, see Blocker 1]         [ready to wire up]
```

Spoolman stays as deployed. The work is all in the layers around it.

## Component status

| Component | Role | Status | Verification |
| --- | --- | --- | --- |
| [Spoolman](https://github.com/Donkie/Spoolman) 0.26.1 | SSOT inventory | Deployed | VERIFIED — active upstream; **no native NFC**, QR labels only |
| [Rinkhals](https://github.com/rinkhals-community/Rinkhals) | Kobra S1 custom firmware | Candidate | VERIFIED — Kobra S1 (+combo) supported, ships Moonraker + Fluidd/Mainsail, keeps stock Anycubic features |
| Rinkhals `mmu_ace.py` | ACE ↔ Spoolman per-gate sync | Candidate, **has a blocker** | VERIFIED — see Blocker 1 |
| [Creality Helper Script](https://guilouz.github.io/Creality-Helper-Script-Wiki/) | K1C root + Moonraker/Fluidd | **Done** | Installed on the K1C |
| Moonraker `[spoolman]` | Consumption reporting | Candidate | VERIFIED — core Moonraker component, tracks **one active spool** |
| [spoolman-homeassistant](https://github.com/Disane87/spoolman-homeassistant) | Single pane of glass (R8) | Candidate | VERIFIED — HACS, 250★, 25+ sensors/spool, bidirectional (`patch_spool`, `use_spool_filament`), threshold events |
| [Ace RFID](https://github.com/DnG-Crafts/ACE-RFID) (Android) | Write ACE-format tags | Candidate | VERIFIED — Play Store app, NTAG213/215, writes SKU/brand/material/RGB/temps/length |
| [SpoolPainter](https://github.com/ni4223/SpoolPainter) (Android) | Tag ↔ Spoolman binding | Candidate | VERIFIED — *reads* Anycubic tags and prefills form, links by UID; **writes OpenSpool only** |
| [SpoolKid](https://github.com/marko-p/SpoolKid) | Would solve R4 outright | **Rejected** | VERIFIED — writes Anycubic/OpenSpool/OpenTag3D *from* Spoolman, but **iOS-only** |
| [OpenSpool](https://github.com/spuder/OpenSpool) | — | **Rejected** | VERIFIED — Bambu-only; its own README lists Creality, Anycubic and Spoolman as "Planned" |
| [SpoolSense](https://spoolsense.org/) | DIY multi-standard reader | Deferred | VERIFIED — reads OpenTag3D/OpenSpool/OpenPrintTag/TigerTag/Bambu; not needed given R7 |
| [Spoolman-NG](https://github.com/sherrmann/Spoolman-NG) | Fork w/ native NFC | **Not recommended** | VERIFIED — real NFC features (`POST /api/v1/nfc/lookup`, Web NFC scanner) but ~5★, single maintainer → bad bus factor for the SSOT |

## Blockers and open questions

### Blocker 1 — RFID-tagged ACE gates cannot hold a Spoolman ID

`VERIFIED` — [Rinkhals issue #141](https://github.com/rinkhals-community/Rinkhals/issues/141),
opened 2026-09-14, **open**.

This is the central architectural conflict and it hits R1 + R6 together:

- `update_gate()` early-returns on any RFID-tagged gate, rejecting Spoolman ID writes.
- The periodic hardware sync overwrites `gate.spool_id` with a **pseudo-ID derived from the
  RFID chip serial/hash** — unrelated to any real Spoolman record.
- Net effect: the moment a spool carries an Anycubic RFID tag (which R6 requires), it can no
  longer be bound to its Spoolman entry (which R1 requires).

No workaround documented. Reporter proposed an `MMU_SET_SPOOL GATE=<n> SPOOLID=<id>` command
and referenced PR #142. **Merge status unverified — check before committing to this path.**

Possible sidestep, untested: the ACE never exposes the tag UID, only the **SKU** field. Writing
`SM<spoolId>` into SKU (the trick [multiACE](https://github.com/Simon-CR/multiACE) uses) might
carry the binding through where UID cannot. Needs testing against Rinkhals' actual read path.

### Blocker 2 — nothing on Android writes Anycubic-format tags from Spoolman

`VERIFIED` — hits R4 directly.

- Ace RFID writes the Anycubic format, but from manual entry — it knows nothing about Spoolman.
- SpoolPainter/SpoolStudio know Spoolman, read Anycubic tags, but only **write** OpenSpool.
- SpoolKid does exactly what is needed, and is iOS-only.

Current best workflow is two apps: write with Ace RFID → tap with SpoolPainter, which decodes
the Anycubic payload and prefills the Spoolman form, so it is roughly one-and-a-bit entries
rather than two. Acceptable, but it is the weakest point against R8.

### Open question 1 — can Web NFC write Anycubic tags? (contradiction)

**Unresolved conflict in sources:**

- SpoolKid and Molodos both describe the Anycubic format as **raw page writes**, which the
  W3C Web NFC API (NDEF-only) should not be able to produce.
- [SimplyPrint's docs](https://help.simplyprint.io/en/article/the-anycubic-material-standard-nfcrfid-for-the-anycubic-ace-js3oty/)
  nonetheless list **"Web NFC (browser)" — Chrome on Android — with full ACE tag read/write.**

This matters a lot. If Web NFC really can write the format, a small self-hosted page next to
Spoolman could do write-tag-from-spool in one tap, closing Blocker 2 and R4 properly without
depending on anyone's app store. If it cannot, a native Android app is the only route and the
realistic move is a feature request to SpoolPainter.

**Resolve this first — it decides the shape of the whole solution.**

### Open question 2 — ACE Pro 2 tag format

`CLAIMED, needs 10-minute test.` multiACE states tags written in Anycubic format are recognised
by "every ACE (V1, V2 Stock)". The dedicated writing tools (Molodos, DnG) predate ACE Pro 2 and
only document "ACE Pro". Write one tag, test on the actual ACE Pro 2, before buying tags in bulk.

### Open question 3 — Rinkhals firmware version gate

`VERIFIED as a constraint.` Rinkhals supports specific Kobra S1 firmware versions only
(currently 2.7.0.9, 2.7.2.7). Check the printer's installed version **before** planning this,
and beware Anycubic auto-updating it out of support.

### Open question 4 — one tag or two per spool

Molodos documents **two** NFC stickers per spool; SimplyPrint documents one. Likely a read
position/orientation issue. Start with one, add a second opposite it if reads are unreliable.

## Decisions taken so far

- **Keep Spoolman as the SSOT.** It is not a tag system and was never going to be — the tag
  layer is a separate concern. No reason to replace it. Not migrating to Spoolman-NG.
- **NTAG215, not NTAG213.** ACE needs ≥144 bytes user area; NTAG213 is exactly 144, leaving
  zero headroom. NTAG215 is 504 bytes, same price, keeps R11 open.
- **No custom reader hardware.** R7 removes the need.
- **Home Assistant is the single pane of glass** (R8), not a second inventory.

## Next actions

**Can be done now — independent of every blocker below it:**

1. Confirm Moonraker is installed on the K1C, add `[spoolman]` pointing at the existing
   instance, and add a spool by hand. This makes the K1C half of R1/R7/R10 real today and
   gives live data to build the rest against. The ACE blockers do not touch this path.
2. Add spoolman-homeassistant via HACS once step 1 produces real data (R8).

**Gated on investigation:**

3. Resolve Open question 1 (Web NFC + Anycubic format). Decides the shape of the tag-writing
   half of the solution.
4. Check the Kobra S1's current firmware version against the Rinkhals support list.
5. Buy a handful of NTAG215 tags; write one with Ace RFID; verify ACE Pro 2 reads it (OQ2).
6. Verify SpoolPainter decodes that tag and prefills a Spoolman entry.
7. Check whether Rinkhals PR #142 is merged, and test the SKU=`SM<id>` sidestep for Blocker 1.
8. Only then: decide on Rinkhals for the Kobra.

## Rejected, with reasons

| Option | Why not |
| --- | --- |
| OpenSpool (as a device) | Bambu-only. Does nothing on either of these printers today. |
| OpenSpool (as a format) | Neither printer reads it. Fine as a secondary payload, not as the plan. |
| SpoolSense / nfc2klipper reader | Hardware solving a problem R7 removes. |
| Spoolman-NG | Bus factor too low for the system of record. |
| SimplyPrint | Cloud SaaS, violates R9. Useful only as proof Web NFC writing may be possible. |
| OpenPrintTag (Prusa) | ISO15693 — needs PN5180; neither printer reads it. |
