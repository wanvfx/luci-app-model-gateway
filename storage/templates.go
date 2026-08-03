package storage

// templates.go 提示词模板存储：落 MODEL_GATEWAY_DATA/templates.json（禁 UCI dataDir 字段，
// 符合 iStoreOS 铁律）。模板内容原样保存（{{变量}} 替换由前端负责），后端仅做持久化与 CRUD。

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
)

const templateFile = "templates.json"

// Template 提示词模板
type Template struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Content   string `json:"content"` // 支持 {{变量}} 占位，原样存储
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

// TemplateStore 提示词模板存储（线程安全）
type TemplateStore struct {
	dir string
	mu  sync.Mutex
}

// NewTemplateStore 创建存储（dataDir 约定与 vkeys/history 一致：调用方传入 MODEL_GATEWAY_DATA）
func NewTemplateStore(dataDir string) *TemplateStore {
	return &TemplateStore{dir: dataDir}
}

func (s *TemplateStore) path() string { return filepath.Join(s.dir, templateFile) }

// Load 读取全部模板；文件不存在时返回空切片且不报错（与 vkeys 约定一致）
func (s *TemplateStore) Load() ([]Template, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := os.ReadFile(s.path())
	if err != nil {
		if os.IsNotExist(err) {
			return []Template{}, nil
		}
		return nil, err
	}
	var ts []Template
	if err := json.Unmarshal(b, &ts); err != nil {
		// 文件损坏时打印告警，便于定位（与 vkeys 行为一致），但返回空切片不崩溃
		log.Printf("[templates] 解析 %s 失败（文件可能损坏），模板列表将为空: %v", s.path(), err)
		return []Template{}, nil
	}
	if ts == nil {
		ts = []Template{}
	}
	return ts, nil
}

// Save 原子写入全部模板（写临时文件再 rename，避免半截文件）
func (s *TemplateStore) Save(ts []Template) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dir == "" {
		return nil
	}
	if err := os.MkdirAll(s.dir, 0755); err != nil {
		return err
	}
	if ts == nil {
		ts = []Template{}
	}
	b, err := json.MarshalIndent(ts, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path() + ".tmp"
	if err := os.WriteFile(tmp, b, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path())
}

// GenTemplateID 生成新的模板 ID（导出的包级函数，供 api 层创建模板时调用）
func GenTemplateID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "tpl-" + hex.EncodeToString(b)
}
