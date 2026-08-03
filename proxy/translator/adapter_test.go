package translator

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
)

// ---------- 工具函数 ----------

func TestJSONPath(t *testing.T) {
	var v interface{}
	if err := json.Unmarshal([]byte(`{"choices":[{"message":{"content":"hello"}}],"data":{"text":"world"},"n":42}`), &v); err != nil {
		t.Fatal(err)
	}
	cases := []struct{ path, want string }{
		{"choices.0.message.content", "hello"},
		{"data.text", "world"},
		{"n", "42"},
		{"choices.5.message", ""},
		{"nope.deep.path", ""},
	}
	for _, c := range cases {
		if got := asString(jsonPath(v, c.path)); got != c.want {
			t.Errorf("path %q = %q, want %q", c.path, got, c.want)
		}
	}
}

func TestAsStringVariants(t *testing.T) {
	if got := asString([]interface{}{"a", "b", "c"}); got != "abc" {
		t.Errorf("array join = %q", got)
	}
	if got := asString(map[string]interface{}{"text": "x"}); got != "x" {
		t.Errorf("map text = %q", got)
	}
	if got := asString(nil); got != "" {
		t.Errorf("nil = %q", got)
	}
}

// ---------- 请求体模板渲染 ----------

func TestBuildRequestBodyTemplate(t *testing.T) {
	spec := &AdapterSpec{
		ID: "t1",
		Request: RequestSpec{
			Template: `{"m": ${model}, "p": ${prompt}, "s": ${system}, "h": ${history}, "st": ${stream}, "mt": ${max_tokens}}`,
		},
		Stream: StreamSpec{Mode: "sse-json"},
	}
	req := `{"model":"gpt-x","stream":true,"max_tokens":128,"messages":[
		{"role":"system","content":"be nice"},
		{"role":"user","content":"first"},
		{"role":"assistant","content":"ok"},
		{"role":"user","content":"second"}]}`
	body, model, stream, err := spec.BuildRequestBody([]byte(req), "")
	if err != nil {
		t.Fatal(err)
	}
	if model != "gpt-x" || !stream {
		t.Fatalf("model=%q stream=%v", model, stream)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("rendered body not valid JSON: %v\n%s", err, body)
	}
	if got["p"] != "second" {
		t.Errorf("prompt = %v, want last user message", got["p"])
	}
	if got["s"] != "be nice" {
		t.Errorf("system = %v", got["s"])
	}
	if got["st"] != true {
		t.Errorf("stream = %v", got["st"])
	}
	if got["mt"].(float64) != 128 {
		t.Errorf("max_tokens = %v", got["mt"])
	}
	// history 应排除最后一条 user 消息
	h := got["h"].([]interface{})
	if len(h) != 3 {
		t.Fatalf("history len = %d, want 3", len(h))
	}
}

// 关键安全用例：prompt 里含引号/换行/反斜杠时，渲染结果必须仍是合法 JSON。
func TestBuildRequestBodyEscaping(t *testing.T) {
	spec := &AdapterSpec{
		ID:      "t2",
		Request: RequestSpec{Template: `{"q": ${prompt}}`},
		Stream:  StreamSpec{Mode: "none"},
	}
	nasty := "he said \"hi\"\nand \\ then {\"injected\": true}"
	reqObj := map[string]interface{}{
		"model":    "m",
		"messages": []map[string]string{{"role": "user", "content": nasty}},
	}
	raw, _ := json.Marshal(reqObj)
	body, _, _, err := spec.BuildRequestBody(raw, "")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("escaping broke JSON: %v\n%s", err, body)
	}
	if got["q"] != nasty {
		t.Errorf("prompt mangled:\n got=%q\nwant=%q", got["q"], nasty)
	}
}

// 无模板时应原样透传 OpenAI 请求体（「仅路径不同」的提供者）。
func TestBuildRequestBodyPassthrough(t *testing.T) {
	spec := &AdapterSpec{ID: "t3", Stream: StreamSpec{Mode: "sse-json"}}
	raw := []byte(`{"model":"m","messages":[{"role":"user","content":"x"}]}`)
	body, _, _, err := spec.BuildRequestBody(raw, "")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, raw) {
		t.Errorf("passthrough changed body: %s", body)
	}
}

