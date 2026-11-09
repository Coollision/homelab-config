# TODO — Bazarr subtitle gaps and Lingarr

[← Back to Home](../Home.md) · Related: [Media stack](../03-media-stack/overview.md)

> **Status (2026-11-09):** a full missing-subtitle search found nothing new. About 130 episodes and 3 movies still miss
> a subtitle, mostly Dutch for English-audio shows. OpenSubtitles.com reports `DownloadLimitExceeded` (daily quota) and
> `tvsubtitles` returns HTTP errors (still enabled on 2026-11-09, throttled every 10 min).

## Left to do

- [ ] **Investigate Lingarr** as the translator for Bazarr (English subtitle to Dutch). Expectation: better than the
      built-in Google Translate because it can use an LLM or DeepL and keeps context across lines. Check: Bazarr
      integration support in 1.6, which translation backends it offers, cost or API limits, CPU/RAM footprint on K3s,
      and whether a self-hosted model is realistic on the minis. Compare translated Dutch quality on a few episodes
      against Google Translate.
- [ ] Re-run the missing search after the OpenSubtitles quota resets; find out whether the account is free or VIP.
- [ ] Disable `tvsubtitles` (404s) and shorten adaptive searching (now 3w delay, 1w delta).
- [ ] Optional: Whisper for the roughly 22 episodes with no English subtitle either. Bazarr's Whisper provider needs a
      `whisper-asr-webservice`-style API, so it needs a separate deployment; est. 5-10 min per episode on the iGPU
      (unmeasured), run off-hours so it does not compete with voice.
