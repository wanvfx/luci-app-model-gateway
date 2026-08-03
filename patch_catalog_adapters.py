# -*- coding: utf-8 -*-
"""Phase C：把 3 家非 OpenAI 兼容的免 Key 提供者切到通用协议适配器；
把架构上确实做不到的 3 家如实标红。"""
import json
import shutil
from pathlib import Path

SRC = Path(__file__).parent / "providers_catalog.json"
shutil.copy(SRC, str(SRC) + ".bak-adapters")

PATCH = {
    "duckduckgo-web": {
        "base_url": "https://duckduckgo.com/duckchat/v1",
        "format": "duckduckgo",
        "keyfree_status": "adapter",
        "keyfree_note": (
            "已内置协议适配器：网关自动完成 x-vqd-4 令牌握手 → POST /chat → 自定义 SSE 转 OpenAI 流。"
            "若上游升级为浏览器端计算的 x-vqd-hash-1，握手会失败（属站点反自动化，非网关缺陷）。"
            "添加后请在「提供者」页点检测确认。"
        ),
    },
    "theoldllm": {
        "base_url": "https://theoldllm.vercel.app",
        "format": "theoldllm",
        "keyfree_status": "adapter",
        "keyfree_note": (
            "已内置协议适配器：自定义路径 /api/chatgpt，网关按声明式规格转换请求/响应。"
            "请求字段（message/model/history）与响应字段（response）依站点前端形态推定，"
            "如实测不符，改数据目录 adapters.json 即可自行修正，无需等新版本。"
        ),
    },
    "felo-web": {
        "base_url": "https://felo.ai",
        "format": "felo",
        "keyfree_status": "adapter",
        "keyfree_note": (
            "已内置协议适配器：POST /api/search/threads，上游推「累计全文」，网关自动差分成增量流。"
            "注意它本质是联网搜索问答（自带检索与引用），不等同通用对话模型，"
            "不支持系统提示词与严格多轮上下文。"
        ),
    },
    "chipotle": {
        "keyfree_status": "incompatible",
        "keyfree_note": (
            "架构不兼容：amelia.chipotle.com 走 SockJS/STOMP WebSocket 长连接，非 HTTP 对话接口。"
            "2026-07-31 实测 /info 与 /chat/info 均返回 404（Azure 网关），端点已不存在。"
            "且它是快餐店客服机器人而非通用 LLM，无接入价值。"
        ),
    },
    "auggie": {
        "keyfree_status": "incompatible",
        "keyfree_note": (
            "架构不兼容：base_url 为 auggie://cli/stdio —— 本地 CLI 的 stdio 管道，根本不是 HTTP 服务。"
            "HTTP 网关无法转发，除非另写本地进程适配器（需在路由器上装 Auggie CLI 并登录，超出网关范畴）。"
        ),
    },
    "veoaifree-web": {
        "keyfree_status": "incompatible",
        "keyfree_note": (
            "架构不兼容：WordPress admin-ajax.php 表单接口，且被 Cloudflare 拦截（403「Just a moment」）。"
            "同时它是视频生成站点，不提供对话补全，与本网关的 OpenAI Chat 语义不匹配。"
        ),
    },
}

data = json.loads(SRC.read_text(encoding="utf-8"))
provs = data["providers"] if isinstance(data, dict) and "providers" in data else data

hit = 0
for p in provs:
    pid = p.get("id")
    if pid in PATCH:
        p.update(PATCH[pid])
        hit += 1
        print(f"  patched {pid}: format={p.get('format','-')} status={p['keyfree_status']}")

assert hit == len(PATCH), f"only patched {hit}/{len(PATCH)}"
SRC.write_text(json.dumps(data, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
print(f"OK: {hit} providers patched, total {len(provs)}")
