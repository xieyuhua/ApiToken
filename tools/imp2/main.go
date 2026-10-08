package main

// 临时补丁：theme_test.go 补 os import。

import (
	"fmt"
	"os"
	"strings"
)

func main() {
	const file = "internal/gateway/theme_test.go"
	data, err := os.ReadFile(file)
	if err != nil {
		fmt.Println("read err:", err)
		os.Exit(1)
	}
	s := string(data)
	old := "\t\"net/http/httptest\"\n\t\"strings\""
	nw := "\t\"net/http/httptest\"\n\t\"os\"\n\t\"strings\""
	if !strings.Contains(s, old) {
		fmt.Println("not found")
		return
	}
	s = strings.Replace(s, old, nw, 1)
	if err := os.WriteFile(file, []byte(s), 0o644); err != nil {
		fmt.Println("write err:", err)
		os.Exit(1)
	}
	fmt.Println("patched import")
}
