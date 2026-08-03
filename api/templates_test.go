package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/wanvfx/luci-app-model-gateway/config"
	"github.com/wanvfx/luci-app-model-gateway/storage"
)

// newTestTemplateHandler 构造带临时数据目录与测试 admin_key 的 AdminHandler
func newTestTemplateHandler(t *testing.T) (*AdminHandler, string) {
	t.Helper()
	dir := t.TempDir()
	h := &AdminHandler{dataDir: dir}
	cfg := &config.Config{}
	cfg.SetAdminKey("sk-local-testkey0000000000000000")
	h.SetConfig(cfg)
	return h, dir
}

func authReq(method, target, body, key string) *http.Request {
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, target, bytes.NewBufferString(body))
	} else {
		r = httptest.NewRequest(method, target, nil)
	}
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	return r
}

func TestTemplatesCRUD(t *testing.T) {
	h, dir := newTestTemplateHandler(t)
	const key = "sk-local-testkey0000000000000000"

	// 无鉴权 → 401
	w := httptest.NewRecorder()
	h.HandleTemplates(w, authReq(http.MethodGet, "/api/templates", "", ""))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("期望 401，实际 %d", w.Code)
	}

	// 列出（初始为空）
	w = httptest.NewRecorder()
	h.HandleTemplates(w, authReq(http.MethodGet, "/api/templates", "", key))
	if w.Code != http.StatusOK {
		t.Fatalf("list 期望 200，实际 %d", w.Code)
	}
	var list []storage.Template
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("解析列表失败: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("初始应为空，实际 %d", len(list))
	}

	// 创建（缺 name → 400）
	w = httptest.NewRecorder()
	h.HandleTemplates(w, authReq(http.MethodPost, "/api/templates", `{"content":"hi"}`, key))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("缺 name 期望 400，实际 %d", w.Code)
	}

	// 创建
	createBody := `{"name":"问候","content":"你好 {{name}}"}`
	w = httptest.NewRecorder()
	h.HandleTemplates(w, authReq(http.MethodPost, "/api/templates", createBody, key))
	if w.Code != http.StatusCreated {
		t.Fatalf("create 期望 201，实际 %d body=%s", w.Code, w.Body.String())
	}
	var created storage.Template
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("解析创建结果失败: %v", err)
	}
	if created.ID == "" || created.Name != "问候" || created.Content != "你好 {{name}}" {
		t.Fatalf("创建结果异常: %+v", created)
	}
	if created.CreatedAt == 0 || created.UpdatedAt == 0 {
		t.Fatalf("时间戳未设置: %+v", created)
	}

	// 已落盘
	if _, err := os.Stat(filepath.Join(dir, "templates.json")); err != nil {
		t.Fatalf("templates.json 未落盘: %v", err)
	}

	// 更新
	updateBody := `{"name":"问候2","content":"您好 {{name}}"}`
	w = httptest.NewRecorder()
	h.HandleTemplates(w, authReq(http.MethodPut, "/api/templates/"+created.ID, updateBody, key))
	if w.Code != http.StatusOK {
		t.Fatalf("update 期望 200，实际 %d body=%s", w.Code, w.Body.String())
	}
	var updated storage.Template
	_ = json.Unmarshal(w.Body.Bytes(), &updated)
	if updated.Name != "问候2" || updated.Content != "您好 {{name}}" {
		t.Fatalf("更新结果异常: %+v", updated)
	}
	if updated.UpdatedAt < updated.CreatedAt {
		t.Fatalf("UpdatedAt 应不小于 CreatedAt")
	}

	// 更新不存在 → 404
	w = httptest.NewRecorder()
	h.HandleTemplates(w, authReq(http.MethodPut, "/api/templates/nope", updateBody, key))
	if w.Code != http.StatusNotFound {
		t.Fatalf("更新不存在期望 404，实际 %d", w.Code)
	}

	// 删除
	w = httptest.NewRecorder()
	h.HandleTemplates(w, authReq(http.MethodDelete, "/api/templates/"+created.ID, "", key))
	if w.Code != http.StatusOK {
		t.Fatalf("delete 期望 200，实际 %d", w.Code)
	}

	// 删除不存在 → 404
	w = httptest.NewRecorder()
	h.HandleTemplates(w, authReq(http.MethodDelete, "/api/templates/"+created.ID, "", key))
	if w.Code != http.StatusNotFound {
		t.Fatalf("重复删除期望 404，实际 %d", w.Code)
	}

	// 最终列表为空
	w = httptest.NewRecorder()
	h.HandleTemplates(w, authReq(http.MethodGet, "/api/templates", "", key))
	var finalList []storage.Template
	_ = json.Unmarshal(w.Body.Bytes(), &finalList)
	if len(finalList) != 0 {
		t.Fatalf("最终应为空，实际 %d", len(finalList))
	}
}

func TestStorageLoadTemplatesMissingFile(t *testing.T) {
	// 文件不存在时返回空切片且不报错
	ts, err := storage.NewTemplateStore(t.TempDir()).Load()
	if err != nil {
		t.Fatalf("不应报错，实际 %v", err)
	}
	if len(ts) != 0 {
		t.Fatalf("应为空切片，实际 %d", len(ts))
	}
}
