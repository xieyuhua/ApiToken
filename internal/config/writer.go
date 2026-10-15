package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/xieyuhua/apitoken/internal/util"
)

// writer.go 负责「把运行时配置写回 config.yaml」。
//
// 与 config.go 分开的原因：这里是纯粹的文档写操作（读原文件 → yaml.Node 增量
// 合并 → 原子替换），与配置解析/校验/快照这些读侧关注点互不相干，
// 混在一起让 config.go 变成了一个做任何事的文件。

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
		Server: c.Server,
		UI:     UIConfig{Theme: rt.Theme},
		Security: SecurityConfig{ClientKeys: append([]string(nil), rt.ClientKeys...), AdminToken: rt.AdminToken,
			TrustProxyHeaders: util.BoolPtr(rt.TrustProxyHeaders)},
		Routing: RoutingConfig{
			Strategy: rt.Strategy, MaxAttempts: rt.MaxAttempts,
			ForceStreamUsage: rt.ForceStreamUsage, RetryStatus: append([]int(nil), rt.RetryStatus...),
			Cooldown:     durationPtr(rt.Cooldown),
			ProbeEnabled: util.BoolPtr(rt.ProbeEnabled), ProbeInterval: durationPtr(rt.ProbeInterval),
			ProbeFailThreshold: rt.ProbeFailThreshold,
		},
		Logging: LoggingConfig{
			Level: c.Logging.Level, AccessLog: rt.AccessLog, KeepLogs: rt.KeepLogs,
			RecordPayload: rt.RecordPayload, PayloadLimit: rt.PayloadLimit, LogArchive: rt.LogArchive,
			FileEnabled: c.Logging.FileEnabled, FileDir: c.Logging.FileDir,
			FileMaxMB: c.Logging.FileMaxMB, FileKeep: c.Logging.FileKeep,
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
				merged.HeadComment = util.FirstNonEmpty(oldVal.HeadComment, val.HeadComment)
				merged.LineComment = util.FirstNonEmpty(oldVal.LineComment, val.LineComment)
				merged.FootComment = util.FirstNonEmpty(oldVal.FootComment, val.FootComment)
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
	// 嵌套 mapping 必须递归合并，不能直接整体替换：
	// 否则 server:/routing: 这类二级块里用户写的注释、以及网关当前版本不认识的
	// 旧字段，会在第一次保存设置时被整块抹掉（顶层键是保留的，所以问题不易被发现）。
	if old.Kind == yaml.MappingNode && new.Kind == yaml.MappingNode {
		return mergeNode(old, new)
	}
	if old.Kind == yaml.SequenceNode && new.Kind == yaml.SequenceNode {
		// 序列整体替换（列表顺序即语义，半合并没有意义），注释由上层继承
		return new
	}
	return new
}
