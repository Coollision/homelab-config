# Karen's print journey — user stories, current state, technical needs

End goal, restated plainly: **Karen picks a model, picks a printer, picks a
color (or colors), hits print — and it either prints or waits for Youri's
go-ahead, safely, with zero slicer knowledge required.**

This doc is the living source of truth for that specific goal. It
supersedes the framing in `AUTO-SLICING-PLAN.md` (which is more
implementation-detail-oriented — keep both, this one is the "are we done
yet" check, that one is the "how does it work" reference) and assumes
`RUNBOOK.md`'s architecture is already built (auto-slice on upload, the two
upstream hotfixes, MakerWorld import).

Every "current state" line below was **tested live tonight**, not assumed —
printers are physically off, so testing went all the way to "attempt
dispatch" safely and observed the real failure mode.

---

## User stories

### US1 — Pick or add a model
*As Karen, I upload a model (or someone already added one, e.g. via
MakerWorld), and I don't need to know anything about slicing.*

**Status: ✅ Done.** Upload → `ingest_mesh()` hook fires automatically →
one-shot slicing Job runs headless OrcaSlicer → gcode revisions attach to
the model on their own. Verified tonight end-to-end: uploaded a test STL,
got back two real gcode revisions (Kobra S1 + Creality K1C) with correct
`filament_type`/`gcode_flavor` in the actual output, zero manual slicing
steps.

**Known current limits:**
- Hardcoded to exactly 2 targets (one per printer, one specific process +
  filament preset each) — not yet material/color-aware. See US3.
- No thumbnail on the gcode revision itself (cosmetic — the model still has
  its STL-derived thumbnail).
- MakerWorld import has a live bug as of tonight: `ERROR: could not resolve
  a direct download link` — **not yet investigated**, happened after
  everything else was verified working. Printables/Thingiverse imports are
  unaffected (different code path).

### US2 — Pick a printer
*As Karen, I choose which printer prints my model.*

**Status: ✅ Mostly done, ⚠️ one real gap.** `POST /api/v1/fleet/queue` with
`printer_id` — a real, working, documented PrintStash API. Tested tonight:
successfully queued a job against a real gcode revision for a real (if
currently powered-off) printer.

**Gap:** Karen picking a printer only makes sense if a slice already exists
*for that printer*. Today's auto-slice hardcodes both printers every time,
so this is fine right now — but it's brittle (see US3, "material-aware
fan-out" in `AUTO-SLICING-PLAN.md` Phase 3, not yet built).

### US3 — Pick color(s)
*As Karen, I choose the filament color for a single-material print, or
colors/tool assignments for a multi-material print.*

**Status: ❌ Not built for single color-selection; ❌ multi-color is
architecturally a different problem, not just "not built yet".**

Tested/confirmed tonight, from PrintStash's actual `QueueJobCreate` schema:
a print job takes exactly one `spool_id` (a single spool reference) at
queue time. There is **no per-tool/multi-slot list** — color/material
selection is fundamentally single-value at the PrintStash API layer.

What this means concretely:
- **Single-color**: buildable. The queue-time `spool_id` field already
  exists; what's missing is Karen ever seeing a color picker tied to it —
  today nothing in the auto-slice pipeline surfaces "which of the 4 loaded
  ACE spools do you want" as a choice. This needs either (a) slicing once
  per *loaded material* as `AUTO-SLICING-PLAN.md` §2 describes (color
  becomes metadata on an already-existing revision, Karen picks the
  printer+material combo from the revision dropdown — no new UI needed,
  reuses PrintStash's own send-to-printer picker), or (b) a real "pick spool
  at queue time" step using the existing `spool_id` field (needs a frontend
  affordance PrintStash doesn't have today — recall the frontend is a
  prebuilt static export, not patchable the way the backend is).
  **(a) is the buildable option; (b) would need an upstream feature
  request or accepting a frontend-patching cost we've avoided everywhere
  else.**
- **Multi-color** (single print, multiple tools/colors): confirmed
  architecturally out of reach for the auto-slice path. Real multi-material
  needs per-object tool assignment done at slice time in an interactive
  GUI (deciding which mesh feature gets which filament) — there's no
  "auto-assign colors" mode a headless Job could reasonably do. This one
  stays a manual-GUI-slicing job, permanently, not a "build it later" gap.
  (Consistent with what was already flagged in `AUTO-SLICING-PLAN.md`.)

### US4 — Hit print
*As Karen, I hit one button and trust the rest.*

**Status: ✅ The mechanics work.** `POST /api/v1/fleet/queue` immediately
returns a job in `state: "queued"`, and PrintStash's own dispatch worker
picks it up on its own — confirmed tonight, both as an admin account and as
Karen's own (non-superuser) account. No manual "start" step needed once
queued.

