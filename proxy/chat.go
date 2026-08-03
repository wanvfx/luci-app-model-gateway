package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/wanvfx/luci-app-model-gateway/calllog"
	"github.com/wanvfx/luci-app-model-gateway/config"
	"github.com/wanvfx/luci-app-model-gateway/engine"
	"github.com/wanvfx/luci-app-model-gateway/proxy/translator"
	"github.com/wanvfx/luci-app-model-gateway/storage"
	"github.com/wanvfx/luci-app-model-gateway/vision"
)

// injectProviderSpecificHeaders 为已知免 Key 提供者注入额外必需头。
// 与 OmniRoute 的 NOAUTH_PROVIDERS + provider-specific executor 对齐：
//   - 某些 provider 即使不需要 API key，也需要固定头/会话标识才能正常调用。
//   - incomingReq 为原始客户端请求（可为 nil，仅用于转发客户端携带的 provider 专属头）。
func injectProviderSpecificHeaders(provider *config.Provider, h http.Header, incomingReq *http.Request) {
	if provider == nil || h == nil {
		return
	}
	id := strings.ToLower(provider.Name)
	base := strings.ToLower(provider.BaseURL)

	switch {
	case id == "opencode" || strings.Contains(base, "opencode.ai"):
		injectOpenCodeHeaders(h, incomingReq)
	case id == "pollinations" || strings.Contains(base, "pollinations.ai"):
		injectPollinationsFingerprint(h)
	}
}

// injectOpenCodeHeaders 为 OpenCode 注入 CLI 身份头 / 会话标识。
// 参考 OmniRoute open-sse/utils/opencodeHeaders.ts + open-sse/executors/opencode.ts：
//   - 合成 x-opencode-session / x-opencode-request（随机 UUID）
//   - 设置 x-opencode-client = "cli"、x-opencode-project = "default"
//   - 转发客户端携带的 x-opencode-* / x-session-id / x-title（若有）
//   - 补齐 User-Agent = opencode-cli/1.0.0（若客户端未带）
func injectOpenCodeHeaders(h http.Header, incomingReq *http.Request) {
	// 1. 转发客户端携带的 OpenCode 专属头（大小写不敏感）
	if incomingReq != nil {
		for _, key := range []string{
			"x-opencode-session", "x-opencode-request", "x-opencode-project", "x-opencode-client",
			"x-session-id", "x-title",
		} {
			if v := incomingReq.Header.Get(key); v != "" {
				h.Set(key, v)
			}
		}
	}
	// 2. 补齐缺失的必需头（Cloudflare VPS egress 需要）
	if h.Get("User-Agent") == "" && h.Get("user-agent") == "" {
		h.Set("User-Agent", "opencode-cli/1.0.0")
	}
	if h.Get("x-opencode-client") == "" {
		h.Set("x-opencode-client", "cli")
	}
	if h.Get("x-opencode-project") == "" {
		h.Set("x-opencode-project", "default")
	}
	if h.Get("x-opencode-request") == "" {
		h.Set("x-opencode-request", newUUID())
	}
	if h.Get("x-opencode-session") == "" {
		h.Set("x-opencode-session", newUUID())
	}
}

// injectPollinationsFingerprint 为匿名 Pollinations 请求合成浏览器指纹头。
// 参考 OmniRoute open-sse/services/sessionPool/session.ts buildHeaders()。
// 由于 Go 单二进制无 session pool，此处用固定 synthetic fingerprint + 随机 request id
// 模拟匿名浏览器身份，绕过 Pollinations 匿名限流。
func injectPollinationsFingerprint(h http.Header) {
	if h.Get("User-Agent") == "" && h.Get("user-agent") == "" {
		h.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36")
	}
	if h.Get("Accept-Language") == "" {
		h.Set("Accept-Language", "en-US,en;q=0.9")
	}
	// Sec-CH-UA 系列（Chromium 客户端提示）
	if h.Get("Sec-CH-UA") == "" {
		h.Set("Sec-CH-UA", `"Chromium";v="150", "Google Chrome";v="150"`)
	}
	if h.Get("Sec-CH-UA-Mobile") == "" {
		h.Set("Sec-CH-UA-Mobile", "?0")
	}
	if h.Get("Sec-CH-UA-Platform") == "" {
		h.Set("Sec-CH-UA-Platform", `"Windows"`)
	}
}

