package translate

import (
	"encoding/json"
	"strings"
	"testing"
)

// 解码后按字段取值，避免测试依赖具体类型
func msgsOf(t *testing.T, payload map[string]any) []any {
	t.Helper()
	m, ok := payload["messages"].([]any)
	if !ok {
		t.Fatalf("payload 缺少 messages: %v", payload)
	}
	return m
}

func msgAt(t *testing.T, msgs []any, i int) map[string]any {
	t.Helper()
	if i >= len(msgs) {
		t.Fatalf("messages 只有 %d 条，取不到第 %d 条", len(msgs), i)
	}
	m, _ := msgs[i].(map[string]any)
	return m
}

func TestRequestToOpenAIBasics(t *testing.T) {
	raw := `{"model":"m1","max_tokens":64,"system":"系统提示",
		"messages":[{"role":"user","content":"你好"}],
		"temperature":0.3,"top_p":0.9,"stop_sequences":["END"]}`

	payload, info, err := RequestToOpenAI([]byte(raw))
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	if info.Model != "m1" {
		t.Fatalf("info.Model = %q", info.Model)
	}
	if !strings.HasPrefix(info.MessageID, "msg_") {
		t.Fatalf("MessageID = %q", info.MessageID)
	}
	if payload["max_tokens"] != 64 {
		t.Fatalf("max_tokens = %v", payload["max_tokens"])
	}
	if payload["temperature"] != 0.3 || payload["top_p"] != 0.9 {
		t.Fatalf("采样参数未透传: %v", payload)
	}
	if stop, ok := payload["stop"].([]string); !ok || len(stop) != 1 || stop[0] != "END" {
		t.Fatalf("stop_sequences 未转成 stop: %v", payload["stop"])
	}
	// system 必须在首位
	msgs := msgsOf(t, payload)
	if len(msgs) != 2 {
		t.Fatalf("messages = %d 条", len(msgs))
	}
	if m := msgAt(t, msgs, 0); m["role"] != "system" || m["content"] != "系统提示" {
		t.Fatalf("system 位置不对: %v", m)
	}
	if m := msgAt(t, msgs, 1); m["role"] != "user" || m["content"] != "你好" {
		t.Fatalf("user 消息不对: %v", m)
	}
}

// system 为块数组时应拼成纯文本
func TestRequestToOpenAISystemBlocks(t *testing.T) {
	raw := `{"model":"m","max_tokens":8,
		"system":[{"type":"text","text":"A"},{"type":"text","text":"B"}],
		"messages":[{"role":"user","content":"x"}]}`
	payload, _, err := RequestToOpenAI([]byte(raw))
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	msgs := msgsOf(t, payload)
	if m := msgAt(t, msgs, 0); m["content"] != "AB" {
		t.Fatalf("system 块未拼接: %v", m)
	}
}

func TestRequestToOpenAITools(t *testing.T) {
	raw := `{"model":"m","max_tokens":8,
		"tools":[
			{"name":"a","description":"da","input_schema":{"type":"object"}},
			{"name":"b","input_schema":{"type":"object"}}
		],
		"tool_choice":{"type":"tool","name":"a"},
		"messages":[{"role":"user","content":"x"}]}`
	payload, _, err := RequestToOpenAI([]byte(raw))
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	tools, _ := payload["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("tools = %v", payload["tools"])
	}
	t0, _ := tools[0].(map[string]any)
	if t0["type"] != "function" {
		t.Fatalf("tool.type = %v", t0["type"])
	}
	fn, _ := t0["function"].(map[string]any)
	if fn["name"] != "a" || fn["description"] != "da" {
		t.Fatalf("function = %v", fn)
	}
	if _, ok := fn["parameters"]; !ok {
		t.Fatalf("缺 parameters: %v", fn)
	}
	// 缺 input_schema 时补默认空 schema
	t1, _ := tools[1].(map[string]any)
	fn1, _ := t1["function"].(map[string]any)
	if _, ok := fn1["parameters"]; !ok {
		t.Fatalf("缺 input_schema 时未补默认值: %v", fn1)
	}
	tc, _ := payload["tool_choice"].(map[string]any)
	if f, _ := tc["function"].(map[string]any); f["name"] != "a" {
		t.Fatalf("tool_choice 映射错误: %v", payload["tool_choice"])
	}
}

