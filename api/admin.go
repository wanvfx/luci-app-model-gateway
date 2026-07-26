package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wanvfx/luci-app-model-gateway/calllog"
	"github.com/wanvfx/luci-app-model-gateway/config"
	"github.com/wanvfx/luci-app-model-gateway/engine"
	"github.com/wanvfx/luci-app-model-gateway/internal/netutil"
	"github.com/wanvfx/luci-app-model-gateway/storage"
)

// NewBypassClient 创建绕过本地/内网代理的 HTTP 客户端（供本包及 main 复用）。
// 绕过判定逻辑统一走 internal/netutil，避免各包复制粘贴后逻辑跑偏。
func NewBypassClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: &http.Transport{Proxy: netutil.BypassProxyFunc},
	}
}

// bypassClient 绕过本地/内网代理的 HTTP 客户端（与 proxy 包共用逻辑）
var bypassClient = NewBypassClient(15 * time.Second)

// bypassClientLong 长超时版本（用于模型拉取等耗时操作）
var bypassClientLong = NewBypassClient(30 * time.Second)

// MetaStoreInterface 元数据存储接口（避免循环 import）
type MetaStoreInterface interface {
	ContextLimits() map[string]int
	SupportsVision() map[string]bool
	AllModelDescriptions() map[string]map[string]string
	AllAliases() map[string]string
	SetContextLimit(model string, length int, dataDir string) error
	DeleteContextLimit(model string, dataDir string) error
}

// ModelCacheInterface 模型详情缓存接口
type ModelCacheInterface interface {
	Get(modelID string) *ModelDetailItem
}

// ModelDetailItem 模型详情缓存条目（简化引用）
type ModelDetailItem struct {
	ID          string `json:"id"`
	Object      string `json:"object"`
	Created     int64  `json:"created"`
	OwnedBy     string `json:"owned_by"`
	ContextLen  int    `json:"context_length"`
	MaxPosEmb   int    `json:"max_position_embeddings"`
	MaxModelLen int    `json:"max_model_len"`
}

// AdminHandler 管理面 API 处理器
type AdminHandler struct {
	cfg               atomic.Pointer[config.Config]
	history           *storage.History
	usage             *storage.Usage
	dataDir           string
	appDir            string
	announcementURL   string
	announcementCache struct {
		content string
		ts      time.Time
	}
	stabilityCacheMu sync.Mutex // 保护 stabilityCache 并发读写（修复 #10 data race）
	stabilityCache   struct {
		data  interface{}
		hours int
		ts    time.Time
	}
	uciTool       *UCITool
	reloadConfig  func() (*config.Config, error)
	providersPath string
	configPath    string
	meta          MetaStoreInterface
	modelCache    ModelCacheInterface
}

// NewAdminHandler 创建管理面处理器
func NewAdminHandler(cfg *config.Config, history *storage.History, usage *storage.Usage, dataDir string, appDir string, configPath string, reloadConfig func() (*config.Config, error), meta MetaStoreInterface, modelCache ModelCacheInterface) *AdminHandler {
	h := &AdminHandler{
		history:         history,
		usage:           usage,
		dataDir:         dataDir,
		appDir:          appDir,
		configPath:      configPath,
		reloadConfig:    reloadConfig,
		meta:            meta,
		modelCache:      modelCache,
		announcementURL: "https://raw.githubusercontent.com/wanvfx/luci-app-model-gateway/refs/heads/main/announcement.md",
	}

	h.cfg.Store(cfg)

	// 初始化 UCI 工具（如果 uci 命令可用）
	if IsUCIAvailable() {
		h.uciTool = NewUCITool("model-gateway")
	}

	return h
}

// SetConfig 原子更新当前配置（供 reload 同步，避免与 proxy.Server 两份配置不同步，修复 #2）
func (h *AdminHandler) SetConfig(cfg *config.Config) {
	h.cfg.Store(cfg)
}

// notifyProvidersChanged 在 provider 配置变更（新增/编辑/删除/勾选模型/一键配置）后调用：
//  1. 失效稳定性 30s 缓存，让前端尽快看到最新聚合结果；
//  2. 唤醒巡检器立即巡检并重置省电计数——否则 limited 策略休眠后，
//     新增平台的模型要等到次日 12 点才被探测，稳定性列表长期不显示新平台。
func (h *AdminHandler) notifyProvidersChanged() {
	h.stabilityCacheMu.Lock()
	h.stabilityCache.data = nil
	h.stabilityCacheMu.Unlock()
	engine.KickPoll()
}

// RegisterRoutes 注册路由
func (h *AdminHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/config", h.handleConfig)
	mux.HandleFunc("/api/providers", h.handleProviders)
	mux.HandleFunc("/api/providers/verify-key", h.handleVerifyKey)
	mux.HandleFunc("/api/providers/preset", h.handlePreset)
	mux.HandleFunc("/api/preset-info", h.handlePresetInfo)
	mux.HandleFunc("/api/providers/", h.handleProviderRoutes)
	mux.HandleFunc("/api/routers", h.handleRouters)
	mux.HandleFunc("/api/history", h.handleHistory)
	mux.HandleFunc("/api/usage", h.handleUsage)
	mux.HandleFunc("/api/announcement", h.handleAnnouncement)
	mux.HandleFunc("/api/stability", h.handleStability)
	mux.HandleFunc("/api/model-details", h.handleModelDetails)
	mux.HandleFunc("/api/context-limits", h.handleContextLimits)
	mux.HandleFunc("/api/context-limits/", h.handleContextLimits)
	mux.HandleFunc("/api/vision-models", h.handleVisionModels)
	mux.HandleFunc("/api/vision-assist", h.handleVisionAssist)
	mux.HandleFunc("/api/poll-status", h.handlePollStatus)
	mux.HandleFunc("/api/poll-strategy", h.handlePollStrategy)
	mux.HandleFunc("/api/check/", h.handleCheck)
	mux.HandleFunc("/api/check-update", h.handleCheckUpdate)
	mux.HandleFunc("/api/call-log", h.handleCallLog)
	mux.HandleFunc("/api/open-url", h.handleOpenURL)
	mux.HandleFunc("/api/start-download", h.handleStartDownload)
	mux.HandleFunc("/api/download-progress", h.handleDownloadProgress)
	mux.HandleFunc("/api/apply-update", h.handleApplyUpdate)
}

// sameOrigin 判断请求是否来自本网关自带的 Web 面板（同源）。
// 面板由本服务同源提供，浏览器 fetch 会带上与请求 host 一致的 Origin/Referer；
// 而局域网内匿名 curl（无 Origin）或跨站页面（Origin 不符，且被浏览器 CORS 拦截）不会拿到完整密钥。
// 据此在匿名 GET /api/config 中仅对同源请求返回完整 admin_key，避免密钥被随意读取（修复 #1）。
func sameOrigin(r *http.Request) bool {
	host := r.Host // 例如 192.168.1.1:12211
	if origin := r.Header.Get("Origin"); origin != "" {
		if u, err := url.Parse(origin); err == nil && u.Host == host {
			return true
		}
		return false
	}
	if referer := r.Header.Get("Referer"); referer != "" {
		if u, err := url.Parse(referer); err == nil && u.Host == host {
			return true
		}
	}
	// 既无 Origin 也无 Referer（如直接 curl）：视为匿名，不返回完整密钥
	return false
}

// handleConfig 处理 /api/config
func (h *AdminHandler) handleConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	switch r.Method {
	case http.MethodGet:
		// 安全：仅在同源（本面板）请求时返回完整 admin_key，避免局域网内匿名读取密钥（修复 #1）。
		// 跨站或直接 curl 等无 Origin / Origin 不符的请求，仅返回掩码，密钥不落地。
		adminKey := "sk-local-****"
		if sameOrigin(r) {
			adminKey = h.cfg.Load().AdminKey()
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"port":            h.cfg.Load().Port(),
			"admin_key":       adminKey,
			"poll_interval":   h.cfg.Load().PollInterval(),
			"poll_strategy":   h.cfg.Load().PollStrategy(),
			"headless":        h.cfg.Load().Headless(),
			"vision_router":   h.cfg.Load().VisionRouter(),
			"disabled_models": h.cfg.Load().DisabledModels(),
		})
	case http.MethodPost:
		h.handleConfigPost(w, r)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleConfigPost 持久化配置到 UCI
