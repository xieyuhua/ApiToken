package webui

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// getStar 以给定头请求某个静态资源。
func getStatic(t *testing.T, file string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/static/"+file, nil)
	r.SetPathValue("file", file)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	StaticHandler()(w, r)
	return w
}

func TestStaticAssetHeaders(t *testing.T) {
	w := getStatic(t, "app.js", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if got := w.Header().Get("Content-Type"); got != "application/javascript; charset=utf-8" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := w.Header().Get("Cache-Control"); got != staticCacheControl {
		t.Fatalf("Cache-Control = %q，期望 %q", got, staticCacheControl)
	}
	etag := w.Header().Get("ETag")
	if etag == "" {
		t.Fatal("缺少 ETag：没有指纹就无法做条件请求，每次都会全量重传")
	}
	// 响应体随 Accept-Encoding 变化，必须声明，否则中间 CDN 会给不支持 gzip 的客户端发压缩内容
	if got := w.Header().Get("Vary"); got != "Accept-Encoding" {
		t.Fatalf("Vary = %q", got)
	}
	if w.Body.Len() == 0 {
		t.Fatal("正文为空")
	}
}

func TestStaticAssetGzip(t *testing.T) {
	plain := getStatic(t, "style.css", nil)
	zipped := getStatic(t, "style.css", map[string]string{"Accept-Encoding": "gzip"})

	if zipped.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("支持 gzip 的客户端未拿到压缩内容: %q", zipped.Header())
	}
	if zipped.Body.Len() >= plain.Body.Len() {
		t.Fatalf("压缩后反而更大：gzip %d >= 原始 %d", zipped.Body.Len(), plain.Body.Len())
	}
	// 压缩内容必须能被解开，且解出来与原文一致
	gr, err := gzip.NewReader(strings.NewReader(zipped.Body.String()))
	if err != nil {
		t.Fatalf("响应不是合法 gzip: %v", err)
	}
	defer gr.Close()
	raw, err := io.ReadAll(gr)
	if err != nil {
		t.Fatalf("解压失败: %v", err)
	}
	if string(raw) != plain.Body.String() {
		t.Fatal("解压结果与原始内容不一致")
	}
	// ETag 与实体是同一个，不能因为编码方式不同变出第二个指纹
	if zipped.Header().Get("ETag") != plain.Header().Get("ETag") {
		t.Fatal("不同编码应有相同 ETag")
	}
}

func TestConditionalRequest(t *testing.T) {
	first := getStatic(t, "app.js", nil)
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("缺少 ETag")
	}

	// 命中：只回 304，不带正文
	again := getStatic(t, "app.js", map[string]string{"If-None-Match": etag})
	if again.Code != http.StatusNotModified {
		t.Fatalf("status = %d，期望 304", again.Code)
	}
	if again.Body.Len() != 0 {
		t.Fatalf("304 不应带正文，实际 %d 字节", again.Body.Len())
	}
	if again.Header().Get("ETag") != etag {
		t.Fatal("304 也应回传 ETag")
	}

	// 不匹配的值不能误判为命中，否则客户端永远拿不到更新
	changed := getStatic(t, "app.js", map[string]string{"If-None-Match": `"deadbeef"`})
	if changed.Code != http.StatusOK {
		t.Fatalf("未命中应返回 200，实际 %d", changed.Code)
	}
	if changed.Body.Len() == 0 {
		t.Fatal("未命中时不能把正文省掉")
	}
}

func TestPageHandlers(t *testing.T) {
	for name, h := range map[string]http.HandlerFunc{
		"index":       Handler(),
		"channels":    ChannelsHandler(),
		"routes":      RoutesHandler(),
		"logs":        LogsHandler(),
		"diagnostics": DiagnosticsHandler(),
		"chat":        ChatHandler(),
		"settings":    SettingsHandler(),
	} {
		w := httptest.NewRecorder()
		h(w, httptest.NewRequest(http.MethodGet, "/", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status = %d", name, w.Code)
		}
		if got := w.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
			t.Fatalf("%s: Content-Type = %q", name, got)
		}
		// HTML 是 UI 入口，必须每次都要回源校验（升级后立刻生效）
		if got := w.Header().Get("Cache-Control"); got != htmlCacheControl {
			t.Fatalf("%s: Cache-Control = %q，期望 %q", name, got, htmlCacheControl)
		}
		if w.Header().Get("ETag") == "" {
			t.Fatalf("%s: 缺少 ETag，条件请求无法生效", name)
		}
	}
}

func TestMissingAssetIs404(t *testing.T) {
	if got := getStatic(t, "not-exists.js", nil).Code; got != http.StatusNotFound {
		t.Fatalf("不存在的资源 status = %d，期望 404", got)
	}
	// 路径穿越必须被挡住（path.Clean 归一后嵌入 FS 里查不到），不能把上层文件读出去
	if got := getStatic(t, "../../etc/passwd", nil).Code; got != http.StatusNotFound {
		t.Fatalf("路径穿越 status = %d，期望 404", got)
	}
}

func TestAssetCacheIsStable(t *testing.T) {
	// 嵌入资源内容不变，重复请求的指纹必须一致，否则每次都会判定为「已更新」
	a := getStatic(t, "app.js", nil)
	b := getStatic(t, "app.js", nil)
	if a.Header().Get("ETag") != b.Header().Get("ETag") {
		t.Fatal("同一资源的 ETag 不稳定")
	}
	if a.Body.String() != b.Body.String() {
		t.Fatal("同一资源的内容不一致")
	}
}

func TestContentTypeMapping(t *testing.T) {
	cases := []struct{ name, want string }{
		{"a.html", "text/html; charset=utf-8"},
		{"a.css", "text/css; charset=utf-8"},
		{"a.js", "application/javascript; charset=utf-8"},
		{"a.bin", "application/octet-stream"}, // 认不出来时按二进制流，别让浏览器瞎猜
	}
	for _, c := range cases {
		if got := contentType(c.name); got != c.want {
			t.Fatalf("contentType(%q) = %q，期望 %q", c.name, got, c.want)
		}
	}
}

func TestEtagMatch(t *testing.T) {
	cases := []struct {
		header, etag string
		want         bool
	}{
		{`"abc"`, `"abc"`, true},
		{`"abc", "def"`, `"def"`, true}, // 逗号分隔列表
		{`W/"abc"`, `"abc"`, false},     // 弱校验器暂不支持，如实回 200
		{`*`, `"abc"`, true},
		{`"x"`, `"abc"`, false},
		{"", `"abc"`, false},
	}
	for _, c := range cases {
		if got := etagMatch(c.header, c.etag); got != c.want {
			t.Fatalf("etagMatch(%q, %q) = %v，期望 %v", c.header, c.etag, got, c.want)
		}
	}
}
