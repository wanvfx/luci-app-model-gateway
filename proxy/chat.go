package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/wanvfx/luci-app-model-gateway/calllog"
	"github.com/wanvfx/luci-app-model-gateway/config"
	"github.com/wanvfx/luci-app-model-gateway/storage"
	"github.com/wanvfx/luci-app-model-gateway/vision"
)

// UpstreamError 携带上游 HTTP 状态码和响应体的错误
type UpstreamError struct {
	StatusCode int
	Body       []byte
}

func (e *UpstreamError) Error() string {
	return fmt.Sprintf("upstream returned %d", e.StatusCode)
}

// ChatRequest OpenAI 兼容请求
type ChatRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Stream   bool      `json:"stream,omitempty"`
}

// Message 消息
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatResponse OpenAI 兼容响应
type ChatResponse struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index   int `json:"index"`
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage,omitempty"`
}

// handleChatCompletions 处理 /v1/chat/completions
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if !s.authenticateClient(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	var req ChatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	// Hermes 压缩 + 强制中文
	body = s.meta.CompressHermes(body)
	body = s.meta.EnsureLangReply(body)

	// 识图检测（与 Python 原版一致：先检查 vision_assist 开关）
	visionDetector := vision.NewDetector(s.cfg.Load().VisionRouter(), s.cfg.Load().VisionMaxTokens())
	hasImage, _ := visionDetector.Detect(body)
	isVisionRouter := s.router.Load().IsRouter(req.Model)
	visionPrelude := ""

	visionEnabled := s.cfg.Load().VisionAssist()
	if visionEnabled && hasImage && !isVisionRouter {
		if s.cfg.Load().VisionRouter() != "" {
			visionPrelude = "🖼️ 已切换到视觉模型回复…\n\n"
			// 先用原模型名替换 body，再更新 req.Model，
			// 否则 Replace(body, 新名, 新名) 是 no-op，上游收到原始模型名（历史 bug #02）
			originalModel := req.Model
			req.Model = s.cfg.Load().VisionRouter()
			body = []byte(strings.Replace(string(body), `"model":"`+originalModel+`"`, `"model":"`+req.Model+`"`, 1))
			// 兼容带空格的 JSON 格式（"model": "xxx"）
			body = []byte(strings.Replace(string(body), `"model": "`+originalModel+`"`, `"model": "`+req.Model+`"`, 1))
			body = visionDetector.ClampMaxTokens(body)
			// 同时限制 max_completion_tokens（与 Python 原版一致）
			body = clampMaxCompletionTokens(body, visionDetector.GetMaxTokens())
			body = visionDetector.InjectCNHint(body)
		} else {
			http.Error(w, "识图辅助已开启，但未配置识图路由组，无法处理图片。", http.StatusServiceUnavailable)
			return
		}
	} else if isVisionRouter {
		// 用户直接请求识图路由组
		body = visionDetector.ClampMaxTokens(body)
		body = clampMaxCompletionTokens(body, visionDetector.GetMaxTokens())
		body = visionDetector.InjectCNHint(body)
	}

	// 流式转发
	if req.Stream {
		s.handleStreamChat(w, r, &req, body, visionPrelude)
		return
	}

	// 构建禁用集合（全局 + 各 provider 的 per-provider 禁用，对齐 Python）
	disabled := map[string]bool{}
	for _, dm := range s.cfg.Load().AllDisabledModels() {
		disabled[strings.ToLower(strings.TrimSpace(dm))] = true
	}

	// 挑选候选上游
	candidates := s.router.Load().PickCandidates(req.Model, s.cfg.Load().Providers, disabled)
	if len(candidates) == 0 {
		http.Error(w, fmt.Sprintf("no available models: %s", req.Model), http.StatusBadGateway)
		return
	}

	// 路由组 failover 即「遍历全部候选」，无需再乘 2（修复 #3：曾导致同一上游被请求两遍、重复计费）
	maxAttempts := 1

	var lastErr error

	for attempt := 0; attempt < maxAttempts; attempt++ {
		for _, candidate := range candidates {
			modelKey := candidate.Provider.Name + "||" + candidate.Model
			resp, err := s.forwardChat(r.Context(), req.Model, candidate.Model, body)
			if err != nil {
				lastErr = err
				s.circuits.RecordFailure(modelKey)
				calllog.Append(calllog.Entry{
					Provider: candidate.Provider.Name,
					Model:    candidate.Model,
					Status:   "fail",
					Error:    err.Error(),
				})
				continue
			}

			// 读取响应
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				lastErr = &UpstreamError{StatusCode: resp.StatusCode, Body: b}
				s.circuits.RecordFailure(modelKey)
				calllog.Append(calllog.Entry{
					Provider: candidate.Provider.Name,
					Model:    candidate.Model,
					Status:   "fail",
					Error:    fmt.Sprintf("HTTP %d", resp.StatusCode),
				})
				continue
			}

			// 成功：记录熔断成功
			s.circuits.RecordSuccess(modelKey)

			// 解析响应提取 usage（与 Python 原版一致）
			var chatResp ChatResponse
			if err := json.Unmarshal(b, &chatResp); err == nil {
				// 替换 model 字段为 provider · model（与 Python 原版一致）
				chatResp.Model = fmt.Sprintf("%s · %s", candidate.Provider.Name, candidate.Model)

				// 添加 🤖 provider · model 前缀到 content
				if len(chatResp.Choices) > 0 && chatResp.Choices[0].Message.Content != "" {
					prefix := fmt.Sprintf("🤖 %s · %s\n\n", candidate.Provider.Name, candidate.Model)
					chatResp.Choices[0].Message.Content = prefix + chatResp.Choices[0].Message.Content
				}
				// Hermes 还原
				if len(chatResp.Choices) > 0 {
					chatResp.Choices[0].Message.Content = s.meta.RestoreHermesText(chatResp.Choices[0].Message.Content)
				}
				b, _ = json.Marshal(chatResp)

				// 记录 usage 到 usage.jsonl
				s.recordUsage(candidate.Provider, candidate.Model, &chatResp)
				// 记录调用日志（成功）
				tokens := 0
				if chatResp.Usage != nil {
					tokens = chatResp.Usage.TotalTokens
				}
				calllog.Append(calllog.Entry{
					Provider: candidate.Provider.Name,
					Model:    candidate.Model,
					Status:   "ok",
					Tokens:   tokens,
				})
			} else {
				// 解析失败，透传原始响应
				b = []byte(s.meta.RestoreHermesText(string(b)))
			}

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(b)
			return
		}
	}

	// 全部失败：对齐 Python 原版返回 502（屏蔽上游细节，仅给统一提示）。
	// 注意：这样会牺牲「透传上游原始错误码便于调试」的便利，换来与 Python 一致的行为，
	// 避免客户端因上游返回 400/401 等而收到非 502 的意外状态码。
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadGateway)
	respBody, _ := json.Marshal(map[string]interface{}{
		"detail": fmt.Sprintf("所有候选模型均失败: %v", lastErr),
	})
	_, _ = w.Write(respBody)
}

