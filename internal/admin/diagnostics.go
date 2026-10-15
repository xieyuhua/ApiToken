package admin

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/xieyuhua/apitoken/internal/config"
	"github.com/xieyuhua/apitoken/internal/model"
	"github.com/xieyuhua/apitoken/internal/proxy"
)

// 诊断结论等级。
const (
	LevelPass = "pass" // 通过
	LevelWarn = "warn" // 警告
	LevelFail = "fail" // 失败
	LevelSkip = "skip" // 跳过
)

// Check 单条自检结果。
type Check struct {
	Group  string `json:"group"` // config / channel / route / storage / logging / network
	Name   string `json:"name"`
	Level  string `json:"level"`
	Detail string `json:"detail,omitempty"`
	Fix    string `json:"fix,omitempty"` // 修复建议
}

// ChannelProbe 单个渠道的连通性体检结果。
type ChannelProbe struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	BaseURL   string `json:"base_url"`
	OK        bool   `json:"ok"`
	Status    int    `json:"status"`
	LatencyMS int64  `json:"latency_ms"`
	Model     string `json:"model"`
	Reply     string `json:"reply,omitempty"`
	Error     string `json:"error,omitempty"`
	Models    int    `json:"models_count"`
}

// NetCheck 域名级网络体检结果。
type NetCheck struct {
	Host  string   `json:"host"`
	DNS   bool     `json:"dns"`
	Addrs []string `json:"addrs,omitempty"`
	TCP   bool     `json:"tcp"`
	TCPMS int64    `json:"tcp_ms"`
	TLS   bool     `json:"tls"`
	TLSMS int64    `json:"tls_ms"`
	Level string   `json:"level"`
	Error string   `json:"error,omitempty"`
}

// DiagSummary 体检汇总。
type DiagSummary struct {
	Pass int `json:"pass"`
	Warn int `json:"warn"`
	Fail int `json:"fail"`
	Skip int `json:"skip"`
}

// DiagReport 体检报告。
type DiagReport struct {
	StartedAt  time.Time      `json:"started_at"`
	DurationMS int64          `json:"duration_ms"`
	Checks     []Check        `json:"checks"`
	Channels   []ChannelProbe `json:"channels"`
	Network    []NetCheck     `json:"network"`
	Summary    DiagSummary    `json:"summary"`
	Healthy    bool           `json:"healthy"`
}

// diagnosticsOverview 网关静态自检（不发起外部请求），用于页面首屏。
func (h *Handler) diagnosticsOverview(w http.ResponseWriter, _ *http.Request) {
	checks := h.buildChecks()
	sum := summarize(checks)
	writeJSON(w, http.StatusOK, map[string]any{
		"checks":    checks,
		"summary":   sum,
		"healthy":   sum.Fail == 0,
		"channels":  len(h.enabledChannels()),
		"logs_kept": h.store.LogsCount(),
		"uptime":    h.store.UptimeString(),
		"config":    h.cfg.Path(),
		"data_file": h.cfg.DataFile(),
	})
}

// diagnosticsRun 执行完整体检：配置自检 + 渠道连通性 + 域名网络探测。
func (h *Handler) diagnosticsRun(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()

	checks := h.buildChecks()
	channels := h.probeChannels(ctx)
	network := h.probeNetwork(ctx)

	report := DiagReport{
		StartedAt:  start,
		DurationMS: time.Since(start).Milliseconds(),
		Checks:     checks,
		Channels:   channels,
		Network:    network,
		Summary:    summarize(checks),
	}
	for _, c := range channels {
		if c.OK {
			report.Summary.Pass++
		} else {
			report.Summary.Fail++
		}
	}
	for _, n := range network {
		switch n.Level {
		case LevelFail:
			report.Summary.Fail++
		case LevelWarn:
			report.Summary.Warn++
		default:
			report.Summary.Pass++
		}
	}
	report.Healthy = report.Summary.Fail == 0
	writeJSON(w, http.StatusOK, report)
}

// emit 是某个检查组记录结果的函数：name 为组内的检查项名，group 由注册表指定。
type emit func(name, level, detail, fix string)

// checkGroup 一组自检项。
//
// buildChecks 原来是 165 行、内含闭包 add(...) 的顺序执行：20 多个检查项挤在一起，
// 新增一项要在函数中间找位置，也没法单独复用「只检查渠道」这类子集合。
// 改成注册表后每组一个函数，顺序即页面上的展示顺序，新增只需加一行。
type checkGroup struct {
	group string
	fn    func(c *checkCtx, add emit)
}

func checkGroups() []checkGroup {
	return []checkGroup{
		{"config", checkAuth},
		{"config", checkRouting},
		{"channel", checkChannels},
		{"route", checkRoutes},
		{"storage", checkStorage},
		{"logging", checkLogging},
		{"network", checkNetwork},
	}
}

