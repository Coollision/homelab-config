import json, sys, os
S = {(o["node"], o["cfg"]): o for o in json.load(open("res/summary.json"))}
OUT = sys.argv[1]
os.makedirs(OUT, exist_ok=True)
TXT, GRID, MUTED = "#4b5563", "#d1d5db", "#6b7280"
C1, C2, C3, C4 = "#2563eb", "#ea580c", "#0d9488", "#7c3aed"
NODE = {"pi": "Raspberry Pi", "vm": "VM worker (J4125, no AVX)", "minib": "Mini B (N100)", "minia": "Mini A (N150)", "nas": "Synology NAS (J4125)"}


def svg(w, h, body, title):
    return (f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {w} {h}" width="{w}" height="{h}" role="img" aria-label="{title}" '
            f'font-family="system-ui,-apple-system,Segoe UI,sans-serif" font-size="13">'
            f'<title>{title}</title>{body}</svg>')


def t(x, y, s, anchor="start", fill=TXT, size=13, weight="normal"):
    return f'<text x="{x}" y="{y}" text-anchor="{anchor}" fill="{fill}" font-size="{size}" font-weight="{weight}">{s}</text>'


def hbar(path, title, rows, unit="s", ref=None, maxv=None, note=None, left=230):
    """rows: [(label, value, color, annotation)]"""
    L, R, top, bh, gap = left, 150, 50, 26, 12
    w = 590 + left
    h = top + len(rows) * (bh + gap) + 40
    maxv = maxv or max(r[1] for r in rows) * 1.1
    sc = (w - L - R) / maxv
    b = t(10, 24, title, weight="600", size=15)
    for i in range(0, 5):
        v = maxv * i / 4
        x = L + v * sc
        b += f'<line x1="{x:.0f}" y1="{top-8}" x2="{x:.0f}" y2="{h-30}" stroke="{GRID}" stroke-width="1"/>' + t(x, h - 12, f"{v:.0f}{unit}", "middle", MUTED, 11)
    if ref:
        x = L + ref[0] * sc
        b += f'<line x1="{x:.0f}" y1="{top-8}" x2="{x:.0f}" y2="{h-30}" stroke="{C3}" stroke-width="2" stroke-dasharray="5 4"/>' + t(x + 4, top - 12, ref[1], "start", C3, 11)
    for i, (lab, v, col, ann) in enumerate(rows):
        y = top + i * (bh + gap)
        b += t(L - 10, y + bh * 0.68, lab, "end") + f'<rect x="{L}" y="{y}" width="{max(v*sc,2):.0f}" height="{bh}" rx="3" fill="{col}"/>'
        b += t(L + v * sc + 6, y + bh * 0.68, ann or f"{v:.1f}{unit}", "start", TXT, 12)
    open(path, "w").write(svg(w, h, b, title))


def g(node, cfg): return S.get((node, cfg))


# 1. platform comparison: best configuration of each engine per platform
CF, CC, CG = C1, C2, C4   # faster-whisper CPU / whisper.cpp CPU / whisper.cpp iGPU
plat = [("minia", "cpp:small/gpu/ac400/b3/p1/grid", "iGPU whisper.cpp", CG), ("minia", "fw:small", "CPU faster-whisper", CF),
        ("minib", "cpp:small/gpu/ac400/b3/p1", "iGPU whisper.cpp", CG), ("minib", "fw:small", "CPU faster-whisper", CF),
        ("nas", "cpp:small/gpu/ac400/b3/p1", "iGPU whisper.cpp", CG), ("nas", "fw:small", "CPU faster-whisper", CF),
        ("vm", "fw:small", "CPU faster-whisper", CF),
        ("pi", "cpp:small/cpu/ac400/b3/p1", "CPU whisper.cpp", CC), ("pi", "fw:small", "CPU faster-whisper", CF)]
rows = []
for n, c, lab, col in plat:
    o = g(n, c)
    if o: rows.append((f"{NODE[n]}: {lab}", o["p50"], col, f'{o["p50"]:.1f}s'))
hbar(f"{OUT}/platforms.svg", "Median time per voice command, small model, best settings per engine", rows, ref=(2, "2s: feels responsive"), maxv=24, left=340)

# 2. model size on mini A + accuracy
rows = []
for m, col in [("tiny", C3), ("base", C2), ("small", C1)]:
    o = g("minia", "fw:" + m)
    if o: rows.append((f"{m}  (latency)", o["p50"], col, f'{o["p50"]:.2f}s'))
