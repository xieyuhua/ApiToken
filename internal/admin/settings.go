package admin

import (
	"net/http"
	"strings"
	"time"

	"github.com/xieyuhua/apitoken/internal/config"
	"github.com/xieyuhua/apitoken/internal/model"
)

// SettingsView 设置页可读写的配置（仅限可热更新项）。
type SettingsView struct {
	// 鉴权
	ClientKeys []string `json:"client_keys"`
	AdminToken string   `json:"admin_token"`
	// TrustProxyHeaders 是否信任反向代理头作为来源 IP（nil = 不修改）
	TrustProxyHeaders *bool `json:"trust_proxy_headers"`

	// 路由（布尔项为指针：nil 表示不修改）
	Strategy         string `json:"strategy"`
	MaxAttempts      int    `json:"max_attempts"`
	ForceStreamUsage *bool  `json:"force_stream_usage"`
	RetryStatus      []int  `json:"retry_status"`
	// CooldownSeconds 渠道失败后的冷却秒数；0 = 关闭冷却。nil = 不修改
	CooldownSeconds *int `json:"cooldown_seconds"`

	// ProbeEnabled 是否开启渠道后台自动探活（连续失败即进冷却）。nil = 不修改
	ProbeEnabled *bool `json:"probe_enabled"`
	// ProbeIntervalSeconds 探活间隔（秒）。nil = 不修改
	ProbeIntervalSeconds *int `json:"probe_interval_seconds"`
	// ProbeFailThreshold 连续探活失败几次才进冷却；0 = 不修改
	ProbeFailThreshold int `json:"probe_fail_threshold"`

	// 日志
	AccessLog     *bool `json:"access_log"`
	KeepLogs      int   `json:"keep_logs"`
	RecordPayload *bool `json:"record_payload"`
	PayloadLimit  int   `json:"payload_limit"`
	// LogArchive 调用日志页的默认数据源：true = 磁盘归档，false = 内存热缓冲。nil = 不修改
	LogArchive *bool  `json:"log_archive"`
	LogLevel   string `json:"log_level"`

	// 超时（秒）
	RequestTimeout int `json:"request_timeout"`
	StreamTimeout  int `json:"stream_timeout"`

	// 界面主题：auto | dark | light
	Theme string `json:"theme"`
}

// SettingsMeta 展示用的只读信息与提示。
type SettingsMeta struct {
	ConfigFile    string   `json:"config_file"`
	DataFile      string   `json:"data_file"`
	StatsFile     string   `json:"stats_file"`
	Addr          string   `json:"addr"`
	Version       string   `json:"version"`
	Uptime        string   `json:"uptime"`
	RestartNeeded []string `json:"restart_needed"`
	HotFields     []string `json:"hot_fields"`
}

func viewFromRuntime(rt *config.Runtime, level string) SettingsView {
	cd := int(rt.Cooldown / time.Second)
	iv := int(rt.ProbeInterval / time.Second)
	return SettingsView{
		ClientKeys:           append([]string(nil), rt.ClientKeys...),
		AdminToken:           rt.AdminToken,
		TrustProxyHeaders:    boolPtr(rt.TrustProxyHeaders),
		Strategy:             rt.Strategy,
		MaxAttempts:          rt.MaxAttempts,
		ForceStreamUsage:     boolPtr(rt.ForceStreamUsage),
		RetryStatus:          append([]int(nil), rt.RetryStatus...),
		CooldownSeconds:      &cd,
		ProbeEnabled:         boolPtr(rt.ProbeEnabled),
		ProbeIntervalSeconds: &iv,
		ProbeFailThreshold:   rt.ProbeFailThreshold,
		AccessLog:            boolPtr(rt.AccessLog),
		KeepLogs:             rt.KeepLogs,
		RecordPayload:        boolPtr(rt.RecordPayload),
		PayloadLimit:         rt.PayloadLimit,
		LogArchive:           boolPtr(rt.LogArchive),
		LogLevel:             level,
		RequestTimeout:       int(rt.RequestTimeout / time.Second),
		StreamTimeout:        int(rt.StreamTimeout / time.Second),
		Theme:                rt.Theme,
	}
}

