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

// PollRecord 单次巡检记录
type PollRecord struct {
	Time    time.Time `json:"time"`
	Model   string    `json:"model"`
	OK      bool      `json:"ok"`
	Latency int64     `json:"latency_ms"`
	Error   string    `json:"error,omitempty"`
}

// History 巡检历史存储（JSONL，5MB×3 rotate）
type History struct {
	dir     string
	file    *os.File
	encoder *json.Encoder
	mu      sync.Mutex
	size    int64
	maxSize int64
}

// NewHistory 创建历史存储
func NewHistory(dir string) (*History, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	path := dir + "/history.jsonl"
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, err
	}
	stat, _ := f.Stat()
	return &History{
		dir:     dir,
		file:    f,
		encoder: json.NewEncoder(f),
		size:    stat.Size(),
		maxSize: 5 << 20, // 5MB
	}, nil
}

// Append 追加一条记录（超限 rotate）
func (h *History) Append(r PollRecord) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.size >= h.maxSize {
		if err := h.rotate(); err != nil {
			return err
		}
	}
	if err := h.encoder.Encode(r); err != nil {
		return err
	}
	// 按实际序列化长度累计（+1 为 Encode 附加的换行符），避免粗估导致轮转滞后
	if b, err := json.Marshal(r); err == nil {
		h.size += int64(len(b)) + 1
	} else {
		h.size += 128
	}
	return nil
}

// Read 读取最近 hours 小时内的巡检记录（包括轮转文件）
func (h *History) Read(hours int) ([]PollRecord, error) {
	cutoff := time.Now().Add(time.Duration(-hours) * time.Hour)
	var records []PollRecord

	// 读取当前文件和所有轮转文件（.1, .2, .3）
	files := []string{
		h.dir + "/history.jsonl",
		h.dir + "/history.1.jsonl",
		h.dir + "/history.2.jsonl",
		h.dir + "/history.3.jsonl",
	}

	for _, path := range files {
		f, err := os.Open(path)
		if err != nil {
			continue // 文件不存在则跳过
		}
		decoder := json.NewDecoder(f)
		for decoder.More() {
			var r PollRecord
			if err := decoder.Decode(&r); err != nil {
				continue
			}
			if r.Time.After(cutoff) {
				records = append(records, r)
			}
		}
		f.Close()
	}
	return records, nil
}

// Cleanup 清理超过 maxAge 的过期记录
func (h *History) Cleanup(maxAge time.Duration) (int, error) {
	path := h.dir + "/history.jsonl"
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
		var r PollRecord
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

// rotate 轮转：当前文件改名为 .1，旧的 .1→.2，.2→.3 删除
func (h *History) rotate() error {
	h.file.Close()
	for i := 2; i >= 1; i-- {
		src := h.dir + "/history." + strconv.Itoa(i-1) + ".jsonl"
		dst := h.dir + "/history." + strconv.Itoa(i) + ".jsonl"
		os.Rename(src, dst)
	}
	h.size = 0
	f, err := os.OpenFile(h.dir+"/history.jsonl", os.O_TRUNC|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	h.file = f
	h.encoder = json.NewEncoder(f)
	return nil
}

// Close 关闭文件
func (h *History) Close() error {
	return h.file.Close()
}
