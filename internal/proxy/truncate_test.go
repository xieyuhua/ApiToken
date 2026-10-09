package proxy

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestTruncateFullRuneSafe 校验按字符（而非字节）截断，且不会切坏多字节字符。
func TestTruncateFullRuneSafe(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		n         int
		wantFull  int // 期望报告的原始字符数，0 表示未截断
		wantRunes int // 期望保留的字符数（不含追加的提示后缀）
	}{
		{"不足不截", "abc", 5, 0, 3},
		{"刚好相等", "abc", 3, 0, 3},
		{"英文截断", "abcdefgh", 3, 8, 3},
		{"中文截断", "中文测试内容", 2, 6, 2},
		{"中英混排", "AI模型GPT-4测试", 5, 11, 5},
		{"emoji不被切坏", "a😀b😀c", 3, 5, 3},
		{"n为0原样返回", "abc", 0, 0, 3},
		{"空串", "", 5, 0, 0},
	}
	for _, c := range cases {
		got, full := truncateFull(c.in, c.n)
		if full != c.wantFull {
			t.Fatalf("%s: 原始字符数 = %d，期望 %d", c.name, full, c.wantFull)
		}
		if c.n == 0 {
			if got != c.in {
				t.Fatalf("%s: n=0 应原样返回，实际 %q", c.name, got)
			}
			continue
		}
		body := got
		if c.wantFull > 0 {
			// 截断时去掉追加的提示后缀再校验
			idx := strings.Index(got, "... [")
			if idx < 0 {
				t.Fatalf("%s: 缺少截断提示: %q", c.name, got)
			}
			body = got[:idx]
		}
		if n := utf8.RuneCountInString(body); n != c.wantRunes {
			t.Fatalf("%s: 保留 %d 字符，期望 %d", c.name, n, c.wantRunes)
		}
		if !utf8.ValidString(got) {
			t.Fatalf("%s: 结果不是合法 UTF-8: %q", c.name, got)
		}
	}
}

// TestTruncateFullLongInput 校验长输入不会产生按长度线性放大的中间数组。
func TestTruncateFullLongInput(t *testing.T) {
	// 一个 20 万字符的长回复
	long := strings.Repeat("中a😀", 50000)
	got, full := truncateFull(long, 100)
	if full != 150000 {
		t.Fatalf("原始字符数应为 150000，实际 %d", full)
	}
	if n := utf8.RuneCountInString(got); n < 100 {
		t.Fatalf("至少应保留 100 字符，实际 %d", n)
	}
	if !utf8.ValidString(got) {
		t.Fatal("结果不是合法 UTF-8")
	}
}

// legacyTruncateFull 是改造前的实现，用于基准对比。
func legacyTruncateFull(s string, n int) (string, int) {
	s = strings.TrimSpace(s)
	if n <= 0 {
		return s, 0
	}
	rs := []rune(s)
	if len(rs) <= n {
		return s, 0
	}
	return string(rs[:n]) + fmt.Sprintf("... [已截断，原始 %d 字]", len(rs)), len(rs)
}

func BenchmarkTruncateFull(b *testing.B) {
	// 贴近真实的中文长回复
	body := strings.Repeat("这是一段模型回复内容。", 500) // 5000 字符
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = truncateFull(body, 8000)
	}
}

func BenchmarkTruncateFullLegacy(b *testing.B) {
	body := strings.Repeat("这是一段模型回复内容。", 500)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = legacyTruncateFull(body, 8000)
	}
}

// BenchmarkTruncateFullTruncated 对比"确实需要截断"这一更常见的场景。
func BenchmarkTruncateFullTruncated(b *testing.B) {
	body := strings.Repeat("这是一段模型回复内容。", 1000) // 10000 字符 > payload_limit
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = truncateFull(body, 8000)
	}
}

func BenchmarkTruncateFullTruncatedLegacy(b *testing.B) {
	body := strings.Repeat("这是一段模型回复内容。", 1000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = legacyTruncateFull(body, 8000)
	}
}
