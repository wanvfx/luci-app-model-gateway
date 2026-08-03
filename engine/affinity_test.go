package engine

import (
	"testing"
	"time"
)

func TestAffinityBindLookup(t *testing.T) {
	a := NewSessionAffinity()
	a.Bind("sess1", "openai")
	p, ok := a.Lookup("sess1")
	if !ok || p != "openai" {
		t.Fatalf("expected openai, got %s ok=%v", p, ok)
	}
}

func TestAffinityTTLExpiry(t *testing.T) {
	a := NewSessionAffinity()
	a.Bind("sess1", "openai")
	// 模拟过期：直接写入一个旧时间戳
	a.mu.Lock()
	a.binds["sess1"] = affinityEntry{provider: "openai", ts: time.Now().Add(-(affinityTTL + time.Minute))}
	a.mu.Unlock()
	if _, ok := a.Lookup("sess1"); ok {
		t.Fatal("expected expired binding to be ignored")
	}
}

func TestAffinityCapEviction(t *testing.T) {
	a := NewSessionAffinity()
	// 写入超过上限的绑定（时间戳递减，最旧的先被淘汰）
	for i := 0; i < affinityCap+50; i++ {
		a.Bind(string(rune('a'+i%26))+string(rune('0'+i%10)), "p")
	}
	a.mu.Lock()
	n := len(a.binds)
	a.mu.Unlock()
	if n > affinityCap {
		t.Fatalf("expected binds capped at %d, got %d", affinityCap, n)
	}
}
