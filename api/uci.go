package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
)

// UCITool UCI 配置操作工具
type UCITool struct {
	configName string
}

// NewUCITool 创建 UCI 工具
func NewUCITool(configName string) *UCITool {
	return &UCITool{configName: configName}
}

// ref 构造一个合法的 UCI 段/选项引用。
// UCI 的段引用规则：命名段用 name（如 settings → model-gateway.settings）；
// 匿名段用 @type[index] 或 cfgXXXX（如 @provider[0] → model-gateway.@provider[0]）。
// 无论哪种，sectionName 本身就是完整的段标识，直接拼 configName.sectionName 即可，
// 绝不能再拼成 configName.sectionType.sectionName（那样会变成非法的 4 段引用）。
// 仅当 sectionName 为空时，才退化为按类型引用 configName.sectionType。
func (u *UCITool) ref(sectionType, sectionName string) string {
	if sectionName != "" {
		return u.configName + "." + sectionName
	}
	return u.configName + "." + sectionType
}

// escapeUCIValue 转义单引号，供 uci batch 的单引号包裹使用。
func escapeUCIValue(v string) string {
	return strings.ReplaceAll(v, "'", `'\''`)
}

// SetOption 设置单个 option
// 段引用统一走 ref()：命名段 → config.name，匿名/类型 → config.type，
// 严禁拼出 config.type.name 的非法 4 段引用（历史 bug #03 / 铁律 #27）
func (u *UCITool) SetOption(sectionType, sectionName, key, value string) error {
	cmd := exec.Command("uci", "set", fmt.Sprintf("%s.%s=%s", u.ref(sectionType, sectionName), key, value))
	cmd.Dir = "/etc/config"
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("uci set failed: %v, output: %s", err, string(out))
	}
	return u.Commit()
}

// SetList 设置 list 项（先删除旧值再添加）
func (u *UCITool) SetList(sectionType, sectionName, key string, values []string) error {
	ref := u.ref(sectionType, sectionName)
	// 删除旧值
	cmd := exec.Command("uci", "delete", fmt.Sprintf("%s.%s", ref, key))
	cmd.Dir = "/etc/config"
	cmd.Run() // 忽略删除错误（可能不存在）

	// 添加新值
	for _, v := range values {
		cmd = exec.Command("uci", "add_list", fmt.Sprintf("%s.%s=%s", ref, key, v))
		cmd.Dir = "/etc/config"
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("uci add_list failed: %v, output: %s", err, string(out))
		}
	}
	return u.Commit()
}

// AddSection 添加新 section
func (u *UCITool) AddSection(sectionType string) (string, error) {
	cmd := exec.Command("uci", "add", u.configName, sectionType)
	cmd.Dir = "/etc/config"
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("uci add failed: %v, output: %s", err, string(out))
	}
	sectionName := strings.TrimSpace(string(out))
	if err := u.Commit(); err != nil {
		return "", err
	}
	return sectionName, nil
}

// DeleteSections 批量删除多个 section（原子操作，单次 commit）。
// 排序规则：匿名段 @type[N] 按 N 倒序在前，命名段在后。
// 倒序确保删除高索引段时不会影响低索引段的引用（避免索引漂移）；
// 命名段引用是稳定名称，放最后删不会被匿名段索引变化影响。
func (u *UCITool) DeleteSections(ids []string) error {
	if len(ids) == 0 {
		return nil
	}

	type sectionEntry struct {
		id     string
		isAnon bool
		index  int // 匿名段的 [N] 值
	}

	entries := make([]sectionEntry, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		// 解析 @type[N] 格式：提取 N
		if idx := strings.Index(id, "["); idx >= 0 {
			end := strings.Index(id[idx:], "]")
			if end >= 0 {
				n, err := strconv.Atoi(id[idx+1 : idx+end])
				if err == nil {
					entries = append(entries, sectionEntry{id, true, n})
					continue
				}
			}
		}
		// 命名段（如 cfg0a1b2c 或 nvidia）：index=0，放最后
		entries = append(entries, sectionEntry{id, false, 0})
	}

	// 排序：匿名段按 N 倒序（先删大号），命名段排在最后
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].isAnon != entries[j].isAnon {
			return entries[i].isAnon // 匿名段在前
		}
		if entries[i].isAnon {
			return entries[i].index > entries[j].index // 倒序
		}
		return false // 命名段保持原顺序
	})

	// 构建 uci batch：逐条 delete，最后一条 commit
	var buf bytes.Buffer
	for _, e := range entries {
		buf.WriteString(fmt.Sprintf("delete %s.%s\n", u.configName, e.id))
	}
	buf.WriteString(fmt.Sprintf("commit %s\n", u.configName))

	cmd := exec.Command("uci", "batch")
	cmd.Dir = "/etc/config"
	cmd.Stdin = strings.NewReader(buf.String())
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("uci batch delete failed: %v, output: %s", err, string(out))
	}
	return nil
}

