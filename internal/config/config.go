// Package config 负责加载、校验、运行时热更新与持久化网关配置。
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/xieyuhua/apitoken/internal/model"
	"github.com/xieyuhua/apitoken/internal/util"
)

// Runtime 运行时可热更新的配置集合。修改后无需重启即刻生效。
type Runtime struct {
	// 鉴权
	ClientKeys []string
	AdminToken string
	// TrustProxyHeaders 是否信任反向代理头（X-Forwarded-For / X-Real-IP）作为来源 IP。
	TrustProxyHeaders bool

	// 路由
	Strategy         string
	MaxAttempts      int
	RetryStatus      []int
	ForceStreamUsage bool
	// Cooldown 渠道失败后的冷却时长；冷却期内该渠道不再进入候选列表，
	// 避免「第一个渠道刚失败，下一轮会话又第一个打到它」。
	Cooldown time.Duration

	// ProbeEnabled / ProbeInterval / ProbeFailThreshold 控制渠道后台自动探活：
	// 关闭时不产生任何额外的上游请求，渠道健康状态只由真实请求与手动检测更新。
	ProbeEnabled       bool
	ProbeInterval      time.Duration
	ProbeFailThreshold int

	// 日志
	AccessLog     bool
	KeepLogs      int
	RecordPayload bool
	PayloadLimit  int
	// LogArchive 调用日志页的默认数据源：false = 内存热缓冲（快，仅最近 keep_logs 条）；
	// true = 磁盘归档（能查更早的历史，但要读文件，单次上限 2000 条）。
	LogArchive bool

	// 超时
	RequestTimeout time.Duration
	StreamTimeout  time.Duration
	MaxBodyBytes   int64
	ConnectTimeout time.Duration
	DefaultTimeout time.Duration

	// 出网
	ProxyURL string

	// 界面
	Theme string // auto | dark | light
}

// Clone 深拷贝运行时配置。
func (r *Runtime) Clone() *Runtime {
	if r == nil {
		return &Runtime{}
	}
	cp := *r
	cp.ClientKeys = append([]string(nil), r.ClientKeys...)
	cp.RetryStatus = append([]int(nil), r.RetryStatus...)
	return &cp
}

// UIConfig 界面外观配置。
type UIConfig struct {
	Theme string `yaml:"theme"`
}

// Config 网关总配置。
type Config struct {
	UI       UIConfig        `yaml:"ui"`
	Server   ServerConfig    `yaml:"server"`
	Security SecurityConfig  `yaml:"security"`
	Routing  RoutingConfig   `yaml:"routing"`
	Logging  LoggingConfig   `yaml:"logging"`
	Upstream UpstreamConfig  `yaml:"upstream"`
	Routes   []model.Route   `yaml:"routes"`
	Channels []model.Channel `yaml:"channels"`

	path    string
	once    sync.Once
	runtime atomic.Pointer[Runtime]
}

// ServerConfig 服务监听相关配置。
type ServerConfig struct {
	Addr           string   `yaml:"addr"`
	RequestTimeout Duration `yaml:"request_timeout"`
	StreamTimeout  Duration `yaml:"stream_timeout"`
	ReadTimeout    Duration `yaml:"read_timeout"`
	WriteTimeout   Duration `yaml:"write_timeout"`
	MaxBodyMB      int      `yaml:"max_body_mb"`
	DataDir        string   `yaml:"data_dir"`
}

// SecurityConfig 鉴权配置。
type SecurityConfig struct {
	ClientKeys []string `yaml:"client_keys"`
	AdminToken string   `yaml:"admin_token"`
	// TrustProxyHeaders 是否信任反向代理头（X-Forwarded-For / X-Real-IP）作为来源 IP。
	// nil = 未配置（默认信任，兼容旧行为）；显式 false = 只取直连对端地址，
	// 防止网关直接暴露公网时客户端伪造来源 IP。
	TrustProxyHeaders *bool `yaml:"trust_proxy_headers"`
}

