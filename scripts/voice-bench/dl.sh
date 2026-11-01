#!/bin/sh
for m in tiny base small; do
  [ -f /models/ggml-$m.done ] && continue
  rm -f /models/ggml-$m.bin
  curl -sfL --retry 3 --max-time 1200 -o /models/ggml-$m.bin https://huggingface.co/ggerganov/whisper.cpp/resolve/main/ggml-$m.bin && touch /models/ggml-$m.done
done
