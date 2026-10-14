// Package admin 提供网关管理 API（渠道/路由增删改查、连通性测试、统计与日志）。
package admin

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/xieyuhua/apitoken/internal/config"
	"github.com/xieyuhua/apitoken/internal/model"
	"github.com/xieyuhua/apitoken/internal/proxy"
	"github.com/xieyuhua/apitoken/internal/store"
)

// Handler 管理接口处理器。
type Handler struct {
	store  *store.Store
	cfg    *config.Config
	log    *slog.Logger
	client *proxy.Client

	// 后台自动探活：每个渠道的连续失败计数。
	// 连续失败达到阈值才进冷却，避免单次网络抖动把健康渠道误伤出候选列表。
	probeMu    sync.Mutex
	probeFails map[string]int
}

// New 构造管理处理器。
func New(st *store.Store, cfg *config.Config, log *slog.Logger) *Handler {
	return &Handler{store: st, cfg: cfg, log: log, client: proxy.NewClient(cfg)}
}

// Register 注册路由。
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/api-info", h.apiInfo)
	mux.HandleFunc("GET /admin/settings", h.getSettings)
	mux.HandleFunc("POST /admin/settings", h.updateSettings)

	mux.HandleFunc("GET /admin/channels", h.listChannels)
	mux.HandleFunc("POST /admin/channels", h.createChannel)
	mux.HandleFunc("GET /admin/channels/{id}", h.getChannel)
	mux.HandleFunc("PUT /admin/channels/{id}", h.updateChannel)
	mux.HandleFunc("PATCH /admin/channels/{id}", h.patchChannel)
	mux.HandleFunc("DELETE /admin/channels/{id}", h.deleteChannel)
	mux.HandleFunc("POST /admin/channels/{id}/toggle", h.toggleChannel)
	mux.HandleFunc("POST /admin/channels/{id}/test", h.testChannel)
	mux.HandleFunc("GET /admin/channels/{id}/models", h.fetchModels)
	mux.HandleFunc("POST /admin/channels/{id}/models", h.fetchModels)
	// 草稿探测：新建渠道未保存时也能测试 / 拉取模型
	mux.HandleFunc("POST /admin/channels/test", h.probeDraft)
	// 拉取模型是读取操作，用 GET；POST 保留兼容旧调用方
	mux.HandleFunc("GET /admin/channels/models", h.fetchDraftModels)
	mux.HandleFunc("POST /admin/channels/models", h.fetchDraftModels)

	mux.HandleFunc("GET /admin/routes", h.listRoutes)
	mux.HandleFunc("POST /admin/routes", h.upsertRoute)
	mux.HandleFunc("DELETE /admin/routes/{model}", h.deleteRoute)

	mux.HandleFunc("GET /admin/stats", h.stats)
	mux.HandleFunc("GET /admin/stats/timeseries", h.statsTimeseries)

	// 网关自检 / 诊断
	mux.HandleFunc("GET /admin/diagnostics", h.diagnosticsOverview)
	mux.HandleFunc("POST /admin/diagnostics/run", h.diagnosticsRun)

	// 调用日志
	mux.HandleFunc("GET /admin/logs", h.logs)
	mux.HandleFunc("GET /admin/logs/facets", h.logFacets)
	mux.HandleFunc("GET /admin/logs/export", h.exportLogs)
	mux.HandleFunc("GET /admin/logs/{request_id}", h.logDetail)
	mux.HandleFunc("DELETE /admin/logs", h.clearLogs)

	// 运行日志（进程自身 slog 输出）
	mux.HandleFunc("GET /admin/runtime-logs", h.runtimeLogs)
}

