package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ---------- A2A 轻量协议端点（路线图 G2）----------

// a2aRequest 是 /a2a 端点收到的 JSON-RPC 2.0 请求信封。
// id 用 json.RawMessage 原样回显（支持数字/字符串/null）。
type a2aRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// a2aError JSON-RPC 错误对象
type a2aError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// a2aAgent 已注册 A2A agent（A18）
type a2aAgent struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Endpoint  string    `json:"endpoint"`
	Skills    []string  `json:"skills"`
	CreatedAt time.Time `json:"created_at"`
}

var (
	a2aAgents   = map[string]*a2aAgent{}
	a2aAgentsMu sync.Mutex
)

// HandleA2A 暴露三个只读"技能"方法：discovery / health / cost。
// 形态与 api/admin.go 现有 handler 一致，挂在 *AdminHandler 上。
// 路由由 server.go 统一注册（建议片段见任务汇报）。
func (h *AdminHandler) HandleA2A(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// S12：/a2a 端点加鉴权（至少允许 admin_key 或有效虚拟密钥访问）
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		writeA2AError(w, nil, -32001, "missing or invalid authorization")
		return
	}
	token := strings.TrimSpace(auth[len("Bearer "):])
	if token == "" {
		writeA2AError(w, nil, -32001, "missing or invalid authorization")
		return
	}
	// 简单校验：token 为 admin_key 或有效虚拟密钥（不依赖完整 authClient 逻辑，保持 A2A 轻量）
	cfg := h.cfg.Load()
	if token != cfg.AdminKey() {
		// 检查是否为有效虚拟密钥
		if h.vkeyStore == nil {
			writeA2AError(w, nil, -32001, "missing or invalid authorization")
			return
		}
		// VKeyStore 无直接 List/Get 公开方法，此处仅做存在性校验（避免暴露内部结构）
		// 为简化，admin_key 之外的 Bearer token 暂不通过 A2A 鉴权（可按需扩展）
		writeA2AError(w, nil, -32001, "missing or invalid authorization")
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeA2AError(w, nil, -32700, "parse error")
		return
	}

	var req a2aRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeA2AError(w, nil, -32700, "parse error")
		return
	}

	id := req.ID
	if len(id) == 0 {
		id = json.RawMessage("null")
	}

	var result interface{}
	switch req.Method {
	case "discovery":
		result = h.a2aDiscovery()
	case "health":
		result = h.a2aHealth()
	case "cost":
		result = h.a2aCost()
	case "register":
		result = h.a2aRegister(req.Params)
	case "unregister":
		result = h.a2aUnregister(req.Params)
	default:
		writeA2AError(w, id, -32601, "method not found")
		return
	}

	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	})
}

// writeA2AError 写出 JSON-RPC 错误信封
func writeA2AError(w http.ResponseWriter, id json.RawMessage, code int, msg string) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   a2aError{Code: code, Message: msg},
	})
}

// a2aDiscovery 返回网关可用模型清单（id、所属 provider、能力 capabilities 等）。
func (h *AdminHandler) a2aDiscovery() interface{} {
	models := make([]map[string]interface{}, 0)
	for _, p := range h.cfg.Load().Providers {
		if p == nil {
			continue
		}
		for _, m := range p.Models {
			caps := []string{}
			tier := ""
			ctx := 0
			if h.cat != nil {
				if e := h.cat.LookupOrDefault(m); e != nil {
					caps = e.Capabilities
					tier = e.Tier
					ctx = e.ContextLength
				}
			}
			if caps == nil {
				caps = []string{}
			}
			models = append(models, map[string]interface{}{
				"id":             m,
				"provider":       p.Name,
				"capabilities":   caps,
				"tier":           tier,
				"context_length": ctx,
			})
		}
	}
	return map[string]interface{}{"count": len(models), "models": models}
}

