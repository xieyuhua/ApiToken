package gateway_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/demo1/apitoken/internal/model"
)

// ---------------------------------------------------------------------------
// /v1/messages（Anthropic Messages 协议入口）测试
// ---------------------------------------------------------------------------

// msgMock 是可自定义应答的 mock 上游。
type msgMock struct {
	srv      *httptest.Server
	lastBody atomic.Value // 上游收到的 OpenAI 请求体
}

func newMsgMock(respond func(w http.ResponseWriter, payload map[string]any)) *msgMock {
	m := &msgMock{}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)
		m.lastBody.Store(payload)
		respond(w, payload)
	}))
	return m
}

func (m *msgMock) upstream() map[string]any {
	v, _ := m.lastBody.Load().(map[string]any)
	return v
}

// sseChunks 以 SSE 形式输出若干 OpenAI 数据块。
func sseChunks(w http.ResponseWriter, chunks ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	for _, c := range chunks {
		fmt.Fprint(w, "data: "+c+"\n\n")
		if fl != nil {
			fl.Flush()
		}
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	if fl != nil {
		fl.Flush()
	}
}

// jsonChunks 生成 OpenAI 流式文本增量数据块。
func jsonChunks(text ...string) []string {
	out := make([]string, 0, len(text))
	for _, t := range text {
		out = append(out, fmt.Sprintf(
			`{"id":"chatcmpl-x","choices":[{"index":0,"delta":{"content":%q},"finish_reason":""}]}`, t))
	}
	return out
}

type evt struct {
	Name string
	Data map[string]any
}

// parseSSE 解析 Anthropic 事件序列。
func parseSSE(t *testing.T, body string) []evt {
	t.Helper()
	var out []evt
	for _, block := range strings.Split(body, "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		e := evt{}
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				e.Name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &e.Data); err != nil {
					t.Fatalf("事件 data 不是合法 JSON: %v\n%s", err, block)
				}
			}
		}
		if e.Name != "" {
			out = append(out, e)
		}
	}
	return out
}

func names(evs []evt) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.Name
	}
	return out
}

