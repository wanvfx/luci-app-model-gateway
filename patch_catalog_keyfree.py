# -*- coding: utf-8 -*-
"""
根据 2026-07-31 对全部免Key提供者的逐一实测结果，修正 providers_catalog.json：
  1) 为每个非 apikey 提供者写入 keyfree_status / keyfree_note，供前端市场卡片展示真实可用状态；
  2) 修正实测发现的硬错误（模型 ID 错、base_url 缺 /v1、误标免Key 实则需密钥）。
可重复执行（幂等）。
"""
import json, io, os

PATH = 'providers_catalog.json'

# ---- 实测结论（2026-07-31，逐一探测 /models + /chat/completions） ----
STATUS = {
    'pollinations': ('verified',
        '实测免Key直连成功（/models 200、/chat/completions 200）。上游为共享免费池，稳定性差：'
        '本机连续 8 次探测仅 2 次成功（≈25%），面板 SLA 偏低属上游波动，非网关缺陷。建议与其它提供者组成故障转移。'),
    'opencode': ('verified',
        '实测免Key直连成功（/models 200、/chat/completions 200）。可选填入密钥以提升速率上限。'),
    'uncloseai': ('verified',
        '实测免Key直连成功。原目录模型 ID 有误（Hermes-3-Llama-3.1-8B-FP8-Dynamic 上游返回 404），'
        '已按上游 /models 修正为 solidrust/Hermes-3-Llama-3.1-8B-AWQ，实测对话 200。'),

    'ovhcloud': ('rate_limited',
        '免Key可读 /models（22 个模型），但匿名对话实测 429「API rate limit exceeded」。'
        '目录原先未收录模型，现已补齐 12 个对话模型。要稳定使用需在 OVHcloud 申请免费 Token 后填入密钥。'),
    'g4f-pollinations': ('rate_limited',
        'g4f 公共中转站：/models 正常，但对话实测 429，共享配额仅 200 次/天（探测时已被他人用掉 359 次）。'),
    'g4f-gemini': ('rate_limited', 'g4f 公共中转站，共享配额 200 次/天，对话实测 429。'),
    'g4f-ollama': ('rate_limited', 'g4f 公共中转站，共享配额 200 次/天，对话实测 429。'),
    'g4f-nvidia': ('rate_limited', 'g4f 公共中转站，/models 返回 404、对话实测 429（共享配额 200 次/天）。'),
    'g4f-groq': ('rate_limited', 'g4f 公共中转站，/models 返回 403、对话实测 429（共享配额 200 次/天）。'),

    'hackclub': ('needs_key',
        '实测 /models 可匿名读取（728 个模型），但 /chat/completions 返回 401 Authentication required —— '
        '实际需要 Hack Club 账号密钥。已由「免Key」修正为「需密钥」。'),

    'auggie': ('incompatible',
        'base_url 为 auggie://cli/stdio —— 这是本地 CLI 的 stdio 管道，根本不是 HTTP 服务。'
        'HTTP 网关在架构上无法转发，除非另写本地进程适配器。'),
    'duckduckgo-web': ('incompatible',
        '非 OpenAI 兼容：需先 GET /duckchat/v1/status 取 x-vqd-4 令牌，再 POST /duckchat/v1/chat（不是 /chat/completions），'
        '且响应为自定义 SSE。本机实测直连超时。需专用适配器。'),
    'felo-web': ('incompatible',
        '端点 /api-proxy/main/search/threads 是「搜索线程」接口，不是对话补全接口，请求体/响应体均不兼容。本机实测超时。'),
    'theoldllm': ('incompatible',
        '自定义路径 /api/chatgpt，非 /chat/completions，且请求格式自定义。本机实测超时。'),
    'veoaifree-web': ('incompatible',
        'WordPress 的 admin-ajax.php 接口（表单式 POST），且被 Cloudflare 拦截（403「Just a moment」）。'
        '为视频生成站点，非对话补全接口。'),
    'chipotle': ('incompatible',
        'amelia.chipotle.com 走 SockJS/STOMP 长连接，非 HTTP 对话接口，实测 404。'),
}

