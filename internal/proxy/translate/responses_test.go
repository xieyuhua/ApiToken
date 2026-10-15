package translate

import (
	"encoding/json"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 测试用例在小工具函数
// ---------------------------------------------------------------------------

// wantField 断言转换错误指向了预期字段。
func wantField(t *testing.T, err error, field string) {
	t.Helper()
	if err == nil {
		t.Fatalf("期望报错，实际成功")
	}
	e, ok := err.(*Error)
	if !ok {
		t.Fatalf("错误类型 = %T，期望 *Error: %v", err, err)
	}
	if e.Field != field {
		t.Fatalf("错误字段 = %q，期望 %q（错误内容：%s）", e.Field, field, e.Msg)
	}
}

// responsesOutput 是 ResponseToResponses 产出的 output 数组结构。
type responsesOutputItem struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	Role    string `json:"role"`
	Status  string `json:"status"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Summary []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"summary"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// decodeResponsesBody 解码非流式 Responses 响应体。
func decodeResponsesBody(t *testing.T, raw []byte) (map[string]any, []responsesOutputItem) {
	t.Helper()
	var body struct {
		ID     string                `json:"id"`
		Model  string                `json:"model"`
		Status string                `json:"status"`
		Output []responsesOutputItem `json:"output"`
		Usage  struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
			TotalTokens  int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("解析响应失败: %v\n%s", err, raw)
	}
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	return generic, body.Output
}

// collectResponsesStream 把一串上游 SSE data 负载喂给转换器，返回完整事件流。
func collectResponsesStream(t *testing.T, s *ResponsesStream, chunks []string) string {
	t.Helper()
	var sb strings.Builder
	sb.Write(s.Begin())
	for _, c := range chunks {
		sb.Write(s.Line(c, false))
	}
	sb.Write(s.End())
	return sb.String()
}

// responsesEventNames 按顺序提取 SSE 事件名。
func responsesEventNames(sse string) []string {
	var names []string
	for _, block := range strings.Split(sse, "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		if strings.HasPrefix(block, "event: ") {
			line := block[:strings.Index(block, "\n")]
			names = append(names, strings.TrimPrefix(line, "event: "))
		}
	}
	return names
}

// responsesEventIndex 返回某事件名首次出现的位置，-1 表示没出现。
func responsesEventIndex(names []string, want string) int {
	for i, n := range names {
		if n == want {
			return i
		}
	}
	return -1
}

// ---------------------------------------------------------------------------
// 请求方向：Responses → Chat Completions
// ---------------------------------------------------------------------------

func TestResponsesRequestBasics(t *testing.T) {
	raw := `{"model":"m1","instructions":"系统提示","input":"你好",
		"max_output_tokens":64,"temperature":0.3,"top_p":0.9}`

	payload, info, err := RequestToChatCompletions([]byte(raw))
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	if info.Model != "m1" {
		t.Fatalf("info.Model = %q", info.Model)
	}
	if !strings.HasPrefix(info.ResponseID, "resp_") {
		t.Fatalf("info.ResponseID = %q", info.ResponseID)
	}
	if info.WantReasoning {
		t.Fatal("未传 reasoning 不应标记 WantReasoning")
	}
	if payload["model"] != "m1" {
		t.Fatalf("model = %v", payload["model"])
	}
	if payload["max_tokens"] != 64 {
		t.Fatalf("max_output_tokens 未转成 max_tokens: %v", payload["max_tokens"])
	}
	if payload["temperature"] != 0.3 || payload["top_p"] != 0.9 {
		t.Fatalf("采样参数未透传: %v", payload)
	}

	msgs := msgsOf(t, payload)
	if len(msgs) != 2 {
		t.Fatalf("messages = %d 条，期望 system + user", len(msgs))
	}
	if m := msgAt(t, msgs, 0); m["role"] != "system" || m["content"] != "系统提示" {
		t.Fatalf("instructions 未变成首条 system: %v", m)
	}
	if m := msgAt(t, msgs, 1); m["role"] != "user" || m["content"] != "你好" {
		t.Fatalf("字符串 input 未变成 user 消息: %v", m)
	}
}

func TestResponsesRequestInputItems(t *testing.T) {
	raw := `{"model":"m1","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"第一段"},{"type":"input_text","text":"第二段"}]},
		{"type":"message","role":"developer","content":"约束条件"},
		{"type":"message","role":"assistant","content":[
			{"type":"reasoning","summary":[{"type":"summary_text","text":"想了一下"}]},
			{"type":"output_text","text":"回答"},
			{"type":"function_call","call_id":"c1","name":"weather","arguments":"{\"city\":\"上海\"}"}]},
		{"type":"function_call_output","call_id":"c1","output":"晴 25℃"},
		{"type":"function_call","call_id":"c2","name":"time","arguments":"{}"}
	]}`

	payload, _, err := RequestToChatCompletions([]byte(raw))
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	msgs := msgsOf(t, payload)
	if len(msgs) != 5 {
		t.Fatalf("messages = %d 条，期望 5 条", len(msgs))
	}

	// 单个纯文本块应塌缩成字符串 content，多块才是 content 数组
	if m := msgAt(t, msgs, 0); m["role"] != "user" {
		t.Fatalf("第一条应为 user: %v", m)
	} else if _, ok := m["content"].(string); ok {
		t.Fatalf("两个文本块应保持数组形态: %v", m["content"])
	} else if parts, ok := m["content"].([]any); !ok || len(parts) != 2 {
		t.Fatalf("content 应为 2 段: %v", m["content"])
	}

	if m := msgAt(t, msgs, 1); m["role"] != "developer" || m["content"] != "约束条件" {
		t.Fatalf("developer 消息转换有误: %v", m)
	}

	asst := msgAt(t, msgs, 2)
	if asst["role"] != "assistant" {
		t.Fatalf("第三条应为 assistant: %v", asst)
	}
	if asst["reasoning_content"] != "想了一下" {
		t.Fatalf("reasoning 块未转成 reasoning_content: %v", asst)
	}
	if asst["content"] != "回答" {
		t.Fatalf("assistant 正文有误: %v", asst)
	}
	calls, ok := asst["tool_calls"].([]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("tool_calls 缺失: %v", asst)
	}
	call, _ := calls[0].(map[string]any)
	if call["id"] != "c1" {
		t.Fatalf("tool_call.id = %v", call["id"])
	}
	if fn, _ := call["function"].(map[string]any); fn["name"] != "weather" {
		t.Fatalf("tool_call function = %v", fn)
	}

	// function_call_output → role=tool
	tool := msgAt(t, msgs, 3)
	if tool["role"] != "tool" || tool["tool_call_id"] != "c1" || tool["content"] != "晴 25℃" {
		t.Fatalf("function_call_output 转换有误: %v", tool)
	}

	// 独立 function_call item 也必须是 assistant 的 tool_calls
	standalone := msgAt(t, msgs, 4)
	if standalone["role"] != "assistant" {
		t.Fatalf("独立 function_call 应为 assistant: %v", standalone)
	}
	if _, ok := standalone["tool_calls"].([]any); !ok {
		t.Fatalf("独立 function_call 未转成 tool_calls: %v", standalone)
	}
}

func TestResponsesRequestDroppableAndUnsupportedFields(t *testing.T) {
	// 可丢弃字段：不影响结果，但要能被观测到
	raw := `{"model":"m1","input":"你好","store":false,"metadata":{"k":"v"},"service_tier":"auto"}`
	payload, info, err := RequestToChatCompletions([]byte(raw))
	if err != nil {
		t.Fatalf("可丢弃字段不应报错: %v", err)
	}
	if len(msgsOf(t, payload)) != 1 {
		t.Fatalf("messages 条数异常: %v", payload["messages"])
	}
	dropped := strings.Join(info.Dropped, ",")
	for _, want := range []string{"store", "metadata", "service_tier"} {
		if !strings.Contains(dropped, want) {
			t.Fatalf("%s 未记入 Dropped: %q", want, dropped)
		}
	}

	// 未知字段必须在入口拒绝，避免「静默忽略」被当成「已支持」
	_, _, err = RequestToChatCompletions([]byte(`{"model":"m1","input":"你好","response_format":{"type":"json_object"}}`))
	wantField(t, err, "response_format")
}

func TestResponsesRequestInputValidation(t *testing.T) {
	cases := []struct{ raw, field string }{
		{`{"model":"m1"}`, "input"},
		{`{"model":"","input":"你好"}`, "model"},
		{`{"model":"m1","input":""}`, "input"},
		{`{"model":"m1","input":[]}`, "input"},
		{`{"model":"m1","input":"你好","instructions":42}`, "instructions"},
		{`{"model":"m1","input":[{"type":"message","content":"你好"}]}`, "input[0].role"},
		{`{"model":"m1","input":[{"type":"message","role":"guest","content":"你好"}]}`, "input[0].role"},
		{`{"model":"m1","input":[{"type":"audio","role":"user"}]}`, "input[0].type"},
		{`{"model":"m1","input":[{"type":"function_call_output","output":"x"}]}`, "input[0].call_id"},
	}
	for _, c := range cases {
		_, _, err := RequestToChatCompletions([]byte(c.raw))
		wantField(t, err, c.field)
	}
}

func TestResponsesRequestTools(t *testing.T) {
	raw := `{"model":"m1","input":"查天气","tools":[
		{"type":"function","name":"weather","description":"查天气","strict":true,
		 "parameters":{"type":"object","properties":{"city":{"type":"string"}}}},
		{"type":"function","name":"time"}],
		"tool_choice":"required"}`

	payload, _, err := RequestToChatCompletions([]byte(raw))
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	tools, ok := payload["tools"].([]any)
	if !ok || len(tools) != 2 {
		t.Fatalf("tools = %v", payload["tools"])
	}
	first, _ := tools[0].(map[string]any)
	fn, _ := first["function"].(map[string]any)
	if first["type"] != "function" || fn["name"] != "weather" || fn["description"] != "查天气" {
		t.Fatalf("工具结构有误: %v", first)
	}
	if fn["strict"] != true {
		t.Fatalf("strict 未透传: %v", fn)
	}
	// 缺 parameters 时必须补齐空 schema，否则部分上游会报 schema 缺失。
	// 注意 parameters 是 json.RawMessage（保留上游原文），不是已经解码的 map。
	second, _ := tools[1].(map[string]any)
	fn2, _ := second["function"].(map[string]any)
	schema, ok := fn2["parameters"].(json.RawMessage)
	if !ok || !strings.Contains(string(schema), `"type":"object"`) {
		t.Fatalf("缺 parameters 未补默认 schema: %v", fn2["parameters"])
	}
	if payload["tool_choice"] != "required" {
		t.Fatalf("tool_choice = %v", payload["tool_choice"])
	}

	// 只在有 tools 时才允许带 tool_choice：无 tools 时不往 payload 里塞，避免上游报「未定义的工具」
	onlyChoice, _, err := RequestToChatCompletions([]byte(`{"model":"m1","input":"hi","tool_choice":"none"}`))
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	if _, exists := onlyChoice["tool_choice"]; exists {
		t.Fatalf("无 tools 时不应输出 tool_choice: %v", onlyChoice)
	}

	if _, _, err := RequestToChatCompletions([]byte(
		`{"model":"m1","input":"hi","tools":[{"type":"file_search","name":"fs"}]}`)); err == nil {
		t.Fatal("非 function 工具应报错")
	} else {
		wantField(t, err, "tools[0].type")
	}
	if _, _, err := RequestToChatCompletions([]byte(
		`{"model":"m1","input":"hi","tools":[{"type":"function","name":" "}]}`)); err == nil {
		t.Fatal("空工具名应报错")
	} else {
		wantField(t, err, "tools[0].name")
	}
}

func TestResponsesRequestToolChoiceMapping(t *testing.T) {
	const toolArgs = `"tools":[{"type":"function","name":"weather"}]`
	cases := []struct {
		choice string
		want   string // 期望 parsed tool_choice 的 JSON 片段
		field  string // 期望报错的字段，空表示不报错
	}{
		{`"auto"`, `"auto"`, ""},
		{`"none"`, `"none"`, ""},
		{`"required"`, `"required"`, ""},
		{`{"type":"function","name":"weather"}`, `"function":{"name":"weather"}`, ""},
		{`"file_search"`, "", "tool_choice"},
		{`{"type":"file_search"}`, "", "tool_choice.type"},
		{`{"type":"function"}`, "", "tool_choice.name"},
	}
	for _, c := range cases {
		raw := `{"model":"m1","input":"hi",` + toolArgs + `,"tool_choice":` + c.choice + `}`
		payload, _, err := RequestToChatCompletions([]byte(raw))
		if c.field != "" {
			wantField(t, err, c.field)
			continue
		}
		if err != nil {
			t.Fatalf("tool_choice=%s 转换失败: %v", c.choice, err)
		}
		got, err := json.Marshal(payload["tool_choice"])
		if err != nil {
			t.Fatalf("序列化失败: %v", err)
		}
		if !strings.Contains(string(got), c.want) {
			t.Fatalf("tool_choice %s → %s，期望包含 %s", c.choice, got, c.want)
		}
	}
}

func TestResponsesRequestReasoning(t *testing.T) {
	raw := `{"model":"m1","input":"推理一下","reasoning":{"effort":"high"}}`
	payload, info, err := RequestToChatCompletions([]byte(raw))
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	if !info.WantReasoning {
		t.Fatal("出现 reasoning 字段应标记 WantReasoning")
	}
	if payload["reasoning_effort"] != "high" {
		t.Fatalf("reasoning_effort = %v", payload["reasoning_effort"])
	}

	// effort 为空时不下发该字段，避免给不支持的上游传个空串
	payload, _, err = RequestToChatCompletions([]byte(`{"model":"m1","input":"x","reasoning":{}}`))
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	if _, exists := payload["reasoning_effort"]; exists {
		t.Fatalf("effort 为空不该下发: %v", payload)
	}

	_, _, err = RequestToChatCompletions([]byte(`{"model":"m1","input":"x","reasoning":{"effort":"ultra"}}`))
	wantField(t, err, "reasoning.effort")
}

func TestResponsesRequestImageBlock(t *testing.T) {
	// image_url 既可能是纯字符串，也可能是 {"url": ...} 对象
	raw := `{"model":"m1","input":[{"type":"message","role":"user","content":[
		{"type":"input_text","text":"看图"},
		{"type":"input_image","image_url":"https://example.com/a.png"},
		{"type":"input_image","image_url":{"url":"https://example.com/b.png"}}]}]}`

	payload, _, err := RequestToChatCompletions([]byte(raw))
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	msg := msgAt(t, msgsOf(t, payload), 0)
	parts, ok := msg["content"].([]any)
	if !ok || len(parts) != 3 {
		t.Fatalf("content 应为 3 块: %v", msg["content"])
	}
	wantURLs := []string{"https://example.com/a.png", "https://example.com/b.png"}
	for i, want := range wantURLs {
		part, _ := parts[i+1].(map[string]any)
		if part["type"] != "image_url" {
			t.Fatalf("第 %d 块应为 image_url: %v", i+1, part)
		}
		image, _ := part["image_url"].(map[string]any)
		if image["url"] != want {
			t.Fatalf("image_url = %v，期望 %s", image["url"], want)
		}
	}
}

// ---------------------------------------------------------------------------
// 响应方向：Chat Completions → Responses
// ---------------------------------------------------------------------------

func TestResponseToResponsesBasics(t *testing.T) {
	up := `{"id":"chatcmpl-abc","model":"up-1","choices":[{"message":{
		"role":"assistant","content":"你好世界",
		"tool_calls":[{"id":"c1","type":"function","function":{"name":"weather","arguments":"{\"city\":\"上海\"}"}}]},
		"finish_reason":"tool_calls"}],
		"usage":{"prompt_tokens":11,"completion_tokens":7}}`

	info := &ResponsesInfo{Model: "m1", ResponseID: "resp_fallback"}
	raw, dropped, err := ResponseToResponses([]byte(up), info)
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	if len(dropped) != 0 {
		t.Fatalf("无 reasoning 时不应有丢弃项: %v", dropped)
	}
	body, output := decodeResponsesBody(t, raw)

	// 回包里的 model 必须是客户端认识的名字，不能是内部上游名
	if body["model"] != "m1" {
		t.Fatalf("model = %v，期望 m1", body["model"])
	}
	if body["status"] != "completed" {
		t.Fatalf("status = %v", body["status"])
	}
	if strings.HasPrefix(body["id"].(string), "chatcmpl") {
		t.Fatalf("上游 ID 未转成 resp_ 前缀: %v", body["id"])
	}
	if len(output) != 2 {
		t.Fatalf("output = %d 项，期望 message + function_call", len(output))
	}
	if output[0].Type != "message" || output[0].Role != "assistant" {
		t.Fatalf("第一项应为 assistant message: %+v", output[0])
	}
	if len(output[0].Content) != 1 || output[0].Content[0].Text != "你好世界" {
		t.Fatalf("正文有误: %+v", output[0].Content)
	}
	if output[1].Type != "function_call" || output[1].Name != "weather" || output[1].CallID != "c1" {
		t.Fatalf("function_call 有误: %+v", output[1])
	}
	if output[1].Arguments != `{"city":"上海"}` {
		t.Fatalf("arguments = %s", output[1].Arguments)
	}
	usage, _ := body["usage"].(map[string]any)
	if usage["input_tokens"] != float64(11) || usage["output_tokens"] != float64(7) || usage["total_tokens"] != float64(18) {
		t.Fatalf("usage 换算有误: %v", usage)
	}
}

func TestResponseToResponsesReasoningVisibility(t *testing.T) {
	up := `{"id":"chatcmpl-x","choices":[{"message":{
		"role":"assistant","content":"答案","reasoning_content":"先想一下"}}],
		"usage":{"prompt_tokens":1,"completion_tokens":1}}`

	// 客户端要了 reasoning：思维链必须排在正文之前
	info := &ResponsesInfo{Model: "m1", ResponseID: "resp_1", WantReasoning: true}
	raw, dropped, err := ResponseToResponses([]byte(up), info)
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	if len(dropped) != 0 {
		t.Fatalf("请求了 reasoning 却丢弃了: %v", dropped)
	}
	_, output := decodeResponsesBody(t, raw)
	if len(output) != 2 || output[0].Type != "reasoning" || output[1].Type != "message" {
		t.Fatalf("reasoning 应排在 message 之前: %+v", output)
	}
	if len(output[0].Summary) != 1 || output[0].Summary[0].Text != "先想一下" {
		t.Fatalf("reasoning summary 有误: %+v", output[0].Summary)
	}

	// 客户端没要：丢弃而不是呈现给前端
	info = &ResponsesInfo{Model: "m1", ResponseID: "resp_1", WantReasoning: false}
	raw, dropped, err = ResponseToResponses([]byte(up), info)
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	if len(dropped) == 0 {
		t.Fatal("未请求 reasoning 时应记录丢弃项")
	}
	_, output = decodeResponsesBody(t, raw)
	if len(output) != 1 || output[0].Type != "message" {
		t.Fatalf("未请求 reasoning 时不应输出 reasoning 项: %+v", output)
	}
}

func TestResponseToResponsesValidation(t *testing.T) {
	info := &ResponsesInfo{Model: "m1"}
	if _, _, err := ResponseToResponses([]byte(`not-json`), info); err == nil {
		t.Fatal("非法 JSON 应报错")
	}
	if _, _, err := ResponseToResponses([]byte(`{"choices":[]}`), info); err == nil {
		t.Fatal("缺 choices 应报错")
	}
}

func TestResponseToResponsesDefaultToolArguments(t *testing.T) {
	up := `{"choices":[{"message":{"role":"assistant",
		"tool_calls":[{"id":"c9","type":"function","function":{"name":"ping"}}]}}]}`
	raw, _, err := ResponseToResponses([]byte(up), &ResponsesInfo{Model: "m1"})
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	_, output := decodeResponsesBody(t, raw)
	if len(output) != 1 {
		t.Fatalf("output = %d 项", len(output))
	}
	if output[0].Arguments != "{}" {
		t.Fatalf("空 arguments 应补成 {}，实际 %q", output[0].Arguments)
	}
}

func TestRespIDMapping(t *testing.T) {
	cases := []struct{ upstream, fallback, want string }{
		{"chatcmpl-abc", "resp_fb", "resp_abc"},
		{"chatcmpl.xyz", "resp_fb", "resp_xyz"},
		{"resp_old", "resp_fb", "resp_old"},
		{"chatcmpl-", "resp_fb", "resp_fb"}, // 前缀去掉后为空，退回 fallback
		{"", "resp_fb", "resp_fb"},
		{"plain-id", "resp_fb", "resp_plain-id"},
	}
	for _, c := range cases {
		if got := respID(c.upstream, c.fallback); got != c.want {
			t.Fatalf("respID(%q, %q) = %q，期望 %q", c.upstream, c.fallback, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// 流式方向
// ---------------------------------------------------------------------------

func TestResponsesStreamEventOrder(t *testing.T) {
	info := &ResponsesInfo{Model: "m1", ResponseID: "resp_1", WantReasoning: true}
	sse := collectResponsesStream(t, NewResponsesStream(info), []string{
		`{"choices":[{"delta":{"role":"assistant","reasoning_content":"想了"}}]}`,
		`{"choices":[{"delta":{"content":"你好"}}]}`,
		`{"choices":[{"delta":{"content":"世界"},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":5}}`,
	})
	names := responsesEventNames(sse)

	// created 必须是第一个事件，completed 必须是最后一个
	if names[0] != "response.created" {
		t.Fatalf("首个事件 = %s", names[0])
	}
	if names[len(names)-1] != "response.completed" {
		t.Fatalf("末个事件 = %s", names[len(names)-1])
	}
	// reasoning 必须先于正文（Responses 协议要求）
	if i, j := responsesEventIndex(names, "response.reasoning_summary_text.delta"), responsesEventIndex(names, "response.output_text.delta"); i < 0 || j < 0 || i > j {
		t.Fatalf("reasoning 应先于正文出现: %v", names)
	}
	// 每个 output item 都要有 added → content → done 的完整生命周期
	for _, want := range []string{
		"response.output_item.added", "response.reasoning_summary_part.added",
		"response.reasoning_summary_text.done", "response.output_text.done",
		"response.content_part.done", "response.output_item.done",
	} {
		if responsesEventIndex(names, want) < 0 {
			t.Fatalf("缺少事件 %s:\n%s", want, sse)
		}
	}
	if !strings.Contains(sse, "你好世界") {
		t.Fatalf("正文增量未累积:\n%s", sse)
	}
	// 注意要用 "event: " 定位：completed 事件的 data 里也有 "response.completed" 字面量
	if u := sse[strings.LastIndex(sse, "event: response.completed"):]; !strings.Contains(u, `"total_tokens":8`) {
		t.Fatalf("completed 事件用量有误:\n%s", u)
	}
}

