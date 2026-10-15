package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeCfg 在临时目录里写一份配置文件。
func writeCfg(t *testing.T, dir, body string) string {
	t.Helper()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("写配置文件失败: %v", err)
	}
	return p
}

// tempDataDir 返回临时数据目录，避免 validate 时真的去创建 ./data。
func tempDataDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "data")
}

func TestLoadDefaults(t *testing.T) {
	p := writeCfg(t, t.TempDir(), "server:\n  data_dir: "+tempDataDir(t)+"\n")

	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	rt := cfg.RT()

	// 默认策略必须是「按优先级分组 + 组内轮询」，并且灰度到 proto string 常量
	if rt.Strategy != StrategyPriorityRoundRobin {
		t.Fatalf("默认策略 = %q", rt.Strategy)
	}
	if rt.MaxAttempts != 3 {
		t.Fatalf("默认 max_attempts = %d", rt.MaxAttempts)
	}
	if rt.KeepLogs != DefaultKeepLogs {
		t.Fatalf("默认 keep_logs = %d", rt.KeepLogs)
	}
	if rt.PayloadLimit != DefaultPayloadLimit {
		t.Fatalf("默认 payload_limit = %d", rt.PayloadLimit)
	}
	if rt.MaxBodyBytes != 32<<20 {
		t.Fatalf("默认 max_body = %d", rt.MaxBodyBytes)
	}
	if rt.Cooldown != DefaultCooldown {
		t.Fatalf("默认冷却 = %v", rt.Cooldown)
	}
	if !rt.ProbeEnabled || rt.ProbeInterval != DefaultProbeInterval || rt.ProbeFailThreshold != DefaultProbeFailThreshold {
		t.Fatalf("默认探活配置有误: %+v", rt)
	}
	if !rt.TrustProxyHeaders {
		t.Fatal("未显式配置时应默认信任代理头（兼容旧行为）")
	}
	if rt.Theme != ThemeAuto {
		t.Fatalf("默认主题 = %q，期望 %q", rt.Theme, ThemeAuto)
	}
	if cfg.Server.Addr != ":8080" {
		t.Fatalf("默认监听地址 = %q", cfg.Server.Addr)
	}
	if cfg.Logging.Level != "info" {
		t.Fatalf("默认日志级别 = %q", cfg.Logging.Level)
	}
	if len(cfg.Routing.RetryStatus) == 0 {
		t.Fatal("默认重试状态码不应为空")
	}
}

func TestLoadDurationFormats(t *testing.T) {
	body := `server:
  data_dir: ` + tempDataDir(t) + `
  request_timeout: 45s
  stream_timeout: 2m
routing:
  cooldown: 0
upstream:
  connect_timeout: 5s`
	p := writeCfg(t, t.TempDir(), body)

	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if got := cfg.Server.RequestTimeout.D(); got != 45*time.Second {
		t.Fatalf("request_timeout = %v", got)
	}
	if got := cfg.Upstream.ConnectTimeout.D(); got != 5*time.Second {
		t.Fatalf("connect_timeout = %v", got)
	}
	if got := cfg.Server.StreamTimeout.D(); got != 2*time.Minute {
		t.Fatalf("stream_timeout = %v", got)
	}
	// cooldown: 0（裸零）表示显式关闭冷却，这是文档里写明的用法
	if got := cfg.RT().Cooldown; got != 0 {
		t.Fatalf("cooldown=0 应关闭冷却，实际 %v", got)
	}

	// 裸数字必须报错并提示补单位：猜成秒还是毫秒都可能差 1000 倍
	missingUnit := writeCfg(t, t.TempDir(), "server:\n  request_timeout: 1500\n")
	if _, err := Load(missingUnit); err == nil {
		t.Fatal("裸数字时长应报错")
	} else if !strings.Contains(err.Error(), "缺少单位") {
		t.Fatalf("错误应提示缺少单位，实际: %v", err)
	}

	// 非法时长同样必须报错，而不是默默按 0 处理
	bad := writeCfg(t, t.TempDir(), "server:\n  request_timeout: 很快\n")
	if _, err := Load(bad); err == nil {
		t.Fatal("非法时长应报错")
	}
}

