#!/usr/bin/env python3
"""Test modelscope directly + try /api/providers endpoint."""
import json, urllib.request, urllib.error, ssl

ctx = ssl.create_default_context()
ctx.check_hostname = False
ctx.verify_mode = ssl.CERT_NONE

# 1. Try /api/providers endpoint
print("=== /api/providers ===")
req = urllib.request.Request("http://192.168.100.1:12211/api/providers",
    headers={"Authorization": "Bearer sk-local-12da6905686cc1cacfde9cf7387b9bdf"})
try:
    with urllib.request.urlopen(req, timeout=15, context=ctx) as r:
        data = json.loads(r.read().decode())
        if isinstance(data, list):
            ps = data
        elif isinstance(data, dict):
            ps = data.get("providers", data.get("data", []))
        else:
            ps = []
        print(f"Total: {len(ps)}")
        for p in ps:
            name = p.get("name", p.get("id", "?"))
            base = p.get("base_url", "?")
            key = p.get("api_key", "")
            fmt = p.get("format", "")
            models = p.get("models", [])
            if isinstance(models, list):
                mcount = len(models)
            else:
                mcount = models
            print(f"  {name}: base={base} key={'***'+key[-4:] if key else '(empty)'} fmt={fmt} models={mcount}")
except Exception as e:
    print(f"ERROR: {e}")

# 2. Test modelscope with dummy key
print("\n=== modelscope direct test (dummy key) ===")
base = "https://api-inference.modelscope.cn/v1"
body = json.dumps({"model": "deepseek-ai/DeepSeek-V4-Flash",
    "messages": [{"role": "user", "content": "hi"}], "max_tokens": 16}).encode()

print("--- with dummy key ---")
req = urllib.request.Request(f"{base}/chat/completions", data=body,
    headers={"Authorization": "Bearer ms-dummy123456", "Content-Type": "application/json"}, method="POST")
try:
    with urllib.request.urlopen(req, timeout=30, context=ctx) as r:
        print(f"OK: {r.read().decode()[:300]}")
except urllib.error.HTTPError as e:
    print(f"HTTP {e.code}: {e.read().decode()[:500]}")
except Exception as e:
    print(f"ERROR: {e}")

print("\n--- without key ---")
req = urllib.request.Request(f"{base}/chat/completions", data=body,
    headers={"Content-Type": "application/json"}, method="POST")
try:
    with urllib.request.urlopen(req, timeout=30, context=ctx) as r:
        print(f"OK: {r.read().decode()[:300]}")
except urllib.error.HTTPError as e:
    print(f"HTTP {e.code}: {e.read().decode()[:500]}")
except Exception as e:
    print(f"ERROR: {e}")

# 3. List models with dummy key
print("\n--- /v1/models (dummy key) ---")
req = urllib.request.Request(f"{base}/models", headers={"Authorization": "Bearer ms-dummy123456"})
try:
    with urllib.request.urlopen(req, timeout=20, context=ctx) as r:
        data = json.loads(r.read().decode())
        models = [m.get("id", "") for m in data.get("data", [])]
        print(f"OK: {len(models)} models, first 10: {models[:10]}")
except urllib.error.HTTPError as e:
    print(f"HTTP {e.code}: {e.read().decode()[:500]}")
except Exception as e:
    print(f"ERROR: {e}")
