#!/usr/bin/env python3
"""Rewrite the tags of the legacy music library into what Music Assistant expects.

Dry-run by default: prints a plan per album and writes nothing. Pass --apply to write.
See wiki/03-media-stack/music.md for why each rule exists.

Rules (all derived from the 2026-10 audit of the library):
  * An album = (folder, album tag with any trailing CD/Disc marker stripped).
  * Compilation: no artist reaches 60% of the tracks and there are more than 5 artists.
    -> ALBUMARTIST = exactly "Various Artists" (the only spelling Music Assistant recognises), TCMP = 1.
  * Otherwise ALBUMARTIST = the dominant artist.
  * "CD1"/"CD2" suffixes are stripped from the album name and moved into TPOS (disc number/total).
  * Artist spelling: case variants of the same name are merged to the most common spelling, and
    "Artist" + "ArtistGuest" (guest names glued on without a separator) is folded back to "Artist".
  * Tags are rewritten as ID3v2.4 with UTF-8 text, and the ID3v1 block is removed.
  * Track number "001" -> "1". A title prefix equal to the track number ("01 Pump It") is stripped.
  * Year is only filled when the album name itself contains one (e.g. "Top Hits 1999-1").
  * Folders starting with _ . # @ are ignored (Music Assistant ignores them too).

Manual fixes go in an overrides JSON file: per folder, plus an "_artists" map that renames an artist
everywhere (all case variants of the key are renamed):
  {"Folder name": {"albumartist": "Tracy Chapman", "compilation": false},
   "_artists": {"Sita-Happy": "Sita"}}

Needs: pip install mutagen
"""
import argparse
import collections
import concurrent.futures
import json
import os
import re
import subprocess
import sys
import tempfile

from mutagen.id3 import (APIC, ID3, ID3NoHeaderError, TALB, TCMP, TDRC, TIT2, TPE1, TPE2, TPOS, TRCK, TXXX)

VARIOUS = "Various Artists"
IGNORED_PREFIX = ("_", ".", "#", "@")
DISC_RE = re.compile(r"[\s\-_]*[\(\[]?\b(?:cd|disc|disk)[\s._-]*0*(\d+)[\)\]]?\s*$", re.I)
YEAR_RE = re.compile(r"(?<!\d)((?:19[5-9]|20[0-3])\d)(?!\d)")
RG_REFERENCE_LUFS = -18.0  # ReplayGain 2.0; Music Assistant reads REPLAYGAIN_* against this reference
DOMINANT_SHARE = 0.6
COMPILATION_MIN_ARTISTS = 6


def norm(s):
    return re.sub(r"\s+", " ", s.strip()).lower()


def first(tags, key):
    frame = tags.get(key)
    if frame is None or not getattr(frame, "text", None):
        return ""
    return str(frame.text[0]).strip()


def split_disc(album):
    """'Bassleader 2012 Disc 2' -> ('Bassleader 2012', 2). No marker -> (album, None)."""
    m = DISC_RE.search(album)
    if not m:
        return album.strip(), None
    return album[: m.start()].strip(" -_"), int(m.group(1))


def int_part(value):
    m = re.match(r"\s*(\d+)", value or "")
    return int(m.group(1)) if m else None


def load_tracks(root, only):
    albums = collections.OrderedDict()
    for folder in sorted(os.listdir(root)):
        path = os.path.join(root, folder)
        if folder.startswith(IGNORED_PREFIX) or not os.path.isdir(path):
            continue
        if only and folder not in only:
            continue
        for name in sorted(os.listdir(path)):
            if not name.lower().endswith(".mp3"):
                continue
            file = os.path.join(path, name)
            try:
                tags = ID3(file)
            except ID3NoHeaderError:
                print(f"WARN untagged, skipped: {folder}/{name}", file=sys.stderr)
                continue
            album_raw = first(tags, "TALB")
            album, disc = split_disc(album_raw) if album_raw else (folder, None)
            track = dict(
                file=file, tags=tags, artist=first(tags, "TPE1"), album_raw=album_raw,
                disc=disc if disc is not None else int_part(first(tags, "TPOS")),
                track=int_part(first(tags, "TRCK")), track_total=re.search(r"/\s*(\d+)", first(tags, "TRCK")),
                title=first(tags, "TIT2"), year=first(tags, "TDRC") or first(tags, "TYER"),
            )
            albums.setdefault((folder, album), []).append(track)
    return albums


