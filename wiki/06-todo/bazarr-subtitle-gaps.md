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
- [x] Re-run the missing search after the OpenSubtitles quota resets; find out whether the account is free or VIP. Done 2026-11-20: ran it, all providers Good and unthrottled, 0 new subtitles (140 episodes still missing); free vs VIP still unknown.
- [x] Disable `tvsubtitles` (404s): done 2026-11-12 via the Bazarr settings API.
- [x] Shorten adaptive searching (still 3w delay, 1w delta). Done 2026-11-20 via the settings API: delay 1w, delta 3d.
- [ ] Optional: Whisper for the roughly 22 episodes with no English subtitle either. Bazarr's Whisper provider needs a
      `whisper-asr-webservice`-style API, so it needs a separate deployment; est. 5-10 min per episode on the iGPU
      (unmeasured), run off-hours so it does not compete with voice.
