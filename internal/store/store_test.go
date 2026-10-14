package store

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/xieyuhua/apitoken/internal/config"
	"github.com/xieyuhua/apitoken/internal/model"
)

// newCoolStore 构造一个带 n 个同模型渠道的 Store，用于冷却逻辑测试。
// cooldown 为空表示不在配置里写该项（走默认值）。
func newCoolStore(t *testing.T, cooldown string, n int) *Store {
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
	if cooldown != "" {
		yaml += "  cooldown: \"" + cooldown + "\"\n"
	}
	yaml += "channels:\n"
	for i := 0; i < n; i++ {
		id := string(rune('a' + i))
		yaml += `  - id: "` + id + `"
    name: "` + id + `"
    base_url: "https://example.com/v1"
    api_key: "sk-x"
    models: ["m1"]
    enabled: true
`
	}
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	st, err := New(cfg, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(st.CloseLogs)
	return st
}

// 冷却期内失败的渠道不再进入候选；成功后立即解除冷却。
func TestCooldownSkipsFailedChannel(t *testing.T) {
	st := newCoolStore(t, "60s", 2)

	got := st.Candidates("m1")
	if len(got) != 2 || got[0].ID != "a" || got[1].ID != "b" {
		t.Fatalf("初始候选 = %v, want [a b]", candidateIDs(got))
	}

	// a 失败 -> 进入冷却，候选只剩 b（顺序保持）
	st.RecordChannelAttempt("a", false, time.Millisecond, 0, 0, "boom")
	got = st.Candidates("m1")
	if len(got) != 1 || got[0].ID != "b" {
		t.Fatalf("a 冷却后候选 = %v, want [b]", candidateIDs(got))
	}

	// b 成功 -> 解除 b 的冷却（没有也无妨），a 仍在冷却
	st.RecordFinish("b", "m1", true, time.Millisecond, time.Millisecond, 0, 0, "")
	if rem := st.cooldownRemaining("a"); rem <= 0 {
		t.Fatalf("a 应仍在冷却中, remaining=%v", rem)
	}
	got = st.Candidates("m1")
	if len(got) != 1 || got[0].ID != "b" {
		t.Fatalf("b 成功后候选 = %v, want [b]", candidateIDs(got))
	}

	// a 成功 -> 冷却解除，候选恢复
	st.RecordFinish("a", "m1", true, time.Millisecond, time.Millisecond, 0, 0, "")
	got = st.Candidates("m1")
	if len(got) != 2 {
		t.Fatalf("a 恢复后候选 = %v, want [a b]", candidateIDs(got))
	}
}

// 冷却为 0 时不过滤（旧行为：每轮都从第一个渠道开始）。
func TestCooldownDisabled(t *testing.T) {
	st := newCoolStore(t, "0s", 2)
	st.RecordChannelAttempt("a", false, time.Millisecond, 0, 0, "boom")
	got := st.Candidates("m1")
	if len(got) != 2 || got[0].ID != "a" {
		t.Fatalf("冷却关闭时候选 = %v, want [a b]", candidateIDs(got))
	}
}

// 未配置时使用默认冷却（失败渠道被跳过）。
func TestCooldownDefaultValue(t *testing.T) {
	st := newCoolStore(t, "", 2)
	if got := st.Config().RT().Cooldown; got != config.DefaultCooldown {
		t.Fatalf("默认冷却 = %v, want %v", got, config.DefaultCooldown)
	}
	st.RecordChannelAttempt("a", false, time.Millisecond, 0, 0, "boom")
	if got := st.Candidates("m1"); len(got) != 1 || got[0].ID != "b" {
		t.Fatalf("默认冷却下候选 = %v, want [b]", candidateIDs(got))
	}
}

// 所有渠道都在冷却时退化为全量候选，不会把请求判死。
func TestCooldownAllCoolingFallsBack(t *testing.T) {
	st := newCoolStore(t, "60s", 2)
	st.RecordChannelAttempt("a", false, time.Millisecond, 0, 0, "boom")
	st.RecordChannelAttempt("b", false, time.Millisecond, 0, 0, "boom")
	got := st.Candidates("m1")
	if len(got) != 2 {
		t.Fatalf("全部冷却时候选 = %v, want 全量 2 个", candidateIDs(got))
	}
}

// 冷却到期后渠道重新参与候选。
func TestCooldownExpires(t *testing.T) {
	st := newCoolStore(t, "30ms", 2)
	st.RecordChannelAttempt("a", false, time.Millisecond, 0, 0, "boom")
	if got := st.Candidates("m1"); len(got) != 1 || got[0].ID != "b" {
		t.Fatalf("冷却中候选 = %v, want [b]", candidateIDs(got))
	}
	time.Sleep(60 * time.Millisecond)
	got := st.Candidates("m1")
	if len(got) != 2 || got[0].ID != "a" {
		t.Fatalf("冷却到期后候选 = %v, want [a b]", candidateIDs(got))
	}
}

// 显式路由同样受冷却影响，并保持声明的顺序。
func TestCooldownOnExplicitRoute(t *testing.T) {
	st := newCoolStore(t, "60s", 3)
	if _, err := st.UpsertRoute(modelRoute()); err != nil {
		t.Fatal(err)
	}
	st.RecordChannelAttempt("a", false, time.Millisecond, 0, 0, "boom")
	got := st.Candidates("m1")
	if len(got) != 2 || got[0].ID != "b" || got[1].ID != "c" {
		t.Fatalf("显式路由冷却后候选 = %v, want [b c]", candidateIDs(got))
	}
}

// 统计（渠道/模型/全局）落盘 stats.json，重启后恢复，不清零。
func TestStatsPersistAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	dataDir := filepath.Join(dir, "data")
	yaml := `server:
  addr: ":0"
  data_dir: "` + filepath.ToSlash(dataDir) + `"
security:
  client_keys: ["sk-x"]
  admin_token: "adm"
routing:
  strategy: "failover"
  max_attempts: 3
channels:
  - id: "a"
    name: "a"
    base_url: "https://example.com/v1"
    api_key: "sk-x"
    models: ["m1"]
    enabled: true
`
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	newStore := func() *Store {
		cfg, err := config.Load(cfgPath)
		if err != nil {
			t.Fatalf("load config: %v", err)
		}
		st, err := New(cfg, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
		if err != nil {
			t.Fatalf("new store: %v", err)
		}
		return st
	}

	st := newStore()
	// 与 serve() 一致：成功走 RecordFinish；失败走 RecordChannelAttempt + RecordRequest
	st.RecordFinish("a", "m1", true, 120*time.Millisecond, 30*time.Millisecond, 10, 20, "")
	st.RecordChannelAttempt("a", false, 50*time.Millisecond, 0, 0, "boom")
	st.RecordRequest("m1", false, 50*time.Millisecond, 0, 0)
	st.CloseLogs() // 优雅退出：最终 flush

	// 模拟重启：同一数据目录重新加载
	st2 := newStore()
	defer st2.CloseLogs()
	snap := st2.Snapshot()
	if snap.TotalRequests != 2 || snap.TotalSuccess != 1 || snap.TotalFailed != 1 {
		t.Fatalf("重启后全局统计 = %+v, want requests=2 success=1 failed=1", snap)
	}
	if snap.PromptTokens != 10 || snap.CompletionTokens != 20 {
		t.Fatalf("重启后 token 统计 = %d/%d, want 10/20", snap.PromptTokens, snap.CompletionTokens)
	}
	var ch *model.ChannelStat
	for i := range snap.Channels {
		if snap.Channels[i].ID == "a" {
			ch = &snap.Channels[i]
		}
	}
	if ch == nil || ch.Stats.Requests != 2 || ch.Stats.Failed != 1 {
		t.Fatalf("重启后渠道统计 = %+v, want requests=2 failed=1", ch)
	}
	if m := snap.Models["m1"]; m == nil || m.Requests != 2 {
		t.Fatalf("重启后模型统计 = %+v, want requests=2", m)
	} // 数据文件存在（用户不手动清理就一直保留）
	if _, err := os.Stat(st2.StatsFile()); err != nil {
		t.Fatalf("stats.json 应存在: %v", err)
	}
}

func candidateIDs(list []model.Candidate) []string {
	out := make([]string, 0, len(list))
	for _, c := range list {
		out = append(out, c.ID)
	}
	return out
}

// modelRoute 显式路由：按声明顺序 a -> b -> c 尝试模型 m1。
func modelRoute() model.Route {
	enabled := true
	return model.Route{
		Model:    "m1",
		Channels: []string{"a", "b", "c"},
		Enabled:  &enabled,
	}
}
