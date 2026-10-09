package store

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/demo1/apitoken/internal/config"
	"github.com/demo1/apitoken/internal/model"
)

func entry(i int) model.LogEntry {
	// Time 递增：既让并发测试里的时间序断言有意义，也贴近真实日志的时间列
	return model.LogEntry{
		RequestID: fmt.Sprintf("req-%03d", i),
		Model:     "mock-chat",
		Status:    200,
		Time:      time.Unix(int64(i), 0),
	}
}

func ids(entries []model.LogEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.RequestID)
	}
	return out
}

func eq(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("长度不符: got %v want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("第 %d 项不符: got %v want %v", i, got, want)
		}
	}
}

// TestLogBufferOrder 校验环形缓冲的顺序语义：List 始终返回"最新在前"。
func TestLogBufferOrder(t *testing.T) {
	b := NewLogBuffer(4)
	for i := 1; i <= 3; i++ {
		b.Add(entry(i))
	}
	if b.Count() != 3 {
		t.Fatalf("Count 应为 3，实际 %d", b.Count())
	}
	eq(t, ids(b.List(0)), "req-003", "req-002", "req-001")
	eq(t, ids(b.List(2)), "req-003", "req-002")

	// 超过容量：覆写最旧的，条数封顶在 max
	b.Add(entry(4))
	b.Add(entry(5))
	if b.Count() != 4 {
		t.Fatalf("写满后 Count 应为 4，实际 %d", b.Count())
	}
	eq(t, ids(b.List(0)), "req-005", "req-004", "req-003", "req-002")
	eq(t, ids(b.Snapshot()), "req-005", "req-004", "req-003", "req-002")
}

// TestLogBufferSetMax 校验动态调整保留条数后仍保留"最近 max 条"且顺序正确。
func TestLogBufferSetMax(t *testing.T) {
	b := NewLogBuffer(5)
	for i := 1; i <= 5; i++ {
		b.Add(entry(i))
	}

	// 扩容：原有 5 条全部保留，新写入接在后面
	b.SetMax(8)
	if b.Count() != 5 || len(b.List(0)) != 5 {
		t.Fatalf("扩容后应保留 5 条: count=%d", b.Count())
	}
	eq(t, ids(b.List(0)), "req-005", "req-004", "req-003", "req-002", "req-001")
	b.Add(entry(6))
	eq(t, ids(b.List(1)), "req-006")

	// 缩容：只保留最近 3 条，且覆写位置正确
	b.SetMax(3)
	if b.Count() != 3 {
		t.Fatalf("缩容后 Count 应为 3，实际 %d", b.Count())
	}
	eq(t, ids(b.List(0)), "req-006", "req-005", "req-004")
	b.Add(entry(7))
	eq(t, ids(b.List(0)), "req-007", "req-006", "req-005")

	// 非法值应被忽略，不破坏现有缓冲
	b.SetMax(0)
	if b.Count() != 3 {
		t.Fatalf("SetMax(0) 应被忽略: %d", b.Count())
	}
}

// TestLogBufferClear 校验清空后计数归零且能继续写入。
func TestLogBufferClear(t *testing.T) {
	b := NewLogBuffer(3)
	b.Add(entry(1))
	b.Add(entry(2))
	b.Clear()
	if b.Count() != 0 || len(b.List(0)) != 0 {
		t.Fatalf("清空后应为空: %d", b.Count())
	}
	b.Add(entry(9))
	eq(t, ids(b.List(0)), "req-009")
}

// TestLogBufferConcurrentAdd 并发写入 + 读取，验证锁与环形索引无竞争。
// 需要配合 go test -race 运行。
func TestLogBufferConcurrentAdd(t *testing.T) {
	b := NewLogBuffer(64)
	const workers, each = 8, 200

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				b.Add(entry(w*each + i))
			}
		}(w)
	}
	// 同时并发读，模拟日志页在写入期间查询
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				for _, e := range b.List(10) {
					_ = e.OK()
				}
				b.Count()
			}
		}()
	}
	wg.Wait()

	if b.Count() != 64 {
		t.Fatalf("并发写入后 Count 应封顶在 64，实际 %d", b.Count())
	}
	list := b.List(0)
	if len(list) != 64 {
		t.Fatalf("List 应返回 64 条，实际 %d", len(list))
	}
	// 并发下"哪条最后写入"不确定，因此只校验不变量：条数封顶、无重复、读取稳定。
	// 严格的覆写与顺序语义由 TestLogBufferOrder / TestLogBufferSetMax 单线程覆盖。
	seen := map[string]bool{}
	for _, e := range list {
		if seen[e.RequestID] {
			t.Fatalf("出现重复条目 %q", e.RequestID)
		}
		seen[e.RequestID] = true
	}
	again := ids(b.List(0))
	if fmt.Sprint(again) != fmt.Sprint(ids(list)) {
		t.Fatalf("连续两次读取结果不一致: %v vs %v", ids(list), again)
	}
}

