package translate

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ResponsesInfo Responses 协议转换的附带信息，用于日志与回包。
type ResponsesInfo struct {
	// Model 客户端请求的原始模型名（回包时必须用客户端认识的名字）。
	Model string
	// ResponseID 生成的 Responses 风格响应 ID（resp_ 前缀）。
	ResponseID string
	// WantReasoning 客户端是否请求了 reasoning（reasoning 字段存在即视为开启）。
	WantReasoning bool
	// Dropped 被安全丢弃的字段名，用于日志观测。
	Dropped []string
}

// ---------------------------------------------------------------------------
// Responses 请求结构
// ---------------------------------------------------------------------------

type responsesRequest struct {
	Model             string          `json:"model"`
	Input             json.RawMessage `json:"input"`
	Instructions      json.RawMessage `json:"instructions,omitempty"`
	MaxOutputTokens   *int            `json:"max_output_tokens,omitempty"`
	Temperature       *float64        `json:"temperature,omitempty"`
	TopP              *float64        `json:"top_p,omitempty"`
	Stream            bool            `json:"stream,omitempty"`
	Tools             []responsesTool `json:"tools,omitempty"`
	ToolChoice        json.RawMessage `json:"tool_choice,omitempty"`
	Reasoning         json.RawMessage `json:"reasoning,omitempty"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls,omitempty"`
}

// responsesTool Responses 协议的 function 工具（扁平结构，无外层 function 包裹）。
type responsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

// inputItem input 数组中的一项。
type inputItem struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	CallID    string          `json:"call_id"`
	Name      string          `json:"name"`
	Arguments string          `json:"arguments"`
	Output    json.RawMessage `json:"output"`
}

// contentBlock message.content 数组里的一个块。
type contentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ImageURL  json.RawMessage `json:"image_url"`
	CallID    string          `json:"call_id"`
	Name      string          `json:"name"`
	Arguments string          `json:"arguments"`
	Summary   []any           `json:"summary"`
	Content   json.RawMessage `json:"content"`
}

// responsesSupportedFields 允许出现的顶层字段（其余一律 400，避免静默忽略）。
var responsesSupportedFields = map[string]bool{
	"model": true, "input": true, "instructions": true, "max_output_tokens": true,
	"temperature": true, "top_p": true, "stream": true, "tools": true,
	"tool_choice": true, "reasoning": true, "parallel_tool_calls": true,
	"truncation": true, "stream_options": true, "background": true,
}

// responsesDroppableFields 无副作用、可安全丢弃的字段，会记入 Info.Dropped。
var responsesDroppableFields = map[string]string{
	"store":                "网关不存储对话历史",
	"metadata":             "仅用于上游风控，转发无意义",
	"service_tier":         "优先级无法透传",
	"include":              "响应额外字段无法映射",
	"user":                 "OpenAI 兼容上游不识别",
	"previous_response_id": "网关不做对话状态关联",
	"prompt_cache_key":     "缓存键无意义",
}

// ---------------------------------------------------------------------------
// 请求方向：Responses → OpenAI Chat Completions
// ---------------------------------------------------------------------------

