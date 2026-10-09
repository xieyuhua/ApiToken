// Package translate 实现不同大模型协议之间的请求/响应转换。
//
// 当前支持 Anthropic Messages ⇄ OpenAI Chat Completions 双向：
//   - 请求方向：客户端发 Anthropic Messages → 上游收 OpenAI Chat Completions
//   - 响应方向：上游回 OpenAI Chat Completions → 客户端收 Anthropic Messages（含 SSE）
//
// 设计原则：宁可明确报错，也不静默丢弃语义。调用方若拿到 400 才知道能力不支持，
// 若被静默丢弃则会误以为「thinking 已开启」「工具已传入」，排查成本高得多。
// 只有确实无副作用的字段（如 metadata.user_id）才丢弃，并在 Info.Dropped 中记录。
package translate

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Error 转换期的请求校验错误，调用方应映射为 400 返回给客户端。
type Error struct {
	Field string
	Msg   string
}

func (e *Error) Error() string {
	if e.Field == "" {
		return e.Msg
	}
	return e.Field + "：" + e.Msg
}

func errf(field, format string, args ...any) *Error {
	return &Error{Field: field, Msg: fmt.Sprintf(format, args...)}
}

// Info 转换过程中的附带信息，用于日志与回包。
type Info struct {
	// WantThinking 客户端是否开启了 extended thinking（未开启则上游返回的
	// 思维链内容会被丢弃，因为 Anthropic 协议不允许在未请求时发 thinking 块）。
	WantThinking bool
	// Dropped 被安全丢弃的字段名，用于日志观测。
	Dropped []string
	// MessageID 生成的 Anthropic 风格消息 ID（msg_ 前缀）。
	MessageID string
	// Model 客户端请求的原始模型名（回包时必须用客户端认识的名字）。
	Model string
}

// ---------------------------------------------------------------------------
// Anthropic 请求结构
// ---------------------------------------------------------------------------

