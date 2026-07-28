package storage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Budget 预算/余额护栏：按日累计上游调用成本（USD），持久化到 dataDir/budget.json。
// 成本 = (prompt_tokens / 1e6) * price_in + (completion_tokens / 1e6) * price_out。
// 落 MODEL_GATEWAY_DATA（符合 iStoreOS 铁律：可写数据统一走该 env，不新增 UCI dataDir 字段）。
type Budget struct {
	dir  string
	mu   sync.Mutex
	data map[string]float64 // date(YYYY-MM-DD) -> 当日累计成本 USD
}

// NewBudget 创建预算追踪器并从磁盘加载
func NewBudget(dir string) *Budget {
	b := &Budget{dir: dir, data: map[string]float64{}}
	b.load()
	return b
}

// AddCost 累加一次请求的成本，返回本次成本与当日累计
func (b *Budget) AddCost(model, provider string, promptTokens, completionTokens int, priceIn, priceOut float64) (float64, float64) {
	cost := (float64(promptTokens)/1e6)*priceIn + (float64(completionTokens)/1e6)*priceOut
	if cost <= 0 {
		return 0, b.DailyTotal()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	today := time.Now().Format("2006-01-02")
	b.data[today] += cost
	b.cleanupLocked()
	b.persistLocked()
	return cost, b.data[today]
}

// DailyTotal 当日累计成本
func (b *Budget) DailyTotal() float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	today := time.Now().Format("2006-01-02")
	return b.data[today]
}

// BudgetStatus 预算状态
type BudgetStatus struct {
	DailyTotal float64 `json:"daily_total"`
	Limit      float64 `json:"limit"`        // 0 = 不限
	WarningPct int     `json:"warning_pct"`  // 预警阈值百分比
	Action     string  `json:"action"`       // warn | block
	Status     string  `json:"status"`       // ok | warn | exceeded
	WarnAt     float64 `json:"warn_at"`      // 触发预警的额度
	Blocked    bool    `json:"blocked"`       // 是否处于拦截状态
}

// Status 计算当前预算状态
func (b *Budget) Status(limit float64, action string, warningPct int) BudgetStatus {
	total := b.DailyTotal()
	if limit <= 0 {
		return BudgetStatus{DailyTotal: total, Limit: 0, WarningPct: warningPct, Action: action, Status: "ok", Blocked: false}
	}
	if warningPct <= 0 {
		warningPct = 80
	}
	warnAt := limit * float64(warningPct) / 100.0
	st := BudgetStatus{
		DailyTotal: total,
		Limit:      limit,
		WarningPct: warningPct,
		Action:     action,
		WarnAt:     warnAt,
		Status:     "ok",
		Blocked:    false,
	}
	switch {
	case total >= limit:
		st.Status = "exceeded"
		st.Blocked = action == "block"
	case total >= warnAt:
		st.Status = "warn"
	}
	return st
}

// load 从磁盘加载
func (b *Budget) load() {
	if b.dir == "" {
		return
	}
	path := filepath.Join(b.dir, "budget.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var m map[string]float64
	if err := json.Unmarshal(data, &m); err != nil {
		return
	}
	b.mu.Lock()
	b.data = m
	b.cleanupLocked()
	b.mu.Unlock()
}

// persistLocked 落盘（调用方持锁）
func (b *Budget) persistLocked() {
	if b.dir == "" {
		return
	}
	data, err := json.Marshal(b.data)
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(b.dir, "budget.json"), data, 0644)
}

// cleanupLocked 删除 30 天前的旧日期（调用方持锁）
func (b *Budget) cleanupLocked() {
	cutoff := time.Now().AddDate(0, 0, -30).Format("2006-01-02")
	for d := range b.data {
		if d < cutoff {
			delete(b.data, d)
		}
	}
}