func (h *AdminHandler) handleConfigPost(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Port           int      `json:"port"`
		AdminKey       string   `json:"admin_key"`
		PollInterval   int      `json:"poll_interval"`
		Headless       bool     `json:"headless"`
		VisionRouter   string   `json:"vision_router"`
		DisabledModels []string `json:"disabled_models"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid request: %v", err), http.StatusBadRequest)
		return
	}

	if h.uciTool == nil {
		http.Error(w, "uci not available", http.StatusNotImplemented)
		return
	}

	// 写入 settings section
	if err := h.uciTool.SetOptionWithCommit("model-gateway", "settings", "port", fmt.Sprintf("%d", req.Port)); err != nil {
		http.Error(w, fmt.Sprintf("set port failed: %v", err), http.StatusInternalServerError)
		return
	}
	if req.AdminKey != "" && req.AdminKey != h.cfg.Load().AdminKey() {
		if err := h.uciTool.SetOptionWithCommit("model-gateway", "settings", "admin_key", req.AdminKey); err != nil {
			http.Error(w, fmt.Sprintf("set admin_key failed: %v", err), http.StatusInternalServerError)
			return
		}
	}
	if req.PollInterval > 0 {
		if err := h.uciTool.SetOptionWithCommit("model-gateway", "settings", "poll_interval", fmt.Sprintf("%d", req.PollInterval)); err != nil {
			http.Error(w, fmt.Sprintf("set poll_interval failed: %v", err), http.StatusInternalServerError)
			return
		}
	}
	if err := h.uciTool.SetOptionWithCommit("model-gateway", "settings", "headless", fmt.Sprintf("%d", map[bool]int{true: 1, false: 0}[req.Headless])); err != nil {
		http.Error(w, fmt.Sprintf("set headless failed: %v", err), http.StatusInternalServerError)
		return
	}
	if req.VisionRouter != "" {
		if err := h.uciTool.SetOptionWithCommit("model-gateway", "settings", "vision_router", req.VisionRouter); err != nil {
			http.Error(w, fmt.Sprintf("set vision_router failed: %v", err), http.StatusInternalServerError)
			return
		}
	}

	// 处理 disabled_models list
	if err := h.uciTool.SetList("model-gateway", "settings", "disabled_models", req.DisabledModels); err != nil {
		http.Error(w, fmt.Sprintf("set disabled_models failed: %v", err), http.StatusInternalServerError)
		return
	}

	// 热重载配置
	if h.reloadConfig != nil {
		if newCfg, err := h.reloadConfig(); err == nil {
			h.cfg.Store(newCfg)
		}
	}

	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// handleProviders 处理 /api/providers
func (h *AdminHandler) handleProviders(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	switch r.Method {
	case http.MethodGet:
		h.handleProvidersGet(w, r)
	case http.MethodPost:
		h.handleProvidersPost(w, r)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleProvidersGet 获取提供商列表（含健康状态和脱敏 Key）
func (h *AdminHandler) handleProvidersGet(w http.ResponseWriter, r *http.Request) {
	// 从巡检历史（最近 1 小时）聚合每个 provider 的最新健康状态
	// 记录 Model 格式为 "provider||model"（poll.go 写入）
	latestHealth := map[string]map[string]interface{}{}
	if h.history != nil {
		if records, err := h.history.Read(1); err == nil {
			for _, rec := range records {
				providerName := rec.Model
				if parts := strings.SplitN(rec.Model, "||", 2); len(parts) == 2 {
					providerName = parts[0]
				}
				cur, exists := latestHealth[providerName]
				// 保留时间最新的一条
				if exists {
					if t, ok := cur["time"].(time.Time); ok && !rec.Time.After(t) {
						continue
					}
				}
				status := "ok"
				if !rec.OK {
					status = "fail"
				}
				latestHealth[providerName] = map[string]interface{}{
					"status":     status,
					"latency_ms": rec.Latency,
					"error":      rec.Error,
					"time":       rec.Time,
				}
			}
		}
	}

	providers := make([]map[string]interface{}, 0, len(h.cfg.Load().Providers))
	for _, p := range h.cfg.Load().Providers {
		health := map[string]interface{}{}
		if hInfo, ok := latestHealth[p.Name]; ok {
			health = hInfo
		}

		providers = append(providers, map[string]interface{}{
			"name":            p.Name,
			"base_url":        p.BaseURL,
			"api_key_masked":  maskKey(p.APIKey),
			"models":          p.Models,
			"disabled_models": p.DisabledModels,
			"free_only":       p.FreeOnly,
			"enabled":         p.Enabled,
			"health":          health,
		})
	}
	_ = json.NewEncoder(w).Encode(providers)
}

// handleProvidersPost 新增提供商（持久化到 UCI）
func (h *AdminHandler) handleProvidersPost(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name    string   `json:"name"`
		BaseURL string   `json:"base_url"`
		APIKey  string   `json:"api_key"`
		Models  []string `json:"models"`
		Enabled bool     `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid request: %v", err), http.StatusBadRequest)
		return
	}

	if h.uciTool == nil {
		http.Error(w, "uci not available", http.StatusNotImplemented)
		return
	}

	// 检查名称是否合法
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}

	// 原子地新增 provider section 并设置 option / models list
	// 新增提供商默认启用（前端不传 enabled，且新提供商应立即可用，复刻 Python model-gateway 1.6.1 行为）
	options := map[string]string{
		"name":     req.Name,
		"base_url": req.BaseURL,
		"api_key":  req.APIKey,
		"enabled":  "1",
	}
	lists := map[string][]string{}
	if len(req.Models) > 0 {
		lists["models"] = req.Models
	} else if req.BaseURL != "" && req.APIKey != "" {
		// 复刻 Python 1.6.1 add_provider：添加提供商时自动拉取上游模型并全部选中（默认全选），
		// 使输出模型 / 路由配置 / 识图 / 巡检扫描立即生效，无需手动到“模型管理”逐个勾选。
		if fetched, ferr := fetchModelsFromUpstream(req.BaseURL, req.APIKey, true); ferr == nil && len(fetched) > 0 {
			lists["models"] = fetched
			log.Printf("auto-selected %d models for new provider %q", len(fetched), req.Name)
		}
	}
	if err := h.uciTool.AddSectionWithOptions("provider", options, lists); err != nil {
		http.Error(w, fmt.Sprintf("add provider failed: %v", err), http.StatusInternalServerError)
		return
	}

	// 热重载配置
	if h.reloadConfig != nil {
		if newCfg, err := h.reloadConfig(); err == nil {
			h.cfg.Store(newCfg)
		}
	}

	h.notifyProvidersChanged()

	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "name": req.Name})
}

