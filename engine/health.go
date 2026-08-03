package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/wanvfx/luci-app-model-gateway/config"
	"github.com/wanvfx/luci-app-model-gateway/proxy/translator"
)

// chatProbeAdapter 用声明式适配器规格对非 OpenAI 兼容上游做真实对话探测（Phase C）。
//
// 与通用分支的差别：端点、方法、请求头、请求体全部来自 AdapterSpec，
// 并且会先完成预检握手（如 DuckDuckGo 的 x-vqd-4）。握手失败即判定为不可用，
// 这正是这类站点最常见的失效原因（反自动化升级），应当如实反映到面板上。
func chatProbeAdapter(spec *translator.AdapterSpec, prov *config.Provider, model string, client *http.Client) (bool, string, time.Duration, FailKind) {
	start := time.Now()
	token, err := spec.EnsureToken(prov.BaseURL, client)
	if err != nil {
		return false, fmt.Sprintf("preflight: %v", err), time.Since(start), FailAuth
	}

	minimal, _ := json.Marshal(map[string]interface{}{
		"model":      model,
		"messages":   []map[string]string{{"role": "user", "content": "hi"}},
		"max_tokens": 5,
		"stream":     false,
	})
	payload, _, _, err := spec.BuildRequestBody(minimal, token)
	if err != nil {
		return false, fmt.Sprintf("build body: %v", err), time.Since(start), FailClient
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, spec.HTTPMethod(), spec.ChatURL(prov.BaseURL, token), bytes.NewReader(payload))
	if err != nil {
		return false, fmt.Sprintf("build request: %v", err), time.Since(start), FailClient
	}
	req.Header.Set("Content-Type", "application/json")
	prov.ApplyAuth(req.Header)
	spec.ApplyHeaders(req.Header, token)

	resp, err := client.Do(req)
	latency := time.Since(start)
	if err != nil {
		return false, fmt.Sprintf("connect: %v", err), latency, FailServer
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		return true, "HTTP 200", latency, FailServer
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		// 令牌大概率已失效，作废缓存以便下次重新握手。
		translator.InvalidateToken(prov.BaseURL, spec.ID)
	}
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
	detail := fmt.Sprintf("HTTP %d %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return false, detail, latency, FailAuth
	default:
		return false, detail, latency, ClassifyStatus(resp.StatusCode)
	}
}

// HealthCheck 对单个 provider 做轻量级健康探测：GET {BaseURL}/models。
//
// 设计要点（与原 Python 版一致）：
//   - 仅校验「端点可达 + 鉴权有效」，不发起真实 chat 生成。
//   - 好处：不消耗上游生成额度（避免免费额度触发限流）、不受冷启动生成超时影响、
//     不受最小 payload 内容过滤误判，因此健康率与延迟都与原 Python 版对齐。
//   - 旧实现打 POST /chat/completions 真实生成，导致 NVIDIA / 商汤 SenseNova / 魔搭
//     ModelScope 等平台大量误报「异常」：SLA 偏低、延迟虚高、稳定性列表被
//     「隐藏失败模型」清空（bug）。现在统一改用与预设 Key 校验相同的轻量探活。
func HealthCheck(prov *config.Provider, client *http.Client) (ok bool, detail string, latency time.Duration, kind FailKind) {
	if prov == nil {
		return false, "nil provider", 0, FailClient
	}
	if prov.BaseURL == "" {
		return false, "empty base_url", 0, FailClient
	}
	if prov.APIKey == "" && prov.AuthHeader == "" && prov.AuthScheme == "" && !prov.NoAuth {
		// 完全未配置鉴权信息且非免 Key 提供者 → 按旧行为快速失败；
		// 免 Key 提供者（marketAdd 传入 auth_scheme=none 或显式 no_auth=true）允许空密钥继续探测。
		return false, "empty api_key", 0, FailClient
	}

	url := strings.TrimRight(prov.BaseURL, "/") + "/models"
	// 通用协议适配器（Phase C）：这类上游多数根本没有 /models 端点。
	// 有 models_path 的按声明走；没有的直接退回对话探测，否则必然 404 误判。
	if spec := translator.LookupAdapter(prov.FormatOrDefault()); spec != nil {
		mu := spec.ModelsURL(prov.BaseURL)
		if mu == "" {
			m := "probe"
			if len(prov.Models) > 0 {
				m = prov.Models[0]
			}
			return chatProbeAdapter(spec, prov, m, client)
		}
		url = mu
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return false, fmt.Sprintf("build request: %v", err), 0, FailClient
	}
	prov.ApplyAuth(req.Header)
	req.Header.Set("Accept", "application/json")

	start := time.Now()
	resp, err := client.Do(req)
	latency = time.Since(start)
	if err != nil {
		return false, fmt.Sprintf("connect: %v", err), latency, FailServer
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK, http.StatusNoContent:
		return true, fmt.Sprintf("HTTP %d", resp.StatusCode), latency, FailServer
	case http.StatusUnauthorized, http.StatusForbidden:
		return false, fmt.Sprintf("auth failed (HTTP %d)", resp.StatusCode), latency, FailAuth
	default:
		return false, fmt.Sprintf("HTTP %d", resp.StatusCode), latency, ClassifyStatus(resp.StatusCode)
	}
}

