package engine

import (
	"crypto/sha256"
	"encoding/json"
	"hash/fnv"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// cachedEntry 单条缓存（存答案文本，非原始 API 字节，便于流式/非流式统一复用）
type cachedEntry struct {
	ExactKey   string `json:"exact_key"`
	Model      string `json:"model"`
	Simhash    uint64 `json:"simhash"`
	PromptNorm string `json:"prompt_norm"`
	Content    string `json:"content"`
	CreatedAt  int64  `json:"created_at"`
	Streamed   bool   `json:"streamed"`
}

// CacheStats 缓存统计
type CacheStats struct {
	Hits         int64 `json:"hits"`
	Misses       int64 `json:"misses"`
	SemanticHits int64 `json:"semantic_hits"`
	Puts         int64 `json:"puts"`
	Entries      int   `json:"entries"`
}

// ResponseCache 响应缓存：精确哈希 + simhash 近重复（语义缓存）。
// 数据落 MODEL_GATEWAY_DATA/cache，不新增 UCI dataDir 字段（符合 iStoreOS 铁律）。
type ResponseCache struct {
	mu       sync.RWMutex
	dir      string
	enabled  bool
	ttl      int
	max      int
	semantic bool
	hamming  int
	exact    map[string]*cachedEntry
	simIndex map[uint64][]*cachedEntry
	stats    CacheStats
	stopCh   chan struct{}
	doneCh   chan struct{} // 后台落盘协程退出信号（Stop 同步等待，确保最后一次落盘完成）
}

// NewResponseCache 创建缓存并从磁盘加载已有条目
func NewResponseCache(dataDir string) *ResponseCache {
	c := &ResponseCache{
		dir:      dataDir,
		enabled:  true,
		ttl:      300,
		max:      1000,
		semantic: true,
		hamming:  3,
		exact:    map[string]*cachedEntry{},
		simIndex: map[uint64][]*cachedEntry{},
		stopCh:   make(chan struct{}),
		doneCh:   make(chan struct{}),
	}
	// 启动时从磁盘加载（前向兼容：文件不存在则跳过）
	c.loadFromDisk()
	// 后台周期落盘（每 30s）
	go func() {
		defer close(c.doneCh)
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-c.stopCh:
				c.persist()
				return
			case <-ticker.C:
				c.persist()
			}
		}
	}()
	return c
}

// Stop 停止后台落盘并同步等待最后一次写入完成（防止进程退出丢缓存/落盘竞态）
func (c *ResponseCache) Stop() {
	select {
	case <-c.stopCh:
	default:
		close(c.stopCh)
	}
	<-c.doneCh
}

// SetConfig 热更新缓存配置（enabled/ttl/max/semantic）
func (c *ResponseCache) SetConfig(enabled bool, ttl, max int, semantic bool) {
	c.mu.Lock()
	c.enabled = enabled
	if ttl > 0 {
		c.ttl = ttl
	}
	if max > 0 {
		c.max = max
	}
	c.semantic = semantic
	c.mu.Unlock()
}

// Clear 清空缓存（保留配置）
func (c *ResponseCache) Clear() {
	c.mu.Lock()
	c.exact = map[string]*cachedEntry{}
	c.simIndex = map[uint64][]*cachedEntry{}
	c.stats = CacheStats{}
	c.mu.Unlock()
	c.persist()
}

// Stats 返回统计
func (c *ResponseCache) Stats() CacheStats {
	c.mu.RLock()
	defer c.mu.RUnlock()
	s := c.stats
	// 计数器经 atomic 写入，读也用 atomic.Load 避免与并发写产生 data race
	s.Hits = atomic.LoadInt64(&c.stats.Hits)
	s.Misses = atomic.LoadInt64(&c.stats.Misses)
	s.SemanticHits = atomic.LoadInt64(&c.stats.SemanticHits)
	s.Puts = atomic.LoadInt64(&c.stats.Puts)
	s.Entries = len(c.exact)
	return s
}

