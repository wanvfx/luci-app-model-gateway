// ssrf.go SSRF 出站防护：网关在转发请求到 provider 的 base_url 前，
// 解析目标主机并校验解析出的 IP 是否落入「禁止直连」范围，阻断针对
// 云元数据(169.254.169.254)、回环、链路本地等内网地址的 SSRF 攻击。
//
// 设计取舍（关键）：
//   - 默认模式（strict=false）仅拦截回环/未指定/链路本地/多播，
//     其中链路本地覆盖 169.254.0.0/16（云元数据 169.254.169.254 是 SSRF 头号目标）。
//     这样既能防住最危险的元数据泄露，又放行 RFC1918 私网，
//     兼容用户在局域网自托管 Ollama/OpenWebUI 等场景（base_url=http://192.168.x）。
//   - 严格模式（strict=true）额外拦截 RFC1918 私网 + IPv6 ULA + CGNAT，
//     仅允许公网地址——适合只用公有云 provider、要把内网完全屏蔽的用户。
package netutil

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"
)

// errSSRFBlocked 是 SSRF 拦截时返回的错误（含被拦截的目标地址）。
var errSSRFBlocked = errors.New("ssrf guard: destination address blocked")

// ssrfDefaultCIDRs 默认（非严格）拦截范围：回环/未指定/链路本地/多播。
// 链路本地 169.254.0.0/16 覆盖云元数据服务 169.254.169.254（SSRF 头号目标）。
var ssrfDefaultCIDRs = []string{
	"127.0.0.0/8",    // IPv4 回环
	"::1/128",        // IPv6 回环
	"0.0.0.0/8",      // IPv4 未指定
	"::/128",         // IPv6 未指定
	"169.254.0.0/16", // IPv4 链路本地（含云元数据 169.254.169.254）
	"fe80::/10",      // IPv6 链路本地
	"224.0.0.0/4",    // IPv4 多播
	"ff00::/8",       // IPv6 多播
}

// ssrfStrictCIDRs 严格模式额外拦截：RFC1918 私网 + IPv6 ULA + CGNAT。
var ssrfStrictCIDRs = []string{
	"10.0.0.0/8",     // RFC1918
	"172.16.0.0/12",  // RFC1918
	"192.168.0.0/16", // RFC1918（局域网 Ollama/群晖等）
	"fc00::/7",       // IPv6 唯一本地（ULA）
	"100.64.0.0/10",  // CGNAT 共享地址空间
}

var (
	ssrfDefaultNets []*net.IPNet
	ssrfStrictNets  []*net.IPNet
)

func init() {
	ssrfDefaultNets = parseCIDRs(ssrfDefaultCIDRs)
	ssrfStrictNets = parseCIDRs(ssrfStrictCIDRs)
}

func parseCIDRs(cidrs []string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		if _, ipnet, err := net.ParseCIDR(c); err == nil {
			out = append(out, ipnet)
		}
	}
	return out
}

// IsSSRFBlocked 判断 IP 是否命中 SSRF 拦截范围。
// strict=false：仅默认范围（回环/未指定/链路本地/多播，含云元数据）。
// strict=true：额外拦截 RFC1918 私网 + ULA + CGNAT。
// ip 为 nil 视为拦截（宁可拒绝）。
func IsSSRFBlocked(ip net.IP, strict bool) bool {
	if ip == nil {
		return true
	}
	for _, n := range ssrfDefaultNets {
		if n.Contains(ip) {
			return true
		}
	}
	if strict {
		for _, n := range ssrfStrictNets {
			if n.Contains(ip) {
				return true
			}
		}
	}
	return false
}

// SSRFSafeDialContext 返回带 SSRF 防护的 DialContext。
//   - 域名：解析全部 A/AAAA，挑选首个非拦截 IP 直连（DNS 重绑定安全：连接已解析的特定 IP）。
//   - IP 字面量：直接校验后直连。
//   - 若所有解析 IP 均命中拦截范围，返回 errSSRFBlocked。
//
// strictFn 在每次拨号时读取最新严格模式，支持配置热更新。
func SSRFSafeDialContext(strictFn func() bool) func(ctx context.Context, network, addr string) (net.Conn, error) {
	base := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			// 无端口（理论上不会发生）：放行交给底层，避免误拦
			return base.DialContext(ctx, network, addr)
		}
		strict := strictFn != nil && strictFn()

		// IP 字面量：直接校验
		if ip := net.ParseIP(host); ip != nil {
			if IsSSRFBlocked(ip, strict) {
				return nil, fmt.Errorf("%w: %s", errSSRFBlocked, addr)
			}
			return base.DialContext(ctx, network, addr)
		}

		// 域名：解析全部地址，挑选首个安全 IP 直连（防 DNS 重绑定）
		addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		var lastErr error
		for _, ipa := range addrs {
			ip := ipa.IP
			if IsSSRFBlocked(ip, strict) {
				continue
			}
			conn, derr := base.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if derr == nil {
				return conn, nil
			}
			lastErr = derr
		}
		if lastErr != nil {
			return nil, lastErr
		}
		return nil, fmt.Errorf("%w: %s (all resolved addresses blocked)", errSSRFBlocked, addr)
	}
}
