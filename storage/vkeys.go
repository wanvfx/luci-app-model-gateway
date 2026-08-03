package storage

// vkeys.go 虚拟密钥（子密钥）存储：落 MODEL_GATEWAY_DATA/vkeys.json（禁 UCI dataDir 字段，
// 符合 iStoreOS 铁律）。下游客户端用 Bearer <Key> 访问网关，可独立限额（每日请求/Token）、
// 到期/禁用；主密钥 admin_key 不受影响。配额按本地自然日清零。

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const vkeyFile = "vkeys.json"

// VKey 虚拟密钥
type VKey struct {
	ID            string   `json:"id"`
	Key           string   `json:"key"` // sk-vk-... 明文（仅创建时返回一次，列表接口已脱敏）
	Name          string   `json:"name"`
	Enabled       bool     `json:"enabled"`
	QuotaRequests int      `json:"quota_requests"` // 每日请求上限（0=不限）
	QuotaTokens   int      `json:"quota_tokens"`   // 每日 Token 上限（0=不限）
	AllowedModels []string `json:"allowed_models"` // 允许访问的模型/路由组（空=不限）
	Notes         string   `json:"notes"`
	CreatedAt     string   `json:"created_at"`
}

// vkeyUsage 当日用量（按本地自然日清零）
type vkeyUsage struct {
	Date     string `json:"date"`     // YYYY-MM-DD
	Requests int    `json:"requests"` // 当日累计请求数
	Tokens   int    `json:"tokens"`   // 当日累计 Token 数
}

type vkeyStoreFile struct {
	Keys  []*VKey               `json:"keys"`
	Usage map[string]*vkeyUsage `json:"usage"` // id -> 当日用量
}

// VKeyStore 虚拟密钥存储
type VKeyStore struct {
	dir  string
	mu   sync.Mutex
	data vkeyStoreFile
}

// NewVKeyStore 创建存储（自动加载既有数据）
func NewVKeyStore(dataDir string) *VKeyStore {
	s := &VKeyStore{dir: dataDir, data: vkeyStoreFile{Usage: map[string]*vkeyUsage{}}}
	s.load()
	return s
}

func (s *VKeyStore) path() string { return filepath.Join(s.dir, vkeyFile) }

func (s *VKeyStore) load() {
	b, err := os.ReadFile(s.path())
	if err == nil {
		// 可观测性修复：文件损坏时此前错误被静默吞掉，所有虚拟密钥凭空消失且无任何日志。
		// 现在打印告警日志，便于定位（数据文件在 MODEL_GATEWAY_DATA/vkeys.json）。
		if uerr := json.Unmarshal(b, &s.data); uerr != nil {
			log.Printf("[vkeys] 解析 %s 失败（文件可能损坏），虚拟密钥将暂不可用: %v", s.path(), uerr)
		}
	}
	if s.data.Usage == nil {
		s.data.Usage = map[string]*vkeyUsage{}
	}
	// 清理非今日的用量（防无限增长）
	today := time.Now().Format("2006-01-02")
	for id, u := range s.data.Usage {
		if u.Date != today {
			delete(s.data.Usage, id)
		}
	}
}

func (s *VKeyStore) save() error {
	if s.dir == "" {
		return nil
	}
	// P2-4：锁内序列化、锁外写盘，避免持锁执行 I/O 阻塞并发读。
	// 调用方需先释放 s.mu 再调用本函数（sync.Mutex 不可重入，否则死锁）。
	s.mu.Lock()
	b, err := json.MarshalIndent(s.data, "", "  ")
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, 0755); err != nil {
		return err
	}
	tmp := s.path() + ".tmp"
	if err := os.WriteFile(tmp, b, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path())
}

func genVKey() string {
	b := make([]byte, 18)
	_, _ = rand.Read(b)
	return "sk-vk-" + hex.EncodeToString(b)
}

// Add 新增虚拟密钥（生成 id + key），返回创建的密钥（含明文 key）
func (s *VKeyStore) Add(name string, quotaReq, quotaTok int, allowed []string, notes string) (*VKey, error) {
	s.mu.Lock()
	vk := &VKey{
		ID:            genVKey(),
		Key:           genVKey(),
		Name:          strings.TrimSpace(name),
		Enabled:       true,
		QuotaRequests: quotaReq,
		QuotaTokens:   quotaTok,
		AllowedModels: allowed,
		Notes:         notes,
		CreatedAt:     time.Now().Format(time.RFC3339),
	}
	s.data.Keys = append(s.data.Keys, vk)
	s.mu.Unlock()
	if err := s.save(); err != nil {
		return nil, err
	}
	// 返回副本（含明文 key）；列表接口会脱敏
	return vk, nil
}

