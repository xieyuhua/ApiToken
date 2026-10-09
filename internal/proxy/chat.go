package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/demo1/apitoken/internal/config"
	"github.com/demo1/apitoken/internal/model"
	"github.com/demo1/apitoken/internal/store"
)

// Handler OpenAI 兼容代理。
type Handler struct {
	store  *store.Store
	cfg    *config.Config
	log    *slog.Logger
	client *Client
}

// currentMaxBody 当前允许的请求体上限（支持运行时调整）。
func (h *Handler) currentMaxBody() int64 {
	if n := h.cfg.RT().MaxBodyBytes; n > 0 {
		return n
	}
	return 32 << 20
}

// NewHandler 构造代理处理器。
func NewHandler(st *store.Store, cfg *config.Config, log *slog.Logger) *Handler {
	return &Handler{
		store:  st,
		cfg:    cfg,
		log:    log,
		client: NewClient(cfg),
	}
}

// relayRequest 描述一次转发的入站上下文。
//
// /v1/chat/completions 与 /v1/messages 共用同一套渠道候选、鉴权、故障转移与日志逻辑，
// 差异只在三处：请求体如何解析、响应如何写回、出错时用什么错误结构。
// 新增入站协议时只需实现 responder，不必再复制一遍故障转移循环。
type relayRequest struct {
	// Payload 已转换为 OpenAI 格式的请求体。
	Payload map[string]any
	// Model 客户端请求的原始模型名（用于日志与错误提示）。
	Model string
	// Stream 是否为流式。
	Stream bool
	// Entry 日志条目的初始字段。
	Entry model.LogEntry
	// Resp 响应写回策略。
	Resp responder
	// WriteErr 出错时的回包函数（不同协议的错误体结构不同）。
	WriteErr func(w http.ResponseWriter, status int, code, message, typ string)
	// BuildOpts 上游请求构造选项。
	BuildOpts BuildOpts
}

// ChatCompletions POST /v1/chat/completions
func (h *Handler) ChatCompletions(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	reqID := requestIDOf(r)
	entry := model.LogEntry{
		Time: start, RequestID: reqID, Method: r.Method, Path: r.URL.Path,
		ClientIP: clientIP(r),
	}

	if r.Method != http.MethodPost {
		WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "仅支持 POST", "")
		return
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, h.currentMaxBody()))
	if err != nil {
		WriteError(w, http.StatusBadRequest, "read_body_failed", "读取请求体失败: "+err.Error(), "")
		return
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		WriteError(w, http.StatusBadRequest, "invalid_json", "请求体不是合法 JSON: "+err.Error(), "")
		return
	}
	modelName, _ := payload["model"].(string)
	entry.Model = modelName
	if modelName == "" {
		WriteError(w, http.StatusBadRequest, "missing_model", "缺少 model 字段", "")
		return
	}
	stream, _ := payload["stream"].(bool)
	entry.Stream = stream
	entry.RequestSnippet = h.requestSnippet(payload)

	h.serve(w, r, relayRequest{
		Payload:  payload,
		Model:    modelName,
		Stream:   stream,
		Entry:    entry,
		Resp:     openAIResponder{},
		WriteErr: WriteError,
	})
}