// newUUID 生成一个 RFC4122 风格的随机 UUID（用于合成会话/请求标识）。
func newUUID() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// 极端降级：用时间戳拼一个可读 id
		return fmt.Sprintf("%x-%x-%x-%x-%x", time.Now().UnixNano(), time.Now().UnixNano(), time.Now().UnixNano(), time.Now().UnixNano(), time.Now().UnixNano())
	}
	return fmt.Sprintf("%x-%x-%x-%x-%x", buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:])
}

// transformRequestBody 对特定 provider 的 OpenAI 兼容请求体做运行时转换。
// 目前支持：
//   - OpenCode： stripping "client_metadata"（上游 400）
//   - Pollinations： response_format 为 json_object/json_schema 时注�� jsonMode=true
func transformRequestBody(provider *config.Provider, body []byte) []byte {
	if provider == nil || len(body) == 0 {
		return body
	}
	id := strings.ToLower(provider.Name)
	base := strings.ToLower(provider.BaseURL)

	if id == "opencode" || strings.Contains(base, "opencode.ai") {
		body = stripClientMetadata(body)
	}
	if id == "pollinations" || strings.Contains(base, "pollinations.ai") {
		body = injectPollinationsJsonMode(body)
	}
	return body
}

// stripClientMetadata 从 OpenAI 兼容请求体中移除 "client_metadata" 字段。
// OpenCode 上游会对该字段 400，参考 OmniRoute base executor strip 逻辑。
func stripClientMetadata(body []byte) []byte {
	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	delete(m, "client_metadata")
	b, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return b
}

// injectPollinationsJsonMode 当请求体包含 response_format 为 json_object / json_schema 时，
// 注入 jsonMode: true（Pollinations 要求此字段才能正确返回 JSON）。
// 参考 OmniRoute pollinations executor transformRequest。
func injectPollinationsJsonMode(body []byte) []byte {
	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	rf, ok := m["response_format"].(map[string]interface{})
	if !ok {
		return body
	}
	t, _ := rf["type"].(string)
	if t == "json_object" || t == "json_schema" {
		m["jsonMode"] = true
	}
	b, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return b
}

// UpstreamError 携带上游 HTTP 状态码和响应体的错误
type UpstreamError struct {
	StatusCode int
	Body       []byte
}

// preferredProvider 返回会话当前绑定的 provider（未绑定返回空串），供会话亲和前置使用。
func (s *Server) preferredProvider(sessionID string) string {
	if sessionID == "" || s.affinity == nil {
		return ""
	}
	p, _ := s.affinity.Lookup(sessionID)
	return p
}

// clientIP 从请求中提取客户端 IP（去掉端口）。
// S7 安全修复：仅当直连地址（RemoteAddr）命中受信任反代列表时，才采信
// X-Forwarded-For 头（取首个），否则一律使用 RemoteAddr。
// 这避免客户端伪造 XFF 绕过 IP 白名单（allowClients）或劫持会话亲和。
// trustedNets 为空表示不信任任何 XFF，所有请求都以 RemoteAddr 为准。
func clientIP(r *http.Request, trustedNets []net.IPNet) string {
	remote := remoteAddrIP(r.RemoteAddr)
	if len(trustedNets) > 0 && remote != "" {
		if ip := net.ParseIP(remote); ip != nil {
			for _, n := range trustedNets {
				if n.Contains(ip) {
					if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
						if i := strings.IndexByte(xff, ','); i >= 0 {
							return strings.TrimSpace(xff[:i])
						}
						return strings.TrimSpace(xff)
					}
					break
				}
			}
		}
	}
	return remote
}

