package api

// templates.go 提示词模板 CRUD 处理器（G4 后端部分）。
// 复用 api/admin.go 中 AdminHandler 的形态（Server 结构体方法、JSON 响应、switch 按 Method 分发），
// 并复用 proxy/server.go 的 admin_key Bearer 鉴权逻辑（此处镜像实现 requireAdmin，
// 即使路由脱离 adminAuth 中间件也自带鉴权保护）。

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/wanvfx/luci-app-model-gateway/storage"
)

// requireAdmin 校验请求是否携带合法的 Bearer admin_key（逻辑与 proxy.Server.authenticateAdmin 一致）
func (h *AdminHandler) requireAdmin(r *http.Request) bool {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return false
	}
	parts := strings.SplitN(auth, " ", 2)
	if len(parts) != 2 || parts[0] != "Bearer" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(parts[1]), []byte(h.cfg.Load().AdminKey())) == 1
}

func (h *AdminHandler) writeError(w http.ResponseWriter, code int, msg string) {
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// HandleTemplates 处理 /api/templates 与 /api/templates/{id}
func (h *AdminHandler) HandleTemplates(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if !h.requireAdmin(r) {
		h.writeError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	switch r.Method {
	case http.MethodGet:
		if r.URL.Path == "/api/templates" {
			h.handleTemplatesList(w, r)
			return
		}
		h.writeError(w, http.StatusNotFound, "not found")
	case http.MethodPost:
		if r.URL.Path == "/api/templates" {
			h.handleTemplatesCreate(w, r)
			return
		}
		h.writeError(w, http.StatusNotFound, "not found")
	case http.MethodPut:
		h.handleTemplatesUpdate(w, r)
	case http.MethodDelete:
		h.handleTemplatesDelete(w, r)
	default:
		h.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (h *AdminHandler) handleTemplatesList(w http.ResponseWriter, r *http.Request) {
	store := storage.NewTemplateStore(h.dataDir)
	ts, err := store.Load()
	if err != nil {
		h.writeError(w, http.StatusInternalServerError, fmt.Sprintf("load templates failed: %v", err))
		return
	}
	_ = json.NewEncoder(w).Encode(ts)
}

func (h *AdminHandler) handleTemplatesCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name    string `json:"name"`
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid request: %v", err))
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		h.writeError(w, http.StatusBadRequest, "name is required")
		return
	}

	store := storage.NewTemplateStore(h.dataDir)
	ts, err := store.Load()
	if err != nil {
		h.writeError(w, http.StatusInternalServerError, fmt.Sprintf("load templates failed: %v", err))
		return
	}

	t := storage.Template{
		ID:        storage.GenTemplateID(),
		Name:      name,
		Content:   req.Content,
		CreatedAt: time.Now().Unix(),
		UpdatedAt: time.Now().Unix(),
	}
	ts = append(ts, t)
	if err := store.Save(ts); err != nil {
		h.writeError(w, http.StatusInternalServerError, fmt.Sprintf("save templates failed: %v", err))
		return
	}
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(t)
}

func (h *AdminHandler) handleTemplatesUpdate(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/templates/")
	if id == "" {
		h.writeError(w, http.StatusBadRequest, "template id required")
		return
	}

	var req struct {
		Name    string `json:"name"`
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid request: %v", err))
		return
	}

	store := storage.NewTemplateStore(h.dataDir)
	ts, err := store.Load()
	if err != nil {
		h.writeError(w, http.StatusInternalServerError, fmt.Sprintf("load templates failed: %v", err))
		return
	}

	for i := range ts {
		if ts[i].ID == id {
			if strings.TrimSpace(req.Name) != "" {
				ts[i].Name = strings.TrimSpace(req.Name)
			}
			ts[i].Content = req.Content
			ts[i].UpdatedAt = time.Now().Unix()
			if err := store.Save(ts); err != nil {
				h.writeError(w, http.StatusInternalServerError, fmt.Sprintf("save templates failed: %v", err))
				return
			}
			_ = json.NewEncoder(w).Encode(ts[i])
			return
		}
	}
	h.writeError(w, http.StatusNotFound, "template not found")
}

func (h *AdminHandler) handleTemplatesDelete(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/templates/")
	if id == "" {
		h.writeError(w, http.StatusBadRequest, "template id required")
		return
	}

	store := storage.NewTemplateStore(h.dataDir)
	ts, err := store.Load()
	if err != nil {
		h.writeError(w, http.StatusInternalServerError, fmt.Sprintf("load templates failed: %v", err))
		return
	}

	kept := ts[:0]
	found := false
	for _, t := range ts {
		if t.ID == id {
			found = true
			continue
		}
		kept = append(kept, t)
	}
	if !found {
		h.writeError(w, http.StatusNotFound, "template not found")
		return
	}
	if err := store.Save(kept); err != nil {
		h.writeError(w, http.StatusInternalServerError, fmt.Sprintf("save templates failed: %v", err))
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}
