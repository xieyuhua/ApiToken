package gateway_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xieyuhua/apitoken/internal/model"
)

// ---------------------------------------------------------------------------
// /v1/responses（OpenAI Responses 协议入口，Codex CLI 使用）测试
// ---------------------------------------------------------------------------

func respChannel(m *msgMock) model.Channel {
	return model.Channel{
		ID: "r1", Name: "mock", BaseURL: m.srv.URL + "/v1", APIKey: "k",
		Models: []string{"resp-test"}, Priority: 1, Weight: 1, Enabled: true,
	}
}

func postResponses(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestResponsesNonStream 覆盖基础对话：input 字符串转 user、instructions 转 system，
// 响应转回 Responses 结构。
func TestResponsesNonStream(t *testing.T) {
	mock := newMsgMock(func(w http.ResponseWriter, _ map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-abc","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":3}}`))
	})
	defer mock.srv.Close()
	h := newTestGateway(t, []model.Channel{respChannel(mock)})

	rec := postResponses(t, h, `{"model":"resp-test","input":"ping","instructions":"你是助手"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, body = %s", rec.Code, rec.Body.String())
	}

	// 上游必须收到 OpenAI 结构
	up := mock.upstream()
	msgs, _ := up["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("上游 messages = %v, 期望 2 条", up["messages"])
	}
	if m0, _ := msgs[0].(map[string]any); m0["role"] != "system" || m0["content"] != "你是助手" {
		t.Fatalf("instructions 未转成首条 system: %v", msgs[0])
	}
	if m1, _ := msgs[1].(map[string]any); m1["role"] != "user" || m1["content"] != "ping" {
		t.Fatalf("input 字符串未转成 user: %v", msgs[1])
	}

	// 响应必须是 Responses 结构
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if out["object"] != "response" {
		t.Fatalf("object = %v, 期望 response", out["object"])
	}
	if !strings.HasPrefix(fmt.Sprint(out["id"]), "resp_") {
		t.Fatalf("id 应为 resp_ 前缀: %v", out["id"])
	}
	if out["model"] != "resp-test" {
		t.Fatalf("回包模型名应为客户端请求的名字，实际 %v", out["model"])
	}
	output, _ := out["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("output = %v", out["output"])
	}
	item, _ := output[0].(map[string]any)
	if item["type"] != "message" || item["role"] != "assistant" {
		t.Fatalf("output[0] = %v", item)
	}
	content, _ := item["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content = %v", content)
	}
	if c0, _ := content[0].(map[string]any); c0["type"] != "output_text" || c0["text"] != "pong" {
		t.Fatalf("content[0] = %v", content[0])
	}
	usage, _ := out["usage"].(map[string]any)
	if usage["input_tokens"] != float64(11) || usage["output_tokens"] != float64(3) {
		t.Fatalf("usage = %v", usage)
	}
}

// TestResponsesTools 覆盖工具调用：Responses function 工具转成 OpenAI function，
// 响应里的 tool_calls 转成 function_call 输出项。
func TestResponsesTools(t *testing.T) {
	mock := newMsgMock(func(w http.ResponseWriter, _ map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-t","choices":[{"index":0,"message":{"role":"assistant",
			"content":"","tool_calls":[{"id":"call_1","type":"function",
			"function":{"name":"get_weather","arguments":"{\"city\":\"上海\"}"}}]},
			"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`))
	})
	defer mock.srv.Close()
	h := newTestGateway(t, []model.Channel{respChannel(mock)})

	rec := postResponses(t, h, `{"model":"resp-test",
		"tools":[{"type":"function","name":"get_weather","description":"查天气",
			"parameters":{"type":"object","properties":{"city":{"type":"string"}}}}],
		"input":"上海天气"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, body = %s", rec.Code, rec.Body.String())
	}

	// 请求方向：Responses 扁平工具 → OpenAI function
	tools, _ := mock.upstream()["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v", mock.upstream()["tools"])
	}
	t0, _ := tools[0].(map[string]any)
	fn, _ := t0["function"].(map[string]any)
	if t0["type"] != "function" || fn["name"] != "get_weather" {
		t.Fatalf("工具未转成 OpenAI function: %v", tools[0])
	}

	// 响应方向：function_call 输出项
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	output, _ := out["output"].([]any)
	var fc map[string]any
	for _, o := range output {
		if m, _ := o.(map[string]any); m["type"] == "function_call" {
			fc = m
		}
	}
	if fc == nil {
		t.Fatalf("缺少 function_call 输出项: %v", output)
	}
	if fc["call_id"] != "call_1" || fc["name"] != "get_weather" {
		t.Fatalf("function_call 字段不对: %v", fc)
	}
	if !strings.Contains(fmt.Sprint(fc["arguments"]), `"city"`) {
		t.Fatalf("arguments 应保留 JSON: %v", fc["arguments"])
	}
}

// TestResponsesReasoning 覆盖推理：请求带 reasoning 时输出 reasoning 项（排最前），
// 未请求时必须丢弃。
func TestResponsesReasoning(t *testing.T) {
	mock := newMsgMock(func(w http.ResponseWriter, _ map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"1","choices":[{"index":0,"message":{"role":"assistant",
			"reasoning_content":"先想一下","content":"答案"},"finish_reason":"stop"}]}`))
	})
	defer mock.srv.Close()
	h := newTestGateway(t, []model.Channel{respChannel(mock)})

	// 开启 reasoning
	rec := postResponses(t, h, `{"model":"resp-test","input":"hi","reasoning":{"effort":"medium"}}`)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	output, _ := out["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("开启 reasoning 后应有 2 个输出项: %v", output)
	}
	if o0, _ := output[0].(map[string]any); o0["type"] != "reasoning" {
		t.Fatalf("reasoning 项应排在最前: %v", output)
	}
	if o1, _ := output[1].(map[string]any); o1["type"] != "message" {
		t.Fatalf("第二个输出项应是 message: %v", output[1])
	}
	if got := mock.upstream()["reasoning_effort"]; got != "medium" {
		t.Fatalf("reasoning.effort 未映射为 reasoning_effort: %v", mock.upstream())
	}

	// 未请求 reasoning → 必须丢弃
	rec2 := postResponses(t, h, `{"model":"resp-test","input":"hi"}`)
	var out2 map[string]any
	_ = json.Unmarshal(rec2.Body.Bytes(), &out2)
	output2, _ := out2["output"].([]any)
	if len(output2) != 1 {
		t.Fatalf("未请求 reasoning 时不应有 reasoning 项: %v", output2)
	}
	if o0, _ := output2[0].(map[string]any); o0["type"] != "message" {
		t.Fatalf("output[0] = %v", output2[0])
	}
}