func TestLoadExplicitOverridesDefaults(t *testing.T) {
	body := `server:
  data_dir: ` + tempDataDir(t) + `
  addr: :9000
security:
  client_keys: [k1, k2]
  admin_token: t1
  trust_proxy_headers: false
ui:
  theme: dark
routing:
  strategy: failover
  max_attempts: 5
  cooldown: 0s
  probe_enabled: false
  probe_interval: 120s
logging:
  keep_logs: 50
  level: debug`
	p := writeCfg(t, t.TempDir(), body)

	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	rt := cfg.RT()

	if rt.AdminToken != "t1" || len(rt.ClientKeys) != 2 {
		t.Fatalf("鉴权配置有误: %+v", rt)
	}
	if rt.TrustProxyHeaders {
		t.Fatal("显式 false 未生效")
	}
	if rt.Strategy != StrategyFailover || rt.MaxAttempts != 5 {
		t.Fatalf("路由配置有误: %+v", rt)
	}
	// 显式 0 与「未配置」语义不同：0 = 关闭冷却
	if rt.Cooldown != 0 {
		t.Fatalf("显式 0 应关闭冷却，实际 %v", rt.Cooldown)
	}
	if rt.ProbeEnabled {
		t.Fatal("显式 false 应关闭探活")
	}
	if rt.ProbeInterval != 120*time.Second {
		t.Fatalf("探活间隔 = %v", rt.ProbeInterval)
	}
	if rt.KeepLogs != 50 {
		t.Fatalf("keep_logs = %d", rt.KeepLogs)
	}
	if rt.Theme != ThemeDark {
		t.Fatalf("主题 = %q", rt.Theme)
	}
}

func TestLoadValidation(t *testing.T) {
	dir := tempDataDir(t)
	cases := []struct {
		name string
		body string
		want string
	}{
		{"缺 id", "server:\n  data_dir: " + dir + "\nchannels:\n  - base_url: https://x", "缺少 id"},
		{"id 重复", "server:\n  data_dir: " + dir + "\nchannels:\n  - id: a\n    base_url: https://x\n  - id: a\n    base_url: https://y", "id 重复"},
		{"缺 base_url", "server:\n  data_dir: " + dir + "\nchannels:\n  - id: a", "缺少 base_url"},
		{"路由缺 model", "server:\n  data_dir: " + dir + "\nroutes:\n  - channels: [a]", "缺少 model"},
	}
	for _, c := range cases {
		p := writeCfg(t, t.TempDir(), c.body)
		_, err := Load(p)
		if err == nil {
			t.Fatalf("%s：期望报错", c.name)
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s：错误=%q，期望包含 %q", c.name, err, c.want)
		}
	}

	// 文件不存在时也要给出可读的错误
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("配置文件不存在时应报错")
	}
}

func TestLoadInvalidStrategyFallsBack(t *testing.T) {
	p := writeCfg(t, t.TempDir(), "server:\n  data_dir: "+tempDataDir(t)+"\nrouting:\n  strategy: 不存在的策略\n")
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if cfg.Routing.Strategy != StrategyPriorityRoundRobin {
		t.Fatalf("非法策略应退回默认值，实际 %q", cfg.Routing.Strategy)
	}
}

func TestSaveRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := writeCfg(t, dir, "server:\n  data_dir: "+filepath.Join(dir, "data")+"\n")

	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}

	rt := cfg.RT().Clone()
	rt.MaxAttempts = 7
	rt.Strategy = StrategyRandom
	rt.AdminToken = "new-token"
	rt.ClientKeys = []string{"k9"}
	rt.TrustProxyHeaders = false
	rt.Cooldown = 10 * time.Second
	rt.ProbeEnabled = false
	rt.KeepLogs = 123
	cfg.ApplyRuntime(rt)
	if err := Save(cfg); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}

	reloaded, err := Load(p)
	if err != nil {
		t.Fatalf("重新 Load 失败: %v", err)
	}
	got := reloaded.RT()
	if got.MaxAttempts != 7 || got.Strategy != StrategyRandom {
		t.Fatalf("运行时改动未持久化: %+v", got)
	}
	if got.AdminToken != "new-token" || len(got.ClientKeys) != 1 || got.ClientKeys[0] != "k9" {
		t.Fatalf("鉴权改动未持久化: %+v", got)
	}
	if got.TrustProxyHeaders {
		t.Fatal("trust_proxy_headers=false 未持久化")
	}
	if got.Cooldown != 10*time.Second {
		t.Fatalf("冷却 = %v", got.Cooldown)
	}
	if got.ProbeEnabled {
		t.Fatal("探活开关未持久化")
	}
	if got.KeepLogs != 123 {
		t.Fatalf("keep_logs = %d", got.KeepLogs)
	}
}

