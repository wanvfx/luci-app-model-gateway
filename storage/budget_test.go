package storage

import (
	"testing"
)

// 预算记账：成本累计与状态阈值
func TestBudgetAddCostAndStatus(t *testing.T) {
	dir := t.TempDir()
	b := NewBudget(dir)

	// 1M prompt @ $2/M + 0.5M completion @ $6/M = 2 + 3 = 5 USD
	cost, total := b.AddCost("m", "p", 1_000_000, 500_000, 2.0, 6.0)
	if cost < 4.99 || cost > 5.01 {
		t.Fatalf("cost = %v, want ~5", cost)
	}
	if total != cost {
		t.Fatalf("dailyTotal = %v, want %v", total, cost)
	}

	// limit=10, warn 80% → 5/10=50%，状态 ok
	st := b.Status(10, "block", 80)
	if st.Status != "ok" || st.Blocked {
		t.Fatalf("50%% usage should be ok, got %+v", st)
	}

	// 再加 4 USD → 9/10=90% > 80% → warn（未超限不拦截）
	b.AddCost("m", "p", 2_000_000, 0, 2.0, 0)
	st = b.Status(10, "block", 80)
	if st.Status != "warn" || st.Blocked {
		t.Fatalf("90%% usage should warn without block, got %+v", st)
	}

	// 再加 2 USD → 11/10 超限：action=block 时 Blocked=true
	b.AddCost("m", "p", 1_000_000, 0, 2.0, 0)
	st = b.Status(10, "block", 80)
	if st.Status != "exceeded" || !st.Blocked {
		t.Fatalf("over limit with block action should block, got %+v", st)
	}
	// action=warn 时超限但不拦截
	st = b.Status(10, "warn", 80)
	if st.Status != "exceeded" || st.Blocked {
		t.Fatalf("over limit with warn action should not block, got %+v", st)
	}
	// limit=0 不限
	st = b.Status(0, "block", 80)
	if st.Blocked {
		t.Fatalf("no limit should never block, got %+v", st)
	}
}

// 持久化：重建实例后当日累计仍在
func TestBudgetPersistence(t *testing.T) {
	dir := t.TempDir()
	b1 := NewBudget(dir)
	b1.AddCost("m", "p", 1_000_000, 0, 3.0, 0) // 3 USD

	b2 := NewBudget(dir)
	if got := b2.DailyTotal(); got < 2.99 || got > 3.01 {
		t.Fatalf("persisted total = %v, want ~3", got)
	}
}