// TestResponsesStream 校验流式事件序列顺序与正文拼接。
func TestResponsesStream(t *testing.T) {
	mock := newMsgMock(func(w http.ResponseWriter, _ map[string]any) {
		sseChunks(w, jsonChunks("你", "好")...)
	})
	defer mock.srv.Close()
	h := newTestGateway(t, []model.Channel{respChannel(mock)})

	rec := postResponses(t, h, `{"model":"resp-test","input":"hi","stream":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, body = %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q", ct)
	}

	evs := parseSSE(t, rec.Body.String())
	want := []string{"response.created", "response.output_item.added", "response.content_part.added",
		"response.output_text.delta", "response.output_text.delta", "response.output_text.done",
		"response.content_part.done", "response.output_item.done", "response.completed"}
	if len(evs) != len(want) {
		t.Fatalf("事件序列 = %v, 期望 %v", names(evs), want)
	}
	for i, wnt := range want {
		if evs[i].Name != wnt {
			t.Fatalf("第 %d 个事件 = %s, 期望 %s（完整 %v）", i, evs[i].Name, wnt, names(evs))
		}
	}

	// 增量拼回原文
	var text string
	for _, e := range evs {
		if e.Name == "response.output_text.delta" {
			text += fmt.Sprint(e.Data["delta"])
		}
	}
	if text != "你好" {
		t.Fatalf("正文 = %q, 期望 你好", text)
	}

	// response.completed 必须带 output 与 usage
	last := evs[len(evs)-1].Data
	resp, _ := last["response"].(map[string]any)
	if resp == nil || resp["object"] != "response" {
		t.Fatalf("response.completed 缺 response 对象: %v", last)
	}
	if _, ok := resp["usage"]; !ok {
		t.Fatalf("response.completed 缺 usage: %v", resp)
	}
	// 流式必须强制开启 include_usage
	if _, ok := mock.upstream()["stream_options"]; !ok {
		t.Fatal("Responses 流式未强制注入 stream_options.include_usage")
	}
}

// TestResponsesStrictValidation 不支持的字段必须 400（OpenAI 错误结构）。
func TestResponsesStrictValidation(t *testing.T) {
	mock := newMsgMock(func(w http.ResponseWriter, _ map[string]any) {
		t.Error("校验失败的请求不应发往上游")
		w.WriteHeader(http.StatusOK)
	})
	defer mock.srv.Close()
	h := newTestGateway(t, []model.Channel{respChannel(mock)})

	cases := []struct {
		name string
		body string
		want string
	}{
		{"不支持的顶层字段", `{"model":"resp-test","input":"hi","foo":1}`, "foo"},
		{"缺 input", `{"model":"resp-test"}`, "input"},
		{"未知 input 项", `{"model":"resp-test","input":[{"type":"weird"}]}`, "weird"},
		{"非法 reasoning.effort", `{"model":"resp-test","input":"hi","reasoning":{"effort":"max"}}`, "effort"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := postResponses(t, h, c.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("状态码 = %d, 期望 400; body = %s", rec.Code, rec.Body.String())
			}
			var e struct {
				Error struct {
					Message string `json:"message"`
					Type    string `json:"type"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
				t.Fatalf("错误体不是 JSON: %s", rec.Body.String())
			}
			if e.Error.Type != "invalid_request_error" {
				t.Fatalf("错误类型 = %s, 期望 invalid_request_error", e.Error.Type)
			}
			if !strings.Contains(e.Error.Message, c.want) {
				t.Fatalf("错误信息应指出问题字段 %q，实际: %s", c.want, e.Error.Message)
			}
		})
	}
}

// TestResponsesAuth 鉴权：无密钥 401，Bearer 通过。
func TestResponsesAuth(t *testing.T) {
	mock := newMsgMock(func(w http.ResponseWriter, _ map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"1","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	})
	defer mock.srv.Close()
	h := newTestGateway(t, []model.Channel{respChannel(mock)})
	body := `{"model":"resp-test","input":"hi"}`

	if rec := postResponses(t, h, body); rec.Code != http.StatusOK {
		t.Fatalf("Bearer 鉴权失败: %d %s", rec.Code, rec.Body.String())
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer wrong")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("错误密钥应返回 401，实际 %d", rec.Code)
	}
}
