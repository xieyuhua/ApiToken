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
  document.documentElement.dataset.theme = resolveTheme(t || currentTheme());
  const btn = $('themeBtn');
  if (btn) {
    btn.textContent = THEME_ICON[t || currentTheme()] || '🌗';
    btn.title = '主题：' + (THEME_CN[t || currentTheme()] || '跟随系统') + '（点击切换）';
  }
}

function toggleTheme() {
  const cur = currentTheme();
  const next = THEMES[(THEMES.indexOf(cur) + 1) % THEMES.length];
  localStorage.setItem('apitoken_theme', next);
  applyTheme(next);
  toast('主题已切换为：' + (THEME_CN[next] || next));
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

applyTheme();
