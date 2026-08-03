package translator

// adapter.go —— 通用协议适配引擎（Phase C）
//
// 背景：
//
//	F 阶段为 gemini / claude / openai-responses 三种主流协议各写了一套硬编码翻译器。
//	但「免 Key」提供者生态里还有大量一次性的、站点自定义的 HTTP 协议（DuckDuckGo 的
//	duckchat、theoldllm 的 /api/chatgpt、felo 的搜索线程接口……）。为每一家写一套
//	Go 翻译器不可持续：改一次要重编 Go、发一次版，且这些站点接口随时会变。
//
// 方案：
//
//	把「一家提供者的 HTTP 协议」抽象成一份可声明的 JSON 规格（AdapterSpec），
//	由本引擎在运行时解释执行。新增一家非标提供者 = 写一段 JSON，无需改 Go、无需重编。
//	规格既可内置（adapters_builtin.go），也可由用户放到数据目录的 adapters.json 覆盖/扩展。
//
// 覆盖能力：
//   - 自定义对话路径 / 模型列表路径（chat_path / models_path）
//   - 固定请求头、查询参数（支持占位符）
//   - 预检握手取令牌（preflight：从响应头或响应 JSON 提取 token，带 TTL 缓存）
//   - 请求体模板渲染（把 OpenAI 请求拆成 ${model} ${prompt} ${messages} 等占位符）
//   - 响应提取（按 JSON 路径取文本，或整体当纯文本）
//   - 流式解析（sse-json / sse-text / ndjson / none），统一转成 OpenAI SSE
//
// 设计约束（与 translator.go 一致）：
//   - 纯数据转换，不依赖 proxy 内部状态；
//   - 只有 provider.format 命中已注册适配器时才启用，openai 默认路径零行为变更。

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------- 规格定义 ----------

// AdapterSpec 声明式描述一家非 OpenAI 兼容提供者的 HTTP 对话协议。
type AdapterSpec struct {
	ID    string `json:"id"`    // 适配器标识，对应 provider.format 的取值
	Label string `json:"label"` // 展示名（前端下拉用）
	Note  string `json:"note"`  // 备注/来源说明

	ChatPath   string `json:"chat_path"`   // 对话端点，相对 base_url（空 = base_url 本身即端点）
	ModelsPath string `json:"models_path"` // 模型列表端点（空 = 上游不提供，健康检查退回对话探测）
	Method     string `json:"method"`      // HTTP 方法，默认 POST

	Headers map[string]string `json:"headers"` // 固定请求头（值支持 ${token} 等原样占位符）
	Query   map[string]string `json:"query"`   // 固定查询参数（值支持原样占位符）

	Preflight *PreflightSpec `json:"preflight"` // 可选：调用前的令牌握手

	Request  RequestSpec  `json:"request"`
	Response ResponseSpec `json:"response"`
	Stream   StreamSpec   `json:"stream"`

	// TokenGen 声明「算法令牌」生成器类型（无需 HTTP 握手）。
	// 例如 "theoldllm" 表示每请求用 djb2 算法现场生成 X-Request-Token。
	// 与 Preflight 互斥：配置了 TokenGen 时，EnsureToken 直接返回算法结果（per-request，无缓存）。
	TokenGen string `json:"token_gen"`

	// InjectSystemMarker 是一段固定的「反滥用系统提示词」——某些免 Key 提供者要求请求体
	// 必须含一条 content 等于该字符串的 system 消息，否则拒绝服务（上游反爬闸门）。
	// 网关会在渲染后的请求体 messages 数组头部幂等注入该消息（已存在相同 content 则跳过）。
	// 例如 Mimocode 免费层要求系统消息内容为固定字符串，否则返回 403。
	InjectSystemMarker string `json:"inject_system_marker"`
}

// PreflightSpec 描述「先握手拿令牌，再带着令牌发对话请求」的流程。
// 典型场景：DuckDuckGo 需要先 GET /status 拿 x-vqd-4 响应头。
type PreflightSpec struct {
	Method      string            `json:"method"`       // 默认 GET
	URL         string            `json:"url"`          // 绝对 URL，或以 / 开头的相对 base_url 路径
	Headers     map[string]string `json:"headers"`      // 握手请求头
	Body        string            `json:"body"`         // 可选握手请求体（JSON 模板）
	TokenHeader string            `json:"token_header"` // 从响应头提取令牌，如 x-vqd-4
	TokenPath   string            `json:"token_path"`   // 或从响应 JSON 提取，如 data.token
	TTLSeconds  int               `json:"ttl_seconds"`  // 令牌缓存秒数（默认 600）
}