// DeleteSectionsByName 删除指定类型中所有 name option 匹配的 section。
// 返回 (删除数量, error)。调用方可据此判断"未找到"。
func (u *UCITool) DeleteSectionsByName(sectionType, name string) (int, error) {
	sectionNames, err := u.GetSectionNames(sectionType)
	if err != nil {
		return 0, err
	}

	var toDelete []string
	for _, sn := range sectionNames {
		opts, err := u.GetOptions(sectionType, sn)
		if err != nil {
			continue
		}
		if opts["name"] == name {
			toDelete = append(toDelete, sn)
		}
	}

	if len(toDelete) == 0 {
		return 0, nil
	}

	return len(toDelete), u.DeleteSections(toDelete)
}

// Commit 提交配置更改
func (u *UCITool) Commit() error {
	cmd := exec.Command("uci", "commit", u.configName)
	cmd.Dir = "/etc/config"
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("uci commit failed: %v, output: %s", err, string(out))
	}
	return nil
}

// GetConfig 获取当前配置（返回原始文本）
func (u *UCITool) GetConfig() (string, error) {
	cmd := exec.Command("uci", "show", u.configName)
	cmd.Dir = "/etc/config"
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("uci show failed: %v, output: %s", err, string(out))
	}
	return string(out), nil
}

// GetOptions 获取指定 section 的所有 option。
// uci show 对匿名段引用（如 @provider[2]）会解析为实际名称（如 cfg07ae50），
// 因此需要从输出的第一行提取解析后的前缀，而不是用原始引用做前缀匹配。
func (u *UCITool) GetOptions(sectionType, sectionName string) (map[string]string, error) {
	sref := u.ref(sectionType, sectionName)
	cmd := exec.Command("uci", "show", sref)
	cmd.Dir = "/etc/config"
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("uci show failed: %v, output: %s", err, string(out))
	}

	result := make(map[string]string)
	// 先从段声明行（第一行）提取实际前缀：model-gateway.cfg07ae50=provider → "model-gateway.cfg07ae50."
	var refPrefix string
	lines := strings.Split(string(out), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		eq := strings.Index(line, "=")
		if eq < 0 {
			continue
		}
		left := strings.TrimSpace(line[:eq])
		// 段声明行：左侧不含 option 名（如 model-gateway.cfg07ae50 或 model-gateway.@provider[0]）
		if !strings.Contains(left[strings.Index(left, ".")+1:], ".") {
			refPrefix = left + "."
			break
		}
	}
	if refPrefix == "" {
		// 兜底：用原始引用前缀
		refPrefix = sref + "."
	}

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasPrefix(line, refPrefix) {
			continue
		}
		rest := strings.TrimPrefix(line, refPrefix)
		kv := strings.SplitN(rest, "=", 2)
		if len(kv) != 2 {
			continue
		}
		key := strings.TrimSpace(kv[0])
		value := strings.Trim(strings.TrimSpace(kv[1]), "'\"")
		if key != "" {
			result[key] = value
		}
	}
	return result, nil
}

// GetLists 获取指定 section 的 list 项
func (u *UCITool) GetLists(sectionType, sectionName, key string) ([]string, error) {
	cmd := exec.Command("uci", "get", fmt.Sprintf("%s.%s", u.ref(sectionType, sectionName), key))
	cmd.Dir = "/etc/config"
	out, err := cmd.CombinedOutput()
	if err != nil {
		// 可能不存在，返回空列表
		if strings.Contains(string(out), "Entry not found") {
			return []string{}, nil
		}
		return nil, fmt.Errorf("uci get failed: %v, output: %s", err, string(out))
	}

	// uci get 对 list 返回单行、以空格分隔多个值
	var result []string
	for _, tok := range strings.Fields(string(out)) {
		tok = strings.Trim(tok, "'\"")
		if tok != "" {
			result = append(result, tok)
		}
	}
	return result, nil
}

