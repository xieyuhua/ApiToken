// Package gateway 组装 HTTP 路由与中间件。
package gateway

import (
	"bufio"
	"crypto/subtle"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/demo1/apitoken/internal/admin"
	"github.com/demo1/apitoken/internal/config"
	"github.com/demo1/apitoken/internal/model"
	"github.com/demo1/apitoken/internal/proxy"
	"github.com/demo1/apitoken/internal/store"
	"github.com/demo1/apitoken/internal/webui"
)

// Server 网关 HTTP 服务。
// Version 网关版本号。
const Version = model.Version

type Server struct {
	cfg   *config.Config
	store *store.Store
	log   *slog.Logger
	proxy *proxy.Handler
	admin *admin.Handler
}

// New 构造网关服务。
func New(cfg *config.Config, st *store.Store, log *slog.Logger) *Server {
	return &Server{
		cfg:   cfg,
		store: st,
		log:   log,
		proxy: proxy.NewHandler(st, cfg, log),
		admin: admin.New(st, cfg, log),
	}
}

// Handler 构建完整路由。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// OpenAI 兼容接口
	mux.Handle("POST /v1/chat/completions", s.clientAuth(s.proxy.ChatCompletions))
	// Anthropic Messages 兼容接口：请求与响应都按 Anthropic 协议，
	// 上游仍走各渠道的 OpenAI 兼容端点，客户端无需任何改动即可接入。
	mux.Handle("POST /v1/messages", s.clientAuth(s.proxy.Messages))
	mux.Handle("GET /v1/models", s.clientAuth(s.proxy.ListModels))
	mux.HandleFunc("GET /v1/models/{model}", s.clientAuth(func(w http.ResponseWriter, r *http.Request) {
		want := r.PathValue("model")
		for _, m := range s.store.PublicModels() {
			if m.ID == want {
				writeJSON(w, map[string]any{"id": m.ID, "object": m.Object, "created": m.Created, "owned_by": m.OwnedBy})
				return
			}
		}
		proxy.WriteError(w, http.StatusNotFound, "model_not_found", "模型 "+want+" 不存在", "")
	}))

	// 管理接口（需管理令牌）
	adminMux := http.NewServeMux()
	s.admin.Register(adminMux)
	mux.Handle("/admin/", s.AdminAuth(adminMux))

	// 健康检查
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"status": "ok", "version": model.Version, "uptime": s.store.Uptime().String()})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		enabled := 0
		for _, c := range s.store.ListChannels() {
			if c.Enabled {
				enabled++
			}
		}
		if enabled == 0 {
			writeJSON(w, map[string]any{"status": "no_channel", "enabled_channels": 0})
			return
		}
		writeJSON(w, map[string]any{"status": "ok", "enabled_channels": enabled})
	})

	// 内置管理页面
	mux.Handle("GET /{$}", webui.Handler())
	mux.Handle("GET /logs", webui.LogsHandler())
	mux.Handle("GET /diagnostics", webui.DiagnosticsHandler())
	mux.Handle("GET /chat", webui.ChatHandler())
	mux.Handle("GET /settings", webui.SettingsHandler())
	mux.Handle("GET /static/{file}", webui.StaticHandler())

	return recoverMW(s.log, requestIDMW(securityHeaders(corsMW(mux))))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

// clientAuth 校验客户端调用密钥。
func (s *Server) clientAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		keys := s.cfg.RT().ClientKeys
		if len(keys) == 0 {
			next(w, r)
			return
		}
		token := bearer(r)
		if token == "" {
			token = r.Header.Get("X-Api-Key")
		}
		if !matchAny(token, keys) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			proxy.WriteError(w, http.StatusUnauthorized, "invalid_api_key",
				"网关密钥无效，请在 Authorization: Bearer <key> 中携带有效密钥", "authentication_error")
			return
		}
		next(w, r)
	}
}

// AdminAuth 校验管理令牌（供网关自身使用）。
func (s *Server) AdminAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := s.cfg.RT().AdminToken
		if token == "" {
			next.ServeHTTP(w, r)
			return
		}
		got := r.Header.Get("X-Admin-Token")
		if got == "" {
			got = bearer(r)
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			proxy.WriteError(w, http.StatusUnauthorized, "invalid_admin_token", "管理令牌无效", "authentication_error")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func bearer(r *http.Request) string {
	v := r.Header.Get("Authorization")
	if v == "" {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(v), "bearer ") {
		return strings.TrimSpace(v[7:])
	}
	return strings.TrimSpace(v)
}

func matchAny(token string, keys []string) bool {
	if token == "" {
		return false
	}
	for _, k := range keys {
		if subtle.ConstantTimeCompare([]byte(token), []byte(k)) == 1 {
			return true
		}
	}
	return false
}

func requestIDMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" {
			// 客户端未带 ID 时生成一个并写回请求头，保证代理、日志与响应头
			// 使用同一个 ID，观测链路统一（覆盖 /v1/models、/healthz 等非代理路径）。
			id = proxy.NewRequestID()
			r.Header.Set("X-Request-Id", id)
		}
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r)
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func corsMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 管理接口不开放跨域：仅数据面（/v1/* 等）允许，且不放行 X-Admin-Token，
		// 避免任意网页脚本对管理接口发起带令牌的跨域请求。
		if !strings.HasPrefix(r.URL.Path, "/admin/") {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", "*")
			h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Api-Key, X-Request-Id")
			h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			h.Set("Access-Control-Max-Age", "86400")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// ReadFrom 把底层 ResponseWriter 的 io.ReaderFrom 能力透传出去，
// 静态文件等场景可走 sendfile 零拷贝，同时保证状态与字节计数准确。
func (w *statusWriter) ReadFrom(r io.Reader) (int64, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if rf, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		n, err := rf.ReadFrom(r)
		w.bytes += int(n)
		return n, err
	}
	// 底层不支持时退化为普通拷贝，直接写底层避免 io.Copy 再次命中本 ReadFrom 造成递归
	n, err := io.Copy(w.ResponseWriter, r)
	w.bytes += int(n)
	return n, err
}

// Hijack 透传底层连接劫持能力（WebSocket 等需要）。
func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := w.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, http.ErrNotSupported
}

// Push 透传 HTTP/2 Server Push 能力。
func (w *statusWriter) Push(target string, opts *http.PushOptions) error {
	if p, ok := w.ResponseWriter.(http.Pusher); ok {
		return p.Push(target, opts)
	}
	return http.ErrNotSupported
}

func recoverMW(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w}
		start := time.Now()
		defer func() {
			if rec := recover(); rec != nil {
				log.Error("panic", "path", r.URL.Path, "panic", rec, "stack", string(debug.Stack()))
				// 响应头可能已写出（流式中途 panic），此时无法再写 500，避免重复写头
				if sw.status == 0 {
					proxy.WriteError(w, http.StatusInternalServerError, "internal_error", "网关内部错误", "")
				}
			}
		}()
		next.ServeHTTP(sw, r)
		if log != nil && r.URL.Path != "/" {
			log.Debug("request", "method", r.Method, "path", r.URL.Path,
				"status", sw.status, "bytes", sw.bytes, "cost", time.Since(start).String())
		}
	})
}
