/* apitoken 管理端公共脚本 */
const $ = id => document.getElementById(id);

/* ---------------- 主题 ---------------- */
const THEMES = ['auto', 'dark', 'light'];
const THEME_ICON = {auto: '🌗', dark: '🌙', light: '☀️'};
const THEME_CN = {auto: '跟随系统', dark: '深色', light: '浅色'};

function systemTheme() {
  return window.matchMedia && window.matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark';
}

function resolveTheme(t) {
  return t === 'auto' ? systemTheme() : t;
}

function currentTheme() {
  return localStorage.getItem('apitoken_theme') || 'auto';
}

function applyTheme(t) {
  const theme = t || currentTheme();
  document.documentElement.dataset.theme = resolveTheme(theme);
  const btn = $('themeBtn') || document.querySelector('#themeBtn');
  if (btn) {
    btn.textContent = THEME_ICON[theme] || '🌗';
    btn.title = '主题：' + (THEME_CN[theme] || '跟随系统') + '（点击切换）';
  }
}

function setTheme(t) {
  localStorage.setItem('apitoken_theme', t);
  applyTheme(t);
}

/** 主题按钮：顶栏一个按钮，点击即在 跟随系统 / 深色 / 浅色 之间切换 */
function mountThemePicker() {
  if ($('themeBtn')) return;
  const header = document.querySelector('header');
  if (!header) return;

  const btn = document.createElement('button');
  btn.id = 'themeBtn';
  btn.className = 'ghost theme-btn';
  btn.title = '切换主题';
  btn.addEventListener('click', toggleTheme);

  const anchor = header.querySelector('input#token') || header.querySelector('.sp');
  if (anchor && anchor.parentNode) anchor.parentNode.insertBefore(btn, anchor);
  else header.appendChild(btn);

  // 按钮晚于首次 applyTheme 创建，这里直接给出初始图标与提示
  const cur = currentTheme();
  btn.textContent = THEME_ICON[cur] || '🌗';
  btn.title = '主题：' + (THEME_CN[cur] || '跟随系统') + '（点击切换）';
  applyTheme(cur);
}
/** 点击按钮：跟随系统 -> 深色 -> 浅色 -> 跟随系统（无弹窗，按钮图标即状态） */

function toggleTheme() {
  const next = THEMES[(THEMES.indexOf(currentTheme()) + 1) % THEMES.length];
  setTheme(next);
}

/** 保存服务端下发的默认主题（仅当本机没有手动选择时生效） */
function applyServerTheme(t) {
  if (!t || THEMES.indexOf(t) < 0) return;
  if (localStorage.getItem('apitoken_theme')) return;
  applyTheme(t);
}

/* ---------------- 鉴权与请求 ---------------- */
function adminToken() { return localStorage.getItem('apitoken_admin') || ''; }
function saveToken() {
  localStorage.setItem('apitoken_admin', $('token').value.trim());
  toast('管理令牌已保存');
}
function initToken() { const t = $('token'); if (t) t.value = adminToken(); }

async function api(path, opts = {}) {
  const headers = Object.assign({'Content-Type': 'application/json'}, opts.headers || {});
  if (adminToken()) headers['X-Admin-Token'] = adminToken();
  const res = await fetch(path, Object.assign({}, opts, {headers}));
  const text = await res.text();
  let data = null;
  try { data = text ? JSON.parse(text) : null; } catch (e) { data = text; }
  if (!res.ok) {
    const msg = (data && data.error && (data.error.message || data.error)) || ('HTTP ' + res.status);
    throw new Error(msg);
  }
  return data;
}

/** 下载文件：携带管理令牌鉴权，失败用 toast 提示（不跳转到错误 JSON 页面）。 */
async function downloadFile(path, fallbackName) {
  try {
    const headers = {};
    if (adminToken()) headers['X-Admin-Token'] = adminToken();
    const res = await fetch(path, { headers });
    if (!res.ok) {
      let msg = 'HTTP ' + res.status;
      try {
        const d = await res.json();
        msg = (d.error && (d.error.message || d.error)) || msg;
      } catch (e) {}
      throw new Error(msg);
    }
    const blob = await res.blob();
    let name = fallbackName;
    const cd = res.headers.get('Content-Disposition') || '';
    const m = /filename="?([^";]+)"?/.exec(cd);
    if (m && m[1]) name = m[1];
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = name;
    document.body.appendChild(a);
    a.click();
    a.remove();
    URL.revokeObjectURL(url);
    toast('已导出 ' + name);
  } catch (e) {
    toast('导出失败：' + e.message, true);
  }
}

