package engine

import (
	"math/rand"
	"sort"
	"strings"
	"sync"

	"github.com/wanvfx/luci-app-model-gateway/config"
)

// Candidate 候选上游
type Candidate struct {
	Model    string
	Provider *config.Provider
}

// Router 路由引擎
type Router struct {
	routers    map[string][]string // groupName -> []memberModel
	strategies map[string]string   // groupName -> strategy（空 = quality）
	weights    map[string][]int    // groupName -> 成员权重（weighted 策略用）
	scorer     *Scorer
	catalog    *Catalog        // 模型参考库（cost 策略用，可为 nil）
	penalty    *PenaltyTracker // 动态惩罚追踪器（可为 nil；priority 策略不参与降权）
	rrMu       sync.Mutex
	rrCounters map[string]uint64 // groupName -> 轮转计数（loadbalance 策略用）
}

// NewRouter 创建路由引擎
func NewRouter(scorer *Scorer) *Router {
	return &Router{
		routers:    make(map[string][]string),
		strategies: make(map[string]string),
		weights:    make(map[string][]int),
		rrCounters: make(map[string]uint64),
		scorer:     scorer,
	}
}

// SetCatalog 注入模型参考库（cost 策略排序依据）
func (r *Router) SetCatalog(c *Catalog) {
	r.catalog = c
}

// SetPenalty 注入动态惩罚追踪器（近期频繁 429/5xx 的成员临时降权到队尾）
func (r *Router) SetPenalty(p *PenaltyTracker) {
	r.penalty = p
}

// SetWeights 设置路由组成员权重（weighted 策略；与 members 一一对应）
func (r *Router) SetWeights(group string, weights []int) {
	if len(weights) > 0 {
		r.weights[group] = weights
	}
}

// AddRouter 添加路由组（默认 quality 策略，向后兼容）
func (r *Router) AddRouter(name string, members []string) {
	r.AddRouterWithStrategy(name, members, "")
}

// AddRouterWithStrategy 添加带策略的路由组
func (r *Router) AddRouterWithStrategy(name string, members []string, strategy string) {
	r.routers[name] = members
	if strategy != "" {
		r.strategies[name] = strategy
	}
}

// Strategy 返回路由组策略（默认 quality）
func (r *Router) Strategy(name string) string {
	if s, ok := r.strategies[name]; ok && s != "" {
		return s
	}
	return "quality"
}

// IsRouter 检查 model 是否为路由组名
func (r *Router) IsRouter(model string) bool {
	_, ok := r.routers[model]
	return ok
}

// PickOptions 选候选时的附加选项（第二批扩展：严格能力矩阵 + 内容分类路由）
type PickOptions struct {
	Strict       bool     // 严格能力矩阵：候选必须全部具备 RequiredCaps
	RequiredCaps []string // 请求所需能力（如 vision），来自请求解析
	Content      string   // 请求正文（内容分类路由 classify 策略用）
}

// PickCandidates 挑选候选模型
//   - 如果 model 是路由组名，返回组内按分数排序的候选列表
//   - classify 策略路由组：按 Content 分类后委托到对应子组（members 写成 cat=group）
//   - 否则返回 [model]（显式真实模型直接透传）
//   - disabled 集合中的模型会被跳过（对齐 Python get_enabled_models 排除 disabled_models）
//   - Strict + RequiredCaps：过滤不满足所需能力的候选（严格能力矩阵）
//
// 注意：对齐 Python 原版（app.py:828-850）——在 pick 阶段「不」对点名模型/路由组做熔断过滤：
// 用户显式点名的模型（app.py:835 `if force or model`）直接入选，不做熔断判断；
// 路由组分支（app.py:814-826）也不参与熔断。熔断仅在转发失败时被记录（RecordFailure），
// 用于 is_circuit_open 的健康统计，但不在此阻断选择。这也修正了此前用错熔断 key
// （模型名而非 provider||model）导致路由路径熔断形同虚设的隐患。
func (r *Router) PickCandidates(model string, providers []*config.Provider, disabled map[string]bool, opts PickOptions) []*Candidate {
	return r.pickCandidates(model, providers, disabled, opts, 0)
}

