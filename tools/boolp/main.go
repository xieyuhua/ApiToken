package main

// 临时补丁：设置项布尔值改为 *bool，缺省表示不修改。

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
		{"internal/admin/settings.go",
			"	// 路由\n\tStrategy         string `json:\"strategy\"`\n\tMaxAttempts      int    `json:\"max_attempts\"`\n\tForceStreamUsage bool   `json:\"force_stream_usage\"`\n\tRetryStatus      []int  `json:\"retry_status\"`\n\n\t// 日志\n\tAccessLog     bool `json:\"access_log\"`\n\tKeepLogs      int  `json:\"keep_logs\"`\n\tRecordPayload bool `json:\"record_payload\"`\n\tPayloadLimit  int  `json:\"payload_limit\"`\n\tLogLevel      string `json:\"log_level\"`",
			"	// 路由（布尔项为指针：nil 表示不修改）\n\tStrategy         string `json:\"strategy\"`\n\tMaxAttempts      int    `json:\"max_attempts\"`\n\tForceStreamUsage *bool  `json:\"force_stream_usage\"`\n\tRetryStatus      []int  `json:\"retry_status\"`\n\n\t// 日志\n\tAccessLog     *bool  `json:\"access_log\"`\n\tKeepLogs      int    `json:\"keep_logs\"`\n\tRecordPayload *bool  `json:\"record_payload\"`\n\tPayloadLimit  int    `json:\"payload_limit\"`\n\tLogLevel      string `json:\"log_level\"`"},

		{"internal/admin/settings.go",
			"	return SettingsView{\n\t\tClientKeys:       append([]string(nil), rt.ClientKeys...),\n\t\tAdminToken:       rt.AdminToken,\n\t\tStrategy:         rt.Strategy,\n\t\tMaxAttempts:      rt.MaxAttempts,\n\t\tForceStreamUsage: rt.ForceStreamUsage,\n\t\tRetryStatus:      append([]int(nil), rt.RetryStatus...),\n\t\tAccessLog:        rt.AccessLog,\n\t\tKeepLogs:         rt.KeepLogs,\n\t\tRecordPayload:    rt.RecordPayload,\n\t\tPayloadLimit:     rt.PayloadLimit,\n\t\tLogLevel:         level,",
			"	return SettingsView{\n\t\tClientKeys:       append([]string(nil), rt.ClientKeys...),\n\t\tAdminToken:       rt.AdminToken,\n\t\tStrategy:         rt.Strategy,\n\t\tMaxAttempts:      rt.MaxAttempts,\n\t\tForceStreamUsage: boolPtr(rt.ForceStreamUsage),\n\t\tRetryStatus:      append([]int(nil), rt.RetryStatus...),\n\t\tAccessLog:        boolPtr(rt.AccessLog),\n\t\tKeepLogs:         rt.KeepLogs,\n\t\tRecordPayload:    boolPtr(rt.RecordPayload),\n\t\tPayloadLimit:     rt.PayloadLimit,\n\t\tLogLevel:         level,"},

		{"internal/admin/settings.go",
			"	next.MaxAttempts = in.MaxAttempts\n\tnext.ForceStreamUsage = in.ForceStreamUsage",
			"	next.MaxAttempts = in.MaxAttempts\n\tif in.ForceStreamUsage != nil {\n\t\tnext.ForceStreamUsage = *in.ForceStreamUsage\n\t}"},

		{"internal/admin/settings.go",
			"	// 日志\n\tnext.AccessLog = in.AccessLog",
			"	// 日志\n\tif in.AccessLog != nil {\n\t\tnext.AccessLog = *in.AccessLog\n\t}"},

		{"internal/admin/settings.go",
			"	next.RecordPayload = in.RecordPayload",
			"	if in.RecordPayload != nil {\n\t\tnext.RecordPayload = *in.RecordPayload\n\t}"},

		{"internal/admin/settings.go",
			"// getSettings GET /admin/settings",
			"func boolPtr(b bool) *bool { return &b }\n\n// getSettings GET /admin/settings"},
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
		s = strings.Replace(s, r.old, r.nw, 1)
		if err := os.WriteFile(r.file, []byte(s), 0o644); err != nil {
			fmt.Println("write err:", err)
			os.Exit(1)
		}
		fmt.Printf("patched %s :: %.45q\n", r.file, r.old)
	}
}
