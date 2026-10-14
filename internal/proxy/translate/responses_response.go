package translate

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// NewResponseID 生成 Responses 风格的 resp_ ID。
func NewResponseID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "resp_" + strconv.FormatUint(uint64(time.Now().UnixNano()), 16) +
			"-" + strconv.FormatUint(msgIDFallbackSeq.Add(1), 10)
	}
	return "resp_" + hex.EncodeToString(b)
}

// NewItemID 生成 Responses output item 的 ID（msg_/fc_/rs_ 前缀）。
func NewItemID(prefix string) string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return prefix + strconv.FormatUint(uint64(time.Now().UnixNano()), 16) +
			"-" + strconv.FormatUint(msgIDFallbackSeq.Add(1), 10)
	}
	return prefix + hex.EncodeToString(b)
}

// ResponseToResponses 把 OpenAI 非流式响应转成 Responses API 响应。
//
// info 来自请求转换阶段：客户端没请求 reasoning 时，思维链内容会被丢弃并记入 Dropped。
func ResponseToResponses(raw []byte, info *ResponsesInfo) ([]byte, []string, error) {
	var up openAIResponse
	if err := json.Unmarshal(raw, &up); err != nil {
		return nil, nil, errf("", "上游响应不是合法 JSON: %v", err)
	}
	if len(up.Choices) == 0 {
		return nil, nil, errf("", "上游响应缺少 choices")
	}
	ch := up.Choices[0]

	var dropped []string
	output := make([]any, 0, 3)

	// reasoning（排在最前）
	thinking := ch.Message.ReasoningContent
	if thinking == "" {
		thinking = ch.Message.Reasoning
	}
	if thinking != "" {
		if info.WantReasoning {
			output = append(output, map[string]any{
				"type": "reasoning", "id": NewItemID("rs_"),
				"summary": []any{map[string]any{"type": "summary_text", "text": thinking}},
				"content": []any{}, "status": "completed",
			})
		} else {
			dropped = append(dropped, "choices[0].message.reasoning_content")
		}
	}

	// message（正文）
	if ch.Message.Content != "" {
		output = append(output, map[string]any{
			"type": "message", "id": NewItemID("msg_"), "status": "completed", "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": ch.Message.Content, "annotations": []any{}}},
		})
	}

	// function_call（工具调用）
	for _, tc := range ch.Message.ToolCalls {
		args := tc.Function.Arguments
		if args == "" {
			args = "{}"
		}
		output = append(output, map[string]any{
			"type": "function_call", "id": NewItemID("fc_"), "call_id": tc.ID,
			"name": tc.Function.Name, "arguments": args, "status": "completed",
		})
	}

	resp := map[string]any{
		"id":         respID(up.ID, info.ResponseID),
		"object":     "response",
		"created_at": time.Now().Unix(),
		"status":     "completed",
		"model":      info.Model,
		"output":     output,
		"usage": map[string]any{
			"input_tokens":  up.Usage.PromptTokens,
			"output_tokens": up.Usage.CompletionTokens,
			"total_tokens":  up.Usage.PromptTokens + up.Usage.CompletionTokens,
		},
		"error":              nil,
		"incomplete_details": nil,
	}
	return mustJSON(resp), dropped, nil
}

// respID 把上游返回的 ID 转成 Responses 风格（resp_ 前缀）。
func respID(upstream, fallback string) string {
	if upstream == "" {
		return fallback
	}
	s := upstream
	s = strings.TrimPrefix(s, "chatcmpl-")
	s = strings.TrimPrefix(s, "chatcmpl.")
	s = strings.TrimPrefix(s, "resp_")
	if s == "" {
		return fallback
	}
	return "resp_" + s
}
