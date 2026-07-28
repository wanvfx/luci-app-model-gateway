package engine

import (
	"testing"
	"time"

	"github.com/wanvfx/luci-app-model-gateway/config"
)

// TestPriorityStrategyRespectsOrder 验证 priority 策略严格按用户拖拽顺序返回候选，
// 不被质量分重排（核心修复：此前 PickCandidates 总是用 scorer.Rank 覆盖用户顺序）。
func TestPriorityStrategyRespectsOrder(t *testing.T) {
	providers := []*config.Provider{
		{Name: "A", Enabled: true, Models: []string{"m1"}},
		{Name: "B", Enabled: true, Models: []string{"m2"}},
		{Name: "C", Enabled: true, Models: []string{"m3"}},
	}
	scorer := NewScorer()
	// 让 m3 分数最高（若按 quality 排会排第一）
	scorer.Record(ProbeResult{Model: "C-m3", OK: true, Latency: 50 * time.Millisecond, Time: time.Now()})
	scorer.Record(ProbeResult{Model: "A-m1", OK: false, Time: time.Now()})

	r := NewRouter(scorer)
	r.AddRouterWithStrategy("mygroup", []string{"A-m1", "B-m2", "C-m3"}, "priority")

	cands := r.PickCandidates("mygroup", providers, nil, PickOptions{})
	if len(cands) != 3 {
		t.Fatalf("期望 3 个候选，实际 %d", len(cands))
	}
	want := []string{"m1", "m2", "m3"}
	for i, c := range cands {
		if c.Model != want[i] {
			t.Errorf("priority 策略第 %d 位期望 %q，实际 %q（顺序被重排 = 回归）", i, want[i], c.Model)
		}
	}
}

// TestLoadbalanceStrategyRotates 验证 loadbalance 策略每次请求轮转起点
func TestLoadbalanceStrategyRotates(t *testing.T) {
	providers := []*config.Provider{
		{Name: "A", Enabled: true, Models: []string{"m1", "m2"}},
	}
	r := NewRouter(NewScorer())
	r.AddRouterWithStrategy("lb", []string{"A-m1", "A-m2"}, "loadbalance")

	first := r.PickCandidates("lb", providers, nil, PickOptions{})
	second := r.PickCandidates("lb", providers, nil, PickOptions{})
	if len(first) != 2 || len(second) != 2 {
		t.Fatalf("候选数量异常: %d / %d", len(first), len(second))
	}
	if first[0].Model == second[0].Model {
		t.Errorf("loadbalance 应轮转起点，两次首选相同: %q", first[0].Model)
	}
}

// TestCostStrategyPrefersCheap 验证 cost 策略按参考库价格升序（免费优先）
func TestCostStrategyPrefersCheap(t *testing.T) {
	providers := []*config.Provider{
		{Name: "A", Enabled: true, Models: []string{"expensive-model", "free-model"}},
	}
	r := NewRouter(NewScorer())
	cat := &Catalog{models: map[string]*CatalogEntry{
		"expensive-model": {PriceIn: 2.0, PriceOut: 6.0},
		"free-model":      {PriceIn: 0, PriceOut: 0},
	}}
	r.SetCatalog(cat)
	r.AddRouterWithStrategy("costgrp", []string{"A-expensive-model", "A-free-model"}, "cost")

	cands := r.PickCandidates("costgrp", providers, nil, PickOptions{})
	if len(cands) != 2 {
		t.Fatalf("期望 2 个候选，实际 %d", len(cands))
	}
	if cands[0].Model != "free-model" {
		t.Errorf("cost 策略应免费模型优先，实际首选 %q", cands[0].Model)
	}
}

// TestCircuitFailureAttribution 验证熔断归因：429/鉴权/客户端取消不跳闸，仅服务端故障跳闸
func TestCircuitFailureAttribution(t *testing.T) {
	cp := NewCircuitPool(3, time.Minute)
	key := "P||m"

	// 429/鉴权/配额/客户端各记 10 次，不应跳闸
	for i := 0; i < 10; i++ {
		cp.RecordFailureWithType(key, FailRate)
		cp.RecordFailureWithType(key, FailAuth)
		cp.RecordFailureWithType(key, FailQuota)
		cp.RecordFailureWithType(key, FailClient)
	}
	if cp.IsOpen(key) {
		t.Errorf("非服务端故障不应触发熔断")
	}

	// 服务端故障 3 次达到阈值，应跳闸
	for i := 0; i < 3; i++ {
		cp.RecordFailureWithType(key, FailServer)
	}
	if !cp.IsOpen(key) {
		t.Errorf("3 次服务端故障后应熔断")
	}
}

// TestClassifyStatus 验证状态码归因映射
func TestClassifyStatus(t *testing.T) {
	cases := map[int]FailKind{
		429: FailRate,
		401: FailAuth,
		403: FailAuth,
		402: FailQuota,
		400: FailClient,
		404: FailClient,
		500: FailServer,
		502: FailServer,
		503: FailServer,
	}
	for code, want := range cases {
		if got := ClassifyStatus(code); got != want {
			t.Errorf("ClassifyStatus(%d) = %v，期望 %v", code, got, want)
		}
	}
}

// TestCatalogLookup 验证参考库查找：精确/前缀形态/关键词兜底
func TestCatalogLookup(t *testing.T) {
	cat := &Catalog{
		models: map[string]*CatalogEntry{
			"deepseek-v4-flash": {Tier: "mid", Capabilities: []string{"text"}},
		},
		rules: []CatalogRule{
			{Contains: []string{"-vl"}, CatalogEntry: CatalogEntry{Capabilities: []string{"text", "vision"}}},
		},
	}

	// 精确
	if e := cat.Lookup("deepseek-v4-flash"); e == nil || e.Tier != "mid" {
		t.Errorf("精确匹配失败")
	}
	// provider/ 前缀
	if e := cat.Lookup("deepseek-ai/deepseek-v4-flash"); e == nil || e.Tier != "mid" {
		t.Errorf("provider/ 前缀归一化失败")
	}
	// 网关前缀形态 p.Name-m
	if e := cat.Lookup("NVIDIA-deepseek-v4-flash"); e == nil || e.Tier != "mid" {
		t.Errorf("网关前缀后缀匹配失败")
	}
	// 关键词兜底
	if e := cat.Lookup("some-model-vl-8b"); e == nil || len(e.Capabilities) != 2 {
		t.Errorf("关键词兜底规则失败")
	}
	// 未命中
	if e := cat.Lookup("totally-unknown"); e != nil {
		t.Errorf("未知模型应返回 nil")
	}
}
