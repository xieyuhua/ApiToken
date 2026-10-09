package gateway_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/demo1/apitoken/internal/config"
	"github.com/demo1/apitoken/internal/model"
)

// 本文件覆盖并发场景：锁的粒度、死锁与计数自洽。
// 注意 go test -race 在部分 Windows 环境不可用（tsan 运行时缺失），
// 因此这里用"高并发 + 不变量断言"代替竞态检测。

// adminGet 以管理令牌发起 GET 请求并返回响应体。
func adminGet(t *testing.T, h http.Handler, path string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("X-Admin-Token", "adm-test")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s 异常: %d", path, rec.Code)
	}
	return rec.Body.String()
}

// adminCall 以管理令牌发起写请求，返回状态码与响应体。
func adminCall(t *testing.T, h http.Handler, method, path, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("X-Admin-Token", "adm-test")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// TestConcurrentChatStatsAndLogs 校验并发请求下的统计与日志计数完全自洽：
// 每个成功请求各贡献 1 次请求级统计 + 1 次渠道级统计 + 1 条访问日志，
// 多 goroutine 并发时不允许丢计数或重复计数。
func TestConcurrentChatStatsAndLogs(t *testing.T) {
	mock := newMock(false)
	defer mock.srv.Close()
	h := newTestGateway(t, []model.Channel{
		{ID: "c1", Name: "并发渠道", BaseURL: mock.srv.URL + "/v1", APIKey: "k",
			Models: []string{"mock-chat"}, Priority: 1, Weight: 1, Enabled: true},
	})

	const total, workers = 60, 12
	codes := make([]int, total)
	var wg sync.WaitGroup
	for i := 0; i < total; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = post(t, h, "/v1/chat/completions", `{"model":"mock-chat","messages":[]}`).Code
		}(i)
	}
	wg.Wait()

	for i, c := range codes {
		if c != http.StatusOK {
			t.Fatalf("第 %d 个并发请求失败: %d", i, c)
		}
	}

	var snap struct {
		TotalRequests int64 `json:"total_requests"`
		TotalSuccess  int64 `json:"total_success"`
		TotalFailed   int64 `json:"total_failed"`
		Channels      []struct {
			ID    string `json:"id"`
			Stats struct {
				Requests int64 `json:"requests"`
				Success  int64 `json:"success"`
				Failed   int64 `json:"failed"`
			} `json:"stats"`
		} `json:"channels"`
	}
	if err := json.Unmarshal([]byte(adminGet(t, h, "/admin/stats")), &snap); err != nil {
		t.Fatalf("解析统计失败: %v", err)
	}
	if snap.TotalRequests != total || snap.TotalSuccess != total || snap.TotalFailed != 0 {
		t.Fatalf("请求级统计不符：total=%d success=%d failed=%d，期望各 %d",
			snap.TotalRequests, snap.TotalSuccess, snap.TotalFailed, total)
	}
	var reqs, succ int64
	for _, c := range snap.Channels {
		reqs += c.Stats.Requests
		succ += c.Stats.Success
	}
	if reqs != total || succ != total {
		t.Fatalf("渠道级统计不符：requests=%d success=%d，期望各 %d", reqs, succ, total)
	}

	// 每请求 1 条日志；keep_logs=100 > total，因此一条都不能丢
	var page struct {
		Total int              `json:"total"`
		Kept  int              `json:"kept"`
		Items []model.LogEntry `json:"items"`
	}
	if err := json.Unmarshal([]byte(adminGet(t, h, "/admin/logs?page=1&page_size=100")), &page); err != nil {
		t.Fatalf("解析日志失败: %v", err)
	}
	if page.Total != total || len(page.Items) != total {
		t.Fatalf("日志条数不符：total=%d items=%d，期望各 %d", page.Total, len(page.Items), total)
	}
	// 日志不应有重复 request_id
	seen := map[string]bool{}
	for _, e := range page.Items {
		if seen[e.RequestID] {
			t.Fatalf("日志出现重复 request_id: %s", e.RequestID)
		}
		seen[e.RequestID] = true
	}
}

