package proxy

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/wanvfx/luci-app-model-gateway/config"
)

// 钩子事件
const (
	HookEventDone   = "request_done"
	HookEventFailed = "request_failed"
)

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

// deliver 实际投递（后台协程内调用）
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
	req, err := http.NewRequest(http.MethodPost, h.URL, strings.NewReader(string(body)))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "model-gateway-hook/1.0")
	if h.Secret != "" {
		mac := hmac.New(sha256.New, []byte(h.Secret))
		mac.Write(body)
		req.Header.Set("X-Hook-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[hook] deliver failed: %v", err)
		return
	}
	_ = resp.Body.Close()
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
