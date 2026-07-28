package engine

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wanvfx/luci-app-model-gateway/config"
)

// TestHealthCheckModelsEndpoint 锁定健康探活为「轻量 GET /models」（与原 Python 版一致）：
// 不再真实生成 chat/completions，避免冷启动超时/限流/内容过滤误报。
func TestHealthCheckModelsEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer good" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := &http.Client{Timeout: 5 * time.Second}

	// 正常：/models 返回 200 + 正确 Key
	ok, detail, _, _ := HealthCheck(&config.Provider{BaseURL: srv.URL, APIKey: "good"}, client)
	if !ok {
		t.Fatalf("expected ok, got fail: %s", detail)
	}

	// 鉴权失败：错误 Key
	ok, _, _, _ = HealthCheck(&config.Provider{BaseURL: srv.URL, APIKey: "bad"}, client)
	if ok {
		t.Fatalf("expected auth fail for bad key")
	}

	// 空 Key
	ok, _, _, _ = HealthCheck(&config.Provider{BaseURL: srv.URL, APIKey: ""}, client)
	if ok {
		t.Fatalf("expected fail for empty key")
	}

	// 空 BaseURL
	ok, _, _, _ = HealthCheck(&config.Provider{BaseURL: "", APIKey: "good"}, client)
	if ok {
		t.Fatalf("expected fail for empty base_url")
	}
}
