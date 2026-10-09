package gateway_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/demo1/apitoken/internal/model"
)

// TestChatPageAndChannelHeaders 校验聊天页面可访问，且响应带命中的渠道头（前端据此展示路由结果）。
func TestChatPageAndChannelHeaders(t *testing.T) {
	mock := newMock(false)
	defer mock.srv.Close()
	h := newTestGateway(t, []model.Channel{
		{ID: "c1", Name: "测试渠道", BaseURL: mock.srv.URL + "/v1", APIKey: "k",
			Models: []string{"mock-chat"}, Priority: 1, Weight: 1, Enabled: true},
	})

	// 页面与静态脚本可访问
	for _, p := range []string{"/chat", "/static/chat.js"} {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || rec.Body.Len() == 0 {
			t.Fatalf("%s 异常: %d", p, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/chat", nil)
	h.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "/static/chat.js") {
		t.Fatal("聊天页应引用 chat.js")
	}

	// 非流式：渠道头
	rec = post(t, h, "/v1/chat/completions", `{"model":"mock-chat","messages":[]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("请求失败: %d", rec.Code)
	}
	if got := rec.Header().Get("X-Gateway-Channel"); got != "c1" {
		t.Fatalf("缺少 X-Gateway-Channel: %q", got)
	}
	if got := rec.Header().Get("X-Gateway-Channel-Name"); got != "测试渠道" {
		t.Fatalf("缺少 X-Gateway-Channel-Name: %q", got)
	}
	if got := rec.Header().Get("X-Gateway-Upstream-Model"); got != "mock-chat" {
		t.Fatalf("缺少 X-Gateway-Upstream-Model: %q", got)
	}
	if rec.Header().Get("X-Request-Id") == "" {
		t.Fatal("缺少 X-Request-Id")
	}

	// 流式：同样带渠道头，且是 SSE
	rec = post(t, h, "/v1/chat/completions", `{"model":"mock-chat","messages":[],"stream":true}`)
	if rec.Code != http.StatusOK || rec.Header().Get("X-Gateway-Channel") != "c1" {
		t.Fatalf("流式响应头异常: %d %q", rec.Code, rec.Header().Get("X-Gateway-Channel"))
	}

	// 模型列表带 upstreams，供页面显示可用渠道
	req = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer sk-test")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var list struct {
		Data []struct {
			ID        string `json:"id"`
			Upstreams []struct {
				ChannelID string `json:"channel_id"`
			} `json:"upstreams"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("解析 /v1/models 失败: %v", err)
	}
	if len(list.Data) == 0 || len(list.Data[0].Upstreams) == 0 || list.Data[0].Upstreams[0].ChannelID != "c1" {
		t.Fatalf("模型应带 upstreams 信息: %s", rec.Body.String())
	}
}

// TestChatModelSearchableCombo 校验对话页的模型选择器是可搜索组合框：
// 507 个模型时原生 select 无法定位，必须提供搜索框、结果列表与提示。
func TestChatModelSearchableCombo(t *testing.T) {
	mock := newMock(false)
	defer mock.srv.Close()
	h := newTestGateway(t, nil)

	req := httptest.NewRequest(http.MethodGet, "/chat", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("对话页不可访问: %d", rec.Code)
	}
	for _, want := range []string{`id="modelBox"`, `id="modelList"`, `id="modelHint"`, "搜索模型"} {
		if !strings.Contains(body, want) {
			t.Fatalf("对话页缺少可搜索下拉结构 %q", want)
		}
	}
	if strings.Contains(body, `<select id="model"`) {
		t.Fatal("原生 select 无法搜索，应替换为组合框")
	}
	// 必须复用 /static/combo.js，不要再自带一份实现
	if !strings.Contains(body, "/static/combo.js") {
		t.Fatal("对话页未引入 combo.js")
	}

	req = httptest.NewRequest(http.MethodGet, "/static/chat.js", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	js := rec.Body.String()
	for _, want := range []string{
		"function initModelCombo(", "Combo.attach(inp", "function modelOptions(",
		"modelChannelText(m)", "initModelCombo();",
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("chat.js 缺少 %q", want)
		}
	}
	// 重复实现（与 combo.js 重复的渲染/高亮/键盘逻辑）应当已删除
	for _, gone := range []string{"function renderModelList(", "function pickModel(", "MODEL_LIST_MAX", "function hl("} {
		if strings.Contains(js, gone) {
			t.Fatalf("chat.js 仍自带一份下拉实现（%q），应复用 combo.js", gone)
		}
	}
}
