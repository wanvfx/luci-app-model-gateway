package proxy

import (
	"sync"
	"testing"

	"github.com/wanvfx/luci-app-model-gateway/config"
	"github.com/wanvfx/luci-app-model-gateway/engine"
	"github.com/wanvfx/luci-app-model-gateway/storage"
)

// TestConfigHotUpdateRace 验证：并发热更新配置（UpdateConfig）与并发读配置/路由
// 不会触发 data race（修复 #2：proxy.Server.cfg 与 s.router 改为 atomic.Pointer）。
// 用 `go test -race` 运行；若仍有非原子读写，race detector 会直接报错。
func TestConfigHotUpdateRace(t *testing.T) {
	cfg := &config.Config{}
	usage := &storage.Usage{}
	circuits := engine.NewCircuitPool(3, 1)
	scorer := engine.NewScorer()
	s := New(cfg, t.TempDir(), usage, circuits, scorer)

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// 写 goroutine：持续热更新配置
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					s.UpdateConfig(&config.Config{})
				}
			}
		}()
	}

	// 读 goroutine：并发读配置与路由（模拟聊天请求的热路径）
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = s.cfg.Load()
					r := s.router.Load()
					if r != nil {
						_ = r.IsRouter("x")
					}
					_ = s.cfg.Load().Providers
				}
			}
		}()
	}

	close(stop)
	wg.Wait()
}