// pickCandidates 内部实现，带递归深度护栏（安全修复）：
// classify 路由组若被用户误配成自引用/互相引用（A→B→A），无护栏会无限递归栈溢出崩溃。
// 深度上限 4 足够覆盖正常的 classify→子组 委托链。
func (r *Router) pickCandidates(model string, providers []*config.Provider, disabled map[string]bool, opts PickOptions, depth int) []*Candidate {
	if depth > 4 {
		return []*Candidate{}
	}
	if members, ok := r.routers[model]; ok {
		// classify 策略：按 Content 分类后委托到对应子路由组（递归，子组不再走 classify）
		if r.Strategy(model) == "classify" {
			target := r.classifyTarget(members, opts.Content)
			if target == "" || target == model {
				return []*Candidate{}
			}
			return r.pickCandidates(target, providers, disabled, opts, depth+1)
		}
		// 路由组：按组策略排序成员（quality/priority/least-latency/cost/loadbalance/weighted）
		ordered := r.orderMembers(model, members)
		var result []*Candidate
		for _, m := range ordered {
			// 过滤禁用模型
			if disabled != nil && disabled[strings.ToLower(m)] {
				continue
			}
			provider, realModel := findProviderByModel(m, providers)
			if realModel == "" {
				realModel = m
			}
			result = append(result, &Candidate{
				Model:    realModel,
				Provider: provider,
			})
		}
		// 严格能力矩阵：过滤不满足所需能力的候选
		result = r.filterCapabilities(result, opts)
		if len(result) == 0 {
			// fallback: 全部成员（不过滤，保留原始行为）
			for _, m := range members {
				provider, realModel := findProviderByModel(m, providers)
				if realModel == "" {
					realModel = m
				}
				result = append(result, &Candidate{
					Model:    realModel,
					Provider: provider,
				})
			}
		}
		return result
	}
	// 显式真实模型（或前缀名 p.Name-m），直接透传
	provider, realModel := findProviderByModel(model, providers)
	if provider == nil || realModel == "" {
		return []*Candidate{}
	}
	// 过滤禁用模型
	if disabled != nil && disabled[strings.ToLower(realModel)] {
		return []*Candidate{}
	}
	return []*Candidate{{
		Model:    realModel,
		Provider: provider,
	}}
}

// classifyTarget 从 classify 路由组的 members（cat=group 形式）中选出与 Content 分类匹配的组名。
// 命中 category→group 则返回该组；否则返回 default=group（无 default 则返回第一个成员）。
func (r *Router) classifyTarget(members []string, content string) string {
	cat := Classify(content)
	rules := map[string]string{}
	var def string
	for _, m := range members {
		if i := strings.Index(m, "="); i >= 0 {
			k := strings.ToLower(strings.TrimSpace(m[:i]))
			v := strings.TrimSpace(m[i+1:])
			if k == "default" || k == "general" {
				if def == "" {
					def = v
				}
			} else if v != "" {
				rules[k] = v
			}
		} else if def == "" {
			// 无 '=' 的成员视为默认组
			def = m
		}
	}
	if g, ok := rules[cat]; ok && g != "" {
		return g
	}
	return def
}

// filterCapabilities 严格能力矩阵：仅保留具备 RequiredCaps 全部能力的候选。
func (r *Router) filterCapabilities(cands []*Candidate, opts PickOptions) []*Candidate {
	if !opts.Strict || len(opts.RequiredCaps) == 0 || r.catalog == nil {
		return cands
	}
	out := cands[:0]
	for _, c := range cands {
		e := r.catalog.Lookup(c.Model)
		if e != nil && e.HasCapabilities(opts.RequiredCaps) {
			out = append(out, c)
		}
	}
	return out
}

// orderMembers 按路由组策略对成员排序，返回排序后的成员名列表。
//   - quality（默认）：质量分降序，跳过不可用成员（保持历史行为）
//   - priority：严格保留 Members 原始顺序（尊重用户拖拽的优先级，不被评分覆盖）
//   - least-latency：巡检平均延迟升序（无数据的排后面）
//   - cost：参考库价格升序（免费/便宜优先，未知价格排后面）
//   - loadbalance：每次请求轮转起点（简单均衡分摊）
//   - weighted：按权重随机抽首选（A/B 灰度分流），其余按权重降序作 failover
//
// 除 priority（用户显式顺序神圣不可动）外，最终结果再经动态惩罚降权：
// 近期频繁 429/5xx 的成员被临时挪到队尾，恢复后自动回位。
func (r *Router) orderMembers(group string, members []string) []string {
	strategy := r.Strategy(group)
	out := r.orderByStrategy(group, strategy, members)
	if strategy != "priority" && r.penalty != nil {
		out = r.penalty.Demote(out)
	}
	return out
}

