package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/xieyuhua/apitoken/internal/model"
	"github.com/xieyuhua/apitoken/internal/proxy"
	"github.com/xieyuhua/apitoken/internal/store"
)

// healthInfoFromProbe 把连通性探测结果转成渠道健康状态（连通性测试与渠道体检共用）。
func healthInfoFromProbe(res proxy.ProbeResult) store.HealthInfo {
	info := store.HealthInfo{CheckedAt: time.Now(), LatencyMS: res.LatencyMS}
	if res.OK {
		info.Status = "healthy"
		info.Detail = res.Reply
	} else {
		info.Status = "unhealthy"
		info.Error = res.Error
	}
	return info
}

// probeFailThreshold 连续探活失败多少次才进入冷却。探活是低频的（默认 60s 一次），
// 用连续失败阈值过滤单次抖动，避免把健康渠道误伤出候选列表。
const probeFailThreshold = 3

// ProbeAllChannels 后台自动探活：对所有启用渠道做一次最小对话请求，
// 更新健康状态；连续失败达到阈值时进入冷却，成功则立即清除冷却。
//
// 与真实请求路径的冷却区分开：探活冷却只是「提前」把挂掉的渠道移出候选，
// 真实请求失败仍会走 recordChannelStatsLocked 的冷却逻辑，两者互不干扰。
func (h *Handler) ProbeAllChannels(ctx context.Context) {
	cd := h.cfg.RT().Cooldown

	for _, ch := range h.store.ListChannels() {
		if !ch.Enabled {
			continue
		}
		res := proxy.Probe(ctx, h.cfg, h.client, ch, "", "ping")
		h.store.SetHealth(ch.ID, healthInfoFromProbe(res))

		h.probeMu.Lock()
		if h.probeFails == nil {
			h.probeFails = map[string]int{}
		}
		if res.OK {
			h.probeFails[ch.ID] = 0
			if cd > 0 {
				h.store.SetCooldown(ch.ID, time.Time{}) // 恢复：立即回到候选列表
			}
		} else {
			h.probeFails[ch.ID]++
			if h.probeFails[ch.ID] >= probeFailThreshold && cd > 0 {
				h.store.SetCooldown(ch.ID, time.Now().Add(cd))
				h.log.Warn("渠道连续探活失败，进入冷却", "channel", ch.ID, "err", res.Error)
			}
		}
		h.probeMu.Unlock()
	}
}

// probeDraft 按提交的草稿配置测试连通性（不保存到存储）。
// 用于前端「添加渠道」弹窗：用户还没保存、没有渠道 ID 也能先验证。
func (h *Handler) probeDraft(w http.ResponseWriter, r *http.Request) {
	ch, ok := h.draftChannel(w, r)
	if !ok {
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
	writeJSON(w, http.StatusOK, res)
}

// fetchDraftModels 按提交的草稿配置拉取上游 /models（不保存）。
func (h *Handler) fetchDraftModels(w http.ResponseWriter, r *http.Request) {
	ch, ok := h.draftChannel(w, r)
	if !ok {
		return
	}
	ids, err := proxy.FetchModels(r.Context(), h.client, ch)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": ids})
}

// draftChannel 解析并校验请求中的渠道草稿。
// GET 时从 query 读取（拉取模型是读取操作，避免为带配置而硬凑 POST）；
// POST 时仍从 body 读取，兼容旧调用方。
func (h *Handler) draftChannel(w http.ResponseWriter, r *http.Request) (model.Channel, bool) {
	var ch model.Channel
	if r.Method == http.MethodGet {
		ch = draftFromQuery(r)
	} else if err := decodeJSON(r, &ch); err != nil {
		writeErr(w, http.StatusBadRequest, "渠道配置解析失败: "+err.Error())
		return ch, false
	}
	ch.BaseURL = strings.TrimRight(strings.TrimSpace(ch.BaseURL), "/")
	if ch.BaseURL == "" {
		writeErr(w, http.StatusBadRequest, "请先填写 Base URL")
		return ch, false
	}
	applyChannelDefaults(&ch)
	return ch, true
}

// draftFromQuery 从 query 参数还原渠道草稿（GET /admin/channels/models 用）。
// 只覆盖拉取模型需要的鉴权相关字段。
func draftFromQuery(r *http.Request) model.Channel {
	q := r.URL.Query()
	ch := model.Channel{
		BaseURL:    q.Get("base_url"),
		APIKey:     q.Get("api_key"),
		AuthStyle:  q.Get("auth_style"),
		AuthHeader: q.Get("auth_header"),
		AuthPrefix: q.Get("auth_prefix"),
		ChatPath:   q.Get("chat_path"),
		ProxyURL:   q.Get("proxy_url"),
	}
	if n, err := strconv.Atoi(q.Get("timeout_sec")); err == nil {
		ch.TimeoutSec = n
	}
	// extra_headers 以 JSON 字符串传递：{"X-Foo":"bar"}
	if raw := strings.TrimSpace(q.Get("extra_headers")); raw != "" {
		var hdrs map[string]string
		if json.Unmarshal([]byte(raw), &hdrs) == nil {
			ch.ExtraHeaders = hdrs
		}
	}
	return ch
}
