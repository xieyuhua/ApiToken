package translate

import (
	"encoding/json"
	"strings"
	"time"
)

// ResponsesStream 把上游 OpenAI SSE 流实时转成 Responses API 事件序列。
//
// Responses 事件顺序（文本）：
//
//	response.created → response.output_item.added → response.content_part.added
//	→ response.output_text.delta… → response.output_text.done → response.content_part.done
//	→ response.output_item.done → … → response.completed
//
// reasoning 排在正文之前，function_call 用独立的 arguments.delta 事件承载参数增量。
type ResponsesStream struct {
	info *ResponsesInfo

	seq         int // 事件序号
	outputIndex int // 下一个 output item 的下标
	started     bool
	cur         *responsesItem
	items       []map[string]any // 已完成的 output item（按顺序）

	finishReason string
	usage        Usage
	dropped      []string
}

// responsesItem 当前正在接收增量的一条 output item。
type responsesItem struct {
	kind         string // message / function_call / reasoning
	id           string
	callID       string
	name         string
	toolIndex    int // function_call：对应的 tool_calls[].index，用于区分多个工具
	contentIndex int // message 的 content part 下标
	text         strings.Builder
	args         strings.Builder
	partOpened   bool // message：是否已发 content_part.added
}

// NewResponsesStream 构造 Responses 流式转换器。
func NewResponsesStream(info *ResponsesInfo) *ResponsesStream {
	return &ResponsesStream{info: info, outputIndex: 0}
}

// Begin 输出 response.created（必须在任何内容之前）。
func (s *ResponsesStream) Begin() []byte {
	if s.started {
		return nil
	}
	s.started = true
	resp := map[string]any{
		"id": s.info.ResponseID, "object": "response", "created_at": time.Now().Unix(),
		"status": "in_progress", "model": s.info.Model, "output": []any{},
		"usage": nil, "error": nil, "incomplete_details": nil,
	}
	return []byte(s.event("response.created", map[string]any{
		"type": "response.created", "response": resp,
	}))
}

// Line 处理一个上游 data: 负载（done 为 true 表示收到 [DONE]）。
func (s *ResponsesStream) Line(payload string, done bool) []byte {
	var out strings.Builder
	if !s.started {
		out.Write(s.Begin())
	}
	if done || strings.TrimSpace(payload) == "" {
		return []byte(out.String())
	}

	var ch openAIChunk
	if err := json.Unmarshal([]byte(payload), &ch); err != nil {
		s.dropped = append(s.dropped, "chunk(unparsable)")
		return []byte(out.String())
	}
	if ch.Usage != nil {
		if ch.Usage.PromptTokens > 0 {
			s.usage.InputTokens = ch.Usage.PromptTokens
		}
		if ch.Usage.CompletionTokens > 0 {
			s.usage.OutputTokens = ch.Usage.CompletionTokens
		}
	}
	if len(ch.Choices) == 0 {
		return []byte(out.String())
	}
	d := ch.Choices[0].Delta

	// 思维链优先（Responses 要求 reasoning 排在正文之前）
	if rc := firstNonEmpty(d.ReasoningContent, d.Reasoning); rc != "" {
		if s.info.WantReasoning {
			out.Write(s.reasoningDelta(rc))
		} else {
			s.dropped = append(s.dropped, "choices[].delta.reasoning_content")
		}
	}
	if d.Content != "" {
		out.Write(s.textDelta(d.Content))
	}
	for _, tc := range d.ToolCalls {
		out.Write(s.toolDelta(tc.Index, tc.ID, tc.Function.Name, tc.Function.Arguments))
	}
	if fr := ch.Choices[0].FinishReason; fr != "" {
		s.finishReason = fr
	}
	return []byte(out.String())
}

// End 关闭当前 item 并输出 response.completed。
func (s *ResponsesStream) End() []byte {
	var out strings.Builder
	if !s.started {
		out.Write(s.Begin())
	}
	out.Write(s.closeCurrent())
	out.WriteString(s.completedEvent())
	return []byte(out.String())
}

// Usage 返回统计到的用量。
func (s *ResponsesStream) Usage() Usage { return s.usage }

// Dropped 返回被丢弃的内容说明。
func (s *ResponsesStream) Dropped() []string { return s.dropped }

