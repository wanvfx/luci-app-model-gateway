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
	"github.com/wanvfx/luci-app-model-gateway/engine"
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
	ca := s.authClient(r)
	if !ca.authed {
		if ca.quotaExceeded {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"virtual key daily quota exceeded","type":"quota_exceeded"}}`))
			return
		}
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	vkey := ca.vkey // 非 nil 表示以虚拟密钥鉴权，成功时需记录用量

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

	// 别名解析（G8）：友好名 -> 内部模型/路由组/auto，透明重写请求
	if req.Model != "" {
		if resolved := s.resolveAlias(req.Model); resolved != req.Model {
			body = replaceModelInBody(body, req.Model, resolved)
			req.Model = resolved
		}
	}

	// 虚拟密钥模型白名单（安全修复）：配置了 AllowedModels 的子密钥只能调用白名单内的模型。
	// 此前 Allowed() 从未被调用，导致白名单形同虚设（越权漏洞）。
	if vkey != nil && s.vkeys != nil && !s.vkeys.Allowed(vkey, req.Model) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"message":"model not allowed for this virtual key","type":"forbidden"}}`))
		return
	}

	// Hermes 压缩 + 强制中文
	body = s.meta.CompressHermes(body)
	body = s.meta.EnsureLangReply(body)

	// PII 正则脱敏（转发第三方前脱敏手机号/身份证/邮箱/银行卡，网关设置可开关）
	if s.cfg.Load().PIISanitize() {
		body = sanitizePIIBody(body)
	}

	// 幂等键（Idempotency-Key 请求头，仅非流式）：相同 key 的重试直接返回首次响应
	idemKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idemKey != "" && !req.Stream {
		if e, hit := s.idem.Get(idemKey); hit {
			w.Header().Set("Content-Type", e.contentType)
			w.Header().Set("X-Idempotent-Replay", "true")
			w.WriteHeader(e.status)
			_, _ = w.Write(e.body)
			return
		}
	}

	// 预算/余额护栏：超限且 action=block 直接拦截（在消耗上游前）
	if bcfg := s.cfg.Load().EffectiveBudget(); bcfg.DailyLimitUSD > 0 {
		if st := s.budget.Status(bcfg.DailyLimitUSD, bcfg.Action, bcfg.WarningPct); st.Blocked {
			http.Error(w, "daily budget exceeded", http.StatusTooManyRequests)
			s.hookDispatcher().Fire(HookEventFailed, req.Model, "", req.Stream, 0, "budget exceeded", "")
			return
		}
	}

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
			body = replaceModelInBody(body, originalModel, req.Model)
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

	// 缓存键（基于最终 upstream body + model）
	cacheCfg := s.cfg.Load().EffectiveCache()
	var exactKey, promptNorm string
	if cacheCfg.Enabled {
		exactKey = s.cache.ExactKey(req.Model, string(body))
		promptNorm = s.cache.PromptNorm(string(body))
		// 缓存命中：非流式直接返回；流式以 SSE 回放（精确 + 语义近重复）
		if content, hit, _ := s.cache.GetContent(req.Model, exactKey, promptNorm); hit {
			s.hookDispatcher().Fire(HookEventDone, req.Model, "", req.Stream, 0, "", "cache")
			if req.Stream {
				replayCachedStream(w, req.Model, content)
			} else {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(synthesizeCachedResponse(req.Model, content))
			}
			return
		}
	}

	// 流式转发
	if req.Stream {
		s.handleStreamChat(w, r, &req, body, visionPrelude, exactKey, promptNorm, vkey)
		return
	}

	// 并发护栏：全局槽位
	globalMax := s.cfg.Load().MaxConcurrency
	releaseGlobal, ok := s.guard.TryAcquire("", globalMax, 0, 3*time.Second)
	if !ok {
		http.Error(w, "too many concurrent requests", http.StatusTooManyRequests)
		s.hookDispatcher().Fire(HookEventFailed, req.Model, "", false, 0, "concurrency limit", "")
		return
	}
	defer releaseGlobal()

	// 构建禁用集合（全局 + 各 provider 的 per-provider 禁用，对齐 Python）
	disabled := map[string]bool{}
	for _, dm := range s.cfg.Load().AllDisabledModels() {
		disabled[strings.ToLower(strings.TrimSpace(dm))] = true
	}

	// 挑选候选上游（携带能力矩阵/分类路由选项）
	requiredCaps := []string{}
	if hasImage {
		requiredCaps = append(requiredCaps, "vision")
	}
	pickOpts := engine.PickOptions{
		Strict:       s.cfg.Load().StrictCapability(),
		RequiredCaps: requiredCaps,
		Content:      string(body),
	}
	candidates := s.router.Load().PickCandidates(req.Model, s.cfg.Load().Providers, disabled, pickOpts)
	if len(candidates) == 0 {
		http.Error(w, fmt.Sprintf("no available models: %s", req.Model), http.StatusBadGateway)
		return
	}

	// 路由组 failover 即「遍历全部候选」，无需再乘 2（修复 #3）
	maxAttempts := 1
	var lastErr error
	allBusy := false

	for attempt := 0; attempt < maxAttempts; attempt++ {
		for _, candidate := range candidates {
			modelKey := candidate.Provider.Name + "||" + candidate.Model
			// 并发护栏：单 provider 槽位
			rel, pok := s.guard.TryAcquire(candidate.Provider.Name, 0, candidate.Provider.MaxConcurrency, 3*time.Second)
			if !pok {
				allBusy = true
				continue
			}
			started := time.Now()
			resp, err := s.forwardChat(r.Context(), req.Model, candidate.Model, body)
			rel() // 上游请求已返回，释放 provider 槽位
			if err != nil {
				lastErr = err
				// 归因熔断：客户端取消不算上游故障，超时/连接失败才跳闸
				s.circuits.RecordFailureWithType(modelKey, engine.ClassifyNetErr(err))
				s.metrics.ObserveRequest(candidate.Provider.Name, candidate.Model, "fail", time.Since(started), 0, 0)
				s.recordPenalty(candidate.Provider.Name, candidate.Model, 0, true)
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
				// 归因熔断：429/401/403/402 等不误杀，仅 5xx 跳闸
				s.circuits.RecordFailureWithType(modelKey, engine.ClassifyStatus(resp.StatusCode))
				s.metrics.ObserveRequest(candidate.Provider.Name, candidate.Model, "fail", time.Since(started), 0, 0)
				s.recordPenalty(candidate.Provider.Name, candidate.Model, resp.StatusCode, false)
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
			promptTok, complTok := 0, 0
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
				// 预算记账（成本护栏）
				s.recordBudget(candidate.Provider, candidate.Model, &chatResp)
				// 写入响应缓存（非流式）
				if cacheCfg.Enabled {
					s.cache.PutContent(req.Model, exactKey, promptNorm, chatResp.Choices[0].Message.Content, false)
				}
				// 钩子：请求成功
				tokens := 0
				if chatResp.Usage != nil {
					tokens = chatResp.Usage.TotalTokens
					promptTok = chatResp.Usage.PromptTokens
					complTok = chatResp.Usage.CompletionTokens
				}
				s.hookDispatcher().Fire(HookEventDone, req.Model, candidate.Provider.Name, false, tokens, "", chatResp.ID)
				// 记录调用日志（成功）
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

			// Prometheus 指标（成功）
			s.metrics.ObserveRequest(candidate.Provider.Name, candidate.Model, "ok", time.Since(started), promptTok, complTok)

		// 幂等键：保存首次成功响应快照
		if idemKey != "" {
			s.idem.Put(idemKey, http.StatusOK, "application/json", b)
		}

		// 虚拟密钥：记录当日用量（1 请求 + 本次 Token）
		s.recordVKeyUsage(vkey, 1, promptTok+complTok)

		w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(b)
			return
		}
	}

	// 并发全忙：返回 429
	if allBusy && lastErr == nil {
		http.Error(w, "too many concurrent requests", http.StatusTooManyRequests)
		s.hookDispatcher().Fire(HookEventFailed, req.Model, "", false, 0, "concurrency limit", "")
		return
	}

	// 全部失败：对齐 Python 原版返回 502（屏蔽上游细节，仅给统一提示）
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadGateway)
	errMsg := ""
	if lastErr != nil {
		errMsg = lastErr.Error()
	}
	respBody, _ := json.Marshal(map[string]interface{}{
		"detail": fmt.Sprintf("所有候选模型均失败: %v", lastErr),
	})
	s.hookDispatcher().Fire(HookEventFailed, req.Model, "", false, 0, errMsg, "")
	_, _ = w.Write(respBody)
}