func TestSaveKeepsCommentsAndUnknownKeys(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	// 用户在配置里写的注释和网关不认识的键，都不该被回写覆盖掉
	original := `# 这是我的网关配置
server:
  addr: :9000      # 监听地址
  data_dir: ` + dataDir + `
unknown_key: keep-me
routing:
  strategy: failover   # 主备模式
`
	p := writeCfg(t, dir, original)

	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	rt := cfg.RT().Clone()
	rt.MaxAttempts = 4
	cfg.ApplyRuntime(rt)
	if err := Save(cfg); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}

	out, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	text := string(out)
	for _, want := range []string{"# 这是我的网关配置", "# 监听地址", "# 主备模式", "unknown_key: keep-me"} {
		if !strings.Contains(text, want) {
			t.Fatalf("回写丢了 %q:\n%s", want, text)
		}
	}
	if !strings.Contains(text, "max_attempts: 4") {
		t.Fatalf("新值未写入:\n%s", text)
	}
}

func TestSaveKeepsChannelsInYAML(t *testing.T) {
	dir := t.TempDir()
	// 渠道/路由由 gateway.json 管理，回写 config.yaml 时不应被覆盖成空
	body := `server:
  data_dir: ` + filepath.Join(dir, "data") + `
channels:
  - id: ch1
    base_url: https://api.example.com/v1
    models: [gpt-4o]
routes:
  - model: gpt-4o
    channels: [ch1]
`
	p := writeCfg(t, dir, body)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if err := Save(cfg); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}
	reloaded, err := Load(p)
	if err != nil {
		t.Fatalf("重新 Load 失败: %v", err)
	}
	if len(reloaded.Channels) != 1 || reloaded.Channels[0].ID != "ch1" {
		t.Fatalf("回写丢失了 channels: %+v", reloaded.Channels)
	}
	if len(reloaded.Routes) != 1 || reloaded.Routes[0].Model != "gpt-4o" {
		t.Fatalf("回写丢失了 routes: %+v", reloaded.Routes)
	}
}

func TestSaveWithoutPathFails(t *testing.T) {
	// 直接构造（没走 Load）的 Config 没有落盘目标，必须显式报错而不是写到当前目录
	if err := Save(&Config{}); err == nil {
		t.Fatal("无路径时应报错")
	}
}

func TestRuntimeCloneIsDeep(t *testing.T) {
	rt := &Runtime{ClientKeys: []string{"a"}, RetryStatus: []int{500}}
	cp := rt.Clone()
	cp.ClientKeys[0] = "changed"
	cp.RetryStatus[0] = 404
	if rt.ClientKeys[0] != "a" {
		t.Fatal("Clone 未深拷贝 ClientKeys")
	}
	if rt.RetryStatus[0] != 500 {
		t.Fatal("Clone 未深拷贝 RetryStatus")
	}
	if (&Runtime{}).Clone() == nil {
		t.Fatal("nil Clone 应返回空 Runtime 而不是 nil")
	}
}

func TestApplyRuntimeIsSnapshot(t *testing.T) {
	cfg := &Config{}
	in := &Runtime{MaxAttempts: 9, ClientKeys: []string{"k"}}
	cfg.ApplyRuntime(in)

	got := cfg.RT()
	if got.MaxAttempts != 9 {
		t.Fatalf("ApplyRuntime 未生效: %+v", got)
	}
	// ApplyRuntime 存的是副本：调用方之后再改不会穿透进来
	in.MaxAttempts = 1
	if cfg.RT().MaxAttempts != 9 {
		t.Fatal("ApplyRuntime 应保存快照，不能受后续修改影响")
	}
}

