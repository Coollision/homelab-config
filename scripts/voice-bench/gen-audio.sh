#!/bin/bash
# Generate the benchmark audio: 10 Dutch home-automation phrases (phrases.txt) spoken by the cluster's Piper
# voice, as 16 kHz mono wav, plus a copy of each with pink noise mixed in at ~10 dB SNR.
# Needs: kubectl (smarthome namespace), ffmpeg, python3. Output: ./audio/clean_NN.wav and noisy10_NN.wav
# A throwaway pod is used because exec'ing piper inside the live piper pod OOM-kills it.
set -euo pipefail
NS=${NS:-smarthome}; VOICE=${VOICE:-nl_BE-rdh-medium}; IMG=${IMG:-rhasspy/wyoming-piper:2.5.2}
mkdir -p audio
kubectl -n "$NS" run bench-tts --image="$IMG" --restart=Never --labels=bench=voice \
  --overrides='{"spec":{"containers":[{"name":"bench-tts","image":"'"$IMG"'","command":["sleep","3600"],"resources":{"limits":{"memory":"1Gi"}}}]}}'
kubectl -n "$NS" wait pod/bench-tts --for=condition=Ready --timeout=180s
k() { kubectl -n "$NS" exec bench-tts -- "$@"; }
k sh -c "cd /tmp && /usr/src/.venv/bin/python3 -m piper.download_voices --data-dir /tmp $VOICE"
while IFS='|' read -r id text; do
  k /usr/src/.venv/bin/python3 -m piper -m "$VOICE" --data-dir /tmp -f "/tmp/c$id.wav" -- "$text" >/dev/null 2>&1
  k cat "/tmp/c$id.wav" > "audio/raw_$id.wav"
done < phrases.txt
kubectl -n "$NS" delete pod bench-tts --wait=false

meanvol() { ffmpeg -i "$1" -af volumedetect -f null - 2>&1 | sed -n 's/.*mean_volume: \(-[0-9.]*\) dB.*/\1/p'; }
noise_ref=$(ffmpeg -f lavfi -t 2 -i "anoisesrc=color=pink:amplitude=0.5:sample_rate=22050" -af volumedetect -f null - 2>&1 | sed -n 's/.*mean_volume: \(-[0-9.]*\) dB.*/\1/p')
for raw in audio/raw_*.wav; do
  id=${raw##*raw_}; id=${id%.wav}
  speech=$(meanvol "$raw"); dur=$(ffprobe -v error -show_entries format=duration -of csv=p=0 "$raw")
  gain=$(python3 -c "print(round(($speech - 10) - ($noise_ref), 1))")   # noise 10 dB below the speech level
  ffmpeg -v error -y -i "$raw" -f lavfi -t "$dur" -i "anoisesrc=color=pink:amplitude=0.5:sample_rate=22050:seed=7" \
    -filter_complex "[1]volume=${gain}dB[n];[0][n]amix=inputs=2:normalize=0,aresample=16000" -ac 1 "audio/noisy10_$id.wav"
  ffmpeg -v error -y -i "$raw" -ar 16000 -ac 1 "audio/clean_$id.wav"
  rm "$raw"
done
ls audio
