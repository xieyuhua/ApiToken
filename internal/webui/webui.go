// Package webui 提供内置的零依赖管理页面。
package webui

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
)

//go:embed index.html channels.html routes.html logs.html runtime_logs.html diagnostics.html chat.html settings.html static
var assets embed.FS

const (
	// htmlCacheControl HTML 页面的缓存策略：允许缓存，但每次都要回源校验。
	// 页面本身只有几 KB，却是 UI 的入口——升级后必须立刻拿到新版本，
	// 所以不能直接强缓存；配合 ETag 校验命中时只回 304，正文不用重传。
	htmlCacheControl = "no-cache"
	// staticCacheControl 静态资源（CSS/JS）的缓存策略。
	// 嵌入内容只在重新构建/换二进制时变化，允许短暂强缓存减少重复校验；
	// must-revalidate 保证过期后一定会拿着 old ETag 回来问一次。
	staticCacheControl = "public, max-age=300, must-revalidate"
	// minGzipSize 小于这个体积不压：这些 asset 都是纯文本，
	// 压缩收益明显，但 gzip 本身有帧头开销，零碎小文件压了反而更大。
	minGzipSize = 512
)

// Handler 返回管理控制台首页。
func Handler() http.HandlerFunc { return serve("index.html") }

// ChannelsHandler 返回渠道管理页面。
func ChannelsHandler() http.HandlerFunc { return serve("channels.html") }

// RoutesHandler 返回路由管理页面。
func RoutesHandler() http.HandlerFunc { return serve("routes.html") }

// LogsHandler 返回调用日志页面。
func LogsHandler() http.HandlerFunc { return serve("logs.html") }

// RuntimeLogsHandler 返回运行日志页面。
func RuntimeLogsHandler() http.HandlerFunc { return serve("runtime_logs.html") }

// DiagnosticsHandler 返回网关自检页面。
func DiagnosticsHandler() http.HandlerFunc { return serve("diagnostics.html") }

// ChatHandler 返回对话测试页面。
func ChatHandler() http.HandlerFunc { return serve("chat.html") }

// SettingsHandler 返回设置页面。
func SettingsHandler() http.HandlerFunc { return serve("settings.html") }

// StaticHandler 返回静态资源（样式与公共脚本）。
func StaticHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// path.Clean 会把 "../x" 归一成 "/x"，嵌入 FS 里没有就返回 404
		name := path.Clean("/" + r.PathValue("file"))
		if err := writeAsset(w, r, "static"+name, staticCacheControl); err != nil {
			http.NotFound(w, r)
			return
		}
	}
}

func serve(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := writeAsset(w, r, name, htmlCacheControl); err != nil {
			http.Error(w, "page not found", http.StatusNotFound)
			return
		}
	}
}

// assetEntry 一份嵌入资源的预处理结果。
//
// embed.FS 的内容在进程生命周期内不会变，所以「算指纹 + 压 gzip」每个资源只做一次，
// 之后每个请求就是一次内容协商加一次内存拷贝。资源本身是个位数 KB，
// 但它们是每次打开控制台都要重新拉的；没有缓存/压缩时，一个 60KB 的静态集
// 会在每次刷新都全量重传。
type assetEntry struct {
	data    []byte // 原始内容
	etag    string // 内容指纹，形如 "sha256hex"，用于条件请求
	gzipped []byte // gzip 结果；压缩后没变小则为 nil
}

// assetCache 资源路径 -> 预处理结果。
var assetCache sync.Map

// loadAsset 读取嵌入资源并缓存其预处理结果。
func loadAsset(name string) (*assetEntry, error) {
	if v, ok := assetCache.Load(name); ok {
		return v.(*assetEntry), nil
	}
	data, err := assets.ReadFile(name)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	e := &assetEntry{data: data, etag: `"` + hex.EncodeToString(sum[:]) + `"`}
	if len(data) >= minGzipSize {
		var buf bytes.Buffer
		gw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
		if err == nil {
			_, _ = gw.Write(data)
			// 只有真的变小了才留着；否则宁可多传一点也不用 gzip 的解码开销
			if gw.Close() == nil && buf.Len() < len(data) {
				e.gzipped = buf.Bytes()
			}
		}
	}
	assetCache.Store(name, e)
	return e, nil
}

// writeAsset 输出一份嵌入资源：带 ETag、支持 304 条件请求、按 Accept-Encoding 压缩。
// 资源不存在时返回错误，由调用方决定回 404 还是别的。
func writeAsset(w http.ResponseWriter, r *http.Request, name, cacheControl string) error {
	e, err := loadAsset(name)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", contentType(name))
	w.Header().Set("Cache-Control", cacheControl)
	w.Header().Set("ETag", e.etag)
	// 响应体随 Accept-Encoding 变化，必须声明，否则中间缓存会给不支持 gzip 的客户端压缩内容
	w.Header().Add("Vary", "Accept-Encoding")

	if etagMatch(r.Header.Get("If-None-Match"), e.etag) {
		w.WriteHeader(http.StatusNotModified)
		return nil
	}
	if e.gzipped != nil && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Length", strconv.Itoa(len(e.gzipped)))
		_, _ = w.Write(e.gzipped)
		return nil
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(e.data)))
	_, _ = w.Write(e.data)
	return nil
}

// etagMatch 判断 If-None-Match 是否命中当前 ETag。支持 "*" 与逗号分隔的列表。
func etagMatch(header, etag string) bool {
	if header == "" {
		return false
	}
	for _, part := range strings.Split(header, ",") {
		if trimmed := strings.TrimSpace(part); trimmed == etag || trimmed == "*" {
			return true
		}
	}
	return false
}

// contentType 按扩展名推断 MIME。先认自己一定会用到的三种文本类型
// （mime 包的结果依赖运行环境，不保证带 charset），其余交给系统，
// 认不出来的按二进制流处理，避免浏览器猜类型。
func contentType(name string) string {
	switch path.Ext(name) {
	case ".html":
		return "text/html; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".js":
		return "application/javascript; charset=utf-8"
	}
	if t := mime.TypeByExtension(path.Ext(name)); t != "" {
		return t
	}
	return "application/octet-stream"
}
