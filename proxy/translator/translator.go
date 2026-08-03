// Package translator 提供上游协议格式翻译层（F 阶段）。
//
// 当前支持（F 阶段格式翻译层）：
//   - OpenAI chat.completions 请求  <->  Google Gemini generateContent 原生协议（请求体 / 非流式响应 / SSE 流式）。
//   - OpenAI chat.completions 请求  <->  Anthropic /v1/messages 原生协议（请求体 / 非流式响应 / SSE 流式）。
//   - OpenAI chat.completions 请求  <->  OpenAI Responses API /v1/responses 原生协议（请求体 / 非流式响应 / SSE 流式）。
//
// 设计原则：
//   - 纯 JSON 转换，不依赖 proxy 包内部状态（无 meta/catalog 依赖），可独立单测。
//   - openai 格式为默认；gemini / claude / openai-responses 仅在 provider.Format 命中时启用，直连 openai 路径零改动。
//   - 流式翻译以 io.Reader 包装器实现：把上游 SSE 流实时转成 OpenAI SSE 流（data: ... 行），
//     使上游转发主循环保持单一、通用的解析逻辑。
package translator

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// 协议格式常量
const (
	FormatOpenAI    = "openai"
	FormatGemini    = "gemini"
	FormatClaude    = "claude"
	FormatResponses = "openai-responses"
)

// ---------- OpenAI 请求结构 ----------

type oaMessage struct {
	Role       string       `json:"role"`
	Content    interface{}  `json:"content"` // string 或 []part（多模态）
	ToolCalls  []oaToolCall `json:"tool_calls,omitempty"`
	ToolCallID string       `json:"tool_call_id,omitempty"`
	Name       string       `json:"name,omitempty"`
}

// oaToolCall OpenAI assistant 消息中的工具调用（function calling）
type oaToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type oaRequest struct {
	Model               string      `json:"model"`
	Messages            []oaMessage `json:"messages"`
	Stream              bool        `json:"stream"`
	Temperature         *float64    `json:"temperature"`
	TopP                *float64    `json:"top_p"`
	MaxTokens           *int        `json:"max_tokens"`
	MaxCompletionTokens *int        `json:"max_completion_tokens"`
	Stop                interface{} `json:"stop"`
	Tools               []oaTool    `json:"tools,omitempty"`
	ToolChoice          interface{} `json:"tool_choice,omitempty"`
	ResponseFormat      *struct {
		Type string `json:"type"`
	} `json:"response_format,omitempty"`
}

// oaTool OpenAI 工具定义（function calling）
type oaTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string      `json:"name"`
		Description string      `json:"description"`
		Parameters  interface{} `json:"parameters"`
	} `json:"function"`
}

// ---------- Gemini 请求结构 ----------

type geminiPart struct {
	Text       string            `json:"text,omitempty"`
	InlineData *geminiInlineData `json:"inline_data,omitempty"`
}

type geminiInlineData struct {
	MimeType string `json:"mime_type"`
	Data     string `json:"data"` // base64
}

type geminiContent struct {
	Role  string       `json:"role"`
	Parts []geminiPart `json:"parts"`
}

type geminiGenConfig struct {
	Temperature     *float64 `json:"temperature,omitempty"`
	TopP            *float64 `json:"topP,omitempty"`
	MaxOutputTokens *int     `json:"maxOutputTokens,omitempty"`
	StopSequences   []string `json:"stopSequences,omitempty"`
}

type geminiRequest struct {
	SystemInstruction *struct {
		Parts []geminiPart `json:"parts"`
	} `json:"systemInstruction,omitempty"`
	Contents         []geminiContent `json:"contents"`
	GenerationConfig geminiGenConfig `json:"generationConfig"`
}

// ToGeminiBody 将 OpenAI chat.completions 请求体翻译为 Gemini generateContent 请求体。
// 返回翻译后的 body、模型名、是否请求流式（流式由调用方决定 URL，这里仅透出标记）。
func ToGeminiBody(openAIReq []byte) (geminiBody []byte, model string, stream bool, err error) {
	var req oaRequest
	if err := json.Unmarshal(openAIReq, &req); err != nil {
		return nil, "", false, fmt.Errorf("parse openai request: %w", err)
	}
	model = req.Model
	stream = req.Stream

	var sysParts []geminiPart
	var contents []geminiContent
	for _, m := range req.Messages {
		role := m.Role
		switch role {
		case "system":
			sysParts = append(sysParts, extractParts(m.Content)...)
			continue
		case "assistant":
			role = "model" // Gemini 用 model 而非 assistant
		case "", "user":
			role = "user"
		}
		contents = append(contents, geminiContent{
			Role:  role,
			Parts: extractParts(m.Content),
		})
	}

	g := geminiRequest{
		Contents: contents,
		GenerationConfig: geminiGenConfig{
			Temperature:     req.Temperature,
			TopP:            req.TopP,
			MaxOutputTokens: req.MaxTokens,
		},
	}
	if g.GenerationConfig.MaxOutputTokens == nil && req.MaxCompletionTokens != nil {
		g.GenerationConfig.MaxOutputTokens = req.MaxCompletionTokens
	}
	if len(sysParts) > 0 {
		g.SystemInstruction = &struct {
			Parts []geminiPart `json:"parts"`
		}{Parts: sysParts}
	}
	if req.Stop != nil {
		switch s := req.Stop.(type) {
		case string:
			if s != "" {
				g.GenerationConfig.StopSequences = []string{s}
			}
		case []interface{}:
			for _, v := range s {
				if vs, ok := v.(string); ok && vs != "" {
					g.GenerationConfig.StopSequences = append(g.GenerationConfig.StopSequences, vs)
				}
			}
		}
	}

	b, err := json.Marshal(g)
	if err != nil {
		return nil, "", false, fmt.Errorf("marshal gemini request: %w", err)
	}
	return b, model, stream, nil
}

