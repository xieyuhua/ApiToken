package store

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/demo1/apitoken/internal/config"
	"github.com/demo1/apitoken/internal/model"
)

// 日志管道参数。
const (
	// logQueueSize 排队缓冲条数。取 4096 足以吸收突发写入又不至于无界占用内存
	//（每条只含结构体与字符串头，底层文本由 GC 回收）。
	logQueueSize = 4096
	// logBatchMax 单批最多落盘条数：攒到就立即写，减少延迟。
	logBatchMax = 256
	// logFlushInterval 攒批的最长等待时间，保证低流量时也能及时落盘。
	logFlushInterval = 200 * time.Millisecond
	// logRecoverChunk 启动恢复时每行允许的最大字节数（防止超长行吃满内存）。
	logRecoverChunk = 4 << 20
)

// LogSink 访问日志的异步落盘管道。
//
// 设计要点（针对"日志不能拖慢网关"）：
//   - 请求路径只做一次非阻塞 channel 写入（纳秒级、无锁、不做磁盘 IO）；
//   - 落盘与内存入缓冲都由后台单个 goroutine 批量完成，磁盘慢只影响日志延迟，
//     不影响转发；
//   - 队列满时**丢弃并计数**而不是阻塞，日志属于可观测数据，不值得用它反压网关；
//   - 落盘为 JSONL 分段文件，追加写，进程被杀最多丢最后一个未 flush 的批次。
type LogSink struct {
	queue  chan model.LogEntry
	buf    *LogBuffer // 热数据缓冲（页面查询走这里）
	dir    string     // 分段文件目录，空则不落盘
	keep   int        // 保留的最近分段文件数
	maxSeg int64      // 单个分段文件大小上限（字节）

	dropped atomic.Int64  // 因队列满而丢弃的条数
	written atomic.Int64  // 已落盘条数
	epoch   atomic.Uint64 // 清空日志的代际号，用于让在途批次自行丢弃

	stopOnce sync.Once
	stopped  chan struct{}
	done     chan struct{}
}

// LogSinkOptions 落盘行为配置。
type LogSinkOptions struct {
	// Dir 分段文件目录；为空表示只保留内存日志（不落盘）。
	Dir string
	// Keep 保留的最近分段文件数，<=0 时用默认值。
	Keep int
	// MaxSegmentBytes 单文件滚动阈值，<=0 时用默认值。
	MaxSegmentBytes int64
	// Buffer 热数据缓冲；不能为空。
	Buffer *LogBuffer
}

// newLogSink 按配置创建日志管道：
//   - file_enabled=false 时返回 nil（纯内存模式，AddLog 直接入缓冲）；
//   - 开启落盘时启动后台协程，并把最近的分段文件尾部恢复进内存缓冲。
func newLogSink(cfg *config.Config, buf *LogBuffer, log *slog.Logger) *LogSink {
	if !cfg.Logging.FileEnabled {
		return nil
	}
	dir := strings.TrimSpace(cfg.Logging.FileDir)
	if dir == "" {
		dir = filepath.Join(cfg.Server.DataDir, "logs")
	}
	s := NewLogSink(LogSinkOptions{
		Dir:             dir,
		Keep:            cfg.Logging.FileKeep,
		MaxSegmentBytes: int64(cfg.Logging.FileMaxMB) << 20,
		Buffer:          buf,
	})
	s.SetLogger(func(format string, args ...any) { log.Warn(fmt.Sprintf(format, args...)) })
	// 重启后让日志页面仍有内容：恢复最近 keep_logs 条
	keep := cfg.RT().KeepLogs
	if keep <= 0 {
		keep = config.DefaultKeepLogs
	}
	for _, e := range RecoverLogs(dir, keep) {
		buf.Add(e)
	}
	return s
}

// NewLogSink 启动后台落盘协程。
func NewLogSink(opts LogSinkOptions) *LogSink {
	keep := opts.Keep
	if keep <= 0 {
		keep = 7
	}
	maxSeg := opts.MaxSegmentBytes
	if maxSeg <= 0 {
		maxSeg = 64 << 20
	}
	s := &LogSink{
		queue:   make(chan model.LogEntry, logQueueSize),
		buf:     opts.Buffer,
		keep:    keep,
		maxSeg:  maxSeg,
		stopped: make(chan struct{}),
		done:    make(chan struct{}),
	}
	if s.buf == nil {
		s.buf = NewLogBuffer(200)
	}
	s.dir = strings.TrimSpace(opts.Dir)
	if s.dir != "" {
		if err := os.MkdirAll(s.dir, 0o755); err != nil {
			// 落盘失败不能拖垮网关：降级为纯内存日志
			s.logf("日志目录不可用，本次仅记录到内存: %v", err)
			s.dir = ""
		}
	}
	go s.loop()
	return s
}

