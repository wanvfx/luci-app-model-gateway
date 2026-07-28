package proxy

import (
	"sync"
	"time"
)

// semaphore 信号量（用 buffered channel 实现）
type semaphore struct {
	ch chan struct{}
}

func newSemaphore(n int) *semaphore {
	if n <= 0 {
		n = 1 << 30 // 实际不限
	}
	return &semaphore{ch: make(chan struct{}, n)}
}

// tryAcquire 在超时内尝试获取一个槽位；成功返回 release 函数
func (s *semaphore) tryAcquire(timeout time.Duration) (func(), bool) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case s.ch <- struct{}{}:
		return func() { <-s.ch }, true
	case <-timer.C:
		return nil, false
	}
}

// sizedSem 带容量记录的信号量（容量变化时重建，支持配置热更新）
type sizedSem struct {
	sem *semaphore
	max int
}

// ConcurrencyGuard 并发护栏：全局 + 单 provider 两级信号量。
// 超限返回 429（Too Many Requests），避免软路由被上游限流/打满。
// 配置热更新：max 变化时按新容量重建信号量（已占用槽位随旧信号量自然释放）。
type ConcurrencyGuard struct {
	mu        sync.Mutex
	global    sizedSem
	providers map[string]*sizedSem
}

// NewConcurrencyGuard 创建并发护栏
func NewConcurrencyGuard() *ConcurrencyGuard {
	return &ConcurrencyGuard{
		providers: map[string]*sizedSem{},
	}
}

// globalSem 获取全局信号量（容量与配置不符时重建）
func (g *ConcurrencyGuard) globalSem(max int) *semaphore {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.global.sem == nil || g.global.max != max {
		g.global = sizedSem{sem: newSemaphore(max), max: max}
	}
	return g.global.sem
}

// providerSem 获取 provider 信号量（惰性创建；容量与配置不符时重建）
func (g *ConcurrencyGuard) providerSem(name string, max int) *semaphore {
	g.mu.Lock()
	defer g.mu.Unlock()
	ss, ok := g.providers[name]
	if !ok || ss.max != max {
		ss = &sizedSem{sem: newSemaphore(max), max: max}
		g.providers[name] = ss
	}
	return ss.sem
}

// TryAcquire 尝试获取全局 + provider 两个槽位。
// globalMax / providerMax 来自配置（0 = 不限）。返回 release（释放两个槽位）与是否成功。
func (g *ConcurrencyGuard) TryAcquire(provider string, globalMax, providerMax int, timeout time.Duration) (func(), bool) {
	if globalMax <= 0 && providerMax <= 0 {
		return func() {}, true
	}
	var relGlobal, relProv func()
	// 全局槽位
	if globalMax > 0 {
		rel, ok := g.globalSem(globalMax).tryAcquire(timeout)
		if !ok {
			return nil, false
		}
		relGlobal = rel
	}
	// provider 槽位
	if providerMax > 0 {
		rel, ok := g.providerSem(provider, providerMax).tryAcquire(timeout)
		if !ok {
			if relGlobal != nil {
				relGlobal() // 回滚已获全局槽位
			}
			return nil, false
		}
		relProv = rel
	}
	return func() {
		if relProv != nil {
			relProv()
		}
		if relGlobal != nil {
			relGlobal()
		}
	}, true
}
