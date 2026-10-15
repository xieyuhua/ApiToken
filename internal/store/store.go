// Package store 维护渠道、路由、统计与访问日志，并持久化到磁盘。
package store

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xieyuhua/apitoken/internal/config"
	"github.com/xieyuhua/apitoken/internal/model"
)

// Store 内存态存储。
type Store struct {
	mu       sync.RWMutex
	cfg      *config.Config
	log      *slog.Logger
	dataFile string

	channels map[string]*model.Channel
	order    []string
	routes   []model.Route

	health map[string]HealthInfo
	// cooldownUntil 渠道失败后的冷却截止时间；冷却期内不进候选列表。
	// 只由 Store.mu 保护，读写都在持锁路径上完成。
	cooldownUntil map[string]time.Time

	// noStreamOpts 已确认「不接受 stream_options 字段」的渠道。
	// 记住后该渠道的流式请求不再注入该字段，省掉每次请求先撞一次 400。
	// 仅内存态：上游哪天支持了，重启网关即重新探测。
	noStreamOpts map[string]bool

	// 统计持久化：usage / totals / 渠道 Stats 定时落盘到 stats.json，
	// 重启后恢复，避免「一重启用量数据全清零」。除非用户手动清理，否则一直保留。
	statsFile     string
	statsDirty    atomic.Bool
	statsStop     chan struct{}
	statsStopOnce sync.Once

	usage  map[string]*model.ModelUsage
	totals model.ModelUsage

	// hourly 按小时聚合的统计桶（趋势图用），key = 整点 Unix 秒。
	// 与 usage/totals 同属请求级统计，写在同一把 s.mu 下。
	hourly map[int64]*model.ModelUsage

	// persistMu 串行化所有“写文件”动作：gateway.json 与 stats.json 都走它。
	//
	// 两条持久化路径（管理写操作、统计定时落盘）各自的目标是不同文件，
	// 不会互相覆盖内容，但共用一把锁后：
	//   1. 统计落盘与管理写盘不会同时进行，磁盘峰值只有一次，避免落盘抖动；
	//   2. 不需要再来第二把锁（statsSaveMu），省去“两把锁各写一半”的心智负担。
	// 两条路径都是低频写（统计 3s 一次、管理写随操作触发），串行化代价可忽略。
	persistMu sync.Mutex

	rr sync.Map // model 名 -> *uint64 轮询游标，读多写少，避免全局互斥

	logs *LogBuffer
	sink *LogSink // 异步落盘管道；为 nil 时日志只进内存

	started time.Time
}

type HealthInfo struct {
	Status    string    `json:"status"`
	CheckedAt time.Time `json:"checked_at"`
	Error     string    `json:"error,omitempty"`
	LatencyMS int64     `json:"latency_ms,omitempty"`
	Detail    string    `json:"detail,omitempty"`
}

type persisted struct {
	Channels []model.Channel `json:"channels"`
	Routes   []model.Route   `json:"routes"`
}