func postMessages(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	// Anthropic 客户端用 x-api-key，这里同时验证该鉴权路径
	req.Header.Set("x-api-key", "sk-test")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func msgChannel(m *msgMock) model.Channel {
	return model.Channel{
		ID: "m1", Name: "mock", BaseURL: m.srv.URL + "/v1", APIKey: "k",
		Models: []string{"claude-test"}, Priority: 1, Weight: 1, Enabled: true,
	}
}

// TestMessagesNonStream 覆盖基础对话：顶层 system 转成 system 消息，响应转回 Anthropic 结构。
func TestMessagesNonStream(t *testing.T) {
	mock := newMsgMock(func(w http.ResponseWriter, _ map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-abc","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":3}}`))
	})
	defer mock.srv.Close()
	h := newTestGateway(t, []model.Channel{msgChannel(mock)})

	rec := postMessages(t, h, `{"model":"claude-test","max_tokens":100,
		"system":"你是助手","messages":[{"role":"user","content":"ping"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, body = %s", rec.Code, rec.Body.String())
	}

	// 上游必须收到 OpenAI 结构，且 system 被提到 messages 首位
	up := mock.upstream()
	msgs, _ := up["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("上游 messages = %v, 期望 2 条", up["messages"])
	}
	if m0, _ := msgs[0].(map[string]any); m0["role"] != "system" || m0["content"] != "你是助手" {
		t.Fatalf("system 未转成首条 system 消息: %v", msgs[0])
	}
	if up["max_tokens"] != float64(100) {
		t.Fatalf("max_tokens 未透传: %v", up["max_tokens"])
	}

	// 响应必须是 Anthropic 结构，且模型名用客户端请求的名字
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if out["type"] != "message" || out["role"] != "assistant" {
		t.Fatalf("响应结构不对: %v", out)
	}
	if !strings.HasPrefix(fmt.Sprint(out["id"]), "msg_") {
		t.Fatalf("id 应为 msg_ 前缀: %v", out["id"])
	}
	if out["model"] != "claude-test" {
		t.Fatalf("回包模型名应为客户端请求的名字，实际 %v", out["model"])
	}
	if out["stop_reason"] != "end_turn" {
		t.Fatalf("stop_reason = %v, 期望 end_turn", out["stop_reason"])
	}
	content, _ := out["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content = %v", out["content"])
	}
	if blk, _ := content[0].(map[string]any); blk["type"] != "text" || blk["text"] != "pong" {
		t.Fatalf("content[0] = %v", content[0])
	}
	usage, _ := out["usage"].(map[string]any)
	if usage["input_tokens"] != float64(11) || usage["output_tokens"] != float64(3) {
		t.Fatalf("usage = %v", usage)
	}
}

// TestMessagesStream 校验 Anthropic 事件序列的顺序与完整性。
func TestMessagesStream(t *testing.T) {
	mock := newMsgMock(func(w http.ResponseWriter, _ map[string]any) {
		sseChunks(w, jsonChunks("你", "好")...)
	})
	defer mock.srv.Close()
	h := newTestGateway(t, []model.Channel{msgChannel(mock)})

	rec := postMessages(t, h, `{"model":"claude-test","max_tokens":50,"stream":true,
		"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, body = %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q", ct)
	}

	evs := parseSSE(t, rec.Body.String())
	// 顺序必须是 message_start → content_block_start → delta… → content_block_stop
	// → message_delta → message_stop
	want := []string{"message_start", "content_block_start", "content_block_delta",
		"content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	if len(evs) != len(want) {
		t.Fatalf("事件序列 = %v, 期望 %v", names(evs), want)
	}
	for i, wnt := range want {
		if evs[i].Name != wnt {
			t.Fatalf("第 %d 个事件 = %s, 期望 %s（完整序列 %v）", i, evs[i].Name, wnt, names(evs))
		}
	}

	// 正文增量应能拼回原文
	var text string
	for _, e := range evs {
		if e.Name != "content_block_delta" {
			continue
		}
		d, _ := e.Data["delta"].(map[string]any)
		if d["type"] == "text_delta" {
			text += fmt.Sprint(d["text"])
		}
	}
	if text != "你好" {
		t.Fatalf("正文 = %q, 期望 你好", text)
	}

	// message_delta 必须带 stop_reason 与用量
	md := evs[len(evs)-2].Data
	if delta, _ := md["delta"].(map[string]any); delta["stop_reason"] != "end_turn" {
		t.Fatalf("message_delta 缺 stop_reason: %v", md)
	}
	if _, ok := md["usage"]; !ok {
		t.Fatalf("message_delta 缺 usage: %v", md)
	}
	// 流式必须强制开启 include_usage，否则 token 统计恒为 0
	if _, ok := mock.upstream()["stream_options"]; !ok {
		t.Fatal("Anthropic 流式未强制注入 stream_options.include_usage")
	}
}

// TestMessagesStreamNoUsage 模拟上游未回 usage 时不应报错。
func TestMessagesStreamNoUsage(t *testing.T) {
	mock := newMsgMock(func(w http.ResponseWriter, _ map[string]any) {
		sseChunks(w, `{"id":"1","choices":[{"index":0,"delta":{"content":"hi"}}]}`)
	})
	defer mock.srv.Close()
	h := newTestGateway(t, []model.Channel{msgChannel(mock)})

	rec := postMessages(t, h, `{"model":"claude-test","max_tokens":50,"stream":true,
		"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", rec.Code)
	}
	evs := parseSSE(t, rec.Body.String())
	if evs[len(evs)-1].Name != "message_stop" {
		t.Fatalf("缺 message_stop: %v", names(evs))
	}
}

// TestMessagesTools 覆盖工具调用：请求方向 input_schema → parameters，
// 响应方向 tool_calls → tool_use 块。
func TestMessagesTools(t *testing.T) {
	mock := newMsgMock(func(w http.ResponseWriter, _ map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-t","choices":[{"index":0,"message":{"role":"assistant",
			"content":"","tool_calls":[{"id":"call_1","type":"function",
			"function":{"name":"get_weather","arguments":"{\"city\":\"上海\"}"}}]},
			"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`))
	})
	defer mock.srv.Close()
	h := newTestGateway(t, []model.Channel{msgChannel(mock)})

	rec := postMessages(t, h, `{"model":"claude-test","max_tokens":100,
		"tools":[{"name":"get_weather","description":"查天气",
			"input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}],
		"tool_choice":{"type":"auto"},
		"messages":[{"role":"user","content":"上海天气"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, body = %s", rec.Code, rec.Body.String())
	}

	// 请求方向
	tools, _ := mock.upstream()["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v", mock.upstream()["tools"])
	}
	t0, _ := tools[0].(map[string]any)
	fn, _ := t0["function"].(map[string]any)
	if t0["type"] != "function" || fn["name"] != "get_weather" {
		t.Fatalf("工具未转成 OpenAI function: %v", tools[0])
	}
	if _, ok := fn["parameters"]; !ok {
		t.Fatalf("input_schema 未映射为 parameters: %v", fn)
	}
	if mock.upstream()["tool_choice"] != "auto" {
		t.Fatalf("tool_choice = %v", mock.upstream()["tool_choice"])
	}

	// 响应方向
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["stop_reason"] != "tool_use" {
		t.Fatalf("stop_reason = %v, 期望 tool_use", out["stop_reason"])
	}
	content, _ := out["content"].([]any)
	var tu map[string]any
	for _, c := range content {
		if m, _ := c.(map[string]any); m["type"] == "tool_use" {
			tu = m
		}
	}
	if tu == nil {
		t.Fatalf("缺少 tool_use 块: %v", content)
	}
	if tu["id"] != "call_1" || tu["name"] != "get_weather" {
		t.Fatalf("tool_use 字段不对: %v", tu)
	}
	input, _ := tu["input"].(map[string]any)
	if input["city"] != "上海" {
		t.Fatalf("tool_use.input 未把 arguments 解析成对象: %v", tu["input"])
	}
}

// TestMessagesToolResultRoundTrip 覆盖客户端回传工具结果时的拆包规则：
// tool_result 必须变成独立的 role=tool 消息，且排在同消息的正文之前。
func TestMessagesToolResultRoundTrip(t *testing.T) {
	mock := newMsgMock(func(w http.ResponseWriter, _ map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"1","choices":[{"index":0,"message":{"role":"assistant","content":"好的"},"finish_reason":"stop"}]}`))
	})
	defer mock.srv.Close()
	h := newTestGateway(t, []model.Channel{msgChannel(mock)})

	rec := postMessages(t, h, `{"model":"claude-test","max_tokens":100,
		"messages":[
			{"role":"user","content":"查天气"},
			{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"get_weather","input":{"city":"上海"}}]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"call_1","content":"晴 25℃"},
				{"type":"text","text":"谢谢"}]}
		]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, body = %s", rec.Code, rec.Body.String())
	}

	msgs, _ := mock.upstream()["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("messages = %d 条, 期望 4: %v", len(msgs), msgs)
	}
	// [user, assistant(tool_calls), tool, user]
	m2, _ := msgs[2].(map[string]any)
	if m2["role"] != "tool" || m2["tool_call_id"] != "call_1" || m2["content"] != "晴 25℃" {
		t.Fatalf("tool 消息不对: %v", msgs[2])
	}
	m3, _ := msgs[3].(map[string]any)
	if m3["role"] != "user" || m3["content"] != "谢谢" {
		t.Fatalf("tool_result 之后的正文不对: %v", msgs[3])
	}
	m1, _ := msgs[1].(map[string]any)
	tcs, _ := m1["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("assistant 的 tool_use 未转成 tool_calls: %v", msgs[1])
	}
}

// TestMessagesThinking 覆盖思维链：开启时输出 thinking 块（且排在正文之前），
// 未开启时必须丢弃（Anthropic 协议不允许未请求就发 thinking 块）。
func TestMessagesThinking(t *testing.T) {
	mock := newMsgMock(func(w http.ResponseWriter, _ map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"1","choices":[{"index":0,"message":{"role":"assistant",
			"reasoning_content":"先想一下","content":"答案"},"finish_reason":"stop"}]}`))
	})
	defer mock.srv.Close()
	h := newTestGateway(t, []model.Channel{msgChannel(mock)})

	// 开启 thinking
	rec := postMessages(t, h, `{"model":"claude-test","max_tokens":100,
		"thinking":{"type":"enabled","budget_tokens":50},
		"messages":[{"role":"user","content":"hi"}]}`)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	content, _ := out["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("开启 thinking 后应有 2 个块: %v", content)
	}
	if b0, _ := content[0].(map[string]any); b0["type"] != "thinking" || b0["thinking"] != "先想一下" {
		t.Fatalf("thinking 块必须排在正文之前: %v", content)
	}

	// 未开启 thinking → 必须丢弃
	rec2 := postMessages(t, h, `{"model":"claude-test","max_tokens":100,
		"messages":[{"role":"user","content":"hi"}]}`)
	var out2 map[string]any
	_ = json.Unmarshal(rec2.Body.Bytes(), &out2)
	content2, _ := out2["content"].([]any)
	if len(content2) != 1 {
		t.Fatalf("未开启 thinking 时不应出现 thinking 块: %v", content2)
	}
	if b0, _ := content2[0].(map[string]any); b0["type"] != "text" {
		t.Fatalf("content[0] = %v", content2[0])
	}
}

