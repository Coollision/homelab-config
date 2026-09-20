# PrintStash operational notes — hotfixes & MakerWorld

This deployment carries temporary, hand-applied fixes for known upstream bugs,
plus (in progress) a custom MakerWorld import hook. This doc is the single
place to check **on every PrintStash image bump** — what's patched, why,
whether it's still needed, and what to do when something here breaks.

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

---

## MakerWorld import

**What it does:** pasting a makerworld.com model URL into PrintStash's native
"Import from URL" box works, the same as Printables/Thingiverse — via a hook
in `import_resolvers.py`'s `resolve_page_url()` (`elif kind == "makerworld":`,
patched into `app-patch-configmap.yaml` alongside the two upstream-PR
hotfixes above). Upstream hard-raises `ImportError_("makerworld_extension_required")`
there, deliberately, since [PR #86](https://github.com/xiao-villamor/PrintStash/pull/86)
ripped out server-side MakerWorld credential storage for security reasons.
We're restoring an equivalent, scoped to our own private cluster, credential
in Vault, never typed in by anyone through the UI.

### Why this is a Job, not an in-process resolve

MakerWorld's download-link API (`.../design-service/instance/{id}/f3mf`) is
Cloudflare-challenged hard enough that a static cookie replay fails — a
`cf_clearance` cookie that works fine on the metadata endpoint gets a
403 "Just a moment..." JS-challenge page on this one. Solving it needs a
live, real headless-Chromium challenge-solve (**Patchright**, a
stealth-patched Playwright fork — vanilla Playwright is more easily
fingerprinted), which does not belong inside PrintStash's own always-running
web process.

**Getting Patchright to actually solve the challenge took three attempts —
the mechanism matters, don't regress it.** `context.request.get(api_url)`
(a bare HTTP client sharing the browser's cookies/TLS session) gets the 403
challenge page. So does an in-page `fetch()` executed via
`page.evaluate()` — both were tested directly against the real API and both
still fail, because neither actually loads and executes the challenge
page's own JavaScript the way a real navigation does. Only
`page.goto(api_url)` — a genuine page navigation — runs the challenge JS and
clears it. Chrome's own JSON viewer then wraps the real response body in a
`<pre>` tag; read it back with `page.evaluate("document.body.innerText")`,
not `page.content()` (that returns serialized HTML with the JSON's quotes
HTML-entity-escaped). This is exactly what `fetch_download_link_via_browser()`
in `fetch_makerworld.py` does now — if a future edit "simplifies" it back to
`context.request` or an in-page `fetch()`, it will silently regress to the
403.

So `_queue_makerworld_import()` in the patched
`import_resolvers.py` does the minimum possible: build a one-shot Kubernetes
`Job` manifest and `POST` it to the in-cluster API server, then immediately
raise `ImportError_("makerworld_import_queued")` so the inbox item shows an
accurate "queued, check back shortly" instead of a hard failure. It never
polls the Job — the Job pushes its own result straight to PrintStash's
`POST /api/v1/inbox/browser-upload` (the same endpoint the browser extension
uses) when it finishes, so success shows up as a fresh Pending Import a
little later. Two-step UX (an immediate "queued" item, then later a separate
successful one), traded for zero coupling between this always-running pod
and Patchright/Chromium's resource footprint.

### The pieces

- **`ghcr.io/coollision/printstash-makerworld-importer`** — the Job image.
  Source lives on its own orphan branch, `orphan/printstash-makerworld-importer`
  (same pattern as `orphan/aws-tunnels-operator-standalone` — root-level
  files, not nested in a subdirectory), built by
  `.github/workflows/printstash-makerworld-importer.yaml` on every push to
  that branch. **Must be `python:3.12-slim-bookworm`, not the floating
  `-slim` tag** — it now resolves to Debian trixie, where Patchright's
  `--with-deps` font packages (`ttf-unifont`, `ttf-ubuntu-font-family`) no
  longer exist and the build hard-fails. `fetch_makerworld.py` there does:
  plain httpx + cookies for the metadata call, Patchright for the
  download-link call, plain httpx to download the resolved file and push it
  to PrintStash.
- **`makerworld-job-rbac.yaml`** — a `ServiceAccount` (`printstash`, wired
  into the Deployment via `serviceAccountName` in `app-source-patch.yaml`), a
  `Role` granting only `create` on `jobs.batch` (no get/list/watch/delete —
  fire-and-forget, and each Job sets its own `ttlSecondsAfterFinished` so
  Kubernetes garbage-collects it), and the `RoleBinding` connecting them.
- **`makerworld-job-secret.yaml`** — a plain `kind: Secret` with `<secret:...>`
  Vault markers (same convention as `workload/apps/kubernetes-mcp-server/templates/auth-secret.yaml`,
  confirmed to resolve fine outside Helm-templated files too — the
  vault-injection plugin operates on the whole rendered manifest stream, not
  just Helm output). Holds the 3 MakerWorld cookies + the 3 PrintStash
  importer credentials. **Deliberately not mounted into printstash's own pod
  env** — only referenced by the Job manifest `_queue_makerworld_import()`
  builds at runtime, so these credentials never touch the always-running
  process's environment.
- **`kv/apps/printstash-makerworld`** (fields `token`, `cf_clearance`,
  `cf_bm`) and **`kv/apps/printstash-makerworld-importer`** (fields
  `username`, `api_key`, `url`) in Vault — the actual credential material.

### Credentials — what they are and how to rotate them

MakerWorld session cookies are day-to-day account credentials (the `token`
cookie is a long-lived Bambu/MakerWorld login token), **not** a scoped API
key — treat them accordingly. Only the three needed cookies are stored; never
add `mall_token`/`mall_refresh_token` (a separate Bambu Store/commerce JWT
with the account email embedded) or any other cookie from a full browser
export to that Vault secret — they're not needed and are unnecessary blast
radius if it ever leaks.

- `cf_clearance`/`cf_bm` are short-lived (Cloudflare typically rotates these
  within hours to ~30 days) and tied to the requesting IP/fingerprint. If
  imports start failing with 403s after previously working, **this is the
  first thing to refresh** — re-extract from an active, already-challenged
  browser session on the same network as the cluster, `vault kv put
  kv/apps/printstash-makerworld cf_clearance=... cf_bm=... token=$(vault kv get -field=token kv/apps/printstash-makerworld)`.
  No code change, no redeploy needed — the Job reads the Secret fresh on
  every run.
- `token` is longer-lived but not permanent. If MakerWorld starts responding
  with an explicit login-required error instead of a raw 403/Cloudflare
  page, the login session itself expired — re-log-in in a real browser,
  re-extract `token`.
- The PrintStash importer's own API key (`kv/apps/printstash-makerworld-importer`)
  doesn't expire on its own; rotate it the normal way (`POST /api/v1/auth/api-keys`
  for a new one, `DELETE /api/v1/auth/api-keys/{id}` for the old) if it's
  ever suspected leaked.

### When this breaks (it will)

1. **403 + Cloudflare HTML on the download-link call, even through
   Patchright** → first check it's still using `page.goto()` + `innerText`
   (see above) and hasn't regressed to `context.request`/`page.evaluate(fetch)`.
   If it genuinely is using `page.goto()` and still 403s, Cloudflare escalated
   past what a headless-Chromium challenge-solve handles (e.g. Turnstile
   requiring real interaction) — that would be the one genuinely open-ended
   failure mode, no code fix, only re-evaluating viability.
2. **`kubectl -n apps logs job/makerworld-import-<id>` shows a 403 with a
   plain Cloudflare page** (not from Patchright's step, from the plain-httpx
   metadata call) → `cf_clearance`/`cf_bm` expired, see rotation above.
3. **Job succeeds fetching but fails pushing to PrintStash** → check
   `kv/apps/printstash-makerworld-importer`'s API key is still valid
   (`POST /api/v1/auth/api-keys` list via the admin account) and that
   `PRINTSTASH_URL` still resolves in-cluster.
4. **PrintStash's own resolver call to the k8s API 403s** (visible in
   printstash's own pod logs, not the Job's) → the `printstash`
   ServiceAccount/Role/RoleBinding got out of sync somehow — re-check
   `makerworld-job-rbac.yaml` is still in `kustomization.yaml`'s `resources:`.
5. **New failure shape entirely** (different error, different status code,
   empty/different JSON structure) → MakerWorld changed their API. Re-derive
   from a fresh manual walkthrough (browser DevTools → Network tab against a
   real model page), the same way this was originally built.
6. Regardless of failure mode: this whole section is a bespoke addition on
   top of upstream PrintStash, unlike the two PR-backed patches above, which
   at least track a known upstream fix. There's no upstream to sync against
   here — this is ours to maintain indefinitely, or drop it in favor of the
   manual-download-then-upload workflow (already proven reliable) if the
   maintenance burden stops being worth it.

### Rebuilding the importer image

Any change to `fetch_makerworld.py`/`Dockerfile` on `orphan/printstash-makerworld-importer`
triggers a rebuild automatically. After it lands, update
`_MAKERWORLD_IMPORTER_IMAGE` in the patched `import_resolvers.py` (inside
`app-patch-configmap.yaml`) to the new `sha-<shortsha>` tag, and bump
`printstash.workload/app-patch-checksum` in `app-source-patch.yaml` — same
"nothing rolls out without the checksum bump" rule as the rest of this file.
