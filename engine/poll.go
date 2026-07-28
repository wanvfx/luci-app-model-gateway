package engine

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wanvfx/luci-app-model-gateway/config"
	"github.com/wanvfx/luci-app-model-gateway/storage"
)

// pollMaxCount 智能省电（limited）策略下的最大自动巡检次数，达到后进入休眠，
// 仅每天中午 12 点强制复查一次。与 Python 原版 1.6.1 POLL_MAX_COUNT 一致。
const pollMaxCount = 20

// lastPollTimeUnix 上次巡检完成的 Unix 时间戳，供 admin 接口读取
var lastPollTimeUnix atomic.Int64

// pollCountAtomic 已执行的自动巡检次数（供状态展示）
var pollCountAtomic atomic.Int64

// pollStageAtomic 当前巡检阶段：init | running | idle（供状态展示）
var pollStageAtomic atomic.Value

// LastPollTimeUnix 返回上次巡检完成的 Unix 时间戳（秒）
func LastPollTimeUnix() int64 {
	return lastPollTimeUnix.Load()
}

// PollCount 返回已执行的自动巡检次数
func PollCount() int64 {
	return pollCountAtomic.Load()
}

// PollMaxCount 返回智能省电策略的最大巡检次数
func PollMaxCount() int {
	return pollMaxCount
}

// PollStage 返回当前巡检阶段（init/running/idle）
func PollStage() string {
	if v := pollStageAtomic.Load(); v != nil {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return "init"
}

// kickCh 巡检唤醒信号（容量 1，重复 Kick 合并为一次）
var kickCh = make(chan struct{}, 1)

// KickPoll 唤醒巡检器立即执行一轮，并重置省电（limited）计数。
// 供 provider 配置变更（新增/编辑/删除/一键配置/模型勾选）后调用：
// 否则 limited 策略下巡检器可能已休眠（20 次用尽），新增平台的模型
// 要等到次日 12 点才会被探测 → 稳定性列表永远看不到新平台（bug #29）。
func KickPoll() {
	select {
	case kickCh <- struct{}{}:
	default: // 已有待处理的唤醒信号，合并
	}
}

// Poller 后台健康巡检
type Poller struct {
	interval time.Duration
	client   *http.Client
	scorer   *Scorer
	history  *storage.History
	circuits *CircuitPool
	resolve  func(string) string // 模型别名解析（Python: MODEL_ALIASES.get(model, model)），nil 则原样

	// 智能省电（limited）策略的内存态计数（重启归零，避免频繁写 flash）
	pollCount     int    // 已执行的自动巡检次数
	lastDailyPoll string // 上次执行每日复查的日期（YYYY-MM-DD）
}

// SetResolver 注入模型别名解析器（main.go 用 MetaStore.ResolveModel 注入）。
// Python 原版 check_model 探测前先做 MODEL_ALIASES 解析，否则别名模型探测 404 误报失败。
func (p *Poller) SetResolver(fn func(string) string) {
	p.resolve = fn
}

// NewPoller 创建巡检器
func NewPoller(interval time.Duration, scorer *Scorer, history *storage.History, client *http.Client, circuits *CircuitPool) *Poller {
	return &Poller{
		interval: interval,
		client:   client,
		scorer:   scorer,
		history:  history,
		circuits: circuits,
	}
}

// Start 启动后台巡检（阻塞直到 ctx 取消）。
// getStrategy 每轮读取最新策略，支持运行时热切换：
//   - "continuous"（1.6.0）：每个 interval 都巡检，持续监控，永不休眠。
//   - "limited"（1.6.1，默认）：密集巡检 pollMaxCount 次后进入休眠，
//     此后仅每天中午 12 点强制复查一次；ticker 仍每 interval 唤醒判断是否需复查。
func (p *Poller) Start(ctx context.Context, getProviders func() []*config.Provider, getStrategy func() string) {
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	// 首次立即执行
	p.runOnce(getProviders(), getStrategy())

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.runOnce(getProviders(), getStrategy())
		case <-kickCh:
			// 配置变更唤醒：仅重置智能省电计数，使巡检器恢复"密集巡检"阶段，
			// 新平台的模型会在下一个 ticker（≤ POLL_INTERVAL，默认 300s）被探测。
			// 与原 Python 1.6.1 一致：保存配置从不额外触发即时探测，
			// 避免上游被重复轰击、探针频率过快（问题 4 修复）。
			// 注意：不调用 runOnce，避免"每次保存都立即全量探测"的高频轰击。
			if p.pollCount >= pollMaxCount {
				p.pollCount = 0
				pollCountAtomic.Store(0)
			}
		}
	}
}

