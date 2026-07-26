// Package netutil 提供本地/内网地址判定与代理绕过工具。
// 全项目唯一真相源：proxy 包与 api 包均复用此处实现，
// 避免各自复制粘贴后逻辑跑偏（历史 drift：api 版曾漏 IsLinkLocalMulticast）。
package netutil

import (
	"net"
	"net/http"
	"net/url"
	"strings"
)

// IsPrivateOrLocalHost 判断主机名是否为本地/内网/回环地址。
// 传入可带端口（自动去端口）；识别 localhost、回环、私网、链路本地
// (单播+组播) 以及 .local/.lan/.internal 结尾的内网域名。
func IsPrivateOrLocalHost(host string) bool {
	// 去掉端口
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	// 回环地址
	if host == "localhost" || host == "127.0.0.1" || host == "::1" || host == "[::1]" {
		return true
	}
	// 解析为 IP 判断
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast()
	}
	// 主机名以常见内网域名结尾
	lower := strings.ToLower(host)
	if strings.HasSuffix(lower, ".local") || strings.HasSuffix(lower, ".lan") || strings.HasSuffix(lower, ".internal") {
		return true
	}
	return false
}

// BypassProxyFunc 是 http.Transport.Proxy 回调：
// 对本地/内网地址返回 nil（直连、不走代理），其余走环境变量代理设置。
// 避免系统设置 HTTP_PROXY/HTTPS_PROXY 时无法访问局域网地址导致 502。
func BypassProxyFunc(req *http.Request) (*url.URL, error) {
	if IsPrivateOrLocalHost(req.URL.Hostname()) {
		return nil, nil
	}
	return http.ProxyFromEnvironment(req)
}
