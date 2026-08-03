package engine

import (
	"testing"
	"time"
)

func TestCircuitLockAfterThreshold(t *testing.T) {
	cp := NewCircuitPool(3, 60*time.Second)
	key := "A||m1"
	// 连续达到 lockThreshold（= threshold*3 = 9）次服务端失败应锁定
	for i := 0; i < 9; i++ {
		cp.RecordFailureWithType(key, FailServer)
	}
	if !cp.Locked(key) {
		t.Fatal("expected model to be locked after repeated server failures")
	}
	if cp.State(key) != "locked" {
		t.Fatalf("expected state locked, got %s", cp.State(key))
	}
	// 巡检成功解锁（C4 自愈）
	cp.RecordSuccess(key)
	if cp.Locked(key) {
		t.Fatal("expected unlocked after probe success")
	}
}

func TestCircuitLockNotOnRateLimit(t *testing.T) {
	cp := NewCircuitPool(3, 60*time.Second)
	key := "A||m1"
	// 429 不计入熔断，更不应锁定
	for i := 0; i < 20; i++ {
		cp.RecordFailureWithType(key, FailRate)
	}
	if cp.Locked(key) {
		t.Fatal("429 rate-limit must not trigger lock")
	}
}
