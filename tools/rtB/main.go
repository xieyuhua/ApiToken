package main

// 临时补丁 B：diagnostics 改读运行时配置。

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
	const file = "internal/admin/diagnostics.go"
	data, err := os.ReadFile(file)
	if err != nil {
		fmt.Println("read err:", err)
		os.Exit(1)
	}
	s := string(data)
	rules := []rule{
		{file, "func (h *Handler) buildChecks() []Check {\n\tcfg := h.cfg",
			"func (h *Handler) buildChecks() []Check {\n\tcfg := h.cfg\n\trt := cfg.RT()"},
		{file, "len(cfg.Security.ClientKeys) == 0", "len(rt.ClientKeys) == 0"},
		{file, "len(cfg.Security.ClientKeys),", "len(rt.ClientKeys),"},
		{file, "cfg.Security.AdminToken == \"\"", "rt.AdminToken == \"\""},
		{file, "cfg.Routing.Strategy", "rt.Strategy"},
		{file, "cfg.Routing.MaxAttempts", "rt.MaxAttempts"},
		{file, "cfg.Routing.RetryStatus", "rt.RetryStatus"},
		{file, "cfg.Logging.KeepLogs", "rt.KeepLogs"},
		{file, "cfg.Logging.AccessLog", "rt.AccessLog"},
		{file, "cfg.Logging.RecordPayload", "rt.RecordPayload"},
		{file, "cfg.Server.StreamTimeout.D() <= 0", "rt.StreamTimeout <= 0"},
		{file, "if p := strings.TrimSpace(cfg.Upstream.ProxyURL); p != \"\"", "if p := strings.TrimSpace(rt.ProxyURL); p != \"\""},
	}
	for _, r := range rules {
		if !strings.Contains(s, r.old) {
			fmt.Printf("skip :: %.45q\n", r.old)
			continue
		}
		s = strings.ReplaceAll(s, r.old, r.nw)
		fmt.Printf("patched :: %.45q\n", r.old)
	}
	if err := os.WriteFile(file, []byte(s), 0o644); err != nil {
		fmt.Println("write err:", err)
		os.Exit(1)
	}
}
