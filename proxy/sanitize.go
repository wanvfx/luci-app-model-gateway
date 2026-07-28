package proxy

// sanitize.go PII 正则脱敏：转发上游前对 messages 内容脱敏敏感信息，
// 防止手机号/身份证/邮箱/银行卡等隐私数据泄露给第三方模型服务商。
// 纯正则、纯 Go，参考 nexus 网关的 PII redaction 设计。
// 通过网关设置 pii_sanitize 开关控制（默认关闭）。

import (
	"encoding/json"
	"regexp"
	"strings"
)

var (
	// 中国大陆手机号：1[3-9] 开头 11 位，前后无数字（避免误伤长数字串的一部分）
	rePhone = regexp.MustCompile(`(^|[^0-9])(1[3-9]\d{9})([^0-9]|$)`)
	// 身份证：18 位（17 数字 + 数字/X），前后无数字
	reIDCard = regexp.MustCompile(`(^|[^0-9Xx])(\d{17}[\dXx])([^0-9Xx]|$)`)
	// 邮箱
	reEmail = regexp.MustCompile(`([A-Za-z0-9._%+-]+)@([A-Za-z0-9.-]+\.[A-Za-z]{2,})`)
	// 银行卡：16-19 位连续数字，前后无数字（覆盖主流借记/信用卡）
	reBankCard = regexp.MustCompile(`(^|[^0-9])(\d{16,19})([^0-9]|$)`)
)

// maskMiddle 保留前 keep 位与后 tail 位，中间以 **** 替代
func maskMiddle(s string, keep, tail int) string {
	if len(s) <= keep+tail {
		return "****"
	}
	return s[:keep] + "****" + s[len(s)-tail:]
}

// sanitizeText 对一段文本执行全部脱敏规则
func sanitizeText(text string) string {
	if text == "" {
		return text
	}
	// 手机号：138****8000
	text = rePhone.ReplaceAllStringFunc(text, func(m string) string {
		sub := rePhone.FindStringSubmatch(m)
		return sub[1] + maskMiddle(sub[2], 3, 4) + sub[3]
	})
	// 身份证：保留前 4 后 3
	text = reIDCard.ReplaceAllStringFunc(text, func(m string) string {
		sub := reIDCard.FindStringSubmatch(m)
		return sub[1] + maskMiddle(sub[2], 4, 3) + sub[3]
	})
	// 邮箱：首字符 + ****@域名
	text = reEmail.ReplaceAllStringFunc(text, func(m string) string {
		sub := reEmail.FindStringSubmatch(m)
		local := sub[1]
		if len(local) <= 1 {
			return "*@" + sub[2]
		}
		return local[:1] + "****@" + sub[2]
	})
	// 银行卡：保留前 4 后 4
	text = reBankCard.ReplaceAllStringFunc(text, func(m string) string {
		sub := reBankCard.FindStringSubmatch(m)
		return sub[1] + maskMiddle(sub[2], 4, 4) + sub[3]
	})
	return text
}

// sanitizePIIBody 对请求体中全部 messages 的文本内容脱敏。
// 支持 string content 与多模态数组 content（仅处理 type=text 段，图片不动）。
// 解析失败时原样返回（宁可不脱敏也不破坏请求）。
func sanitizePIIBody(body []byte) []byte {
	// 快速预筛：无数字且无 @ 的请求体不可能命中任何规则，跳过完整解析
	s := string(body)
	if !strings.ContainsAny(s, "0123456789@") {
		return body
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return body
	}
	msgs, ok := parsed["messages"].([]interface{})
	if !ok {
		return body
	}
	changed := false
	for _, mi := range msgs {
		msg, ok := mi.(map[string]interface{})
		if !ok {
			continue
		}
		switch content := msg["content"].(type) {
		case string:
			if out := sanitizeText(content); out != content {
				msg["content"] = out
				changed = true
			}
		case []interface{}:
			for _, pi := range content {
				part, ok := pi.(map[string]interface{})
				if !ok {
					continue
				}
				if t, _ := part["type"].(string); t == "text" {
					if txt, ok := part["text"].(string); ok {
						if out := sanitizeText(txt); out != txt {
							part["text"] = out
							changed = true
						}
					}
				}
			}
		}
	}
	if !changed {
		return body
	}
	out, err := json.Marshal(parsed)
	if err != nil {
		return body
	}
	return out
}
