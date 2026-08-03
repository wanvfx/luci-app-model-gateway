package engine

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// CatalogEntry 模型参考库条目（分类/档位/能力/成本的唯一数据源）
// Tier: lite(轻量) / mid(中端) / top(顶级)，空表示未分类
// Capabilities: text / vision / reasoning / image / audio / embedding
// PriceIn/PriceOut: 美元 / 每百万 token（0 表示免费或未知，配合 Free 字段区分）
type CatalogEntry struct {
	Tier          string   `json:"tier,omitempty"`
	Capabilities  []string `json:"capabilities,omitempty"`
	ContextLength int      `json:"context_length,omitempty"`
	PriceIn       float64  `json:"price_in,omitempty"`
	PriceOut      float64  `json:"price_out,omitempty"`
	Family        string   `json:"family,omitempty"`
}

// CatalogRule 关键词兜底规则：模型名（小写）包含任一关键词即套用
type CatalogRule struct {
	Contains []string `json:"contains"`
	CatalogEntry
}

// catalogFile JSON 文件结构
type catalogFile struct {
	Version int                      `json:"version"`
	Models  map[string]*CatalogEntry `json:"models"`
	Rules   []CatalogRule            `json:"rules"`
}

// Catalog 模型参考库（三层加载：内置 → share → dataDir 覆盖）
type Catalog struct {
	mu     sync.RWMutex
	models map[string]*CatalogEntry // key 已归一化（小写、去 provider/ 前缀）
	rules  []CatalogRule
}

// LoadCatalog 加载 models_catalog.json，查找顺序与 MetaStore 一致：
// 1. APP_DIR/models_catalog.json（开发环境）
// 2. APP_DIR/../share/model-gateway/models_catalog.json（路由器安装位置，随包只读）
// 3. DATA_DIR/models_catalog_sync.json（价格自动同步覆盖层，来自 models.dev）
// 4. DATA_DIR/models_catalog.json（用户手工覆盖，优先级最高；可写数据统一走 MODEL_GATEWAY_DATA）
func LoadCatalog(appDir, dataDir string) *Catalog {
	c := &Catalog{models: map[string]*CatalogEntry{}}
	c.loadLayers(appDir, dataDir)
	return c
}

// loadLayers 按优先级依次合并各层数据（后加载的覆盖先加载的）
func (c *Catalog) loadLayers(appDir, dataDir string) {
	paths := []string{}
	// S5：环境变量资源目录优先（MODEL_GATEWAY_APP），覆盖开发/安装布局差异
	if env := os.Getenv("MODEL_GATEWAY_APP"); env != "" {
		paths = append(paths, filepath.Join(env, "models_catalog.json"))
	}
	paths = append(paths,
		filepath.Join(appDir, "models_catalog.json"),
		filepath.Join(appDir, "..", "share", "model-gateway", "models_catalog.json"),
	)
	if dataDir != "" {
		paths = append(paths,
			filepath.Join(dataDir, "models_catalog_sync.json"),
			filepath.Join(dataDir, "models_catalog.json"),
		)
	}
	for _, p := range paths {
		if data, err := os.ReadFile(p); err == nil {
			c.merge(data)
		}
	}
	log.Printf("[catalog] Loaded %d entries from %d paths (appDir=%s dataDir=%s)", len(c.models), len(paths), appDir, dataDir)
}

// Reload 原子重载全部分层数据（价格自动同步落盘后调用）。
// 先在临时容器完成合并，再一次性替换，避免读方看到半成品。
func (c *Catalog) Reload(appDir, dataDir string) {
	tmp := &Catalog{models: map[string]*CatalogEntry{}}
	tmp.loadLayers(appDir, dataDir)
	c.mu.Lock()
	c.models = tmp.models
	c.rules = tmp.rules
	c.mu.Unlock()
}

// merge 合并一层数据（models 按 key 覆盖，rules 追加）
func (c *Catalog) merge(data []byte) {
	var f catalogFile
	if err := json.Unmarshal(data, &f); err != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, v := range f.Models {
		if v != nil {
			c.models[normalizeCatalogKey(k)] = v
		}
	}
	if len(f.Rules) > 0 {
		c.rules = append(c.rules, f.Rules...)
	}
}

// normalizeCatalogKey 归一化：去 provider/ 前缀 + 小写
func normalizeCatalogKey(model string) string {
	if idx := strings.LastIndex(model, "/"); idx >= 0 {
		model = model[idx+1:]
	}
	return strings.ToLower(strings.TrimSpace(model))
}

