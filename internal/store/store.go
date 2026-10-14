// Package store 维护渠道、路由、统计与访问日志，并持久化到磁盘。
package store

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
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
	statsSaveMu   sync.Mutex
	statsDirty    atomic.Bool
	statsStop     chan struct{}
	statsStopOnce sync.Once

	usage  map[string]*model.ModelUsage
	totals model.ModelUsage

	// saveMu 串行化数据文件落盘，避免并发写同一个 .tmp 文件
	saveMu sync.Mutex

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
// saveMu 串行化写盘：否则两个并发的管理请求会同时写同一个 .tmp 文件并各自 rename，
// 产生交错内容（数据文件损坏）。管理写操作是低频的，串行化不影响吞吐。
func (s *Store) save() error {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()

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

// Candidates 计算某个对外模型名的候选渠道列表（已按策略排序）。
func (s *Store) Candidates(name string) []model.Candidate {
	list, fixed := s.candidateSnapshot(name)
	if list == nil {
		return list
	}
	// 冷却过滤放在排序前：把刚失败的渠道剔出候选，让请求直接从下一个健康渠道开始
	// （显式路由同样过滤，仍保持用户声明的相对顺序）。
	// 若过滤后一个不剩（所有渠道都在冷却），则退化为原列表，避免直接把请求判死。
	filtered := s.filterCooldown(list)
	if len(filtered) == 0 {
		filtered = list
	}
	if fixed {
		return filtered
	}
	// 排序、加权展开与轮询游标都放到锁外执行：它们只依赖已拷出的快照数据。
	// 这些计算包含建 map、排序与按权重展开（最多 20 倍），放在读锁内会阻塞
	// RecordRequest / RecordChannelAttempt 的统计写入，抬高整体并发延迟。
	return s.orderCandidates(name, filtered)
}

// filterCooldown 剔除处于失败冷却期内的渠道。必须在锁外调用。
func (s *Store) filterCooldown(in []model.Candidate) []model.Candidate {
	if s.cooldownSetting() <= 0 {
		return in
	}
	s.mu.RLock()
	now := time.Now()
	out := make([]model.Candidate, 0, len(in))
	for _, c := range in {
		until, ok := s.cooldownUntil[c.ID]
		if ok && now.Before(until) {
			continue
		}
		out = append(out, c)
	}
	s.mu.RUnlock()
	return out
}

// cooldownRemaining 渠道剩余冷却时长（<=0 表示可用）。
func (s *Store) cooldownRemaining(id string) time.Duration {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return time.Until(s.cooldownUntil[id])
}

// cooldownSetting 当前生效的冷却时长（配置缺失时按关闭处理）。
func (s *Store) cooldownSetting() time.Duration {
	if s.cfg == nil {
		return 0
	}
	return s.cfg.RT().Cooldown
}

// markCooldown 记录渠道冷却截止时间。调用方需持有 s.mu；
// map 懒初始化以兼容直接构造的 Store（如测试）。
func (s *Store) markCooldown(id string, until time.Time) {
	if s.cooldownUntil == nil {
		s.cooldownUntil = map[string]time.Time{}
	}
	s.cooldownUntil[id] = until
}

// SupportsStreamOptions 渠道是否可以注入 stream_options（未被上游拒绝过）。
func (s *Store) SupportsStreamOptions(id string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return !s.noStreamOpts[id]
}

// MarkStreamOptionsUnsupported 记录「上游不接受 stream_options」，之后不再注入。
func (s *Store) MarkStreamOptionsUnsupported(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.noStreamOpts == nil {
		s.noStreamOpts = map[string]bool{}
	}
	s.noStreamOpts[id] = true
}

// candidateSnapshot 在读锁内取出候选快照。
// fixed 为 true 表示命中显式路由，list 已是最终顺序，不需要再排序。
func (s *Store) candidateSnapshot(name string) (list []model.Candidate, fixed bool) {
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
			return list, true
		}
	}

	// 2) 自动路由：按渠道 models / alias 匹配（仅过滤，排序交给调用方）
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
	return list, false
}

// maxChannelWeight 加权轮询时单个渠道的最大展开倍数（权重再大也按 20 计）。
const maxChannelWeight = 20

