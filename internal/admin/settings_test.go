package admin

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xieyuhua/apitoken/internal/config"
	"github.com/xieyuhua/apitoken/internal/store"
)

// newDispatchHandler 构造带真实配置文件（可写回）的 Handler。
func newDispatchHandler(t *testing.T) (*Handler, *config.Config, string) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	yaml := `server:
  addr: ":0"
  data_dir: "` + filepath.ToSlash(filepath.Join(dir, "data")) + `"
security:
  client_keys: ["sk-x"]
  admin_token: "adm"
routing:
  strategy: "failover"
  max_attempts: 3
`
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.New(cfg, log)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(st.CloseLogs)
	return New(st, cfg, log), cfg, cfgPath
}

// TestLogArchiveSetting 验证「调用日志数据源」配置项：默认值、保存生效、写回文件、数据源判定。
func TestLogArchiveSetting(t *testing.T) {
	h, cfg, cfgPath := newDispatchHandler(t)

	// 未配置时默认查内存热缓冲
	if h.useDiskArchive(httptest.NewRequest(http.MethodGet, "/admin/logs", nil)) {
		t.Fatal("未开启 log_archive 时应默认查内存热缓冲")
	}

	// 经管理接口保存并立即生效
	rec := httptest.NewRecorder()
	h.updateSettings(rec, httptest.NewRequest(http.MethodPost, "/admin/settings",
		strings.NewReader(`{"log_archive":true}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("保存失败 %d: %s", rec.Code, rec.Body.String())
	}
	if !cfg.RT().LogArchive {
		t.Fatal("保存后运行时配置未生效")
	}
	if !h.useDiskArchive(httptest.NewRequest(http.MethodGet, "/admin/logs", nil)) {
		t.Fatal("log_archive=true 时应默认查磁盘归档")
	}

	// URL 参数可临时覆盖（日志页顶部的一键切换）
	if h.useDiskArchive(httptest.NewRequest(http.MethodGet, "/admin/logs?history=0", nil)) {
		t.Fatal("history=0 应临时覆盖为查内存")
	}
	if !h.useDiskArchive(httptest.NewRequest(http.MethodGet, "/admin/logs?history=1", nil)) {
		t.Fatal("history=1 应覆盖为查磁盘归档")
	}

	// 应写回配置文件，重启后不丢
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "log_archive: true") {
		t.Fatalf("配置未写回 log_archive，文件内容:\n%s", raw)
	}
	reloaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !reloaded.RT().LogArchive {
		t.Fatal("重新加载后 log_archive 丢失，yaml 往返失败")
	}
}
