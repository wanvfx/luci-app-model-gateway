package engine

import (
	"net/http"
	"testing"
	"time"
)

func TestCooldownRetryAfterSeconds(t *testing.T) {
	cd := NewCooldownTracker()
	h := http.Header{}
	h.Set("Retry-After", "30")
	cd.SetFromHeaders("A-m1", h)
	if !cd.Active("A-m1") {
		t.Fatal("expected active cooldown after Retry-After: 30")
	}
	if until := cd.Until("A-m1"); !until.After(time.Now()) {
		t.Fatal("expected future cooldown until")
	}
}

func TestCooldownRateLimitResetRemaining(t *testing.T) {
	cd := NewCooldownTracker()
	h := http.Header{}
	h.Set("X-RateLimit-Reset", "45")
	cd.SetFromHeaders("A-m1", h)
	if !cd.Active("A-m1") {
		t.Fatal("expected active cooldown after X-RateLimit-Reset")
	}
}

func TestCooldownExpiredNotActive(t *testing.T) {
	cd := NewCooldownTracker()
	h := http.Header{}
	h.Set("Retry-After", "1")
	cd.SetFromHeaders("A-m1", h)
	time.Sleep(1100 * time.Millisecond)
	if cd.Active("A-m1") {
		t.Fatal("expected cooldown to expire")
	}
}

func TestCooldownNoHeaderNoop(t *testing.T) {
	cd := NewCooldownTracker()
	cd.SetFromHeaders("A-m1", http.Header{})
	if cd.Active("A-m1") {
		t.Fatal("expected no cooldown without header")
	}
}