// RoutingConfig 路由策略配置。
type RoutingConfig struct {
	Strategy         string `yaml:"strategy"`
	MaxAttempts      int    `yaml:"max_attempts"`
	ForceStreamUsage bool   `yaml:"force_stream_usage"`
	RetryStatus      []int  `yaml:"retry_status"`
	// Cooldown 渠道失败后的冷却时长。nil = 未配置（用默认值）；
	// 0 = 显式关闭冷却（恢复「每轮都从第一个渠道开始」的旧行为）。
	Cooldown *Duration `yaml:"cooldown"`
	// ProbeEnabled 是否开启渠道后台自动探活（按间隔探测所有启用渠道，连续失败即进冷却）。
	// nil = 未配置（默认开启）；显式 false = 关闭（此时只在真实请求失败时才冷却）。
	ProbeEnabled *bool `yaml:"probe_enabled"`
	// ProbeInterval 探活间隔。nil = 未配置（默认 60s）；过短会给上游增加额外请求负担。
	ProbeInterval *Duration `yaml:"probe_interval"`
	// ProbeFailThreshold 连续探活失败多少次才进冷却；<=0 取默认值 3。
	ProbeFailThreshold int `yaml:"probe_fail_threshold"`
}

// LoggingConfig 日志配置。
type LoggingConfig struct {
	Level         string `yaml:"level"`
	AccessLog     bool   `yaml:"access_log"`
	KeepLogs      int    `yaml:"keep_logs"`
	RecordPayload bool   `yaml:"record_payload"`
	PayloadLimit  int    `yaml:"payload_limit"`
	// LogArchive 调用日志页的默认数据源：true = 查磁盘归档，false = 查内存热缓冲
	LogArchive bool `yaml:"log_archive"`

	// 以下几项控制访问日志的异步落盘，改后需重启生效（涉及文件句柄）。
	// 落盘由后台协程完成，请求路径只做一次非阻塞入队，不受磁盘速度影响。
	// FileEnabled 为指针：nil = 未配置（默认开启，日志重启不丢）；
	// 显式 false = 纯内存模式（重启即清空）。
	FileEnabled *bool  `yaml:"file_enabled"`
	FileDir     string `yaml:"file_dir"`    // 空 = <data_dir>/logs
	FileMaxMB   int    `yaml:"file_max_mb"` // 单个分段文件大小上限
	FileKeep    int    `yaml:"file_keep"`   // 保留的最近分段文件数
}

// UpstreamConfig 上游 HTTP 客户端配置。
type UpstreamConfig struct {
	DefaultTimeout Duration `yaml:"default_timeout"`
	ConnectTimeout Duration `yaml:"connect_timeout"`
	KeepAlive      Duration `yaml:"keep_alive"`
	MaxIdleConns   int      `yaml:"max_idle_conns"`
	// MaxIdleConnsPerHost 单个上游的空闲连接上限；<=0 时等于 MaxIdleConns。
	// 流式请求会长时间独占连接，该值偏小会导致连接反复新建（TCP + TLS 握手开销）。
	MaxIdleConnsPerHost int    `yaml:"max_idle_conns_per_host"`
	ProxyURL            string `yaml:"proxy_url"`
}

// RT 返回当前生效的运行时配置（并发安全）。
// 若 Config 是直接构造而非 Load 而来，这里按字段懒初始化一份默认值。
func (c *Config) RT() *Runtime {
	c.once.Do(func() {
		if c.runtime.Load() == nil {
			c.runtime.Store(c.runtimeFromFields())
		}
	})
	if rt := c.runtime.Load(); rt != nil {
		return rt
	}
	return &Runtime{}
}

// ApplyRuntime 原子替换运行时配置（保存即生效）。
func (c *Config) ApplyRuntime(rt *Runtime) { c.runtime.Store(rt.Clone()) }

// DataFile 渠道/路由持久化文件路径。
func (c *Config) DataFile() string {
	dir := c.Server.DataDir
	if dir == "" {
		dir = "./data"
	}
	return filepath.Join(dir, "gateway.json")
}

// Path 配置文件路径。
func (c *Config) Path() string { return c.path }

// Load 读取配置文件并填充默认值。
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置失败 %s: %w", path, err)
	}
	cfg := &Config{path: path}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("解析配置失败 %s: %w", path, err)
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	cfg.ApplyRuntime(cfg.runtimeFromFields())
	return cfg, nil
}

