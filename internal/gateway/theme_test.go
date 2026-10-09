package gateway_test

import (
	"encoding/json"
	"github.com/demo1/apitoken/internal/model"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// TestThemeAndSecretReveal 验证主题设置与密钥明文查看。
// TestMaskSecretFixedWidth 校验脱敏输出为固定宽度：长密钥脱敏后不应等长，
// 否则管理页表格会被超长密钥撑开；同时不应泄露密钥长度。
func TestMaskSecretFixedWidth(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"short", "*****"},
		{"12345678", "********"},
		{"sk-abcdefghijklmnopqrstuvwxyz0123456789", "sk-a****6789"},
		{"averyveryverylongapikeywithoutprefix", "aver****efix"},
	}
	for _, tc := range cases {
		got := model.MaskSecret(tc.in)
		if got != tc.want {
			t.Fatalf("MaskSecret(%q) = %q, 期望 %q", tc.in, got, tc.want)
		}
		if tc.in != "" && len(got) > 12 {
			t.Fatalf("脱敏结果过长(%d 字符)，会撑宽表格: %q", len(got), got)
		}
	}
	// 渠道列表接口返回的必须是脱敏值，且长度固定
	mock := newMock(false)
	defer mock.srv.Close()
	h := newTestGateway(t, nil)
	req := httptest.NewRequest(http.MethodPost, "/admin/channels",
		strings.NewReader(`{"id":"longkey","name":"长密钥渠道","base_url":"`+mock.srv.URL+
			`/v1","api_key":"sk-1234567890abcdefghijklmnopqrstuvwxyz","models":["m"],"enabled":true}`))
	req.Header.Set("X-Admin-Token", "adm-test")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("创建渠道失败: %d %s", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "/admin/channels", nil)
	req.Header.Set("X-Admin-Token", "adm-test")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var items []struct {
		ID     string `json:"id"`
		APIKey string `json:"api_key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &items); err != nil {
		t.Fatalf("解析渠道列表失败: %v", err)
	}
	for _, it := range items {
		if strings.Contains(it.APIKey, "1234567890abcdef") {
			t.Fatalf("渠道 %s 的密钥未脱敏: %q", it.ID, it.APIKey)
		}
		if it.APIKey == "" {
			continue
		}
		if len(it.APIKey) > 12 {
			t.Fatalf("渠道 %s 的脱敏密钥过长(%d): %q", it.ID, len(it.APIKey), it.APIKey)
		}
	}
	// 明文仍可通过 reveal 取回
	req = httptest.NewRequest(http.MethodGet, "/admin/channels?reveal=1", nil)
	req.Header.Set("X-Admin-Token", "adm-test")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "sk-1234567890abcdefghijklmnopqrstuvwxyz") {
		t.Fatal("reveal=1 应返回完整密钥")
	}
}
func TestThemeAndSecretReveal(t *testing.T) {
	h, cfgPath := newSettingsGateway(t)

	// 默认脱敏
	rec := adminDo(t, h, "adm-old", http.MethodGet, "/admin/settings", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("读取设置失败: %d", rec.Code)
	}
	var got struct {
		Settings struct {
			ClientKeys []string `json:"client_keys"`
			AdminToken string   `json:"admin_token"`
			Theme      string   `json:"theme"`
		} `json:"settings"`
		Revealed bool `json:"revealed"`
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
	rec = adminDo(t, h, "adm-old", http.MethodPost, "/admin/settings", `{"theme":"light"}`)
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
	if rec := adminDo(t, h, "adm-old", http.MethodPost, "/admin/settings", `{"theme":"neon"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("非法主题应 400，实际 %d", rec.Code)
	}

	// 主题选择器由 app.js 动态挂载，页面需引用该脚本且提供主题样式
	for _, page := range []string{"/", "/settings", "/logs", "/diagnostics", "/chat"} {
		req := httptest.NewRequest(http.MethodGet, page, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		body := rec.Body.String()
		if rec.Code != http.StatusOK {
			t.Fatalf("页面 %s 异常: %d", page, rec.Code)
		}
		if !strings.Contains(body, "/static/app.js") {
			t.Fatalf("页面 %s 未引用 app.js（主题选择器依赖它）", page)
		}
	}
}
