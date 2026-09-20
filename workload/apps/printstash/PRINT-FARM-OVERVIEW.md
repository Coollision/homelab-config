# Print farm — full overview & decision doc

Single entry point covering everything: the printers, the current live
setup, everything built this session, the architecture question on the
table, and what's actually left. Read this first; it links out to
`RUNBOOK.md` (operational detail), `AUTO-SLICING-PLAN.md` (Spoolman/RFID/
Selkies plan), and `KAREN-JOURNEY-PLAN.md` (live-tested user-story status)
for depth on each area.

**Status: decision point.** Everything below is either done-and-verified or
a clearly-scoped open question — nothing here is "in progress and
unclear."

---

## 1. The printers, as they actually are

| | Kobra S1 | Creality K1C |
|---|---|---|
| Firmware | Rinkhals `20260901_01-2` + GoKlipper (NOT Klipper, NOT Happy Hare) | Stock Creality Helper Script, Moonraker v0.11.0 |
| Moonraker | `192.168.40.40:7125`, unauthenticated on the LAN | `192.168.40.202:7125`, unauthenticated on the LAN |
| Material tracking | **ACE (4-slot) with live RFID** — `filament_hub` Klipper object exposes material/color/SKU per slot over plain HTTP, no printer-side changes needed. Slot 0 (orange, `HABVO-103`) is genuine Anycubic RFID; slots 1-3 are hand-entered. | Has a real `spoolman.py` Moonraker component available but **not enabled** |
| Resources | 217MB RAM, ~105MB free — tight. Has its own `[memory_manager]` GC loop. | 214MB RAM |
| Hard constraint | **Never enable `[spoolman]` here** — Rinkhals' own source has a disabled Spoolman emulation with the comment `# Spoolman emulation removed - causes system freeze`, and `/server/spoolman/*` is contested territory on this box. Confirmed via SSH, not inferred. | None known |
| PrintStash printer id | 3, `operator_release_required: true` | 4, `operator_release_required: true` |

