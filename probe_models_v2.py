import os, json, time, urllib.request, urllib.error
from concurrent.futures import ThreadPoolExecutor, as_completed
os.environ.pop('http_proxy', None); os.environ.pop('https_proxy', None)
BASE="http://192.168.100.1:12211/v1"; KEY="sk-local-12da6905686cc1cacfde9cf7387b9bdf"
OUT="probe_models_v2.json"

def fetch_models():
    req=urllib.request.Request(f"{BASE}/models", headers={"Authorization":f"Bearer {KEY}"})
    return json.load(urllib.request.urlopen(req, timeout=20)).get("data",[])

def provider_of(m):
    mid=m.get("id",""); head=mid.split("-",1)[0]
    if head in ("NVIDIA","SenseNova","auto","256k","1m","识图"): return head
    return m.get("owned_by","?")

def test_one(m):
    mid=m.get("id",""); prov=provider_of(m)
    p={"model":mid,"messages":[{"role":"user","content":"Say hello in one word."}],
       "max_tokens":256,"stream":False,"temperature":0.3}
    t0=time.time()
    try:
        req=urllib.request.Request(f"{BASE}/chat/completions", data=json.dumps(p).encode(),
            headers={"Authorization":f"Bearer {KEY}","Content-Type":"application/json"})
        with urllib.request.urlopen(req, timeout=60) as r:
            body=r.read().decode("utf-8","replace"); st=r.status
        try:
            j=json.loads(body); ch=j.get("choices",[{}])[0]
            content=(ch.get("message",{}) or {}).get("content","") or ""
            fr=ch.get("finish_reason","")
        except Exception:
            content=""; fr=""
        if st==200 and content.strip():
            cat="OK"
        elif st==200:
            cat="EMPTY"   # 200 但无可见内容（思考模型/上游异常）
        else:
            cat="FAIL"
        return {"id":mid,"provider":prov,"status":st,"cat":cat,
                "content":content[:80],"finish":fr,"latency":round(time.time()-t0,2),
                "error":"" if cat!="FAIL" else body[:200]}
    except urllib.error.HTTPError as e:
        b=e.read().decode("utf-8","replace") if e.fp else str(e)
        return {"id":mid,"provider":prov,"status":e.code,"cat":"FAIL","content":"",
                "finish":"","latency":round(time.time()-t0,2),"error":b[:200]}
    except Exception as e:
        return {"id":mid,"provider":prov,"status":0,"cat":"FAIL","content":"",
                "finish":"","latency":round(time.time()-t0,2),"error":f"{type(e).__name__}:{e}"[:200]}

def main():
    models=fetch_models()
    print(f"全量探测: {len(models)} 模型 (max_tokens=256)", flush=True)
    res=[]
    with ThreadPoolExecutor(max_workers=15) as ex:
        for f in as_completed({ex.submit(test_one,m):m for m in models}):
            r=f.result(); res.append(r)
            print(f"[{r['cat']:5}] {r['provider']:13} {r['id']}  {r['status']}  {r['latency']}s  {r['content']!r}", flush=True)
    from collections import defaultdict
    byp=defaultdict(lambda:{"OK":0,"EMPTY":0,"FAIL":0,"fails":[]})
    for r in res:
        d=byp[r["provider"]]; d[r["cat"]]+=1
        if r["cat"]!="OK":
            d["fails"].append(f"{r['id']} [{r['cat']}/{r['status']}] {r['error'][:70]}")
    print("\n===== 供应商汇总 (OK / EMPTY / FAIL) =====", flush=True)
    for p,d in sorted(byp.items(), key=lambda x:-(x[1]["OK"]+x[1]["EMPTY"]+x[1]["FAIL"])):
        print(f"{p:14} OK={d['OK']:2}  EMPTY={d['EMPTY']:2}  FAIL={d['FAIL']:2}", flush=True)
        for f in d["fails"]:
            print(f"      - {f}", flush=True)
    ok=sum(1 for r in res if r["cat"]=="OK"); emp=sum(1 for r in res if r["cat"]=="EMPTY"); fa=sum(1 for r in res if r["cat"]=="FAIL")
    print(f"\n总计: OK={ok}  EMPTY={emp}  FAIL={fa}  共 {len(res)}", flush=True)
    json.dump({"generated_at":time.strftime("%Y-%m-%dT%H:%M:%S"),"base":BASE,
               "total":len(res),"ok":ok,"empty":emp,"fail":fa,"results":res},
              open(OUT,"w",encoding="utf-8"), ensure_ascii=False, indent=2)
    print(f"写入 {OUT}", flush=True)

if __name__=="__main__":
    main()
