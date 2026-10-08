package gateway_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestChannelAliasMapping 验证别名映射（客户端模型名 → 上游模型名）能保存并生效。
func TestChannelAliasMapping(t *testing.T) {
	mock := newMock(false)
	defer mock.srv.Close()
	h := newTestGateway(t, nil)

	call := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("X-Admin-Token", "adm-test")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// 前端 collectAlias() 的产物：客户端名 -> 上游名
	create := "{\"id\":\"al1\",\"name\":\"别名渠道\",\"base_url\":\"" + mock.srv.URL + "/v1\"," +
		"\"api_key\":\"k\",\"models\":[\"up-model-a\",\"up-model-b\"]," +
		"\"alias\":{\"gpt-4o\":\"up-model-a\",\"my-chat\":\"up-model-b\"},\"enabled\":true}"
	rec := call(http.MethodPost, "/admin/channels", create)
	if rec.Code != http.StatusCreated {
		t.Fatalf("创建渠道失败: %d %s", rec.Code, rec.Body.String())
	}

	// 读取校验（保存的是对象）
	rec = call(http.MethodGet, "/admin/channels/al1", "")
	var ch struct {
		Alias map[string]string `json:"alias"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &ch); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(ch.Alias) != 2 || ch.Alias["gpt-4o"] != "up-model-a" || ch.Alias["my-chat"] != "up-model-b" {
		t.Fatalf("别名映射不对: %+v", ch.Alias)
	}

	// 用别名请求：上游应收到映射后的真实模型名
	rec = post(t, h, "/v1/chat/completions", `{"model":"gpt-4o","messages":[]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("别名请求失败: %d %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Gateway-Upstream-Model"); got != "up-model-a" {
		t.Fatalf("未按别名改写模型名: %q", got)
	}
	rec = post(t, h, "/v1/chat/completions", `{"model":"my-chat","messages":[]}`)
	if got := rec.Header().Get("X-Gateway-Upstream-Model"); got != "up-model-b" {
		t.Fatalf("第二条别名未生效: %q", got)
	}

	// PATCH 清空别名
	rec = call(http.MethodPatch, "/admin/channels/al1", `{"alias":{}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("清空别名失败: %s", rec.Body.String())
	}
	rec = call(http.MethodGet, "/admin/channels/al1", "")
	if strings.Contains(rec.Body.String(), "\"alias\"") {
		t.Fatalf("别名应被清空: %s", rec.Body.String())
	}
	ch.Alias = map[string]string{}
	_ = json.Unmarshal(rec.Body.Bytes(), &ch)
	if len(ch.Alias) != 0 {
		t.Fatalf("别名应被清空: %+v", ch.Alias)
	}
}
