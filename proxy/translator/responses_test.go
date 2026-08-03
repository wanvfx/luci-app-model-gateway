package translator

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// ---------- OpenAI Responses API (F3) 测试 ----------

func TestToResponsesBodyBasic(t *testing.T) {
	oa := `{
		"model": "gpt-4o",
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
	b, model, err := ToResponsesBody([]byte(oa))
	if err != nil {
		t.Fatalf("ToResponsesBody err: %v", err)
	}
	if model != "gpt-4o" {
		t.Fatalf("model = %q", model)
	}
	var r responsesRequest
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatalf("unmarshal responses: %v", err)
	}
	if r.Instructions != "You are helpful." {
		t.Fatalf("instructions wrong: %q", r.Instructions)
	}
	items := r.Input
	if len(items) != 3 {
		t.Fatalf("input items len = %d, want 3", len(items))
	}
	if items[0].Role != "user" || items[1].Role != "assistant" || items[2].Role != "user" {
		t.Fatalf("role mapping wrong: %+v", items)
	}
	if items[0].Content != "Hello" {
		t.Fatalf("first content wrong: %+v", items[0].Content)
	}
	if r.Temperature == nil || *r.Temperature != 0.5 {
		t.Fatalf("temperature wrong: %+v", r.Temperature)
	}
	if r.MaxOutputTokens == nil || *r.MaxOutputTokens != 100 {
		t.Fatalf("maxOutputTokens wrong: %+v", r.MaxOutputTokens)
	}
	if r.Stream {
		t.Fatalf("stream should be false")
	}
}

func TestToResponsesBodyTools(t *testing.T) {
	oa := `{
		"model": "gpt-4o",
		"messages": [{"role":"user","content":"Hi"}],
		"tools": [
			{"type":"function","function":{"name":"get_weather","description":"get weather","parameters":{"type":"object","properties":{"loc":{"type":"string"}}}}}
		],
		"tool_choice": {"type":"function","function":{"name":"get_weather"}},
		"response_format": {"type":"json_object"}
	}`
	b, _, err := ToResponsesBody([]byte(oa))
	if err != nil {
		t.Fatalf("ToResponsesBody err: %v", err)
	}
	var r responsesRequest
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatalf("unmarshal responses: %v", err)
	}
	if len(r.Tools) != 1 || r.Tools[0].Name != "get_weather" || r.Tools[0].Type != "function" {
		t.Fatalf("tools wrong: %+v", r.Tools)
	}
	tc, ok := r.ToolChoice.(map[string]interface{})
	if !ok || tc["type"] != "function" || tc["name"] != "get_weather" {
		t.Fatalf("tool_choice wrong: %+v", r.ToolChoice)
	}
	if r.Text == nil || r.Text.Format == nil || r.Text.Format.Type != "json_object" {
		t.Fatalf("text.format wrong: %+v", r.Text)
	}
}

func TestToResponsesBodyToolCalls(t *testing.T) {
	oa := `{
		"model": "gpt-4o",
		"messages": [
			{"role":"user","content":"What's the weather?"},
			{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"loc\":\"SF\"}"}}]},
			{"role":"tool","tool_call_id":"call_1","content":"sunny"}
		]
	}`
	b, _, err := ToResponsesBody([]byte(oa))
	if err != nil {
		t.Fatalf("ToResponsesBody err: %v", err)
	}
	var r responsesRequest
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatalf("unmarshal responses: %v", err)
	}
	items := r.Input
	if len(items) != 3 {
		t.Fatalf("input items len = %d, want 3", len(items))
	}
	if items[0].Role != "user" {
		t.Fatalf("item0 role wrong: %+v", items[0])
	}
	if items[1].Type != "function_call" || items[1].Name != "get_weather" || items[1].CallID != "call_1" || items[1].Arguments != "{\"loc\":\"SF\"}" {
		t.Fatalf("function_call item wrong: %+v", items[1])
	}
	if items[2].Type != "function_call_output" || items[2].CallID != "call_1" || items[2].Output != "sunny" {
		t.Fatalf("function_call_output item wrong: %+v", items[2])
	}
}

func TestResponsesToOpenAI(t *testing.T) {
	resp := `{
		"id": "resp_abc",
		"object": "response",
		"model": "gpt-4o",
		"output": [
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Hello "},{"type":"output_text","text":"world"}]}
		],
		"usage": {"input_tokens": 10, "output_tokens": 5, "total_tokens": 15}
	}`
	b, err := ResponsesToOpenAI([]byte(resp), "Provider · gpt-4o")
	if err != nil {
		t.Fatalf("ResponsesToOpenAI err: %v", err)
	}
	var oa map[string]interface{}
	if err := json.Unmarshal(b, &oa); err != nil {
		t.Fatalf("unmarshal openai: %v", err)
	}
	if oa["object"] != "chat.completion" {
		t.Fatalf("object wrong: %v", oa["object"])
	}
	choices := oa["choices"].([]interface{})
	msg := choices[0].(map[string]interface{})["message"].(map[string]interface{})
	if msg["content"] != "Hello world" {
		t.Fatalf("content wrong: %v", msg["content"])
	}
	usage := oa["usage"].(map[string]interface{})
	if usage["prompt_tokens"].(float64) != 10 || usage["completion_tokens"].(float64) != 5 {
		t.Fatalf("usage wrong: %+v", usage)
	}
	if oa["id"] != "resp-abc" {
		t.Fatalf("id wrong: %v", oa["id"])
	}
}

func TestResponsesToOpenAIToolUse(t *testing.T) {
	resp := `{
		"id": "resp_xyz",
		"object": "response",
		"model": "gpt-4o",
		"output": [
			{"type":"function_call","call_id":"call_9","name":"get_weather","arguments":"{\"loc\":\"SF\"}"}
		]
	}`
	b, err := ResponsesToOpenAI([]byte(resp), "Provider · gpt-4o")
	if err != nil {
		t.Fatalf("ResponsesToOpenAI err: %v", err)
	}
	var oa map[string]interface{}
	if err := json.Unmarshal(b, &oa); err != nil {
		t.Fatalf("unmarshal openai: %v", err)
	}
	choice := oa["choices"].([]interface{})[0].(map[string]interface{})
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason should be tool_calls: %v", choice["finish_reason"])
	}
	msg := choice["message"].(map[string]interface{})
	tcs := msg["tool_calls"].([]interface{})
	if len(tcs) != 1 {
		t.Fatalf("tool_calls len = %d", len(tcs))
	}
	tc := tcs[0].(map[string]interface{})
	if tc["id"] != "call_9" || tc["type"] != "function" {
		t.Fatalf("tool_call id/type wrong: %+v", tc)
	}
	fn := tc["function"].(map[string]interface{})
	if fn["name"] != "get_weather" || fn["arguments"] != "{\"loc\":\"SF\"}" {
		t.Fatalf("tool_call function wrong: %+v", fn)
	}
}

func TestResponsesErrorToOpenAI(t *testing.T) {
	resp := `{"error":{"message":"invalid model","type":"invalid_request_error","code":"400"}}`
	out := ResponsesErrorToOpenAI([]byte(resp))
	var e struct {
		Error struct {
			Message string      `json:"message"`
			Type    string      `json:"type"`
			Code    interface{} `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(out, &e); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if e.Error.Message != "invalid model" || e.Error.Type != "invalid_request_error" {
		t.Fatalf("error transln wrong: %+v", e)
	}
}