// orderByStrategy 按策略排序（不含惩罚降权）
func (r *Router) orderByStrategy(group, strategy string, members []string) []string {
	switch strategy {
	case "priority":
		// 严格按用户排列的顺序（核心修复：不再被 scorer.Rank 重排）
		out := make([]string, len(members))
		copy(out, members)
		return out

	case "weighted":
		return r.orderWeighted(group, members)

	case "least-latency":
		type item struct {
			model   string
			latency int64
			hasData bool
		}
		items := make([]item, 0, len(members))
		for _, m := range members {
			sc := r.scorerScore(m)
			it := item{model: m}
			if sc != nil && sc.Latency > 0 {
				it.latency = sc.Latency.Milliseconds()
				it.hasData = true
			}
			items = append(items, it)
		}
		sort.SliceStable(items, func(i, j int) bool {
			if items[i].hasData != items[j].hasData {
				return items[i].hasData // 有数据的排前面
			}
			return items[i].latency < items[j].latency
		})
		out := make([]string, len(items))
		for i, it := range items {
			out[i] = it.model
		}
		return out

	case "cost":
		type item struct {
			model string
			price float64
			known bool
		}
		items := make([]item, 0, len(members))
		for _, m := range members {
			it := item{model: m}
			if r.catalog != nil {
				if e := r.catalog.Lookup(m); e != nil {
					it.price = e.PriceIn + e.PriceOut
					it.known = true
				}
			}
			items = append(items, it)
		}
		sort.SliceStable(items, func(i, j int) bool {
			if items[i].known != items[j].known {
				return items[i].known // 已知价格的排前面（免费=0 天然最前）
			}
			return items[i].price < items[j].price
		})
		out := make([]string, len(items))
		for i, it := range items {
			out[i] = it.model
		}
		return out

	case "loadbalance":
		if len(members) == 0 {
			return nil
		}
		r.rrMu.Lock()
		start := int(r.rrCounters[group] % uint64(len(members)))
		r.rrCounters[group]++
		r.rrMu.Unlock()
		out := make([]string, 0, len(members))
		out = append(out, members[start:]...)
		out = append(out, members[:start]...)
		return out

	default: // quality
		if r.scorer == nil {
			// scorer 未注入时退化为原始顺序（防御性，避免 nil panic）
			out := make([]string, len(members))
			copy(out, members)
			return out
		}
		scores := r.scorer.Rank(members)
		out := make([]string, 0, len(scores))
		for _, s := range scores {
			if !s.Available {
				continue
			}
			out = append(out, s.Model)
		}
		return out
	}
}

// orderWeighted weighted（A/B 条件路由）：按权重随机抽取首选成员实现灰度分流，
// 其余成员按权重降序排列作为 failover 链。权重缺省/不足时视为 1；全 0 时退化为均分。
func (r *Router) orderWeighted(group string, members []string) []string {
	n := len(members)
	if n == 0 {
		return nil
	}
	ws := r.weights[group]
	eff := make([]int, n)
	total := 0
	for i := 0; i < n; i++ {
		w := 1
		if i < len(ws) && ws[i] > 0 {
			w = ws[i]
		}
		eff[i] = w
		total += w
	}
	// 按权重随机抽首选
	pick := 0
	if total > 0 {
		t := rand.Intn(total)
		for i, w := range eff {
			if t < w {
				pick = i
				break
			}
			t -= w
		}
	}
	// 其余按权重降序（稳定）
	type item struct {
		m string
		w int
	}
	rest := make([]item, 0, n-1)
	for i, m := range members {
		if i != pick {
			rest = append(rest, item{m, eff[i]})
		}
	}
	sort.SliceStable(rest, func(i, j int) bool { return rest[i].w > rest[j].w })
	out := make([]string, 0, n)
	out = append(out, members[pick])
	for _, it := range rest {
		out = append(out, it.m)
	}
	return out
}

// scorerScore 安全获取评分（scorer 可能为 nil，如单测）
func (r *Router) scorerScore(model string) *ModelScore {
	if r.scorer == nil {
		return nil
	}
	sc := r.scorer.Score(model)
	return &sc
}

// findProviderByModel 根据模型名查找提供商，并返回还原后的真实模型名。
// 同时支持「真实模型名 m」与「前缀名 p.Name-m」：
// /v1/models 对外暴露的 id 是前缀名（与 Python 原版 f"{name}-{m}" 一致），
// 客户端回传聊天请求时也是前缀名，必须能还原到真实模型名再转发上游，
// 否则上游会报 "model not supported"。对应 Python pick_available_models 的
// `model != m and model != prefixed` 双向匹配。
func findProviderByModel(model string, providers []*config.Provider) (*config.Provider, string) {
	for _, p := range providers {
		for _, m := range p.Models {
			if m == model || (p.Name+"-"+m) == model {
				return p, m
			}
		}
	}
	// 未匹配到任何模型：返回 nil，由调用方判定为「无可用模型」
	return nil, ""
}