// serve 是两个入站协议共用的转发核心：渠道候选、故障转移、统计与访问日志都在这里。
func (h *Handler) serve(w http.ResponseWriter, r *http.Request, in relayRequest) {
	start := time.Now()
	entry := in.Entry
	entry.Time = start
	reqID := entry.RequestID
	payload := in.Payload
	modelName := in.Model
	stream := in.Stream

	candidates := h.store.Candidates(modelName)
	if len(candidates) == 0 {
		entry.Status = http.StatusNotFound
		entry.LatencyMS = time.Since(start).Milliseconds()
		entry.Error = "无可用渠道"
		h.finish(entry)
		in.WriteErr(w, http.StatusNotFound, "model_not_found",
			fmt.Sprintf("模型 %q 没有可用的渠道，请先在管理端添加/启用渠道", modelName), "")
		return
	}

	limit := h.cfg.RT().MaxAttempts
	if limit <= 0 || limit > len(candidates) {
		limit = len(candidates)
	}

	var lastErr *upstreamError

	for i := 0; i < limit; i++ {
		cand := candidates[i]
		entry.Attempts = i + 1
		attStart := time.Now()

		res, uerr := h.attempt(w, r, reqID, cand, payload, stream, in.Resp, in.BuildOpts)
		latency := time.Since(attStart)
		if uerr == nil {
			// 成功路径：渠道统计与请求级统计合并为一次加锁写入
			h.store.RecordFinish(cand.ID, modelName, true, time.Since(start), latency,
				res.Usage.Prompt, res.Usage.Completion, "")
			entry.Error = ""
			entry.Status = http.StatusOK
			entry.ChannelID = cand.ID
			entry.ChannelName = cand.Name
			entry.UpstreamModel = cand.UpstreamModel
			entry.Endpoint = res.Endpoint
			entry.ResponseSnippet = res.Snippet
			entry.ResponseSnippetFull = res.SnippetFull
			entry.ResponseCapped = res.SnippetCapped
			entry.PromptTokens = res.Usage.Prompt
			entry.CompletionTokens = res.Usage.Completion
			entry.LatencyMS = time.Since(start).Milliseconds()
			h.logConvert(in.Resp, modelName, cand.ID)
			h.finish(entry)
			return
		}

		lastErr = uerr
		h.store.RecordChannelAttempt(cand.ID, false, latency, 0, 0, uerr.Error())
		entry.ChannelID = cand.ID
		entry.ChannelName = cand.Name
		entry.UpstreamModel = cand.UpstreamModel
		entry.Endpoint = res.Endpoint
		entry.Error = uerr.Error()
		// 每次上游尝试单独记录一条日志，便于在 /logs 页面追踪故障转移过程
		attemptEntry := entry
		attemptEntry.Time = time.Now()
		attemptEntry.Status = statusOf(uerr)
		attemptEntry.LatencyMS = latency.Milliseconds()
		attemptEntry.ResponseSnippet = "" // 失败原因只放在 Error 字段
		h.finish(attemptEntry)

		if !uerr.retryable(h.cfg.RT().RetryStatus) {
			break
		}
		if r.Context().Err() != nil {
			break
		}
		if i < limit-1 {
			h.log.Warn("渠道调用失败，尝试故障转移",
				"request_id", reqID, "model", modelName, "channel", cand.ID,
				"status", uerr.Status, "err", uerr.Error(), "next", candidates[i+1].ID)
		}
	}

	entry.Status = statusOf(lastErr)
	entry.LatencyMS = time.Since(start).Milliseconds()
	h.store.RecordRequest(modelName, false, time.Since(start), 0, 0)
	if entry.Attempts == 0 {
		// 未真正发起上游调用（如无可用候选）时补一条请求级日志
		h.finish(entry)
	}

	if lastErr == nil {
		in.WriteErr(w, http.StatusBadGateway, "no_channel", "没有可用渠道", "")
		return
	}
	if lastErr.Transport != nil && r.Context().Err() == nil {
		if lastErr.ChannelID != "" {
			w.Header().Set("X-Gateway-Channel", lastErr.ChannelID)
		}
		in.WriteErr(w, http.StatusBadGateway, "upstream_error", lastErr.Error(), "")
		return
	}
	// 上游返回了错误状态码：按入站协议的错误结构回包，便于客户端正确解析
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Request-Id", reqID)
	if lastErr.ChannelID != "" {
		w.Header().Set("X-Gateway-Channel", lastErr.ChannelID)
	}
	if lastErr.UpstreamModel != "" {
		w.Header().Set("X-Gateway-Upstream-Model", lastErr.UpstreamModel)
	}
	if isAnthropic(in.Resp) {
		// Anthropic 客户端按 error.type 分支处理，拿到 OpenAI 结构会显示成未知错误
		w.WriteHeader(lastErr.Status)
		writeAnthropicError(w, lastErr.Status, anthropicErrorType(lastErr.Status),
			describeError(lastErr.Body, lastErr.Status))
		return
	}
	w.WriteHeader(lastErr.Status)
	body := lastErr.Body
	if len(body) == 0 {
		body = []byte(`{"error":{"message":"upstream error","type":"api_error"}}`)
	}
	_, _ = w.Write(body)
}