// RequestToChatCompletions 把 Responses API 请求转成 OpenAI Chat Completions 请求体。
//
// 返回的 map 可直接交给 proxy.BuildRequest 发送；info 携带转换决策信息。
func RequestToChatCompletions(raw []byte) (payload map[string]any, info *ResponsesInfo, err error) {
	var probe map[string]json.RawMessage
	if e := json.Unmarshal(raw, &probe); e != nil {
		return nil, nil, errf("", "请求体不是合法 JSON: %v", e)
	}
	info = &ResponsesInfo{ResponseID: NewResponseID()}

	for k := range probe {
		if responsesSupportedFields[k] {
			continue
		}
		if _, ok := responsesDroppableFields[k]; ok {
			info.Dropped = append(info.Dropped, k)
			continue
		}
		return nil, nil, errf(k, "网关暂不支持该字段（仅兼容 Responses API 的 model/input/instructions/"+
			"max_output_tokens/temperature/top_p/stream/tools/tool_choice/reasoning/parallel_tool_calls 等）")
	}

	var req responsesRequest
	if e := json.Unmarshal(raw, &req); e != nil {
		return nil, nil, errf("", "请求体结构不合法: %v", e)
	}
	info.Model = req.Model

	if strings.TrimSpace(req.Model) == "" {
		return nil, nil, errf("model", "缺少 model 字段")
	}
	if len(req.Input) == 0 || string(req.Input) == "null" {
		return nil, nil, errf("input", "input 不能为空")
	}

	payload = map[string]any{
		"model":  req.Model,
		"stream": req.Stream,
	}
	if req.MaxOutputTokens != nil && *req.MaxOutputTokens > 0 {
		payload["max_tokens"] = *req.MaxOutputTokens
	}
	if req.Temperature != nil {
		payload["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		payload["top_p"] = *req.TopP
	}
	if req.ParallelToolCalls != nil {
		payload["parallel_tool_calls"] = *req.ParallelToolCalls
	}

	// reasoning.effort → OpenAI 的 reasoning_effort；存在 reasoning 即认为客户端想要推理
	if len(req.Reasoning) > 0 {
		info.WantReasoning = true
		if effort, e := reasoningEffort(req.Reasoning); e != nil {
			return nil, nil, e
		} else if effort != "" {
			payload["reasoning_effort"] = effort
		}
	}

	// instructions → 首条 system 消息
	msgs := make([]any, 0, 8)
	if sys, e := instructionsToText(req.Instructions); e != nil {
		return nil, nil, e
	} else if sys != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": sys})
	}

	conv, e := inputToMessages(req.Input, info)
	if e != nil {
		return nil, nil, e
	}
	msgs = append(msgs, conv...)
	payload["messages"] = msgs

	tools, e := responsesToolsToOpenAI(req.Tools)
	if e != nil {
		return nil, nil, e
	}
	if len(tools) > 0 {
		payload["tools"] = tools
		if tc, e := responsesToolChoiceToOpenAI(req.ToolChoice); e != nil {
			return nil, nil, e
		} else if tc != nil {
			payload["tool_choice"] = tc
		}
	}
	return payload, info, nil
}

