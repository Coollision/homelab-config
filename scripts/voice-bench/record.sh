#!/bin/bash
# Record the benchmark phrases (phrases.txt) with a real microphone, one at a time, as 16 kHz mono wav.
# Usage: ./record.sh [quiet|noisy]   (default quiet; noisy = same phrases with background noise, e.g. radio/tap)
# Needs: ffmpeg on macOS (avfoundation). Output: ./audio-real/<mode>_NN.wav  (same NN as phrases.txt)
# Mic: MIC=<index> to override (list with: ffmpeg -f avfoundation -list_devices true -i "")
set -euo pipefail
cd "$(dirname "$0")"   # phrases.txt and audio-real/ live next to this script, whatever the caller's cwd
MODE=${1:-quiet}; MIC=${MIC:-0}
case "$MODE" in quiet|noisy) ;; *) echo "usage: $0 [quiet|noisy]" >&2; exit 1 ;; esac
mkdir -p audio-real
# phrases come in on fd 3 and key presses from the terminal: stdin of the loop would otherwise feed the prompts
INPUT=${INPUT:-/dev/tty}
while IFS='|' read -r -u 3 id text; do
  out="audio-real/${MODE}_${id}.wav"
  printf '\n[%s/%s] Say:  %s\n' "$id" "$MODE" "$text"
  read -r -p "  Press Enter to START recording (speak naturally, from where you normally stand)... " <"$INPUT"
  ffmpeg -nostdin -v error -y -f avfoundation -i ":$MIC" -ar 16000 -ac 1 "$out" &
  pid=$!
  read -r -p "  ...recording. Press Enter to STOP. " <"$INPUT"
  kill -TERM "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true
  dur=$(ffprobe -v error -show_entries format=duration -of csv=p=0 "$out")
  printf '  saved %s (%.1f s)\n' "$out" "$dur"
done 3< phrases.txt
echo; echo "Done. Files in audio-real/"
