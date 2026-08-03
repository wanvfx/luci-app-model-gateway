package engine

import (
	"net/http"
	"strconv"
	"sync"
	"time"
)

// CooldownTracker 精确冷却感知（C1）：
// 上游被限流（429 / 携带 x-ratelimit-reset）时，解析响应头里"再等 N 秒"的提示，
// 在该期限内把对应候选降权到队尾——比盲目熔断更聪明（照上游说的精确冷却）。
// 只降权不过滤，对齐熔断铁律。key 形如 provider-model（与惩罚追踪器一致）。
type CooldownTracker struct {
	mu    sync.Mutex
	until map[string]time.Time
}

// NewCooldownTracker 创建冷却追踪器
func NewCooldownTracker() *CooldownTracker {
	return &CooldownTracker{until: map[string]time.Time{}}
}

// SetFromHeaders 解析限流响应头并设置精确冷却截止时间。
// 支持 Retry-After（整数秒或 HTTP-date）与 X-RateLimit-Reset（Unix 秒或剩余秒）。
func (c *CooldownTracker) SetFromHeaders(key string, h http.Header) {
	if h == nil || key == "" {
		return
	}
	var until time.Time

	if v := h.Get("Retry-After"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil {
			until = time.Now().Add(time.Duration(secs) * time.Second)
		} else if t, err := http.ParseTime(v); err == nil {
			until = t
		}
	}
	if until.IsZero() {
		if v := h.Get("X-RateLimit-Reset"); v != "" {
			if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
				var delta time.Duration
				switch {
				case secs <= 0:
					// 无冷却
				case secs > 1e10: // 视为 Unix 秒
					delta = time.Until(time.Unix(secs, 0))
				default: // 视为剩余秒
					delta = time.Duration(secs) * time.Second
				}
				if delta > 0 {
					until = time.Now().Add(delta)
				}
			}
		}
	}
	if until.IsZero() || !until.After(time.Now()) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if existing, ok := c.until[key]; !ok || until.After(existing) {
		c.until[key] = until
	}
}

// Active 返回该 key 当前是否处于冷却中（过期自动清理）
func (c *CooldownTracker) Active(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.until[key]
	if !ok {
		return false
	}
	if time.Now().After(t) {
		delete(c.until, key)
		return false
	}
	return true
}

// Until 返回冷却截止时间（零值表示未冷却）
func (c *CooldownTracker) Until(key string) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.until[key]
}

// Snapshot 返回当前所有冷却中的 key 及其剩余秒数（供调试/面板查看）
func (c *CooldownTracker) Snapshot() map[string]int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]int64{}
	now := time.Now()
	for k, t := range c.until {
		if now.After(t) {
			delete(c.until, k)
			continue
		}
		out[k] = int64(time.Until(t).Seconds())
	}
	return out
}