// RequestSpec 描述如何把 OpenAI 请求体渲染成上游请求体。
//
// Template 是一段 JSON 文本，其中的占位符会被替换为**合法 JSON 值**（含引号与转义），
// 因此模板里应写 {"message": ${prompt}} 而不是 {"message": "${prompt}"}，
// 这样可以彻底规避引号/换行导致的 JSON 注入与转义 bug。
//
// 可用占位符：
//
//	${model}        模型 ID（JSON 字符串）
//	${prompt}       最后一条 user 消息的纯文本（JSON 字符串）
//	${system}       system 提示词，无则 ""（JSON 字符串）
//	${messages}     全部消息，content 已扁平化为字符串（JSON 数组）
//	${messages_raw} 原样透传的 messages（JSON 数组，保留多模态结构）
//	${history}      除最后一条 user 消息外的历史（JSON 数组）
//	${stream}       是否流式（JSON 布尔）
//	${max_tokens}   最大 token，无则 null（JSON 数字）
//	${temperature}  温度，无则 null（JSON 数字）
//	${uuid}         随机 32 位十六进制会话 ID（JSON 字符串）
//	${token}        预检握手拿到的令牌（JSON 字符串）
//	${timestamp}    当前 Unix 秒（JSON 数字）
type RequestSpec struct {
	Template string `json:"template"`
}

// ResponseSpec 描述如何从上游非流式响应里取出回复文本。
type ResponseSpec struct {
	ContentPath string `json:"content_path"` // 点号路径，如 choices.0.message.content / data.text / response
	ErrorPath   string `json:"error_path"`   // 错误信息路径，如 error.message
	PlainText   bool   `json:"plain_text"`   // true = 响应体整体就是回复文本（非 JSON）
}

// StreamSpec 描述如何把上游流式响应转成 OpenAI SSE。
type StreamSpec struct {
	// Mode 取值：
	//   sse-json  上游是 SSE，每条 data: 是 JSON，按 DeltaPath 取增量文本
	//   sse-text  上游是 SSE，每条 data: 本身就是文本片段
	//   ndjson    上游是按行 JSON（无 data: 前缀），按 DeltaPath 取增量
	//   none      上游不支持流式 —— 网关改用非流式调用，再包成单块 SSE 返回
	Mode string `json:"mode"`

	DeltaPath   string `json:"delta_path"`   // 增量文本在每条事件 JSON 中的路径
	FullPath    string `json:"full_path"`    // 若上游每条推的是「全量文本」而非增量，填这里（引擎自动做差分）
	DoneMarker  string `json:"done_marker"`  // 结束标记，如 [DONE]
	SkipPrefix  string `json:"skip_prefix"`  // 需要忽略的行前缀（如心跳 :）
	FinishPath  string `json:"finish_path"`  // 结束原因路径（可选）
	UsagePath   string `json:"usage_path"`   // usage 对象路径（可选）
	EventFilter string `json:"event_filter"` // 只处理该 event: 名的事件（可选）
}

// ---------- 注册表 ----------

var (
	adapterMu    sync.RWMutex
	adapterTable = map[string]*AdapterSpec{}
)

// RegisterAdapter 注册（或覆盖）一个适配器规格。
func RegisterAdapter(spec *AdapterSpec) {
	if spec == nil || strings.TrimSpace(spec.ID) == "" {
		return
	}
	adapterMu.Lock()
	defer adapterMu.Unlock()
	adapterTable[strings.ToLower(strings.TrimSpace(spec.ID))] = spec
}

// LookupAdapter 按 format 值查找适配器；未命中返回 nil（调用方走原有 openai 路径）。
func LookupAdapter(format string) *AdapterSpec {
	f := strings.ToLower(strings.TrimSpace(format))
	if f == "" || f == FormatOpenAI || f == FormatGemini || f == FormatClaude || f == FormatResponses {
		return nil
	}
	adapterMu.RLock()
	defer adapterMu.RUnlock()
	return adapterTable[f]
}

