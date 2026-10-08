package main

// 临时补丁：还原主题默认值并更新 README。

import (
	"fmt"
	"os"
	"strings"
)

func main() {
	// 1) config.yaml 主题改回 auto（跟随系统）
	f1 := "config.yaml"
	d1, err := os.ReadFile(f1)
	if err != nil {
		fmt.Println("read err:", err)
		os.Exit(1)
	}
	s1 := strings.Replace(string(d1), "ui:\n  theme: light", "ui:\n  theme: auto", 1)
	if err := os.WriteFile(f1, []byte(s1), 0o644); err != nil {
		fmt.Println("write err:", err)
		os.Exit(1)
	}
	fmt.Println("config theme -> auto")

	// 2) README
	f2 := "README.md"
	d2, err := os.ReadFile(f2)
	if err != nil {
		fmt.Println("read err:", err)
		os.Exit(1)
	}
	s2 := string(d2)

	navOld := "| http://127.0.0.1:8080/settings | 设置：修改网关密钥、路由、日志、超时（保存即生效） |"
	navNew := navOld + "\n\n所有页面右上角有主题按钮（跟随系统 / 深色 / 浅色），选择会记在浏览器本地；\n在「设置 → 界面外观」可指定默认主题并写回配置（`ui.theme`）。"

	lines := []string{
		"### 9.1 主题与密钥查看",
		"",
		"- **主题**：顶栏主题按钮在「跟随系统 / 深色 / 浅色」间循环，选择存浏览器 localStorage；",
		"  设置页「界面外观 → 主题」可指定默认主题，保存后写回 `config.yaml` 的 `ui.theme`，",
		"  其他浏览器/新设备首次打开时会采用该默认值（本机手动选过则以本机为准）",
		"- **查看密钥明文**：设置页「接入密钥」区点「查看明文」，会二次确认后请求 `GET /admin/settings?reveal=1`",
		"  取回完整密钥并填入输入框，可直接复制或修改后保存；点「隐藏」即清空并回到脱敏状态",
		"- 渠道弹窗同样提供「查看已保存」，可取回该渠道的完整 API Key",
		"- 明文接口仍需管理令牌，未授权返回 401；请只在受信任环境使用，注意截图与旁观",
		"",
		"## 10. 测试（自检）页面",
	}
	secOld := "## 10. 测试（自检）页面"
	secNew := strings.Join(lines, "\n")

	for _, r := range []struct{ old, nw string }{{navOld, navNew}, {secOld, secNew}} {
		if !strings.Contains(s2, r.old) {
			fmt.Printf("skip :: %.45q\n", r.old)
			continue
		}
		s2 = strings.Replace(s2, r.old, r.nw, 1)
		fmt.Printf("patched readme :: %.45q\n", r.old)
	}
	if err := os.WriteFile(f2, []byte(s2), 0o644); err != nil {
		fmt.Println("write err:", err)
		os.Exit(1)
	}
}
