# TODO: Claude agent gateway and remote sessions {#claude-agent-gateway}

[← Back to Home](../Home.md) · Related: [Voice](../02-smarthome/voice.md) · [Home Assistant](../02-smarthome/home-assistant.md) · [Windmill pilot](windmill-pilot.md)

> **Status (2026-11-22): planned, nothing deployed.** Two things on the Claude Team seat, built in two phases:
> a **Remote Control pod** (`claude-remote`, the main use case: drive Claude Code sessions that run in the cluster
> from the phone) and a **small gateway** (a Home Assistant chat agent plus text clean-up for n8n). **v1 is the
> testing phase**, deployed from the `tests` branch; **v2 makes it production**. Decisions below come from a design
> review and a question-by-question walk-through on 2026-11-22.

## Goal and terms {#goal}

Use the already-paid Team seat for personal automation at a low rate: a few calls an hour from the gateway, and
interactive sessions on the phone through Remote Control.

**Terms (reviewed 2026-11-20, [Anthropic legal & compliance](https://code.claude.com/docs/en/legal-and-compliance)):**

- **Gateway:** the policy says Agent SDK developers should use API-key auth and does not allow intermediating
  Claude.ai credentials, and it may be enforced without notice. A FastAPI service in front of the SDK serving HA, n8n
  and other clients is the programmatic use that policy points at. Treat it as a risk taken knowingly, not as
  allowed. If it ever becomes shared or high-rate, switch to an API key (set `ANTHROPIC_API_KEY`, which overrides
  the subscription token; cost per token applies).
- **Remote Control:** the unmodified `claude` binary used as documented, so a smaller question. A headless 24/7 pod
  with a login created on a laptop is less clear than an interactive terminal, so not risk-free either. There is
  **no API-key fallback**: Remote Control rejects API keys.
- **Seat:** the repo owner is the Team Owner (confirmed 2026-11-22), so no third-party approval is needed. The traffic
  and transcripts still sit under the org's commercial terms and retention (what an admin can see is
  *unconfirmed*), and the automation shares the seat's usage limits with your own Claude Code work.

## Phases {#phases}

| | v1: testing | v2: production |
|---|---|---|
| Where | `tests/claude/claude-remote` and `tests/claude/claude-gateway`, deployed from the `tests` branch (see `tests/README.md`) | Promoted to `workload/claude/` with the documented ownerReference handover (`/promote-test-app`) |
| Image | Stock Python image, dependencies installed at start into a per-architecture venv on the PVC | Small image built in CI (same path as the storage operator), multi-arch, no runtime install |
| Start/stop | CronJob scale plus manual `kubectl scale` | Go operator, see [Phase 2](#phase-2-operator) |
| Review gate | Use it for a few weeks first | Start v2 once v1 has proven itself |

**Namespace: a new dedicated `claude` namespace** (decided 2026-11-22, for maximum separation: its own NetworkPolicy
scope, quotas, secrets and RBAC, away from `smarthome`). The `tests` ApplicationSet derives the namespace from the
second path segment and sets `CreateNamespace=true`, so `tests/claude/<app>` creates it, and both AppProjects allow
any destination. HA and n8n call the gateway across namespaces through its Service name; the namespace's egress
policy only restricts what the claude pods themselves reach. Vault paths follow the repo-dir convention, which does not define `tests/`: use
`workload/claude/...` from the start so promotion needs no copy and no delete. A pushed `tests` branch on a public
repo is public too.

**The `tests` branch is stale.** On 2026-11-22 `origin/tests` is 134 commits behind `master` (last commit 2026-11-03)
and its `shared-lib` has no `automountServiceAccountToken` (verified), so the "no service-account token" rule below
would silently not apply. First v1 step: merge `master` into `tests` (a merge, never a force-push) and keep `tests`
tracking master's `lib/` for as long as v1 runs.

## Common rules for both pods {#common}

| Topic | Decision |
|---|---|
| Egress | NetworkPolicy (flannel/kube-router cannot match hostnames, so `ipBlock` only): allow the public internet with `0.0.0.0/0` **except RFC1918, the pod and service CIDRs, the node network and the home WAN IP** (the WAN IP is public, and the NAS forwards 443/5001/6690 on purpose, so hairpin NAT would otherwise reach it; take the value from Vault with the `<secret:...>` placeholder). **DNS is a separate peer**: `namespaceSelector` for `kube-system` plus `podSelector` `k8s-app: kube-dns`, UDP and TCP 53 (an `ipBlock` on the kube-dns Service IP never matches, the destination is already rewritten). The existing `cert-push` policy is the opposite pattern (a /32 allowlist with no DNS rule), so it is only a syntax example. This stops reaching the LAN and cluster services; it does **not** stop sending a token to a public host. A hostname allowlist (egress proxy or Cilium) is a v2 option. |
| Ingress | The gateway gets an ingress policy admitting only the HA and n8n namespaces (otherwise any pod in the cluster can call it and only the bearer keys protect it). The remote pod has no Service. No NetworkPolicy exists under `workload/` today, so HA and n8n reach nothing else being blocked. |
| Cluster access | `automountServiceAccountToken: false`, no kubeconfig, non-root, read-only root filesystem if the CLI tolerates it (untested: `HOME` and `CLAUDE_CONFIG_DIR` on the PVC, `/tmp` emptyDir). Both `HOME` and `CLAUDE_CONFIG_DIR` set; where `~/.claude.json` lands relative to `CLAUDE_CONFIG_DIR` is *unverified*, so check isolation. |
| Strategy | `Recreate` (RWO PVC; one replica; the in-process locks assume one process). |
| Memory | The docs suggest about 1 GiB per agent (*unverified*). The tightest node has about 6Gi **total**, much less free. Starting guess, to be measured in v1: gateway request 256Mi, limit about 1.5Gi (Python plus one CLI process); remote pod limit about 2Gi with `--capacity 1` (the CLI plus helm and git children); anti-affinity away from the tightest node until measured. |
| Tools in the pod | Each pod is on the stock Python image with runtime installs (pinned, per architecture, on the PVC): the `claude` CLI (SDK wheel or its installer) in both; in the remote pod also `git`, `helm`, `yamllint` and `yq` for the allowlisted checks. Without these the first phone session cannot run any allowlisted command, so list the exact versions in the values. |
| Backups | Longhorn backups of the PVCs will contain the credential file; keep that in mind for the backup target. |
| Transcripts and worktrees | One-shot gateway calls and phone sessions leave transcripts on the PVC; phone sessions (remote pod only, the gateway has no git) also leave git worktrees. Set a transcript cleanup period and prune worktrees. |

## Remote sessions: `claude-remote` {#remote-sessions}

Full Claude Code sessions that run **in the cluster**, driven from the Claude mobile app or claude.ai/code. Researched
2026-11-22 from the [Remote Control docs](https://code.claude.com/docs/en/remote-control); items marked *unconfirmed*
were not in the docs. It is Anthropic's own `claude remote-control` (server mode) in its own Deployment; the gateway
code is not involved.

| Topic | Decision / finding |
|---|---|
| Auth | Needs a full claude.ai `/login` credential (`.credentials.json` under `CLAUDE_CONFIG_DIR`). `setup-token` tokens, API keys, Bedrock/Vertex and a custom `ANTHROPIC_BASE_URL` all fail. Unset those variables in this pod. |
| Credential source | **Vault seeds once, the PVC is authoritative.** An init step copies the Vault credential onto the PVC only if no credentials file exists, and never overwrites it (the CLI rewrites the file on refresh, and re-seeding a stale copy could log the pod out; rotation behaviour *unconfirmed*). Recovery after PVC loss or expiry is a fresh `/login` on the laptop and a new Vault value. |
| Expiry | A `/login` has an end date (`claude auth status --text`, `Sign-in ends:`); Remote Control disconnects after it passes. Total length and silent refresh are *unconfirmed*. The end date is not secret: when you log in, put it as an epoch in the app's values and render a `PrometheusRule` from it (for example `time() > <end_epoch> - 14*86400`). Prometheus cannot read Vault and nothing else in the repo moves a Vault value into a metric, so do not store the date only in Vault. Nothing needs to exec into the pod. |
| Admin | The Owner (you) must enable Remote Control in the Claude Code admin settings (off by default). Not available under Zero Data Retention. Leave Trusted Devices off (probably blocks a headless pod, *unconfirmed*). |
| Command | `claude remote-control --spawn worktree --capacity 1 --settings <file>` from a stable working directory (own clone of the repo on the PVC). Each phone session gets its own worktree. |
| First start | The trust prompt and "Enable Remote Control?" need a terminal (without a TTY it exits with `Workspace not trusted`). One `kubectl exec -it` after the first deploy. **This gates the CronJob design:** if the accepted state does not survive a restart, every scheduled start (and every start after PVC loss) needs a terminal. Test it first; the fallback is an init step that pre-seeds the accepted-trust flag in `.claude.json` (*unverified* that it works for this prompt). |
| Supervision | After about 10 minutes without network the process exits (docs), so rely on the Kubernetes restart. Sessions can be resumed for about 4 hours after a stop (docs), only if config dir and workspace are on the PVC. A longer stop ends the sessions, but the repo clone and uncommitted work survive. A crashed session is revived by sending it a message. |
| Start/stop (v1) | **No Sablier** (no inbound HTTP to wake on, and the process must stay connected). A CronJob scales the Deployment on a schedule through a ServiceAccount that can only scale that one Deployment; ad-hoc start/stop is `kubectl scale`. ArgoCD must ignore `/spec/replicas` for this app. `set-tests.yaml` has no generated region and the generator only scans `workload/` and `system/`, so add a hand-written `ignoreDifferences` entry for this Deployment in `applications/set-tests.yaml` on `master` (that file affects the whole cluster: show the diff first); on promotion, move it to `applications/set-workloads.yaml` **outside** the autogen markers, like the existing `kubernetes-mcp-server` entry. shared-lib renders `replicas: {{ .Values.replicaCount \| default "1" }}`, so `replicaCount: 0` renders 1: git can never hold "stopped", the CronJob and `kubectl scale` own it. |
| Network | Outbound HTTPS only, no Service or ingress. Hosts the CLI needs: `api.anthropic.com`, `claude.ai`, `platform.claude.com` (and `claude.com` for login) on 443. They are all public, so the common egress policy already allows them; the list is recorded for a later hostname allowlist. The streaming channel's host is *unconfirmed*; note it from a test session. Do not set `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC` (disables Remote Control). Set `ENABLE_CLAUDEAI_MCP_SERVERS=false`. |
| Permissions | **Prompts go to the phone.** Default permission mode with a tight allowlist in **managed settings** (`/etc/claude-code/managed-settings.json` from a read-only ConfigMap; path and precedence *unverified*), not in a file the agent can write: user settings under `CLAUDE_CONFIG_DIR` live on the PVC, so also deny edits to `$CLAUDE_CONFIG_DIR/**`. The rules: allow read-only commands (git status/diff/log, grep, `helm template`, the validate script run from a read-only copy), deny edits to `.claude/**` and to git config, `disableBypassPermissionsMode: "disable"`, no cluster credentials. No `denyInsteadOfPrompting` (it would turn phone approvals into silent denials; existence *unverified*). Sandbox mode inside a pod is untested (bubblewrap needs user namespaces): test it in v1 before relying on it. |
| Data | Files and commands stay in the pod; the session transcript is stored by Anthropic while connected. |

### Pushing changes {#remote-push}

Decision: the pod gets a fine-grained token on the **public repo**, so pushes publish immediately. That is a chosen
trade-off, and these guards are required:

- **Separate push identity.** A fine-grained token acts as the user who created it and cannot be limited to branches,
  so a token created by the repo owner would inherit the owner's ruleset bypass and could push to `master`. Pushes come
  from a **separate identity**: a machine-user collaborator with Write access, or a GitHub App with `contents: write`
  and no `workflows`. Do not use a deploy key (it is not held back by the workflows restriction, *unverified*).
- **Ruleset:** limits that identity to `claude/**`, and it must not be able to create the `renovate/master-*`
  branches that `renovate-window.yml` auto-merges. The bypass list is the admin role only. Which bypass actors a
  user-owned repo's rulesets support is *unverified*: check before relying on it.
- **Workflows:** `helm-validate.yml` runs on every pushed branch and executes `scripts/gen-scale-to-zero-ignores.sh`
  and `scripts/mosquitto-clients-lint.sh` from the pushed commit, so the bot can run code in Actions without touching a
  workflow file. Impact is low (read-only token, no secrets used there); keep it that way.
- **Blocking identifier scan before every push: `scripts/identifier-scan.sh`** (written and committed 2026-11-22 as a laptop `pre-push` hook, 0176ac1a; before that the rule was held only by `CLAUDE.md` and by you reading diffs, since
  `.githooks/pre-commit` is just the wiki-sync hook and `.pre-commit-config.yaml` has no identifier check). It contains
  no patterns: it reads them from Vault (`kv/shared/identifier-patterns`, cached outside the repo), then a gitignored
  extras file, and `IDENTIFIER_PATTERNS_FILE` overrides everything; with no patterns it fails closed. It runs as a
  `pre-push` hook, masks matches in its output, and `SKIP_IDENTIFIER_SCAN=1`, `CI` and `--no-verify` skip it. **Pod
  wiring:** the pod has no Vault token, so mount the pattern list as a read-only Secret and set
  `IDENTIFIER_PATTERNS_FILE` to it; mount the script and the hook as a read-only ConfigMap with `core.hooksPath`
  pointing there (outside the agent-writable clone); never set `CI` in the pod. Tracked in the open questions.
- **It is a guard rail, not a wall:** the agent has Bash and can read the token, and the scan can be skipped with an
  environment variable or `--no-verify`, so a determined or steered session can bypass it. The phone approval of each
  push stays. Bash patterns like `git push` are not a control either (trivially bypassed). The real controls are the
  separate identity and the ruleset; the scan catches mistakes, not attacks.
- Never run the repo's validation or commit tooling from the writable clone: `scripts/validate-charts.sh` and any
  hook config can be edited by the agent, so run copies from a read-only path.

## Gateway: `claude-gateway` (v1, testing) {#gateway}

A deliberately small FastAPI service in front of the Claude Agent SDK (the `claude` CLI underneath). Following the
whisper bridge idea (script in a ConfigMap), but on the stock Python image the **dependencies are not already
present**: v1 installs them at start into a per-architecture venv on the PVC from a hash-pinned requirements file
(Renovate needs a regex manager for it; `DISABLE_AUTOUPDATER=1`). Expect a slow first start and a 157 MB+ download
after PVC loss; only `claude-agent-sdk` 0.2.186 on macOS arm64 was tried, not Linux (glibc only, so no Alpine). v2
replaces this with a built image.

The off-the-shelf `RichardAtCT/claude-code-openai-wrapper` was rejected (README read 2026-11-20): images become text
placeholders, no files or MCP, no published image, in-memory sessions for one hour, no per-path restrictions.

| Topic | v1 decision |
|---|---|
| Availability | **Always on, ClusterIP only, no scaleToZero, no ingress.** Sablier wakes only through the Traefik plugin, so a ClusterIP caller would hit a sleeping pod and never wake it, and voice cannot take a cold start. One pod serves all profiles. |
| Auth to Claude | `claude setup-token` on a laptop (inference-only, one year per the docs, works on Team; no claude.ai connectors), in Vault, passed as `CLAUDE_CODE_OAUTH_TOKEN`. Put the creation date as an epoch in the values and alert (`PrometheusRule`) 30 days **before** it reaches one year; when it expires every profile fails at once. A different credential from the remote pod's login, never shared. |
| Endpoints | `POST /p/<profile>/v1/chat/completions` and `GET /p/<profile>/v1/models` only. No Anthropic-style endpoint, no `/files`, no session API (see [Parked](#parked)). |
| Gate | URL path selects a **profile**; unknown profile = 404. Each profile has its own bearer key from Vault (held as hashes); a profile with no key refuses all requests. A request may only narrow a profile (lower `max_turns`, fewer tools, model from the profile's allowlist), with the explicit exception of HA's `tools` in passthrough mode. |
| Isolation | No profile in v1 has `Read` or `Bash`, so no profile can read another's keys from the process environment. Pass the CLI a minimal explicit environment anyway. If a profile with file tools is ever added, deny `Read` on `/proc/**` and anything outside its working directory. One config dir per profile. |
| Sessions | One-shot per request. HA resends its history each turn, so there is no CLI session to resume. (Named sessions are parked.) |
| Streaming | HA's integrations send `stream: true` with `stream_options.include_usage`. v1 sends the whole answer in one SSE delta but as a complete stream: `tool_calls` deltas with an `index`, a `finish_reason` chunk, a usage chunk, `[DONE]`, and keepalive comments during the multi-second run. |
| Concurrency | Concurrency 1 and a per-profile timeout in v1; raise after measuring memory. |
| Quota | No API is known to report the seat's remaining limit (*unconfirmed*; the SDK may surface rate-limit events). Count `ResultMessage.usage` per profile and cap per day as a rough guard; the seat's windows are 5-hour and weekly, so a daily count is only approximate. |

```yaml
profiles:
  ha-chat:             # Home Assistant conversation agent
    tools: []
    tool_passthrough: true   # profile *allows* it; it only happens when the request carries `tools` (chat-only otherwise)
    model: <model>
    key_env: KEY_HA_CHAT
  transcript-cleanup:  # whisper text in, notes / punctuation / action items out
    tools: []
    model: <model>
    key_env: KEY_CLEANUP
```

### Home Assistant voice and tool passthrough (experiment) {#home-assistant}

HA's conversation agent sends text and, for device control, its own function definitions as `tools`, and HA executes
the calls. **Integration (checked 2026-11-20):** core `openai_conversation` uses the Responses API and only works with
the official endpoint, so it is out. The HACS integrations
[`extended_openai_conversation`](https://github.com/jekalmin/extended_openai_conversation) and
[`hass_local_openai_llm`](https://github.com/skye-harris/hass_local_openai_llm) take a custom base URL and use chat
completions with tools, so only `/v1/chat/completions` and a complete stream are needed.

- **Chat only** (no `tools` in the request): plain answers; HA's local intents still handle "turn on the light".
- **Tool-call passthrough (shipped in v1 as an experiment, default off per request).** The earlier "capture the first
  tool call and abort" idea does not work: it leaves an unanswered `tool_use`, drops multi-device commands, and HA's
  history cannot continue a CLI session. Corrected design: **stateless per HA turn**; render HA's history and tools
  into a fresh run, collect **all** `tool_use` blocks of the turn, return them as OpenAI `tool_calls`, and let HA run
  them and send the results next turn. Local measurement of the plain prompt-emulation variant on Haiku: 3 turns,
  14.4 s, about 26k cache-creation tokens, and no calls returned, so v1 is a measurement exercise first.
- **Safety:** entity names and calendar text reach the prompt, so injected text could produce actions. Expose to this
  agent only an allowlist of entities that excludes locks, garage, alarm and other security domains.
- **Voice latency:** every call spawns an agent loop, so expect seconds; keep `max_turns` low and measure against the
  [voice benchmark](../02-smarthome/voice-benchmark.md).

## Speech-to-text {#stt}

Not possible through the subscription: Claude does not accept audio input and the apps' mic button is client-side
dictation. STT stays on `whisper-cpp` (see [Voice](../02-smarthome/voice.md)); Claude only post-processes the text
(the `transcript-cleanup` profile).

## Phase 2: operator and single credential source {#phase-2-operator}

Planned for **v2**, expected to follow quickly if v1 works. It replaces the v1 CronJob and hand-written Deployments.
Same pattern as the Go storage operator. Review this design before building (contradictions the 2026-11-22 review
found are already fixed below).

| Topic | Design |
|---|---|
| Component | Small Go operator, own namespace, RBAC limited to Deployments, PVCs, Secrets, NetworkPolicies and CRs in its target namespace. **No `pods/exec`.** |
| CRD | `ClaudeAgent`: `spec.kind` (`remote` or `gateway`), credential reference, workspace and PVC size, settings file, limits, `spec.schedule`, `spec.override` (`state` plus `until`). Actual state is written to `status`, never into `spec`. |
| Start and stop | Schedule windows are handled in the operator. A manual start or stop is an `override` with an expiry, which wins over the schedule until it expires, so a 10:00 stop inside a 07:00 to 23:00 window is not undone. Stopping keeps the PVC. |
| Credentials | Vault is the **bootstrap** source only, read by the operator through a Kubernetes-auth Vault role (to be defined). It seeds the remote pod's PVC once and never overwrites it (same rule as v1). The `setup-token` goes to the gateway pod. The login is held by **one** `remote` agent at a time; the operator rejects a second `remote` CR for it. The login is never shared across pods. |
| Expiry | From the end date (an epoch in the `ClaudeAgent` CR or values, not only in Vault) and a Prometheus rule, same as v1. |
| GitOps | Git holds the operator and the `ClaudeAgent` CRs. Pods and PVCs are created by the operator; do **not** copy ArgoCD's tracking label onto children (ArgoCD would prune them as orphans). The CRD must sync before any CR (put it in an earlier app or sync wave). Because `spec` carries the override, an `ignoreDifferences` for it is needed; accept that git is then not the truth for whether the agent is running. |
| Failure | Operator down: running pods keep running; only start/stop and credential changes pause. Pod crash: Kubernetes restarts it. |
| Out of scope | Session sharing, an operator-side Claude proxy, auto-renewal of the login. |

Still to specify before building v2 (found by the second review):

- How the seed reaches the PVC: an init container mounting an operator-created Secret, which means the login then also
  sits in a Secret object. Say so, or have the operator write the file some other way.
- How "reject a second `remote` CR" is enforced (validating webhook or only a status condition).
- Where the operator lives (repo path, namespace) and how it is released (same path as the storage operator).
- A direct Vault Kubernetes-auth role is a second secret path next to the lovely-plugin placeholders, against the
  "fewer components" preference: justify it, or reuse lovely-rendered Secrets and keep Vault out of the operator.
- Schedule timezone.
- CRD before CR: a separate app or `SkipDryRunOnMissingResource`, not only a sync wave.

## Parked (not in v1 or v2 scope) {#parked}

Cut on 2026-11-22 because Remote Control already covers interactive repo and HA work with a human approving each
step, and these were the riskiest parts of the first design. Revisit only if Remote Control proves unusable or a
client needs them.

- **Slack bot that edits the repo (`homelab-edit`)**, and committing/pushing from the gateway. If revived: sessions
  keyed to Slack threads, ack within 3 seconds and dedupe on `event_id`, one allowed user and channel, the agent never
  commits (gateway code does), `setting_sources=[]` and no edits to `.claude/**` or `.git/**`, validation and hook
  tooling run from read-only copies, push credential kept **out** of the gateway pod, the pre-push scan above, a
  `claude/**`-scoped token without `workflows`, no cluster credentials, and per-profile blast-radius caps.
- **`ha-edit` through ha-mcp.** Blocked by ha-mcp's OAuth-only mode today; the known answer in this repo is the
  `kubernetes-mcp-server` pattern (OAuth disabled server-side behind static auth at Traefik). Never mount the HA config
  volume (it holds `secrets.yaml` and auth tokens that would reach the model and its transcripts).
- **Named sessions** (`session: <name>`; use a per-profile namespace uuid for `uuid5`, not string concatenation, and
  reject raw uuids from other profiles), **files / PDFs / images** (content blocks, `/files`), the **Anthropic-style
  `/v1/messages`** endpoint and `DELETE /sessions`.
- **Self-hosted environments for cloud sessions** (Team/Enterprise): would run claude.ai/code sessions on our own
  cluster without a pod holding a login. Only the summary was read; auth and network model unknown.

## Open questions {#open-questions}

- [x] **Seat owner:** the repo owner is the Team Owner (2026-11-22).
- [x] **Namespace:** dedicated `claude` (`tests/claude/...`, later `workload/claude/...`). HA and n8n reach the gateway
      across namespaces: no NetworkPolicy exists under `workload/` today (only `cert-push` and Longhorn in `system/`),
      and the gateway's own ingress policy admits them explicitly.
- [ ] **Remote Control checks:** flip the admin toggle, keep Trusted Devices off; does a `.credentials.json` made on the
      laptop work headless, how long does a login last, does the first-start prompt state persist, which hosts does the
      streaming channel use?
- [x] **Replicas ignore for the CronJob:** hand-written entry for `claude-remote` in `applications/set-tests.yaml`
      (no autogen region there), moved to `applications/set-workloads.yaml` outside the markers on promotion (see
      `claude-remote` Start/stop). Remaining: get your OK on that cluster-wide file change.
- [ ] **Trust prompt vs CronJob:** does the accepted state survive a restart? This decides whether the daily CronJob
      is viable (see `claude-remote` First start).
- [ ] **Stale `tests` branch:** merge `master` into `tests` first (see Phases).
- [ ] **Memory:** measure the real footprint of both pods in v1, then set limits and node placement.
- [ ] **Sandbox and read-only root filesystem** in a pod: test that they work with the CLI.
- [ ] **Linux:** verify the pinned wheel set installs on both mini architectures (only macOS arm64 was tried).
- [ ] **Passthrough** with a real HACS integration: measure latency and correctness; define the entity allowlist.
- [ ] **Identifier scan:** done on the laptop (script committed, `pre-push` hook live, patterns seeded in Vault, 2026-11-22). Left: wire it into the
      pod (read-only Secret + `IDENTIFIER_PATTERNS_FILE`, read-only hook).
- [ ] **Push identity and ruleset:** a separate machine user or GitHub App (not the owner's token), limited to
      `claude/**`, no `renovate/master-*` creation, no `workflows`, bypass for the admin role only (check what a
      user-owned repo supports).
- [ ] **Remote pod tools:** pin `git`, `helm`, `yamllint`, `yq` and the `claude` CLI per architecture.
- [ ] **Egress policy test:** DNS works through the `kube-system` peer, the WAN IP and LAN are blocked, HA reaches the
      gateway.
- [ ] **Per-profile daily quota numbers** for the gateway.

## Build checklist {#build-checklist}

v1 (testing, `tests` branch):

- [ ] **First:** merge `master` into the `tests` branch (merge, no force-push) so `shared-lib` is current, and get the
      `applications/set-tests.yaml` replicas-ignore diff approved.
- [ ] `claude-remote`: values, PVC, Vault-seeded credential (seed once), settings file with the allowlist, egress
      policy, one-time `kubectl exec -it` bootstrap, CronJob scaler, expiry alert, push token and scan.
- [ ] `claude-gateway`: script, profile config, values (small neighbour such as `whisper`, remembering its dependencies
      are not preinstalled), Vault keys, egress policy; test chat, then passthrough with entity allowlist.
- [ ] Local render check for both, then deploy and test: a phone session, a restart and resume, an expired-network
      reconnect, one HA chat turn, one passthrough turn.
- [ ] Wiki page under `02-smarthome/` or `05-operations/` with the real facts; keep this TODO for the leftovers.

v2 (production):

- [ ] Review the operator design; build the small CI image; promote both apps from `tests` to `workload/`; remove
      the CronJob.
