package gateway_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/demo1/apitoken/internal/config"
	"github.com/demo1/apitoken/internal/gateway"
	"github.com/demo1/apitoken/internal/model"
	"github.com/demo1/apitoken/internal/store"
)

// strictStreamUpstream 模拟「不接受 stream_options 字段」的上游：
// 流式请求体里带 stream_options 就返回 400（腾讯 WorkBuddy / 部分商汤端点即如此）。
type strictStreamUpstream struct {
	srv             *httptest.Server
	sawStreamOpts   bool
	streamOptsCalls int
	plainCalls      int
}

func newStrictStreamUpstream() *strictStreamUpstream {
	m := &strictStreamUpstream{}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "stream_options") {
			m.sawStreamOpts = true
			m.streamOptsCalls++
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"unknown field stream_options","type":"invalid_request_error"}}`))
			return
		}
		m.plainCalls++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n"))
		fl.Flush()
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		fl.Flush()
	}))
	return m
}

// TestStreamOptionsInjectionFallback 验证：全局开启 force_stream_usage 时，
// 遇到不接受 stream_options 的上游应自动去掉该字段重试，而不是让流式请求直接 400。
func TestStreamOptionsInjectionFallback(t *testing.T) {
	up := newStrictStreamUpstream()
	defer up.srv.Close()

	dir := t.TempDir()
	cfg := &config.Config{
		Server:   config.ServerConfig{Addr: ":0", DataDir: dir, MaxBodyMB: 8},
		Security: config.SecurityConfig{ClientKeys: []string{"sk-test"}, AdminToken: "adm-test"},
		Routing: config.RoutingConfig{
			Strategy: "failover", MaxAttempts: 3, RetryStatus: []int{429, 500, 502, 503},
			ForceStreamUsage: true, // 用户当前配置：开启
		},
		Logging: config.LoggingConfig{Level: "error", KeepLogs: 100, RecordPayload: false},
		Channels: []model.Channel{
			{ID: "c1", Name: "严格上游", BaseURL: up.srv.URL + "/v1", APIKey: "k",
				Models: []string{"m1"}, Priority: 1, Weight: 1, Enabled: true},
		},
	}
	st, err := store.New(cfg, testLogger())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(st.CloseLogs)
	h := gateway.New(cfg, st, testLogger()).Handler()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m1","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("流式请求应通过降级重试成功，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"content":"ok"`) {
		t.Fatalf("响应应包含上游内容: %s", rec.Body.String())
	}
	if !up.sawStreamOpts {
		t.Fatal("复现失败：网关没有注入 stream_options，该用例无法覆盖目标场景")
	}
	if up.plainCalls != 1 {
		t.Fatalf("应在去掉 stream_options 后重试 1 次，实际 plainCalls=%d streamOptsCalls=%d",
			up.plainCalls, up.streamOptsCalls)
	}

	// 第二次请求：渠道已记住「不支持 stream_options」，应直接不发该字段
	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m1","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req2.Header.Set("Authorization", "Bearer sk-test")
	req2.Header.Set("Content-Type", "application/json")
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("第二次流式请求失败: %d %s", rec2.Code, rec2.Body.String())
	}
	if up.streamOptsCalls != 1 {
		t.Fatalf("第二次请求不应再注入 stream_options，实际 streamOptsCalls=%d", up.streamOptsCalls)
	}
	if up.plainCalls != 2 {
		t.Fatalf("第二次请求应直接走无 stream_options 路径，实际 plainCalls=%d", up.plainCalls)
	}
}
