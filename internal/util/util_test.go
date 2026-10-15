package util

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFirstNonEmpty(t *testing.T) {
	cases := []struct {
		vals []string
		want string
	}{
		{[]string{"a", "b"}, "a"},
		{[]string{"", "b"}, "b"},     // 空串要跳过，继续往后找
		{[]string{"", "", "c"}, "c"}, // 多个空值要能穿透到第一个非空值
		{[]string{"", ""}, ""},       // 全空 -> 空串
		{[]string{}, ""},             // 空参数列表
		{[]string{" "}, " "},         // 空格不是空串，应原样返回
	}
	for _, c := range cases {
		if got := FirstNonEmpty(c.vals...); got != c.want {
			t.Fatalf("FirstNonEmpty(%q) = %q，期望 %q", c.vals, got, c.want)
		}
	}
}

func TestBoolPtr(t *testing.T) {
	// 配置里用 *bool 区分「未提交」与「提交 false」，所以取到的值必须准确
	for _, want := range []bool{true, false} {
		p := BoolPtr(want)
		if p == nil {
			t.Fatalf("BoolPtr(%v) 返回 nil", want)
		}
		if *p != want {
			t.Fatalf("*BoolPtr(%v) = %v", want, *p)
		}
	}
	// 两次调用不能共享同一个底层变量，否则改一个会影响另一个
	a, b := BoolPtr(true), BoolPtr(true)
	*a = false
	if *b != true {
		t.Fatal("两次调用返回了同一个指针")
	}
}

func TestWriteJSON(t *testing.T) {
	w := httptest.NewRecorder()
	WriteJSON(w, http.StatusCreated, map[string]string{"k": "v"})

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d", w.Code)
	}
	if got := w.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type = %q", got)
	}
	body := strings.TrimSpace(w.Body.String())
	if body != `{"k":"v"}` {
		t.Fatalf("body = %s", body)
	}
}

func TestWriteError(t *testing.T) {
	// 错误体必须是 OpenAI 结构，且 type 按状态码推断
	cases := []struct {
		status int
		typ    string
		want   string
	}{
		{400, "", "invalid_request_error"},
		{401, "", "authentication_error"},
		{403, "", "authentication_error"},
		{404, "", "not_found_error"},
		{500, "", "api_error"},
		{502, "", "api_error"},
		{400, "custom_type", "custom_type"}, // 显式指定优先
	}
	for _, c := range cases {
		w := httptest.NewRecorder()
		WriteError(w, c.status, "code_x", "出错了", c.typ)
		if w.Code != c.status {
			t.Fatalf("status = %d，期望 %d", w.Code, c.status)
		}
		body := w.Body.String()
		if !strings.Contains(body, `"type":"`+c.want+`"`) {
			t.Fatalf("status=%d 的 type 不对: %s", c.status, body)
		}
		if !strings.Contains(body, `"message":"出错了"`) {
			t.Fatalf("message 缺失: %s", body)
		}
		if !strings.Contains(body, `"code":"code_x"`) {
			t.Fatalf("code 缺失: %s", body)
		}
	}
}

func TestDecodeJSON(t *testing.T) {
	var got struct {
		Name string `json:"name"`
		Age  int    `json:"age"`
	}

	// 正常解析
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":"a","age":3}`))
	if err := DecodeJSON(r, &got); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if got.Name != "a" || got.Age != 3 {
		t.Fatalf("解析结果有误: %+v", got)
	}

	// 空 body 视为「不修改」，不应报错、保持零值
	got = struct {
		Name string `json:"name"`
		Age  int    `json:"age"`
	}{Name: "keep", Age: 9}
	r = httptest.NewRequest(http.MethodPost, "/", strings.NewReader("   "))
	if err := DecodeJSON(r, &got); err != nil {
		t.Fatalf("空 body 不应报错: %v", err)
	}
	if got.Name != "keep" || got.Age != 9 {
		t.Fatalf("空 body 不应改动目标值: %+v", got)
	}

	// 非法 JSON 要报错，不能默默留零值
	r = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{bad`))
	if err := DecodeJSON(r, &got); err == nil {
		t.Fatal("非法 JSON 应报错")
	}
}

func TestDecodeJSONLimitsBodySize(t *testing.T) {
	// 管理请求体必须有上限，否则一个误传的大文件就能把内存吃干
	big := strings.Repeat("x", maxDecodeBytes+1024)
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"k":"`+big+`"}`))
	var got map[string]any
	if err := DecodeJSON(r, &got); err == nil {
		t.Fatal("超长 body 应报错")
	}
}
