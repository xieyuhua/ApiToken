package proxy

import (
	"encoding/json"
	"fmt"
	"strings"
)

// relayResult 单次上游调用的结果（用于日志记录）。
type relayResult struct {
	Usage    Usage
	Snippet  string // 响应内容摘要
	Endpoint string // 实际请求的上游地址
}

// payloadLimit 摘要长度上限。
func (h *Handler) payloadLimit() int {
	if h.cfg.Logging.PayloadLimit <= 0 {
		return 2000
	}
	return h.cfg.RT().PayloadLimit
}

// recordPayload 是否记录请求/响应摘要。
func (h *Handler) recordPayload() bool { return h.cfg.RT().RecordPayload }

// requestSnippet 生成请求摘要：模型 + 最后一条消息内容。
func (h *Handler) requestSnippet(payload map[string]any) string {
	if !h.recordPayload() {
		return ""
	}
	var sb strings.Builder
	if m, ok := payload["model"].(string); ok {
		sb.WriteString("model=")
		sb.WriteString(m)
	}
	if s, ok := payload["stream"].(bool); ok && s {
		sb.WriteString(" stream=true")
	}
	msgs, _ := payload["messages"].([]any)
	last := ""
	if n := len(msgs); n > 0 {
		last = contentText(msgs[n-1])
	}
	if last != "" {
		sb.WriteString("\n")
		sb.WriteString(truncate(last, h.payloadLimit()))
	}
	return truncate(sb.String(), h.payloadLimit())
}

// responseSnippet 从非流式响应中提取内容摘要。
func (h *Handler) responseSnippet(data []byte) string {
	if !h.recordPayload() {
		return ""
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &out); err == nil && len(out.Choices) > 0 {
		if c := out.Choices[0].Message.Content; c != "" {
			return truncate(c, h.payloadLimit())
		}
	}
	return truncate(string(data), h.payloadLimit()/2)
}

// contentText 兼容字符串与多模态数组两种 content 形式。
func contentText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case map[string]any:
		if s, ok := t["content"].(string); ok {
			return s
		}
		return textFromParts(t["content"])
	case []any:
		return textFromParts(t)
	default:
		return ""
	}
}

func textFromParts(v any) string {
	arr, ok := v.([]any)
	if !ok {
		return ""
	}
	var sb strings.Builder
	for _, p := range arr {
		m, ok := p.(map[string]any)
		if !ok {
			continue
		}
		if s, ok := m["text"].(string); ok {
			sb.WriteString(s)
		}
	}
	return sb.String()
}

// streamDelta 提取流式增量文本。
func streamDelta(line string) string {
	line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "data:"))
	if line == "" || line == "[DONE]" {
		return ""
	}
	var out struct {
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(line), &out); err != nil || len(out.Choices) == 0 {
		return ""
	}
	return out.Choices[0].Delta.Content
}

// describeError 生成简洁错误摘要。
func describeError(body []byte, status int) string {
	var out struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    any    `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &out); err == nil && out.Error.Message != "" {
		return fmt.Sprintf("%d %s: %s", status, out.Error.Type, out.Error.Message)
	}
	return truncate(string(body), 300)
}