// mode=none 时，即使客户端要求流式，模板里的 ${stream} 也必须渲染成 false。
func TestBuildRequestBodyStreamForcedFalse(t *testing.T) {
	spec := &AdapterSpec{
		ID:      "t4",
		Request: RequestSpec{Template: `{"stream": ${stream}}`},
		Stream:  StreamSpec{Mode: "none"},
	}
	body, _, clientWants, err := spec.BuildRequestBody([]byte(`{"model":"m","stream":true,"messages":[]}`), "")
	if err != nil {
		t.Fatal(err)
	}
	if !clientWants {
		t.Error("客户端流式意图应如实返回 true")
	}
	if !strings.Contains(string(body), `"stream": false`) {
		t.Errorf("上游 stream 应被强制为 false，实际: %s", body)
	}
}

// ---------- 响应提取 ----------

func TestExtractContent(t *testing.T) {
	spec := &AdapterSpec{ID: "t5", Response: ResponseSpec{ContentPath: "data.text"}}
	got, err := spec.ExtractContent([]byte(`{"data":{"text":"hello world"}}`))
	if err != nil || got != "hello world" {
		t.Fatalf("got=%q err=%v", got, err)
	}

	plain := &AdapterSpec{ID: "t6", Response: ResponseSpec{PlainText: true}}
	got, _ = plain.ExtractContent([]byte("  just text  "))
	if got != "just text" {
		t.Errorf("plain = %q", got)
	}
}

func TestExtractContentError(t *testing.T) {
	spec := &AdapterSpec{ID: "t7", Response: ResponseSpec{ContentPath: "x", ErrorPath: "error.message"}}
	_, err := spec.ExtractContent([]byte(`{"error":{"message":"rate limited"}}`))
	if err == nil || !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("err = %v", err)
	}
}

// 上游无视 stream=false 仍回 SSE —— 引擎必须自动收敛成整段文本。
func TestExtractContentCollapsesSSE(t *testing.T) {
	spec := &AdapterSpec{
		ID:       "t8",
		Response: ResponseSpec{ContentPath: "message"},
		Stream:   StreamSpec{Mode: "sse-json", DeltaPath: "message"},
	}
	sse := "data: {\"message\":\"Hel\"}\n\ndata: {\"message\":\"lo!\"}\n\ndata: [DONE]\n\n"
	got, err := spec.ExtractContent([]byte(sse))
	if err != nil {
		t.Fatal(err)
	}
	if got != "Hello!" {
		t.Errorf("collapsed = %q, want %q", got, "Hello!")
	}
}

// ---------- 流式翻译 ----------

// 逐条读出 SSE 里的 content 增量，便于断言。
func collectDeltas(t *testing.T, r io.Reader) (string, int) {
	t.Helper()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	done := 0
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		d := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if d == "[DONE]" {
			done++
			continue
		}
		var v interface{}
		if json.Unmarshal([]byte(d), &v) != nil {
			continue
		}
		sb.WriteString(asString(jsonPath(v, "choices.0.delta.content")))
	}
	return sb.String(), done
}

func TestSSETranslatorDeltaMode(t *testing.T) {
	spec := &AdapterSpec{ID: "s1", Stream: StreamSpec{Mode: "sse-json", DeltaPath: "message"}}
	src := strings.NewReader("data: {\"message\":\"你好\"}\n\ndata: {\"message\":\"世界\"}\n\ndata: [DONE]\n\n")
	text, done := collectDeltas(t, NewAdapterSSETranslator(spec, src, "lbl"))
	if text != "你好世界" {
		t.Errorf("text = %q", text)
	}
	if done != 1 {
		t.Errorf("[DONE] count = %d, want 1", done)
	}
}

// 上游推「累计全文」时，引擎必须自动差分成增量，否则下游会看到文本重复。
func TestSSETranslatorFullDiffMode(t *testing.T) {
	spec := &AdapterSpec{ID: "s2", Stream: StreamSpec{Mode: "sse-json", FullPath: "data.text"}}
	src := strings.NewReader(
		"data: {\"data\":{\"text\":\"Hel\"}}\n\n" +
			"data: {\"data\":{\"text\":\"Hello\"}}\n\n" +
			"data: {\"data\":{\"text\":\"Hello world\"}}\n\n" +
			"data: [DONE]\n\n")
	text, _ := collectDeltas(t, NewAdapterSSETranslator(spec, src, "lbl"))
	if text != "Hello world" {
		t.Errorf("差分失败，text = %q, want %q", text, "Hello world")
	}
}