// TestConcurrentMixedTraffic 数据面与管理面同时并发，验证不会死锁，
// 且诊断这类重读接口不被转发路径饿死。
func TestConcurrentMixedTraffic(t *testing.T) {
	mock := newMock(false)
	defer mock.srv.Close()
	h := newTestGateway(t, []model.Channel{
		{ID: "c1", Name: "并发渠道", BaseURL: mock.srv.URL + "/v1", APIKey: "k",
			Models: []string{"mock-chat"}, Priority: 1, Weight: 1, Enabled: true},
	})

	paths := []string{
		"/admin/stats", "/admin/logs?page=1", "/admin/channels", "/admin/routes",
		"/v1/models", "/healthz", "/readyz", "/admin/diagnostics",
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		for w := 0; w < 16; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				for i := 0; i < 10; i++ {
					if w%2 == 0 {
						post(t, h, "/v1/chat/completions", `{"model":"mock-chat","messages":[]}`)
						continue
					}
					req := httptest.NewRequest(http.MethodGet, paths[(w+i)%len(paths)], nil)
					req.Header.Set("X-Admin-Token", "adm-test")
					rec := httptest.NewRecorder()
					h.ServeHTTP(rec, req)
				}
			}(w)
		}
		wg.Wait()
	}()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("并发流量下发生死锁（30s 未完成）")
	}
}

// TestConcurrentChannelUpsertKeepsDataFileIntact 校验并发写渠道后数据文件仍然完整可解析。
//
// 数据文件走 tmp + rename 落盘，若多个写请求同时写同一个 .tmp 文件并各自 rename，
// 就会出现内容交错损坏。这里并发写入 8 个不同渠道后校验文件。
func TestConcurrentChannelUpsertKeepsDataFileIntact(t *testing.T) {
	mock := newMock(false)
	defer mock.srv.Close()
	h, dir := newTestGatewayWithDir(t, nil)

	const n = 8
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := fmt.Sprintf(
				`{"id":"ch-%d","name":"渠道%d","base_url":"%s/v1","api_key":"k%d","models":["mock-chat"],"enabled":true}`,
				i, i, mock.srv.URL, i)
			codes[i], _ = adminCall(t, h, http.MethodPost, "/admin/channels", body)
		}(i)
	}
	wg.Wait()

	for i, c := range codes {
		if c != http.StatusCreated {
			t.Fatalf("第 %d 个渠道创建失败: %d", i, c)
		}
	}
	// 所有渠道都应出现在列表中
	list := adminGet(t, h, "/admin/channels")
	for i := 0; i < n; i++ {
		if !strings.Contains(list, fmt.Sprintf(`"id":"ch-%d"`, i)) {
			t.Fatalf("渠道 ch-%d 丢失", i)
		}
	}

	// 校验落盘结果：数据文件必须完整可解析，且含全部并发创建的渠道
	path := filepath.Join(dir, "gateway.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取数据文件失败: %v", err)
	}
	var p struct {
		Channels []model.Channel `json:"channels"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("数据文件损坏（JSON 解析失败）: %v\n内容: %s", err, raw)
	}
	if len(p.Channels) != n {
		t.Fatalf("数据文件应有 %d 个渠道，实际 %d", n, len(p.Channels))
	}
}

// TestChatWithFileLoggingEndToEnd 开启日志落盘时的端到端行为：
// 落盘是异步的，但请求仍成功、日志最终可见、详情接口可用、分段文件已生成。
func TestChatWithFileLoggingEndToEnd(t *testing.T) {
	mock := newMock(false)
	defer mock.srv.Close()
	h, dir := newTestGatewayWithLogging(t, []model.Channel{
		{ID: "c1", Name: "落盘渠道", BaseURL: mock.srv.URL + "/v1", APIKey: "k",
			Models: []string{"mock-chat"}, Priority: 1, Weight: 1, Enabled: true},
	}, config.LoggingConfig{
		Level: "error", KeepLogs: 100, RecordPayload: true, PayloadLimit: 2000,
		FileEnabled: true, FileMaxMB: 1, FileKeep: 3,
	})

	rec := post(t, h, "/v1/chat/completions", `{"model":"mock-chat","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("请求失败: %d %s", rec.Code, rec.Body.String())
	}

	// 日志异步写入内存热数据，轮询等待其出现
	var page struct {
		Total int              `json:"total"`
		Items []model.LogEntry `json:"items"`
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := json.Unmarshal([]byte(adminGet(t, h, "/admin/logs?page=1&page_size=10")), &page); err != nil {
			t.Fatalf("解析日志失败: %v", err)
		}
		if page.Total >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("异步落盘模式下日志始终未出现")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// 详情接口：按 request_id 取回该次请求
	req := httptest.NewRequest(http.MethodGet, "/admin/logs/"+page.Items[0].RequestID, nil)
	req.Header.Set("X-Admin-Token", "adm-test")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("日志详情异常: %d %s", rec.Code, rec.Body.String())
	}

	// 分段文件应已生成且非空
	deadline = time.Now().Add(5 * time.Second)
	for {
		files, _ := filepath.Glob(filepath.Join(dir, "logs", "access-*.jsonl"))
		for _, f := range files {
			if fi, err := os.Stat(f); err == nil && fi.Size() > 0 {
				return // 已落盘，用例通过
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待日志分段文件超时（dir=%s）", dir)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