// forwardChat 转发到上游
func (s *Server) forwardChat(ctx context.Context, originalModel, targetModel string, body []byte) (*http.Response, error) {
	if len(s.cfg.Load().Providers) == 0 {
		return nil, fmt.Errorf("no providers configured")
	}

	// 查找目标模型对应的 provider
	var provider *config.Provider
	for _, p := range s.cfg.Load().Providers {
		for _, m := range p.Models {
			if m == targetModel {
				provider = p
				break
			}
		}
		if provider != nil {
			break
		}
	}
	if provider == nil {
		provider = s.cfg.Load().Providers[0]
	}

	// 替换模型名（使用别名解析）
	resolvedModel := s.meta.ResolveModel(targetModel)
	reqBody := strings.Replace(string(body), originalModel, resolvedModel, 1)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, provider.BaseURL+"/chat/completions", bytes.NewReader([]byte(reqBody)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+provider.APIKey)

	return s.httpClient.Do(req)
}

// handleStreamChat 流式转发
func (s *Server) handleStreamChat(w http.ResponseWriter, r *http.Request, req *ChatRequest, body []byte, prelude string) {
	// 构建禁用集合（全局 + 各 provider 的 per-provider 禁用，对齐 Python）
	disabled := map[string]bool{}
	for _, dm := range s.cfg.Load().AllDisabledModels() {
		disabled[strings.ToLower(strings.TrimSpace(dm))] = true
	}

	candidates := s.router.Load().PickCandidates(req.Model, s.cfg.Load().Providers, disabled)
	if len(candidates) == 0 {
		http.Error(w, "no available providers", http.StatusBadGateway)
		return
	}

	s.forwardStreamWithFailover(w, r, req, body, candidates, prelude)
}

// clampMaxCompletionTokens 限制 max_completion_tokens（与 Python 原版一致）
func clampMaxCompletionTokens(body []byte, maxTokens int) []byte {
	if maxTokens <= 0 {
		return body
	}
	var req struct {
		MaxCompletionTokens *int `json:"max_completion_tokens"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.MaxCompletionTokens == nil || *req.MaxCompletionTokens <= maxTokens {
		return body
	}
	s := string(body)
	oldStr := fmt.Sprintf("\"max_completion_tokens\":%d", *req.MaxCompletionTokens)
	newStr := fmt.Sprintf("\"max_completion_tokens\":%d", maxTokens)
	s = strings.Replace(s, oldStr, newStr, 1)
	// 也处理带空格格式
	oldStr2 := fmt.Sprintf("\"max_completion_tokens\": %d", *req.MaxCompletionTokens)
	newStr2 := fmt.Sprintf("\"max_completion_tokens\": %d", maxTokens)
	s = strings.Replace(s, oldStr2, newStr2, 1)
	return []byte(s)
}

// recordUsage 记录 usage 到 usage.jsonl
func (s *Server) recordUsage(provider *config.Provider, model string, resp *ChatResponse) {
	if s.usage == nil || resp.Usage == nil {
		return
	}
	_ = s.usage.Append(storage.UsageRecord{
		Time:             time.Now(),
		Provider:         provider.Name,
		Model:            model,
		RequestID:        resp.ID,
		PromptTokens:     resp.Usage.PromptTokens,
		CompletionTokens: resp.Usage.CompletionTokens,
		TotalTokens:      resp.Usage.TotalTokens,
	})
}
