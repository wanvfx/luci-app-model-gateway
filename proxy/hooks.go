package proxy

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/wanvfx/luci-app-model-gateway/config"
	"github.com/wanvfx/luci-app-model-gateway/internal/netutil"
)

// hookNoRedirect 禁止跟随重定向：webhook 投递不需要跳转，
// 且跟随 302 可被恶意端点用来把请求弹射到内网/云元数据（SSRF 绕过）。
// ErrUseLastResponse 使 3xx 原样返回，按非 2xx 计失败走重试。
func hookNoRedirect(req *http.Request, via []*http.Request) error {
	return http.ErrUseLastResponse
}

// hookClient webhook 出站客户端（P0 修复）：
// 复用 internal/netutil 的 SSRF 安全拨号——域名先解析、只直连非拦截 IP（防 DNS 重绑定），
// 默认拦截回环/链路本地（含云元数据 169.254.169.254）/未指定/多播，放行 RFC1918 以兼容
// 局域网自托管 webhook 服务；并禁止跟随重定向。旧版用裸 http.Client，
// hookURLAllowed 的静态检查可被 DNS 重绑定或 302 跳转绕过。
var hookClient = &http.Client{
	Timeout: 5 * time.Second,
	Transport: &http.Transport{
		Proxy:               netutil.BypassProxyFunc,
		DialContext:         netutil.SSRFSafeDialContext(nil),
		MaxIdleConns:        10,
		IdleConnTimeout:     60 * time.Second,
		TLSHandshakeTimeout: 5 * time.Second,
	},
	CheckRedirect: hookNoRedirect,
}

// hookLoopbackClient 本机回环钩子专用客户端：
// http://127.0.0.1|localhost|::1 是策略显式放行的本机投递通道（SSRF 安全拨号会拦回环，
// 故单独直连），同样禁止跟随重定向。
var hookLoopbackClient = &http.Client{
	Timeout:       5 * time.Second,
	CheckRedirect: hookNoRedirect,
}

// hookURLLoopback 判断钩子 URL 的主机是否为显式放行的本机回环地址。
func hookURLLoopback(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := u.Hostname()
	return host == "127.0.0.1" || host == "localhost" || host == "::1"
}

// 钩子事件
const (
	HookEventDone          = "request_done"
	HookEventFailed        = "request_failed"
	HookEventProviderDown  = "provider_down"  // 熔断/锁定触发，provider 视为下线
	HookEventProviderUp    = "provider_up"    // 熔断恢复（closed），provider 重新上线
	HookEventQuotaExceeded = "quota_exceeded" // 虚拟密钥/账户额度耗尽
	HookEventCircuitOpen   = "circuit_open"   // 熔断器打开（含锁定层级）
)

// HookEventLifecycle 返回所有生命周期类事件名（供前端/配置提示用）
func HookEventLifecycle() []string {
	return []string{HookEventProviderDown, HookEventProviderUp, HookEventQuotaExceeded, HookEventCircuitOpen}
}

// HookPayload 钩子投递的请求上下文
type HookPayload struct {
	Event     string `json:"event"`
	Time      int64  `json:"time"`
	Model     string `json:"model"`
	Provider  string `json:"provider"`
	Stream    bool   `json:"stream"`
	Tokens    int    `json:"tokens"`
	RequestID string `json:"request_id,omitempty"`
	Error     string `json:"error,omitempty"`
}

// deadLetter 死信队列条目（A17）
type deadLetter struct {
	HookURL  string      `json:"hook_url"`
	Payload  HookPayload `json:"payload"`
	Attempts int         `json:"attempts"`
	LastErr  string      `json:"last_error"`
	Time     time.Time   `json:"time"`
}

var (
	dlqMu     sync.Mutex
	dlqPath   string
	dlqBuffer []deadLetter
	dlqMax    = 200 // 内存缓冲上限，超过则 oldest-first 裁剪
)

// HookDispatcher REST 回调钩子分发器（iStoreOS 插件机制：Go 对外发 HTTP，CGO-free）
type HookDispatcher struct {
	hooks []*config.Hook
}