// TestMessagesThinkingStream 校验流式 thinking 事件序列。
func TestMessagesThinkingStream(t *testing.T) {
	mock := newMsgMock(func(w http.ResponseWriter, _ map[string]any) {
		sseChunks(w,
			`{"id":"1","choices":[{"index":0,"delta":{"reasoning_content":"想"}}]}`,
			`{"id":"1","choices":[{"index":0,"delta":{"content":"答"}}]}`,
		)
	})
	defer mock.srv.Close()
	h := newTestGateway(t, []model.Channel{msgChannel(mock)})

	rec := postMessages(t, h, `{"model":"claude-test","max_tokens":100,"stream":true,
		"thinking":{"type":"enabled","budget_tokens":50},
		"messages":[{"role":"user","content":"hi"}]}`)
	evs := parseSSE(t, rec.Body.String())
	// 期望：message_start → thinking 块 start/delta → (stop) → text 块 start/delta → …
	if len(evs) < 6 {
		t.Fatalf("事件过少: %v", names(evs))
	}
	if d, _ := evs[1].Data["content_block"].(map[string]any); d["type"] != "thinking" {
		t.Fatalf("首个块应是 thinking: %v", evs[1].Data)
	}
	if evs[2].Name != "content_block_delta" {
		t.Fatalf("第 3 个事件 = %s", evs[2].Name)
	}
	if evs[3].Name != "content_block_stop" {
		t.Fatalf("块切换缺少 content_block_stop: %v", names(evs))
	}
	if d, _ := evs[4].Data["content_block"].(map[string]any); d["type"] != "text" {
		t.Fatalf("第二个块应是 text: %v", evs[4].Data)
	}
}

