package engine

import (
	"testing"

	"github.com/wanvfx/luci-app-model-gateway/config"
)

// TestClassify 验证内容分类路由的启发式分类
func TestClassify(t *testing.T) {
	cases := []struct {
		content string
		want    string
	}{
		{"请看这张图 {\"type\":\"image_url\"}", CatVision},
		{"data:image/png;base64,xxxx", CatVision},
		{"def hello():\n    return 1\n", CatCode},
		{"SELECT * FROM users WHERE id=1", CatCode},
		{"这是一段非常非常长的文本……" + string(make([]byte, 5000)), CatLong},
		{"请逐步推理一下这道题 step by step", CatReasoning},
		{"今天天气不错，帮我写首诗", CatGeneral},
	}
	for _, c := range cases {
		if got := Classify(c.content); got != c.want {
			t.Errorf("Classify(%q) = %q，期望 %q", c.content[:min(20, len(c.content))], got, c.want)
		}
	}
}

// TestPickCandidatesClassify 验证 classify 路由组按内容委托到对应子组
func TestPickCandidatesClassify(t *testing.T) {
	providers := []*config.Provider{
		{Name: "Coders", Enabled: true, Models: []string{"coder-model"}},
		{Name: "VisionX", Enabled: true, Models: []string{"vision-model"}},
		{Name: "General", Enabled: true, Models: []string{"general-model"}},
	}
	r := NewRouter(nil)
	// 子组
	r.AddRouterWithStrategy("coders", []string{"Coders-coder-model"}, "quality")
	r.AddRouterWithStrategy("visionx", []string{"VisionX-vision-model"}, "quality")
	r.AddRouterWithStrategy("general", []string{"General-general-model"}, "quality")
	// classify 路由组：cat=group 映射
	r.AddRouterWithStrategy("router", []string{"code=coders", "vision=visionx", "default=general"}, "classify")

	code := r.PickCandidates("router", providers, nil, PickOptions{Content: "def add(a,b): return a+b"})
	if len(code) != 1 || code[0].Model != "coder-model" {
		t.Fatalf("code 内容应委托到 coders 组，实际 %+v", code)
	}

	vis := r.PickCandidates("router", providers, nil, PickOptions{Content: "看这张图 image_url"})
	if len(vis) != 1 || vis[0].Model != "vision-model" {
		t.Fatalf("vision 内容应委托到 visionx 组，实际 %+v", vis)
	}

	gen := r.PickCandidates("router", providers, nil, PickOptions{Content: "讲个笑话"})
	if len(gen) != 1 || gen[0].Model != "general-model" {
		t.Fatalf("general 内容应委托到 general 组，实际 %+v", gen)
	}
}

// TestFilterCapabilities 验证严格能力矩阵过滤不满足能力的候选
func TestFilterCapabilities(t *testing.T) {
	providers := []*config.Provider{
		{Name: "V", Enabled: true, Models: []string{"v-model"}},
		{Name: "T", Enabled: true, Models: []string{"t-model"}},
	}
	r := NewRouter(nil)
	cat := &Catalog{models: map[string]*CatalogEntry{
		"v-model": {Capabilities: []string{"text", "vision"}},
		"t-model": {Capabilities: []string{"text"}},
	}}
	r.SetCatalog(cat)
	r.AddRouterWithStrategy("grp", []string{"V-v-model", "T-t-model"}, "quality")

	// 需要 vision 且严格 → 仅 v-model
	cands := r.PickCandidates("grp", providers, nil, PickOptions{Strict: true, RequiredCaps: []string{"vision"}})
	if len(cands) != 1 || cands[0].Model != "v-model" {
		t.Fatalf("严格 vision 应仅剩 v-model，实际 %+v", cands)
	}

	// 非严格 → 全部保留
	all := r.PickCandidates("grp", providers, nil, PickOptions{Strict: false, RequiredCaps: []string{"vision"}})
	if len(all) != 2 {
		t.Fatalf("非严格应保留 2 个候选，实际 %d", len(all))
	}
}
