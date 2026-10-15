package util

// FirstNonEmpty 返回 vals 中第一个非空字符串，全为空时返回 ""。
//
// 典型用法是 fallback：`util.FirstNonEmpty(a, b)` 表示优先取 a，a 为空退回 b。
// 可变参数是为了同时覆盖两参数与多参数的调用点，避免再长出第二个同名函数。
func FirstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
