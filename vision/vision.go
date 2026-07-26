package vision

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/wanvfx/luci-app-model-gateway/engine"
)

// Detector 识图检测器
type Detector struct {
	visionRouterName string
	maxTokens        int
}

// NewDetector 创建识图检测器
func NewDetector(visionRouterName string, maxTokens int) *Detector {
	if maxTokens <= 0 {
		maxTokens = 16384
	}
	return &Detector{
		visionRouterName: visionRouterName,
		maxTokens:        maxTokens,
	}
}

// SetMaxTokens 设置识图模型 max_tokens 上限
func (d *Detector) SetMaxTokens(max int) {
	if max > 0 {
		d.maxTokens = max
	}
}

// GetMaxTokens 获取当前上限
func (d *Detector) GetMaxTokens() int {
	return d.maxTokens
}

// Detect 检测请求是否包含图片（仅检查最后一条 user 消息，与 Python 原版一致）
func (d *Detector) Detect(body []byte) (bool, error) {
	if d.visionRouterName == "" {
		return false, nil
	}

	var wrapper struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &wrapper); err != nil {
		return false, err
	}

	// 从后往前找最后一条 user 消息（与 Python 原版一致：只看最后一轮 user 消息，历史图片不算）
	for i := len(wrapper.Messages) - 1; i >= 0; i-- {
		var msg struct {
			Role    string      `json:"role"`
			Content interface{} `json:"content"`
		}
		if err := json.Unmarshal(wrapper.Messages[i], &msg); err != nil {
			continue
		}
		if msg.Role != "user" {
			continue
		}

		// content 为数组格式（多模态）
		if contentArr, ok := msg.Content.([]interface{}); ok {
			for _, part := range contentArr {
				partMap, ok := part.(map[string]interface{})
				if !ok {
					continue
				}
				if partMap["type"] == "image_url" {
					fmt.Printf("[vision] Detect: found image_url in last user message\n")
					return true, nil
				}
				if text, ok := partMap["text"].(string); ok && strings.Contains(text, "data:image") {
					fmt.Printf("[vision] Detect: found base64 image in last user message text\n")
					return true, nil
				}
			}
		}

		// content 为字符串格式
		if contentStr, ok := msg.Content.(string); ok {
			if strings.Contains(contentStr, "data:image") {
				fmt.Printf("[vision] Detect: found base64 image in last user message string\n")
				return true, nil
			}
		}

		// 最后一条 user 消息没有图，不再往前看（与 Python 原版一致）
		return false, nil
	}

	return false, nil
}

// ResolveModel 解析模型名：如果是视觉请求且 model 不是组名，返回视觉路由组名
func (d *Detector) ResolveModel(model string, router *engine.Router) string {
	if d.visionRouterName == "" {
		return model
	}
	if router.IsRouter(model) {
		return model
	}
	return d.visionRouterName
}

// ClampMaxTokens 限制请求中的 max_tokens（识图保护）
func (d *Detector) ClampMaxTokens(body []byte) []byte {
	if d.maxTokens <= 0 {
		return body
	}

	var req struct {
		MaxTokens *int `json:"max_tokens"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.MaxTokens == nil {
		return body
	}

	if *req.MaxTokens > d.maxTokens {
		clamped := d.maxTokens
		s := string(body)
		oldStr := fmt.Sprintf("\"max_tokens\":%d", *req.MaxTokens)
		newStr := fmt.Sprintf("\"max_tokens\":%d", clamped)
		s = strings.Replace(s, oldStr, newStr, 1)
		oldStr2 := fmt.Sprintf("\"max_tokens\": %d", *req.MaxTokens)
		newStr2 := fmt.Sprintf("\"max_tokens\": %d", clamped)
		s = strings.Replace(s, oldStr2, newStr2, 1)
		return []byte(s)
	}

	return body
}

// CNHint 识图模型中文兜底提示
const CNHint = "\n\n【重要】请始终使用简体中文回答用户。思考过程(reasoning)也请用中文。代码、命令、文件名、专有名词、标识符等保持原样即可，不要翻译。"

// InjectCNHint 识图兜底：给最后一条 user 消息追加中文回复提示（幂等）
func (d *Detector) InjectCNHint(body []byte) []byte {
	if d.visionRouterName == "" {
		return body
	}

	var req struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil || len(req.Messages) == 0 {
		return body
	}

	// 从后往前找最后一条 user 消息（与 Python 原版一致）
	targetIdx := -1
	for i := len(req.Messages) - 1; i >= 0; i-- {
		var msg map[string]interface{}
		if err := json.Unmarshal(req.Messages[i], &msg); err != nil {
			continue
		}
		role, _ := msg["role"].(string)
		if role == "user" {
			targetIdx = i
			break
		}
	}

	if targetIdx == -1 {
		return body
	}

	var msg map[string]interface{}
	if err := json.Unmarshal(req.Messages[targetIdx], &msg); err != nil {
		return body
	}

	// content 为字符串
	if content, ok := msg["content"].(string); ok {
		if strings.Contains(content, "请始终使用简体中文") {
			return body
		}
		msg["content"] = content + CNHint
		newMsg, _ := json.Marshal(msg)
		req.Messages[targetIdx] = newMsg
		out, _ := json.Marshal(req)
		return out
	}

	// content 为数组（多模态识图）
	if contentArr, ok := msg["content"].([]interface{}); ok {
		modified := false
		for _, item := range contentArr {
			part, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			if part["type"] != "text" {
				continue
			}
			text, ok := part["text"].(string)
			if !ok {
				continue
			}
			if strings.Contains(text, "请始终使用简体中文") {
				return body
			}
			part["text"] = text + CNHint
			modified = true
			break
		}
		if modified {
			newMsg, _ := json.Marshal(msg)
			req.Messages[targetIdx] = newMsg
			out, _ := json.Marshal(req)
			return out
		}
	}

	return body
}