func (h *Handler) apiInfo(w http.ResponseWriter, _ *http.Request) {
	rt := h.cfg.RT()
	writeJSON(w, http.StatusOK, map[string]any{
		"version":     model.Version,
		"routing":     h.cfg.Routing,
		"auth":        map[string]any{"client_keys_required": len(rt.ClientKeys) > 0, "admin_token_required": rt.AdminToken != ""},
		"data_file":   h.cfg.DataFile(),
		"uptime":      h.store.UptimeString(),
		"client_keys": maskedKeys(rt.ClientKeys),
		"defaults": map[string]any{
			"chat_path":   proxy.DefaultChatPath,
			"auth_style":  "bearer",
			"auth_header": "Authorization",
		},
	})
}

func maskedKeys(keys []string) []string {
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, model.MaskSecret(k))
	}
	return out
}

func (h *Handler) listChannels(w http.ResponseWriter, r *http.Request) {
	reveal := r.URL.Query().Get("reveal") == "1"
	list := h.store.ListChannels()
	out := make([]model.Channel, 0, len(list))
	for _, ch := range list {
		if reveal {
			out = append(out, ch)
		} else {
			out = append(out, ch.Masked())
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) getChannel(w http.ResponseWriter, r *http.Request) {
	ch, ok := h.store.GetChannel(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "渠道不存在")
		return
	}
	writeJSON(w, http.StatusOK, ch)
}

func (h *Handler) createChannel(w http.ResponseWriter, r *http.Request) {
	ch, err := decodeChannel(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	applyChannelDefaults(&ch)
	if ch.BaseURL == "" {
		writeErr(w, http.StatusBadRequest, "base_url 不能为空")
		return
	}
	if ch.Name == "" {
		ch.Name = ch.ID
	}
	if ch.ID == "" {
		ch.ID = model.NewChannelID(ch.Name)
	}
	saved, err := h.store.Upsert(ch)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, saved.Masked())
}

// channelPatch 用于 PATCH 局部更新。
type channelPatch struct {
	Name         *string            `json:"name"`
	BaseURL      *string            `json:"base_url"`
	APIKey       *string            `json:"api_key"`
	Models       *[]string          `json:"models"`
	Alias        *map[string]string `json:"alias"`
	Weight       *int               `json:"weight"`
	Priority     *int               `json:"priority"`
	Enabled      *bool              `json:"enabled"`
	TimeoutSec   *int               `json:"timeout_sec"`
	ChatPath     *string            `json:"chat_path"`
	AuthStyle    *string            `json:"auth_style"`
	AuthHeader   *string            `json:"auth_header"`
	AuthPrefix   *string            `json:"auth_prefix"`
	ExtraHeaders *map[string]string `json:"extra_headers"`
	ExtraParams  *map[string]any    `json:"extra_params"`
	Note         *string            `json:"note"`
}

func (h *Handler) updateChannel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	old, ok := h.store.GetChannel(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "渠道不存在")
		return
	}
	ch, err := decodeChannel(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ch.ID = id
	ch.CreatedAt = old.CreatedAt
	if ch.APIKey == "" {
		ch.APIKey = old.APIKey
	}
	applyChannelDefaults(&ch)
	saved, err := h.store.Upsert(ch)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, saved.Masked())
}

func (h *Handler) patchChannel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ch, ok := h.store.GetChannel(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "渠道不存在")
		return
	}
	var p channelPatch
	if err := decodeJSON(r, &p); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if p.Name != nil {
		ch.Name = *p.Name
	}
	if p.BaseURL != nil {
		ch.BaseURL = *p.BaseURL
	}
	if p.APIKey != nil && *p.APIKey != "" {
		ch.APIKey = *p.APIKey
	}
	if p.Models != nil {
		ch.Models = *p.Models
	}
	if p.Alias != nil {
		ch.Alias = *p.Alias
	}
	if p.Weight != nil {
		ch.Weight = *p.Weight
	}
	if p.Priority != nil {
		ch.Priority = *p.Priority
	}
	if p.Enabled != nil {
		ch.Enabled = *p.Enabled
	}
	if p.TimeoutSec != nil {
		ch.TimeoutSec = *p.TimeoutSec
	}
	if p.ChatPath != nil {
		ch.ChatPath = *p.ChatPath
	}
	if p.AuthStyle != nil {
		ch.AuthStyle = *p.AuthStyle
	}
	if p.AuthHeader != nil {
		ch.AuthHeader = *p.AuthHeader
	}
	if p.AuthPrefix != nil {
		ch.AuthPrefix = *p.AuthPrefix
	}
	if p.ExtraHeaders != nil {
		ch.ExtraHeaders = *p.ExtraHeaders
	}
	if p.ExtraParams != nil {
		ch.ExtraParams = *p.ExtraParams
	}
	if p.Note != nil {
		ch.Note = *p.Note
	}
	applyChannelDefaults(&ch)
	saved, err := h.store.Upsert(ch)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, saved.Masked())
}