func TestResponsesStreamDropsReasoningWhenNotRequested(t *testing.T) {
	info := &ResponsesInfo{Model: "m1", ResponseID: "resp_1", WantReasoning: false}
	s := NewResponsesStream(info)
	sse := collectResponsesStream(t, s, []string{
		`{"choices":[{"delta":{"reasoning_content":"偷偷想的"}}]}`,
		`{"choices":[{"delta":{"content":"正文"}}]}`,
	})
	if strings.Contains(sse, "偷偷想的") {
		t.Fatalf("未请求 reasoning 却输出了思维链:\n%s", sse)
	}
	if len(s.Dropped()) == 0 {
		t.Fatal("未请求 reasoning 时应记录丢弃项")
	}
	if !strings.Contains(sse, "正文") {
		t.Fatalf("正文丢失:\n%s", sse)
	}
}

func TestResponsesStreamToolUse(t *testing.T) {
	info := &ResponsesInfo{Model: "m1", ResponseID: "resp_1"}
	s := NewResponsesStream(info)
	sse := collectResponsesStream(t, s, []string{
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"weather","arguments":"{\"c"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ity\":\"上海\"}"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"c2","type":"function","function":{"name":"time","arguments":"{}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	})

	// 两个工具必须各自一个 function_call item，参数拼完整，且不能串到一个里
	completed := sse[strings.LastIndex(sse, "event: response.completed"):]
	if !strings.Contains(completed, `"call_id":"c1"`) || !strings.Contains(completed, `"call_id":"c2"`) {
		t.Fatalf("completed 里缺 call_id:\n%s", completed)
	}
	if !strings.Contains(completed, `{\"city\":\"上海\"}`) {
		t.Fatalf("工具参数未按 index 聚合:\n%s", completed)
	}
	if strings.Count(completed, `"type":"function_call"`) != 2 {
		t.Fatalf("function_call item 数量不对:\n%s", completed)
	}
	if responsesEventIndex(responsesEventNames(sse), "response.function_call_arguments.done") < 0 {
		t.Fatalf("缺少 arguments.done 事件:\n%s", sse)
	}
}

func TestResponsesStreamEmptyAndBadChunk(t *testing.T) {
	// 空流：仍需 created + completed 两个事件，客户端不能卡住
	info := &ResponsesInfo{Model: "m1", ResponseID: "resp_1"}
	empty := collectResponsesStream(t, NewResponsesStream(info), nil)
	names := responsesEventNames(empty)
	if len(names) != 2 || names[0] != "response.created" || names[1] != "response.completed" {
		t.Fatalf("空流事件异常: %v", names)
	}

	// 坏块不应中断整个流
	s := NewResponsesStream(info)
	sse := collectResponsesStream(t, s, []string{
		`{"choices":[{"delta":{"content":"前半"}}]}`,
		`not-json`,
		`{"choices":[{"delta":{"content":"后半"}}]}`,
	})
	if !strings.Contains(sse, "前半") || !strings.Contains(sse, "后半") {
		t.Fatalf("坏块吃掉了正常内容:\n%s", sse)
	}
	if !strings.Contains(sse, `"text":"前半后半"`) {
		t.Fatalf("坏块之后正文应继续累积:\n%s", sse)
	}
}

func TestResponsesStreamUsage(t *testing.T) {
	s := NewResponsesStream(&ResponsesInfo{Model: "m1", ResponseID: "resp_1"})
	collectResponsesStream(t, s, []string{
		`{"choices":[{"delta":{"content":"hi"}}]}`,
		`{"choices":[],"usage":{"prompt_tokens":4,"completion_tokens":6}}`,
	})
	if got := s.Usage(); got.InputTokens != 4 || got.OutputTokens != 6 {
		t.Fatalf("usage = %+v", got)
	}
}
