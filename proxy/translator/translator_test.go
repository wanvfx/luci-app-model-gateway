package translator

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func TestToGeminiBodyBasic(t *testing.T) {
	oa := `{
		"model": "gemini-1.5-flash",
		"stream": false,
		"messages": [
			{"role":"system","content":"You are helpful."},
			{"role":"user","content":"Hello"},
			{"role":"assistant","content":"Hi there"},
			{"role":"user","content":"Bye"}
		],
		"temperature": 0.5,
		"max_tokens": 100
	}`
	b, model, stream, err := ToGeminiBody([]byte(oa))
	if err != nil {
		t.Fatalf("ToGeminiBody err: %v", err)
	}
	if model != "gemini-1.5-flash" {
		t.Fatalf("model = %q", model)
	}
	if stream {
		t.Fatalf("stream should be false")
	}
	var g geminiRequest
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatalf("unmarshal gemini: %v", err)
	}
	if g.SystemInstruction == nil || len(g.SystemInstruction.Parts) != 1 || g.SystemInstruction.Parts[0].Text != "You are helpful." {
		t.Fatalf("systemInstruction wrong: %+v", g.SystemInstruction)
	}
	if len(g.Contents) != 3 {
		t.Fatalf("contents len = %d, want 3", len(g.Contents))
	}
	if g.Contents[0].Role != "user" || g.Contents[1].Role != "model" || g.Contents[2].Role != "user" {
		t.Fatalf("role mapping wrong: %+v", g.Contents)
	}
	if g.Contents[0].Parts[0].Text != "Hello" {
		t.Fatalf("first content wrong: %+v", g.Contents[0])
	}
	if g.GenerationConfig.Temperature == nil || *g.GenerationConfig.Temperature != 0.5 {
		t.Fatalf("temperature wrong: %+v", g.GenerationConfig.Temperature)
	}
	if g.GenerationConfig.MaxOutputTokens == nil || *g.GenerationConfig.MaxOutputTokens != 100 {
		t.Fatalf("maxOutputTokens wrong: %+v", g.GenerationConfig.MaxOutputTokens)
	}
}

func TestToGeminiBodyImage(t *testing.T) {
	oa := `{
		"model": "gemini-1.5-pro",
		"messages": [
			{"role":"user","content":[
				{"type":"text","text":"What is this?"},
				{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}}
			]}
		]
	}`
	b, _, _, err := ToGeminiBody([]byte(oa))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	var g geminiRequest
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(g.Contents) != 1 || len(g.Contents[0].Parts) != 2 {
		t.Fatalf("parts count wrong: %+v", g.Contents)
	}
	if g.Contents[0].Parts[0].Text != "What is this?" {
		t.Fatalf("text part wrong")
	}
	if g.Contents[0].Parts[1].InlineData == nil || g.Contents[0].Parts[1].InlineData.MimeType != "image/png" || g.Contents[0].Parts[1].InlineData.Data != "iVBORw0KGgo=" {
		t.Fatalf("inline_data wrong: %+v", g.Contents[0].Parts[1])
	}
}

func TestGeminiToOpenAI(t *testing.T) {
	g := `{
		"candidates":[{"content":{"parts":[{"text":"Hello "},{"text":"world"}],"role":"model"},"finishReason":"STOP","index":0}],
		"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":2,"totalTokenCount":12}
	}`
	b, err := GeminiToOpenAI([]byte(g), "Provider · gemini-1.5-flash")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	var oa map[string]interface{}
	if err := json.Unmarshal(b, &oa); err != nil {
		t.Fatalf("unmarshal openai: %v", err)
	}
	if oa["model"] != "Provider · gemini-1.5-flash" {
		t.Fatalf("model = %v", oa["model"])
	}
	choices := oa["choices"].([]interface{})
	msg := choices[0].(map[string]interface{})["message"].(map[string]interface{})
	if msg["content"] != "Hello world" {
		t.Fatalf("content = %v", msg["content"])
	}
	if choices[0].(map[string]interface{})["finish_reason"] != "stop" {
		t.Fatalf("finish_reason wrong")
	}
	usage := oa["usage"].(map[string]interface{})
	if usage["total_tokens"] != float64(12) || usage["prompt_tokens"] != float64(10) {
		t.Fatalf("usage wrong: %+v", usage)
	}
}