// MarshalJSON 实现 json.Marshaler 接口
func (u *UCITool) MarshalJSON() ([]byte, error) {
	// 返回当前配置的 JSON 表示（简化版）
	config, err := u.GetConfig()
	if err != nil {
		return nil, err
	}

	// 简单返回原始文本的 JSON 包装
	return json.Marshal(map[string]string{
		"raw": config,
	})
}

// ToMap 将配置转换为 map
func (u *UCITool) ToMap() (map[string]interface{}, error) {
	config, err := u.GetConfig()
	if err != nil {
		return nil, err
	}

	result := make(map[string]interface{})
	lines := strings.Split(config, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])

		// 去掉前缀
		key = strings.TrimPrefix(key, u.configName+".")

		// 解析嵌套结构
		keys := strings.Split(key, ".")
		current := result
		for i, k := range keys {
			if i == len(keys)-1 {
				// 最后一个键，设置值
				current[k] = value
			} else {
				if _, ok := current[k].(map[string]interface{}); !ok {
					current[k] = make(map[string]interface{})
				}
				current = current[k].(map[string]interface{})
			}
		}
	}
	return result, nil
}

// GetSectionNames 获取指定类型的所有 section 标识（可直接用于 ref）。
// uci show 中段声明行的格式为：configName.<段标识>=<段类型>
// 匿名段的 <段标识> 形如 @provider[0]，命名段则是其名字。
// 返回的标识可直接传给 GetOptions/DeleteSection 等（它们内部用 configName.<标识> 引用）。
// 注意：uci show 对匿名段只输出 @type[index] 格式，不会输出 cfgXXXX，
// 因此这里必须保留 @type[index] 格式，否则匿名段会被跳过导致删除/枚举失效。
func (u *UCITool) GetSectionNames(sectionType string) ([]string, error) {
	cmd := exec.Command("uci", "show", u.configName)
	cmd.Dir = "/etc/config"
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("uci show failed: %v, output: %s", err, string(out))
	}

	var names []string
	pkgPrefix := u.configName + "."
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasPrefix(line, pkgPrefix) {
			continue
		}
		eq := strings.Index(line, "=")
		if eq < 0 {
			continue
		}
		left := line[:eq]                           // configName.<段标识> 或 configName.<段标识>.<option>
		typeVal := strings.Trim(line[eq+1:], "'\"") // 段声明行右侧即段类型
		sec := strings.TrimPrefix(left, pkgPrefix)  // <段标识> 或 <段标识>.<option>
		// 段声明行的左侧不含点（选项行会含点，跳过）
		if strings.Contains(sec, ".") {
			continue
		}
		if typeVal == sectionType {
			names = append(names, sec)
		}
	}
	return names, nil
}

// AddSectionWithOptions 原子地新增一个匿名 section 并设置其 option 与 list。
// 用 uci batch + @type[-1]（指向刚 add 的最后一个 section）一次性完成，
// 避免依赖 "uci add 返回名 → 后续再引用" 这种在多进程/commit 之间容易失效的方式。
func (u *UCITool) AddSectionWithOptions(sectionType string, options map[string]string, lists map[string][]string) error {
	ref := fmt.Sprintf("%s.@%s[-1]", u.configName, sectionType)
	var buf bytes.Buffer
	buf.WriteString(fmt.Sprintf("add %s %s\n", u.configName, sectionType))
	for k, v := range options {
		buf.WriteString(fmt.Sprintf("set %s.%s='%s'\n", ref, k, escapeUCIValue(v)))
	}
	for k, vals := range lists {
		for _, v := range vals {
			buf.WriteString(fmt.Sprintf("add_list %s.%s='%s'\n", ref, k, escapeUCIValue(v)))
		}
	}
	buf.WriteString(fmt.Sprintf("commit %s\n", u.configName))

	cmd := exec.Command("uci", "batch")
	cmd.Dir = "/etc/config"
	cmd.Stdin = strings.NewReader(buf.String())
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("uci batch add failed: %v, output: %s", err, string(out))
	}
	return nil
}