// ListAdapters 返回全部已注册适配器（前端下拉、诊断用），按 ID 稳定输出。
func ListAdapters() []*AdapterSpec {
	adapterMu.RLock()
	defer adapterMu.RUnlock()
	out := make([]*AdapterSpec, 0, len(adapterTable))
	for _, v := range adapterTable {
		out = append(out, v)
	}
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].ID < out[i].ID {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// LoadAdaptersFile 从磁盘 JSON 加载用户自定义适配器（可覆盖内置同名 ID）。
// 文件格式：{"adapters":[{...},{...}]} 或直接一个数组 [{...}]。
// 文件不存在不算错误（返回 0, nil），便于开机静默调用。
func LoadAdaptersFile(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	b = bytes.TrimSpace(b)
	if len(b) == 0 {
		return 0, nil
	}
	var specs []*AdapterSpec
	if b[0] == '[' {
		if err := json.Unmarshal(b, &specs); err != nil {
			return 0, fmt.Errorf("parse adapters file: %w", err)
		}
	} else {
		var env struct {
			Adapters []*AdapterSpec `json:"adapters"`
		}
		if err := json.Unmarshal(b, &env); err != nil {
			return 0, fmt.Errorf("parse adapters file: %w", err)
		}
		specs = env.Adapters
	}
	n := 0
	for _, s := range specs {
		if s == nil || strings.TrimSpace(s.ID) == "" {
			continue
		}
		RegisterAdapter(s)
		n++
	}
	return n, nil
}

// ---------- 令牌握手与缓存 ----------

type cachedToken struct {
	value   string
	expires time.Time
}

// tokenInFlightEntry 作为 singleflight 的「握手完成」广播信号。
// done 通道关闭即代表 leader 已结束（成功或失败），所有跟随者通过 <-done 被唤醒；
// once 保证任何路径（success/fail/InvalidateToken）只 close 一次，彻底杜绝 double-close panic。
type tokenInFlightEntry struct {
	done chan struct{}
	once sync.Once
}

func (e *tokenInFlightEntry) broadcast() { e.once.Do(func() { close(e.done) }) }

var (
	tokenMu       sync.Mutex
	tokenCache    = map[string]cachedToken{}
	tokenInFlight = make(map[string]*tokenInFlightEntry) // P2-5: singleflight 去重，防止缓存踩踏
)

// InvalidateToken 主动作废某个提供者的握手令牌（上游返回 401/403 时调用，下次重新握手）。
// P2-5: 同时清理 in-flight 记录，防止下次请求被旧的 flight channel 阻塞。
func InvalidateToken(baseURL, adapterID string) {
	tokenMu.Lock()
	defer tokenMu.Unlock()
	delete(tokenCache, adapterID+"|"+baseURL)
	if flight, ok := tokenInFlight[adapterID+"|"+baseURL]; ok {
		delete(tokenInFlight, adapterID+"|"+baseURL)
		flight.broadcast()
	}
}

// EnsureToken 取得（必要时先握手获取）该提供者的令牌。
// 未配置 preflight 时返回空串且不报错。
// P2-5: 使用 singleflight 模式防止缓存踩踏——并发请求同一 token 时只执行一次握手。
func (a *AdapterSpec) EnsureToken(baseURL string, client *http.Client) (string, error) {
	if a == nil || a.Preflight == nil {
		// 无 HTTP 握手：若声明了算法令牌生成器（如 theoldllm 的 djb2 签名），
		// 每次调用现场生成新令牌（per-request 模式，无缓存必要）。
		if a != nil && a.TokenGen != "" {
			return generateAlgToken(a.TokenGen), nil
		}
		return "", nil
	}
	key := a.ID + "|" + baseURL

	// P2-5: singleflight 去重——并发请求同一 token 时只执行一次握手
	tokenMu.Lock()
	if c, ok := tokenCache[key]; ok && time.Now().Before(c.expires) {
		tokenMu.Unlock()
		return c.value, nil
	}
	if flight, ok := tokenInFlight[key]; ok {
		// 已有 goroutine 正在握手。等待 done 通道关闭（代表握手结束，成功或失败均可）：
		// 不直接从通道取值，避免多跟随者争抢同一值而永久阻塞；带超时兜底，防极端环境下卡死。
		tokenMu.Unlock()
		select {
		case <-flight.done:
			// leader 已结束（成功写入了缓存，或失败），重查缓存取结果
		case <-time.After(25 * time.Second):
			// 兜底超时：即便 leader 异常未广播，也不让本跟随者永久阻塞
		}
		tokenMu.Lock()
		if c, ok := tokenCache[key]; ok && time.Now().Before(c.expires) {
			tokenMu.Unlock()
			return c.value, nil
		}
		tokenMu.Unlock()
		return "", fmt.Errorf("preflight failed (concurrent)")
	}
	// 注册 in-flight 记录。done 通道仅作「握手完成」的广播信号（close 通知）使用，
	// 不再用于传递 token 值，确保多跟随者都能被唤醒并各自重查缓存。
	flight := &tokenInFlightEntry{done: make(chan struct{})}
	tokenInFlight[key] = flight
	tokenMu.Unlock()

	// 执行握手逻辑（与原有逻辑一致，但返回前统一通过 channel 通知）
	pf := a.Preflight
	url := pf.URL
	if url == "" {
		url = strings.TrimRight(baseURL, "/") + "/status"
	} else if strings.HasPrefix(url, "/") {
		url = strings.TrimRight(baseURL, "/") + url
	}
	method := strings.ToUpper(strings.TrimSpace(pf.Method))
	if method == "" {
		method = http.MethodGet
	}
	var bodyReader io.Reader
	if strings.TrimSpace(pf.Body) != "" {
		bodyReader = strings.NewReader(renderRaw(pf.Body, ""))
	}
	req, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		tokenMu.Lock()
		delete(tokenInFlight, key)
		flight.broadcast()
		tokenMu.Unlock()
		return "", err
	}
	for k, v := range pf.Headers {
		req.Header.Set(k, v)
	}
	if bodyReader != nil && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		tokenMu.Lock()
		delete(tokenInFlight, key)
		flight.broadcast()
		tokenMu.Unlock()
		return "", fmt.Errorf("preflight failed: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	token := ""
	if pf.TokenHeader != "" {
		token = resp.Header.Get(pf.TokenHeader)
	}
	if token == "" && pf.TokenPath != "" {
		var v interface{}
		if json.Unmarshal(raw, &v) == nil {
			token = asString(jsonPath(v, pf.TokenPath))
		}
	}
	if token == "" {
		tokenMu.Lock()
		delete(tokenInFlight, key)
		flight.broadcast()
		tokenMu.Unlock()
		return "", fmt.Errorf("preflight got no token (status=%d)", resp.StatusCode)
	}

	ttl := pf.TTLSeconds
	if ttl <= 0 {
		ttl = 600
	}
	tokenMu.Lock()
	tokenCache[key] = cachedToken{value: token, expires: time.Now().Add(time.Duration(ttl) * time.Second)}
	// P1-2: 成功后先写缓存，再通过 broadcast 通知所有等待的跟随者重查缓存。
	// 若 InvalidateToken 已在握手期间移除此条目，此处不再重复广播，避免通知到不相关记录。
	if _, stillInFlight := tokenInFlight[key]; stillInFlight {
		delete(tokenInFlight, key)
		flight.broadcast()
	}
	tokenMu.Unlock()

	return token, nil
}

// ---------- URL / 头部构造 ----------

// ChatURL 返回该适配器的对话端点完整 URL（含固定查询参数）。
func (a *AdapterSpec) ChatURL(baseURL, token string) string {
	u := strings.TrimRight(baseURL, "/")
	p := strings.TrimSpace(a.ChatPath)
	if p != "" {
		if !strings.HasPrefix(p, "/") {
			p = "/" + p
		}
		u += p
	}
	if len(a.Query) == 0 {
		return u
	}
	parts := make([]string, 0, len(a.Query))
	for k, v := range a.Query {
		parts = append(parts, k+"="+renderRaw(v, token))
	}
	sep := "?"
	if strings.Contains(u, "?") {
		sep = "&"
	}
	return u + sep + strings.Join(parts, "&")
}

// ModelsURL 返回模型列表端点；未配置返回空串。
func (a *AdapterSpec) ModelsURL(baseURL string) string {
	p := strings.TrimSpace(a.ModelsPath)
	if p == "" {
		return ""
	}
	if strings.HasPrefix(p, "http://") || strings.HasPrefix(p, "https://") {
		return p
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return strings.TrimRight(baseURL, "/") + p
}

// ApplyHeaders 把适配器声明的固定头写入请求头（占位符按原样字符串渲染）。
func (a *AdapterSpec) ApplyHeaders(h http.Header, token string) {
	if h == nil {
		return
	}
	for k, v := range a.Headers {
		h.Set(k, renderRaw(v, token))
	}
}

// HTTPMethod 返回对话请求的 HTTP 方法（默认 POST）。
func (a *AdapterSpec) HTTPMethod() string {
	m := strings.ToUpper(strings.TrimSpace(a.Method))
	if m == "" {
		return http.MethodPost
	}
	return m
}

// SupportsStream 报告上游是否支持真流式（mode=none 表示不支持，需降级为一次性调用）。
func (a *AdapterSpec) SupportsStream() bool {
	m := strings.ToLower(strings.TrimSpace(a.Stream.Mode))
	return m != "" && m != "none"
}

// ---------- 请求体渲染 ----------

// BuildRequestBody 把 OpenAI 请求体按模板渲染成上游请求体。
// 返回渲染后的 body、模型名、客户端是否请求了流式。
func (a *AdapterSpec) BuildRequestBody(openAIReq []byte, token string) (body []byte, model string, stream bool, err error) {
	var req oaRequest
	if err = json.Unmarshal(openAIReq, &req); err != nil {
		return nil, "", false, fmt.Errorf("parse openai request: %w", err)
	}
	stream = req.Stream

	tpl := strings.TrimSpace(a.Request.Template)
	var rendered []byte
	if tpl == "" {
		// 未声明模板：原样透传 OpenAI 请求体（适用于「仅路径不同」的提供者）。
		rendered = openAIReq
	} else {
		// 上游不支持流式时，模板里的 ${stream} 一律渲染为 false（引擎再把一次性响应包成 SSE）。
		effStream := stream && a.SupportsStream()

		vals := map[string]string{
			"model":        jsonStr(req.Model),
			"prompt":       jsonStr(lastUserText(req.Messages)),
			"system":       jsonStr(systemText(req.Messages)),
			"messages":     jsonVal(flattenMessages(req.Messages)),
			"messages_raw": jsonVal(req.Messages),
			"history":      jsonVal(historyMessages(req.Messages)),
			"stream":       strconv.FormatBool(effStream),
			"max_tokens":   jsonNumPtr(req.MaxTokens),
			"temperature":  jsonFloatPtr(req.Temperature),
			"uuid":         jsonStr(randomHex(16)),
			"token":        jsonStr(token),
			"timestamp":    strconv.FormatInt(time.Now().Unix(), 10),
		}
		out := tpl
		for k, v := range vals {
			out = strings.ReplaceAll(out, "${"+k+"}", v)
		}
		// 渲染结果必须是合法 JSON，否则说明模板写错了 —— 早失败好过把垃圾发给上游。
		if !json.Valid([]byte(out)) {
			return nil, "", false, fmt.Errorf("adapter %s: rendered request body is not valid JSON", a.ID)
		}
		rendered = []byte(out)
	}

	// 注入反滥用系统标记（幂等：已含相同 content 的 system 消息则跳过）。
	if a.InjectSystemMarker != "" {
		if injected, e := injectSystemMarker(rendered, a.InjectSystemMarker); e == nil {
			rendered = injected
		}
	}
	return rendered, req.Model, stream, nil
}

// injectSystemMarker 向上游请求体的 messages 数组头部注入一条固定的反滥用系统提示词。
// 幂等：若已存在 content 完全相同的 system 消息则跳过，避免重复注入。
// 仅当 body 是含 messages 数组的 JSON 对象时生效；否则原样返回。
func injectSystemMarker(body []byte, marker string) ([]byte, error) {
	var root map[string]interface{}
	if err := json.Unmarshal(body, &root); err != nil {
		return body, err
	}
	raw, ok := root["messages"]
	if !ok {
		return body, nil
	}
	arr, ok := raw.([]interface{})
	if !ok {
		return body, nil
	}
	// 已存在相同 content 的 system 消息则跳过（幂等）。
	for _, m := range arr {
		mm, ok := m.(map[string]interface{})
		if !ok {
			continue
		}
		if role, _ := mm["role"].(string); role == "system" {
			if content, _ := mm["content"].(string); content == marker {
				return body, nil
			}
		}
	}
	// 注入到数组头部，作为第一条系统消息（最贴近系统提示语义）。
	root["messages"] = append([]interface{}{
		map[string]interface{}{"role": "system", "content": marker},
	}, arr...)
	return json.Marshal(root)
}

// renderRaw 渲染头部/URL 里的占位符（原样字符串，不加引号）。
// P2-2: 净化 token 中的 CRLF，防止注入 HTTP 头（如 Transfer-Encoding 走私）。
func renderRaw(s, token string) string {
	if !strings.Contains(s, "${") {
		return s
	}
	// 净化 token：移除所有 \r\n，防止通过占位符注入 CRLF 分割 HTTP 头
	token = strings.ReplaceAll(token, "\r", "")
	token = strings.ReplaceAll(token, "\n", "")
	s = strings.ReplaceAll(s, "${token}", token)
	s = strings.ReplaceAll(s, "${uuid}", randomHex(16))
	s = strings.ReplaceAll(s, "${timestamp}", strconv.FormatInt(time.Now().Unix(), 10))
	return s
}

// ---------- 响应解析 ----------

// ToOpenAIResponse 把上游非流式响应翻译成 OpenAI chat.completion 形式。
func (a *AdapterSpec) ToOpenAIResponse(respBody []byte, modelLabel string) ([]byte, error) {
	text, err := a.ExtractContent(respBody)
	if err != nil {
		return nil, err
	}
	out := map[string]interface{}{
		"id":      "chatcmpl-" + randomHex(12),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   modelLabel,
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"message":       map[string]interface{}{"role": "assistant", "content": text},
				"finish_reason": "stop",
			},
		},
		"usage": map[string]interface{}{
			"prompt_tokens":     0,
			"completion_tokens": 0,
			"total_tokens":      0,
		},
	}
	return json.Marshal(out)
}

