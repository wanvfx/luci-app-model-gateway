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
)

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
	if prov.APIKey == "" {
		return false, "empty api_key", 0, FailClient
	}

	url := strings.TrimRight(prov.BaseURL, "/") + "/models"
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return false, fmt.Sprintf("build request: %v", err), 0, FailClient
	}
	req.Header.Set("Authorization", "Bearer "+prov.APIKey)
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

// ChatProbe 对单个模型做真实探测：POST {BaseURL}/chat/completions。
//
// 与原 Python 1.6.1 check_model 逐项对齐（问题 3 修复的核心）：
//   - payload: {"model": <别名解析后>, "messages":[{"role":"user","content":"hi"}], "max_tokens":5, "stream":false}
//   - 超时 30 秒（Python: httpx timeout=30；此前 Go 用 15s 客户端导致冷启动模型误报失败）
//   - HTTP 200 → ok，其余状态码 → fail（detail 带响应体前 200 字节，与 Python 一致）
//   - 网络错误 → error（FailServer，允许熔断记账）
//
// 说明：v1.5.1 曾误将探测改为 GET /models 端点级探活——那会导致：
//   ① 每个模型重复轰击同一 /models 端点（NVIDIA 12 个模型=一轮 12 次大列表拉取）→ 限流 429 误报失败；
//   ② 所有模型共享同一端点结果，无法区分单个模型状态；
//   ③ 延迟是列表接口延迟而非生成延迟，与 Python 版仪表数据完全对不上。
// 现回归 Python 原版逐模型真实探测。GET /models 仅保留给 Key 校验（HealthCheck）。
func ChatProbe(baseURL, apiKey, model string, client *http.Client) (ok bool, detail string, latency time.Duration, kind FailKind) {
	if baseURL == "" {
		return false, "empty base_url", 0, FailClient
	}
	if apiKey == "" {
		return false, "empty api_key", 0, FailClient
	}

	url := strings.TrimRight(baseURL, "/") + "/chat/completions"
	payload := map[string]interface{}{
		"model":      model,
		"messages":   []map[string]string{{"role": "user", "content": "hi"}},
		"max_tokens": 5,
		"stream":     false,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return false, fmt.Sprintf("marshal: %v", err), 0, FailClient
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return false, fmt.Sprintf("build request: %v", err), 0, FailClient
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

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