// TestLogBufferConcurrentSetMax 写入过程中动态调整容量，不应崩溃或丢数据。
func TestLogBufferConcurrentSetMax(t *testing.T) {
	b := NewLogBuffer(32)
	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
				b.Add(entry(i))
			}
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
				b.SetMax(16 + i%40)
			}
		}
	}()

	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()

	if n := b.Count(); n > b.max {
		t.Fatalf("Count(%d) 不应超过 max(%d)", n, b.max)
	}
	// 结构仍然自洽：读回来的条数与计数一致，且没有重复条目
	list := b.List(0)
	if len(list) != b.Count() {
		t.Fatalf("List(%d) 与 Count(%d) 不一致", len(list), b.Count())
	}
	seen := map[string]bool{}
	for _, e := range list {
		if seen[e.RequestID] {
			t.Fatalf("出现重复条目 %q", e.RequestID)
		}
		seen[e.RequestID] = true
	}
}

// TestMatchKeyword 校验关键词匹配：跨字段不误命中，大小写不敏感。
func TestMatchKeyword(t *testing.T) {
	e := entry(1)
	e.ChannelName = "DeepSeek 官方"
	e.Error = "rate limited"

	cases := []struct {
		kw   string
		want bool
	}{
		{"deepseek", true}, // 大小写不敏感
		{"官方", true},       // 中文子串
		{"rate limited", true},
		{"req-001", true},
		{"mock-chat", true},
		{"nope", false},
		{"", true},
	}
	for _, c := range cases {
		q := model.LogQuery{Keyword: c.kw}
		if got := Match(e, q); got != c.want {
			t.Fatalf("关键词 %q：期望 %v，实际 %v", c.kw, c.want, got)
		}
	}
}

// TestFilterSinglePass 校验 Filter 与 Snapshot + 手工过滤结果一致。
func TestFilterSinglePass(t *testing.T) {
	b := NewLogBuffer(10)
	for i := 1; i <= 6; i++ {
		b.Add(entry(i))
	}
	only := func(e model.LogEntry) bool { return e.Status == 200 }

	got := b.Filter(only)
	want := b.Snapshot()
	var expect []model.LogEntry
	for _, e := range want {
		if only(e) {
			expect = append(expect, e)
		}
	}
	if len(got) != len(expect) {
		t.Fatalf("Filter 返回 %d 条，期望 %d", len(got), len(expect))
	}
	for i := range got {
		if got[i].RequestID != expect[i].RequestID {
			t.Fatalf("第 %d 条不匹配: %s vs %s", i, got[i].RequestID, expect[i].RequestID)
		}
	}
	// keep 为 nil 时等价于全量快照
	all := b.Filter(nil)
	if len(all) != 6 {
		t.Fatalf("Filter(nil) 应返回 6 条，实际 %d", len(all))
	}
	if all[0].RequestID != "req-006" {
		t.Fatalf("最新一条应在最前，实际 %s", all[0].RequestID)
	}
}