// extractParts 把 OpenAI 消息 content（string 或 []part）转换为 Gemini parts。
// 支持文本与图片（image_url 的 data URI -> inline_data）。
func extractParts(content interface{}) []geminiPart {
	switch c := content.(type) {
	case string:
		if c == "" {
			return nil
		}
		return []geminiPart{{Text: c}}
	case []interface{}:
		var parts []geminiPart
		for _, item := range c {
			m, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			typ, _ := m["type"].(string)
			switch typ {
			case "text":
				if t, _ := m["text"].(string); t != "" {
					parts = append(parts, geminiPart{Text: t})
				}
			case "image_url":
				if ij, ok := m["image_url"].(map[string]interface{}); ok {
					if url, _ := ij["url"].(string); url != "" {
						if mime, data, ok := parseDataURI(url); ok {
							parts = append(parts, geminiPart{InlineData: &geminiInlineData{MimeType: mime, Data: data}})
						}
					}
				}
			}
		}
		return parts
	default:
		return nil
	}
}

// parseDataURI 解析 data URI（data:<mime>;base64,<data>），仅支持 base64。
func parseDataURI(uri string) (mime, data string, ok bool) {
	const prefix = "data:"
	if !strings.HasPrefix(uri, prefix) {
		return "", "", false
	}
	rest := uri[len(prefix):]
	comma := strings.Index(rest, ",")
	if comma < 0 {
		return "", "", false
	}
	meta := rest[:comma]
	raw := rest[comma+1:]
	if !strings.HasSuffix(meta, ";base64") {
		return "", "", false
	}
	meta = strings.TrimSuffix(meta, ";base64")
	if meta == "" {
		meta = "application/octet-stream"
	}
	return meta, raw, true
}

// ---------- Gemini 响应结构 ----------

type geminiResp struct {
	Candidates    []geminiCandidate `json:"candidates"`
	UsageMetadata *struct {
		PromptTokenCount     int `json:"promptTokenCount"`
		CandidatesTokenCount int `json:"candidatesTokenCount"`
		TotalTokenCount      int `json:"totalTokenCount"`
	} `json:"usageMetadata"`
}

type geminiCandidate struct {
	Content *struct {
		Parts []geminiPart `json:"parts"`
		Role  string       `json:"role"`
	} `json:"content"`
	FinishReason string `json:"finishReason"`
	Index        int    `json:"index"`
}

// GeminiToOpenAI 将 Gemini 非流式响应翻译为 OpenAI chat.completion 响应（model 设为传入的 model）。
func GeminiToOpenAI(geminiRespBytes []byte, model string) ([]byte, error) {
	var g geminiResp
	if err := json.Unmarshal(geminiRespBytes, &g); err != nil {
		return nil, fmt.Errorf("parse gemini response: %w", err)
	}
	var sb strings.Builder
	for _, c := range g.Candidates {
		if c.Content != nil {
			for _, p := range c.Content.Parts {
				if p.Text != "" {
					sb.WriteString(p.Text)
				}
			}
		}
	}
	content := sb.String()
	finish := "stop"
	if len(g.Candidates) > 0 {
		finish = mapFinishReason(g.Candidates[0].FinishReason)
	}
	oa := map[string]interface{}{
		"id":      "gemini-" + fmt.Sprintf("%d", time.Now().UnixNano()),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]interface{}{
			{
				"index": 0,
				"message": map[string]interface{}{
					"role":    "assistant",
					"content": content,
				},
				"finish_reason": finish,
			},
		},
	}
	if g.UsageMetadata != nil {
		oa["usage"] = map[string]interface{}{
			"prompt_tokens":     g.UsageMetadata.PromptTokenCount,
			"completion_tokens": g.UsageMetadata.CandidatesTokenCount,
			"total_tokens":      g.UsageMetadata.TotalTokenCount,
		}
	} else {
		oa["usage"] = nil
	}
	return json.Marshal(oa)
}

