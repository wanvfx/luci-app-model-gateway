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
	"regexp"
	"sort"
	"strconv"
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
// 内置 SSRF 防护（默认模式）：拦截回环/链路本地/云元数据(169.254.169.254)，
// 放行 RFC1918 私网以兼容局域网自托管场景。与 proxy 层 newLocalBypassClient 对齐。
func NewBypassClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:                 netutil.BypassProxyFunc,
			DialContext:           netutil.SSRFSafeDialContext(func() bool { return false }),
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   20,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
		},
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
	IsChatModel(modelID string) bool
	ResolveModel(model string) string
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
		mu      sync.Mutex // P2-6: 保护并发读写
		content string
		ts      time.Time
	}
	stabilityCacheMu sync.Mutex // 保护 stabilityCache 并发读写（修复 #10 data race）
	stabilityCache   struct {
		data  interface{}
		hours int
		ts    time.Time
	}
	uciTool        *UCITool
	reloadConfig   func() (*config.Config, error)
	providersPath  string
	configPath     string
	meta           MetaStoreInterface
	modelCache     ModelCacheInterface
	cacheCtl       CacheControl           // 响应缓存运行时（统计/清空），main.go 注入
	budgetCtl      BudgetControl          // 预算运行时（当日成本/状态），main.go 注入
	vault          *storage.Vault         // 密钥保险库（api_key AES 加密落 UCI），main.go 注入
	priceSync      *engine.PriceSync      // 价格自动同步器（models.dev），main.go 注入
	freeModelGuard *engine.FreeModelGuard // 免费模型自动巡检器，main.go 注入
	vkeyStore      *storage.VKeyStore     // 虚拟密钥（子密钥）存储，main.go 注入
	cat            *engine.Catalog        // 模型参考库（成本仪表盘价格来源），main.go 注入
	circuits       *engine.CircuitPool    // 熔断器池（C4 锁定状态查询），main.go 注入
}

// SetVKeyStore 注入虚拟密钥存储（/api/vkeys 端点）
func (h *AdminHandler) SetVKeyStore(v *storage.VKeyStore) {
	h.vkeyStore = v
}

// SetCatalog 注入模型参考库（/api/cost-dashboard 价格来源）
func (h *AdminHandler) SetCatalog(c *engine.Catalog) {
	h.cat = c
}

// SetCircuits 注入熔断器池（C4 锁定状态查询，/api/check/all 显示 🔒）
func (h *AdminHandler) SetCircuits(cp *engine.CircuitPool) {
	h.circuits = cp
}

// SetVault 注入密钥保险库（写 UCI 时对 api_key 加密）
func (h *AdminHandler) SetVault(v *storage.Vault) {
	h.vault = v
}

// SetPriceSync 注入价格同步器（/api/price-sync 端点）
func (h *AdminHandler) SetPriceSync(ps *engine.PriceSync) {
	h.priceSync = ps
}

// SetFreeModelGuard 注入免费模型巡检器（/api/free-model-guard 端点）
func (h *AdminHandler) SetFreeModelGuard(g *engine.FreeModelGuard) {
	h.freeModelGuard = g
}

// UpdateProviderModels 更新指定 provider 的模型列表（供 FreeModelGuard 回调使用）
func (h *AdminHandler) UpdateProviderModels(name string, models []string) error {
	if h.uciTool == nil {
		return fmt.Errorf("uci not available")
	}
	sectionNames, err := h.uciTool.GetSectionNames("provider")
	if err != nil {
		return err
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
		return fmt.Errorf("provider %q not found", name)
	}
	if err := h.uciTool.SetList("provider", sectionName, "models", models); err != nil {
		return err
	}
	if h.reloadConfig != nil {
		if newCfg, err := h.reloadConfig(); err == nil {
			h.cfg.Store(newCfg)
		}
	}
	h.notifyProvidersChanged()
	return nil
}

