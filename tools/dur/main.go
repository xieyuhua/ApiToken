package main

// 临时补丁：Duration 支持 YAML 序列化回字符串。

import (
	"fmt"
	"os"
	"strings"
)

func main() {
	const file = "internal/config/config.go"
	data, err := os.ReadFile(file)
	if err != nil {
		fmt.Println("read err:", err)
		os.Exit(1)
	}
	s := string(data)
	old := "// D 转换为 time.Duration。\nfunc (d Duration) D() time.Duration { return time.Duration(d) }"
	nw := "// D 转换为 time.Duration。\nfunc (d Duration) D() time.Duration { return time.Duration(d) }\n\n" +
		"// MarshalYAML 序列化为 \"300s\" 这类可读字符串，避免写出纳秒整数。\n" +
		"func (d Duration) MarshalYAML() (any, error) {\n\treturn time.Duration(d).String(), nil\n}"
	if !strings.Contains(s, old) {
		fmt.Println("skip(not found)")
		return
	}
	s = strings.Replace(s, old, nw, 1)
	if err := os.WriteFile(file, []byte(s), 0o644); err != nil {
		fmt.Println("write err:", err)
		os.Exit(1)
	}
	fmt.Println("patched Duration.MarshalYAML")
}
