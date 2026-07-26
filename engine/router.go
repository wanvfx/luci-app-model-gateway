package engine

import (
	"strings"

	"github.com/wanvfx/luci-app-model-gateway/config"
)

// Candidate 候选上游
type Candidate struct {
	Model    string
	Provider *config.Provider
}

// Router 路由引擎
type Router struct {
	routers map[string][]string // groupName -> []memberModel
	scorer  *Scorer
}

// NewRouter 创建路由引擎
func NewRouter(scorer *Scorer) *Router {
	return &Router{
		routers: make(map[string][]string),
		scorer:  scorer,
	}
}

// AddRouter 添加路由组
func (r *Router) AddRouter(name string, members []string) {
	r.routers[name] = members
}

// IsRouter 检查 model 是否为路由组名
func (r *Router) IsRouter(model string) bool {
	_, ok := r.routers[model]
	return ok
}

// PickCandidates 挑选候选模型
//   - 如果 model 是路由组名，返回组内按分数排序的候选列表
//   - 否则返回 [model]（显式真实模型直接透传）
//   - disabled 集合中的模型会被跳过（对齐 Python get_enabled_models 排除 disabled_models）
//
// 注意：对齐 Python 原版（app.py:828-850）——在 pick 阶段「不」对点名模型/路由组做熔断过滤：
// 用户显式点名的模型（app.py:835 `if force or model`）直接入选，不做熔断判断；
// 路由组分支（app.py:814-826）也不参与熔断。熔断仅在转发失败时被记录（RecordFailure），
// 用于 is_circuit_open 的健康统计，但不在此阻断选择。这也修正了此前用错熔断 key
// （模型名而非 provider||model）导致路由路径熔断形同虚设的隐患。
func (r *Router) PickCandidates(model string, providers []*config.Provider, disabled map[string]bool) []*Candidate {
	if members, ok := r.routers[model]; ok {
		// 路由组：按质量分排序
		scores := r.scorer.Rank(members)
		var result []*Candidate
		for _, s := range scores {
			if !s.Available {
				continue
			}
			// 过滤禁用模型
			if disabled != nil && disabled[strings.ToLower(s.Model)] {
				continue
			}
			provider, realModel := findProviderByModel(s.Model, providers)
			if realModel == "" {
				realModel = s.Model
			}
			result = append(result, &Candidate{
				Model:    realModel,
				Provider: provider,
			})
		}
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
