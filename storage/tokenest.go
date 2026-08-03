package storage

// EstimateTokens 对文本做 CJK 感知的 token 估算：
//   - 中日韩等东亚表意文字（含假名、谚文、CJK 标点）按 1 字 ≈ 1 token；
//   - 其余字符（英文、数字、空格、标点等）按约 4 字符 ≈ 1 token（向上取整）。
//
// 用于上游未返回或返回 0 的 usage 时的兜底计数。
func EstimateTokens(s string) int {
	cjk := 0
	other := 0
	for _, r := range s {
		if isCJK(r) {
			cjk++
		} else {
			other++
		}
	}
	return cjk + (other+3)/4
}

// HybridTokens 混合 token 计数：上游真实值 > 0 时采用真实值，
// 否则用文本估算兜底。仅影响「缺失/为 0」的分支，不改动已有真实值路径。
func HybridTokens(real int, text string) int {
	if real > 0 {
		return real
	}
	return EstimateTokens(text)
}

// isCJK 判断 rune 是否落在东亚表意文字相关范围内。
func isCJK(r rune) bool {
	return (r >= 0x4E00 && r <= 0x9FFF) || // CJK 统一表意文字
		(r >= 0x3400 && r <= 0x4DBF) || // CJK 扩展 A
		(r >= 0x20000 && r <= 0x2A6DF) || // CJK 扩展 B
		(r >= 0x2A700 && r <= 0x2B73F) || // CJK 扩展 C/D
		(r >= 0x3040 && r <= 0x30FF) || // 平假名 + 片假名
		(r >= 0xAC00 && r <= 0xD7AF) || // 谚文音节
		(r >= 0xF900 && r <= 0xFAFF) || // CJK 兼容表意文字
		(r >= 0x3000 && r <= 0x303F) || // CJK 符号和标点
		(r >= 0xFF00 && r <= 0xFFEF) // 全角字符（含全角标点/字母，常伴随 CJK）
}
