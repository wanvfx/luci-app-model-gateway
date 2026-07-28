package proxy

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

// modelsCache /v1/models 响应缓存（30 秒，与 Python 原版 MODELS_CACHE_TTL 一致）
type modelsCache struct {
	mu      sync.RWMutex
	data    []byte
	expires time.Time
	ttl     time.Duration
}

func newModelsCache(ttl time.Duration) *modelsCache {
	return &modelsCache{ttl: ttl}
}

func (c *modelsCache) Get() []byte {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.data != nil && time.Now().Before(c.expires) {
		return c.data
	}
	return nil
}

func (c *modelsCache) Set(data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.data = data
	c.expires = time.Now().Add(c.ttl)
}

// Invalidate 清空缓存，使下一次 /v1/models 请求立即用最新配置重新构建。
// 配置热更新（新增/编辑/勾选模型/删除提供商）后必须调用，否则面板要等 TTL 过期才刷新。
func (c *modelsCache) Invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.data = nil
	c.expires = time.Time{}
}

// enrichWithCatalog 从模型参考库注入分类信息（档位/能力/价格/家族）。
// 未命中参考库时走兜底（默认 mid 档 + text 能力，并按名族关键词补全 vision/reasoning），
// 确保前端不再出现"未分类"。
func (s *Server) enrichWithCatalog(entry map[string]interface{}, model string) {
	if s.catalog == nil {
		return
	}
	e := s.catalog.LookupOrDefault(model)
	if e.Tier != "" {
		entry["tier"] = e.Tier
	}
	if len(e.Capabilities) > 0 {
		entry["capabilities"] = e.Capabilities
	}
	if e.PriceIn > 0 || e.PriceOut > 0 {
		entry["price_in"] = e.PriceIn
		entry["price_out"] = e.PriceOut
	}
	if e.Family != "" {
		entry["family"] = e.Family
	}
	// 参考库有上下文长度而上游详情缺失时兜底
	if e.ContextLength > 0 {
		if v, ok := entry["context_length"]; !ok || v == nil || v == 0 {
			entry["context_length"] = e.ContextLength
		}
	}
}

// handleModels 处理 GET /v1/models
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if !s.authClient(r).authed {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 检查缓存（与 Python 原版一致：30 秒缓存）
	if cached := s.modelsCache.Get(); cached != nil {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(cached)
		return
	}

	// 构建禁用集合（全局 + 各 provider 的 per-provider 禁用，对齐 Python）
	disabled := map[string]bool{}
	for _, dm := range s.cfg.Load().AllDisabledModels() {
		disabled[strings.ToLower(strings.TrimSpace(dm))] = true
	}

	var data []map[string]interface{}

	// 自定义路由组作为可输出的模型（附带策略，前端展示用）
	hasAuto := false
	for _, rtr := range s.cfg.Load().Routers {
		strategy := rtr.Strategy
		if strategy == "" {
			strategy = "quality"
		}
		if rtr.Name == "auto" {
			hasAuto = true
		}
		data = append(data, map[string]interface{}{
			"id":        rtr.Name,
			"object":    "model",
			"owned_by":  "Router",
			"available": true,
			"strategy":  strategy,
		})
	}
	// auto 虚拟路由组（内存态，聚合所有启用模型；用户自建同名组时不重复）
	if !hasAuto && s.router.Load().IsRouter("auto") {
		data = append(data, map[string]interface{}{
			"id":        "auto",
			"object":    "model",
			"owned_by":  "Router",
			"available": true,
			"strategy":  "quality",
			"virtual":   true,
		})
	}

	for _, p := range s.cfg.Load().Providers {
		if !p.Enabled {
			continue
		}
		for _, m := range p.Models {
			// 过滤禁用模型
			if disabled[strings.ToLower(m)] {
				continue
			}

			// 获取模型描述（与 Python 原版一致：返回 desc 字段）
			desc := ""
			if descriptions := s.meta.ModelDescriptions(); descriptions != nil {
				if d, ok := descriptions[m]; ok {
					desc = d["desc"]
				}
			}

			// 对外暴露的 id 始终为前缀名 p.Name-m（与 Python 原版 f"{name}-{m}" 一致），
			// 这样客户端拉取到的模型名与聊天路由双向匹配，避免「列表看得到、调用报 model not supported」。
			// 详情缓存按真实模型名 m 索引（FetchAndCache / UpdateFromUpstream 均用真实名 key），
			// 故此处用 m 查询以正确回填 context_length 等字段。
			fullID := p.Name + "-" + m
			// available 字段：对齐 Python 原版按健康态计算（而非硬编码 true）。
			// Go 以熔断状态作为「近期是否连续失败」的信号：该 provider||model 熔断开启则视为不可用。
			available := true
			if s.circuits != nil && s.circuits.IsOpen(p.Name+"||"+m) {
				available = false
			}
			detail := s.modelCache.Get(m)
			if detail != nil {
				entry := map[string]interface{}{
					"id":                      fullID,
					"model":                   m, // 原始模型名（含 provider/ 前缀），供面板按 d.model 直接索引参考库
					"object":                  detail.Object,
					"created":                 detail.Created,
					"owned_by":                detail.OwnedBy,
					"available":               available,
					"context_length":          detail.ContextLen,
					"max_position_embeddings": detail.MaxPosEmb,
					"max_model_len":           detail.MaxModelLen,
					"is_free":                 s.meta.IsFreeModel(m),
					"supports_vision":         s.meta.IsVisionModel(m),
				}
				if desc != "" {
					entry["desc"] = desc
				}
				s.enrichWithCatalog(entry, m)
				data = append(data, entry)
			} else {
				ctxLen := s.meta.ContextLength(m)
				entry := map[string]interface{}{
					"id":                      fullID,
					"model":                   m, // 原始模型名（含 provider/ 前缀），供面板按 d.model 直接索引参考库
					"object":                  "model",
					"owned_by":                p.Name,
					"available":               available,
					"context_length":          ctxLen,
					"max_position_embeddings": ctxLen,
					"max_model_len":           ctxLen,
					"is_free":                 s.meta.IsFreeModel(m),
					"supports_vision":         s.meta.IsVisionModel(m),
				}
				if desc != "" {
					entry["desc"] = desc
				}
				s.enrichWithCatalog(entry, m)
				data = append(data, entry)
			}
		}
	}

	result := map[string]interface{}{
		"object": "list",
		"data":   data,
	}
	resultBytes, _ := json.Marshal(result)

	// 写入缓存
	s.modelsCache.Set(resultBytes)

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(resultBytes)
}
