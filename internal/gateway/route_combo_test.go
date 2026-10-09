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

// TestRouteModelNameFreeInput 校验「对外模型名」可以直接自由输入：
// 该字段本就是客户端自定义的名字，不能强制从模型列表里选；
// 旧的「其他…（自定义名称）」选项排在列表末尾，会被 MAX_LIST 截断后根本点不到。
func TestRouteModelNameFreeInput(t *testing.T) {
	h := newTestGateway(t, nil)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()

	// 旧的占位方案必须彻底移除
	for _, gone := range []string{"__custom__", "r_model_custom", "其他…"} {
		if strings.Contains(body, gone) {
			t.Fatalf("仍残留 %q：会因渲染上限而不可选，且不是真正的自由输入", gone)
		}
	}
	// 同一个输入框既可搜索也可自由输入
	if !strings.Contains(body, `id="r_model"`) {
		t.Fatal("缺少对外模型名输入框")
	}
	seg := body[strings.Index(body, "function mountRouteCombos()"):]
	seg = seg[:strings.Index(seg, "// ② 添加渠道")]
	if !strings.Contains(seg, "allowFree: true") {
		t.Fatal("对外模型名下拉未开启 allowFree，无法输入候选之外的名称")
	}
	if !strings.Contains(body, "function routeModelValue()") ||
		!strings.Contains(body, "Combo.value($('r_model'))") {
		t.Fatal("routeModelValue 应直接取输入框的值")
	}

	req = httptest.NewRequest(http.MethodGet, "/static/combo.js", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	js := rec.Body.String()
	for _, want := range []string{
		"opts.allowFree", "使用自定义名称", "freeHint", "free: true",
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("combo.js 缺少自由输入能力 %q", want)
		}
	}
	// 自由输入项必须置顶渲染，否则同样会被 MAX_LIST 截断
	if strings.Index(js, "const rows = free ? [free].concat(hit) : hit;") < 0 {
		t.Fatal("自由输入项应与命中项合并后置顶渲染")
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
