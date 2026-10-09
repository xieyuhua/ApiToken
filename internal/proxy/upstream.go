// Package proxy 实现 OpenAI 兼容协议的上游转发。
package proxy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/demo1/apitoken/internal/config"
	"github.com/demo1/apitoken/internal/model"
)

// Client 上游 HTTP 客户端。
type Client struct {
	http *http.Client
	cfg  *config.Config
}

// NewClient 构造上游客户端。
func NewClient(cfg *config.Config) *Client {
	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   cfg.Upstream.ConnectTimeout.D(),
			KeepAlive: cfg.Upstream.KeepAlive.D(),
		}).DialContext,
		MaxIdleConns:          cfg.Upstream.MaxIdleConns,
		MaxIdleConnsPerHost:   cfg.Upstream.MaxIdleConns / 2,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: time.Second,
		ForceAttemptHTTP2:     true,
	}
	if p := strings.TrimSpace(cfg.Upstream.ProxyURL); p != "" {
		if u, err := url.Parse(p); err == nil {
			transport.Proxy = http.ProxyURL(u)
		}
	}
	// 强制直连模式：忽略系统环境变量中的 HTTP_PROXY/HTTPS_PROXY
	if strings.EqualFold(strings.TrimSpace(cfg.Upstream.ProxyURL), "none") ||
		strings.EqualFold(strings.TrimSpace(cfg.Upstream.ProxyURL), "direct") {
		transport.Proxy = nil
	}
	return &Client{
		cfg:  cfg,
		http: &http.Client{Transport: transport}, // 不设全局 Timeout，改用 context 控制
	}
}

// Do 发起上游请求。
func (c *Client) Do(req *http.Request) (*http.Response, error) { return c.http.Do(req) }

// Usage token 用量。
type Usage struct {
	Prompt     int64 `json:"prompt_tokens"`
	Completion int64 `json:"completion_tokens"`
}

// Total 总 token。
func (u Usage) Total() int64 { return u.Prompt + u.Completion }

type usageEnvelope struct {
	Usage *struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
		InputTokens      int64 `json:"input_tokens"`
		OutputTokens     int64 `json:"output_tokens"`
	} `json:"usage"`
}

// ParseUsage 从响应 JSON 中提取 token 用量（兼容 OpenAI 与 Anthropic 字段名）。
func ParseUsage(raw []byte) Usage {
	var env usageEnvelope
	if err := json.Unmarshal(raw, &env); err != nil || env.Usage == nil {
		return Usage{}
	}
	u := Usage{Prompt: env.Usage.PromptTokens, Completion: env.Usage.CompletionTokens}
	if u.Prompt == 0 {
		u.Prompt = env.Usage.InputTokens
	}
	if u.Completion == 0 {
		u.Completion = env.Usage.OutputTokens
	}
	return u
}

// ParseUsageStream 从一行 SSE 数据中提取用量。
func ParseUsageStream(line string) Usage {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "data:") {
		return Usage{}
	}
	payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if payload == "" || payload == "[DONE]" {
		return Usage{}
	}
	return ParseUsage([]byte(payload))
}

// upstreamError 上游调用错误。
type upstreamError struct {
	Status    int
	Body      []byte
	Transport error
	ChannelID string
	// Endpoint 实际请求的上游地址
	Endpoint string
	// UpstreamModel 发往上游的模型名
	UpstreamModel string
}

func (e *upstreamError) Error() string {
	switch {
	case e.Transport != nil:
		return fmt.Sprintf("上游请求失败: %v", e.Transport)
	case len(e.Body) > 0:
		return fmt.Sprintf("上游返回 %d: %s", e.Status, truncate(string(e.Body), 500))
	default:
		return fmt.Sprintf("上游返回 %d", e.Status)
	}
}

func (e *upstreamError) retryable(retryStatus []int) bool {
	if e.Transport != nil {
		// 上下文取消（客户端断开）不重试
		if e.Transport == context.Canceled || e.Transport == context.DeadlineExceeded {
			return false
		}
		if strings.Contains(e.Transport.Error(), "context canceled") {
			return false
		}
		return true
	}
	for _, s := range retryStatus {
		if e.Status == s {
			return true
		}
	}
	return false
}

// BuildOpts 上游请求构造选项。
type BuildOpts struct {
	// ForceUsage 强制注入 stream_options.include_usage。
	//
	// Anthropic 端点依赖它：OpenAI 流式把用量放在最后一个数据块，
	// 不开启则流式请求的 token 统计恒为 0。
	ForceUsage bool
}

