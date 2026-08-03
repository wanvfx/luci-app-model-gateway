import os, json, urllib.request, urllib.error, time
from concurrent.futures import ThreadPoolExecutor, as_completed
os.environ.pop('http_proxy', None); os.environ.pop('https_proxy', None)
BASE="http://192.168.100.1:12211/v1"; KEY="sk-local-12da6905686cc1cacfde9cf7387b9bdf"

def call(mid, max_tokens):
    p={"model":mid,"messages":[{"role":"user","content":"Say hello in one word."}],
       "max_tokens":max_tokens,"stream":False,"temperature":0.3}
    req=urllib.request.Request(f"{BASE}/chat/completions", data=json.dumps(p).encode(),
        headers={"Authorization":f"Bearer {KEY}","Content-Type":"application/json"})
    t0=time.time()
    try:
        with urllib.request.urlopen(req, timeout=60) as r:
            body=r.read().decode("utf-8","replace"); st=r.status
    except urllib.error.HTTPError as e:
        body=(e.read().decode("utf-8","replace") if e.fp else str(e)); st=e.code
    except Exception as e:
        return {"id":mid,"status":0,"ok":False,"content":"","err":f"{type(e).__name__}:{e}","latency":round(time.time()-t0,2)}
    try:
        j=json.loads(body); content=j.get("choices",[{}])[0].get("message",{}).get("content","")
        fr=j.get("choices",[{}])[0].get("finish_reason","")
    except Exception:
        content=""; fr=""
    return {"id":mid,"status":st,"ok":(st==200 and bool(content.strip())),
            "content":content[:120],"finish":fr,"latency":round(time.time()-t0,2)}

d=json.load(open("probe_models_result.json",encoding="utf-8"))
# 取第一批里 status==200 的（含空内容误判），以及几个代表性 502 复核
cands=[r["id"] for r in d["results"] if r["status"]==200]
# 代表性质疑 502
extra=["pollinations-openai","duckduckgo-web-gpt-5.4-mini","felo-web-felo-chat",
       "NVIDIA-deepseek-ai/deepseek-v4-flash","ovhcloud-Qwen3.5-9B","SenseNova-glm-5.2"]
for e in extra:
    if e not in cands: cands.append(e)
print(f"复核模型数: {len(cands)}", flush=True)

res=[]
with ThreadPoolExecutor(max_workers=15) as ex:
    futs={ex.submit(call,m,256):m for m in cands}
    for f in as_completed(futs):
        r=f.result(); res.append(r)
        print(f"[{'OK ' if r['ok'] else 'FAIL'}] {r['id']}  {r['status']}  {r['latency']}s  finish={r['finish']}  content={r['content']!r}", flush=True)

ok=[r for r in res if r["ok"]]
print(f"\n复核可用: {len(ok)}/{len(res)}", flush=True)
json.dump(res, open("probe_models_refined.json","w",encoding="utf-8"), ensure_ascii=False, indent=2)
print("写入 probe_models_refined.json", flush=True)
