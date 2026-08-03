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
	dir        string // S6：可配置 uci 工作目录（如 /etc/config），为空则使用默认 PATH
}

// NewUCITool 创建 UCI 工具
func NewUCITool(configName string) *UCITool {
	return &UCITool{configName: configName}
}

// SetDir 设置 uci 配置目录（P3-6: 改用 -c <dir> 参数，不再用 cmd.Dir）
func (u *UCITool) SetDir(dir string) {
	u.dir = dir
}

// execCommand 执行 uci 子命令（P3-6: uci 不读 cwd，用 -c <dir> 指定配置目录）
func (u *UCITool) execCommand(name string, args ...string) *exec.Cmd {
	if u.dir != "" {
		// 在 args 最前面插入 -c <dir>（uci 所有子命令均支持 -c）
		args = append([]string{"-c", u.dir}, args...)
	}
	return exec.Command(name, args...)
}

// checkUCI 检查 uci 命令是否可用（S6：缺失时显式报错，避免静默失败）
func (u *UCITool) checkUCI() error {
	cmd := u.execCommand("uci", "--version")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("uci command not found or not executable: %v, output: %s", err, string(out))
	}
	return nil
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

// escapeUCIValue 转义供 UCI 写入使用的值（P2-1 / P2-3）。
// 1) 单引号 → '\”（UCI 标准转义，供单引号包裹或 uci set 解析）；
// 2) 真实换行/回车 → \n / \r 字面（防止逃逸单引号包裹执行任意 uci 子命令，P2-1）；
// 3) 反斜杠 → \\ ，保证与 unescapeUCIValue 严格互逆（含 `\`、`\n` 等既存字面）。
func escapeUCIValue(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, "'", `'\''`)
	v = strings.ReplaceAll(v, "\n", `\n`)
	v = strings.ReplaceAll(v, "\r", `\r`)
	return v
}

// unescapeUCIValue 还原 uci show 输出中被转义的值（P2-2 修复）。
// uci 对含特殊字符的值用单引号包裹，内部单引号转义为 '\”。
// 例如存储值 it's 在 `uci show` 中输出为 'it'\”s'，
// 旧实现 strings.Trim(..., "'\"") 仅剥掉首尾引号，残留内部 '\” 导致值被错误截断。
// 这里先按引号规则剥壳，再把内部的 '\” 还原为单引号。
// P2-3：与 escapeUCIValue 严格互逆——按序列单遍扫描还原 \\  \n  \r，
// 避免 ReplaceAll 顺序问题导致「字面反斜杠+n」与「真实换行」无法区分。
func unescapeUCIValue(v string) string {
	v = strings.TrimSpace(v)
	inQuote := false
	if len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'' {
		inQuote = true
		v = v[1 : len(v)-1]
	} else if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		v = v[1 : len(v)-1]
	}
	// 单引号包裹下：先还原内部转义引号 '\'' -> '（字面反斜杠已被 escape 写成 \\，不会误伤）
	if inQuote {
		v = strings.ReplaceAll(v, `'\''`, "'")
	}
	// 单遍扫描还原反斜杠转义序列（\\ \n \r），其余原样
	var b strings.Builder
	b.Grow(len(v))
	for i := 0; i < len(v); i++ {
		if v[i] == '\\' && i+1 < len(v) {
			switch v[i+1] {
			case '\\':
				b.WriteByte('\\')
				i++
				continue
			case 'n':
				b.WriteByte('\n')
				i++
				continue
			case 'r':
				b.WriteByte('\r')
				i++
				continue
			}
		}
		b.WriteByte(v[i])
	}
	return b.String()
}

// SetOption 设置单个 option
// 段引用统一走 ref()：命名段 → config.name，匿名/类型 → config.type，
// 严禁拼出 config.type.name 的非法 4 段引用（历史 bug #03 / 铁律 #27）
func (u *UCITool) SetOption(sectionType, sectionName, key, value string) error {
	if err := u.checkUCI(); err != nil {
		return err
	}
	cmd := u.execCommand("uci", "set", fmt.Sprintf("%s.%s=%s", u.ref(sectionType, sectionName), key, escapeUCIValue(value)))
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
	cmd := u.execCommand("uci", "delete", fmt.Sprintf("%s.%s", ref, key))
	cmd.Run() // 忽略删除错误（可能不存在）

	// 添加新值
	for _, v := range values {
		cmd = u.execCommand("uci", "add_list", fmt.Sprintf("%s.%s=%s", ref, key, escapeUCIValue(v)))
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("uci add_list failed: %v, output: %s", err, string(out))
		}
	}
	return u.Commit()
}

// AddSection 添加新 section
func (u *UCITool) AddSection(sectionType string) (string, error) {
	cmd := u.execCommand("uci", "add", u.configName, sectionType)
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

	cmd := u.execCommand("uci", "batch")
	cmd.Stdin = strings.NewReader(buf.String())
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("uci batch delete failed: %v, output: %s", err, string(out))
	}
	return nil
}

// ReplaceSectionsAtomic 原子地全量替换指定类型的所有 section（P2-5）。
// 在单个 uci batch 内完成「先删除旧段 → 逐条 add 新段并设置 option/list → 一次 commit」，
// 彻底避免「先删后建、各自单独 commit」在中途失败时留下的半删半建中间态。
// 每个 items 元素含 options（单值）+ lists（list 字段），均可为空。
// 删除顺序复用 DeleteSections 的（匿名段倒序、命名段最后）策略，防索引漂移。
type ReplaceItem struct {
	Options map[string]string
	Lists   map[string][]string
}

