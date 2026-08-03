package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// LastGoodTracker 末次成功优先（LKGP，C2）：
// 记住"上次哪个 provider 成功了"，平手裁决时给该候选小幅加权（如 +5 分，满分 100），
// 像叫外卖优先叫上次准时的那家。绝不盖过健康度/价格主排序，只做平手裁决。
// 可持久化到 MODEL_GATEWAY_DATA/lkgp.json（重启后保留"口味"）。
type LastGoodTracker struct {
	mu      sync.Mutex
	mapping map[string]string // logicalModel -> providerName
	dataDir string
	saveAt  time.Time
}

// LKGPExtraBoost 末次成功 provider 在质量排序中的加权值（满分 100）。
// 默认 +5，只影响平手裁决，不会压倒真实的健康度/延迟差距。
const LKGPExtraBoost = 5.0

// lkgpSaveInterval 落盘节流：避免每次成功都写盘（路由器 flash 寿命有限）
const lkgpSaveInterval = 5 * time.Second

// NewLastGoodTracker 创建末次成功追踪器（dataDir 为空则仅内存）
func NewLastGoodTracker(dataDir string) *LastGoodTracker {
	t := &LastGoodTracker{mapping: map[string]string{}, dataDir: dataDir}
	t.load()
	return t
}

// Record 记录逻辑模型上次成功回复的 provider
func (t *LastGoodTracker) Record(logicalModel, provider string) {
	if logicalModel == "" || provider == "" {
		return
	}
	t.mu.Lock()
	if t.mapping[logicalModel] == provider {
		t.mu.Unlock()
		return
	}
	t.mapping[logicalModel] = provider
	now := time.Now()
	due := now.Sub(t.saveAt) >= lkgpSaveInterval
	t.mu.Unlock()
	if due {
		t.save()
	}
}

// Get 返回逻辑模型末次成功的 provider（空串表示未知）
func (t *LastGoodTracker) Get(logicalModel string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.mapping[logicalModel]
}

// Boost 返回指定 provider 在该逻辑模型下的排序加权（非末次成功者返回 0）
func (t *LastGoodTracker) Boost(logicalModel, provider string) float64 {
	if provider == "" {
		return 0
	}
	if t.Get(logicalModel) == provider {
		return LKGPExtraBoost
	}
	return 0
}

func (t *LastGoodTracker) load() {
	if t.dataDir == "" {
		return
	}
	data, err := os.ReadFile(filepath.Join(t.dataDir, "lkgp.json"))
	if err != nil {
		return
	}
	_ = json.Unmarshal(data, &t.mapping)
}

func (t *LastGoodTracker) save() {
	if t.dataDir == "" {
		return
	}
	t.mu.Lock()
	t.saveAt = time.Now()
	data, err := json.Marshal(t.mapping)
	t.mu.Unlock()
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(t.dataDir, "lkgp.json"), data, 0644)
}

// Snapshot 返回当前所有末次成功映射（供调试/面板查看）
func (t *LastGoodTracker) Snapshot() map[string]string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[string]string, len(t.mapping))
	for k, v := range t.mapping {
		out[k] = v
	}
	return out
}