// handleRouters 处理 /api/routers
func (h *AdminHandler) handleRouters(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	switch r.Method {
	case http.MethodGet:
		routers := make(map[string][]string)
		for _, r := range h.cfg.Load().Routers {
			routers[r.Name] = r.Members
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "data": routers})
	case http.MethodPost:
		h.handleRoutersPost(w, r)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleRoutersPost 持久化路由组到 UCI
func (h *AdminHandler) handleRoutersPost(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("read body failed: %v", err), http.StatusBadRequest)
		return
	}

	// 支持两种格式：数组 [{name, members}, ...] 或对象 {name: [members], ...}
	var req []map[string]interface{}
	if err := json.Unmarshal(body, &req); err != nil {
		var objReq map[string]interface{}
		if err2 := json.Unmarshal(body, &objReq); err2 != nil {
			http.Error(w, fmt.Sprintf("invalid request: %v", err), http.StatusBadRequest)
			return
		}
		req = make([]map[string]interface{}, 0, len(objReq))
		for name, membersInterface := range objReq {
			members, _ := membersInterface.([]interface{})
			memberStrs := make([]string, 0, len(members))
			for _, m := range members {
				if s, ok := m.(string); ok {
					memberStrs = append(memberStrs, s)
				}
			}
			membersIfaces := make([]interface{}, len(memberStrs))
			for i, s := range memberStrs {
				membersIfaces[i] = s
			}
			req = append(req, map[string]interface{}{"name": name, "members": membersIfaces})
		}
	}

	if h.uciTool == nil {
		http.Error(w, "uci not available", http.StatusNotImplemented)
		return
	}

	// GetSectionNames 返回 UCI 段标识（如 @router[0] / cfg0a1b2c），
	// 不是 option name 的值。需要通过 GetOptions 读取 name 字段建立映射。
	existingSectionIDs, _ := h.uciTool.GetSectionNames("router")
	type routerSection struct {
		id   string // UCI section ID (可用于 ref/delete)
		name string // option name 值
	}
	existingBySectionID := make(map[string]string)   // sectionID → name
	existingByName := make(map[string]routerSection) // name → section
	for _, sid := range existingSectionIDs {
		opts, _ := h.uciTool.GetOptions("router", sid)
		rname := opts["name"]
		existingBySectionID[sid] = rname
		if rname != "" {
			existingByName[rname] = routerSection{id: sid, name: rname}
		}
	}

	// 构建新路由组名称集合
	newNames := make(map[string]bool)
	for _, router := range req {
		name, _ := router["name"].(string)
		if name != "" {
			newNames[name] = true
		}
	}

	// 删除不在新列表中的旧 router（收集待删 ids，批量原子删除避免索引漂移）
	var toDeleteIDs []string
	for sid, rname := range existingBySectionID {
		if rname == "" || !newNames[rname] {
			toDeleteIDs = append(toDeleteIDs, sid)
		}
	}
	if len(toDeleteIDs) > 0 {
		_ = h.uciTool.DeleteSections(toDeleteIDs)
	}

	// 创建/更新路由组
	for _, router := range req {
		name, _ := router["name"].(string)
		members, _ := router["members"].([]interface{})
		if name == "" {
			continue
		}

		// 转换为字符串切片
		memberStrs := make([]string, 0, len(members))
		for _, m := range members {
			if s, ok := m.(string); ok {
				memberStrs = append(memberStrs, s)
			}
		}

		var secID string
		if existing, ok := existingByName[name]; ok {
			secID = existing.id
		} else {
			// 创建新 section
			var err error
			secID, err = h.uciTool.AddSection("router")
			if err != nil {
				http.Error(w, fmt.Sprintf("add router failed: %v", err), http.StatusInternalServerError)
				return
			}
			if err := h.uciTool.SetOptionWithCommit("router", secID, "name", name); err != nil {
				http.Error(w, fmt.Sprintf("set router name failed: %v", err), http.StatusInternalServerError)
				return
			}
		}

		// 更新 members list（用 section ID 引用 UCI）
		if err := h.uciTool.SetList("router", secID, "members", memberStrs); err != nil {
			http.Error(w, fmt.Sprintf("set router members failed: %v", err), http.StatusInternalServerError)
			return
		}
	}

	// 热重载配置
	if h.reloadConfig != nil {
		if newCfg, err := h.reloadConfig(); err == nil {
			h.cfg.Store(newCfg)
		}
	}

	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// handleHistory 处理 /api/history?hours=24
func (h *AdminHandler) handleHistory(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")

	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	hours := 24
	if s := r.URL.Query().Get("hours"); s != "" {
		fmt.Sscanf(s, "%d", &hours)
		if hours <= 0 {
			hours = 24
		}
		if hours > 720 {
			hours = 720
		}
	}

	records, err := h.history.Read(hours)
	if err != nil {
		http.Error(w, fmt.Sprintf("read history failed: %v", err), http.StatusInternalServerError)
		return
	}

	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"hours":   hours,
		"records": records,
	})
}

// handleUsage 处理 /api/usage?days=1
func (h *AdminHandler) handleUsage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")

	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	days := 1
	if s := r.URL.Query().Get("days"); s != "" {
		fmt.Sscanf(s, "%d", &days)
		if days <= 0 {
			days = 1
		}
		if days > 30 {
			days = 30
		}
	}

	records, err := h.usage.Read(days)
	if err != nil {
		http.Error(w, fmt.Sprintf("read usage failed: %v", err), http.StatusInternalServerError)
		return
	}

	total := map[string]int64{"pt": 0, "ct": 0, "tt": 0, "requests": 0}
	byDay := map[string]map[string]int64{}
	byModel := map[string]map[string]int64{}

	for _, rec := range records {
		pt := int64(rec.PromptTokens)
		ct := int64(rec.CompletionTokens)
		tt := pt + ct
		total["pt"] += pt
		total["ct"] += ct
		total["tt"] += tt
		total["requests"]++

		day := rec.Time.Format("2006-01-02")
		d := byDay[day]
		if d == nil {
			d = map[string]int64{"pt": 0, "ct": 0, "tt": 0, "requests": 0}
			byDay[day] = d
		}
		d["pt"] += pt
		d["ct"] += ct
		d["tt"] += tt
		d["requests"]++

		mk := rec.Provider + " · " + rec.Model
		m := byModel[mk]
		if m == nil {
			m = map[string]int64{"pt": 0, "ct": 0, "tt": 0, "requests": 0, "provider": 0, "model": 0}
			byModel[mk] = m
		}
		m["pt"] += pt
		m["ct"] += ct
		m["tt"] += tt
		m["requests"]++
	}

	type dayRow struct {
		Date     string `json:"date"`
		Pt       int64  `json:"pt"`
		Ct       int64  `json:"ct"`
		Tt       int64  `json:"tt"`
		Requests int64  `json:"requests"`
	}
	type modelRow struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
		Pt       int64  `json:"pt"`
		Ct       int64  `json:"ct"`
		Tt       int64  `json:"tt"`
		Requests int64  `json:"requests"`
	}

	dayList := make([]dayRow, 0, len(byDay))
	for d, v := range byDay {
		dayList = append(dayList, dayRow{Date: d, Pt: v["pt"], Ct: v["ct"], Tt: v["tt"], Requests: v["requests"]})
	}
	// 按日期排序
	sort.Slice(dayList, func(i, j int) bool {
		return dayList[i].Date < dayList[j].Date
	})

	modelList := make([]modelRow, 0, len(byModel))
	for mk, v := range byModel {
		parts := strings.SplitN(mk, " · ", 2)
		prov := ""
		mod := mk
		if len(parts) == 2 {
			prov = parts[0]
			mod = parts[1]
		}
		modelList = append(modelList, modelRow{Provider: prov, Model: mod, Pt: v["pt"], Ct: v["ct"], Tt: v["tt"], Requests: v["requests"]})
	}
	// 按总 token 降序排序
	sort.Slice(modelList, func(i, j int) bool {
		return modelList[i].Tt > modelList[j].Tt
	})

	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"days":     days,
		"total":    total,
		"by_day":   dayList,
		"by_model": modelList,
	})
}

// handleAnnouncement 处理 /api/announcement
func (h *AdminHandler) handleAnnouncement(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")

	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 优先读本地 announcement.json
	localPath := filepath.Join(h.dataDir, "announcement.json")
	if data, err := os.ReadFile(localPath); err == nil {
		var obj map[string]interface{}
		if json.Unmarshal(data, &obj) == nil {
			if content, ok := obj["content"].(string); ok && strings.TrimSpace(content) != "" {
				h := sha256.Sum256([]byte(content))
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"ok":      true,
					"content": content,
					"hash":    fmt.Sprintf("%x", h[:]),
				})
				return
			}
		}
	}

	// 远程拉取
	if h.announcementURL != "" {
		announceClient := &http.Client{Timeout: 10 * time.Second}
		req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, h.announcementURL, nil)
		resp, err := announceClient.Do(req)
		if err == nil && resp.StatusCode == http.StatusOK {
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			content := strings.TrimSpace(string(b))
			if content != "" {
				h.announcementCache.content = content
				h.announcementCache.ts = time.Now()
				// 持久化到本地缓存文件
				if err := os.WriteFile(localPath, []byte(`{"content":`+jsonString(content)+`}`), 0644); err != nil {
					log.Printf("write announcement cache failed: %v", err)
				}
				hash := sha256.Sum256([]byte(content))
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"ok":      true,
					"content": content,
					"hash":    fmt.Sprintf("%x", hash[:]),
				})
				return
			}
		}
	}

	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":      false,
		"content": "",
	})
}