func TestSSETranslatorTextMode(t *testing.T) {
	spec := &AdapterSpec{ID: "s3", Stream: StreamSpec{Mode: "sse-text"}}
	src := strings.NewReader("data: abc\n\ndata: def\n\ndata: [DONE]\n\n")
	text, _ := collectDeltas(t, NewAdapterSSETranslator(spec, src, "lbl"))
	if text != "abcdef" {
		t.Errorf("text = %q", text)
	}
}

func TestSSETranslatorNDJSON(t *testing.T) {
	spec := &AdapterSpec{ID: "s4", Stream: StreamSpec{Mode: "ndjson", DeltaPath: "chunk"}}
	src := strings.NewReader("{\"chunk\":\"a\"}\n{\"chunk\":\"b\"}\n")
	text, done := collectDeltas(t, NewAdapterSSETranslator(spec, src, "lbl"))
	if text != "ab" {
		t.Errorf("text = %q", text)
	}
	// 流自然结束（无 [DONE] 标记）时，引擎也必须补一条 [DONE]，否则客户端会挂住。
	if done != 1 {
		t.Errorf("[DONE] count = %d, want 1", done)
	}
}

// 上游一个字都没吐时，也必须产出结构完整的空回复 + [DONE]。
func TestSSETranslatorEmptyStream(t *testing.T) {
	spec := &AdapterSpec{ID: "s5", Stream: StreamSpec{Mode: "sse-json", DeltaPath: "message"}}
	text, done := collectDeltas(t, NewAdapterSSETranslator(spec, strings.NewReader(""), "lbl"))
	if text != "" {
		t.Errorf("text = %q", text)
	}
	if done != 1 {
		t.Errorf("空流也必须补 [DONE]，实际 %d", done)
	}
}

// 上游偶发非 JSON 行（心跳/注释）不能中断整条流。
func TestSSETranslatorIgnoresGarbage(t *testing.T) {
	spec := &AdapterSpec{ID: "s6", Stream: StreamSpec{Mode: "sse-json", DeltaPath: "message", SkipPrefix: ":"}}
	src := strings.NewReader(": heartbeat\n\ndata: not-json\n\ndata: {\"message\":\"ok\"}\n\ndata: [DONE]\n\n")
	text, _ := collectDeltas(t, NewAdapterSSETranslator(spec, src, "lbl"))
	if text != "ok" {
		t.Errorf("text = %q", text)
	}
}

func TestOneShotStream(t *testing.T) {
	spec := &AdapterSpec{ID: "s7", Response: ResponseSpec{ContentPath: "response"}}
	r := NewAdapterOneShotStream(spec, []byte(`{"response":"整包回复"}`), "lbl")
	text, done := collectDeltas(t, r)
	if text != "整包回复" {
		t.Errorf("text = %q", text)
	}
	if done != 1 {
		t.Errorf("[DONE] = %d", done)
	}
}

// ---------- 预检握手 ----------

func TestEnsureTokenFromHeader(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/status" {
			hits++
			if r.Header.Get("x-vqd-accept") != "1" {
				t.Errorf("握手请求头未带上：%v", r.Header)
			}
			w.Header().Set("x-vqd-4", "TOKEN-123")
			w.WriteHeader(200)
			return
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()

	spec := &AdapterSpec{
		ID: "pf1",
		Preflight: &PreflightSpec{
			Method: "GET", URL: "/status",
			Headers:     map[string]string{"x-vqd-accept": "1"},
			TokenHeader: "x-vqd-4", TTLSeconds: 300,
		},
	}
	tok, err := spec.EnsureToken(srv.URL, srv.Client())
	if err != nil || tok != "TOKEN-123" {
		t.Fatalf("token=%q err=%v", tok, err)
	}
	// 第二次应命中缓存，不再打上游
	if tok2, _ := spec.EnsureToken(srv.URL, srv.Client()); tok2 != "TOKEN-123" || hits != 1 {
		t.Errorf("缓存未生效：hits=%d", hits)
	}
	// 作废后应重新握手
	InvalidateToken(srv.URL, spec.ID)
	if _, err := spec.EnsureToken(srv.URL, srv.Client()); err != nil || hits != 2 {
		t.Errorf("作废后未重新握手：hits=%d err=%v", hits, err)
	}
}

func TestEnsureTokenFromJSONPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"token":"JT-9"}}`))
	}))
	defer srv.Close()
	spec := &AdapterSpec{ID: "pf2", Preflight: &PreflightSpec{URL: "/t", TokenPath: "data.token"}}
	tok, err := spec.EnsureToken(srv.URL, srv.Client())
	if err != nil || tok != "JT-9" {
		t.Fatalf("token=%q err=%v", tok, err)
	}
}

func TestEnsureTokenMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()
	spec := &AdapterSpec{ID: "pf3", Preflight: &PreflightSpec{URL: "/t", TokenHeader: "x-none"}}
	if _, err := spec.EnsureToken(srv.URL, srv.Client()); err == nil {
		t.Fatal("拿不到令牌必须报错，不能静默放行")
	}
}

// ---------- URL / 头部 ----------

func TestChatURLAndHeaders(t *testing.T) {
	spec := &AdapterSpec{
		ID: "u1", ChatPath: "/chat",
		Query:   map[string]string{"tk": "${token}"},
		Headers: map[string]string{"x-vqd-4": "${token}", "X-Fixed": "v"},
	}
	if got := spec.ChatURL("https://x.com/api/", "T1"); got != "https://x.com/api/chat?tk=T1" {
		t.Errorf("url = %q", got)
	}
	h := http.Header{}
	spec.ApplyHeaders(h, "T1")
	if h.Get("x-vqd-4") != "T1" || h.Get("X-Fixed") != "v" {
		t.Errorf("headers = %v", h)
	}
	if spec.ModelsURL("https://x.com") != "" {
		t.Error("未声明 models_path 时应返回空串")
	}
}

// ---------- 注册表 ----------

func TestLookupAdapterSkipsNativeFormats(t *testing.T) {
	for _, f := range []string{"", "openai", "OpenAI", "gemini", "claude", "openai-responses"} {
		if LookupAdapter(f) != nil {
			t.Errorf("format %q 不应命中适配器（必须走原生分支）", f)
		}
	}
}

func TestBuiltinAdaptersRegistered(t *testing.T) {
	for _, id := range []string{AdapterDuckDuckGo, AdapterTheOldLLM, AdapterFelo, AdapterMimocode, AdapterTextPlain} {
		spec := LookupAdapter(id)
		if spec == nil {
			t.Fatalf("内置适配器 %s 未注册", id)
		}
		if spec.Label == "" {
			t.Errorf("%s 缺少 Label", id)
		}
		// 每个内置适配器的模板都必须能渲染出合法 JSON
		raw := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
		if _, _, _, err := spec.BuildRequestBody(raw, "tok"); err != nil {
			t.Errorf("%s 模板渲染失败: %v", id, err)
		}
	}
}

func TestLoadAdaptersFileMissingIsNotError(t *testing.T) {
	n, err := LoadAdaptersFile("Z:/definitely/not/here/adapters.json")
	if err != nil || n != 0 {
		t.Fatalf("缺文件应静默返回 0：n=%d err=%v", n, err)
	}
}

func TestLoadAdaptersFileOverride(t *testing.T) {
	dir := t.TempDir()
	p := dir + "/adapters.json"
	content := `{"adapters":[{"id":"my-custom","label":"自定义","chat_path":"/x",
		"request":{"template":"{\"q\": ${prompt}}"},
		"response":{"content_path":"out"},"stream":{"mode":"none"}}]}`
	if err := writeFile(p, content); err != nil {
		t.Fatal(err)
	}
	n, err := LoadAdaptersFile(p)
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	spec := LookupAdapter("my-custom")
	if spec == nil || spec.ChatPath != "/x" {
		t.Fatalf("自定义适配器未生效: %+v", spec)
	}
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}

// ---------- 端到端：模拟 DuckDuckGo 全链路 ----------

func TestEndToEndDuckDuckGoLikeFlow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/status":
			w.Header().Set("x-vqd-4", "VQD-TOKEN")
			w.WriteHeader(200)
		case "/chat":
			if r.Header.Get("x-vqd-4") != "VQD-TOKEN" {
				w.WriteHeader(401)
				return
			}
			var got map[string]interface{}
			body, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(body, &got); err != nil {
				t.Errorf("上游收到非法 JSON: %s", body)
			}
			if got["model"] != "gpt-5.4-mini" {
				t.Errorf("model 未透传: %v", got["model"])
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"message\":\"你\"}\n\ndata: {\"message\":\"好\"}\n\ndata: [DONE]\n\n"))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	spec := LookupAdapter(AdapterDuckDuckGo)
	if spec == nil {
		t.Fatal("duckduckgo 适配器未注册")
	}
	InvalidateToken(srv.URL, spec.ID)

	token, err := spec.EnsureToken(srv.URL, srv.Client())
	if err != nil {
		t.Fatalf("握手失败: %v", err)
	}
	openAIReq := []byte(`{"model":"gpt-5.4-mini","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	body, _, _, err := spec.BuildRequestBody(openAIReq, token)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(spec.HTTPMethod(), spec.ChatURL(srv.URL, token), bytes.NewReader(body))
	spec.ApplyHeaders(req.Header, token)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("上游状态 %d", resp.StatusCode)
	}
	text, done := collectDeltas(t, NewAdapterSSETranslator(spec, resp.Body, "DDG · gpt-5.4-mini"))
	if text != "你好" {
		t.Errorf("端到端文本 = %q, want 你好", text)
	}
	if done != 1 {
		t.Errorf("[DONE] = %d", done)
	}
}

