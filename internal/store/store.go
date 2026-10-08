// Package store 维护渠道、路由、统计与访问日志，并持久化到磁盘。
package store

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/demo1/apitoken/internal/config"
	"github.com/demo1/apitoken/internal/model"
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

	usage  map[string]*model.ModelUsage
	totals model.ModelUsage

	rrMu sync.Mutex
	rr   map[string]*uint64

	logs *LogBuffer

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
		cfg:      cfg,
		log:      log,
		dataFile: cfg.DataFile(),
		channels: map[string]*model.Channel{},
		health:   map[string]HealthInfo{},
		usage:    map[string]*model.ModelUsage{},
		rr:       map[string]*uint64{},
		logs:     NewLogBuffer(cfg.RT().KeepLogs),
		started:  time.Now(),
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	if len(s.channels) == 0 {
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
	} else {
		// 配置文件中新增的渠道补充进来（已存在的保持运行期修改结果）
		for i := range cfg.Channels {
			ch := cfg.Channels[i]
			if _, ok := s.channels[ch.ID]; ok {
				continue
			}
			if ch.Weight <= 0 {
				ch.Weight = 1
			}
			if ch.CreatedAt.IsZero() {
				ch.CreatedAt = time.Now()
			}
			s.channels[ch.ID] = &ch
			s.order = append(s.order, ch.ID)
		}
	}
	return s, nil
}

