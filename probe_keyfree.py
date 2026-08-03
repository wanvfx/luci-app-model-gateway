#!/usr/bin/env python3
# 复刻网关探测逻辑，对 providers_catalog.json 中所有免Key(auth!=apikey)提供者逐一实测。
import json, urllib.request, urllib.error, socket, os, ssl, time, sys

os.environ.pop("http_proxy", None); os.environ.pop("https_proxy", None)
os.environ.pop("HTTP_PROXY", None); os.environ.pop("HTTPS_PROXY", None)

cat = json.load(open("providers_catalog.json", encoding="utf-8"))
providers = cat["providers"]

# 仅免Key（auth != apikey）
free = [p for p in providers if p.get("auth") != "apikey"]

# 建立会话级 context，跳过证书校验以降低误报（探测目的）
ctx = ssl.create_unsafe_context() if hasattr(ssl,'create_unsafe_context') else ssl._create_unverified_context()

def trim(s): return s.rstrip("/")

def do(method, url, body=None, headers=None, timeout=25):
    headers = headers or {}
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, method=method, headers=headers)
    req.add_header("User-Agent", "Mozilla/5.0 (compatible; model-gateway-probe/1.0)")
    if data is not None:
        req.add_header("Content-Type", "application/json")
    t0 = time.time()
    try:
        with urllib.request.urlopen(req, timeout=timeout, context=ctx) as r:
            snip = r.read(220)
            return r.status, time.time()-t0, None, snip
    except urllib.error.HTTPError as e:
        try: snip = e.read(220)
        except: snip = b""
        return e.code, time.time()-t0, None, snip
    except Exception as e:
        return None, time.time()-t0, f"{type(e).__name__}: {str(e)[:140]}", b""

print(f"=== 免Key提供者总数: {len(free)} ===\n")
rows = []
for p in free:
    pid = p.get("id")
    base = p.get("base_url","")
    auth = p.get("auth")
    models = p.get("models",[]) or []
    first_model = models[0].get("id") if models else None
    # HealthCheck: GET {base}/models
    h_url = trim(base) + "/models"
    h_st, h_lat, h_err, h_snip = do("GET", h_url, timeout=20)
    # ChatProbe: POST {base}/chat/completions (openai default)
    c_st=c_err=c_snip=c_lat=None
    if first_model:
        c_url = trim(base) + "/chat/completions"
        c_st, c_lat, c_err, c_snip = do("POST", c_url,
            body={"model":first_model,"messages":[{"role":"user","content":"hi"}],"max_tokens":5,"stream":False},
            timeout=30)
    # 判断
    health_ok = (h_st in (200,204))
    chat_ok = (c_st == 200)
    if not first_model:
        verdict = "无模型(目录缺模型)"
    elif base.startswith("auggie://") or base.startswith("stdio") or "://cli" in base:
        verdict = "本地CLI/stdio,HTTP代理不可达(架构限制)"
    elif health_ok and chat_ok:
        verdict = "真免Key可用"
    elif health_ok and not chat_ok:
        verdict = "端点可达但/chat/completions不通(非OpenAI兼容)"
    elif (not health_ok) and chat_ok:
        verdict = "端点存活(chat通,/models不通)"
    else:
        verdict = "连不上(非OpenAI兼容/端点不存在/需特殊协议)"
    rows.append((pid, auth, base, first_model, h_st, h_err, c_st, c_err, verdict))
    print(f"[{pid}] auth={auth}")
    print(f"   base={base}")
    print(f"   Health(/models): st={h_st} err={h_err} snip={(h_snip or b'')[:80]!r}")
    print(f"   Chat(/chat/completions, model={first_model}): st={c_st} err={c_err} snip={(c_snip or b'')[:80]!r}")
    print(f"   >>> 判定: {verdict}\n")
    sys.stdout.flush()

# pollinations 重复探测，评估SLA
print("=== pollinations 重复探测(评估SLA可用性) ===")
pn_ok=0; pn_total=8
for i in range(pn_total):
    st,lat,err,snip = do("POST", "https://gen.pollinations.ai/v1/chat/completions",
        body={"model":"openai","messages":[{"role":"user","content":"hi"}],"max_tokens":5,"stream":False}, timeout=30)
    ok = (st==200)
    pn_ok += 1 if ok else 0
    print(f"  probe#{i+1}: st={st} err={err} snip={snip[:60]!r}")
    sys.stdout.flush()
print(f"  pollinations 可用率 ≈ {pn_ok}/{pn_total} = {pn_ok/pn_total*100:.1f}%\n")

print("=== 汇总判定 ===")
for r in rows:
    print(f"  {r[0]:18s} {r[8]}")
