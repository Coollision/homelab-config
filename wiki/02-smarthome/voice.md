# Voice — Whisper & Piper

[← Back to Home](../Home.md) · Smarthome: [Home Assistant](home-assistant.md)

Home Assistant's Assist voice pipeline uses two backend services talking the Wyoming
protocol — neither has any Multus/VLAN legs; both are purely cluster-internal.

## Whisper (speech-to-text) — `workload/smarthome/whisper`

Runs on CPU (not the node's integrated GPU — see below), tuned specifically for a small,
low-power CPU:

- `--cpu-threads` is set to **match** the pod's CPU resource limit exactly. Mismatching
  these causes CFS-throttling latency that's easy to misdiagnose as "the model is just
  slow."
- A domain-specific initial prompt (in the household's spoken language, biased toward
  home-automation vocabulary — room names, device types) fixes first-word misrecognition
  on short voice commands. **Keep this short** — the model has a hard token cap on the
  prompt, and an overly long prompt causes it to hallucinate prompt words back into
  transcriptions instead of improving accuracy. Only grow it with words the model
  actually mis-hears; Home Assistant's own conversation/entity matching already does
  fuzzy matching, so the prompt doesn't need to be exhaustive.

**iGPU acceleration was evaluated and rejected** — on this hardware class, the
integrated-GPU path had negligible speed gains and open accuracy bugs in its Vulkan
compute path, with only unmaintained Wyoming bridges available for it. Worth
re-evaluating only on meaningfully more capable GPU hardware, not on an incremental CPU
upgrade.

## Piper (text-to-speech) — `workload/smarthome/piper`

Much simpler — a fixed voice model, no comparable tuning surface. Runs on the same class
of node as Whisper for co-location convenience, not because it needs the CPU.

## What's missing

There is no wake-word component currently wired into the active voice pipeline — a
self-hosted wake-word engine was trialed at some point and is currently disabled. If
wake-word detection is happening at all today, it's most likely on-device in ESPHome
voice-satellite firmware rather than in this cluster-side pipeline — worth confirming
before assuming voice activation "just works" end-to-end through these two services
alone.