// IsUCIAvailable 检查 uci 命令是否可用
func IsUCIAvailable() bool {
	_, err := exec.LookPath("uci")
	return err == nil
}

// BackupConfig 备份配置
func (u *UCITool) BackupConfig(backupPath string) error {
	config, err := u.GetConfig()
	if err != nil {
		return err
	}
	return os.WriteFile(backupPath, []byte(config), 0644)
}

// RestoreConfig 恢复配置
func (u *UCITool) RestoreConfig(backupPath string) error {
	data, err := os.ReadFile(backupPath)
	if err != nil {
		return err
	}

	// 解析配置并重建
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// 执行 uci set
		cmd := exec.Command("uci", "set", line)
		cmd.Dir = "/etc/config"
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("restore failed on line '%s': %v, output: %s", line, err, string(out))
		}
	}
	return u.Commit()
}

// DiffConfig 比较两个配置的差异（简化版）
func (u *UCITool) DiffConfig(other *UCITool) ([]string, error) {
	config1, err := u.GetConfig()
	if err != nil {
		return nil, err
	}
	config2, err := other.GetConfig()
	if err != nil {
		return nil, err
	}

	if config1 == config2 {
		return []string{"No differences"}, nil
	}

	// 简单行比较
	lines1 := strings.Split(config1, "\n")
	lines2 := strings.Split(config2, "\n")

	var diffs []string
	maxLen := len(lines1)
	if len(lines2) > maxLen {
		maxLen = len(lines2)
	}

	for i := 0; i < maxLen; i++ {
		line1 := ""
		line2 := ""
		if i < len(lines1) {
			line1 = strings.TrimSpace(lines1[i])
		}
		if i < len(lines2) {
			line2 = strings.TrimSpace(lines2[i])
		}
		if line1 != line2 {
			diffs = append(diffs, fmt.Sprintf("Line %d:\n  - %s\n  + %s", i+1, line1, line2))
		}
	}

	return diffs, nil
}

// ValidateConfig 验证配置是否合法
func (u *UCITool) ValidateConfig() error {
	// 检查配置是否可以被解析
	cmd := exec.Command("uci", "show", u.configName)
	cmd.Dir = "/etc/config"
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("config validation failed: %v, output: %s", err, string(out))
	}

	// 基本检查：是否有 settings section
	if !strings.Contains(string(out), "config model-gateway") {
		return fmt.Errorf("missing required section: model-gateway settings")
	}

	return nil
}

// GetOption 获取单个 option 的值
func (u *UCITool) GetOption(sectionType, sectionName, key string) (string, error) {
	cmd := exec.Command("uci", "get", fmt.Sprintf("%s.%s", u.ref(sectionType, sectionName), key))
	cmd.Dir = "/etc/config"
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("uci get failed: %v, output: %s", err, string(out))
	}
	return strings.TrimSpace(string(out)), nil
}

// SetOptionWithCommit 设置 option 并提交
func (u *UCITool) SetOptionWithCommit(sectionType, sectionName, key, value string) error {
	cmd := exec.Command("uci", "set", fmt.Sprintf("%s.%s=%s", u.ref(sectionType, sectionName), key, value))
	cmd.Dir = "/etc/config"
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("uci set failed: %v, output: %s", err, string(out))
	}
	return u.Commit()
}

// DeleteOption 删除 option
func (u *UCITool) DeleteOption(sectionType, sectionName, key string) error {
	cmd := exec.Command("uci", "delete", fmt.Sprintf("%s.%s", u.ref(sectionType, sectionName), key))
	cmd.Dir = "/etc/config"
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("uci delete failed: %v, output: %s", err, string(out))
	}
	return u.Commit()
}

// AddListItem 添加 list 项
func (u *UCITool) AddListItem(sectionType, sectionName, key, value string) error {
	cmd := exec.Command("uci", "add_list", fmt.Sprintf("%s.%s=%s", u.ref(sectionType, sectionName), key, value))
	cmd.Dir = "/etc/config"
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("uci add_list failed: %v, output: %s", err, string(out))
	}
	return u.Commit()
}

