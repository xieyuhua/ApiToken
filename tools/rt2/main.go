package main

// 临时补丁：补 currentMaxBody 方法并修正 Handler 结构。

import (
	"fmt"
	"os"
	"strings"
)

func main() {
	const file = "internal/proxy/chat.go"
	data, err := os.ReadFile(file)
	if err != nil {
		fmt.Println("read err:", err)
		os.Exit(1)
	}
	s := string(data)

	rules := []struct{ old, nw string }{
		{
			"\tclient  *Client\n\tmaxBody int64\n}\n\n// NewHandler 构造代理处理器。",
			"\tclient *Client\n}\n\n// currentMaxBody 当前允许的请求体上限（支持运行时调整）。\nfunc (h *Handler) currentMaxBody() int64 {\n\tif n := h.cfg.RT().MaxBodyBytes; n > 0 {\n\t\treturn n\n\t}\n\treturn 32 << 20\n}\n\n// NewHandler 构造代理处理器。",
		},
		{
			"\t\tclient:  NewClient(cfg),\n\t\tmaxBody: cfg.RT().MaxBodyBytes,\n\t}",
			"\t\tclient: NewClient(cfg),\n\t}",
		},
	}
	for _, r := range rules {
		if !strings.Contains(s, r.old) {
			fmt.Printf("skip :: %.45q\n", r.old)
			continue
		}
		s = strings.Replace(s, r.old, r.nw, 1)
		fmt.Printf("patched :: %.45q\n", r.old)
	}
	if err := os.WriteFile(file, []byte(s), 0o644); err != nil {
		fmt.Println("write err:", err)
		os.Exit(1)
	}
}
