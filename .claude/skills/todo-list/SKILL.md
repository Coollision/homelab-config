---
name: todo-list
description: Pool every open TODO/idea from wiki/06-todo, .tmp, tmp, auto-memory, .claude scratch files, open PRs and code comments, check what is already done, and rank by quick gain vs importance (flagging 1h evening candidates). Use when asked "what's on the todo list", "what is left", "give me the todo list again" or "something for this evening".
allowed-tools: Read, Grep, Glob, Bash(git status:*), Bash(git log:*), Bash(git grep:*), Bash(git branch:*), Bash(git worktree list:*), Bash(gh pr list:*), Bash(kubectl get:*), Bash(ls:*), Bash(wc:*)
---

Read-only. Never tick, delete, commit or change anything unless the user asks afterwards; only report.

## 1. Pool every source (the wiki alone misses things)

| Source | How |
|---|---|
| `wiki/06-todo/*.md` | open boxes: `grep -c -E '^\s*- \[ \]'` per file, then read the boxes. Many "Background:" boxes are notes, not tasks. |
| Other wiki pages | grep `wiki/` (not 06-todo) for `- [ ]`, "follow-up", "not yet", "still open". Preflight checklists in runbooks are not TODOs. |
| `.tmp/`, `tmp/` | gitignored scratch: `todo/`, plans, handoff notes. Read headers and status lines; flag stale ones (feature already live) instead of listing them as open. |
| Auto-memory (`MEMORY.md` index + files, path in the session context) | grep descriptions/bodies for TODO, open, pending, remind, parked, "plan only", "waits". Memory-only TODOs exist (e.g. hooks, decisions before a date). Also read `feedback_*` so you do not re-list things the user dropped or rejected. |
| `.claude/` extras | `handoff-*.md` scratch files, `worktrees/`, local dumps: cleanup candidates, usually stale. |
| Repo | `git grep -n -E "TODO|FIXME"` outside `wiki/`; open PRs (`gh pr list`) and leftover branches. |
| Today's date | convert every relative date ("in a week", "around 11-22") to an absolute date and sort date-gated items by it. |

Delegate pure bulk grep/reads to `haiku-worker` when the output would be large; keep the judgement here. Say which sources you only skimmed.

## 2. Verify before listing as open

For each candidate that could already be done, spend one cheap read-only check: `git log --since=<note date>`, `kubectl get`, a live API read, the memory/wiki page's own ticks. Drop what is done; if a note is stale, say so and propose the fix (do not edit unasked). If two sources describe the same item, list it once with both sources.

## 3. Rank and present

Tiers, in this order, one line per item with its source in brackets:

1. **Evening candidates (about 1h or less)**: ordered by quick gain x importance, risk last. Say rough effort and whether it needs an OK (deletes, Vault, UniFi writes, destructive cluster actions always do).
2. **Date-gated**: absolute date, earliest first.
3. **Medium**: 1-3h, or needs a decision.
4. **Projects and ideas**: multi-session; group by theme.
5. **Blocked/parked**: say what unblocks it.
6. **Housekeeping**: stale scratch files, leftover worktrees/dumps, finished pages that could be deleted.

Ranking hints: protects the public repo or data > fixes a live oddity > removes recurring noise > nice-to-have. A decision the user must make before a deadline outranks work. Items the user explicitly dropped stay off the list.

## 4. Close the loop (only when asked)

After the user decides: tick/drop items in the wiki page with a dated note (`Done YYYY-MM-DD: ...`), add memory-only TODOs to `wiki/06-todo` (generic wording, no private domains/IPs/hostnames/emails: public repo), fix stale memory notes, and keep `wiki/Home.md` descriptions in sync. Commit only when asked, and stage only your own hunks if the tree has someone else's changes.
