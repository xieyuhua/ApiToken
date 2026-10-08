package main

// 临时补丁：1) 设置支持 ui.theme；2) 密钥支持明文查看（reveal=1）。

import (
	"fmt"
	"os"
	"strings"
)

type rule struct {
	file    string
	old, nw string
}

func main() {
	rules := []rule{
		// config: Runtime 增加主题字段
		{"internal/config/config.go",
			"\t// 出网\n\tProxyURL string\n}",
			"\t// 出网\n\tProxyURL string\n\n\t// 界面\n\tTheme string // auto | dark | light\n}"},

		{"internal/config/config.go",
			"\t\tProxyURL:         c.Upstream.ProxyURL,\n\t}",
			"\t\tProxyURL:         c.Upstream.ProxyURL,\n\t\tTheme:            c.UI.Theme,\n\t}"},

		{"internal/config/config.go",
			"// Config 网关总配置。\ntype Config struct {\n\tServer    ServerConfig   `yaml:\"server\"`",
			"// UIConfig 界面外观配置。\ntype UIConfig struct {\n\tTheme string `yaml:\"theme\"`\n}\n\n// Config 网关总配置。\ntype Config struct {\n\tServer    ServerConfig   `yaml:\"server\"`\n\tUI        UIConfig       `yaml:\"ui\"`"},

		{"internal/config/config.go",
			"	if c.Upstream.MaxIdleConns <= 0 {\n\t\tc.Upstream.MaxIdleConns = 200\n\t}",
			"	if c.Upstream.MaxIdleConns <= 0 {\n\t\tc.Upstream.MaxIdleConns = 200\n\t}\n\tswitch c.UI.Theme {\n\tcase \"dark\", \"light\", \"auto\":\n\tdefault:\n\t\tc.UI.Theme = \"auto\"\n\t}"},

		// saveView 增加 ui 段
		{"internal/config/config.go",
			"type saveView struct {\n\tServer   ServerConfig   `yaml:\"server\"`",
			"type saveView struct {\n\tServer   ServerConfig   `yaml:\"server\"`\n\tUI       UIConfig       `yaml:\"ui\"`"},

		{"internal/config/config.go",
			"\tview := saveView{\n\t\tServer:   c.Server,",
			"\tview := saveView{\n\t\tServer:   c.Server,\n\t\tUI:       UIConfig{Theme: rt.Theme},"},

		// settings: 主题字段 + reveal
		{"internal/admin/settings.go",
			"\t// 出网\n\tProxyURL string `json:\"proxy_url\"`",
			"\t// 出网\n\tProxyURL string `json:\"proxy_url\"`\n\n\t// 界面主题：auto | dark | light\n\tTheme string `json:\"theme\"`"},

		{"internal/admin/settings.go",
			"\t\tStreamTimeout:  int(rt.StreamTimeout / time.Second),\n\t\tProxyURL:       rt.ProxyURL,",
			"\t\tStreamTimeout:  int(rt.StreamTimeout / time.Second),\n\t\tProxyURL:       rt.ProxyURL,\n\t\tTheme:          rt.Theme,"},

		{"internal/admin/settings.go",
			"\tnext.ProxyURL = strings.TrimSpace(in.ProxyURL)",
			"\tnext.ProxyURL = strings.TrimSpace(in.ProxyURL)\n\n\t// 界面主题\n\tswitch in.Theme {\n\tcase \"\":\n\tcase \"auto\", \"dark\", \"light\":\n\t\tnext.Theme = in.Theme\n\tdefault:\n\t\twriteErr(w, http.StatusBadRequest, \"主题仅支持 auto / dark / light\")\n\t\treturn\n\t}"},

		{"internal/admin/settings.go",
			"// getSettings GET /admin/settings\nfunc (h *Handler) getSettings(w http.ResponseWriter, _ *http.Request) {",
			"// getSettings GET /admin/settings（?reveal=1 返回密钥明文）\nfunc (h *Handler) getSettings(w http.ResponseWriter, r *http.Request) {"},

		{"internal/admin/settings.go",
			"\tmasked.AdminToken = model.MaskSecret(v.AdminToken)",
			"\tmasked.AdminToken = model.MaskSecret(v.AdminToken)\n\trevealed := r.URL.Query().Get(\"reveal\") == \"1\"\n\tif revealed {\n\t\t// 已通过管理令牌鉴权，允许查看明文（前端需二次确认）\n\t\tmasked.ClientKeys = v.ClientKeys\n\t\tmasked.AdminToken = v.AdminToken\n\t}"},

		{"internal/admin/settings.go",
			"\t\t\"admin_token_set\": rt.AdminToken != \"\",",
			"\t\t\"admin_token_set\": rt.AdminToken != \"\",\n\t\t\"revealed\":         revealed,"},
	}
	for _, r := range rules {
		data, err := os.ReadFile(r.file)
		if err != nil {
			fmt.Println("read err:", r.file, err)
			os.Exit(1)
		}
		s := string(data)
		if !strings.Contains(s, r.old) {
			fmt.Printf("skip %s :: %.45q\n", r.file, r.old)
			continue
		}
		s = strings.Replace(s, r.old, r.nw, 1)
		if err := os.WriteFile(r.file, []byte(s), 0o644); err != nil {
			fmt.Println("write err:", err)
			os.Exit(1)
		}
		fmt.Printf("patched %s :: %.45q\n", r.file, r.old)
	}
}
