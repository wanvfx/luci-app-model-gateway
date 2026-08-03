package config

import (
	"os"
	"path/filepath"
	"testing"
)

// 安全套件（v1.6.0）配置解析与运行时助手回归测试：
//   - ssrf_strict 选项解析
//   - allow_clients 列表解析 + ClientAllowed 白名单匹配（含 CIDR/裸 IP/空=放行）
//   - banned_providers 列表解析 + Banned 打标 + UsableProviders 排除
func TestSecuritySettingsLoad(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "model-gateway")
	content := `config model-gateway 'settings'
	option ssrf_strict '1'
	list allow_clients '192.168.1.0/24'
	list allow_clients '10.0.0.5'
	list banned_providers 'evil'

config provider 'evil'
	option name 'evil'
	option base_url 'https://evil.example.com/v1'
	option enabled '1'
	list models 'evil/model'

config provider 'good'
	option name 'good'
	option base_url 'https://api.good.com/v1'
	option enabled '1'
	list models 'good/model'

config provider 'disabledp'
	option name 'disabledp'
	option base_url 'https://api.disabled.com/v1'
	option enabled '0'
	list models 'disabledp/model'
`
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	// SSRF strict
	if !cfg.SSRFStrict() {
		t.Fatal("expected ssrf_strict=true")
	}

	// allow_clients 解析 + 匹配
	ac := cfg.AllowClients()
	if len(ac) != 2 {
		t.Fatalf("expected 2 allow_clients, got %d: %v", len(ac), ac)
	}
	if !cfg.ClientAllowed("192.168.1.50") {
		t.Fatal("192.168.1.50 should be allowed (inside /24)")
	}
	if !cfg.ClientAllowed("10.0.0.5") {
		t.Fatal("10.0.0.5 should be allowed (exact)")
	}
	if cfg.ClientAllowed("8.8.8.8") {
		t.Fatal("8.8.8.8 should be denied")
	}
	// 白名单为空时放行全部（向后兼容默认）
	cfg.allowNets = nil
	if !cfg.ClientAllowed("8.8.8.8") {
		t.Fatal("with empty allowlist, all clients must be allowed")
	}

	// banned providers 打标
	if len(cfg.Providers) != 3 {
		t.Fatalf("expected 3 providers parsed, got %d", len(cfg.Providers))
	}
	var evil *Provider
	for _, p := range cfg.Providers {
		if p.Name == "evil" {
			evil = p
		}
	}
	if evil == nil || !evil.Banned {
		t.Fatal("evil provider should be marked Banned")
	}

	// UsableProviders 排除 banned 与 disabled
	usable := cfg.UsableProviders()
	if len(usable) != 1 || usable[0].Name != "good" {
		names := []string{}
		for _, p := range usable {
			names = append(names, p.Name)
		}
		t.Fatalf("expected only 'good' usable, got %v", names)
	}
}
