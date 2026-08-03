package config

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
)

// Config 是 model-gateway 的运行时配置
type Config struct {
	file            string
	port            int
	bindAddr        string // 监听网卡地址（空=0.0.0.0 全网卡，P2-3 可配置 bind_addr）
	adminKey        string
	pollInterval    int
	pollStrategy    string
	headless        bool
	visionAssist    bool
	visionRouter    string
	visionMaxTokens int
	disabledModels  []string
	Providers       []*Provider
	Routers         []*Router
	// Phase 1.5 扩展：别名 / 缓存 / 钩子 / 预算 / 并发
	Aliases        []*Alias     // 友好名 -> 内部模型/路由组/auto
	Cache          CacheConfig  // 响应缓存（语义 simhash）
	Hooks          []*Hook      // REST 回调钩子（iStoreOS 插件机制）
	Budget         BudgetConfig // 预算/余额护栏
	MaxConcurrency int          // 全局并发上限（0=不限）
	// r20260727c 扩展
	piiSanitize bool // PII 正则脱敏（转发上游前对消息内容脱敏手机号/身份证/邮箱/银行卡）
	// r20260727d 扩展
	strictCapability bool // 严格能力矩阵：请求所需能力（如 vision）必须被候选模型支持，否则过滤
	// P2-F7：响应 content 前缀（🤖 provider·model）开关。默认开；关闭则不再注入前缀；
	// 即使开启，结构化输出（JSON 等）也不加前缀，避免污染机器可读结果。
	contentPrefix bool
	// D 阶段安全套件（v1.6.0）
	ssrfStrict      bool        // SSRF 严格模式：额外拦截 RFC1918 私网（默认仅拦回环/链路本地/云元数据）
	allowClients    []string    // 客户端 IP 白名单（空=不限制）；支持裸 IP 或 CIDR
	bannedProviders []string    // 被封禁的 provider 名称（永不参与路由/探测/模型列表）
	allowNets       []net.IPNet // allowClients 编译后的匹配表（Load 时构建）
	// S7 安全修复：受信任反代列表。仅当 RemoteAddr 命中该列表时才采信 X-Forwarded-For，
	// 否则始终用 RemoteAddr（直连客户端 IP），避免伪造 XFF 绕过 IP 白名单/会话亲和劫持。
	trustedProxies []string    // 受信任反代 IP/CIDR（空=不信任任何 XFF）
	trustedNets    []net.IPNet // trustedProxies 编译后的匹配表（Load 时构建）
	// A15 价格目录远端同步开关：默认开启、24 小时一次。
	// 离线部署或不希望网关自行外联的用户可以关掉，此时只用随包的静态价格表。
	priceSyncEnabled  bool
	priceSyncInterval int // 小时，最小 1
	// B 方案：免费模型自动巡检开关：默认开启、24 小时一次。
	freeModelGuardEnabled  bool
	freeModelGuardInterval int // 小时，最小 1
	// S6：UCI 目录路径（空=/etc/config 默认）
	uciDir string
}

// ProviderBanned 返回 provider 是否被安全封禁（供前端展示徽标）。
func (p *Provider) ProviderBanned() bool {
	return p.Banned
}