func TestToolChoiceMapping(t *testing.T) {
	cases := map[string]any{
		`{"type":"auto"}`: "auto",
		`{"type":"any"}`:  "required",
		`{"type":"none"}`: "none",
	}
	for in, want := range cases {
		raw := `{"model":"m","max_tokens":8,"tools":[{"name":"a"}],"tool_choice":` + in +
			`,"messages":[{"role":"user","content":"x"}]}`
		payload, _, err := RequestToOpenAI([]byte(raw))
		if err != nil {
			t.Fatalf("%s 转换失败: %v", in, err)
		}
		if payload["tool_choice"] != want {
			t.Fatalf("%s → %v, 期望 %v", in, payload["tool_choice"], want)
		}
	}
}

// tool_result 必须拆成独立的 role=tool 消息，且排在同消息正文之前
func TestToolResultBecomesToolMessage(t *testing.T) {
	raw := `{"model":"m","max_tokens":8,"messages":[
		{"role":"user","content":"查天气"},
		{"role":"assistant","content":[{"type":"tool_use","id":"tu1","name":"f","input":{"a":1}}]},
		{"role":"user","content":[
			{"type":"tool_result","tool_use_id":"tu1","content":"结果"},
			{"type":"text","text":"继续"}
		]}]}`
	payload, _, err := RequestToOpenAI([]byte(raw))
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	msgs := msgsOf(t, payload)
	if len(msgs) != 4 {
		t.Fatalf("messages = %d 条: %v", len(msgs), msgs)
	}
	asst := msgAt(t, msgs, 1)
	tcs, _ := asst["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("tool_use 未转成 tool_calls: %v", asst)
	}
	tc, _ := tcs[0].(map[string]any)
	fn, _ := tc["function"].(map[string]any)
	if fn["name"] != "f" {
		t.Fatalf("function.name = %v", fn["name"])
	}
	// arguments 必须是 JSON 字符串
	if args, ok := fn["arguments"].(string); !ok || !strings.Contains(args, `"a":1`) {
		t.Fatalf("arguments 应为 JSON 字符串: %v", fn["arguments"])
	}
	tm := msgAt(t, msgs, 2)
	if tm["role"] != "tool" || tm["tool_call_id"] != "tu1" || tm["content"] != "结果" {
		t.Fatalf("tool 消息不对: %v", tm)
	}
	if m := msgAt(t, msgs, 3); m["role"] != "user" || m["content"] != "继续" {
		t.Fatalf("tool_result 后的正文不对: %v", m)
	}
}

// 客户端回传的 thinking 应转成 reasoning_content
func TestThinkingBlockBecomesReasoningContent(t *testing.T) {
	raw := `{"model":"m","max_tokens":100,
		"thinking":{"type":"enabled","budget_tokens":16},
		"messages":[
			{"role":"user","content":"q"},
			{"role":"assistant","content":[
				{"type":"thinking","thinking":"思考中","signature":"sig"},
				{"type":"text","text":"答案"}]}
		]}`
	payload, info, err := RequestToOpenAI([]byte(raw))
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	if !info.WantThinking {
		t.Fatal("WantThinking 应为 true")
	}
	msgs := msgsOf(t, payload)
	asst := msgAt(t, msgs, 1)
	if asst["reasoning_content"] != "思考中" {
		t.Fatalf("thinking 未转成 reasoning_content: %v", asst)
	}
	if asst["content"] != "答案" {
		t.Fatalf("content = %v", asst["content"])
	}
	// signature 无法映射，应记录为丢弃
	if !containsStr(info.Dropped, "signature") {
		t.Fatalf("signature 应记入 Dropped: %v", info.Dropped)
	}
}

// 未请求 thinking 时，即便上游给了思维链也必须丢弃
func TestResponseDropsReasoningWhenNotRequested(t *testing.T) {
	up := `{"id":"chatcmpl-1","choices":[{"index":0,"message":{
		"role":"assistant","reasoning_content":"思考","content":"答案"},"finish_reason":"stop"}]}`
	_, dropped, err := ResponseToAnthropic([]byte(up), &Info{Model: "m", MessageID: "msg_x"})
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(mustJSON(map[string]any{}), &out); err != nil {
		t.Fatal(err)
	}
	if len(dropped) == 0 || !strings.Contains(strings.Join(dropped, ","), "reasoning_content") {
		t.Fatalf("未请求 thinking 时应丢弃思维链: %v", dropped)
	}
}