hbar(f"{OUT}/models-latency.svg", "Model size vs latency on Mini A (faster-whisper, int8)", rows, maxv=4.5, ref=(2, "2s"))
rows = []
for m, col in [("tiny", C3), ("base", C2), ("small", C1)]:
    o = g("minia", "fw:" + m)
    if o:
        rows.append((f"{m} clean", o["wer_clean"], col, f'{o["wer_clean"]:.0f}%'))
        rows.append((f"{m} noisy (10 dB)", o["wer_noisy"], col, f'{o["wer_noisy"]:.0f}%'))
hbar(f"{OUT}/models-wer.svg", "Word error rate by model (lower is better)", rows, unit="%", maxv=35)

# 3. engine/backend on mini A: latency vs error
pts = []
for (n, c), o in S.items():
    if n != "minia" or o["n"] < 12 or o["wer_clean"] is None: continue
    err = (o["wer_clean"] + (o["wer_noisy"] or o["wer_clean"])) / 2
    kind = "gpu" if "/gpu/" in c else "cpp" if c.startswith("cpp:") else "fw"
    pts.append((o["p50"], err, kind, c))
W, H, L, B, T_ = 760, 420, 60, 50, 50
xmax, ymax = 8, 40
b = t(10, 24, "Mini A: speed vs accuracy per engine/backend (bottom-left is best)", weight="600", size=15)
for i in range(0, 5):
    x = L + (W - L - 30) * i / 4
    b += f'<line x1="{x:.0f}" y1="{T_}" x2="{x:.0f}" y2="{H-B}" stroke="{GRID}"/>' + t(x, H - B + 18, f"{xmax*i/4:.0f}s", "middle", MUTED, 11)
    y = H - B - (H - B - T_) * i / 4
    b += f'<line x1="{L}" y1="{y:.0f}" x2="{W-30}" y2="{y:.0f}" stroke="{GRID}"/>' + t(L - 8, y + 4, f"{ymax*i/4:.0f}%", "end", MUTED, 11)
b += t((L + W - 30) / 2, H - 10, "median latency per command", "middle", MUTED, 12) + t(14, T_ - 12, "word error (avg clean+noisy)", "start", MUTED, 12)
col = {"fw": C1, "cpp": C2, "gpu": C4}
for x, y, k, c in pts:
    px = L + (W - L - 30) * min(x, xmax) / xmax
    py = H - B - (H - B - T_) * min(y, ymax) / ymax
    b += f'<circle cx="{px:.0f}" cy="{py:.0f}" r="6" fill="{col[k]}" fill-opacity="0.85"><title>{c}: {x:.2f}s, {y:.0f}% error</title></circle>'
lx = W - 250
for i, (k, lab) in enumerate([("fw", "faster-whisper (CPU)"), ("cpp", "whisper.cpp (CPU)"), ("gpu", "whisper.cpp (iGPU, Vulkan)")]):
    b += f'<circle cx="{lx}" cy="{T_+10+i*20}" r="6" fill="{col[k]}"/>' + t(lx + 14, T_ + 14 + i * 20, lab, "start", TXT, 12)
open(f"{OUT}/engines-mini-a.svg", "w").write(svg(W, H, b, "Mini A: speed vs accuracy per engine and backend"))

# 3b. iGPU audio-context sweep (whisper.cpp small, beam 3, prompt)
rows = []
for ac, key in [(0, "cpp:small/gpu/ac0/b3/p1/grid"), (800, "cpp:small/gpu/ac800/b3/p1"), (600, "cpp:small/gpu/ac600/b3/p1"),
                (400, "cpp:small/gpu/ac400/b3/p1/grid"), (300, "cpp:small/gpu/ac300/b3/p1")]:
    o = g("minia", key)
    if o: rows.append((("full 30 s window" if ac == 0 else f"-ac {ac}  (max {ac*0.02:.0f} s of audio)"), o["p50"], C4, f'{o["p50"]:.1f}s   error {o["wer_clean"]:.0f}% / {o["wer_noisy"]:.0f}% (clean / noisy)'))
hbar(f"{OUT}/igpu-context.svg", "Mini A iGPU: shortening the audio context (whisper.cpp small)", rows, maxv=12, ref=(2, "2s"), left=250)

# 4. settings sweep on mini A (faster-whisper small), from the earlier full matrix (latency only, medians)
sweep = [("small beam 1", 3.65, C3), ("small (prod: beam 3, prompt)", 3.78, C1), ("small, no prompt", 4.75, C2),
         ("small, 2 threads", 4.59, C2), ("small, beam 5", 6.43, C2), ("small, fp32", 11.21, "#dc2626")]
hbar(f"{OUT}/settings-mini-a.svg", "Settings sweep on Mini A (faster-whisper small): latency", [(a, b_, c, f"{b_:.1f}s") for a, b_, c in sweep], maxv=13, ref=(2, "2s"))
print("charts ->", OUT, os.listdir(OUT))