// New 创建 Store，优先从数据文件恢复，其次使用配置文件内容。
func New(cfg *config.Config, log *slog.Logger) (*Store, error) {
	s := &Store{
		cfg:           cfg,
		log:           log,
		dataFile:      cfg.DataFile(),
		channels:      map[string]*model.Channel{},
		health:        map[string]HealthInfo{},
		usage:         map[string]*model.ModelUsage{},
		hourly:        map[int64]*model.ModelUsage{},
		cooldownUntil: map[string]time.Time{},
		noStreamOpts:  map[string]bool{},
		logs:          NewLogBuffer(cfg.RT().KeepLogs),
		started:       time.Now(),
	}
	// 启动日志落盘管道；若配置为纯内存模式，此处返回 nil 并退化为直接入缓冲
	s.sink = newLogSink(cfg, s.logs, log)
	seeded, err := s.load()
	if err != nil {
		return nil, err
	}
	// 统计恢复：渠道已就绪后再灌入历史统计
	s.statsFile = statsFilePath(s.dataFile)
	s.loadStats()
	s.statsStop = make(chan struct{})
	go s.statsFlushLoop()
	// 仅当数据文件不存在（首次启动）或损坏时，用配置文件的 channels/routes 播种。
	// 一旦数据文件存在，就以它为准：不再把 config.yaml 里的渠道补进内存，
	// 否则「管理界面删除渠道后重启」会被 config 里的旧渠道复活。
	if !seeded {
		for i := range cfg.Channels {
			ch := cfg.Channels[i]
			if ch.Weight <= 0 {
				ch.Weight = 1
			}
			if ch.CreatedAt.IsZero() {
				ch.CreatedAt = time.Now()
			}
			s.channels[ch.ID] = &ch
			s.order = append(s.order, ch.ID)
		}
		s.routes = append(s.routes, cfg.Routes...)
		if err := s.save(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// load 从数据文件恢复渠道与路由。返回 loaded 表示「数据文件存在且解析成功」：
// 只有 loaded=false（文件不存在或损坏）才需要用配置文件播种，避免
// 「管理界面删光/删除渠道后，重启又被 config.yaml 复活」。
func (s *Store) load() (bool, error) {
	data, err := os.ReadFile(s.dataFile)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("读取 %s 失败: %w", s.dataFile, err)
	}
	var p persisted
	if err := json.Unmarshal(data, &p); err != nil {
		s.log.Warn("数据文件解析失败，忽略", "file", s.dataFile, "err", err)
		return false, nil
	}
	for i := range p.Channels {
		ch := p.Channels[i]
		if ch.Weight <= 0 {
			ch.Weight = 1
		}
		s.channels[ch.ID] = &ch
		s.order = append(s.order, ch.ID)
	}
	s.routes = p.Routes
	return true, nil
}

// save 把渠道与路由原子落盘。
//
// persistMu 串行化写盘：否则两个并发的管理请求会同时写同一个 .tmp 文件并各自 rename，
// 产生交错内容（数据文件损坏）。管理写操作是低频的，串行化不影响吞吐。
func (s *Store) save() error {
	s.persistMu.Lock()
	defer s.persistMu.Unlock()

	p := persisted{Channels: s.ListChannels(), Routes: s.ListRoutes()}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.dataFile), 0o755); err != nil {
		return err
	}
	tmp := s.dataFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.dataFile)
}

// ListChannels 返回所有渠道（按配置顺序）。
func (s *Store) ListChannels() []model.Channel {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]model.Channel, 0, len(s.order))
	for _, id := range s.order {
		if ch, ok := s.channels[id]; ok {
			out = append(out, *ch)
		}
	}
	return out
}

// GetChannel 按 id 取渠道。
func (s *Store) GetChannel(id string) (model.Channel, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ch, ok := s.channels[id]
	if !ok {
		return model.Channel{}, false
	}
	return *ch, true
}

// Upsert 新增或更新渠道，持久化后返回最终结果。
func (s *Store) Upsert(ch model.Channel) (model.Channel, error) {
	s.mu.Lock()
	if ch.ID == "" {
		ch.ID = model.NewChannelID(ch.Name)
	}
	if _, ok := s.channels[ch.ID]; !ok {
		ch.CreatedAt = time.Now()
		s.order = append(s.order, ch.ID)
	} else {
		ch.CreatedAt = s.channels[ch.ID].CreatedAt
	}
	if ch.Weight <= 0 {
		ch.Weight = 1
	}
	ch.UpdatedAt = time.Now()
	cp := ch
	s.channels[ch.ID] = &cp
	s.mu.Unlock()

	if err := s.save(); err != nil {
		return ch, err
	}
	s.log.Info("渠道已保存", "id", ch.ID, "name", ch.Name, "base_url", ch.BaseURL)
	return ch, nil
}

// SetEnabled 启用/停用渠道。
func (s *Store) SetEnabled(id string, enabled bool) (model.Channel, error) {
	s.mu.Lock()
	ch, ok := s.channels[id]
	if !ok {
		s.mu.Unlock()
		return model.Channel{}, fmt.Errorf("渠道 %s 不存在", id)
	}
	cp := *ch
	cp.Enabled = enabled
	cp.UpdatedAt = time.Now()
	s.channels[id] = &cp
	s.mu.Unlock()
	if err := s.save(); err != nil {
		return cp, err
	}
	return cp, nil
}