// Provider 上游提供商配置
type Provider struct {
	Name           string
	BaseURL        string
	APIKey         string
	Models         []string
	DisabledModels []string
	Enabled        bool
	FreeOnly       bool
	MaxConcurrency int    // 单 provider 并发上限（0=不限，继承全局）
	AuthHeader     string // 鉴权头名（默认 Authorization；如 x-goog-api-key / api-key）
	AuthScheme     string // 鉴权值前缀（默认 Bearer；"none" 或空+自定义头 = 裸密钥）
	Format         string // 上游协议格式：openai（默认）| gemini（原生 generateContent 协议）| claude（原生 /v1/messages 协议）| openai-responses（原生 /v1/responses 协议），F 阶段
	// 适配器格式：duckduckgo | theoldllm | felo | mimocode | text-plain（由 adapters_builtin.go 注册）
	ThinkingBudget int  // 思考预算（token 上限）：>0 时对支持推理的模型注入（claude: thinking.budget_tokens；gemini: thinkingConfig.thinkingBudget）；openai 推理模型走原生参数，此处不注入
	Banned         bool // 安全封禁：被 banned_providers 命中，永不参与路由（优先于 Enabled）
	// NoAuth 标识该提供者为免 Key 提供者（与 OmniRoute 的 synthetic noauth 对齐）。
	// 为 true 时：ApplyAuth 不会注入鉴权头；日志/统计可追踪 no_auth=true。
	NoAuth bool
	// AuthOptional 标识该提供者支持"可选鉴权"（对应 OmniRoute authType: "optional"）。
	// 为 true 时：首次请求仍注入 APIKey（若有）；若上游返回 401，则自动去掉鉴权头重试一次，
	// 让免费模型即使用户填了无效 key 也能工作（如 Pollinations 免费层）。
	AuthOptional bool
	// AnonymousAPIKey 为免 Key 提供者提供 documented anonymous key（如 AI Horde 的 "0000000000"）。
	// 当 APIKey 为空且 AnonymousAPIKey 非空时，ApplyAuth 会注入 Bearer <AnonymousAPIKey>。
	AnonymousAPIKey string
}

// FormatOrDefault 返回上游协议格式，空值时回退 openai（零行为变更保障）。
func (p *Provider) FormatOrDefault() string {
	if strings.TrimSpace(p.Format) == "" {
		return "openai"
	}
	return strings.TrimSpace(p.Format)
}

// AuthHeaderValue 根据鉴权配置生成 (头名, 头值)。
// 规则：
//   - apiKey 为空 → 返回空（免 Key 提供者不注入任何鉴权头）；
//   - headerName 为空 → 默认 Authorization；
//   - scheme 为空且头为 Authorization → 默认 Bearer（向后兼容既有行为）；
//   - scheme 为空且头为自定义（如 x-goog-api-key）→ 裸密钥；
//   - scheme == "none" → 强制裸密钥。
func AuthHeaderValue(headerName, scheme, apiKey string) (string, string) {
	if strings.TrimSpace(apiKey) == "" {
		return "", ""
	}
	h := strings.TrimSpace(headerName)
	if h == "" {
		h = "Authorization"
	}
	s := strings.TrimSpace(scheme)
	if s == "" && strings.EqualFold(h, "Authorization") {
		s = "Bearer"
	}
	if s == "" || strings.EqualFold(s, "none") {
		return h, apiKey
	}
	return h, s + " " + apiKey
}

// ApplyAuth 将该提供者的上游鉴权头注入 http.Header（chat 转发 / models 探测 / poller 统一走这里）。
// 规则：
//   - NoAuth=true（免 Key 提供者）：不注入用户 APIKey，仅注入 AnonymousAPIKey（如 AI Horde 的 "0000000000"）。
//     这确保免 Key 提供者即使用户误填了 key 也不会将其注入上游（避免无效 key 导致 401）。
//   - NoAuth=false（常规/可选鉴权提供者）：优先使用 APIKey；若为空且存在 AnonymousAPIKey，则注入 anonymous key。
//   - 两者皆空时不注入任何头，保持无认证请求。
//   - AuthOptional=true 时，首次请求仍注入 APIKey（若有）；若上游返回 401，由 chat.go/stream.go 自动去 auth 重试。
func (p *Provider) ApplyAuth(h http.Header) {
	if p.NoAuth {
		// 免 Key 提供者：仅注入 documented anonymous key（如有），不注入用户 key。
		if p.AnonymousAPIKey == "" {
			return
		}
		name, val := AuthHeaderValue(p.AuthHeader, p.AuthScheme, p.AnonymousAPIKey)
		if name != "" && val != "" {
			h.Set(name, val)
		}
		return
	}
	apiKey := p.APIKey
	if apiKey == "" {
		apiKey = p.AnonymousAPIKey
	}
	name, val := AuthHeaderValue(p.AuthHeader, p.AuthScheme, apiKey)
	if name != "" && val != "" {
		h.Set(name, val)
	}
}

// Alias 别名映射：Name（对外友好名） -> Target（内部模型前缀名 / 路由组名 / auto）
type Alias struct {
	Name   string `json:"name"`
	Target string `json:"target"`
}