type geminiError struct {
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

// GeminiErrorToOpenAI 把 Gemini 错误体翻译为 OpenAI 错误形（保留原状态码由调用方设置）。
func GeminiErrorToOpenAI(geminiErr []byte) []byte {
	var e geminiError
	if err := json.Unmarshal(geminiErr, &e); err == nil && e.Error != nil {
		out, _ := json.Marshal(map[string]interface{}{
			"error": map[string]interface{}{
				"message": e.Error.Message,
				"type":    e.Error.Status,
				"code":    e.Error.Code,
			},
		})
		return out
	}
	return geminiErr
}

// mapFinishReason 把 Gemini finishReason 映射为 OpenAI finish_reason。
func mapFinishReason(r string) string {
	switch strings.ToUpper(strings.TrimSpace(r)) {
	case "STOP", "":
		return "stop"
	case "MAX_TOKENS":
		return "length"
	case "SAFETY", "RECITATION", "OTHER", "BLOCKLIST", "PROHIBITED_CONTENT":
		return "content_filter"
	default:
		return "stop"
	}
}

// ---------- Claude (Anthropic Messages) 请求结构 ----------

type claudeContentBlock struct {
	Type   string             `json:"type"` // "text" | "image" | "tool_use"
	Text   string             `json:"text,omitempty"`
	Source *claudeImageSource `json:"source,omitempty"`
	ID     string             `json:"id,omitempty"`
	Name   string             `json:"name,omitempty"`
	Input  interface{}        `json:"input,omitempty"`
}

type claudeImageSource struct {
	Type      string `json:"type"` // "base64" | "url"
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

type claudeMessage struct {
	Role    string      `json:"role"` // "user" | "assistant"
	Content interface{} `json:"content"`
}

type claudeTool struct {
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	InputSchema interface{} `json:"input_schema"`
}

type claudeRespFmt struct {
	Type string `json:"type"`
}

type claudeRequest struct {
	Model          string          `json:"model"`
	Messages       []claudeMessage `json:"messages"`
	System         string          `json:"system,omitempty"`
	MaxTokens      int             `json:"max_tokens"`
	Temperature    *float64        `json:"temperature,omitempty"`
	TopP           *float64        `json:"top_p,omitempty"`
	StopSequences  []string        `json:"stop_sequences,omitempty"`
	Stream         bool            `json:"stream"`
	Tools          []claudeTool    `json:"tools,omitempty"`
	ToolChoice     interface{}     `json:"tool_choice,omitempty"`
	ResponseFormat *claudeRespFmt  `json:"response_format,omitempty"`
}

// claudeDefaultMaxTokens 当 OpenAI 请求未携带 max_tokens 时，Claude 协议要求必填，给一个保守默认。
const claudeDefaultMaxTokens = 4096

// ToClaudeBody 将 OpenAI chat.completions 请求体翻译为 Anthropic /v1/messages 请求体。
// 返回翻译后的 body 与模型名。Claude 的 stream 标记直接读自原请求的 stream 字段（流式由调用方决定 URL）。
func ToClaudeBody(openAIReq []byte) (claudeBody []byte, model string, err error) {
	var req oaRequest
	if err := json.Unmarshal(openAIReq, &req); err != nil {
		return nil, "", fmt.Errorf("parse openai request: %w", err)
	}
	model = req.Model

	var system string
	var msgs []claudeMessage
	for _, m := range req.Messages {
		switch m.Role {
		case "system":
			if t := stringContent(m.Content); t != "" {
				if system != "" {
					system += "\n\n"
				}
				system += t
			}
			continue
		default:
			role := m.Role
			if role == "" {
				role = "user"
			}
			msgs = append(msgs, claudeMessage{Role: role, Content: toClaudeContent(m.Content)})
		}
	}

	maxTokens := claudeDefaultMaxTokens
	if req.MaxTokens != nil {
		maxTokens = *req.MaxTokens
	} else if req.MaxCompletionTokens != nil {
		maxTokens = *req.MaxCompletionTokens
	}

	c := claudeRequest{
		Model:     model,
		Messages:  msgs,
		MaxTokens: maxTokens,
		Stream:    req.Stream,
	}
	if system != "" {
		c.System = system
	}
	if req.Temperature != nil {
		c.Temperature = req.Temperature
	}
	if req.TopP != nil {
		c.TopP = req.TopP
	}
	if req.Stop != nil {
		if ss := toStrSlice(req.Stop); len(ss) > 0 {
			c.StopSequences = ss
		}
	}
	if len(req.Tools) > 0 {
		for _, t := range req.Tools {
			c.Tools = append(c.Tools, claudeTool{
				Name:        t.Function.Name,
				Description: t.Function.Description,
				InputSchema: t.Function.Parameters,
			})
		}
	}
	if req.ToolChoice != nil {
		c.ToolChoice = mapToolChoice(req.ToolChoice)
	}
	if req.ResponseFormat != nil && req.ResponseFormat.Type == "json_object" {
		c.ResponseFormat = &claudeRespFmt{Type: "json"}
	}

	b, err := json.Marshal(c)
	if err != nil {
		return nil, "", fmt.Errorf("marshal claude request: %w", err)
	}
	return b, model, nil
}

// stringContent 把消息 content（string 或 []part）抽取为纯文本（多模态块只取文本）。
func stringContent(content interface{}) string {
	switch c := content.(type) {
	case string:
		return c
	case []interface{}:
		var sb strings.Builder
		for _, item := range c {
			m, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			if typ, _ := m["type"].(string); typ == "text" {
				if t, _ := m["text"].(string); t != "" {
					sb.WriteString(t)
				}
			}
		}
		return sb.String()
	default:
		return ""
	}
}

// toClaudeContent 把 OpenAI content 转为 Claude content（string 或 []claudeContentBlock）。
// 多模态：image_url 的 data URI → image 块（base64 source）。
func toClaudeContent(content interface{}) interface{} {
	switch c := content.(type) {
	case string:
		return c
	case []interface{}:
		var blocks []claudeContentBlock
		for _, item := range c {
			m, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			typ, _ := m["type"].(string)
			switch typ {
			case "text":
				if t, _ := m["text"].(string); t != "" {
					blocks = append(blocks, claudeContentBlock{Type: "text", Text: t})
				}
			case "image_url":
				if ij, ok := m["image_url"].(map[string]interface{}); ok {
					if url, _ := ij["url"].(string); url != "" {
						if mime, data, ok := parseDataURI(url); ok {
							blocks = append(blocks, claudeContentBlock{
								Type:   "image",
								Source: &claudeImageSource{Type: "base64", MediaType: mime, Data: data},
							})
						}
					}
				}
			}
		}
		if len(blocks) == 0 {
			return ""
		}
		return blocks
	default:
		return ""
	}
}

// mapToolChoice 把 OpenAI tool_choice 映射为 Claude tool_choice。
// OpenAI {type:"function",function:{name}} → Claude {type:"tool",name}；其余（auto/none/any）原样透传。
func mapToolChoice(tc interface{}) interface{} {
	m, ok := tc.(map[string]interface{})
	if !ok {
		return tc
	}
	if typ, _ := m["type"].(string); typ == "function" {
		if fn, ok := m["function"].(map[string]interface{}); ok {
			name, _ := fn["name"].(string)
			return map[string]interface{}{"type": "tool", "name": name}
		}
	}
	return tc
}

// toStrSlice 把 OpenAI stop（string 或 []string）转为 []string。
func toStrSlice(stop interface{}) []string {
	switch s := stop.(type) {
	case string:
		if s != "" {
			return []string{s}
		}
	case []interface{}:
		var out []string
		for _, v := range s {
			if vs, ok := v.(string); ok && vs != "" {
				out = append(out, vs)
			}
		}
		return out
	}
	return nil
}

// ---------- Claude 响应结构 ----------

type claudeResp struct {
	ID           string                   `json:"id"`
	Type         string                   `json:"type"`
	Role         string                   `json:"role"`
	Model        string                   `json:"model"`
	Content      []map[string]interface{} `json:"content"`
	StopReason   string                   `json:"stop_reason"`
	StopSequence *string                  `json:"stop_sequence"`
	Usage        *struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// ClaudeToOpenAI 将 Claude 非流式响应翻译为 OpenAI chat.completion 响应（model 设为传入的 model）。
// 支持文本块拼接与 tool_use 块 → tool_calls。
func ClaudeToOpenAI(claudeRespBytes []byte, model string) ([]byte, error) {
	var c claudeResp
	if err := json.Unmarshal(claudeRespBytes, &c); err != nil {
		return nil, fmt.Errorf("parse claude response: %w", err)
	}
	var content string
	var toolCalls []map[string]interface{}
	for _, blk := range c.Content {
		typ, _ := blk["type"].(string)
		switch typ {
		case "text":
			if t, ok := blk["text"].(string); ok {
				content += t
			}
		case "tool_use":
			id, _ := blk["id"].(string)
			name, _ := blk["name"].(string)
			input := blk["input"]
			argsBytes, _ := json.Marshal(input)
			toolCalls = append(toolCalls, map[string]interface{}{
				"id":       id,
				"type":     "function",
				"function": map[string]interface{}{"name": name, "arguments": string(argsBytes)},
			})
		}
	}
	msg := map[string]interface{}{"role": "assistant", "content": content}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}
	finish := mapClaudeStopReason(c.StopReason)
	if len(toolCalls) > 0 && finish == "stop" {
		finish = "tool_calls"
	}
	oa := map[string]interface{}{
		"id":      "claude-" + fmt.Sprintf("%d", time.Now().UnixNano()),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"message":       msg,
				"finish_reason": finish,
			},
		},
	}
	if c.Usage != nil {
		oa["usage"] = map[string]interface{}{
			"prompt_tokens":     c.Usage.InputTokens,
			"completion_tokens": c.Usage.OutputTokens,
			"total_tokens":      c.Usage.InputTokens + c.Usage.OutputTokens,
		}
	} else {
		oa["usage"] = nil
	}
	return json.Marshal(oa)
}

