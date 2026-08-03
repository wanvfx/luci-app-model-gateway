package engine

import (
	"testing"

	"github.com/wanvfx/luci-app-model-gateway/config"
)

// TestBannedProviderExcludedFromCandidates 验证：当候选 provider 列表已排除被封禁
// provider（即实际调用方传 UsableProviders()）时，路由组内的 banned 成员不会产出
// 空 provider 候选，也不会被选中。对应 D3 的「Provider 封禁」全链路排除。
func TestBannedProviderExcludedFromCandidates(t *testing.T) {
	r := NewRouter(NewScorer())
	r.AddRouterWithStrategy("g", []string{"good-m", "evil-m"}, "priority")

	providers := []*config.Provider{
		{Name: "good", Enabled: true, Models: []string{"m"}},
		{Name: "evil", Enabled: true, Banned: true, Models: []string{"m"}},
	}
	usable := make([]*config.Provider, 0)
	for _, p := range providers {
		if !p.Banned && p.Enabled {
			usable = append(usable, p)
		}
	}

	cands := r.PickCandidates("g", usable, nil, PickOptions{})
	for _, c := range cands {
		if c.Provider != nil && c.Provider.Name == "evil" {
			t.Fatalf("banned provider evil must not appear in candidates")
		}
	}
	if len(cands) != 1 || cands[0].Provider == nil || cands[0].Provider.Name != "good" {
		t.Fatalf("expected exactly one 'good' candidate, got %+v", cands)
	}
}

// TestNilProviderCandidateSkipped 验证：findProviderByModel 在候选集里找不到模型时，
// 该成员被跳过（不会产出 Provider=nil 的候选导致后续拨号 panic）。
func TestNilProviderCandidateSkipped(t *testing.T) {
	r := NewRouter(NewScorer())
	r.AddRouterWithStrategy("g", []string{"unknown-m"}, "priority")
	usable := []*config.Provider{{Name: "good", Enabled: true, Models: []string{"m"}}}

	cands := r.PickCandidates("g", usable, nil, PickOptions{})
	for _, c := range cands {
		if c.Provider == nil {
			t.Fatalf("nil-provider candidate must be skipped, got %+v", cands)
		}
	}
}