// jsonString 辅助函数：将字符串转义为 JSON 字符串字面量
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// handleStability 处理 /api/stability?hours=24
func (h *AdminHandler) handleStability(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")

	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	hours := 24
	if s := r.URL.Query().Get("hours"); s != "" {
		fmt.Sscanf(s, "%d", &hours)
		if hours <= 0 {
			hours = 24
		}
		if hours > 720 {
			hours = 720
		}
	}

	// 缓存逻辑（与 Python 原版 STABILITY_CACHE_TTL = 30 一致）；加锁防并发 data race
	h.stabilityCacheMu.Lock()
	if h.stabilityCache.data != nil && h.stabilityCache.hours == hours && time.Since(h.stabilityCache.ts) < 30*time.Second {
		cached := h.stabilityCache.data
		h.stabilityCacheMu.Unlock()
		_ = json.NewEncoder(w).Encode(cached)
		return
	}
	h.stabilityCacheMu.Unlock()

	records, err := h.history.Read(hours)
	if err != nil {
		http.Error(w, fmt.Sprintf("read history failed: %v", err), http.StatusInternalServerError)
		return
	}

	type modelStat struct {
		Provider     string  `json:"provider"`
		Model        string  `json:"model"`
		Checks       int     `json:"checks"`
		Ok           int     `json:"ok"`
		Fail         int     `json:"fail"`
		Error        int     `json:"error"`
		Availability float64 `json:"availability"`
		AvgLatencyMs *int    `json:"avg_latency_ms"`
		MinLatencyMs *int    `json:"min_latency_ms"`
		MaxLatencyMs *int    `json:"max_latency_ms"`
		LastStatus   string  `json:"last_status"`
		Vision       bool    `json:"vision"`
	}

	stats := map[string]*modelStat{}
	for _, rec := range records {
		// rec.Model 格式为 "provider||model"（由 poll.go 写入）
		key := rec.Model
		provider := ""
		model := key
		if parts := strings.SplitN(key, "||", 2); len(parts) == 2 {
			provider = parts[0]
			model = parts[1]
		}

		st, ok := stats[key]
		if !ok {
			st = &modelStat{
				Provider:   provider,
				Model:      model,
				LastStatus: "unknown",
			}
			stats[key] = st
		}
		st.Checks++
		if rec.OK {
			st.Ok++
		} else {
			st.Fail++
		}
		if rec.Error != "" {
			st.Error++
		}
		if rec.Latency > 0 {
			lat := int(rec.Latency)
			if st.MinLatencyMs == nil || lat < *st.MinLatencyMs {
				v := lat
				st.MinLatencyMs = &v
			}
			if st.MaxLatencyMs == nil || lat > *st.MaxLatencyMs {
				v := lat
				st.MaxLatencyMs = &v
			}
			if st.AvgLatencyMs == nil {
				v := lat
				st.AvgLatencyMs = &v
			} else {
				v := *st.AvgLatencyMs + lat
				st.AvgLatencyMs = &v
			}
		}
		st.LastStatus = "ok"
		if !rec.OK {
			st.LastStatus = "fail"
		}
	}

	result := make([]modelStat, 0, len(stats))
	for _, st := range stats {
		if st.Checks > 0 {
			st.Availability = float64(st.Ok) / float64(st.Checks) * 100
		}
		if st.AvgLatencyMs != nil && st.Checks > 0 {
			v := *st.AvgLatencyMs / st.Checks
			st.AvgLatencyMs = &v
		}
		result = append(result, *st)
	}

	// 按可用率降序、平均延迟升序排序（与原 Python 版一致）
	sort.Slice(result, func(i, j int) bool {
		if result[i].Availability != result[j].Availability {
			return result[i].Availability > result[j].Availability
		}
		if result[i].AvgLatencyMs == nil && result[j].AvgLatencyMs == nil {
			return false
		}
		if result[i].AvgLatencyMs == nil {
			return false
		}
		if result[j].AvgLatencyMs == nil {
			return true
		}
		return *result[i].AvgLatencyMs < *result[j].AvgLatencyMs
	})

	// 写入缓存（加锁）
	h.stabilityCacheMu.Lock()
	h.stabilityCache.data = result
	h.stabilityCache.hours = hours
	h.stabilityCache.ts = time.Now()
	h.stabilityCacheMu.Unlock()

	_ = json.NewEncoder(w).Encode(result)
}

