package store

import (
	"sort"
	"strings"
	"sync"

	"github.com/demo1/apitoken/internal/model"
)

// LogBuffer 定长访问日志环形缓冲。
//
// buf 固定分配 max 个槽位，head 指向下一个写入位置，n 为有效条数，
// 因此 Add 是 O(1) 的"覆写最旧槽位"，不会因为保留条数大而整段搬移内存。
type LogBuffer struct {
	mu   sync.RWMutex
	buf  []model.LogEntry
	max  int
	head int // 下一个写入位置
	n    int // 有效条数（<= max）
}

// NewLogBuffer 创建环形缓冲。
func NewLogBuffer(max int) *LogBuffer {
	if max <= 0 {
		max = 200
	}
	return &LogBuffer{buf: make([]model.LogEntry, max), max: max}
}

// Add 追加一条日志（O(1)：满则覆写最旧的槽位）。
func (b *LogBuffer) Add(e model.LogEntry) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.buf) == 0 {
		return
	}
	b.buf[b.head] = e
	b.head = (b.head + 1) % len(b.buf)
	if b.n < len(b.buf) {
		b.n++
	}
}

// List 返回最近 n 条（最新在前）。
func (b *LogBuffer) List(n int) []model.LogEntry {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if n <= 0 || n > b.n {
		n = b.n
	}
	out := make([]model.LogEntry, 0, n)
	for i := 0; i < n; i++ {
		// 从最新的前一条开始（head-1）反向绕回
		idx := b.head - 1 - i
		for idx < 0 {
			idx += len(b.buf)
		}
		out = append(out, b.buf[idx])
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
	if max == b.max {
		return
	}
	// 取仍需保留的条目并翻成时间正序（最旧在前）；此处不能调用 List，
	// 因为 List 会加读锁，而本方法已持有写锁。
	keep := make([]model.LogEntry, 0, b.n)
	for i := b.n - 1; i >= 0; i-- {
		idx := b.head - 1 - i
		for idx < 0 {
			idx += len(b.buf)
		}
		keep = append(keep, b.buf[idx])
	}
	if len(keep) > max {
		keep = keep[len(keep)-max:]
	}
	b.buf = make([]model.LogEntry, max)
	copy(b.buf, keep)
	b.max = max
	b.n = len(keep)
	// 下一个空槽；写满一圈时正好回到 0，即覆写最旧的一条
	b.head = b.n % max
}

// Count 当前保留条数.
func (b *LogBuffer) Count() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.n
}

// Clear 清空日志。
func (b *LogBuffer) Clear() {
	b.mu.Lock()
	defer b.mu.Unlock()
	// 清零槽位，避免日志内容在内存里残留到被覆写为止
	for i := range b.buf {
		b.buf[i] = model.LogEntry{}
	}
	b.head = 0
	b.n = 0
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

// Find 在缓冲内部遍历，只返回匹配项（最新在前）。
//
// 与 Filter 的区别是不再先做一份全量快照拷贝：日志详情要按 request_id 找出
// 那一两条记录，若每次都拷贝整个缓冲（keep_logs 条 × 数百字节）纯属浪费。
func (b *LogBuffer) Find(keep func(model.LogEntry) bool) []model.LogEntry {
	b.mu.RLock()
	defer b.mu.RUnlock()
	var out []model.LogEntry
	for i := 0; i < b.n; i++ {
		idx := b.head - 1 - i
		for idx < 0 {
			idx += len(b.buf)
		}
		e := b.buf[idx]
		if keep == nil || keep(e) {
			out = append(out, e)
		}
	}
	return out
}

// ForEach 在缓冲内部遍历（最新在前），不产生任何切片分配。
// 用于日志页的筛选项统计这类只需要汇总值的场景。
func (b *LogBuffer) ForEach(fn func(model.LogEntry)) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for i := 0; i < b.n; i++ {
		idx := b.head - 1 - i
		for idx < 0 {
			idx += len(b.buf)
		}
		fn(b.buf[idx])
	}
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
		if !matchKeyword(e, strings.ToLower(q.Keyword)) {
			return false
		}
	}
	return true
}

// matchKeyword 在日志各字段中查找关键词（kw 必须已转小写）。
//
// 逐字段匹配而非把 11 个字段 Join 成一个大串再 ToLower：日志页每次查询都会对
// 上千条日志调用这里，Join + ToLower 会为每条日志额外分配两段字符串。
func matchKeyword(e model.LogEntry, kw string) bool {
	// 先用原始字段匹配：多数情况下命中，无需做任何分配
	for _, f := range keywordFields(&e) {
		if f != "" && strings.Contains(f, kw) {
			return true
		}
	}
	// 未命中再逐字段小写后匹配（仍然不构造拼接串）
	for _, f := range keywordFields(&e) {
		if f != "" && strings.Contains(strings.ToLower(f), kw) {
			return true
		}
	}
	return false
}

// keywordFields 列出参与关键词搜索的字段。
func keywordFields(e *model.LogEntry) [11]string {
	return [11]string{
		e.RequestID, e.Model, e.UpstreamModel, e.ChannelID, e.ChannelName,
		e.Path, e.Error, e.ClientIP, e.Endpoint,
		e.RequestSnippet, e.ResponseSnippet,
	}
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
