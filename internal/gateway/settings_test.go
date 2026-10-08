package gateway_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/demo1/apitoken/internal/config"
	"github.com/demo1/apitoken/internal/gateway"
	"github.com/demo1/apitoken/internal/store"
)

// newSettingsGateway 构造一个带真实配置文件的网关，用于验证设置保存与热更新。
func newSettingsGateway(t *testing.T) (http.Handler, string) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	yaml := `server:
  addr: ":0"
  request_timeout: 300s
  stream_timeout: 900s
  max_body_mb: 32
  data_dir: "` + filepath.ToSlash(filepath.Join(dir, "data")) + `"
security:
  client_keys:
    - "sk-old-0001"
  admin_token: "adm-old"
routing:
  strategy: "priority_round_robin"
  max_attempts: 3
  retry_status: [429, 500, 503]
logging:
  level: "info"
  access_log: true
  keep_logs: 500
  record_payload: false
  payload_limit: 2000
upstream:
  default_timeout: 120s
  connect_timeout: 10s
  proxy_url: ""
channels:
  - id: "c1"
    name: "本地"
    base_url: "https://example.com/v1"
    api_key: "sk-x"
    models: ["m1"]
    enabled: true
`
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	st, err := store.New(cfg, testLogger())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	return gateway.New(cfg, st, testLogger()).Handler(), cfgPath
}

func adminDo(t *testing.T, h http.Handler, token, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("X-Admin-Token", token)
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestSettingsPageAndHotUpdate 验证设置页可访问、密钥可改且立即生效、并写回配置文件。
func TestSettingsPageAndHotUpdate(t *testing.T) {
	h, cfgPath := newSettingsGateway(t)

	// 页面可访问
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/settings", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "网关密钥") {
		t.Fatalf("设置页异常: %d", rec.Code)
	}

	// 读取当前设置（密钥脱敏）
	rec = adminDo(t, h, "adm-old", http.MethodGet, "/admin/settings", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("读取设置失败: %d %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Settings struct {
			ClientKeys  []string `json:"client_keys"`
			AdminToken  string   `json:"admin_token"`
			MaxAttempts int      `json:"max_attempts"`
			KeepLogs    int      `json:"keep_logs"`
		} `json:"settings"`
		Meta struct {
			ConfigFile string `json:"config_file"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析设置失败: %v", err)
	}
	if len(got.Settings.ClientKeys) != 1 || strings.Contains(got.Settings.ClientKeys[0], "sk-old-0001") {
		t.Fatalf("密钥应脱敏: %+v", got.Settings.ClientKeys)
	}
	if got.Meta.ConfigFile != cfgPath {
		t.Fatalf("配置文件路径不对: %s", got.Meta.ConfigFile)
	}

	// 修改密钥与部分参数
	body := `{"client_keys":["sk-new-aaa1","sk-new-bbb2"],"admin_token":"adm-new-9","strategy":"failover",` +
		`"max_attempts":2,"force_stream_usage":true,"retry_status":[429,500,503,504],` +
		`"access_log":false,"keep_logs":300,"record_payload":true,"payload_limit":1500,` +
		`"request_timeout":120,"stream_timeout":600,"proxy_url":"none"}`
	rec = adminDo(t, h, "adm-old", http.MethodPost, "/admin/settings", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("保存设置失败: %d %s", rec.Code, rec.Body.String())
	}
	var saved struct {
		OK            bool     `json:"ok"`
		AdminTokenNew bool     `json:"admin_token_new"`
		ClientKeysNew bool     `json:"client_keys_new"`
		AdminToken    string   `json:"admin_token"`
		ClientKeys    []string `json:"client_keys"`
		Persist       string   `json:"persist"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &saved); err != nil {
		t.Fatalf("解析保存结果失败: %v", err)
	}
	if !saved.OK || !saved.AdminTokenNew || !saved.ClientKeysNew {
		t.Fatalf("应提示密钥/令牌已变更: %+v", saved)
	}
	if saved.AdminToken != "adm-new-9" || len(saved.ClientKeys) != 2 {
		t.Fatalf("返回值异常: %+v", saved)
	}

	// 新密钥立即生效：旧密钥 401，新密钥可访问模型
	rec = adminDo(t, h, "adm-old", http.MethodGet, "/admin/channels", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("旧管理令牌应失效，实际 %d", rec.Code)
	}
	rec = adminDo(t, h, "adm-new-9", http.MethodGet, "/admin/channels", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("新管理令牌应生效，实际 %d", rec.Code)
	}

	chatReq := func(key string) int {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := chatReq("sk-old-0001"); code != http.StatusUnauthorized {
		t.Fatalf("旧网关密钥应失效，实际 %d", code)
	}
	if code := chatReq("sk-new-bbb2"); code != http.StatusOK {
		t.Fatalf("新网关密钥应生效，实际 %d", code)
	}
	if code := chatReq(""); code != http.StatusUnauthorized {
		t.Fatalf("空密钥应被拒绝，实际 %d", code)
	}

	// 配置写回磁盘，且保留注释与未涉及字段（channels）
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	saved2 := string(data)
	for _, want := range []string{"sk-new-aaa1", "sk-new-bbb2", "adm-new-9", "failover", "channels:", "id: \"c1\"", "keep_logs: 300", "proxy_url: none"} {
		if !strings.Contains(saved2, want) {
			t.Fatalf("配置文件缺少 %q:\n%s", want, saved2)
		}
	}
	if strings.Contains(saved2, "sk-old-0001") {
		t.Fatalf("旧密钥仍留在配置文件:\n%s", saved2)
	}

	// 重新加载配置，确认持久化结果可被再次解析
	if _, err := config.Load(cfgPath); err != nil {
		t.Fatalf("保存后的配置无法加载: %v", err)
	}

	// 非法值应被拒绝
	if rec := adminDo(t, h, "adm-new-9", http.MethodPost, "/admin/settings", `{"strategy":"unknown","max_attempts":1}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("非法策略应返回 400，实际 %d", rec.Code)
	}
	if rec := adminDo(t, h, "adm-new-9", http.MethodPost, "/admin/settings", `{"strategy":"failover","max_attempts":-1}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("max_attempts=-1 应返回 400，实际 %d", rec.Code)
	}

	// 不修改密钥（留空 / 掩码）时保持原值
	if rec := adminDo(t, h, "adm-new-9", http.MethodPost, "/admin/settings",
		`{"client_keys":null,"admin_token":"","strategy":"failover","max_attempts":2,"keep_logs":300}`); rec.Code != http.StatusOK {
		t.Fatalf("留空保存应成功: %s", rec.Body.String())
	}
	rec = adminDo(t, h, "adm-new-9", http.MethodGet, "/admin/settings", "")
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if len(got.Settings.ClientKeys) != 2 {
		t.Fatalf("留空不应清空密钥: %+v", got.Settings.ClientKeys)
	}
}