// replaceModelInBody 在请求体中替换 model 字段（兼容紧凑/带空格两种 JSON 格式）
func replaceModelInBody(body []byte, old, new string) []byte {
	s := string(body)
	s = strings.Replace(s, `"model":"`+old+`"`, `"model":"`+new+`"`, 1)
	s = strings.Replace(s, `"model": "`+old+`"`, `"model": "`+new+`"`, 1)
	return []byte(s)
}

// synthesizeCachedResponse 由缓存内容合成 OpenAI 兼容的非流式响应
func synthesizeCachedResponse(modelLabel, content string) []byte {
	resp := map[string]interface{}{
		"id":      "cache-" + fmt.Sprintf("%d", time.Now().UnixNano()),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   modelLabel,
		"choices": []map[string]interface{}{
			{
				"index": 0,
				"message": map[string]interface{}{
					"role":    "assistant",
					"content": content,
				},
				"finish_reason": "stop",
			},
		},
		"usage": nil,
	}
	b, _ := json.Marshal(resp)
	return b
}

// recordBudget 按参考库价格累加本次请求成本（预算/余额护栏）
func (s *Server) recordBudget(provider *config.Provider, model string, resp *ChatResponse) {
	if s.budget == nil || resp.Usage == nil {
		return
	}
	var priceIn, priceOut float64
	if s.catalog != nil {
		if e := s.catalog.Lookup(model); e != nil {
			priceIn, priceOut = e.PriceIn, e.PriceOut
		}
	}
	s.budget.AddCost(model, provider.Name, resp.Usage.PromptTokens, resp.Usage.CompletionTokens, priceIn, priceOut)
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

	// 替换模型名（使用别名解析）。
	// 安全修复：只替换 JSON 的 "model" 字段，不再对整个 body 做子串替换，
	// 避免用户 prompt 内容恰好包含模型名时被误改（污染对话内容）。
	resolvedModel := s.meta.ResolveModel(targetModel)
	reqBody := replaceModelInBody(body, originalModel, resolvedModel)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, provider.BaseURL+"/chat/completions", bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+provider.APIKey)

	return s.httpClient.Do(req)
}