// handleVerifyKey 校验上游 API Key 有效性
func (h *AdminHandler) handleVerifyKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		BaseURL string `json:"base_url"`
		APIKey  string `json:"api_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid request: %v", err), http.StatusBadRequest)
		return
	}

	client := bypassClient
	url := strings.TrimRight(req.BaseURL, "/") + "/models"
	reqHttp, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, url, nil)
	reqHttp.Header.Set("Authorization", "Bearer "+req.APIKey)
	resp, err := client.Do(reqHttp)
	if err != nil {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "detail": err.Error()})
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "detail": "连接成功"})
	} else {
		b, _ := io.ReadAll(resp.Body)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "detail": fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(b)[:min(len(b), 200)])})
	}
}

// verifyProviderKey 校验上游 API Key 是否有效
func (h *AdminHandler) verifyProviderKey(baseURL, apiKey string) (bool, error) {
	client := bypassClient
	url := strings.TrimRight(baseURL, "/") + "/models"
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK, nil
}

// fetchModelsFromUpstream 从上游拉取模型列表
func fetchModelsFromUpstream(baseURL, apiKey string, freeOnly bool) ([]string, error) {
	client := bypassClientLong
	url := strings.TrimRight(baseURL, "/") + "/models"
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upstream returned %d", resp.StatusCode)
	}
	var result struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	models := make([]string, 0, len(result.Data))
	for _, m := range result.Data {
		models = append(models, m.ID)
	}
	return models, nil
}

// handlePresetInfo 获取预设配置清单
func (h *AdminHandler) handlePresetInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "application/json")

	// 优先读取内置 presets.json（兼容开发环境和软路由安装路径）
	presetPaths := []string{
		filepath.Join(h.appDir, "presets.json"),
		filepath.Join(h.appDir, "..", "share", "model-gateway", "presets.json"),
	}
	var presetData []byte
	for _, path := range presetPaths {
		if data, err := os.ReadFile(path); err == nil {
			presetData = data
			break
		}
	}
	if len(presetData) > 0 {
		_, _ = w.Write(presetData)
		return
	}

	// 内置兜底预设
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"version": "2026-07-21",
		"platforms": map[string]interface{}{
			"NVIDIA": map[string]interface{}{
				"base_url":       "https://integrate.api.nvidia.com/v1",
				"free_only":      true,
				"key_page_url":   "https://build.nvidia.com/",
				"auth_hint":      "需绑定手机号",
				"models_visible": []string{"deepseek-ai/deepseek-v4-flash", "deepseek-ai/deepseek-v4-pro", "minimaxai/minimax-m3", "mistralai/mistral-large-3-675b-instruct-2512", "mistralai/mistral-small-4-119b-2603", "nvidia/nemotron-3-super-120b-a12b", "nvidia/nemotron-3-ultra-550b-a55b", "qwen/qwen3.5-122b-a10b", "z-ai/glm-5.2", "meta/llama-3.2-11b-vision-instruct", "nvidia/nemotron-nano-12b-v2-vl", "nvidia/llama-3.1-nemotron-nano-vl-8b-v1"},
			},
			"SenseNova": map[string]interface{}{
				"base_url":       "https://token.sensenova.cn/v1",
				"free_only":      false,
				"key_page_url":   "https://platform.sensenova.cn/console/keys",
				"auth_hint":      "手机号注册登录即可",
				"models_visible": []string{"deepseek-v4-flash", "glm-5.2", "sensenova-6.7-flash-lite"},
			},
			"魔搭": map[string]interface{}{
				"base_url":       "https://api-inference.modelscope.cn/v1",
				"free_only":      false,
				"key_page_url":   "https://modelscope.cn/my/myaccesstoken",
				"auth_hint":      "需绑定阿里云账号（支付宝实名）",
				"models_visible": []string{"Qwen/Qwen3.5-122B-A10B", "Qwen/Qwen3.5-397B-A17B", "deepseek-ai/DeepSeek-V4-Flash", "deepseek-ai/DeepSeek-V4-Pro", "OpenGVLab/InternVL3_5-241B-A28B", "Qwen/Qwen3-VL-8B-Thinking", "Qwen/Qwen3-VL-8B-Instruct", "PaddlePaddle/ERNIE-4.5-VL-28B-A3B-PT"},
			},
		},
		"routers": map[string]interface{}{
			"256k": []string{"mistralai/mistral-large-3-675b-instruct-2512", "mistralai/mistral-small-4-119b-2603", "nvidia/nemotron-3-super-120b-a12b", "nvidia/nemotron-3-ultra-550b-a55b", "qwen/qwen3.5-122b-a10b", "sensenova-6.7-flash-lite", "Qwen/Qwen3.5-122B-A10B", "Qwen/Qwen3.5-397B-A17B"},
			"1m":   []string{"deepseek-ai/deepseek-v4-pro", "minimaxai/minimax-m3", "z-ai/glm-5.2", "glm-5.2", "deepseek-ai/DeepSeek-V4-Pro", "deepseek-ai/DeepSeek-V4-Flash", "deepseek-v4-flash", "deepseek-ai/DeepSeek-V4-Flash"},
			"识图":   []string{"sensenova-6.7-flash-lite", "mistralai/mistral-large-3-675b-instruct-2512", "mistralai/mistral-small-4-119b-2603", "meta/llama-3.2-11b-vision-instruct", "nvidia/nemotron-nano-12b-v2-vl", "nvidia/llama-3.1-nemotron-nano-vl-8b-v1", "OpenGVLab/InternVL3_5-241B-A28B", "Qwen/Qwen3-VL-8B-Thinking", "Qwen/Qwen3-VL-8B-Instruct", "PaddlePaddle/ERNIE-4.5-VL-28B-A3B-PT"},
		},
	})
}

// handleCallLog 返回最近调用记录（与 Python 原版 1.6.1 /api/call-log 等价）
func (h *AdminHandler) handleCallLog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(calllog.List())
}

// handlePreset 一键应用预设
func (h *AdminHandler) handlePreset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if h.uciTool == nil {
		http.Error(w, "uci not available", http.StatusNotImplemented)
		return
	}

	// ---- 一键配置断电保护（与 Python 原版 1.6.1 一致，修复 #4 断电恢复冲突）----
	// 用 sentinel 文件标记“一键配置进行中”：仅当被中断（进程被杀/断电）时 sentinel 才会残留，
	// 正常完成会被删除。启动时 recoverPresetBackup 仅当 sentinel + 备份同时存在才还原，
	// 否则视为上次已正常完成，丢弃残留备份，避免“部分生效配置”被静默保留。
	backupPath := h.configPath + ".preset_bak"
	inProgressPath := h.configPath + ".preset_in_progress"
	_ = os.Remove(backupPath)     // 清理理论不该存在的旧备份
	_ = os.Remove(inProgressPath) // 清理理论不该存在的旧标记
	if data, berr := os.ReadFile(h.configPath); berr == nil {
		if werr := os.WriteFile(backupPath, data, 0644); werr != nil {
			log.Printf("preset: backup config failed: %v", werr) // 备份失败不阻塞主流程
		}
	}
	// 标记“进行中”：必须在真正写配置之前落盘；若此处之后被强杀，sentinel+备份均存在 → 启动还原
	_ = os.WriteFile(inProgressPath, []byte("1"), 0644)
	defer func() {
		if r := recover(); r != nil {
			log.Printf("preset panic recovered: %v", r)
			if bdata, berr := os.ReadFile(backupPath); berr == nil {
				_ = os.WriteFile(h.configPath, bdata, 0644)
			}
			_ = os.Remove(backupPath)
			_ = os.Remove(inProgressPath)
			// 返回 500 而非重新 panic 崩溃整个进程（全局 recoverMiddleware 也会兜底）
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "detail": fmt.Sprintf("preset failed: %v", r)})
			return
		}
		// 正常完成：清理备份与进行中标记
		_ = os.Remove(backupPath)
		_ = os.Remove(inProgressPath)
	}()

	// 解析请求体：{keys: {platformName: apiKey}}
	var req struct {
		Keys map[string]string `json:"keys"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid request: %v", err), http.StatusBadRequest)
		return
	}

	// 读取预设配置
	presetPaths := []string{
		filepath.Join(h.appDir, "presets.json"),
		filepath.Join(h.appDir, "..", "share", "model-gateway", "presets.json"),
	}
	var presetData []byte
	for _, path := range presetPaths {
		if data, err := os.ReadFile(path); err == nil {
			presetData = data
			break
		}
	}
	if len(presetData) == 0 {
		http.Error(w, "read presets failed: presets.json not found", http.StatusInternalServerError)
		return
	}

	var presets struct {
		Platforms map[string]struct {
			BaseURL       string   `json:"base_url"`
			FreeOnly      bool     `json:"free_only"`
			KeyPageURL    string   `json:"key_page_url"`
			AuthHint      string   `json:"auth_hint"`
			ModelsVisible []string `json:"models_visible"`
		} `json:"platforms"`
		Routers map[string][]string `json:"routers"`
	}
	if err := json.Unmarshal(presetData, &presets); err != nil {
		http.Error(w, fmt.Sprintf("parse presets failed: %v", err), http.StatusInternalServerError)
		return
	}

	results := make(map[string]interface{})
	createdNames := []string{}

	// 获取现有 provider 名称集合
	existingNames := make(map[string]bool)
	if sectionNames, err := h.uciTool.GetSectionNames("provider"); err == nil {
		for _, sn := range sectionNames {
			opts, err := h.uciTool.GetOptions("provider", sn)
			if err != nil {
				continue
			}
			if name, ok := opts["name"]; ok {
				existingNames[name] = true
			}
		}
	}

	for platName, platCfg := range presets.Platforms {
		key := strings.TrimSpace(req.Keys[platName])
		if key == "" {
			// 没填 Key：如果已有同名 provider，复用已有的 key 继续执行（重新拉模型）
			// 确保预设新增的模型能生效，与原 Python 版行为一致
			if existingNames[platName] {
				// 查找已有 provider 的 api_key
				sectionNames, _ := h.uciTool.GetSectionNames("provider")
				for _, sn := range sectionNames {
					opts, err := h.uciTool.GetOptions("provider", sn)
					if err != nil {
						continue
					}
					if opts["name"] == platName && opts["api_key"] != "" {
						key = opts["api_key"]
						break
					}
				}
				if key == "" {
					results[platName] = map[string]interface{}{"ok": true, "detail": "保留已有配置（无可用 Key）"}
					continue
				}
				// 继续往下执行（复用已有 key 重新拉模型）
			} else {
				results[platName] = map[string]interface{}{"ok": false, "detail": "未填写 Key"}
				continue
			}
		}

		// 校验 Key
		if ok, _ := h.verifyProviderKey(platCfg.BaseURL, key); !ok {
			results[platName] = map[string]interface{}{"ok": false, "detail": "Key 校验失败"}
			continue
		}

		// 同名则先移除旧配置（包括可能的历史重复 section）
		if existingNames[platName] {
			if _, delErr := h.uciTool.DeleteSectionsByName("provider", platName); delErr != nil {
				log.Printf("preset: delete old provider %s failed: %v", platName, delErr)
			}
		}

		// 拉模型（失败不阻断：与原 Python 版一致，provider 必须创建，模型列表退化为空或预设可见列表）
		models, _ := fetchModelsFromUpstream(platCfg.BaseURL, key, platCfg.FreeOnly)

		// 按预设 models_visible 收敛模型列表（与原 Python 版 apply_preset 一致）
		if visible := platCfg.ModelsVisible; len(visible) > 0 {
			visibleSet := make(map[string]bool, len(visible))
			inFiltered := make(map[string]bool)
			filtered := make([]string, 0, len(visible))
			for _, m := range visible {
				visibleSet[m] = true
			}
			for _, m := range models {
				if visibleSet[m] && !inFiltered[m] {
					filtered = append(filtered, m)
					inFiltered[m] = true
				}
			}
			// 保证预设里显式列出的模型都在（即使上游没返回）
			for _, m := range visible {
				if !inFiltered[m] {
					filtered = append(filtered, m)
					inFiltered[m] = true
				}
			}
			models = filtered
		}

		// 原子地新增 provider section 并设置 option / models list
		options := map[string]string{
			"name":     platName,
			"base_url": platCfg.BaseURL,
			"api_key":  key,
			"enabled":  "1",
		}
		if platCfg.FreeOnly {
			options["free_only"] = "1"
		}
		lists := map[string][]string{}
		if len(models) > 0 {
			lists["models"] = models
		}
		if err := h.uciTool.AddSectionWithOptions("provider", options, lists); err != nil {
			results[platName] = map[string]interface{}{"ok": false, "detail": fmt.Sprintf("设置 provider 失败: %v", err)}
			continue
		}

		createdNames = append(createdNames, platName)
		results[platName] = map[string]interface{}{"ok": true, "detail": fmt.Sprintf("已配置 %d 个模型", len(models))}
	}

	// 应用预设路由组（预设有的组完全替换，预设没的组保留不动）
	if len(createdNames) > 0 {
		for gname, members := range presets.Routers {
			memberStrs := make([]string, 0, len(members))
			for _, m := range members {
				memberStrs = append(memberStrs, m)
			}
			// 删除同名旧路由组（批量原子删除，避免索引漂移）
			if _, delErr := h.uciTool.DeleteSectionsByName("router", gname); delErr != nil {
				log.Printf("preset: delete old router %s failed: %v", gname, delErr)
			}
			// 创建新路由组
			sectionName, err := h.uciTool.AddSection("router")
			if err != nil {
				continue
			}
			_ = h.uciTool.SetOptionWithCommit("router", sectionName, "name", gname)
			_ = h.uciTool.SetList("router", sectionName, "members", memberStrs)
		}
	}

	// 写盘成功：立即删除备份，缩小"成功却被强杀→误回滚"的窗口
	_ = os.Remove(backupPath)

	// 热重载配置
	if h.reloadConfig != nil {
		if newCfg, err := h.reloadConfig(); err == nil {
			h.cfg.Store(newCfg)
		}
	}

	h.notifyProvidersChanged()

	_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "results": results, "created": createdNames})
}

