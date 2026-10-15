package proxy

import (
	"testing"
	"time"

	"github.com/xieyuhua/apitoken/internal/config"
	"github.com/xieyuhua/apitoken/internal/model"
)

// newHandlerForLimits 只填充限额相关字段，不依赖 store（这些决策本身与存储无关）。
func newHandlerForLimits(rt *config.Runtime) *Handler {
	cfg := &config.Config{}
	cfg.ApplyRuntime(rt)
	return &Handler{cfg: cfg}
}

func TestAttemptLimit(t *testing.T) {
	cases := []struct {
		configured, candidates, want int
		name                         string
	}{
		{3, 5, 3, "配置值有效时取其值"},
		{0, 4, 4, "未配置时按候选数"},
		{-1, 4, 4, "非法负值退化成候选数"},
		{10, 2, 2, "不能超过候选数"},
		{1, 1, 1, "单候选"},
		{2, 1, 1, "候选少于配置值"},
	}
	for _, c := range cases {
		if got := attemptLimit(c.configured, c.candidates); got != c.want {
			t.Fatalf("%s: attemptLimit(%d, %d) = %d，期望 %d",
				c.name, c.configured, c.candidates, got, c.want)
		}
	}
}

func TestCurrentMaxBody(t *testing.T) {
	// 未配置时兜底 32MB
	if got := newHandlerForLimits(&config.Runtime{}).currentMaxBody(); got != 32<<20 {
		t.Fatalf("默认 request body 上限 = %d", got)
	}
	// 热更新后立即生效：这是 server.max_body_mb 的意义所在
	h := newHandlerForLimits(&config.Runtime{MaxBodyBytes: 8 << 20})
	if got := h.currentMaxBody(); got != 8<<20 {
		t.Fatalf("热更新后的上限 = %d", got)
	}
}

func TestRelayBodyLimit(t *testing.T) {
	// 响应体上限不能低于 minRelayBodyLimit：把 MaxBodyBytes 调小只影响入站请求，
	// 不能把长上下文的回复也裁掉
	if minRelayBodyLimit != 64<<20 {
		t.Fatalf("最小响应体上限 = %d", minRelayBodyLimit)
	}
	cases := []struct {
		maxBody int64
		want    int64
	}{
		{0, 64 << 20},
		{1 << 20, 64 << 20},    // 配置 1MB 也守着 64MB 的下限
		{8 << 20, 64 << 20},    // 仍小于下限
		{64 << 20, 64 << 20},   // 恰好等于下限
		{128 << 20, 128 << 20}, // 超过下限时才跟着走
	}
	for _, c := range cases {
		h := newHandlerForLimits(&config.Runtime{MaxBodyBytes: c.maxBody})
		if got := h.relayBodyLimit(); got != c.want {
			t.Fatalf("MaxBodyBytes=%d 时 relayBodyLimit = %d，期望 %d", c.maxBody, got, c.want)
		}
	}
}

// TestTimeoutPriority 锁住 README 里写明的超时优先级：
// channel.timeout_sec → stream_timeout（仅流式） → request_timeout → default_timeout → 120s
func TestTimeoutPriority(t *testing.T) {
	base := func() *config.Config {
		cfg := &config.Config{}
		cfg.ApplyRuntime(&config.Runtime{
			StreamTimeout:  7 * time.Second,
			RequestTimeout: 5 * time.Second,
			DefaultTimeout: 3 * time.Second,
		})
		return cfg
	}

	// 非流式：跳过 stream_timeout
	if got := timeoutFor(base(), model.Channel{}, false); got != 5*time.Second {
		t.Fatalf("非流式应取 request_timeout，实际 %v", got)
	}
	// 流式：stream_timeout 优先于 request_timeout
	if got := timeoutFor(base(), model.Channel{}, true); got != 7*time.Second {
		t.Fatalf("流式应取 stream_timeout，实际 %v", got)
	}
	// 渠道级超时最高优先级，且不区分是否流式
	for _, stream := range []bool{false, true} {
		if got := timeoutFor(base(), model.Channel{TimeoutSec: 42}, stream); got != 42*time.Second {
			t.Fatalf("渠道超时应最优先（stream=%v），实际 %v", stream, got)
		}
	}
	// 流式且未配 stream_timeout 时应退回 request_timeout
	cfg := base()
	rt := cfg.RT().Clone()
	rt.StreamTimeout = 0
	cfg.ApplyRuntime(rt)
	if got := timeoutFor(cfg, model.Channel{}, true); got != 5*time.Second {
		t.Fatalf("缺 stream_timeout 应退回 request_timeout，实际 %v", got)
	}
	// 全都没配时兜底 120s（最后一道防线，不能退化为 0 = 无限等待）
	empty := &config.Config{}
	if got := timeoutFor(empty, model.Channel{}, true); got != 120*time.Second {
		t.Fatalf("兜底超时 = %v", got)
	}
}
