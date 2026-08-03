#!/usr/bin/env python3
"""Test pollinations directly without key to see if free models work."""
import json, urllib.request, urllib.error, ssl

ctx = ssl.create_default_context()
ctx.check_hostname = False
ctx.verify_mode = ssl.CERT_NONE

base = "https://gen.pollinations.ai/v1"

# Test a few free models without key
free_models = ["openai", "openai-fast", "mistral", "deepseek", "qwen-coder"]

for model in free_models:
    body = json.dumps({"model": model,
        "messages": [{"role": "user", "content": "hi"}],
        "max_tokens": 16, "stream": False}).encode()
    req = urllib.request.Request(f"{base}/chat/completions", data=body,
        headers={"Content-Type": "application/json"}, method="POST")
    try:
        with urllib.request.urlopen(req, timeout=30, context=ctx) as r:
            data = json.loads(r.read().decode())
            content = ""
            choices = data.get("choices", [])
            if choices:
                content = choices[0].get("message", {}).get("content", "")[:80]
            print(f"  {model}: OK -> {content}")
    except urllib.error.HTTPError as e:
        raw = e.read().decode()[:200]
        print(f"  {model}: HTTP {e.code} -> {raw}")
    except Exception as e:
        print(f"  {model}: ERROR -> {e}")

# Also test with an invalid key to see if it blocks free models
print("\n--- with invalid key 'invalid123' ---")
for model in ["openai", "mistral"]:
    body = json.dumps({"model": model,
        "messages": [{"role": "user", "content": "hi"}],
        "max_tokens": 16, "stream": False}).encode()
    req = urllib.request.Request(f"{base}/chat/completions", data=body,
        headers={"Authorization": "Bearer invalid123", "Content-Type": "application/json"}, method="POST")
    try:
        with urllib.request.urlopen(req, timeout=30, context=ctx) as r:
            data = json.loads(r.read().decode())
            content = ""
            choices = data.get("choices", [])
            if choices:
                content = choices[0].get("message", {}).get("content", "")[:80]
            print(f"  {model}: OK -> {content}")
    except urllib.error.HTTPError as e:
        raw = e.read().decode()[:200]
        print(f"  {model}: HTTP {e.code} -> {raw}")
    except Exception as e:
        print(f"  {model}: ERROR -> {e}")