// ExactKey 计算精确缓存键：规范化请求体（去 stream 字段）+ model
func (c *ResponseCache) ExactKey(model, reqBody string) string {
	norm := normalizeRequest(model, reqBody)
	h := sha256.Sum256([]byte(norm))
	return "ex:" + model + ":" + string([]byte(hexEncode(h[:])))
}

// normalizeRequest 规范化请求体：解析为 map，删除 stream/model 字段，重排为确定性 JSON
// （model 作为独立参数参与缓存键，避免 body 内 model 字段与参数不一致导致键漂移）
func normalizeRequest(model, reqBody string) string {
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(reqBody), &m); err != nil {
		// 解析失败则用原始体（去空白）
		return model + "|" + strings.Join(strings.Fields(reqBody), "")
	}
	delete(m, "stream")
	delete(m, "stream_options")
	delete(m, "model")
	// messages 保留（含顺序与内容），但去掉可能的非语义字段
	b, err := json.Marshal(m)
	if err != nil {
		return model + "|" + reqBody
	}
	return model + "|" + string(b)
}

// PromptNorm 提取语义归一化提示（取最后一条 user 消息内容，小写、去标点空白）
func (c *ResponseCache) PromptNorm(reqBody string) string {
	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(reqBody), &req); err != nil {
		return strings.ToLower(strings.Join(strings.Fields(reqBody), " "))
	}
	var last string
	for _, msg := range req.Messages {
		if msg.Role == "user" && strings.TrimSpace(msg.Content) != "" {
			last = msg.Content
		}
	}
	if last == "" {
		// 无 user 消息则用全部消息内容拼接
		var sb strings.Builder
		for _, msg := range req.Messages {
			sb.WriteString(msg.Content)
			sb.WriteString("\n")
		}
		last = sb.String()
	}
	// 去标点、压缩空白、小写
	norm := strings.ToLower(last)
	norm = strings.Join(strings.FieldsFunc(norm, func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') && r != ' ' && r != '\u4e00' && r <= '\u9fff'
	}), " ")
	norm = strings.Join(strings.Fields(norm), " ")
	return norm
}

// Simhash 计算 64 位 simhash
func Simhash(text string) uint64 {
	const bits = 64
	vec := make([]int, bits)
	tokens := strings.Fields(text)
	if len(tokens) == 0 {
		return 0
	}
	for _, tok := range tokens {
		h := fnv.New64a()
		h.Write([]byte(tok))
		tokenHash := h.Sum64()
		for i := 0; i < bits; i++ {
			if tokenHash&(1<<uint(i)) != 0 {
				vec[i]++
			} else {
				vec[i]--
			}
		}
	}
	var out uint64
	for i := 0; i < bits; i++ {
		if vec[i] > 0 {
			out |= 1 << uint(i)
		}
	}
	return out
}

// hamming 计算两个 uint64 的汉明距离
func hamming(a, b uint64) int {
	x := a ^ b
	cnt := 0
	for x != 0 {
		cnt += int(x & 1)
		x >>= 1
	}
	return cnt
}

// GetContent 查询缓存：先精确，再（若开启语义）simhash 近重复。
// 返回 (内容, 是否命中, 是否语义命中)
// P1-8：stats 计数改用 atomic 操作，读路径降级为 RLock，避免高并发读下的写锁瓶颈
// （此前在 RLock 下 ++stats 是真实 data race，故被迫用写锁；现 atomic 化后读锁即可）。
func (c *ResponseCache) GetContent(model, exactKey, promptNorm string) (string, bool, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.enabled {
		atomic.AddInt64(&c.stats.Misses, 1)
		return "", false, false
	}
	// 1. 精确命中
	if e, ok := c.exact[exactKey]; ok {
		if c.ttl <= 0 || time.Now().Unix()-e.CreatedAt <= int64(c.ttl) {
			atomic.AddInt64(&c.stats.Hits, 1)
			return e.Content, true, false
		}
	}
	// 2. 语义（simhash 近重复）命中
	if c.semantic && promptNorm != "" {
		sh := Simhash(promptNorm)
		for _, e := range c.simIndex[sh] {
			if e.Model != model {
				continue
			}
			if c.ttl > 0 && time.Now().Unix()-e.CreatedAt > int64(c.ttl) {
				continue
			}
			if hamming(sh, e.Simhash) <= c.hamming {
				atomic.AddInt64(&c.stats.Hits, 1)
				atomic.AddInt64(&c.stats.SemanticHits, 1)
				return e.Content, true, true
			}
		}
	}
	atomic.AddInt64(&c.stats.Misses, 1)
	return "", false, false
}