// ExtractContent 从上游响应里取出回复文本。
//
// 兼容一个常见坑：不少站点无视 stream=false，照样以 SSE 流返回。
// 此时按 JSON 解析必然失败，因此先探测 SSE 特征并把整条流收敛成整段文本。
func (a *AdapterSpec) ExtractContent(respBody []byte) (string, error) {
	if a.Response.PlainText {
		return string(bytes.TrimSpace(respBody)), nil
	}
	if looksLikeSSE(respBody) {
		if text := a.collapseSSE(respBody); text != "" {
			return text, nil
		}
	}
	var v interface{}
	if err := json.Unmarshal(respBody, &v); err != nil {
		// 不是 JSON：按纯文本兜底，总比丢内容强。
		return string(bytes.TrimSpace(respBody)), nil
	}
	if a.Response.ErrorPath != "" {
		if e := jsonPath(v, a.Response.ErrorPath); e != nil {
			if msg := asString(e); msg != "" {
				return "", fmt.Errorf("upstream error: %s", msg)
			}
		}
	}
	path := a.Response.ContentPath
	if path == "" {
		path = "choices.0.message.content"
	}
	got := jsonPath(v, path)
	if got == nil {
		return "", fmt.Errorf("adapter %s: content path %q not found in response", a.ID, path)
	}
	return asString(got), nil
}