func TestGeminiToOpenAIMaxTokens(t *testing.T) {
	g := `{"candidates":[{"content":{"parts":[{"text":"x"}],"role":"model"},"finishReason":"MAX_TOKENS"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`
	b, err := GeminiToOpenAI([]byte(g), "m")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	var oa map[string]interface{}
	json.Unmarshal(b, &oa)
	choices := oa["choices"].([]interface{})
	if choices[0].(map[string]interface{})["finish_reason"] != "length" {
		t.Fatalf("finish_reason should be length, got %v", choices[0].(map[string]interface{})["finish_reason"])
	}
}

func TestGeminiErrorToOpenAI(t *testing.T) {
	g := `{"error":{"code":400,"message":"API key not valid","status":"INVALID_ARGUMENT"}}`
	out := GeminiErrorToOpenAI([]byte(g))
	var e map[string]interface{}
	json.Unmarshal(out, &e)
	inner := e["error"].(map[string]interface{})
	if inner["message"] != "API key not valid" {
		t.Fatalf("message wrong: %+v", inner)
	}
	if inner["type"] != "INVALID_ARGUMENT" {
		t.Fatalf("type wrong: %+v", inner)
	}
}

func TestGeminiSSETranslator(t *testing.T) {
	// 模拟 Gemini 流式事件：两段内容 + 一段 usage + 一段 finish（无内容）
	src := strings.Join([]string{
		"data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"Hello \"}],\"role\":\"model\"}}]}",
		"data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"world\"}],\"role\":\"model\"}}]}",
		"data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"!\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":5,\"candidatesTokenCount\":3,\"totalTokenCount\":8}}",
		"",
	}, "\n")

	r := NewGeminiSSETranslator(strings.NewReader(src), "Provider · gemini-1.5-flash")
	raw, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	out := string(raw)
	if !strings.Contains(out, "data: [DONE]") {
		t.Fatalf("missing [DONE]:\n%s", out)
	}
	// 应有三个 content chunk
	if strings.Count(out, `"content":"Hello "`) == 0 ||
		strings.Count(out, `"content":"world"`) == 0 ||
		strings.Count(out, `"content":"!"`) == 0 {
		t.Fatalf("content chunks missing:\n%s", out)
	}
	// usage 应出现在某个 chunk
	if !strings.Contains(out, `"usage"`) || !strings.Contains(out, `"total_tokens":8`) {
		t.Fatalf("usage missing:\n%s", out)
	}
	// 末尾 finish_reason=stop
	if !strings.Contains(out, `"finish_reason":"stop"`) {
		t.Fatalf("finish_reason missing:\n%s", out)
	}
	// 每个 data: 行应以 \n\n 结尾（标准 SSE）
	for _, line := range strings.Split(out, "\n\n") {
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			t.Fatalf("non-SSE line: %q", line)
		}
	}
}

func TestToClaudeBodyBasic(t *testing.T) {
	oa := `{
		"model": "claude-3-5-sonnet-20241022",
		"stream": false,
		"messages": [
			{"role":"system","content":"You are helpful."},
			{"role":"user","content":"Hello"},
			{"role":"assistant","content":"Hi there"},
			{"role":"user","content":"Bye"}
		],
		"temperature": 0.7,
		"stop": ["STOP"]
	}`
	b, model, err := ToClaudeBody([]byte(oa))
	if err != nil {
		t.Fatalf("ToClaudeBody err: %v", err)
	}
	if model != "claude-3-5-sonnet-20241022" {
		t.Fatalf("model = %q", model)
	}
	var c claudeRequest
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatalf("unmarshal claude: %v", err)
	}
	if c.System != "You are helpful." {
		t.Fatalf("system wrong: %q", c.System)
	}
	if len(c.Messages) != 3 {
		t.Fatalf("messages len = %d, want 3", len(c.Messages))
	}
	if c.Messages[0].Role != "user" || c.Messages[1].Role != "assistant" || c.Messages[2].Role != "user" {
		t.Fatalf("role mapping wrong: %+v", c.Messages)
	}
	if c.Messages[0].Content != "Hello" || c.Messages[2].Content != "Bye" {
		t.Fatalf("content wrong: %+v", c.Messages)
	}
	if c.MaxTokens != claudeDefaultMaxTokens {
		t.Fatalf("max_tokens default wrong: %d", c.MaxTokens)
	}
	if c.Temperature == nil || *c.Temperature != 0.7 {
		t.Fatalf("temperature wrong: %+v", c.Temperature)
	}
	if len(c.StopSequences) != 1 || c.StopSequences[0] != "STOP" {
		t.Fatalf("stop_sequences wrong: %+v", c.StopSequences)
	}
	if c.Stream {
		t.Fatalf("stream should be false")
	}
}

