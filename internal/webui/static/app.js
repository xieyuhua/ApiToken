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

const fmt = n => (n ?? 0).toLocaleString();
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
function statusPill(code) {
  const cls = code >= 200 && code < 300 ? 'on' : (code >= 500 || code === 0 ? 'unhealthy' : 'warn');
  return `<span class="pill ${cls}">${code || '-'}</span>`;
}

function copyText(text) {
  if (!text) return toast('没有可复制的内容', true);
  navigator.clipboard?.writeText(text).then(() => toast('已复制'), () => toast('复制失败', true));
}

/* 通用横向条形图 */
function renderBars(elId, items, opts = {}) {
  const el = $(elId);
  if (!el) return;
  if (!items || !items.length) { el.innerHTML = '<div class="muted">暂无数据</div>'; return; }
  const max = Math.max(...items.map(i => i.count), 1);
  el.innerHTML = items.map(i => {
    const pct = Math.round(i.count * 100 / max);
    const bad = i.failed > 0;
    const right = opts.showLatency ? `<span class="muted">${fmtLatency(i.avg_latency_ms)}</span>`
      : `<span class="muted">${fmt(i.count)} 次${i.failed ? ' / 失败 ' + fmt(i.failed) : ''}</span>`;
    return `<div class="bar"><span class="mono" title="${esc(i.name)}">${esc(String(i.name).slice(0, 22))}</span>
      <span class="track"><span class="fill ${bad ? 'bad' : ''}" style="width:${pct}%"></span></span>${right}</div>`;
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
