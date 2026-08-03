package proxy

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	cfg              atomic.Pointer[config.Config]
	mux              *http.ServeMux
	router           atomic.Pointer[engine.Router]
	httpClient       *http.Client
	httpClientStream *http.Client // 流式专用：无整体超时，仅由请求 context（客户端断开）取消，避免长 SSE 被 30s 截断（S9）
	meta             *MetaStore
	modelCache       *ModelCache
	modelsCache      *modelsCache
	usage            *storage.Usage
	circuits         *engine.CircuitPool
	scorer           *engine.Scorer
	catalog          *engine.Catalog
	cache            *engine.ResponseCache             // 响应缓存（语义 simhash）
	budget           *storage.Budget                   // 预算/余额护栏
	guard            *ConcurrencyGuard                 // 并发护栏
	metrics          *engine.Metrics                   // Prometheus /metrics 指标注册表
	penalty          *engine.PenaltyTracker            // 动态惩罚路由（429/5xx 临时降权）
	cooldown         *engine.CooldownTracker           // 精确冷却感知（C1，解析 Retry-After）
	lkgp             *engine.LastGoodTracker           // 末次成功优先（C2）
	affinity         *engine.SessionAffinity           // 会话亲和（C3）
	idem             *idemStore                        // 幂等键存储（Idempotency-Key）
	vkeys            atomic.Pointer[storage.VKeyStore] // 虚拟密钥（子密钥）存储（P2-7: atomic 保证并发安全）
	ssrfStrict       atomic.Bool                       // SSRF 严格模式（从配置热更新读取）
	budgetAlert      budgetAlertState                  // A11：预算阈值告警去重（每日每档只发一次 hook）
	appDir           string
	httpServer       *http.Server
	// P1-3：CSRF 防护。每进程启动生成一个随机令牌；写操作（POST/PUT/DELETE/PATCH）
	// 必须携带 X-CSRF-Token 头，配合 Bearer 鉴权阻断跨站伪造请求。
	csrfSecret []byte
	csrfToken  string
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

	// 创建绕过本地/内网代理的 HTTP 客户端（内置 SSRF 出站防护）
	s := &Server{
		mux:         http.NewServeMux(),
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
		cooldown:    engine.NewCooldownTracker(),
		lkgp:        engine.NewLastGoodTracker(dataDir),
		affinity:    engine.NewSessionAffinity(),
		idem:        newIdemStore(),
		appDir:      appDir,
	}
	// SSRF 严格模式从配置热更新读取；注入上游 HTTP 传输的拨号层
	s.ssrfStrict.Store(cfg.SSRFStrict())
	s.httpClient = newLocalBypassClient(30*time.Second, func() bool { return s.ssrfStrict.Load() })
	// 流式专用客户端：去掉整体 Timeout（非流式仍走带 30s 超时的 httpClient 兜底），
	// 流式长连接仅由请求的 context（客户端断开即 r.Context() 取消）控制，避免 SSE 被 30s 截断（S9）。
	s.httpClientStream = newLocalBypassClient(0, func() bool { return s.ssrfStrict.Load() })
	s.cfg.Store(cfg)
	// P1-3：生成 CSRF 令牌（进程级随机值）。后端重启后令牌刷新，
	// 前端检测到 403 会自动重新拉取（见 SPA 的 fetchCsrf 重试逻辑）。
	csrfSecret := make([]byte, 32)
	if _, err := rand.Read(csrfSecret); err != nil {
		// 极罕见：系统熵源不可用，退化为基于进程启动时间的字节（仍足够随机，仅降级）
		seed := time.Now().UnixNano()
		for i := range csrfSecret {
			csrfSecret[i] = byte(seed >> (8 * (i % 8)))
		}
	}
	s.csrfSecret = csrfSecret
	s.csrfToken = computeCSRFToken(csrfSecret)
	s.initRouter()
	// 注册 engine 生命周期钩子回调：熔断/恢复/配额等事件经现有 REST 钩子分发（HMAC+SSRF 护栏）
	engine.HookSink = func(event, provider, extra string) {
		s.hookDispatcher().Fire(event, provider, provider, false, 0, extra, "")
	}
	s.routes()
	return s
}

