package main

// 临时补丁：把热字段读取切换为 cfg.RT()。

import (
	"fmt"
	"os"
	"strings"
)

type rule struct {
	file    string
	old, nw string
	all     bool
}

func main() {
	rules := []rule{
		{"internal/gateway/gateway.go",
			"keys := s.cfg.Security.ClientKeys", "keys := s.cfg.RT().ClientKeys", true},
		{"internal/gateway/gateway.go",
			"token := s.cfg.Security.AdminToken", "token := s.cfg.RT().AdminToken", false},

		{"internal/proxy/chat.go", "limit := h.cfg.Routing.MaxAttempts", "limit := h.cfg.RT().MaxAttempts", false},
		{"internal/proxy/chat.go", "uerr.retryable(h.cfg.Routing.RetryStatus)", "uerr.retryable(h.cfg.RT().RetryStatus)", false},
		{"internal/proxy/chat.go", "if h.cfg.Logging.AccessLog {", "if h.cfg.RT().AccessLog {", false},
		{"internal/proxy/chat.go", "maxBody: int64(cfg.Server.MaxBodyMB) << 20,", "maxBody: cfg.RT().MaxBodyBytes,", false},
		{"internal/proxy/chat.go", "h.maxBody = int64(cfg.Server.MaxBodyMB) << 20", "h.maxBody = cfg.RT().MaxBodyBytes", false},
		{"internal/proxy/chat.go", "io.LimitReader(r.Body, h.maxBody)", "io.LimitReader(r.Body, h.currentMaxBody())", false},
		{"internal/proxy/chat.go", "if h.cfg.Routing.ForceStreamUsage {", "if h.cfg.RT().ForceStreamUsage {", false},

		{"internal/proxy/upstream.go", "if cfg.Server.StreamTimeout.D() <= 0 {", "if cfg.RT().StreamTimeout <= 0 {", false},
		{"internal/proxy/upstream.go", "return cfg.Server.StreamTimeout.D()", "return cfg.RT().StreamTimeout", false},
		{"internal/proxy/upstream.go", "return cfg.Server.RequestTimeout.D()", "return cfg.RT().RequestTimeout", false},
		{"internal/proxy/upstream.go", "return cfg.Upstream.DefaultTimeout.D()", "return cfg.RT().DefaultTimeout", false},

		{"internal/proxy/snippet.go", "return h.cfg.Logging.PayloadLimit", "return h.cfg.RT().PayloadLimit", false},
		{"internal/proxy/snippet.go", "return h.cfg.Logging.RecordPayload }", "return h.cfg.RT().RecordPayload }", false},

		{"internal/store/store.go", "strategy := s.cfg.Routing.Strategy", "strategy := s.cfg.RT().Strategy", false},
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
		if r.all {
			s = strings.ReplaceAll(s, r.old, r.nw)
		} else {
			s = strings.Replace(s, r.old, r.nw, 1)
		}
		if err := os.WriteFile(r.file, []byte(s), 0o644); err != nil {
			fmt.Println("write err:", err)
			os.Exit(1)
		}
		fmt.Printf("patched %s :: %.45q\n", r.file, r.old)
	}
}
