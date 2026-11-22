#!/usr/bin/env bash
# Block private identifiers (domains, IPs, hostnames, emails) from reaching the public repo.
#
# This script contains NO private patterns. They come from, in order:
#   1. Vault  kv/shared/identifier-patterns  key `patterns` (one ERE per line, `#` comments allowed);
#      a successful read refreshes a local cache so pushes keep working while Vault is down
#   2. the cache  ${XDG_CACHE_HOME:-~/.cache}/homelab-identifier-scan/patterns  (mode 600, outside the repo)
#   3. optional extras in .claude/private-patterns.txt (gitignored, same format)
#   $IDENTIFIER_PATTERNS_FILE overrides all of it (tests). No patterns at all => fail closed.
# Generic rules (RFC1918 addresses; the k3s default 10.42/10.43 ranges and CIDR ranges are allowed) are built in.
# Matches are printed MASKED (`***`), so the block message never leaks the value into logs or terminals.
#
# Usage:
#   identifier-scan.sh pre-push <remote> <url>   git pre-push hook (reads ref lines on stdin); scans every added
#                                                line and commit message of the commits being pushed
#   identifier-scan.sh --tree [rev]              audit all tracked files at rev (default HEAD)
#   identifier-scan.sh --range <a..b>            scan a commit range
#   identifier-scan.sh --seed-vault [--write]    build the pattern list from Vault shared/domains + shared/network
#                                                (+ extras file + what is already stored) and show counts;
#                                                --write stores it in Vault. Add one-off rules with `vault kv patch`
#   identifier-scan.sh --sources                 which source would be used (names/counts only)
# Skip once (guard rail, not a lock): SKIP_IDENTIFIER_SCAN=1 git push   (or git push --no-verify)
set -euo pipefail
[ -n "${SKIP_IDENTIFIER_SCAN:-}" ] && { echo "identifier-scan: skipped (SKIP_IDENTIFIER_SCAN)" >&2; exit 0; }
[ -n "${CI:-}" ] && exit 0
cd "$(git rev-parse --show-toplevel)"
export IDS_ROOT="$PWD"
# python reads its own code from stdin (heredoc), so hand the hook's ref lines over via the environment
[ "${1:-}" = pre-push ] && { IDS_STDIN=$(cat); export IDS_STDIN; }
exec python3 -I - "$@" <<'PY'
import json, os, re, subprocess, sys

ROOT = os.environ["IDS_ROOT"]
VAULT_PATH = "shared/identifier-patterns"
CACHE = os.path.join(os.environ.get("XDG_CACHE_HOME") or os.path.expanduser("~/.cache"),
                     "homelab-identifier-scan", "patterns")
EXTRAS = os.path.join(ROOT, ".claude", "private-patterns.txt")
ZERO = "0" * 40

# Generic, non-private rules. RFC1918 except the k3s default pod/service ranges (10.42/10.43); CIDR forms are allowed.
GENERIC = [
    r"(?<![\d.])(?:192\.168\.\d{1,3}\.\d{1,3}|172\.(?:1[6-9]|2\d|3[01])\.\d{1,3}\.\d{1,3}|10\.(?!4[23]\.)\d{1,3}\.\d{1,3}\.\d{1,3})(?!\d)(?!/\d)",
]

def run(*a, inp=None):
    return subprocess.run(a, capture_output=True, text=True, input=inp)

def parse(text):
    return [l.strip() for l in text.splitlines() if l.strip() and not l.lstrip().startswith("#")]

def vault_get(path, field=None):
    env = dict(os.environ, VAULT_CLIENT_TIMEOUT="6")
    cmd = ["vault", "kv", "get", "-mount=kv"] + (["-field=" + field] if field else ["-format=json"]) + [path]
    try:
        r = subprocess.run(cmd, capture_output=True, text=True, env=env, timeout=15)
    except (OSError, subprocess.TimeoutExpired):
        return None
    return r.stdout if r.returncode == 0 else None

def load_patterns():
    """Return (list_of_ERE, source_label) or ([], reason)."""
    override = os.environ.get("IDENTIFIER_PATTERNS_FILE")
    if override:
        return parse(open(override).read()), "IDENTIFIER_PATTERNS_FILE"
    pats, src = [], None
    txt = vault_get(VAULT_PATH, "patterns")
    if txt and parse(txt):
        pats, src = parse(txt), "vault"
        try:
            os.makedirs(os.path.dirname(CACHE), mode=0o700, exist_ok=True)
            with open(CACHE, "w") as f: f.write(txt if txt.endswith("\n") else txt + "\n")
            os.chmod(CACHE, 0o600)
        except OSError:
            pass
    elif os.path.exists(CACHE):
        pats, src = parse(open(CACHE).read()), "cache (Vault unreachable or empty)"
    if os.path.exists(EXTRAS):
        pats += parse(open(EXTRAS).read())
        src = (src + " + extras") if src else "extras only"
    return pats, src or "none"

def compile_all(pats):
    out = []
    for p in pats + GENERIC:
        try: out.append(re.compile(p, re.I))
        except re.error as e: print(f"identifier-scan: bad pattern skipped ({e})", file=sys.stderr)
    return out

def mask(line, rx):
    for r in rx: line = r.sub("***", line)
    return line

def hit(line, rx):
    return any(r.search(line) for r in rx)

