package main

// 临时补丁：渠道弹窗支持查看已保存的 Key 明文。

import (
	"fmt"
	"os"
	"strings"
)

func main() {
	const file = "internal/webui/index.html"
	data, err := os.ReadFile(file)
	if err != nil {
		fmt.Println("read err:", err)
		os.Exit(1)
	}
	s := string(data)

	type rule struct{ old, nw string }
	rules := []rule{
		{
			`      <div><label>API Key</label><input id="f_api_key" type="password" placeholder="sk-..." /></div>`,
			`      <div><label>API Key</label>
        <div class="row" style="gap:6px">
          <input id="f_api_key" type="password" class="secret" placeholder="sk-..." />
          <button class="mini ghost" onclick="toggleChKey()">显示</button>
          <button class="mini ghost" onclick="revealChannelKey()">查看已保存</button>
        </div>
      </div>`,
		},
		{
			`function saveChannel() {`,
			`function toggleChKey() {
  const el = $('f_api_key');
  el.type = el.type === 'password' ? 'text' : 'password';
}

async function revealChannelKey() {
  const id = $('f_id').value.trim() || editingId;
  if (!id) return toast('请先填写渠道 ID 或保存后再查看', true);
  if (!confirm('即将显示该渠道已保存的完整 API Key，请确认环境安全。\\n\\n是否继续？')) return;
  try {
    const list = await api('/admin/channels?reveal=1');
    const ch = list.find(c => c.id === id);
    if (!ch) return toast('未找到渠道 ' + id, true);
    $('f_api_key').value = ch.api_key || '';
    $('f_api_key').type = 'text';
    $('f_api_key').classList.add('revealed');
    toast('已显示 ' + id + ' 的密钥');
  } catch (e) { toast('获取失败：' + e.message, true); }
}

async function saveChannel() {`,
		},
		{
			`  $('f_api_key').value = '';
  if (!id) {`,
			`  $('f_api_key').value = '';
  $('f_api_key').type = 'password';
  $('f_api_key').classList.remove('revealed');
  if (!id) {`,
		},
	}
	for _, r := range rules {
		if !strings.Contains(s, r.old) {
			fmt.Printf("skip :: %.50q\n", r.old)
			continue
		}
		s = strings.Replace(s, r.old, r.nw, 1)
		fmt.Printf("patched :: %.50q\n", r.old)
	}
	if err := os.WriteFile(file, []byte(s), 0o644); err != nil {
		fmt.Println("write err:", err)
		os.Exit(1)
	}
}
