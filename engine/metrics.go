package engine

// metrics.go Prometheus 文本格式指标（纯 Go，无第三方依赖，符合 iStoreOS 铁律）。
// 暴露 /metrics 端点，供 Prometheus / Uptime Kuma / Grafana Agent 抓取。
// 设计约束：标签基数受控——provider 数量有限（用户配置），model 不进 histogram 标签。

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// latencyBuckets 直方图桶边界（秒）
var latencyBuckets = []float64{0.1, 0.3, 0.5, 1, 2, 5, 10, 30, 60}

// counterKey 计数器标签组合
type counterKey struct {
	Provider string
	Model    string
	Status   string // ok | fail
}

// histo 单 provider 延迟直方图
type histo struct {
	counts []uint64 // len = len(latencyBuckets)+1（最后一个为 +Inf）
	sum    float64
	total  uint64
}

// Metrics 指标注册表（进程内聚合，重启清零——Prometheus counter 语义允许 reset）
type Metrics struct {
	mu        sync.Mutex
	startTime time.Time
	requests  map[counterKey]uint64
	tokens    map[string][2]uint64 // provider -> [promptTokens, completionTokens]
	histos    map[string]*histo    // provider -> 延迟直方图
}

// NewMetrics 创建指标注册表
func NewMetrics() *Metrics {
	return &Metrics{
		startTime: time.Now(),
		requests:  map[counterKey]uint64{},
		tokens:    map[string][2]uint64{},
		histos:    map[string]*histo{},
	}
}

// ObserveRequest 记录一次上游调用结果（status: ok|fail）
func (m *Metrics) ObserveRequest(provider, model, status string, dur time.Duration, promptTokens, completionTokens int) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests[counterKey{provider, model, status}]++
	if promptTokens > 0 || completionTokens > 0 {
		t := m.tokens[provider]
		t[0] += uint64(promptTokens)
		t[1] += uint64(completionTokens)
		m.tokens[provider] = t
	}
	h, ok := m.histos[provider]
	if !ok {
		h = &histo{counts: make([]uint64, len(latencyBuckets)+1)}
		m.histos[provider] = h
	}
	sec := dur.Seconds()
	h.sum += sec
	h.total++
	placed := false
	for i, b := range latencyBuckets {
		if sec <= b {
			h.counts[i]++
			placed = true
			break
		}
	}
	if !placed {
		h.counts[len(latencyBuckets)]++
	}
}

// escapeLabel 转义 Prometheus 标签值
func escapeLabel(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return s
}

// Render 输出 Prometheus text exposition format。
// extra 允许调用方注入即时值（如缓存命中率、当日预算），格式 name -> value（gauge）。
func (m *Metrics) Render(extra map[string]float64) string {
	if m == nil {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	var sb strings.Builder

	// 进程运行时长
	sb.WriteString("# HELP model_gateway_uptime_seconds 进程运行时长（秒）\n")
	sb.WriteString("# TYPE model_gateway_uptime_seconds gauge\n")
	fmt.Fprintf(&sb, "model_gateway_uptime_seconds %.0f\n", time.Since(m.startTime).Seconds())

	// 请求计数
	sb.WriteString("# HELP model_gateway_requests_total 上游调用总数（按 provider/model/status）\n")
	sb.WriteString("# TYPE model_gateway_requests_total counter\n")
	keys := make([]counterKey, 0, len(m.requests))
	for k := range m.requests {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Provider != keys[j].Provider {
			return keys[i].Provider < keys[j].Provider
		}
		if keys[i].Model != keys[j].Model {
			return keys[i].Model < keys[j].Model
		}
		return keys[i].Status < keys[j].Status
	})
	for _, k := range keys {
		fmt.Fprintf(&sb, "model_gateway_requests_total{provider=\"%s\",model=\"%s\",status=\"%s\"} %d\n",
			escapeLabel(k.Provider), escapeLabel(k.Model), escapeLabel(k.Status), m.requests[k])
	}

	// token 计数
	sb.WriteString("# HELP model_gateway_tokens_total 消耗 token 总数（按 provider/direction）\n")
	sb.WriteString("# TYPE model_gateway_tokens_total counter\n")
	provs := make([]string, 0, len(m.tokens))
	for p := range m.tokens {
		provs = append(provs, p)
	}
	sort.Strings(provs)
	for _, p := range provs {
		t := m.tokens[p]
		fmt.Fprintf(&sb, "model_gateway_tokens_total{provider=\"%s\",direction=\"prompt\"} %d\n", escapeLabel(p), t[0])
		fmt.Fprintf(&sb, "model_gateway_tokens_total{provider=\"%s\",direction=\"completion\"} %d\n", escapeLabel(p), t[1])
	}

	// 延迟直方图
	sb.WriteString("# HELP model_gateway_request_duration_seconds 上游调用耗时直方图（按 provider）\n")
	sb.WriteString("# TYPE model_gateway_request_duration_seconds histogram\n")
	hprovs := make([]string, 0, len(m.histos))
	for p := range m.histos {
		hprovs = append(hprovs, p)
	}
	sort.Strings(hprovs)
	for _, p := range hprovs {
		h := m.histos[p]
		cum := uint64(0)
		for i, b := range latencyBuckets {
			cum += h.counts[i]
			fmt.Fprintf(&sb, "model_gateway_request_duration_seconds_bucket{provider=\"%s\",le=\"%g\"} %d\n", escapeLabel(p), b, cum)
		}
		cum += h.counts[len(latencyBuckets)]
		fmt.Fprintf(&sb, "model_gateway_request_duration_seconds_bucket{provider=\"%s\",le=\"+Inf\"} %d\n", escapeLabel(p), cum)
		fmt.Fprintf(&sb, "model_gateway_request_duration_seconds_sum{provider=\"%s\"} %g\n", escapeLabel(p), h.sum)
		fmt.Fprintf(&sb, "model_gateway_request_duration_seconds_count{provider=\"%s\"} %d\n", escapeLabel(p), h.total)
	}

	// 调用方注入的即时 gauge（缓存命中/条目、预算等）
	extraNames := make([]string, 0, len(extra))
	for name := range extra {
		extraNames = append(extraNames, name)
	}
	sort.Strings(extraNames)
	for _, name := range extraNames {
		fmt.Fprintf(&sb, "# TYPE %s gauge\n%s %g\n", name, name, extra[name])
	}

	return sb.String()
}
