#!/bin/sh
me=$$
for d in /proc/[0-9]*; do p=${d#/proc/}; [ "$p" = "$me" ] && continue
  c=$(tr "\0" " " < $d/cmdline 2>/dev/null)
  case "$c" in *wcpp.sh*|*runcpp*|*whisper-server*|*curl*|*dl.sh*|*"sh -c"*wcpp*|*"sh -c for m"*) kill -9 $p 2>/dev/null;; esac
done