// logConvert 记录协议转换耗时与被丢弃的字段（仅发生转换时输出）。
func (h *Handler) logConvert(resp responder, modelName, channelID string) {
	ar, ok := resp.(*anthropicResponder)
	if !ok {
		return
	}
	attrs := []any{"model", modelName, "channel", channelID, "convert_ms", ar.ConvertMs()}
	if len(ar.dropped) > 0 {
		attrs = append(attrs, "dropped", strings.Join(ar.dropped, ","))
	}
	h.log.Info("protocol_convert", attrs...)
}

// withClientUA 把入站请求的 User-Agent 带上，其余选项原样保留。
// 每次故障转移都复用同一个客户端 UA。
func withClientUA(opts BuildOpts, r *http.Request) BuildOpts {
	if r == nil {
		return opts
	}
	opts.UserAgent = r.Header.Get("User-Agent")
	return opts
}

// attempt 向单个渠道发起请求，成功时写出响应（返回用量与摘要信息）。
func (h *Handler) attempt(w http.ResponseWriter, r *http.Request, reqID string, cand model.Candidate,
	payload map[string]any, stream bool, resp responder, opts BuildOpts) (res relayResult, uerr *upstreamError) {

	timeout := timeoutFor(h.cfg, cand.Channel, stream)
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	req, err := h.BuildRequestOpts(ctx, cand, payload, stream, withClientUA(opts, r))
	if err != nil {
		return res, &upstreamError{Transport: err, ChannelID: cand.ID}
	}
	req.Header.Set("X-Request-Id", reqID)
	res.Endpoint = req.URL.String()
	defer func() {
		if uerr != nil {
			uerr.ChannelID = cand.ID
			uerr.Endpoint = res.Endpoint
			uerr.UpstreamModel = cand.UpstreamModel
		}
	}()

	httpResp, err := h.client.Do(req)
	if err != nil {
		return res, &upstreamError{Transport: err, ChannelID: cand.ID}
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(httpResp.Body, 16<<10))
		res.Snippet = describeError(body, httpResp.StatusCode)
		return res, &upstreamError{Status: httpResp.StatusCode, Body: body, ChannelID: cand.ID}
	}

	var rerr *upstreamError
	if stream {
		res, rerr = h.relayStream(w, httpResp, cand, reqID, resp)
	} else {
		res, rerr = h.relayJSON(w, httpResp, reqID, cand, resp)
	}
	res.Endpoint = req.URL.String()
	return res, rerr
}

func (h *Handler) relayJSON(w http.ResponseWriter, resp *http.Response, reqID string, cand model.Candidate,
	rp responder) (relayResult, *upstreamError) {

	var res relayResult
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return res, &upstreamError{Transport: err}
	}
	res.Usage = ParseUsage(data)
	// 摘要取上游原文（转换前的诊断价值更高）
	res.Snippet, res.SnippetFull = h.responseSnippet(data)
	w.Header().Set("X-Request-Id", reqID)
	rp.JSON(w, resp.StatusCode, data, resp.Header, cand)
	return res, nil
}

