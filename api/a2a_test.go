package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wanvfx/luci-app-model-gateway/config"
	"github.com/wanvfx/luci-app-model-gateway/engine"
)

// newTestA2AHandler 构造最小可用的 AdminHandler 用于单测（仅填充 a2a 需要的字段）。
func newTestA2AHandler() *AdminHandler {
	h := &AdminHandler{}
	cfg := &config.Config{
		Providers: []*config.Provider{
			{Name: "openai", Models: []string{"gpt-4o", "gpt-4o-vision"}, Enabled: true},
			{Name: "local", Models: []string{"llama-3"}, Enabled: false},
		},
	}
	cfg.SetAdminKey("test-admin-key")
	h.cfg.Store(cfg)
	h.cat = engine.LoadCatalog("", "") // 空参考库：LookupOrDefault 仍返回兜底能力
	h.circuits = engine.NewCircuitPool(3, time.Minute)
	return h
}

// postA2A 发送一个 JSON-RPC 请求并返回解码后的响应信封。
func postA2A(t *testing.T, h *AdminHandler, method string) map[string]interface{} {
	t.Helper()
	reqBody, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  method,
	})
	req := httptest.NewRequest(http.MethodPost, "/a2a", bytes.NewReader(reqBody))
	// S12：A2A 鉴权已启用，测试需携带 Bearer admin_key
	req.Header.Set("Authorization", "Bearer test-admin-key")
	rec := httptest.NewRecorder()
	h.HandleA2A(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("method %q: expected 200, got %d (body=%s)", method, rec.Code, rec.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("method %q: invalid json: %v", method, err)
	}
	return resp
}

func TestA2ADiscovery(t *testing.T) {
	resp := postA2A(t, newTestA2AHandler(), "discovery")
	if resp["jsonrpc"] != "2.0" || resp["id"].(float64) != 1 {
		t.Fatalf("envelope wrong: %v", resp)
	}
	result := resp["result"].(map[string]interface{})
	models := result["models"].([]interface{})
	if len(models) != 3 {
		t.Fatalf("expected 3 models, got %d", len(models))
	}
	// 校验字段：id / provider / capabilities
	for _, m := range models {
		mm := m.(map[string]interface{})
		if mm["id"] == nil || mm["provider"] == nil {
			t.Fatalf("missing id/provider: %v", mm)
		}
		caps, ok := mm["capabilities"].([]interface{})
		if !ok || len(caps) == 0 {
			t.Fatalf("expected non-empty capabilities: %v", mm)
		}
	}
}

func TestA2AHealth(t *testing.T) {
	resp := postA2A(t, newTestA2AHandler(), "health")
	result := resp["result"].(map[string]interface{})
	providers := result["providers"].([]interface{})
	if len(providers) != 2 {
		t.Fatalf("expected 2 providers, got %d", len(providers))
	}
	for _, p := range providers {
		pp := p.(map[string]interface{})
		if pp["name"] == nil || pp["status"] == nil {
			t.Fatalf("missing name/status: %v", pp)
		}
	}
}

func TestA2ACost(t *testing.T) {
	resp := postA2A(t, newTestA2AHandler(), "cost")
	result := resp["result"].(map[string]interface{})
	// 空参考库：无定价条目，count 应为 0（验证"若数据存在"分支）
	if result["count"].(float64) != 0 {
		t.Fatalf("expected 0 cost rows with empty catalog, got %v", result["count"])
	}
}

func TestA2AUnknownMethod(t *testing.T) {
	h := newTestA2AHandler()
	reqBody, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      7,
		"method":  "nope",
	})
	req := httptest.NewRequest(http.MethodPost, "/a2a", bytes.NewReader(reqBody))
	// S12：A2A 鉴权已启用
	req.Header.Set("Authorization", "Bearer test-admin-key")
	rec := httptest.NewRecorder()
	h.HandleA2A(rec, req)

	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if resp["error"] == nil {
		t.Fatalf("expected error for unknown method: %v", resp)
	}
	errObj := resp["error"].(map[string]interface{})
	if int(errObj["code"].(float64)) != -32601 {
		t.Fatalf("expected code -32601, got %v", errObj["code"])
	}
	if errObj["message"] != "method not found" {
		t.Fatalf("expected 'method not found', got %v", errObj["message"])
	}
	if resp["id"].(float64) != 7 {
		t.Fatalf("id not echoed: %v", resp["id"])
	}
}

func TestA2AMethodNotPost(t *testing.T) {
	h := newTestA2AHandler()
	req := httptest.NewRequest(http.MethodGet, "/a2a", nil)
	rec := httptest.NewRecorder()
	h.HandleA2A(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rec.Code)
	}
}
