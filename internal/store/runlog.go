package store

import (
	"context"
	"log/slog"
	"strconv"
	"sync"
	"time"
)

// RunLogAttr 运行日志里的一个结构化属性，key/value 分开输出便于前端着色。
type RunLogAttr struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// RunLogEntry 一条运行日志（进程自身的 slog 输出）。
type RunLogEntry struct {
	Time  time.Time    `json:"time"`
	Level string       `json:"level"`
	Msg   string       `json:"msg"`
	Attrs []RunLogAttr `json:"attrs,omitempty"`
}

// RunLog 运行日志的环形缓冲：内存保留最近若干条，供网页端「运行日志」查看。
// 运行日志属于进程级观测数据，用包级单例即可（与写 stdout 的语义一致）。
type RunLog struct {
	mu   sync.Mutex
	cap  int
	ring []RunLogEntry
}

// runLogCapacity 内存保留的运行日志条数上限。
const runLogCapacity = 2000

var runtimeLog = NewRunLog(runLogCapacity)

// RuntimeLog 返回进程运行日志缓冲（供 admin 读取）。
func RuntimeLog() *RunLog { return runtimeLog }

// NewRunLog 创建容量为 cap 条的运行日志缓冲。
func NewRunLog(cap int) *RunLog {
	if cap <= 0 {
		cap = runLogCapacity
	}
	return &RunLog{cap: cap, ring: make([]RunLogEntry, 0, cap)}
}

// add 追加一条日志；满时丢弃最旧的一半，摊销 O(1)。
func (r *RunLog) add(t time.Time, level slog.Level, msg string, attrs []RunLogAttr) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.ring) >= r.cap {
		keep := r.cap / 2
		copy(r.ring, r.ring[len(r.ring)-keep:])
		r.ring = r.ring[:keep]
	}
	r.ring = append(r.ring, RunLogEntry{Time: t, Level: level.String(), Msg: msg, Attrs: attrs})
}

// Lines 返回全部日志，最新在前（适合网页端倒序展示）。
func (r *RunLog) Lines() []RunLogEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]RunLogEntry, len(r.ring))
	for i, e := range r.ring {
		out[len(r.ring)-1-i] = e
	}
	return out
}

// Len 返回当前保留的条数。
func (r *RunLog) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.ring)
}

// runLogHandler 把日志同时写入下游 handler（stdout）与运行日志缓冲。
type runLogHandler struct {
	next slog.Handler
	rl   *RunLog
}

// RuntimeLogHandler 包装 next，使每条日志也进入运行日志缓冲。
func RuntimeLogHandler(next slog.Handler) slog.Handler {
	return &runLogHandler{next: next, rl: runtimeLog}
}

func (h *runLogHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.next.Enabled(ctx, l)
}

func (h *runLogHandler) Handle(ctx context.Context, r slog.Record) error {
	attrs := make([]RunLogAttr, 0, r.NumAttrs())
	r.Attrs(func(a slog.Attr) bool {
		attrs = append(attrs, RunLogAttr{Key: a.Key, Value: formatAttrValue(a.Value)})
		return true
	})
	h.rl.add(r.Time, r.Level, r.Message, attrs)
	return h.next.Handle(ctx, r)
}

func (h *runLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &runLogHandler{next: h.next.WithAttrs(attrs), rl: h.rl}
}

func (h *runLogHandler) WithGroup(name string) slog.Handler {
	return &runLogHandler{next: h.next.WithGroup(name), rl: h.rl}
}

// formatAttrValue 把 slog 值格式化成适合展示的文本：
// 字符串含空白/特殊字符时加引号，时间/时长用可读格式，其余用 String()。
func formatAttrValue(v slog.Value) string {
	switch v.Kind() {
	case slog.KindString:
		s := v.String()
		if needsQuote(s) {
			return strconv.Quote(s)
		}
		return s
	case slog.KindTime:
		return v.Time().Format("2006-01-02 15:04:05.000")
	case slog.KindDuration:
		return v.Duration().String()
	default:
		return v.String()
	}
}

func needsQuote(s string) bool {
	if s == "" {
		return true
	}
	for _, r := range s {
		switch r {
		case ' ', '"', '=', '\t', '\n', '\r':
			return true
		}
	}
	return false
}
