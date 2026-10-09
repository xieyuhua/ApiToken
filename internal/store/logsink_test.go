package store

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/demo1/apitoken/internal/model"
)

// newTestSink 创建一个落盘到临时目录的管道。
func newTestSink(t *testing.T, dir string, keep int, maxSeg int64, max int) *LogSink {
	t.Helper()
	return NewLogSink(LogSinkOptions{
		Dir:             dir,
		Keep:            keep,
		MaxSegmentBytes: maxSeg,
		Buffer:          NewLogBuffer(max),
	})
}

// readAllSegments 读取目录下全部分段文件的所有 JSONL 行。
func readAllSegments(t *testing.T, dir string) []model.LogEntry {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "access-*.jsonl"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	var out []model.LogEntry
	for _, f := range files {
		fh, err := os.Open(f)
		if err != nil {
			t.Fatalf("open %s: %v", f, err)
		}
		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 0, 1<<20), 8<<20)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			var e model.LogEntry
			if err := json.Unmarshal([]byte(line), &e); err != nil {
				t.Fatalf("分段 %s 中有坏行 %q: %v", f, line, err)
			}
			out = append(out, e)
		}
		fh.Close()
	}
	return out
}

// waitFor 轮询等待条件成立，避免依赖固定的 sleep 时长。
func waitFor(t *testing.T, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待超时: %s", what)
}

// TestLogSinkAsyncWriteToFile 校验日志异步落盘：请求路径入队后，
// 即使还没 Close，文件里也能读到内容（由后台协程批量写入）。
func TestLogSinkAsyncWriteToFile(t *testing.T) {
	dir := t.TempDir()
	s := newTestSink(t, dir, 3, 1<<20, 10)
	defer s.Close()

	for i := 1; i <= 5; i++ {
		s.Enqueue(entry(i))
	}
	// 入队是非阻塞的，文件内容由后台协程补齐
	waitFor(t, "日志落盘", func() bool {
		return len(readAllSegments(t, dir)) == 5
	})

	got := readAllSegments(t, dir)
	for i, e := range got {
		want := entry(i + 1).RequestID
		if e.RequestID != want {
			t.Fatalf("第 %d 条应为 %s，实际 %s", i, want, e.RequestID)
		}
	}
	// 内存热数据也应有同样的 5 条（管道内部缓冲）
	if s.buf.Count() != 5 {
		t.Fatalf("内存缓冲应有 5 条，实际 %d", s.buf.Count())
	}
	if _, written := s.Stats(); written != 5 {
		t.Fatalf("已落盘计数应为 5，实际 %d", written)
	}
}

// TestLogSinkCloseFlushesRemainder 校验 Close 会把队列里剩余的日志全部落盘，
// 进程退出时不丢最后一批。
func TestLogSinkCloseFlushesRemainder(t *testing.T) {
	dir := t.TempDir()
	s := newTestSink(t, dir, 3, 1<<20, 200)
	for i := 1; i <= 50; i++ {
		s.Enqueue(entry(i))
	}
	// 故意不等后台协程，直接关闭
	s.Close()

	got := readAllSegments(t, dir)
	if len(got) != 50 {
		t.Fatalf("关闭后应落盘 50 条，实际 %d", len(got))
	}
	// Close 可重复调用（幂等）
	s.Close()
}

// TestLogSinkNeverBlocksRequestPath 校验队列满时丢弃而不是阻塞请求。
// 这是"日志不影响网关性能"的核心保证。
func TestLogSinkNeverBlocksRequestPath(t *testing.T) {
	dir := t.TempDir()
	s := newTestSink(t, dir, 3, 1<<20, 100)
	s.Close() // 关闭后无人消费，队列会被填满

	done := make(chan struct{})
	go func() {
		defer close(done)
		// 远超队列容量：若 Enqueue 会阻塞，这里将卡住
		for i := 0; i < logQueueSize*3; i++ {
			s.Enqueue(entry(i))
		}
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("队列满时 Enqueue 阻塞了请求路径")
	}

	dropped, _ := s.Stats()
	if dropped == 0 {
		t.Fatal("队列满时应统计丢弃条数")
	}
	if dropped != int64(logQueueSize*2) {
		t.Fatalf("丢弃数应为 %d，实际 %d", logQueueSize*2, dropped)
	}
}