// PutContent 写入缓存
func (c *ResponseCache) PutContent(model, exactKey, promptNorm, content string, streamed bool) {
	c.mu.Lock()
	if !c.enabled {
		c.mu.Unlock()
		return
	}
	now := time.Now().Unix()
	e := &cachedEntry{
		ExactKey:   exactKey,
		Model:      model,
		Simhash:    Simhash(promptNorm),
		PromptNorm: promptNorm,
		Content:    content,
		CreatedAt:  now,
		Streamed:   streamed,
	}
	c.exact[exactKey] = e
	if promptNorm != "" {
		sh := e.Simhash
		c.simIndex[sh] = append(c.simIndex[sh], e)
	}
	atomic.AddInt64(&c.stats.Puts, 1)
	// 容量裁剪：超过 max 时丢弃最旧
	if len(c.exact) > c.max {
		c.evictOldest()
	}
	c.mu.Unlock()
}

// evictOldest 丢弃最旧的若干条目（按 CreatedAt）
func (c *ResponseCache) evictOldest() {
	// 收集并排序
	type cacheItem struct {
		key string
		ts  int64
	}
	items := make([]cacheItem, 0, len(c.exact))
	for k, e := range c.exact {
		items = append(items, cacheItem{k, e.CreatedAt})
	}
	// 简单插入排序（条目量不大，按 CreatedAt 升序）
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && items[j-1].ts > items[j].ts; j-- {
			items[j-1], items[j] = items[j], items[j-1]
		}
	}
	// 删除约 20%
	drop := len(items) / 5
	if drop < 1 {
		drop = 1
	}
	for i := 0; i < drop && i < len(items); i++ {
		delete(c.exact, items[i].key)
	}
	// 重建 simIndex（简单起见整体重建）
	c.simIndex = map[uint64][]*cachedEntry{}
	for _, e := range c.exact {
		if e.PromptNorm != "" {
			c.simIndex[e.Simhash] = append(c.simIndex[e.Simhash], e)
		}
	}
}

// persist 落盘（dataDir/cache/cache.json）
func (c *ResponseCache) persist() {
	c.mu.RLock()
	entries := make([]*cachedEntry, 0, len(c.exact))
	for _, e := range c.exact {
		entries = append(entries, e)
	}
	c.mu.RUnlock()
	if c.dir == "" {
		return
	}
	dir := filepath.Join(c.dir, "cache")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return
	}
	data, err := json.Marshal(entries)
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, "cache.json"), data, 0644)
}

// loadFromDisk 从磁盘加载
func (c *ResponseCache) loadFromDisk() {
	if c.dir == "" {
		return
	}
	path := filepath.Join(c.dir, "cache", "cache.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var entries []*cachedEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return
	}
	now := time.Now().Unix()
	for _, e := range entries {
		if c.ttl > 0 && now-e.CreatedAt > int64(c.ttl) {
			continue // 过期跳过
		}
		c.exact[e.ExactKey] = e
		if e.PromptNorm != "" {
			c.simIndex[e.Simhash] = append(c.simIndex[e.Simhash], e)
		}
	}
}

// hexEncode 小工具：将字节转十六进制串（避免引入额外依赖）
func hexEncode(b []byte) string {
	const hexc = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, v := range b {
		out[i*2] = hexc[v>>4]
		out[i*2+1] = hexc[v&0x0f]
	}
	return string(out)
}
