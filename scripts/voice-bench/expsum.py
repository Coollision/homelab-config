import sys, json, re, statistics as st, collections
ref = {l.split("|")[0]: l.strip().split("|")[1] for l in open("phrases.txt")}
def norm(s): return re.sub(r"[^a-z0-9 ]", "", s.lower().replace("-", " ")).split()
def wer(r, h):
    r, h = norm(r), norm(h); d = list(range(len(h)+1))
    for i in range(1, len(r)+1):
        p, d[0] = d[0], i
        for j in range(1, len(h)+1): p, d[j] = d[j], min(d[j]+1, d[j-1]+1, p + (r[i-1] != h[j-1]))
    return d[len(h)] / len(r)
g = collections.defaultdict(list)
for fn in sys.argv[1:]:
    for l in open(fn):
        c, f, t, txt = l.rstrip("\n").split("\t")
        try: txt = json.loads(txt)["text"]
        except Exception: txt = ""
        g[c].append((f, float(t), wer(ref[f[-6:-4]], txt), "noisy" in f))
for c, rs in g.items():
    lat = [r[1] for r in rs]; fast = sum(1 for x in lat if x < 3)
    cl = [r[2] for r in rs if not r[3]]; no = [r[2] for r in rs if r[3]]
    print("%-36s n=%2d p50=%5.2fs min=%5.2f max=%5.2f fast(<3s)=%2d/%d  WER clean=%3.0f%% noisy=%3.0f%%" % (c, len(rs), st.median(lat), min(lat), max(lat), fast, len(rs), 100*st.mean(cl), 100*st.mean(no)))
