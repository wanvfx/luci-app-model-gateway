package engine

import (
	"net/http"
	"testing"
	"time"

	"github.com/wanvfx/luci-app-model-gateway/config"
)

func mkProv(name string, models ...string) *config.Provider {
	return &config.Provider{Name: name, Models: models, Enabled: true}
}

func TestRouterCooldownDemote(t *testing.T) {
	cd := NewCooldownTracker()
	r := NewRouter(NewScorer())
	r.SetCooldown(cd)
	cd.SetFromHeaders("A-m1", http.Header{"Retry-After": []string{"30"}})
	provs := []*config.Provider{mkProv("A", "m1"), mkProv("B", "m2")}
	r.AddRouterWithStrategy("g", []string{"A-m1", "B-m2"}, "quality")
	out := r.orderMembers("g", []string{"A-m1", "B-m2"}, provs, PickOptions{})
	if out[0] != "B-m2" || out[len(out)-1] != "A-m1" {
		t.Fatalf("cooldown member should be demoted to tail, got %v", out)
	}
}

func TestRouterLockDemoteTail(t *testing.T) {
	cp := NewCircuitPool(3, 60*time.Second)
	r := NewRouter(NewScorer())
	r.SetCircuitPool(cp)
	key := "A||m1"
	for i := 0; i < 9; i++ {
		cp.RecordFailureWithType(key, FailServer)
	}
	provs := []*config.Provider{mkProv("A", "m1"), mkProv("B", "m2")}
	r.AddRouterWithStrategy("g", []string{"A-m1", "B-m2"}, "quality")
	out := r.orderMembers("g", []string{"A-m1", "B-m2"}, provs, PickOptions{})
	if out[len(out)-1] != "A-m1" {
		t.Fatalf("locked member should be at tail, got %v", out)
	}
}

func TestRouterLKGPFloat(t *testing.T) {
	lg := NewLastGoodTracker("")
	r := NewRouter(NewScorer())
	r.SetLKGP(lg)
	lg.Record("g", "B") // 末次成功 provider B
	provs := []*config.Provider{mkProv("A", "m1"), mkProv("B", "m2")}
	r.AddRouterWithStrategy("g", []string{"A-m1", "B-m2"}, "quality")
	out := r.orderMembers("g", []string{"A-m1", "B-m2"}, provs, PickOptions{})
	if out[0] != "B-m2" {
		t.Fatalf("LKGP preferred provider should float up, got %v", out)
	}
}

func TestRouterAffinityFloat(t *testing.T) {
	aff := NewSessionAffinity()
	r := NewRouter(NewScorer())
	r.SetAffinity(aff)
	aff.Bind("sess1", "B")
	provs := []*config.Provider{mkProv("A", "m1"), mkProv("B", "m2")}
	r.AddRouterWithStrategy("g", []string{"A-m1", "B-m2"}, "quality")
	out := r.orderMembers("g", []string{"A-m1", "B-m2"}, provs, PickOptions{PreferredProvider: "B"})
	if out[0] != "B-m2" {
		t.Fatalf("affinity preferred provider should float front, got %v", out)
	}
}

func TestRouterPriorityUntouched(t *testing.T) {
	cd := NewCooldownTracker()
	aff := NewSessionAffinity()
	lg := NewLastGoodTracker("")
	cp := NewCircuitPool(3, 60*time.Second)
	r := NewRouter(NewScorer())
	r.SetCooldown(cd)
	r.SetAffinity(aff)
	r.SetLKGP(lg)
	r.SetCircuitPool(cp)
	cd.SetFromHeaders("A-m1", http.Header{"Retry-After": []string{"30"}})
	aff.Bind("s", "B")
	lg.Record("g", "B")
	provs := []*config.Provider{mkProv("A", "m1"), mkProv("B", "m2")}
	r.AddRouterWithStrategy("g", []string{"A-m1", "B-m2"}, "priority")
	out := r.orderMembers("g", []string{"A-m1", "B-m2"}, provs, PickOptions{PreferredProvider: "B"})
	// priority 策略：弹性降权不干预，保持原始顺序
	if out[0] != "A-m1" {
		t.Fatalf("priority strategy must keep original order, got %v", out)
	}
}
