import json, re, sys, statistics as st, collections
ref = {l.split("|")[0]: l.strip().split("|")[1] for l in open("phrases.txt")}
def norm(s): return re.sub(r"[^a-z0-9 ]", "", s.lower().replace("-", " ")).split()
def wer(r, h):
    r, h = norm(r), norm(h)
    d = list(range(len(h)+1))
    for i in range(1, len(r)+1):
        p, d[0] = d[0], i
        for j in range(1, len(h)+1):
            p, d[j] = d[j], min(d[j]+1, d[j-1]+1, p + (r[i-1] != h[j-1]))
    return d[len(h)] / len(r), len(r)
rows = []  # (node, config, cond, id, lat, wer)
for n in ["vm","pi","minia","minib","nas"]:
    try: data = [json.loads(l[2:]) for l in open(f"fw_{n}.log") if l.startswith("R ")]
    except Exception: continue
    for x in data:
        i = x["file"][-6:-4]; cond = "noisy" if x["file"].startswith("noisy") else "clean"
        rows.append((n, "fw:"+x["model"], cond, i, x["lat"][0], wer(ref[i], x["text"])[0]))
import os
for node in ["minia","minib","pi","vm","nas"]:
  fn = f"wcpp_{node}.tsv"
  if not os.path.exists(fn): continue
  try:
    for l in open(fn):
        c, f, t, txt = l.rstrip("\n").split("\t")
        i = f[-6:-4]; cond = "noisy" if f.startswith("noisy") else "clean"
        try: txt = json.loads(txt)["text"]
        except Exception: txt = ""
        if float(t) < 0.05 or not txt.strip(): continue   # crashed-server rows
        rows.append((node, "cpp:"+c, cond, i, float(t), wer(ref[i], txt)[0]))
  except Exception as e: print("skip", fn, e)
g = collections.defaultdict(list)
for r in rows: g[(r[0], r[1])].append(r)
out = []
for (n, c), rs in g.items():
    lat = [r[4] for r in rs]; cl = [r[5] for r in rs if r[2]=="clean"]; no = [r[5] for r in rs if r[2]=="noisy"]
    out.append(dict(node=n, cfg=c, n=len(rs), p50=st.median(lat), p95=max(lat) if len(lat)<20 else sorted(lat)[int(.95*len(lat))],
      wer_clean=100*st.mean(cl) if cl else None, wer_noisy=100*st.mean(no) if no else None))
json.dump(out, open("res/summary.json","w"), indent=1)
for o in sorted(out, key=lambda o:(o["node"], o["cfg"])):
    print("%-7s %-34s n=%2d p50=%6.2fs max=%6.2fs WERclean=%s WERnoisy=%s" % (o["node"], o["cfg"], o["n"], o["p50"], o["p95"], "%.0f%%"%o["wer_clean"] if o["wer_clean"] is not None else "-", "%.0f%%"%o["wer_noisy"] if o["wer_noisy"] is not None else "-"))
