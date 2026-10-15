// 候选计算：某个模型应该打到哪些渠道、按什么顺序打。
//
// Store 曾把渠道 CRUD、候选排序、统计、日志查询都堆在一个文件里（1000+ 行、7 个 map、
// 4 把锁），改一处要在长文件里上下翻。这里只放「候选」这一类：锁定一份快照后，
// 冷却过滤与排序都在锁外完成。
package store

import (
	"math/rand/v2"
	"sort"
	"sync/atomic"
	"time"

	"github.com/xieyuhua/apitoken/internal/config"
	"github.com/xieyuhua/apitoken/internal/model"
)

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

// SetCooldown 设置或清除渠道冷却截止时间（后台自动探活使用）。
// until 为零值时清除冷却。真实请求路径的冷却由 recordChannelStatsLocked 维护，
// 这里单独暴露给探活逻辑，让「探活发现渠道挂了」也能提前把它移出候选列表。
func (s *Store) SetCooldown(id string, until time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if until.IsZero() {
		delete(s.cooldownUntil, id)
	} else {
		s.markCooldown(id, until)
	}
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
	if len(list) <= 1 || strategy == config.StrategyFailover {
		sort.SliceStable(list, func(i, j int) bool { return list[i].Channel.Priority < list[j].Channel.Priority })
		return list
	}

	if strategy == config.StrategyPriorityRoundRobin || strategy == config.StrategyRandom {
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
	if strategy == config.StrategyRandom {
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