func (h *Handler) deleteChannel(w http.ResponseWriter, r *http.Request) {
	if err := h.store.DeleteChannel(r.PathValue("id")); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) toggleChannel(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	id := r.PathValue("id")
	current, ok := h.store.GetChannel(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "渠道不存在")
		return
	}
	enabled := !current.Enabled
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	saved, err := h.store.SetEnabled(id, enabled)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, saved.Masked())
}

func (h *Handler) testChannel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ch, ok := h.store.GetChannel(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "渠道不存在")
		return
	}
	var body struct {
		Model  string `json:"model"`
		Prompt string `json:"prompt"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	res := proxy.Probe(r.Context(), h.cfg, h.client, ch, body.Model, body.Prompt)
	h.store.SetHealth(id, healthInfoFromProbe(res))
	writeJSON(w, http.StatusOK, res)
}

func (h *Handler) fetchModels(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Apply bool `json:"apply"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	// GET 时 apply 走 query（?apply=1）
	if r.Method == http.MethodGet && !body.Apply {
		body.Apply = r.URL.Query().Get("apply") == "1" || strings.EqualFold(r.URL.Query().Get("apply"), "true")
	}
	ch, ok := h.store.GetChannel(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "渠道不存在")
		return
	}
	ids, err := proxy.FetchModels(r.Context(), h.client, ch)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if body.Apply {
		ch.Models = ids
		if _, err := h.store.Upsert(ch); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": ids, "applied": body.Apply})
}

func (h *Handler) listRoutes(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, h.store.ListRoutes())
}

func (h *Handler) upsertRoute(w http.ResponseWriter, r *http.Request) {
	var rt model.Route
	if err := decodeJSON(r, &rt); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	rt.Model = strings.TrimSpace(rt.Model)
	if rt.Model == "" {
		writeErr(w, http.StatusBadRequest, "model 不能为空")
		return
	}
	if rt.Enabled == nil {
		t := true
		rt.Enabled = &t
	}
	saved, err := h.store.UpsertRoute(rt)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, saved)
}

func (h *Handler) deleteRoute(w http.ResponseWriter, r *http.Request) {
	if err := h.store.DeleteRoute(r.PathValue("model")); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) stats(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, h.store.Snapshot())
}

// statsTimeseries 返回按小时聚合的用量趋势，供控制台趋势图使用。
// GET /admin/stats/timeseries?hours=24（hours 可选，默认 24，上限为数据保留窗口 7 天）
func (h *Handler) statsTimeseries(w http.ResponseWriter, r *http.Request) {
	hours := 24
	if v := strings.TrimSpace(r.URL.Query().Get("hours")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			hours = n
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"hours":  hours,
		"series": h.store.HourlySeries(hours),
	})
}

func decodeChannel(r *http.Request) (model.Channel, error) {
	var ch model.Channel
	if err := decodeJSON(r, &ch); err != nil {
		return ch, err
	}
	return ch, nil
}

func decodeJSON(r *http.Request, v any) error {
	data, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, v); err != nil {
		return err
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"ok": false, "error": msg})
}