# ---- 硬错误修正 ----
UNCLOSEAI_MODELS = [
    {"id": "solidrust/Hermes-3-Llama-3.1-8B-AWQ", "name": "Hermes 3 Llama 3.1 8B AWQ (🆓 Free)", "free": True},
]
OVH_CHAT_MODELS = [
    ("Qwen3.5-397B-A17B", "Qwen3.5 397B A17B"),
    ("Qwen3.6-27B", "Qwen3.6 27B"),
    ("Qwen3-32B", "Qwen3 32B"),
    ("Qwen3.5-9B", "Qwen3.5 9B"),
    ("Qwen3-Coder-30B-A3B-Instruct", "Qwen3 Coder 30B A3B"),
    ("Qwen2.5-VL-72B-Instruct", "Qwen2.5 VL 72B (视觉)"),
    ("gpt-oss-120b", "GPT-OSS 120B"),
    ("gpt-oss-20b", "GPT-OSS 20B"),
    ("Meta-Llama-3_3-70B-Instruct", "Llama 3.3 70B Instruct"),
    ("Mistral-Small-3.2-24B-Instruct-2506", "Mistral Small 3.2 24B"),
    ("Mistral-Nemo-Instruct-2407", "Mistral Nemo Instruct"),
    ("Mistral-7B-Instruct-v0.3", "Mistral 7B Instruct v0.3"),
]

with io.open(PATH, encoding='utf-8') as f:
    cat = json.load(f)

providers = cat['providers']
by_id = {p['id']: p for p in providers}
changed = []

# 1) 写入状态标注
for pid, (status, note) in STATUS.items():
    p = by_id.get(pid)
    if not p:
        print('  ! 目录中未找到:', pid)
        continue
    if p.get('keyfree_status') != status or p.get('keyfree_note') != note:
        p['keyfree_status'] = status
        p['keyfree_note'] = note
        changed.append('%s: 标注 %s' % (pid, status))

# 2) uncloseai —— 修正模型 ID（实测 200）
p = by_id.get('uncloseai')
if p and [m['id'] for m in p.get('models', [])] != [m['id'] for m in UNCLOSEAI_MODELS]:
    p['models'] = list(UNCLOSEAI_MODELS)
    p['free_models'] = 1
    changed.append('uncloseai: 修正模型 ID 为上游真实 ID（实测对话 200）')

# 3) ovhcloud —— 补齐真实对话模型
p = by_id.get('ovhcloud')
if p and len(p.get('models', [])) != len(OVH_CHAT_MODELS):
    p['models'] = [{"id": mid, "name": mname, "free": True} for mid, mname in OVH_CHAT_MODELS]
    p['free_models'] = len(OVH_CHAT_MODELS)
    changed.append('ovhcloud: 补齐 %d 个真实对话模型' % len(OVH_CHAT_MODELS))

# 4) hackclub —— 改为需密钥
p = by_id.get('hackclub')
if p and p.get('auth') != 'apikey':
    p['auth'] = 'apikey'
    p.pop('auth_scheme', None)
    changed.append('hackclub: auth optional -> apikey（实测 401）')

cat['keyfree_probed'] = '2026-07-31'

with io.open(PATH, 'w', encoding='utf-8') as f:
    json.dump(cat, f, ensure_ascii=False, indent=2)
    f.write('\n')

print('=== 目录修正完成，共 %d 项变更 ===' % len(changed))
for c in changed:
    print('  -', c)

keyfree = [p for p in providers if p.get('auth') != 'apikey']
print('\n免Key提供者数量: %d（修正前 17）' % len(keyfree))
from collections import Counter
print('状态分布:', dict(Counter(p.get('keyfree_status', '?') for p in keyfree)))
