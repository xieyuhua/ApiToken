package gateway_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/demo1/apitoken/internal/model"
)

// TestChannelDraftProbe 验证「添加渠道」弹窗中未保存的草稿也能测试连通性与拉取模型。
func TestChannelDraftProbe(t *testing.T) {
	mock := newMock(false)
	defer mock.srv.Close()
	h := newTestGateway(t, nil)

	call := func(path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("X-Admin-Token", "adm-test")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	draft := func(extra string) string {
		base := fmt.Sprintf(`{"name":"草稿渠道","base_url":%q,"api_key":"k-draft","models":[]`, mock.srv.URL+"/v1")
		if extra != "" {
			base += "," + extra
		}
		return base + "}"
	}

	// 测试连通性
	rec := call("/admin/channels/test", draft(`"model":"mock-chat"`))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("草稿测试失败: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "pong") {
		t.Fatalf("应返回上游回复: %s", rec.Body.String())
	}
	if auth, _ := mock.lastAuth.Load().(string); auth != "Bearer k-draft" {
		t.Fatalf("草稿 Key 未注入: %q", auth)
	}

	// 拉取模型
	rec = call("/admin/channels/models", draft(""))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "mock-reason") {
		t.Fatalf("草稿拉取模型失败: %d %s", rec.Code, rec.Body.String())
	}

	// 草稿不应被保存
	req := httptest.NewRequest(http.MethodGet, "/admin/channels", nil)
	req.Header.Set("X-Admin-Token", "adm-test")
	recList := httptest.NewRecorder()
	h.ServeHTTP(recList, req)
	var list []model.Channel
	_ = json.Unmarshal(recList.Body.Bytes(), &list)
	if len(list) != 0 {
		t.Fatalf("草稿探测不应写入渠道: %s", recList.Body.String())
	}

	// 缺少 base_url
	if rec := call("/admin/channels/test", `{"api_key":"k"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("缺少 base_url 应返回 400，实际 %d: %s", rec.Code, rec.Body.String())
	}

	// 自定义 header 鉴权的草稿也能测试
	if rec := call("/admin/channels/test", draft(`"auth_style":"header","auth_header":"X-API-Key"`)); rec.Code != http.StatusOK {
		t.Fatalf("header 鉴权草稿测试失败: %d %s", rec.Code, rec.Body.String())
	}
	if v, _ := mock.lastAPI.Load().(string); v != "k-draft" {
		t.Fatalf("自定义头未注入: %q", v)
	}
}
