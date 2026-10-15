// Package model 定义网关核心数据结构（渠道、路由、统计、日志）。
package model

import (
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Version 网关版本号。
const Version = "1.0.0"

// channelIDSeq 渠道 ID 生成的进程内自增序号（配合秒级时间戳保证同秒唯一）。
var channelIDSeq atomic.Uint64

// NewChannelID 由名称生成渠道 ID：小写字母/数字保留，空格、-、_ 归一为 -，
// 截断到 20 字符，追加 HHMMSS 时间戳后缀与自增序号保证唯一；名称不含有效字符时退回 "channel"。
// 管理端新增渠道与配置文件播种渠道共用这一套规则，避免两处行为分叉。
func NewChannelID(name string) string {
	base := make([]rune, 0, 20)
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			base = append(base, r)
		case r == ' ' || r == '-' || r == '_':
			base = append(base, '-')
		}
		if len(base) >= 20 {
			break
		}
	}
	id := strings.Trim(string(base), "-")
	if id == "" {
		id = "channel"
	}
	// 秒级时间戳 + 进程内自增序号：同一秒内批量创建同名渠道也不会撞 ID
	return id + "-" + time.Now().Format("150405") + "-" + strconv.FormatUint(channelIDSeq.Add(1), 10)
}

// Channel 一个上游账号 / 服务节点。
type Channel struct {
	ID      string `json:"id" yaml:"id"`
	Name    string `json:"name" yaml:"name"`
	BaseURL string `json:"base_url" yaml:"base_url"`
	APIKey  string `json:"api_key" yaml:"api_key"`

	// 鉴权方式：bearer（默认，Authorization: Bearer <key>）、header（自定义头直接放 key）、query（?key=xxx）
	AuthStyle  string `json:"auth_style,omitempty" yaml:"auth_style,omitempty"`
	AuthHeader string `json:"auth_header,omitempty" yaml:"auth_header,omitempty"`
	AuthPrefix string `json:"auth_prefix,omitempty" yaml:"auth_prefix,omitempty"`

	// Models 该渠道支持的模型名；包含 "*" 表示放通全部模型
	Models []string `json:"models" yaml:"models"`
	// Alias 客户端模型名 -> 上游真实模型名
	Alias map[string]string `json:"alias,omitempty" yaml:"alias,omitempty"`

	Weight   int  `json:"weight" yaml:"weight"`     // 同一优先级内的权重
	Priority int  `json:"priority" yaml:"priority"` // 数值越小优先级越高
	Enabled  bool `json:"enabled" yaml:"enabled"`

	TimeoutSec int `json:"timeout_sec,omitempty" yaml:"timeout_sec,omitempty"`
	// ProxyURL 该渠道专用的上游代理：留空=跟随全局 proxy_url / 环境变量；none 或 direct=强制直连；否则为代理地址。
	ProxyURL     string            `json:"proxy_url,omitempty" yaml:"proxy_url,omitempty"`
	ChatPath     string            `json:"chat_path,omitempty" yaml:"chat_path,omitempty"`
	ExtraHeaders map[string]string `json:"extra_headers,omitempty" yaml:"extra_headers,omitempty"`
	ExtraParams  map[string]any    `json:"extra_params,omitempty" yaml:"extra_params,omitempty"`
	Note         string            `json:"note,omitempty" yaml:"note,omitempty"`

	CreatedAt time.Time `json:"created_at" yaml:"created_at"`
	UpdatedAt time.Time `json:"updated_at" yaml:"updated_at"`

	Stats Stats `json:"stats" yaml:"-"`
}

// Supports 判断渠道是否支持该客户端模型名，返回应发往上游的模型名。
// models 留空（或含 "*"）表示放通全部模型名。
func (c Channel) Supports(name string) (string, bool) {
	if !c.Enabled {
		return "", false
	}
	for _, m := range c.Models {
		if m == "*" || strings.EqualFold(m, name) {
			return name, true
		}
	}
	if up, ok := c.Alias[name]; ok {
		return up, true
	}
	if len(c.Models) == 0 && len(c.Alias) == 0 {
		return name, true
	}
	return "", false
}

// Masked 返回隐藏密钥后的副本，用于管理接口输出。
func (c Channel) Masked() Channel {
	c.APIKey = MaskSecret(c.APIKey)
	return c
}

