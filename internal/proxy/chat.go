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
	store   *store.Store
	cfg     *config.Config
	log     *slog.Logger
	client  *Client
	maxBody int64
}

// NewHandler 构造代理处理器。
func NewHandler(st *store.Store, cfg *config.Config, log *slog.Logger) *Handler {
	return &Handler{
		store:   st,
		cfg:     cfg,
		log:     log,
		client:  NewClient(cfg),
		maxBody: int64(cfg.Server.MaxBodyMB) << 20,
	}
}

// ChatCompletions POST /v1/chat/completions
func (h *Handler) ChatCompletions(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	reqID := r.Header.Get("X-Request-Id")
	if reqID == "" {
		reqID = NewRequestID()
	}
	entry := model.LogEntry{
		Time: start, RequestID: reqID, Method: r.Method, Path: r.URL.Path,
		ClientIP: clientIP(r),
	}

	if r.Method != http.MethodPost {
		WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "仅支持 POST", "")
		return
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, h.maxBody))
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

	candidates := h.store.Candidates(modelName)
	if len(candidates) == 0 {
		entry.Status = http.StatusNotFound
		entry.LatencyMS = time.Since(start).Milliseconds()
		entry.Error = "无可用渠道"
		h.finish(entry)
		WriteError(w, http.StatusNotFound, "model_not_found",
			fmt.Sprintf("模型 %q 没有可用的渠道，请先在管理端添加/启用渠道", modelName), "")
		return
	}

	limit := h.cfg.Routing.MaxAttempts
	if limit <= 0 || limit > len(candidates) {
		limit = len(candidates)
	}

	var lastErr *upstreamError

	for i := 0; i < limit; i++ {
		cand := candidates[i]
		entry.Attempts = i + 1
		attStart := time.Now()

		res, uerr := h.attempt(w, r, reqID, cand, payload, stream)
		latency := time.Since(attStart)
		if uerr == nil {
			h.store.RecordChannelAttempt(cand.ID, true, latency, res.Usage.Prompt, res.Usage.Completion, "")
			h.store.RecordRequest(modelName, true, time.Since(start), res.Usage.Prompt, res.Usage.Completion)
			entry.Status = http.StatusOK
			entry.ChannelID = cand.ID
			entry.ChannelName = cand.Name
			entry.UpstreamModel = cand.UpstreamModel
			entry.Endpoint = res.Endpoint
			entry.ResponseSnippet = res.Snippet
			entry.PromptTokens = res.Usage.Prompt
			entry.CompletionTokens = res.Usage.Completion
			entry.LatencyMS = time.Since(start).Milliseconds()
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
		attemptEntry.ResponseSnippet = res.Snippet
		h.finish(attemptEntry)

		if !uerr.retryable(h.cfg.Routing.RetryStatus) {
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
		WriteError(w, http.StatusBadGateway, "no_channel", "没有可用渠道", "")
		return
	}
	if lastErr.Transport != nil && r.Context().Err() == nil {
		WriteError(w, http.StatusBadGateway, "upstream_error", lastErr.Error(), "")
		return
	}
	// 透传上游错误体，便于排查（如 401 key 无效、429 限流）
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Request-Id", reqID)
	w.WriteHeader(lastErr.Status)
	body := lastErr.Body
	if len(body) == 0 {
		body = []byte(`{"error":{"message":"upstream error","type":"api_error"}}`)
	}
	_, _ = w.Write(body)
}

// attempt 向单个渠道发起请求，成功时写出响应（返回用量与摘要信息）。
func (h *Handler) attempt(w http.ResponseWriter, r *http.Request, reqID string, cand model.Candidate,
	payload map[string]any, stream bool) (relayResult, *upstreamError) {

	var res relayResult
	timeout := timeoutFor(h.cfg, cand.Channel, stream)
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	req, err := h.BuildRequest(ctx, cand, payload, stream)
	if err != nil {
		return res, &upstreamError{Transport: err, ChannelID: cand.ID}
	}
	req.Header.Set("X-Request-Id", reqID)
	res.Endpoint = req.URL.String()

	resp, err := h.client.Do(req)
	if err != nil {
		return res, &upstreamError{Transport: err, ChannelID: cand.ID}
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
		res.Snippet = describeError(body, resp.StatusCode)
		return res, &upstreamError{Status: resp.StatusCode, Body: body, ChannelID: cand.ID}
	}

	var rerr *upstreamError
	if stream {
		res, rerr = h.relayStream(w, resp, cand, reqID)
	} else {
		res, rerr = h.relayJSON(w, resp, reqID)
	}
	res.Endpoint = req.URL.String()
	return res, rerr
}

func (h *Handler) relayJSON(w http.ResponseWriter, resp *http.Response, reqID string) (relayResult, *upstreamError) {
	var res relayResult
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return res, &upstreamError{Transport: err}
	}
	res.Usage = ParseUsage(data)
	res.Snippet = h.responseSnippet(data)
	w.Header().Set("Content-Type", pickCT(resp.Header.Get("Content-Type")))
	w.Header().Set("X-Request-Id", reqID)
	copyHeader(w.Header(), resp.Header, "Openai-Organization", "Openai-Processing-Ms", "X-Ratelimit-Limit-Requests", "X-Ratelimit-Remaining-Requests")
	w.WriteHeader(resp.StatusCode)
	if _, err := w.Write(data); err != nil {
		h.log.Warn("写回响应失败", "err", err)
	}
	return res, nil
}

func (h *Handler) relayStream(w http.ResponseWriter, resp *http.Response, cand model.Candidate, reqID string) (relayResult, *upstreamError) {
	var res relayResult
	reader := bufio.NewReaderSize(resp.Body, 32*1024)
	// 先探测首字节，确保在写出响应头之前确认上游确实有数据
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

	var acc strings.Builder
	limit := h.payloadLimit()
	collect := h.recordPayload()
	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			if u := ParseUsageStream(line); u.Total() > 0 {
				res.Usage = u
			}
			if collect && acc.Len() < limit {
				acc.WriteString(streamDelta(line))
			}
			if _, werr := io.WriteString(w, line); werr != nil {
				res.Snippet = truncate(acc.String(), limit) // 客户端断开，按成功处理
				return res, nil
			}
			if flusher != nil {
				flusher.Flush()
			}
			if strings.HasPrefix(strings.TrimSpace(line), "data:") && strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "data:")) == "[DONE]" {
				res.Snippet = truncate(acc.String(), limit)
				return res, nil
			}
		}
		if err != nil {
			if err != io.EOF {
				h.log.Warn("流式转发中断", "channel", cand.ID, "err", err)
			}
			res.Snippet = truncate(acc.String(), limit)
			return res, nil
		}
	}
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
	if h.cfg.Logging.AccessLog {
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
