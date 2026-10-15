package config

// 界面主题取值。与路由策略同理：散落的字符串字面量收敛到这里，
// 避免「设置页能写进去、配置校验时却不认识」。
const (
	ThemeAuto  = "auto"
	ThemeDark  = "dark"
	ThemeLight = "light"
)

// IsValidTheme 判断主题取值是否受支持。
func IsValidTheme(t string) bool {
	switch t {
	case ThemeAuto, ThemeDark, ThemeLight:
		return true
	default:
		return false
	}
}
