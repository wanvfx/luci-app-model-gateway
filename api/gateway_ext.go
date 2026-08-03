package api

// gateway_ext.go 三缺口管理端点：别名映射 / 语义缓存 / 扩展性（钩子·预算·并发）。
// 全部沿用既有 UCI 写入模式（uciTool + reloadConfig 热重载），
// 运行时对象（缓存统计/预算状态）经 SetGatewayRuntime 由 main.go 注入接口，避免 api→proxy 循环依赖。

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/wanvfx/luci-app-model-gateway/engine"
	"github.com/wanvfx/luci-app-model-gateway/storage"
)

// CacheControl 响应缓存运行时控制接口（由 proxy.Server 实现体注入）
type CacheControl interface {
	Stats() engine.CacheStats
	Clear()
}

// BudgetControl 预算运行时状态接口（由 storage.Budget 满足）
type BudgetControl interface {
	DailyTotal() float64
	Status(limit float64, action string, warningPct int) storage.BudgetStatus
}

// SetGatewayRuntime 注入运行时对象（main.go 装配时调用）
func (h *AdminHandler) SetGatewayRuntime(cache CacheControl, budget BudgetControl) {
	h.cacheCtl = cache
	h.budgetCtl = budget
}

// registerGatewayExtRoutes 注册三缺口端点（由 RegisterRoutes 调用）
func (h *AdminHandler) registerGatewayExtRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/aliases", h.handleAliases)
	mux.HandleFunc("/api/hooks", h.handleHooks)
	mux.HandleFunc("/api/gateway-settings", h.handleGatewaySettings)
	mux.HandleFunc("/api/cache", h.handleCacheAdmin)
	mux.HandleFunc("/api/budget-status", h.handleBudgetStatus)
	mux.HandleFunc("/api/price-sync", h.handlePriceSync)
	mux.HandleFunc("/api/free-model-guard", h.handleFreeModelGuard)
}

// ---------- 模型价格自动同步（models.dev） ----------

// handlePriceSync GET 返回同步状态；POST 立即触发一次同步（拉取→覆盖层→重载 catalog）
func (h *AdminHandler) handlePriceSync(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.Method {
	case http.MethodGet:
		if h.priceSync == nil {
			http.Error(w, "price-sync not available", http.StatusNotImplemented)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "data": h.priceSync.Status()})
	case http.MethodPost:
		if h.priceSync == nil {
			http.Error(w, "price-sync not available", http.StatusNotImplemented)
			return
		}
		n, err := h.priceSync.Sync()
		if err != nil {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"ok":    false,
				"count": n,
				"error": err.Error(),
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "count": n})
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// ---------- 免费模型自动巡检 ----------

// handleFreeModelGuard GET 返回巡检状态；POST 立即触发一次巡检
func (h *AdminHandler) handleFreeModelGuard(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.Method {
	case http.MethodGet:
		if h.freeModelGuard == nil {
			http.Error(w, "free-model-guard not available", http.StatusNotImplemented)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "data": h.freeModelGuard.Status()})
	case http.MethodPost:
		if h.freeModelGuard == nil {
			http.Error(w, "free-model-guard not available", http.StatusNotImplemented)
			return
		}
		go h.freeModelGuard.CheckNow()
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "message": "triggered"})
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// ---------- 别名映射 ----------

