package engine

import (
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/wanvfx/luci-app-model-gateway/config"
)

// FreeModelGuard 免费模型自动巡检器（B 方案）
// 定期对 free_only=true 的 provider 重新拉取上游模型列表，自动踢掉不再免费的模型。
type FreeModelGuard struct {
	mu       sync.Mutex
	enabled  bool
	interval time.Duration
	client   *http.Client
	status   struct {
		LastRun   string `json:"last_run"`   // RFC3339，空=从未运行
		LastError string `json:"last_error"` // 最近一次错误
		Checked   int    `json:"checked"`    // 累计检查 provider 数
		Removed   int    `json:"removed"`    // 累计移除模型数
	}
	// 回调（由 main.go 注入，避免 engine 依赖 api）
	getProviders         func() []*config.Provider
	updateProviderModels func(name string, models []string) error
	fetchModels          func(baseURL, apiKey, authHeader, authScheme string, freeOnly bool) ([]string, error)
	logFunc              func(format string, args ...interface{})
}

// NewFreeModelGuard 创建免费模型巡检器
func NewFreeModelGuard(
	getProviders func() []*config.Provider,
	updateProviderModels func(name string, models []string) error,
	fetchModels func(baseURL, apiKey, authHeader, authScheme string, freeOnly bool) ([]string, error),
	client *http.Client,
	logFunc func(format string, args ...interface{}),
) *FreeModelGuard {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	if logFunc == nil {
		logFunc = func(format string, args ...interface{}) {}
	}
	return &FreeModelGuard{
		client:               client,
		getProviders:         getProviders,
		updateProviderModels: updateProviderModels,
		fetchModels:          fetchModels,
		logFunc:              logFunc,
	}
}

// SetEnabled 设置是否启用自动巡检
func (g *FreeModelGuard) SetEnabled(on bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.enabled = on
}

// SetInterval 设置巡检间隔（小时，最小 1 小时）
func (g *FreeModelGuard) SetInterval(hours int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if hours < 1 {
		hours = 24
	}
	g.interval = time.Duration(hours) * time.Hour
}

// Status 当前巡检状态快照
func (g *FreeModelGuard) Status() struct {
	LastRun   string `json:"last_run"`
	LastError string `json:"last_error"`
	Checked   int    `json:"checked"`
	Removed   int    `json:"removed"`
} {
	g.mu.Lock()
	defer g.mu.Unlock()
	return struct {
		LastRun   string `json:"last_run"`
		LastError string `json:"last_error"`
		Checked   int    `json:"checked"`
		Removed   int    `json:"removed"`
	}{
		LastRun:   g.status.LastRun,
		LastError: g.status.LastError,
		Checked:   g.status.Checked,
		Removed:   g.status.Removed,
	}
}

// AutoGuardLoop 后台自动巡检：按 enabled/interval 动态检查
func (g *FreeModelGuard) AutoGuardLoop(stop <-chan struct{}) {
	// 首次延迟 5 分钟（避开开机网络未就绪窗口）
	// P2-1：改用 time.NewTimer + 显式 Stop，避免 goroutine 取消后 time.After 定时器泄漏
	timer := time.NewTimer(5 * time.Minute)
	defer timer.Stop()
	select {
	case <-timer.C:
		g.CheckNow()
	case <-stop:
		return
	}

	// 用最小 1h ticker + 内部间隔判断，支持运行时改 interval
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	var next time.Time
	for {
		select {
		case <-stop:
			return
		default:
		}

		now := time.Now()
		if next.IsZero() || now.After(next) {
			next = now.Add(g.getInterval())
			g.CheckNow()
		}

		select {
		case <-ticker.C:
			// 继续循环
		case <-stop:
			return
		}
	}
}

// CheckNow 立即执行一次巡检（线程安全，供 POST /api/free-model-guard 触发）
func (g *FreeModelGuard) CheckNow() {
	g.tryOnce()
}

func (g *FreeModelGuard) tryOnce() {
	g.mu.Lock()
	if !g.enabled {
		g.mu.Unlock()
		return
	}
	interval := g.interval
	g.mu.Unlock()
	if interval <= 0 {
		interval = 24 * time.Hour
	}

	g.logFunc("[free-model-guard] start checking")
	providers := g.getProviders()
	checked := 0
	removed := 0

	for _, p := range providers {
		if !p.Enabled || !p.FreeOnly {
			continue
		}
		checked++

		// 重新拉取上游模型列表（仅免费模型）
		models, err := g.fetchModels(p.BaseURL, p.APIKey, p.AuthHeader, p.AuthScheme, true)
		if err != nil {
			g.logFunc("[free-model-guard] provider %s fetch failed: %v", p.Name, err)
			continue
		}

		// 对比当前已选模型，找出需要移除的（已选中但不在新免费列表中）
		currentSet := make(map[string]struct{}, len(p.Models))
		for _, m := range p.Models {
			currentSet[m] = struct{}{}
		}
		newSet := make(map[string]struct{}, len(models))
		for _, m := range models {
			newSet[m] = struct{}{}
		}

		toRemove := []string{}
		for m := range currentSet {
			if _, ok := newSet[m]; !ok {
				toRemove = append(toRemove, m)
			}
		}

		if len(toRemove) > 0 {
			newModels := make([]string, 0, len(currentSet)-len(toRemove))
			for m := range currentSet {
				if _, ok := newSet[m]; ok {
					newModels = append(newModels, m)
				}
			}
			if err := g.updateProviderModels(p.Name, newModels); err != nil {
				g.logFunc("[free-model-guard] provider %s update failed: %v", p.Name, err)
				continue
			}
			removed += len(toRemove)
			g.logFunc("[free-model-guard] provider %s removed %d free models: %v", p.Name, len(toRemove), toRemove)
		}
	}

	g.mu.Lock()
	g.status.LastRun = time.Now().Format(time.RFC3339)
	g.status.LastError = ""
	g.status.Checked += checked
	g.status.Removed += removed
	g.mu.Unlock()
	g.logFunc("[free-model-guard] done: checked=%d removed=%d", checked, removed)
}

func (g *FreeModelGuard) getInterval() time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.interval <= 0 {
		return 24 * time.Hour
	}
	return g.interval
}

// ErrFetchModelsNotSet 表示未注入 fetchModels 回调
var ErrFetchModelsNotSet = errors.New("fetchModels callback not set")

// ErrUpdateProviderModelsNotSet 表示未注入 updateProviderModels 回调
var ErrUpdateProviderModelsNotSet = errors.New("updateProviderModels callback not set")