func boolPtr(b bool) *bool { return &b }

// getSettings GET /admin/settings（?reveal=1 返回密钥明文）
func (h *Handler) getSettings(w http.ResponseWriter, r *http.Request) {
	rt := h.cfg.RT()
	v := viewFromRuntime(rt, h.cfg.Logging.Level)
	masked := v
	masked.ClientKeys = make([]string, 0, len(v.ClientKeys))
	for _, k := range v.ClientKeys {
		masked.ClientKeys = append(masked.ClientKeys, model.MaskSecret(k))
	}
	masked.AdminToken = model.MaskSecret(v.AdminToken)
	revealed := r.URL.Query().Get("reveal") == "1"
	if revealed {
		// 已通过管理令牌鉴权，允许查看明文（前端需二次确认）
		masked.ClientKeys = v.ClientKeys
		masked.AdminToken = v.AdminToken
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"settings": masked,
		"meta": SettingsMeta{
			ConfigFile: h.cfg.Path(), DataFile: h.cfg.DataFile(), StatsFile: h.store.StatsFile(), Addr: h.cfg.Server.Addr,
			Version: model.Version, Uptime: h.store.UptimeString(),
			RestartNeeded: []string{"监听地址 addr", "数据目录 data_dir", "单次请求体积上限 max_body_mb", "上游连接池参数", "访问日志落盘开关 file_enabled"},
			HotFields:     []string{"网关密钥 / 管理令牌", "来源 IP 解析", "路由策略与重试", "渠道失败冷却", "渠道自动探活", "日志设置", "请求超时"},
		},
		"admin_token_set": rt.AdminToken != "",
		"revealed":        revealed,
	})
}