func TestResponseToAnthropicOrdering(t *testing.T) {
	up := `{"id":"chatcmpl-9","model":"upstream-model","choices":[{"index":0,"message":{
		"role":"assistant","reasoning_content":"思考","content":"答案",
		"tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{\"k\":1}"}}]},
		"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":4}}`

	body, dropped, err := ResponseToAnthropic([]byte(up), &Info{
		Model: "client-model", MessageID: "msg_x", WantThinking: true,
	})
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	var out struct {
		Type    string `json:"type"`
		ID      string `json:"id"`
		Model   string `json:"model"`
		Content []struct {
			Type     string          `json:"type"`
			Text     string          `json:"text"`
			Thinking string          `json:"thinking"`
			ID       string          `json:"id"`
			Name     string          `json:"name"`
			Input    json.RawMessage `json:"input"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			Input  int64 `json:"input_tokens"`
			Output int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("输出不是 JSON: %v", err)
	}
	if out.Type != "message" {
		t.Fatalf("type = %q", out.Type)
	}
	// 上游 id 转成 msg_ 前缀
	if out.ID != "msg_9" {
		t.Fatalf("id = %q", out.ID)
	}
	// 模型名必须用客户端请求的，避免客户端看到不认识的名字
	if out.Model != "client-model" {
		t.Fatalf("model = %q, 期望 client-model", out.Model)
	}
	if len(out.Content) != 3 {
		t.Fatalf("content 块数 = %d: %s", len(out.Content), body)
	}
	if out.Content[0].Type != "thinking" || out.Content[0].Thinking != "思考" {
		t.Fatalf("thinking 块位置或字段不对: %+v", out.Content[0])
	}
	if out.Content[1].Type != "text" || out.Content[1].Text != "答案" {
		t.Fatalf("text 块不对: %+v", out.Content[1])
	}
	if out.Content[2].Type != "tool_use" || out.Content[2].Name != "f" {
		t.Fatalf("tool_use 块不对: %+v", out.Content[2])
	}
	if !strings.Contains(string(out.Content[2].Input), `"k":1`) {
		t.Fatalf("tool_use.input 未解析 arguments: %s", out.Content[2].Input)
	}
	if out.StopReason != "tool_use" {
		t.Fatalf("stop_reason = %q", out.StopReason)
	}
	if out.Usage.Input != 3 || out.Usage.Output != 4 {
		t.Fatalf("usage = %+v", out.Usage)
	}
	if len(dropped) != 0 {
		t.Fatalf("不应有丢弃项: %v", dropped)
	}
}

// arguments 不是合法 JSON 时保留原文，而不是让整条响应失败
func TestResponseKeepsBrokenToolArguments(t *testing.T) {
	up := `{"id":"1","choices":[{"index":0,"message":{"role":"assistant",
		"tool_calls":[{"id":"c1","function":{"name":"f","arguments":"{oops"}}]},
		"finish_reason":"tool_calls"}]}`
	body, dropped, err := ResponseToAnthropic([]byte(up), &Info{Model: "m", MessageID: "msg_x"})
	if err != nil {
		t.Fatalf("转换不应失败: %v", err)
	}
	if !strings.Contains(string(body), "__raw") {
		t.Fatalf("应保留原始参数: %s", body)
	}
	if len(dropped) == 0 {
		t.Fatal("应记录被降级的字段")
	}
}

func TestFinishReasonMapping(t *testing.T) {
	cases := map[string]string{
		"stop":           "end_turn",
		"length":         "max_tokens",
		"tool_calls":     "tool_use",
		"function_call":  "tool_use",
		"content_filter": "end_turn",
		"":               "end_turn",
	}
	for in, want := range cases {
		if got := FinishReason(in); got != want {
			t.Fatalf("FinishReason(%q) = %q, 期望 %q", in, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// SSE 流式转换
// ---------------------------------------------------------------------------

func collectStream(t *testing.T, s *Stream, chunks []string) string {
	t.Helper()
	var out strings.Builder
	out.Write(s.Begin())
	for _, c := range chunks {
		out.Write(s.Line(c, false))
	}
	out.Write(s.End())
	return out.String()
}

func TestStreamEventOrder(t *testing.T) {
	s := NewStream(&Info{Model: "m", MessageID: "msg_1"})
	got := collectStream(t, s, []string{
		`{"id":"1","choices":[{"index":0,"delta":{"content":"你"}}]}`,
		`{"id":"1","choices":[{"index":0,"delta":{"content":"好"}}]}`,
		`{"id":"1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":5}}`,
	})
	want := []string{
		"event: message_start",
		"event: content_block_start",
		"event: content_block_delta",
		"event: content_block_delta",
		"event: content_block_stop",
		"event: message_delta",
		"event: message_stop",
	}
	pos := 0
	for _, w := range want {
		i := strings.Index(got[pos:], w)
		if i < 0 {
			t.Fatalf("缺少事件 %s（已消费 %v）:\n%s", w, want[:pos], got)
		}
		pos += i
	}
	if strings.Contains(got, "data: [DONE]") {
		t.Fatal("Anthropic 协议不应出现 [DONE]")
	}
	if u := s.Usage(); u.InputTokens != 7 || u.OutputTokens != 5 {
		t.Fatalf("usage = %+v", u)
	}
}

func TestStreamStopReason(t *testing.T) {
	s := NewStream(&Info{Model: "m", MessageID: "msg_1"})
	got := collectStream(t, s, []string{
		`{"choices":[{"index":0,"delta":{"content":"x"},"finish_reason":"length"}]}`,
	})
	if !strings.Contains(got, `"stop_reason":"max_tokens"`) {
		t.Fatalf("length 未映射为 max_tokens:\n%s", got)
	}
}

// 未请求 thinking 时丢弃思维链增量
func TestStreamDropsReasoningWhenNotRequested(t *testing.T) {
	s := NewStream(&Info{Model: "m", MessageID: "msg_1"})
	got := collectStream(t, s, []string{
		`{"choices":[{"index":0,"delta":{"reasoning_content":"思考"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"答案"}}]}`,
	})
	if strings.Contains(got, "thinking_delta") {
		t.Fatalf("未请求 thinking 却发出了 thinking 事件:\n%s", got)
	}
	if !strings.Contains(got, "text_delta") {
		t.Fatalf("正文丢失:\n%s", got)
	}
	if len(s.Dropped()) == 0 {
		t.Fatal("应记录丢弃项")
	}
}

// 工具调用：首帧开块，后续帧只追加 arguments
func TestStreamToolUse(t *testing.T) {
	s := NewStream(&Info{Model: "m", MessageID: "msg_1"})
	got := collectStream(t, s, []string{
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1",
			"function":{"name":"get_weather","arguments":""}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,
			"function":{"arguments":"{\"city\":"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,
			"function":{"arguments":"\"北京\"}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	})
	if strings.Count(got, "event: content_block_start") != 1 {
		t.Fatalf("同一工具不应重复开块:\n%s", got)
	}
	if strings.Count(got, "input_json_delta") != 2 {
		t.Fatalf("arguments 分片数不对:\n%s", got)
	}
	if !strings.Contains(got, `"name":"get_weather"`) {
		t.Fatalf("工具名丢失:\n%s", got)
	}
	if !strings.Contains(got, `"stop_reason":"tool_use"`) {
		t.Fatalf("stop_reason = %s", got)
	}
	// 同一个工具的所有增量必须指向同一个块下标
	toolIdx := blockIndexOf(got, "content_block_start")
	if toolIdx < 0 {
		t.Fatalf("未找到 content_block_start:\n%s", got)
	}
	if deltaIdx := blockIndexOf(got, "input_json_delta"); deltaIdx != toolIdx {
		t.Fatalf("input_json_delta 块下标 = %d, 期望 %d:\n%s", deltaIdx, toolIdx, got)
	}
}

// blockIndexOf 返回首个指定事件类型里的 index 值。
func blockIndexOf(sse, deltaType string) int {
	for _, block := range strings.Split(sse, "\n\n") {
		if !strings.Contains(block, `"type":"`+deltaType+`"`) {
			continue
		}
		for _, line := range strings.Split(block, "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var e struct {
				Index int `json:"index"`
			}
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &e); err == nil {
				return e.Index
			}
		}
	}
	return -1
}

// 工具调用后又有正文：应新开 text 块并先关闭 tool 块
func TestStreamToolThenText(t *testing.T) {
	s := NewStream(&Info{Model: "m", MessageID: "msg_1"})
	got := collectStream(t, s, []string{
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1",
			"function":{"name":"f","arguments":"{}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"补充说明"}}]}`,
	})
	toolStart := strings.Index(got, `"type":"tool_use"`)
	textStart := strings.Index(got, `"type":"text"`)
	if toolStart < 0 || textStart < 0 || toolStart > textStart {
		t.Fatalf("块顺序不对:\n%s", got)
	}
	stopBeforeText := strings.LastIndex(got[:textStart], "event: content_block_stop")
	if stopBeforeText < 0 {
		t.Fatalf("切换类型前未关闭 tool 块:\n%s", got)
	}
}