// Lookup 查找模型条目：精确 → 去前缀 → 关键词兜底规则。
// 未命中返回 nil；需要"绝不返回 nil"的兜底请改用 LookupOrDefault。
func (c *Catalog) Lookup(model string) *CatalogEntry {
	c.mu.RLock()
	defer c.mu.RUnlock()

	norm := normalizeCatalogKey(model)

	// 1. 精确匹配（key 已归一化）
	if e, ok := c.models[norm]; ok {
		return e
	}

	// 2. 网关前缀形态（如 nvidia-deepseek-v4-flash）：取最长 key 后缀匹配
	var best *CatalogEntry
	bestLen := 0
	for k, e := range c.models {
		if len(k) > bestLen && (strings.HasSuffix(norm, "-"+k) || strings.HasSuffix(norm, "/"+k)) {
			best, bestLen = e, len(k)
		}
	}
	if best != nil {
		return best
	}

	// 3. 关键词兜底规则（叠加合并：后面的规则补前面未填的字段）
	var merged *CatalogEntry
	for i := range c.rules {
		r := &c.rules[i]
		hit := false
		for _, kw := range r.Contains {
			if kw != "" && strings.Contains(norm, strings.ToLower(kw)) {
				hit = true
				break
			}
		}
		if !hit {
			continue
		}
		if merged == nil {
			merged = &CatalogEntry{}
		}
		if merged.Tier == "" && r.Tier != "" {
			merged.Tier = r.Tier
		}
		if len(merged.Capabilities) == 0 && len(r.Capabilities) > 0 {
			merged.Capabilities = append([]string{}, r.Capabilities...)
		}
		if merged.ContextLength == 0 && r.ContextLength > 0 {
			merged.ContextLength = r.ContextLength
		}
		if merged.PriceIn == 0 && r.PriceIn > 0 {
			merged.PriceIn = r.PriceIn
		}
		if merged.PriceOut == 0 && r.PriceOut > 0 {
			merged.PriceOut = r.PriceOut
		}
		if merged.Family == "" && r.Family != "" {
			merged.Family = r.Family
		}
	}
	return merged
}

// enhanceCapabilitiesByName 按模型名族关键词补全能力（容错兜底，消灭"未分类"）。
// 仅在已有能力未覆盖时追加，不覆盖参考库/规则已给出的精确能力。
func enhanceCapabilitiesByName(name string, e *CatalogEntry) {
	present := make(map[string]bool, len(e.Capabilities))
	for _, c := range e.Capabilities {
		present[strings.ToLower(c)] = true
	}
	add := func(cap string) {
		if !present[cap] {
			present[cap] = true
			e.Capabilities = append(e.Capabilities, cap)
		}
	}
	n := strings.ToLower(name)
	if strings.Contains(n, "vision") || strings.Contains(n, "-vl") || strings.Contains(n, "vl-") || strings.Contains(n, "visual") {
		add("vision")
	}
	if strings.Contains(n, "thinking") || strings.Contains(n, "reason") ||
		strings.Contains(n, "r1") || strings.Contains(n, "qwq") ||
		strings.Contains(n, "o1") || strings.Contains(n, "o3") || strings.Contains(n, "o4") {
		add("reasoning")
	}
}

// LookupOrDefault 查找模型条目；未命中任何精确/后缀/规则时返回合理兜底（绝不返回 nil），
// 默认 mid 档 + text 能力，并按模型名族关键词补全 vision/reasoning，消灭前端"未分类"。
func (c *Catalog) LookupOrDefault(model string) *CatalogEntry {
	e := c.Lookup(model)
	if e == nil {
		e = &CatalogEntry{Tier: "mid", Capabilities: []string{"text"}}
	}
	if e.Tier == "" {
		e.Tier = "mid"
	}
	enhanceCapabilitiesByName(normalizeCatalogKey(model), e)
	return e
}

// HasCapabilities 判断条目是否具备所需全部能力（不区分大小写）。
// 用于「严格能力矩阵」：请求需要 vision/reasoning 等能力时，只选具备的候选。
func (e *CatalogEntry) HasCapabilities(caps []string) bool {
	if len(caps) == 0 {
		return true
	}
	set := make(map[string]bool, len(e.Capabilities))
	for _, c := range e.Capabilities {
		set[strings.ToLower(strings.TrimSpace(c))] = true
	}
	for _, need := range caps {
		if !set[strings.ToLower(strings.TrimSpace(need))] {
			return false
		}
	}
	return true
}

// All 返回全量条目（key 已归一化，供 /api/catalog 调试查看）
func (c *Catalog) All() map[string]*CatalogEntry {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]*CatalogEntry, len(c.models))
	for k, v := range c.models {
		out[k] = v
	}
	return out
}

// Size 条目数量
func (c *Catalog) Size() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.models)
}