// NewHookDispatcher 从配置构建分发器
func NewHookDispatcher(hooks []*config.Hook) *HookDispatcher {
	return &HookDispatcher{hooks: hooks}
}

// Fire 触发事件：对每个匹配且启用的钩子，后台异步 POST，带 SSRF 护栏与可选 HMAC 签名
func (d *HookDispatcher) Fire(event, model, provider string, stream bool, tokens int, errStr, requestID string) {
	if d == nil {
		return
	}
	for _, h := range d.hooks {
		if h == nil || !h.Enabled || h.URL == "" || len(h.Events) == 0 {
			continue
		}
		matched := false
		for _, ev := range h.Events {
			if ev == event {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		payload := HookPayload{
			Event:     event,
			Time:      time.Now().Unix(),
			Model:     model,
			Provider:  provider,
			Stream:    stream,
			Tokens:    tokens,
			RequestID: requestID,
			Error:     errStr,
		}
		go d.deliver(h, payload)
	}
}

// deliver 实际投递（后台协程内调用），失败最多重试 3 次（指数退避）。
func (d *HookDispatcher) deliver(h *config.Hook, payload HookPayload) {
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	// SSRF 护栏：仅放行 https 任意域名；http 仅允许 127.0.0.1/localhost（防内网探测）
	if !hookURLAllowed(h.URL) {
		log.Printf("[hook] blocked unsafe url: %s", h.URL)
		return
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 500 * time.Millisecond) // 0.5s, 1s 退避
		}
		req, err := http.NewRequest(http.MethodPost, h.URL, strings.NewReader(string(body)))
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "model-gateway-hook/1.0")
		if h.Secret != "" {
			mac := hmac.New(sha256.New, []byte(h.Secret))
			mac.Write(body)
			req.Header.Set("X-Hook-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		}
		// P0 修复：非回环钩子走 SSRF 安全拨号客户端（防 DNS 重绑定/302 弹射内网）；
		// 显式放行的本机回环钩子走直连客户端（安全拨号会拦回环）。
		client := hookClient
		if hookURLLoopback(h.URL) {
			client = hookLoopbackClient
		}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		_ = resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return // 投递成功
		}
		lastErr = fmt.Errorf("http status %d", resp.StatusCode)
	}
	if lastErr != nil {
		log.Printf("[hook] deliver failed after retries: %v", lastErr)
		appendDeadLetter(h, payload, lastErr)
	}
}

// appendDeadLetter 将失败投递写入死信队列（A17），超阈值则归档并告警。
func appendDeadLetter(h *config.Hook, payload HookPayload, lastErr error) {
	dlqMu.Lock()
	defer dlqMu.Unlock()
	entry := deadLetter{
		HookURL:  h.URL,
		Payload:  payload,
		Attempts: 3,
		LastErr:  lastErr.Error(),
		Time:     time.Now(),
	}
	dlqBuffer = append(dlqBuffer, entry)
	if len(dlqBuffer) > dlqMax {
		dlqBuffer = dlqBuffer[len(dlqBuffer)-dlqMax:]
	}
	if dlqPath != "" {
		b, _ := json.Marshal(entry)
		if f, err := os.OpenFile(dlqPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644); err == nil {
			_, _ = f.Write(append(b, '\n'))
			_ = f.Close()
		}
	}
	if len(dlqBuffer) >= dlqMax {
		log.Printf("[hook] dead-letter queue reached %d entries, consider reviewing failed hooks", dlqMax)
	}
}

// InitDeadLetterQueue 初始化死信队列落盘（由 main.go 调用）。
func InitDeadLetterQueue(dir string) {
	if dlqPath != "" {
		return
	}
	dlqPath = filepath.Join(dir, "hook_dlq.json")
}

// hookURLAllowed SSRF 护栏：scheme 必须为 http/https；
// https 任意主机允许；http 仅允许 127.0.0.1 / localhost（防路由器内网探测）。
func hookURLAllowed(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	switch u.Scheme {
	case "https":
		return true
	case "http":
		host := u.Hostname()
		return host == "127.0.0.1" || host == "localhost" || host == "::1"
	default:
		return false
	}
}
