package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/wanvfx/luci-app-model-gateway/config"
)

// ModelDetail 模型详情缓存条目
type ModelDetail struct {
	ID            string  `json:"id"`
	Object        string  `json:"object"`
	Created       int64   `json:"created"`
	OwnedBy       string  `json:"owned_by"`
	ContextLen    int     `json:"context_length"`
	MaxPosEmb     int     `json:"max_position_embeddings"`
	MaxModelLen   int     `json:"max_model_len"`
	PromptPrice   float64 `json:"prompt_price"`
	CompletePrice float64 `json:"completion_price"`
}

// GetContextLen 实现接口供 ContextLengthWithFallback 使用
func (d *ModelDetail) GetContextLen() int {
	return d.ContextLen
}

// ModelCache 模型详情缓存
type ModelCache struct {
	mu       sync.RWMutex
	details  map[string]*ModelDetail
	expireAt time.Time
	ttl      time.Duration
}

// NewModelCache 创建模型缓存
func NewModelCache(ttl time.Duration) *ModelCache {
	return &ModelCache{
		details: make(map[string]*ModelDetail),
		ttl:     ttl,
	}
}

// UpdateFromUpstream 从上游 /v1/models 响应更新缓存
func (c *ModelCache) UpdateFromUpstream(body []byte) {
	var resp struct {
		Data []ModelDetail `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.details = make(map[string]*ModelDetail)
	c.expireAt = time.Now().Add(c.ttl)
	for i := range resp.Data {
		item := resp.Data[i]
		item.Object = "model"
		if item.ContextLen == 0 {
			item.ContextLen = 32768
		}
		if item.MaxPosEmb == 0 {
			item.MaxPosEmb = item.ContextLen
		}
		if item.MaxModelLen == 0 {
			item.MaxModelLen = item.ContextLen
		}
		c.details[item.ID] = &item
	}
}

// Get 获取模型详情（未缓存返回 nil）
func (c *ModelCache) Get(modelID string) *ModelDetail {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if time.Now().After(c.expireAt) {
		return nil
	}
	return c.details[modelID]
}

// IsValid 缓存是否有效
func (c *ModelCache) IsValid() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return time.Now().Before(c.expireAt) && len(c.details) > 0
}

// FetchAndCache 从上游拉取 /v1/models 并缓存（并发拉取所有 provider 后合并）。
//
// ⚠️ 铁律（#29 根因）：绝不能在持有 c.mu 写锁期间做网络 IO / 等待 channel。
// 旧实现在 c.mu.Lock() 之后逐个 <-ch 等待上游响应（单个超时最长 30s），
// 期间 Get()/IsValid() 的读锁全部被阻塞 → /api/model-details、/v1/models、
// 代理转发统统卡住 → 前端刷新后"上游提供商"区域 10~30 秒空白。
// 更严重的是：goroutine 只为「启用且 BaseURL 非空」的 provider 派发，
// 而接收循环按 len(providers) 计数，一旦存在禁用/空 URL 的 provider，
// <-ch 永久阻塞且写锁永不释放 → 所有读取方永久卡死（死锁）。
// 现实现：先无锁并发收集（按实际派发数接收），全部完成后仅在内存交换时短暂加锁。
func (c *ModelCache) FetchAndCache(providers []*config.Provider, client *http.Client) {
	if len(providers) == 0 {
		return
	}

	// 并发拉取所有启用的 provider
	type result struct {
		provider *config.Provider
		body     []byte
		err      error
	}

	ch := make(chan result, len(providers))
	spawned := 0 // 实际派发的 goroutine 数（接收循环必须与此一致，绝不能用 len(providers)）
	for _, p := range providers {
		if p == nil || !p.Enabled || p.BaseURL == "" {
			continue
		}
		spawned++
		go func(prov *config.Provider) {
			// base_url 已包含 /v1（如 https://integrate.api.nvidia.com/v1），
			// 直接拼 "/models"，切勿再加 /v1 否则变成 /v1/v1/models → 404
			url := strings.TrimRight(prov.BaseURL, "/") + "/models"
			req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
			if err != nil {
				ch <- result{provider: prov, err: err}
				return
			}
			req.Header.Set("Authorization", "Bearer "+prov.APIKey)
			resp, err := client.Do(req)
			if err != nil {
				ch <- result{provider: prov, err: err}
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				ch <- result{provider: prov, err: fmt.Errorf("HTTP %d", resp.StatusCode)}
				return
			}
			b, _ := io.ReadAll(resp.Body)
			ch <- result{provider: prov, body: b}
		}(p)
	}

	// 无锁收集所有响应并合并到局部 map（网络等待期间不影响任何读取方）
	newDetails := make(map[string]*ModelDetail)
	done := make(map[string]bool)
	for i := 0; i < spawned; i++ {
		res := <-ch
		if res.err != nil || len(res.body) == 0 {
			continue
		}

		var resp struct {
			Data []ModelDetail `json:"data"`
		}
		if err := json.Unmarshal(res.body, &resp); err != nil {
			continue
		}

		for j := range resp.Data {
			item := resp.Data[j]
			item.Object = "model"

			// free_only 过滤：如果 provider 设置了 free_only，只保留免费模型
			if res.provider.FreeOnly {
				// 检查模型名是否包含免费标记
				lowerID := strings.ToLower(item.ID)
				isFree := strings.Contains(lowerID, ":free") ||
					strings.Contains(lowerID, "-free") ||
					strings.Contains(lowerID, "_free")
				if !isFree {
					continue
				}
			}

			if item.ContextLen == 0 {
				item.ContextLen = 32768
			}
			if item.MaxPosEmb == 0 {
				item.MaxPosEmb = item.ContextLen
			}
			if item.MaxModelLen == 0 {
				item.MaxModelLen = item.ContextLen
			}

			// 避免重复（先到先得）
			key := item.ID
			if !done[key] {
				newDetails[key] = &item
				done[key] = true
			}
		}
	}

	// 仅在交换内存快照时短暂加锁（微秒级）
	c.mu.Lock()
	c.details = newDetails
	c.expireAt = time.Now().Add(c.ttl)
	c.mu.Unlock()
}