// handleModelDetails 获取模型详情（合并上游探测 + 元数据）
func (h *AdminHandler) handleModelDetails(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	merged := map[string]map[string]interface{}{}

	// 1. 从 modelCache 获取上游探测结果（键为上游模型 id / 原始名）
	if h.modelCache != nil {
		// 遍历所有 provider 的模型，用模型原始 ID 查询缓存
		for _, p := range h.cfg.Load().Providers {
			if p == nil || !p.Enabled {
				continue
			}
			for _, m := range p.Models {
				// 直接用模型名查询（上游返回的 ID 与 provider.Models 中的名称一致）
				detail := h.modelCache.Get(m)
				entry := map[string]interface{}{}
				if detail != nil {
					if detail.ContextLen > 0 {
						entry["context_length"] = detail.ContextLen
					}
				}
				merged[m] = entry
			}
		}
	}

	// 2. 用 meta 别名归一化 + 模型描述兜底
	if h.meta != nil {
		aliases := h.meta.AllAliases()
		descriptions := h.meta.AllModelDescriptions()
		limits := h.meta.ContextLimits()

		for _, p := range h.cfg.Load().Providers {
			for _, m := range p.Models {
				norm := m
				if a, ok := aliases[m]; ok {
					norm = a
				}
				entry, exists := merged[m]
				if !exists {
					entry = map[string]interface{}{}
					merged[m] = entry
				}
				if entry["context_length"] == nil || entry["context_length"] == 0 {
					if ctx, ok := limits[norm]; ok {
						entry["context_length"] = ctx
					}
				}
				if desc, ok := descriptions[norm]; ok {
					if _, hasDesc := entry["desc"]; !hasDesc {
						entry["desc"] = desc["desc"]
					}
				}
			}
		}

		// 3. 对 meta 里规范化名也建条目
		for k, v := range descriptions {
			entry, exists := merged[k]
			if !exists {
				entry = map[string]interface{}{}
				merged[k] = entry
			}
			if entry["context_length"] == nil || entry["context_length"] == 0 {
				if ctx, ok := limits[k]; ok {
					entry["context_length"] = ctx
				} else if v["ctx"] != "" {
					// 从 desc map 的 ctx 字段兜底（如果存在）
				}
			}
			if _, hasDesc := entry["desc"]; !hasDesc {
				entry["desc"] = v["desc"]
			}
		}
	}

	_ = json.NewEncoder(w).Encode(merged)
}

// handleContextLimits 处理上下文长度配置
func (h *AdminHandler) handleContextLimits(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	switch r.Method {
	case http.MethodGet:
		limits := map[string]int{}
		if h.meta != nil {
			limits = h.meta.ContextLimits()
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "data": limits})
	case http.MethodPut:
		var req struct {
			Model         string `json:"model"`
			ContextLength int    `json:"context_length"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf("invalid request: %v", err), http.StatusBadRequest)
			return
		}
		if h.meta == nil {
			http.Error(w, "meta store not available", http.StatusNotImplemented)
			return
		}
		if err := h.meta.SetContextLimit(req.Model, req.ContextLength, h.dataDir); err != nil {
			http.Error(w, fmt.Sprintf("save failed: %v", err), http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
	case http.MethodDelete:
		model := r.URL.Query().Get("model")
		if model == "" {
			// 兼容 /api/context-limits/{model} 路径格式
			path := strings.TrimPrefix(r.URL.Path, "/api/context-limits/")
			if path != "" && path != "context-limits" {
				model = path
			}
		}
		if model == "" {
			http.Error(w, "model parameter required", http.StatusBadRequest)
			return
		}
		if h.meta == nil {
			http.Error(w, "meta store not available", http.StatusNotImplemented)
			return
		}
		if err := h.meta.DeleteContextLimit(model, h.dataDir); err != nil {
			http.Error(w, fmt.Sprintf("delete failed: %v", err), http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleVisionModels 获取识图模型列表
func (h *AdminHandler) handleVisionModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	visionModels := []string{}
	if h.meta != nil {
		sv := h.meta.SupportsVision()
		for model, ok := range sv {
			if ok {
				visionModels = append(visionModels, model)
			}
		}
	}

	_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "data": visionModels})
}

// handleVisionAssist 获取/设置识图辅助开关
func (h *AdminHandler) handleVisionAssist(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	switch r.Method {
	case http.MethodGet:
		enabled := h.cfg.Load().VisionAssist()
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "enabled": enabled})
	case http.MethodPut:
		var req struct {
			Enabled bool `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf("invalid request: %v", err), http.StatusBadRequest)
			return
		}
		// 更新内存配置
		h.cfg.Load().SetVisionAssist(req.Enabled)
		// 持久化到 UCI
		if h.uciTool != nil {
			val := "0"
			if req.Enabled {
				val = "1"
			}
			_ = h.uciTool.SetOptionWithCommit("model-gateway", "settings", "vision_assist", val)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// handlePollStatus 获取轮询状态
func (h *AdminHandler) handlePollStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 计算启用的模型总数
	totalModels := 0
	for _, p := range h.cfg.Load().Providers {
		if p != nil && p.Enabled {
			totalModels += len(p.Models)
		}
	}

	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"last_poll_time": engine.LastPollTimeUnix(),
		"total_models":   totalModels,
		"poll_strategy":  h.cfg.Load().PollStrategy(),
		"poll_count":     engine.PollCount(),
		"poll_max":       engine.PollMaxCount(),
		"poll_stage":     engine.PollStage(),
	})
}