def build_artist_map(albums, renames=None):
    """norm(name) -> canonical spelling, and fold 'Artist' + 'ArtistGuest' into 'Artist'."""
    counts = collections.Counter()
    for tracks in albums.values():
        for t in tracks:
            if t["artist"]:
                counts[t["artist"]] += 1
    by_norm = collections.defaultdict(collections.Counter)
    for name, n in counts.items():
        by_norm[norm(name)][name] += n

    def best(variants):
        # most common spelling; on a tie prefer mixed case over all-lower / all-upper
        return sorted(variants.items(), key=lambda kv: (-kv[1], kv[0].islower() or kv[0].isupper(), kv[0]))[0][0]

    canon = {key: best(v) for key, v in by_norm.items()}
    for src, dst in (renames or {}).items():
        canon[norm(src)] = dst
    return canon


def fold_guest(name, album_artists):
    """'Black Eyed PeasJustin Timberlake' -> 'Black Eyed Peas' when that is the album's dominant artist."""
    for main in album_artists:
        if name != main and norm(name).startswith(norm(main)) and len(name) > len(main) + 2:
            return main
    return name


def plan_album(folder, album, tracks, canon, overrides):
    names = [canon.get(norm(t["artist"]), t["artist"]) for t in tracks if t["artist"]]
    counts = collections.Counter(names)
    dominant, top = (counts.most_common(1)[0] if counts else ("", 0))
    # fold glued-on guest names into the dominant artist before judging "dominance"
    folded = [fold_guest(n, [dominant]) if dominant else n for n in names]
    counts = collections.Counter(folded)
    dominant, top = (counts.most_common(1)[0] if counts else ("", 0))
    share = top / len(tracks) if tracks else 0
    distinct = len(counts)

    ov = overrides.get(folder, {})
    if "compilation" in ov:
        compilation = bool(ov["compilation"])
        reason = "override"
    elif share >= DOMINANT_SHARE:
        compilation, reason = False, f"dominant artist {share:.0%}"
    elif distinct >= COMPILATION_MIN_ARTISTS:
        compilation, reason = True, f"{distinct} artists, top {share:.0%}"
    else:
        compilation, reason = False, f"REVIEW: {distinct} artists, top {share:.0%}"
    albumartist = ov.get("albumartist") or (VARIOUS if compilation else dominant)

    # multi-disc only when the album really has two or more distinct disc numbers. (KnuffelRock stores its
    # album number in TPOS, so every album there has exactly one distinct value.)
    discs = {t["disc"] for t in tracks if t["disc"]}
    disc_total = max(discs) if len(discs) > 1 else 0
    year = ""
    if not any(t["year"] for t in tracks):
        m = YEAR_RE.findall(album)
        year = m[-1] if m else ""

    return dict(
        folder=folder, album=album, compilation=compilation, albumartist=albumartist, reason=reason,
        review=reason.startswith("REVIEW"), disc_total=disc_total if disc_total > 1 else 0, year=year,
        tracks=len(tracks), artists=distinct, dominant=dominant,
    )


def measure(ffmpeg, args):
    """Integrated loudness (LUFS) and true peak (linear) of an ffmpeg input, via the ebur128 filter."""
    out = subprocess.run([ffmpeg, "-nostats", "-hide_banner", *args, "-af", "ebur128=peak=true", "-f", "null", "-"],
                         capture_output=True, text=True).stderr
    summary = out[out.rfind("Summary:"):]
    lufs = re.search(r"I:\s+(-?[\d.]+) LUFS", summary)
    peak = re.search(r"Peak:\s+(-?[\d.]+) dBFS", summary)
    if not lufs or not peak:
        raise RuntimeError("could not measure loudness: " + out[-300:])
    # Loud, clipped MP3s decode to a few dB over full scale (+2..+4 dBFS is normal here). A single corrupt
    # frame can report +27 dBFS, which would permanently cap that album's gain in players: treat anything above
    # +12 dBFS as a decode glitch.
    return float(lufs.group(1)), min(10 ** (float(peak.group(1)) / 20), 4.0)


