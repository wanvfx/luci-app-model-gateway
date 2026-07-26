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
}

// Router 路由组配置
type Router struct {
	Name    string
	Members []string
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

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "config ") {
			parts := strings.Fields(line)
			// 兼容命名段 `config provider 'nvidia'`(len>=3) 与匿名段 `config provider`(len==2)。
			// uci add 生成的匿名段在文件里写作 `config provider`（无名字），
			// 若仍要求 len>=3 会整段被跳过，导致一键配置创建的 provider 读不出来。
			if len(parts) >= 2 {
				section = strings.Trim(parts[1], "'\"")
				currentProvider = nil
				currentRouter = nil
				switch section {
				case "model-gateway":
					// settings 段，不做特殊处理
				case "provider":
					currentProvider = &Provider{Enabled: true, FreeOnly: true}
					cfg.Providers = append(cfg.Providers, currentProvider)
				case "router":
					currentRouter = &Router{}
					cfg.Routers = append(cfg.Routers, currentRouter)
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
			}
		case "router":
			if currentRouter == nil {
				currentRouter = &Router{}
				cfg.Routers = append(cfg.Routers, currentRouter)
			}
			switch key {
			case "name":
				currentRouter.Name = val
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