// List 返回全部虚拟密钥（Key 脱敏为 sk-vk-****，避免泄露）
func (s *VKeyStore) List() []*VKey {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*VKey, 0, len(s.data.Keys))
	for _, vk := range s.data.Keys {
		c := *vk
		if len(c.Key) > 8 {
			c.Key = c.Key[:4] + "****" + c.Key[len(c.Key)-4:]
		} else {
			c.Key = "****"
		}
		out = append(out, &c)
	}
	return out
}

// Get 按 id 获取（Key 明文，仅内部/管理使用）。返回深拷贝，避免调用方在锁外使用时与 Update 并发修改 race。
func (s *VKeyStore) Get(id string) *VKey {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, vk := range s.data.Keys {
		if vk.ID == id {
			c := *vk
			return &c
		}
	}
	return nil
}

// Delete 按 id 删除
func (s *VKeyStore) Delete(id string) error {
	s.mu.Lock()
	kept := s.data.Keys[:0]
	found := false
	for _, vk := range s.data.Keys {
		if vk.ID == id {
			found = true
			continue
		}
		kept = append(kept, vk)
	}
	if !found {
		s.mu.Unlock()
		return os.ErrNotExist
	}
	s.data.Keys = kept
	delete(s.data.Usage, id)
	s.mu.Unlock()
	return s.save()
}

// Update 按 id 更新可变更字段（启用状态/备注名/配额/模型白名单）。
// 明文 Key 不可改（改了等于换新密钥，应当由调用方走「删除+新建」）。
func (s *VKeyStore) Update(id string, name string, enabled bool, quotaReq, quotaTok int, allowed []string, notes string) error {
	s.mu.Lock()
	var target *VKey
	for _, vk := range s.data.Keys {
		if vk.ID == id {
			target = vk
			break
		}
	}
	if target == nil {
		s.mu.Unlock()
		return os.ErrNotExist
	}
	target.Name = strings.TrimSpace(name)
	target.Enabled = enabled
	target.QuotaRequests = quotaReq
	target.QuotaTokens = quotaTok
	target.AllowedModels = allowed
	target.Notes = notes
	s.mu.Unlock()
	return s.save()
}

// Validate 校验 Bearer token，返回匹配的已启用密钥（否则 nil）。
// 返回深拷贝（含 AllowedModels slice），避免调用方在锁外使用时与 Update() 并发修改 race。
// 使用 subtle.ConstantTimeCompare 防止时序侧信道攻击。
func (s *VKeyStore) Validate(token string) *VKey {
	if token == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, vk := range s.data.Keys {
		if vk.Enabled && subtle.ConstantTimeCompare([]byte(vk.Key), []byte(token)) == 1 {
			c := *vk
			return &c
		}
	}
	return nil
}

// QuotaExceeded 判断密钥当日配额是否已耗尽。
// vk 为 Validate 返回的深拷贝，读取 vk.QuotaRequests/QuotaTokens 无 race；
// 但 todayUsage 读 s.data.Usage map 需加锁。
func (s *VKeyStore) QuotaExceeded(vk *VKey) bool {
	if vk.QuotaRequests <= 0 && vk.QuotaTokens <= 0 {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.todayUsage(vk.ID)
	if vk.QuotaRequests > 0 && u.Requests >= vk.QuotaRequests {
		return true
	}
	if vk.QuotaTokens > 0 && u.Tokens >= vk.QuotaTokens {
		return true
	}
	return false
}

// Allowed 判断模型/路由组是否在允许列表内（空列表=不限）
func (s *VKeyStore) Allowed(vk *VKey, model string) bool {
	if len(vk.AllowedModels) == 0 {
		return true
	}
	m := strings.ToLower(strings.TrimSpace(model))
	for _, a := range vk.AllowedModels {
		if strings.ToLower(strings.TrimSpace(a)) == m {
			return true
		}
	}
	return false
}

// RecordUsage 累加当日用量（请求数 + Token 数）
func (s *VKeyStore) RecordUsage(id string, reqs, toks int) {
	s.mu.Lock()
	u := s.todayUsage(id)
	u.Requests += reqs
	u.Tokens += toks
	s.data.Usage[id] = u
	s.mu.Unlock()
	_ = s.save()
}

// UsageOf 返回密钥当日用量快照。加锁防止与 RecordUsage 并发读写 map。
func (s *VKeyStore) UsageOf(id string) (requests, tokens int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.todayUsage(id)
	return u.Requests, u.Tokens
}

// todayUsage 返回密钥当日用量指针。调用方必须持有 s.mu 锁。
func (s *VKeyStore) todayUsage(id string) *vkeyUsage {
	today := time.Now().Format("2006-01-02")
	u, ok := s.data.Usage[id]
	if !ok || u.Date != today {
		return &vkeyUsage{Date: today}
	}
	return u
}
