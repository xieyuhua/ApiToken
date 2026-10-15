package admin

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/xieyuhua/apitoken/internal/model"
	"github.com/xieyuhua/apitoken/internal/store"
)

// logQuery 从请求参数解析日志查询条件。
func logQuery(r *http.Request) model.LogQuery {
	q := r.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	size, _ := strconv.Atoi(q.Get("page_size"))
	if size == 0 {
		size, _ = strconv.Atoi(q.Get("limit"))
	}
	return model.LogQuery{
		Model:     strings.TrimSpace(q.Get("model")),
		Channel:   strings.TrimSpace(q.Get("channel")),
		RequestID: strings.TrimSpace(q.Get("request_id")),
		Keyword:   strings.TrimSpace(q.Get("keyword")),
		Status:    strings.TrimSpace(q.Get("status")),
		Stream:    strings.TrimSpace(q.Get("stream")),
		Page:      page,
		PageSize:  size,
		From:      parseTime(q.Get("from")),
		To:        parseTime(q.Get("to")),
	}
}

// parseTime 解析时间参数，兼容 RFC3339 与常见的本地写法（2006-01-02 15:04:05 / 2006-01-02）。
// 空串或无法识别的值返回零值（表示不限）：时间范围只是收窄结果的辅助条件，
// 解析失败不该让整次查询失败。
func parseTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, l := range []string{
		time.RFC3339, "2006-01-02 15:04:05", "2006-01-02T15:04:05",
		"2006-01-02T15:04", "2006-01-02 15:04", "2006-01-02",
	} {
		if t, err := time.ParseInLocation(l, s, time.Local); err == nil {
			return t
		}
	}
	return time.Time{}
}

// useDiskArchive 判断本次查询的数据源：
// 以运行时配置 logging.log_archive 为准（设置页调整，立即生效）；
// URL 参数 history=1/0 可临时覆盖，便于在页面上做一次性切换。
func (h *Handler) useDiskArchive(r *http.Request) bool {
	use := h.cfg.RT().LogArchive
	switch r.URL.Query().Get("history") {
	case "1":
		use = true
	case "0":
		use = false
	}
	return use
}

// logs GET /admin/logs —— 分页 + 筛选 + 聚合统计。
// 兼容旧用法：带 limit 参数且未使用分页时，直接返回数组。
func (h *Handler) logs(w http.ResponseWriter, r *http.Request) {
	q := logQuery(r)
	var page model.LogPage
	if h.useDiskArchive(r) {
		page = h.store.QueryDiskLogs(q)
	} else {
		page = h.store.QueryLogs(q)
	}

	if _, ok := r.URL.Query()["limit"]; ok && r.URL.Query().Get("page") == "" {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		items := page.Items
		if limit > 0 && limit < len(items) {
			items = items[:limit]
		}
		writeJSON(w, http.StatusOK, items)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// logDetail GET /admin/logs/{request_id} —— 查看一次请求的全部尝试链路。
func (h *Handler) logDetail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("request_id")
	items := h.store.RequestLogs(id)
	if len(items) == 0 {
		writeErr(w, http.StatusNotFound, "未找到该 request_id 的日志："+id)
		return
	}
	summary := store.BuildSummary(items)
	writeJSON(w, http.StatusOK, map[string]any{
		"request_id": id,
		"model":      items[0].Model,
		"client_ip":  items[0].ClientIP,
		"user_agent": items[0].UserAgent,
		"attempts":   items,
		"summary":    summary,
	})
}

// clearLogs DELETE /admin/logs —— 清空调用日志。
func (h *Handler) clearLogs(w http.ResponseWriter, _ *http.Request) {
	n := h.store.ClearLogs()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "cleared": n})
}

// runtimeLogs GET /admin/runtime-logs —— 查看进程运行日志（slog 输出，最新在前）。
func (h *Handler) runtimeLogs(w http.ResponseWriter, _ *http.Request) {
	rl := store.RuntimeLog()
	writeJSON(w, http.StatusOK, map[string]any{"items": rl.Lines(), "total": rl.Len()})
}

// logFacets GET /admin/logs/facets —— 日志页面下拉选项（模型、渠道、平台）。
func (h *Handler) logFacets(w http.ResponseWriter, _ *http.Request) {
	models := map[string]bool{}
	channels := map[string]string{}
	// 用 ForEach 在缓冲内聚合，避免为拿几个名字拷贝整个日志缓冲
	h.store.ForEachLog(func(e model.LogEntry) {
		if e.Model != "" {
			models[e.Model] = true
		}
		if e.ChannelID != "" {
			channels[e.ChannelID] = e.ChannelName
		}
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"models":   sortedKeys(models),
		"channels": sortedPairs(channels),
	})
}

// exportLogs GET /admin/logs/export —— 导出 CSV / JSON。
func (h *Handler) exportLogs(w http.ResponseWriter, r *http.Request) {
	q := logQuery(r)
	// 导出走 ExportLogs 取全部匹配项，避免被 QueryLogs 的 500 条分页上限截断。
	items := h.store.ExportLogs(q)
	if h.useDiskArchive(r) {
		items = h.store.ExportDiskLogs(q)
	}
	format := r.URL.Query().Get("format")
	stamp := time.Now().Format("20060102-150405")

	if format == "json" {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="apitoken-logs-%s.json"`, stamp))
		writeJSON(w, http.StatusOK, items)
		return
	}

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="apitoken-logs-%s.csv"`, stamp))
	// UTF-8 BOM：让 Excel 正确识别中文，避免打开后乱码
	_, _ = w.Write([]byte("\xEF\xBB\xBF"))
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{
		"time", "request_id", "client_ip", "user_agent", "model", "upstream_model", "channel",
		"stream", "status", "latency_ms", "attempts", "prompt_tokens", "completion_tokens", "error", "endpoint",
	})
	for _, e := range items {
		_ = cw.Write([]string{
			e.Time.Format("2006-01-02 15:04:05.000"), e.RequestID, e.ClientIP, e.UserAgent, e.Model, e.UpstreamModel,
			e.ChannelID, strconv.FormatBool(e.Stream), strconv.Itoa(e.Status),
			strconv.FormatInt(e.LatencyMS, 10), strconv.Itoa(e.Attempts),
			strconv.FormatInt(e.PromptTokens, 10), strconv.FormatInt(e.CompletionTokens, 10),
			oneLine(e.Error), e.Endpoint,
		})
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		h.log.Warn("导出 CSV 失败", "err", err)
	}
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 500 {
		s = s[:500] + "…"
	}
	return s
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedPairs(m map[string]string) []map[string]string {
	out := make([]map[string]string, 0, len(m))
	for k, v := range m {
		out = append(out, map[string]string{"id": k, "name": v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["id"] < out[j]["id"] })
	return out
}