// ---------- 端到端：模拟 theoldllm（OpenAI 兼容 + 算法 token） ----------

func TestEndToEndTheOldLLM(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chatgpt" {
			w.WriteHeader(404)
			return
		}
		// 校验免 Key 头已被算法注入
		if r.Header.Get("X-Request-Token") == "" {
			t.Errorf("缺少算法生成的 X-Request-Token 头")
		}
		if r.Header.Get("X-Client-Version") != "3.8.4" {
			t.Errorf("X-Client-Version 应为 3.8.4，实际 %q", r.Header.Get("X-Client-Version"))
		}
		var got map[string]interface{}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		// 上游接受 OpenAI 格式，model 原样透传
		if got["model"] != "GPT_5" {
			t.Errorf("model 应为 GPT_5，实际 %v", got["model"])
		}
		w.Header().Set("Content-Type", "application/json")
		// 上游为 OpenAI 兼容：非流式返回 choices.0.message.content
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"这是整包回复"}}]}`))
	}))
	defer srv.Close()

	spec := LookupAdapter(AdapterTheOldLLM)
	openAIReq := []byte(`{"model":"GPT_5","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	token, err := spec.EnsureToken(srv.URL, nil) // 走 TokenGen 算法生成
	if err != nil {
		t.Fatal(err)
	}
	if token == "" {
		t.Fatal("算法 token 不应为空")
	}
	body, _, clientStream, err := spec.BuildRequestBody(openAIReq, token)
	if err != nil {
		t.Fatal(err)
	}
	if !clientStream {
		t.Error("应如实报告客户端要了流式")
	}
	req, _ := http.NewRequest(spec.HTTPMethod(), spec.ChatURL(srv.URL, token), bytes.NewReader(body))
	spec.ApplyHeaders(req.Header, token)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	// 非流式响应 → OpenAI chat.completion
	oa, err := spec.ToOpenAIResponse(raw, "TheOldLLM · GPT_5")
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]interface{}
	_ = json.Unmarshal(oa, &parsed)
	if asString(jsonPath(parsed, "choices.0.message.content")) != "这是整包回复" {
		t.Errorf("非流式翻译失败: %s", oa)
	}
	// 同一整包 → 单块 SSE
	text, done := collectDeltas(t, NewAdapterOneShotStream(spec, raw, "TheOldLLM · GPT_5"))
	if text != "这是整包回复" || done != 1 {
		t.Errorf("单块 SSE 失败 text=%q done=%d", text, done)
	}
}

// TestTheOldLLMTokenGen 校验 djb2 算法令牌生成器的结构与哈希对齐。
func TestTheOldLLMTokenGen(t *testing.T) {
	// djb2 ×31 变体（与 OmniRoute executor 的 (t<<5)-t+s 一致）已知向量校验。
	if got := djb2Int32("test"); got != 3556498 {
		t.Errorf("djb2Int32(\"test\") = %d, want 3556498", got)
	}
	spec := LookupAdapter(AdapterTheOldLLM)
	if spec == nil || spec.TokenGen != "theoldllm" {
		t.Fatal("theoldllm adapter 未注册或 TokenGen 不正确")
	}
	tok := theOldLLMToken()
	parts := strings.Split(tok, "-")
	if len(parts) != 3 {
		t.Fatalf("token 格式应为 ts-hash-rand，实际 %q", tok)
	}
	if _, err := strconv.ParseUint(parts[0], 36, 64); err != nil {
		t.Errorf("ts 段不是 base36: %q", parts[0])
	}
	if _, err := strconv.ParseUint(parts[1], 36, 64); err != nil {
		t.Errorf("hash 段不是 base36: %q", parts[1])
	}
	if len(parts[2]) != 8 {
		t.Errorf("随机段应为 8 位 hex，实际 %q", parts[2])
	}
}

