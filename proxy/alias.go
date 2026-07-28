package proxy

import (
	"strings"

	"github.com/wanvfx/luci-app-model-gateway/config"
)

// resolveAlias 解析友好名 -> 内部目标（模型前缀名 / 路由组名 / auto）。
// 优先级：UCI alias 配置 > MetaStore 静态别名(meta.json)。支持递归解析（别名指向别名）。
// 用于实现 G8「别名屏蔽底层路由细节」：用户对外只暴露友好名，网关内部透明重写。
func (s *Server) resolveAlias(model string) string {
	aliasMap := s.cfg.Load().AliasMap()
	seen := map[string]bool{}
	resolved := model
	for i := 0; i < 8; i++ { // 防止别名环
		target, ok := aliasMap[resolved]
		if !ok {
			break
		}
		if seen[resolved] {
			break
		}
		seen[resolved] = true
		resolved = target
	}
	// 回退静态别名（models_meta.json）
	if resolved == model && s.meta != nil {
		if m := s.meta.ResolveModel(model); m != model {
			resolved = m
		}
	}
	return resolved
}

// compileAliasList 将配置别名整理为前端可用结构（去空保护）
func compileAliasList(aliases []*config.Alias) []config.Alias {
	out := make([]config.Alias, 0, len(aliases))
	for _, a := range aliases {
		if a == nil || strings.TrimSpace(a.Name) == "" || strings.TrimSpace(a.Target) == "" {
			continue
		}
		out = append(out, config.Alias{Name: strings.TrimSpace(a.Name), Target: strings.TrimSpace(a.Target)})
	}
	return out
}