def replaygain(ffmpeg, tracks, jobs=1):
    """-> ({file: (gain_db, peak)}, (album_gain_db, album_peak)); album = all tracks concatenated."""
    with tempfile.NamedTemporaryFile("w", suffix=".txt", delete=False) as lst:
        for t in tracks:
            lst.write("file '%s'\n" % os.path.abspath(t["file"]).replace("'", "'\\''"))
    try:
        # ffmpeg is the bottleneck (every file is decoded twice), so run the decodes side by side
        with concurrent.futures.ThreadPoolExecutor(max_workers=jobs) as pool:
            album = pool.submit(measure, ffmpeg, ["-f", "concat", "-safe", "0", "-i", lst.name])
            results = list(pool.map(lambda t: measure(ffmpeg, ["-i", t["file"]]), tracks))
            album_lufs, _ = album.result()
    finally:
        os.unlink(lst.name)
    per_track = {t["file"]: (RG_REFERENCE_LUFS - lufs, peak) for t, (lufs, peak) in zip(tracks, results)}
    return per_track, (RG_REFERENCE_LUFS - album_lufs, max(p for _, p in per_track.values()))


def set_rg(tags, name, gain, peak):
    tags.setall(f"TXXX:REPLAYGAIN_{name}_GAIN", [TXXX(encoding=3, desc=f"REPLAYGAIN_{name}_GAIN", text=[f"{gain:.2f} dB"])])
    tags.setall(f"TXXX:REPLAYGAIN_{name}_PEAK", [TXXX(encoding=3, desc=f"REPLAYGAIN_{name}_PEAK", text=[f"{peak:.6f}"])])


def already_done(tracks, ffmpeg):
    """True when every file is ID3v2.4 and (if ReplayGain is wanted) already has album gain: lets a run resume."""
    return all(t["tags"].version == (2, 4, 0) and (not ffmpeg or "TXXX:REPLAYGAIN_ALBUM_GAIN" in t["tags"])
               and t["tags"].get("TPE2") is not None for t in tracks)


