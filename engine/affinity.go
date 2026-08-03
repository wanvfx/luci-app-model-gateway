package engine

import (
	"sort"
	"sync"
	"time"
)

// SessionAffinity 会话亲和（C3）：
// 同一会话（客户端带 X-Session-Id，或按 vkey+IP 推断）尽量固定走同一 provider，
// 避免同一段对话上下文风格来回跳。失败则正常故障转移并更新绑定。
// TTL 30 分钟、上限 1000 条防内存涨（路由器 RAM 有限）。
type SessionAffinity struct {
	mu    sync.Mutex
	binds map[string]affinityEntry
}

const (
	affinityTTL = 30 * time.Minute
	affinityCap = 1000
)

type affinityEntry struct {
	provider string
	ts       time.Time
}

// NewSessionAffinity 创建会话亲和追踪器
func NewSessionAffinity() *SessionAffinity {
	return &SessionAffinity{binds: map[string]affinityEntry{}}
}

// Bind 将会话绑定到某个 provider（覆盖式更新时间戳）
func (a *SessionAffinity) Bind(session, provider string) {
	if session == "" || provider == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.binds) >= affinityCap {
		a.evictOld()
	}
	a.binds[session] = affinityEntry{provider: provider, ts: time.Now()}
}

// Lookup 返回会话当前绑定的 provider（已过期或不存在返回 "", false）
func (a *SessionAffinity) Lookup(session string) (string, bool) {
	if session == "" {
		return "", false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	e, ok := a.binds[session]
	if !ok {
		return "", false
	}
	if time.Since(e.ts) > affinityTTL {
		delete(a.binds, session)
		return "", false
	}
	return e.provider, true
}

// Refresh 续期现有绑定（不覆盖 provider），用于同一会话持续活动时保活
func (a *SessionAffinity) Refresh(session string) {
	if session == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if e, ok := a.binds[session]; ok {
		e.ts = time.Now()
		a.binds[session] = e
	}
}

// evictOld 淘汰最旧的 20%（防内存无限增长）
func (a *SessionAffinity) evictOld() {
	type kv struct {
		k  string
		ts time.Time
	}
	items := make([]kv, 0, len(a.binds))
	for k, v := range a.binds {
		items = append(items, kv{k, v.ts})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ts.Before(items[j].ts) })
	n := len(items) / 5
	for i := 0; i < n && i < len(items); i++ {
		delete(a.binds, items[i].k)
	}
}

// Snapshot 返回当前所有有效绑定（供调试/面板查看）
func (a *SessionAffinity) Snapshot() map[string]string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := map[string]string{}
	now := time.Now()
	for k, v := range a.binds {
		if now.Sub(v.ts) > affinityTTL {
			delete(a.binds, k)
			continue
		}
		out[k] = v.provider
	}
	return out
}
