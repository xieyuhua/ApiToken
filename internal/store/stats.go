// 请求级统计：渠道维度、模型维度、全局汇总与小时趋势桶。
package store

import (
	"time"

	"github.com/xieyuhua/apitoken/internal/model"
	"github.com/xieyuhua/apitoken/internal/util"
)

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
	s.recordHourlyLocked(now, ok, total, prompt, completion)
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
	s.recordHourlyLocked(time.Now(), ok, latency, prompt, completion)
	s.markStatsDirty()
}

// hourlyRetention 趋势数据的保留窗口。7 天足够看清日间波动，
// 同时把桶数量压在一百多个，内存与落盘体积都可忽略。
const hourlyRetention = 7 * 24 * time.Hour

// recordHourlyLocked 把一次请求累加到它所属的小时桶。调用方需持有 s.mu。
//
// 只记录「请求级」维度（次数/成功/失败/token/耗时），不含渠道维度：
// 趋势图回答的是「近期用量如何变化」，不需要把故障转移的每次尝试都算进去。
func (s *Store) recordHourlyLocked(now time.Time, ok bool, latency time.Duration, prompt, completion int64) {
	if s.hourly == nil {
		s.hourly = map[int64]*model.ModelUsage{}
	}
	key := now.Truncate(time.Hour).Unix()
	b := s.hourly[key]
	if b == nil {
		b = &model.ModelUsage{}
		s.hourly[key] = b
	}
	b.Requests++
	b.TotalLatencyMS += latency.Milliseconds()
	if ok {
		b.Success++
		b.PromptTokens += prompt
		b.CompletionTokens += completion
	} else {
		b.Failed++
	}
}

// pruneHourlyLocked 丢弃超出保留窗口的小时桶。调用方需持有 s.mu。
func (s *Store) pruneHourlyLocked(now time.Time) {
	if len(s.hourly) == 0 {
		return
	}
	cutoff := now.Add(-hourlyRetention).Truncate(time.Hour).Unix()
	for k := range s.hourly {
		if k < cutoff {
			delete(s.hourly, k)
		}
	}
}

// HourlySeries 返回最近 hours 个小时的趋势序列（按时间正序）。
// 缺失的小时补全 0 桶，保证前端折线连续、不会因为空档被拉平。
func (s *Store) HourlySeries(hours int) []model.HourlyBucket {
	if hours <= 0 {
		hours = 24
	}
	if hours > int(hourlyRetention/time.Hour) {
		hours = int(hourlyRetention / time.Hour)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	now := time.Now().Truncate(time.Hour)
	start := now.Add(-time.Duration(hours-1) * time.Hour)
	out := make([]model.HourlyBucket, 0, hours)
	for t := start; !t.After(now); t = t.Add(time.Hour) {
		b := model.HourlyBucket{TS: t.Unix()}
		if u := s.hourly[t.Unix()]; u != nil {
			b.Requests = u.Requests
			b.Success = u.Success
			b.Failed = u.Failed
			b.PromptTokens = u.PromptTokens
			b.CompletionTokens = u.CompletionTokens
			b.TotalLatencyMS = u.TotalLatencyMS
		}
		out = append(out, b)
	}
	return out
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
			LastError: util.FirstNonEmpty(ch.Stats.LastError, h.Error), LastUsed: ch.Stats.LastUsed,
			CooldownRemainingMS: cdMS, Stats: ch.Stats,
		})
	}
	return snap
}
