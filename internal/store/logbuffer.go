package store

import (
	"sort"
	"strings"
	"sync"

	"github.com/demo1/apitoken/internal/model"
)

// LogBuffer 定长访问日志环形缓冲。
type LogBuffer struct {
	mu  sync.RWMutex
	buf []model.LogEntry
	max int
}

// NewLogBuffer 创建环形缓冲。
func NewLogBuffer(max int) *LogBuffer {
	if max <= 0 {
		max = 200
	}
	return &LogBuffer{buf: make([]model.LogEntry, 0, max), max: max}
}

// Add 追加一条日志。
func (b *LogBuffer) Add(e model.LogEntry) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.buf) >= b.max {
		copy(b.buf, b.buf[1:])
		b.buf = b.buf[:len(b.buf)-1]
	}
	b.buf = append(b.buf, e)
}

// List 返回最近 n 条（最新在前）。
func (b *LogBuffer) List(n int) []model.LogEntry {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if n <= 0 || n > len(b.buf) {
		n = len(b.buf)
	}
	out := make([]model.LogEntry, 0, n)
	for i := len(b.buf) - 1; i >= len(b.buf)-n; i-- {
		out = append(out, b.buf[i])
	}
	return out
}

// Snapshot 返回全部日志（最新在前）。
func (b *LogBuffer) Snapshot() []model.LogEntry {
	return b.List(0)
}

// SetMax 动态调整保留条数（保留最近的 max 条）。
func (b *LogBuffer) SetMax(max int) {
	if max <= 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.max = max
	if len(b.buf) > max {
		b.buf = append([]model.LogEntry(nil), b.buf[len(b.buf)-max:]...)
	}
}

// Count 当前保留条数.
func (b *LogBuffer) Count() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.buf)
}

// Clear 清空日志。
func (b *LogBuffer) Clear() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = b.buf[:0]
}

// Filter 按条件过滤（最新在前）。
func (b *LogBuffer) Filter(keep func(model.LogEntry) bool) []model.LogEntry {
	all := b.Snapshot()
	if keep == nil {
		return all
	}
	out := make([]model.LogEntry, 0, len(all))
	for _, e := range all {
		if keep(e) {
			out = append(out, e)
		}
	}
	return out
}

// Match 判断日志是否满足查询条件。
func Match(e model.LogEntry, q model.LogQuery) bool {
	if q.Model != "" && e.Model != q.Model {
		return false
	}
	if q.Channel != "" && e.ChannelID != q.Channel {
		return false
	}
	if q.RequestID != "" && e.RequestID != q.RequestID {
		return false
	}
	if q.Stream == "1" && !e.Stream {
		return false
	}
	if q.Stream == "0" && e.Stream {
		return false
	}
	switch strings.ToLower(q.Status) {
	case "success", "ok":
		if !e.OK() {
			return false
		}
	case "error", "fail", "failed":
		if e.OK() {
			return false
		}
	default:
		if q.Status != "" && !strings.EqualFold(q.Status, itoa(e.Status)) {
			return false
		}
	}
	if q.Keyword != "" {
		kw := strings.ToLower(q.Keyword)
		hay := strings.ToLower(strings.Join([]string{
			e.RequestID, e.Model, e.UpstreamModel, e.ChannelID, e.ChannelName,
			e.Path, e.Error, e.ClientIP, e.Endpoint,
			e.RequestSnippet, e.ResponseSnippet,
		}, " "))
		if !strings.Contains(hay, kw) {
			return false
		}
	}
	return true
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// BuildSummary 对过滤后的日志做聚合统计。
func BuildSummary(items []model.LogEntry) model.LogSummary {
	s := model.LogSummary{Total: int64(len(items))}
	latencies := make([]int64, 0, len(items))
	modelAgg := map[string]*model.LogKV{}
	channelAgg := map[string]*model.LogKV{}
	statusAgg := map[string]*model.LogKV{}
	errorAgg := map[string]*model.LogKV{}
	var totalLatency int64

	for _, e := range items {
		if e.OK() {
			s.Success++
		} else {
			s.Failed++
		}
		if e.Attempts > 1 {
			s.FailoverCount++
		}
		s.PromptTokens += e.PromptTokens
		s.CompletionTokens += e.CompletionTokens
		totalLatency += e.LatencyMS
		latencies = append(latencies, e.LatencyMS)

		if e.Model != "" {
			k := modelAgg[e.Model]
			if k == nil {
				k = &model.LogKV{Name: e.Model}
				modelAgg[e.Model] = k
			}
			accumulate(k, e)
		}
		name := e.ChannelID
		if name == "" {
			name = "(未匹配)"
		}
		k := channelAgg[name]
		if k == nil {
			k = &model.LogKV{Name: name}
			channelAgg[name] = k
		}
		accumulate(k, e)

		sk := itoa(e.Status)
		ks := statusAgg[sk]
		if ks == nil {
			ks = &model.LogKV{Name: sk}
			statusAgg[sk] = ks
		}
		accumulate(ks, e)

		if e.Error != "" {
			msg := e.Error
			if len(msg) > 160 {
				msg = msg[:160]
			}
			ke := errorAgg[msg]
			if ke == nil {
				ke = &model.LogKV{Name: msg}
				errorAgg[msg] = ke
			}
			ke.Count++
		}
	}

	if s.Total > 0 {
		s.AvgLatencyMS = totalLatency / s.Total
		sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
		idx := int(float64(len(latencies))*0.95) - 1
		if idx < 0 {
			idx = 0
		}
		if idx < len(latencies) {
			s.P95LatencyMS = latencies[idx]
		}
	}

	s.ByModel = finalize(modelAgg, 20)
	s.ByChannel = finalize(channelAgg, 20)
	s.ByStatus = finalize(statusAgg, 20)
	s.TopErrors = finalize(errorAgg, 10)
	return s
}

func accumulate(k *model.LogKV, e model.LogEntry) {
	k.Count++
	if !e.OK() {
		k.Failed++
	}
	k.TotalLatencyMS += e.LatencyMS
	if k.Count > 0 {
		k.AvgLatencyMS = k.TotalLatencyMS / k.Count
	}
}

func finalize(m map[string]*model.LogKV, limit int) []model.LogKV {
	out := make([]model.LogKV, 0, len(m))
	for _, v := range m {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Name < out[j].Name
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}
