#!/bin/bash
# Runs one /wcpp.sh invocation per configuration (a fresh server each time) and reports leaked servers.
# usage: matrix.sh label "model gpu|cpu ac beam prompt" ...   (stdout = TSV rows, stderr = progress)
# usage: other_matrix.sh label cfg...
label=$1; shift
for c in "$@"; do
  /wcpp.sh $label "$c"
  echo "done: $c servers_left=$(for d in /proc/[0-9]*; do tr '\0' ' ' < $d/cmdline 2>/dev/null; echo; done | grep -c '^whisper-server')" >&2
done
echo MATRIX-DONE >&2
