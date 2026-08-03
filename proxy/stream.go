package proxy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/wanvfx/luci-app-model-gateway/calllog"
	"github.com/wanvfx/luci-app-model-gateway/config"
	"github.com/wanvfx/luci-app-model-gateway/engine"
	"github.com/wanvfx/luci-app-model-gateway/proxy/translator"
	"github.com/wanvfx/luci-app-model-gateway/storage"
)

// streamResult 流式转发结果（用于续写判断）
type streamResult struct {
	accumulated      string // 累计收到的 content + reasoning_content
	upstreamErr      error
	streamOK         bool // 流是否完整结束（收到 [DONE]）
	totalTokens      int  // 本次消耗的 token 总数（用于调用日志）
	promptTokens     int  // prompt token（Prometheus 指标用）
	completionTokens int  // completion token（Prometheus 指标用）
}

// streamFromUpstream 从单个上游流式转发。
// headersSent 为跨候选共享状态（安全修复）：failover 到第二个候选时不再重复
// WriteHeader / 重复发送 vision prelude，避免客户端收到重复内容与
// "superfluous response.WriteHeader" 告警（流式响应乱码问题）。
func (s *Server) streamFromUpstream(w http.ResponseWriter, r *http.Request, req *ChatRequest, body []byte, candidate *engine.Candidate, prelude string, headersSent *bool) streamResult {
	provider := candidate.Provider
	if provider == nil {
		return streamResult{upstreamErr: fmt.Errorf("no provider for model %s", candidate.Model)}
	}

	// 替换模型名（使用别名解析）。
	// 安全修复：只替换 JSON 的 "model" 字段，不再对整个 body 做子串替换，
	// 避免用户 prompt 内容恰好包含模型名时被误改（污染对话内容）。
	resolvedModel := s.meta.ResolveModel(candidate.Model)
	isGemini := strings.EqualFold(provider.FormatOrDefault(), translator.FormatGemini)
	isClaude := strings.EqualFold(provider.FormatOrDefault(), translator.FormatClaude)
	isResponses := strings.EqualFold(provider.FormatOrDefault(), translator.FormatResponses)
	// 通用协议适配器（Phase C）：format 命中声明式规格时，按规格构造上游请求。
	adapterSpec := translator.LookupAdapter(provider.FormatOrDefault())
	adapterToken := ""
	if adapterSpec != nil {
		tk, terr := adapterSpec.EnsureToken(provider.BaseURL, s.httpClient)
		if terr != nil {
			return streamResult{upstreamErr: fmt.Errorf("adapter %s preflight: %w", adapterSpec.ID, terr)}
		}
		adapterToken = tk
	}

	// 计算上游 URL 与请求体：Gemini/Claude 走各自原生协议，openai 直连。
	var upstreamURL string
	var upstreamBodyStr string
	if adapterSpec != nil {
		ab, _, _, aerr := adapterSpec.BuildRequestBody([]byte(replaceModelInBody(body, req.Model, resolvedModel)), adapterToken)
		if aerr != nil {
			return streamResult{upstreamErr: aerr}
		}
		upstreamBodyStr = string(ab)
		upstreamURL = adapterSpec.ChatURL(provider.BaseURL, adapterToken)
	} else if isGemini {
		gb, model, _, gerr := translator.ToGeminiBody([]byte(replaceModelInBody(body, req.Model, resolvedModel)))
		if gerr != nil {
			return streamResult{upstreamErr: gerr}
		}
		upstreamBodyStr = string(gb)
		upstreamURL = strings.TrimRight(provider.BaseURL, "/") + "/models/" + model + ":streamGenerateContent?alt=sse"
	} else if isClaude {
		cb, _, cerr := translator.ToClaudeBody([]byte(replaceModelInBody(body, req.Model, resolvedModel)))
		if cerr != nil {
			return streamResult{upstreamErr: cerr}
		}
		upstreamBodyStr = string(cb)
		upstreamURL = strings.TrimRight(provider.BaseURL, "/") + "/v1/messages"
	} else if isResponses {
		rb, _, rerr := translator.ToResponsesBody([]byte(replaceModelInBody(body, req.Model, resolvedModel)))
		if rerr != nil {
			return streamResult{upstreamErr: rerr}
		}
		upstreamBodyStr = string(rb)
		upstreamURL = strings.TrimRight(provider.BaseURL, "/") + "/v1/responses"
	} else {
		reqBody := string(replaceModelInBody(body, req.Model, resolvedModel))
		// 注入 stream_options: {"include_usage": true}（确保上游在流中返回 usage）
		reqBody = injectStreamOptions(reqBody)
		upstreamBodyStr = reqBody
		upstreamURL = provider.BaseURL + "/chat/completions"
	}

	// G5 思考预算：对支持推理的模型注入 thinking 参数（claude / gemini）。openai 推理模型走原生参数，此处不注入。
	if provider.ThinkingBudget > 0 {
		if isClaude {
			upstreamBodyStr = injectClaudeThinking(upstreamBodyStr, provider.ThinkingBudget)
		} else if isGemini {
			upstreamBodyStr = injectGeminiThinking(upstreamBodyStr, provider.ThinkingBudget)
		}
	}
	// Provider 专属请求体转换（OpenCode strip client_metadata / Pollinations jsonMode）。
	upstreamBodyStr = string(transformRequestBody(provider, []byte(upstreamBodyStr)))

	// 创建上游请求
	upstreamMethod := http.MethodPost
	if adapterSpec != nil {
		upstreamMethod = adapterSpec.HTTPMethod()
	}
	upstreamReq, err := http.NewRequestWithContext(r.Context(), upstreamMethod, upstreamURL, strings.NewReader(upstreamBodyStr))
	if err != nil {
		return streamResult{upstreamErr: err}
	}
	upstreamReq.Header.Set("Content-Type", "application/json")
	provider.ApplyAuth(upstreamReq.Header)
	injectProviderSpecificHeaders(provider, upstreamReq.Header, r)
	if isClaude {
		// Anthropic 必需头（与鉴权头无关，固定值）
		upstreamReq.Header.Set("anthropic-version", "2023-06-01")
	}
	if adapterSpec != nil {
		if adapterSpec.SupportsStream() {
			upstreamReq.Header.Set("Accept", "text/event-stream")
		} else {
			// 上游不支持流式：按普通 JSON 请求，稍后由引擎把整包包成单块 SSE。
			upstreamReq.Header.Set("Accept", "application/json, text/plain, */*")
		}
		// 适配器声明的固定头（含 ${token} 渲染）最后写入，优先级最高。
		adapterSpec.ApplyHeaders(upstreamReq.Header, adapterToken)
	} else {
		upstreamReq.Header.Set("Accept", "text/event-stream")
	}

	// 发送请求（流式专用客户端：无整体超时，避免长 SSE 被 30s 截断；取消由 r.Context() 控制，S9）
	resp, err := s.httpClientStream.Do(upstreamReq)
	if err != nil {
		return streamResult{upstreamErr: fmt.Errorf("upstream request failed: %w", err)}
	}

	// 可选鉴权重试：提供者标记为 AuthOptional（如 Pollinations 免费层）且上游返回 401 时，
	// 自动去掉 Authorization 头重试一次（仅 openai 直连路径，adapter 已有自己的令牌刷新）。
	if resp.StatusCode == http.StatusUnauthorized && provider.AuthOptional && provider.APIKey != "" && adapterSpec == nil {
		resp.Body.Close()
		upstreamReq2, err2 := http.NewRequestWithContext(r.Context(), upstreamMethod, upstreamURL, strings.NewReader(upstreamBodyStr))
		if err2 != nil {
			return streamResult{upstreamErr: fmt.Errorf("upstream retry request build failed: %w", err2)}
		}
		upstreamReq2.Header.Set("Content-Type", "application/json")
		injectProviderSpecificHeaders(provider, upstreamReq2.Header, r)
		upstreamReq2.Header.Set("Accept", "text/event-stream")
		resp2, err2 := s.httpClientStream.Do(upstreamReq2)
		if err2 != nil {
			return streamResult{upstreamErr: fmt.Errorf("upstream request failed: %w", err2)}
		}
		resp = resp2
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		// 适配器令牌过期（401/403）：作废缓存，让下一次请求重新握手。
		if adapterSpec != nil && adapterSpec.Preflight != nil &&
			(resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
			translator.InvalidateToken(provider.BaseURL, adapterSpec.ID)
		}
		return streamResult{upstreamErr: &UpstreamError{StatusCode: resp.StatusCode, Body: errBody}}
	}

	// 设置 SSE 头（仅首个成功拿到 200 的候选写一次；failover 续写时跳过）
	firstWriter := headersSent == nil || !*headersSent
	if firstWriter {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		if headersSent != nil {
			*headersSent = true
		}
	}

	flusher, _ := w.(http.Flusher)

	// 发送 vision prelude（如有；仅首个候选发送，failover 不重复）
	if prelude != "" && firstWriter {
		preludeObj := map[string]interface{}{
			"choices": []map[string]interface{}{
				{"delta": map[string]interface{}{"content": prelude}, "index": 0},
			},
		}
		preludeBytes, _ := json.Marshal(preludeObj)
		fmt.Fprintf(w, "data: %s\n\n", string(preludeBytes))
		if flusher != nil {
			flusher.Flush()
		}
	}

	// 首 chunk 添加 🤖 provider · model 前缀
	prefix := fmt.Sprintf("🤖 %s · %s\n\n", provider.Name, candidate.Model)
	prefixSent := false

	// 从流式 chunk 中提取 usage
	var usageObj map[string]interface{}

	// 累计内容（用于续写）
	var accumulated strings.Builder

	result := streamResult{streamOK: false}

	// 使用 Scanner 逐行读取上游 SSE
	// 默认 64KB 行上限会被大 chunk（长 reasoning/大响应）撑爆导致流中断，扩容到 10MB
	// Gemini 源：经翻译器把 Gemini SSE 实时转成 OpenAI SSE 流，后续解析逻辑完全复用。
	scanSrc := io.Reader(resp.Body)
	if adapterSpec != nil {
		label := fmt.Sprintf("%s · %s", provider.Name, candidate.Model)
		if adapterSpec.SupportsStream() {
			scanSrc = translator.NewAdapterSSETranslator(adapterSpec, resp.Body, label)
		} else {
			// 上游只会整包返回：读完后包成单块 OpenAI SSE，对下游客户端仍是标准流式体验。
			oneShot, rerr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
			if rerr != nil {
				return streamResult{upstreamErr: rerr}
			}
			scanSrc = translator.NewAdapterOneShotStream(adapterSpec, oneShot, label)
		}
	} else if isGemini {
		scanSrc = translator.NewGeminiSSETranslator(resp.Body, fmt.Sprintf("%s · %s", provider.Name, candidate.Model))
	} else if isClaude {
		scanSrc = translator.NewClaudeSSETranslator(resp.Body, fmt.Sprintf("%s · %s", provider.Name, candidate.Model))
	} else if isResponses {
		scanSrc = translator.NewResponsesSSETranslator(resp.Body, fmt.Sprintf("%s · %s", provider.Name, candidate.Model))
	}
	scanner := bufio.NewScanner(scanSrc)
	scanner.Buffer(make([]byte, 0, 1<<20), 10<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimPrefix(line, "data:")
		data = strings.TrimSpace(data)

		if data == "[DONE]" {
			fmt.Fprint(w, "data: [DONE]\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			result.streamOK = true
			break
		}

		// 还原 Hermes 工具名
		data = s.meta.RestoreHermesText(data)

		// 解析 chunk：提取 usage、累加 content/reasoning_content、替换 model 字段
		var parsed map[string]interface{}
		if err := json.Unmarshal([]byte(data), &parsed); err == nil {
			// 提取 usage
			if u, ok := parsed["usage"].(map[string]interface{}); ok && u != nil {
				usageObj = u
			}

			// 替换 model 字段（与 Python 原版 obj["model"] = provider·model 一致）
			if _, ok := parsed["model"].(string); ok {
				parsed["model"] = fmt.Sprintf("%s · %s", provider.Name, candidate.Model)
			}

			// 累加 content + reasoning_content（用于续写）
			if choices, ok := parsed["choices"].([]interface{}); ok && len(choices) > 0 {
				if choice, ok := choices[0].(map[string]interface{}); ok {
					if delta, ok := choice["delta"].(map[string]interface{}); ok {
						if c, ok := delta["content"].(string); ok {
							accumulated.WriteString(c)
						}
						if rc, ok := delta["reasoning_content"].(string); ok {
							accumulated.WriteString(rc)
						}
					}
				}
			}

			// 首条 content 消息添加 🤖 前缀
			if !prefixSent {
				if choices, ok := parsed["choices"].([]interface{}); ok && len(choices) > 0 {
					if choice, ok := choices[0].(map[string]interface{}); ok {
						if delta, ok := choice["delta"].(map[string]interface{}); ok {
							if content, ok := delta["content"].(string); ok && content != "" {
								delta["content"] = prefix + content
								prefixSent = true
							}
						}
					}
				}
			}

			dataBytes, _ := json.Marshal(parsed)
			data = string(dataBytes)
		}

		fmt.Fprintf(w, "data: %s\n\n", data)
		if flusher != nil {
			flusher.Flush()
		}
	}

	// 记录 usage（真实值缺失时按字符估算兜底）
	pt := 0
	ct := 0
	if usageObj != nil {
		if v, ok := usageObj["prompt_tokens"].(float64); ok {
			pt = int(v)
		}
		if v, ok := usageObj["completion_tokens"].(float64); ok {
			ct = int(v)
		}
	}
	promptText := promptTextFromMessages(req.Messages)
	completionText := accumulated.String()
	hpt, hct := s.recordStreamUsage(provider, candidate.Model, pt, ct, promptText, completionText)
	// 成本护栏仍按上游真实值（缺失不估算计费），避免误扣费
	s.recordStreamBudget(provider, candidate.Model, pt, ct)
	result.totalTokens = hpt + hct
	result.promptTokens = hpt
	result.completionTokens = hct

	if err := scanner.Err(); err != nil {
		result.upstreamErr = err
	}

	result.accumulated = accumulated.String()
	return result
}

// forwardStreamWithFailover 流式转发（带自动故障转移 + 续写）
func (s *Server) forwardStreamWithFailover(w http.ResponseWriter, r *http.Request, req *ChatRequest, body []byte, candidates []*engine.Candidate, prelude, cacheExactKey, cachePromptNorm string, vkey *storage.VKey, sessionID string) {
	maxAttempts := 1

	accumulated := ""
	allBusy := true      // 只要有一个候选真正尝试过，就置 false
	headersSent := false // SSE 头是否已写（跨候选共享，failover 时不重复写头/prelude）

	for attempt := 0; attempt < maxAttempts; attempt++ {
		for _, candidate := range candidates {
			modelKey := candidate.Provider.Name + "||" + candidate.Model

			// 并发护栏：单 provider 槽位（流式期间占用）
			rel, pok := s.guard.TryAcquire(candidate.Provider.Name, 0, candidate.Provider.MaxConcurrency, 3*time.Second)
			if !pok {
				continue
			}
			allBusy = false

			// 如果有续写内容，注入续写提示（与 Python 原版一致）
			requestBody := body
			if accumulated != "" {
				requestBody = injectContinuation(body, accumulated)
			}

			started := time.Now()
			result := s.streamFromUpstream(w, r, req, requestBody, candidate, prelude, &headersSent)
			rel() // 流已结束（成功或失败），释放 provider 槽位

			if result.streamOK {
				// 完整成功：记录熔断成功
				s.circuits.RecordSuccess(modelKey)
				// C2/C3：记录末次成功 provider + 会话绑定（会话亲和，平手优先）
				s.lkgp.Record(req.Model, candidate.Provider.Name)
				if sessionID != "" {
					s.affinity.Bind(sessionID, candidate.Provider.Name)
				}
				// Prometheus 指标（流式成功；token 明细已在 streamFromUpstream 记账，
				// 此处直方图只需耗时 + 计数，token 按 usage 拆分不可得时记 0）
				s.metrics.ObserveRequest(candidate.Provider.Name, candidate.Model, "ok", time.Since(started), result.promptTokens, result.completionTokens)
				// 虚拟密钥：记录当日用量（1 请求 + 本次 Token）
				s.recordVKeyUsage(vkey, 1, result.totalTokens)
				calllog.Append(calllog.Entry{
					Provider: candidate.Provider.Name,
					Model:    candidate.Model,
					Status:   "ok",
					Tokens:   result.totalTokens,
				})
				// 写入响应缓存（流式：完整累计内容 + 之前 failover 的累计）
				fullContent := accumulated + result.accumulated
				if cacheExactKey != "" && fullContent != "" {
					s.cache.PutContent(req.Model, cacheExactKey, cachePromptNorm, fullContent, false)
				}
				// 钩子：请求成功
				s.hookDispatcher().Fire(HookEventDone, req.Model, candidate.Provider.Name, true, result.totalTokens, "", "")
				return
			}

			if result.upstreamErr != nil {
				// 失败：按错误类型归因记录熔断（429/鉴权/配额/客户端取消不误杀）
				kind := engine.ClassifyNetErr(result.upstreamErr)
				status := 0
				netErr := true
				if ue, ok := result.upstreamErr.(*UpstreamError); ok {
					kind = engine.ClassifyStatus(ue.StatusCode)
					status = ue.StatusCode
					netErr = false
				}
				s.circuits.RecordFailureWithType(modelKey, kind)
				s.metrics.ObserveRequest(candidate.Provider.Name, candidate.Model, "fail", time.Since(started), 0, 0)
				s.recordPenalty(candidate.Provider.Name, candidate.Model, status, netErr)
				errMsg := result.upstreamErr.Error()
				if ue, ok := result.upstreamErr.(*UpstreamError); ok {
					errMsg = fmt.Sprintf("HTTP %d", ue.StatusCode)
				}
				calllog.Append(calllog.Entry{
					Provider: candidate.Provider.Name,
					Model:    candidate.Model,
					Status:   "fail",
					Error:    errMsg,
				})
				// 累加已收到的内容（用于续写）
				if result.accumulated != "" {
					accumulated += result.accumulated
				}
				fmt.Printf("[stream] failover from %s (attempt %d): %v\n", modelKey, attempt, result.upstreamErr)
				continue
			}
		}
	}

	// 并发全忙：所有候选 provider 槽位都拿不到，返回 429（尚未写 SSE 头，可安全返回）
	if allBusy {
		http.Error(w, "too many concurrent requests", http.StatusTooManyRequests)
		s.hookDispatcher().Fire(HookEventFailed, req.Model, "", true, 0, "concurrency limit", "")
		return
	}

	// 钩子：全部候选失败
	s.hookDispatcher().Fire(HookEventFailed, req.Model, "", true, 0, "all candidates failed", "")

	// 全部候选均失败：对齐 Python 原版（app.py:1922），先推「回复中断」提示 delta，
	// 再发送 [DONE]，避免用户看到一段「看似完整、实际被截断」的回答且无任何报错。
	warn := map[string]interface{}{
		"choices": []map[string]interface{}{
			{"delta": map[string]interface{}{"content": "\n\n⚠️ 所有模型均失败，回复中断。"}, "index": 0},
		},
	}
	warnBytes, _ := json.Marshal(warn)
	fmt.Fprintf(w, "data: %s\n\n", string(warnBytes))
	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

// injectStreamOptions 向请求体注入 stream_options: {"include_usage": true}
func injectStreamOptions(body string) string {
	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		return body
	}
	// 只在 stream=true 时注入
	if stream, ok := parsed["stream"].(bool); !ok || !stream {
		return body
	}
	parsed["stream_options"] = map[string]interface{}{
		"include_usage": true,
	}
	out, _ := json.Marshal(parsed)
	return string(out)
}

// injectContinuation 向请求体注入续写上下文（与 Python 原版一致）
func injectContinuation(body []byte, accumulated string) []byte {
	var parsed map[string]interface{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return body
	}
	msgs, ok := parsed["messages"].([]interface{})
	if !ok {
		return body
	}
	// 追加 assistant 已回复内容 + 用户续写请求
	msgs = append(msgs, map[string]interface{}{
		"role":    "assistant",
		"content": accumulated,
	})
	msgs = append(msgs, map[string]interface{}{
		"role":    "user",
		"content": "请继续上面的回复，从中断处接着写。",
	})
	parsed["messages"] = msgs
	out, _ := json.Marshal(parsed)
	return out
}

// recordStreamBudget 流式请求预算记账（成本护栏）
func (s *Server) recordStreamBudget(provider *config.Provider, model string, promptTokens, completionTokens int) {
	if s.budget == nil {
		return
	}
	var priceIn, priceOut float64
	if s.catalog != nil {
		if e := s.catalog.Lookup(model); e != nil {
			priceIn, priceOut = e.PriceIn, e.PriceOut
		}
	}
	s.budget.AddCost(model, provider.Name, promptTokens, completionTokens, priceIn, priceOut)
	// A11：同非流式路径，跨过阈值时推送 quota_exceeded 钩子
	s.checkBudgetThreshold()
}

// recordStreamUsage 记录流式请求的 usage（返回混合计数后的 pt/ct 供调用方复用）。
// 上游真实值 > 0 时采用真实值；缺失或为 0 时用 promptText/completionText 估算兜底。
func (s *Server) recordStreamUsage(provider *config.Provider, model string, promptTokens, completionTokens int, promptText, completionText string) (int, int) {
	pt := storage.HybridTokens(promptTokens, promptText)
	ct := storage.HybridTokens(completionTokens, completionText)
	if s.usage == nil || (pt == 0 && ct == 0) {
		return pt, ct
	}
	_ = s.usage.Append(storage.UsageRecord{
		Time:             time.Now(),
		Provider:         provider.Name,
		Model:            s.normalizeUsageModel(model),
		PromptTokens:     pt,
		CompletionTokens: ct,
		TotalTokens:      pt + ct,
	})
	return pt, ct
}