// handlePollStrategy 处理 /api/poll-strategy：GET 返回当前策略，POST 切换策略并落 UCI
func (h *AdminHandler) handlePollStrategy(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	switch r.Method {
	case http.MethodGet:
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"strategy": h.cfg.Load().PollStrategy(),
		})
	case http.MethodPost:
		var req struct {
			Strategy string `json:"strategy"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf("invalid request: %v", err), http.StatusBadRequest)
			return
		}
		if req.Strategy != "limited" && req.Strategy != "continuous" {
			http.Error(w, "strategy must be 'limited' or 'continuous'", http.StatusBadRequest)
			return
		}
		if h.uciTool == nil {
			http.Error(w, "uci not available", http.StatusNotImplemented)
			return
		}
		if err := h.uciTool.SetOptionWithCommit("model-gateway", "settings", "poll_strategy", req.Strategy); err != nil {
			http.Error(w, fmt.Sprintf("set poll_strategy failed: %v", err), http.StatusInternalServerError)
			return
		}
		// 热重载配置，让后台巡检下一轮读到新策略
		if h.reloadConfig != nil {
			if newCfg, err := h.reloadConfig(); err == nil {
				h.cfg.Store(newCfg)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "strategy": req.Strategy})
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleProviderRoutes 统一分发 /api/providers/{name}/... 的所有子路由
func (h *AdminHandler) handleProviderRoutes(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	path := strings.TrimPrefix(r.URL.Path, "/api/providers/")
	parts := strings.Split(path, "/")
	if len(parts) == 0 || parts[0] == "" {
		http.Error(w, "provider name required", http.StatusBadRequest)
		return
	}
	name := parts[0]

	// /api/providers/{name} — PUT/DELETE
	if len(parts) == 1 {
		switch r.Method {
		case http.MethodPut:
			h.handleProviderUpdate(w, r, name)
		case http.MethodDelete:
			h.handleProviderDelete(w, r, name)
		default:
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}

	// /api/providers/{name}/{action} — 子路由分发
	action := parts[1]
	switch action {
	case "toggle-model":
		if r.Method == http.MethodPost {
			h.handleToggleModel(w, r, name)
		} else {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	case "fetch-models":
		if r.Method == http.MethodPost {
			h.handleFetchModels(w, r, name)
		} else {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	case "available-models":
		if r.Method == http.MethodGet {
			h.handleAvailableModels(w, r, name)
		} else {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	default:
		http.Error(w, "unknown action", http.StatusNotFound)
	}
}

// handleProviderUpdate 更新提供商配置
func (h *AdminHandler) handleProviderUpdate(w http.ResponseWriter, r *http.Request, name string) {
	var req struct {
		BaseURL *string  `json:"base_url"`
		APIKey  *string  `json:"api_key"`
		Models  []string `json:"models"`
		Enabled *bool    `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid request: %v", err), http.StatusBadRequest)
		return
	}

	if h.uciTool == nil {
		http.Error(w, "uci not available", http.StatusNotImplemented)
		return
	}

	sectionNames, err := h.uciTool.GetSectionNames("provider")
	if err != nil {
		http.Error(w, fmt.Sprintf("get providers failed: %v", err), http.StatusInternalServerError)
		return
	}

	var sectionName string
	for _, sn := range sectionNames {
		opts, err := h.uciTool.GetOptions("provider", sn)
		if err != nil {
			continue
		}
		if opts["name"] == name {
			sectionName = sn
			break
		}
	}

	if sectionName == "" {
		http.Error(w, "provider not found", http.StatusNotFound)
		return
	}

	// 仅更新请求体中实际提供的字段，避免保存模型选择时把 base_url / api_key / enabled 误清空（回归修复）
	options := map[string]string{}
	if req.BaseURL != nil {
		options["base_url"] = *req.BaseURL
	}
	if req.APIKey != nil {
		options["api_key"] = *req.APIKey
	}
	if req.Enabled != nil {
		options["enabled"] = map[bool]string{true: "1", false: "0"}[*req.Enabled]
	}
	if len(options) > 0 {
		if err := h.uciTool.BatchSetOptions("provider", sectionName, options); err != nil {
			http.Error(w, fmt.Sprintf("update provider failed: %v", err), http.StatusInternalServerError)
			return
		}
	}

	if req.Models != nil {
		if err := h.uciTool.SetList("provider", sectionName, "models", req.Models); err != nil {
			http.Error(w, fmt.Sprintf("update models failed: %v", err), http.StatusInternalServerError)
			return
		}
	}

	if h.reloadConfig != nil {
		if newCfg, err := h.reloadConfig(); err == nil {
			h.cfg.Store(newCfg)
		}
	}

	h.notifyProvidersChanged()

	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// handleProviderDelete 删除提供商