// ErrorToOpenAI 把上游错误响应包成 OpenAI 错误体。
func (a *AdapterSpec) ErrorToOpenAI(respBody []byte) []byte {
	msg := strings.TrimSpace(string(respBody))
	var v interface{}
	if json.Unmarshal(respBody, &v) == nil && a.Response.ErrorPath != "" {
		if e := jsonPath(v, a.Response.ErrorPath); e != nil {
			if s := asString(e); s != "" {
				msg = s
			}
		}
	}
	if len(msg) > 2000 {
		msg = msg[:2000]
	}
	if msg == "" {
		msg = "upstream returned an error"
	}
	out, _ := json.Marshal(map[string]interface{}{
		"error": map[string]interface{}{
			"message": msg,
			"type":    "upstream_error",
			"code":    a.ID,
		},
	})
	return out
}

// ---------- 流式翻译 ----------

// NewAdapterSSETranslator 把上游流（或非流式整包）实时转换成 OpenAI SSE 流。
// 返回的 io.Reader 产出的就是标准 `data: {...}\n\n` 行，末尾带 `data: [DONE]`，
// 因此 proxy/stream.go 的既有解析逻辑可以完全复用。
func NewAdapterSSETranslator(spec *AdapterSpec, src io.Reader, modelLabel string) io.Reader {
	t := &adapterSSETranslator{
		spec:       spec,
		modelLabel: modelLabel,
		scanner:    bufio.NewScanner(src),
		idPrefix:   randomHex(8),
	}
	t.scanner.Buffer(make([]byte, 0, 1<<20), 10<<20)
	return t
}