// BuildRequest 构造发往上游的请求（鉴权与路径均取自渠道配置）。
func (h *Handler) BuildRequest(ctx context.Context, cand model.Candidate, payload map[string]any, stream bool) (*http.Request, error) {
	return h.BuildRequestOpts(ctx, cand, payload, stream, BuildOpts{})
}

// BuildRequestOpts 是 BuildRequest 的可配置版本。
func (h *Handler) BuildRequestOpts(ctx context.Context, cand model.Candidate, payload map[string]any,
	stream bool, opts BuildOpts) (*http.Request, error) {

	ch := cand.Channel
	body := make(map[string]any, len(payload)+4)
	for k, v := range payload {
		body[k] = v
	}
	body["model"] = cand.UpstreamModel
	body["stream"] = stream
	for k, v := range ch.ExtraParams {
		body[k] = v
	}
	forceUsage := opts.ForceUsage || h.cfg.Routing.ForceStreamUsage
	if stream && forceUsage {
		if so, ok := body["stream_options"].(map[string]any); ok {
			so["include_usage"] = true
		} else {
			body["stream_options"] = map[string]any{"include_usage": true}
		}
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	chatPath := ch.ChatPath
	if chatPath == "" {
		chatPath = DefaultChatPath
	}
	target := JoinURL(ch.BaseURL, chatPath)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(string(raw)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	applyAuth(req, ch)
	for k, v := range ch.ExtraHeaders {
		req.Header.Set(k, v)
	}
	return req, nil
}

// DefaultChatPath 默认的 OpenAI 兼容对话路径。
const DefaultChatPath = "/chat/completions"

// applyAuth 按渠道配置注入 API Key。
// 默认 Authorization: Bearer <key>；auth_style=header 时自定义头直接放 key；auth_style=query 时用 ?key=xxx。
func applyAuth(req *http.Request, ch model.Channel) {
	if ch.APIKey == "" {
		return
	}
	header := ch.AuthHeader
	if header == "" {
		header = "Authorization"
	}
	prefix := ch.AuthPrefix
	if prefix == "" {
		prefix = "Bearer "
	}
	switch strings.ToLower(ch.AuthStyle) {
	case "header":
		req.Header.Set(header, ch.APIKey)
	case "query":
		q := req.URL.Query()
		q.Set("key", ch.APIKey)
		req.URL.RawQuery = q.Encode()
	default:
		req.Header.Set(header, prefix+ch.APIKey)
	}
}

// JoinURL 拼接 base_url 与路径。
func JoinURL(base, path string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if path == "" {
		return base
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return base + path
}

// ModelsPath 上游模型列表路径。
const ModelsPath = "/models"

func timeoutFor(cfg *config.Config, ch model.Channel, stream bool) time.Duration {
	if ch.TimeoutSec > 0 {
		return time.Duration(ch.TimeoutSec) * time.Second
	}
	if stream {
		if n := cfg.RT().StreamTimeout; n > 0 {
			return n
		}
	}
	if n := cfg.RT().RequestTimeout; n > 0 {
		return n
	}
	if n := cfg.RT().DefaultTimeout; n > 0 {
		return n
	}
	return 120 * time.Second
}

func truncate(s string, n int) string {
	out, _ := truncateFull(s, n)
	return out
}

// truncateFull 与 truncate 相同，但同时返回截断前的原始字符数（未截断时为 0）。
// 用于日志详情明确告知"这是存储上限导致的截断"，而不是页面展示不全。
//
// 按**字符**（rune）而非字节截断：中文回复 2000 字节只有约 660 字，
// 按字节切会严重少存内容，也是"回复内容看起来不完整"的根因。
func truncateFull(s string, n int) (string, int) {
	s = strings.TrimSpace(s)
	if n <= 0 {
		return s, 0
	}
	rs := []rune(s)
	if len(rs) <= n {
		return s, 0
	}
	return string(rs[:n]) + fmt.Sprintf("... [已截断，原始 %d 字]", len(rs)), len(rs)
}

// NewRequestID 生成请求 ID。
func NewRequestID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	return "chatcmpl-" + hex.EncodeToString(b)
}

// WriteError 输出 OpenAI 风格错误。
func WriteError(w http.ResponseWriter, status int, code, message, typ string) {
	if typ == "" {
		typ = "invalid_request_error"
	}
	if status == 401 || status == 403 {
		typ = "authentication_error"
	} else if status == 404 {
		typ = "not_found_error"
	} else if status >= 500 {
		typ = "api_error"
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": message, "type": typ, "code": code},
	})
}
