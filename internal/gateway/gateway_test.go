package gateway_test

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/demo1/apitoken/internal/config"
	"github.com/demo1/apitoken/internal/gateway"
	"github.com/demo1/apitoken/internal/model"
	"github.com/demo1/apitoken/internal/store"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

type mockUpstream struct {
	srv       *httptest.Server
	failNext  atomic.Bool
	calls     atomic.Int64
	lastAuth  atomic.Value // Authorization 头
	lastAPI   atomic.Value // X-API-Key 之类自定义头
	lastQuery atomic.Value // ?key=
	lastUA    atomic.Value // User-Agent 头
}

func newMock(fail bool) *mockUpstream {
	m := &mockUpstream{}
	m.failNext.Store(fail)
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.calls.Add(1)
		m.lastAuth.Store(r.Header.Get("Authorization"))
		m.lastAPI.Store(r.Header.Get("X-API-Key"))
		m.lastQuery.Store(r.URL.Query().Get("key"))
		m.lastUA.Store(r.Header.Get("User-Agent"))
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)

		if strings.HasSuffix(r.URL.Path, "/models") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"mock-chat"},{"id":"mock-reason"}]}`))
			return
		}
		if m.failNext.Load() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limited","type":"rate_limit_error"}}`))
			return
		}
		modelName, _ := payload["model"].(string)
		if stream, _ := payload["stream"].(bool); stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fl := w.(http.Flusher)
			fmt.Fprint(w, "data: {\"id\":\"1\",\"choices\":[{\"delta\":{\"content\":\"你\"}}]}\n\n")
			fl.Flush()
			fmt.Fprint(w, "data: {\"id\":\"1\",\"choices\":[{\"delta\":{\"content\":\"好\"}}]}\n\n")
			fl.Flush()
			fmt.Fprint(w, "data: {\"id\":\"1\",\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":5}}\n\n")
			fl.Flush()
			fmt.Fprint(w, "data: [DONE]\n\n")
			fl.Flush()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"id":      "chatcmpl-mock",
			"model":   modelName,
			"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "pong"}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 11, "completion_tokens": 3, "total_tokens": 14},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	return m
}

func newTestGateway(t *testing.T, channels []model.Channel) http.Handler {
	t.Helper()
	h, _ := newTestGatewayWithDir(t, channels)
	return h
}

// newTestGatewayWithDir 额外返回数据目录，供需要校验持久化文件的用例使用。
func newTestGatewayWithDir(t *testing.T, channels []model.Channel) (http.Handler, string) {
	t.Helper()
	return newTestGatewayWithLogging(t, channels, config.LoggingConfig{
		Level: "error", KeepLogs: 100, RecordPayload: true, PayloadLimit: 2000,
	})
}

// newTestGatewayWithLogging 允许自定义日志配置（如开启文件落盘）。
func newTestGatewayWithLogging(t *testing.T, channels []model.Channel, logging config.LoggingConfig) (http.Handler, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{
		Server:   config.ServerConfig{Addr: ":0", DataDir: dir, MaxBodyMB: 8, RequestTimeout: config.Duration(20 * time.Second), StreamTimeout: config.Duration(30 * time.Second)},
		Security: config.SecurityConfig{ClientKeys: []string{"sk-test"}, AdminToken: "adm-test"},
		Routing:  config.RoutingConfig{Strategy: "priority_round_robin", MaxAttempts: 3, RetryStatus: []int{429, 500, 502, 503}},
		Logging:  logging,
		Channels: channels,
	}
	st, err := store.New(cfg, testLogger())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(st.CloseLogs) // 落盘模式下有后台协程，测试结束必须关闭
	return gateway.New(cfg, st, testLogger()).Handler(), dir
}