type claudeErrorEnvelope struct {
	Type  string `json:"type"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// ClaudeErrorToOpenAI 把 Claude 错误体翻译为 OpenAI 错误形（保留原状态码由调用方设置）。
func ClaudeErrorToOpenAI(claudeErr []byte) []byte {
	var e claudeErrorEnvelope
	if err := json.Unmarshal(claudeErr, &e); err == nil && e.Error != nil {
		out, _ := json.Marshal(map[string]interface{}{
			"error": map[string]interface{}{
				"message": e.Error.Message,
				"type":    e.Error.Type,
			},
		})
		return out
	}
	return claudeErr
}

// mapClaudeStopReason 把 Claude stop_reason 映射为 OpenAI finish_reason。
func mapClaudeStopReason(r string) string {
	switch strings.ToLower(strings.TrimSpace(r)) {
	case "", "end_turn", "stop_sequence":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "refusal":
		return "content_filter"
	default:
		return "stop"
	}
}

// ---------- 流式翻译：Gemini SSE -> OpenAI SSE ----------

// NewGeminiSSETranslator 返回一个 io.Reader，把 Gemini 的 SSE 流（每行 data: {json}）实时
// 翻译为 OpenAI 兼容的 SSE 流（data: {chunk}\n\n），并在上游流结束时追加 data: [DONE]。
// modelLabel 用于 chunk 的 model 字段（通常为 "provider · model"）。
func NewGeminiSSETranslator(src io.Reader, modelLabel string) io.Reader {
	s := bufio.NewScanner(src)
	s.Buffer(make([]byte, 0, 1<<20), 10<<20)
	return &geminiSSETranslator{scanner: s, modelLabel: modelLabel}
}

type geminiSSETranslator struct {
	scanner    *bufio.Scanner
	modelLabel string
	buf        []byte
	done       bool
	roleSent   bool // P3-5: 是否已发过首个 role:assistant chunk
}

func (t *geminiSSETranslator) Read(p []byte) (int, error) {
	for len(t.buf) == 0 {
		if t.done {
			return 0, io.EOF
		}
		if !t.scanner.Scan() {
			// 上游流结束：补 [DONE] 并结束
			t.buf = []byte("data: [DONE]\n\n")
			t.done = true
			continue
		}
		line := strings.TrimSpace(t.scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		// P3-5: 首个有效数据块前，先发 role:assistant chunk（与 Claude/Responses 保持一致，
		// 避免严格客户端报「首个 chunk 缺 role」）。
		if !t.roleSent {
			t.roleSent = true
			t.buf = append(t.buf, []byte("data: "+chunkJSON("gemini-", t.modelLabel, map[string]interface{}{"role": "assistant"}, nil)+"\n\n")...)
		}
		for _, oc := range translateGeminiLineToOpenAI(data, t.modelLabel) {
			t.buf = append(t.buf, []byte("data: "+oc+"\n\n")...)
		}
	}
	n := copy(p, t.buf)
	t.buf = t.buf[n:]
	return n, nil
}

// translateGeminiLineToOpenAI 把单个 Gemini 事件（一个 GenerateContentResponse JSON）翻译为
// 一个或多个 OpenAI chunk JSON 字符串（不含 "data: " 前缀）。顺序：content -> usage -> finish_reason。
func translateGeminiLineToOpenAI(data, modelLabel string) []string {
	var g geminiResp
	if err := json.Unmarshal([]byte(data), &g); err != nil {
		return nil
	}
	var text, finish string
	for _, c := range g.Candidates {
		if c.Content != nil {
			for _, p := range c.Content.Parts {
				text += p.Text
			}
		}
		if c.FinishReason != "" {
			finish = mapFinishReason(c.FinishReason)
		}
	}
	var out []string
	if text != "" {
		out = append(out, chunkJSON("gemini-", modelLabel, map[string]interface{}{"content": text}, nil))
	}
	if g.UsageMetadata != nil {
		out = append(out, chunkJSON("gemini-", modelLabel, map[string]interface{}{}, nil, withUsage(map[string]interface{}{
			"prompt_tokens":     g.UsageMetadata.PromptTokenCount,
			"completion_tokens": g.UsageMetadata.CandidatesTokenCount,
			"total_tokens":      g.UsageMetadata.TotalTokenCount,
		})))
	}
	if finish != "" {
		out = append(out, chunkJSON("gemini-", modelLabel, map[string]interface{}{}, finish))
	}
	return out
}

// chunkJSON 生成单个 OpenAI chat.completion.chunk JSON 字符串。
// idPrefix 用于 chunk id 前缀（如 "gemini-" / "claude-"），便于下游/日志区分来源。
func chunkJSON(idPrefix, modelLabel string, delta map[string]interface{}, finishReason interface{}, opts ...func(map[string]interface{})) string {
	chunk := map[string]interface{}{
		"id":      idPrefix + fmt.Sprintf("%d", time.Now().UnixNano()),
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   modelLabel,
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"delta":         delta,
				"finish_reason": finishReason,
			},
		},
	}
	for _, o := range opts {
		o(chunk)
	}
	b, _ := json.Marshal(chunk)
	return string(b)
}

func withUsage(u map[string]interface{}) func(map[string]interface{}) {
	return func(c map[string]interface{}) {
		c["usage"] = u
	}
}

// ---------- 流式翻译：Claude SSE -> OpenAI SSE ----------

// NewClaudeSSETranslator 返回一个 io.Reader，把 Anthropic 的 SSE 流（event:/data: 行）实时
// 翻译为 OpenAI 兼容的 SSE 流（data: {chunk}\n\n），并在上游流结束时追加 data: [DONE]。
// 与 Gemini 翻译器输出同一契约，使上游转发主循环保持单一、通用的解析逻辑。
// modelLabel 用于 chunk 的 model 字段（通常为 "provider · model"）。
func NewClaudeSSETranslator(src io.Reader, modelLabel string) io.Reader {
	s := bufio.NewScanner(src)
	s.Buffer(make([]byte, 0, 1<<20), 10<<20)
	return &claudeSSETranslator{scanner: s, modelLabel: modelLabel}
}

type claudeSSETranslator struct {
	scanner      *bufio.Scanner
	modelLabel   string
	buf          []byte
	done         bool
	inputTokens  int
	outputTokens int
	finishReason string
}

func (t *claudeSSETranslator) Read(p []byte) (int, error) {
	for len(t.buf) == 0 {
		if t.done {
			return 0, io.EOF
		}
		event, data, eof := t.readEvent()
		if eof {
			// 上游流结束：补 finish_reason + usage + [DONE]
			t.emitFinish()
			continue
		}
		if event == "" || data == "" {
			continue
		}
		for _, oc := range t.translateClaudeEvent(event, data) {
			t.buf = append(t.buf, []byte("data: "+oc+"\n\n")...)
		}
	}
	n := copy(p, t.buf)
	t.buf = t.buf[n:]
	return n, nil
}

// readEvent 从流中读出一个完整 SSE 事件（event: + data: 直到空行或 EOF），返回 (event, data, eof)。
func (t *claudeSSETranslator) readEvent() (event, data string, eof bool) {
	var curEvent, curData string
	sawContent := false
	for t.scanner.Scan() {
		line := strings.TrimRight(t.scanner.Text(), "\r")
		line = strings.TrimSpace(line)
		if line == "" {
			return curEvent, curData, false
		}
		sawContent = true
		switch {
		case strings.HasPrefix(line, "event:"):
			curEvent = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			curData = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
	}
	if sawContent {
		return curEvent, curData, false
	}
	return "", "", true
}

// translateClaudeEvent 把单个 Claude SSE 事件翻译为一个或多个 OpenAI chunk JSON 字符串（不含 "data: " 前缀）。
func (t *claudeSSETranslator) translateClaudeEvent(event, data string) []string {
	var ev map[string]interface{}
	if err := json.Unmarshal([]byte(data), &ev); err != nil {
		return nil
	}
	switch event {
	case "message_start":
		if msg, ok := ev["message"].(map[string]interface{}); ok {
			if u, ok := msg["usage"].(map[string]interface{}); ok {
				if v, ok := u["input_tokens"].(float64); ok {
					t.inputTokens = int(v)
				}
			}
		}
		// 首 chunk：声明 assistant 角色
		return []string{chunkJSON("claude-", t.modelLabel, map[string]interface{}{"role": "assistant"}, nil)}
	case "content_block_start":
		if cb, ok := ev["content_block"].(map[string]interface{}); ok {
			if cb["type"] == "tool_use" {
				id, _ := cb["id"].(string)
				name, _ := cb["name"].(string)
				return []string{chunkJSON("claude-", t.modelLabel, map[string]interface{}{
					"tool_calls": []map[string]interface{}{
						{
							"index": intFromIndex(ev["index"]),
							"id":    id,
							"type":  "function",
							"function": map[string]interface{}{
								"name":      name,
								"arguments": "",
							},
						},
					},
				}, nil)}
			}
		}
		return nil
	case "content_block_delta":
		delta, _ := ev["delta"].(map[string]interface{})
		if delta == nil {
			return nil
		}
		dType, _ := delta["type"].(string)
		switch dType {
		case "text_delta":
			text, _ := delta["text"].(string)
			if text == "" {
				return nil
			}
			return []string{chunkJSON("claude-", t.modelLabel, map[string]interface{}{"content": text}, nil)}
		case "input_json_delta":
			partial, _ := delta["partial_json"].(string)
			return []string{chunkJSON("claude-", t.modelLabel, map[string]interface{}{
				"tool_calls": []map[string]interface{}{
					{
						"index": intFromIndex(ev["index"]),
						"function": map[string]interface{}{
							"arguments": partial,
						},
					},
				},
			}, nil)}
		}
		return nil
	case "content_block_stop", "ping":
		return nil
	case "message_delta":
		if d, ok := ev["delta"].(map[string]interface{}); ok {
			if sr, ok := d["stop_reason"].(string); ok {
				t.finishReason = mapClaudeStopReason(sr)
			}
		}
		if u, ok := ev["usage"].(map[string]interface{}); ok {
			if v, ok := u["output_tokens"].(float64); ok {
				t.outputTokens = int(v)
			}
		}
		return nil
	case "message_stop":
		return nil
	default:
		return nil
	}
}

// emitFinish 在流结束时补一个带 finish_reason（与可选 usage）的 OpenAI chunk，随后追加 [DONE]。
func (t *claudeSSETranslator) emitFinish() {
	finish := t.finishReason
	if finish == "" {
		finish = "stop"
	}
	chunk := map[string]interface{}{
		"id":      "claude-" + fmt.Sprintf("%d", time.Now().UnixNano()),
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   t.modelLabel,
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"delta":         map[string]interface{}{},
				"finish_reason": finish,
			},
		},
	}
	if t.inputTokens > 0 || t.outputTokens > 0 {
		chunk["usage"] = map[string]interface{}{
			"prompt_tokens":     t.inputTokens,
			"completion_tokens": t.outputTokens,
			"total_tokens":      t.inputTokens + t.outputTokens,
		}
	}
	b, _ := json.Marshal(chunk)
	t.buf = append(t.buf, []byte("data: "+string(b)+"\n\n")...)
	t.buf = append(t.buf, []byte("data: [DONE]\n\n")...)
	t.done = true
}

// intFromIndex 把 Claude 事件里的 index 字段（JSON 数字）安全转为 int（默认 0）。
func intFromIndex(v interface{}) int {
	if f, ok := v.(float64); ok {
		return int(f)
	}
	return 0
}

// ---------- OpenAI Responses API (/v1/responses) 请求结构（F3） ----------

type responsesInputText struct {
	Type string `json:"type"` // "input_text"
	Text string `json:"text"`
}

type responsesInputImage struct {
	Type     string `json:"type"` // "input_image"
	ImageURL string `json:"image_url,omitempty"`
}

type responsesFunctionTool struct {
	Type        string      `json:"type"` // "function"
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	Parameters  interface{} `json:"parameters"`
}

type responsesInputItem struct {
	Type      string      `json:"type,omitempty"` // "message" | "function_call" | "function_call_output"
	Role      string      `json:"role,omitempty"`
	Content   interface{} `json:"content,omitempty"` // string | []part
	CallID    string      `json:"call_id,omitempty"`
	Name      string      `json:"name,omitempty"`
	Arguments string      `json:"arguments,omitempty"`
	Output    string      `json:"output,omitempty"`
}

type responsesTextFormat struct {
	Type string `json:"type"` // "json_object"
}

type responsesText struct {
	Format *responsesTextFormat `json:"format,omitempty"`
}

type responsesRequest struct {
	Model             string                  `json:"model"`
	Input             []responsesInputItem    `json:"input"`
	Instructions      string                  `json:"instructions,omitempty"`
	Temperature       *float64                `json:"temperature,omitempty"`
	TopP              *float64                `json:"top_p,omitempty"`
	MaxOutputTokens   *int                    `json:"max_output_tokens,omitempty"`
	Stream            bool                    `json:"stream"`
	Tools             []responsesFunctionTool `json:"tools,omitempty"`
	ToolChoice        interface{}             `json:"tool_choice,omitempty"`
	Text              *responsesText          `json:"text,omitempty"`
	ParallelToolCalls *bool                   `json:"parallel_tool_calls,omitempty"`
}

// ToResponsesBody 将 OpenAI chat.completions 请求体翻译为 OpenAI Responses API（/v1/responses）请求体。
// 返回翻译后的 body 与模型名。Responses API 把 system 提到顶层 instructions，把 chat messages 重排为 input items，
// 把 max_tokens 改为 max_output_tokens，把 response_format:json_object 改为 text.format。
func ToResponsesBody(openAIReq []byte) (responsesBody []byte, model string, err error) {
	var req oaRequest
	if err := json.Unmarshal(openAIReq, &req); err != nil {
		return nil, "", fmt.Errorf("parse openai request: %w", err)
	}
	model = req.Model

	var instructions string
	var input []responsesInputItem
	for _, m := range req.Messages {
		switch m.Role {
		case "system":
			if t := stringContent(m.Content); t != "" {
				if instructions != "" {
					instructions += "\n\n"
				}
				instructions += t
			}
			continue
		case "assistant":
			if len(m.ToolCalls) > 0 {
				for _, tc := range m.ToolCalls {
					input = append(input, responsesInputItem{
						Type:      "function_call",
						CallID:    tc.ID,
						Name:      tc.Function.Name,
						Arguments: tc.Function.Arguments,
					})
				}
				continue
			}
			role := m.Role
			if role == "" {
				role = "assistant"
			}
			input = append(input, responsesInputItem{Role: role, Content: toResponsesContent(m.Content)})
		case "tool":
			input = append(input, responsesInputItem{
				Type:   "function_call_output",
				CallID: m.ToolCallID,
				Output: stringContent(m.Content),
			})
		default:
			role := m.Role
			if role == "" {
				role = "user"
			}
			input = append(input, responsesInputItem{Role: role, Content: toResponsesContent(m.Content)})
		}
	}

	rr := responsesRequest{
		Model:  model,
		Input:  input,
		Stream: req.Stream,
	}
	if instructions != "" {
		rr.Instructions = instructions
	}
	if req.Temperature != nil {
		rr.Temperature = req.Temperature
	}
	if req.TopP != nil {
		rr.TopP = req.TopP
	}
	if req.MaxTokens != nil {
		rr.MaxOutputTokens = req.MaxTokens
	} else if req.MaxCompletionTokens != nil {
		rr.MaxOutputTokens = req.MaxCompletionTokens
	}
	if len(req.Tools) > 0 {
		for _, t := range req.Tools {
			rr.Tools = append(rr.Tools, responsesFunctionTool{
				Type:        "function",
				Name:        t.Function.Name,
				Description: t.Function.Description,
				Parameters:  t.Function.Parameters,
			})
		}
	}
	if req.ToolChoice != nil {
		rr.ToolChoice = mapResponsesToolChoice(req.ToolChoice)
	}
	if req.ResponseFormat != nil && req.ResponseFormat.Type == "json_object" {
		rr.Text = &responsesText{Format: &responsesTextFormat{Type: "json_object"}}
	}

	b, err := json.Marshal(rr)
	if err != nil {
		return nil, "", fmt.Errorf("marshal responses request: %w", err)
	}
	return b, model, nil
}

// toResponsesContent 把 OpenAI content 转为 Responses API input content（string 或 []part）。
// 多模态：image_url → input_image 块。
func toResponsesContent(content interface{}) interface{} {
	switch c := content.(type) {
	case string:
		return c
	case []interface{}:
		var parts []interface{}
		for _, item := range c {
			m, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			typ, _ := m["type"].(string)
			switch typ {
			case "text":
				if t, _ := m["text"].(string); t != "" {
					parts = append(parts, responsesInputText{Type: "input_text", Text: t})
				}
			case "image_url":
				if ij, ok := m["image_url"].(map[string]interface{}); ok {
					if url, _ := ij["url"].(string); url != "" {
						parts = append(parts, responsesInputImage{Type: "input_image", ImageURL: url})
					}
				}
			}
		}
		if len(parts) == 0 {
			return ""
		}
		return parts
	default:
		return ""
	}
}

// mapResponsesToolChoice 把 OpenAI tool_choice 映射为 Responses API tool_choice。
// auto/none/required 原样透传为字符串；{type:"function",function:{name}} → {type:"function",name}。
func mapResponsesToolChoice(tc interface{}) interface{} {
	m, ok := tc.(map[string]interface{})
	if !ok {
		return tc
	}
	switch m["type"] {
	case "auto", "none", "required":
		return m["type"]
	case "function":
		if fn, ok := m["function"].(map[string]interface{}); ok {
			name, _ := fn["name"].(string)
			return map[string]interface{}{"type": "function", "name": name}
		}
	}
	return tc
}

// ---------- OpenAI Responses API 响应结构（F3） ----------

type responsesResp struct {
	ID     string                   `json:"id"`
	Model  string                   `json:"model"`
	Status string                   `json:"status"`
	Output []map[string]interface{} `json:"output"`
	Usage  *struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage"`
}