// ---------------------------------------------------------------------------
// 文本 / 推理 / 工具 增量
// ---------------------------------------------------------------------------

// textDelta 输出正文增量：切换到 message item 并发送 output_text.delta。
func (s *ResponsesStream) textDelta(text string) []byte {
	var out strings.Builder
	if s.cur == nil || s.cur.kind != "message" {
		out.Write(s.switchItem("message"))
	}
	out.WriteString(s.event("response.output_text.delta", map[string]any{
		"type": "response.output_text.delta", "item_id": s.cur.id,
		"output_index": s.outputIndex - 1, "content_index": s.cur.contentIndex, "delta": text,
	}))
	s.cur.text.WriteString(text)
	return []byte(out.String())
}

// reasoningDelta 输出推理增量：切换到 reasoning item 并发送 summary_text.delta。
func (s *ResponsesStream) reasoningDelta(text string) []byte {
	var out strings.Builder
	if s.cur == nil || s.cur.kind != "reasoning" {
		out.Write(s.switchItem("reasoning"))
	}
	out.WriteString(s.event("response.reasoning_summary_text.delta", map[string]any{
		"type": "response.reasoning_summary_text.delta", "item_id": s.cur.id,
		"output_index": s.outputIndex - 1, "summary_index": 0, "delta": text,
	}))
	s.cur.text.WriteString(text)
	return []byte(out.String())
}

// toolDelta 输出工具调用参数增量：按 tool_calls[].index 区分不同工具并发送 arguments.delta。
func (s *ResponsesStream) toolDelta(index int, id, name, args string) []byte {
	var out strings.Builder
	if s.cur == nil || s.cur.kind != "function_call" || s.cur.toolIndex != index {
		out.Write(s.switchTool(index, id, name))
	}
	if args != "" {
		out.WriteString(s.event("response.function_call_arguments.delta", map[string]any{
			"type": "response.function_call_arguments.delta", "item_id": s.cur.id,
			"output_index": s.outputIndex - 1, "delta": args,
		}))
		s.cur.args.WriteString(args)
	}
	return []byte(out.String())
}

// ---------------------------------------------------------------------------
// item 开关
// ---------------------------------------------------------------------------

// switchItem 关闭当前 item，开启指定类型的新 item。
func (s *ResponsesStream) switchItem(kind string) []byte {
	var out strings.Builder
	out.Write(s.closeCurrent())

	idx := s.outputIndex
	s.outputIndex++
	var item map[string]any
	switch kind {
	case "message":
		id := NewItemID("msg_")
		s.cur = &responsesItem{kind: "message", id: id}
		item = map[string]any{
			"id": id, "type": "message", "status": "in_progress", "role": "assistant", "content": []any{},
		}
		out.WriteString(s.event("response.output_item.added", map[string]any{
			"type": "response.output_item.added", "output_index": idx, "item": item,
		}))
		// message 需要先开一个 output_text part
		s.cur.partOpened = true
		out.WriteString(s.event("response.content_part.added", map[string]any{
			"type": "response.content_part.added", "item_id": id, "output_index": idx,
			"content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
		}))
		s.cur.contentIndex = 0
	case "reasoning":
		id := NewItemID("rs_")
		s.cur = &responsesItem{kind: "reasoning", id: id}
		item = map[string]any{
			"id": id, "type": "reasoning", "summary": []any{}, "content": []any{}, "status": "in_progress",
		}
		out.WriteString(s.event("response.output_item.added", map[string]any{
			"type": "response.output_item.added", "output_index": idx, "item": item,
		}))
		out.WriteString(s.event("response.reasoning_summary_part.added", map[string]any{
			"type": "response.reasoning_summary_part.added", "item_id": id, "output_index": idx,
			"summary_index": 0, "part": map[string]any{"type": "summary_text", "text": ""},
		}))
	}
	return []byte(out.String())
}

// switchTool 关闭当前 item，开启 function_call item。
func (s *ResponsesStream) switchTool(index int, id, name string) []byte {
	var out strings.Builder
	out.Write(s.closeCurrent())

	idx := s.outputIndex
	s.outputIndex++
	if id == "" {
		id = "call_" + NewItemID("fc_")[3:]
	}
	s.cur = &responsesItem{kind: "function_call", id: NewItemID("fc_"), callID: id, name: name, toolIndex: index}
	out.WriteString(s.event("response.output_item.added", map[string]any{
		"type": "response.output_item.added", "output_index": idx,
		"item": map[string]any{
			"id": s.cur.id, "type": "function_call", "call_id": id, "name": name,
			"arguments": "", "status": "in_progress",
		},
	}))
	return []byte(out.String())
}

