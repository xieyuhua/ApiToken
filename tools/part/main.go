package main

// 临时补丁：部分更新时缺省字段不修改。

import (
	"fmt"
	"os"
	"strings"
)

func main() {
	const file = "internal/admin/settings.go"
	data, err := os.ReadFile(file)
	if err != nil {
		fmt.Println("read err:", err)
		os.Exit(1)
	}
	s := string(data)

	rules := []struct{ old, nw string }{
		{
			"	// 路由\n\tswitch in.Strategy {\n\tcase \"priority_round_robin\", \"round_robin\", \"failover\", \"random\":\n\t\tnext.Strategy = in.Strategy\n\tdefault:\n\t\twriteErr(w, http.StatusBadRequest, \"未知路由策略: \"+in.Strategy)\n\t\treturn\n\t}\n\tif in.MaxAttempts < 1 {\n\t\twriteErr(w, http.StatusBadRequest, \"max_attempts 至少为 1\")\n\t\treturn\n\t}\n\tnext.MaxAttempts = in.MaxAttempts",
			"	// 路由（留空表示不修改）\n\tif in.Strategy != \"\" {\n\t\tswitch in.Strategy {\n\t\tcase \"priority_round_robin\", \"round_robin\", \"failover\", \"random\":\n\t\t\tnext.Strategy = in.Strategy\n\t\tdefault:\n\t\t\twriteErr(w, http.StatusBadRequest, \"未知路由策略: \"+in.Strategy)\n\t\t\treturn\n\t\t}\n\t}\n\tif in.MaxAttempts != 0 {\n\t\tif in.MaxAttempts < 1 {\n\t\t\twriteErr(w, http.StatusBadRequest, \"max_attempts 至少为 1\")\n\t\t\treturn\n\t\t}\n\t\tnext.MaxAttempts = in.MaxAttempts\n\t}",
		},
		{
			"	if in.KeepLogs >= 10 {\n\t\tnext.KeepLogs = in.KeepLogs\n\t}",
			"	if in.KeepLogs != 0 {\n\t\tif in.KeepLogs < 10 {\n\t\t\twriteErr(w, http.StatusBadRequest, \"keep_logs 至少为 10\")\n\t\t\treturn\n\t\t}\n\t\tnext.KeepLogs = in.KeepLogs\n\t}",
		},
		{
			"	if in.PayloadLimit >= 100 {\n\t\tnext.PayloadLimit = in.PayloadLimit\n\t}",
			"	if in.PayloadLimit != 0 {\n\t\tif in.PayloadLimit < 100 {\n\t\t\twriteErr(w, http.StatusBadRequest, \"payload_limit 至少为 100\")\n\t\t\treturn\n\t\t}\n\t\tnext.PayloadLimit = in.PayloadLimit\n\t}",
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