// ResponsesToOpenAI 将 Responses API 非流式响应翻译为 OpenAI chat.completion 响应（model 设为传入的 model）。
// 支持 output_text 拼接与 function_call → tool_calls。
func ResponsesToOpenAI(responsesRespBytes []byte, model string) ([]byte, error) {
	var r responsesResp
	if err := json.Unmarshal(responsesRespBytes, &r); err != nil {
		return nil, fmt.Errorf("parse responses response: %w", err)
	}
	var content string
	var toolCalls []map[string]interface{}
	sawFunc := false
	for _, item := range r.Output {
		typ, _ := item["type"].(string)
		switch typ {
		case "message":
			if arr, ok := item["content"].([]interface{}); ok {
				for _, c := range arr {
					if cm, ok := c.(map[string]interface{}); ok {
						if cm["type"] == "output_text" {
							if t, ok := cm["text"].(string); ok {
								content += t
							}
						}
					}
				}
			}
		case "function_call":
			id, _ := item["call_id"].(string)
			name, _ := item["name"].(string)
			args, _ := item["arguments"].(string)
			toolCalls = append(toolCalls, map[string]interface{}{
				"id":       id,
				"type":     "function",
				"function": map[string]interface{}{"name": name, "arguments": args},
			})
			sawFunc = true
		}
	}
	msg := map[string]interface{}{"role": "assistant", "content": content}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}
	finish := "stop"
	if sawFunc {
		finish = "tool_calls"
	}
	id := "resp-" + strings.TrimPrefix(r.ID, "resp_")
	oa := map[string]interface{}{
		"id":      id,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"message":       msg,
				"finish_reason": finish,
			},
		},
	}
	if r.Usage != nil {
		total := r.Usage.TotalTokens
		if total == 0 {
			total = r.Usage.InputTokens + r.Usage.OutputTokens
		}
		oa["usage"] = map[string]interface{}{
			"prompt_tokens":     r.Usage.InputTokens,
			"completion_tokens": r.Usage.OutputTokens,
			"total_tokens":      total,
		}
	} else {
		oa["usage"] = nil
	}
	return json.Marshal(oa)
}

