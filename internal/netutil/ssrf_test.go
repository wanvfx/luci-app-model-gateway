package netutil

import (
	"net"
	"testing"
)

func TestIsSSRFBlocked(t *testing.T) {
	cases := []struct {
		ip     string
		strict bool
		want   bool
	}{
		{"127.0.0.1", false, true},
		{"::1", false, true},
		{"0.0.0.0", false, true},
		{"169.254.169.254", false, true}, // 云元数据（SSRF 头号目标）
		{"169.254.1.1", false, true},
		{"fe80::1", false, true},
		{"224.0.0.1", false, true},
		{"192.168.1.1", false, false}, // 默认放行 RFC1918 私网（兼容局域网自托管）
		{"10.0.0.1", false, false},
		{"172.16.0.1", false, false},
		{"8.8.8.8", false, false},
		{"192.168.1.1", true, true}, // 严格模式额外拦截私网
		{"10.0.0.1", true, true},
		{"172.16.0.1", true, true},
		{"100.64.0.1", true, true}, // CGNAT
		{"8.8.8.8", true, false},   // 公网仍放行
		{"2606:4700::1", true, false},
	}
	for _, c := range cases {
		got := IsSSRFBlocked(net.ParseIP(c.ip), c.strict)
		if got != c.want {
			t.Errorf("IsSSRFBlocked(%s, strict=%v) = %v, want %v", c.ip, c.strict, got, c.want)
		}
	}
}

func TestIsSSRFBlockedNil(t *testing.T) {
	if !IsSSRFBlocked(nil, false) {
		t.Error("nil IP must be treated as blocked")
	}
}
