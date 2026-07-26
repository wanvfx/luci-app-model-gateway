package engine

import (
	"sync"
	"time"
)

// CircuitPool 线程安全的熔断器池（按模型 key 管理）
// key 格式："provider||model"，与 Python 原版一致
type CircuitPool struct {
	mu        sync.Mutex
	breakers  map[string]*CircuitBreaker
	threshold int
	window    time.Duration
}

// NewCircuitPool 创建熔断器池
func NewCircuitPool(threshold int, window time.Duration) *CircuitPool {
	return &CircuitPool{
		breakers:  make(map[string]*CircuitBreaker),
		threshold: threshold,
		window:    window,
	}
}

// Get 获取或创建指定 key 的熔断器
func (cp *CircuitPool) Get(key string) *CircuitBreaker {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	if cb, ok := cp.breakers[key]; ok {
		return cb
	}
	cb := NewCircuitBreaker(cp.threshold, cp.window)
	cp.breakers[key] = cb
	return cb
}

// IsOpen 检查指定 key 的熔断器是否处于熔断状态
func (cp *CircuitPool) IsOpen(key string) bool {
	return cp.Get(key).Allow() == false
}

// RecordSuccess 记录指定 key 的成功
func (cp *CircuitPool) RecordSuccess(key string) {
	cp.Get(key).Success()
}

// RecordFailure 记录指定 key 的失败
func (cp *CircuitPool) RecordFailure(key string) {
	cp.Get(key).Failure()
}

// State 返回指定 key 的熔断状态
func (cp *CircuitPool) State(key string) string {
	return cp.Get(key).State()
}