type responsesErrorEnvelope struct {
	Error *struct {
		Message string      `json:"message"`
		Type    string      `json:"type"`
		Code    interface{} `json:"code"`
	} `json:"error"`
}

// ResponsesErrorToOpenAI 把 Responses API 错误体翻译为 OpenAI 错误形（保留原状态码由调用方设置）。
func ResponsesErrorToOpenAI(responsesErr []byte) []byte {
	var e responsesErrorEnvelope
	if err := json.Unmarshal(responsesErr, &e); err == nil && e.Error != nil {
		out, _ := json.Marshal(map[string]interface{}{
			"error": map[string]interface{}{
				"message": e.Error.Message,
				"type":    e.Error.Type,
				"code":    e.Error.Code,
			},
		})
		return out
	}
	return responsesErr
}

// ---------- 流式翻译：Responses SSE -> OpenAI SSE ----------

// NewResponsesSSETranslator 返回一个 io.Reader，把 OpenAI Responses API 的 SSE 流（event:/data: 行）实时
// 翻译为 OpenAI 兼容的 SSE 流（data: {chunk}\n\n），并在上游流结束时追加 data: [DONE]。
// 与 Gemini/Claude 翻译器输出同一契约，使上游转发主循环保持单一、通用的解析逻辑。
// modelLabel 用于 chunk 的 model 字段（通常为 "provider · model"）。
func NewResponsesSSETranslator(src io.Reader, modelLabel string) io.Reader {
	s := bufio.NewScanner(src)
	s.Buffer(make([]byte, 0, 1<<20), 10<<20)
	return &responsesSSETranslator{scanner: s, modelLabel: modelLabel, toolIndex: map[string]int{}}
}

