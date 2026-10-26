---
name: grill-me
description: Interview the user relentlessly, one question at a time, before designing or changing anything non-trivial (new app, migration, network/storage change, refactor of shared-lib). Use when the request is ambiguous or high blast-radius, or when the user says "grill me", "interview me", "let's plan".
argument-hint: <topic>
disable-model-invocation: true
---

# Grill me: $ARGUMENTS

Goal: reach shared understanding and a decision-complete plan **before** touching files.

1. **Explore first, ask second.** Anything answerable from the repo (grep `wiki/`, values files, `git log`) or a read-only `kubectl get` you look up yourself; never ask the user for facts you can verify.
2. **One question per turn**, each with your **recommended answer** and a one-line why. Use AskUserQuestion with 2-4 options when choices are discrete (recommended first).
3. Walk the decision tree depth-first; resolve a branch's dependencies before moving on. Typical branches for this repo: where does it run (K3s/NAS/HA host?), namespace + ApplicationSet (`workload` vs `tests` branch), storage (NFS vs Longhorn, size, replicas), ingress (internal/secure/external), secrets (Vault path), scale-to-zero fit, VLAN/mDNS needs, DB (CNPG role/db), rollback path, wiki page affected.
4. Challenge assumptions and surface risks (data loss, selfHeal reverting live edits, Vault sealed, downtime) explicitly; don't just agree.
5. Stop when remaining questions don't change what you'd do. Then output a **plan**: files to change, order, verification commands (`scripts/validate-charts.sh`, live checks), rollback. Wait for "go" before editing.