// newLocalBypassClient 创建绕过本地/内网代理的 HTTP 客户端
// 当系统设置了 HTTP_PROXY/HTTPS_PROXY 时，对本地/内网地址不走代理，
// 避免 502 连接被中断（如 clash/v2ray 代理无法访问局域网地址）。
// strictFn 在每次拨号时被调用，决定是否启用 SSRF 严格模式（额外拦截 RFC1918 私网）。
// DialContext 内置 SSRF 防护：解析目标主机后只直连非拦截 IP，阻断针对云元数据/
// 回环/链路本地的 SSRF 攻击（默认拦回环/链路本地/云元数据，放行 RFC1918 以兼容局域网自托管）。
func newLocalBypassClient(timeout time.Duration, strictFn func() bool) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:                 netutil.BypassProxyFunc,
			DialContext:           netutil.SSRFSafeDialContext(strictFn),
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
	r.SetCircuitPool(s.circuits)
	r.SetCooldown(s.cooldown)
	r.SetLKGP(s.lkgp)
	r.SetAffinity(s.affinity)
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
		for _, p := range cfg.UsableProviders() {
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

// Serve 在调用方已建立的监听器上启动服务（A12 端口冲突自动重试用）。
// 与 ListenAndServe 的区别：监听器由外部创建，因此「端口是否可用」在启动前就已确定，
// 调用方可以在冲突时换端口重试，而不是等到这里失败才发现。
func (s *Server) Serve(ln net.Listener) error {
	s.httpServer = &http.Server{
		Addr:    ln.Addr().String(),
		Handler: recoverMiddleware(s.mux),
	}
	return s.httpServer.Serve(ln)
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
	s.ssrfStrict.Store(newCfg.SSRFStrict())
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
	s.modelCache.FetchAndCache(s.cfg.Load().UsableProviders(), s.httpClient)
}

func (s *Server) routes() {
	htdocsDir := s.htdocsDir()
	s.mux.HandleFunc("/", s.handleIndex)
	s.mux.HandleFunc("/v1/models", s.clientAllowlist(s.handleModels))
	s.mux.HandleFunc("/v1/chat/completions", s.clientAllowlist(s.handleChatCompletions))
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

// htdocsDir 返回 htdocs 目录（S5：优先 MODEL_GATEWAY_APP 环境变量指定的资源目录，
// 其次默认 ipk 安装位置，最后回退到可执行文件所在目录/appDir，兼容开发环境）。
func (s *Server) htdocsDir() string {
	if env := os.Getenv("MODEL_GATEWAY_APP"); env != "" {
		if dir := filepath.Join(env, "htdocs"); dirExists(dir) {
			return dir
		}
	}
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

// Cooldown 返回精确冷却追踪器（C1）
func (s *Server) Cooldown() *engine.CooldownTracker {
	return s.cooldown
}

// LastGood 返回末次成功优先追踪器（C2）
func (s *Server) LastGood() *engine.LastGoodTracker {
	return s.lkgp
}

// Affinity 返回会话亲和追踪器（C3）
func (s *Server) Affinity() *engine.SessionAffinity {
	return s.affinity
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
	// P1-3：CSRF 令牌端点（经 adminAuth 鉴权；adminAuth 已特例放行 GET 且要求 Bearer）
	adminMux.HandleFunc("/api/csrf", s.handleCSRFToken)
	// G2 A2A 轻量只读协议端点（供外部 Agent 发现/健康/成本查询），只读、无密钥。
	// S12：复用 clientAllowlist（IP 白名单）收口——配置了 allow_clients 时仅放行白名单，
	// 未配置（默认 LAN 开放）时与 /v1/models 口径一致保持开放，避免误暴露但也不阻断合法发现。
	s.mux.HandleFunc("/a2a", s.clientAllowlist(handler.HandleA2A))
	// S8：管理面请求体统一限制 4MB，防止超大 body 耗尽内存（聊天端点不在此 mux，不受影响）
	s.mux.Handle("/api/", s.adminAuth(maxBodyReader(4<<20, adminMux)))
}

// maxBodyReader 限制请求体大小（S8）：超过上限的读取会返回错误，避免恶意/异常超大 body 压垮服务。
func maxBodyReader(limit int64, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		next.ServeHTTP(w, r)
	})
}

// normalizeUsageModel 计费别名归一 + 未收录告警（F9）。
// 统一用 catalog 规范名记录用量，避免别名/内部名分流导致统计偏差；
// 当模型不在参考库时价格为 0、成本被低估，打印告警便于排查（而非静默记为 0）。
func (s *Server) normalizeUsageModel(model string) string {
	norm := s.meta.ResolveModel(model)
	if norm == "" {
		norm = model
	}
	if s.catalog != nil && s.catalog.Lookup(norm) == nil {
		log.Printf("[usage] 模型 %q 未在参考库收录，本次用量按价格 0 计，成本统计可能偏低", norm)
	}
	return norm
}

// computeCSRFToken 由进程级密钥派生固定令牌（HMAC-SHA256）。
func computeCSRFToken(secret []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte("model-gateway-csrf-token"))
	return hex.EncodeToString(mac.Sum(nil))
}

// isWriteMethod 判断是否为会改变状态的方法（需 CSRF 校验）。
func isWriteMethod(m string) bool {
	switch m {
	case http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch:
		return true
	}
	return false
}

// csrfValid 校验请求携带的 X-CSRF-Token 是否与本进程令牌一致（常量时间比较防时序侧信道）。
func (s *Server) csrfValid(r *http.Request) bool {
	tok := r.Header.Get("X-CSRF-Token")
	if tok == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(tok), []byte(s.csrfToken)) == 1
}

