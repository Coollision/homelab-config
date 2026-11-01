# TODO — Fast Whisper: what is left after the iGPU deployment

[← Back to Home](../Home.md) · Related: [Voice](../02-smarthome/voice.md) · [Voice benchmark](../02-smarthome/voice-benchmark.md#real-voice)

> **Status (2026-11-01):** deployed. The Intel device plugin, the `whisper-cpp` server (iGPU) and the Wyoming bridge are
> live and verified with real clips (2.2 s, 8 % word error) and with spoken commands through Home Assistant. The old
> `whisper-data` volume has been removed. How it works is on the [Voice](../02-smarthome/voice.md)
> page. Delete this page once the checklist below is done.

## Left to do

- [ ] Noisy-room pass: `scripts/voice-bench/record.sh noisy`, then compare with the 12 % benchmark figure.
- [ ] Optional: try `-bs 1` and `-ac 300` (1.7 s) after the first week of real use; look for a Dutch fine-tuned `small`
      model in `ggml` format.

## Watch-outs

- `-ac 400` truncates audio beyond about 8 s (the bridge logs a warning).
- Intel Vulkan compute has a history of accuracy bugs: the images are pinned, re-run `scripts/voice-bench` after bumping.
- The model is downloaded from Hugging Face on every `whisper-cpp` pod start; if that proves flaky, move it to an NFS volume.
- If `whisper-cpp` is down, voice is down (no automatic CPU fallback); rollback is restoring the old `faster-whisper`
  values from git history.