func TestResponsesSSETranslator(t *testing.T) {
	src := strings.Join([]string{
		"event: response.created",
		`data: {"response":{"id":"resp_1","model":"gpt-4o"}}`,
		"",
		"event: response.output_item.added",
		`data: {"item":{"type":"message","role":"assistant","content":[]}}`,
		"",
		"event: response.output_text.delta",
		`data: {"delta":"Hello "}`,
		"",
		"event: response.output_text.delta",
		`data: {"delta":"world"}`,
		"",
		"event: response.completed",
		`data: {"response":{"status":"completed","usage":{"input_tokens":7,"output_tokens":2},"output":[{"type":"message"}]}}`,
		"",
	}, "\n")

	r := NewResponsesSSETranslator(strings.NewReader(src), "Provider · gpt-4o")
	raw, _ := io.ReadAll(r)
	out := string(raw)
	if !strings.Contains(out, `"role":"assistant"`) {
		t.Fatalf("role missing:\n%s", out)
	}
	if !strings.Contains(out, `"content":"Hello "`) || !strings.Contains(out, `"content":"world"`) {
		t.Fatalf("content delta missing:\n%s", out)
	}
	if !strings.Contains(out, `"finish_reason":"stop"`) {
		t.Fatalf("finish_reason missing:\n%s", out)
	}
	if !strings.Contains(out, `"prompt_tokens":7`) || !strings.Contains(out, `"completion_tokens":2`) {
		t.Fatalf("usage missing:\n%s", out)
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Fatalf("[DONE] missing:\n%s", out)
	}
}

func TestResponsesSSETranslatorToolUse(t *testing.T) {
	src := strings.Join([]string{
		"event: response.output_item.added",
		`data: {"item":{"type":"function_call","call_id":"call_a","name":"get_weather"}}`,
		"",
		"event: response.function_call_arguments.delta",
		`data: {"item_id":"call_a","delta":"{\"loc\""}`,
		"",
		"event: response.function_call_arguments.delta",
		`data: {"item_id":"call_a","delta":"\"SF\"}"}`,
		"",
		"event: response.completed",
		`data: {"response":{"status":"completed","usage":{"input_tokens":3,"output_tokens":1},"output":[{"type":"function_call"}]}}`,
		"",
	}, "\n")

	r := NewResponsesSSETranslator(strings.NewReader(src), "Provider · gpt-4o")
	raw, _ := io.ReadAll(r)
	out := string(raw)
	if !strings.Contains(out, `"finish_reason":"tool_calls"`) {
		t.Fatalf("finish_reason should be tool_calls:\n%s", out)
	}
	if !strings.Contains(out, `"id":"call_a"`) || !strings.Contains(out, `"name":"get_weather"`) {
		t.Fatalf("tool id/name missing:\n%s", out)
	}
	// partial json 被分片流式推送（JSON 转义后），应分别出现在不同 chunk 的 arguments 中
	if !strings.Contains(out, `{\"loc\"`) || !strings.Contains(out, `\"SF\"}`) {
		t.Fatalf("tool partial json missing:\n%s", out)
	}
}
