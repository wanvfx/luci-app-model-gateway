package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/wanvfx/luci-app-model-gateway/config"
)

// TestHealKeyfreeAdapterFormats 验证“保留配置升级”自愈：
// 旧免 Key 实例 format 为空时，能从目录按 id 取回非 openai 的 format。
func TestHealKeyfreeAdapterFormats(t *testing.T) {
	dir := t.TempDir()
	catalog := `{
  "providers": [
    {"id": "duckduckgo-web", "format": "duckduckgo", "auth": "none"},
    {"id": "felo-web", "format": "felo", "auth": "none"},
    {"id": "theoldllm", "format": "theoldllm", "auth": "none"},
    {"id": "pollinations", "format": "", "auth": "optional"},
    {"id": "NVIDIA", "format": "", "auth": "apikey"}
  ]
}`
	catalogPath := filepath.Join(dir, "providers_catalog.json")
	if err := os.WriteFile(catalogPath, []byte(catalog), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Providers: []*config.Provider{
			{Name: "duckduckgo-web", NoAuth: true},                            // 应被补全为 duckduckgo
			{Name: "felo-web", NoAuth: true},                                  // 应被补全为 felo
			{Name: "theoldllm", NoAuth: true},                                 // 应被补全为 theoldllm
			{Name: "pollinations", NoAuth: true},                              // 目录 format 为空 → 不补全
			{Name: "NVIDIA", NoAuth: false},                                   // 非免 Key → 跳过
			{Name: "duckduckgo-web-explicit", NoAuth: true, Format: "openai"}, // 已显式设置 → 不覆盖
		},
	}

	healKeyfreeAdapterFormats(cfg, dir)

	byName := map[string]*config.Provider{}
	for _, p := range cfg.Providers {
		byName[p.Name] = p
	}
	// 取第一个 duckduckgo-web（补全的那个，format 应为 duckduckgo）
	if got := byName["duckduckgo-web"].Format; got != "duckduckgo" {
		t.Errorf("duckduckgo-web format = %q, want duckduckgo", got)
	}
	if got := byName["felo-web"].Format; got != "felo" {
		t.Errorf("felo-web format = %q, want felo", got)
	}
	if got := byName["theoldllm"].Format; got != "theoldllm" {
		t.Errorf("theoldllm format = %q, want theoldllm", got)
	}
	if got := byName["pollinations"].Format; got != "" {
		t.Errorf("pollinations format = %q, want empty (catalog format empty)", got)
	}
	// 已显式为 openai 的实例不应被覆盖
	if got := byName["duckduckgo-web-explicit"].Format; got != "openai" {
		t.Errorf("duckduckgo-web-explicit format = %q, want openai (preserved)", got)
	}
}

// TestHealKeyfreeAdapterFormatsNoCatalog 验证目录缺失时静默跳过、不 panic。
func TestHealKeyfreeAdapterFormatsNoCatalog(t *testing.T) {
	cfg := &config.Config{
		Providers: []*config.Provider{
			{Name: "duckduckgo-web", NoAuth: true},
		},
	}
	// 传入一个不存在的目录，函数应静默返回，不修改、不 panic。
	healKeyfreeAdapterFormats(cfg, filepath.Join(t.TempDir(), "nope"))
	if cfg.Providers[0].Format != "" {
		t.Errorf("expected no change, got format=%q", cfg.Providers[0].Format)
	}
}
