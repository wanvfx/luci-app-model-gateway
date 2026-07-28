package engine

// penalty.go 动态惩罚路由：记录各成员（provider-model 前缀名）近期 429/5xx 事件，
// 指数时间衰减；排序时把「近期表现差」的成员临时降权到队尾，恢复后自动回位。
// 与熔断器互补：熔断是「彻底坏了就跳闸」，惩罚是「频繁限流/偶发失败就降权」。
// 纯内存、纯 Go，符合 iStoreOS 铁律。

import (
	"math"
	"strings"
	"sync"
	"time"
)

// penaltyEntry 单成员惩罚记录
type penaltyEntry struct {
	score float64   // 衰减后累计分
	last  time.Time // 上次更新时刻
}

// PenaltyTracker 惩罚追踪器
type PenaltyTracker struct {
	mu      sync.Mutex
	entries map[string]*penaltyEntry
	tau     float64 // 衰减时间常数（秒）
}

// 事件权重：429 限流最重（说明该通道正被打满），5xx/超时次之
const (
	PenaltyRate   = 1.0
	PenaltyServer = 0.7
)

// penaltyThreshold 分数达到该值即视为「受罚中」，排序降权
const penaltyThreshold = 1.0

// NewPenaltyTracker 创建惩罚追踪器（tau=120s：约 2 分钟无新失败即衰减过半）
func NewPenaltyTracker() *PenaltyTracker {
	return &PenaltyTracker{
		entries: map[string]*penaltyEntry{},
		tau:     120,
	}
}

// normKey 归一化 key（provider-model 前缀名，小写）
func normKey(key string) string {
	return strings.ToLower(strings.TrimSpace(key))
}

// Record 记录一次失败事件（weight 用 PenaltyRate / PenaltyServer）
func (p *PenaltyTracker) Record(key string, weight float64) {
	if p == nil || weight <= 0 {
		return
	}
	k := normKey(key)
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	e, ok := p.entries[k]
	if !ok {
		p.entries[k] = &penaltyEntry{score: weight, last: now}
		return
	}
	dt := now.Sub(e.last).Seconds()
	e.score = e.score*math.Exp(-dt/p.tau) + weight
	e.last = now
	// 上限保护，避免长时间雪崩累出天文数字
	if e.score > 20 {
		e.score = 20
	}
}

// Score 返回当前衰减后分数（只读，不落新时间戳）
func (p *PenaltyTracker) Score(key string) float64 {
	if p == nil {
		return 0
	}
	k := normKey(key)
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.entries[k]
	if !ok {
		return 0
	}
	dt := time.Since(e.last).Seconds()
	s := e.score * math.Exp(-dt/p.tau)
	if s < 0.01 {
		// 已衰减殆尽，顺手清理
		delete(p.entries, k)
		return 0
	}
	return s
}

// Penalized 是否处于受罚状态
func (p *PenaltyTracker) Penalized(key string) bool {
	return p.Score(key) >= penaltyThreshold
}

// Demote 稳定重排：未受罚成员保序在前，受罚成员按分数升序排后。
// 用于各排序策略产出的成员序列（priority 策略除外——用户显式顺序不动）。
func (p *PenaltyTracker) Demote(members []string) []string {
	if p == nil || len(members) < 2 {
		return members
	}
	var clean, punished []string
	scores := map[string]float64{}
	for _, m := range members {
		s := p.Score(m)
		if s >= penaltyThreshold {
			punished = append(punished, m)
			scores[m] = s
		} else {
			clean = append(clean, m)
		}
	}
	if len(punished) == 0 {
		return members
	}
	// 受罚组内按分数升序（罚得轻的先试）——稳定插入排序
	for i := 1; i < len(punished); i++ {
		for j := i; j > 0 && scores[punished[j-1]] > scores[punished[j]]; j-- {
			punished[j-1], punished[j] = punished[j], punished[j-1]
		}
	}
	return append(clean, punished...)
}

// Snapshot 返回当前所有受罚成员（供 /api/dashboard 或调试查看）
func (p *PenaltyTracker) Snapshot() map[string]float64 {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	keys := make([]string, 0, len(p.entries))
	for k := range p.entries {
		keys = append(keys, k)
	}
	p.mu.Unlock()
	out := map[string]float64{}
	for _, k := range keys {
		if s := p.Score(k); s > 0 {
			out[k] = s
		}
	}
	return out
}
