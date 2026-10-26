---
name: wiki-sync
description: Reconcile wiki/ pages with the current working-tree/staged changes (new or removed apps, changed ports/paths/versions, finished TODOs). Use after finishing a feature or when asked to update the docs.
context: fork
agent: general-purpose
model: sonnet
allowed-tools: Read, Grep, Glob, Edit(wiki/**), Write(wiki/**), Bash(git diff:*), Bash(git status:*), Bash(git log:*)
---

Working tree changes: !`git diff HEAD --stat -- . ':(exclude)wiki' | tail -30`

1. Decide which of those changes make a wiki page wrong, stale or incomplete. Routine version bumps and refactors usually need nothing.
2. Grep `wiki/` for the affected app/port/path, verify facts against the repo, edit minimally. Rules in `.claude/rules/wiki.md` apply (anchors, blank line before tables, no private identifiers, nav entry for new pages).
3. Edit only under `wiki/`/`properdocs.yml`. Do not commit.
4. Finish with `NO_UPDATE` or `UPDATED: <files>` + a one-line reason.
