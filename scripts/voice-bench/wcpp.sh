#!/bin/bash
# Time whisper.cpp's whisper-server on the benchmark clips. Prints TSV: config  file  seconds  json-text
# usage: [IDS="03 06"] [REPS=1] [THREADS=4] [EXTRA="-nfa"] [TAG=nfa] wcpp.sh label "model gpu|cpu ac beam prompt(0/1)" ...
#   ac = audio context (0 = full 30 s, 400 = short clips), prompt = use the home-automation initial prompt
#   EXTRA = extra whisper-server flags, TAG = suffix added to the config name; environment variables such as
#   GGML_VK_DISABLE_F16=1 are inherited by the server.
THREADS=${THREADS:-4}
PROMPT="Zet het licht aan in de keuken. Zijn de luiken in de kinderkamer gesloten? Doe het rolluik van het grote raam omhoog. Zet de plafondspots in de woonkamer uit. Speel muziek in de keuken. Wat is de temperatuur buiten?"
for cfg in "${@:2}"; do
  set -- $cfg; model=$1 dev=$2 ac=$3 beam=$4 pr=$5
  name="$1/$2/ac$3/b$4/p$5${TAG:+/$TAG}"
  args="-m /models/ggml-$model.bin -l nl -t $THREADS -bs $beam -ac $ac --port 8099 --host 127.0.0.1 $EXTRA"
  [ "$dev" = cpu ] && args="$args -ng"
  [ "$pr" = 1 ] && args="$args --prompt \"$PROMPT\""
  eval exec whisper-server $args > /tmp/srv.log 2>&1 &
  pid=$!
  for i in $(seq 1 120); do curl -s -o /dev/null http://127.0.0.1:8099/ && break; sleep 1; done
  f0=/audio/clean_01.wav
  for w in 1 2; do curl -s -o /dev/null -F file=@$f0 -F response_format=json http://127.0.0.1:8099/inference; done
  for id in ${IDS:-01 03 05 06 08 10}; do for c in clean noisy10; do for r in $(seq 1 ${REPS:-1}); do
    f=/audio/${c}_$id.wav
    out=$(curl -s -w '\n%{time_total}' -F file=@$f -F response_format=json http://127.0.0.1:8099/inference)
    t=$(echo "$out" | tail -1); txt=$(echo "$out" | head -n -1 | tr '\n\t' '  ')
    printf '%s\t%s\t%s\t%s\n' "$name" "$(basename $f)" "$t" "$txt"
  done; done; done
  kill $pid 2>/dev/null; pkill whisper-server 2>/dev/null; wait $pid 2>/dev/null; sleep 1   # exec + pkill: an orphaned server keeps ~1-2 GB and OOM-kills the pod after ~10 configs
done
