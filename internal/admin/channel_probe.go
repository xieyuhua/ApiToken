package admin

import (
	"net/http"
	"strings"

	"github.com/demo1/apitoken/internal/model"
	"github.com/demo1/apitoken/internal/proxy"
)

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
	_ = decodeJSON(r, &body)
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

// draftChannel 解析并校验请求体中的渠道草稿。
func (h *Handler) draftChannel(w http.ResponseWriter, r *http.Request) (model.Channel, bool) {
	var ch model.Channel
	if err := decodeJSON(r, &ch); err != nil {
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
