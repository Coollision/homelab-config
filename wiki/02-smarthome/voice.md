# Voice — Whisper & Piper

[← Back to Home](../Home.md) · Smarthome: [Home Assistant](home-assistant.md)

Home Assistant's Assist voice pipeline uses two backend services talking the Wyoming
protocol — neither has any Multus/VLAN legs; both are purely cluster-internal.

## Whisper (speech-to-text) — `whisper` bridge + `whisper-cpp`

Speech-to-text is two apps in the `smarthome` namespace:

```text
Home Assistant ──Wyoming──▶ whisper-service:10300      workload/smarthome/whisper      (bridge, any node)
                                   │ HTTP :8099
                                   ▼
                       whisper-cpp-service:8099        workload/smarthome/whisper-cpp  (whisper.cpp, Intel iGPU)
```

- **`whisper-cpp`** runs the `whisper.cpp` server (`small` model, Vulkan) on the **Intel iGPU**. It asks the scheduler
  for `gpu.intel.com/i915`, which the Intel device plugin advertises on both mini nodes
  (`system/inteldeviceplugins-system`, sharing slots set by `sharedDevNum`). There is deliberately **no preference
  for one mini**: the N150 and N100 iGPUs differ by only about 0.6 s per command, and a pod that moves just works.
  The model (488 MB) lives in an `emptyDir` and is re-downloaded on every pod start (pinned Hugging Face revision,
  sha256 checked): no Longhorn volume, so nothing ties the pod to one node.
- **`whisper`** is a small **Wyoming bridge** (the `rhasspy/wyoming-whisper` image running a script from a
  ConfigMap): it buffers the audio until the end of the utterance, wraps it as a WAV, POSTs it to `whisper-cpp` and
  returns the text. It returns an empty transcript on any failure, so a pipeline never hangs on it. The service name
  `whisper-service` did not change, so Home Assistant needed no change. The image tag is pinned because the script uses
  that image's `wyoming` package; Renovate bumps of it are labelled `test-voice-bridge` and never automerge.

Settings that matter (measured on the iGPU, see [Voice benchmark](voice-benchmark.md#igpu)):

- **`-ac 400`** (audio context) limits a command to 8 s of audio and is the big speed-up (6.8 s to 2.1 s) with no
  accuracy loss. A longer command is cut off (the bridge logs a warning above 8 s). Real commands recorded so far were
  under 5 s.
- A domain-specific **`--prompt`** (household language, home-automation vocabulary) cuts word error in a noisy room
  from 23–26 % to 10–12 %. Keep it to words the model actually mis-hears; Home Assistant matches entity names fuzzily.
- **`-t 3`**: the minis also run etcd and zigbee2mqtt, and the GPU does the heavy lifting.
- **Warm-up**: the first request after a start compiles the Vulkan shaders (several seconds). `whisper-cpp` warms
  itself up in a `postStart` hook and the bridge sends one warm-up request when it starts.
- The `whisper-cpp` image is pinned by digest: Intel Vulkan compute has had accuracy bugs, so re-run
  `scripts/voice-bench` ([Re-running](voice-benchmark.md#rerun)) before bumping it.
- The old setup, `faster-whisper` on a mini's CPU (9.6 s), is the rollback: restore its values from git history and the
  bridge goes away; `whisper-service` keeps its name.
- Do **not** run speech-to-text on the Raspberry Pi, the VM worker or the NAS (15–22 s per command).

**Result in real use (2026-11-01):** a command spoken in the kitchen's normal voice transcribes in about **2.2 s**
with **8 % word error** on the benchmark's clips, the same as the synthetic voice
([details](voice-benchmark.md#real-voice)).

**iGPU acceleration: rejected in 2026-07, reversed in 2026-11.** The first evaluation (July) found negligible gains and
open accuracy bugs in the Vulkan compute path, with only unmaintained Wyoming bridges available. The November
[benchmark](voice-benchmark.md#igpu), once both minis' iGPUs worked for compute, showed the opposite with `whisper.cpp`
and a shortened audio context. The earlier "unstable accuracy" turned out to be a benchmark bug, not the GPU.

Whisper was scaled to zero during the benchmark (so it did not compete for CPU); that is over.

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
