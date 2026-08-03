package proxy

import (
	"fmt"
	"sync"
	"time"
)

// budgetAlertState 记录「今天已经就哪一档预算状态告过警」，用于 A11 阈值告警去重。
// 没有它的话，超过告警线之后每一个请求都会打一次 webhook，用户会被刷屏。
// 跨日自动重置（day 字段变化即清空 last）。
type budgetAlertState struct {
	mu   sync.Mutex
	day  string // YYYY-MM-DD
	last string // 已告警的最高档："" | "warn" | "exceeded"
}

// shouldFire 判断当前状态是否需要发出告警（同一天同一档只发一次；升档才再发）。
func (b *budgetAlertState) shouldFire(status string) bool {
	if status != "warn" && status != "exceeded" {
		return false
	}
	today := time.Now().Format("2006-01-02")
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.day != today {
		b.day = today
		b.last = ""
	}
	// 档位升序：warn(1) < exceeded(2)；只有更高档才再次告警
	rank := func(s string) int {
		switch s {
		case "warn":
			return 1
		case "exceeded":
			return 2
		}
		return 0
	}
	if rank(status) <= rank(b.last) {
		return false
	}
	b.last = status
	return true
}

// budgetDegradeActive 判定是否应触发 A3 预算降级（免费/低价模型优先）。
// 仅在已配置每日限额、且当日累计已达告警线（warn）或已超额但未拦截（warn 模式的 exceeded）时为真。
// action=block 的 exceeded 会在更早处直接 429 拦截，不会走到这里。
func (s *Server) budgetDegradeActive() bool {
	if s.budget == nil {
		return false
	}
	bcfg := s.cfg.Load().EffectiveBudget()
	if bcfg.DailyLimitUSD <= 0 {
		return false
	}
	st := s.budget.Status(bcfg.DailyLimitUSD, bcfg.Action, bcfg.WarningPct)
	return st.Status == "warn" || st.Status == "exceeded"
}

// fireBudgetThreshold 在预算达到告警线/超额时推送 quota_exceeded 生命周期钩子（A11）。
// 去重由 budgetAlertState 保证：同一天同一档只推一次。
func (s *Server) fireBudgetThreshold(status string) {
	if !s.budgetAlert.shouldFire(status) {
		return
	}
	bcfg := s.cfg.Load().EffectiveBudget()
	st := s.budget.Status(bcfg.DailyLimitUSD, bcfg.Action, bcfg.WarningPct)
	detail := fmt.Sprintf("daily budget %s: $%.4f / $%.2f (warn at $%.2f, action=%s)",
		status, st.DailyTotal, st.Limit, st.WarnAt, st.Action)
	s.hookDispatcher().Fire(HookEventQuotaExceeded, "", "budget", false, 0, detail, "")
}

// checkBudgetThreshold 在每次成本记账之后调用，负责在跨过告警线的那一刻推送钩子。
// 与 fireBudgetThreshold 分开是为了让调用方（记账路径）无需自己判状态。
func (s *Server) checkBudgetThreshold() {
	if s.budget == nil {
		return
	}
	bcfg := s.cfg.Load().EffectiveBudget()
	if bcfg.DailyLimitUSD <= 0 {
		return
	}
	st := s.budget.Status(bcfg.DailyLimitUSD, bcfg.Action, bcfg.WarningPct)
	s.fireBudgetThreshold(st.Status)
}
