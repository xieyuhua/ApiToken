// 对外模型视图：/v1/models 需要「哪些模型可用、分别映射到哪个渠道」。
package store

import (
	"sort"
	"time"

	"github.com/xieyuhua/apitoken/internal/model"
)

// discoverySnapshot 是「哪些模型对外可用」所需的输入快照。
//
// 取快照而不是持锁遍历：PublicModels 要做 渠道×模型×别名 的笛卡尔展开再排序，
// 属于 O(渠道数 × 模型数) 的重计算，放在读锁内会把 RecordFinish 等统计写入
// 全部挡在外面（RWMutex 的读锁也会阻塞写者），拉高转发路径延迟。
// 快照里只放值拷贝，后续的渠道字段更新不会被这次遍历看到——这不影响正确性：
// /v1/models 本来就是尽力而为的视图，下一秒就会刷新。
type discoverySnapshot struct {
	channels map[string]model.Channel // id -> 渠道值拷贝（仅包含启用渠道）
	order    []string                 // 渠道顺序
	routes   []model.Route
	started  time.Time
}

// snapshotForDiscovery 在读锁内拷贝 Discovery 所需的输入。
func (s *Store) snapshotForDiscovery() discoverySnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	snap := discoverySnapshot{
		channels: make(map[string]model.Channel, len(s.order)),
		order:    make([]string, 0, len(s.order)),
		routes:   append([]model.Route(nil), s.routes...),
		started:  s.started,
	}
	for _, id := range s.order {
		ch, ok := s.channels[id]
		if !ok || !ch.Enabled {
			continue
		}
		snap.channels[id] = *ch
		snap.order = append(snap.order, id)
	}
	return snap
}

// PublicModels 汇总对外可用的模型名。
//
// 只在入口取一次快照，聚合与排序全部在锁外完成（见 discoverySnapshot）。
func (s *Store) PublicModels() []ModelInfo {
	snap := s.snapshotForDiscovery()

	byModel := map[string][]UpstreamInfo{}
	seen := map[string]bool{}
	add := func(name string, ch model.Channel, up string) {
		key := name + "|" + ch.ID + "|" + up
		if seen[key] {
			return
		}
		seen[key] = true
		byModel[name] = append(byModel[name], UpstreamInfo{
			ChannelID: ch.ID, ChannelName: ch.Name, UpstreamModel: up,
		})
	}
	// 自动路由：按渠道 models / alias 字段匹配
	collect := func(name string) {
		for _, id := range snap.order {
			ch := snap.channels[id]
			if up, ok := ch.Supports(name); ok {
				add(name, ch, up)
			}
		}
	}
	// 显式路由：直接使用渠道列表（不受渠道 models 字段限制）
	for _, r := range snap.routes {
		if !r.IsEnabled() || r.Model == "" {
			continue
		}
		for _, cid := range r.Channels {
			ch, ok := snap.channels[cid]
			if !ok {
				continue
			}
			up := r.Model
			if m, ok2 := r.ModelMap[cid]; ok2 && m != "" {
				up = m
			} else if m, ok2 := ch.Alias[r.Model]; ok2 {
				up = m
			}
			add(r.Model, ch, up)
		}
	}
	for _, id := range snap.order {
		ch := snap.channels[id]
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
				ChannelID: c.ChannelID, ChannelName: c.ChannelName, UpstreamModel: c.UpstreamModel,
			})
		}
		out = append(out, ModelInfo{ID: n, Object: "model", Created: snap.started.Unix(), OwnedBy: "apitoken", Upstreams: ups})
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
