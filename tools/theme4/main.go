package main

// 临时补丁：设置页支持密钥明文查看 + 主题选择。

import (
	"fmt"
	"os"
	"strings"
)

func main() {
	const file = "internal/webui/settings.html"
	data, err := os.ReadFile(file)
	if err != nil {
		fmt.Println("read err:", err)
		os.Exit(1)
	}
	s := string(data)

	type rule struct{ old, nw string }
	rules := []rule{
		// 密钥区：加查看/隐藏与复制
		{
			`        <div class="keys">
          <label>网关密钥（client_keys，一行一个；客户端用 Authorization: Bearer &lt;key&gt;）</label>
          <textarea id="clientKeys" rows="4" spellcheck="false"></textarea>
          <p class="hint" id="keysHint">当前生效：-</p>
        </div>
        <div>
          <label>管理令牌（admin_token，访问 /admin/* 需 X-Admin-Token）</label>
          <div class="row" style="gap:6px">
            <input id="adminToken" type="password" placeholder="留空表示不修改" />
            <button class="mini ghost" onclick="togglePwd()">显示</button>
            <button class="mini ghost" onclick="genToken()">随机生成</button>
          </div>
          <p class="hint" id="adminHint">当前生效：-</p>`,
			`        <div class="keys">
          <label>网关密钥（client_keys，一行一个；客户端用 Authorization: Bearer &lt;key&gt;）</label>
          <textarea id="clientKeys" rows="4" spellcheck="false" class="secret"></textarea>
          <div class="row" style="margin-top:6px">
            <button class="mini ghost" onclick="revealSecrets()">查看明文</button>
            <button class="mini ghost" onclick="copyText($('clientKeys').value)">复制</button>
            <button class="mini ghost" onclick="maskSecrets()">隐藏</button>
            <span class="muted" id="revealState" style="font-size:12px"></span>
          </div>
          <p class="hint" id="keysHint">当前生效：-</p>
        </div>
        <div>
          <label>管理令牌（admin_token，访问 /admin/* 需 X-Admin-Token）</label>
          <div class="row" style="gap:6px">
            <input id="adminToken" type="password" class="secret" placeholder="留空表示不修改" />
            <button class="mini ghost" onclick="togglePwd()">显示</button>
            <button class="mini ghost" onclick="copyText($('adminToken').value)">复制</button>
            <button class="mini ghost" onclick="genToken()">随机生成</button>
          </div>
          <p class="hint" id="adminHint">当前生效：-</p>`,
		},
		// 提示文案
		{
			`          <div class="warnbox" style="margin-top:10px">
            修改后立即生效：若修改了管理令牌，请把新令牌填入右上角并保存，否则会被拒之门外。
            对话测试页使用的网关密钥若被删除，需要重新填写。
          </div>`,
			`          <div class="warnbox" style="margin-top:10px">
            修改后立即生效：若修改了管理令牌，请把新令牌填入右上角并保存，否则会被拒之门外。
            对话测试页使用的网关密钥若被删除，需要重新填写。<br/>
            「查看明文」会在浏览器中显示已保存的完整密钥，仅在受信任的环境下使用，注意截图与 shoulder surfing。
          </div>`,
		},
		// 主题设置区
		{
			`  <section>
    <h2>超时与出网</h2>`,
			`  <section>
    <h2>界面外观</h2>
    <div class="pad grid2">
      <div>
        <label>主题</label>
        <select id="theme" onchange="previewTheme(this.value)">
          <option value="auto">跟随系统</option>
          <option value="dark">深色</option>
          <option value="light">浅色</option>
        </select>
        <p class="hint">保存后对本机生效并写回配置文件，作为新浏览器的默认主题</p>
      </div>
      <div>
        <label>快速切换</label>
        <div class="row" style="padding-top:4px">
          <button class="ghost mini" onclick="toggleTheme()">切换主题</button>
          <span class="muted" id="themeNow" style="font-size:12px"></span>
        </div>
      </div>
    </div>
  </section>

  <section>
    <h2>超时与出网</h2>`,
		},
		// load(): 读取主题 + 密钥查看状态
		{
			`    $('clientKeys').value = '';
    $('keysHint').textContent = '当前生效 ' + s.client_keys.length + ' 个：' + s.client_keys.join('、') + '（出于安全不回显，留空表示不修改）';`,
			`    $('theme').value = s.theme || 'auto';
    $('themeNow').textContent = '本机当前：' + (THEME_CN[currentTheme()] || currentTheme());
    $('clientKeys').value = '';
    $('keysHint').textContent = '当前生效 ' + s.client_keys.length + ' 个：' + s.client_keys.join('、') + '（出于安全不回显，留空表示不修改）';`,
		},
		// save(): 带上 theme
		{
			`    proxy_url: $('proxyUrl').value.trim()
  };`,
			`    proxy_url: $('proxyUrl').value.trim(),
    theme: $('theme').value
  };`,
		},
		// 保存成功后应用服务端主题
		{
			`    const notes = [];
    if (r.admin_token_new) notes.push('管理令牌已更新，请牢记新令牌');`,
			`    applyServerTheme(r.settings && r.settings.theme);
    $('themeNow').textContent = '本机当前：' + (THEME_CN[currentTheme()] || currentTheme());
    const notes = [];
    if (r.admin_token_new) notes.push('管理令牌已更新，请牢记新令牌');`,
		},
		// 新增函数：明文查看 / 隐藏 / 预览主题
		{
			`function resetKeys() {`,
			`let revealedCache = null;

async function revealSecrets() {
  if (revealedCache) {
    fillSecrets(revealedCache);
    return;
  }
  if (!confirm('即将在浏览器中显示已保存的完整密钥。\\n\\n请确认当前环境安全（避免截图、录屏或旁人看到）。\\n\\n是否继续？')) return;
  try {
    const d = await api('/admin/settings?reveal=1');
    revealedCache = {client_keys: d.settings.client_keys || [], admin_token: d.settings.admin_token || ''};
    fillSecrets(revealedCache);
    toast('已显示明文密钥，请注意保管');
  } catch (e) { toast('获取失败：' + e.message, true); }
}

function fillSecrets(d) {
  $('clientKeys').value = (d.client_keys || []).join('\\n');
  $('adminToken').value = d.admin_token || '';
  $('adminToken').type = 'text';
  $('clientKeys').classList.add('revealed');
  $('adminToken').classList.add('revealed');
  $('revealState').textContent = '正在显示明文（输入框已可编辑，保存将覆盖原值）';
  $('keysHint').textContent = '明文模式：可直接修改后保存，或点「隐藏」放弃修改';
}

function maskSecrets() {
  revealedCache = null;
  $('clientKeys').value = '';
  $('clientKeys').classList.remove('revealed');
  $('adminToken').value = '';
  $('adminToken').type = 'password';
  $('adminToken').classList.remove('revealed');
  $('revealState').textContent = '';
  load();
}

function previewTheme(t) {
  localStorage.setItem('apitoken_theme', t);
  applyTheme(t);
  $('themeNow').textContent = '本机当前：' + (THEME_CN[currentTheme()] || currentTheme());
  toast('预览：' + (THEME_CN[t] || t) + '（点「保存并生效」写入配置）');
}

function resetKeys() {`,
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
