package proxy

import (
	"context"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/wanvfx/luci-app-model-gateway/api"
	"github.com/wanvfx/luci-app-model-gateway/config"
	"github.com/wanvfx/luci-app-model-gateway/engine"
	"github.com/wanvfx/luci-app-model-gateway/internal/netutil"
	"github.com/wanvfx/luci-app-model-gateway/storage"
)

// Server 是 HTTP 服务
type Server struct {
	cfg         atomic.Pointer[config.Config]
	mux         *http.ServeMux
	router      atomic.Pointer[engine.Router]
	httpClient  *http.Client
	meta        *MetaStore
	modelCache  *ModelCache
	modelsCache *modelsCache
	usage       *storage.Usage
	circuits    *engine.CircuitPool
	scorer      *engine.Scorer
	catalog     *engine.Catalog
	cache       *engine.ResponseCache // 响应缓存（语义 simhash）
	budget      *storage.Budget        // 预算/余额护栏
	guard       *ConcurrencyGuard       // 并发护栏
	metrics     *engine.Metrics         // Prometheus /metrics 指标注册表
	penalty     *engine.PenaltyTracker  // 动态惩罚路由（429/5xx 临时降权）
	idem        *idemStore              // 幂等键存储（Idempotency-Key）
	vkeys       *storage.VKeyStore       // 虚拟密钥（子密钥）存储（Bearer 鉴权 + 配额限流）
	appDir      string
	httpServer  *http.Server
}

// New 创建服务
func New(cfg *config.Config, dataDir string, usage *storage.Usage, circuits *engine.CircuitPool, scorer *engine.Scorer) *Server {
	appDir, _ := os.Getwd()
	if envApp := os.Getenv("MODEL_GATEWAY_APP"); envApp != "" {
		appDir = envApp
	}
	// 优先使用可执行文件所在目录，避免 procd 等环境下 cwd 为 / 导致相对路径失效
	if ex, err := os.Executable(); err == nil {
		appDir = filepath.Dir(ex)
	}
	os.MkdirAll(dataDir, 0755)

	// 创建绕过本地/内网代理的 HTTP 客户端
	httpClient := newLocalBypassClient(30 * time.Second)

	s := &Server{
		mux:         http.NewServeMux(),
		httpClient:  httpClient,
		meta:        NewMetaStore(appDir, dataDir),
		modelCache:  NewModelCache(5 * time.Minute),
		modelsCache: newModelsCache(30 * time.Second),
		usage:       usage,
		circuits:    circuits,
		scorer:      scorer,
		catalog:     engine.LoadCatalog(appDir, dataDir),
		cache:       engine.NewResponseCache(dataDir),
		budget:      storage.NewBudget(dataDir),
		guard:       NewConcurrencyGuard(),
		metrics:     engine.NewMetrics(),
		penalty:     engine.NewPenaltyTracker(),
		idem:        newIdemStore(),
		appDir:      appDir,
	}
	s.cfg.Store(cfg)
	s.initRouter()
	s.routes()
	return s
}

// newLocalBypassClient 创建绕过本地/内网代理的 HTTP 客户端
// 当系统设置了 HTTP_PROXY/HTTPS_PROXY 时，对本地/内网地址不走代理，
// 避免 502 连接被中断（如 clash/v2ray 代理无法访问局域网地址）
func newLocalBypassClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:                 netutil.BypassProxyFunc,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   20,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
		},
	}
}

// initRouter 初始化路由引擎
// 注意：必须复用 s.scorer（main.go 注入、Poller 持续写入的同一实例），
// 否则 Router 拿到的是空评分器，质量排序永远无数据（历史 bug #01）。
func (s *Server) initRouter() {
	cfg := s.cfg.Load()
	r := engine.NewRouter(s.scorer)
	r.SetCatalog(s.catalog)
	r.SetPenalty(s.penalty)
	hasAuto := false
	for _, rt := range cfg.Routers {
		r.AddRouterWithStrategy(rt.Name, rt.Members, rt.Strategy)
		r.SetWeights(rt.Name, rt.Weights)
		if rt.Name == "auto" {
			hasAuto = true
		}
	}
	// auto 虚拟路由组：聚合所有启用 provider 的全部模型（前缀名），质量分排序。
	// 用户配置了同名路由组则尊重用户配置；auto 只存在于内存，绝不持久化到 UCI。
	if !hasAuto {
		disabled := map[string]bool{}
		for _, dm := range cfg.AllDisabledModels() {
			disabled[strings.ToLower(strings.TrimSpace(dm))] = true
		}
		var members []string
		for _, p := range cfg.Providers {
			if !p.Enabled {
				continue
			}
			for _, m := range p.Models {
				if disabled[strings.ToLower(m)] {
					continue
				}
				members = append(members, p.Name+"-"+m)
			}
		}
		if len(members) > 0 {
			r.AddRouterWithStrategy("auto", members, "quality")
		}
	}
	s.router.Store(r)
}