func TestRTLazilyInitializes(t *testing.T) {
	// 不走 Load（例如单测里直接构造）时也要有一份可用的运行时配置
	cfg := &Config{}
	rt := cfg.RT()
	if rt == nil {
		t.Fatal("RT 不应为 nil")
	}
	if rt.Strategy != StrategyPriorityRoundRobin || rt.MaxAttempts <= 0 {
		t.Fatalf("懒初始化结果不完整: %+v", rt)
	}
	if rt.MaxBodyBytes <= 0 {
		t.Fatalf("MaxBodyBytes 未兜底: %d", rt.MaxBodyBytes)
	}
}

func TestChannelWeightDefault(t *testing.T) {
	p := writeCfg(t, t.TempDir(), `server:
  data_dir: `+tempDataDir(t)+`
channels:
  - id: a
    base_url: https://x
    weight: 0
  - id: b
    base_url: https://y
    weight: 5
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if cfg.Channels[0].Weight != 1 {
		t.Fatalf("weight=0 应补 1，实际 %d", cfg.Channels[0].Weight)
	}
	if cfg.Channels[1].Weight != 5 {
		t.Fatalf("显式权重被覆盖: %d", cfg.Channels[1].Weight)
	}
}

func TestStrategyAndThemeVocabulary(t *testing.T) {
	valid := []string{StrategyPriorityRoundRobin, StrategyRoundRobin, StrategyFailover, StrategyRandom}
	for _, v := range valid {
		if !IsValidStrategy(v) {
			t.Fatalf("%q 应被判定为合法策略", v)
		}
	}
	for _, bad := range []string{"", "round-robin", "PRIORITY_ROUND_ROBIN"} {
		if IsValidStrategy(bad) {
			t.Fatalf("%q 不该被判定为合法策略", bad)
		}
	}
	if len(Strategies) != len(valid) {
		t.Fatalf("Strategies 列表与常量不一致: %v", Strategies)
	}

	for _, v := range []string{ThemeAuto, ThemeDark, ThemeLight} {
		if !IsValidTheme(v) {
			t.Fatalf("%q 应被判定为合法主题", v)
		}
	}
	if IsValidTheme("blue") || IsValidTheme("") {
		t.Fatal("非 auto/dark/light 的主题应被拒绝")
	}
}

func TestLogFileEnabled(t *testing.T) {
	if !(&Config{}).LogFileEnabled() {
		t.Fatal("未配置时应默认开启落盘")
	}
	off := false
	if (&Config{Logging: LoggingConfig{FileEnabled: &off}}).LogFileEnabled() {
		t.Fatal("显式 false 应关闭落盘")
	}
}

// TestRepoConfigFilesParse 守住仓库自带的配置样例：
// 改配置解析（尤其是 Duration）后，这两份文件必须还能被加载，
// 否则用户照着 README/config.yaml 抄就起不来。
func TestRepoConfigFilesParse(t *testing.T) {
	for _, name := range []string{"config.yaml", "dev-config.yaml"} {
		p := filepath.Join("..", "..", name)
		if _, err := os.Stat(p); err != nil {
			t.Skipf("跳过：%s 不存在", p)
		}
		cfg, err := Load(p)
		if err != nil {
			t.Fatalf("加载 %s 失败: %v", name, err)
		}
		defer cleanupEmptyDir(cfg.Server.DataDir) // Load 的 validate 会创建数据目录，别在包里留垃圾
		if cfg.Server.Addr == "" {
			t.Fatalf("%s 缺少监听地址", name)
		}
		if cfg.RT().MaxAttempts <= 0 {
			t.Fatalf("%s 的 max_attempts 未生效", name)
		}
		if cfg.RT().RequestTimeout <= 0 {
			t.Fatalf("%s 的 request_timeout 未生效", name)
		}
	}
}

// cleanupEmptyDir 删除 Load 过程中创建的空目录（只删空的，避免误伤运行时数据）。
func cleanupEmptyDir(dir string) {
	if dir == "" {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) > 0 {
		return
	}
	_ = os.Remove(dir)
}

func TestDataFilePath(t *testing.T) {
	if got := (&Config{}).DataFile(); got != filepath.Join("data", "gateway.json") {
		t.Fatalf("默认数据文件 = %q", got)
	}
	c := &Config{Server: ServerConfig{DataDir: "/tmp/x"}}
	if got := c.DataFile(); got != filepath.Join("/tmp/x", "gateway.json") {
		t.Fatalf("数据文件 = %q", got)
	}
}