// runOnce 按当前策略决定本轮是否真正巡检
func (p *Poller) runOnce(providers []*config.Provider, strategy string) {
	if strategy == "continuous" {
		pollStageAtomic.Store("running")
		p.poll(providers)
		p.pollCount++
		pollCountAtomic.Store(int64(p.pollCount))
		return
	}

	// limited（智能省电，默认）
	now := time.Now()
	today := now.Format("2006-01-02")
	// 每天中午 12 点后，若当天尚未做过复查，则强制复查一次（即使已达上限）
	shouldDaily := today != p.lastDailyPoll && now.Hour() >= 12

	if p.pollCount >= pollMaxCount {
		// 已达密集巡检上限：进入休眠，不再累加「密集进度」计数（pollCount 保持 pollMaxCount）。
		// 每日复查会在此时执行一次真实探测以刷新模型状态，但「不」计入 pollCount，
		// 否则徽标「探针次数」会被每日静默复查无限累加（20→21→…→39），
		// 与前端「智能巡检约 20 次后休眠」的描述矛盾（问题修复）。
		if shouldDaily {
			pollStageAtomic.Store("running")
			p.poll(providers)
			p.lastDailyPoll = today
			pollStageAtomic.Store("idle")
		} else {
			pollStageAtomic.Store("idle")
		}
		return
	}

	pollStageAtomic.Store("running")
	p.poll(providers)
	p.pollCount++
	pollCountAtomic.Store(int64(p.pollCount))
}

// poll 执行一轮巡检：并发探测所有 provider 的所有模型（Semaphore(10)）
func (p *Poller) poll(providers []*config.Provider) {
	// 收集所有待探测任务
	type task struct {
		prov  *config.Provider
		model string
	}
	var tasks []task
	for _, prov := range providers {
		if prov == nil || !prov.Enabled {
			continue
		}
		for _, model := range prov.Models {
			tasks = append(tasks, task{prov: prov, model: model})
		}
	}

	if len(tasks) == 0 {
		lastPollTimeUnix.Store(time.Now().Unix())
		return
	}

	// 并发探测，Semaphore(10) 限制并发数
	sem := make(chan struct{}, 10)
	var wg sync.WaitGroup
	for _, t := range tasks {
		wg.Add(1)
		sem <- struct{}{} // 获取信号量
		go func(task task) {
			defer wg.Done()
			defer func() { <-sem }() // 释放信号量
			p.probeAndRecord(task.prov, task.model)
		}(t)
	}
	wg.Wait()

	lastPollTimeUnix.Store(time.Now().Unix())
}

// probeAndRecord 探测单个模型并记录结果
func (p *Poller) probeAndRecord(prov *config.Provider, model string) {
	modelKey := prov.Name + "||" + model

	ok, detail, latency, kind := p.checkModel(prov, model)

	result := ProbeResult{
		Model:   model,
		OK:      ok,
		Latency: latency,
		Time:    time.Now(),
	}
	p.scorer.Record(result)

	// 记录熔断状态（按失败归因：仅超时/连接失败/5xx 跳闸，429/鉴权/配额不误杀）
	if ok {
		p.circuits.RecordSuccess(modelKey)
	} else {
		p.circuits.RecordFailureWithType(modelKey, kind)
	}

	if err := p.history.Append(storage.PollRecord{
		Time:    time.Now(),
		Model:   modelKey,
		OK:      ok,
		Latency: latency.Milliseconds(),
		Error:   detail,
	}); err != nil {
		fmt.Printf("[poll] history append failed: %v\n", err)
	}

	status := "ok"
	if !ok {
		status = "fail"
	}
	fmt.Printf("[poll] %s/%s -> %s (%dms) %s\n", prov.Name, model, status, latency.Milliseconds(), detail)
}

// checkModel 对单个模型做真实探测（与原 Python 1.6.1 check_model 一致）：
// 先做别名解析，再 POST {BaseURL}/chat/completions（max_tokens=5、30s 超时）。
// 逐模型独立结果、生成延迟真实可比；GET /models 仅用于 Key 校验，不再用于巡检
//（端点级探活会导致每模型重复轰击 /models → 限流误报 + 所有模型共享一个结果）。
// 第 4 个返回值为失败归因（成功时无意义），供熔断决定是否跳闸
func (p *Poller) checkModel(prov *config.Provider, model string) (ok bool, detail string, latency time.Duration, kind FailKind) {
	actual := model
	if p.resolve != nil {
		actual = p.resolve(model)
	}
	return ChatProbe(prov.BaseURL, prov.APIKey, actual, p.client)
}
