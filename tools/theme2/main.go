package main

// 临时补丁：补 UIConfig 定义与 Config.UI 字段、settings 主题赋值。

import (
	"fmt"
	"os"
	"strings"
)

func main() {
	// 1) config.go：UIConfig 类型 + Config.UI 字段 + viewFromRuntime 主题
	f1 := "internal/config/config.go"
	d1, err := os.ReadFile(f1)
	if err != nil {
		fmt.Println("read err:", err)
		os.Exit(1)
	}
	s1 := string(d1)
	type rule struct{ old, nw string }
	r1 := []rule{
		{"// Config 网关总配置。\ntype Config struct {",
			"// UIConfig 界面外观配置。\ntype UIConfig struct {\n\tTheme string `yaml:\"theme\"`\n}\n\n// Config 网关总配置。\ntype Config struct {\n\tUI        UIConfig       `yaml:\"ui\"`"},
	}
	for _, r := range r1 {
		if !strings.Contains(s1, r.old) {
			fmt.Printf("skip cfg :: %.45q\n", r.old)
			continue
		}
		s1 = strings.Replace(s1, r.old, r.nw, 1)
		fmt.Printf("patched cfg :: %.45q\n", r.old)
	}
	if err := os.WriteFile(f1, []byte(s1), 0o644); err != nil {
		fmt.Println("write err:", err)
		os.Exit(1)
	}

	// 2) settings.go：viewFromRuntime 增加 Theme
	f2 := "internal/admin/settings.go"
	d2, err := os.ReadFile(f2)
	if err != nil {
		fmt.Println("read err:", err)
		os.Exit(1)
	}
	s2 := string(d2)
	r2 := []rule{
		{"\t\tProxyURL:       rt.ProxyURL,\n\t}", "\t\tProxyURL:       rt.ProxyURL,\n\t\tTheme:          rt.Theme,\n\t}"},
	}
	for _, r := range r2 {
		if !strings.Contains(s2, r.old) {
			fmt.Printf("skip settings :: %.45q\n", r.old)
			continue
		}
		s2 = strings.Replace(s2, r.old, r.nw, 1)
		fmt.Printf("patched settings :: %.45q\n", r.old)
	}
	if err := os.WriteFile(f2, []byte(s2), 0o644); err != nil {
		fmt.Println("write err:", err)
		os.Exit(1)
	}
}
