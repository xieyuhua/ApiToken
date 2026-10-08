package main

// 临时补丁：注册 /settings 页面并给各页导航加「设置」入口。

import (
	"fmt"
	"os"
	"strings"
)

type rule struct{ file, old, nw string }

func main() {
	rules := []rule{
		{"internal/gateway/gateway.go",
			"\tmux.Handle(\"GET /chat\", webui.ChatHandler())",
			"\tmux.Handle(\"GET /chat\", webui.ChatHandler())\n\tmux.Handle(\"GET /settings\", webui.SettingsHandler())"},

		{"internal/webui/index.html",
			"    <a href=\"/chat\">对话</a>\n  </nav>",
			"    <a href=\"/chat\">对话</a>\n    <a href=\"/settings\">设置</a>\n  </nav>"},
		{"internal/webui/logs.html",
			"    <a href=\"/chat\">对话</a>\n  </nav>",
			"    <a href=\"/chat\">对话</a>\n    <a href=\"/settings\">设置</a>\n  </nav>"},
		{"internal/webui/diagnostics.html",
			"    <a href=\"/chat\">对话</a>\n  </nav>",
			"    <a href=\"/chat\">对话</a>\n    <a href=\"/settings\">设置</a>\n  </nav>"},
		{"internal/webui/chat.html",
			"    <a href=\"/chat\" class=\"active\">对话</a>\n  </nav>",
			"    <a href=\"/chat\" class=\"active\">对话</a>\n    <a href=\"/settings\">设置</a>\n  </nav>"},
	}
	for _, r := range rules {
		data, err := os.ReadFile(r.file)
		if err != nil {
			fmt.Println("read err:", r.file, err)
			os.Exit(1)
		}
		s := string(data)
		if !strings.Contains(s, r.old) {
			fmt.Printf("skip %s :: %.40q\n", r.file, r.old)
			continue
		}
		s = strings.Replace(s, r.old, r.nw, 1)
		if err := os.WriteFile(r.file, []byte(s), 0o644); err != nil {
			fmt.Println("write err:", err)
			os.Exit(1)
		}
		fmt.Printf("patched %s\n", r.file)
	}
}
