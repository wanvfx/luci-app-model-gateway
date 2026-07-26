package engine

import (
	"sync"
	"time"
)

// CircuitBreaker 熔断器
type CircuitBreaker struct {
	mu        sync.Mutex
	failCount int
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

// Failure 记录失败
func (c *CircuitBreaker) Failure() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failCount++
	if c.failCount >= c.threshold {
		c.openUntil = time.Now().Add(c.window)
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
