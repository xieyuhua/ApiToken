package util

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
)

// maxDecodeBytes 管理接口请求体的上限。管理请求都是小 JSON（渠道、路由、设置），
// 4MB 远超实际需要，同时挡住误传的大文件上传。
const maxDecodeBytes = 4 << 20

// WriteJSON 写出 JSON 响应。
//
// 只有一处实现的原因很实际：之前 gateway 与 admin 各写一份，
// charset、编码器、错误处理稍有不同就会出现「同一接口两种 Content-Type」。
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// WriteError 输出 OpenAI 风格错误响应：{"error":{"message","type","code"}}。
// typ 为空时按状态码推断默认值。
func WriteError(w http.ResponseWriter, status int, code, message, typ string) {
	if typ == "" {
		typ = "invalid_request_error"
	}
	if status == 401 || status == 403 {
		typ = "authentication_error"
	} else if status == 404 {
		typ = "not_found_error"
	} else if status >= 500 {
		typ = "api_error"
	}
	WriteJSON(w, status, map[string]any{
		"error": map[string]any{"message": message, "type": typ, "code": code},
	})
}

// DecodeJSON 读取并解析 JSON 请求体。空 body 视为「不修改」，留 v 为零值。
func DecodeJSON(r *http.Request, v any) error {
	data, err := io.ReadAll(io.LimitReader(r.Body, maxDecodeBytes))
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	return json.Unmarshal(data, v)
}
