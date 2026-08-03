import json, shutil, os

CATALOG = "providers_catalog.json"
BUILD_ROOT_CATALOG = "ipk-build/ipk-build-root/usr/share/model-gateway/providers_catalog.json"

NEW_PROVIDERS = [
    {
        "id": "openvecta",
        "name": "OpenVecta",
        "base_url": "https://api.openvecta.com/v1",
        "auth": "apikey",
        "free_models": 0,
        "models": [
            {"id": "glm-4.7-flash", "name": "GLM 4.7 Flash"},
            {"id": "claude-sonnet-4.6", "name": "Claude Sonnet 4.6"},
            {"id": "deepseek-v4-flash", "name": "DeepSeek V4 Flash"},
            {"id": "gpt-oss-120b", "name": "GPT OSS 120B"},
            {"id": "gemma-4-31b", "name": "Gemma 4 31B"},
            {"id": "kimi-k2.6", "name": "Kimi K2.6"},
            {"id": "llama-3.3-70b-instruct", "name": "Llama 3.3 70B Instruct"},
            {"id": "llama-4-maverick", "name": "Llama 4 Maverick"},
            {"id": "nemotron-3-super-120b", "name": "Nemotron 3 Super 120B"},
        ],
        "keyfree_status": "needs_key",
        "keyfree_note": "OpenAI 兼容网关，需 Bearer Key。",
    },
    {
        "id": "ainative",
        "name": "AINative",
        "base_url": "https://api.ainative.studio/api/v1",
        "auth": "apikey",
        "free_models": 0,
        "models": [
            {"id": "qwen3-235b-cerebras", "name": "Qwen3 235B (Cerebras)"},
            {"id": "qwen3-32b", "name": "Qwen3 32B"},
            {"id": "qwen3-14b", "name": "Qwen3 14B"},
            {"id": "qwen3-8b", "name": "Qwen3 8B"},
            {"id": "llama-4-maverick", "name": "Llama 4 Maverick"},
            {"id": "llama3.1-8b-cerebras", "name": "Llama 3.1 8B (Cerebras)"},
            {"id": "deepseek-r1", "name": "DeepSeek R1"},
            {"id": "nous-coder", "name": "Nous Coder"},
            {"id": "gemini-flash", "name": "Gemini Flash"},
        ],
        "keyfree_status": "needs_key",
        "keyfree_note": "OpenAI 兼容聚合器，公开 /models 84 模型，需 Bearer Key。",
    },
    {
        "id": "aion",
        "name": "Aion Labs",
        "base_url": "https://api.aionlabs.ai/v1",
        "auth": "apikey",
        "free_models": 0,
        "models": [
            {"id": "aion-labs/aion-3.0", "name": "Aion 3.0"},
            {"id": "aion-labs/aion-3.0-mini", "name": "Aion 3.0 Mini"},
            {"id": "aion-labs/aion-2.5", "name": "Aion 2.5"},
            {"id": "aion-labs/aion-2.0", "name": "Aion 2.0"},
            {"id": "aion-labs/aion-rp-llama-3.1-8b", "name": "Aion RP Llama 3.1 8B"},
        ],
        "keyfree_status": "needs_key",
        "keyfree_note": "免费层 20k tokens/day（无卡注册），需 Bearer Key。",
    },
    {
        "id": "agnes",
        "name": "Agnes",
        "base_url": "https://apihub.agnes-ai.com/v1",
        "auth": "apikey",
        "free_models": 0,
        "models": [
            {"id": "agnes-2.0-flash", "name": "Agnes 2.0 Flash"},
            {"id": "agnes-1.5-flash", "name": "Agnes 1.5 Flash"},
        ],
        "keyfree_status": "needs_key",
        "keyfree_note": "OpenAI 兼容，需 Bearer Key。",
    },
    {
        "id": "siliconflow",
        "name": "SiliconFlow",
        "base_url": "https://api.siliconflow.com/v1",
        "auth": "apikey",
        "free_models": 0,
        "models": [
            {"id": "deepseek-ai/DeepSeek-V3.2", "name": "DeepSeek V3.2"},
            {"id": "deepseek-ai/DeepSeek-V3.2-Exp", "name": "DeepSeek V3.2 Exp"},
            {"id": "deepseek-ai/DeepSeek-V3.1", "name": "DeepSeek V3.1"},
            {"id": "deepseek-ai/DeepSeek-V3.1-Terminus", "name": "DeepSeek V3.1 Terminus"},
            {"id": "deepseek-ai/DeepSeek-V3", "name": "DeepSeek V3"},
            {"id": "deepseek-ai/DeepSeek-R1", "name": "DeepSeek R1"},
            {"id": "deepseek-ai/deepseek-vl2", "name": "DeepSeek VL2"},
            {"id": "Qwen/Qwen3.6-35B-A3B", "name": "Qwen 3.6 35B A3B"},
            {"id": "Qwen/Qwen3.6-27B", "name": "Qwen 3.6 27B"},
            {"id": "Qwen/Qwen3.5-397B-A17B", "name": "Qwen 3.5 397B A17B"},
            {"id": "Qwen/Qwen3.5-122B-A10B", "name": "Qwen 3.5 122B A10B"},
            {"id": "Qwen/Qwen3.5-35B-A3B", "name": "Qwen 3.5 35B A3B"},
            {"id": "Qwen/Qwen3.5-27B", "name": "Qwen 3.5 27B"},
            {"id": "Qwen/Qwen3.5-9B", "name": "Qwen 3.5 9B"},
            {"id": "Qwen/Qwen3-235B-A22B", "name": "Qwen3 235B A22B"},
            {"id": "Qwen/Qwen3-32B", "name": "Qwen3 32B"},
            {"id": "Qwen/Qwen3-14B", "name": "Qwen3 14B"},
            {"id": "Qwen/Qwen3-8B", "name": "Qwen3 8B"},
            {"id": "Qwen/Qwen3-Coder-480B-A35B-Instruct", "name": "Qwen3 Coder 480B"},
            {"id": "Qwen/Qwen3-Coder-30B-A3B-Instruct", "name": "Qwen3 Coder 30B"},
            {"id": "Qwen/Qwen2.5-72B-Instruct", "name": "Qwen 2.5 72B"},
            {"id": "Qwen/Qwen2.5-32B-Instruct", "name": "Qwen 2.5 32B"},
            {"id": "Qwen/Qwen2.5-14B-Instruct", "name": "Qwen 2.5 14B"},
            {"id": "Qwen/Qwen2.5-7B-Instruct", "name": "Qwen 2.5 7B"},
            {"id": "zai-org/GLM-5.1", "name": "GLM 5.1"},
            {"id": "zai-org/GLM-5", "name": "GLM 5"},
            {"id": "zai-org/GLM-4.7", "name": "GLM 4.7"},
            {"id": "zai-org/GLM-4.6", "name": "GLM 4.6"},
            {"id": "THUDM/GLM-4-32B-0414", "name": "GLM 4 32B"},
            {"id": "THUDM/GLM-4-9B-0414", "name": "GLM 4 9B"},
            {"id": "moonshotai/Kimi-K2.6", "name": "Kimi K2.6"},
            {"id": "moonshotai/Kimi-K2.5", "name": "Kimi K2.5"},
            {"id": "moonshotai/Kimi-K2-Thinking", "name": "Kimi K2 Thinking"},
            {"id": "openai/gpt-oss-120b", "name": "GPT OSS 120B"},
            {"id": "openai/gpt-oss-20b", "name": "GPT OSS 20B"},
            {"id": "baidu/ERNIE-4.5-300B-A47B", "name": "ERNIE 4.5 300B"},
            {"id": "tencent/Hunyuan-A13B-Instruct", "name": "Hunyuan A13B"},
            {"id": "meta-llama/Meta-Llama-3.1-8B-Instruct", "name": "Llama 3.1 8B"},
            {"id": "MiniMaxAI/MiniMax-M2.5", "name": "MiniMax M2.5"},
            {"id": "google/gemma-4-31B-it", "name": "Gemma 4 31B"},
            {"id": "google/gemma-4-26B-A4B-it", "name": "Gemma 4 26B"},
            {"id": "ByteDance-Seed/Seed-OSS-36B-Instruct", "name": "Seed OSS 36B"},
        ],
        "keyfree_status": "needs_key",
        "keyfree_note": "OpenAI 兼容，80+ 模型，需 Bearer Key。",
    },
    {
        "id": "together",
        "name": "Together AI",
        "base_url": "https://api.together.xyz/v1",
        "auth": "apikey",
        "free_models": 2,
        "models": [
            {"id": "meta-llama/Llama-3.3-70B-Instruct-Turbo-Free", "name": "Llama 3.3 70B Turbo (Free)", "free": True},
            {"id": "meta-llama/Llama-Vision-Free", "name": "Llama Vision (Free)", "free": True},
            {"id": "meta-llama/Llama-3.3-70B-Instruct-Turbo", "name": "Llama 3.3 70B Turbo"},
            {"id": "deepseek-ai/DeepSeek-R1", "name": "DeepSeek R1"},
            {"id": "Qwen/Qwen3-235B-A22B", "name": "Qwen3 235B"},
            {"id": "meta-llama/Llama-4-Maverick-17B-128E-Instruct-FP8", "name": "Llama 4 Maverick"},
        ],
        "keyfree_status": "needs_key",
        "keyfree_note": "OpenAI 兼容，有免费层（Llama 3.3 70B Turbo / Llama Vision），需 Bearer Key。",
    },
]

with open(CATALOG, encoding="utf-8") as f:
    data = json.load(f)

existing = {p["id"] for p in data["providers"]}
added = 0
for p in NEW_PROVIDERS:
    if p["id"] in existing:
        print(f"  SKIP {p['id']} (already exists)")
        continue
    data["providers"].append(p)
    added += 1
    print(f"  ADD  {p['id']} ({len(p['models'])} models)")

tmp = CATALOG + ".tmp"
with open(tmp, "w", encoding="utf-8") as f:
    json.dump(data, f, ensure_ascii=False, indent=2)
    f.write("\n")
os.replace(tmp, CATALOG)
print(f"Written to {CATALOG}")

shutil.copy2(CATALOG, BUILD_ROOT_CATALOG)
print(f"Synced to {BUILD_ROOT_CATALOG}")
print(f"Added={added}, total_providers={len(data['providers'])}")
