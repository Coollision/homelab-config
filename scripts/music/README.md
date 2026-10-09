# Music library tag cleanup

`cleanup.py` rewrites the tags of the legacy library (flat, one folder per album) into what Music
Assistant expects. Rules and reasons: [wiki/03-media-stack/music.md](../../wiki/03-media-stack/music.md).

```
pip install mutagen
python3 cleanup.py /path/to/Music                      # dry-run: prints the plan, writes nothing
python3 cleanup.py /path/to/Music --report plan.json   # also save the plan
python3 cleanup.py /path/to/Music --only "Dido"        # one folder (repeatable)
python3 cleanup.py /path/to/Music --overrides fixes.json --apply --replaygain --folder-covers
```

`fixes.json` overrides the detection per folder:
`{"Folder name": {"albumartist": "Tracy Chapman", "compilation": false}}`

- Folders starting with `_ . # @` are skipped, like Music Assistant does.
- `--replaygain` shells out to `ffmpeg` (EBU R128, reference -18 LUFS) and reads every file: run it on the
  NAS, not over SMB. Per-folder lines are printed while it runs.
- `--apply` rewrites every file in place (ID3v2.4, UTF-8, ID3v1 removed), which changes the mtime so
  Music Assistant re-reads it. Run it before the real provider is added.
- Test on a copy first (`--only` plus a copied folder); a header-only copy (first 300 KB of each file)
  is enough for the tag logic.
