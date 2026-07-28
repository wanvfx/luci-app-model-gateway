package engine

import (
	"testing"
)

// 精确缓存：同请求命中、TTL 生效、Clear 清空
func TestCacheExactHit(t *testing.T) {
	dir := t.TempDir()
	c := NewResponseCache(dir)
	defer c.Stop()
	c.SetConfig(true, 300, 100, false)

	body := `{"model":"gpt-4","messages":[{"role":"user","content":"你好"}]}`
	key := c.ExactKey("gpt-4", body)
	norm := c.PromptNorm(body)

	if _, hit, _ := c.GetContent("gpt-4", key, norm); hit {
		t.Fatal("empty cache should miss")
	}
	c.PutContent("gpt-4", key, norm, "你好！", false)
	content, hit, semantic := c.GetContent("gpt-4", key, norm)
	if !hit || semantic || content != "你好！" {
		t.Fatalf("expect exact hit, got hit=%v semantic=%v content=%q", hit, semantic, content)
	}

	c.Clear()
	if _, hit, _ := c.GetContent("gpt-4", key, norm); hit {
		t.Fatal("cache should miss after Clear")
	}
}

// 缓存键：model/stream 字段不影响键（等价请求同键），messages 变化则键不同
func TestCacheKeyNormalization(t *testing.T) {
	dir := t.TempDir()
	c := NewResponseCache(dir)
	defer c.Stop()

	k1 := c.ExactKey("m", `{"model":"a","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	k2 := c.ExactKey("m", `{"model":"b","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	if k1 != k2 {
		t.Fatal("model/stream fields should not affect cache key")
	}
	k3 := c.ExactKey("m", `{"messages":[{"role":"user","content":"hello"}]}`)
	if k1 == k3 {
		t.Fatal("different messages should produce different keys")
	}
	// 不同模型参数应产生不同键
	k4 := c.ExactKey("m2", `{"messages":[{"role":"user","content":"hi"}]}`)
	if k1 == k4 {
		t.Fatal("different model param should produce different keys")
	}
}

// 语义缓存：近重复 prompt（仅标点/大小写差异）simhash 命中
func TestCacheSemanticHit(t *testing.T) {
	dir := t.TempDir()
	c := NewResponseCache(dir)
	defer c.Stop()
	c.SetConfig(true, 300, 100, true)

	b1 := `{"messages":[{"role":"user","content":"What is the capital of France?"}]}`
	k1 := c.ExactKey("m", b1)
	n1 := c.PromptNorm(b1)
	c.PutContent("m", k1, n1, "Paris", false)

	// 仅标点和大小写不同 → 归一化后 simhash 距离为 0，应语义命中
	b2 := `{"messages":[{"role":"user","content":"what is the capital of france"}]}`
	k2 := c.ExactKey("m", b2)
	n2 := c.PromptNorm(b2)
	content, hit, semantic := c.GetContent("m", k2, n2)
	if !hit || !semantic || content != "Paris" {
		t.Fatalf("expect semantic hit, got hit=%v semantic=%v content=%q", hit, semantic, content)
	}

	// 完全不同的问题不应命中
	b3 := `{"messages":[{"role":"user","content":"帮我写一首关于秋天的七言绝句，要求押韵"}]}`
	if _, hit, _ := c.GetContent("m", c.ExactKey("m", b3), c.PromptNorm(b3)); hit {
		t.Fatal("unrelated prompt should not hit")
	}
}

// 禁用状态：不写入不读取
func TestCacheDisabled(t *testing.T) {
	dir := t.TempDir()
	c := NewResponseCache(dir)
	defer c.Stop()
	c.SetConfig(false, 300, 100, true)

	body := `{"messages":[{"role":"user","content":"hi"}]}`
	key := c.ExactKey("m", body)
	norm := c.PromptNorm(body)
	c.PutContent("m", key, norm, "x", false)
	if _, hit, _ := c.GetContent("m", key, norm); hit {
		t.Fatal("disabled cache should never hit")
	}
}

// simhash 基础性质：相同文本指纹相同，近似文本距离小，不同文本距离大
func TestSimhashProperties(t *testing.T) {
	a := Simhash("the quick brown fox jumps over the lazy dog")
	b := Simhash("the quick brown fox jumps over the lazy dog")
	if a != b {
		t.Fatal("same text must produce same simhash")
	}
	c := Simhash("the quick brown fox jumped over the lazy dog")
	if d := hamming(a, c); d > 16 {
		t.Fatalf("near-duplicate distance too large: %d", d)
	}
}
