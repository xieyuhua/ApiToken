package admin

import (
	"strings"

	"github.com/demo1/apitoken/internal/model"
	"github.com/demo1/apitoken/internal/proxy"
)

// applyChannelDefaults 归一化渠道配置：不填的字段一律走默认值。
// 鉴权默认 Authorization: Bearer <api_key>，OpenAI 兼容服务都能直接用；
// 非 Bearer 的服务可单独设置 auth_style=header / query 与 auth_header。
func applyChannelDefaults(ch *model.Channel) {
	ch.BaseURL = strings.TrimRight(strings.TrimSpace(ch.BaseURL), "/")
	ch.APIKey = strings.TrimSpace(ch.APIKey)
	ch.ChatPath = strings.TrimSpace(ch.ChatPath)
	if ch.ChatPath == "" {
		ch.ChatPath = proxy.DefaultChatPath
	}
	ch.AuthStyle = strings.ToLower(strings.TrimSpace(ch.AuthStyle))
	switch ch.AuthStyle {
	case "header", "query", "bearer":
	default:
		ch.AuthStyle = "bearer"
	}
	ch.AuthHeader = strings.TrimSpace(ch.AuthHeader)
	ch.AuthPrefix = strings.TrimSpace(ch.AuthPrefix)
	if ch.Weight <= 0 {
		ch.Weight = 1
	}
}