let toastTimer;
function toast(msg, isErr) {
  const t = $('toast');
  if (!t) return;
  t.textContent = msg;
  t.className = 'toast' + (isErr ? ' err' : '');
  t.style.display = 'block';
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { t.style.display = 'none'; }, 3600);
}

// 数字格式化：≥100 万显示为 M、≥10 亿显示为 B（如 2.35M、1.2B），其余按千分位
const fmt = n => {
  n = n ?? 0;
  if (n >= 1e9) return parseFloat((n / 1e9).toFixed(2)) + 'B';
  if (n >= 1e6) return parseFloat((n / 1e6).toFixed(2)) + 'M';
  return n.toLocaleString();
};
const esc = s => String(s ?? '').replace(/[&<>"]/g, c => ({'&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;'}[c]));

function fmtTime(t) {
  if (!t) return '-';
  const d = new Date(t);
  const p = n => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
}
function fmtTimeShort(t) {
  if (!t) return '-';
  const d = new Date(t);
  const p = n => String(n).padStart(2, '0');
  return `${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
}
function fmtLatency(ms) {
  if (ms == null) return '-';
  return ms >= 1000 ? (ms / 1000).toFixed(2) + ' s' : ms + ' ms';
}
/* 运行时长（秒）→ 中文可读格式，与后端 FormatUptime 保持一致 */
function fmtUptime(secs) {
  if (secs == null || secs < 0) return '-';
  secs = Math.floor(secs);
  if (secs < 60) return secs + ' 秒';
  if (secs < 3600) return Math.floor(secs / 60) + ' 分 ' + (secs % 60) + ' 秒';
  if (secs < 86400) return Math.floor(secs / 3600) + ' 小时 ' + Math.floor((secs % 3600) / 60) + ' 分';
  return Math.floor(secs / 86400) + ' 天 ' + Math.floor((secs % 86400) / 3600) + ' 小时';
}
function statusPill(code) {
  const cls = code >= 200 && code < 300 ? 'on' : (code >= 500 || code === 0 ? 'unhealthy' : 'warn');
  return `<span class="pill ${cls}">${code || '-'}</span>`;
}

function copyText(text) {
  if (!text) return toast('没有可复制的内容', true);
  navigator.clipboard?.writeText(text).then(() => toast('已复制'), () => toast('复制失败', true));
}

/* 通用竖直柱状图 */
function renderColumns(elId, items, opts = {}) {
  const el = $(elId);
  if (!el) return;
  if (!items || !items.length) { el.innerHTML = '<div class="muted">暂无数据</div>'; return; }
  // 度量值：默认按次数；showTokens 时按「输入+输出 token 总量」
  const val = i => opts.showTokens ? ((i.prompt_tokens || 0) + (i.completion_tokens || 0)) : (i.count || 0);
  // 统一按度量值降序，柱状图横向空间有限，只取前 N 个
  const list = items.slice().sort((a, b) => val(b) - val(a)).slice(0, opts.top || 12);
  const max = Math.max(...list.map(val), 1);
  el.innerHTML = list.map(i => {
    const pct = Math.max(3, Math.round(val(i) * 92 / max)); // 92% 封顶，柱顶留出数值位
    const bad = !opts.showTokens && i.failed > 0;
    const head = opts.showTokens ? `↓${fmt(i.prompt_tokens)} ↑${fmt(i.completion_tokens)}`
      : opts.showLatency ? fmtLatency(i.avg_latency_ms)
      : `${fmt(i.count)} 次`;
    const tip = [
      i.name, `${fmt(i.count)} 次`, i.failed ? `失败 ${fmt(i.failed)}` : '',
      i.avg_latency_ms != null ? `平均 ${fmtLatency(i.avg_latency_ms)}` : '',
      i.prompt_tokens || i.completion_tokens ? `↓${fmt(i.prompt_tokens)} ↑${fmt(i.completion_tokens)}` : ''
    ].filter(Boolean).join(' · ');
    return `<div class="col" title="${esc(tip)}">
      <div class="col-top"><div class="col-val">${head}</div><div class="col-bar ${bad ? 'bad' : ''}" style="height:${pct}%"></div></div>
      <div class="col-name">${esc(String(i.name).slice(0, 10))}</div></div>`;
  }).join('');
}

// 跟随系统时，系统主题变化实时生效
if (window.matchMedia) {
  const mq = window.matchMedia('(prefers-color-scheme: light)');
  const onChange = () => { if (currentTheme() === 'auto') applyTheme('auto'); };
  if (mq.addEventListener) mq.addEventListener('change', onChange);
  else if (mq.addListener) mq.addListener(onChange);
}
applyTheme();
mountThemePicker();