def apply_album(plan, tracks, canon, write_cover, ffmpeg=None, jobs=1):
    rg_tracks, rg_album = replaygain(ffmpeg, tracks, jobs) if ffmpeg else ({}, None)
    dominant = plan["dominant"]
    for t in tracks:
        tags = t["tags"]
        artist = canon.get(norm(t["artist"]), t["artist"])
        if dominant:
            artist = fold_guest(artist, [dominant])
        title = t["title"]
        if t["track"] is not None and title:
            m = re.match(r"^0*%d\s*[-.]?\s+(.+)$" % t["track"], title)
            if m:
                title = m.group(1)
        # set every text frame explicitly as UTF-8; assigning here also avoids mutagen's implicit
        # "/"-splitting of v2.3 frames, so AC/DC stays AC/DC
        if artist:
            tags.setall("TPE1", [TPE1(encoding=3, text=[artist])])
        tags.setall("TPE2", [TPE2(encoding=3, text=[plan["albumartist"]])])
        tags.setall("TALB", [TALB(encoding=3, text=[plan["album"]])])
        if title:
            tags.setall("TIT2", [TIT2(encoding=3, text=[title])])
        if plan["compilation"]:
            tags.setall("TCMP", [TCMP(encoding=3, text=["1"])])
        else:
            tags.delall("TCMP")
        if plan["disc_total"] and t["disc"]:
            tags.setall("TPOS", [TPOS(encoding=3, text=[f"{t['disc']}/{plan['disc_total']}"])])
        else:
            tags.delall("TPOS")
        if t["track"] is not None:
            total = t["track_total"].group(1) if t["track_total"] else ""
            tags.setall("TRCK", [TRCK(encoding=3, text=[f"{t['track']}/{total}" if total else str(t["track"])])])
        if plan["year"]:
            tags.setall("TDRC", [TDRC(encoding=3, text=[plan["year"]])])
        tags.delall("TYER")
        if ffmpeg:
            set_rg(tags, "TRACK", *rg_tracks[t["file"]])
            set_rg(tags, "ALBUM", *rg_album)
        for frame in tags.values():  # remaining text frames (genre, ...) -> UTF-8 as well
            if hasattr(frame, "encoding") and not isinstance(frame, APIC):
                frame.encoding = 3
        tags.save(t["file"], v2_version=4, v1=0)  # v1=0 removes the ID3v1 block
    if write_cover:
        folder = os.path.dirname(tracks[0]["file"])
        if not any(os.path.exists(os.path.join(folder, n)) for n in ("cover.jpg", "folder.jpg")):
            for t in tracks:
                pics = t["tags"].getall("APIC")
                if pics and pics[0].mime in ("image/jpeg", "image/jpg"):
                    with open(os.path.join(folder, "cover.jpg"), "wb") as fh:
                        fh.write(pics[0].data)
                    break


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("root", help="library root (one folder per album)")
    ap.add_argument("--apply", action="store_true", help="write the tags (default is a dry-run)")
    ap.add_argument("--only", action="append", help="only this folder name (repeatable)")
    ap.add_argument("--overrides", help="JSON file with per-folder fixes")
    ap.add_argument("--report", help="write the plan as JSON to this file")
    ap.add_argument("--replaygain", action="store_true",
                    help="also measure loudness (ffmpeg ebur128) and write REPLAYGAIN_* tags; slow, read-heavy")
    ap.add_argument("--ffmpeg", default="ffmpeg", help="ffmpeg binary for --replaygain")
    ap.add_argument("--jobs", type=int, default=1, help="parallel ffmpeg decodes for --replaygain")
    ap.add_argument("--resume", action="store_true",
                    help="skip albums whose files are already ID3v2.4 with ReplayGain album tags")
    ap.add_argument("--folder-covers", action="store_true", help="also write cover.jpg from embedded art if missing")
    args = ap.parse_args()

    overrides = json.load(open(args.overrides)) if args.overrides else {}
    albums = load_tracks(args.root, set(args.only or []))
    canon = build_artist_map(albums, overrides.get("_artists"))
    plans = [(plan_album(f, a, ts, canon, overrides), ts) for (f, a), ts in albums.items()]

    for plan, _ in plans:
        flag = "VA " if plan["compilation"] else "ART"
        extra = f" discs={plan['disc_total']}" if plan["disc_total"] else ""
        extra += f" year={plan['year']}" if plan["year"] else ""
        mark = "  <== REVIEW" if plan["review"] else ""
        print(f"{flag} {plan['folder'][:34]:34} | {plan['album'][:34]:34} -> {plan['albumartist'][:24]:24} "
              f"({plan['reason']}){extra}{mark}")
    comps = sum(p["compilation"] for p, _ in plans)
    review = sum(p["review"] for p, _ in plans)
    print(f"\n{len(plans)} albums, {comps} compilations, {len(plans) - comps} artist albums, {review} to review, "
          f"{sum(len(t) for _, t in plans)} files")
    if args.report:
        json.dump([p for p, _ in plans], open(args.report, "w"), indent=1)
    if not args.apply:
        print("dry-run: nothing written (use --apply)")
        return
    for plan, tracks in plans:
        ffmpeg = args.ffmpeg if args.replaygain else None
        if args.resume and already_done(tracks, ffmpeg):
            print(f"  skip (done) {plan['folder']} / {plan['album']}", flush=True)
            continue
        print(f"  {plan['folder']} / {plan['album']}", flush=True)
        apply_album(plan, tracks, canon, args.folder_covers, ffmpeg, args.jobs)
    print("applied")


if __name__ == "__main__":
    main()