// runtimeFromFields 从配置字段构造运行时配置。
func (c *Config) runtimeFromFields() *Runtime {
	rt := &Runtime{
		ClientKeys:        append([]string(nil), c.Security.ClientKeys...),
		AdminToken:        c.Security.AdminToken,
		TrustProxyHeaders: true, // 默认信任（兼容旧行为），下方按显式配置覆盖
		Strategy:          c.Routing.Strategy,
		MaxAttempts:       c.Routing.MaxAttempts,
		RetryStatus:       append([]int(nil), c.Routing.RetryStatus...),
		ForceStreamUsage:  c.Routing.ForceStreamUsage,
		// Cooldown 在下方按「是否显式配置」单独处理（nil 与 0 语义不同）
		AccessLog:      c.Logging.AccessLog,
		KeepLogs:       c.Logging.KeepLogs,
		RecordPayload:  c.Logging.RecordPayload,
		PayloadLimit:   c.Logging.PayloadLimit,
		LogArchive:     c.Logging.LogArchive,
		RequestTimeout: c.Server.RequestTimeout.D(),
		StreamTimeout:  c.Server.StreamTimeout.D(),
		MaxBodyBytes:   int64(c.Server.MaxBodyMB) << 20,
		ConnectTimeout: c.Upstream.ConnectTimeout.D(),
		DefaultTimeout: c.Upstream.DefaultTimeout.D(),
		ProxyURL:       c.Upstream.ProxyURL,
		Theme:          c.UI.Theme,
	}
	if rt.MaxAttempts <= 0 {
		rt.MaxAttempts = 3
	}
	if rt.KeepLogs <= 0 {
		rt.KeepLogs = DefaultKeepLogs
	}
	if rt.PayloadLimit <= 0 {
		rt.PayloadLimit = DefaultPayloadLimit
	}
	if rt.MaxBodyBytes <= 0 {
		rt.MaxBodyBytes = 32 << 20
	}
	if rt.Strategy == "" {
		rt.Strategy = StrategyPriorityRoundRobin
	}
	// 是否信任反向代理头：未配置时默认信任（兼容旧行为），显式 false 关闭
	if c.Security.TrustProxyHeaders != nil {
		rt.TrustProxyHeaders = *c.Security.TrustProxyHeaders
	}
	// 未显式配置冷却时给一个保守默认值；显式配 0 表示关闭冷却（保留旧行为）。
	if c.Routing.Cooldown != nil {
		rt.Cooldown = c.Routing.Cooldown.D()
	} else {
		rt.Cooldown = DefaultCooldown
	}
	// 探活三项：未配置时取默认值
	rt.ProbeEnabled = DefaultProbeEnabled
	if c.Routing.ProbeEnabled != nil {
		rt.ProbeEnabled = *c.Routing.ProbeEnabled
	}
	rt.ProbeInterval = DefaultProbeInterval
	if c.Routing.ProbeInterval != nil {
		rt.ProbeInterval = c.Routing.ProbeInterval.D()
	}
	rt.ProbeFailThreshold = DefaultProbeFailThreshold
	if c.Routing.ProbeFailThreshold > 0 {
		rt.ProbeFailThreshold = c.Routing.ProbeFailThreshold
	}
	return rt
}

// syncFields 把运行时配置写回配置字段（供持久化使用）。
func (c *Config) syncFields(rt *Runtime) {
	c.Security.ClientKeys = append([]string(nil), rt.ClientKeys...)
	c.Security.AdminToken = rt.AdminToken
	c.Security.TrustProxyHeaders = boolPtr(rt.TrustProxyHeaders)
	c.Routing.Strategy = rt.Strategy
	c.Routing.MaxAttempts = rt.MaxAttempts
	c.Routing.RetryStatus = append([]int(nil), rt.RetryStatus...)
	c.Routing.ForceStreamUsage = rt.ForceStreamUsage
	c.Routing.Cooldown = durationPtr(rt.Cooldown)
	c.Routing.ProbeEnabled = boolPtr(rt.ProbeEnabled)
	c.Routing.ProbeInterval = durationPtr(rt.ProbeInterval)
	c.Routing.ProbeFailThreshold = rt.ProbeFailThreshold
	c.Logging.AccessLog = rt.AccessLog
	c.Logging.KeepLogs = rt.KeepLogs
	c.Logging.RecordPayload = rt.RecordPayload
	c.Logging.PayloadLimit = rt.PayloadLimit
	c.Logging.LogArchive = rt.LogArchive
	c.Server.RequestTimeout = Duration(rt.RequestTimeout)
	c.Server.StreamTimeout = Duration(rt.StreamTimeout)
	c.Upstream.ProxyURL = rt.ProxyURL
}

// LogFileEnabled 访问日志是否落盘（nil 视为开启）。
func (c *Config) LogFileEnabled() bool {
	return c.Logging.FileEnabled == nil || *c.Logging.FileEnabled
}