Custom NFC tags for the ACE: **confirmed feasible.** Plaintext NTAG213/215/216
format, no encryption, no signing (unlike Bambu's AMS). Cheapest write path:
Android phone + the free **"ACE RFID" app by DnG-Crafts** (Play Store).
Real gotchas: once a tag is present on a gate, Rinkhals locks it against
software override — get it right the first time; color is stored
byte-swapped (ABGR).

---

## 2. What's actually deployed right now

- **PrintStash** (`ghcr.io/xiao-villamor/printstash`, pinned by digest) — the
  model library, upload/import/tagging, print queue/fleet dispatch.
  Connected to Spoolman. Two real upstream bugs patched via a source-patch
  overlay mechanism (see `RUNBOOK.md`): session-cookie auth on import routes
  (PR #183), Printables GraphQL schema drift (PR #185) — both tracked
  against real upstream PRs, remove once merged.
- **OrcaSlicer** (`lscr.io/linuxserver/orcaslicer`, Selkies-streamed GUI) —
  still running, still has known bugs (missing vendor bundles so printer
  presets don't load in the GUI itself, port 8082 unauthenticated and
  reachable cluster-wide, Sablier scale-to-zero silently not applied). **Not
  yet retired** — see §5.
- **Spoolman** (`0.26.1`) — deployed, no auth. Seeded: Anycubic vendor, 4 ABS
  filaments matching the ACE's real colors (one with the real RFID SKU),
  spools bound via `location` to `Kobra S1 / ACE 1..4`. K1C has a clearly
  labeled `PLACEHOLDER` spool since it has no live material tracking yet.
- **MakerWorld importer** (`ghcr.io/coollision/printstash-makerworld-importer`,
  own orphan branch + CI) — a one-shot k8s Job, triggered from PrintStash's
  own "Import from URL" box, that solves MakerWorld's Cloudflare gate with a
  headless Chromium and hands the resolved URL back to PrintStash's normal
  import pipeline. **Works for most models** (verified twice tonight with
  real downloads); **a real, permanent limitation exists**: some models
  trigger an interactive Cloudflare Turnstile checkbox that cannot be
  auto-solved (confirmed via direct browser inspection — a real challenge
  iframe present after 20+ seconds, not a timing issue). Logging was just
  improved to label this distinctly instead of a generic error. Fallback:
  manual download + PrintStash's normal file upload, already proven working.
- **Auto-slicing on upload** — every mesh uploaded to PrintStash
  automatically gets headless-sliced (no display server, the existing
  OrcaSlicer image, verified against real `AppRun` invocations) for both
  printers and pushed back as gcode revisions. Two real slicer bugs found
  and fixed by direct bisection testing tonight:
  - `--load-settings`/`--load-filaments` never resolves `inherits` for
    *values* — an unflattened ABS preset silently sliced as PLA. Fix:
    flatten every preset's full inherits chain at export time.
  - Separately, OrcaSlicer's *compatibility checker* (not the value loader)
    requires machine presets to keep a populated `inherits` field even
    after full flattening, or it fails with exit -17 ("not compatible with
    the process preset") — confirmed by bisecting the exact failing
    combination step by step. Both fixes are live and verified end-to-end
    on both printers with correct `filament_type`/`gcode_flavor` in real
    gcode output.
- **Karen's PrintStash account** — real, non-admin, scoped to `PrinterRole`
  permissions per printer.

Full credential/RBAC/architecture detail for all of the above is in
`RUNBOOK.md` — this section is the summary, that file is the reference.

---

## 3. Karen's actual journey — tested live tonight, not assumed

Full detail and evidence in `KAREN-JOURNEY-PLAN.md`. Summary:

| Step | Status |
|---|---|
| Upload/add a model | ✅ Done — auto-slice fires automatically |
| Pick a printer | ✅ Mechanically works (`POST /fleet/queue`) |
| Pick color(s) | ⚠️ Single-color: buildable, not built. Multi-color: **architecturally impossible** for the automated path (needs per-object tool assignment, inherently a manual GUI job) |
| Hit print | ✅ Works, tested against a real (powered-off) printer, failed honestly with a real error rather than silently |
| **Approve before printing** | ❌ **The one flag we set (`operator_release_required`) doesn't do what we assumed** — confirmed by reading the real dispatch code and live-testing. It only fires *after* a print completes, as a hold-the-queue-between-jobs safety net, not a pre-dispatch approval gate on a specific job |
| See status | ✅ Works as-is |

**Clarified requirement (from you, this conversation):** approval should
happen **after slicing, before printing** — not before slicing. This
actually simplifies things: PrintStash's own `enqueue_job` already requires
a gcode file to exist before a job can be created at all, so "queue a
specific sliced revision for a printer+color" *is* the natural approval
checkpoint — there's no separate "pre-slice" gate to design.

---

## 4. The two-surface-architecture question — reviewed, verdict: rethink the shape, not abandon the idea

You proposed splitting into two surfaces: PrintStash stays the model
library, a new app (built on your `go-react-todo-starter`) handles
printer/color selection + approval + dispatch-triggering. An independent
adversarial review (fresh context, read the actual starter repo and
PrintStash source, live-tested nothing but verified everything against real
code) came back **"go-with-changes"** — worth reading in full, but the
short version:

**Two of the three original justifications don't hold up:**
- The approval gate the whole idea was partly motivated by is **already
  ~95% built** in PrintStash (the `operator_gate_state` column, a dispatch
  blocker honoring it, an approve/reject API, even a UI badge all already
  exist) — making it fire at queue-time instead of only post-completion is
  a small patch through the exact mechanism already used three times
  tonight, or even simpler: **give Karen `PrinterRole.VIEW` only and she
  physically cannot dispatch anything** — a zero-code approval gate.
- Color selection needs **zero slicing changes** — confirmed color doesn't
  affect the actual slice, only gcode header metadata. Picking a color is
  just passing `spool_id` to PrintStash's existing queue API. The idea of
  "parameterizing the slicer for color" was solving a non-problem.

**The one justification that's real:** PrintStash has no "print request"
concept anywhere in its data model — no way to represent "Karen wants model
X on printer Y in color Z, awaiting approval" as a first-class thing. That
gap is genuine and can't be patched onto PrintStash's frontend (which is a
prebuilt static export — confirmed not patchable the way the Python backend
is).

**But the proposed shape needs to change:** a companion app that still
sends Karen to PrintStash to upload means two logins, two URLs, for a
beginner user — objectively worse than today. Either the new app owns
upload entirely (Karen has one URL, ever), or don't build a second app at
all.

**Before building anything:** the review flagged, correctly, that this
homelab already runs **n8n** (with Form Trigger + human-approval nodes) and
has a **disabled `ombi`** (a request-and-approve app) sitting unused in
`arr-stack`. Per your own standing rule (search existing tools by
enumeration before writing bespoke apps), these need to be ruled out on the
record first — a form → approval → `POST /fleet/queue` flow in n8n is
plausibly an afternoon with zero new app, database, or auth system.

### Auth consolidation (new, from this conversation)

You want to start consolidating auth across whatever surfaces exist, rather
than each app growing its own login. Concretely:

- PrintStash **already has full native OIDC support** — issuer URL, groups
  claim, `oidc_admin_groups` mapping — just unconfigured. No patch needed,
  it's a real shipped feature.
- The `go-react-todo-starter` has OAuth, but only two hardcoded providers
  (Google, Facebook) — no generic OIDC client yet. Adding one (goth's
  `openidConnect` provider) is a real but small addition if that app gets
  built.
- **Simplest lightweight option, avoiding new code in either app**: deploy
  a small OIDC/forward-auth provider (**Authelia** fits — YAML-configured,
  no heavy runtime dependency, and since Traefik is already the ingress
  here, it plugs in as a forward-auth middleware on the IngressRoutes
  directly) as a single login gate in front of whichever surfaces exist.
  This gets you one login across everything without needing OIDC client
  code inside every app — PrintStash's own native OIDC support becomes a
  nice-to-have layered on top later, not a blocker.
- **Not yet deployed or deeply scoped** — this is a direction, not a plan.
  Needs its own short design pass (how does Authelia's user store map onto
  "Karen" and "Youri," 2FA or not, session duration) before building.

---

## 5. What's actually left, in priority order

1. **Rule out n8n (and look at why `ombi` was disabled)** for the
   intake/approval flow before writing any new app. This could make most of
   §4 moot.
2. **Land the queue-time approval gate** regardless of the above — cheap,
   uses the proven patch mechanism, real safety net. Pair with giving Karen
   `PrinterRole.VIEW` only as the zero-code baseline.
3. **Decide the auth-consolidation shape** (Authelia forward-auth vs.
   per-app OIDC) once it's clearer whether a second app is being built at
   all.
4. **Material-aware auto-slicing** (`AUTO-SLICING-PLAN.md` Phase 3) — turns
   "pick a color" into a real, non-stale choice grounded in live ACE/Spoolman
   state instead of hand-seeded data that goes stale the moment a spool
   moves. Not started.
5. **Retire Selkies** — planned, not yet executed. Lower priority; it's a
   cost/security cleanup, not a Karen-blocking gap.
6. **Independent housekeeping items, not blocking anything above but real:**
   - The MakerWorld Job's service-account token is a point-in-time copy
     with a 30-day expiry (`app-source-patch.yaml`) — if the PrintStash pod
     outlives a month without restarting, MakerWorld import and
     auto-slicing both silently stop working. Worth a calendar reminder or
     a proper fix.
   - The patch tripwire in the init container is warning-only — a base
     image bump that breaks a patch still boots the pod; only a log line
     flags it.
   - `RUNBOOK.md` doesn't yet have a slicing section despite comments in
     the code pointing readers there — worth fixing so future-you isn't
     chasing a dead link.

## Open decisions needed from you

- n8n/ombi check — go look, or should I?
- Auth consolidation: Authelia forward-auth, or something else?
- Second app: build it (scoped to *own upload*, per the review), or lean
  fully on n8n + PrintStash's existing approval machinery instead?