func TestToClaudeBodyMaxTokensAndStream(t *testing.T) {
	oa := `{"model":"claude-3-opus","stream":true,"messages":[{"role":"user","content":"Hi"}],"max_tokens":2048}`
	b, _, err := ToClaudeBody([]byte(oa))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	var c claudeRequest
	json.Unmarshal(b, &c)
	if c.MaxTokens != 2048 {
		t.Fatalf("max_tokens = %d, want 2048", c.MaxTokens)
	}
	if !c.Stream {
		t.Fatalf("stream should be true")
	}
}

func TestToClaudeBodyImage(t *testing.T) {
	oa := `{
		"model": "claude-3-5-sonnet",
		"messages": [
			{"role":"user","content":[
				{"type":"text","text":"What is this?"},
				{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}}
			]}
		]
	}`
	b, _, err := ToClaudeBody([]byte(oa))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	var c struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string               `json:"role"`
			Content []claudeContentBlock `json:"content"`
		} `json:"messages"`
	}
	json.Unmarshal(b, &c)
	if len(c.Messages) != 1 {
		t.Fatalf("messages len = %d", len(c.Messages))
	}
	blocks := c.Messages[0].Content
	if len(blocks) != 2 {
		t.Fatalf("content blocks len = %d, want 2: %+v", len(blocks), blocks)
	}
	if blocks[0].Type != "text" || blocks[0].Text != "What is this?" {
		t.Fatalf("text block wrong: %+v", blocks[0])
	}
	if blocks[1].Type != "image" || blocks[1].Source == nil || blocks[1].Source.Type != "base64" || blocks[1].Source.MediaType != "image/png" || blocks[1].Source.Data != "iVBORw0KGgo=" {
		t.Fatalf("image block wrong: %+v", blocks[1])
	}
}

func TestToClaudeBodyTools(t *testing.T) {
	oa := `{
		"model":"claude-3-5-sonnet",
		"messages":[{"role":"user","content":"Hi"}],
		"tools":[{"type":"function","function":{"name":"get_weather","description":"weather","parameters":{"type":"object","properties":{"loc":{"type":"string"}}}}}],
		"tool_choice":{"type":"function","function":{"name":"get_weather"}}
	}`
	b, _, err := ToClaudeBody([]byte(oa))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	var c claudeRequest
	json.Unmarshal(b, &c)
	if len(c.Tools) != 1 || c.Tools[0].Name != "get_weather" {
		t.Fatalf("tools wrong: %+v", c.Tools)
	}
	tc, ok := c.ToolChoice.(map[string]interface{})
	if !ok || tc["type"] != "tool" || tc["name"] != "get_weather" {
		t.Fatalf("tool_choice wrong: %+v", c.ToolChoice)
	}
}

func TestClaudeToOpenAI(t *testing.T) {
	c := `{
		"id":"msg_1","type":"message","role":"assistant","model":"claude-3-5-sonnet",
		"content":[{"type":"text","text":"Hello "},{"type":"text","text":"world"}],
		"stop_reason":"end_turn",
		"usage":{"input_tokens":10,"output_tokens":2}
	}`
	b, err := ClaudeToOpenAI([]byte(c), "Provider · claude-3-5-sonnet")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	var oa map[string]interface{}
	json.Unmarshal(b, &oa)
	if oa["model"] != "Provider · claude-3-5-sonnet" {
		t.Fatalf("model = %v", oa["model"])
	}
	choices := oa["choices"].([]interface{})
	msg := choices[0].(map[string]interface{})["message"].(map[string]interface{})
	if msg["content"] != "Hello world" {
		t.Fatalf("content = %v", msg["content"])
	}
	if choices[0].(map[string]interface{})["finish_reason"] != "stop" {
		t.Fatalf("finish_reason wrong")
	}
	usage := oa["usage"].(map[string]interface{})
	if usage["total_tokens"] != float64(12) {
		t.Fatalf("usage wrong: %+v", usage)
	}
}

