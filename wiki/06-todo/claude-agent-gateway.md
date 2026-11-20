# TODO: Claude agent gateway (subscription as a personal API)

[← Back to Home](../Home.md) · Related: [Voice](../02-smarthome/voice.md) · [Home Assistant](../02-smarthome/home-assistant.md) · [Windmill pilot](windmill-pilot.md)

> **Status (2026-11-20): planned, nothing deployed.** A small service in the cluster that exposes the Claude
> subscription through an API-shaped HTTP interface, for personal, low-rate use only: a Home Assistant
> conversation agent, n8n workflows and a Slack bot that edits config. This page is the design; build it after the
> open questions below are answered.

## Goal {#goal}

One in-cluster service that looks like an API but runs the Claude Agent SDK (the `claude` CLI underneath) with the
subscription login. Personal use, a few calls an hour at most. If it ever becomes shared or high-rate, switch to a
real API key instead.

**Terms risk (reviewed 2026-11-20, [Anthropic legal & compliance](https://code.claude.com/docs/en/legal-and-compliance)):**
this is more than a grey zone. The policy says Agent SDK developers should use API-key auth, does not allow
intermediating Claude.ai credentials, and may enforce without notice. What is explicitly fine is a user signing in
to the unmodified `claude` binary with their own subscription, which is what the gateway runs, but the service
around it is the questionable part. A Team seat also falls under the commercial terms, so the Team admin and its
data retention would see this personal home traffic. **Check with the seat owner before building.** Auth precedence
makes the fallback trivial: setting `ANTHROPIC_API_KEY` overrides the subscription token, so moving to a real key
is a one-line change.

## Why our own service, not the off-the-shelf wrapper {#why-custom}

`RichardAtCT/claude-code-openai-wrapper` was the first candidate (README read 2026-11-20). It falls short:

- Images become text placeholders and there is no file or PDF input.
- No MCP support.
- No published image, and sessions live in memory for one hour only.
- No per-path restrictions.

So the plan is a thin FastAPI service of our own. Following the whisper bridge pattern, the script lives in a
ConfigMap and runs on a stock Python image, so there is no image to build. Dependencies (`claude-agent-sdk`,
`fastapi`, `uvicorn`, `python-multipart`, `pyyaml`) are installed once into the PVC and reused on restart. The SDK
bundles the CLI (Linux wheels are glibc only, about 157 MB, so no Alpine). Install from a pinned requirements file
with hashes (a bare `pip install` escapes Renovate and pinning) and set `DISABLE_AUTOUPDATER=1`. Only
`claude-agent-sdk` 0.2.186 on macOS arm64 was tried locally, not Linux.

## Design {#design}

Where: `workload/smarthome/claude-agent`, internal-only secure ingress, `scaleToZero` (low rate).

| Topic | Decision |
|---|---|
| Auth to Claude | `claude setup-token` run once on a laptop gives a token valid for **one year** (works on Pro, Max, Team and Enterprise; model requests only, no claude.ai connectors), stored in Vault and passed as `CLAUDE_CODE_OAUTH_TOKEN`. No refresh handling in the pod (replaces the earlier "run it on the Mac" idea). Add a renewal reminder. |
| Storage | `HOME` and `CLAUDE_CONFIG_DIR` must both be on the PVC: transcripts live in `$CLAUDE_CONFIG_DIR/projects/<encoded-cwd>/` and `~/.claude.json` in `HOME`, so resumes only survive a restart if both are persistent. `/tmp` is an emptyDir. One config dir **per profile**. |
| Network | HA and n8n run in the cluster, so a ClusterIP Service is enough; add an ingress only for a client outside the cluster. |
| Endpoints | `POST /p/<profile>/v1/chat/completions` (OpenAI style), `POST /p/<profile>/v1/messages` (Anthropic style), `POST /p/<profile>/v1/files`, `GET /p/<profile>/v1/models`, `DELETE /p/<profile>/v1/sessions/<name>`. |
| Gate | The URL path selects a **profile**; unknown profile = 404. Profiles are configured in values (a ConfigMap), each with its own bearer key from Vault. A profile with no key configured refuses all requests (fail closed). |
| Narrow only | A request may lower `max_turns` or reduce `allowed_tools`, and pick a model only from the profile's allowlist. It can never widen the profile. |
| Sessions | No `session` field: a fresh one-shot (the new id is returned). `session: "<name or uuid>"`: resume if it exists, create otherwise. A name maps to `uuid5(profile + name)`. The SDK's `session_id` (create with a chosen uuid) cannot be combined with `resume`, so "resume or create" needs an existence check (`get_session_info`) under the per-session lock. One run per session at a time. **Isolation:** the CLI can resume a session id even from another working dir, so without per-profile config dirs and the uuid5 profile prefix, any profile's key could resume another profile's session (including `homelab-edit`). |
| Files | Multipart upload to `/files`, or base64 image/document blocks (Anthropic) and `data:` image URLs (OpenAI) in a message. Preferred path: pass images and PDFs to the model as content blocks in streaming input; saving to the session dir for the Read tool is the fallback, and then the profile must allow `Read`. Sanitise filenames (path traversal), size cap per upload. |
| Streaming | HA's HACS integrations send `stream: true` with `stream_options.include_usage`. v1 sends the whole answer in one SSE delta, but it must be a complete stream: `tool_calls` deltas with an `index`, a `finish_reason` chunk, a usage chunk, `[DONE]`, and keepalive comments during the multi-second run so ingress and HA timeouts do not fire. |
| Concurrency and memory | Semaphore 2 and a per-profile timeout. The docs suggest about 1 GiB RAM per agent, which is tight against the ~6Gi node: set requests and limits deliberately and consider pinning the pod to a node. |
| Quota | No API reports the seat's remaining limit. Count `ResultMessage.usage` per profile in the gateway and cap it per day; automation shares the limit with your own Claude Code use. |
| Scale to zero | Fine for n8n and Slack profiles. **Not for the voice profile**: a Sablier cold start plus loading deps and starting the CLI breaks the voice pipeline. |

### Profile sketch {#profiles}

```yaml
profiles:
  ha-chat:             # Home Assistant conversation agent, text only
    tools: []
    tool_passthrough: true   # see below
    model: <model>
    key_env: KEY_HA_CHAT
  files:               # n8n: send a PDF/image, get text back (content blocks; Read only as fallback)
    tools: []
    files: true
    key_env: KEY_FILES
  transcript-cleanup:  # whisper text in, notes / punctuation / action items out
    tools: []
  homelab-edit:        # Slack bot, see below (writes: own key, internal only)
    cwd: /repo                # own clone; gateway code does commit/push/PR
    setting_sources: []
    tools: [Read, Grep, Glob, Edit, Write]   # deny .claude/** and .git/**
    key_env: KEY_HOMELAB_EDIT
```

## Home Assistant voice assistant {#home-assistant}

Home Assistant's conversation agent sends text and (for device control) its own function definitions as `tools`; HA
executes the calls itself. So the gateway needs **no Home Assistant access of its own** for the voice case.

**Integration (checked 2026-11-20):** core `openai_conversation` uses the Responses API and only works with the
official OpenAI endpoint (no custom base URL), so it is out. Two HACS integrations take a custom base URL and use
chat completions with tools: [`extended_openai_conversation`](https://github.com/jekalmin/extended_openai_conversation)
and [`hass_local_openai_llm`](https://github.com/skye-harris/hass_local_openai_llm). Both send `stream: true` with
usage, so the gateway only needs `/v1/chat/completions` and a complete stream (see Design). No `/v1/responses`.

Two modes, switchable per experiment (same profile, the request decides):

- **Chat only** (no `tools` in the request): plain answers. HA's local intents still handle "turn on the light".
- **Tool-call passthrough** (`tools` present, profile has `tool_passthrough: true`). Experimental, two candidate
  implementations:
    - **Native (preferred, untested):** build the request's `tools` into a per-request in-process SDK MCP server
      (`create_sdk_mcp_server`), capture the first `ToolUseBlock` from the stream, abort the run and return it as
      OpenAI `tool_calls`. HA runs the call and sends the result in the next request. The earlier claim that the
      SDK cannot take client tools was wrong.
    - **Prompt emulation (fallback):** render HA's resent history into one prompt, disable built-in tools and force a
      JSON schema. Measured locally on Haiku: 3 turns, 14.4 s and about 26k cache-creation tokens, and the model
      tried the real tool, failed, and returned no calls. Too slow and unreliable for voice as is.

Latency: every call spawns an agent loop, so expect seconds, not sub-second. Keep `max_turns` low and the system
prompt short on voice profiles, and measure against the [voice benchmark](../02-smarthome/voice-benchmark.md). The
voice profile must not scale to zero.

## Speech-to-text {#stt}

Not possible through the subscription: the mic button in the Claude apps is client-side dictation, and Claude does
not accept audio input. STT stays on `whisper-cpp` (see [Voice](../02-smarthome/voice.md)); if workflows need an
HTTP STT, expose its `/inference` endpoint internally. Claude only post-processes the text (the
`transcript-cleanup` profile).

## Slack bot that edits config {#slack-edit}

A second client of the same service: a Slack bot (Slack app in Socket Mode, built in n8n or as a small bot) that
forwards messages to `/p/homelab-edit/` so Home Assistant and this repo can be edited from chat. This is the riskiest
profile (an agent that writes config), so it is built last and hardened as below (from the 2026-11-20 review).

- **Sessions = Slack threads**: `session: "<channel>-<thread_ts>"`, so a thread keeps its context and files.
- **Slack side**: acknowledge each event within 3 seconds and dedupe on `event_id`, or Slack retries and runs twice.
  Restrict the app to one user ID and one channel; log every request.
- **The agent never commits or pushes.** It edits files in a dedicated clone on its own PVC; the gateway's own code
  then runs the commit, push and PR. Pushes use a GitHub App or fine-grained token limited by a ruleset to
  `claude/**` branches (a deploy key cannot be limited to branches). A human merges, so GitOps and the
  no-private-identifiers rule stay enforced by review, pre-commit and CI.
- **No self-widening.** `setting_sources=[]` (the repo's `.claude/settings.json`, hooks, `CLAUDE.md` and skills must
  not load), rules go in the system prompt, and Edit/Write are denied on `.claude/**` and `.git/**`. Otherwise the
  agent could edit a hook or setting and have it run, which makes any Bash allowlist ineffective.
- **Tool surface**: file tools plus a narrow Bash allowlist (`helm template`, `yamllint`, the validate script).
  No cluster credentials: `automountServiceAccountToken: false`, and it never applies anything, ArgoCD does.
- **Egress**: the subscription token sits in the pod's environment where the agent could read it, so add an egress
  NetworkPolicy allowing only the Anthropic API and the git host.
- **Home Assistant is edited through ha-mcp, not the config volume.** A separate `ha-edit` profile gets the ha-mcp
  server and a tool allowlist, never a write mount. At most a read-only mount of the HA config volume, for reading
  YAML. This works through HA's own API with a scoped token, so changes are validated by HA, and the profile
  can start read-only (state, search, traces, logs) with write tools (automations, scripts, helpers) opt in.
  Take a backup before a write session.
- **Blast radius**: own key, ClusterIP only, `max_turns` and timeout caps, per-day quota cap.

## Open questions {#open-questions}

Closed by the 2026-11-20 review: HA integration (HACS chat-completions ones, see above); `setup-token` works for Team
and lasts one year; Linux needs a glibc image; native tool passthrough is possible in principle.

- [ ] **Seat owner and terms:** is this acceptable on the Team seat (see Goal)? Decide before building.
- [ ] Agent SDK as non-root with a read-only root filesystem is not documented: test early (`HOME` and
      `CLAUDE_CONFIG_DIR` on the PVC, `/tmp` emptyDir).
- [ ] Test the native tool-call passthrough with a real HACS integration and measure voice latency.
- [ ] Verify on Linux (only macOS arm64 was tried) and that the pinned wheel set installs on both mini architectures.
- [ ] Slack transport: n8n workflow vs a small dedicated bot.
- [ ] ha-mcp profile: `mcp_servers` takes HTTP servers with static headers, but ha-mcp runs OAuth-only today (browser
      consent), which a headless gateway cannot do. Needs a static-token mode or a second ha-mcp instance on an
      internal address, with its own scoped HA token, before this profile can exist. Decide which.
- [ ] Per-profile daily quota numbers.

## Build checklist {#build-checklist}

- [ ] Write the service script, profile config and values (copy a small neighbour such as `whisper`).
- [ ] Vault: `CLAUDE_CODE_OAUTH_TOKEN` and one key per profile (the user stores these, never in git).
- [ ] Local render check, deploy, test a one-shot, a resume, a file upload and a passthrough tool call.
- [ ] Add a wiki page under `02-smarthome/`, move the leftovers there and delete this TODO.