// checkCtx 检查所需的输入。一次性算好派生数据，各组只读取不再重复查 store。
type checkCtx struct {
	h       *Handler
	cfg     *config.Config
	rt      *config.Runtime
	all     []model.Channel
	enabled []model.Channel
	routes  []model.Route
}

// buildChecks 静态配置自检：按注册表顺序产出全部检查项。
func (h *Handler) buildChecks() []Check {
	c := &checkCtx{
		h:       h,
		cfg:     h.cfg,
		rt:      h.cfg.RT(),
		all:     h.store.ListChannels(),
		routes:  h.store.ListRoutes(),
		enabled: h.enabledChannels(),
	}
	var out []Check
	for _, g := range checkGroups() {
		g := g
		g.fn(c, func(name, level, detail, fix string) {
			out = append(out, Check{Group: g.group, Name: name, Level: level, Detail: detail, Fix: fix})
		})
	}
	return out
}

// checkAuth 客户端密钥与管理令牌是否有配置。
func checkAuth(c *checkCtx, add emit) {
	if len(c.rt.ClientKeys) == 0 {
		add("客户端密钥", LevelWarn, "未配置 client_keys，知道地址即可调用网关", "在 config.yaml 的 security.client_keys 中配置至少一个密钥")
	} else {
		add("客户端密钥", LevelPass, fmt.Sprintf("已配置 %d 个", len(c.rt.ClientKeys)), "")
	}
	if c.rt.AdminToken == "" {
		add("管理令牌", LevelWarn, "未配置 admin_token，管理接口无鉴权", "配置 security.admin_token 保护 /admin/*")
	} else {
		add("管理令牌", LevelPass, "已配置", "")
	}
}

// checkRouting 路由策略、故障转移与重试状态码。
func checkRouting(c *checkCtx, add emit) {
	if config.IsValidStrategy(c.rt.Strategy) {
		add("路由策略", LevelPass, c.rt.Strategy, "")
	} else {
		add("路由策略", LevelFail, "未知策略 "+c.rt.Strategy, "可选："+strings.Join(config.Strategies, " / "))
	}
	if c.rt.MaxAttempts <= 1 {
		add("故障转移", LevelWarn, "max_attempts=1，渠道失败不会切换到下一个", "建议设为 2~3")
	} else {
		add("故障转移", LevelPass, fmt.Sprintf("最多尝试 %d 个渠道", c.rt.MaxAttempts), "")
	}
	has5xx := false
	for _, s := range c.rt.RetryStatus {
		if s >= 500 {
			has5xx = true
		}
	}
	if !has5xx {
		add("重试状态码", LevelWarn, "未包含任何 5xx，上游 5xx 不会触发切换", "在 routing.retry_status 中加入 500/502/503/504")
	} else {
		add("重试状态码", LevelPass, fmt.Sprintf("%v", c.rt.RetryStatus), "")
	}
}

// channelCounts 单个渠道配置的常见问题计数。
type channelCounts struct {
	noKey, noModel, wildcard, badURL int
}

func countChannelIssues(list []model.Channel) channelCounts {
	var n channelCounts
	for _, c := range list {
		if c.APIKey == "" {
			n.noKey++
		}
		if len(c.Models) == 0 {
			n.noModel++
		}
		for _, m := range c.Models {
			if m == "*" {
				n.wildcard++
			}
		}
		if !strings.HasPrefix(c.BaseURL, "http://") && !strings.HasPrefix(c.BaseURL, "https://") {
			n.badURL++
		}
	}
	return n
}

// checkChannels 渠道数量与渠道配置的完整性。
func checkChannels(c *checkCtx, add emit) {
	switch {
	case len(c.all) == 0:
		add("渠道数量", LevelFail, "尚未添加任何渠道", "在控制台添加渠道或写入 config.yaml 的 channels")
	case len(c.enabled) == 0:
		add("渠道数量", LevelFail, fmt.Sprintf("共 %d 个渠道但全部停用", len(c.all)), "启用至少一个渠道")
	default:
		add("渠道数量", LevelPass, fmt.Sprintf("启用 %d / 共 %d", len(c.enabled), len(c.all)), "")
	}

	n := countChannelIssues(c.enabled)
	if n.noKey > 0 {
		add("API Key", LevelFail, fmt.Sprintf("%d 个启用渠道未填 api_key", n.noKey), "补齐渠道密钥，否则上游会返回 401")
	} else {
		add("API Key", LevelPass, "全部启用渠道均已配置", "")
	}
	if n.badURL > 0 {
		add("Base URL 格式", LevelFail, fmt.Sprintf("%d 个渠道的 base_url 不以 http(s) 开头", n.badURL), "应形如 https://api.example.com/v1")
	} else {
		add("Base URL 格式", LevelPass, "格式正确", "")
	}
	if n.noModel > 0 {
		add("模型范围", LevelWarn, fmt.Sprintf("%d 个渠道未限定模型，等于放通全部模型名", n.noModel), "在渠道里列出实际支持的模型名，避免误路由")
	} else {
		add("模型范围", LevelPass, "均已限定模型", "")
	}
	if n.wildcard > 0 {
		add("通配模型", LevelWarn, "存在 * 通配配置，会接收任意模型名", "移除 * 并显式列出模型")
	}
}

