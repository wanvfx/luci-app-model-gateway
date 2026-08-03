package storage

import "testing"

func TestEstimateTokens(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
	}{
		{"empty", "", 0},
		{"pure ascii 4 chars", "abcd", 1},
		{"pure ascii 8 chars", "abcdefgh", 2},
		{"ascii 3 chars", "abc", 1}, // 向上取整
		{"single cjk", "你", 1},
		{"cjk 5 chars", "你好世界啊", 5},
		{"mixed cjk+ascii", "你好abc", 3},    // 2 CJK + ceil(3/4)=1
		{"mixed longer", "今日天气不错, go!", 8}, // 6 CJK + ", go!"(5 非CJK)=> 6 + ceil(5/4)=2 => 8
		{"kana", "こんにちは", 5},
		{"hangul", "안녕하세요", 5},
		{"cjk punctuation", "，。", 2},
	}
	for _, c := range cases {
		got := EstimateTokens(c.in)
		if got != c.want {
			t.Errorf("%s: EstimateTokens(%q) = %d, want %d", c.name, c.in, got, c.want)
		}
	}
}

func TestHybridTokens(t *testing.T) {
	// 真实值优先
	if got := HybridTokens(10, "任意文本"); got != 10 {
		t.Errorf("HybridTokens real path = %d, want 10", got)
	}
	// 真实值为 0 时走估算兜底
	if got := HybridTokens(0, "你好abc"); got != 3 {
		t.Errorf("HybridTokens fallback = %d, want 3", got)
	}
}

func TestIsCJK(t *testing.T) {
	if isCJK('A') || isCJK(' ') {
		t.Error("ascii should not be CJK")
	}
	if !isCJK('中') || !isCJK('あ') || !isCJK('한') || !isCJK('，') {
		t.Error("CJK ranges misclassified")
	}
}
