package main

// 临时补丁：注册设置路由 + store 热更新回调 + 移除未用函数。

import (
	"fmt"
	"os"
	"strings"
)

type rule struct{ file, old, nw string }

func main() {
	rules := []rule{
		{"internal/admin/handler.go",
			"\tmux.HandleFunc(\"GET /admin/config\", h.getConfig)",
			"\tmux.HandleFunc(\"GET /admin/config\", h.getConfig)\n\tmux.HandleFunc(\"GET /admin/settings\", h.getSettings)\n\tmux.HandleFunc(\"POST /admin/settings\", h.updateSettings)"},

		{"internal/admin/settings.go",
			"\nfunc parseSeconds(s string, def int) int {\n\tif s == \"\" {\n\t\treturn def\n\t}\n\tn, err := strconv.Atoi(s)\n\tif err != nil {\n\t\treturn def\n\t}\n\treturn n\n}\n",
			""},
		{"internal/admin/settings.go", "\t\"strconv\"\n", ""},

		{"internal/store/store.go",
			"// AddLog 追加访问日志。\nfunc (s *Store) AddLog(e model.LogEntry) { s.logs.Add(e) }",
			"// OnRuntimeChanged 配置热更新回调（目前用于调整日志容量）。\nfunc (s *Store) OnRuntimeChanged(rt *config.Runtime) {\n\ts.logs.SetMax(rt.KeepLogs)\n}\n\n// AddLog 追加访问日志。\nfunc (s *Store) AddLog(e model.LogEntry) { s.logs.Add(e) }"},

		{"internal/store/logbuffer.go",
			"// Count 当前保留条数。",
			"// SetMax 动态调整保留条数（保留最近的 max 条）。\nfunc (b *LogBuffer) SetMax(max int) {\n\tif max <= 0 {\n\t\treturn\n\t}\n\tb.mu.Lock()\n\tdefer b.mu.Unlock()\n\tb.max = max\n\tif len(b.buf) > max {\n\t\tb.buf = append([]model.LogEntry(nil), b.buf[len(b.buf)-max:]...)\n\t}\n}\n\n// Count 当前保留条数."},
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