// MaskSecret 脱敏展示密钥：固定输出宽度（前 4 + 4 星号 + 后 4）。
// 不按原长度补星号，避免长密钥脱敏后依然撑宽管理页表格，同时也隐蔽了密钥长度。
//
// 按 rune 而非 byte 切分：按字节切会在中文这类多字节字符中间下刀，
// 产出半个码点，管理页展示成乱码（U+FFFD），导出的 JSON 也是非法 UTF-8。
func MaskSecret(s string) string {
	if s == "" {
		return ""
	}
	rs := []rune(s)
	if len(rs) <= 8 {
		return strings.Repeat("*", len(rs))
	}
	return string(rs[:4]) + "****" + string(rs[len(rs)-4:])
}

// Route 显式路由：一个对外模型名 -> 有序渠道列表（顺序即故障转移顺序）。
type Route struct {
	Model    string            `json:"model" yaml:"model"`
	Channels []string          `json:"channels" yaml:"channels"`
	ModelMap map[string]string `json:"model_map,omitempty" yaml:"model_map,omitempty"`
	// Enabled 缺省视为启用
	Enabled *bool  `json:"enabled,omitempty" yaml:"enabled"`
	Note    string `json:"note,omitempty" yaml:"note,omitempty"`
}

// IsEnabled 路由是否启用。
func (r Route) IsEnabled() bool { return r.Enabled == nil || *r.Enabled }

// Candidate 一次请求的候选目标。
type Candidate struct {
	Channel       Channel `json:"-"`
	ID            string  `json:"id"`
	Name          string  `json:"name"`
	UpstreamModel string  `json:"upstream_model"`
}

// Stats 渠道维度的累计统计。
type Stats struct {
	Requests         int64     `json:"requests"`
	Success          int64     `json:"success"`
	Failed           int64     `json:"failed"`
	PromptTokens     int64     `json:"prompt_tokens"`
	CompletionTokens int64     `json:"completion_tokens"`
	TotalLatencyMS   int64     `json:"total_latency_ms"`
	LastUsed         time.Time `json:"last_used,omitempty"`
	LastError        string    `json:"last_error,omitempty"`
}

// ModelUsage 模型维度统计。
type ModelUsage struct {
	Requests         int64 `json:"requests"`
	Success          int64 `json:"success"`
	Failed           int64 `json:"failed"`
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalLatencyMS   int64 `json:"total_latency_ms"`
}

// HourlyBucket 按小时聚合的统计桶（用于控制台趋势图）。
// TS 为该小时起点的 Unix 秒；缺失的小时前端会得到全 0 的桶，保证折线连续。
type HourlyBucket struct {
	TS               int64 `json:"ts"`
	Requests         int64 `json:"requests"`
	Success          int64 `json:"success"`
	Failed           int64 `json:"failed"`
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalLatencyMS   int64 `json:"total_latency_ms"`
}

// LogEntry 访问日志条目。
type LogEntry struct {
	Time             time.Time `json:"time"`
	RequestID        string    `json:"request_id"`
	Method           string    `json:"method"`
	Path             string    `json:"path"`
	ClientIP         string    `json:"client_ip"`
	UserAgent        string    `json:"user_agent,omitempty"`
	Model            string    `json:"model"`
	UpstreamModel    string    `json:"upstream_model,omitempty"`
	ChannelID        string    `json:"channel_id,omitempty"`
	ChannelName      string    `json:"channel_name,omitempty"`
	Stream           bool      `json:"stream"`
	Status           int       `json:"status"`
	LatencyMS        int64     `json:"latency_ms"`
	PromptTokens     int64     `json:"prompt_tokens"`
	CompletionTokens int64     `json:"completion_tokens"`
	Attempts         int       `json:"attempts"`
	Error            string    `json:"error,omitempty"`

	// 以下字段仅在 logging.record_payload = true 时填充
	Endpoint        string `json:"endpoint,omitempty"`
	RequestSnippet  string `json:"request_snippet,omitempty"`
	ResponseSnippet string `json:"response_snippet,omitempty"`

	// RequestSnippetFull / ResponseSnippetFull 为截断前的原始字符数；
	// 0 表示未截断。前端据此提示"内容已按 payload_limit 截断"，避免误以为展示不全。
	RequestSnippetFull  int `json:"request_snippet_full,omitempty"`
	ResponseSnippetFull int `json:"response_snippet_full,omitempty"`

	// ResponseCapped 表示流式回复超过采集上限：日志只保存了开头一部分，
	// 完整长度未知（网关不缓存完整回复）。与 ResponseSnippetFull 语义不同。
	ResponseCapped bool `json:"response_capped,omitempty"`
}

