// Package config 负责加载、校验、运行时热更新与持久化网关配置。
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/demo1/apitoken/internal/model"
)

// Duration 支持 YAML 中 "300s" / "2m" 写法。
type Duration time.Duration

// UnmarshalYAML 解析字符串时长。
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err == nil {
		v, err := time.ParseDuration(strings.TrimSpace(s))
		if err != nil {
			return fmt.Errorf("非法时长 %q: %w", s, err)
		}
		*d = Duration(v)
		return nil
	}
	var ms int64
	if err := value.Decode(&ms); err != nil {
		return fmt.Errorf("非法时长: %w", err)
	}
	*d = Duration(time.Duration(ms) * time.Millisecond)
	return nil
}

// D 转换为 time.Duration。
func (d Duration) D() time.Duration { return time.Duration(d) }

// MarshalYAML 序列化为 "300s" 这类可读字符串，避免写出纳秒整数。
func (d Duration) MarshalYAML() (any, error) {
	return time.Duration(d).String(), nil
}

// Runtime 运行时可热更新的配置集合。修改后无需重启即刻生效。
type Runtime struct {
	// 鉴权
	ClientKeys []string
	AdminToken string

	// 路由
	Strategy         string
	MaxAttempts      int
	RetryStatus      []int
	ForceStreamUsage bool

	// 日志
	AccessLog     bool
	KeepLogs      int
	RecordPayload bool
	PayloadLimit  int

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
}

// RoutingConfig 路由策略配置。
type RoutingConfig struct {
	Strategy         string `yaml:"strategy"`
	MaxAttempts      int    `yaml:"max_attempts"`
	ForceStreamUsage bool   `yaml:"force_stream_usage"`
	RetryStatus      []int  `yaml:"retry_status"`
}

// LoggingConfig 日志配置。
type LoggingConfig struct {
	Level         string `yaml:"level"`
	AccessLog     bool   `yaml:"access_log"`
	KeepLogs      int    `yaml:"keep_logs"`
	RecordPayload bool   `yaml:"record_payload"`
	PayloadLimit  int    `yaml:"payload_limit"`

	// 以下几项控制访问日志的异步落盘，改后需重启生效（涉及文件句柄）。
	// 落盘由后台协程完成，请求路径只做一次非阻塞入队，不受磁盘速度影响。
	FileEnabled bool   `yaml:"file_enabled"`
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
		ClientKeys:       append([]string(nil), c.Security.ClientKeys...),
		AdminToken:       c.Security.AdminToken,
		Strategy:         c.Routing.Strategy,
		MaxAttempts:      c.Routing.MaxAttempts,
		RetryStatus:      append([]int(nil), c.Routing.RetryStatus...),
		ForceStreamUsage: c.Routing.ForceStreamUsage,
		AccessLog:        c.Logging.AccessLog,
		KeepLogs:         c.Logging.KeepLogs,
		RecordPayload:    c.Logging.RecordPayload,
		PayloadLimit:     c.Logging.PayloadLimit,
		RequestTimeout:   c.Server.RequestTimeout.D(),
		StreamTimeout:    c.Server.StreamTimeout.D(),
		MaxBodyBytes:     int64(c.Server.MaxBodyMB) << 20,
		ConnectTimeout:   c.Upstream.ConnectTimeout.D(),
		DefaultTimeout:   c.Upstream.DefaultTimeout.D(),
		ProxyURL:         c.Upstream.ProxyURL,
		Theme:            c.UI.Theme,
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
		rt.Strategy = "priority_round_robin"
	}
	return rt
}

