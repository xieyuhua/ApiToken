package gateway_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRouteDialogSearchableCombos 校验路由弹窗的四个下拉都换成了可搜索组件：
// 模型候选有数百个时，原生 <select> 无法检索，必须提供关键词过滤。
func TestRouteDialogSearchableCombos(t *testing.T) {
	h := newTestGateway(t, nil)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("控制台不可访问: %d", rec.Code)
	}

	// 必须引入 combo 组件
	if !strings.Contains(body, "/static/combo.js") {
		t.Fatal("控制台未引入 combo.js")
	}
	// 四个下拉都不能再是原生 select
	for _, id := range []string{"r_model", "r_channelPick", "r_mapChannel", "r_mapPickModel"} {
		if strings.Contains(body, `<select id="`+id+`"`) {
			t.Fatalf("%s 仍是原生 select，无法搜索", id)
		}
		if !strings.Contains(body, `id="`+id+`"`) {
			t.Fatalf("缺少 %s", id)
		}
	}
	// 组件本身可访问且包含关键能力
	req = httptest.NewRequest(http.MethodGet, "/static/combo.js", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	js := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("combo.js 不可访问: %d", rec.Code)
	}
	for _, want := range []string{
		"window.Combo", "function attach(", "function value(",
		"MAX_LIST", "combo-item", "ArrowDown", "Escape",
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("combo.js 缺少 %q", want)
		}
	}
	// 搜索必须同时匹配副标题（渠道名），便于按渠道反查模型
	if !strings.Contains(js, "String(o.hint || '').toLowerCase().includes(k)") {
		t.Fatal("搜索应同时匹配 hint（渠道名）")
	}
	// 行内映射下拉也要走 combo
	if !strings.Contains(body, "r_mapSel") || !strings.Contains(body, "Combo.attach(input") {
		t.Fatal("行内映射模型下拉未接入 combo 组件")
	}
}

// TestModelPickListSearchable 校验「拉取模型」的勾选列表也可搜索：
// 上游可达 469 个模型，逐条勾选不现实；全选还必须只作用于当前过滤结果。
func TestModelPickListSearchable(t *testing.T) {
	h := newTestGateway(t, nil)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()

	if !strings.Contains(body, `id="mdlSearch"`) {
		t.Fatal("缺少模型搜索框")
	}
	for _, want := range []string{
		"function renderModelPickList()", "oninput=\"renderModelPickList()\"",
		"modelPicked", "Combo.hl(m, kw)",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("缺少 %q", want)
		}
	}
	// 全选必须按当前过滤结果过滤，而不是全量 469 个
	sel := strings.Index(body, "function toggleAllModels(")
	if sel < 0 {
		t.Fatal("缺少 toggleAllModels")
	}
	seg := body[sel : sel+700]
	if !strings.Contains(seg, "kw ? fetchedModels.filter") {
		t.Fatal("全选应只作用于当前过滤后的可见项，避免误选大量模型")
	}
}
