// Package config 负责加载与校验网关配置文件。
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// Config 网关总配置。
type Config struct {
	Server   ServerConfig    `yaml:"server"`
	Security SecurityConfig  `yaml:"security"`
	Routing  RoutingConfig   `yaml:"routing"`
	Logging  LoggingConfig   `yaml:"logging"`
	Upstream UpstreamConfig  `yaml:"upstream"`
	Routes   []model.Route   `yaml:"routes"`
	Channels []model.Channel `yaml:"channels"`

	path string
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
	Level     string `yaml:"level"`
	AccessLog bool   `yaml:"access_log"`
	KeepLogs  int    `yaml:"keep_logs"`
	// RecordPayload 开启后在调用日志中记录请求/响应摘要（可能含敏感内容）
	RecordPayload bool `yaml:"record_payload"`
	PayloadLimit  int  `yaml:"payload_limit"`
}

// UpstreamConfig 上游 HTTP 客户端配置。
type UpstreamConfig struct {
	DefaultTimeout Duration `yaml:"default_timeout"`
	ConnectTimeout Duration `yaml:"connect_timeout"`
	KeepAlive      Duration `yaml:"keep_alive"`
	MaxIdleConns   int      `yaml:"max_idle_conns"`
	ProxyURL       string   `yaml:"proxy_url"`
}

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
	return cfg, nil
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
		c.Logging.KeepLogs = 1000
	}
	if c.Logging.PayloadLimit <= 0 {
		c.Logging.PayloadLimit = 2000
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
	for i := range c.Channels {
		if c.Channels[i].Weight <= 0 {
			c.Channels[i].Weight = 1
		}
	}
}

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
		for _, cid := range r.Channels {
			if !ids[cid] {
				return fmt.Errorf("route %s 引用了不存在的渠道 %s", r.Model, cid)
			}
		}
	}
	if err := os.MkdirAll(c.Server.DataDir, 0o755); err != nil {
		return fmt.Errorf("创建数据目录失败: %w", err)
	}
	return nil
}
