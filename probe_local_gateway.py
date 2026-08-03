#!/usr/bin/env python3
"""Probe all models through local gateway: fetch /v1/models then chat-completion each."""
import json
import time
import urllib.request
import urllib.error
import ssl
import concurrent.futures
import sys

GATEWAY = "http://192.168.100.1:12211"
API_KEY = "sk-local-12da6905686cc1cacfde9cf7387b9bdf"
PROMPT = "hi"
MAX_TOKENS = 256
TIMEOUT = 60
CONCURRENCY = 8

ctx = ssl.create_default_context()
ctx.check_hostname = False
ctx.verify_mode = ssl.CERT_NONE


def api_get(path):
    req = urllib.request.Request(
        f"{GATEWAY}{path}",
        headers={"Authorization": f"Bearer {API_KEY}"},
    )
    try:
        with urllib.request.urlopen(req, timeout=TIMEOUT, context=ctx) as r:
            return json.loads(r.read().decode())
    except Exception as e:
        print(f"GET {path} failed: {e}", file=sys.stderr)
        return {}


def api_chat(model):
    body = json.dumps({
        "model": model,
        "messages": [{"role": "user", "content": PROMPT}],
        "max_tokens": MAX_TOKENS,
        "stream": False,
    }).encode()
    req = urllib.request.Request(
        f"{GATEWAY}/v1/chat/completions",
        data=body,
        headers={
            "Authorization": f"Bearer {API_KEY}",
            "Content-Type": "application/json",
        },
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=TIMEOUT, context=ctx) as r:
            raw = r.read().decode()
            data = json.loads(raw)
            content = ""
            choices = data.get("choices", [])
            if choices:
                msg = choices[0].get("message", {})
                content = msg.get("content", "") or ""
            return {"status": r.status, "ok": True, "empty": len(content.strip()) == 0, "content": content[:200], "raw": raw[:500]}
    except urllib.error.HTTPError as e:
        raw = e.read().decode() if e.fp else ""
        try:
            j = json.loads(raw)
            err = j.get("error", {}).get("message", raw[:200])
        except Exception:
            err = raw[:200] or str(e)
        return {"status": e.code, "ok": False, "empty": False, "content": "", "error": err, "raw": raw[:500]}
    except Exception as e:
        return {"status": 0, "ok": False, "empty": False, "content": "", "error": str(e), "raw": ""}


def main():
    print("Fetching model list from gateway ...")
    models_data = api_get("/v1/models")
    models = []
    if models_data and "data" in models_data:
        models = [m.get("id", "") for m in models_data["data"] if m.get("id")]
    if not models:
        print("No models from /v1/models, abort")
        sys.exit(1)
    print(f"Total models to probe: {len(models)}")

    results = {"ok": [], "empty": [], "fail": []}
    start = time.time()

    def probe(model):
        return model, api_chat(model)

    with concurrent.futures.ThreadPoolExecutor(max_workers=CONCURRENCY) as pool:
        futures = {pool.submit(probe, m): m for m in models}
        done = 0
        for fut in concurrent.futures.as_completed(futures):
            model, res = fut.result()
            done += 1
            if res.get("ok"):
                if res.get("empty"):
                    results["empty"].append(model)
                else:
                    results["ok"].append(model)
                    print(f"[{done}/{len(models)}] OK {model} -> {res.get('content','')[:80]}")
            else:
                results["fail"].append((model, res.get("status"), res.get("error", res.get("raw", ""))[:120]))
                print(f"[{done}/{len(models)}] FAIL {model} -> {res.get('status')} {res.get('error','')[:100]}")

    elapsed = time.time() - start
    print(f"\n=== Result ({elapsed:.1f}s) ===")
    print(f"OK: {len(results['ok'])}")
    print(f"EMPTY: {len(results['empty'])}")
    print(f"FAIL: {len(results['fail'])}")
    if results["empty"]:
        print("\n--- EMPTY (try larger max_tokens) ---")
        for m in results["empty"]:
            print(f"  - {m}")
    if results["fail"]:
        print("\n--- FAIL details ---")
        for m, code, err in results["fail"]:
            print(f"  - {m}: {code} {err}")

    out = {
        "gateway": GATEWAY,
        "timestamp": time.strftime("%Y-%m-%d %H:%M:%S"),
        "elapsed_sec": round(elapsed, 1),
        "total": len(models),
        "ok": len(results["ok"]),
        "empty": len(results["empty"]),
        "fail": len(results["fail"]),
        "ok_list": results["ok"],
        "empty_list": results["empty"],
        "fail_list": [{"model": m, "status": c, "error": e} for m, c, e in results["fail"]],
    }
    with open("probe_local_gateway.json", "w", encoding="utf-8") as f:
        json.dump(out, f, ensure_ascii=False, indent=2)
    print("\nSaved to probe_local_gateway.json")


if __name__ == "__main__":
    main()
