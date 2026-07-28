package proxy

// idempotency.go 幂等键：客户端携带 Idempotency-Key 请求头时，
// 相同 key 的重复请求（网络重试/双击）直接返回首次响应，不重复消耗上游额度。
// 参考 open-llm-router 的幂等设计；进程内存储（TTL 10 分钟 / 上限 512 条），
// 独立于响应缓存开关——幂等语义不应受缓存配置影响。仅作用于非流式请求。

import (
	"sync"
	"time"
)

const (
	idemTTL        = 10 * time.Minute
	idemMaxEntries = 512
)

// idemEntry 已完成请求的响应快照
type idemEntry struct {
	body        []byte
	contentType string
	status      int
	at          time.Time
}

// idemStore 幂等键存储（进程内）
type idemStore struct {
	mu      sync.Mutex
	entries map[string]*idemEntry
}

func newIdemStore() *idemStore {
	return &idemStore{entries: map[string]*idemEntry{}}
}

// Get 查询幂等键（命中返回响应快照）
func (s *idemStore) Get(key string) (*idemEntry, bool) {
	if s == nil || key == "" {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[key]
	if !ok {
		return nil, false
	}
	if time.Since(e.at) > idemTTL {
		delete(s.entries, key)
		return nil, false
	}
	return e, true
}

// Put 保存响应快照（仅保存成功响应；超上限时先清过期再淘汰最旧）
func (s *idemStore) Put(key string, status int, contentType string, body []byte) {
	if s == nil || key == "" || status != 200 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.entries) >= idemMaxEntries {
		now := time.Now()
		var oldestKey string
		var oldestAt time.Time
		for k, e := range s.entries {
			if now.Sub(e.at) > idemTTL {
				delete(s.entries, k)
				continue
			}
			if oldestKey == "" || e.at.Before(oldestAt) {
				oldestKey, oldestAt = k, e.at
			}
		}
		if len(s.entries) >= idemMaxEntries && oldestKey != "" {
			delete(s.entries, oldestKey)
		}
	}
	cp := make([]byte, len(body))
	copy(cp, body)
	s.entries[key] = &idemEntry{body: cp, contentType: contentType, status: status, at: time.Now()}
}
