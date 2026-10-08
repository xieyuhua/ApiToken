// Package webui 提供内置的零依赖管理页面。
package webui

import (
	"embed"
	"net/http"
	"path"
	"strings"
)

//go:embed index.html logs.html diagnostics.html chat.html settings.html static
var assets embed.FS

// Handler 返回管理控制台首页。
func Handler() http.HandlerFunc { return serve("index.html") }

// LogsHandler 返回调用日志页面。
func LogsHandler() http.HandlerFunc { return serve("logs.html") }

// DiagnosticsHandler 返回网关自检页面。
func DiagnosticsHandler() http.HandlerFunc { return serve("diagnostics.html") }

// ChatHandler 返回对话测试页面。
func ChatHandler() http.HandlerFunc { return serve("chat.html") }

// SettingsHandler 返回设置页面。
func SettingsHandler() http.HandlerFunc { return serve("settings.html") }

// StaticHandler 返回静态资源（样式与公共脚本）。
func StaticHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := path.Clean("/" + r.PathValue("file"))
		data, err := assets.ReadFile("static" + name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		switch {
		case strings.HasSuffix(name, ".css"):
			w.Header().Set("Content-Type", "text/css; charset=utf-8")
		case strings.HasSuffix(name, ".js"):
			w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		default:
			w.Header().Set("Content-Type", "application/octet-stream")
		}
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(data)
	}
}

func serve(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data, err := assets.ReadFile(name)
		if err != nil {
			http.Error(w, "page not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(data)
	}
}
