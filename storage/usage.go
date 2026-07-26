package storage

import (
	"bufio"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// UsageRecord 消耗统计记录
type UsageRecord struct {
	Time             time.Time `json:"time"`
	Provider         string    `json:"provider"`
	Model            string    `json:"model"`
	RequestID        string    `json:"request_id,omitempty"`
	PromptTokens     int       `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	TotalTokens      int       `json:"total_tokens"`
}

// Usage 消耗统计存储（JSONL，5MB×3 rotate）
type Usage struct {
	dir     string
	file    *os.File
	encoder *json.Encoder
	mu      sync.Mutex
	size    int64
	maxSize int64
}

// NewUsage 创建统计存储
func NewUsage(dir string) (*Usage, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	path := dir + "/usage.jsonl"
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, err
	}
	stat, _ := f.Stat()
	return &Usage{
		dir:     dir,
		file:    f,
		encoder: json.NewEncoder(f),
		size:    stat.Size(),
		maxSize: 5 << 20, // 5MB
	}, nil
}

// Append 追加记录
func (u *Usage) Append(r UsageRecord) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.size >= u.maxSize {
		if err := u.rotate(); err != nil {
			return err
		}
	}
	if err := u.encoder.Encode(r); err != nil {
		return err
	}
	// 按实际序列化长度累计（+1 为 Encode 附加的换行符），避免粗估导致轮转滞后
	if b, err := json.Marshal(r); err == nil {
		u.size += int64(len(b)) + 1
	} else {
		u.size += 128
	}
	return nil
}

// Read 读取最近 days 天内的用量记录
func (u *Usage) Read(days int) ([]UsageRecord, error) {
	path := u.dir + "/usage.jsonl"
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	cutoff := time.Now().AddDate(0, 0, -days)
	var records []UsageRecord
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var r UsageRecord
		if err := json.Unmarshal(scanner.Bytes(), &r); err != nil {
			continue
		}
		if r.Time.After(cutoff) {
			records = append(records, r)
		}
	}
	return records, scanner.Err()
}

// Cleanup 清理超过 maxAge 的过期记录
func (u *Usage) Cleanup(maxAge time.Duration) (int, error) {
	path := u.dir + "/usage.jsonl"
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	defer f.Close()

	cutoff := time.Now().Add(-maxAge)
	var kept []string
	removed := 0
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		var r UsageRecord
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			continue
		}
		if r.Time.After(cutoff) {
			kept = append(kept, line)
		} else {
			removed++
		}
	}
	if removed > 0 {
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, []byte(strings.Join(kept, "\n")+"\n"), 0644); err != nil {
			return 0, err
		}
		if err := os.Rename(tmp, path); err != nil {
			return 0, err
		}
	}
	return removed, nil
}

// rotate 轮转
func (u *Usage) rotate() error {
	u.file.Close()
	for i := 2; i >= 1; i-- {
		src := u.dir + "/usage." + strconv.Itoa(i-1) + ".jsonl"
		dst := u.dir + "/usage." + strconv.Itoa(i) + ".jsonl"
		os.Rename(src, dst)
	}
	u.size = 0
	f, err := os.OpenFile(u.dir+"/usage.jsonl", os.O_TRUNC|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	u.file = f
	u.encoder = json.NewEncoder(f)
	return nil
}

// Close 关闭
func (u *Usage) Close() error {
	return u.file.Close()
}