// remoteAddrIP 从 "host:port" / "[::1]:port" 中提取 IP 部分。
func remoteAddrIP(remoteAddr string) string {
	host := remoteAddr
	if i := strings.LastIndexByte(host, ':'); i >= 0 {
		// 可能是 IPv6 [::1]:port，需按 ']' 判断
		if strings.HasPrefix(host, "[") {
			if j := strings.IndexByte(host, ']'); j >= 0 {
				return host[1:j]
			}
		}
		return host[:i]
	}
	return host
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
			// 配额耗尽钩子（G1：微信推送/钉钉机器人等可接此通知）
			s.hookDispatcher().Fire(HookEventQuotaExceeded, "", "", false, 0, "virtual key daily quota exceeded", "")
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"virtual key daily quota exceeded","type":"quota_exceeded"}}`))
			return
		}
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	// S8：请求体大小限制（4MB），防止大请求体打满内存（DoS）
	const maxRequestBytes = 4 << 20 // 4MB
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	vkey := ca.vkey // 非 nil 表示以虚拟密钥鉴权，成功时需记录用量

	// 会话标识（C3 会话亲和）：优先用客户端显式 X-Session-Id；
	// 否则按「身份(vkey 或 admin) + 客户端 IP」推断一个稳定标识，使同一对话固定走同一 provider。
	sessionID := strings.TrimSpace(r.Header.Get("X-Session-Id"))
	if sessionID == "" {
		id := "admin"
		if vkey != nil {
			id = "vk:" + vkey.ID
		}
		sessionID = id + "@" + clientIP(r, s.cfg.Load().TrustedNets())
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

	// 别名解析（G8）：友好名 -> 内部模型/路由组/auto，透明重写请求
	if req.Model != "" {
		if resolved := s.resolveAlias(req.Model); resolved != req.Model {
			body = replaceModelInBody(body, req.Model, resolved)
			req.Model = resolved
		}
	}

	// 虚拟密钥模型白名单（安全修复）：配置了 AllowedModels 的子密钥只能调用白名单内的模型。
	// 此前 Allowed() 从未被调用，导致白名单形同虚设（越权漏洞）。
	if vkey != nil && s.vkeys.Load() != nil && !s.vkeys.Load().Allowed(vkey, req.Model) {
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
			s.fireBudgetThreshold(st.Status)
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
		s.handleStreamChat(w, r, &req, body, visionPrelude, exactKey, promptNorm, vkey, sessionID)
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
		Strict:            s.cfg.Load().StrictCapability(),
		RequiredCaps:      requiredCaps,
		Content:           string(body),
		PreferredProvider: s.preferredProvider(sessionID),
		CheapFirst:        s.budgetDegradeActive(),
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
			b, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				lastErr = &UpstreamError{StatusCode: http.StatusBadGateway, Body: []byte(fmt.Sprintf("read upstream response body failed: %v", err))}
				continue
			}

			if resp.StatusCode != http.StatusOK {
				lastErr = &UpstreamError{StatusCode: resp.StatusCode, Body: b}
				// 归因熔断：429/401/403/402 等不误杀，仅 5xx 跳闸
				s.circuits.RecordFailureWithType(modelKey, engine.ClassifyStatus(resp.StatusCode))
				// C1 精确冷却：429 限流时解析 Retry-After / X-RateLimit-Reset 精确冷却该候选
				if resp.StatusCode == http.StatusTooManyRequests {
					s.cooldown.SetFromHeaders(candidate.Provider.Name+"-"+candidate.Model, resp.Header)
				}
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
			// C2/C3：记录末次成功 provider + 会话绑定（会话亲和，平手优先）
			s.lkgp.Record(req.Model, candidate.Provider.Name)
			if sessionID != "" {
				s.affinity.Bind(sessionID, candidate.Provider.Name)
			}

			// 解析响应提取 usage（与 Python 原版一致）
			promptTok, complTok := 0, 0
			var chatResp ChatResponse
			if err := json.Unmarshal(b, &chatResp); err == nil {
				// 替换 model 字段为 provider · model（与 Python 原版一致）
				chatResp.Model = fmt.Sprintf("%s · %s", candidate.Provider.Name, candidate.Model)

				// 添加 🤖 provider · model 前缀到 content（F7：仅纯文本且前缀开关开启时注入；
				// 结构化输出（JSON/数组）不加前缀，避免污染机器可读结果）。
				if s.cfg.Load().ContentPrefix() && len(chatResp.Choices) > 0 {
					content := chatResp.Choices[0].Message.Content
					if content != "" && !looksLikeJSON(content) {
						prefix := fmt.Sprintf("🤖 %s · %s\n\n", candidate.Provider.Name, candidate.Model)
						chatResp.Choices[0].Message.Content = prefix + content
					}
				}
				// Hermes 还原
				if len(chatResp.Choices) > 0 {
					chatResp.Choices[0].Message.Content = s.meta.RestoreHermesText(chatResp.Choices[0].Message.Content)
				}
				b, _ = json.Marshal(chatResp)

				// 混合 token 计数的兜底文本
				promptText := promptTextFromMessages(req.Messages)
				completionText := ""
				if len(chatResp.Choices) > 0 {
					completionText = chatResp.Choices[0].Message.Content
				}

				// 记录 usage 到 usage.jsonl（真实值缺失时按字符估算兜底）
				s.recordUsage(candidate.Provider, candidate.Model, &chatResp, promptText, completionText)
				// 预算记账（成本护栏，仍按上游真实值）
				s.recordBudget(candidate.Provider, candidate.Model, &chatResp)
				// 写入响应缓存（非流式）
				if cacheCfg.Enabled {
					s.cache.PutContent(req.Model, exactKey, promptNorm, chatResp.Choices[0].Message.Content, false)
				}
				// 钩子：请求成功（混合 token 计数）
				pt := 0
				ct := 0
				if chatResp.Usage != nil {
					pt = chatResp.Usage.PromptTokens
					ct = chatResp.Usage.CompletionTokens
				}
				pt = storage.HybridTokens(pt, promptText)
				ct = storage.HybridTokens(ct, completionText)
				tokens := pt + ct
				promptTok = pt
				complTok = ct
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
		errMsg = sanitizeClientError(lastErr.Error())
	}
	respBody, _ := json.Marshal(map[string]interface{}{
		"detail": fmt.Sprintf("所有候选模型均失败: %s", errMsg),
	})
	s.hookDispatcher().Fire(HookEventFailed, req.Model, "", false, 0, errMsg, "")
	_, _ = w.Write(respBody)
}

// replaceModelInBody 在请求体中替换 model 字段。
// P2-3：优先走 JSON 解析后安全修改 model 字段再序列化，避免纯字符串替换在
// new 含引号/大括号等字符时破坏 JSON 结构（产生无效 JSON 被上游拒绝）。
// 解析失败或顶层无匹配 model 字段时回退到原字符串替换（兼容非标准/紧凑格式）。
func replaceModelInBody(body []byte, old, new string) []byte {
	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err == nil {
		if cur, ok := m["model"].(string); ok && cur == old {
			m["model"] = new
			if out, err := json.Marshal(m); err == nil {
				return out
			}
		}
	}
	// 回退：兼容含空白/紧凑两种 JSON 字面量格式
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
	// A11：记账后立即检查是否跨过告警线/超额线，跨越瞬间推送 quota_exceeded 钩子（每日每档去重）
	s.checkBudgetThreshold()
}

// looksLikeJSON 快速判断内容是否像 JSON 对象/数组（用于 F7：结构化输出不注入 content 前缀）。
// 仅检查去空白后的首字符，不解析完整 JSON，开销极低。
func looksLikeJSON(s string) bool {
	t := strings.TrimSpace(s)
	if t == "" {
		return false
	}
	c := t[0]
	return c == '{' || c == '['
}

// P2-8: sanitizeClientError 从客户端错误信息中移除敏感数据（API key、token 等），
// 防止上游错误响应中的凭证泄露到前端。
func sanitizeClientError(s string) string {
	// 替换常见 API key 模式
	s = regexp.MustCompile(`(?i)(sk-[a-zA-Z0-9]{20,}|key\s*[:=]\s*[a-zA-Z0-9_\-]{16,}|bearer\s+[a-zA-Z0-9_\-\.]+)`).ReplaceAllString(s, "[REDACTED]")
	// 截断过长的错误信息
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
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
		if len(s.cfg.Load().Providers) == 0 {
			return nil, fmt.Errorf("no providers configured")
		}
		provider = s.cfg.Load().Providers[0]
	}

	// Gemini 原生协议：走独立翻译分支，直连 openai 路径保持不变（零行为变更）。
	if strings.EqualFold(provider.FormatOrDefault(), translator.FormatGemini) {
		return s.forwardChatGemini(ctx, originalModel, targetModel, body)
	}
	// Claude 原生协议：走独立翻译分支。
	if strings.EqualFold(provider.FormatOrDefault(), translator.FormatClaude) {
		return s.forwardChatClaude(ctx, originalModel, targetModel, body)
	}
	// OpenAI Responses API 原生协议：走独立翻译分支。
	if strings.EqualFold(provider.FormatOrDefault(), translator.FormatResponses) {
		return s.forwardChatResponses(ctx, originalModel, targetModel, body)
	}
	// 通用协议适配器（Phase C）：format 命中已注册的 AdapterSpec 时，
	// 按声明式规格与上游对话（自定义路径/握手/请求体/响应提取）。未命中则继续走 openai 直连。
	if spec := translator.LookupAdapter(provider.FormatOrDefault()); spec != nil {
		return s.forwardChatAdapter(ctx, spec, provider, targetModel, body)
	}

	// 替换模型名（使用别名解析）。
	// 安全修复：只替换 JSON 的 "model" 字段，不再对整个 body 做子串替换，
	// 避免用户 prompt 内容恰好包含模型名时被误改（污染对话内容）。
	resolvedModel := s.meta.ResolveModel(targetModel)
	reqBody := replaceModelInBody(body, originalModel, resolvedModel)
	reqBody = transformRequestBody(provider, reqBody)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, provider.BaseURL+"/chat/completions", bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	provider.ApplyAuth(req.Header)
	injectProviderSpecificHeaders(provider, req.Header, nil)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	// 可选鉴权重试：提供者标记为 AuthOptional（如 Pollinations 免费层）且上游返回 401 时，
	// 自动去掉 Authorization 头重试一次，让免费模型即使用户填了无效 key 也能工作。
	if resp.StatusCode == http.StatusUnauthorized && provider.AuthOptional && provider.APIKey != "" {
		log.Printf("[auth-optional-retry] provider=%s model=%s status=401 removing auth header for retry", provider.Name, targetModel)
		resp.Body.Close()
		req2, err2 := http.NewRequestWithContext(ctx, http.MethodPost, provider.BaseURL+"/chat/completions", bytes.NewReader(reqBody))
		if err2 != nil {
			return nil, err2
		}
		req2.Header.Set("Content-Type", "application/json")
		injectProviderSpecificHeaders(provider, req2.Header, nil)
		return s.httpClient.Do(req2)
	}
	return resp, nil
}

// buildSyntheticResponse 用给定状态码与 body 构造一个 *http.Response（用于把上游非 HTTP 原生
// 协议的响应包成调用方期望的 OpenAI 形式后再回传，避免改动 handleChatCompletions 的成功/失败分支）。
func buildSyntheticResponse(status int, body []byte) *http.Response {
	return &http.Response{
		StatusCode:    status,
		Status:        fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header{"Content-Type": []string{"application/json"}},
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
	}
}

// forwardChatAdapter 按声明式适配器规格与非 OpenAI 兼容上游对话（Phase C）。
//
// 流程：预检握手取令牌 → 渲染请求体模板 → 发请求 → 按 content_path 提取文本 →
// 包成 OpenAI chat.completion 返回。上游返回 401/403 时作废令牌并重试一次握手，
// 覆盖「令牌过期」这一最常见的失败模式。
func (s *Server) forwardChatAdapter(ctx context.Context, spec *translator.AdapterSpec, provider *config.Provider, targetModel string, body []byte) (*http.Response, error) {
	resolvedModel := s.meta.ResolveModel(targetModel)
	reqBody := replaceModelInBody(body, targetModel, resolvedModel)

	doOnce := func() (*http.Response, []byte, error) {
		token, err := spec.EnsureToken(provider.BaseURL, s.httpClient)
		if err != nil {
			return nil, nil, fmt.Errorf("adapter %s preflight: %w", spec.ID, err)
		}
		upBody, _, _, err := spec.BuildRequestBody(reqBody, token)
		if err != nil {
			return nil, nil, err
		}
		url := spec.ChatURL(provider.BaseURL, token)
		req, err := http.NewRequestWithContext(ctx, spec.HTTPMethod(), url, bytes.NewReader(upBody))
		if err != nil {
			return nil, nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		spec.ApplyHeaders(req.Header, token)
		// 免 Key 提供者通常无鉴权头；若用户填了 key 仍然按常规注入（部分站点填 key 可提额度）。
		provider.ApplyAuth(req.Header)
		injectProviderSpecificHeaders(provider, req.Header, nil)

		resp, err := s.httpClient.Do(req)
		if err != nil {
			return nil, nil, err
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		return resp, raw, nil
	}

	resp, raw, err := doOnce()
	if err != nil {
		return nil, err
	}
	// 令牌过期：作废缓存后重握手重试一次。
	if (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) && spec.Preflight != nil {
		translator.InvalidateToken(provider.BaseURL, spec.ID)
		if resp2, raw2, err2 := doOnce(); err2 == nil {
			resp, raw = resp2, raw2
		}
	}

	if resp.StatusCode != http.StatusOK {
		return buildSyntheticResponse(resp.StatusCode, spec.ErrorToOpenAI(raw)), nil
	}
	// ToOpenAIResponse 内部会自动识别「非流式请求却回了 SSE」并收敛成整段文本。
	openAIBody, err := spec.ToOpenAIResponse(raw, fmt.Sprintf("%s · %s", provider.Name, targetModel))
	if err != nil {
		return buildSyntheticResponse(http.StatusBadGateway, spec.ErrorToOpenAI(raw)), nil
	}
	return buildSyntheticResponse(http.StatusOK, openAIBody), nil
}

// forwardChatGemini 以 Gemini 原生 generateContent 协议转发非流式请求，并把响应翻译回 OpenAI 形式。
// 上游错误（非 200）也会被翻译为 OpenAI 错误体，状态码保持不变，交由调用方统一处理。
func (s *Server) forwardChatGemini(ctx context.Context, originalModel, targetModel string, body []byte) (*http.Response, error) {
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
		if len(s.cfg.Load().Providers) == 0 {
			return nil, fmt.Errorf("no providers configured")
		}
		provider = s.cfg.Load().Providers[0]
	}

	geminiBody, model, _, err := translator.ToGeminiBody(body)
	if err != nil {
		return nil, err
	}
	// G5 思考预算：gemini 注入 thinkingConfig.thinkingBudget
	if provider.ThinkingBudget > 0 {
		geminiBody = []byte(injectGeminiThinking(string(geminiBody), provider.ThinkingBudget))
	}
	url := strings.TrimRight(provider.BaseURL, "/") + "/models/" + model + ":generateContent"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(geminiBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	provider.ApplyAuth(req.Header)
	injectProviderSpecificHeaders(provider, req.Header, nil)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	b, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("read upstream response body failed: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return buildSyntheticResponse(resp.StatusCode, translator.GeminiErrorToOpenAI(b)), nil
	}
	openAIBody, err := translator.GeminiToOpenAI(b, fmt.Sprintf("%s · %s", provider.Name, model))
	if err != nil {
		// 解析失败，原样回退（调用方会透传）
		return buildSyntheticResponse(http.StatusOK, b), nil
	}
	return buildSyntheticResponse(http.StatusOK, openAIBody), nil
}

// forwardChatClaude 以 Anthropic /v1/messages 原生协议转发非流式请求，并把响应翻译回 OpenAI 形式。
// 上游错误（非 200）也会被翻译为 OpenAI 错误体，状态码保持不变，交由调用方统一处理。
func (s *Server) forwardChatClaude(ctx context.Context, originalModel, targetModel string, body []byte) (*http.Response, error) {
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
		if len(s.cfg.Load().Providers) == 0 {
			return nil, fmt.Errorf("no providers configured")
		}
		provider = s.cfg.Load().Providers[0]
	}

	resolvedModel := s.meta.ResolveModel(targetModel)
	claudeBody, model, err := translator.ToClaudeBody([]byte(replaceModelInBody(body, originalModel, resolvedModel)))
	if err != nil {
		return nil, err
	}
	// G5 思考预算：claude 注入 thinking.{type:enabled, budget_tokens}
	if provider.ThinkingBudget > 0 {
		claudeBody = []byte(injectClaudeThinking(string(claudeBody), provider.ThinkingBudget))
	}
	url := strings.TrimRight(provider.BaseURL, "/") + "/v1/messages"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(claudeBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	provider.ApplyAuth(req.Header)
	injectProviderSpecificHeaders(provider, req.Header, nil)
	// Anthropic 必需头（与鉴权头无关，固定值）
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	b, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("read upstream response body failed: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return buildSyntheticResponse(resp.StatusCode, translator.ClaudeErrorToOpenAI(b)), nil
	}
	openAIBody, err := translator.ClaudeToOpenAI(b, fmt.Sprintf("%s · %s", provider.Name, model))
	if err != nil {
		// 解析失败，原样回退（调用方会透传）
		return buildSyntheticResponse(http.StatusOK, b), nil
	}
	return buildSyntheticResponse(http.StatusOK, openAIBody), nil
}

// injectClaudeThinking 向 Claude 原生请求体注入思考预算（extended thinking）。
// budget 必须 < max_tokens，否则上游会报错（调用方应保证 thinking_budget < 请求的 max_tokens）。
func injectClaudeThinking(body string, budget int) string {
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		return body
	}
	m["thinking"] = map[string]interface{}{"type": "enabled", "budget_tokens": budget}
	b, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return string(b)
}

// injectGeminiThinking 向 Gemini 原生请求体注入思考预算（thinkingConfig.thinkingBudget）。
func injectGeminiThinking(body string, budget int) string {
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		return body
	}
	gc, _ := m["generationConfig"].(map[string]interface{})
	if gc == nil {
		gc = map[string]interface{}{}
	}
	gc["thinkingConfig"] = map[string]interface{}{"thinkingBudget": budget}
	m["generationConfig"] = gc
	b, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return string(b)
}

// forwardChatResponses 以 OpenAI Responses API（/v1/responses）原生协议转发非流式请求，并把响应翻译回 OpenAI 形式。
// 上游错误（非 200）也会被翻译为 OpenAI 错误体，状态码保持不变，交由调用方统一处理。
func (s *Server) forwardChatResponses(ctx context.Context, originalModel, targetModel string, body []byte) (*http.Response, error) {
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
		if len(s.cfg.Load().Providers) == 0 {
			return nil, fmt.Errorf("no providers configured")
		}
		provider = s.cfg.Load().Providers[0]
	}

	resolvedModel := s.meta.ResolveModel(targetModel)
	responsesBody, model, err := translator.ToResponsesBody([]byte(replaceModelInBody(body, originalModel, resolvedModel)))
	if err != nil {
		return nil, err
	}
	url := strings.TrimRight(provider.BaseURL, "/") + "/v1/responses"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(responsesBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	provider.ApplyAuth(req.Header)
	injectProviderSpecificHeaders(provider, req.Header, nil)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	b, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("read upstream response body failed: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return buildSyntheticResponse(resp.StatusCode, translator.ResponsesErrorToOpenAI(b)), nil
	}
	openAIBody, err := translator.ResponsesToOpenAI(b, fmt.Sprintf("%s · %s", provider.Name, model))
	if err != nil {
		// 解析失败，原样回退（调用方会透传）
		return buildSyntheticResponse(http.StatusOK, b), nil
	}
	return buildSyntheticResponse(http.StatusOK, openAIBody), nil
}

// handleStreamChat 流式转发
func (s *Server) handleStreamChat(w http.ResponseWriter, r *http.Request, req *ChatRequest, body []byte, prelude, cacheExactKey, cachePromptNorm string, vkey *storage.VKey, sessionID string) {
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
	// F8：图像能力判定统一走 visionDetector.Detect，与 handleChatCompletions 共享入口口径一致；
	// 不再用 strings.Contains 粗判，避免 base64 图片("data:image")误判或遗漏真实图像结构。
	requiredCaps := []string{}
	visionDetector := vision.NewDetector(s.cfg.Load().VisionRouter(), s.cfg.Load().VisionMaxTokens())
	if hasImage, _ := visionDetector.Detect(body); hasImage {
		requiredCaps = append(requiredCaps, "vision")
	}
	pickOpts := engine.PickOptions{
		Strict:            s.cfg.Load().StrictCapability(),
		RequiredCaps:      requiredCaps,
		Content:           string(body),
		PreferredProvider: s.preferredProvider(sessionID),
		CheapFirst:        s.budgetDegradeActive(),
	}
	candidates := s.router.Load().PickCandidates(req.Model, s.cfg.Load().Providers, disabled, pickOpts)
	if len(candidates) == 0 {
		http.Error(w, "no available providers", http.StatusBadGateway)
		s.hookDispatcher().Fire(HookEventFailed, req.Model, "", true, 0, "no available providers", "")
		return
	}

	s.forwardStreamWithFailover(w, r, req, body, candidates, prelude, cacheExactKey, cachePromptNorm, vkey, sessionID)
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

// recordUsage 记录 usage 到 usage.jsonl。
// 上游真实值 > 0 时采用真实值；缺失或为 0 时用 promptText/completionText 估算兜底（混合 token 计数）。
func (s *Server) recordUsage(provider *config.Provider, model string, resp *ChatResponse, promptText, completionText string) {
	pt, ct := 0, 0
	if resp.Usage != nil {
		pt = resp.Usage.PromptTokens
		ct = resp.Usage.CompletionTokens
	}
	pt = storage.HybridTokens(pt, promptText)
	ct = storage.HybridTokens(ct, completionText)
	if s.usage == nil || (pt == 0 && ct == 0) {
		return
	}
	_ = s.usage.Append(storage.UsageRecord{
		Time:             time.Now(),
		Provider:         provider.Name,
		Model:            s.normalizeUsageModel(model),
		RequestID:        resp.ID,
		PromptTokens:     pt,
		CompletionTokens: ct,
		TotalTokens:      pt + ct,
	})
}

// promptTextFromMessages 拼接请求消息内容，作为 prompt token 估算的兜底文本。
func promptTextFromMessages(msgs []Message) string {
	var sb strings.Builder
	for _, m := range msgs {
		sb.WriteString(m.Content)
	}
	return sb.String()
}
