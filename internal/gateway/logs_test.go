package gateway_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/demo1/apitoken/internal/model"
)

// TestLogQueryFilterPagingAndExport 覆盖分页、筛选、聚合、详情、导出与清空。
func TestLogQueryFilterPagingAndExport(t *testing.T) {
	mock := newMock(false)
	defer mock.srv.Close()

	chans := []model.Channel{
		{ID: "ok1", Name: "正常", BaseURL: mock.srv.URL + "/v1", APIKey: "k",
			Models: []string{"mock-chat"}, Priority: 1, Weight: 1, Enabled: true},
	}
	h := newTestGateway(t, chans)

	// 产生：2 条成功（1 普通 + 1 流式）+ 1 条 404（模型不存在）
	post(t, h, "/v1/chat/completions", `{"model":"mock-chat","messages":[{"role":"user","content":"hi"}]}`)
	post(t, h, "/v1/chat/completions", `{"model":"nope","messages":[]}`)
	post(t, h, "/v1/chat/completions", `{"model":"mock-chat","messages":[],"stream":true}`)

	call := func(method, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("X-Admin-Token", "adm-test")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	type pageBody struct {
		Items []model.LogEntry `json:"items"`
		Total int              `json:"total"`
		Page  int              `json:"page"`
		Pages int              `json:"pages"`
		Kept  int              `json:"kept"`
		Stats model.LogSummary `json:"stats"`
	}
	readPage := func(path string) pageBody {
		rec := call(http.MethodGet, path)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s 异常: %d %s", path, rec.Code, rec.Body.String())
		}
		var p pageBody
		if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
			t.Fatalf("解析 %s 失败: %v", path, err)
		}
		return p
	}

	// 分页 + 聚合统计
	p := readPage("/admin/logs?page=1&page_size=2")
	if p.Total != 3 || len(p.Items) != 2 || p.Pages != 2 || p.Kept != 3 {
		t.Fatalf("分页结果异常: total=%d items=%d pages=%d kept=%d", p.Total, len(p.Items), p.Pages, p.Kept)
	}
	if p.Stats.Success != 2 || p.Stats.Failed != 1 {
		t.Fatalf("聚合统计异常: %+v", p.Stats)
	}
	if p.Stats.PromptTokens != 18 { // 11 + 7
		t.Fatalf("token 统计异常: %+v", p.Stats)
	}
	if len(p.Stats.ByModel) == 0 || len(p.Stats.ByChannel) == 0 || len(p.Stats.ByStatus) == 0 {
		t.Fatalf("缺少分组统计: %+v", p.Stats)
	}

	// 过滤：仅失败
	if p = readPage("/admin/logs?status=error"); p.Total != 1 || p.Items[0].Status != http.StatusNotFound {
		t.Fatalf("按失败过滤异常: %+v", p.Items)
	}
	// 过滤：仅流式
	if p = readPage("/admin/logs?stream=1"); p.Total != 1 || !p.Items[0].Stream {
		t.Fatalf("按流式过滤异常: %+v", p.Items)
	}
	// 过滤：模型 + 关键字
	if p = readPage("/admin/logs?model=mock-chat&keyword=mock-chat&status=success"); p.Total != 2 {
		t.Fatalf("关键字过滤异常: total=%d", p.Total)
	}
	// 过滤：渠道
	if p = readPage("/admin/logs?channel=ok1"); p.Total != 2 {
		t.Fatalf("渠道过滤异常: total=%d", p.Total)
	}

	// 兼容旧用法：?limit= 返回数组
	rec := call(http.MethodGet, "/admin/logs?limit=1")
	if !strings.HasPrefix(strings.TrimSpace(rec.Body.String()), "[") {
		t.Fatalf("limit 模式应返回数组: %s", rec.Body.String())
	}

	// 详情：按 request_id 查看全部尝试
	rid := p.Items[0].RequestID
	rec = call(http.MethodGet, "/admin/logs/"+rid)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), rid) {
		t.Fatalf("日志详情异常: %d %s", rec.Code, rec.Body.String())
	}
	if rec = call(http.MethodGet, "/admin/logs/chatcmpl-not-exist"); rec.Code != http.StatusNotFound {
		t.Fatalf("未知 request_id 应返回 404，实际 %d", rec.Code)
	}

	// 导出 CSV / JSON
	rec = call(http.MethodGet, "/admin/logs/export?format=csv")
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Fatalf("导出 Content-Type 异常: %s", ct)
	}
	if !strings.Contains(rec.Body.String(), "request_id") || !strings.Contains(rec.Body.String(), "mock-chat") {
		t.Fatalf("CSV 内容异常: %s", rec.Body.String())
	}
	rec = call(http.MethodGet, "/admin/logs/export?format=json")
	if !strings.HasPrefix(strings.TrimSpace(rec.Body.String()), "[") {
		t.Fatalf("JSON 导出异常: %s", rec.Body.String())
	}

	// 筛选项
	rec = call(http.MethodGet, "/admin/logs/facets")
	if !strings.Contains(rec.Body.String(), "mock-chat") {
		t.Fatalf("facets 异常: %s", rec.Body.String())
	}

	// 清空
	if rec = call(http.MethodDelete, "/admin/logs"); rec.Code != http.StatusOK {
		t.Fatalf("清空日志失败: %s", rec.Body.String())
	}
	if p = readPage("/admin/logs"); p.Total != 0 {
		t.Fatalf("清空后仍有 %d 条", p.Total)
	}
}

