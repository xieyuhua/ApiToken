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

	"github.com/demo1/apitoken/internal/model"
	"github.com/demo1/apitoken/internal/proxy"
	"github.com/demo1/apitoken/internal/store"
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
		"logs_kept": len(h.store.Logs(0)),
		"uptime":    h.store.Uptime().String(),
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

// buildChecks 静态配置自检。
func (h *Handler) buildChecks() []Check {
	cfg := h.cfg
	var out []Check
	add := func(group, name, level, detail, fix string) {
		out = append(out, Check{Group: group, Name: name, Level: level, Detail: detail, Fix: fix})
	}

	// 鉴权
	if len(cfg.Security.ClientKeys) == 0 {
		add("config", "客户端密钥", LevelWarn, "未配置 client_keys，知道地址即可调用网关", "在 config.yaml 的 security.client_keys 中配置至少一个密钥")
	} else {
		add("config", "客户端密钥", LevelPass, fmt.Sprintf("已配置 %d 个", len(cfg.Security.ClientKeys)), "")
	}
	if cfg.Security.AdminToken == "" {
		add("config", "管理令牌", LevelWarn, "未配置 admin_token，管理接口无鉴权", "配置 security.admin_token 保护 /admin/*")
	} else {
		add("config", "管理令牌", LevelPass, "已配置", "")
	}

	// 路由策略
	switch cfg.Routing.Strategy {
	case "priority_round_robin", "round_robin", "failover", "random":
		add("config", "路由策略", LevelPass, cfg.Routing.Strategy, "")
	default:
		add("config", "路由策略", LevelFail, "未知策略 "+cfg.Routing.Strategy, "可选：priority_round_robin / round_robin / failover / random")
	}
	if cfg.Routing.MaxAttempts <= 1 {
		add("config", "故障转移", LevelWarn, "max_attempts=1，渠道失败不会切换到下一个", "建议设为 2~3")
	} else {
		add("config", "故障转移", LevelPass, fmt.Sprintf("最多尝试 %d 个渠道", cfg.Routing.MaxAttempts), "")
	}
	has5xx := false
	for _, s := range cfg.Routing.RetryStatus {
		if s >= 500 {
			has5xx = true
		}
	}
	if !has5xx {
		add("config", "重试状态码", LevelWarn, "未包含任何 5xx，上游 5xx 不会触发切换", "在 routing.retry_status 中加入 500/502/503/504")
	} else {
		add("config", "重试状态码", LevelPass, fmt.Sprintf("%v", cfg.Routing.RetryStatus), "")
	}

	// 渠道
	all := h.store.ListChannels()
	enabled := h.enabledChannels()
	if len(all) == 0 {
		add("channel", "渠道数量", LevelFail, "尚未添加任何渠道", "在控制台添加渠道或写入 config.yaml 的 channels")
	} else if len(enabled) == 0 {
		add("channel", "渠道数量", LevelFail, fmt.Sprintf("共 %d 个渠道但全部停用", len(all)), "启用至少一个渠道")
	} else {
		add("channel", "渠道数量", LevelPass, fmt.Sprintf("启用 %d / 共 %d", len(enabled), len(all)), "")
	}

	noKey, noModel, wildcard, badURL := 0, 0, 0, 0
	for _, c := range enabled {
		if c.APIKey == "" {
			noKey++
		}
		if len(c.Models) == 0 {
			noModel++
		}
		for _, m := range c.Models {
			if m == "*" {
				wildcard++
			}
		}
		if !strings.HasPrefix(c.BaseURL, "http://") && !strings.HasPrefix(c.BaseURL, "https://") {
			badURL++
		}
	}
	if noKey > 0 {
		add("channel", "API Key", LevelFail, fmt.Sprintf("%d 个启用渠道未填 api_key", noKey), "补齐渠道密钥，否则上游会返回 401")
	} else {
		add("channel", "API Key", LevelPass, "全部启用渠道均已配置", "")
	}
	if badURL > 0 {
		add("channel", "Base URL 格式", LevelFail, fmt.Sprintf("%d 个渠道的 base_url 不以 http(s) 开头", badURL), "应形如 https://api.example.com/v1")
	} else {
		add("channel", "Base URL 格式", LevelPass, "格式正确", "")
	}
	if noModel > 0 {
		add("channel", "模型范围", LevelWarn, fmt.Sprintf("%d 个渠道未限定模型，等于放通全部模型名", noModel), "在渠道里列出实际支持的模型名，避免误路由")
	} else {
		add("channel", "模型范围", LevelPass, "均已限定模型", "")
	}
	if wildcard > 0 {
		add("channel", "通配模型", LevelWarn, "存在 * 通配配置，会接收任意模型名", "移除 * 并显式列出模型")
	}

	// 路由引用
	routes := h.store.ListRoutes()
	ids := map[string]bool{}
	for _, c := range all {
		ids[c.ID] = true
	}
	missing, dup := 0, 0
	seenModel := map[string]bool{}
	for _, rt := range routes {
		if seenModel[rt.Model] {
			dup++
		}
		seenModel[rt.Model] = true
		if len(rt.Channels) == 0 {
			add("route", "路由 "+rt.Model, LevelWarn, "未绑定任何渠道", "为该路由选择至少一个渠道，或删除它")
		}
		for _, cid := range rt.Channels {
			if !ids[cid] {
				missing++
			}
		}
	}
	if missing > 0 {
		add("route", "路由引用", LevelFail, fmt.Sprintf("%d 处路由引用了不存在的渠道", missing), "检查路由里的渠道 ID 是否拼写正确")
	} else {
		add("route", "路由引用", LevelPass, fmt.Sprintf("%d 条路由引用均有效", len(routes)), "")
	}
	if dup > 0 {
		add("route", "路由重复", LevelWarn, fmt.Sprintf("%d 个模型被重复定义", dup), "同名路由以最后一条为准")
	}
	if len(routes) == 0 {
		add("route", "显式路由", LevelSkip, "未配置，全部按渠道 models 自动路由", "")
	}

	// 数据与日志
	dir := filepath.Dir(cfg.DataFile())
	probe := filepath.Join(dir, ".write-test")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		add("storage", "数据目录", LevelFail, dir+" 不可创建: "+err.Error(), "检查目录权限或 data_dir 配置")
	} else if err := os.WriteFile(probe, []byte("ok"), 0o600); err != nil {
		add("storage", "数据目录", LevelFail, dir+" 不可写: "+err.Error(), "检查目录权限")
	} else {
		_ = os.Remove(probe)
		add("storage", "数据目录", LevelPass, dir+" 可写", "")
	}
	if fi, err := os.Stat(cfg.DataFile()); err == nil {
		add("storage", "渠道持久化", LevelPass, fmt.Sprintf("%s（%d 字节）", cfg.DataFile(), fi.Size()), "")
	} else {
		add("storage", "渠道持久化", LevelWarn, "尚无 data/gateway.json，将在首次变更时生成", "")
	}
	if cfg.Logging.KeepLogs < 100 {
		add("logging", "日志容量", LevelWarn, fmt.Sprintf("keep_logs=%d 偏小", cfg.Logging.KeepLogs), "建议 500~2000")
	} else {
		add("logging", "日志容量", LevelPass, fmt.Sprintf("内存保留 %d 条调用日志", cfg.Logging.KeepLogs), "")
	}
	if !cfg.Logging.AccessLog {
		add("logging", "访问日志", LevelWarn, "access_log=false，控制台不输出访问日志", "排查问题时建议开启")
	}
	if cfg.Logging.RecordPayload {
		add("logging", "载荷记录", LevelWarn, "record_payload=true，日志会记录请求/响应内容（可能含敏感信息）", "生产环境建议关闭")
	}

	// 网络
	proxyMode := "跟随环境变量 HTTP_PROXY/HTTPS_PROXY"
	if p := strings.TrimSpace(cfg.Upstream.ProxyURL); p != "" {
		proxyMode = "proxy_url = " + p
	}
	add("network", "出网代理", LevelPass, proxyMode, "")
	if cfg.Server.StreamTimeout.D() <= 0 {
		add("network", "流式超时", LevelWarn, "stream_timeout 未配置", "建议 300s 以上")
	}
	return out
}

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
			info := store.HealthInfo{CheckedAt: time.Now(), LatencyMS: res.LatencyMS}
			if res.OK {
				info.Status = "healthy"
				info.Detail = res.Reply
			} else {
				info.Status = "unhealthy"
				info.Error = res.Error
			}
			h.store.SetHealth(c.ID, info)
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
	if addrs, err := net.DefaultResolver.LookupHost(ctx, host); err == nil && len(addrs) > 0 {
		res.DNS = true
		res.Addrs = addrs
	} else {
		res.Error = "DNS 解析失败: " + err.Error()
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
