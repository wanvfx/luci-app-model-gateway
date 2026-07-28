package config

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"os"
	"strconv"
	"strings"
)

// Config 是 model-gateway 的运行时配置
type Config struct {
	file            string
	port            int
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
	Aliases        []*Alias      // 友好名 -> 内部模型/路由组/auto
	Cache          CacheConfig   // 响应缓存（语义 simhash）
	Hooks          []*Hook       // REST 回调钩子（iStoreOS 插件机制）
	Budget         BudgetConfig  // 预算/余额护栏
	MaxConcurrency int           // 全局并发上限（0=不限）
	// r20260727c 扩展
	piiSanitize bool // PII 正则脱敏（转发上游前对消息内容脱敏手机号/身份证/邮箱/银行卡）
	// r20260727d 扩展
	strictCapability bool // 严格能力矩阵：请求所需能力（如 vision）必须被候选模型支持，否则过滤
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
	MaxConcurrency int // 单 provider 并发上限（0=不限，继承全局）
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
	Events  []string `json:"events"`  // request_done / request_failed
	Enabled bool     `json:"enabled"`
	Secret  string   `json:"secret"` // 可选 HMAC 校验密钥（注入 X-Hook-Secret 头）
}

// BudgetConfig 预算/余额护栏
type BudgetConfig struct {
	DailyLimitUSD float64 `json:"daily_limit_usd"` // 每日成本上限（USD），0=不限
	Action        string  `json:"action"`         // warn（仅告警）| block（超限拦截）
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
		}
		if strings.HasPrefix(line, "list ") {
			kv := strings.SplitN(line[len("list "):], " ", 2)
			if len(kv) != 2 {
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
			continue
		}
		if !strings.HasPrefix(line, "option ") {
			continue
		}
		kv := strings.SplitN(line[len("option "):], " ", 2)
		if len(kv) != 2 {
			continue
		}
		key := kv[0]
		val := strings.Trim(kv[1], "'\"")
		switch section {
		case "model-gateway":
			switch key {
			case "port":
				if v, err := strconv.Atoi(val); err == nil {
					cfg.port = v
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
	if cfg.adminKey == "" {
		cfg.adminKey = genKey()
	}
	return cfg, nil
}

// BindAddr 返回监听地址
func (c *Config) BindAddr() string {
	return "0.0.0.0:" + strconv.Itoa(c.port)
}

// Port 返回端口
func (c *Config) Port() int {
	return c.port
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

// 生成随机密钥（crypto/rand），格式：sk-local- + 32 位十六进制
func genKey() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		for i := range buf {
			buf[i] = byte('A' + (i*7+3)%26)
		}
	}
	return "sk-local-" + hex.EncodeToString(buf)
}
