import os, json, time, urllib.request, urllib.error
from concurrent.futures import ThreadPoolExecutor, as_completed

os.environ.pop('http_proxy', None); os.environ.pop('https_proxy', None)

BASE = "http://192.168.100.1:12211/v1"
KEY = "sk-local-12da6905686cc1cacfde9cf7387b9bdf"
OUT = "probe_models_result.json"

def http_post(url, payload, timeout):
    req = urllib.request.Request(url, data=json.dumps(payload).encode(),
                                 headers={"Authorization": f"Bearer {KEY}",
                                          "Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return r.status, r.read().decode("utf-8", "replace")

def fetch_models():
    req = urllib.request.Request(f"{BASE}/models",
                                 headers={"Authorization": f"Bearer {KEY}"})
    return json.load(urllib.request.urlopen(req, timeout=20)).get("data", [])

def provider_of(m):
    mid = m.get("id", "")
    head = mid.split("-", 1)[0]
    if head in ("NVIDIA", "SenseNova", "auto", "256k", "1m", "识图"):
        return head
    return m.get("owned_by", "?")

def test_one(m):
    mid = m.get("id", "")
    payload = {
        "model": mid,
        "messages": [{"role": "user", "content": "ping"}],
        "max_tokens": 8,
        "stream": False,
        "temperature": 0,
    }
    t0 = time.time()
    try:
        status, body = http_post(f"{BASE}/chat/completions", payload, timeout=50)
        dt = round(time.time() - t0, 2)
        ok = False
        err = ""
        try:
            j = json.loads(body)
            content = j.get("choices", [{}])[0].get("message", {}).get("content", "")
            if status == 200 and content:
                ok = True
            else:
                err = (j.get("error", {}) or {}).get("message", "") or body[:200]
        except Exception as e:
            ok = False
            err = f"parse_fail:{e} | {body[:200]}"
        return {"id": mid, "provider": provider_of(m), "status": status,
                "ok": ok, "latency": dt, "error": err[:300]}
    except urllib.error.HTTPError as e:
        return {"id": mid, "provider": provider_of(m), "status": e.code,
                "ok": False, "latency": round(time.time() - t0, 2),
                "error": (e.read().decode("utf-8", "replace")[:300] if e.fp else str(e))}
    except Exception as e:
        return {"id": mid, "provider": provider_of(m), "status": 0,
                "ok": False, "latency": round(time.time() - t0, 2),
                "error": f"{type(e).__name__}: {e}"[:300]}

def main():
    models = fetch_models()
    print(f"待探测模型数: {len(models)}", flush=True)
    results = []
    with ThreadPoolExecutor(max_workers=15) as ex:
        futs = {ex.submit(test_one, m): m for m in models}
        done = 0
        for f in as_completed(futs):
            r = f.result()
            results.append(r)
            done += 1
            tag = "OK " if r["ok"] else "FAIL"
            print(f"[{done}/{len(models)}] {tag} {r['provider']:12} {r['id']}  {r['status']}  {r['latency']}s  {r['error']}", flush=True)
    # summary
    from collections import defaultdict
    byp = defaultdict(lambda: {"total": 0, "ok": 0, "fails": []})
    for r in results:
        d = byp[r["provider"]]
        d["total"] += 1
        if r["ok"]:
            d["ok"] += 1
        else:
            d["fails"].append(f"{r['id']} ({r['status']}: {r['error'][:60]})")
    print("\n===== 供应商汇总 =====", flush=True)
    for p, d in sorted(byp.items(), key=lambda x: -x[1]["total"]):
        print(f"{p:14} {d['ok']}/{d['total']} 可用", flush=True)
        for f in d["fails"]:
            print(f"     ✗ {f}", flush=True)
    total_ok = sum(1 for r in results if r["ok"])
    print(f"\n总计: {total_ok}/{len(results)} 可用", flush=True)
    json.dump({"generated_at": time.strftime("%Y-%m-%dT%H:%M:%S"),
               "base": BASE, "total": len(results), "ok": total_ok,
               "results": results}, open(OUT, "w", encoding="utf-8"),
              ensure_ascii=False, indent=2)
    print(f"结果已写入 {OUT}", flush=True)

if __name__ == "__main__":
    main()
