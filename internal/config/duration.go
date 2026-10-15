package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration 解析 YAML 中带单位的时长字符串（"300s" / "2m" / "1h30m"）。
//
// 只接受带单位的形式：裸整数不接受，因为它可能想表达 300 秒也可能想表达 300 毫秒，
// 猜错就是「超时时间差 1000 倍」这种要命的差别。宁可在启动时报错（提示怎么改），
// 也不要悄悄按某个单位解释。
type Duration time.Duration

// UnmarshalYAML 解析字符串时长。
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return fmt.Errorf("时长必须是字符串形式（如 300s / 5m / 1h）: %w", err)
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return fmt.Errorf("时长为空，请写成 300s / 5m / 1h 这类带单位的形式")
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		// 纯数字漏写单位是最常见的手误，单独告诉用户该怎么改
		if _, convErr := strconv.ParseFloat(s, 64); convErr == nil {
			return fmt.Errorf("时长 %q 缺少单位，请写成 %ss（秒）这类形式", s, s)
		}
		return fmt.Errorf("非法时长 %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

// D 转换为 time.Duration。
func (d Duration) D() time.Duration { return time.Duration(d) }

// MarshalYAML 序列化为 "300s" 这类可读字符串，避免写出纳秒整数。
func (d Duration) MarshalYAML() (any, error) {
	return time.Duration(d).String(), nil
}