type responsesSSETranslator struct {
	scanner      *bufio.Scanner
	modelLabel   string
	buf          []byte
	done         bool
	inputTokens  int
	outputTokens int
	finishReason string
	sawRole      bool
	toolIndex    map[string]int
	toolCounter  int
}

func (t *responsesSSETranslator) Read(p []byte) (int, error) {
	for len(t.buf) == 0 {
		if t.done {
			return 0, io.EOF
		}
		event, data, eof := t.readEvent()
		if eof {
			t.emitFinish()
			continue
		}
		if event == "" || data == "" {
			continue
		}
		for _, oc := range t.translateResponsesEvent(event, data) {
			t.buf = append(t.buf, []byte("data: "+oc+"\n\n")...)
		}
	}
	n := copy(p, t.buf)
	t.buf = t.buf[n:]
	return n, nil
}

// readEvent 从流中读出一个完整 SSE 事件（event: + data: 直到空行或 EOF），返回 (event, data, eof)。
func (t *responsesSSETranslator) readEvent() (event, data string, eof bool) {
	var curEvent, curData string
	sawContent := false
	for t.scanner.Scan() {
		line := strings.TrimRight(t.scanner.Text(), "\r")
		line = strings.TrimSpace(line)
		if line == "" {
			return curEvent, curData, false
		}
		sawContent = true
		switch {
		case strings.HasPrefix(line, "event:"):
			curEvent = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			curData = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
	}
	if sawContent {
		return curEvent, curData, false
	}
	return "", "", true
}

// translateResponsesEvent 把单个 Responses SSE 事件翻译为一个或多个 OpenAI chunk JSON 字符串（不含 "data: " 前缀）。
func (t *responsesSSETranslator) translateResponsesEvent(event, data string) []string {
	var ev map[string]interface{}
	if err := json.Unmarshal([]byte(data), &ev); err != nil {
		return nil
	}
	switch event {
	case "response.created", "response.in_progress", "response.queued":
		return nil
	case "response.output_item.added":
		item, _ := ev["item"].(map[string]interface{})
		if item == nil {
			return nil
		}
		typ, _ := item["type"].(string)
		switch typ {
		case "message":
			if !t.sawRole {
				t.sawRole = true
				return []string{chunkJSON("resp-", t.modelLabel, map[string]interface{}{"role": "assistant"}, nil)}
			}
			return nil
		case "function_call":
			id, _ := item["call_id"].(string)
			name, _ := item["name"].(string)
			idx := t.toolCounter
			t.toolCounter++
			if id != "" {
				t.toolIndex[id] = idx
			}
			return []string{chunkJSON("resp-", t.modelLabel, map[string]interface{}{
				"tool_calls": []map[string]interface{}{
					{
						"index": idx,
						"id":    id,
						"type":  "function",
						"function": map[string]interface{}{
							"name":      name,
							"arguments": "",
						},
					},
				},
			}, nil)}
		}
		return nil
	case "response.output_item.done", "response.content_part.added", "response.content_part.done":
		return nil
	case "response.output_text.delta":
		delta, _ := ev["delta"].(string)
		if delta == "" {
			return nil
		}
		return []string{chunkJSON("resp-", t.modelLabel, map[string]interface{}{"content": delta}, nil)}
	case "response.function_call_arguments.delta":
		delta, _ := ev["delta"].(string)
		itemID, _ := ev["item_id"].(string)
		idx := 0
		if itemID != "" {
			if i, ok := t.toolIndex[itemID]; ok {
				idx = i
			}
		}
		return []string{chunkJSON("resp-", t.modelLabel, map[string]interface{}{
			"tool_calls": []map[string]interface{}{
				{
					"index": idx,
					"function": map[string]interface{}{
						"arguments": delta,
					},
				},
			},
		}, nil)}
	case "response.refusal.delta":
		delta, _ := ev["delta"].(string)
		if delta == "" {
			return nil
		}
		return []string{chunkJSON("resp-", t.modelLabel, map[string]interface{}{"content": delta}, nil)}
	case "response.completed":
		if r, ok := ev["response"].(map[string]interface{}); ok {
			if u, ok := r["usage"].(map[string]interface{}); ok {
				if v, ok := u["input_tokens"].(float64); ok {
					t.inputTokens = int(v)
				}
				if v, ok := u["output_tokens"].(float64); ok {
					t.outputTokens = int(v)
				}
			}
			if out, ok := r["output"].([]interface{}); ok {
				for _, o := range out {
					if om, ok := o.(map[string]interface{}); ok {
						if om["type"] == "function_call" {
							t.finishReason = "tool_calls"
						}
					}
				}
			}
		}
		return nil
	case "response.failed":
		t.finishReason = "stop"
		return nil
	default:
		return nil
	}
}

// emitFinish 在流结束时补一个带 finish_reason（与可选 usage）的 OpenAI chunk，随后追加 [DONE]。
func (t *responsesSSETranslator) emitFinish() {
	finish := t.finishReason
	if finish == "" {
		finish = "stop"
	}
	chunk := map[string]interface{}{
		"id":      "resp-" + fmt.Sprintf("%d", time.Now().UnixNano()),
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   t.modelLabel,
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"delta":         map[string]interface{}{},
				"finish_reason": finish,
			},
		},
	}
	if t.inputTokens > 0 || t.outputTokens > 0 {
		chunk["usage"] = map[string]interface{}{
			"prompt_tokens":     t.inputTokens,
			"completion_tokens": t.outputTokens,
			"total_tokens":      t.inputTokens + t.outputTokens,
		}
	}
	b, _ := json.Marshal(chunk)
	t.buf = append(t.buf, []byte("data: "+string(b)+"\n\n")...)
	t.buf = append(t.buf, []byte("data: [DONE]\n\n")...)
	t.done = true
}
