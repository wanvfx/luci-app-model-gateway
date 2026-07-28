package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 回归测试：模拟“重载配置”场景——UCI 落盘的是 enc: 前缀密文，
// 重载到内存后必须调用 DecryptAPIKeys 还原为明文，否则巡检/代理会带密文密钥请求上游 → 全 401。
// 这是 r20260727d 之前“保存或一键配置后所有模型变红”的根因修复（main.go doReloadConfig 现已解密）。
func TestReloadDecryptsAPIKeys(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "model-gateway")
	// 模拟落盘（加密）配置：api_key 为 enc: 密文，明文应为 "secret-key-123"
	content := `config model-gateway 'settings'
	option enable '1'

config provider 'nvidia'
	option name 'nvidia'
	option base_url 'https://integrate.api.nvidia.com/v1'
	option api_key 'enc:deadbeef'
	option enabled '1'
	list models 'nvidia/nemotron-3-super-120b-a12b'
`
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	loaded, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	// 未解密前：密钥仍是密文
	if !strings.HasPrefix(loaded.Providers[0].APIKey, "enc:") {
		t.Fatalf("precondition: expected enc: prefix before decrypt, got %q", loaded.Providers[0].APIKey)
	}

	// 模拟 doReloadConfig 中调用解密
	loaded.DecryptAPIKeys(func(s string) string {
		return strings.TrimPrefix(s, "enc:") + "-plain"
	})
	if got := loaded.Providers[0].APIKey; got != "deadbeef-plain" {
		t.Fatalf("after reload decrypt, expected plaintext key, got %q", got)
	}

	// 幂等：再次解密（明文不再带 enc: 前缀）应保持原样，不能二次改写
	loaded.DecryptAPIKeys(func(s string) string {
		return "MUST-NOT-APPLY"
	})
	if got := loaded.Providers[0].APIKey; got != "deadbeef-plain" {
		t.Fatalf("DecryptAPIKeys must be idempotent on plaintext keys, got %q", got)
	}
}