// encryptKey 写 UCI 前加密 api_key（vault 未注入/主密钥不可用时降级明文，功能优先）
func (h *AdminHandler) encryptKey(k string) string {
	if h.vault != nil && k != "" {
		return h.vault.Encrypt(k)
	}
	return k
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
		// S6：注入 uci 工作目录，避免强假��� /etc/config 在 PATH 中
		if cfg != nil && cfg.UciDir() != "" {
			h.uciTool.SetDir(cfg.UciDir())
		}
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
	mux.HandleFunc("/api/provider-market", h.handleProviderMarket)
	mux.HandleFunc("/api/provider/probe", h.handleProviderProbe)
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
	// r20260727d 端点：虚拟密钥 / 成本用量仪表盘
	mux.HandleFunc("/api/vkeys", h.handleVKeys)
	mux.HandleFunc("/api/vkeys/", h.handleVKeys) // 子路径 /api/vkeys/{id}/reveal
	mux.HandleFunc("/api/cost-dashboard", h.handleCostDashboard)
	// G4 提示词模板 CRUD（走 adminAuth 鉴权；HandleTemplates 内部再校验一次 requireAdmin）
	mux.HandleFunc("/api/templates", h.HandleTemplates)
	mux.HandleFunc("/api/templates/", h.HandleTemplates)
	// 三缺口端点：别名 / 缓存 / 钩子·预算·并发（gateway_ext.go）
	h.registerGatewayExtRoutes(mux)
}

// handleConfig 处理 /api/config
func (h *AdminHandler) handleConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	switch r.Method {
	case http.MethodGet:
		// 安全（P0 修复）：完整 admin_key 仅在请求携带合法 Bearer admin_key 时返回。
		// 旧版靠 Origin/Referer 判"同源"——这两个头客户端完全可控（curl -H "Origin: ..." 即可伪造），
		// 等于把管理密钥裸奔给局域网内任何人。现在改为标准鉴权：未带密钥一律只给掩码。
		// 面板首次访问需人工输入一次密钥（可从 LuCI 或 uci get model-gateway.settings.admin_key 获取），
		// 之后由前端 localStorage 记住，体验不受影响。
		adminKey := "sk-local-****"
		if h.requireAdmin(r) {
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
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
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
			"name":              p.Name,
			"base_url":          p.BaseURL,
			"api_key_masked":    maskKey(p.APIKey),
			"models":            p.Models,
			"disabled_models":   p.DisabledModels,
			"free_only":         p.FreeOnly,
			"enabled":           p.Enabled,
			"auth_header":       p.AuthHeader,
			"auth_scheme":       p.AuthScheme,
			"format":            p.FormatOrDefault(),
			"thinking_budget":   p.ThinkingBudget,
			"no_auth":           p.NoAuth,
			"auth_optional":     p.AuthOptional,
			"anonymous_api_key": maskKey(p.AnonymousAPIKey),
			"health":            health,
		})
	}
	_ = json.NewEncoder(w).Encode(providers)
}

