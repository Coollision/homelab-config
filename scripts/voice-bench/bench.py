import sys, time, json, glob, os, resource
from faster_whisper import WhisperModel
label, threads = sys.argv[1], int(sys.argv[2])
models = sys.argv[3].split(",")
reps = int(sys.argv[4])
PROMPT = "Zet het licht aan in de keuken. Zijn de luiken in de kinderkamer gesloten? Doe het rolluik van het grote raam omhoog. Zet de plafondspots in de woonkamer uit. Speel muziek in de keuken. Wat is de temperatuur buiten?"
files = [f for f in sorted(glob.glob("/audio/*.wav")) if f[-6:-4] in ("01", "03", "05", "06", "08", "10")]
out = []
for spec in models:
    name, *opts = spec.split("+")   # e.g. small+noprompt+b1+fp32+t2
    prompt = None if "noprompt" in opts else PROMPT
    beam = next((int(o[1:]) for o in opts if o[0] == "b" and o[1:].isdigit()), 3)
    ct = "float32" if "fp32" in opts else "int8"
    th = next((int(o[1:]) for o in opts if o[0] == "t" and o[1:].isdigit()), threads)
    t0 = time.time()
    m = WhisperModel(name, device="cpu", compute_type=ct, cpu_threads=th, download_root="/tmp/models")
    load = time.time() - t0
    t0 = time.time()
    m.transcribe(files[0], language="nl", beam_size=beam, initial_prompt=prompt)[0]; list(m.transcribe(files[0], language="nl", beam_size=beam, initial_prompt=prompt)[0])
    for f in files:
        lat = []
        for r in range(reps):
            t = time.time()
            segs, info = m.transcribe(f, language="nl", beam_size=beam, initial_prompt=prompt, vad_filter=False)
            text = " ".join(s.text.strip() for s in segs)
            lat.append(time.time() - t)
        out.append(dict(node=label, model=spec, file=os.path.basename(f), dur=info.duration, lat=lat, text=text, load=load))
        print("R " + json.dumps(out[-1]), flush=True)
    print(spec, "load %.1fs" % load, "median lat %.2fs" % sorted(x["lat"][0] for x in out if x["model"] == spec)[len(files)//2], "maxrss %dMB" % (resource.getrusage(resource.RUSAGE_SELF).ru_maxrss // 1024), flush=True)
    json.dump(out, open(f"/tmp/res_{label}.json", "w"))
    del m
print("DONE", flush=True)
