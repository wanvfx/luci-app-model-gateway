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
	// 嵌套 pricing（OpenRouter 等上游返回 pricing.prompt / pricing.completion）。
	// 非 nil 表示上游确实返回了 pricing 字段，可用于免费判定（区分“未提供”与“价格为 0”）。
	Pricing *PriceInfo `json:"pricing"`
}

// PriceInfo 上游 /models 返回的嵌套价格信息（prompt/completion 为主，input/output 作别名兜底）
type PriceInfo struct {
	Prompt     float64 `json:"prompt"`
	Completion float64 `json:"completion"`
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
}

// IsFree 判断是否为免费模型：对齐 Python 原版 is_free_model / is_free_by_name
//   - 名称含 :free / -free / _free → 免费
//   - 上游返回 pricing 且 prompt/completion/input/output 全为 0 → 免费
//   - 上游返回 pricing 且任一分量非 0 → 收费
//   - 上游未返回任何 pricing 信息 → 保守视为免费（不误删，对齐 Python 兜底返回全部对话模型）
func (d *ModelDetail) IsFree() bool {
	lower := strings.ToLower(d.ID)
	if strings.Contains(lower, ":free") || strings.Contains(lower, "-free") || strings.Contains(lower, "_free") {
		return true
	}
	if d.Pricing != nil {
		return d.Pricing.Prompt == 0 && d.Pricing.Completion == 0 && d.Pricing.Input == 0 && d.Pricing.Output == 0
	}
	if d.PromptPrice != 0 || d.CompletePrice != 0 {
		return false
	}
	// 无 pricing 信息，保守视为免费（避免把未标注价格的免费模型误删）
	return true
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
			prov.ApplyAuth(req.Header)
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

			// free_only 过滤：如果 provider 设置了 free_only，只保留免费模型。
			// 免费判定：名称含 :free/-free/_free，或上游 pricing 全为 0（对齐 Python is_free_model）。
			// 上游未返回 pricing 时保守保留（避免误删 NVIDIA 等无 -free 后缀的免费模型）。
			if res.provider.FreeOnly {
				if !item.IsFree() {
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
