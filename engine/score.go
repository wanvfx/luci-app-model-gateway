package engine

import (
	"math"
	"sync"
	"time"
)

// ProbeResult 单次探测结果
type ProbeResult struct {
	Model   string
	OK      bool
	Latency time.Duration
	Time    time.Time
}

// ModelScore 模型质量分
type ModelScore struct {
	Model     string
	Score     float64
	Latency   time.Duration
	Available bool
}

const (
	QualityWindow = 20 // 滑动窗口大小
	// 权重：可用率 70%、延迟 30%（见 Score 内 score 计算）
	AvailabilityWeight = 0.7
	LatencyWeight      = 0.3
)

// Scorer 质量分计算器
type Scorer struct {
	mu      sync.Mutex
	history []ProbeResult
}

// NewScorer 创建 scorer
func NewScorer() *Scorer {
	return &Scorer{}
}

// Record 记录一次探测结果
func (s *Scorer) Record(r ProbeResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.history = append(s.history, r)
	if len(s.history) > QualityWindow {
		s.history = s.history[1:]
	}
}

// Score 计算模型质量分（0-100，越高越好）
func (s *Scorer) Score(model string) ModelScore {
	s.mu.Lock()
	defer s.mu.Unlock()

	var totalLatency time.Duration
	var okCount int
	var totalCount int

	for _, r := range s.history {
		if r.Model != model {
			continue
		}
		totalCount++
		if r.OK {
			okCount++
			totalLatency += r.Latency
		}
	}

	if totalCount == 0 {
		return ModelScore{Model: model, Score: 50, Available: true}
	}

	availability := float64(okCount) / float64(totalCount)
	avgLatency := totalLatency / time.Duration(totalCount)

	// 延迟分：100ms 得 100 分，每增加 100ms 扣 10 分，最低 0
	latencyScore := 100.0 - math.Max(0, (float64(avgLatency.Milliseconds())-100)/100*10)

	// 综合分：可用率 70% + 延迟 30%，二者都归一化到 0~100 后按权重加权。
	// P3-2：显式写出权重系数（0.7/0.3），避免旧写法 availability*70 + latencyScore*0.3
	// 量纲混用导致「可用率满分到底是 70 还是 100」的歧义。
	availabilityScore := availability * 100 // 0~100
	score := availabilityScore*AvailabilityWeight + latencyScore*LatencyWeight

	return ModelScore{
		Model:     model,
		Score:     score,
		Latency:   avgLatency,
		Available: okCount > 0,
	}
}

// Rank 按质量分排序候选模型（从高到低）
func (s *Scorer) Rank(models []string) []ModelScore {
	var scores []ModelScore
	for _, m := range models {
		scores = append(scores, s.Score(m))
	}
	// 按分数降序
	for i := 0; i < len(scores); i++ {
		for j := i + 1; j < len(scores); j++ {
			if scores[j].Score > scores[i].Score {
				scores[i], scores[j] = scores[j], scores[i]
			}
		}
	}
	return scores
}