// handleAliases GET 返回全部别名；POST 全量替换（[{name,target},...]）
func (h *AdminHandler) handleAliases(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.Method {
	case http.MethodGet:
		type item struct {
			Name   string `json:"name"`
			Target string `json:"target"`
		}
		out := []item{}
		for _, a := range h.cfg.Load().Aliases {
			if a != nil && a.Name != "" {
				out = append(out, item{Name: a.Name, Target: a.Target})
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "data": out})
	case http.MethodPost:
		h.handleAliasesPost(w, r)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *AdminHandler) handleAliasesPost(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("read body failed: %v", err), http.StatusBadRequest)
		return
	}
	var req []struct {
		Name   string `json:"name"`
		Target string `json:"target"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, fmt.Sprintf("invalid request: %v", err), http.StatusBadRequest)
		return
	}
	if h.uciTool == nil {
		http.Error(w, "uci not available", http.StatusNotImplemented)
		return
	}

	// 校验：名称/目标非空、名称不重复、不允许自指
	seen := map[string]bool{}
	cleaned := req[:0]
	for _, a := range req {
		name := strings.TrimSpace(a.Name)
		target := strings.TrimSpace(a.Target)
		if name == "" || target == "" || name == target || seen[name] {
			continue
		}
		seen[name] = true
		a.Name, a.Target = name, target
		cleaned = append(cleaned, a)
	}

	// 全量替换：P2-5 改为单次 uci batch 原子替换（删除旧段 + 重建新段一次 commit），
	// 避免旧实现「先 DeleteSections、再逐个 AddSection+SetOptionWithCommit 各单独 commit」
	// 在中途失败时留下半删半建的中间态。
	items := make([]ReplaceItem, 0, len(cleaned))
	for _, a := range cleaned {
		items = append(items, ReplaceItem{Options: map[string]string{"name": a.Name, "target": a.Target}})
	}
	if err := h.uciTool.ReplaceSectionsAtomic("alias", items); err != nil {
		http.Error(w, fmt.Sprintf("replace aliases failed: %v", err), http.StatusInternalServerError)
		return
	}

	h.reloadAndStore()
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// ---------- REST 回调钩子 ----------

// handleHooks GET 返回全部钩子；POST 全量替换（[{url,events,enabled,secret},...]）
func (h *AdminHandler) handleHooks(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.Method {
	case http.MethodGet:
		type item struct {
			URL     string   `json:"url"`
			Events  []string `json:"events"`
			Enabled bool     `json:"enabled"`
			Secret  string   `json:"secret"`
		}
		out := []item{}
		for _, hk := range h.cfg.Load().Hooks {
			if hk != nil && hk.URL != "" {
				out = append(out, item{URL: hk.URL, Events: hk.Events, Enabled: hk.Enabled, Secret: hk.Secret})
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "data": out})
	case http.MethodPost:
		h.handleHooksPost(w, r)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *AdminHandler) handleHooksPost(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("read body failed: %v", err), http.StatusBadRequest)
		return
	}
	var req []struct {
		URL     string   `json:"url"`
		Events  []string `json:"events"`
		Enabled *bool    `json:"enabled"`
		Secret  string   `json:"secret"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, fmt.Sprintf("invalid request: %v", err), http.StatusBadRequest)
		return
	}
	if h.uciTool == nil {
		http.Error(w, "uci not available", http.StatusNotImplemented)
		return
	}

	// 校验：URL 必须 http(s)；事件白名单过滤（与 proxy/hooks.go 枚举对齐，共 6 种）
	validEvent := map[string]bool{
		"request_done":   true,
		"request_failed": true,
		"provider_down":  true,
		"provider_up":    true,
		"quota_exceeded": true,
		"circuit_open":   true,
	}
	type hookItem struct {
		url, secret string
		events      []string
		enabled     bool
	}
	var cleaned []hookItem
	for _, hk := range req {
		u := strings.TrimSpace(hk.URL)
		if u == "" || (!strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://")) {
			continue
		}
		var evs []string
		for _, ev := range hk.Events {
			if validEvent[ev] {
				evs = append(evs, ev)
			}
		}
		if len(evs) == 0 {
			evs = []string{"request_done", "request_failed"}
		}
		enabled := true
		if hk.Enabled != nil {
			enabled = *hk.Enabled
		}
		cleaned = append(cleaned, hookItem{url: u, secret: strings.TrimSpace(hk.Secret), events: evs, enabled: enabled})
	}

	// 全量替换：P2-5 改为单次 uci batch 原子替换（删除旧段 + 重建新段一次 commit）。
	items := make([]ReplaceItem, 0, len(cleaned))
	for _, hk := range cleaned {
		opt := map[string]string{"url": hk.url}
		if !hk.enabled {
			opt["enabled"] = "0"
		}
		if hk.secret != "" {
			opt["secret"] = hk.secret
		}
		items = append(items, ReplaceItem{Options: opt, Lists: map[string][]string{"events": hk.events}})
	}
	if err := h.uciTool.ReplaceSectionsAtomic("hook", items); err != nil {
		http.Error(w, fmt.Sprintf("replace hooks failed: %v", err), http.StatusInternalServerError)
		return
	}

	h.reloadAndStore()
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// ---------- 缓存 / 预算 / 并发 设置 ----------

