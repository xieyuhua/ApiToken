// 访问日志的查询视图：分页、过滤、导出与详情。
package store

import (
	"github.com/xieyuhua/apitoken/internal/model"
)

// Logs 读取最近 n 条访问日志.
func (s *Store) Logs(n int) []model.LogEntry { return s.logs.List(n) }

// LogsCount 当前内存中保留的日志条数（O(1)，不拷贝缓冲）。
func (s *Store) LogsCount() int { return s.logs.Count() }

// ForEachLog 在日志缓冲内遍历（最新在前），不产生切片分配。
// 用于筛选项汇总这类只需汇总值的场景，避免整份拷贝。
func (s *Store) ForEachLog(fn func(model.LogEntry)) { s.logs.ForEach(fn) }

// QueryLogs 分页查询访问日志（含聚合统计）。数据源是内存热缓冲（最近 keep_logs 条）。
func (s *Store) QueryLogs(q model.LogQuery) model.LogPage {
	items := s.logs.Filter(func(e model.LogEntry) bool { return Match(e, q) })
	return paginateLogs(items, q, s.logs.Count())
}

// QueryDiskLogs 在磁盘归档里分页查询访问日志（含聚合统计）。
//
// 与 QueryLogs 只差数据源：这里扫 data/logs 下的 JSONL 分段文件，能查到超出
// 内存保留窗口的历史数据。日志未开启落盘（file_enabled=false）时返回空结果。
// 代价是比内存查询慢（要读文件），所以默认仍走 QueryLogs，页面显式切换才用它。
func (s *Store) QueryDiskLogs(q model.LogQuery) model.LogPage {
	var items []model.LogEntry
	if s.sink != nil {
		items = s.sink.Search(q)
	}
	return paginateLogs(items, q, len(items))
}

// paginateLogs 是日志分页的统一实现（内存查询与磁盘归档查询共用）。
// kept 表示数据源侧的规模：内存查询传缓冲保留条数，磁盘查询传本次检索到的命中条数
// （受 maxHistoryResults 上限约束）。
func paginateLogs(items []model.LogEntry, q model.LogQuery, kept int) model.LogPage {
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
		Kept:     kept,
		Stats:    summary,
	}
}

// ExportLogs 返回全部匹配的日志（最新在前），不套用分页的 page_size 上限。
// CSV/JSON 导出需要完整数据，若走 QueryLogs 会被 500 条分页上限截断成第一页。
func (s *Store) ExportLogs(q model.LogQuery) []model.LogEntry {
	return s.logs.Filter(func(e model.LogEntry) bool { return Match(e, q) })
}

// ExportDiskLogs 返回磁盘归档中全部匹配的日志（最新在前），不套分页上限。
// 导出历史数据（history=1）时使用；日志未开启落盘时返回 nil。
func (s *Store) ExportDiskLogs(q model.LogQuery) []model.LogEntry {
	if s.sink == nil {
		return nil
	}
	return s.sink.Search(q)
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
