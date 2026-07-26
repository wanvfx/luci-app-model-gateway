package proxy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// ModelMeta 模型元数据
type ModelMeta struct {
	Aliases           map[string]string            `json:"aliases"`
	ContextLimits     map[string]int               `json:"context_limits"`
	NonChatKeywords   []string                     `json:"non_chat_keywords"`
	ModelDescriptions map[string]map[string]string `json:"model_descriptions"`
	SupportsVision    map[string]bool              `json:"supports_vision"`
}

// MetaStore 模型元数据存储
type MetaStore struct {
	meta ModelMeta
}

// NewMetaStore 加载 models_meta.json（支持三层：内置 + 外部覆盖）
func NewMetaStore(appDir, dataDir string) *MetaStore {
	ms := &MetaStore{}

	// 默认空值
	ms.meta = ModelMeta{
		Aliases:           map[string]string{},
		ContextLimits:     map[string]int{},
		NonChatKeywords:   []string{},
		ModelDescriptions: map[string]map[string]string{},
		SupportsVision:    map[string]bool{},
	}

	// 1. 内置版（APP_DIR/models_meta.json）
	builtin := filepath.Join(appDir, "models_meta.json")
	if data, err := os.ReadFile(builtin); err == nil {
		ms.merge(data)
	}

	// 1.5 安装位置（/usr/share/model-gateway/models_meta.json，与 presets.json 查找方式一致）
	// appDir 在路由器上为 /usr/bin，故 .. 回退到 /usr，再进入 share/model-gateway
	share := filepath.Join(appDir, "..", "share", "model-gateway", "models_meta.json")
	if data, err := os.ReadFile(share); err == nil {
		ms.merge(data)
	}

	// 2. 外部版（DATA_DIR/models_meta.json）覆盖
	ext := filepath.Join(dataDir, "models_meta.json")
	if data, err := os.ReadFile(ext); err == nil {
		ms.merge(data)
	}

	return ms
}

// merge 合并 JSON（dict 深合并，其余覆盖）
func (ms *MetaStore) merge(data []byte) {
	var extra ModelMeta
	if err := json.Unmarshal(data, &extra); err != nil {
		return
	}
	if len(extra.Aliases) > 0 {
		for k, v := range extra.Aliases {
			ms.meta.Aliases[k] = v
		}
	}
	if len(extra.ContextLimits) > 0 {
		for k, v := range extra.ContextLimits {
			ms.meta.ContextLimits[k] = v
		}
	}
	if len(extra.NonChatKeywords) > 0 {
		ms.meta.NonChatKeywords = append(ms.meta.NonChatKeywords, extra.NonChatKeywords...)
	}
	if len(extra.ModelDescriptions) > 0 {
		for k, v := range extra.ModelDescriptions {
			if ms.meta.ModelDescriptions[k] == nil {
				ms.meta.ModelDescriptions[k] = map[string]string{}
			}
			for dk, dv := range v {
				ms.meta.ModelDescriptions[k][dk] = dv
			}
		}
	}
	if len(extra.SupportsVision) > 0 {
		for k, v := range extra.SupportsVision {
			ms.meta.SupportsVision[k] = v
		}
	}
}

// Aliases 返回别名表
func (ms *MetaStore) Aliases() map[string]string {
	return ms.meta.Aliases
}

// ContextLimits 返回上下文长度表
func (ms *MetaStore) ContextLimits() map[string]int {
	return ms.meta.ContextLimits
}

// NonChatKeywords 返回非对话关键词
func (ms *MetaStore) NonChatKeywords() []string {
	return ms.meta.NonChatKeywords
}

// ModelDescriptions 返回模型描述
func (ms *MetaStore) ModelDescriptions() map[string]map[string]string {
	return ms.meta.ModelDescriptions
}

// SupportsVision 返回支持视觉的模型表
func (ms *MetaStore) SupportsVision() map[string]bool {
	return ms.meta.SupportsVision
}

// IsChatModel 是否为对话模型
func (ms *MetaStore) IsChatModel(modelID string) bool {
	lower := strings.ToLower(modelID)
	for _, kw := range ms.meta.NonChatKeywords {
		if strings.Contains(lower, kw) {
			return false
		}
	}
	return true
}

// IsFreeModel 根据模型名判断是否免费
func (ms *MetaStore) IsFreeModel(modelID string) bool {
	lower := strings.ToLower(modelID)
	return strings.Contains(lower, ":free") || strings.Contains(lower, "-free") || strings.Contains(lower, "_free")
}

// ResolveModel 解析模型名（别名 -> 标准名）
func (ms *MetaStore) ResolveModel(model string) string {
	if v, ok := ms.meta.Aliases[model]; ok {
		return v
	}
	return model
}

// ContextLength 获取上下文长度
func (ms *MetaStore) ContextLength(model string) int {
	if v, ok := ms.meta.ContextLimits[model]; ok {
		return v
	}
	norm := strings.ToLower(model)
	for k, v := range ms.meta.ContextLimits {
		if strings.ToLower(k) == norm {
			return v
		}
	}
	return 32768
}

// SetContextLimit 设置/覆盖自定义上下文长度并持久化到 models_meta.json
func (ms *MetaStore) SetContextLimit(model string, length int, dataDir string) error {
	ms.meta.ContextLimits[model] = length
	return ms.persist(dataDir)
}

// DeleteContextLimit 删除自定义上下文长度并持久化到 models_meta.json
func (ms *MetaStore) DeleteContextLimit(model string, dataDir string) error {
	delete(ms.meta.ContextLimits, model)
	return ms.persist(dataDir)
}

