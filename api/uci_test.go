package api

import "testing"

// TestEscapeUnescapeRoundTrip 验证 escapeUCIValue 与 unescapeUCIValue 严格互逆（P2-3）。
// 覆盖：普通文本、内部单引号、多单引号、真实换行、真实回车、反斜杠、既有字面 \n、混合场景。
func TestEscapeUnescapeRoundTrip(t *testing.T) {
	cases := []string{
		"",
		"hello world",
		"it's a test",
		"a'b'c",
		"line1\nline2",
		"crlf\r\n",
		"backslash \\ path",
		"both \\ and ' newline\n",
		"literal \\n should roundtrip",
		"/mnt/sda1/Configs/model-gateway",
		"中文 + 特殊 ` $ & | < > 符号",
	}
	for _, orig := range cases {
		esc := escapeUCIValue(orig)
		got := unescapeUCIValue("'" + esc + "'")
		if got != orig {
			t.Errorf("roundtrip mismatch: orig=%q esc=%q got=%q", orig, esc, got)
		}
	}
}

// TestEscapeUCIValueNoBreakout 验证 escapeUCIValue 不产生可逃逸 batch 的裸换行（P2-1 回归）。
func TestEscapeUCIValueNoBreakout(t *testing.T) {
	// 换行/回车必须被转义为字面 \n / \r，不能残留裸换行导致 uci batch 注入。
	esc := escapeUCIValue("a\nb")
	if esc != `a\nb` {
		t.Errorf("newline not escaped: %q", esc)
	}
	esc2 := escapeUCIValue("a\r\nb")
	if esc2 != `a\r\nb` {
		t.Errorf("crlf not escaped: %q", esc2)
	}
	// 单引号必须转义
	if escapeUCIValue("it's") != `it'\''s` {
		t.Errorf("quote not escaped: %q", escapeUCIValue("it's"))
	}
}

// TestUnescapeUCIValueExplicit 显式验证若干 uci show 输出形态（P2-2 / P2-3）。
func TestUnescapeUCIValueExplicit(t *testing.T) {
	// 剥壳 + 还原内部单引号
	if got := unescapeUCIValue(`'it'\''s'`); got != "it's" {
		t.Errorf("quote unescape: got %q", got)
	}
	// 还原换行
	if got := unescapeUCIValue(`'a\nb'`); got != "a\nb" {
		t.Errorf("newline unescape: got %q", got)
	}
	// 无引号原样
	if got := unescapeUCIValue("plain"); got != "plain" {
		t.Errorf("plain: got %q", got)
	}
}