func post(t *testing.T, h http.Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestChatNonStreamWithFailover(t *testing.T) {
	bad := newMock(true)
	good := newMock(false)
	defer bad.srv.Close()
	defer good.srv.Close()

	chans := []model.Channel{
		{ID: "bad", Name: "限流渠道", BaseURL: bad.srv.URL + "/v1", APIKey: "k1", Models: []string{"mock-chat"}, Priority: 1, Weight: 1, Enabled: true},
		{ID: "good", Name: "正常渠道", BaseURL: good.srv.URL + "/v1", APIKey: "k2", Models: []string{"mock-chat"}, Priority: 2, Weight: 1, Enabled: true},
	}
	h := newTestGateway(t, chans)

	rec := post(t, h, "/v1/chat/completions", `{"model":"mock-chat","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens int64 `json:"prompt_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if out.Choices[0].Message.Content != "pong" {
		t.Fatalf("未走故障转移，内容=%q", out.Choices[0].Message.Content)
	}
	if out.Usage.PromptTokens != 11 {
		t.Fatalf("usage 解析错误: %+v", out.Usage)
	}
	if bad.calls.Load() == 0 || good.calls.Load() == 0 {
		t.Fatalf("期望两个渠道都被调用 bad=%d good=%d", bad.calls.Load(), good.calls.Load())
	}
	if auth, _ := good.lastAuth.Load().(string); auth != "Bearer k2" {
		t.Fatalf("鉴权头不正确: %q", auth)
	}
}

func TestChatStream(t *testing.T) {
	mock := newMock(false)
	defer mock.srv.Close()

	chans := []model.Channel{
		{ID: "s1", Name: "流式", BaseURL: mock.srv.URL + "/v1", APIKey: "k", Models: []string{"mock-chat"}, Priority: 1, Weight: 1, Enabled: true},
	}
	h := newTestGateway(t, chans)

	rec := post(t, h, "/v1/chat/completions", `{"model":"mock-chat","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", rec.Code)
	}
	if !strings.Contains(body, "data: [DONE]") || !strings.Contains(body, "你") || !strings.Contains(body, "好") {
		t.Fatalf("流式内容不完整: %s", body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type 错误: %s", ct)
	}
}

func TestModelsAndAuth(t *testing.T) {
	mock := newMock(false)
	defer mock.srv.Close()
	chans := []model.Channel{
		{ID: "m1", Name: "模型渠道", BaseURL: mock.srv.URL + "/v1", APIKey: "k", Models: []string{"mock-chat", "mock-reason"}, Priority: 1, Weight: 1, Enabled: true},
	}
	h := newTestGateway(t, chans)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer sk-test")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "mock-chat") {
		t.Fatalf("/v1/models 异常: %d %s", rec.Code, rec.Body.String())
	}

	// 缺少密钥
	req2 := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("期望 401，实际 %d", rec2.Code)
	}

	// 模型不存在
	rec3 := post(t, h, "/v1/chat/completions", `{"model":"not-exist","messages":[]}`)
	if rec3.Code != http.StatusNotFound {
		t.Fatalf("期望 404，实际 %d", rec3.Code)
	}
}

func TestAdminChannelCRUD(t *testing.T) {
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

	// 未带令牌应 401
	req := httptest.NewRequest(http.MethodGet, "/admin/channels", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("管理接口应校验令牌，实际 %d", rec.Code)
	}

	// 新建渠道：未填写的字段应被默认值补全（chat_path / auth_style / weight）
	create := fmt.Sprintf(`{"id":"ds1","name":"DeepSeek 官方","base_url":"%s","api_key":"sk-secret","models":[],"enabled":true}`, mock.srv.URL+"/v1")
	if rec := call(http.MethodPost, "/admin/channels", create); rec.Code != http.StatusCreated {
		t.Fatalf("创建渠道失败: %d %s", rec.Code, rec.Body.String())
	}
	list := call(http.MethodGet, "/admin/channels", "").Body.String()
	if !strings.Contains(list, mock.srv.URL) || strings.Contains(list, "sk-secret") {
		t.Fatalf("渠道信息异常或密钥未脱敏: %s", list)
	}
	if !strings.Contains(list, `"chat_path":"/chat/completions"`) || !strings.Contains(list, `"auth_style":"bearer"`) {
		t.Fatalf("默认值未补全: %s", list)
	}

	// 拉取上游模型
	if rec := call(http.MethodPost, "/admin/channels/ds1/models", `{"apply":true}`); rec.Code != http.StatusOK {
		t.Fatalf("拉取模型失败: %s", rec.Body.String())
	}
	if !strings.Contains(call(http.MethodGet, "/admin/channels", "").Body.String(), "mock-reason") {
		t.Fatal("模型未写入渠道")
	}

	// 连通性测试
	rec = call(http.MethodPost, "/admin/channels/ds1/test", `{"prompt":"ping"}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("测试接口异常: %d %s", rec.Code, rec.Body.String())
	}

	// 路由与统计
	if rec := call(http.MethodPost, "/admin/routes", `{"model":"gpt-4o","channels":["ds1"]}`); rec.Code != http.StatusOK {
		t.Fatalf("创建路由失败: %s", rec.Body.String())
	}
	if rec := call(http.MethodGet, "/admin/stats", ""); rec.Code != http.StatusOK {
		t.Fatalf("统计接口异常: %s", rec.Body.String())
	}
	if rec := call(http.MethodDelete, "/admin/routes/gpt-4o", ""); rec.Code != http.StatusOK {
		t.Fatalf("删除路由失败: %s", rec.Body.String())
	}
	if rec := call(http.MethodDelete, "/admin/channels/ds1", ""); rec.Code != http.StatusOK {
		t.Fatalf("删除渠道失败: %s", rec.Body.String())
	}
}
