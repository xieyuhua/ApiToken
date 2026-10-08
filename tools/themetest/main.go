package main

// 临时补丁：新增主题与密钥查看的测试。

import (
	"fmt"
	"os"
)

const testFile = "internal/gateway/theme_test.go"

const content = `package gateway_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestThemeAndSecretReveal 验证主题设置与密钥明文查看。
func TestThemeAndSecretReveal(t *testing.T) {
	h, cfgPath := newSettingsGateway(t)

	// 默认脱敏
	rec := adminDo(t, h, "adm-old", http.MethodGet, "/admin/settings", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("读取设置失败: %d", rec.Code)
	}
	var got struct {
		Settings struct {
			ClientKeys []string ` + "`json:\"client_keys\"`" + `
			AdminToken string   ` + "`json:\"admin_token\"`" + `
			Theme      string   ` + "`json:\"theme\"`" + `
		} ` + "`json:\"settings\"`" + `
		Revealed bool ` + "`json:\"revealed\"`" + `
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if got.Revealed {
		t.Fatal("默认不应返回明文")
	}
	if len(got.Settings.ClientKeys) != 1 || !strings.Contains(got.Settings.ClientKeys[0], "*") {
		t.Fatalf("密钥应脱敏: %+v", got.Settings.ClientKeys)
	}
	if strings.Contains(got.Settings.AdminToken, "adm-old") {
		t.Fatalf("令牌应脱敏: %q", got.Settings.AdminToken)
	}
	if got.Settings.Theme != "auto" {
		t.Fatalf("主题默认值应为 auto，实际 %q", got.Settings.Theme)
	}

	// reveal=1 返回明文
	rec = adminDo(t, h, "adm-old", http.MethodGet, "/admin/settings?reveal=1", "")
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if !got.Revealed || got.Settings.ClientKeys[0] != "sk-old-0001" || got.Settings.AdminToken != "adm-old" {
		t.Fatalf("reveal 未返回明文: %+v", got)
	}

	// 未授权不能查看明文
	if rec := adminDo(t, h, "", http.MethodGet, "/admin/settings?reveal=1", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("无令牌应 401，实际 %d", rec.Code)
	}

	// 修改主题并落盘
	rec = adminDo(t, h, "adm-old", http.MethodPost, "/admin/settings", ` + "`" + `{"theme":"light"}` + "`" + `)
	if rec.Code != http.StatusOK {
		t.Fatalf("保存主题失败: %s", rec.Body.String())
	}
	rec = adminDo(t, h, "adm-old", http.MethodGet, "/admin/settings", "")
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Settings.Theme != "light" {
		t.Fatalf("主题未生效: %q", got.Settings.Theme)
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "ui:") || !strings.Contains(string(data), "theme: light") {
		t.Fatalf("配置文件未写入 ui.theme:\n%s", data)
	}

	// 非法主题被拒绝
	if rec := adminDo(t, h, "adm-old", http.MethodPost, "/admin/settings", ` + "`" + `{"theme":"neon"}` + "`" + `); rec.Code != http.StatusBadRequest {
		t.Fatalf("非法主题应 400，实际 %d", rec.Code)
	}

	// 页面应包含主题按钮
	for _, page := range []string{"/", "/settings", "/logs", "/diagnostics", "/chat"} {
		req := httptest.NewRequest(http.MethodGet, page, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "id=\"themeBtn\"") {
			t.Fatalf("页面 %s 缺少主题按钮: %d", page, rec.Code)
		}
	}
}
`

func main() {
	if err := os.WriteFile(testFile, []byte(content), 0o644); err != nil {
		fmt.Println("write err:", err)
		os.Exit(1)
	}
	fmt.Println("created", testFile)
}