type adapterSSETranslator struct {
	spec       *AdapterSpec
	modelLabel string
	scanner    *bufio.Scanner
	buf        bytes.Buffer
	idPrefix   string
	done       bool
	started    bool
	lastFull   string // FullPath 模式下的上一次全量文本（用于差分）
	usage      map[string]interface{}
	finish     string
	curEvent   string
}

func (t *adapterSSETranslator) Read(p []byte) (int, error) {
	for t.buf.Len() == 0 {
		if t.done {
			return 0, io.EOF
		}
		if !t.scanner.Scan() {
			t.emitFinish()
			t.done = true
			if t.buf.Len() == 0 {
				return 0, io.EOF
			}
			break
		}
		for _, chunk := range t.translateLine(t.scanner.Text()) {
			t.buf.WriteString(chunk)
		}
	}
	return t.buf.Read(p)
}

// translateLine 把上游一行转换成 0..n 条 OpenAI SSE 行。
func (t *adapterSSETranslator) translateLine(line string) []string {
	mode := strings.ToLower(strings.TrimSpace(t.spec.Stream.Mode))
	sp := t.spec.Stream

	// 记录 SSE 事件名（供 event_filter 使用）
	if strings.HasPrefix(line, "event:") {
		t.curEvent = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		return nil
	}
	if sp.SkipPrefix != "" && strings.HasPrefix(line, sp.SkipPrefix) {
		return nil
	}

	var data string
	switch mode {
	case "sse-json", "sse-text":
		if !strings.HasPrefix(line, "data:") {
			return nil
		}
		data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	case "ndjson":
		data = strings.TrimSpace(line)
	default:
		return nil
	}
	if data == "" {
		return nil
	}

	marker := sp.DoneMarker
	if marker == "" {
		marker = "[DONE]"
	}
	if data == marker {
		t.emitFinish()
		t.done = true
		return nil
	}
	if sp.EventFilter != "" && t.curEvent != "" && !strings.EqualFold(t.curEvent, sp.EventFilter) {
		return nil
	}

	// sse-text：data 本身就是文本片段。
	if mode == "sse-text" {
		return t.emitDelta(data)
	}

	var v interface{}
	if err := json.Unmarshal([]byte(data), &v); err != nil {
		// 上游偶发非 JSON 行（心跳、注释）：忽略而不是中断整条流。
		return nil
	}
	if sp.UsagePath != "" {
		if u, ok := jsonPath(v, sp.UsagePath).(map[string]interface{}); ok && u != nil {
			t.usage = u
		}
	}
	if sp.FinishPath != "" {
		if f := asString(jsonPath(v, sp.FinishPath)); f != "" {
			t.finish = f
		}
	}

	// 全量模式：上游每条推的是累计文本，引擎自动做差分成增量。
	if sp.FullPath != "" {
		full := asString(jsonPath(v, sp.FullPath))
		if full == "" {
			return nil
		}
		delta := full
		if strings.HasPrefix(full, t.lastFull) {
			delta = full[len(t.lastFull):]
		}
		t.lastFull = full
		if delta == "" {
			return nil
		}
		return t.emitDelta(delta)
	}

	dp := sp.DeltaPath
	if dp == "" {
		dp = "choices.0.delta.content"
	}
	delta := asString(jsonPath(v, dp))
	if delta == "" {
		return nil
	}
	return t.emitDelta(delta)
}