// SnippetTruncated 是否存在被截断的摘要。
func (l LogEntry) SnippetTruncated() bool {
	return l.RequestSnippetFull > 0 || l.ResponseSnippetFull > 0 || l.ResponseCapped
}

// OK 是否成功。
func (l LogEntry) OK() bool { return l.Status >= 200 && l.Status < 400 }

// LogQuery 日志查询条件。
type LogQuery struct {
	Model     string
	Channel   string
	RequestID string
	Keyword   string
	Status    string // success | error | 具体状态码
	Stream    string // 1 | 0
	Page      int
	PageSize  int
	// From / To 时间范围（闭区间），零值表示不限。
	// 磁盘归档查询用它划定范围，避免每次都全量扫描所有分段文件。
	From time.Time
	To   time.Time
}

// LogPage 分页结果。
type LogPage struct {
	Items    []LogEntry `json:"items"`
	Total    int        `json:"total"`
	Page     int        `json:"page"`
	PageSize int        `json:"page_size"`
	Pages    int        `json:"pages"`
	Kept     int        `json:"kept"` // 内存中保留的总条数
	Stats    LogSummary `json:"stats"`
}

// LogSummary 日志聚合统计。
type LogSummary struct {
	Total            int64   `json:"total"`
	Success          int64   `json:"success"`
	Failed           int64   `json:"failed"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	AvgLatencyMS     int64   `json:"avg_latency_ms"`
	P95LatencyMS     int64   `json:"p95_latency_ms"`
	FailoverCount    int64   `json:"failover_count"`
	ByModel          []LogKV `json:"by_model"`
	ByChannel        []LogKV `json:"by_channel"`
	ByStatus         []LogKV `json:"by_status"`
	TopErrors        []LogKV `json:"top_errors"`
}

// LogKV 聚合项。
type LogKV struct {
	Name             string `json:"name"`
	Count            int64  `json:"count"`
	Failed           int64  `json:"failed"`
	TotalLatencyMS   int64  `json:"total_latency_ms"`
	AvgLatencyMS     int64  `json:"avg_latency_ms"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
}

// Snapshot 全局统计快照。
type Snapshot struct {
	Version          string                 `json:"version"`
	UptimeSeconds    int64                  `json:"uptime_seconds"`
	StartedAt        time.Time              `json:"started_at"`
	TotalRequests    int64                  `json:"total_requests"`
	TotalSuccess     int64                  `json:"total_success"`
	TotalFailed      int64                  `json:"total_failed"`
	PromptTokens     int64                  `json:"prompt_tokens"`
	CompletionTokens int64                  `json:"completion_tokens"`
	AvgLatencyMS     int64                  `json:"avg_latency_ms"`
	ChannelCount     int                    `json:"channel_count"`
	EnabledChannels  int                    `json:"enabled_channels"`
	Models           map[string]*ModelUsage `json:"models"`
	Channels         []ChannelStat          `json:"channels"`
}

// ChannelStat 管理界面展示的渠道状态。
type ChannelStat struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	BaseURL       string    `json:"base_url"`
	Enabled       bool      `json:"enabled"`
	Weight        int       `json:"weight"`
	Priority      int       `json:"priority"`
	Models        []string  `json:"models"`
	MaskedKey     string    `json:"api_key"`
	Health        string    `json:"health"`
	LastCheckedAt time.Time `json:"last_checked_at,omitempty"`
	LastError     string    `json:"last_error,omitempty"`
	LastUsed      time.Time `json:"last_used,omitempty"`
	// CooldownRemainingMS 失败冷却剩余毫秒数；>0 表示该渠道暂时不参与候选
	CooldownRemainingMS int64 `json:"cooldown_remaining_ms,omitempty"`
	Stats               Stats `json:"stats"`
}