func TestClaudeToOpenAIToolUse(t *testing.T) {
	c := `{
		"id":"msg_1","type":"message","role":"assistant","model":"claude-3-5-sonnet",
		"content":[
			{"type":"text","text":"Sure"},
			{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"loc":"SF"}}
		],
		"stop_reason":"tool_use",
		"usage":{"input_tokens":8,"output_tokens":4}
	}`
	b, err := ClaudeToOpenAI([]byte(c), "m")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	var oa map[string]interface{}
	json.Unmarshal(b, &oa)
	choices := oa["choices"].([]interface{})
	msg := choices[0].(map[string]interface{})["message"].(map[string]interface{})
	if msg["content"] != "Sure" {
		t.Fatalf("content = %v", msg["content"])
	}
	if choices[0].(map[string]interface{})["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason should be tool_calls, got %v", choices[0].(map[string]interface{})["finish_reason"])
	}
	tcs := msg["tool_calls"].([]interface{})
	tc := tcs[0].(map[string]interface{})
	if tc["id"] != "toolu_1" {
		t.Fatalf("tool id wrong: %v", tc["id"])
	}
	fn := tc["function"].(map[string]interface{})
	if fn["name"] != "get_weather" {
		t.Fatalf("tool name wrong: %v", fn["name"])
	}
	if fn["arguments"] != `{"loc":"SF"}` {
		t.Fatalf("tool arguments wrong: %v", fn["arguments"])
	}
}

func TestClaudeErrorToOpenAI(t *testing.T) {
	c := `{"type":"error","error":{"type":"invalid_request_error","message":"x-api-key header is required"}}`
	out := ClaudeErrorToOpenAI([]byte(c))
	var e map[string]interface{}
	json.Unmarshal(out, &e)
	inner := e["error"].(map[string]interface{})
	if inner["message"] != "x-api-key header is required" {
		t.Fatalf("message wrong: %+v", inner)
	}
}

func TestClaudeSSETranslator(t *testing.T) {
	src := strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-3-5-sonnet","usage":{"input_tokens":5,"output_tokens":0}}}`,
		"",
		"event: content_block_start",
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello "}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"world"}}`,
		"",
		"event: content_block_stop",
		`data: {"type":"content_block_stop","index":0}`,
		"",
		"event: message_delta",
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`,
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
	}, "\n")

	r := NewClaudeSSETranslator(strings.NewReader(src), "Provider · claude-3-5-sonnet")
	raw, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	out := string(raw)
	if !strings.Contains(out, "data: [DONE]") {
		t.Fatalf("missing [DONE]:\n%s", out)
	}
	if strings.Count(out, `"content":"Hello "`) == 0 || strings.Count(out, `"content":"world"`) == 0 {
		t.Fatalf("content chunks missing:\n%s", out)
	}
	if !strings.Contains(out, `"role":"assistant"`) {
		t.Fatalf("role chunk missing:\n%s", out)
	}
	if !strings.Contains(out, `"finish_reason":"stop"`) {
		t.Fatalf("finish_reason missing:\n%s", out)
	}
	if !strings.Contains(out, `"usage"`) || !strings.Contains(out, `"total_tokens":8`) {
		t.Fatalf("usage missing:\n%s", out)
	}
	// 每个 data: 行应以 \n\n 结尾（标准 SSE）
	for _, line := range strings.Split(out, "\n\n") {
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			t.Fatalf("non-SSE line: %q", line)
		}
	}
}

func TestClaudeSSETranslatorToolUse(t *testing.T) {
	src := strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"usage":{"input_tokens":3,"output_tokens":0}}}`,
		"",
		"event: content_block_start",
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather"}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"loc\""}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"SF\"}"}}`,
		"",
		"event: content_block_stop",
		`data: {"type":"content_block_stop","index":0}`,
		"",
		"event: message_delta",
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":2}}`,
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
	}, "\n")

	r := NewClaudeSSETranslator(strings.NewReader(src), "Provider · claude-3-5-sonnet")
	raw, _ := io.ReadAll(r)
	out := string(raw)
	if !strings.Contains(out, `"finish_reason":"tool_calls"`) {
		t.Fatalf("finish_reason should be tool_calls:\n%s", out)
	}
	if !strings.Contains(out, `"id":"toolu_1"`) || !strings.Contains(out, `"name":"get_weather"`) {
		t.Fatalf("tool id/name missing:\n%s", out)
	}
	// partial_json 被分片流式推送（JSON 转义后），应分别出现在不同 chunk 的 arguments 中
	if !strings.Contains(out, `{\"loc\"`) || !strings.Contains(out, `\"SF\"}`) {
		t.Fatalf("tool partial json missing:\n%s", out)
	}
}