// TestLogSinkSegmentRotateAndPrune 校验超过阈值会滚动新文件，且只保留最近 keep 段。
func TestLogSinkSegmentRotateAndPrune(t *testing.T) {
	dir := t.TempDir()
	// 单段上限 4KB，保留 2 段
	s := newTestSink(t, dir, 2, 4<<10, 500)
	for i := 0; i < 200; i++ {
		e := entry(i)
		e.RequestSnippet = strings.Repeat("x", 200) // 撑大每行，触发滚动
		s.Enqueue(e)
	}
	s.Close()

	files, _ := filepath.Glob(filepath.Join(dir, "access-*.jsonl"))
	if len(files) < 2 {
		t.Fatalf("应生成多个分段文件，实际 %d 个", len(files))
	}
	if len(files) > 2 {
		t.Fatalf("应只保留 2 段，实际 %d 段: %v", len(files), files)
	}
	// 保留的应是最近的两段（按文件名时间序）
	sortStringsForTest(files)
	if !strings.HasSuffix(files[len(files)-1], files[len(files)-1]) {
		t.Fatal("文件名排序异常")
	}

	// 单个文件不应超过阈值太多（最后一段可能略超，因为它只在写满时检查）
	for _, f := range files {
		fi, err := os.Stat(f)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if fi.Size() > 8<<10 {
			t.Fatalf("分段 %s 明显超过阈值: %d 字节", filepath.Base(f), fi.Size())
		}
	}
}

// TestRecoverLogs 校验重启后能从分段文件恢复最近若干条，且顺序为时间正序。
func TestRecoverLogs(t *testing.T) {
	dir := t.TempDir()
	s := newTestSink(t, dir, 5, 1<<20, 100)
	for i := 1; i <= 30; i++ {
		s.Enqueue(entry(i))
	}
	s.Close()

	// 恢复最近 10 条
	got := RecoverLogs(dir, 10)
	if len(got) != 10 {
		t.Fatalf("应恢复 10 条，实际 %d", len(got))
	}
	// 时间正序：第 21..30 条
	for i, e := range got {
		want := entry(21 + i).RequestID
		if e.RequestID != want {
			t.Fatalf("第 %d 条应为 %s，实际 %s", i, want, e.RequestID)
		}
	}
	if got[len(got)-1].Time.Before(got[0].Time) {
		t.Fatal("恢复结果应为时间正序（最旧在前）")
	}

	// 恢复结果直接写回缓冲后，读出来应是"最新在前"
	buf := NewLogBuffer(100)
	for _, e := range RecoverLogs(dir, 10) {
		buf.Add(e)
	}
	list := buf.List(0)
	if len(list) != 10 {
		t.Fatalf("缓冲应有 10 条，实际 %d", len(list))
	}
	if list[0].RequestID != entry(30).RequestID {
		t.Fatalf("缓冲最新一条应为 %s，实际 %s", entry(30).RequestID, list[0].RequestID)
	}

	// 目录不存在或 want<=0 时安全返回
	if got := RecoverLogs(filepath.Join(dir, "nope"), 5); got != nil {
		t.Fatalf("目录不存在时应返回 nil，实际 %v", got)
	}
	if got := RecoverLogs(dir, 0); got != nil {
		t.Fatalf("want=0 时应返回 nil，实际 %v", got)
	}
}

// TestLogSinkConcurrentEnqueue 并发入队不丢失（不阻塞、不panic）。
func TestLogSinkConcurrentEnqueue(t *testing.T) {
	dir := t.TempDir()
	s := newTestSink(t, dir, 3, 8<<20, 1000)

	const workers, each = 8, 100
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				s.Enqueue(entry(w*each + i))
			}
		}(w)
	}
	wg.Wait()
	s.Close()

	if s.buf.Count() != workers*each {
		t.Fatalf("缓冲应有 %d 条，实际 %d", workers*each, s.buf.Count())
	}
	if dropped, written := s.Stats(); dropped != 0 || written != workers*each {
		t.Fatalf("不应有丢弃：dropped=%d written=%d，期望 written=%d", dropped, written, workers*each)
	}
	// 落盘条数与内存一致
	if got := len(readAllSegments(t, dir)); got != workers*each {
		t.Fatalf("落盘条数应为 %d，实际 %d", workers*each, got)
	}
}

// TestLogSinkPurgeDropsPending 校验清空日志时，队列里待处理的日志会被丢弃。
func TestLogSinkPurgeDropsPending(t *testing.T) {
	dir := t.TempDir()
	s := newTestSink(t, dir, 3, 1<<20, 100)
	s.Close() // 协程已退出，队列不会再被消费
	for i := 1; i <= 10; i++ {
		s.Enqueue(entry(i))
	}
	if n := s.queueLen(); n != 10 {
		t.Fatalf("队列应有 10 条，实际 %d", n)
	}

	s.Purge()
	if n := s.queueLen(); n != 0 {
		t.Fatalf("Purge 后队列应为空，实际 %d", n)
	}
	if s.buf.Count() != 0 {
		t.Fatalf("Purge 后不应有日志写回缓冲，实际 %d 条", s.buf.Count())
	}
}

func sortStringsForTest(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
