package proxy

import (
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/demo1/apitoken/internal/model"
	"github.com/demo1/apitoken/internal/proxy/translate"
)

// Messages POST /v1/messages —— Anthropic Messages 协议入口。
//
// 存在的意义：让只懂 Anthropic 协议的客户端（Claude Code、Cline 的 Anthropic 模式等）
// 也能用上网关的路由、故障转移、用量统计与调用日志。上游仍按各渠道配置的 OpenAI 兼容
// 端点转发，因此不需要任何上游支持 Anthropic 原生协议。
//
// 鉴权沿用网关密钥，同时接受 Authorization: Bearer 与 x-api-key（Anthropic 客户端用后者）。
func (h *Handler) Messages(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	reqID := requestIDOf(r)
	entry := model.LogEntry{
		Time: start, RequestID: reqID, Method: r.Method, Path: r.URL.Path,
		ClientIP: clientIP(r),
	}

	if r.Method != http.MethodPost {
		writeAnthropicError(w, http.StatusMethodNotAllowed, "invalid_request_error", "仅支持 POST")
		return
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, h.currentMaxBody()))
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "读取请求体失败: "+err.Error())
		return
	}

	t0 := time.Now()
	payload, info, err := translate.RequestToOpenAI(raw)
	if err != nil {
		// 严格校验：不支持的字段直接 400，避免静默丢弃后让调用方误判能力生效
		var te *translate.Error
		if errors.As(err, &te) {
			writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", te.Error())
			return
		}
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	convertMs := time.Since(t0).Milliseconds()

	stream, _ := payload["stream"].(bool)
	modelName := info.Model
	entry.Model = modelName
	entry.Stream = stream
	entry.RequestSnippet = h.requestSnippet(payload)

	resp := &anthropicResponder{info: info}
	resp.addConvert(time.Duration(convertMs) * time.Millisecond)

	if len(info.Dropped) > 0 {
		h.log.Info("protocol_convert",
			"model", modelName, "direction", "request", "convert_ms", convertMs,
			"dropped", joinFields(info.Dropped))
	}

	h.serve(w, r, relayRequest{
		Payload:  payload,
		Model:    modelName,
		Stream:   stream,
		Entry:    entry,
		Resp:     resp,
		WriteErr: anthropicWriteError,
		// Anthropic 用量只在流式末尾的数据块里，必须强制开启
		BuildOpts: BuildOpts{ForceUsage: true},
	})
}

// anthropicWriteError 以 Anthropic 错误结构回包。
func anthropicWriteError(w http.ResponseWriter, status int, code, message, typ string) {
	if typ == "" {
		typ = anthropicErrorType(status)
	}
	_ = code
	writeAnthropicError(w, status, typ, message)
}

func joinFields(v []string) string {
	out := ""
	for i, s := range v {
		if i > 0 {
			out += ","
		}
		out += s
	}
	return out
}