// handleStreamChat 流式转发
func (s *Server) handleStreamChat(w http.ResponseWriter, r *http.Request, req *ChatRequest, body []byte, prelude, cacheExactKey, cachePromptNorm string, vkey *storage.VKey) {
	// 并发护栏：全局槽位（流式在整个流期间占用槽位）
	globalMax := s.cfg.Load().MaxConcurrency
	releaseGlobal, ok := s.guard.TryAcquire("", globalMax, 0, 3*time.Second)
	if !ok {
		http.Error(w, "too many concurrent requests", http.StatusTooManyRequests)
		s.hookDispatcher().Fire(HookEventFailed, req.Model, "", true, 0, "concurrency limit", "")
		return
	}
	defer releaseGlobal()

	// 构建禁用集合（全局 + 各 provider 的 per-provider 禁用，对齐 Python）
	disabled := map[string]bool{}
	for _, dm := range s.cfg.Load().AllDisabledModels() {
		disabled[strings.ToLower(strings.TrimSpace(dm))] = true
	}

	// 严格能力矩阵/分类路由选项（与流式内联计算，避免跨函数传参）
	requiredCaps := []string{}
	if strings.Contains(string(body), "image_url") || strings.Contains(string(body), "data:image") {
		requiredCaps = append(requiredCaps, "vision")
	}
	pickOpts := engine.PickOptions{
		Strict:       s.cfg.Load().StrictCapability(),
		RequiredCaps: requiredCaps,
		Content:      string(body),
	}
	candidates := s.router.Load().PickCandidates(req.Model, s.cfg.Load().Providers, disabled, pickOpts)
	if len(candidates) == 0 {
		http.Error(w, "no available providers", http.StatusBadGateway)
		s.hookDispatcher().Fire(HookEventFailed, req.Model, "", true, 0, "no available providers", "")
		return
	}

	s.forwardStreamWithFailover(w, r, req, body, candidates, prelude, cacheExactKey, cachePromptNorm, vkey)
}

// replayCachedStream 将缓存内容以 OpenAI 兼容 SSE 流回放给客户端
func replayCachedStream(w http.ResponseWriter, modelLabel, content string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	id := fmt.Sprintf("cache-%d", time.Now().UnixNano())
	created := time.Now().Unix()
	// 分块回放（每块约 512 字符，按 rune 边界切分避免撕裂多字节字符）
	runes := []rune(content)
	const chunkSize = 512
	for i := 0; i < len(runes); i += chunkSize {
		end := i + chunkSize
		if end > len(runes) {
			end = len(runes)
		}
		chunk := map[string]interface{}{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   modelLabel,
			"choices": []map[string]interface{}{
				{"index": 0, "delta": map[string]interface{}{"content": string(runes[i:end])}, "finish_reason": nil},
			},
		}
		b, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", string(b))
		if flusher != nil {
			flusher.Flush()
		}
	}
	// 结束 chunk（finish_reason=stop）+ [DONE]
	final := map[string]interface{}{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   modelLabel,
		"choices": []map[string]interface{}{
			{"index": 0, "delta": map[string]interface{}{}, "finish_reason": "stop"},
		},
	}
	fb, _ := json.Marshal(final)
	fmt.Fprintf(w, "data: %s\n\n", string(fb))
	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
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

// recordPenalty 记录动态惩罚事件：429 限流最重、5xx/网络故障次之，其余状态码不惩罚。
// 同时以「前缀名 provider-model」与「裸模型名」两个 key 记录，
// 覆盖路由组成员两种书写形态；指数衰减（约 2 分钟半衰）自动恢复。
func (s *Server) recordPenalty(providerName, model string, status int, netErr bool) {
	var weight float64
	switch {
	case status == http.StatusTooManyRequests:
		weight = engine.PenaltyRate
	case netErr || status >= 500:
		weight = engine.PenaltyServer
	default:
		return
	}
	s.penalty.Record(providerName+"-"+model, weight)
	s.penalty.Record(model, weight)
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