// TestMessagesImage 覆盖多模态：base64 图片 → OpenAI image_url data URI。
func TestMessagesImage(t *testing.T) {
	mock := newMsgMock(func(w http.ResponseWriter, _ map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"1","choices":[{"index":0,"message":{"role":"assistant","content":"图片里是猫"},"finish_reason":"stop"}]}`))
	})
	defer mock.srv.Close()
	h := newTestGateway(t, []model.Channel{msgChannel(mock)})

	rec := postMessages(t, h, `{"model":"claude-test","max_tokens":100,
		"messages":[{"role":"user","content":[
			{"type":"text","text":"这是什么"},
			{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"/9j/4AAQ"}}
		]}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, body = %s", rec.Code, rec.Body.String())
	}
	msgs, _ := mock.upstream()["messages"].([]any)
	m0, _ := msgs[0].(map[string]any)
	parts, _ := m0["content"].([]any)
	if len(parts) != 2 {
		t.Fatalf("多模态 content = %v", parts)
	}
	img, _ := parts[1].(map[string]any)
	if img["type"] != "image_url" {
		t.Fatalf("图片块未转成 image_url: %v", img)
	}
	iu, _ := img["image_url"].(map[string]any)
	if fmt.Sprint(iu["url"]) != "data:image/jpeg;base64,/9j/4AAQ" {
		t.Fatalf("data URI 拼接错误: %v", iu["url"])
	}
}

// TestMessagesStrictValidation 覆盖严格校验：不支持的字段必须明确 400，
// 而不是静默丢弃后让调用方误判能力生效。
func TestMessagesStrictValidation(t *testing.T) {
	mock := newMsgMock(func(w http.ResponseWriter, _ map[string]any) {
		t.Error("校验失败的请求不应发往上游")
		w.WriteHeader(http.StatusOK)
	})
	defer mock.srv.Close()
	h := newTestGateway(t, []model.Channel{msgChannel(mock)})

	cases := []struct {
		name string
		body string
		want string
	}{
		{"不支持的顶层字段", `{"model":"claude-test","max_tokens":10,"service_tier":"x",
			"messages":[{"role":"user","content":"hi"}]}`, "service_tier"},
		{"未知内容块", `{"model":"claude-test","max_tokens":10,
			"messages":[{"role":"user","content":[{"type":"server_tool_use","id":"a"}]}]}`, "server_tool_use"},
		{"缺 max_tokens", `{"model":"claude-test","messages":[{"role":"user","content":"hi"}]}`, "max_tokens"},
		{"document 块不支持", `{"model":"claude-test","max_tokens":10,
			"messages":[{"role":"user","content":[{"type":"document","source":{}}]}]}`, "document"},
		{"tool_result 放错角色", `{"model":"claude-test","max_tokens":10,
			"messages":[{"role":"assistant","content":[{"type":"tool_result","tool_use_id":"a","content":"x"}]}]}`, "tool_result"},
		{"thinking 预算超限", `{"model":"claude-test","max_tokens":10,
			"thinking":{"type":"enabled","budget_tokens":50},
			"messages":[{"role":"user","content":"hi"}]}`, "budget_tokens"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := postMessages(t, h, c.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("状态码 = %d, 期望 400; body = %s", rec.Code, rec.Body.String())
			}
			// 必须是 Anthropic 错误结构，否则客户端会显示成未知错误
			var e struct {
				Type  string `json:"type"`
				Error struct {
					Type    string `json:"type"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
				t.Fatalf("错误体不是 JSON: %v (%s)", err, rec.Body.String())
			}
			if e.Type != "error" || e.Error.Type != "invalid_request_error" {
				t.Fatalf("错误结构不对: %+v", e)
			}
			if !strings.Contains(e.Error.Message, c.want) {
				t.Fatalf("错误信息应指出问题字段 %q，实际: %s", c.want, e.Error.Message)
			}
		})
	}
}

// TestMessagesAuth 校验鉴权：Anthropic 客户端用 x-api-key，也支持 Bearer。
func TestMessagesAuth(t *testing.T) {
	mock := newMsgMock(func(w http.ResponseWriter, _ map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"1","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	})
	defer mock.srv.Close()
	h := newTestGateway(t, []model.Channel{msgChannel(mock)})
	body := `{"model":"claude-test","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`

	// x-api-key
	if rec := postMessages(t, h, body); rec.Code != http.StatusOK {
		t.Fatalf("x-api-key 鉴权失败: %d %s", rec.Code, rec.Body.String())
	}
	// Bearer
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-test")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("Bearer 鉴权失败: %d", rec.Code)
	}
	// 错误密钥
	req = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("x-api-key", "wrong")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("错误密钥应返回 401，实际 %d", rec.Code)
	}
}

// TestMessagesFailover 校验 Anthropic 端点同样享有故障转移。
func TestMessagesFailover(t *testing.T) {
	bad := newMsgMock(func(w http.ResponseWriter, _ map[string]any) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited","type":"rate_limit_error"}}`))
	})
	defer bad.srv.Close()
	good := newMsgMock(func(w http.ResponseWriter, _ map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"1","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	})
	defer good.srv.Close()

	h := newTestGateway(t, []model.Channel{
		{ID: "bad", Name: "限流", BaseURL: bad.srv.URL + "/v1", APIKey: "k",
			Models: []string{"claude-test"}, Priority: 1, Weight: 1, Enabled: true},
		{ID: "good", Name: "正常", BaseURL: good.srv.URL + "/v1", APIKey: "k",
			Models: []string{"claude-test"}, Priority: 2, Weight: 1, Enabled: true},
	})

	rec := postMessages(t, h, `{"model":"claude-test","max_tokens":10,
		"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("故障转移后应成功: %d %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Gateway-Channel"); got != "good" {
		t.Fatalf("应命中第二个渠道，实际 %q", got)
	}
}

// TestMessagesUpstreamErrorFormat 校验上游报错时按 Anthropic 结构回包。
func TestMessagesUpstreamErrorFormat(t *testing.T) {
	mock := newMsgMock(func(w http.ResponseWriter, _ map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid api key","type":"auth"}}`))
	})
	defer mock.srv.Close()
	h := newTestGateway(t, []model.Channel{msgChannel(mock)})

	rec := postMessages(t, h, `{"model":"claude-test","max_tokens":10,
		"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("状态码 = %d, 期望透传 401", rec.Code)
	}
	var e struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("错误体不是 JSON: %s", rec.Body.String())
	}
	if e.Type != "error" || e.Error.Type != "authentication_error" {
		t.Fatalf("401 应映射为 authentication_error: %+v", e)
	}
	if !strings.Contains(e.Error.Message, "invalid api key") {
		t.Fatalf("应保留上游错误信息: %s", e.Error.Message)
	}
}

// TestMessagesModelNotFound 校验模型不存在时也用 Anthropic 结构报错。
func TestMessagesModelNotFound(t *testing.T) {
	mock := newMsgMock(func(w http.ResponseWriter, _ map[string]any) {
		t.Error("无候选渠道时不应发往上游")
	})
	defer mock.srv.Close()
	h := newTestGateway(t, []model.Channel{msgChannel(mock)})

	rec := postMessages(t, h, `{"model":"not-exist","max_tokens":10,
		"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("状态码 = %d, 期望 404", rec.Code)
	}
	var e struct {
		Type  string `json:"type"`
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	if e.Type != "error" || e.Error.Type != "not_found_error" {
		t.Fatalf("404 应映射为 not_found_error: %s", rec.Body.String())
	}
}