// checkRoutes 显式路由的渠道引用与重复定义。
func checkRoutes(c *checkCtx, add emit) {
	ids := map[string]bool{}
	for _, ch := range c.all {
		ids[ch.ID] = true
	}
	missing, dup := 0, 0
	seenModel := map[string]bool{}
	for _, r := range c.routes {
		if seenModel[r.Model] {
			dup++
		}
		seenModel[r.Model] = true
		if len(r.Channels) == 0 {
			add("路由 "+r.Model, LevelWarn, "未绑定任何渠道", "为该路由选择至少一个渠道，或删除它")
		}
		for _, cid := range r.Channels {
			if !ids[cid] {
				missing++
			}
		}
	}
	if missing > 0 {
		add("路由引用", LevelFail, fmt.Sprintf("%d 处路由引用了不存在的渠道", missing), "检查路由里的渠道 ID 是否拼写正确")
	} else {
		add("路由引用", LevelPass, fmt.Sprintf("%d 条路由引用均有效", len(c.routes)), "")
	}
	if dup > 0 {
		add("路由重复", LevelWarn, fmt.Sprintf("%d 个模型被重复定义", dup), "同名路由以最后一条为准")
	}
	if len(c.routes) == 0 {
		add("显式路由", LevelSkip, "未配置，全部按渠道 models 自动路由", "")
	}
}

// checkStorage 数据目录可写性与渠道持久化文件。
func checkStorage(c *checkCtx, add emit) {
	dir := filepath.Dir(c.cfg.DataFile())
	probe := filepath.Join(dir, ".write-test")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		add("数据目录", LevelFail, dir+" 不可创建: "+err.Error(), "检查目录权限或 data_dir 配置")
	} else if err := os.WriteFile(probe, []byte("ok"), 0o600); err != nil {
		add("数据目录", LevelFail, dir+" 不可写: "+err.Error(), "检查目录权限")
	} else {
		_ = os.Remove(probe)
		add("数据目录", LevelPass, dir+" 可写", "")
	}
	if fi, err := os.Stat(c.cfg.DataFile()); err == nil {
		add("渠道持久化", LevelPass, fmt.Sprintf("%s（%d 字节）", c.cfg.DataFile(), fi.Size()), "")
	} else {
		add("渠道持久化", LevelWarn, "尚无 data/gateway.json，将在首次变更时生成", "")
	}
}

// checkLogging 日志容量、开关与落盘管线健康度。
func checkLogging(c *checkCtx, add emit) {
	if c.rt.KeepLogs < minRecommendedKeepLogs {
		add("日志容量", LevelWarn, fmt.Sprintf("keep_logs=%d 偏小", c.rt.KeepLogs), "建议 500~2000")
	} else {
		add("日志容量", LevelPass, fmt.Sprintf("内存保留 %d 条调用日志", c.rt.KeepLogs), "")
	}
	if !c.rt.AccessLog {
		add("访问日志", LevelWarn, "access_log=false，控制台不输出访问日志", "排查问题时建议开启")
	}
	if c.rt.RecordPayload {
		add("载荷记录", LevelWarn, "record_payload=true，日志会记录请求/响应内容（可能含敏感信息）", "生产环境建议关闭")
	}
	// 落盘队列满是静默丢弃的：这里把它上升为可见的自检项，
	// 否则用户只能从「日志缺了」反推，很难定位到磁盘 IO 跟不上。
	if dropped, written := c.h.store.LogSinkStats(); dropped > 0 {
		add("日志落盘", LevelWarn,
			fmt.Sprintf("因队列已满累计丢弃 %d 条（已落盘 %d 条）", dropped, written),
			"磁盘写入跟不上日志量：检查日志目录所在磁盘，或降低 record_payload / payload_limit")
	}
}

// checkNetwork 出网代理与流式超时。
func checkNetwork(c *checkCtx, add emit) {
	proxyMode := "跟随环境变量 HTTP_PROXY/HTTPS_PROXY"
	if p := strings.TrimSpace(c.rt.ProxyURL); p != "" {
		proxyMode = "proxy_url = " + p
	}
	add("出网代理", LevelPass, proxyMode, "")
	if c.rt.StreamTimeout <= 0 {
		add("流式超时", LevelWarn, "stream_timeout 未配置", "建议 300s 以上")
	}
}

