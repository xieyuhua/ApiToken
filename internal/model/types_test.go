package model

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestChannelSupports(t *testing.T) {
	cases := []struct {
		name   string
		ch     Channel
		req    string
		wantOK bool
		wantUp string // 期望发往上游的模型名
	}{
		{
			name: "models 精确命中",
			ch:   Channel{Enabled: true, Models: []string{"gpt-4o", "gpt-4o-mini"}},
			req:  "gpt-4o-mini", wantOK: true, wantUp: "gpt-4o-mini",
		},
		{
			name: "大小写不敏感命中，但上游用客户端原始写法",
			ch:   Channel{Enabled: true, Models: []string{"GPT-4O"}},
			req:  "gpt-4o", wantOK: true, wantUp: "gpt-4o",
		},
		{
			name: "通配放通全部",
			ch:   Channel{Enabled: true, Models: []string{"*"}},
			req:  "随便什么模型", wantOK: true, wantUp: "随便什么模型",
		},
		{
			name: "models 与 alias 都为空时放通",
			ch:   Channel{Enabled: true},
			req:  "any-model", wantOK: true, wantUp: "any-model",
		},
		{
			name: "models 与 alias 是「或」关系：alias 命中即按映射转发",
			ch:   Channel{Enabled: true, Models: []string{"qwen-max"}, Alias: map[string]string{"gpt-4o": "qwen-max"}},
			req:  "gpt-4o", wantOK: true, wantUp: "qwen-max",
		},
		{
			name: "alias 只命中（models 不含该名）",
			ch:   Channel{Enabled: true, Models: []string{"qwen-max"}, Alias: map[string]string{"gpt-4o": "qwen-max"}},
			req:  "gpt-4o-turbo", wantOK: false,
		},
		{
			name: "停用渠道一律不支持",
			ch:   Channel{Enabled: false, Models: []string{"*"}},
			req:  "gpt-4o", wantOK: false,
		},
		{
			name: "models 不含该名且无 alias",
			ch:   Channel{Enabled: true, Models: []string{"qwen-max"}},
			req:  "gpt-4o", wantOK: false,
		},
		{
			name: "空模型名且无通配时不放行",
			ch:   Channel{Enabled: true, Models: []string{"gpt-4o"}},
			req:  "", wantOK: false,
		},
		{
			name: "空模型名命中通配（放通语义一致）",
			ch:   Channel{Enabled: true, Models: []string{"*"}},
			req:  "", wantOK: true, wantUp: "",
		},
	}
	for _, c := range cases {
		up, ok := c.ch.Supports(c.req)
		if ok != c.wantOK {
			t.Fatalf("%s: Supports(%q) = %v，期望 %v", c.name, c.req, ok, c.wantOK)
		}
		if c.wantOK && up != c.wantUp {
			t.Fatalf("%s: 上游模型名 = %q，期望 %q", c.name, up, c.wantUp)
		}
	}
}

// alias 优先级必须写清楚：models 列表在前，alias 是兜底映射。
func TestChannelSupportsAliasFallback(t *testing.T) {
	ch := Channel{
		Enabled: true,
		Models:  []string{"deepseek-chat"},
		Alias:   map[string]string{"gpt-4o": "deepseek-chat", "claude-3": "deepseek-chat"},
	}
	if up, ok := ch.Supports("gpt-4o"); !ok {
		t.Fatal("alias 应能命中未在 models 列表中的客户端模型名")
	} else if up != "deepseek-chat" {
		t.Fatalf("上游模型名 = %q", up)
	}
	// models 里没有的名字不会被 alias 之外的逻辑放行
	if _, ok := ch.Supports("llama-3"); ok {
		t.Fatal("未在 models/alias 中的模型名不应被放行")
	}
}

