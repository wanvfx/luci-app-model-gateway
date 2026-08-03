# -*- coding: utf-8 -*-
"""侦察 B 类免Key 提供者真实协议：duckduckgo-web / theoldllm / felo-web / chipotle"""
import json
import sys
import urllib.request
import urllib.error
import ssl

TIMEOUT = 20
ctx = ssl.create_default_context()
ctx.check_hostname = False
ctx.verify_mode = ssl.CERT_NONE

UA = ("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
      "(KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")


def req(method, url, headers=None, body=None, label=""):
    h = {"User-Agent": UA, "Accept": "*/*"}
    if headers:
        h.update(headers)
    data = None
    if body is not None:
        data = body if isinstance(body, bytes) else json.dumps(body).encode("utf-8")
        h.setdefault("Content-Type", "application/json")
    r = urllib.request.Request(url, data=data, headers=h, method=method)
    print("=" * 70)
    print(f"[{label}] {method} {url}")
    if body is not None:
        print("  req body:", (json.dumps(body)[:300] if not isinstance(body, bytes) else body[:300]))
    try:
        with urllib.request.urlopen(r, timeout=TIMEOUT, context=ctx) as resp:
            raw = resp.read(4000)
            print(f"  -> HTTP {resp.status}")
            for k, v in resp.headers.items():
                if k.lower().startswith("x-") or k.lower() in ("content-type", "set-cookie"):
                    print(f"     {k}: {v[:200]}")
            print("  body[:1200]:", raw[:1200].decode("utf-8", "replace"))
            return resp.status, dict(resp.headers), raw
    except urllib.error.HTTPError as e:
        raw = e.read(3000)
        print(f"  -> HTTPError {e.code}")
        for k, v in e.headers.items():
            if k.lower().startswith("x-") or k.lower() in ("content-type",):
                print(f"     {k}: {v[:200]}")
        print("  body[:900]:", raw[:900].decode("utf-8", "replace"))
        return e.code, dict(e.headers), raw
    except Exception as e:
        print(f"  -> EXC {type(e).__name__}: {e}")
        return None, {}, b""


def probe_ddg():
    print("\n\n########## duckduckgo-web ##########")
    st, hdr, _ = req("GET", "https://duckduckgo.com/duckchat/v1/status",
                     {"x-vqd-accept": "1", "Referer": "https://duckduckgo.com/",
                      "Origin": "https://duckduckgo.com"}, None, "ddg-status")
    vqd = None
    for k, v in (hdr or {}).items():
        if k.lower() in ("x-vqd-4", "x-vqd-hash-1"):
            vqd = v
            print(f"  !! token header {k} = {v[:120]}")
    req("GET", "https://duckduckgo.com/duckchat/v1/models",
        {"Referer": "https://duckduckgo.com/"}, None, "ddg-models")
    if vqd:
        req("POST", "https://duckduckgo.com/duckchat/v1/chat",
            {"x-vqd-4": vqd, "Referer": "https://duckduckgo.com/",
             "Origin": "https://duckduckgo.com", "Accept": "text/event-stream"},
            {"model": "gpt-4o-mini", "messages": [{"role": "user", "content": "hi"}]},
            "ddg-chat")


def probe_theoldllm():
    print("\n\n########## theoldllm ##########")
    req("GET", "https://theoldllm.vercel.app/", None, None, "told-root")
    req("GET", "https://theoldllm.vercel.app/api/models", None, None, "told-models")
    for body in (
        {"message": "hi", "model": "GPT_5"},
        {"messages": [{"role": "user", "content": "hi"}], "model": "GPT_5"},
        {"prompt": "hi", "model": "GPT_5"},
    ):
        req("POST", "https://theoldllm.vercel.app/api/chatgpt", None, body, "told-chat")


def probe_felo():
    print("\n\n########## felo-web ##########")
    req("POST", "https://felo.ai/api/search/threads",
        {"Accept": "text/event-stream", "Referer": "https://felo.ai/"},
        {"query": "hi", "search_uuid": "0123456789abcdef0123456789abcdef",
         "search_options": {"langcode": "zh-CN"}, "search_video": False},
        "felo-threads")
    req("POST", "https://felo.ai/api-proxy/main/search/threads",
        {"Accept": "text/event-stream", "Referer": "https://felo.ai/"},
        {"query": "hi", "search_uuid": "0123456789abcdef0123456789abcdef",
         "search_options": {"langcode": "zh-CN"}, "search_video": False},
        "felo-proxy-threads")


def probe_chipotle():
    print("\n\n########## chipotle ##########")
    req("GET", "https://amelia.chipotle.com/info", None, None, "chip-sockjs-info")
    req("GET", "https://amelia.chipotle.com/chat/info", None, None, "chip-chat-info")


if __name__ == "__main__":
    which = sys.argv[1] if len(sys.argv) > 1 else "all"
    if which in ("all", "ddg"):
        probe_ddg()
    if which in ("all", "told"):
        probe_theoldllm()
    if which in ("all", "felo"):
        probe_felo()
    if which in ("all", "chip"):
        probe_chipotle()