// handleCSRFToken 返回当前进程的 CSRF 令牌。需 Bearer 鉴权（adminAuth 已特例放行 GET）。
// 前端 bootstrap 后拉取一次；写操作前在请求头带上 X-CSRF-Token。
func (s *Server) handleCSRFToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]string{"csrf_token": s.csrfToken})
}

// adminAuth 管理面鉴权中间件：除 GET /api/config 外，所有 /api/* 必须携带 Bearer admin_key。
// P2-4: 增加速率限制，防止暴力破解 admin_key。
type authAttempt struct {
	count int
	first time.Time
}

func (s *Server) adminAuth(next http.Handler) http.Handler {
	var (
		mu          sync.Mutex
		attempts    = make(map[string]*authAttempt)
		maxAttempts = 5
		window      = 5 * time.Minute
	)
	// fail 统一处理鉴权失败 + 速率限制（P2-4）。
	fail := func(w http.ResponseWriter, r *http.Request) {
		ip := extractClientIP(r)
		now := time.Now()
		mu.Lock()
		// 惰性清理过期记录
		for k, a := range attempts {
			if now.Sub(a.first) > window {
				delete(attempts, k)
			}
		}
		a, ok := attempts[ip]
		if !ok || now.Sub(a.first) > window {
			attempts[ip] = &authAttempt{count: 1, first: now}
			mu.Unlock()
		} else {
			a.count++
			count := a.count
			mu.Unlock()
			if count > maxAttempts {
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"detail":"Too many failed attempts, try again later"}`))
				return
			}
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"detail":"Unauthorized"}`))
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// P1-3：CSRF 令牌端点自身需要 Bearer，但不要求 X-CSRF-Token（否则死锁）
		if r.URL.Path == "/api/csrf" {
			if s.authenticateAdmin(r) {
				next.ServeHTTP(w, r)
			} else {
				fail(w, r)
			}
			return
		}
		if !s.authenticateAdmin(r) {
			fail(w, r)
			return
		}
		// P1-3：写方法（POST/PUT/DELETE/PATCH）要求携带合法 X-CSRF-Token 头，阻断跨站伪造请求
		if isWriteMethod(r.Method) && !s.csrfValid(r) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"detail":"CSRF token missing or invalid"}`))
			return
		}
		// 认证成功：清除该 IP 的失败计数
		ip := extractClientIP(r)
		mu.Lock()
		delete(attempts, ip)
		mu.Unlock()
		next.ServeHTTP(w, r)
	})
}

// extractClientIP 从请求中提取客户端 IP（优先 X-Forwarded-For）
func extractClientIP(r *http.Request) string {
	ip := r.RemoteAddr
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		ip = strings.Split(xff, ",")[0]
	}
	return strings.TrimSpace(ip)
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
	quotaExceeded bool          // 虚拟密钥当日配额已耗尽
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
	if subtle.ConstantTimeCompare([]byte(token), []byte(cfg.AdminKey())) == 1 {
		return clientAuth{authed: true}
	}
	if s.vkeys.Load() != nil {
		if vk := s.vkeys.Load().Validate(token); vk != nil {
			if s.vkeys.Load().QuotaExceeded(vk) {
				return clientAuth{quotaExceeded: true}
			}
			return clientAuth{authed: true, vkey: vk}
		}
	}
	return clientAuth{}
}

// clientAllowlist 客户端 IP 白名单中间件：仅作用于 /v1/* 代理端点。
// 白名单为空时（默认）全部放行；非空时仅允许列表内 IP/CIDR 调用 AI 代理，
// 其余返回 403。静态管理面板（/ 与 /htdocs/）不受影响。
func (s *Server) clientAllowlist(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.cfg.Load().ClientAllowed(clientIP(r, s.cfg.Load().TrustedNets())) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"client address not allowed by allowlist"}`))
			return
		}
		next(w, r)
	}
}

// VKeyStore 返回虚拟密钥存储（供 main.go 注入）
func (s *Server) VKeyStore() *storage.VKeyStore {
	return s.vkeys.Load()
}

// SetVKeyStore 注入虚拟密钥存储（main.go 装配时调用）
func (s *Server) SetVKeyStore(v *storage.VKeyStore) {
	s.vkeys.Store(v)
}

// recordVKeyUsage 记录虚拟密钥的当日用量（请求数 + Token 数）
func (s *Server) recordVKeyUsage(vk *storage.VKey, reqs, toks int) {
	if s.vkeys.Load() != nil && vk != nil && (reqs > 0 || toks > 0) {
		s.vkeys.Load().RecordUsage(vk.ID, reqs, toks)
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
	return subtle.ConstantTimeCompare([]byte(parts[1]), []byte(s.cfg.Load().AdminKey())) == 1
}
