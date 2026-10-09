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

// TestUserAgentPassthrough 校验客户端 User-Agent 原样透传到上游。
//
// 上游（尤其第三方中转/风控）会按 UA 做来源识别，OpenAI SDK、Chatbox、Cline 的 UA
// 是真实客户端特征，被网关改写成 Go-http-client/1.1 会触发额外校验或降级。
func TestUserAgentPassthrough(t *testing.T) {
	mock := newMock(false)
	defer mock.srv.Close()
	h := newTestGateway(t, []model.Channel{
		{ID: "c1", Name: "渠道", BaseURL: mock.srv.URL + "/v1", APIKey: "k",
			Models: []string{"mock-chat"}, Priority: 1, Weight: 1, Enabled: true},
	})

	const ua = "Chatbox/1.2.3 (Windows NT 10.0; Win64; x64)"
	send := func(ua string, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer sk-test")
		req.Header.Set("Content-Type", "application/json")
		if ua != "" {
			req.Header.Set("User-Agent", ua)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// 非流式与流式都要透传
	for _, body := range []string{
		`{"model":"mock-chat","messages":[]}`,
		`{"model":"mock-chat","messages":[],"stream":true}`,
	} {
		if rec := send(ua, body); rec.Code != http.StatusOK {
			t.Fatalf("请求失败: %d %s", rec.Code, rec.Body.String())
		}
		if got, _ := mock.lastUA.Load().(string); got != ua {
			t.Fatalf("UA 未透传，上游收到 %q", got)
		}
	}

	// 客户端没带 UA 时不额外注入，沿用 Go 客户端默认值
	// （网关主动发起的 Probe / FetchModels 也走这条路径，不该带某个客户端的 UA）
	send("", `{"model":"mock-chat","messages":[]}`)
	if got, _ := mock.lastUA.Load().(string); got != "Go-http-client/1.1" {
		t.Fatalf("未带 UA 时不应注入，上游收到 %q", got)
	}
}

// TestUserAgentSanitizeAndOverride 校验 UA 的清洗与渠道级覆盖。
func TestUserAgentSanitizeAndOverride(t *testing.T) {
	mock := newMock(false)
	defer mock.srv.Close()

	base := model.Channel{ID: "c1", Name: "渠道", BaseURL: mock.srv.URL + "/v1", APIKey: "k",
		Models: []string{"mock-chat"}, Priority: 1, Weight: 1, Enabled: true}

	// 客户端 UA 里的 CR/LF 必须被剔除，否则会变成非法头值让整个请求失败
	h := newTestGateway(t, []model.Channel{base})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"mock-chat","messages":[]}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "ok\r\nX-Injected: 1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("请求失败: %d %s", rec.Code, rec.Body.String())
	}
	if got, _ := mock.lastUA.Load().(string); strings.ContainsAny(got, "\r\n") {
		t.Fatalf("UA 未剔除换行: %q", got)
	}

	// 渠道 extra_headers 优先级最高：可将 UA 固定成渠道标识
	pinned := base
	pinned.ID = "c2"
	pinned.ExtraHeaders = map[string]string{"User-Agent": "pinned/1.0"}
	h2 := newTestGateway(t, []model.Channel{pinned})
	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"mock-chat","messages":[]}`))
	req2.Header.Set("Authorization", "Bearer sk-test")
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("User-Agent", "Chatbox/1.2.3")
	rec2 := httptest.NewRecorder()
	h2.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("请求失败: %d %s", rec2.Code, rec2.Body.String())
	}
	if got, _ := mock.lastUA.Load().(string); got != "pinned/1.0" {
		t.Fatalf("extra_headers 应覆盖透传的 UA，上游收到 %q", got)
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