// closeCurrent 关闭当前打开的 item（补 done 事件），并把最终 item 记入 items。
func (s *ResponsesStream) closeCurrent() []byte {
	if s.cur == nil {
		return nil
	}
	var out strings.Builder
	idx := s.outputIndex - 1
	cur := s.cur

	switch cur.kind {
	case "message":
		text := cur.text.String()
		out.WriteString(s.event("response.output_text.done", map[string]any{
			"type": "response.output_text.done", "item_id": cur.id, "output_index": idx,
			"content_index": 0, "text": text,
		}))
		out.WriteString(s.event("response.content_part.done", map[string]any{
			"type": "response.content_part.done", "item_id": cur.id, "output_index": idx,
			"content_index": 0, "part": map[string]any{"type": "output_text", "text": text, "annotations": []any{}},
		}))
		out.WriteString(s.event("response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": idx,
			"item": map[string]any{
				"id": cur.id, "type": "message", "status": "completed", "role": "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}},
			},
		}))
		s.items = append(s.items, map[string]any{
			"type": "message", "id": cur.id, "status": "completed", "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}},
		})

	case "reasoning":
		text := cur.text.String()
		out.WriteString(s.event("response.reasoning_summary_text.done", map[string]any{
			"type": "response.reasoning_summary_text.done", "item_id": cur.id, "output_index": idx,
			"summary_index": 0, "text": text,
		}))
		out.WriteString(s.event("response.reasoning_summary_part.done", map[string]any{
			"type": "response.reasoning_summary_part.done", "item_id": cur.id, "output_index": idx,
			"summary_index": 0, "part": map[string]any{"type": "summary_text", "text": text},
		}))
		out.WriteString(s.event("response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": idx,
			"item": map[string]any{
				"id": cur.id, "type": "reasoning",
				"summary": []any{map[string]any{"type": "summary_text", "text": text}},
				"content": []any{}, "status": "completed",
			},
		}))
		s.items = append(s.items, map[string]any{
			"type": "reasoning", "id": cur.id,
			"summary": []any{map[string]any{"type": "summary_text", "text": text}},
			"content": []any{}, "status": "completed",
		})

	case "function_call":
		args := cur.args.String()
		out.WriteString(s.event("response.function_call_arguments.done", map[string]any{
			"type": "response.function_call_arguments.done", "item_id": cur.id, "output_index": idx,
			"arguments": args,
		}))
		out.WriteString(s.event("response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": idx,
			"item": map[string]any{
				"id": cur.id, "type": "function_call", "call_id": cur.callID, "name": cur.name,
				"arguments": args, "status": "completed",
			},
		}))
		s.items = append(s.items, map[string]any{
			"type": "function_call", "id": cur.id, "call_id": cur.callID, "name": cur.name,
			"arguments": args, "status": "completed",
		})
	}

	s.cur = nil
	return []byte(out.String())
}

// completedEvent 输出 response.completed（含完整 output 与 usage）。
func (s *ResponsesStream) completedEvent() string {
	status := "completed"
	resp := map[string]any{
		"id": s.info.ResponseID, "object": "response", "created_at": time.Now().Unix(),
		"status": status, "model": s.info.Model, "output": s.items,
		"usage": map[string]any{
			"input_tokens":  s.usage.InputTokens,
			"output_tokens": s.usage.OutputTokens,
			"total_tokens":  s.usage.InputTokens + s.usage.OutputTokens,
		},
		"error": nil, "incomplete_details": nil,
	}
	return s.event("response.completed", map[string]any{
		"type": "response.completed", "response": resp,
	})
}

// event 编码一条 Responses SSE 事件（带序号）。
func (s *ResponsesStream) event(name string, data map[string]any) string {
	seq := s.seq
	s.seq++
	if _, ok := data["sequence_number"]; !ok {
		data["sequence_number"] = seq
	}
	b, err := json.Marshal(data)
	if err != nil {
		return ""
	}
	return "event: " + name + "\ndata: " + string(b) + "\n\n"
}