// 空响应也要能正常收尾
func TestStreamEmptyResponse(t *testing.T) {
	s := NewStream(&Info{Model: "m", MessageID: "msg_1"})
	got := collectStream(t, s, nil)
	if !strings.Contains(got, "event: message_start") || !strings.Contains(got, "event: message_stop") {
		t.Fatalf("空响应缺首尾事件:\n%s", got)
	}
	if !strings.Contains(got, `"stop_reason":"end_turn"`) {
		t.Fatalf("空响应应有默认 stop_reason:\n%s", got)
	}
}

// 上游坏块不应中断整个流
func TestStreamSkipsBadChunk(t *testing.T) {
	s := NewStream(&Info{Model: "m", MessageID: "msg_1"})
	got := collectStream(t, s, []string{
		`{不是 json}`,
		`{"choices":[{"index":0,"delta":{"content":"好"}}]}`,
	})
	if !strings.Contains(got, "好") {
		t.Fatalf("坏块之后的正常内容丢失:\n%s", got)
	}
	if len(s.Dropped()) == 0 {
		t.Fatal("应记录坏块")
	}
}

func TestStrictValidationErrors(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{"未知字段", `{"model":"m","max_tokens":1,"foo":1,"messages":[{"role":"user","content":"x"}]}`, "foo"},
		{"缺模型", `{"max_tokens":1,"messages":[{"role":"user","content":"x"}]}`, "model"},
		{"缺 max_tokens", `{"model":"m","messages":[{"role":"user","content":"x"}]}`, "max_tokens"},
		{"空 messages", `{"model":"m","max_tokens":1,"messages":[]}`, "messages"},
		{"非法角色", `{"model":"m","max_tokens":1,"messages":[{"role":"system2","content":"x"}]}`, "role"},
		{"未知块", `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":[{"type":"zzz"}]}]}`, "zzz"},
		{"图片无 source", `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":[{"type":"image"}]}]}`, "source"},
		{"system 非文本块", `{"model":"m","max_tokens":1,"system":[{"type":"image"}],"messages":[{"role":"user","content":"x"}]}`, "system"},
		{"tool_choice 非法", `{"model":"m","max_tokens":1,"tools":[{"name":"a"}],"tool_choice":{"type":"zzz"},"messages":[{"role":"user","content":"x"}]}`, "tool_choice"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := RequestToOpenAI([]byte(c.body))
			if err == nil {
				t.Fatal("应报错")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("错误信息应含 %q，实际: %v", c.want, err)
			}
		})
	}
}