### US5 — Approve before it actually prints
*As Youri, nothing prints without me reviewing it first, at least while
Karen is new to this.*

**Status: ❌ Does not currently work the way we assumed. This is the most
important finding of tonight's testing.**

We set `operator_release_required: true` on both printers weeks ago,
believing it meant "hold Karen's job for my approval before it starts."
Tonight's live test disproves that: queuing a job (as both admin and Karen)
went straight to `operator_gate_state: "not_required"` and PrintStash's
worker attempted dispatch within seconds — no hold, no approval step,
nothing waited on us.

Reading the actual dispatch code confirms why: `operator_gate_state` is
only ever set to `PENDING` **when a print job reaches `COMPLETED`** (see
`printer_hub.py`, the state-transition handler) — and only then, if the
printer's `operator_release_required` is set. In other words, this flag is
a **post-print quality gate between consecutive jobs** ("a print just
finished on this printer — operator must release or hold before the *next*
queued job on it is allowed to dispatch"), not a pre-print approval gate on
any specific job. `hold` even puts the *printer itself* into `drain_mode`,
stopping all further auto-dispatch until cleared — this is queue-level
crowd control, not per-job sign-off.

**There is currently no PrintStash mechanism that holds a specific newly
queued job for approval before its first dispatch attempt.** This needs to
be built — likely the same pattern as everything else in this repo: a
source-patch hook (probably on `create_queue_job` in `fleet.py`, or on
`enqueue_job` in the `fleet` module) that, when the queuing user is
non-superuser, creates the job in an explicit held state instead of
`queued`, requiring an admin action to actually release it into the normal
dispatch path. **Not yet designed in detail — this is the top priority gap
before Karen can safely use this unsupervised.**

### US6 — See what's happening
*As Karen or Youri, we can see queued/printing/done/failed status.*

**Status: ✅ Works as-is.** `GET /api/v1/fleet/queue` returns full job
state, including a real, honest `dispatch_outcome_unknown` error when a
printer is unreachable (tested tonight, printers off) — not a silent
failure, not a false "success." PrintStash's own PWA (already installed)
surfaces this without any further work.

---

## Summary table

| Story | Status | Blocking gap |
|---|---|---|
| US1 — pick/add model | ✅ Done | MakerWorld regression (unrelated, needs separate fix) |
| US2 — pick printer | ✅ Done | None currently (fragile until US3 material-aware slicing lands) |
| US3 — pick color(s) | ❌ Not built (single-color); architecturally excluded (multi-color) | Needs `AUTO-SLICING-PLAN.md` Phase 3 material-aware slicing for single-color; multi-color stays manual-GUI forever |
| US4 — hit print | ✅ Done | None |
| US5 — approve before printing | ❌ Does not work as assumed | **Top priority** — needs a new hold-for-approval hook, not yet designed |
| US6 — see status | ✅ Done | None |

---

## What "done" actually requires from here

In priority order (US5 is the safety-critical one — nothing else matters if
an unsupervised print can start):

1. **Build a real pre-dispatch approval gate.** New work, not yet designed.
   Rough shape: patch `fleet.py`'s job-creation path so a non-superuser's
   queued job starts in a genuinely-held state (not just relying on the
   existing `operator_gate_state`, which — confirmed above — only fires
   post-completion); an explicit admin action moves it to normal `queued`.
   Needs its own small design pass before touching code — how "held" should
   render in PrintStash's own UI/PWA is worth checking first (does the
   existing `operator-decision` endpoint's `release`/`hold` UI affordance
   already exist in the frontend for a *different* purpose we could
   piggyback on, or does this need a genuinely new state PrintStash's
   frontend has never rendered before?).
2. **Fix the MakerWorld regression** (`could not resolve a direct download
   link`) — separate, unrelated to slicing. Needs fresh investigation
   (cookie expiry vs. Cloudflare escalation vs. something else, unknown
   until checked).
3. **Material-aware auto-slicing** (`AUTO-SLICING-PLAN.md` Phase 3) — turns
   US3's single-color story into a real "pick from the revision dropdown"
   flow using already-loaded ACE/Spoolman material state. Not yet started.
4. **Retire Selkies** — still not done (only planned). Lower priority than
   the above since it's a cost/security cleanup, not a Karen-blocking gap.
5. Multi-color: no action item — documented as permanently manual/GUI-only,
   not a backlog item.

## Open decisions still needed from you

- Confirm the approval-gate design direction in (1) above before it's built
  — this is genuinely new design, not just "wire up an existing flag" like
  we assumed.
- MakerWorld fix priority — now or later.
- Color fan-out choice from `AUTO-SLICING-PLAN.md` (1 slice/material vs.
  1/color) — still unresolved, now more clearly motivated by US3 above.