// reasoningEffort 提取 reasoning.effort（low/medium/high），非法值报错。
func reasoningEffort(raw json.RawMessage) (string, error) {
	var r struct {
		Effort string `json:"effort"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return "", errf("reasoning", "结构不合法")
	}
	switch r.Effort {
	case "", "low", "medium", "high":
		return r.Effort, nil
	default:
		return "", errf("reasoning.effort", "只支持 low/medium/high，收到 %q", r.Effort)
	}
}

// instructionsToText 把 instructions（字符串）转成纯文本。
func instructionsToText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", errf("instructions", "必须是字符串")
	}
	return s, nil
}

// inputToMessages 把 Responses 的 input 转成 OpenAI messages。
func inputToMessages(input json.RawMessage, info *ResponsesInfo) ([]any, error) {
	var s string
	if err := json.Unmarshal(input, &s); err == nil {
		if strings.TrimSpace(s) == "" {
			return nil, errf("input", "input 不能为空字符串")
		}
		return []any{map[string]any{"role": "user", "content": s}}, nil
	}

	var items []inputItem
	if err := json.Unmarshal(input, &items); err != nil {
		return nil, errf("input", "必须是字符串或 item 数组")
	}
	if len(items) == 0 {
		return nil, errf("input", "input 数组不能为空")
	}

	out := make([]any, 0, len(items))
	for i, it := range items {
		field := fmt.Sprintf("input[%d]", i)
		switch it.Type {
		case "message":
			conv, err := messageItemToOpenAI(field, it, info)
			if err != nil {
				return nil, err
			}
			out = append(out, conv)
		case "function_call_output":
			body, err := functionCallOutputToText(it.Output)
			if err != nil {
				return nil, err
			}
			if it.CallID == "" {
				return nil, errf(field+".call_id", "function_call_output 缺少 call_id")
			}
			out = append(out, map[string]any{
				"role": "tool", "tool_call_id": it.CallID, "content": body,
			})
		case "function_call":
			out = append(out, standaloneFunctionCallToOpenAI(field, it))
		default:
			return nil, errf(field+".type", "网关不支持的 input 项类型 %q", it.Type)
		}
	}
	return out, nil
}

// messageItemToOpenAI 把一条 message 项转成 1 条 OpenAI 消息。
func messageItemToOpenAI(field string, it inputItem, info *ResponsesInfo) (any, error) {
	role := it.Role
	switch role {
	case "user", "assistant", "system", "developer":
	case "":
		return nil, errf(field+".role", "role 不能为空")
	default:
		return nil, errf(field+".role", "只支持 user/assistant/system/developer，收到 %q", role)
	}

	// 纯字符串内容
	var text string
	if err := json.Unmarshal(it.Content, &text); err == nil {
		return map[string]any{"role": role, "content": text}, nil
	}

	var blocks []contentBlock
	if err := json.Unmarshal(it.Content, &blocks); err != nil {
		return nil, errf(field+".content", "必须是字符串或内容块数组")
	}

	if role == "assistant" {
		return assistantMessageToOpenAI(field, blocks, info), nil
	}
	return userMessageToOpenAI(role, field, blocks), nil
}

// userMessageToOpenAI 处理 user/system/developer 消息的内容块（文本 / 图片）。
func userMessageToOpenAI(role, field string, blocks []contentBlock) any {
	parts := make([]any, 0, len(blocks))
	for i, b := range blocks {
		bf := fmt.Sprintf("%s.content[%d]", field, i)
		switch b.Type {
		case "input_text", "output_text", "text":
			if b.Text == "" {
				continue
			}
			parts = append(parts, map[string]any{"type": "text", "text": b.Text})
		case "input_image":
			parts = append(parts, imageURLToOpenAI(bf, b.ImageURL))
		default:
			// 未知块类型不静默丢弃：保留类型线索，便于排查
			parts = append(parts, map[string]any{"type": "text", "text": fmt.Sprintf("[%s]", b.Type)})
		}
	}
	// 单个纯文本时用字符串 content，兼容性更好
	if len(parts) == 1 {
		if m, ok := parts[0].(map[string]any); ok && m["type"] == "text" {
			return map[string]any{"role": role, "content": m["text"]}
		}
	}
	return map[string]any{"role": role, "content": parts}
}

// assistantMessageToOpenAI 处理 assistant 消息：文本 + 思维链 + 工具调用。
func assistantMessageToOpenAI(field string, blocks []contentBlock, info *ResponsesInfo) any {
	asst := map[string]any{"role": "assistant"}
	var text, reasoning strings.Builder
	var toolCalls []any

	for i, b := range blocks {
		bf := fmt.Sprintf("%s.content[%d]", field, i)
		switch b.Type {
		case "output_text", "text":
			text.WriteString(b.Text)
		case "reasoning":
			reasoning.WriteString(reasoningToText(b.Summary, b.Content))
		case "function_call":
			args := b.Arguments
			if args == "" {
				args = "{}"
			}
			toolCalls = append(toolCalls, map[string]any{
				"id": b.CallID, "type": "function",
				"function": map[string]any{"name": b.Name, "arguments": args},
			})
		default:
			info.Dropped = append(info.Dropped, bf+".type="+b.Type)
		}
	}

	if text.Len() > 0 {
		asst["content"] = text.String()
	} else {
		asst["content"] = nil
	}
	if reasoning.Len() > 0 {
		asst["reasoning_content"] = reasoning.String()
	}
	if len(toolCalls) > 0 {
		asst["tool_calls"] = toolCalls
	}
	return asst
}

// standaloneFunctionCallToOpenAI 处理独立的 function_call item（视作 assistant 工具调用）。
func standaloneFunctionCallToOpenAI(field string, it inputItem) any {
	args := it.Arguments
	if args == "" {
		args = "{}"
	}
	return map[string]any{
		"role": "assistant",
		"tool_calls": []any{map[string]any{
			"id": it.CallID, "type": "function",
			"function": map[string]any{"name": it.Name, "arguments": args},
		}},
	}
}

// functionCallOutputToText 把 function_call_output 的 output 摊平成字符串。
func functionCallOutputToText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
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
		// 非字符串也非块数组：按 JSON 原文回传，至少不丢信息
		return string(raw), nil
	}
	var sb strings.Builder
	for _, b := range blocks {
		switch b.Type {
		case "output_text", "text", "":
			sb.WriteString(b.Text)
		default:
			sb.WriteString(fmt.Sprintf("[%s]", b.Type))
		}
	}
	return sb.String(), nil
}

// reasoningToText 从 reasoning 块的 summary/content 提取文本。
func reasoningToText(summary []any, content json.RawMessage) string {
	var sb strings.Builder
	for _, s := range summary {
		if m, ok := s.(map[string]any); ok {
			if t, ok := m["text"].(string); ok {
				sb.WriteString(t)
			}
		}
	}
	if sb.Len() > 0 {
		return sb.String()
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(content, &blocks); err == nil {
		for _, b := range blocks {
			if b.Type == "summary_text" || b.Type == "text" {
				sb.WriteString(b.Text)
			}
		}
	}
	return sb.String()
}

// imageURLToOpenAI 把 input_image.image_url 转成 OpenAI 的 image_url 块。
// image_url 可能是字符串，也可能是 {"url": "..."} 对象。
func imageURLToOpenAI(field string, raw json.RawMessage) map[string]any {
	url := imageURLString(raw)
	if url == "" {
		url = string(raw)
	}
	return map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}}
}

func imageURLString(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var obj struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil {
		return obj.URL
	}
	return ""
}

// responsesToolsToOpenAI 把 Responses 的 function 工具转成 OpenAI function 工具。
func responsesToolsToOpenAI(tools []responsesTool) ([]any, error) {
	if len(tools) == 0 {
		return nil, nil
	}
	out := make([]any, 0, len(tools))
	for i, t := range tools {
		if t.Type != "function" {
			return nil, errf(fmt.Sprintf("tools[%d].type", i), "只支持 function 工具，收到 %q", t.Type)
		}
		if strings.TrimSpace(t.Name) == "" {
			return nil, errf(fmt.Sprintf("tools[%d].name", i), "工具名不能为空")
		}
		params := t.Parameters
		if len(params) == 0 || string(params) == "null" {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		fn := map[string]any{"name": t.Name, "parameters": params}
		if t.Description != "" {
			fn["description"] = t.Description
		}
		if t.Strict != nil {
			fn["strict"] = *t.Strict
		}
		out = append(out, map[string]any{"type": "function", "function": fn})
	}
	return out, nil
}

// responsesToolChoiceToOpenAI 把 Responses 的 tool_choice 转成 OpenAI 格式。
func responsesToolChoiceToOpenAI(raw json.RawMessage) (any, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch s {
		case "auto", "none", "required":
			return s, nil
		case "":
			return nil, nil
		default:
			return nil, errf("tool_choice", "只支持 auto/none/required，收到 %q", s)
		}
	}
	var choice struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &choice); err != nil {
		return nil, errf("tool_choice", "结构不合法")
	}
	switch choice.Type {
	case "function":
		if choice.Name == "" {
			return nil, errf("tool_choice.name", "type=function 时必须指定 name")
		}
		return map[string]any{"type": "function", "function": map[string]any{"name": choice.Name}}, nil
	default:
		return nil, errf("tool_choice.type", "只支持 function，收到 %q", choice.Type)
	}
}
