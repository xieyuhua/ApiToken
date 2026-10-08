package main

// 临时补丁：SettingsView 布尔字段改为指针。

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
	old := "	// 路由\n	Strategy         string `json:\"strategy\"`\n	MaxAttempts      int    `json:\"max_attempts\"`\n	ForceStreamUsage bool   `json:\"force_stream_usage\"`\n	RetryStatus      []int  `json:\"retry_status\"`\n\n	// 日志\n	AccessLog     bool   `json:\"access_log\"`\n	KeepLogs      int    `json:\"keep_logs\"`\n\tRecordPayload bool   `json:\"record_payload\"`\n\tPayloadLimit  int    `json:\"payload_limit\"`\n	LogLevel      string `json:\"log_level\"`"
	nw := "	// 路由（布尔项为指针：nil 表示不修改）\n	Strategy         string `json:\"strategy\"`\n	MaxAttempts      int    `json:\"max_attempts\"`\n	ForceStreamUsage *bool  `json:\"force_stream_usage\"`\n	RetryStatus      []int  `json:\"retry_status\"`\n\n	// 日志\n	AccessLog     *bool  `json:\"access_log\"`\n	KeepLogs      int    `json:\"keep_logs\"`\n\tRecordPayload *bool  `json:\"record_payload\"`\n\tPayloadLimit  int    `json:\"payload_limit\"`\n\tLogLevel      string `json:\"log_level\"`"
	if !strings.Contains(s, old) {
		fmt.Println("not found")
		os.Exit(1)
	}
	s = strings.Replace(s, old, nw, 1)
	if err := os.WriteFile(file, []byte(s), 0o644); err != nil {
		fmt.Println("write err:", err)
		os.Exit(1)
	}
	fmt.Println("patched struct fields")
}