// orderCandidates 按配置策略排序候选。
// 必须在锁外调用（内部会取轮询游标与随机源）。
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
		// rand/v2 的包级函数不加全局锁；math/rand 的 Shuffle 会串行化所有并发请求
		rand.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
		return out
	}
	// 加权展开：权重越大出现次数越多
	seq := make([]model.Candidate, 0, len(list)*maxChannelWeight)
	for _, c := range list {
		w := c.Channel.Weight
		if w <= 0 {
			w = 1
		}
		if w > maxChannelWeight {
			w = maxChannelWeight
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
	v, ok := s.rr.Load(name)
	if !ok {
		cur, _ := s.rr.LoadOrStore(name, new(uint64))
		v = cur
	}
	return atomic.AddUint64(v.(*uint64), 1)
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
	s.recordChannelStatsLocked(ch, ok, latency, prompt, completion, errMsg, time.Now())
	s.markStatsDirty()
}

// recordChannelStatsLocked 累加渠道维度统计并维护冷却状态。
// 调用方必须持有 s.mu。
func (s *Store) recordChannelStatsLocked(ch *model.Channel, ok bool, latency time.Duration, prompt, completion int64, errMsg string, now time.Time) {
	ch.Stats.Requests++
	ch.Stats.TotalLatencyMS += latency.Milliseconds()
	ch.Stats.LastUsed = now
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
	// 失败进入冷却：该渠道在冷却时长内不再进入候选列表，
	// 避免「刚失败，下一轮会话又第一个打到它」。
	if ok {
		delete(s.cooldownUntil, ch.ID)
	} else if cd := s.cooldownSetting(); cd > 0 {
		s.markCooldown(ch.ID, now.Add(cd))
	}
}

// RecordFinish 一次性记录一次请求的最终结果与该次尝试的渠道统计。
//
// 合并 RecordChannelAttempt + RecordRequest 的原因：两者都写同一把 Store.mu，
// 分开调用会让每个请求（故障转移时是每次尝试）都连续抢两次全局写锁，
// 高并发下这把锁就是瓶颈；而两次统计本就在同一请求内完成，合并不改变语义。
func (s *Store) RecordFinish(channelID, modelName string, ok bool, total, attempt time.Duration, prompt, completion int64, errMsg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()

	// 渠道维度
	if ch, exists := s.channels[channelID]; exists {
		s.recordChannelStatsLocked(ch, ok, attempt, prompt, completion, errMsg, now)
	}

	// 模型 + 全局维度
	u := s.usage[modelName]
	if u == nil {
		u = &model.ModelUsage{}
		s.usage[modelName] = u
	}
	u.Requests++
	u.TotalLatencyMS += total.Milliseconds()
	s.totals.Requests++
	s.totals.TotalLatencyMS += total.Milliseconds()
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
	s.markStatsDirty()
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
	s.markStatsDirty()
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
		status := h.Status // 空表示尚未做过连通性测试
		var cdMS int64
		if rem := time.Until(s.cooldownUntil[id]); rem > 0 {
			cdMS = rem.Milliseconds()
		}
		snap.Channels = append(snap.Channels, model.ChannelStat{
			ID: ch.ID, Name: ch.Name, BaseURL: ch.BaseURL,
			Enabled: ch.Enabled, Weight: ch.Weight, Priority: ch.Priority, Models: ch.Models,
			MaskedKey: model.MaskSecret(ch.APIKey), Health: status, LastCheckedAt: h.CheckedAt,
			LastError: firstNonEmpty(ch.Stats.LastError, h.Error), LastUsed: ch.Stats.LastUsed,
			CooldownRemainingMS: cdMS, Stats: ch.Stats,
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

// CloseLogs 停止日志协程并 flush 残留日志（进程退出前调用）。
// 同时做统计的最终落盘，保证优雅退出不丢最近几秒的用量。
func (s *Store) CloseLogs() {
	if s.sink != nil {
		s.sink.Close()
	}
	s.stopStatsFlusher()
}

// Logs 读取最近 n 条访问日志.
func (s *Store) Logs(n int) []model.LogEntry { return s.logs.List(n) }

// LogsCount 当前内存中保留的日志条数（O(1)，不拷贝缓冲）。
func (s *Store) LogsCount() int { return s.logs.Count() }

// ForEachLog 在日志缓冲内遍历（最新在前），不产生切片分配。
// 用于筛选项汇总这类只需汇总值的场景，避免整份拷贝。
func (s *Store) ForEachLog(fn func(model.LogEntry)) { s.logs.ForEach(fn) }

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

// ExportLogs 返回全部匹配的日志（最新在前），不套用分页的 page_size 上限。
// CSV/JSON 导出需要完整数据，若走 QueryLogs 会被 500 条分页上限截断成第一页。
func (s *Store) ExportLogs(q model.LogQuery) []model.LogEntry {
	return s.logs.Filter(func(e model.LogEntry) bool { return Match(e, q) })
}

// RequestLogs 返回同一 request_id 的全部日志，按发生顺序（最早 -> 最新）。
func (s *Store) RequestLogs(requestID string) []model.LogEntry {
	// 用 Find 而不是 Snapshot + 遍历：详情页只关心这一两条记录，
	// 不该为此拷贝整个日志缓冲
	out := s.logs.Find(func(e model.LogEntry) bool { return e.RequestID == requestID })
	// 反转成时间正序：最早 -> 最新，最后一条即最终结果
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// ClearLogs 清空访问日志。
// 只清内存视图，不动磁盘上的分段文件（那是历史归档，删了不可恢复）。
func (s *Store) ClearLogs() int {
	n := s.logs.Count()
	if s.sink != nil {
		// 丢弃队列里待处理的日志，否则它们会被后台协程写回，出现"清空后又冒出来"
		s.sink.Purge()
	}
	s.logs.Clear()
	return n
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