// ChatProbe 对单个模型做真实探测：
// 按 provider 的协议格式（FormatOrDefault）选择对应原生端点与最小探测体，
// 使 gemini/claude/openai-responses 等非 openai 原生端点也能被正确探活，
// 不再因硬编码 POST /chat/completions 返回 404 而误标 🔴 并隐藏模型（问题 F1）。
//
// 与原 Python 1.6.1 check_model 逐项对齐（问题 3 修复的核心）：
//   - 超时 30 秒（Python: httpx timeout=30；此前 Go 用 15s 客户端导致冷启动模型误报失败）
//   - HTTP 200 → ok，其余状态码 → fail（detail 带响应体前 200 字节，与 Python 一致）
//   - 网络错误 → error（FailServer，允许熔断记账）
//
// 说明：v1.5.1 曾误将探测改为 GET /models 端点级探活——那会导致：
//
//	① 每个模型重复轰击同一 /models 端点（NVIDIA 12 个模型=一轮 12 次大列表拉取）→ 限流 429 误报失败；
//	② 所有模型共享同一端点结果，无法区分单个模型状态；
//	③ 延迟是列表接口延迟而非生成延迟，与 Python 版仪表数据完全对不上。
//
// 现回归 Python 原版逐模型真实探测，并补上格式感知。GET /models 仅保留给 Key 校验（HealthCheck）。
func ChatProbe(prov *config.Provider, model string, client *http.Client) (ok bool, detail string, latency time.Duration, kind FailKind) {
	if prov == nil || prov.BaseURL == "" {
		return false, "empty base_url", 0, FailClient
	}
	if prov.APIKey == "" && prov.AuthHeader == "" && prov.AuthScheme == "" && !prov.NoAuth {
		// 与 HealthCheck 同规则：完全未配置鉴权且非免 Key 才快速失败；免 Key（auth_scheme=none 或 no_auth=true）放行
		return false, "empty api_key", 0, FailClient
	}

	// 通用协议适配器（Phase C）：format 命中声明式规格时，按规格构造探测请求。
	// 否则非标提供者会被打到 /chat/completions 而必然 404，面板将永远显示 🔴。
	if spec := translator.LookupAdapter(prov.FormatOrDefault()); spec != nil {
		return chatProbeAdapter(spec, prov, model, client)
	}

	// 按上游协议格式选择原生探测端点与最小探测体（F1）。
	var url string
	var payload []byte
	extraHeaders := map[string]string{}
	switch strings.ToLower(prov.FormatOrDefault()) {
	case "gemini":
		// Gemini 原生 generateContent 协议：模型名在路径中。
		url = strings.TrimRight(prov.BaseURL, "/") + "/models/" + model + ":generateContent"
		p, _ := json.Marshal(map[string]interface{}{
			"contents": []map[string]interface{}{
				{"role": "user", "parts": []map[string]interface{}{{"text": "hi"}}},
			},
			"generationConfig": map[string]interface{}{"maxOutputTokens": 5},
		})
		payload = p
	case "claude":
		// Anthropic /v1/messages 原生协议：需要 anthropic-version 固定头，max_tokens 必填。
		url = strings.TrimRight(prov.BaseURL, "/") + "/v1/messages"
		p, _ := json.Marshal(map[string]interface{}{
			"model":      model,
			"messages":   []map[string]interface{}{{"role": "user", "content": "hi"}},
			"max_tokens": 5,
			"stream":     false,
		})
		payload = p
		extraHeaders["anthropic-version"] = "2023-06-01"
	case "openai-responses":
		// OpenAI Responses API 原生协议：input 替代 messages，max_output_tokens 替代 max_tokens。
		url = strings.TrimRight(prov.BaseURL, "/") + "/v1/responses"
		p, _ := json.Marshal(map[string]interface{}{
			"model":             model,
			"input":             []map[string]interface{}{{"role": "user", "content": "hi"}},
			"max_output_tokens": 5,
			"stream":            false,
		})
		payload = p
	default: // openai（含空值回退）
		url = strings.TrimRight(prov.BaseURL, "/") + "/chat/completions"
		p, _ := json.Marshal(map[string]interface{}{
			"model":      model,
			"messages":   []map[string]string{{"role": "user", "content": "hi"}},
			"max_tokens": 5,
			"stream":     false,
		})
		payload = p
	}
	if len(payload) == 0 {
		return false, "marshal payload failed", 0, FailClient
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return false, fmt.Sprintf("build request: %v", err), 0, FailClient
	}
	prov.ApplyAuth(req.Header)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}

	start := time.Now()
	resp, err := client.Do(req)
	latency = time.Since(start)
	if err != nil {
		return false, fmt.Sprintf("connect: %v", err), latency, FailServer
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		return true, "HTTP 200", latency, FailServer
	}
	// 与 Python 一致：失败时带响应体前 200 字节便于排障
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
	detail = fmt.Sprintf("HTTP %d %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return false, detail, latency, FailAuth
	default:
		return false, detail, latency, ClassifyStatus(resp.StatusCode)
	}
}
