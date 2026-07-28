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
}

// NewCircuitBreaker 创建熔断器（threshold 次失败，window 时间窗口）
func NewCircuitBreaker(threshold int, window time.Duration) *CircuitBreaker {
	return &CircuitBreaker{
		threshold: threshold,
		window:    window,
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

// Success 记录成功
func (c *CircuitBreaker) Success() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failCount = 0
	c.openUntil = time.Time{}
}

// Failure 记录失败。
// 窗口衰减修复：只有「window 时间内连续 threshold 次失败」才跳闸；
// 距上次失败超过 window 则先清零计数，避免数天内零星失败累计误跳闸。
func (c *CircuitBreaker) Failure() {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if !c.lastFail.IsZero() && now.Sub(c.lastFail) > c.window {
		c.failCount = 0
	}
	c.lastFail = now
	c.failCount++
	if c.failCount >= c.threshold {
		c.openUntil = now.Add(c.window)
	}
}

// State 返回当前状态
func (c *CircuitBreaker) State() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Now().Before(c.openUntil) {
		return "open"
	}
	if c.failCount > 0 {
		return "half-open"
	}
	return "closed"
}