// emitDelta 产出一条 OpenAI SSE 增量（首条附带 role: assistant）。
func (t *adapterSSETranslator) emitDelta(text string) []string {
	out := make([]string, 0, 2)
	if !t.started {
		t.started = true
		out = append(out, "data: "+chunkJSON(t.idPrefix, t.modelLabel,
			map[string]interface{}{"role": "assistant", "content": ""}, nil)+"\n\n")
	}
	out = append(out, "data: "+chunkJSON(t.idPrefix, t.modelLabel,
		map[string]interface{}{"content": text}, nil)+"\n\n")
	return out
}

// emitFinish 在流末补 finish_reason（+ usage）与 [DONE]。
func (t *adapterSSETranslator) emitFinish() {
	if t.done {
		return
	}
	fr := t.finish
	if fr == "" {
		fr = "stop"
	}
	if !t.started {
		// 上游一个字都没吐：仍要给下游一个完整的空回复，避免客户端卡住。
		t.buf.WriteString("data: " + chunkJSON(t.idPrefix, t.modelLabel,
			map[string]interface{}{"role": "assistant", "content": ""}, nil) + "\n\n")
		t.started = true
	}
	var opts []func(map[string]interface{})
	if t.usage != nil {
		opts = append(opts, withUsage(t.usage))
	}
	t.buf.WriteString("data: " + chunkJSON(t.idPrefix, t.modelLabel,
		map[string]interface{}{}, fr, opts...) + "\n\n")
	t.buf.WriteString("data: [DONE]\n\n")
}

// NewAdapterOneShotStream 把一次性（非流式）上游响应包装成单块 OpenAI SSE 流。
// 用于 stream.mode = none 的适配器：客户端要流式，但上游只会整包返回。
func NewAdapterOneShotStream(spec *AdapterSpec, respBody []byte, modelLabel string) io.Reader {
	idPrefix := randomHex(8)
	var b bytes.Buffer
	text, err := spec.ExtractContent(respBody)
	if err != nil {
		text = "⚠️ " + err.Error()
	}
	b.WriteString("data: " + chunkJSON(idPrefix, modelLabel,
		map[string]interface{}{"role": "assistant", "content": ""}, nil) + "\n\n")
	if text != "" {
		b.WriteString("data: " + chunkJSON(idPrefix, modelLabel,
			map[string]interface{}{"content": text}, nil) + "\n\n")
	}
	b.WriteString("data: " + chunkJSON(idPrefix, modelLabel,
		map[string]interface{}{}, "stop") + "\n\n")
	b.WriteString("data: [DONE]\n\n")
	return bytes.NewReader(b.Bytes())
}

// looksLikeSSE 粗判响应体是否为 SSE 流（首个非空行以 data: / event: 开头）。
func looksLikeSSE(b []byte) bool {
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		return strings.HasPrefix(line, "data:") || strings.HasPrefix(line, "event:")
	}
	return false
}

// collapseSSE 把一整条 SSE 流按本适配器的流式规格收敛成完整文本。
// 复用流式翻译器，因此增量/全量差分、done 标记、事件过滤等行为与真流式完全一致。
func (a *AdapterSpec) collapseSSE(raw []byte) string {
	spec := a
	if !a.SupportsStream() {
		// 规格声明「不支持流式」但上游偏偏回了 SSE：临时按 sse-json + 默认路径解析。
		clone := *a
		clone.Stream.Mode = "sse-json"
		if clone.Stream.DeltaPath == "" && clone.Stream.FullPath == "" {
			clone.Stream.DeltaPath = a.Response.ContentPath
		}
		spec = &clone
	}
	tr := NewAdapterSSETranslator(spec, bytes.NewReader(raw), "collapse")
	out, err := io.ReadAll(tr)
	if err != nil {
		return ""
	}
	var sb strings.Builder
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 10<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var v interface{}
		if json.Unmarshal([]byte(data), &v) != nil {
			continue
		}
		sb.WriteString(asString(jsonPath(v, "choices.0.delta.content")))
	}
	return sb.String()
}

// ---------- 工具函数 ----------

// ---------- 算法令牌生成（TokenGen）----------
//
// 部分免 Key 提供者的「令牌」不是从握手接口获取，而是由固定算法本地生成
// （如 theoldllm 的 djb2 签名）。这类适配器在 AdapterSpec 声明 TokenGen 类型，
// EnsureToken 据此现场生成 per-request 令牌，无需浏览器、无需 HTTP 握手。

// generateAlgToken 按生成器类型分发。
func generateAlgToken(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "theoldllm":
		return theOldLLMToken()
	default:
		return ""
	}
}

