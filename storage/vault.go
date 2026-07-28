package storage

// vault.go 密钥保险库：AES-256-GCM 加解密 provider api_key。
// 主密钥为 32 字节随机数，首次使用时生成并落 MODEL_GATEWAY_DATA/vault.key（0600），
// 符合 iStoreOS 铁律（可写数据统一走 MODEL_GATEWAY_DATA，不新增 UCI dataDir 字段）。
// UCI 中密文以 "enc:" 前缀 + base64 存储；明文 key 原样兼容（渐进迁移，不破坏旧配置）。

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// EncPrefix 密文标识前缀
const EncPrefix = "enc:"

// Vault 密钥保险库
type Vault struct {
	mu   sync.Mutex
	dir  string
	key  []byte // 32 字节主密钥（惰性加载/生成）
	fail bool   // 主密钥不可用（生成/读取失败），降级为明文直通
}

// NewVault 创建保险库（不立即生成主密钥，首次加/解密时惰性初始化）
func NewVault(dataDir string) *Vault {
	return &Vault{dir: dataDir}
}

// masterKey 加载或生成主密钥
func (v *Vault) masterKey() ([]byte, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.key != nil {
		return v.key, nil
	}
	if v.fail {
		return nil, errors.New("vault master key unavailable")
	}
	if v.dir == "" {
		v.fail = true
		return nil, errors.New("vault dir empty")
	}
	path := filepath.Join(v.dir, "vault.key")
	if data, err := os.ReadFile(path); err == nil {
		raw, derr := base64.StdEncoding.DecodeString(strings.TrimSpace(string(data)))
		if derr == nil && len(raw) == 32 {
			v.key = raw
			return v.key, nil
		}
		// 文件损坏：不覆盖（避免已有密文永久不可解），降级明文
		v.fail = true
		return nil, errors.New("vault.key corrupted")
	}
	// 首次生成
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		v.fail = true
		return nil, err
	}
	if err := os.MkdirAll(v.dir, 0755); err != nil {
		v.fail = true
		return nil, err
	}
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(raw)), 0600); err != nil {
		v.fail = true
		return nil, err
	}
	v.key = raw
	return v.key, nil
}

// Encrypt 加密明文，返回 "enc:" 前缀密文。已是密文/空串/主密钥不可用时原样返回。
func (v *Vault) Encrypt(plain string) string {
	if v == nil || plain == "" || strings.HasPrefix(plain, EncPrefix) {
		return plain
	}
	key, err := v.masterKey()
	if err != nil {
		return plain // 降级：保持明文（功能可用性优先于加密）
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return plain
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return plain
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return plain
	}
	sealed := gcm.Seal(nonce, nonce, []byte(plain), nil)
	return EncPrefix + base64.StdEncoding.EncodeToString(sealed)
}

// Decrypt 解密 "enc:" 前缀密文；非密文原样返回；解密失败返回空串（避免把密文当明文发给上游）。
func (v *Vault) Decrypt(s string) string {
	if v == nil || !strings.HasPrefix(s, EncPrefix) {
		return s
	}
	key, err := v.masterKey()
	if err != nil {
		return ""
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(s, EncPrefix))
	if err != nil {
		return ""
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return ""
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return ""
	}
	if len(raw) < gcm.NonceSize() {
		return ""
	}
	plain, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
	if err != nil {
		return ""
	}
	return string(plain)
}

// IsEncrypted 判断是否为密文形态
func IsEncrypted(s string) bool {
	return strings.HasPrefix(s, EncPrefix)
}
