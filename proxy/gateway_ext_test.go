package proxy

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wanvfx/luci-app-model-gateway/config"
)

// 并发护栏：全局上限真实生效、超时拿不到、释放后可再获取
func TestConcurrencyGuardGlobal(t *testing.T) {
	g := NewConcurrencyGuard()

	rel1, ok := g.TryAcquire("p", 2, 0, 10*time.Millisecond)
	if !ok {
		t.Fatal("first acquire should succeed")
	}
	rel2, ok := g.TryAcquire("p", 2, 0, 10*time.Millisecond)
	if !ok {
		t.Fatal("second acquire should succeed")
	}
	// 第三个超限
	if _, ok := g.TryAcquire("p", 2, 0, 10*time.Millisecond); ok {
		t.Fatal("third acquire should fail (global limit 2)")
	}
	rel1()
	if rel3, ok := g.TryAcquire("p", 2, 0, 10*time.Millisecond); !ok {
		t.Fatal("acquire after release should succeed")
	} else {
		rel3()
	}
	rel2()
}

// 并发护栏：provider 上限独立生效；provider 失败时回滚全局槽位
func TestConcurrencyGuardProvider(t *testing.T) {
	g := NewConcurrencyGuard()

	rel1, ok := g.TryAcquire("a", 0, 1, 10*time.Millisecond)
	if !ok {
		t.Fatal("provider a first acquire should succeed")
	}
	if _, ok := g.TryAcquire("a", 0, 1, 10*time.Millisecond); ok {
		t.Fatal("provider a second acquire should fail")
	}
	// 另一个 provider 不受影响
	if relB, ok := g.TryAcquire("b", 0, 1, 10*time.Millisecond); !ok {
		t.Fatal("provider b should have its own slots")
	} else {
		relB()
	}
	rel1()

	// provider 失败必须回滚全局槽位：全局=1，provider a 占满后再请求 a，
	// 失败后全局槽位应可被 provider b 使用
	g2 := NewConcurrencyGuard()
	relA, _ := g2.TryAcquire("a", 0, 1, 10*time.Millisecond) // 只占 provider a
	if _, ok := g2.TryAcquire("a", 1, 1, 10*time.Millisecond); ok {
		t.Fatal("provider a should be full")
	}
	// 全局槽位应已回滚，b 可获取
	if relB, ok := g2.TryAcquire("b", 1, 1, 10*time.Millisecond); !ok {
		t.Fatal("global slot should be rolled back after provider failure")
	} else {
		relB()
	}
	relA()
}

// 并发护栏：不限流路径零开销
func TestConcurrencyGuardUnlimited(t *testing.T) {
	g := NewConcurrencyGuard()
	for i := 0; i < 100; i++ {
		rel, ok := g.TryAcquire("p", 0, 0, time.Millisecond)
		if !ok {
			t.Fatal("unlimited should always succeed")
		}
		rel()
	}
}

// 钩子 SSRF 护栏：https 任意、http 仅本机、其他拒绝
func TestHookURLAllowed(t *testing.T) {
	cases := []struct {
		url  string
		want bool
	}{
		{"https://example.com/hook", true},
		{"https://192.168.1.5/hook", true},
		{"http://127.0.0.1:8080/hook", true},
		{"http://localhost/hook", true},
		{"http://[::1]/hook", true},
		{"http://192.168.1.1/admin", false}, // 内网探测拒绝
		{"http://example.com/hook", false},
		{"ftp://example.com", false},
		{"file:///etc/passwd", false},
		{"", false},
		{"://bad", false},
	}
	for _, c := range cases {
		if got := hookURLAllowed(c.url); got != c.want {
			t.Errorf("hookURLAllowed(%q) = %v, want %v", c.url, got, c.want)
		}
	}
}

// 别名列表整理：去空、去自指
func TestCompileAliasList(t *testing.T) {
	in := []*config.Alias{
		{Name: "fast", Target: "auto"},
		{Name: "", Target: "x"},
		{Name: "y", Target: ""},
		nil,
		{Name: " gpt ", Target: " provider-model "},
	}
	out := compileAliasList(in)
	if len(out) != 2 {
		t.Fatalf("expect 2 valid aliases, got %d", len(out))
	}
	if out[1].Name != "gpt" || out[1].Target != "provider-model" {
		t.Fatalf("expect trimmed alias, got %+v", out[1])
	}
}

// 流式缓存回放辅助：合成的 SSE 不应丢内容（通过 chunk 拼接还原）
func TestReplayCachedStreamChunking(t *testing.T) {
	// 构造 >512 rune 的中文内容验证 rune 边界切分
	content := ""
	for i := 0; i < 600; i++ {
		content += "测"
	}
	rec := httptest.NewRecorder()
	replayCachedStream(rec, "m", content)
	body := rec.Body.String()
	if !strings.Contains(body, "data: [DONE]") || !strings.Contains(body, `"finish_reason":"stop"`) {
		t.Fatal("replayed stream missing DONE / finish_reason")
	}
	// 内容不应出现乱码替换符（rune 边界切分保证）
	if strings.Contains(body, "\uFFFD") {
		t.Fatal("multi-byte rune torn apart in chunking")
	}
	// 600 个「测」应全部回放（分两个 chunk）
	if strings.Count(body, "测") != 600 {
		t.Fatalf("expect 600 runes replayed, got %d", strings.Count(body, "测"))
	}
}