// DeleteChannel 删除渠道，并清理相关路由引用。
func (s *Store) DeleteChannel(id string) error {
	s.mu.Lock()
	if _, ok := s.channels[id]; !ok {
		s.mu.Unlock()
		return fmt.Errorf("渠道 %s 不存在", id)
	}
	delete(s.channels, id)
	delete(s.health, id)
	delete(s.cooldownUntil, id)
	delete(s.noStreamOpts, id)
	for i, cid := range s.order {
		if cid == id {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
	for i := range s.routes {
		filtered := s.routes[i].Channels[:0]
		for _, cid := range s.routes[i].Channels {
			if cid != id {
				filtered = append(filtered, cid)
			}
		}
		s.routes[i].Channels = filtered
	}
	s.mu.Unlock()
	return s.save()
}

// ListRoutes 返回所有路由。
func (s *Store) ListRoutes() []model.Route {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]model.Route, len(s.routes))
	copy(out, s.routes)
	return out
}

// UpsertRoute 新增或更新路由（按 model 名唯一）。
func (s *Store) UpsertRoute(r model.Route) (model.Route, error) {
	s.mu.Lock()
	found := false
	for i := range s.routes {
		if s.routes[i].Model == r.Model {
			s.routes[i] = r
			found = true
			break
		}
	}
	if !found {
		s.routes = append(s.routes, r)
	}
	s.mu.Unlock()
	if err := s.save(); err != nil {
		return r, err
	}
	return r, nil
}

// DeleteRoute 删除路由。
func (s *Store) DeleteRoute(modelName string) error {
	s.mu.Lock()
	for i := range s.routes {
		if s.routes[i].Model == modelName {
			s.routes = append(s.routes[:i], s.routes[i+1:]...)
			break
		}
	}
	s.mu.Unlock()
	return s.save()
}

// SetHealth 记录渠道健康状态。
func (s *Store) SetHealth(id string, h HealthInfo) {
	s.mu.Lock()
	s.health[id] = h
	s.mu.Unlock()
}

// OnRuntimeChanged 配置热更新回调（目前用于调整日志容量）。
func (s *Store) OnRuntimeChanged(rt *config.Runtime) {
	s.logs.SetMax(rt.KeepLogs)
}

// AddLog 投递一条访问日志。
//
// 只做一次非阻塞 channel 发送：落盘与内存入缓冲都在后台协程里完成，
// 因此这条路径不会把磁盘 IO 或缓冲锁带进请求处理。
func (s *Store) AddLog(e model.LogEntry) {
	if s.sink != nil {
		s.sink.Enqueue(e)
		return
	}
	s.logs.Add(e)
}

// LogSinkStats 返回落盘管线的累计丢弃/已落盘条数；未开启落盘时返回 0。
//
// 丢弃是刻意的选择（不拿日志反压网关），但必须可被观测：队列长期满说明磁盘
// 跟不上写入量，靠这个计数与诊断页的检查项就能发现，而不必等到用户说“日志不全”。
func (s *Store) LogSinkStats() (dropped, written int64) {
	if s.sink == nil {
		return 0, 0
	}
	return s.sink.Stats()
}

// CloseLogs 停止日志协程并 flush 残留日志（进程退出前调用）。
// 同时做统计的最终落盘，保证优雅退出不丢最近几秒的用量。
func (s *Store) CloseLogs() {
	if s.sink != nil {
		s.sink.Close()
	}
	s.stopStatsFlusher()
}

// Config 返回配置。
func (s *Store) Config() *config.Config { return s.cfg }

// Uptime 运行时长。
func (s *Store) Uptime() time.Duration { return time.Since(s.started) }

// UptimeString 运行时长的人类可读格式（如「2 天 3 小时 5 分」）。
func (s *Store) UptimeString() string { return FormatUptime(s.Uptime()) }

// FormatUptime 把时长格式化为紧凑可读的中文描述：
// 不足 1 分显示秒、不足 1 小时显示「X 分 Y 秒」、不足 1 天显示「X 小时 Y 分」、否则「X 天 X 小时」。
// 相比 time.Duration.String()（如 1h2m3.456789s），去掉无意义的亚秒精度并补上天数。
func FormatUptime(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	secs := int64(d / time.Second)
	switch {
	case secs < 60:
		return fmt.Sprintf("%d 秒", secs)
	case secs < 3600:
		return fmt.Sprintf("%d 分 %d 秒", secs/60, secs%60)
	case secs < 86400:
		return fmt.Sprintf("%d 小时 %d 分", secs/3600, (secs%3600)/60)
	default:
		return fmt.Sprintf("%d 天 %d 小时", secs/86400, (secs%86400)/3600)
	}
}
