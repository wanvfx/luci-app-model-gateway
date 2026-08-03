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
	// P1-3: 序列化与写盘都在锁内执行。
	// 早期「锁外写盘」（P2-4）与 Cleanup/rotate 在锁内替换 u.file 句柄形成 data race——
	// 并发时 Append 可能向已被 close/unlink 的旧 fd 写入导致记录丢失。
	// 此处将写盘收回锁内：O_APPEND 单次 Write 原子的前提不变，且保证与句柄替换串行。
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	line := append(b, '\n')
	u.size += int64(len(line))
	if _, werr := u.file.Write(line); werr != nil {
		return werr
	}
	return nil
}

// Read 读取最近 days 天内的用量记录
func (u *Usage) Read(days int) ([]UsageRecord, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
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
	u.mu.Lock()
	defer u.mu.Unlock()
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
		// 重写后旧 inode 已与目录项脱钩，需把追加句柄重定向到新文件，
		// 否则后续 Append 仍写入被 unlink 的旧文件导致数据丢失（且 size 计数错位）。
		if u.file != nil {
			u.file.Close()
		}
		nf, oerr := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if oerr == nil {
			u.file = nf
			u.encoder = json.NewEncoder(nf)
			u.size = 0
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