func (h *AdminHandler) handleProviderDelete(w http.ResponseWriter, r *http.Request, name string) {
	if h.uciTool == nil {
		http.Error(w, "uci not available", http.StatusNotImplemented)
		return
	}

	count, err := h.uciTool.DeleteSectionsByName("provider", name)
	if err != nil {
		http.Error(w, fmt.Sprintf("delete provider failed: %v", err), http.StatusInternalServerError)
		return
	}
	if count == 0 {
		http.Error(w, "provider not found", http.StatusNotFound)
		return
	}

	if h.reloadConfig != nil {
		if newCfg, err := h.reloadConfig(); err == nil {
			h.cfg.Store(newCfg)
		}
	}

	h.notifyProvidersChanged()

	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// handleToggleModel 启用/禁用单个模型
func (h *AdminHandler) handleToggleModel(w http.ResponseWriter, r *http.Request, name string) {
	var req struct {
		Model   string `json:"model"`
		Enabled bool   `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid request: %v", err), http.StatusBadRequest)
		return
	}

	if h.uciTool == nil {
		http.Error(w, "uci not available", http.StatusNotImplemented)
		return
	}

	sectionNames, err := h.uciTool.GetSectionNames("provider")
	if err != nil {
		http.Error(w, fmt.Sprintf("get providers failed: %v", err), http.StatusInternalServerError)
		return
	}

	var sectionName string
	for _, sn := range sectionNames {
		opts, err := h.uciTool.GetOptions("provider", sn)
		if err != nil {
			continue
		}
		if opts["name"] == name {
			sectionName = sn
			break
		}
	}

	if sectionName == "" {
		http.Error(w, "provider not found", http.StatusNotFound)
		return
	}

	disabledModels, _ := h.uciTool.GetLists("provider", sectionName, "disabled_models")

	if req.Enabled {
		for i, m := range disabledModels {
			if m == req.Model {
				disabledModels = append(disabledModels[:i], disabledModels[i+1:]...)
				break
			}
		}
	} else {
		found := false
		for _, m := range disabledModels {
			if m == req.Model {
				found = true
				break
			}
		}
		if !found {
			disabledModels = append(disabledModels, req.Model)
		}
	}

	if err := h.uciTool.SetList("provider", sectionName, "disabled_models", disabledModels); err != nil {
		http.Error(w, fmt.Sprintf("update disabled_models failed: %v", err), http.StatusInternalServerError)
		return
	}

	if h.reloadConfig != nil {
		if newCfg, err := h.reloadConfig(); err == nil {
			h.cfg.Store(newCfg)
		}
	}

	h.notifyProvidersChanged()

	_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "disabled_models": disabledModels})
}

// handleFetchModels 手动刷新某提供商的模型列表
func (h *AdminHandler) handleFetchModels(w http.ResponseWriter, r *http.Request, name string) {
	var targetProvider *config.Provider
	for _, p := range h.cfg.Load().Providers {
		if p.Name == name {
			targetProvider = p
			break
		}
	}

	if targetProvider == nil {
		http.Error(w, "provider not found", http.StatusNotFound)
		return
	}

	client := bypassClientLong
	url := strings.TrimRight(targetProvider.BaseURL, "/") + "/models"
	reqHttp, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, url, nil)
	reqHttp.Header.Set("Authorization", "Bearer "+targetProvider.APIKey)
	resp, err := client.Do(reqHttp)
	if err != nil {
		http.Error(w, fmt.Sprintf("fetch models failed: %v", err), http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		http.Error(w, fmt.Sprintf("upstream returned %d", resp.StatusCode), http.StatusInternalServerError)
		return
	}

	var result struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		http.Error(w, fmt.Sprintf("parse response failed: %v", err), http.StatusInternalServerError)
		return
	}

	models := make([]string, 0, len(result.Data))
	for _, m := range result.Data {
		models = append(models, m.ID)
	}

	if h.uciTool != nil {
		sectionNames, _ := h.uciTool.GetSectionNames("provider")
		for _, sn := range sectionNames {
			opts, err := h.uciTool.GetOptions("provider", sn)
			if err != nil {
				continue
			}
			if opts["name"] == name {
				if err := h.uciTool.SetList("provider", sn, "models", models); err != nil {
					http.Error(w, fmt.Sprintf("save models failed: %v", err), http.StatusInternalServerError)
					return
				}
				break
			}
		}
	}

	if h.reloadConfig != nil {
		if newCfg, err := h.reloadConfig(); err == nil {
			h.cfg.Store(newCfg)
		}
	}

	_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "models": models})
}

// handleAvailableModels 获取某提供商的所有可用模型（含付费）
func (h *AdminHandler) handleAvailableModels(w http.ResponseWriter, r *http.Request, name string) {
	var targetProvider *config.Provider
	for _, p := range h.cfg.Load().Providers {
		if p.Name == name {
			targetProvider = p
			break
		}
	}

	if targetProvider == nil {
		http.Error(w, "provider not found", http.StatusNotFound)
		return
	}

	client := bypassClientLong
	url := strings.TrimRight(targetProvider.BaseURL, "/") + "/models"
	reqHttp, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, url, nil)
	reqHttp.Header.Set("Authorization", "Bearer "+targetProvider.APIKey)
	resp, err := client.Do(reqHttp)
	if err != nil {
		http.Error(w, fmt.Sprintf("fetch models failed: %v", err), http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		http.Error(w, fmt.Sprintf("upstream returned %d", resp.StatusCode), http.StatusInternalServerError)
		return
	}

	var result struct {
		Data []map[string]interface{} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		http.Error(w, fmt.Sprintf("parse response failed: %v", err), http.StatusInternalServerError)
		return
	}

	// 提取模型 ID 字符串列表（与原 Python 版一致，返回 ["model-id-1", "model-id-2", ...]）
	modelIDs := make([]string, 0, len(result.Data))
	for _, m := range result.Data {
		if id, ok := m["id"].(string); ok {
			modelIDs = append(modelIDs, id)
		}
	}
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "models": modelIDs})
}

// handleCheck 处理手动检测 /api/check/{name}/{model} 或 /api/check/all
func (h *AdminHandler) handleCheck(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/check/")
	parts := strings.Split(path, "/")

	if len(parts) == 1 && parts[0] == "all" {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// 真实巡检：对每个 provider 的每个启用模型调用 chat/completions，与 poll.go 一致
		results := make(map[string]interface{})
		client := bypassClient
		for _, p := range h.cfg.Load().Providers {
			if p == nil || !p.Enabled {
				continue
			}
			for _, model := range p.Models {
				url := strings.TrimRight(p.BaseURL, "/") + "/chat/completions"
				payload := map[string]interface{}{
					"model":      model,
					"messages":   []map[string]string{{"role": "user", "content": "hi"}},
					"max_tokens": 5,
					"stream":     false,
				}
				body, _ := json.Marshal(payload)
				reqHttp, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, url, strings.NewReader(string(body)))
				reqHttp.Header.Set("Authorization", "Bearer "+p.APIKey)
				reqHttp.Header.Set("Content-Type", "application/json")
				start := time.Now()
				resp, err := client.Do(reqHttp)
				latency := time.Since(start)
				modelKey := p.Name + "||" + model
				rec := storage.PollRecord{Time: time.Now(), Model: modelKey, Latency: latency.Milliseconds()}
				item := map[string]interface{}{"latency_ms": latency.Milliseconds()}
				if err != nil {
					rec.OK = false
					rec.Error = err.Error()
					item["status"] = "error"
					item["detail"] = err.Error()
				} else {
					code := resp.StatusCode
					resp.Body.Close()
					if code == http.StatusOK || code == http.StatusNoContent {
						rec.OK = true
						item["status"] = "ok"
						item["detail"] = fmt.Sprintf("HTTP %d", code)
					} else {
						rec.OK = false
						rec.Error = fmt.Sprintf("HTTP %d", code)
						item["status"] = "fail"
						item["detail"] = fmt.Sprintf("HTTP %d", code)
					}
				}
				_ = h.history.Append(rec)
				results[modelKey] = item
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(results)
		return
	}

	if len(parts) != 2 {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	name := parts[0]
	model := parts[1]

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var targetProvider *config.Provider
	for _, p := range h.cfg.Load().Providers {
		if p.Name == name {
			targetProvider = p
			break
		}
	}

	if targetProvider == nil {
		http.Error(w, "provider not found", http.StatusNotFound)
		return
	}

	client := bypassClientLong
	url := strings.TrimRight(targetProvider.BaseURL, "/") + "/chat/completions"
	payload := map[string]interface{}{
		"model":      model,
		"messages":   []map[string]string{{"role": "user", "content": "hi"}},
		"max_tokens": 5,
		"stream":     false,
	}
	body, _ := json.Marshal(payload)
	reqHttp, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, url, bytes.NewReader(body))
	reqHttp.Header.Set("Authorization", "Bearer "+targetProvider.APIKey)
	reqHttp.Header.Set("Content-Type", "application/json")

	start := time.Now()
	resp, err := client.Do(reqHttp)
	latency := time.Since(start)

	result := map[string]interface{}{
		"name":       name,
		"model":      model,
		"latency_ms": latency.Milliseconds(),
	}

	if err != nil {
		result["status"] = "error"
		result["detail"] = err.Error()
	} else {
		result["status_code"] = resp.StatusCode
		result["status"] = "ok"
		if resp.StatusCode != http.StatusOK {
			result["status"] = "fail"
			b, _ := io.ReadAll(resp.Body)
			result["detail"] = string(b)[:min(len(b), 200)]
		} else {
			result["detail"] = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		resp.Body.Close()
	}

	_ = json.NewEncoder(w).Encode(result)
}

// min 辅助函数
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// maskKey 脱敏 API Key
func maskKey(key string) string {
	if len(key) <= 12 {
		return "****"
	}
	return key[:6] + "****" + key[len(key)-4:]
}

// handleCheckUpdate 处理 /api/check-update（存根）
func (h *AdminHandler) handleCheckUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":         true,
		"has_update": false,
		"current":    "1.0.0-r20260722",
		"latest":     "1.0.0-r20260722",
	})
}

// handleOpenURL 处理 /api/open-url（存根，OpenWrt 无系统浏览器）
func (h *AdminHandler) handleOpenURL(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// handleStartDownload 处理 /api/start-download（存根）
func (h *AdminHandler) handleStartDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "error": "not supported on OpenWrt"})
}

// handleDownloadProgress 处理 /api/download-progress（存根）
func (h *AdminHandler) handleDownloadProgress(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "error": "not supported"})
}

// handleApplyUpdate 处理 /api/apply-update（存根）
func (h *AdminHandler) handleApplyUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "error": "not supported on OpenWrt"})
}
