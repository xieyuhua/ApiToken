package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/xieyuhua/apitoken/internal/model"
)

// statsFlushInterval 统计落盘间隔：有变更时最多延迟这么久写入 stats.json。
// 统计属于可累积数据，不值得每个请求都写一次盘；3 秒足以把进程被杀的损失
// 控制在一两批请求内。
const statsFlushInterval = 3 * time.Second

// statsFile 统计数据的磁盘快照结构（data/stats.json）。
// 与 gateway.json 分开存放的原因：gateway.json 由管理写操作全量重写，
// 混入高频变化的统计会放大并发写冲突。
type statsFile struct {
	Channels map[string]model.Stats      `json:"channels"`
	Usage    map[string]model.ModelUsage `json:"usage"`
	Totals   model.ModelUsage            `json:"totals"`
	// Hourly 趋势用的小时桶（key 为整点 Unix 秒），随统计一起持久化，重启后趋势不断档
	Hourly  map[int64]model.ModelUsage `json:"hourly,omitempty"`
	SavedAt time.Time                  `json:"saved_at"`
}

// statsSnapshotLocked 生成统计快照。调用方需持有 s.mu（读或写）。
func (s *Store) statsSnapshotLocked() statsFile {
	f := statsFile{
		Channels: make(map[string]model.Stats, len(s.channels)),
		Usage:    make(map[string]model.ModelUsage, len(s.usage)),
		Hourly:   make(map[int64]model.ModelUsage, len(s.hourly)),
		Totals:   s.totals,
		SavedAt:  time.Now(),
	}
	for id, ch := range s.channels {
		f.Channels[id] = ch.Stats
	}
	for name, u := range s.usage {
		f.Usage[name] = *u
	}
	for k, u := range s.hourly {
		f.Hourly[k] = *u
	}
	return f
}

// loadStats 从 data/stats.json 恢复统计（渠道 / 模型 / 全局）。
// 文件不存在时静默跳过；损坏只告警：统计属于可观测数据，不该阻碍网关启动。
func (s *Store) loadStats() {
	if s.statsFile == "" {
		return
	}
	data, err := os.ReadFile(s.statsFile)
	if err != nil {
		return
	}
	var f statsFile
	if err := json.Unmarshal(data, &f); err != nil {
		s.log.Warn("统计数据解析失败，按空统计启动", "file", s.statsFile, "err", err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, st := range f.Channels {
		if ch, ok := s.channels[id]; ok {
			ch.Stats = st
		}
	}
	s.usage = make(map[string]*model.ModelUsage, len(f.Usage))
	for name, u := range f.Usage {
		cp := u
		s.usage[name] = &cp
	}
	s.hourly = make(map[int64]*model.ModelUsage, len(f.Hourly))
	for k, u := range f.Hourly {
		cp := u
		s.hourly[k] = &cp
	}
	s.totals = f.Totals
}

// flushStats 把统计写盘。无变更时直接返回（脏标记在写入前置位）。
func (s *Store) flushStats() {
	if !s.statsDirty.Swap(false) {
		return
	}
	if s.statsFile == "" {
		return
	}
	s.statsSaveMu.Lock()
	defer s.statsSaveMu.Unlock()

	// 小时桶需要在写锁下清理过期数据，故快照整体走写锁而非读锁：
	// 清理与快照在同一临界区内，落盘的 Hourly 保证已剔除过期桶。
	// 每 3s 一次的短临界区（只拷贝少量 map）对转发路径影响可忽略。
	s.mu.Lock()
	s.pruneHourlyLocked(time.Now())
	f := s.statsSnapshotLocked()
	s.mu.Unlock()

	data, err := json.MarshalIndent(f, "", " ")
	if err != nil {
		s.log.Warn("统计序列化失败", "err", err)
		return
	}
	tmp := s.statsFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		s.log.Warn("统计写盘失败", "file", tmp, "err", err)
		return
	}
	if err := os.Rename(tmp, s.statsFile); err != nil {
		s.log.Warn("统计落盘失败", "file", s.statsFile, "err", err)
	}
}

// statsFlushLoop 后台定时落盘统计；进程退出前由 stopStatsFlusher 收尾。
func (s *Store) statsFlushLoop() {
	ticker := time.NewTicker(statsFlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.flushStats()
		case <-s.statsStop:
			return
		}
	}
}

// stopStatsFlusher 停止后台协程并做最后一次 flush（优雅退出时调用）。
func (s *Store) stopStatsFlusher() {
	s.statsStopOnce.Do(func() { close(s.statsStop) })
	s.flushStats()
}

// markStatsDirty 标记统计有变更，等待下次 flush。
// 在所有统计写入路径的持锁区间内调用，避免丢失最后一批增量。
func (s *Store) markStatsDirty() { s.statsDirty.Store(true) }

// statsFilePath 统计数据文件路径（与 gateway.json 同目录）。
func statsFilePath(dataFile string) string {
	if dataFile == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(dataFile), "stats.json")
}

// StatsFile 统计数据文件路径（供诊断页展示）。
func (s *Store) StatsFile() string { return s.statsFile }
