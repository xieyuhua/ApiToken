package translate

import (
	"encoding/json"
	"strings"
)

// Usage Anthropic 风格的用量字段。
type Usage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

// OpenAI 非流式响应（只取转换需要的字段）。
type openAIResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Role             string `json:"role"`
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			Reasoning        string `json:"reasoning"`
			ToolCalls        []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
	} `json:"usage"`
}

type anthropicResponse struct {
	Type         string `json:"type"`
	ID           string `json:"id"`
	Role         string `json:"role"`
	Model        string `json:"model"`
	Content      []any  `json:"content"`
	StopReason   string `json:"stop_reason"`
	StopSequence any    `json:"stop_sequence"`
	Usage        Usage  `json:"usage"`
}

type anthropicBlockOut struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	// thinking 块的内容字段叫 thinking，不是 text
	Thinking string `json:"thinking,omitempty"`
	ID       string `json:"id,omitempty"`
	Name     string `json:"name,omitempty"`
	Input    any    `json:"input,omitempty"`
}

// FinishReason 映射 OpenAI finish_reason → Anthropic stop_reason。
func FinishReason(openai string) string {
	switch openai {
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	case "content_filter":
		// Anthropic 无对应枚举，按正常结束处理
		return "end_turn"
	default:
		return "end_turn"
	}
}

// MessageID 把上游返回的 ID 转成 Anthropic 风格（msg_ 前缀）。
func MessageID(upstream string, fallback string) string {
	if upstream == "" {
		return fallback
	}
	s := upstream
	s = strings.TrimPrefix(s, "chatcmpl-")
	s = strings.TrimPrefix(s, "chatcmpl.")
	s = strings.TrimPrefix(s, "msg_")
	if s == "" {
		return fallback
	}
	return "msg_" + s
}

// ResponseToAnthropic 把 OpenAI 非流式响应转成 Anthropic Messages 响应。
//
// info 来自请求转换阶段：客户端没开 thinking 时，思维链内容会被丢弃并记入 Dropped。
func ResponseToAnthropic(raw []byte, info *Info) ([]byte, []string, error) {
	var up openAIResponse
	if err := json.Unmarshal(raw, &up); err != nil {
		return nil, nil, errf("", "上游响应不是合法 JSON: %v", err)
	}
	var dropped []string
	out := anthropicResponse{
		Type: "message",
		ID:   MessageID(up.ID, info.MessageID),
		Role: "assistant",
		// 回包用客户端请求的模型名，而不是上游真实模型名
		Model:   info.Model,
		Content: []any{},
		Usage:   Usage{InputTokens: up.Usage.PromptTokens, OutputTokens: up.Usage.CompletionTokens},
	}

	if len(up.Choices) == 0 {
		return nil, nil, errf("", "上游响应缺少 choices")
	}
	ch := up.Choices[0]

	// 思维链：Anthropic 要求 thinking 块排在最前
	thinking := ch.Message.ReasoningContent
	if thinking == "" {
		thinking = ch.Message.Reasoning
	}
	if thinking != "" {
		if info.WantThinking {
			out.Content = append(out.Content, anthropicBlockOut{Type: "thinking", Thinking: thinking})
		} else {
			dropped = append(dropped, "choices[0].message.reasoning_content")
		}
	}

	if ch.Message.Content != "" {
		out.Content = append(out.Content, anthropicBlockOut{Type: "text", Text: ch.Message.Content})
	}

	for _, tc := range ch.Message.ToolCalls {
		var input any
		if tc.Function.Arguments != "" {
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &input); err != nil {
				// 参数不是合法 JSON 时保留原文，避免整条响应失败
				input = map[string]any{"__raw": tc.Function.Arguments}
				dropped = append(dropped, "choices[0].message.tool_calls[].function.arguments")
			}
		}
		if input == nil {
			input = map[string]any{}
		}
		name := tc.Function.Name
		if name == "" {
			name = "unknown"
		}
		out.Content = append(out.Content, anthropicBlockOut{
			Type: "tool_use", ID: tc.ID, Name: name, Input: input,
		})
	}

	out.StopReason = FinishReason(ch.FinishReason)
	return mustJSON(out), dropped, nil
}

// ErrorResponse 生成 Anthropic 风格错误体。
func ErrorResponse(typ, message string) []byte {
	return mustJSON(map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    typ,
			"message": message,
		},
	})
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(`{"type":"error","error":{"type":"api_error","message":"转换失败"}}`)
	}
	return b
}