// CacheConfig 响应缓存配置（落 MODEL_GATEWAY_DATA/cache，不新增 UCI dataDir 字段）
type CacheConfig struct {
	Enabled    bool `json:"enabled"`
	TTL        int  `json:"ttl"`         // 秒，默认 300
	MaxEntries int  `json:"max_entries"` // 默认 1000
	Semantic   bool `json:"semantic"`    // 语义（simhash 近重复）开关，默认开
}

// Hook REST 回调钩子配置（iStoreOS 唯一插件机制：Go 对外发 HTTP，CGO-free）
type Hook struct {
	URL     string   `json:"url"`
	Events  []string `json:"events"` // request_done / request_failed
	Enabled bool     `json:"enabled"`
	Secret  string   `json:"secret"` // 可选 HMAC 校验密钥（注入 X-Hook-Secret 头）
}

// BudgetConfig 预算/余额护栏
type BudgetConfig struct {
	DailyLimitUSD float64 `json:"daily_limit_usd"` // 每日成本上限（USD），0=不限
	Action        string  `json:"action"`          // warn（仅告警）| block（超限拦截）
	WarningPct    int     `json:"warning_pct"`     // 预警百分比（默认 80）
}

// Router 路由组配置
// Strategy 候选排序策略：
//   - quality      质量分排序（默认，向后兼容旧配置）
//   - priority     严格按 Members 顺序（尊重用户拖拽的优先级）
//   - least-latency 按巡检平均延迟升序
//   - cost         按参考库价格升序（免费优先）
//   - loadbalance  轮转起点（简单负载均衡）
type Router struct {
	Name     string
	Members  []string
	Strategy string
	Weights  []int // weighted 策略的成员权重（与 Members 一一对应；缺省/不足视为 1）
}

// ValidStrategy 校验策略值是否合法
func ValidStrategy(s string) bool {
	switch s {
	case "quality", "priority", "least-latency", "cost", "loadbalance", "weighted", "classify":
		return true
	}
	return false
}

// ValidFormat 校验上游协议格式是否合法。
// 允许：openai（默认）| gemini | claude | openai-responses（原生协议格式）|
//
//	duckduckgo | theoldllm | felo | mimocode | text-plain（内置适配器格式）。
//
// 适配器格式由 proxy/translator/adapters_builtin.go 注册，必须在白名单中放行，
// 否则 UCI 重载时 format 被静默丢弃 → 回落 openai → adapter 不生效 → 提供者失效。
func ValidFormat(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "openai", "gemini", "claude", "openai-responses",
		"duckduckgo", "theoldllm", "felo", "mimocode", "text-plain":
		return true
	}
	return false
}

// ValidPort 校验 TCP 端口是否在合法范围内（S13）。
// 0 与负数不可监听，>65535 超出协议范围；均视为非法并回落默认值。
func ValidPort(p int) bool {
	return p > 0 && p <= 65535
}

// PrivilegedPort 返回端口是否属于特权端口（<1024）。
// OpenWrt 上守护进程通常以 root 运行故可绑定，但仍值得在界面上提示用户可能与
// 系统服务（如 LuCI 的 80/443、dnsmasq 的 53）冲突。
func PrivilegedPort(p int) bool {
	return p > 0 && p < 1024
}

// AliasMap 返回别名映射表（Name -> Target），供代理层解析
func (c *Config) AliasMap() map[string]string {
	m := make(map[string]string, len(c.Aliases))
	for _, a := range c.Aliases {
		if a.Name != "" && a.Target != "" {
			m[a.Name] = a.Target
		}
	}
	return m
}

// CacheDefaults 应用缓存配置默认值（缺字段时）
func (c *Config) EffectiveCache() CacheConfig {
	cc := c.Cache
	if cc.TTL <= 0 {
		cc.TTL = 300
	}
	if cc.MaxEntries <= 0 {
		cc.MaxEntries = 1000
	}
	return cc
}

// EffectiveBudget 应用预算配置默认值
func (c *Config) EffectiveBudget() BudgetConfig {
	b := c.Budget
	if b.Action != "block" {
		b.Action = "warn"
	}
	if b.WarningPct <= 0 {
		b.WarningPct = 80
	}
	return b
}

