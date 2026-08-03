// Package calllog 记录最近的上游调用（与 Python 原版 1.6.1 的 call_log 等价）。
// 设计为进程内、有界环形缓冲（最多 MaxEntries 条），重启即清空——与 Python 内存队列行为一致。
// 由 proxy 包（每次上游调用时追加）与 api 包（管理面 /api/call-log 读取）共用，无循环依赖。
// A10：同时落盘到 dataDir/calllog.ndjson，供日报与历史回溯。
package calllog

import (
	"encoding/json"
	"log"
	"os"
	"sync"
	"time"
)

// MaxEntries 内存中保留的最大调用记录数（与 Python 原版 CALL_LOG_MAX=100 一致）
const MaxEntries = 100

// Entry 单条调用记录
type Entry struct {
	Time     string `json:"time"`     // 本地时间 HH:MM:SS
	Provider string `json:"provider"` // 账号（provider 名）
	Model    string `json:"model"`    // 模型名
	Status   string `json:"status"`   // "ok" | "fail"
	Tokens   int    `json:"tokens"`   // 本次消耗 token（失败为 0）
	Error    string `json:"error"`    // 失败原因（成功为空）
}

var (
	mu  sync.Mutex
	buf []Entry
)

// fileStore 持久化存储（A10）
var logFile *os.File
var logPath string

// InitFileStore 初始化落盘（由 main.go 在启动时调用一次）。
func InitFileStore(dir string) {
	if logFile != nil {
		return
	}
	logPath = dir + "/calllog.ndjson"
	if f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644); err == nil {
		logFile = f
	} else {
		log.Printf("[calllog] open log file failed: %v", err)
	}
}

// CloseFileStore 关闭落盘句柄（由 main.go 在退出时调用）。
func CloseFileStore() {
	if logFile != nil {
		_ = logFile.Close()
		logFile = nil
	}
}

// Append 追加一条调用记录；超过上限时丢弃最旧的一条。
func Append(e Entry) {
	mu.Lock()
	defer mu.Unlock()
	if e.Time == "" {
		e.Time = time.Now().Format("15:04:05")
	}
	buf = append(buf, e)
	if len(buf) > MaxEntries {
		buf = buf[len(buf)-MaxEntries:]
	}

	// A10：落盘（追加一行 JSON）
	if logFile != nil {
		b, _ := json.Marshal(e)
		if _, err := logFile.Write(append(b, '\n')); err != nil {
			log.Printf("[calllog] write failed: %v", err)
		}
	}
}

// List 返回当前所有记录的快照（按追加顺序，最新在末尾）。
func List() []Entry {
	mu.Lock()
	defer mu.Unlock()
	out := make([]Entry, len(buf))
	copy(out, buf)
	return out
}