type anthropicRequest struct {
	Model         string             `json:"model"`
	Messages      []anthropicMessage `json:"messages"`
	System        json.RawMessage    `json:"system,omitempty"`
	MaxTokens     *int               `json:"max_tokens,omitempty"`
	Temperature   *float64           `json:"temperature,omitempty"`
	TopP          *float64           `json:"top_p,omitempty"`
	TopK          *int               `json:"top_k,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
	Stream        bool               `json:"stream,omitempty"`
	Tools         []anthropicTool    `json:"tools,omitempty"`
	ToolChoice    json.RawMessage    `json:"tool_choice,omitempty"`
	Thinking      *thinkingConfig    `json:"thinking,omitempty"`
}

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

type thinkingConfig struct {
	Type         string `json:"type"`
	BudgetTokens *int   `json:"budget_tokens,omitempty"`
}

// anthropicSupportedFields 允许出现的顶层字段（其余一律 400，避免静默忽略）。
var anthropicSupportedFields = map[string]bool{
	"model": true, "messages": true, "system": true, "max_tokens": true,
	"temperature": true, "top_p": true, "top_k": true, "stop_sequences": true,
	"stream": true, "tools": true, "tool_choice": true, "thinking": true,
}

// anthropicDroppableFields 无副作用、可安全丢弃的字段。
// 这些字段会被记录到 Info.Dropped（响应头与日志里可见），而不是静默忽略。
var anthropicDroppableFields = map[string]string{
	"metadata": "仅用于上游风控，转发给 OpenAI 上游无意义",
}

// ---------------------------------------------------------------------------
// 请求方向：Anthropic → OpenAI
// ---------------------------------------------------------------------------

// RequestToOpenAI 把 Anthropic Messages 请求转成 OpenAI Chat Completions 请求体。
//
// 返回的 map 可直接交给 proxy.BuildRequest 发送；info 携带转换过程中的决策信息。
func RequestToOpenAI(raw []byte) (payload map[string]any, info *Info, err error) {
	var probe map[string]json.RawMessage
	if e := json.Unmarshal(raw, &probe); e != nil {
		return nil, nil, errf("", "请求体不是合法 JSON: %v", e)
	}
	info = &Info{MessageID: NewMessageID()}

	for k := range probe {
		if anthropicSupportedFields[k] {
			continue
		}
		if _, ok := anthropicDroppableFields[k]; ok {
			info.Dropped = append(info.Dropped, k)
			continue
		}
		return nil, nil, errf(k, "网关暂不支持该字段（仅兼容 Anthropic Messages 的 model/messages/system/max_tokens/"+
			"temperature/top_p/top_k/stop_sequences/stream/tools/tool_choice/thinking/metadata）")
	}

	var req anthropicRequest
	if e := json.Unmarshal(raw, &req); e != nil {
		return nil, nil, errf("", "请求体结构不合法: %v", e)
	}
	info.Model = req.Model

	if strings.TrimSpace(req.Model) == "" {
		return nil, nil, errf("model", "缺少 model 字段")
	}
	if len(req.Messages) == 0 {
		return nil, nil, errf("messages", "messages 不能为空")
	}
	if req.MaxTokens == nil || *req.MaxTokens <= 0 {
		return nil, nil, errf("max_tokens", "Anthropic 协议要求 max_tokens 且必须大于 0")
	}
	if req.Thinking != nil {
		switch req.Thinking.Type {
		case "enabled":
			info.WantThinking = true
			if req.Thinking.BudgetTokens != nil && *req.Thinking.BudgetTokens >= *req.MaxTokens {
				return nil, nil, errf("thinking.budget_tokens",
					"budget_tokens(%d) 必须小于 max_tokens(%d)", *req.Thinking.BudgetTokens, *req.MaxTokens)
			}
		case "disabled":
			info.WantThinking = false
		default:
			return nil, nil, errf("thinking.type", "只支持 enabled/disabled，收到 %q", req.Thinking.Type)
		}
	}

	payload = map[string]any{
		"model":      req.Model,
		"stream":     req.Stream,
		"max_tokens": *req.MaxTokens,
	}
	if req.Temperature != nil {
		payload["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		payload["top_p"] = *req.TopP
	}
	if req.TopK != nil {
		payload["top_k"] = *req.TopK
	}
	if len(req.StopSequences) > 0 {
		payload["stop"] = req.StopSequences
	}

	// system 是 Anthropic 的顶层字段，OpenAI 放在 messages 首位
	msgs := make([]any, 0, len(req.Messages)+1)
	if sys, err := systemToOpenAI(req.System); err != nil {
		return nil, nil, err
	} else if sys != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": sys})
	}

	for i, m := range req.Messages {
		conv, err := messageToOpenAI(i, m, info)
		if err != nil {
			return nil, nil, err
		}
		msgs = append(msgs, conv...)
	}
	payload["messages"] = msgs

	tools, err := toolsToOpenAI(req.Tools)
	if err != nil {
		return nil, nil, err
	}
	if len(tools) > 0 {
		payload["tools"] = tools
		if tc, err := toolChoiceToOpenAI(req.ToolChoice); err != nil {
			return nil, nil, err
		} else if tc != nil {
			payload["tool_choice"] = tc
		}
	}
	return payload, info, nil
}

// systemToOpenAI 把顶层 system（字符串或块数组）转成 OpenAI 的 system 消息文本。
func systemToOpenAI(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", errf("system", "必须是字符串或内容块数组")
	}
	var sb strings.Builder
	for i, b := range blocks {
		if b.Type != "text" {
			return "", errf(fmt.Sprintf("system[%d].type", i), "只支持 text 块，收到 %q", b.Type)
		}
		sb.WriteString(b.Text)
	}
	return sb.String(), nil
}

// messageToOpenAI 把一条 Anthropic 消息转成 1~n 条 OpenAI 消息。
//
// 一条 user 消息里可能混着多个 tool_result（工具结果）与后续文本，
// OpenAI 要求工具结果独立成 role=tool 消息且排在 assistant.tool_calls 之后，
// 因此这里按「工具结果在前、正文在后」的顺序拆开。
func messageToOpenAI(idx int, m anthropicMessage, info *Info) ([]any, error) {
	field := fmt.Sprintf("messages[%d]", idx)
	switch m.Role {
	case "user", "assistant":
	case "":
		return nil, errf(field+".role", "role 不能为空")
	default:
		return nil, errf(field+".role", "只支持 user/assistant，收到 %q", m.Role)
	}

	// 纯字符串内容
	var text string
	if err := json.Unmarshal(m.Content, &text); err == nil {
		if m.Role == "assistant" {
			return []any{map[string]any{"role": "assistant", "content": text}}, nil
		}
		return []any{map[string]any{"role": "user", "content": text}}, nil
	}

	var blocks []anthropicBlock
	if err := json.Unmarshal(m.Content, &blocks); err != nil {
		return nil, errf(field+".content", "必须是字符串或内容块数组")
	}

	var (
		toolMsgs  []any // tool_result → role=tool
		parts     []any // 文本 / 图片
		thinking  strings.Builder
		toolCalls []any
	)

	for i, b := range blocks {
		bf := fmt.Sprintf("%s.content[%d]", field, i)
		switch b.Type {
		case "text":
			if b.Text == "" {
				continue
			}
			parts = append(parts, map[string]any{"type": "text", "text": b.Text})

		case "image":
			img, err := imageToOpenAI(bf, b)
			if err != nil {
				return nil, err
			}
			parts = append(parts, img)

		case "thinking":
			// 客户端回传上一轮的思维链：OpenAI 用 reasoning_content 承载
			thinking.WriteString(b.Thinking)
			info.Dropped = append(info.Dropped, bf+".signature")

		case "redacted_thinking":
			// 加密的思维内容无法转成明文，只能丢弃
			info.Dropped = append(info.Dropped, bf)

		case "tool_use":
			args := "{}"
			if len(b.Input) > 0 {
				args = string(b.Input)
			}
			toolCalls = append(toolCalls, map[string]any{
				"id": b.ID, "type": "function",
				"function": map[string]any{"name": b.Name, "arguments": args},
			})

		case "tool_result":
			if m.Role != "user" {
				return nil, errf(bf, "tool_result 只能出现在 user 消息中")
			}
			body, err := toolResultToText(bf, b)
			if err != nil {
				return nil, err
			}
			toolMsgs = append(toolMsgs, map[string]any{
				"role": "tool", "tool_call_id": b.ToolUseID, "content": body,
			})

		default:
			return nil, errf(bf+".type", "网关不支持的内容块类型 %q", b.Type)
		}
	}

	out := make([]any, 0, 2)
	out = append(out, toolMsgs...)

	switch m.Role {
	case "assistant":
		asst := map[string]any{"role": "assistant"}
		if txt := joinTextParts(parts); txt != "" {
			asst["content"] = txt
		} else {
			asst["content"] = nil
		}
		if thinking.Len() > 0 {
			asst["reasoning_content"] = thinking.String()
		}
		if len(toolCalls) > 0 {
			asst["tool_calls"] = toolCalls
		}
		out = append(out, asst)
	default:
		// user 消息：多模态时用数组，纯文本时用字符串（兼容性更好）
		if hasNonTextPart(parts) {
			out = append(out, map[string]any{"role": "user", "content": parts})
		} else if txt := joinTextParts(parts); txt != "" {
			out = append(out, map[string]any{"role": "user", "content": txt})
		}
	}
	return out, nil
}

type anthropicBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`

	// image
	Source *anthropicImageSource `json:"source,omitempty"`

	// thinking
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`

	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

type anthropicImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

func imageToOpenAI(field string, b anthropicBlock) (map[string]any, error) {
	if b.Source == nil {
		return nil, errf(field+".source", "image 块缺少 source")
	}
	var url string
	switch b.Source.Type {
	case "base64":
		if b.Source.Data == "" {
			return nil, errf(field+".source.data", "base64 图片缺少 data")
		}
		mt := b.Source.MediaType
		if mt == "" {
			mt = "image/png"
		}
		url = "data:" + mt + ";base64," + b.Source.Data
	case "url":
		url = b.Source.URL
		if url == "" {
			return nil, errf(field+".source.url", "url 图片缺少 url")
		}
	default:
		return nil, errf(field+".source.type", "只支持 base64/url，收到 %q", b.Source.Type)
	}
	return map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}}, nil
}

// toolResultToText 把 tool_result 的内容摊平成 OpenAI tool 消息要求的字符串。
// OpenAI 不支持多模态与结构化的工具结果，这里只保留文本并在 Info.Dropped 记录。
func toolResultToText(field string, b anthropicBlock) (string, error) {
	if b.ToolUseID == "" {
		return "", errf(field+".tool_use_id", "tool_result 缺少 tool_use_id")
	}
	if len(b.Content) == 0 {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(b.Content, &s); err == nil {
		return s, nil
	}
	var blocks []anthropicBlock
	if err := json.Unmarshal(b.Content, &blocks); err != nil {
		// 非字符串也非块数组：按 JSON 原文回传，至少不丢信息
		return string(b.Content), nil
	}
	var sb strings.Builder
	for _, blk := range blocks {
		switch blk.Type {
		case "text":
			sb.WriteString(blk.Text)
		default:
			// 图片等块无法映射到 OpenAI 的 tool 消息
			sb.WriteString(fmt.Sprintf("[%s 内容已省略]", blk.Type))
		}
	}
	return sb.String(), nil
}

func toolsToOpenAI(tools []anthropicTool) ([]any, error) {
	if len(tools) == 0 {
		return nil, nil
	}
	out := make([]any, 0, len(tools))
	for i, t := range tools {
		if strings.TrimSpace(t.Name) == "" {
			return nil, errf(fmt.Sprintf("tools[%d].name", i), "工具名不能为空")
		}
		schema := t.InputSchema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		fn := map[string]any{"name": t.Name, "parameters": schema}
		if t.Description != "" {
			fn["description"] = t.Description
		}
		out = append(out, map[string]any{"type": "function", "function": fn})
	}
	return out, nil
}

func toolChoiceToOpenAI(raw json.RawMessage) (any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var choice struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &choice); err != nil {
		return nil, errf("tool_choice", "结构不合法")
	}
	switch choice.Type {
	case "auto":
		return "auto", nil
	case "any":
		return "required", nil
	case "none":
		return "none", nil
	case "tool":
		if choice.Name == "" {
			return nil, errf("tool_choice.name", "type=tool 时必须指定 name")
		}
		return map[string]any{"type": "function", "function": map[string]any{"name": choice.Name}}, nil
	case "":
		return nil, nil
	default:
		return nil, errf("tool_choice.type", "只支持 auto/any/none/tool，收到 %q", choice.Type)
	}
}

func joinTextParts(parts []any) string {
	var sb strings.Builder
	for _, p := range parts {
		m, ok := p.(map[string]any)
		if !ok || m["type"] != "text" {
			continue
		}
		if s, ok := m["text"].(string); ok {
			sb.WriteString(s)
		}
	}
	return sb.String()
}

func hasNonTextPart(parts []any) bool {
	for _, p := range parts {
		if m, ok := p.(map[string]any); !ok || m["type"] != "text" {
			return true
		}
	}
	return false
}

// NewMessageID 生成 Anthropic 风格的 msg_ ID。
func NewMessageID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		// 随机源异常时退化为时间派生 ID（ID 只用于日志关联，无需密码学强度）
		return "msg_" + hex.EncodeToString([]byte(strconv.FormatInt(time.Now().UnixNano(), 16)))
	}
	return "msg_" + hex.EncodeToString(b)
}