func (s *Store) load() error {
	data, err := os.ReadFile(s.dataFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("读取 %s 失败: %w", s.dataFile, err)
	}
	var p persisted
	if err := json.Unmarshal(data, &p); err != nil {
		s.log.Warn("数据文件解析失败，忽略", "file", s.dataFile, "err", err)
		return nil
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
	return nil
}

func (s *Store) save() error {
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
		ch.ID = genID(ch.Name)
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

// Candidates 计算某个对外模型名的候选渠道列表（已按策略排序）。
func (s *Store) Candidates(name string) []model.Candidate {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// 1) 显式路由优先
	for _, r := range s.routes {
		if r.Model != name || !r.IsEnabled() {
			continue
		}
		var list []model.Candidate
		for _, cid := range r.Channels {
			ch, ok := s.channels[cid]
			if !ok || !ch.Enabled {
				continue
			}
			up := name
			if m, ok := r.ModelMap[cid]; ok && m != "" {
				up = m
			} else if m, ok := ch.Alias[name]; ok {
				up = m
			}
			list = append(list, model.Candidate{
				Channel: *ch, ID: ch.ID, Name: ch.Name, UpstreamModel: up,
			})
		}
		if len(list) > 0 {
			// 显式路由严格按用户声明的顺序执行故障转移（不再按 priority 重排）
			return list
		}
	}

	// 2) 自动路由：按渠道 models / alias 匹配
	var list []model.Candidate
	for _, cid := range s.order {
		ch, ok := s.channels[cid]
		if !ok {
			continue
		}
		up, ok := ch.Supports(name)
		if !ok {
			continue
		}
		list = append(list, model.Candidate{
			Channel: *ch, ID: ch.ID, Name: ch.Name, UpstreamModel: up,
		})
	}
	return s.orderCandidates(name, list)
}

// orderCandidates 按配置策略排序候选（调用方需持读锁）。
func (s *Store) orderCandidates(name string, list []model.Candidate) []model.Candidate {
	strategy := s.cfg.RT().Strategy
	if len(list) <= 1 || strategy == "failover" {
		sort.SliceStable(list, func(i, j int) bool { return list[i].Channel.Priority < list[j].Channel.Priority })
		return list
	}

	if strategy == "priority_round_robin" || strategy == "random" {
		// 按优先级分组，组内轮询
		groups := map[int][]model.Candidate{}
		order := []int{}
		for _, c := range list {
			p := c.Channel.Priority
			if _, ok := groups[p]; !ok {
				order = append(order, p)
			}
			groups[p] = append(groups[p], c)
		}
		sort.Ints(order)
		out := make([]model.Candidate, 0, len(list))
		for _, p := range order {
			out = append(out, s.rotate(name, groups[p], strategy)...)
		}
		return out
	}

	// round_robin / 其它：忽略优先级，整体轮询
	return s.rotate(name, list, strategy)
}

func (s *Store) rotate(name string, list []model.Candidate, strategy string) []model.Candidate {
	if strategy == "random" {
		out := make([]model.Candidate, len(list))
		copy(out, list)
		rand.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
		return out
	}
	// 加权展开：权重越大出现次数越多
	seq := make([]model.Candidate, 0, len(list)*4)
	for _, c := range list {
		w := c.Channel.Weight
		if w <= 0 {
			w = 1
		}
		if w > 20 {
			w = 20
		}
		for i := 0; i < w; i++ {
			seq = append(seq, c)
		}
	}
	n := s.nextRoundRobin(name)
	start := int(n % uint64(len(seq)))
	out := make([]model.Candidate, 0, len(seq))
	for i := 0; i < len(seq); i++ {
		out = append(out, seq[(start+i)%len(seq)])
	}
	return dedup(out)
}

func dedup(in []model.Candidate) []model.Candidate {
	seen := map[string]bool{}
	out := in[:0]
	for _, c := range in {
		if seen[c.ID] {
			continue
		}
		seen[c.ID] = true
		out = append(out, c)
	}
	return out
}

func (s *Store) nextRoundRobin(name string) uint64 {
	s.rrMu.Lock()
	defer s.rrMu.Unlock()
	v, ok := s.rr[name]
	if !ok {
		v = new(uint64)
		s.rr[name] = v
	}
	return atomic.AddUint64(v, 1)
}

// PublicModels 汇总对外可用的模型名。
func (s *Store) PublicModels() []ModelInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()

	byModel := map[string][]model.Candidate{}
	seen := map[string]bool{}
	add := func(name string, ch *model.Channel, up string) {
		key := name + "|" + ch.ID + "|" + up
		if seen[key] {
			return
		}
		seen[key] = true
		byModel[name] = append(byModel[name], model.Candidate{
			Channel: *ch, ID: ch.ID, Name: ch.Name, UpstreamModel: up,
		})
	}
	// 自动路由：按渠道 models / alias 字段匹配
	collect := func(name string) {
		for _, cid := range s.order {
			ch, ok := s.channels[cid]
			if !ok || !ch.Enabled {
				continue
			}
			if up, ok := ch.Supports(name); ok {
				add(name, ch, up)
			}
		}
	}
	// 显式路由：直接使用渠道列表（不受渠道 models 字段限制）
	for _, r := range s.routes {
		if !r.IsEnabled() || r.Model == "" {
			continue
		}
		for _, cid := range r.Channels {
			ch, ok := s.channels[cid]
			if !ok || !ch.Enabled {
				continue
			}
			up := r.Model
			if m, ok := r.ModelMap[cid]; ok && m != "" {
				up = m
			} else if m, ok := ch.Alias[r.Model]; ok {
				up = m
			}
			add(r.Model, ch, up)
		}
	}
	for _, cid := range s.order {
		if ch, ok := s.channels[cid]; ok && ch.Enabled {
			for _, m := range ch.Models {
				if m == "*" {
					continue
				}
				collect(m)
			}
			for m := range ch.Alias {
				collect(m)
			}
		}
	}

	names := make([]string, 0, len(byModel))
	for m := range byModel {
		names = append(names, m)
	}
	sort.Strings(names)

	out := make([]ModelInfo, 0, len(names))
	for _, n := range names {
		ups := make([]UpstreamInfo, 0, len(byModel[n]))
		for _, c := range byModel[n] {
			ups = append(ups, UpstreamInfo{
				ChannelID: c.ID, ChannelName: c.Name, UpstreamModel: c.UpstreamModel,
			})
		}
		out = append(out, ModelInfo{ID: n, Object: "model", Created: s.started.Unix(), OwnedBy: "apitoken", Upstreams: ups})
	}
	return out
}

// ModelInfo /v1/models 中的模型项。
type ModelInfo struct {
	ID        string         `json:"id"`
	Object    string         `json:"object"`
	Created   int64          `json:"created"`
	OwnedBy   string         `json:"owned_by"`
	Upstreams []UpstreamInfo `json:"upstreams,omitempty"`
}

// UpstreamInfo 模型对应的上游信息。
type UpstreamInfo struct {
	ChannelID     string `json:"channel_id"`
	ChannelName   string `json:"channel_name"`
	UpstreamModel string `json:"upstream_model"`
}

// RecordChannelAttempt 记录一次上游调用结果。
func (s *Store) RecordChannelAttempt(channelID string, ok bool, latency time.Duration, prompt, completion int64, errMsg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch, exists := s.channels[channelID]
	if !exists {
		return
	}
	ch.Stats.Requests++
	ch.Stats.TotalLatencyMS += latency.Milliseconds()
	ch.Stats.LastUsed = time.Now()
	if ok {
		ch.Stats.Success++
		ch.Stats.PromptTokens += prompt
		ch.Stats.CompletionTokens += completion
	} else {
		ch.Stats.Failed++
		if errMsg != "" {
			ch.Stats.LastError = errMsg
		}
	}
}

// RecordRequest 记录一次客户端请求（最终结果）。
func (s *Store) RecordRequest(name string, ok bool, latency time.Duration, prompt, completion int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.usage[name]
	if u == nil {
		u = &model.ModelUsage{}
		s.usage[name] = u
	}
	u.Requests++
	u.TotalLatencyMS += latency.Milliseconds()
	s.totals.Requests++
	s.totals.TotalLatencyMS += latency.Milliseconds()
	if ok {
		u.Success++
		u.PromptTokens += prompt
		u.CompletionTokens += completion
		s.totals.Success++
		s.totals.PromptTokens += prompt
		s.totals.CompletionTokens += completion
	} else {
		u.Failed++
		s.totals.Failed++
	}
}

// Snapshot 统计快照。
func (s *Store) Snapshot() model.Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()

	snap := model.Snapshot{
		Version:          model.Version,
		UptimeSeconds:    int64(time.Since(s.started).Seconds()),
		StartedAt:        s.started,
		TotalRequests:    s.totals.Requests,
		TotalSuccess:     s.totals.Success,
		TotalFailed:      s.totals.Failed,
		PromptTokens:     s.totals.PromptTokens,
		CompletionTokens: s.totals.CompletionTokens,
		Models:           map[string]*model.ModelUsage{},
		ChannelCount:     len(s.order),
	}
	if snap.TotalRequests > 0 {
		snap.AvgLatencyMS = s.totals.TotalLatencyMS / snap.TotalRequests
	}
	for k, v := range s.usage {
		cp := *v
		snap.Models[k] = &cp
	}
	for _, id := range s.order {
		ch, ok := s.channels[id]
		if !ok {
			continue
		}
		if ch.Enabled {
			snap.EnabledChannels++
		}
		h := s.health[id]
		status := h.Status
		if status == "" {
			status = "unknown"
		}
		snap.Channels = append(snap.Channels, model.ChannelStat{
			ID: ch.ID, Name: ch.Name, BaseURL: ch.BaseURL,
			Enabled: ch.Enabled, Weight: ch.Weight, Priority: ch.Priority, Models: ch.Models,
			MaskedKey: model.MaskSecret(ch.APIKey), Health: status, LastCheckedAt: h.CheckedAt,
			LastError: firstNonEmpty(ch.Stats.LastError, h.Error), LastUsed: ch.Stats.LastUsed, Stats: ch.Stats,
		})
	}
	return snap
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// SetHealth 记录渠道健康状态。
func (s *Store) SetHealth(id string, h HealthInfo) {
	s.mu.Lock()
	s.health[id] = h
	s.mu.Unlock()
}

// Health 读取渠道健康状态。
func (s *Store) Health(id string) (HealthInfo, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	h, ok := s.health[id]
	return h, ok
}

// OnRuntimeChanged 配置热更新回调（目前用于调整日志容量）。
func (s *Store) OnRuntimeChanged(rt *config.Runtime) {
	s.logs.SetMax(rt.KeepLogs)
}

// AddLog 追加访问日志。
func (s *Store) AddLog(e model.LogEntry) { s.logs.Add(e) }

// Logs 读取最近 n 条访问日志。
func (s *Store) Logs(n int) []model.LogEntry { return s.logs.List(n) }

// QueryLogs 分页查询访问日志（含聚合统计）。
func (s *Store) QueryLogs(q model.LogQuery) model.LogPage {
	items := s.logs.Filter(func(e model.LogEntry) bool { return Match(e, q) })
	summary := BuildSummary(items)

	page, size := q.Page, q.PageSize
	if page <= 0 {
		page = 1
	}
	if size <= 0 {
		size = 50
	}
	if size > 500 {
		size = 500
	}
	total := len(items)
	pages := (total + size - 1) / size
	if pages == 0 {
		pages = 1
	}
	if page > pages {
		page = pages
	}
	start := (page - 1) * size
	end := start + size
	if start > total {
		start = total
	}
	if end > total {
		end = total
	}
	return model.LogPage{
		Items:    items[start:end],
		Total:    total,
		Page:     page,
		PageSize: size,
		Pages:    pages,
		Kept:     s.logs.Count(),
		Stats:    summary,
	}
}

// RequestLogs 返回同一 request_id 的全部日志（故障转移会产生多条）。
func (s *Store) RequestLogs(requestID string) []model.LogEntry {
	all := s.logs.Snapshot()
	out := make([]model.LogEntry, 0, 2)
	for _, e := range all {
		if e.RequestID == requestID {
			out = append(out, e)
		}
	}
	return out
}

// ClearLogs 清空访问日志。
func (s *Store) ClearLogs() int {
	n := s.logs.Count()
	s.logs.Clear()
	return n
}

// Strategy 返回当前路由策略。
func (s *Store) Strategy() string { return s.cfg.Routing.Strategy }

// Config 返回配置。
func (s *Store) Config() *config.Config { return s.cfg }

// Uptime 运行时长。
func (s *Store) Uptime() time.Duration { return time.Since(s.started) }

func genID(name string) string {
	base := "ch"
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			base += string(r)
		} else if r >= 'A' && r <= 'Z' {
			base += string(r + 32)
		}
	}
	if len(base) <= 2 {
		base = "channel"
	}
	return fmt.Sprintf("%s-%d", base, time.Now().UnixNano()%100000)
}
