package proxy

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/demo1/apitoken/internal/model"
	"github.com/demo1/apitoken/internal/proxy/translate"
)

// responder 决定如何把上游的 OpenAI 兼容响应写回客户端。
//
// /v1/chat/completions 用 openAIResponder 原样透传（零开销）；
// /v1/messages 用 anthropicResponder 转成 Anthropic Messages 事件序列，
// 因此新增一个入站协议不需要改动路由、鉴权、故障转移与日志逻辑。
type responder interface {
	// JSON 处理非流式响应。upstream 是上游返回的原始 OpenAI 兼容响应体，
	// hdr 是上游响应头（用于透传 rate limit 等信息）。
	JSON(w http.ResponseWriter, status int, upstream []byte, hdr http.Header, cand model.Candidate)
	// Stream 返回流式转换器。
	Stream() streamWriter
	// ConvertMs 返回本次响应的协议转换耗时（毫秒），直通实现返回 0。
	ConvertMs() int64
}

// streamWriter 逐行消费上游 OpenAI SSE，并产出客户端协议的数据。
type streamWriter interface {
	// Begin 在写完响应头之后、第一条数据之前调用，返回需立即写出的字节。
	Begin() []byte
	// Line 处理一行 data: 负载（done 为 true 表示收到 [DONE]）。
	Line(payload string, done bool) []byte
	// End 上游结束时收尾。
	End() []byte
	// Usage 返回统计到的用量。
	Usage() Usage
	// Dropped 返回被丢弃的内容说明（用于日志）。
	Dropped() []string
}

// ---------------------------------------------------------------------------
// OpenAI 直通（/v1/chat/completions）
// ---------------------------------------------------------------------------

type openAIResponder struct{}

func (openAIResponder) JSON(w http.ResponseWriter, status int, upstream []byte, hdr http.Header, cand model.Candidate) {
	w.Header().Set("Content-Type", pickCT(hdr.Get("Content-Type")))
	w.Header().Set("X-Gateway-Channel", cand.ID)
	w.Header().Set("X-Gateway-Channel-Name", cand.Name)
	w.Header().Set("X-Gateway-Upstream-Model", cand.UpstreamModel)
	copyHeader(w.Header(), hdr, "Openai-Organization", "Openai-Processing-Ms",
		"X-Ratelimit-Limit-Requests", "X-Ratelimit-Remaining-Requests")
	w.WriteHeader(status)
	_, _ = w.Write(upstream)
}

func (openAIResponder) Stream() streamWriter { return &openAIStream{} }

func (openAIResponder) ConvertMs() int64 { return 0 }

// openAIStream 原样转发上游 SSE 行。
type openAIStream struct {
	usage Usage
}

func (s *openAIStream) Begin() []byte { return nil }

func (s *openAIStream) Line(payload string, done bool) []byte {
	if done {
		return []byte("data: [DONE]\n\n")
	}
	if strings.TrimSpace(payload) == "" {
		return nil
	}
	return []byte("data: " + payload + "\n\n")
}

func (s *openAIStream) End() []byte { return nil }

func (s *openAIStream) Usage() Usage { return s.usage }

func (s *openAIStream) Dropped() []string { return nil }

// ---------------------------------------------------------------------------
// Anthropic Messages（/v1/messages）
// ---------------------------------------------------------------------------

// anthropicResponder 把 OpenAI 响应转成 Anthropic Messages 格式。
type anthropicResponder struct {
	info    *translate.Info
	convert time.Duration // 协议转换累计耗时
	dropped []string
}

// addConvert 累加转换耗时，供日志与响应头观测。
func (r *anthropicResponder) addConvert(d time.Duration) { r.convert += d }

func (r *anthropicResponder) JSON(w http.ResponseWriter, status int, upstream []byte, _ http.Header, cand model.Candidate) {
	t0 := time.Now()
	body, dropped, err := translate.ResponseToAnthropic(upstream, r.info)
	r.addConvert(time.Since(t0))
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "上游响应无法转换为 Anthropic 格式: "+err.Error())
		return
	}
	r.dropped = append(r.dropped, dropped...)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Gateway-Channel", cand.ID)
	w.Header().Set("X-Gateway-Channel-Name", cand.Name)
	w.Header().Set("X-Gateway-Upstream-Model", cand.UpstreamModel)
	w.Header().Set("X-Gateway-Convert-Ms", strconv.FormatInt(r.ConvertMs(), 10))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (r *anthropicResponder) Stream() streamWriter {
	return &anthropicStreamWriter{strm: translate.NewStream(r.info), parent: r}
}

func (r *anthropicResponder) ConvertMs() int64 { return r.convert.Milliseconds() }

type anthropicStreamWriter struct {
	strm   *translate.Stream
	parent *anthropicResponder
}

func (s *anthropicStreamWriter) Begin() []byte { return s.strm.Begin() }

func (s *anthropicStreamWriter) Line(payload string, done bool) []byte {
	t0 := time.Now()
	out := s.strm.Line(payload, done)
	s.parent.addConvert(time.Since(t0))
	return out
}

func (s *anthropicStreamWriter) End() []byte {
	t0 := time.Now()
	out := s.strm.End()
	s.parent.addConvert(time.Since(t0))
	return out
}

func (s *anthropicStreamWriter) Usage() Usage {
	u := s.strm.Usage()
	return Usage{Prompt: u.InputTokens, Completion: u.OutputTokens}
}

func (s *anthropicStreamWriter) Dropped() []string {
	d := s.strm.Dropped()
	s.parent.dropped = append(s.parent.dropped, d...)
	return d
}

// isAnthropic 判断是否需要用 Anthropic 的错误结构回包。
func isAnthropic(resp responder) bool {
	_, ok := resp.(*anthropicResponder)
	return ok
}

// writeAnthropicError 以 Anthropic 的错误结构回包。
func writeAnthropicError(w http.ResponseWriter, status int, typ, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(translate.ErrorResponse(typ, message))
}

// anthropicErrorType 把 HTTP 状态码映射到 Anthropic 的 error.type。
func anthropicErrorType(status int) string {
	switch {
	case status == http.StatusBadRequest:
		return "invalid_request_error"
	case status == http.StatusUnauthorized:
		return "authentication_error"
	case status == http.StatusForbidden:
		return "permission_error"
	case status == http.StatusNotFound:
		return "not_found_error"
	case status == http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case status >= 500:
		return "api_error"
	default:
		return "invalid_request_error"
	}
}
