package engine

// classify.go 内容分类路由：对请求正文做轻量启发式分类（无 ML/WASM，纯规则，符合 iStoreOS 铁律）。
// 分类结果映射到 category，由 classify 路由组（members 写成 cat=group 形式）委托到对应路由组。

import (
	"strings"
)

// 分类类别
const (
	CatVision    = "vision"
	CatCode      = "code"
	CatLong      = "long"
	CatReasoning = "reasoning"
	CatGeneral   = "general"
)

// Classify 对请求正文（通常是首条用户消息文本 + 结构）做启发式分类。
// 优先级：vision > long > code > reasoning > general。
func Classify(content string) string {
	c := strings.ToLower(content)

	// 多模态：含图片标记 → vision 优先
	if strings.Contains(c, "image_url") || strings.Contains(c, "data:image") {
		return CatVision
	}

	// 长文本：按 rune 长度（含中文）判断
	if len([]rune(content)) > 4000 {
		return CatLong
	}

	// 代码
	if isCodeLike(c) {
		return CatCode
	}

	// 推理
	if strings.Contains(c, "think") || strings.Contains(c, "推理") ||
		strings.Contains(c, "思考") || strings.Contains(c, "step by step") ||
		strings.Contains(c, "逐步") || strings.Contains(c, "let's reason") {
		return CatReasoning
	}

	return CatGeneral
}

// isCodeLike 简单代码特征识别
func isCodeLike(c string) bool {
	markers := []string{
		"```", "def ", "function ", "func ", "class ", "import ",
		"public ", "private ", "void ", "print(", "console.log",
		"select ", "from ", "update ", "insert ", "delete from ",
		"</", "<html", "<?php", "#include", "package main", "using namespace",
		"var ", "let ", "const ", "return ", "if __name__",
	}
	for _, m := range markers {
		if strings.Contains(c, m) {
			return true
		}
	}
	return false
}