// DeleteListItem 删除 list 项
func (u *UCITool) DeleteListItem(sectionType, sectionName, key, value string) error {
	cmd := exec.Command("uci", "del_list", fmt.Sprintf("%s.%s=%s", u.ref(sectionType, sectionName), key, value))
	cmd.Dir = "/etc/config"
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("uci del_list failed: %v, output: %s", err, string(out))
	}
	return u.Commit()
}

// PurgeConfig 清空配置（删除所有 section）
func (u *UCITool) PurgeConfig() error {
	cmd := exec.Command("uci", "delete", u.configName)
	cmd.Dir = "/etc/config"
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("uci delete failed: %v, output: %s", err, string(out))
	}
	return u.Commit()
}

// ExportConfig 导出配置到文件
func (u *UCITool) ExportConfig(filePath string) error {
	config, err := u.GetConfig()
	if err != nil {
		return err
	}
	return os.WriteFile(filePath, []byte(config), 0644)
}

// ImportConfig 从文件导入配置
func (u *UCITool) ImportConfig(filePath string) error {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return err
	}

	// 逐行执行 uci set
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		cmd := exec.Command("uci", "set", line)
		cmd.Dir = "/etc/config"
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("import failed on line '%s': %v, output: %s", line, err, string(out))
		}
	}
	return u.Commit()
}

// ToJSON 将配置转换为 JSON 格式
func (u *UCITool) ToJSON() (string, error) {
	m, err := u.ToMap()
	if err != nil {
		return "", err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// FromJSON 从 JSON 恢复配置
func (u *UCITool) FromJSON(jsonData string) error {
	var data map[string]interface{}
	if err := json.Unmarshal([]byte(jsonData), &data); err != nil {
		return err
	}

	// 递归处理
	var processMap func(m map[string]interface{}, prefix string)
	processMap = func(m map[string]interface{}, prefix string) {
		for k, v := range m {
			fullKey := k
			if prefix != "" {
				fullKey = prefix + "." + k
			}

			switch val := v.(type) {
			case map[string]interface{}:
				// 递归处理嵌套 map
				processMap(val, fullKey)
			case []interface{}:
				// 处理数组（list）
				for _, item := range val {
					if str, ok := item.(string); ok {
						cmd := exec.Command("uci", "add_list", fmt.Sprintf("%s.%s", u.configName, fullKey)+"="+str)
						cmd.Dir = "/etc/config"
						cmd.Run()
					}
				}
			default:
				// 设置值
				cmd := exec.Command("uci", "set", fmt.Sprintf("%s.%s=%v", u.configName, fullKey, v))
				cmd.Dir = "/etc/config"
				cmd.Run()
			}
		}
	}

	processMap(data, "")
	return u.Commit()
}

// ExecCommand 执行任意 uci 命令
func (u *UCITool) ExecCommand(args ...string) (string, error) {
	args = append([]string{"-c", u.configName}, args...)
	cmd := exec.Command("uci", args...)
	cmd.Dir = "/etc/config"
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("uci command failed: %v, output: %s", err, string(out))
	}
	return string(out), nil
}

// BatchSetOptions 批量设置 option
func (u *UCITool) BatchSetOptions(sectionType, sectionName string, options map[string]string) error {
	ref := u.ref(sectionType, sectionName)

	var buf bytes.Buffer
	for k, v := range options {
		buf.WriteString(fmt.Sprintf("set %s.%s='%s'\n", ref, k, escapeUCIValue(v)))
	}
	buf.WriteString(fmt.Sprintf("commit %s\n", u.configName))

	cmd := exec.Command("uci", "batch")
	cmd.Dir = "/etc/config"
	cmd.Stdin = strings.NewReader(buf.String())
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("uci batch failed: %v, output: %s", err, string(out))
	}
	return nil
}

// CloneSection 克隆 section
func (u *UCITool) CloneSection(sectionType, sectionName, newName string) error {
	// 获取原 section 的所有配置
	options, err := u.GetOptions(sectionType, sectionName)
	if err != nil {
		return err
	}

	// 创建新 section
	_, err = u.AddSection(sectionType)
	if err != nil {
		return err
	}
	newSectionName := newName

	// 设置所有 option
	if err := u.BatchSetOptions(sectionType, newSectionName, options); err != nil {
		return err
	}

	return nil
}