func (h *Handler) relayStream(w http.ResponseWriter, resp *http.Response, cand model.Candidate,
	reqID string, rp responder) (relayResult, *upstreamError) {

	var res relayResult
	reader := bufio.NewReaderSize(resp.Body, 32*1024)
	// 先探测首字节，确保在写出响应头之前确认上游确实有数据（此时仍可切换渠道）
	if _, err := reader.Peek(1); err != nil && err != io.EOF {
		return res, &upstreamError{Transport: err, ChannelID: cand.ID}
	}

	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("X-Request-Id", reqID)
	w.Header().Set("X-Gateway-Channel", cand.ID)
	w.WriteHeader(resp.StatusCode)
	if flusher != nil {
		flusher.Flush()
	}

	writer := rp.Stream()
	write := func(b []byte) bool {
		if len(b) == 0 {
			return true
		}
		if _, err := w.Write(b); err != nil {
			return false
		}
		if flusher != nil {
			flusher.Flush()
		}
		return true
	}
	// Anthropic 需要在正文之前先发 message_start
	if !write(writer.Begin()) {
		return res, nil
	}

	var acc strings.Builder
	limit := h.payloadLimit()
	collect := h.recordPayload()
	for {
		line, readErr := reader.ReadString('\n')
		if line != "" {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "data:") {
				payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
				isDone := payload == "[DONE]"
				if u := ParseUsageStream(line); u.Total() > 0 {
					res.Usage = u
				}
				if collect {
					switch {
					case acc.Len() < limit:
						acc.WriteString(streamDelta(line))
					case !isDone:
						// 已达采集上限但仍有后续内容：日志只保留了开头，完整长度未知
						res.SnippetCapped = true
					}
				}
				if !write(writer.Line(payload, isDone)) {
					res.Snippet = truncate(acc.String(), limit) // 客户端断开，按成功处理
					return res, nil
				}
				if isDone {
					break
				}
			}
		}
		if readErr != nil {
			if readErr != io.EOF {
				h.log.Warn("流式转发中断", "channel", cand.ID, "err", readErr)
			}
			break
		}
	}
	// 收尾：OpenAI 直通为空；Anthropic 需补 content_block_stop + message_delta + message_stop
	write(writer.End())
	// 转换器可能自带用量（Anthropic 从末尾数据块解析）
	if u := writer.Usage(); u.Total() > 0 {
		res.Usage = u
	}
	res.Snippet = truncate(acc.String(), limit)
	return res, nil
}

func requestIDOf(r *http.Request) string {
	if id := r.Header.Get("X-Request-Id"); id != "" {
		return id
	}
	return NewRequestID()
}

// ListModels GET /v1/models
func (h *Handler) ListModels(w http.ResponseWriter, _ *http.Request) {
	models := h.store.PublicModels()
	data := make([]map[string]any, 0, len(models))
	for _, m := range models {
		data = append(data, map[string]any{
			"id": m.ID, "object": m.Object, "created": m.Created, "owned_by": m.OwnedBy,
			"upstreams": m.Upstreams,
		})
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
}

func (h *Handler) finish(e model.LogEntry) {
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	h.store.AddLog(e)
	if h.cfg.RT().AccessLog {
		h.log.Info("access",
			"req", e.RequestID, "method", e.Method, "path", e.Path, "model", e.Model,
			"channel", e.ChannelID, "stream", e.Stream,
			"status", e.Status, "latency_ms", e.LatencyMS, "attempts", e.Attempts,
			"prompt_tokens", e.PromptTokens, "completion_tokens", e.CompletionTokens,
			"ip", e.ClientIP, "err", e.Error)
	}
}

func statusOf(e *upstreamError) int {
	if e == nil {
		return http.StatusBadGateway
	}
	if e.Status > 0 {
		return e.Status
	}
	return http.StatusBadGateway
}

func copyHeader(dst, src http.Header, keys ...string) {
	for _, k := range keys {
		if v := src.Get(k); v != "" {
			dst.Set(k, v)
		}
	}
}

func pickCT(ct string) string {
	if ct == "" {
		return "application/json; charset=utf-8"
	}
	return ct
}

func clientIP(r *http.Request) string {
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		return strings.TrimSpace(strings.Split(v, ",")[0])
	}
	if v := r.Header.Get("X-Real-IP"); v != "" {
		return v
	}
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i > 0 {
		host = host[:i]
	}
	return host
}