// TestFailoverTraceInLogs 验证故障转移会在日志里留下每次尝试的记录。
func TestFailoverTraceInLogs(t *testing.T) {
	bad := newMock(true)
	good := newMock(false)
	defer bad.srv.Close()
	defer good.srv.Close()

	chans := []model.Channel{
		{ID: "bad", Name: "限流", BaseURL: bad.srv.URL + "/v1", APIKey: "k",
			Models: []string{"mock-chat"}, Priority: 1, Weight: 1, Enabled: true},
		{ID: "good", Name: "正常", BaseURL: good.srv.URL + "/v1", APIKey: "k",
			Models: []string{"mock-chat"}, Priority: 2, Weight: 1, Enabled: true},
	}
	h := newTestGateway(t, chans)

	rec := post(t, h, "/v1/chat/completions", `{"model":"mock-chat","messages":[]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("期望故障转移成功，实际 %d: %s", rec.Code, rec.Body.String())
	}

	req := httptest.NewRequest(http.MethodGet, "/admin/logs?page=1&page_size=50", nil)
	req.Header.Set("X-Admin-Token", "adm-test")
	recLog := httptest.NewRecorder()
	h.ServeHTTP(recLog, req)

	var page struct {
		Items []model.LogEntry `json:"items"`
	}
	if err := json.Unmarshal(recLog.Body.Bytes(), &page); err != nil {
		t.Fatalf("解析日志失败: %v", err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("应记录 2 次尝试，实际 %d: %s", len(page.Items), recLog.Body.String())
	}
	if page.Items[0].Status != http.StatusOK || page.Items[0].ChannelID != "good" || page.Items[0].Attempts != 2 {
		t.Fatalf("最新一条应是 good 渠道的成功记录且尝试 2 次: %+v", page.Items[0])
	}
	if page.Items[1].Status != http.StatusTooManyRequests || page.Items[1].ChannelID != "bad" {
		t.Fatalf("应保留 bad 渠道的 429 失败记录: %+v", page.Items[1])
	}
	if page.Items[0].RequestID != page.Items[1].RequestID {
		t.Fatal("同一次请求的多次尝试应共享 request_id")
	}
	if !strings.Contains(page.Items[0].Endpoint, "/v1/chat/completions") {
		t.Fatalf("应记录上游端点: %+v", page.Items[0])
	}
}

// TestLogPagesRender 校验控制台、日志页面与静态资源可访问。
func TestLogPagesRender(t *testing.T) {
	h := newTestGateway(t, nil)
	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	for _, tc := range []struct{ path, must string }{
		{"/", "/static/app.js"},
		{"/logs", "调用日志"},
	} {
		rec := get(tc.path)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), tc.must) {
			t.Fatalf("页面 %s 异常: %d", tc.path, rec.Code)
		}
	}
	for _, p := range []string{"/static/style.css", "/static/app.js"} {
		rec := get(p)
		if rec.Code != http.StatusOK || rec.Body.Len() == 0 {
			t.Fatalf("静态资源 %s 异常: %d", p, rec.Code)
		}
	}
}

// TestRequestSnippet 校验开启 record_payload 后日志含请求/响应摘要。
func TestRequestSnippet(t *testing.T) {
	mock := newMock(false)
	defer mock.srv.Close()
	h := newTestGateway(t, []model.Channel{
		{ID: "c1", Name: "n", BaseURL: mock.srv.URL + "/v1", APIKey: "k",
			Models: []string{"mock-chat"}, Priority: 1, Weight: 1, Enabled: true},
	})
	rec := post(t, h, "/v1/chat/completions", `{"model":"mock-chat","messages":[{"role":"user","content":"独特内容-ABC"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("请求失败: %d", rec.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/admin/logs?keyword=ABC", nil)
	req.Header.Set("X-Admin-Token", "adm-test")
	recLog := httptest.NewRecorder()
	h.ServeHTTP(recLog, req)
	if !strings.Contains(recLog.Body.String(), "独特内容-ABC") {
		t.Fatalf("日志应记录请求摘要: %s", recLog.Body.String())
	}
	if !strings.Contains(recLog.Body.String(), "pong") {
		t.Fatalf("日志应记录响应摘要: %s", recLog.Body.String())
	}
}

// TestAuthStyles 验证三种鉴权方式都能正确注入 Key。
func TestAuthStyles(t *testing.T) {
	mock := newMock(false)
	defer mock.srv.Close()
	base := mock.srv.URL + "/v1"

	cases := []struct {
		name   string
		ch     model.Channel
		expect string // 期望的 Authorization 头
	}{
		{"bearer 默认", model.Channel{ID: "a", Name: "a", BaseURL: base, APIKey: "k1", Models: []string{"mock-chat"}, Priority: 1, Weight: 1, Enabled: true}, "Bearer k1"},
		{"header 自定义头", model.Channel{ID: "b", Name: "b", BaseURL: base, APIKey: "k2", AuthStyle: "header", AuthHeader: "X-API-Key", Models: []string{"mock-chat"}, Priority: 1, Weight: 1, Enabled: true}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestGateway(t, []model.Channel{tc.ch})
			if rec := post(t, h, "/v1/chat/completions", `{"model":"mock-chat","messages":[]}`); rec.Code != http.StatusOK {
				t.Fatalf("请求失败: %d %s", rec.Code, rec.Body.String())
			}
			if tc.expect == "" {
				return
			}
			if got, _ := mock.lastAuth.Load().(string); got != tc.expect {
				t.Fatalf("鉴权头错误: got=%q want=%q", got, tc.expect)
			}
		})
	}

	// query 方式：?key=xxx
	mock2 := newMock(false)
	defer mock2.srv.Close()
	ch := model.Channel{ID: "q", Name: "q", BaseURL: mock2.srv.URL + "/v1", APIKey: "k3",
		AuthStyle: "query", Models: []string{"mock-chat"}, Priority: 1, Weight: 1, Enabled: true}
	h := newTestGateway(t, []model.Channel{ch})
	if rec := post(t, h, "/v1/chat/completions", `{"model":"mock-chat","messages":[]}`); rec.Code != http.StatusOK {
		t.Fatalf("query 鉴权请求失败: %d %s", rec.Code, rec.Body.String())
	}
	if q, _ := mock2.lastQuery.Load().(string); q != "k3" {
		t.Fatalf("query 鉴权未注入 key: %q", q)
	}
}
