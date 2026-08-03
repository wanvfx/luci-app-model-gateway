package engine

import (
	"strings"
	"sync"
	"time"
)

// HookSink 是 engine 向外部（proxy 钩子分发器）异步通知生命周期事件的回调。
// 由 proxy 在启动时注册，避免 engine 反向依赖 proxy（循环导入）。
// 事件名见 proxy/hooks.go 的 HookEvent* 常量。
var HookSink func(event, provider, extra string)

// FireHook 向 HookSink 派发一个生命周期事件（key 形如 "provider||model"，自动提取 provider）。
// HookSink 未注册时为空操作。
func FireHook(event, key, extra string) {
	if HookSink == nil {
		return
	}
	provider := key
	if i := strings.Index(key, "||"); i >= 0 {
		provider = key[:i]
	}
	HookSink(event, provider, extra)
}

// CircuitPool 线程安全的熔断器池（按模型 key 管理）
// key 格式："provider||model"，与 Python 原版一致
type CircuitPool struct {
	mu        sync.Mutex
	breakers  map[string]*CircuitBreaker
	threshold int
	window    time.Duration
}

// NewCircuitPool 创建熔断器池
func NewCircuitPool(threshold int, window time.Duration) *CircuitPool {
	return &CircuitPool{
		breakers:  make(map[string]*CircuitBreaker),
		threshold: threshold,
		window:    window,
	}
}

// Get 获取或创建指定 key 的熔断器
func (cp *CircuitPool) Get(key string) *CircuitBreaker {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	if cb, ok := cp.breakers[key]; ok {
		return cb
	}
	cb := NewCircuitBreaker(cp.threshold, cp.window)
	cp.breakers[key] = cb
	return cb
}

// IsOpen 检查指定 key 的熔断器是否处于熔断状态
func (cp *CircuitPool) IsOpen(key string) bool {
	return cp.Get(key).Allow() == false
}

// RecordSuccess 记录指定 key 的成功
func (cp *CircuitPool) RecordSuccess(key string) {
	prev := cp.State(key)
	cp.Get(key).Success()
	cp.fireTransition(key, prev)
}

// RecordFailure 记录指定 key 的失败
func (cp *CircuitPool) RecordFailure(key string) {
	prev := cp.State(key)
	cp.Get(key).Failure()
	cp.fireTransition(key, prev)
}

// FailKind 失败归因类型（熔断只应惩罚「上游服务真的坏了」的情况）
type FailKind int

const (
	FailServer FailKind = iota // 超时/连接失败/5xx —— 计入熔断
	FailRate                   // 429 限流 —— 上游活着只是被限，不计入熔断
	FailAuth                   // 401/403 鉴权失败 —— 配置问题，不计入熔断
	FailQuota                  // 402/额度耗尽 —— 配置/账务问题，不计入熔断
	FailClient                 // 其他 4xx / 客户端取消 —— 请求问题，不计入熔断
)

// RecordFailureWithType 按失败类型记录：仅服务端故障（FailServer）跳闸，
// 429/配额/鉴权/客户端取消不误杀（修复「限流一次就熔断整个模型」的误杀问题）。
func (cp *CircuitPool) RecordFailureWithType(key string, kind FailKind) {
	prev := cp.State(key)
	if kind == FailServer {
		cp.Get(key).Failure()
	}
	// 非服务端故障：不计失败也不清零（保留已有失败计数，避免掩盖真实故障）
	cp.fireTransition(key, prev)
}

// fireTransition 在熔断状态发生转移时派发生命周期事件：
//   - 进入 open / locked  → circuit_open + provider_down
//   - 从非 closed 恢复为 closed → provider_up
func (cp *CircuitPool) fireTransition(key, prev string) {
	newState := cp.State(key)
	if newState == prev {
		return
	}
	if newState == "open" || newState == "locked" {
		FireHook("circuit_open", key, "")
		FireHook("provider_down", key, "")
	} else if newState == "closed" && prev != "closed" {
		FireHook("provider_up", key, "")
	}
}

// ClassifyStatus 按 HTTP 状态码归因失败类型
func ClassifyStatus(code int) FailKind {
	switch {
	case code == 429:
		return FailRate
	case code == 401 || code == 403:
		return FailAuth
	case code == 402:
		return FailQuota
	case code >= 500:
		return FailServer
	case code >= 400:
		return FailClient
	default:
		return FailServer
	}
}

// ClassifyNetErr 按网络错误归因：客户端主动取消不算上游故障，其余（超时/连接失败）算服务端故障
func ClassifyNetErr(err error) FailKind {
	if err == nil {
		return FailServer
	}
	msg := err.Error()
	if strings.Contains(msg, "context canceled") {
		return FailClient
	}
	return FailServer
}

// State 返回指定 key 的熔断状态
func (cp *CircuitPool) State(key string) string {
	return cp.Get(key).State()
}

// Locked 返回指定 key 是否处于锁定状态（C4 模型锁定自愈）
func (cp *CircuitPool) Locked(key string) bool {
	return cp.Get(key).Locked()
}
