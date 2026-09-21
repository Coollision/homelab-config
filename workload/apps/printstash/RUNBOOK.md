# PrintStash operational notes — upstream hotfixes

This deployment carries temporary, hand-applied fixes for two known upstream
bugs. This doc is the single place to check **on every PrintStash image
bump** — what's patched, why, whether it's still needed, and what to do when
something here breaks.

## Active patches (`app-patch-configmap.yaml` + `app-source-patch.yaml`)

Mechanism: an init container (`patch-app-source`) on the `printstash`
Deployment, same pinned image as the app, copies `/app/app` into a shared
`emptyDir` and overlays corrected `.py` files from a ConfigMap before the main
container starts. `kustomization.yaml` explains why it's a fixed-name
ConfigMap + manual checksum annotation, not `configMapGenerator` (that
auto-hash approach silently broke — see git history on this file for the
"FailedMount: configmap not found" incident if it's unclear why).

| File patched | Upstream PR | What it fixes |
|---|---|---|
| `app/core/browser_device_auth.py` | [PR #183](https://github.com/xiao-villamor/PrintStash/pull/183) | `require_user_or_browser_import_user` only checked the `Authorization: Bearer` header, never the `printstash_session` cookie the web UI actually authenticates with. Every cookie-authenticated "Upload → URL" import / browser-extension capture 401'd, and the frontend misread that as an expired session, causing an infinite login-loop. Fixed here with a **required** `Request` param (not `Request | None` like upstream — our pinned FastAPI version only auto-injects the bare `Request` type; `Request \| None` crashes at import time with "Invalid args for response field!"). If upstream ever changes this signature again, re-check that detail. |
| `app/modules/ingestion/import_resolvers.py` | [PR #185](https://github.com/xiao-villamor/PrintStash/pull/185) | Prusa changed Printables' GraphQL schema (`title` field removed, `user{name username}` → `user{id handle}`, `license{name code}` → `license{id name}`, and the download-link mutation dropped the `fileId` field). Every Printables import failed with `printables_resolve_failed` / GraphQL 400. |

### On every image digest bump (`values.yaml`)

1. Check both PRs' status: [`#183`](https://github.com/xiao-villamor/PrintStash/pull/183), [`#185`](https://github.com/xiao-villamor/PrintStash/pull/185).
2. If merged **and** the new pinned digest's commit is after the merge commit, delete the fixed file(s) from `app-patch-configmap.yaml`, the corresponding `cp`/`check` lines in `app-source-patch.yaml`, and this table row. If only one has merged, remove only that half — they're independent.
3. If neither has merged yet, just watch the pod come up: the init container's sha256 tripwire (`check()` in `app-source-patch.yaml`) logs a loud `WARNING` if the live file no longer matches the hash this patch expects — meaning the base image changed that file for some *other* reason and this patch may now be stale or actively harmful. Check `kubectl -n apps logs <pod> -c patch-app-source` after any digest bump for that warning before assuming everything's fine.
4. If you add a **third** patched file later, it only needs: a new key in `app-patch-configmap.yaml`, a new `check`/`cp` pair in `app-source-patch.yaml`'s init container script, and a bumped `printstash.workload/app-patch-checksum` value (anything different — it's a dumb rollout trigger, not a real hash of anything).

### Rollout gotcha (already hit once, don't repeat it)

A `kubectl rollout restart` **does not** pick up new patch content — the init
container image is unchanged, so Kubernetes sees no pod-template diff and
doesn't roll a new pod; the old pod just keeps running stale patch content
(or crash-looping on it). Content changes only take effect because
`app-patch-checksum` in `app-source-patch.yaml` is bumped by hand — **always
bump it** when editing `app-patch-configmap.yaml`, or the change silently
does nothing until the next unrelated rollout.

