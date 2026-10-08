package main

// 临时补丁 A：RT() 懒初始化 + 超时兜底 + store 日志容量。

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
		{"internal/config/config.go",
			"	path    string\n\truntime atomic.Pointer[Runtime]",
			"	path    string\n\tonce    sync.Once\n\truntime atomic.Pointer[Runtime]"},
		{"internal/config/config.go",
			"\t\"strings\"\n\t\"sync/atomic\"",
			"\t\"strings\"\n\t\"sync\"\n\t\"sync/atomic\""},
		{"internal/config/config.go",
			"func (c *Config) RT() *Runtime {\n\tif rt := c.runtime.Load(); rt != nil {\n\t\treturn rt\n\t}\n\t// 理论上不会为空：Load 时已初始化\n\treturn &Runtime{}\n}",
			"func (c *Config) RT() *Runtime {\n\tc.once.Do(func() {\n\t\tif c.runtime.Load() == nil {\n\t\t\tc.runtime.Store(c.runtimeFromFields())\n\t\t}\n\t})\n\tif rt := c.runtime.Load(); rt != nil {\n\t\treturn rt\n\t}\n\treturn &Runtime{}\n}"},
		{"internal/config/config.go",
			"// RT 返回当前生效的运行时配置（并发安全）。",
			"// RT 返回当前生效的运行时配置（并发安全）。\n// 若 Config 是直接构造而非 Load 而来，这里按字段懒初始化一份默认值。"},

		{"internal/proxy/upstream.go",
			"\tif cfg.RT().RequestTimeout > 0 {\n\t\treturn cfg.RT().RequestTimeout\n\t}\n\treturn cfg.RT().DefaultTimeout",
			"\tif n := cfg.RT().RequestTimeout; n > 0 {\n\t\treturn n\n\t}\n\tif n := cfg.RT().DefaultTimeout; n > 0 {\n\t\treturn n\n\t}\n\treturn 120 * time.Second"},

		{"internal/store/store.go",
			"logs:     NewLogBuffer(cfg.Logging.KeepLogs),",
			"logs:     NewLogBuffer(cfg.RT().KeepLogs),"},
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
		s = strings.ReplaceAll(s, r.old, r.nw)
		if err := os.WriteFile(r.file, []byte(s), 0o644); err != nil {
			fmt.Println("write err:", err)
			os.Exit(1)
		}
		fmt.Printf("patched %s :: %.45q\n", r.file, r.old)
	}
}