// persist 将当前 meta 写入 DATA_DIR/models_meta.json
func (ms *MetaStore) persist(dataDir string) error {
	data, err := json.MarshalIndent(ms.meta, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dataDir, "models_meta.json")
	return os.WriteFile(path, data, 0644)
}

// AllModelDescriptions 返回所有模型描述（供 /api/model-details 使用）
func (ms *MetaStore) AllModelDescriptions() map[string]map[string]string {
	return ms.meta.ModelDescriptions
}

// AllAliases 返回所有别名（供 /api/model-details 使用）
func (ms *MetaStore) AllAliases() map[string]string {
	return ms.meta.Aliases
}

// IsVisionModel 是否支持识图
func (ms *MetaStore) IsVisionModel(model string) bool {
	if v, ok := ms.meta.SupportsVision[model]; ok {
		return v
	}
	norm := strings.ToLower(model)
	for k, v := range ms.meta.SupportsVision {
		if strings.ToLower(k) == norm {
			return v
		}
	}
	return false
}

// Is1MModel 是否支持 1M 上下文（与 Python 原版 is_1m_model 一致）
func (ms *MetaStore) Is1MModel(model string) bool {
	ctx := ms.ContextLength(model)
	return ctx >= 1048576 // 1M = 1048576
}

// NormalizeModel 归一化模型名：别名解析 → 去除 xxx/ 前缀 → 小写（与 Python 原版 normalize_model 一致）
func (ms *MetaStore) NormalizeModel(model string) string {
	// 1. 先应用别名
	if v, ok := ms.meta.Aliases[model]; ok {
		return v
	}
	// 2. 去除 xxx/ 前缀（如 deepseek-ai/deepseek-v4-flash → deepseek-v4-flash）
	if idx := strings.Index(model, "/"); idx >= 0 {
		model = model[idx+1:]
	}
	// 3. 小写
	return strings.ToLower(model)
}

// ContextLengthWithFallback 获取上下文长度（与 Python 原版 get_context_length 三步查找一致）
// 1. 精确匹配 → 2. 大小写不敏感 → 3. model_details 缓存 → 4. 默认 32768
func (ms *MetaStore) ContextLengthWithFallback(model string, modelCacheGet func(string) interface{ GetContextLen() int }) int {
	// 1. 精确匹配
	if v, ok := ms.meta.ContextLimits[model]; ok {
		return v
	}
	// 2. 大小写不敏感
	norm := strings.ToLower(model)
	for k, v := range ms.meta.ContextLimits {
		if strings.ToLower(k) == norm {
			return v
		}
	}
	// 3. model_details 缓存
	if modelCacheGet != nil {
		if d := modelCacheGet(model); d != nil {
			if ctx := d.GetContextLen(); ctx > 0 {
				return ctx
			}
		}
	}
	// 4. 默认值
	return 32768
}

// IsFreeModelWithPricing 根据 API 返回的 pricing 判断是否免费（与 Python 原版 is_free_model 一致）
func (ms *MetaStore) IsFreeModelWithPricing(modelID string, promptPrice, completionPrice float64) bool {
	return promptPrice == 0 && completionPrice == 0
}

// HERMES_MAP Hermes 工具名压缩映射
var HERMES_MAP = [][2]string{
	{"mcp_hermes_studio_use_hermes_studio_use_", "mcp_hsu_"},
	{"mcp_hermes_studio_devices_hermes_studio_lan_", "mcp_hsd_"},
	{"mcp_hermes_studio_api_hermes_studio_api_", "mcp_hsa_"},
}

// CompressHermes 压缩 Hermes 系工具名
func (ms *MetaStore) CompressHermes(body []byte) []byte {
	s := string(body)
	for _, pair := range HERMES_MAP {
		s = strings.ReplaceAll(s, pair[0], pair[1])
	}
	return []byte(s)
}

// RestoreHermesText 还原 Hermes 工具名
func (ms *MetaStore) RestoreHermesText(text string) string {
	for _, pair := range HERMES_MAP {
		text = strings.ReplaceAll(text, pair[1], pair[0])
	}
	return text
}

// LANG_HINT 强制中文回复提示
const LANG_HINT = "\n\n【重要】请始终使用简体中文回答用户。思考过程(reasoning)也请用中文。代码、命令、文件名、专有名词、标识符等保持原样即可，不要翻译。"

// EnsureLangReply 注入简体中文回复提示
func (ms *MetaStore) EnsureLangReply(body []byte) []byte {
	var full map[string]json.RawMessage
	if err := json.Unmarshal(body, &full); err != nil {
		return body
	}
	rawMsgs, ok := full["messages"]
	if !ok {
		return body
	}
	var msgs []json.RawMessage
	if err := json.Unmarshal(rawMsgs, &msgs); err != nil || len(msgs) == 0 {
		return body
	}

	var first json.RawMessage
	if err := json.Unmarshal(msgs[0], &first); err == nil {
		var msg map[string]interface{}
		if err := json.Unmarshal(first, &msg); err == nil {
			if msg["role"] == "system" {
				if c, ok := msg["content"].(string); ok && !strings.Contains(c, "请始终使用简体中文") {
					msg["content"] = c + LANG_HINT
					newFirst, _ := json.Marshal(msg)
					msgs[0] = newFirst
					full["messages"], _ = json.Marshal(msgs)
					out, _ := json.Marshal(full)
					return out
				}
				return body
			}
		}
	}

	newMsg := map[string]interface{}{
		"role":    "system",
		"content": "请使用简体中文回答。" + LANG_HINT,
	}
	newFirst, _ := json.Marshal(newMsg)
	msgs = append([]json.RawMessage{newFirst}, msgs...)
	full["messages"], _ = json.Marshal(msgs)
	out, _ := json.Marshal(full)
	return out
}