// ---------- injectSystemMarker 幂等注入 ----------

func TestInjectSystemMarker(t *testing.T) {
	marker := "You are MiMoCode, an interactive CLI tool that helps users with software engineering tasks."

	// 无 messages 字段时应原样返回
	got, err := injectSystemMarker([]byte(`{"model":"x"}`), marker)
	if err != nil || string(got) != `{"model":"x"}` {
		t.Fatalf("无 messages 应跳过: %s", got)
	}

	// messages 不是数组时应原样返回
	got, err = injectSystemMarker([]byte(`{"messages":"bad"}`), marker)
	if err != nil || string(got) != `{"messages":"bad"}` {
		t.Fatalf("非数组 messages 应跳过: %s", got)
	}

	// 正常注入：messages 头部插入 system 消息
	orig := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	got, err = injectSystemMarker(orig, marker)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(got, &parsed); err != nil {
		t.Fatalf("注入后 JSON 非法: %v\n%s", err, got)
	}
	arr := parsed["messages"].([]interface{})
	if len(arr) != 2 {
		t.Fatalf("messages 长度 = %d, want 2", len(arr))
	}
	m0 := arr[0].(map[string]interface{})
	if m0["role"] != "system" || m0["content"] != marker {
		t.Fatalf("头部 system 消息错误: %v", m0)
	}

	// 幂等：已含相同 content 的 system 消息则跳过
	got2, err := injectSystemMarker(got, marker)
	if err != nil {
		t.Fatal(err)
	}
	if string(got2) != string(got) {
		t.Errorf("幂等失败: 二次注入结果不同")
	}

	// 幂等：已有不同 content 的 system 消息仍追加头部（不跳过）
	diff := []byte(`{"model":"m","messages":[{"role":"system","content":"other"},{"role":"user","content":"hi"}]}`)
	got3, err := injectSystemMarker(diff, marker)
	if err != nil {
		t.Fatal(err)
	}
	var parsed3 map[string]interface{}
	if err := json.Unmarshal(got3, &parsed3); err != nil {
		t.Fatalf("幂等(不同 content)后 JSON 非法: %v\n%s", err, got3)
	}
	arr3 := parsed3["messages"].([]interface{})
	if arr3[0].(map[string]interface{})["content"] != marker {
		t.Errorf("不同 content 时应在头部注入 marker，实际: %v", arr3[0])
	}
}

// ---------- EnsureToken preflight body renderRaw ----------

func TestEnsureTokenRendersPreflightBody(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/free-ai/bootstrap" {
			b, _ := io.ReadAll(r.Body)
			gotBody = string(b)
			// 返回含 JWT 的 JSON
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jwt":"test-jwt-123"}`))
			return
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()

	spec := &AdapterSpec{
		ID: "pf-mimocode",
		Preflight: &PreflightSpec{
			Method:     "POST",
			URL:        "/api/free-ai/bootstrap",
			Body:       `{"client":"${uuid}"}`,
			TokenPath:  "jwt",
			TTLSeconds: 60,
		},
	}
	tok, err := spec.EnsureToken(srv.URL, srv.Client())
	if err != nil {
		t.Fatalf("EnsureToken failed: %v", err)
	}
	if tok != "test-jwt-123" {
		t.Errorf("token = %q, want test-jwt-123", tok)
	}
	// ${uuid} 必须被替换为一个 32 位十六进制字符串，而非字面 "${uuid}"
	if gotBody == `${"client":"${uuid}"}` || strings.Contains(gotBody, "${uuid}") {
		t.Errorf("preflight body 未 renderRaw: %s", gotBody)
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(gotBody), &parsed); err != nil {
		t.Fatalf("preflight body 不是合法 JSON: %v\n%s", err, gotBody)
	}
	client := parsed["client"].(string)
	if len(client) != 32 {
		t.Errorf("client 指纹长度 = %d, want 32 hex chars", len(client))
	}
}

// ---------- 端到端：模拟 Mimocode（JWT Preflight + 反 abuse system 注入 + SSE） ----------