func TestMaskSecret(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"12345678", "********"},      // 恰好 8 个字符：全遮
		{"123456789", "1234****6789"}, // 9 字符：前后各留 4 位
		{"sk-abcdef1234567890", "sk-a****7890"},
		{"中文密钥测试用", "*******"}, // 7 个字符：全遮
		{"这是一个很长的中文密钥内容", "这是一个****密钥内容"},
	}
	for _, c := range cases {
		got := MaskSecret(c.in)
		if got != c.want {
			t.Fatalf("MaskSecret(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}

	// 多字节字符不能被切出半个码点：产出必须是合法 UTF-8
	long := "这是很长的中文密钥内容之一"
	masked := MaskSecret(long)
	if !json.Valid([]byte(`"` + masked + `"`)) {
		t.Fatalf("脱敏结果不是合法 UTF-8: %q", masked)
	}
	// 且绝不能把密钥原样漏出去
	if strings.Contains(masked, long) {
		t.Fatalf("完整密钥被泄露: %q", masked)
	}

	// 固定宽度：不同长度的长密钥脱敏后宽度一致，避免撑宽表格/暴露长度
	if len([]rune(MaskSecret("a-very-long-secret-key-0001"))) != len([]rune(MaskSecret("b-longer-secret-key-000222"))) {
		t.Fatal("长密钥脱敏后宽度应一致")
	}
}

func TestChannelMasked(t *testing.T) {
	ch := Channel{ID: "c1", Name: "n", APIKey: "sk-1234567890abcdef"}
	m := ch.Masked()
	if m.APIKey == ch.APIKey || !strings.HasPrefix(m.APIKey, "sk-1") {
		t.Fatalf("Masked 未脱敏: %q", m.APIKey)
	}
	if ch.APIKey != "sk-1234567890abcdef" {
		t.Fatal("Masked 必须返回副本，不能改到原对象")
	}
	if m.ID != "c1" || m.Name != "n" {
		t.Fatalf("其他字段被改动: %+v", m)
	}
}

func TestNewChannelID(t *testing.T) {
	id := NewChannelID("My Upstream Provider")
	if !strings.HasPrefix(id, "my-upstream-provider-") {
		t.Fatalf("ID 归一化有误: %q", id)
	}
	if len(strings.TrimSuffix(id, "-")) == 0 {
		t.Fatal("ID 为空")
	}

	// 名称里没有有效字符时退回 channel 前缀，而不是产出空串
	fallback := NewChannelID("！！！")
	if !strings.HasPrefix(fallback, "channel-") {
		t.Fatalf("无有效字符时应回退: %q", fallback)
	}

	// 同名连续创建必须唯一（同秒内靠自增序号兜底）
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		id := NewChannelID("同名渠道")
		if seen[id] {
			t.Fatalf("第 %d 次生成的 ID 与之前重复: %q", i, id)
		}
		seen[id] = true
	}

	// 长名称要截断，避免 ID 长得离谱。
	// ID 形如 base-HHMMSS-seq，去掉末尾两段就是名称部分。
	long := NewChannelID(strings.Repeat("name", 30))
	parts := strings.Split(long, "-")
	if len(parts) < 3 {
		t.Fatalf("ID 结构异常（应含名称/时间/序号三段）: %q", long)
	}
	basePart := strings.Join(parts[:len(parts)-2], "-")
	if n := len([]rune(basePart)); n > 20 {
		t.Fatalf("名称部分应截断到 20 字符，实际 %d: %q", n, basePart)
	}
}

func TestRouteIsEnabled(t *testing.T) {
	// nil = 缺省视为启用（旧配置没有 enabled 字段）
	if !(Route{}).IsEnabled() {
		t.Fatal("缺省应视为启用")
	}
	off := false
	if (Route{Enabled: &off}).IsEnabled() {
		t.Fatal("显式 false 应停用")
	}
	on := true
	if !(Route{Enabled: &on}).IsEnabled() {
		t.Fatal("显式 true 应启用")
	}
}

func TestLogEntryHelpers(t *testing.T) {
	ok := LogEntry{Status: 200}
	if !ok.OK() {
		t.Fatal("200 应算成功")
	}
	// 3xx（如 304 Not Modified）按当前语义算成功：重定向不是网关侧故障
	if !(LogEntry{Status: 302}).OK() {
		t.Fatal("302 应算成功")
	}
	for _, st := range []int{400, 404, 429, 500, 502, 504} {
		if (LogEntry{Status: st}).OK() {
			t.Fatalf("%d 不应算成功", st)
		}
	}
	// 状态码为 0（未真正发出请求）不能算成功
	if (LogEntry{}).OK() {
		t.Fatal("状态码 0 不应算成功")
	}

	if (LogEntry{}).SnippetTruncated() {
		t.Fatal("无截断标记时应返回 false")
	}
	for _, l := range []LogEntry{
		{RequestSnippetFull: 10},
		{ResponseSnippetFull: 10},
		{ResponseCapped: true},
	} {
		if !l.SnippetTruncated() {
			t.Fatalf("存在截断标记时应返回 true: %+v", l)
		}
	}
}

func TestLogSummaryKVDefaults(t *testing.T) {
	// 零值 JSON 序列化：管理接口直接序列化这些结构，字段不能丢
	b, err := json.Marshal(LogKV{Name: "gpt-4o", Count: 3})
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	var back LogKV
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	if back.Name != "gpt-4o" || back.Count != 3 {
		t.Fatalf("字段有丢失: %+v", back)
	}
}

func TestStatsAccumulationShape(t *testing.T) {
	// Stats 是累计量：这里锁住字段语义，防止后续改动把 failed/success 搞反
	s := Stats{Requests: 10, Success: 7, Failed: 3, LastUsed: time.Now()}
	if s.Success+s.Failed != s.Requests {
		t.Fatalf("成功+失败 应等于总请求数: %+v", s)
	}
}
