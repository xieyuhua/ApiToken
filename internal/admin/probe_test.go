package admin

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xieyuhua/apitoken/internal/config"
	"github.com/xieyuhua/apitoken/internal/store"
)

// probeSpy 观察探活有没有真的打到上游，并可让上游返回失败。
type probeSpy struct {
	hits  int32
	failN int32 // 非 0 = 上游返回 500
}

func (s *probeSpy) Hits() int32 { return atomic.LoadInt32(&s.hits) }

func (s *probeSpy) SetFail(v bool) {
	var n int32
	if v {
		n = 1
	}
	atomic.StoreInt32(&s.failN, n)
}

// newProbeHandler 构造带一个 mock 上游渠道的 Handler，用于验证后台探活行为。
func newProbeHandler(t *testing.T) (*Handler, *config.Config, *probeSpy) {
	t.Helper()
	spy := &probeSpy{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&spy.hits, 1)
		w.Header().Set("Content-Type", "application/json")
		if atomic.LoadInt32(&spy.failN) != 0 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"boom"}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	t.Cleanup(up.Close)

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
channels:
  - id: "c1"
    name: "探针目标"
    base_url: "` + up.URL + `/v1"
    api_key: "sk-x"
    models: ["mock-chat"]
    enabled: true
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
	return New(st, cfg, log), cfg, spy
}

// postSettings 经管理接口保存配置。
func postSettings(t *testing.T, h *Handler, body string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.updateSettings(rec, httptest.NewRequest(http.MethodPost, "/admin/settings", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("保存失败 %d: %s", rec.Code, rec.Body.String())
	}
}

// channelStat 读取渠道在快照中的状态（健康 / 冷却剩余）。
func channelStat(t *testing.T, h *Handler, id string) (health string, cooldownMS int64) {
	t.Helper()
	for _, c := range h.store.Snapshot().Channels {
		if c.ID == id {
			return c.Health, c.CooldownRemainingMS
		}
	}
	t.Fatalf("未找到渠道 %s", id)
	return "", 0
}

// TestProbeEnabledSetting 验证探活开关：未配置时默认开启；关闭后一次探测都不发。
func TestProbeEnabledSetting(t *testing.T) {
	h, cfg, spy := newProbeHandler(t)

	// 未配置时应默认开启，并取默认间隔与阈值
	if !cfg.RT().ProbeEnabled {
		t.Fatal("未配置时应默认开启探活")
	}
	if cfg.RT().ProbeInterval != config.DefaultProbeInterval {
		t.Fatalf("默认间隔错误: %v", cfg.RT().ProbeInterval)
	}
	if cfg.RT().ProbeFailThreshold != config.DefaultProbeFailThreshold {
		t.Fatalf("默认阈值错误: %d", cfg.RT().ProbeFailThreshold)
	}

	// 关闭探活
	postSettings(t, h, `{"probe_enabled":false}`)
	if cfg.RT().ProbeEnabled {
		t.Fatal("关闭未生效")
	}
	h.ProbeAllChannels(context.Background())
	if n := spy.Hits(); n != 0 {
		t.Fatalf("关闭时不该请求上游，实际 %d 次", n)
	}
	if got, _ := channelStat(t, h, "c1"); got != "" {
		t.Fatalf("关闭时不该更新健康状态，实际 %q", got)
	}

	// 重新开启：应发出探测请求并记录健康状态
	postSettings(t, h, `{"probe_enabled":true,"probe_interval_seconds":120,"probe_fail_threshold":5}`)
	if !cfg.RT().ProbeEnabled {
		t.Fatal("重新开启未生效")
	}
	if cfg.RT().ProbeInterval != 120*time.Second || cfg.RT().ProbeFailThreshold != 5 {
		t.Fatalf("间隔/阈值未生效: interval=%v threshold=%d",
			cfg.RT().ProbeInterval, cfg.RT().ProbeFailThreshold)
	}
	h.ProbeAllChannels(context.Background())
	if spy.Hits() == 0 {
		t.Fatal("开启后应请求上游")
	}
	if got, _ := channelStat(t, h, "c1"); got != "healthy" {
		t.Fatalf("探测成功后健康状态应为 healthy，实际 %q", got)
	}
}

// TestProbeFailThreshold 验证配置的「连续失败阈值」确实生效：
// 未达阈值只标记不健康，达到阈值才让渠道进入冷却。
func TestProbeFailThreshold(t *testing.T) {
	h, _, spy := newProbeHandler(t)
	postSettings(t, h, `{"probe_enabled":true,"probe_fail_threshold":2}`)
	spy.SetFail(true)

	// 第 1 次失败：标记不健康，但还没到进冷却的阈值
	h.ProbeAllChannels(context.Background())
	if health, cd := channelStat(t, h, "c1"); health != "unhealthy" {
		t.Fatalf("第 1 次失败应标记 unhealthy，实际 %q", health)
	} else if cd > 0 {
		t.Fatalf("未达阈值时不应进冷却，实际剩余 %dms", cd)
	}

	// 第 2 次失败：达到阈值，进入冷却（时长取 routing.cooldown，此处为默认值）
	h.ProbeAllChannels(context.Background())
	if health, cd := channelStat(t, h, "c1"); health != "unhealthy" {
		t.Fatalf("第 2 次失败仍应为 unhealthy，实际 %q", health)
	} else if cd <= 0 {
		t.Fatal("连续失败达到阈值后应进入冷却")
	}

	// 恢复：探测成功应清除计数与冷却
	spy.SetFail(false)
	h.ProbeAllChannels(context.Background())
	if health, cd := channelStat(t, h, "c1"); health != "healthy" || cd > 0 {
		t.Fatalf("探测成功后应恢复 healthy 且清除冷却，实际 health=%q cd=%dms", health, cd)
	}
}