// logf 落盘管线的告警日志（避免与 Store 的 logger 形成循环依赖）。
var logWarn = func(string, ...any) {}

func (s *LogSink) SetLogger(f func(string, ...any)) {
	if f != nil {
		logWarn = f
	}
}

func (s *LogSink) logf(format string, args ...any) { logWarn(format, args...) }

// Enqueue 非阻塞投递一条日志。
//
// 这是请求路径上的唯一动作：一次 channel 发送，无锁、不等待、不做磁盘 IO。
// 队列满时直接丢弃并计数 —— 用日志反压网关是本末倒置。
func (s *LogSink) Enqueue(e model.LogEntry) {
	select {
	case s.queue <- e:
	default:
		if n := s.dropped.Add(1); n%1000 == 1 {
			s.logf("日志队列已满，累计丢弃 %d 条（可调大 keep_logs 或降低流量）", n)
		}
	}
}

// Purge 丢弃队列中尚未处理的日志。
//
// 配合 LogBuffer.Clear 使用：否则"清空日志"之后，队列里残留的日志会被后台
// 协程再写回缓冲，表现为刚清空又冒出旧日志。
// 注意已落盘的分段文件不会被撤回（那是历史归档，属于预期行为）。
func (s *LogSink) Purge() {
	// 先推进代际号，让正在处理的批次在写回内存前自行丢弃
	s.epoch.Add(1)
	for {
		select {
		case <-s.queue:
		default:
			return
		}
	}
}

// queueLen 当前排队条数（观测/测试用）。
func (s *LogSink) queueLen() int { return len(s.queue) }

// Stats 返回丢弃与落盘条数，供 /admin/api-info 等观测。
func (s *LogSink) Stats() (dropped, written int64) {
	return s.dropped.Load(), s.written.Load()
}

// Close 停止后台协程并 flush 残留日志。
func (s *LogSink) Close() {
	s.stopOnce.Do(func() {
		close(s.stopped)
		<-s.done
	})
}

func (s *LogSink) loop() {
	defer close(s.done)

	ticker := time.NewTicker(logFlushInterval)
	defer ticker.Stop()

	var file *os.File
	var bw *bufio.Writer
	var segBytes int64
	var batch []model.LogEntry

	closeFile := func() {
		if bw != nil {
			_ = bw.Flush()
		}
		if file != nil {
			_ = file.Close()
		}
		file, bw, segBytes = nil, nil, 0
	}
	// flushBatch 落盘并写入内存缓冲；文件写失败只告警，不影响网关
	flushBatch := func() {
		if len(batch) == 0 {
			return
		}
		epoch := s.epoch.Load()
		if s.dir != "" {
			n := 0
			for _, e := range batch {
				// 每条之前都检查阈值：单个批次可能远大于分段上限，
				// 只在批次边界检查会让文件无限增长
				if bw == nil || segBytes >= s.maxSeg {
					if bw != nil {
						closeFile() // 写满一段，落盘并滚动
						s.rotate()
					}
					f, name, err := s.openSegment()
					if err != nil {
						s.logf("打开日志分段文件失败，本批仅记录到内存: %v", err)
						break
					}
					file = f
					bw = bufio.NewWriterSize(f, 64<<10)
					segBytes = fileSize(name)
				}
				data, err := json.Marshal(e)
				if err != nil {
					s.logf("序列化日志失败: %v", err)
					break
				}
				if _, err := bw.Write(data); err != nil {
					s.logf("写入日志失败: %v", err)
					break
				}
				_ = bw.WriteByte('\n')
				n++
				segBytes += int64(len(data)) + 1
			}
			if bw != nil {
				_ = bw.Flush()
			}
			s.written.Add(int64(n))
		}
		// 期间调用过 Purge（清空日志）：本批作废，不写回内存缓冲
		if s.epoch.Load() != epoch {
			batch = batch[:0]
			return
		}
		for _, e := range batch {
			s.buf.Add(e)
		}
		batch = batch[:0]
	}

	for {
		select {
		case e := <-s.queue:
			batch = append(batch, e)
			if len(batch) >= logBatchMax {
				flushBatch()
			}
		case <-ticker.C:
			flushBatch()
		case <-s.stopped:
			// 退出前把队列里剩下的都处理完
			for {
				select {
				case e := <-s.queue:
					batch = append(batch, e)
					if len(batch) >= logBatchMax {
						flushBatch()
					}
					continue
				default:
				}
				break
			}
			flushBatch()
			closeFile()
			s.rotate()
			return
		}
	}
}