// minRecommendedKeepLogs keep_logs 的推荐下限，低于此值日志页面基本查不到东西。
const minRecommendedKeepLogs = 100

func (h *Handler) enabledChannels() []model.Channel {
	var out []model.Channel
	for _, c := range h.store.ListChannels() {
		if c.Enabled {
			out = append(out, c)
		}
	}
	return out
}

// probeChannels 并发探测所有启用渠道的连通性（最多 20 个，并发 5）。
func (h *Handler) probeChannels(ctx context.Context) []ChannelProbe {
	channels := h.enabledChannels()
	if len(channels) == 0 {
		return nil
	}
	if len(channels) > 20 {
		channels = channels[:20]
	}
	out := make([]ChannelProbe, len(channels))
	sem := make(chan struct{}, 5)
	var wg sync.WaitGroup
	for i, c := range channels {
		wg.Add(1)
		go func(i int, c model.Channel) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			res := proxy.Probe(ctx, h.cfg, h.client, c, "", "ping")
			out[i] = ChannelProbe{
				ID: c.ID, Name: c.Name, BaseURL: c.BaseURL, OK: res.OK, Status: res.Status,
				LatencyMS: res.LatencyMS, Model: res.Model, Reply: res.Reply, Error: res.Error,
				Models: len(c.Models),
			}
			h.store.SetHealth(c.ID, healthInfoFromProbe(res))
		}(i, c)
	}
	wg.Wait()
	return out
}

// probeNetwork 对所有上游域名做 DNS / TCP / TLS 探测。
func (h *Handler) probeNetwork(ctx context.Context) []NetCheck {
	ports := map[string]string{}
	for _, c := range h.enabledChannels() {
		host := hostOf(c.BaseURL)
		if host == "" {
			continue
		}
		p := "443"
		if strings.HasPrefix(strings.ToLower(c.BaseURL), "http://") {
			p = "80"
		}
		ports[host] = p
	}
	if len(ports) == 0 {
		return nil
	}
	list := make([]string, 0, len(ports))
	for k := range ports {
		list = append(list, k)
	}
	sort.Strings(list)

	out := make([]NetCheck, len(list))
	sem := make(chan struct{}, 6)
	var wg sync.WaitGroup
	for i, host := range list {
		wg.Add(1)
		go func(i int, host string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out[i] = checkHost(ctx, host, ports[host])
		}(i, host)
	}
	wg.Wait()
	return out
}

func checkHost(ctx context.Context, host, port string) NetCheck {
	res := NetCheck{Host: host, Level: LevelFail}
	if addrs, err := net.DefaultResolver.LookupHost(ctx, host); err != nil {
		res.Error = "DNS 解析失败: " + err.Error()
	} else if len(addrs) == 0 {
		res.Error = "DNS 未返回任何地址"
	} else {
		res.DNS = true
		res.Addrs = addrs
	}

	addr := net.JoinHostPort(host, port)
	d := net.Dialer{Timeout: 8 * time.Second}
	start := time.Now()
	if conn, err := d.DialContext(ctx, "tcp", addr); err == nil {
		res.TCP = true
		res.TCPMS = time.Since(start).Milliseconds()
		_ = conn.Close()
	} else if res.Error == "" {
		res.Error = "TCP 连接失败: " + err.Error()
	}

	if res.TCP && port == "443" {
		tstart := time.Now()
		if conn, err := tls.DialWithDialer(&d, "tcp", addr, &tls.Config{ServerName: host}); err == nil {
			res.TLS = true
			res.TLSMS = time.Since(tstart).Milliseconds()
			_ = conn.Close()
		} else if res.Error == "" {
			res.Error = "TLS 握手失败: " + err.Error()
		}
	} else if res.TCP {
		res.TLS = true // 明文 HTTP 视为无需 TLS
	}

	switch {
	case res.DNS && res.TCP && res.TLS:
		res.Level = LevelPass
	case res.DNS && res.TCP:
		res.Level = LevelWarn
	}
	return res
}

func hostOf(rawURL string) string {
	s := strings.TrimSpace(rawURL)
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	if i := strings.Index(s, "/"); i >= 0 {
		s = s[:i]
	}
	if i := strings.Index(s, ":"); i > 0 {
		s = s[:i]
	}
	return s
}

func summarize(checks []Check) DiagSummary {
	var s DiagSummary
	for _, c := range checks {
		switch c.Level {
		case LevelPass:
			s.Pass++
		case LevelWarn:
			s.Warn++
		case LevelFail:
			s.Fail++
		default:
			s.Skip++
		}
	}
	return s
}
