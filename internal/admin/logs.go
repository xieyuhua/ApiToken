package admin

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/demo1/apitoken/internal/model"
	"github.com/demo1/apitoken/internal/store"
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
	}
}

// logs GET /admin/logs —— 分页 + 筛选 + 聚合统计。
// 兼容旧用法：带 limit 参数且未使用分页时，直接返回数组。
func (h *Handler) logs(w http.ResponseWriter, r *http.Request) {
	q := logQuery(r)
	page := h.store.QueryLogs(q)

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
		"attempts":   items,
		"summary":    summary,
	})
}

// clearLogs DELETE /admin/logs —— 清空调用日志。
func (h *Handler) clearLogs(w http.ResponseWriter, _ *http.Request) {
	n := h.store.ClearLogs()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "cleared": n})
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
	page := h.store.QueryLogs(q)
	format := r.URL.Query().Get("format")
	stamp := time.Now().Format("20060102-150405")

	if format == "json" {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="apitoken-logs-%s.json"`, stamp))
		writeJSON(w, http.StatusOK, page.Items)
		return
	}

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="apitoken-logs-%s.csv"`, stamp))
	cw := csv.NewWriter(w)
	defer cw.Flush()
	_ = cw.Write([]string{
		"time", "request_id", "client_ip", "model", "upstream_model", "channel",
		"stream", "status", "latency_ms", "attempts", "prompt_tokens", "completion_tokens", "error", "endpoint",
	})
	for _, e := range page.Items {
		_ = cw.Write([]string{
			e.Time.Format("2006-01-02 15:04:05.000"), e.RequestID, e.ClientIP, e.Model, e.UpstreamModel,
			e.ChannelID, strconv.FormatBool(e.Stream), strconv.Itoa(e.Status),
			strconv.FormatInt(e.LatencyMS, 10), strconv.Itoa(e.Attempts),
			strconv.FormatInt(e.PromptTokens, 10), strconv.FormatInt(e.CompletionTokens, 10),
			oneLine(e.Error), e.Endpoint,
		})
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
	sortStrings(out)
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

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