def scan_patch(rng_args, rx):
    """Scan added lines + commit messages of the given `git log` revision args."""
    p = run("git", "log", "-p", "-U0", "--no-color", "--no-ext-diff", "--format=@@COMMIT %H%n%B%n@@ENDMSG", *rng_args)
    if p.returncode != 0:
        print(p.stderr.strip(), file=sys.stderr); return None
    findings, commit, path, ln, in_msg = [], None, None, 0, False
    for l in p.stdout.splitlines():
        if l.startswith("@@COMMIT "):
            commit, in_msg, path = l.split()[1][:10], True, None; continue
        if in_msg:
            if l == "@@ENDMSG": in_msg = False; continue
            if hit(l, rx): findings.append((commit, "(commit message)", 0, mask(l, rx)))
            continue
        if l.startswith("+++ "):
            path = l[6:] if l.startswith("+++ b/") else None; continue
        if l.startswith("@@ "):
            m = re.search(r"\+(\d+)", l); ln = int(m.group(1)) if m else 0; continue
        if l.startswith("+") and path:
            if hit(l[1:], rx): findings.append((commit, path, ln, mask(l[1:], rx).strip()[:160]))
            ln += 1
    return findings

def scan_tree(rev, rx):
    files = run("git", "ls-tree", "-r", "--name-only", rev).stdout.splitlines()
    findings = []
    for f in files:
        b = subprocess.run(["git", "show", f"{rev}:{f}"], capture_output=True).stdout
        if b"\0" in b[:8192]: continue
        for i, l in enumerate(b.decode("utf-8", "replace").splitlines(), 1):
            if hit(l, rx): findings.append((rev[:10], f, i, mask(l, rx).strip()[:160]))
    return findings

def report(findings, what):
    if not findings: return 0
    print(f"\nidentifier-scan: BLOCKED, {len(findings)} private identifier(s) in {what}:", file=sys.stderr)
    for c, f, n, l in findings[:40]:
        print(f"  {c} {f}{':' + str(n) if n else ''}: {l}", file=sys.stderr)
    if len(findings) > 40: print(f"  ... and {len(findings) - 40} more", file=sys.stderr)
    print("Replace with placeholders (<internal-domain>, <node-name>, <vlan-ip>) or Vault placeholders, then amend/rebase the\n"
          "commits. Matches are masked as ***. Skip once only if you are sure: SKIP_IDENTIFIER_SCAN=1 git push", file=sys.stderr)
    return 1

def seed(write):
    pats = []
    d = vault_get("shared/domains")
    n = vault_get("shared/network")
    for blob in (d, n):
        if not blob: print("identifier-scan: cannot read Vault shared/* (login first)", file=sys.stderr); return 2
        data = (json.loads(blob)["data"]["data"] or {})
        for k, v in data.items():
            for part in re.split(r"[,\s]+", str(v)):
                part = part.strip()
                if len(part) >= 4 and not part.startswith(("http", "-")):
                    pats.append(re.escape(part))
    if os.path.exists(EXTRAS): pats += parse(open(EXTRAS).read())
    existing = vault_get(VAULT_PATH, "patterns")        # merge, never drop hand-added rules (node names, emails)
    if existing: pats += parse(existing)
    pats = sorted(set(pats))
    print(f"seed: {len(pats)} pattern(s) from Vault shared/domains, shared/network and the extras file (values not printed)")
    if not write: print("dry run: add --write to store them in Vault kv/" + VAULT_PATH); return 0
    body = "# one ERE per line; managed by scripts/identifier-scan.sh --seed-vault, edit with `vault kv patch`\n" + "\n".join(pats) + "\n"
    r = run("vault", "kv", "put", "-mount=kv", VAULT_PATH, "patterns=" + body)
    print(r.stdout.strip().splitlines()[-1] if r.returncode == 0 else r.stderr.strip(), file=sys.stderr if r.returncode else sys.stdout)
    return r.returncode

def main(argv):
    if not argv: print(__doc__ if False else "usage: identifier-scan.sh pre-push|--tree|--range|--seed-vault|--sources", file=sys.stderr); return 2
    if argv[0] == "--seed-vault": return seed("--write" in argv)
    pats, src = load_patterns()
    if argv[0] == "--sources":
        print(f"source: {src}; {len(pats)} custom pattern(s) + {len(GENERIC)} generic rule(s)"); return 0 if pats else 1
    if not pats:
        print("identifier-scan: BLOCKED, no private patterns available (Vault unreachable and no cache). Log in to Vault once\n"
              "(`vault login`) or run `scripts/identifier-scan.sh --sources`. Skip once: SKIP_IDENTIFIER_SCAN=1", file=sys.stderr)
        return 1
    rx = compile_all(pats)
    if argv[0] == "--tree":
        rev = argv[1] if len(argv) > 1 else "HEAD"
        f = scan_tree(rev, rx); print(f"identifier-scan: {len(f)} hit(s) in tree {rev} (patterns from {src})"); return 1 if report(f, f"tree {rev}") else 0
    if argv[0] == "--range":
        f = scan_patch([argv[1]], rx); return 1 if f is None or report(f, argv[1]) else 0
    if argv[0] == "pre-push":
        allf = []
        for line in os.environ.get("IDS_STDIN", "").splitlines():
            parts = line.split()
            if len(parts) != 4: continue
            _, local_sha, _, remote_sha = parts
            if local_sha == ZERO: continue                       # branch delete
            rng = [local_sha, "--not", "--remotes"] if remote_sha == ZERO else [f"{remote_sha}..{local_sha}"]
            f = scan_patch(rng, rx)
            if f is None: print("identifier-scan: could not read the push range, blocking to be safe", file=sys.stderr); return 1
            allf += f
        if report(allf, "the commits being pushed"): return 1
        print(f"identifier-scan: clean (patterns from {src})", file=sys.stderr); return 0
    print("unknown mode", file=sys.stderr); return 2

sys.exit(main(sys.argv[1:]))
PY