// theOldLLMToken 复刻 OmniRoute open-sse/executors/theoldllm.ts 的 generateRequestToken。
// 算法：e = "${ts}-oldllm-client-2026-${UA.slice(0,20)}"，对 e 做 32 位 djb2 哈希，
// token = ts.toString(36) + "-" + abs(hash).toString(36) + "-" + 8 位随机 hex。
// 注意：上游（Vercel）对 UA 版本敏感——必须真实 Chrome/149，旧版 UA 直接 403。
func theOldLLMToken() string {
	n := time.Now().UnixMilli()
	e := fmt.Sprintf("%d-oldllm-client-2026-Mozilla/5.0 (Windows", n)
	t := djb2Int32(e)
	r := randomHex(4) // 4 字节 = 8 hex 字符，对齐 OmniRoute 的 randomUUID().slice(0,8)
	return fmt.Sprintf("%s-%s-%s", toBase36(uint64(n)), toBase36(uint64(absInt64(t))), r)
}

// djb2Int32 标准 djb2 哈希，返回 32 位有符号整数（溢出回绕，与 JS ToInt32 一致）。
func djb2Int32(s string) int32 {
	var h int32
	for i := 0; i < len(s); i++ {
		h = (h << 5) - h + int32(s[i])
	}
	return h
}

// absInt64 返回绝对值（用 int64 避免 int32 最小值 abs 溢出，对齐 JS Math.abs 的浮点语义）。
func absInt64(v int32) int64 {
	x := int64(v)
	if x < 0 {
		return -x
	}
	return x
}

// toBase36 把无符号整数转成 base36 字符串（对应 JS Number.prototype.toString(36)）。
func toBase36(n uint64) string {
	if n == 0 {
		return "0"
	}
	const digits = "0123456789abcdefghijklmnopqrstuvwxyz"
	var buf [64]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = digits[n%36]
		n /= 36
	}
	return string(buf[i:])
}

// jsonPath 按点号路径取值，数字段视为数组下标。例：choices.0.message.content
func jsonPath(v interface{}, path string) interface{} {
	if path == "" {
		return v
	}
	cur := v
	for _, seg := range strings.Split(path, ".") {
		if cur == nil {
			return nil
		}
		if idx, err := strconv.Atoi(seg); err == nil {
			arr, ok := cur.([]interface{})
			if !ok || idx < 0 || idx >= len(arr) {
				return nil
			}
			cur = arr[idx]
			continue
		}
		m, ok := cur.(map[string]interface{})
		if !ok {
			return nil
		}
		cur = m[seg]
	}
	return cur
}

// asString 把任意 JSON 值转成字符串（数组会拼接其中的文本片段）。
func asString(v interface{}) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	case []interface{}:
		var sb strings.Builder
		for _, it := range x {
			sb.WriteString(asString(it))
		}
		return sb.String()
	case map[string]interface{}:
		// 常见形态 {"text": "..."} / {"content": "..."}
		for _, k := range []string{"text", "content", "message", "value"} {
			if s, ok := x[k].(string); ok {
				return s
			}
		}
		b, _ := json.Marshal(x)
		return string(b)
	default:
		b, _ := json.Marshal(x)
		return string(b)
	}
}

func jsonStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func jsonVal(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "null"
	}
	return string(b)
}

func jsonNumPtr(p *int) string {
	if p == nil {
		return "null"
	}
	return strconv.Itoa(*p)
}

func jsonFloatPtr(p *float64) string {
	if p == nil {
		return "null"
	}
	return strconv.FormatFloat(*p, 'f', -1, 64)
}

// lastUserText 取最后一条 user 消息的纯文本。
func lastUserText(msgs []oaMessage) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if strings.EqualFold(msgs[i].Role, "user") {
			return stringContent(msgs[i].Content)
		}
	}
	if len(msgs) > 0 {
		return stringContent(msgs[len(msgs)-1].Content)
	}
	return ""
}

// systemText 合并全部 system 消息。
func systemText(msgs []oaMessage) string {
	var parts []string
	for _, m := range msgs {
		if strings.EqualFold(m.Role, "system") || strings.EqualFold(m.Role, "developer") {
			if s := stringContent(m.Content); s != "" {
				parts = append(parts, s)
			}
		}
	}
	return strings.Join(parts, "\n\n")
}

type flatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// flattenMessages 把多模态 content 压平成字符串，便于只认纯文本的上游。
func flattenMessages(msgs []oaMessage) []flatMessage {
	out := make([]flatMessage, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, flatMessage{Role: m.Role, Content: stringContent(m.Content)})
	}
	return out
}

// historyMessages 返回除最后一条 user 消息之外的历史（压平）。
func historyMessages(msgs []oaMessage) []flatMessage {
	last := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if strings.EqualFold(msgs[i].Role, "user") {
			last = i
			break
		}
	}
	out := make([]flatMessage, 0, len(msgs))
	for i, m := range msgs {
		if i == last {
			continue
		}
		out = append(out, flatMessage{Role: m.Role, Content: stringContent(m.Content)})
	}
	return out
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b)
}