// syncFields 把运行时配置写回配置字段（供持久化使用）。
func (c *Config) syncFields(rt *Runtime) {
	c.Security.ClientKeys = append([]string(nil), rt.ClientKeys...)
	c.Security.AdminToken = rt.AdminToken
	c.Routing.Strategy = rt.Strategy
	c.Routing.MaxAttempts = rt.MaxAttempts
	c.Routing.RetryStatus = append([]int(nil), rt.RetryStatus...)
	c.Routing.ForceStreamUsage = rt.ForceStreamUsage
	c.Logging.AccessLog = rt.AccessLog
	c.Logging.KeepLogs = rt.KeepLogs
	c.Logging.RecordPayload = rt.RecordPayload
	c.Logging.PayloadLimit = rt.PayloadLimit
	c.Server.RequestTimeout = Duration(rt.RequestTimeout)
	c.Server.StreamTimeout = Duration(rt.StreamTimeout)
	c.Upstream.ProxyURL = rt.ProxyURL
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
	if c.Routing.Strategy == "" {
		c.Routing.Strategy = "priority_round_robin"
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
	switch c.UI.Theme {
	case "dark", "light", "auto":
	default:
		c.UI.Theme = "auto"
	}
	for i := range c.Channels {
		if c.Channels[i].Weight <= 0 {
			c.Channels[i].Weight = 1
		}
	}
}

// DefaultPayloadLimit 单条日志每侧保存的摘要字符数上限。
// 2000 偏小（长回复常被腰斩），提高到 8000；仍可通过设置页热更新调整。
const DefaultPayloadLimit = 8000

// DefaultKeepLogs 内存中保留的访问日志条数（页面查询只读这部分热数据）。
const DefaultKeepLogs = 1000

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

// saveView 持久化到 config.yaml 的部分（渠道与路由由 data/gateway.json 管理，不在此覆盖）。
type saveView struct {
	Server   ServerConfig   `yaml:"server"`
	UI       UIConfig       `yaml:"ui"`
	Security SecurityConfig `yaml:"security"`
	Routing  RoutingConfig  `yaml:"routing"`
	Logging  LoggingConfig  `yaml:"logging"`
	Upstream UpstreamConfig `yaml:"upstream"`
}

// Save 把运行时配置写回 config.yaml。
// 采用 yaml.Node 增量合并，尽量保留原文件中的注释与未涉及的字段。
func Save(c *Config) error {
	if c.path == "" {
		return fmt.Errorf("配置路径未知，无法保存")
	}
	rt := c.RT()
	view := saveView{
		Server:   c.Server,
		UI:       UIConfig{Theme: rt.Theme},
		Security: SecurityConfig{ClientKeys: append([]string(nil), rt.ClientKeys...), AdminToken: rt.AdminToken},
		Routing: RoutingConfig{
			Strategy: rt.Strategy, MaxAttempts: rt.MaxAttempts,
			ForceStreamUsage: rt.ForceStreamUsage, RetryStatus: append([]int(nil), rt.RetryStatus...),
		},
		Logging: LoggingConfig{
			Level: c.Logging.Level, AccessLog: rt.AccessLog, KeepLogs: rt.KeepLogs,
			RecordPayload: rt.RecordPayload, PayloadLimit: rt.PayloadLimit,
		},
		Upstream: c.Upstream,
	}
	view.Upstream.ProxyURL = rt.ProxyURL
	view.Server.RequestTimeout = Duration(rt.RequestTimeout)
	view.Server.StreamTimeout = Duration(rt.StreamTimeout)

	oldData, err := os.ReadFile(c.path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("读取原配置失败: %w", err)
	}

	newNode := &yaml.Node{}
	if err := nodeFrom(view, newNode); err != nil {
		return err
	}

	merged := newNode
	if len(oldData) > 0 {
		oldNode := &yaml.Node{}
		if err := yaml.Unmarshal(oldData, oldNode); err != nil {
			// 原文件解析失败时直接覆盖
			oldNode = nil
		} else {
			merged = mergeNode(oldNode, newNode)
		}
	}

	out, err := marshalNode(merged)
	if err != nil {
		return err
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, c.path)
}

func nodeFrom(v any, out *yaml.Node) error {
	b, err := yaml.Marshal(v)
	if err != nil {
		return err
	}
	return yaml.Unmarshal(b, out)
}

func marshalNode(n *yaml.Node) ([]byte, error) {
	var sb strings.Builder
	enc := yaml.NewEncoder(&sb)
	enc.SetIndent(2)
	if err := enc.Encode(n); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return []byte(sb.String()), nil
}

// mergeNode 以 old 为基底，用 new 覆盖同名标量/集合，并保留 old 的注释。
func mergeNode(old, new *yaml.Node) *yaml.Node {
	if old == nil {
		return new
	}
	if new == nil {
		return old
	}
	// 解开 document 节点
	if old.Kind == yaml.DocumentNode && len(old.Content) > 0 {
		old = old.Content[0]
	}
	if new.Kind == yaml.DocumentNode && len(new.Content) > 0 {
		new = new.Content[0]
	}
	if old.Kind != yaml.MappingNode || new.Kind != yaml.MappingNode {
		return new
	}
	for i := 0; i+1 < len(new.Content); i += 2 {
		key := new.Content[i]
		val := new.Content[i+1]
		found := false
		for j := 0; j+1 < len(old.Content); j += 2 {
			if old.Content[j].Value == key.Value {
				oldVal := old.Content[j+1]
				merged := mergeValue(oldVal, val)
				// 继承原注释
				merged.HeadComment = firstNonEmptyStr(oldVal.HeadComment, val.HeadComment)
				merged.LineComment = firstNonEmptyStr(oldVal.LineComment, val.LineComment)
				merged.FootComment = firstNonEmptyStr(oldVal.FootComment, val.FootComment)
				old.Content[j+1] = merged
				found = true
				break
			}
		}
		if !found {
			old.Content = append(old.Content, key, val)
		}
	}
	return old
}

func mergeValue(old, new *yaml.Node) *yaml.Node {
	// 值为空（零值被省略）时保留原值
	if new.Tag == "!!null" {
		return old
	}
	if old.Kind == yaml.ScalarNode && new.Kind == yaml.ScalarNode && old.Value == new.Value {
		return old
	}
	if old.Kind == yaml.SequenceNode && new.Kind == yaml.SequenceNode {
		// 序列整体替换，但保留原注释
		return new
	}
	return new
}

func firstNonEmptyStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