// handleProvidersPost 新增提供商（持久化到 UCI）
func (h *AdminHandler) handleProvidersPost(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name            string   `json:"name"`
		BaseURL         string   `json:"base_url"`
		APIKey          string   `json:"api_key"`
		Models          []string `json:"models"`
		Enabled         bool     `json:"enabled"`
		AuthHeader      string   `json:"auth_header"`       // 可选：自定义鉴权头名（如 x-goog-api-key）
		AuthScheme      string   `json:"auth_scheme"`       // 可选：鉴权前缀（Bearer/none/自定义；空=默认）
		Format          string   `json:"format"`            // 可选：上游协议格式（openai/gemini/claude/openai-responses；空=openai）
		ThinkingBudget  int      `json:"thinking_budget"`   // 可选：思考预算 token 上限（>0 注入 claude/gemini）
		FreeOnly        bool     `json:"free_only"`         // 可选：仅自动勾选免费模型；默认 false（全选），仅 preset 流程显式传 true
		NoAuth          bool     `json:"no_auth"`           // 可选：标记为免 Key 提供者
		AuthOptional    bool     `json:"auth_optional"`     // 可选：标记为可选鉴权提供者（401 时自动去 auth 重试）
		AnonymousAPIKey string   `json:"anonymous_api_key"` // 可选：免 Key 提供者的 documented anonymous key（如 AI Horde）
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

	// P3-1: base_url 必须含 scheme（http:// 或 https://）
	if !isValidURL(req.BaseURL) {
		http.Error(w, "base_url must start with http:// or https://", http.StatusBadRequest)
		return
	}

	// 原子地新增 provider section 并设置 option / models list
	// 新增提供商默认启用（前端不传 enabled，且新提供商应立即可用，复刻 Python model-gateway 1.6.1 行为）
	options := map[string]string{
		"name":     req.Name,
		"base_url": req.BaseURL,
		"api_key":  h.encryptKey(req.APIKey),
		"enabled":  "1",
	}
	if strings.TrimSpace(req.AuthHeader) != "" {
		options["auth_header"] = strings.TrimSpace(req.AuthHeader)
	}
	if strings.TrimSpace(req.AuthScheme) != "" {
		options["auth_scheme"] = strings.TrimSpace(req.AuthScheme)
	}
	if f := strings.ToLower(strings.TrimSpace(req.Format)); f != "" && f != "openai" {
		options["format"] = f
	}
	if req.ThinkingBudget > 0 {
		options["thinking_budget"] = strconv.Itoa(req.ThinkingBudget)
	}
	if req.NoAuth {
		options["no_auth"] = "1"
	}
	if req.AuthOptional {
		options["auth_optional"] = "1"
	}
	if strings.TrimSpace(req.AnonymousAPIKey) != "" {
		options["anonymous_api_key"] = strings.TrimSpace(req.AnonymousAPIKey)
	}
	// H1 修复：持久化 free_only，使其与 req.FreeOnly 一致。
	// 否则 Load() 默认 FreeOnly:true（config/uci.go），重启后会把用户取消勾选的
	// “自动选择免费模型”静默翻回勾选。
	if req.FreeOnly {
		options["free_only"] = "1"
	} else {
		options["free_only"] = "0"
	}
	lists := map[string][]string{}
	if len(req.Models) > 0 {
		lists["models"] = req.Models
	} else if req.BaseURL != "" && (req.APIKey != "" || strings.EqualFold(strings.TrimSpace(req.AuthScheme), "none")) {
		// 复刻 Python 1.6.1 add_provider：添加提供商时自动拉取上游模型并全部选中（默认全选），
		// 使输出模型 / 路由配置 / 识图 / 巡检扫描立即生效，无需手动到“模型管理”逐个勾选。
		// 免 Key 提供者（auth_scheme=none）密钥为空也尝试自动拉取。
		// F2 修复：freeOnly 不再硬编码 true（否则付费模型被漏选），默认 false 全选；
		// 仅当调用方显式传 free_only=true（如 preset 流程）时才只选免费模型。
		if fetched, ferr := FetchModelsFromUpstream(req.BaseURL, req.APIKey, req.AuthHeader, req.AuthScheme, req.FreeOnly, h.meta.IsChatModel); ferr == nil && len(fetched) > 0 {
			lists["models"] = fetched
			log.Printf("auto-selected %d models for new provider %q (freeOnly=%v)", len(fetched), req.Name, req.FreeOnly)
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
		strategies := make(map[string]string)
		for _, r := range h.cfg.Load().Routers {
			routers[r.Name] = r.Members
			if r.Strategy != "" {
				strategies[r.Name] = r.Strategy
			} else {
				strategies[r.Name] = "quality"
			}
		}
		// data 保持旧格式（向后兼容），strategies 为新增字段
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "data": routers, "strategies": strategies})
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
		// auto 是内存态虚拟路由组（聚合所有启用模型），绝不持久化到 UCI
		if name == "auto" {
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

		// 持久化策略（仅合法值；未传则不写，读取端默认 quality）
		if strategy, _ := router["strategy"].(string); strategy != "" && config.ValidStrategy(strategy) {
			if err := h.uciTool.SetOptionWithCommit("router", secID, "strategy", strategy); err != nil {
				http.Error(w, fmt.Sprintf("set router strategy failed: %v", err), http.StatusInternalServerError)
				return
			}
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
			// P3-2: 限制远程公告最大长度（防磁盘耗尽）
			const maxAnnouncementBytes = 1 << 20 // 1 MB
			if len(b) > maxAnnouncementBytes {
				b = b[:maxAnnouncementBytes]
				log.Printf("[announcement] remote content truncated to %d bytes (max %d)", maxAnnouncementBytes, maxAnnouncementBytes)
			}
			content := strings.TrimSpace(string(b))
			if content != "" {
				// P2-6: 加锁保护 announcementCache 并发安全
				h.announcementCache.mu.Lock()
				h.announcementCache.content = content
				h.announcementCache.ts = time.Now()
				h.announcementCache.mu.Unlock()
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

// P2-8 / P3-1: sanitizeSensitiveData 从字符串中移除 API key、token 等敏感信息，
// 防止上游错误响应泄露凭证到前端。P3-1 扩展覆盖更多模式（虚拟密钥、JWT、
// Authorization 头、长随机串、密码/secret/token 字段、内部堆栈行）。
var (
	sensitivePatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)sk-[a-zA-Z0-9_\-]{16,}`),                               // OpenAI 类 sk- 密钥
		regexp.MustCompile(`(?i)sk-vk-[a-zA-Z0-9]{16,}`),                               // 虚拟子密钥
		regexp.MustCompile(`(?i)eyJ[a-zA-Z0-9_\-]+\.[a-zA-Z0-9_\-]+\.[a-zA-Z0-9_\-]+`), // JWT（三段式 base64）
		regexp.MustCompile(`(?i)bearer\s+[a-zA-Z0-9_\-\.]+`),                           // Bearer 令牌
		regexp.MustCompile(`(?i)authorization\s*:\s*\S+`),                              // Authorization 头（含任意值）
		regexp.MustCompile(`(?i)api[_-]?key\s*[:=]\s*[^\s"'}\]]+`),                     // api_key=xxx / api-key: xxx
		regexp.MustCompile(`(?i)key\s*[:=]\s*[a-zA-Z0-9_\-]{16,}`),                     // key=xxx
		regexp.MustCompile(`(?i)(password|secret|token)\s*[:=]\s*[^\s"'}\]]{6,}`),      // password/secret/token=xxx
		regexp.MustCompile(`(?i)[a-z0-9]{40,}`),                                        // 长随机串（sha1/长 token 等）
	}
	sensitiveFieldRe = regexp.MustCompile(`(?i)"(api[_-]?key|password|secret|token|authorization)"\s*:\s*"[^"]*"`)
	stackTraceRe     = regexp.MustCompile(`(?m)^(goroutine \d+|panic:|\s+[^\s]+\.go:\d+).*$`)
)

func sanitizeSensitiveData(s string) string {
	for _, re := range sensitivePatterns {
		s = re.ReplaceAllString(s, "[REDACTED]")
	}
	// JSON 中的敏感字段值
	s = sensitiveFieldRe.ReplaceAllString(s, `"$1":"[REDACTED]"`)
	// 内部堆栈/panic 行（避免泄露源码路径与行号）
	s = stackTraceRe.ReplaceAllString(s, "[STACK]")
	return s
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

	// 缓存逻辑（与 Python 原版 STABILITY_CACHE_TTL = 30 一致）；加锁防并发 data race。
	// refresh=1 时绕过缓存，供「立即检测 / 一键配置」后强制刷新探针次数与延迟。
	refresh := r.URL.Query().Get("refresh") == "1"
	h.stabilityCacheMu.Lock()
	if !refresh && h.stabilityCache.data != nil && h.stabilityCache.hours == hours && time.Since(h.stabilityCache.ts) < 30*time.Second {
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
		Provider      string  `json:"provider"`
		Model         string  `json:"model"`
		Checks        int     `json:"checks"`
		Ok            int     `json:"ok"`
		Fail          int     `json:"fail"`
		Error         int     `json:"error"`
		Availability  float64 `json:"availability"`
		AvgLatencyMs  *int    `json:"avg_latency_ms"`
		MinLatencyMs  *int    `json:"min_latency_ms"`
		MaxLatencyMs  *int    `json:"max_latency_ms"`
		LastStatus    string  `json:"last_status"`
		Vision        bool    `json:"vision"`
		LastCheckAt   int64   `json:"last_check_at"`
		lastCheckTime time.Time
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
			// Error 是「失败」的子集（记录失败原因），不再是独立于 ok/fail 的第三态，
			// 否则一条 OK=true 但带非空 detail 的探针会被同时计入 Ok 与 Error，
			// 造成「可用率100% 但成功/失败同时 2/2」的矛盾（问题1修复，参考 Python 1.6.1 互斥统计）。
			if rec.Error != "" {
				st.Error++
			}
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
		if rec.Time.After(st.lastCheckTime) {
			st.lastCheckTime = rec.Time
			st.LastCheckAt = rec.Time.Unix()
		}
	}

	// 合并「配置中存在、但本时间窗内无探测记录」的模型，标记为 pending（灰色 = 还没检测到）。
	// 这样新添加的平台会立刻在稳定性列表出现为灰色，而不是整片空白（修复「列表空 / 灰色未显示」）。
	for _, p := range h.cfg.Load().Providers {
		if p == nil || !p.Enabled {
			continue
		}
		for _, model := range p.Models {
			key := p.Name + "||" + model
			if _, exists := stats[key]; exists {
				continue
			}
			stats[key] = &modelStat{
				Provider:     p.Name,
				Model:        model,
				LastStatus:   "pending",
				Availability: 0,
			}
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
		BaseURL    string `json:"base_url"`
		APIKey     string `json:"api_key"`
		AuthHeader string `json:"auth_header"`
		AuthScheme string `json:"auth_scheme"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid request: %v", err), http.StatusBadRequest)
		return
	}

	client := bypassClient
	url := strings.TrimRight(req.BaseURL, "/") + "/models"
	reqHttp, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, url, nil)
	if hn, hv := config.AuthHeaderValue(req.AuthHeader, req.AuthScheme, req.APIKey); hn != "" {
		reqHttp.Header.Set(hn, hv)
	}
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
		// P2-8: 净化响应体，防止上游错误信息泄露 API key 等敏感数据
		sanitized := sanitizeSensitiveData(string(b))
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "detail": fmt.Sprintf("HTTP %d: %s", resp.StatusCode, sanitized[:min(len(sanitized), 200)])})
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

// upstreamModel 上游 /models 返回的单个模型条目（含嵌套 pricing 用于免费判定）
type upstreamModel struct {
	ID      string `json:"id"`
	Pricing *struct {
		Prompt     float64 `json:"prompt"`
		Completion float64 `json:"completion"`
		Input      float64 `json:"input"`
		Output     float64 `json:"output"`
	} `json:"pricing"`
	PromptPrice   float64 `json:"prompt_price"`
	CompletePrice float64 `json:"completion_price"`
}

// isFreeUpstreamModel 依据名称与 pricing 判定免费（对齐 Python is_free_model / is_free_by_name）
func isFreeUpstreamModel(m upstreamModel) bool {
	lower := strings.ToLower(m.ID)
	if strings.Contains(lower, ":free") || strings.Contains(lower, "-free") || strings.Contains(lower, "_free") {
		return true
	}
	if m.Pricing != nil {
		return m.Pricing.Prompt == 0 && m.Pricing.Completion == 0 && m.Pricing.Input == 0 && m.Pricing.Output == 0
	}
	if m.PromptPrice != 0 || m.CompletePrice != 0 {
		return false
	}
	return true
}

// FetchModelsFromUpstream 从上游拉取模型列表（公开，供 main.go 和 FreeModelGuard 使用）
// freeOnly=true 时仅保留免费模型（名称含 :free/-free/_free，或上游 pricing 全为 0）；
// isChat 非 nil 时仅保留对话模型（过滤 embed/asr/tts 等非对话模型，对齐 Python fetch_models）。
func FetchModelsFromUpstream(baseURL, apiKey, authHeader, authScheme string, freeOnly bool, isChat func(string) bool) ([]string, error) {
	client := bypassClientLong
	url := strings.TrimRight(baseURL, "/") + "/models"
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if hn, hv := config.AuthHeaderValue(authHeader, authScheme, apiKey); hn != "" {
		req.Header.Set(hn, hv)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upstream returned %d", resp.StatusCode)
	}
	var result struct {
		Data []upstreamModel `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	models := make([]string, 0, len(result.Data))
	for _, m := range result.Data {
		if m.ID == "" {
			continue
		}
		if freeOnly && !isFreeUpstreamModel(m) {
			continue
		}
		if isChat != nil && !isChat(m.ID) {
			continue
		}
		models = append(models, m.ID)
	}
	return models, nil
}

// DetectFormat 探测上游协议格式（A2）：按响应结构推断 openai/gemini/claude/openai-responses。
func DetectFormat(baseURL, apiKey, authHeader, authScheme string) string {
	client := bypassClientLong
	base := strings.TrimRight(baseURL, "/")

	// 1) OpenAI 兼容：GET /models 返回 { "data": [...] }
	if models, _ := FetchModelsFromUpstream(baseURL, apiKey, authHeader, authScheme, false, nil); len(models) > 0 {
		return "openai"
	}

	// 2) Claude：POST /v1/messages 端点存在（返回 400 而非 404 说明命中原生协议）
	msgURL := base + "/v1/messages"
	payload, _ := json.Marshal(map[string]interface{}{
		"model":      "detect",
		"max_tokens": 1,
		"messages":   []map[string]interface{}{{"role": "user", "content": "hi"}},
	})
	req, _ := http.NewRequest(http.MethodPost, msgURL, bytes.NewReader(payload))
	if hn, hv := config.AuthHeaderValue(authHeader, authScheme, apiKey); hn != "" {
		req.Header.Set(hn, hv)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	if resp, err := client.Do(req); err == nil {
		resp.Body.Close()
		if resp.StatusCode == http.StatusBadRequest {
			return "claude"
		}
	}

	// 3) OpenAI Responses：POST /v1/responses 端点存在
	respURL := base + "/v1/responses"
	payload, _ = json.Marshal(map[string]interface{}{
		"model":             "detect",
		"max_output_tokens": 1,
		"input":             []map[string]interface{}{{"role": "user", "content": "hi"}},
	})
	req, _ = http.NewRequest(http.MethodPost, respURL, bytes.NewReader(payload))
	if hn, hv := config.AuthHeaderValue(authHeader, authScheme, apiKey); hn != "" {
		req.Header.Set(hn, hv)
	}
	req.Header.Set("Content-Type", "application/json")
	if resp, err := client.Do(req); err == nil {
		resp.Body.Close()
		if resp.StatusCode == http.StatusBadRequest {
			return "openai-responses"
		}
	}

	// 4) Gemini：GET /models 返回非 OpenAI 结构（含 models[].name 路径格式）
	// 兜底回退 openai
	return "openai"
}

// handleProviderProbe provider/probe 向导（A6）：前端传 base_url+key，先连测再返回模型清单+推断格式。
func (h *AdminHandler) handleProviderProbe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		BaseURL    string `json:"base_url"`
		APIKey     string `json:"api_key"`
		AuthHeader string `json:"auth_header"`
		AuthScheme string `json:"auth_scheme"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid request: %v", err), http.StatusBadRequest)
		return
	}
	baseURL := strings.TrimSpace(req.BaseURL)
	if baseURL == "" {
		http.Error(w, "base_url is required", http.StatusBadRequest)
		return
	}

	// 先探测连通性 + 推断格式
	format := DetectFormat(baseURL, req.APIKey, req.AuthHeader, req.AuthScheme)
	models, ferr := FetchModelsFromUpstream(baseURL, req.APIKey, req.AuthHeader, req.AuthScheme, false, nil)
	if ferr != nil {
		// 格式探测成功但模型拉取失败：仍返回推断格式，方便前端先填
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"ok":     false,
			"format": format,
			"error":  ferr.Error(),
			"models": []string{},
		})
		return
	}

	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":     true,
		"format": format,
		"models": models,
	})
}

// handleProviderMarket 提供者市场：返回随包只读 providers_catalog.json（132 个 OpenAI 兼容提供者目录）。
// 前端一次拉取后本地搜索；一键添加复用 POST /api/providers（无内置模型列表时后端自动拉取上游 /models 全选）。
func (h *AdminHandler) handleProviderMarket(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	paths := []string{
		filepath.Join(h.appDir, "providers_catalog.json"),
		filepath.Join(h.appDir, "..", "share", "model-gateway", "providers_catalog.json"),
	}
	for _, p := range paths {
		if data, err := os.ReadFile(p); err == nil {
			_, _ = w.Write(data)
			return
		}
	}
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"version": 0, "providers": []interface{}{}})
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
		// P2-6: 若 key 是密文且 vault 已注入，先解密再验证/发送上游
		if key != "" && h.vault != nil && strings.HasPrefix(key, storage.EncPrefix) {
			key = h.vault.Decrypt(key)
		}
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
		models, _ := FetchModelsFromUpstream(platCfg.BaseURL, key, "", "", platCfg.FreeOnly, h.meta.IsChatModel)

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
			"api_key":  h.encryptKey(key), // 复用已有 key 时已是 enc: 前缀，Encrypt 幂等原样返回
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
		// 持久化到 UCI
		if h.uciTool != nil {
			val := "0"
			if req.Enabled {
				val = "1"
			}
			_ = h.uciTool.SetOptionWithCommit("model-gateway", "settings", "vision_assist", val)
		}
		// 通过 reloadConfig 生成新 Config 后 atomic.Store，避免直接改共享 Config 导致 data race。
		if h.reloadConfig != nil {
			if newCfg, err := h.reloadConfig(); err == nil {
				h.cfg.Store(newCfg)
			}
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
		BaseURL         *string  `json:"base_url"`
		APIKey          *string  `json:"api_key"`
		Models          []string `json:"models"`
		Enabled         *bool    `json:"enabled"`
		AuthHeader      *string  `json:"auth_header"`
		AuthScheme      *string  `json:"auth_scheme"`
		Format          *string  `json:"format"`
		ThinkingBudget  *int     `json:"thinking_budget"`
		NoAuth          *bool    `json:"no_auth"`
		AuthOptional    *bool    `json:"auth_optional"`
		AnonymousAPIKey *string  `json:"anonymous_api_key"`
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
		if !isValidURL(*req.BaseURL) {
			http.Error(w, "base_url must start with http:// or https://", http.StatusBadRequest)
			return
		}
		options["base_url"] = *req.BaseURL
	}
	if req.APIKey != nil {
		options["api_key"] = h.encryptKey(*req.APIKey)
	}
	if req.Enabled != nil {
		options["enabled"] = map[bool]string{true: "1", false: "0"}[*req.Enabled]
	}
	if req.AuthHeader != nil {
		options["auth_header"] = strings.TrimSpace(*req.AuthHeader)
	}
	if req.AuthScheme != nil {
		options["auth_scheme"] = strings.TrimSpace(*req.AuthScheme)
	}
	if req.Format != nil {
		f := strings.ToLower(strings.TrimSpace(*req.Format))
		if f == "" {
			f = "openai"
		}
		options["format"] = f
	}
	if req.ThinkingBudget != nil && *req.ThinkingBudget > 0 {
		options["thinking_budget"] = strconv.Itoa(*req.ThinkingBudget)
	}
	if req.NoAuth != nil {
		options["no_auth"] = map[bool]string{true: "1", false: "0"}[*req.NoAuth]
	}
	if req.AuthOptional != nil {
		options["auth_optional"] = map[bool]string{true: "1", false: "0"}[*req.AuthOptional]
	}
	if req.AnonymousAPIKey != nil {
		options["anonymous_api_key"] = strings.TrimSpace(*req.AnonymousAPIKey)
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
	targetProvider.ApplyAuth(reqHttp.Header)
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
	targetProvider.ApplyAuth(reqHttp.Header)
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

	// 提取模型信息列表（含是否免费标记，供前端模型管理面板自动勾选免费模型）
	type upstreamModel struct {
		ID      string                 `json:"id"`
		Pricing map[string]interface{} `json:"pricing"`
	}
	modelInfos := make([]map[string]interface{}, 0, len(result.Data))
	for _, m := range result.Data {
		info := map[string]interface{}{
			"id": m["id"],
		}
		if p, ok := m["pricing"].(map[string]interface{}); ok {
			info["pricing"] = p
		}
		modelInfos = append(modelInfos, info)
	}
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "models": modelInfos})
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
		// 真实巡检：对每个 provider 的每个启用模型调用 chat/completions，与 poll.go 一致。
		// 并发探测（Semaphore 10），总耗时 ≈ 单次探测（≤30s），避免模型多时串行超 30s 触发前端超时（问题8）。
		results := make(map[string]interface{})
		client := bypassClient
		var mu sync.Mutex
		var wg sync.WaitGroup
		sem := make(chan struct{}, 10)
		for _, p := range h.cfg.Load().Providers {
			if p == nil || !p.Enabled {
				continue
			}
			for _, model := range p.Models {
				wg.Add(1)
				sem <- struct{}{}
				go func(p *config.Provider, model string) {
					defer wg.Done()
					defer func() { <-sem }()
					// 逐模型真实探测：POST {BaseURL}/chat/completions（对齐 Python check_model 与 poll.go），
					// 解析别名后发送最小请求（max_tokens=5, stream=false），30s 超时由 ChatProbe 内部管控。
					actual := h.meta.ResolveModel(model)
					ok, detail, latency, _ := engine.ChatProbe(p, actual, client)
					modelKey := p.Name + "||" + model
					rec := storage.PollRecord{Time: time.Now(), Model: modelKey, Latency: latency.Milliseconds(), OK: ok, Error: detail}
					item := map[string]interface{}{
						"latency_ms": latency.Milliseconds(),
						"status":     map[bool]string{true: "ok", false: "fail"}[ok],
						"detail":     detail,
					}
					// C4 模型锁定自愈：暴露锁定状态（连续失败被关小黑屋）
					if h.circuits != nil {
						item["locked"] = h.circuits.Locked(modelKey)
						item["circuit_state"] = h.circuits.State(modelKey)
					}
					mu.Lock()
					results[modelKey] = item
					mu.Unlock()
					_ = h.history.Append(rec)
				}(p, model)
			}
		}
		wg.Wait()
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
	// 逐模型真实探测：POST {BaseURL}/chat/completions（对齐 Python check_model 与 /api/check/all）
	actual := h.meta.ResolveModel(model)
	ok, detail, latency, _ := engine.ChatProbe(targetProvider, actual, client)
	result := map[string]interface{}{
		"name":       name,
		"model":      model,
		"latency_ms": latency.Milliseconds(),
	}
	if ok {
		result["status"] = "ok"
		result["detail"] = detail
	} else {
		result["status"] = "fail"
		result["detail"] = detail
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

// isValidURL 校验 URL 是否含 http/https scheme（P3-1: base_url 持久化前校验）
func isValidURL(raw string) bool {
	if raw == "" {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	scheme := strings.ToLower(u.Scheme)
	return scheme == "http" || scheme == "https"
}

// maskKey 脱敏 API Key
func maskKey(key string) string {
	if len(key) <= 12 {
		return "****"
	}
	return key[:6] + "****" + key[len(key)-4:]
}

// appVersion 当前构建版本号（与 iStoreOS meta / 面板标题一致，纯 semver 部分）。
// 打包时由 mkipk.py 的 VERSION 与面板三处同步；此处仅取 semver 用于更新比对。
const appVersion = "1.9.0"

// handleCheckUpdate 对接 GitHub Releases 检查新版本（问题6）。
// 拉取 https://api.github.com/repos/wanvfx/luci-app-model-gateway/releases/latest，
// 与当前版本做语义化比较，有更新则 has_update=true 并附带 release_notes / download_url。
func (h *AdminHandler) handleCheckUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	current := normalizeVersion(appVersion)
	latest, notes, downloadURL, fetchErr := fetchLatestRelease()
	hasUpdate := false
	if fetchErr == nil && latest != "" {
		hasUpdate = compareSemver(normalizeVersion(latest), current) > 0
	}
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":            true,
		"has_update":    hasUpdate,
		"current":       current,
		"latest":        latest,
		"release_notes": notes,
		"download_url":  downloadURL,
		"force_update":  false,
		"self_update":   false, // 本设备（OpenWrt/iStoreOS 软路由）不支持一键在线更新，需经 iStore/opkg 升级
		"error":         errToStr(fetchErr),
	})
}

func errToStr(e error) string {
	if e == nil {
		return ""
	}
	return e.Error()
}

// fetchLatestRelease 从 GitHub 获取最新发布版本信息（带 8s 超时，失败优雅降级）。
func fetchLatestRelease() (tag, notes, downloadURL string, err error) {
	req, err := http.NewRequest(http.MethodGet, "https://api.github.com/repos/wanvfx/luci-app-model-gateway/releases/latest", nil)
	if err != nil {
		return "", "", "", err
	}
	req.Header.Set("User-Agent", "luci-app-model-gateway")
	req.Header.Set("Accept", "application/vnd.github+json")
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", "", fmt.Errorf("github returned %d", resp.StatusCode)
	}
	var payload struct {
		TagName string `json:"tag_name"`
		Body    string `json:"body"`
		HTMLURL string `json:"html_url"`
		Assets  []struct {
			BrowserDownloadURL string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", "", "", err
	}
	tag = payload.TagName
	notes = payload.Body
	if len(payload.Assets) > 0 {
		downloadURL = payload.Assets[0].BrowserDownloadURL
	}
	if downloadURL == "" {
		downloadURL = payload.HTMLURL
	}
	return tag, notes, downloadURL, nil
}

// normalizeVersion 统一版本格式：去 v 前缀、去构建后缀（如 -r20260727c），仅保留 主.次.修。
func normalizeVersion(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	v = strings.TrimPrefix(v, "V")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	return v
}

// compareSemver 比较 "a.b.c" 语义版本，返回 -1/0/1。
func compareSemver(a, b string) int {
	pa := parseVer(a)
	pb := parseVer(b)
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			if pa[i] > pb[i] {
				return 1
			}
			return -1
		}
	}
	return 0
}

func parseVer(v string) [3]int {
	var r [3]int
	parts := strings.Split(v, ".")
	for i := 0; i < 3 && i < len(parts); i++ {
		n, _ := strconv.Atoi(strings.TrimSpace(parts[i]))
		r[i] = n
	}
	return r
}

// handleOpenURL 处理 /api/open-url（存根，OpenWrt 无系统浏览器）
func (h *AdminHandler) handleOpenURL(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// handleStartDownload 处理 /api/start-download（本设备不支持在线更新）
func (h *AdminHandler) handleStartDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":    false,
		"error": "本设备（OpenWrt/iStoreOS）不支持一键在线更新，请通过 iStore 应用商店或 opkg 升级到最新版本",
	})
}

// handleDownloadProgress 处理 /api/download-progress（本设备不支持在线更新）
func (h *AdminHandler) handleDownloadProgress(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":    false,
		"error": "本设备（OpenWrt/iStoreOS）不支持一键在线更新",
	})
}

// handleApplyUpdate 处理 /api/apply-update（本设备不支持在线更新）
func (h *AdminHandler) handleApplyUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":    false,
		"error": "本设备（OpenWrt/iStoreOS）不支持一键在线更新，请通过 iStore 应用商店或 opkg 升级到最新版本",
	})
}