// Load 解析 /etc/config/model-gateway（轻量 UCI 子集解析）
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	cfg := &Config{
		file:            path,
		port:            8080,
		pollInterval:    300,
		pollStrategy:    "limited", // 默认 1.6.1 智能省电策略（20 次后休眠 + 每日复查）
		headless:        true,
		visionMaxTokens: 0, // 0 表示使用默认值（16384）
		disabledModels:  []string{},
		contentPrefix:   true, // F7：响应前缀默认开启（"0" 可关），但结构化输出仍不加前缀
		// A15：价格目录远端同步，默认开启、24 小时一轮
		priceSyncEnabled:  true,
		priceSyncInterval: 24,
		// B 方案：免费模型自动巡检，默认开启、24 小时一轮
		freeModelGuardEnabled:  true,
		freeModelGuardInterval: 24,
	}

	var section string
	var currentProvider *Provider
	var currentRouter *Router
	var currentAlias *Alias
	var currentHook *Hook

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "config ") {
			parts := strings.Fields(line)
			// 兼容命名段 `config provider 'nvidia'`(len>=3) 与匿名段 `config provider`(len==2)。
			if len(parts) >= 2 {
				section = strings.Trim(parts[1], "'\"")
				currentProvider = nil
				currentRouter = nil
				currentAlias = nil
				currentHook = nil
				switch section {
				case "model-gateway":
					// settings 段，不做特殊处理
				case "provider":
					currentProvider = &Provider{Enabled: true, FreeOnly: true}
					cfg.Providers = append(cfg.Providers, currentProvider)
				case "router":
					currentRouter = &Router{}
					cfg.Routers = append(cfg.Routers, currentRouter)
				case "alias":
					currentAlias = &Alias{}
					cfg.Aliases = append(cfg.Aliases, currentAlias)
				case "hook":
					currentHook = &Hook{Enabled: true}
					cfg.Hooks = append(cfg.Hooks, currentHook)
				}
			}
			continue
		} else {
			log.Printf("[uci-load] 跳过无法解析的 config 段声明: %q", line)
		}
		if strings.HasPrefix(line, "list ") {
			kv := strings.SplitN(line[len("list "):], " ", 2)
			if len(kv) != 2 {
				// P3-2：配置损坏时打印告警，便于定位（此前静默跳过，问题难排查）
				log.Printf("[uci-load] 跳过无法解析的 list 行: %q", line)
				continue
			}
			key := kv[0]
			val := strings.Trim(kv[1], "'\"")
			if section == "provider" && currentProvider != nil && key == "models" {
				currentProvider.Models = append(currentProvider.Models, val)
			}
			if section == "provider" && currentProvider != nil && key == "disabled_models" {
				currentProvider.DisabledModels = append(currentProvider.DisabledModels, val)
			}
			if section == "model-gateway" && key == "disabled_models" {
				cfg.disabledModels = append(cfg.disabledModels, val)
			}
			if section == "router" && currentRouter != nil && key == "members" {
				currentRouter.Members = append(currentRouter.Members, val)
			}
			if section == "router" && currentRouter != nil && key == "weights" {
				if v, err := strconv.Atoi(val); err == nil && v >= 0 {
					currentRouter.Weights = append(currentRouter.Weights, v)
				} else {
					currentRouter.Weights = append(currentRouter.Weights, 1)
				}
			}
			if section == "hook" && currentHook != nil && key == "events" {
				currentHook.Events = append(currentHook.Events, val)
			}
			if section == "model-gateway" && key == "allow_clients" {
				cfg.allowClients = append(cfg.allowClients, val)
			}
			if section == "model-gateway" && key == "banned_providers" {
				cfg.bannedProviders = append(cfg.bannedProviders, val)
			}
			if section == "model-gateway" && key == "trusted_proxies" {
				cfg.trustedProxies = append(cfg.trustedProxies, val)
			}
			continue
		}
		if !strings.HasPrefix(line, "option ") {
			continue
		}
		kv := strings.SplitN(line[len("option "):], " ", 2)
		if len(kv) != 2 {
			// P3-2：配置损坏时打印告警，便于定位
			log.Printf("[uci-load] 跳过无法解析的 option 行: %q", line)
			continue
		}
		key := kv[0]
		val := strings.Trim(kv[1], "'\"")
		switch section {
		case "model-gateway":
			switch key {
			case "port":
				// S13：端口范围校验。非法值（<=0 / >65535 / 非数字）一律忽略并保留默认 8080，
				// 避免把 0 或 70000 这种写进配置后守护进程直接起不来却查不出原因。
				if v, err := strconv.Atoi(val); err == nil && ValidPort(v) {
					cfg.port = v
				}
			case "price_sync_enabled":
				// A15：远端价格同步开关。默认开启，仅显式写 0/false/off 才关闭
				// （与其它布尔项相反的默认值，因此这里判「关」而不是判「开」）。
				lv := strings.ToLower(strings.TrimSpace(val))
				cfg.priceSyncEnabled = !(lv == "0" || lv == "false" || lv == "off" || lv == "no")
			case "price_sync_interval":
				// A15：同步间隔（小时），最小 1 小时，防止误填 0 造成空转
				if v, err := strconv.Atoi(val); err == nil && v >= 1 {
					cfg.priceSyncInterval = v
				}
			case "free_model_guard_enabled":
				// B 方案：免费模型巡检开关。默认开启，仅显式写 0/false/off 才关闭
				lv := strings.ToLower(strings.TrimSpace(val))
				cfg.freeModelGuardEnabled = !(lv == "0" || lv == "false" || lv == "off" || lv == "no")
			case "free_model_guard_interval":
				// B 方案：巡检间隔（小时），最小 1 小时
				if v, err := strconv.Atoi(val); err == nil && v >= 1 {
					cfg.freeModelGuardInterval = v
				}
			case "bind_addr":
				// S3：监听网卡地址（如 127.0.0.1 仅本机、0.0.0.0 全网卡默认）。
				// 接受非空 host/IP/主机名，但拒绝含端口冒号的值（端口由 port 选项单独控制）；
				// 非法值忽略，保持默认 0.0.0.0。
				trimmed := strings.TrimSpace(val)
				if trimmed != "" && !strings.Contains(trimmed, ":") {
					cfg.bindAddr = trimmed
				}
			case "uci_dir":
				// S6：UCI 工作目录（如 /etc/config），用于非标准安装路径。
				// 空值忽略，保持默认 /etc/config。
				trimmed := strings.TrimSpace(val)
				if trimmed != "" {
					cfg.uciDir = trimmed
				}
			case "admin_key":
				cfg.adminKey = val
			case "poll_interval":
				if v, err := strconv.Atoi(val); err == nil {
					cfg.pollInterval = v
				}
			case "poll_strategy":
				// 仅接受合法值，非法值忽略保持默认
				if val == "limited" || val == "continuous" {
					cfg.pollStrategy = val
				}
			case "headless":
				cfg.headless = val != "0"
			case "vision_assist":
				cfg.visionAssist = val == "1" || strings.ToLower(val) == "true"
			case "vision_router":
				cfg.visionRouter = val
			case "vision_max_tokens":
				if v, err := strconv.Atoi(val); err == nil && v > 0 {
					cfg.visionMaxTokens = v
				}
			case "cache_enabled":
				cfg.Cache.Enabled = val != "0"
			case "cache_ttl":
				if v, err := strconv.Atoi(val); err == nil {
					cfg.Cache.TTL = v
				}
			case "cache_max_entries":
				if v, err := strconv.Atoi(val); err == nil {
					cfg.Cache.MaxEntries = v
				}
			case "cache_semantic":
				cfg.Cache.Semantic = val != "0"
			case "budget_daily_limit":
				if v, err := strconv.ParseFloat(val, 64); err == nil {
					cfg.Budget.DailyLimitUSD = v
				}
			case "budget_action":
				if val == "block" || val == "warn" {
					cfg.Budget.Action = val
				}
			case "budget_warning_pct":
				if v, err := strconv.Atoi(val); err == nil {
					cfg.Budget.WarningPct = v
				}
			case "max_concurrency":
				if v, err := strconv.Atoi(val); err == nil {
					cfg.MaxConcurrency = v
				}
			case "pii_sanitize":
				cfg.piiSanitize = val == "1" || strings.ToLower(val) == "true"
			case "strict_capability":
				cfg.strictCapability = val == "1" || strings.ToLower(val) == "true"
			case "ssrf_strict":
				cfg.ssrfStrict = val == "1" || strings.ToLower(val) == "true"
			case "content_prefix":
				// F7：响应前缀开关（默认开；"0" 关闭）
				cfg.contentPrefix = val != "0"
			}
		case "provider":
			if currentProvider == nil {
				currentProvider = &Provider{Enabled: true, FreeOnly: true}
				cfg.Providers = append(cfg.Providers, currentProvider)
			}
			switch key {
			case "name":
				currentProvider.Name = val
			case "base_url":
				currentProvider.BaseURL = val
			case "api_key":
				currentProvider.APIKey = val
			case "enabled":
				currentProvider.Enabled = val != "0"
			case "free_only":
				currentProvider.FreeOnly = val != "0"
			case "max_concurrency":
				if v, err := strconv.Atoi(val); err == nil {
					currentProvider.MaxConcurrency = v
				}
			case "auth_header":
				currentProvider.AuthHeader = val
			case "auth_scheme":
				currentProvider.AuthScheme = val
			case "format":
				// 上游协议格式：openai（默认）| gemini | claude | openai-responses。
				// 仅接受白名单枚举（F6），非法值忽略保持默认 openai，避免错配未知格式。
				if ValidFormat(val) {
					currentProvider.Format = strings.ToLower(strings.TrimSpace(val))
				}
			case "thinking_budget":
				if v, err := strconv.Atoi(val); err == nil {
					currentProvider.ThinkingBudget = v
				}
			case "no_auth":
				currentProvider.NoAuth = val != "0" && val != "false"
			case "auth_optional":
				currentProvider.AuthOptional = val != "0" && val != "false"
			case "anonymous_api_key":
				currentProvider.AnonymousAPIKey = val
			}
		case "router":
			if currentRouter == nil {
				currentRouter = &Router{}
				cfg.Routers = append(cfg.Routers, currentRouter)
			}
			switch key {
			case "name":
				currentRouter.Name = val
			case "strategy":
				// 仅接受合法值，非法值忽略保持默认（quality）
				if ValidStrategy(val) {
					currentRouter.Strategy = val
				}
			}
		case "alias":
			if currentAlias == nil {
				currentAlias = &Alias{}
				cfg.Aliases = append(cfg.Aliases, currentAlias)
			}
			switch key {
			case "name":
				currentAlias.Name = val
			case "target":
				currentAlias.Target = val
			}
		case "hook":
			if currentHook == nil {
				currentHook = &Hook{Enabled: true}
				cfg.Hooks = append(cfg.Hooks, currentHook)
			}
			switch key {
			case "url":
				currentHook.URL = val
			case "enabled":
				currentHook.Enabled = val != "0"
			case "secret":
				currentHook.Secret = val
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	// 安全套件（v1.6.0）：封禁 provider 打标 + 编译 IP 白名单匹配表
	for _, p := range cfg.Providers {
		p.Banned = containsStr(cfg.bannedProviders, p.Name)
	}
	cfg.allowNets = compileAllowNets(cfg.allowClients)
	cfg.trustedNets = compileAllowNets(cfg.trustedProxies)
	if cfg.adminKey == "" {
		// P3-3: genKey 失败会返回 error；admin_key 生成失败属于致命（无法提供随机管理密钥），禁止静默降级为弱密钥。
		key, err := genKey()
		if err != nil {
			return nil, err
		}
		cfg.adminKey = key
	}
	return cfg, nil
}

// BindAddr 返回监听地址（P2-3：bind_addr 可配置，空=0.0.0.0 全网卡）
func (c *Config) BindAddr() string {
	addr := c.bindAddr
	if addr == "" {
		addr = "0.0.0.0"
	}
	return addr + ":" + strconv.Itoa(c.port)
}

// BindHost 返回监听主机部分（不含端口），空配置回落 0.0.0.0 全网卡（P2-3）。
// 供 A12 端口冲突自愈时自行建立监听器使用。
func (c *Config) BindHost() string {
	if c.bindAddr == "" {
		return "0.0.0.0"
	}
	return c.bindAddr
}

// Port 返回端口
func (c *Config) Port() int {
	return c.port
}

// SetPort 更新内存中的端口（A12：实际监听端口与配置不一致时同步，
// 保证 /api/config 等接口对外报告的是真实生效端口）。
func (c *Config) SetPort(p int) {
	if ValidPort(p) {
		c.port = p
	}
}

// UciDir 返回 uci 工作目录（S6：空=/etc/config 默认）。
func (c *Config) UciDir() string {
	if c.uciDir == "" {
		return "/etc/config"
	}
	return c.uciDir
}

// PriceSyncEnabled 返回价格目录远端同步是否启用（A15，默认 true）
func (c *Config) PriceSyncEnabled() bool {
	return c.priceSyncEnabled
}

// PriceSyncInterval 返回价格同步间隔（小时，A15，默认 24，最小 1）
func (c *Config) PriceSyncInterval() int {
	if c.priceSyncInterval < 1 {
		return 24
	}
	return c.priceSyncInterval
}

// FreeModelGuardEnabled 返回免费模型自动巡检是否启用（B 方案，默认 true）
func (c *Config) FreeModelGuardEnabled() bool {
	return c.freeModelGuardEnabled
}

// FreeModelGuardInterval 返回免费模型巡检间隔（小时，B 方案，默认 24，最小 1）
func (c *Config) FreeModelGuardInterval() int {
	if c.freeModelGuardInterval < 1 {
		return 24
	}
	return c.freeModelGuardInterval
}

// AdminKey 返回管理面密钥
func (c *Config) AdminKey() string {
	return c.adminKey
}

// SetAdminKey 更新内存中的管理面密钥（用于启动时迁移旧格式 key 后同步内存，
// 避免出现「UCI 文件已是新 key、但守护进程内存仍是旧 key」的不一致）
func (c *Config) SetAdminKey(key string) {
	c.adminKey = key
}

// PollInterval 返回巡检间隔（秒）
func (c *Config) PollInterval() int {
	return c.pollInterval
}

// PollStrategy 返回轮询策略：limited（1.6.1 智能省电，默认）或 continuous（1.6.0 持续监控）
func (c *Config) PollStrategy() string {
	if c.pollStrategy == "continuous" {
		return "continuous"
	}
	return "limited"
}

// Headless 是否无头模式
func (c *Config) Headless() bool {
	return c.headless
}

// VisionAssist 是否启用识图辅助
func (c *Config) VisionAssist() bool {
	return c.visionAssist
}

// SetVisionAssist 设置识图辅助开关
func (c *Config) SetVisionAssist(enabled bool) {
	c.visionAssist = enabled
}

// VisionRouter 返回视觉路由组名
func (c *Config) VisionRouter() string {
	return c.visionRouter
}

// VisionMaxTokens 返回识图模型 max_tokens 上限（0 表示使用默认值 16384）
func (c *Config) VisionMaxTokens() int {
	if c.visionMaxTokens <= 0 {
		return 16384
	}
	return c.visionMaxTokens
}

// DisabledModels 返回禁用模型列表
func (c *Config) DisabledModels() []string {
	return c.disabledModels
}

// AllDisabledModels 返回全局禁用模型 + 各 provider 各自禁用的模型。
// 对齐 Python 原版：toggle-model 写入的是 per-provider disabled_models，
// 代理层（聊天/列表/路由）必须同时消费全局与 per-provider 两份禁用列表，
// 否则通过 toggle-model 或 UCI CLI 设置的 per-provider 禁用会静默失效。
func (c *Config) AllDisabledModels() []string {
	out := make([]string, 0, len(c.disabledModels))
	out = append(out, c.disabledModels...)
	for _, p := range c.Providers {
		out = append(out, p.DisabledModels...)
	}
	return out
}

// PIISanitize 是否启用 PII 正则脱敏
func (c *Config) PIISanitize() bool {
	return c.piiSanitize
}

// StrictCapability 是否启用严格能力矩阵
func (c *Config) StrictCapability() bool {
	return c.strictCapability
}

// ContentPrefix 是否启用响应 content 前缀（F7）
func (c *Config) ContentPrefix() bool {
	return c.contentPrefix
}

// SSRFStrict 是否启用 SSRF 严格模式（额外拦截 RFC1918 私网）
func (c *Config) SSRFStrict() bool {
	return c.ssrfStrict
}

// AllowClients 返回原始 IP 白名单配置（裸 IP 或 CIDR 文本）
func (c *Config) AllowClients() []string {
	return c.allowClients
}

// TrustedNets 返回受信任反代编译后的匹配表（S7）。仅当客户端直连地址命中该表时，
// 代理层才采信 X-Forwarded-For 头，否则一律使用 RemoteAddr，防止伪造 XFF 绕过鉴权/白名单。
func (c *Config) TrustedNets() []net.IPNet {
	return c.trustedNets
}

// BannedProviders 返回被封禁的 provider 名称列表
func (c *Config) BannedProviders() []string {
	return c.bannedProviders
}

// ClientAllowed 判断客户端 IP 是否在白名单内。
// 白名单为空时放行全部（向后兼容，默认不限制任何客户端）。
func (c *Config) ClientAllowed(ipStr string) bool {
	if len(c.allowNets) == 0 {
		return true
	}
	ip := net.ParseIP(strings.TrimSpace(ipStr))
	if ip == nil {
		return false
	}
	for _, n := range c.allowNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// UsableProviders 返回当前可参与路由/探测/模型列表的 provider：
// 排除被封禁（Banned）与显式禁用（Enabled=false）的 provider。
// 安全套件「Provider 封禁」的统一出口——全链路调用此函数而非 Providers，
// 确保被封禁 provider 在任何路径都不会被选中。
func (c *Config) UsableProviders() []*Provider {
	out := make([]*Provider, 0, len(c.Providers))
	for _, p := range c.Providers {
		if p.Banned || !p.Enabled {
			continue
		}
		out = append(out, p)
	}
	return out
}

// containsStr 判断字符串切片是否包含（去空格后精确匹配）目标串。
func containsStr(list []string, s string) bool {
	s = strings.TrimSpace(s)
	for _, v := range list {
		if strings.TrimSpace(v) == s {
			return true
		}
	}
	return false
}

// compileAllowNets 将 allow_clients 文本列表编译为可匹配的 IPNet 表。
// 支持裸 IP（自动补 /32 或 /128）与 CIDR；无法解析的条目忽略。
func compileAllowNets(list []string) []net.IPNet {
	var nets []net.IPNet
	for _, s := range list {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if !strings.Contains(s, "/") {
			ip := net.ParseIP(s)
			if ip == nil {
				continue
			}
			if ip.To4() != nil {
				if _, n, err := net.ParseCIDR(s + "/32"); err == nil {
					nets = append(nets, *n)
				}
			} else if _, n, err := net.ParseCIDR(s + "/128"); err == nil {
				nets = append(nets, *n)
			}
			continue
		}
		if _, n, err := net.ParseCIDR(s); err == nil {
			nets = append(nets, *n)
		}
	}
	return nets
}

// DecryptAPIKeys 对全部 provider 的 api_key 应用解密函数（"enc:" 前缀密文 → 明文）。
// dec 由调用方注入（storage.Vault.Decrypt），config 包不依赖 storage，避免循环引用。
// 解密失败（dec 返回空串）时保留空串，宁可上游 401 也不把密文当明文发出去。
func (c *Config) DecryptAPIKeys(dec func(string) string) {
	if dec == nil {
		return
	}
	for _, p := range c.Providers {
		if strings.HasPrefix(p.APIKey, "enc:") {
			p.APIKey = dec(p.APIKey)
		}
	}
}

// 生成随机密钥（crypto/rand），格式：sk-local- + 32 位十六进制。
// 返回 error（P3-3）：crypto/rand 失败不再退化为确定性弱密钥，而是让调用方感知并中断启动。
func genKey() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("crypto/rand failed to generate admin_key: %w", err)
	}
	return "sk-local-" + hex.EncodeToString(buf), nil
}