// openSegment 打开一个新的分段文件。
func (s *LogSink) openSegment() (*os.File, string, error) {
	path := uniqueSegmentPath(s.dir, time.Now())
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, "", err
	}
	return f, path, nil
}

func fileSize(path string) int64 {
	if fi, err := os.Stat(path); err == nil {
		return fi.Size()
	}
	return 0
}

// uniqueSegmentPath 生成不冲突的分段文件名：access-<日期>-<时分秒>.<毫秒>-<序号>.jsonl
//
// 序号不能省：滚动可能发生在同一毫秒内（批量写入时很常见），若只用时间戳，
// 新段会与旧段同名，配合 O_APPEND 就等于写回旧文件，文件将无限增长。
// 序号定长补零，保证字典序 = 时间序（rotate 依赖这个顺序）。
func uniqueSegmentPath(dir string, t time.Time) string {
	base := "access-" + t.Format("20060102-150405.000")
	path := filepath.Join(dir, base+"-000.jsonl")
	for i := 1; i < 1000; i++ {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return path
		}
		path = filepath.Join(dir, fmt.Sprintf("%s-%03d.jsonl", base, i))
	}
	return path
}

// rotate 清理超出保留数量的旧分段文件。
func (s *LogSink) rotate() {
	if s.dir == "" {
		return
	}
	files, err := filepath.Glob(filepath.Join(s.dir, "access-*.jsonl"))
	if err != nil || len(files) <= s.keep {
		return
	}
	sort.Strings(files) // 文件名带时间戳，字典序即时间序
	// 并发实例可能正在写最新的文件，故从最旧的一批开始删，保留 keep 段
	for _, f := range files[:len(files)-s.keep] {
		_ = os.Remove(f)
	}
}

// RecoverLogs 从最近的分段文件尾部恢复若干条日志，返回**时间正序**（最旧 -> 最新），
// 可直接依次写回 LogBuffer。用于重启后让日志页面仍有内容可看。
func RecoverLogs(dir string, want int) []model.LogEntry {
	if strings.TrimSpace(dir) == "" || want <= 0 {
		return nil
	}
	files, err := filepath.Glob(filepath.Join(dir, "access-*.jsonl"))
	if err != nil || len(files) == 0 {
		return nil
	}
	sort.Strings(files)

	var out []model.LogEntry
	// 从最新文件开始往回读，够 want 条就停；每行从文件尾向前取，故结果是倒序
	for i := len(files) - 1; i >= 0 && len(out) < want; i-- {
		out = append(out, readLastLines(files[i], want-len(out))...)
	}
	// 反转成时间正序，调用方才能直接写回缓冲
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// readLastLines 倒序读取文件末尾若干行（避免把整个大文件读进内存）。
func readLastLines(path string, want int) []model.LogEntry {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil
	}
	size := fi.Size()

	const chunk = 64 << 10
	var (
		lines []model.LogEntry
		buf   = make([]byte, 0, chunk)
		pos   = size
		carry []byte // 上一个块末尾未成行的残余
	)
	for pos > 0 && len(lines) < want {
		readSize := int64(chunk)
		if pos < readSize {
			readSize = pos
		}
		pos -= readSize
		buf = make([]byte, readSize)
		if _, err := f.ReadAt(buf, pos); err != nil {
			return nil
		}
		// 拼上上一块的残余，再按行拆分
		data := append(buf, carry...)
		parts := strings.Split(string(data), "\n")
		carry = []byte(parts[0]) // 第一段可能是被截断的半行，留给下一轮
		for i := len(parts) - 1; i >= 1 && len(lines) < want; i-- {
			line := strings.TrimSpace(parts[i])
			if line == "" {
				continue
			}
			if len(line) > logRecoverChunk {
				continue // 异常超长行，丢弃而不是撑爆内存
			}
			var e model.LogEntry
			if err := json.Unmarshal([]byte(line), &e); err != nil {
				continue // 半行或损坏的行直接跳过
			}
			lines = append(lines, e)
		}
	}
	return lines
}