func (c *Config) applyDefaults() {
	if c.Server.Addr == "" {
		c.Server.Addr = ":8080"
	}
	if c.Server.RequestTimeout == 0 {
		c.Server.RequestTimeout = Duration(300 * time.Second)
	}
	if c.Server.StreamTimeout == 0 {
		c.Server.StreamTimeout = Duration(900 * time.Second)
	}
	if c.Server.ReadTimeout == 0 {
		c.Server.ReadTimeout = Duration(60 * time.Second)
	}
	if c.Server.MaxBodyMB <= 0 {
		c.Server.MaxBodyMB = 32
	}
	if c.Server.DataDir == "" {
		c.Server.DataDir = "./data"
	}
	if !IsValidStrategy(c.Routing.Strategy) {
		c.Routing.Strategy = StrategyPriorityRoundRobin
	}
	if c.Routing.MaxAttempts <= 0 {
		c.Routing.MaxAttempts = 3
	}
	if len(c.Routing.RetryStatus) == 0 {
		c.Routing.RetryStatus = []int{408, 409, 425, 429, 500, 502, 503, 504, 529}
	}
	if c.Logging.Level == "" {
		c.Logging.Level = "info"
	}
	if c.Logging.KeepLogs <= 0 {
		c.Logging.KeepLogs = DefaultKeepLogs
	}
	if c.Logging.PayloadLimit <= 0 {
		c.Logging.PayloadLimit = DefaultPayloadLimit
	}
	if c.Logging.FileMaxMB <= 0 {
		c.Logging.FileMaxMB = DefaultLogFileMaxMB
	}
	if c.Logging.FileKeep <= 0 {
		c.Logging.FileKeep = DefaultLogFileKeep
	}
	if c.Upstream.DefaultTimeout == 0 {
		c.Upstream.DefaultTimeout = Duration(120 * time.Second)
	}
	if c.Upstream.ConnectTimeout == 0 {
		c.Upstream.ConnectTimeout = Duration(10 * time.Second)
	}
	if c.Upstream.KeepAlive == 0 {
		c.Upstream.KeepAlive = Duration(30 * time.Second)
	}
	if c.Upstream.MaxIdleConns <= 0 {
		c.Upstream.MaxIdleConns = 200
	}
	switch {
	case IsValidTheme(c.UI.Theme):
	default:
		c.UI.Theme = ThemeAuto
	}
	for i := range c.Channels {
		if c.Channels[i].Weight <= 0 {
			c.Channels[i].Weight = 1
		}
	}
}

func durationPtr(d time.Duration) *Duration {
	v := Duration(d)
	return &v
}

func boolPtr(b bool) *bool { return util.BoolPtr(b) }

// DefaultPayloadLimit 单条日志每侧保存的摘要字符数上限。
// 2000 偏小（长回复常被腰斩），提高到 8000；仍可通过设置页热更新调整。
const DefaultPayloadLimit = 8000

// DefaultKeepLogs 内存中保留的访问日志条数（页面查询只读这部分热数据）。
const DefaultKeepLogs = 1000

// DefaultCooldown 渠道失败后的默认冷却时长：该渠道在冷却期内不再进入候选列表，
// 避免刚失败的渠道在下一轮请求里又被第一个尝试。设为 0 可关闭。
const DefaultCooldown = 60 * time.Second

// 渠道后台自动探活的默认值。
// 默认开启：流量低谷时渠道挂掉也能被发现，提前移出候选列表；
// ProbeFailThreshold 要求「连续」失败才冷却，用于过滤单次网络抖动、避免误伤健康渠道。
const (
	DefaultProbeEnabled       = true
	DefaultProbeInterval      = 60 * time.Second
	DefaultProbeFailThreshold = 3
)

// 访问日志落盘默认值。
const (
	// DefaultLogFileMaxMB 单个日志分段文件的大小上限，超过则滚动到新文件。
	DefaultLogFileMaxMB = 64
	// DefaultLogFileKeep 保留的最近分段文件数，更早的自动删除。
	DefaultLogFileKeep = 7
)

func (c *Config) validate() error {
	ids := map[string]bool{}
	for i, ch := range c.Channels {
		if ch.ID == "" {
			return fmt.Errorf("channels[%d] 缺少 id", i)
		}
		if ids[ch.ID] {
			return fmt.Errorf("channels id 重复: %s", ch.ID)
		}
		ids[ch.ID] = true
		if ch.BaseURL == "" {
			return fmt.Errorf("渠道 %s 缺少 base_url", ch.ID)
		}
	}
	for i, r := range c.Routes {
		if r.Model == "" {
			return fmt.Errorf("routes[%d] 缺少 model", i)
		}
	}
	if err := os.MkdirAll(c.Server.DataDir, 0o755); err != nil {
		return fmt.Errorf("创建数据目录失败: %w", err)
	}
	return nil
}

// 配置的持久化与 YAML 增量合并见 writer.go。