func TestEndToEndMimocode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/free-ai/bootstrap":
			// 校验请求头
			if r.Header.Get("X-Mimo-Source") != "mimocode-cli-free" {
				t.Errorf("bootstrap 缺少 X-Mimo-Source: %v", r.Header)
			}
			if ct := r.Header.Get("Content-Type"); ct != "application/json" {
				t.Errorf("bootstrap Content-Type = %q", ct)
			}
			b, _ := io.ReadAll(r.Body)
			var pf map[string]interface{}
			if err := json.Unmarshal(b, &pf); err != nil {
				t.Errorf("bootstrap body 非法 JSON: %v", err)
			}
			client, _ := pf["client"].(string)
			if len(client) != 32 {
				t.Errorf("bootstrap client 指纹长度 = %d", len(client))
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jwt":"mimo-jwt-abc"}`))

		case "/api/free-ai/openai/chat":
			// 校验 Bearer 头
			auth := r.Header.Get("Authorization")
			if auth != "Bearer mimo-jwt-abc" {
				t.Errorf("Authorization = %q, want Bearer mimo-jwt-abc", auth)
			}
			if r.Header.Get("X-Mimo-Source") != "mimocode-cli-free" {
				t.Errorf("chat 缺少 X-Mimo-Source: %v", r.Header)
			}
			// 校验请求体含反 abuse system 标记
			b, _ := io.ReadAll(r.Body)
			var chatReq map[string]interface{}
			if err := json.Unmarshal(b, &chatReq); err != nil {
				t.Fatalf("chat body 非法 JSON: %v\n%s", err, b)
			}
			msgs := chatReq["messages"].([]interface{})
			if len(msgs) < 1 {
				t.Fatal("messages 数组为空，应已注入 system 标记")
			}
			m0 := msgs[0].(map[string]interface{})
			if m0["role"] != "system" {
				t.Errorf("第一条消息 role = %q, want system", m0["role"])
			}
			marker := "You are MiMoCode, an interactive CLI tool that helps users with software engineering tasks."
			if m0["content"] != marker {
				t.Errorf("system marker 内容不匹配: %q", m0["content"])
			}
			// model 透传
			if chatReq["model"] != "mimo-auto" {
				t.Errorf("model = %v, want mimo-auto", chatReq["model"])
			}
			// 返回 SSE 流
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(
				"data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n" +
					"data: {\"choices\":[{\"delta\":{\"content\":\"lo!\"}}]}\n\n" +
					"data: [DONE]\n\n",
			))

		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	spec := LookupAdapter(AdapterMimocode)
	if spec == nil {
		t.Fatal("mimocode 适配器未注册")
	}
	InvalidateToken(srv.URL, spec.ID)

	// 1) EnsureToken：bootstrap → JWT
	token, err := spec.EnsureToken(srv.URL, srv.Client())
	if err != nil {
		t.Fatalf("bootstrap failed: %v", err)
	}
	if token != "mimo-jwt-abc" {
		t.Errorf("token = %q, want mimo-jwt-abc", token)
	}

	// 2) BuildRequestBody：透传 + 注入 system 标记
	openAIReq := []byte(`{"model":"mimo-auto","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	body, _, clientStream, err := spec.BuildRequestBody(openAIReq, token)
	if err != nil {
		t.Fatal(err)
	}
	if !clientStream {
		t.Error("客户端要求流式，应如实返回 true")
	}
	var bodyMap map[string]interface{}
	if err := json.Unmarshal(body, &bodyMap); err != nil {
		t.Fatalf("body 非法 JSON: %v\n%s", err, body)
	}
	msgs := bodyMap["messages"].([]interface{})
	if msgs[0].(map[string]interface{})["role"] != "system" {
		t.Error("BuildRequestBody 未注入 system 标记")
	}

	// 3) 发请求并校验响应头 + SSE 翻译
	req, _ := http.NewRequest(spec.HTTPMethod(), spec.ChatURL(srv.URL, token), bytes.NewReader(body))
	spec.ApplyHeaders(req.Header, token)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("chat 状态 %d", resp.StatusCode)
	}

	text, done := collectDeltas(t, resp.Body)
	if text != "Hello!" {
		t.Errorf("SSE 文本 = %q, want Hello!", text)
	}
	if done != 1 {
		t.Errorf("[DONE] = %d, want 1", done)
	}
}
