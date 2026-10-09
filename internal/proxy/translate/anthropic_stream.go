package translate

import (
	"encoding/json"
	"strings"
)

// openAIChunk OpenAI 流式响应的一个 SSE 数据块（只取需要的字段）。
type openAIChunk struct {
	ID      string `json:"id"`
	Choices []struct {
		Delta struct {
			Role             string `json:"role"`
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			Reasoning        string `json:"reasoning"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
	} `json:"usage"`
}

const (
	blockNone     = ""
	blockThinking = "thinking"
	blockText     = "text"
	blockTool     = "tool_use"
)

// Stream 把上游 OpenAI SSE 流实时转成 Anthropic Messages 事件序列。
//
// Anthropic 的事件顺序是强约束：
//
//	message_start → (content_block_start → content_block_delta… → content_block_stop)* → message_delta → message_stop
//
// 且 thinking 块必须排在 text/tool_use 之前，因此一旦正文已开始再收到思维链只能丢弃。
type Stream struct {
	info *Info

	started   bool
	curBlock  int // 当前打开的块下标，-1 表示无
	curType   string
	nextIndex int
	toolSlot  map[int]int // OpenAI tool_calls[].index → Anthropic 块下标
	toolOpen  map[int]bool

	finishReason string
	usage        Usage
	dropped      []string
}

// NewStream 构造流式转换器。
func NewStream(info *Info) *Stream {
	return &Stream{
		info:      info,
		curBlock:  -1,
		toolSlot:  map[int]int{},
		toolOpen:  map[int]bool{},
		nextIndex: 0,
	}
}

// Begin 输出 message_start（必须在任何内容之前）。
func (s *Stream) Begin() []byte {
	if s.started {
		return nil
	}
	s.started = true
	msg := map[string]any{
		"type": "message",
		"id":   s.info.MessageID,
		"role": "assistant",
		// 回包用客户端请求的模型名
		"model":         s.info.Model,
		"content":       []any{},
		"stop_reason":   nil,
		"stop_sequence": nil,
		// OpenAI 流式把用量放在最后一个数据块，此刻还未知
		"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
	}
	return []byte(s.event("message_start", map[string]any{"type": "message_start", "message": msg}))
}

// Line 处理一个上游 data: 负载（done 为 true 表示收到 [DONE]），返回发给客户端的字节。
func (s *Stream) Line(payload string, done bool) []byte {
	var out strings.Builder
	if !s.started {
		out.Write(s.Begin())
	}
	if done || strings.TrimSpace(payload) == "" {
		return []byte(out.String())
	}

	var ch openAIChunk
	if err := json.Unmarshal([]byte(payload), &ch); err != nil {
		// 单块解析失败不影响整体流：丢弃并记录
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

	// 思维链优先（Anthropic 协议要求 thinking 块排在正文之前）
	if rc := firstNonEmpty(d.ReasoningContent, d.Reasoning); rc != "" {
		if s.info.WantThinking {
			out.Write(s.blockDelta(blockThinking, "thinking_delta", "thinking", rc))
		} else {
			// 客户端没开 thinking 就不能发 thinking 块，否则 Claude Code 会报错
			s.dropped = append(s.dropped, "choices[].delta.reasoning_content")
		}
	}
	if d.Content != "" {
		out.Write(s.blockDelta(blockText, "text_delta", "text", d.Content))
	}
	for _, tc := range d.ToolCalls {
		out.Write(s.toolDelta(tc.Index, tc.ID, tc.Function.Name, tc.Function.Arguments))
	}
	// finish_reason 在 choice 层（最后一个数据块）
	if fr := ch.Choices[0].FinishReason; fr != "" {
		s.finishReason = FinishReason(fr)
	}
	return []byte(out.String())
}

// End 输出收尾事件（message_delta + message_stop）。
func (s *Stream) End() []byte {
	var out strings.Builder
	if !s.started {
		out.Write(s.Begin())
	}
	// 关闭仍在打开的块
	if s.curBlock >= 0 {
		out.WriteString(s.event("content_block_stop", map[string]any{
			"type": "content_block_stop", "index": s.curBlock,
		}))
		s.curBlock = -1
		s.curType = blockNone
	}
	reason := s.finishReason
	if reason == "" {
		reason = "end_turn"
	}
	out.WriteString(s.event("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": reason, "stop_sequence": nil},
		"usage": map[string]any{
			"input_tokens":  s.usage.InputTokens,
			"output_tokens": s.usage.OutputTokens,
		},
	}))
	out.WriteString(s.event("message_stop", map[string]any{"type": "message_stop"}))
	return []byte(out.String())
}

// Usage 返回统计到的用量。
func (s *Stream) Usage() Usage { return s.usage }

// Dropped 返回被丢弃的内容说明（未请求 thinking 时的思维链等）。
func (s *Stream) Dropped() []string { return s.dropped }

// blockDelta 写入同一类型的连续增量；类型切换时先关闭旧块再开新块。
func (s *Stream) blockDelta(kind, deltaType, field, text string) []byte {
	var out strings.Builder
	if s.curType != kind || s.curBlock < 0 {
		if s.curBlock >= 0 {
			out.WriteString(s.event("content_block_stop", map[string]any{
				"type": "content_block_stop", "index": s.curBlock,
			}))
		}
		idx := s.nextIndex
		s.nextIndex++
		block := map[string]any{"type": kind}
		switch kind {
		case blockText:
			block["text"] = ""
		case blockThinking:
			block["thinking"] = ""
		}
		out.WriteString(s.event("content_block_start", map[string]any{
			"type": "content_block_start", "index": idx, "content_block": block,
		}))
		s.curBlock = idx
		s.curType = kind
	}
	delta := map[string]any{"type": deltaType}
	delta[field] = text
	out.WriteString(s.event("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": s.curBlock, "delta": delta,
	}))
	return []byte(out.String())
}

// toolDelta 处理工具调用增量：首帧开块，后续帧只追加 arguments 片段。
func (s *Stream) toolDelta(idx int, id, name, args string) []byte {
	var out strings.Builder
	slot, ok := s.toolSlot[idx]
	if !ok {
		// 新工具调用：关闭当前块后开 tool_use 块
		if s.curBlock >= 0 {
			out.WriteString(s.event("content_block_stop", map[string]any{
				"type": "content_block_stop", "index": s.curBlock,
			}))
		}
		slot = s.nextIndex
		s.nextIndex++
		s.toolSlot[idx] = slot
		if id == "" {
			id = "toolu_" + NewMessageID()[4:]
		}
		out.WriteString(s.event("content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": slot,
			"content_block": map[string]any{
				"type": blockTool, "id": id, "name": name, "input": map[string]any{},
			},
		}))
		s.curBlock = slot
		s.curType = blockTool
		s.toolOpen[idx] = true
	}
	if args != "" {
		out.WriteString(s.event("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": slot,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": args},
		}))
	}
	return []byte(out.String())
}

// event 编码一条 Anthropic SSE 事件。
func (s *Stream) event(name string, data map[string]any) string {
	b, err := json.Marshal(data)
	if err != nil {
		return ""
	}
	return "event: " + name + "\ndata: " + string(b) + "\n\n"
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