// metadata 无副作用，应丢弃而不是报错
func TestMetadataDroppedNotRejected(t *testing.T) {
	raw := `{"model":"m","max_tokens":1,"metadata":{"user_id":"u1"},
		"messages":[{"role":"user","content":"x"}]}`
	_, info, err := RequestToOpenAI([]byte(raw))
	if err != nil {
		t.Fatalf("metadata 不应导致失败: %v", err)
	}
	if !containsStr(info.Dropped, "metadata") {
		t.Fatalf("metadata 应记入 Dropped: %v", info.Dropped)
	}
}

func TestMessageIDFromUpstream(t *testing.T) {
	cases := map[string]string{
		"chatcmpl-abc": "msg_abc",
		"msg_xyz":      "msg_xyz",
		"abc":          "msg_abc",
		"":             "msg_fallback",
	}
	for in, want := range cases {
		got := MessageID(in, "msg_fallback")
		if in == "" {
			if got != want {
				t.Fatalf("MessageID(%q) = %q", in, got)
			}
			continue
		}
		if got != want {
			t.Fatalf("MessageID(%q) = %q, 期望 %q", in, got, want)
		}
	}
}

func containsStr(list []string, want string) bool {
	for _, s := range list {
		if strings.Contains(s, want) {
			return true
		}
	}
	return false
}
