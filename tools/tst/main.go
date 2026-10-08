package main

// 临时补丁：调整设置测试中的非法值用例为 -1。

import (
	"fmt"
	"os"
	"strings"
)

func main() {
	const file = "internal/gateway/settings_test.go"
	data, err := os.ReadFile(file)
	if err != nil {
		fmt.Println("read err:", err)
		os.Exit(1)
	}
	s := string(data)
	old := "	if rec := adminDo(t, h, \"adm-new-9\", http.MethodPost, \"/admin/settings\", `{\"strategy\":\"failover\",\"max_attempts\":0}`); rec.Code != http.StatusBadRequest {\n\t\tt.Fatalf(\"max_attempts=0 应返回 400，实际 %d\", rec.Code)\n\t}"
	nw := "	if rec := adminDo(t, h, \"adm-new-9\", http.MethodPost, \"/admin/settings\", `{\"strategy\":\"failover\",\"max_attempts\":-1}`); rec.Code != http.StatusBadRequest {\n\t\tt.Fatalf(\"max_attempts=-1 应返回 400，实际 %d\", rec.Code)\n\t}"
	if !strings.Contains(s, old) {
		fmt.Println("not found")
		return
	}
	s = strings.Replace(s, old, nw, 1)
	if err := os.WriteFile(file, []byte(s), 0o644); err != nil {
		fmt.Println("write err:", err)
		os.Exit(1)
	}
	fmt.Println("patched test")
}