// handleGatewaySettings GET 返回缓存/预算/并发生效配置；POST 持久化到 settings 段
func (h *AdminHandler) handleGatewaySettings(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.Method {
	case http.MethodGet:
		cfg := h.cfg.Load()
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"ok":                true,
			"cache":             cfg.EffectiveCache(),
			"budget":            cfg.EffectiveBudget(),
			"max_concurrency":   cfg.MaxConcurrency,
			"strict_capability": cfg.StrictCapability(),
			"ssrf_strict":       cfg.SSRFStrict(),
			"allow_clients":     cfg.AllowClients(),
			"banned_providers":  cfg.BannedProviders(),
		})
	case http.MethodPost:
		h.handleGatewaySettingsPost(w, r)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *AdminHandler) handleGatewaySettingsPost(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("read body failed: %v", err), http.StatusBadRequest)
		return
	}
	// 全部字段可选：只写传入的字段（指针判空）
	var req struct {
		CacheEnabled     *bool    `json:"cache_enabled"`
		CacheTTL         *int     `json:"cache_ttl"`
		CacheMaxEntries  *int     `json:"cache_max_entries"`
		CacheSemantic    *bool    `json:"cache_semantic"`
		BudgetLimit      *float64 `json:"budget_daily_limit"`
		BudgetAction     *string  `json:"budget_action"`
		BudgetWarnPct    *int     `json:"budget_warning_pct"`
		MaxConcurrency   *int     `json:"max_concurrency"`
		StrictCapability *bool    `json:"strict_capability"`
		SSRFStrict       *bool    `json:"ssrf_strict"`
		AllowClients     *string  `json:"allow_clients"`
		BannedProviders  *string  `json:"banned_providers"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, fmt.Sprintf("invalid request: %v", err), http.StatusBadRequest)
		return
	}
	if h.uciTool == nil {
		http.Error(w, "uci not available", http.StatusNotImplemented)
		return
	}

	set := func(key, val string) error {
		return h.uciTool.SetOptionWithCommit("model-gateway", "settings", key, val)
	}
	b2s := func(b bool) string {
		if b {
			return "1"
		}
		return "0"
	}
	if req.CacheEnabled != nil {
		if err := set("cache_enabled", b2s(*req.CacheEnabled)); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if req.CacheTTL != nil && *req.CacheTTL > 0 {
		_ = set("cache_ttl", strconv.Itoa(*req.CacheTTL))
	}
	if req.CacheMaxEntries != nil && *req.CacheMaxEntries > 0 {
		_ = set("cache_max_entries", strconv.Itoa(*req.CacheMaxEntries))
	}
	if req.CacheSemantic != nil {
		_ = set("cache_semantic", b2s(*req.CacheSemantic))
	}
	if req.BudgetLimit != nil && *req.BudgetLimit >= 0 {
		_ = set("budget_daily_limit", strconv.FormatFloat(*req.BudgetLimit, 'f', -1, 64))
	}
	if req.BudgetAction != nil && (*req.BudgetAction == "warn" || *req.BudgetAction == "block") {
		_ = set("budget_action", *req.BudgetAction)
	}
	if req.BudgetWarnPct != nil && *req.BudgetWarnPct > 0 && *req.BudgetWarnPct <= 100 {
		_ = set("budget_warning_pct", strconv.Itoa(*req.BudgetWarnPct))
	}
	if req.MaxConcurrency != nil && *req.MaxConcurrency >= 0 {
		_ = set("max_concurrency", strconv.Itoa(*req.MaxConcurrency))
	}
	if req.StrictCapability != nil {
		_ = set("strict_capability", b2s(*req.StrictCapability))
	}
	if req.SSRFStrict != nil {
		_ = set("ssrf_strict", b2s(*req.SSRFStrict))
	}
	if req.AllowClients != nil {
		if err := h.uciTool.SetList("model-gateway", "settings", "allow_clients", splitList(*req.AllowClients)); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if req.BannedProviders != nil {
		if err := h.uciTool.SetList("model-gateway", "settings", "banned_providers", splitList(*req.BannedProviders)); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	h.reloadAndStore()
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// ---------- 缓存运行时（统计 / 清空） ----------

// handleCacheAdmin GET 返回缓存统计；POST {action:"clear"} 或 DELETE 清空缓存
func (h *AdminHandler) handleCacheAdmin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if h.cacheCtl == nil {
		http.Error(w, "cache runtime not available", http.StatusNotImplemented)
		return
	}
	switch r.Method {
	case http.MethodGet:
		stats := h.cacheCtl.Stats()
		cc := h.cfg.Load().EffectiveCache()
		total := stats.Hits + stats.Misses
		hitRate := 0.0
		if total > 0 {
			hitRate = float64(stats.Hits) / float64(total)
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"ok":       true,
			"stats":    stats,
			"hit_rate": hitRate,
			"config":   cc,
		})
	case http.MethodPost, http.MethodDelete:
		if r.Method == http.MethodPost {
			var req struct {
				Action string `json:"action"`
			}
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &req)
			if req.Action != "clear" {
				http.Error(w, "unsupported action", http.StatusBadRequest)
				return
			}
		}
		h.cacheCtl.Clear()
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// ---------- 预算状态（余额预警 banner 数据源） ----------

// handleBudgetStatus GET 返回当日成本累计与预算状态
func (h *AdminHandler) handleBudgetStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.budgetCtl == nil {
		http.Error(w, "budget runtime not available", http.StatusNotImplemented)
		return
	}
	bc := h.cfg.Load().EffectiveBudget()
	st := h.budgetCtl.Status(bc.DailyLimitUSD, bc.Action, bc.WarningPct)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "data": st})
}

// reloadAndStore 热重载配置并同步 handler 快照（reloadConfig 闭包内部已同步 proxy.Server）
func (h *AdminHandler) reloadAndStore() {
	if h.reloadConfig != nil {
		if newCfg, err := h.reloadConfig(); err == nil {
			h.cfg.Store(newCfg)
		}
	}
}

// splitList 将逗号/换行/空格/分号分隔的文本拆成去空白后的字符串列表，忽略空项。
// 用于 IP 白名单、封禁 Provider 等多值文本字段的解析。
func splitList(s string) []string {
	var out []string
	for _, part := range strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == '\n' || r == ' ' || r == '\t' || r == ';' || r == '\r'
	}) {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// ---------- 虚拟密钥（子密钥）管理 ----------

// handleVKeys GET 列表 / POST 新增 / DELETE 删除。
// POST 返回的 key 为明文（仅此一次可见），列表接口已脱敏。
// GET /api/vkeys/{id}/reveal 返回单个密钥的明文 key（仅复制场景使用）。
func (h *AdminHandler) handleVKeys(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if h.vkeyStore == nil {
		http.Error(w, "vkey store not available", http.StatusNotImplemented)
		return
	}
	// 子路径：/api/vkeys/{id}/reveal → 返回明文 key
	if r.Method == http.MethodGet {
		path := strings.TrimPrefix(r.URL.Path, "/api/vkeys/")
		parts := strings.Split(path, "/")
		if len(parts) == 2 && parts[1] == "reveal" && parts[0] != "" {
			vk := h.vkeyStore.Get(parts[0])
			if vk == nil {
				http.Error(w, "vkey not found", http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "id": vk.ID, "key": vk.Key, "name": vk.Name})
			return
		}
	}
	switch r.Method {
	case http.MethodGet:
		type item struct {
			ID            string   `json:"id"`
			Name          string   `json:"name"`
			Key           string   `json:"key"` // 已脱敏
			Enabled       bool     `json:"enabled"`
			QuotaRequests int      `json:"quota_requests"`
			QuotaTokens   int      `json:"quota_tokens"`
			AllowedModels []string `json:"allowed_models"`
			Notes         string   `json:"notes"`
			CreatedAt     string   `json:"created_at"`
			Usage         struct {
				Requests int `json:"requests"`
				Tokens   int `json:"tokens"`
			} `json:"usage"`
		}
		out := []item{}
		for _, vk := range h.vkeyStore.List() {
			it := item{
				ID:            vk.ID,
				Name:          vk.Name,
				Key:           vk.Key,
				Enabled:       vk.Enabled,
				QuotaRequests: vk.QuotaRequests,
				QuotaTokens:   vk.QuotaTokens,
				AllowedModels: vk.AllowedModels,
				Notes:         vk.Notes,
				CreatedAt:     vk.CreatedAt,
			}
			reqs, toks := h.vkeyStore.UsageOf(vk.ID)
			it.Usage.Requests, it.Usage.Tokens = reqs, toks
			out = append(out, it)
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "data": out})
	case http.MethodPatch:
		var req struct {
			ID            string   `json:"id"`
			Name          string   `json:"name"`
			Enabled       *bool    `json:"enabled"`
			QuotaRequests *int     `json:"quota_requests"`
			QuotaTokens   *int     `json:"quota_tokens"`
			AllowedModels []string `json:"allowed_models"`
			Notes         string   `json:"notes"`
		}
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &req); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if req.ID == "" {
			http.Error(w, "missing id", http.StatusBadRequest)
			return
		}
		// 以现有记录为基准，仅覆盖传入字段
		cur := h.vkeyStore.Get(req.ID)
		if cur == nil {
			http.Error(w, "vkey not found", http.StatusNotFound)
			return
		}
		name := cur.Name
		enabled := cur.Enabled
		qr := cur.QuotaRequests
		qt := cur.QuotaTokens
		allowed := cur.AllowedModels
		notes := cur.Notes
		if req.Name != "" {
			name = req.Name
		}
		if req.Enabled != nil {
			enabled = *req.Enabled
		}
		if req.QuotaRequests != nil {
			qr = *req.QuotaRequests
		}
		if req.QuotaTokens != nil {
			qt = *req.QuotaTokens
		}
		if req.AllowedModels != nil {
			allowed = req.AllowedModels
		}
		notes = req.Notes
		if err := h.vkeyStore.Update(req.ID, name, enabled, qr, qt, allowed, notes); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	case http.MethodPost:
		var req struct {
			Name          string   `json:"name"`
			QuotaRequests int      `json:"quota_requests"`
			QuotaTokens   int      `json:"quota_tokens"`
			AllowedModels []string `json:"allowed_models"`
			Notes         string   `json:"notes"`
		}
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &req); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		vk, err := h.vkeyStore.Add(req.Name, req.QuotaRequests, req.QuotaTokens, req.AllowedModels, req.Notes)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// 明文 key 仅此响应返回一次
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "id": vk.ID, "key": vk.Key, "name": vk.Name})
	case http.MethodDelete:
		id := r.URL.Query().Get("id")
		if id == "" {
			http.Error(w, "missing id", http.StatusBadRequest)
			return
		}
		if err := h.vkeyStore.Delete(id); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// ---------- 成本/用量仪表盘 ----------

type costAgg struct {
	Requests      int      `json:"requests"`
	Prompt        int      `json:"prompt_tokens"`
	Completion    int      `json:"completion_tokens"`
	CostUSD       float64  `json:"cost_usd"`
	UnknownModels []string `json:"unknown_models,omitempty"` // F9：未收录模型（成本为 0 需人工确认）
}

func (a *costAgg) add(rec storage.UsageRecord, cost float64, unknown bool) {
	a.Requests++
	a.Prompt += rec.PromptTokens
	a.Completion += rec.CompletionTokens
	a.CostUSD += cost
	if unknown && rec.Model != "" {
		// 去重追加
		for _, m := range a.UnknownModels {
			if m == rec.Model {
				return
			}
		}
		a.UnknownModels = append(a.UnknownModels, rec.Model)
	}
}

// handleCostDashboard GET 聚合成本与用量（按 provider/model/日），并附预算状态。
// 成本 = (prompt/1e6)*PriceIn + (completion/1e6)*PriceOut，价格来自 models_catalog（分层参考库）。
func (h *AdminHandler) handleCostDashboard(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	days := 7
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 365 {
			days = n
		}
	}
	records, err := h.usage.Read(days)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	byProvider := map[string]*costAgg{}
	byModel := map[string]*costAgg{}
	byDay := map[string]*costAgg{}
	total := &costAgg{}
	get := func(m map[string]*costAgg, key string) *costAgg {
		if m[key] == nil {
			m[key] = &costAgg{}
		}
		return m[key]
	}
	for _, rec := range records {
		cost := 0.0
		unknown := false
		if h.cat != nil {
			if e := h.cat.Lookup(rec.Model); e != nil {
				cost = (float64(rec.PromptTokens)/1e6)*e.PriceIn + (float64(rec.CompletionTokens)/1e6)*e.PriceOut
			} else {
				unknown = true
				// F9：未收录模型记 unknown 并告警（仅首次日志，避免刷屏）
				log.Printf("[cost] unknown model %q for provider %q — cost=0, please add to models_catalog.json", rec.Model, rec.Provider)
			}
		}
		get(byProvider, rec.Provider).add(rec, cost, unknown)
		get(byModel, rec.Model).add(rec, cost, unknown)
		get(byDay, rec.Time.Format("2006-01-02")).add(rec, cost, unknown)
		total.add(rec, cost, unknown)
	}

	bc := h.cfg.Load().EffectiveBudget()
	dailyCost := 0.0
	if h.budgetCtl != nil {
		dailyCost = h.budgetCtl.DailyTotal()
	}
	remaining := -1.0
	if bc.DailyLimitUSD > 0 {
		remaining = bc.DailyLimitUSD - dailyCost
	}

	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":          true,
		"days":        days,
		"total":       total,
		"by_provider": byProvider,
		"by_model":    byModel,
		"by_day":      byDay,
		"budget": map[string]interface{}{
			"daily_limit_usd": bc.DailyLimitUSD,
			"action":          bc.Action,
			"daily_cost_usd":  dailyCost,
			"remaining_usd":   remaining,
		},
	})
}