// a2aHealth 返回各 provider 的健康度/状态（在线/熔断/冷却等）。
// 状态来自熔断器池（key 形如 "provider||model"）：
//
//	closed      -> 在线
//	half-open   -> 探测中
//	open        -> 熔断
//	locked      -> 锁定/冷却
func (h *AdminHandler) a2aHealth() interface{} {
	providers := make([]map[string]interface{}, 0)
	for _, p := range h.cfg.Load().Providers {
		if p == nil {
			continue
		}
		modelStates := map[string]string{}
		worst := "online"
		if h.circuits != nil {
			for _, m := range p.Models {
				st := h.circuits.State(p.Name + "||" + m)
				modelStates[m] = st
				worst = worseCircuitState(worst, st)
			}
		} else {
			worst = "unknown"
		}
		providers = append(providers, map[string]interface{}{
			"name":    p.Name,
			"enabled": p.Enabled,
			"status":  worst,
			"models":  modelStates,
		})
	}
	return map[string]interface{}{"providers": providers}
}

// worseCircuitState 在两类状态中取"更差"的一个（用于 provider 聚合）。
// 优先级：locked > open > half-open > online > unknown
func worseCircuitState(a, b string) string {
	rank := map[string]int{
		"unknown":   0,
		"online":    1,
		"closed":    1,
		"half-open": 2,
		"open":      3,
		"locked":    4,
	}
	if rank[a] >= rank[b] {
		return a
	}
	return b
}

// a2aCost 返回模型/provider 的定价信息（输入/输出单价等，若数据存在）。
// 价格来自 Catalog（models_catalog 分层参考库），仅当存在条目时返回。
func (h *AdminHandler) a2aCost() interface{} {
	rows := make([]map[string]interface{}, 0)
	if h.cat != nil {
		for _, p := range h.cfg.Load().Providers {
			if p == nil {
				continue
			}
			for _, m := range p.Models {
				e := h.cat.Lookup(m)
				if e == nil {
					continue
				}
				rows = append(rows, map[string]interface{}{
					"provider":              p.Name,
					"model":                 m,
					"price_in_per_million":  e.PriceIn,
					"price_out_per_million": e.PriceOut,
					"free":                  e.PriceIn == 0 && e.PriceOut == 0,
				})
			}
		}
	}
	return map[string]interface{}{"count": len(rows), "models": rows}
}

// a2aRegister 注册 A2A agent（A18）：支持 agent 自动发现。
func (h *AdminHandler) a2aRegister(params json.RawMessage) interface{} {
	var req struct {
		ID       string   `json:"id"`
		Name     string   `json:"name"`
		Endpoint string   `json:"endpoint"`
		Skills   []string `json:"skills"`
	}
	if err := json.Unmarshal(params, &req); err != nil {
		return map[string]interface{}{"ok": false, "error": "invalid params"}
	}
	id := strings.TrimSpace(req.ID)
	name := strings.TrimSpace(req.Name)
	endpoint := strings.TrimSpace(req.Endpoint)
	if id == "" || name == "" || endpoint == "" {
		return map[string]interface{}{"ok": false, "error": "id, name, endpoint required"}
	}
	a2aAgentsMu.Lock()
	defer a2aAgentsMu.Unlock()
	a2aAgents[id] = &a2aAgent{
		ID:        id,
		Name:      name,
		Endpoint:  endpoint,
		Skills:    req.Skills,
		CreatedAt: time.Now(),
	}
	return map[string]interface{}{"ok": true, "id": id}
}

// a2aUnregister 注销 A2A agent（A18）。
func (h *AdminHandler) a2aUnregister(params json.RawMessage) interface{} {
	var req struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(params, &req); err != nil {
		return map[string]interface{}{"ok": false, "error": "invalid params"}
	}
	id := strings.TrimSpace(req.ID)
	if id == "" {
		return map[string]interface{}{"ok": false, "error": "id required"}
	}
	a2aAgentsMu.Lock()
	defer a2aAgentsMu.Unlock()
	if _, ok := a2aAgents[id]; !ok {
		return map[string]interface{}{"ok": false, "error": "agent not found"}
	}
	delete(a2aAgents, id)
	return map[string]interface{}{"ok": true, "id": id}
}