// ListenAndServe 启动监听
func (s *Server) ListenAndServe() error {
	s.httpServer = &http.Server{
		Addr:    s.cfg.Load().BindAddr(),
		Handler: recoverMiddleware(s.mux),
	}
	return s.httpServer.ListenAndServe()
}

// recoverMiddleware 全局 panic 恢复：任一 handler 发生 panic 时返回 500，
// 避免整个守护进程崩溃。此前无 recover，handler panic 会杀掉进程，
// 导致前端请求永远拿不到响应、永久卡在“加载中…”（如一键配置弹窗）。
// 与 handlePreset 内的局部 recover（用于还原配置）配合：局部 recover 消费 panic 并还原配置，
// 这里作为兜底，确保即使遗漏也能优雅返回而非崩进程。
func recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("PANIC recovered in %s %s: %v", r.Method, r.URL.Path, rec)
				if w.Header().Get("Content-Type") == "" {
					w.Header().Set("Content-Type", "application/json; charset=utf-8")
				}
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"ok":false,"detail":"internal server error (recovered)"}`))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// UpdateConfig 热更新配置（供管理面持久化后调用）
func (s *Server) UpdateConfig(newCfg *config.Config) {
	s.cfg.Store(newCfg)
	s.initRouter()
	// 同步缓存配置（enabled/ttl/max/semantic 热更新）
	if s.cache != nil {
		cc := newCfg.EffectiveCache()
		s.cache.SetConfig(cc.Enabled, cc.TTL, cc.MaxEntries, cc.Semantic)
	}
	// 配置变更后立即使 /v1/models 缓存失效，输出模型面板立即反映最新选中状态
	s.modelsCache.Invalidate()
}

// hookDispatcher 从当前配置构建钩子分发器（每次请求读取最新配置）
func (s *Server) hookDispatcher() *HookDispatcher {
	return NewHookDispatcher(s.cfg.Load().Hooks)
}

// Shutdown 优雅关闭（与 Python 原版一致：关闭 HTTP server）
func (s *Server) Shutdown(ctx context.Context) error {
	if s.cache != nil {
		s.cache.Stop()
	}
	if s.httpServer != nil {
		return s.httpServer.Shutdown(ctx)
	}
	return nil
}

// ModelCacheFetch 手动触发模型详情缓存刷新
func (s *Server) ModelCacheFetch() {
	s.modelCache.FetchAndCache(s.cfg.Load().Providers, s.httpClient)
}

func (s *Server) routes() {
	htdocsDir := s.htdocsDir()
	s.mux.HandleFunc("/", s.handleIndex)
	s.mux.HandleFunc("/v1/models", s.handleModels)
	s.mux.HandleFunc("/v1/chat/completions", s.handleChatCompletions)
	s.mux.HandleFunc("/metrics", s.handleMetrics)
	s.mux.Handle("/htdocs/", http.StripPrefix("/htdocs/", http.FileServer(http.Dir(htdocsDir))))
}

// handleMetrics Prometheus 文本格式指标端点。
// 只读、不含任何密钥信息，供 Prometheus / Uptime Kuma / Grafana Agent 抓取；
// 注入缓存命中与当日预算即时 gauge。
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	extra := map[string]float64{}
	if s.cache != nil {
		st := s.cache.Stats()
		extra["model_gateway_cache_hits_total"] = float64(st.Hits)
		extra["model_gateway_cache_misses_total"] = float64(st.Misses)
		extra["model_gateway_cache_entries"] = float64(st.Entries)
	}
	if s.budget != nil {
		extra["model_gateway_budget_daily_cost_usd"] = s.budget.DailyTotal()
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(s.metrics.Render(extra)))
}

// htdocsDir 返回 htdocs 目录，优先 OpenWrt ipk 安装位置，回退到 appDir/htdocs
func (s *Server) htdocsDir() string {
	if dir := "/usr/share/model-gateway/htdocs"; dirExists(dir) {
		return dir
	}
	return filepath.Join(s.appDir, "htdocs")
}

// dirExists 检查目录是否存在
func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// Meta 返回元数据存储（供管理面 API 使用）
func (s *Server) Meta() *MetaStore {
	return s.meta
}

// ModelCache 返回模型详情缓存（供管理面 API 使用）
func (s *Server) ModelDetailCache() *ModelCache {
	return s.modelCache
}

// ResponseCache 返回响应缓存（供管理面统计/清空）
func (s *Server) ResponseCache() *engine.ResponseCache {
	return s.cache
}

// Budget 返回预算记账器（供管理面余额预警）
func (s *Server) Budget() *storage.Budget {
	return s.budget
}

// Catalog 返回模型参考库（供价格同步器重载）
func (s *Server) Catalog() *engine.Catalog {
	return s.catalog
}

// Penalty 返回动态惩罚追踪器（供管理面/调试查看）
func (s *Server) Penalty() *engine.PenaltyTracker {
	return s.penalty
}

// Circuits 返回熔断器池
func (s *Server) Circuits() *engine.CircuitPool {
	return s.circuits
}

// Scorer 返回质量评分器
func (s *Server) Scorer() *engine.Scorer {
	return s.scorer
}

// RegisterAdminRoutes 注册管理面 API 路由（Phase 2）
// 安全：所有 /api/* 管理接口统一要求 admin_key 鉴权，避免匿名篡改配置或消耗上游额度。
// 例外：GET /api/config 放行——前端首屏需要匿名拉取 admin_key 用于后续所有鉴权调用，
// 若也加鉴权会导致面板无法 bootstrap 而整页不可用（其余接口仍走鉴权）。
func (s *Server) RegisterAdminRoutes(handler *api.AdminHandler) {
	adminMux := http.NewServeMux()
	handler.RegisterRoutes(adminMux)
	s.mux.Handle("/api/", s.adminAuth(adminMux))
}

// adminAuth 管理面鉴权中间件：除 GET /api/config 外，所有 /api/* 必须携带 Bearer admin_key。
func (s *Server) adminAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 前端 bootstrap 端点放行（仅 GET）
		if r.URL.Path == "/api/config" && r.Method == http.MethodGet {
			next.ServeHTTP(w, r)
			return
		}
		if !s.authenticateAdmin(r) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"detail":"Unauthorized"}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"detail":"Not Found"}`))
		return
	}
	data, err := os.ReadFile(filepath.Join(s.htdocsDir(), "index.html"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// clientAuth 客户端鉴权结果
type clientAuth struct {
	authed        bool
	vkey          *storage.VKey // 非 nil 表示以虚拟密钥鉴权（需配额记录/限制）
	quotaExceeded bool           // 虚拟密钥当日配额已耗尽
}

// authClient 验证客户端 API Key：admin_key 主密钥 或 已启用且未超额的虚拟密钥。
// 用于 /v1/* 代理端点（chat/completions、models）。管理端点仍仅认 admin_key（authenticateAdmin）。
func (s *Server) authClient(r *http.Request) clientAuth {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return clientAuth{}
	}
	parts := strings.SplitN(auth, " ", 2)
	if len(parts) != 2 || parts[0] != "Bearer" {
		return clientAuth{}
	}
	token := parts[1]
	cfg := s.cfg.Load()
	if token == cfg.AdminKey() {
		return clientAuth{authed: true}
	}
	if s.vkeys != nil {
		if vk := s.vkeys.Validate(token); vk != nil {
			if s.vkeys.QuotaExceeded(vk) {
				return clientAuth{quotaExceeded: true}
			}
			return clientAuth{authed: true, vkey: vk}
		}
	}
	return clientAuth{}
}

// VKeyStore 返回虚拟密钥存储（供 main.go 注入）
func (s *Server) VKeyStore() *storage.VKeyStore {
	return s.vkeys
}

// SetVKeyStore 注入虚拟密钥存储（main.go 装配时调用）
func (s *Server) SetVKeyStore(v *storage.VKeyStore) {
	s.vkeys = v
}

// recordVKeyUsage 记录虚拟密钥的当日用量（请求数 + Token 数）
func (s *Server) recordVKeyUsage(vk *storage.VKey, reqs, toks int) {
	if s.vkeys != nil && vk != nil && (reqs > 0 || toks > 0) {
		s.vkeys.RecordUsage(vk.ID, reqs, toks)
	}
}

// authenticateAdmin 验证管理面密钥
func (s *Server) authenticateAdmin(r *http.Request) bool {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return false
	}
	parts := strings.SplitN(auth, " ", 2)
	if len(parts) != 2 || parts[0] != "Bearer" {
		return false
	}
	return parts[1] == s.cfg.Load().AdminKey()
}