// updateSettings POST /admin/settings —— 保存并立即生效。
func (h *Handler) updateSettings(w http.ResponseWriter, r *http.Request) {
	var in SettingsView
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "解析失败: "+err.Error())
		return
	}

	old := h.cfg.RT()
	next := old.Clone()

	// 鉴权：空字符串表示不修改（避免页面回显掩码时把密钥清空）
	if in.ClientKeys != nil {
		keys := make([]string, 0, len(in.ClientKeys))
		for _, k := range in.ClientKeys {
			k = strings.TrimSpace(k)
			if k == "" || strings.Contains(k, "*") { // 掩码值视为未修改
				continue
			}
			keys = append(keys, k)
		}
		if len(keys) > 0 {
			next.ClientKeys = keys
		} else if len(in.ClientKeys) > 0 {
			writeErr(w, http.StatusBadRequest, "至少需要保留一个网关密钥（全部填 * 会被忽略）")
			return
		}
	}
	if strings.TrimSpace(in.AdminToken) != "" && !strings.Contains(in.AdminToken, "*") {
		next.AdminToken = strings.TrimSpace(in.AdminToken)
	}
	if in.TrustProxyHeaders != nil {
		next.TrustProxyHeaders = *in.TrustProxyHeaders
	}

	// 路由（留空表示不修改）
	if in.Strategy != "" {
		switch in.Strategy {
		case "priority_round_robin", "round_robin", "failover", "random":
			next.Strategy = in.Strategy
		default:
			writeErr(w, http.StatusBadRequest, "未知路由策略: "+in.Strategy)
			return
		}
	}
	if in.MaxAttempts != 0 {
		if in.MaxAttempts < 1 {
			writeErr(w, http.StatusBadRequest, "max_attempts 至少为 1")
			return
		}
		next.MaxAttempts = in.MaxAttempts
	}
	if in.ForceStreamUsage != nil {
		next.ForceStreamUsage = *in.ForceStreamUsage
	}
	if len(in.RetryStatus) > 0 {
		next.RetryStatus = in.RetryStatus
	}
	// 冷却：仅当提交了该字段才改（旧客户端不提交时保持现状）。
	if in.CooldownSeconds != nil {
		cd := *in.CooldownSeconds
		if cd < 0 {
			writeErr(w, http.StatusBadRequest, "cooldown_seconds 不能为负，0 表示关闭冷却")
			return
		}
		if cd > 3600 {
			writeErr(w, http.StatusBadRequest, "cooldown_seconds 最多 3600（1 小时）")
			return
		}
		next.Cooldown = time.Duration(cd) * time.Second
	}

	// 渠道后台探活
	if in.ProbeEnabled != nil {
		next.ProbeEnabled = *in.ProbeEnabled
	}
	if in.ProbeIntervalSeconds != nil {
		sec := *in.ProbeIntervalSeconds
		if sec < 10 {
			writeErr(w, http.StatusBadRequest, "probe_interval_seconds 至少为 10（避免频繁打扰上游）")
			return
		}
		if sec > 3600 {
			writeErr(w, http.StatusBadRequest, "probe_interval_seconds 最多 3600（1 小时）")
			return
		}
		next.ProbeInterval = time.Duration(sec) * time.Second
	}
	if in.ProbeFailThreshold != 0 {
		if in.ProbeFailThreshold < 1 {
			writeErr(w, http.StatusBadRequest, "probe_fail_threshold 至少为 1")
			return
		}
		if in.ProbeFailThreshold > 20 {
			writeErr(w, http.StatusBadRequest, "probe_fail_threshold 最多 20（阈值过大会让故障渠道迟迟不被剔除）")
			return
		}
		next.ProbeFailThreshold = in.ProbeFailThreshold
	}

	// 日志
	if in.AccessLog != nil {
		next.AccessLog = *in.AccessLog
	}
	if in.KeepLogs != 0 {
		if in.KeepLogs < 10 {
			writeErr(w, http.StatusBadRequest, "keep_logs 至少为 10")
			return
		}
		next.KeepLogs = in.KeepLogs
	}
	if in.RecordPayload != nil {
		next.RecordPayload = *in.RecordPayload
	}
	if in.PayloadLimit != 0 {
		if in.PayloadLimit < 100 {
			writeErr(w, http.StatusBadRequest, "payload_limit 至少为 100")
			return
		}
		if in.PayloadLimit > 200000 {
			writeErr(w, http.StatusBadRequest, "payload_limit 最多为 200000（避免日志体积过大）")
			return
		}
		next.PayloadLimit = in.PayloadLimit
	}
	if in.LogArchive != nil {
		next.LogArchive = *in.LogArchive
	}

	// 超时
	if in.RequestTimeout >= 1 {
		next.RequestTimeout = time.Duration(in.RequestTimeout) * time.Second
	}
	if in.StreamTimeout >= 1 {
		next.StreamTimeout = time.Duration(in.StreamTimeout) * time.Second
	}

	// 界面主题
	switch in.Theme {
	case "":
	case "auto", "dark", "light":
		next.Theme = in.Theme
	default:
		writeErr(w, http.StatusBadRequest, "主题仅支持 auto / dark / light")
		return
	}

	// 应用：内存立即生效 + 落盘
	h.cfg.ApplyRuntime(next)
	h.store.OnRuntimeChanged(next)
	if err := config.Save(h.cfg); err != nil {
		writeErr(w, http.StatusInternalServerError, "已生效但写回配置文件失败: "+err.Error())
		return
	}
	h.log.Info("配置已更新", "keys", len(next.ClientKeys), "strategy", next.Strategy,
		"max_attempts", next.MaxAttempts, "cooldown", next.Cooldown, "keep_logs", next.KeepLogs)

	adminTokenChanged := old.AdminToken != next.AdminToken
	keysChanged := strings.Join(old.ClientKeys, ",") != strings.Join(next.ClientKeys, ",")

	v := viewFromRuntime(next, h.cfg.Logging.Level)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                    true,
		"settings":              v,
		"admin_token":           next.AdminToken,
		"client_keys":           next.ClientKeys,
		"admin_token_new":       adminTokenChanged,
		"client_keys_new":       keysChanged,
		"persist":               h.cfg.Path(),
		"need_restart":          []string{},
		"effective_immediately": []string{"网关密钥", "管理令牌", "路由策略", "重试状态码", "渠道失败冷却", "渠道自动探活", "日志设置", "超时"},
	})
}
