package proxy

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/xieyuhua/apitoken/internal/model"
	"github.com/xieyuhua/apitoken/internal/proxy/translate"
)

// Responses POST /v1/responses —— OpenAI Responses 协议入口（Codex CLI 等 coding agent 使用）。
//
// 与 /v1/messages 同理：让只懂 Responses 协议的客户端（OpenAI Codex CLI、各种使用
// Responses API 的 agent）也能用上网关的路由、故障转移、用量统计与调用日志。上游仍按
// 各渠道配置的 OpenAI 兼容端点转发，因此不需要上游原生支持 Responses 协议。
//
// 鉴权沿用网关密钥（Authorization: Bearer）。
func (h *Handler) Responses(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	reqID := requestIDOf(r)
	entry := model.LogEntry{
		Time: start, RequestID: reqID, Method: r.Method, Path: r.URL.Path,
		ClientIP: clientIP(r, h.cfg.RT().TrustProxyHeaders), UserAgent: logUA(r),
	}

	if r.Method != http.MethodPost {
		WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "仅支持 POST", "")
		return
	}

	maxBody := h.currentMaxBody()
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		WriteError(w, http.StatusBadRequest, "read_body_failed", "读取请求体失败: "+err.Error(), "")
		return
	}
	// 读满上限仍可能有剩余：明确按 413 拒绝，而不是静默截断后报「非法 JSON」
	if int64(len(raw)) > maxBody {
		WriteError(w, http.StatusRequestEntityTooLarge, "request_too_large",
			fmt.Sprintf("请求体超过 %dMB 上限", maxBody>>20), "")
		return
	}

	t0 := time.Now()
	payload, info, err := translate.RequestToChatCompletions(raw)
	if err != nil {
		// 严格校验：不支持的字段直接 400，避免静默丢弃后让调用方误判能力生效
		var te *translate.Error
		if errors.As(err, &te) {
			WriteError(w, http.StatusBadRequest, "invalid_request_error", te.Error(), "")
			return
		}
		WriteError(w, http.StatusBadRequest, "invalid_request_error", err.Error(), "")
		return
	}
	convertMs := time.Since(t0).Milliseconds()

	stream, _ := payload["stream"].(bool)
	modelName := info.Model
	entry.Model = modelName
	entry.Stream = stream
	entry.RequestSnippet = h.requestSnippet(payload)

	resp := &responsesResponder{info: info}
	resp.addConvert(time.Duration(convertMs) * time.Millisecond)

	if len(info.Dropped) > 0 {
		h.log.Info("protocol_convert",
			"model", modelName, "direction", "request", "convert_ms", convertMs,
			"dropped", strings.Join(info.Dropped, ","))
	}

	h.serve(w, r, relayRequest{
		Payload:  payload,
		Model:    modelName,
		Stream:   stream,
		Entry:    entry,
		Resp:     resp,
		WriteErr: WriteError,
		// Responses 的用量承载在 response.completed / usage 里，流式也需强制开启 include_usage
		BuildOpts: BuildOpts{ForceUsage: true},
	})
}
