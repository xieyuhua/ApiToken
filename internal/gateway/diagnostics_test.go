package gateway_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/demo1/apitoken/internal/model"
)

// TestDiagnostics 验证网关自检：概览 + 完整体检（配置、渠道、网络）。
func TestDiagnostics(t *testing.T) {
	mock := newMock(false)
	defer mock.srv.Close()
	bad := newMock(true) // 恒定返回 429
	defer bad.srv.Close()

	h := newTestGateway(t, []model.Channel{
		{ID: "good", Name: "正常", BaseURL: mock.srv.URL + "/v1", APIKey: "k", Models: []string{"mock-chat"}, Priority: 1, Weight: 1, Enabled: true},
		{ID: "bad", Name: "异常", BaseURL: bad.srv.URL + "/v1", APIKey: "k", Models: []string{"mock-chat"}, Priority: 2, Weight: 1, Enabled: true},
		{ID: "nokey", Name: "缺密钥", BaseURL: mock.srv.URL + "/v1", Models: []string{"mock-chat"}, Priority: 3, Weight: 1, Enabled: true},
	})

	call := func(method, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("X-Admin-Token", "adm-test")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// 概览：静态自检
	rec := call(http.MethodGet, "/admin/diagnostics")
	if rec.Code != http.StatusOK {
		t.Fatalf("概览异常: %d %s", rec.Code, rec.Body.String())
	}
	var overview struct {
		Checks []struct {
			Group  string `json:"group"`
			Name   string `json:"name"`
			Level  string `json:"level"`
			Detail string `json:"detail"`
			Fix    string `json:"fix"`
		} `json:"checks"`
		Summary struct {
			Pass, Warn, Fail, Skip int
		} `json:"summary"`
		Healthy bool `json:"healthy"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &overview); err != nil {
		t.Fatalf("概览解析失败: %v", err)
	}
	if len(overview.Checks) == 0 {
		t.Fatal("概览应包含自检项")
	}
	// 未配置 admin_token/client_keys 在测试里是配置了的，因此应至少有 pass 项
	if overview.Summary.Pass == 0 {
		t.Fatalf("应存在通过项: %+v", overview.Summary)
	}
	// 存在缺密钥渠道 → 应报 fail
	foundKeyIssue := false
	for _, c := range overview.Checks {
		if c.Name == "API Key" && c.Level == "fail" && strings.Contains(c.Detail, "未填") {
			foundKeyIssue = true
		}
	}
	if !foundKeyIssue {
		t.Fatalf("应检出未填 api_key 的渠道: %+v", overview.Checks)
	}

	// 完整体检
	rec = call(http.MethodPost, "/admin/diagnostics/run")
	if rec.Code != http.StatusOK {
		t.Fatalf("体检异常: %d %s", rec.Code, rec.Body.String())
	}
	var report struct {
		DurationMS int64 `json:"duration_ms"`
		Channels   []struct {
			ID        string `json:"id"`
			OK        bool   `json:"ok"`
			Status    int    `json:"status"`
			LatencyMS int64  `json:"latency_ms"`
		} `json:"channels"`
		Network []struct {
			Host  string `json:"host"`
			DNS   bool   `json:"dns"`
			TCP   bool   `json:"tcp"`
			Level string `json:"level"`
		} `json:"network"`
		Summary struct {
			Pass, Warn, Fail int
		} `json:"summary"`
		Healthy bool `json:"healthy"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatalf("体检解析失败: %v", err)
	}
	if len(report.Channels) != 3 {
		t.Fatalf("应探测 3 个渠道，实际 %d: %s", len(report.Channels), rec.Body.String())
	}
	byID := map[string]bool{}
	for _, c := range report.Channels {
		byID[c.ID] = c.OK
	}
	if !byID["good"] {
		t.Fatal("good 渠道应探测成功")
	}
	if byID["bad"] {
		t.Fatal("bad 渠道应探测失败")
	}
	if len(report.Network) == 0 {
		t.Fatal("应包含域名网络探测结果")
	}
	for _, n := range report.Network {
		if n.Host == "" || n.Level == "" {
			t.Fatalf("网络探测项不完整: %+v", n)
		}
	}
	if report.DurationMS < 0 {
		t.Fatal("耗时异常")
	}
	if report.Healthy {
		t.Fatal("存在失败渠道时整体应为不健康")
	}
	_ = time.Now()
}
