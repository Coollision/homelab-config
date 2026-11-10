# TODO: n8n workflow that syncs Matter devices into UniFi

[← Back to Home](../Home.md) · Related: [UniFi OS 6 re-check](unifi-os6-recheck.md) · [Matter & Thread](../02-smarthome/matter-thread.md) · [Known issues: the UniFi Matter API](../05-operations/known-issues.md#unifi-matter-api)

> **Status (2026-11):** the workflow exists in n8n as a draft ("Matter: sync names and enrolment to UniFi", inactive, dry run on) and
> its first half has run for real. It cannot finish until the custom `WebSocket Request` node in `workload/smarthome/n8n` is deployed
> (written, tested locally and validated, not pushed yet), after which the two bridge steps are swapped for that node. By hand so far: three devices enrolled in UniFi and the two that have a custom Home Assistant name renamed.

## Goal

UniFi's Matter monitoring follows Home Assistant without anyone thinking about it: names equal the Home Assistant device names
(Home Assistant is the source of truth), and a Thread device that is in matter-server but not in UniFi gets enrolled.

## How it works

Daily at 03:30 plus a manual trigger (nothing runs more often). `Config` holds the console URL, `dryRun`, `maxEnrolPerRun` (2)
and the matter-server WebSocket URL.

1. List the UniFi Matter nodes and read each node's `unique_id` with a `PUT` that sends the node's **current** name back
   (never `null`, which clears a custom name).
2. Home Assistant device names through the built-in Home Assistant node (template call; the result comes back already parsed
   under `renderedTemplate`).
3. matter-server nodes through the `WebSocket Request` node (`get_nodes`), joined to UniFi on the Matter **UniqueID**; devices Home Assistant identifies by serial
   are paired by model when that is unambiguous.
4. A Code node plans: **rename** where the UniFi name differs, **skip** devices still unnamed in Home Assistant, **enrol** Thread
   devices missing in UniFi (sleepy devices first, router plugs last, the Wi-Fi vacuum never), and no enrolment at all while any UniFi
   node is still commissioning.
5. Dry run only reports. Otherwise: renames are applied, enrolments are limited per run, each opens a pairing window through the
   node (`open_commissioning_window`), posts the code to UniFi, waits 45 s, names the new node and checks the result.
6. One Slack summary in `#homeinfra`; silent when there is nothing to do.

A run that is interrupted or fails halfway is repaired by the next one: it re-reads both sides every time and keeps no state.

## Credentials

`Unifi api Key` (Header Auth, `X-API-KEY`) holds a **local console API key**, not a cloud Site Manager key (the console rejects the
cloud key and the cloud API rejects the local one). The workflow calls the console through its `unifi.` hostname, which carries a
valid certificate, so TLS stays verified and no "ignore certificate" option is needed. Reused: `Home Assistant account` and the
Slack credential (posting works; it cannot list channels).

## Checked

First half run against the live console and Home Assistant. Planning logic tested offline against live data: no renames needed,
one device skipped as unnamed, 8 enrolments in the right order; the in-flight and unreadable-node cases behave. The `WebSocket Request`
node was tested locally against the real matter-server (real `get_nodes`, auto-filled id, two items, timeout, refused URL, size cap,
error reply); opening a real window is the same call the earlier manual pilot used.

## Left to do

- Deploy the node (push; n8n restarts once), swap the workflow's bridge steps for it, save no execution data for successful runs (the
  pairing code is in the reply), run the workflow once in dry run, read the Slack summary, then set `dryRun` to false. At 2 per run the 8
  remaining devices take four daily runs, or use the manual trigger.
- Name the unnamed `GRILLPLATS Plug` in Home Assistant (it has no area or custom name); the rename step skips it until then.
- Group the canvas stages (20 top-level nodes), and expect two Slack messages when a rename and an enrol branch both run.