func (u *UCITool) ReplaceSectionsAtomic(sectionType string, items []ReplaceItem) error {
	var buf bytes.Buffer

	// 1) 收集并排序待删除的旧段（匿名段倒序在前，命名段在后）
	oldIDs, err := u.GetSectionNames(sectionType)
	if err != nil {
		return err
	}
	type sectionEntry struct {
		id     string
		isAnon bool
		index  int
	}
	entries := make([]sectionEntry, 0, len(oldIDs))
	for _, id := range oldIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		idx := strings.Index(id, "[")
		if idx >= 0 {
			if end := strings.Index(id[idx:], "]"); end >= 0 {
				if n, perr := strconv.Atoi(id[idx+1 : idx+end]); perr == nil {
					entries = append(entries, sectionEntry{id, true, n})
					continue
				}
			}
		}
		entries = append(entries, sectionEntry{id, false, 0})
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].isAnon != entries[j].isAnon {
			return entries[i].isAnon
		}
		if entries[i].isAnon {
			return entries[i].index > entries[j].index
		}
		return false
	})
	for _, e := range entries {
		buf.WriteString(fmt.Sprintf("delete %s.%s\n", u.configName, e.id))
	}

	// 2) 逐条 add 新段（匿名段用 @type[-1] 指向上一个刚 add 的）
	for _, it := range items {
		buf.WriteString(fmt.Sprintf("add %s %s\n", u.configName, sectionType))
		ref := fmt.Sprintf("%s.@%s[-1]", u.configName, sectionType)
		for k, v := range it.Options {
			buf.WriteString(fmt.Sprintf("set %s.%s='%s'\n", ref, k, escapeUCIValue(v)))
		}
		for k, vals := range it.Lists {
			for _, v := range vals {
				buf.WriteString(fmt.Sprintf("add_list %s.%s='%s'\n", ref, k, escapeUCIValue(v)))
			}
		}
	}

	// 3) 单次 commit
	buf.WriteString(fmt.Sprintf("commit %s\n", u.configName))

	cmd := u.execCommand("uci", "batch")
	cmd.Stdin = strings.NewReader(buf.String())
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("uci batch replace failed: %v, output: %s", err, string(out))
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
	cmd := u.execCommand("uci", "commit", u.configName)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("uci commit failed: %v, output: %s", err, string(out))
	}
	return nil
}

// GetConfig 获取当前配置（返回原始文本）
func (u *UCITool) GetConfig() (string, error) {
	cmd := u.execCommand("uci", "show", u.configName)
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
	cmd := u.execCommand("uci", "show", sref)
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
		value := unescapeUCIValue(kv[1])
		if key != "" {
			result[key] = value
		}
	}
	return result, nil
}

// GetLists 获取指定 section 的 list 项
func (u *UCITool) GetLists(sectionType, sectionName, key string) ([]string, error) {
	cmd := u.execCommand("uci", "get", fmt.Sprintf("%s.%s", u.ref(sectionType, sectionName), key))
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
		tok = unescapeUCIValue(tok)
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
	cmd := u.execCommand("uci", "show", u.configName)
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

	cmd := u.execCommand("uci", "batch")
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
		cmd := u.execCommand("uci", "set", line)
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
	cmd := u.execCommand("uci", "show", u.configName)
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
	cmd := u.execCommand("uci", "get", fmt.Sprintf("%s.%s", u.ref(sectionType, sectionName), key))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("uci get failed: %v, output: %s", err, string(out))
	}
	return strings.TrimSpace(string(out)), nil
}

// SetOptionWithCommit 设置 option 并提交
func (u *UCITool) SetOptionWithCommit(sectionType, sectionName, key, value string) error {
	cmd := u.execCommand("uci", "set", fmt.Sprintf("%s.%s=%s", u.ref(sectionType, sectionName), key, escapeUCIValue(value)))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("uci set failed: %v, output: %s", err, string(out))
	}
	return u.Commit()
}

// DeleteOption 删除 option
func (u *UCITool) DeleteOption(sectionType, sectionName, key string) error {
	cmd := u.execCommand("uci", "delete", fmt.Sprintf("%s.%s", u.ref(sectionType, sectionName), key))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("uci delete failed: %v, output: %s", err, string(out))
	}
	return u.Commit()
}

// AddListItem 添加 list 项
func (u *UCITool) AddListItem(sectionType, sectionName, key, value string) error {
	cmd := u.execCommand("uci", "add_list", fmt.Sprintf("%s.%s=%s", u.ref(sectionType, sectionName), key, escapeUCIValue(value)))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("uci add_list failed: %v, output: %s", err, string(out))
	}
	return u.Commit()
}

// DeleteListItem 删除 list 项
func (u *UCITool) DeleteListItem(sectionType, sectionName, key, value string) error {
	cmd := u.execCommand("uci", "del_list", fmt.Sprintf("%s.%s=%s", u.ref(sectionType, sectionName), key, escapeUCIValue(value)))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("uci del_list failed: %v, output: %s", err, string(out))
	}
	return u.Commit()
}

// PurgeConfig 清空配置（删除所有 section）
func (u *UCITool) PurgeConfig() error {
	cmd := u.execCommand("uci", "delete", u.configName)
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
		cmd := u.execCommand("uci", "set", line)
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
						cmd := u.execCommand("uci", "add_list", fmt.Sprintf("%s.%s", u.configName, fullKey)+"="+escapeUCIValue(str))
						cmd.Run()
					}
				}
			default:
				// 设置值
				cmd := u.execCommand("uci", "set", fmt.Sprintf("%s.%s=%v", u.configName, fullKey, escapeUCIValue(fmt.Sprintf("%v", v))))
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
	cmd := u.execCommand("uci", args...)
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

	cmd := u.execCommand("uci", "batch")
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
