package engine

import (
	"sync"
	"time"
)

// CircuitBreaker 熔断器
type CircuitBreaker struct {
	mu        sync.Mutex
	failCount int
	lastFail  time.Time // 最近一次失败时间（用于窗口衰减）
	openUntil time.Time
	threshold int
	window    time.Duration

	// —— 锁定层级（C4 模型锁定自愈）——
	// lockCount 连续失败计数（不受短窗口衰减清零影响，仅成功时归零），
	// 达到 lockThreshold 即进入更长冷却（lockUntil），期间巡检探测成功才解锁。
	lockCount     int
	lockUntil     time.Time
	lockThreshold int
	lockWindow    time.Duration
}

// NewCircuitBreaker 创建熔断器（threshold 次失败，window 时间窗口）
// lockThreshold 默认 threshold*3（连续失败这么多次方锁定），lockWindow 默认 1 小时。
func NewCircuitBreaker(threshold int, window time.Duration) *CircuitBreaker {
	lt := threshold * 3
	if lt < 6 {
		lt = 6 // 兜底，避免阈值过松误伤偶发抖动
	}
	return &CircuitBreaker{
		threshold:     threshold,
		window:        window,
		lockThreshold: lt,
		lockWindow:    60 * time.Minute,
	}
}

// Allow 检查是否允许请求通过
func (c *CircuitBreaker) Allow() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Now().Before(c.openUntil) {
		return false // 熔断中
	}
	return true
}

// Success 记录成功（同时解除短冷却与锁定，C4 自愈：巡检探测成功即解锁）
func (c *CircuitBreaker) Success() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failCount = 0
	c.lockCount = 0
	c.openUntil = time.Time{}
	c.lockUntil = time.Time{}
}

// Failure 记录失败。
// 窗口衰减修复：只有「window 时间内连续 threshold 次失败」才跳闸；
// 距上次失败超过 window 则先清零计数，避免数天内零星失败累计误跳闸。
// 锁定计数 lockCount 不受短窗口衰减影响（只成功时归零），连续失败累积到
// lockThreshold 即进入更长冷却（lockUntil），压住持续坏掉的模型刷错误。
func (c *CircuitBreaker) Failure() {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if !c.lastFail.IsZero() && now.Sub(c.lastFail) > c.window {
		c.failCount = 0
	}
	c.lastFail = now
	c.failCount++
	c.lockCount++
	if c.failCount >= c.threshold {
		c.openUntil = now.Add(c.window)
	}
	if c.lockCount >= c.lockThreshold {
		c.lockUntil = now.Add(c.lockWindow)
	}
}

// Locked 返回当前是否处于锁定状态（C4 模型锁定自愈）
func (c *CircuitBreaker) Locked() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Before(c.lockUntil)
}

// State 返回当前状态
func (c *CircuitBreaker) State() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Now().Before(c.lockUntil) {
		return "locked"
	}
	if time.Now().Before(c.openUntil) {
		return "open"
	}
	if c.failCount > 0 {
		return "half-open"
	}
	return "closed"
}