// TestRecordFinishMergesStats 校验合并写入与分开写入的统计结果一致。
func TestRecordFinishMergesStats(t *testing.T) {
	build := func() *Store {
		return &Store{
			cfg:      &config.Config{}, // RecordFinish 只用锁，不读配置
			channels: map[string]*model.Channel{"c1": {ID: "c1", Name: "渠道"}},
			health:   map[string]HealthInfo{},
			usage:    map[string]*model.ModelUsage{},
			logs:     NewLogBuffer(10),
		}
	}

	// 合并写入：成功
	s1 := build()
	s1.RecordFinish("c1", "m1", true, 200*time.Millisecond, 100*time.Millisecond, 10, 20, "")

	// 分开写入：成功（改造前的调用方式）
	s2 := build()
	s2.RecordChannelAttempt("c1", true, 100*time.Millisecond, 10, 20, "")
	s2.RecordRequest("m1", true, 200*time.Millisecond, 10, 20)

	for _, s := range []*Store{s1, s2} {
		ch := s.channels["c1"]
		if ch.Stats.Requests != 1 || ch.Stats.Success != 1 || ch.Stats.Failed != 0 {
			t.Fatalf("渠道统计不符: %+v", ch.Stats)
		}
		if ch.Stats.TotalLatencyMS != 100 {
			t.Fatalf("渠道延迟应取单次尝试的 100ms，实际 %d", ch.Stats.TotalLatencyMS)
		}
		if ch.Stats.PromptTokens != 10 || ch.Stats.CompletionTokens != 20 {
			t.Fatalf("渠道 token 统计不符: %+v", ch.Stats)
		}
		if s.totals.Requests != 1 || s.totals.Success != 1 {
			t.Fatalf("全局统计不符: %+v", s.totals)
		}
		if s.totals.TotalLatencyMS != 200 {
			t.Fatalf("全局延迟应取请求总耗时 200ms，实际 %d", s.totals.TotalLatencyMS)
		}
		u := s.usage["m1"]
		if u == nil || u.Requests != 1 || u.PromptTokens != 10 {
			t.Fatalf("模型统计不符: %+v", u)
		}
	}

	// 合并写入：失败并带错误信息
	s3 := build()
	s3.RecordFinish("c1", "m1", false, 300*time.Millisecond, 300*time.Millisecond, 0, 0, "boom")
	ch := s3.channels["c1"]
	if ch.Stats.Failed != 1 || ch.Stats.LastError != "boom" {
		t.Fatalf("失败统计不符: %+v", ch.Stats)
	}
	if s3.totals.Failed != 1 || s3.totals.Success != 0 {
		t.Fatalf("全局失败统计不符: %+v", s3.totals)
	}
	// 渠道不存在时不应 panic，且请求级统计仍要写入
	s4 := build()
	s4.RecordFinish("nope", "m1", true, 10*time.Millisecond, 10*time.Millisecond, 1, 1, "")
	if s4.totals.Requests != 1 {
		t.Fatalf("渠道不存在时请求级统计仍应写入: %+v", s4.totals)
	}
}

// legacyLogBuffer 是环形缓冲改造前的实现，Add 每次都整段搬移内存。
// 仅用于基准对比，验证 O(n) -> O(1) 的实际收益。
type legacyLogBuffer struct {
	mu  sync.RWMutex
	buf []model.LogEntry
	max int
}

func (l *legacyLogBuffer) Add(e model.LogEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.buf) >= l.max {
		copy(l.buf, l.buf[1:])
		l.buf = l.buf[:len(l.buf)-1]
	}
	l.buf = append(l.buf, e)
}

const benchMax = 1000 // 与 config.yaml 默认 keep_logs 一致

// BenchmarkLogBufferAdd 对比改造前后的写入开销（均已填满缓冲，即最坏情况）。
func BenchmarkLogBufferAdd(b *testing.B) {
	buf := NewLogBuffer(benchMax)
	for i := 0; i < benchMax; i++ {
		buf.Add(entry(i))
	}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		buf.Add(entry(i))
	}
}

func BenchmarkLogBufferAddLegacy(b *testing.B) {
	l := &legacyLogBuffer{buf: make([]model.LogEntry, 0, benchMax), max: benchMax}
	for i := 0; i < benchMax; i++ {
		l.Add(entry(i))
	}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		l.Add(entry(i))
	}
}

// BenchmarkLogBufferQuery 日志页查询开销（对每条日志做条件匹配）。
func BenchmarkLogBufferQuery(b *testing.B) {
	buf := NewLogBuffer(benchMax)
	for i := 0; i < benchMax; i++ {
		buf.Add(entry(i))
	}
	q := model.LogQuery{Model: "mock-chat", Status: "success"}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = buf.Filter(func(e model.LogEntry) bool { return Match(e, q) })
	}
}
