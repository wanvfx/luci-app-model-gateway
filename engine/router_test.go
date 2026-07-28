package engine

import (
	"testing"

	"github.com/wanvfx/luci-app-model-gateway/config"
)

// TestPickCandidatesPrefixedModel 验证：客户端用 /v1/models 返回的前缀名
// p.Name-m 发起聊天时，路由能还原成真实模型名并匹配到正确的 provider。
// 这是修复「列表看得到、调用报 model not supported」的核心回归测试。
func TestPickCandidatesPrefixedModel(t *testing.T) {
	providers := []*config.Provider{
		{Name: "NVIDIA", BaseURL: "https://x/v1", APIKey: "k", Enabled: true, Models: []string{"meta/llama-3.1-8b"}},
		{Name: "MyPlatform", BaseURL: "https://y/v1", APIKey: "k", Enabled: true, Models: []string{"gpt-4o", "claude-3"}},
	}
	r := NewRouter(nil)

	cases := []struct {
		req      string
		wantName string
		wantProv string
	}{
		{"MyPlatform-gpt-4o", "gpt-4o", "MyPlatform"}, // 前缀名
		{"gpt-4o", "gpt-4o", "MyPlatform"},            // 真实名
		{"NVIDIA-meta/llama-3.1-8b", "meta/llama-3.1-8b", "NVIDIA"},
		{"claude-3", "claude-3", "MyPlatform"},
	}

	for _, c := range cases {
		cands := r.PickCandidates(c.req, providers, nil, PickOptions{})
		if len(cands) != 1 {
			t.Fatalf("req=%q 期望 1 个候选，实际 %d", c.req, len(cands))
		}
		if cands[0].Model != c.wantName {
			t.Errorf("req=%q 期望模型名 %q，实际 %q", c.req, c.wantName, cands[0].Model)
		}
		if cands[0].Provider == nil || cands[0].Provider.Name != c.wantProv {
			t.Errorf("req=%q 期望 provider %q，实际 %v", c.req, c.wantProv, cands[0].Provider)
		}
	}

	// 未知模型必须返回空（不再 fallback 到错误 provider 转发错名）
	bad := r.PickCandidates("NotExist-model", providers, nil, PickOptions{})
	if len(bad) != 0 {
		t.Errorf("未知模型应返回 0 候选，实际 %d", len(bad))
	}
}

// TestPickCandidatesDisabledFilter 验证：传入包含 per-provider 禁用模型名的
// disabled 集合时，PickCandidates 会过滤掉该模型（对齐 Python get_enabled_models
// 排除 disabled_models）。这是修复「per-provider disabled_models 在代理层静默失效」的回归测试。
func TestPickCandidatesDisabledFilter(t *testing.T) {
	providers := []*config.Provider{
		{Name: "NVIDIA", BaseURL: "https://x/v1", APIKey: "k", Enabled: true, Models: []string{"meta/llama-3.1-8b"}},
		{Name: "MyPlatform", BaseURL: "https://y/v1", APIKey: "k", Enabled: true, Models: []string{"gpt-4o", "claude-3"}},
	}
	r := NewRouter(nil)

	// 禁用 MyPlatform 的 gpt-4o（真实模型名，与 per-provider disabled_models 存储一致）
	disabled := map[string]bool{"gpt-4o": true}

	// 前缀名与真实名都应被过滤
	for _, req := range []string{"MyPlatform-gpt-4o", "gpt-4o"} {
		cands := r.PickCandidates(req, providers, disabled, PickOptions{})
		for _, c := range cands {
			if c.Model == "gpt-4o" {
				t.Errorf("req=%q 不应返回被禁用的模型 gpt-4o", req)
			}
		}
	}

	// 未被禁用的模型仍可正常选中
	ok := r.PickCandidates("claude-3", providers, disabled, PickOptions{})
	if len(ok) != 1 || ok[0].Model != "claude-3" {
		t.Errorf("未被禁用的 claude-3 应可正常选中，实际 %+v", ok)
	}
}
