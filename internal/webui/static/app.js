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

/* ===== 横向条形图（展示全部条目 + 详细数值） ===== */
function renderBars(elId, items, opts = {}) {
  const el = $(elId);
  if (!el) return;
  if (!items || !items.length) { el.innerHTML = '<div class="muted">暂无数据</div>'; return; }
  const val = i => opts.showTokens ? ((i.prompt_tokens || 0) + (i.completion_tokens || 0)) : (i.count || 0);
  // 不截断：展示全部条目，按度量值降序
  const list = items.slice().sort((a, b) => val(b) - val(a));
  const max = Math.max(...list.map(val), 1);

  el.innerHTML = list.map(i => {
    const v = val(i);
    const pct = Math.max(1, Math.round(v * 100 / max));
    const bad = !opts.showTokens && (i.failed || 0) > 0;
    // 主值：条形对应的度量；副值：失败数 + 平均延迟
    const main = opts.showTokens
      ? '↓' + fmt(i.prompt_tokens) + ' ↑' + fmt(i.completion_tokens)
      : fmt(i.count || 0) + ' 次';
    const extra = [
      i.failed ? '失败 ' + fmt(i.failed) : '',
      i.avg_latency_ms != null ? '平均 ' + fmtLatency(i.avg_latency_ms) : ''
    ].filter(Boolean).join(' · ');
    return '<div class="hrow">' +
      '<div class="hname" title="' + esc(i.name) + '">' + esc(i.name) + '</div>' +
      '<div class="hbar-track"><div class="hbar' + (bad ? ' bad' : '') + '" style="width:' + pct + '%"></div></div>' +
      '<div class="hinfo">' + main + (extra ? ' <span class="hm">· ' + extra + '</span>' : '') + '</div>' +
      '</div>';
  }).join('');
}

/* ===== 折线图（Canvas 绘制：文字固定像素不变形、随主题变色、窗口缩放自适应） ===== */
const _lineCharts = {};

// 首页聚合分析折线图：语义与 renderColumns 完全一致，仅把「柱」换成「折线 + 面积」
function renderLine(elId, items, opts = {}) {
  const el = $(elId);
  if (!el) return;
  if (!items || !items.length) { el.innerHTML = '<div class="muted">暂无数据</div>'; delete _lineCharts[elId]; return; }
  _lineCharts[elId] = { items, opts };
  drawLineChart(el, items, opts);
}

function drawLineChart(el, items, opts) {
  const val = i => opts.showTokens ? ((i.prompt_tokens || 0) + (i.completion_tokens || 0)) : (i.count || 0);
  const list = items.slice().sort((a, b) => val(b) - val(a)).slice(0, opts.top || 12);
  const max = Math.max(...list.map(val), 1);
  const n = list.length;

  let cv = el.querySelector('canvas');
  if (!cv) { cv = document.createElement('canvas'); el.innerHTML = ''; el.appendChild(cv); }
  const dpr = window.devicePixelRatio || 1;
  const w = Math.max(el.clientWidth, 240);
  const h = 220;
  cv.width = Math.round(w * dpr);
  cv.height = Math.round(h * dpr);
  cv.style.width = w + 'px';
  cv.style.height = h + 'px';
  const ctx = cv.getContext('2d');
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  ctx.clearRect(0, 0, w, h);

  const cs = getComputedStyle(document.documentElement);
  const primary = cs.getPropertyValue('--primary').trim();
  const muted = cs.getPropertyValue('--muted').trim();
  const err = cs.getPropertyValue('--err').trim();
  const panel = cs.getPropertyValue('--panel').trim();
  const border = cs.getPropertyValue('--border').trim();

  const padL = 46, padR = 20, padT = 24, padB = 30;
  const iw = w - padL - padR, ih = h - padT - padB;
  const pts = list.map((i, idx) => {
    const x = n > 1 ? padL + idx * iw / (n - 1) : padL + iw / 2;
    const y = padT + ih - (val(i) / max) * ih;
    return { x, y, i };
  });
  const font = '11px -apple-system, "Segoe UI", Roboto, "Microsoft YaHei", sans-serif';

  // 水平网格线 + Y 轴刻度（0 / ½max / max）
  ctx.font = '10px -apple-system, "Segoe UI", Roboto, "Microsoft YaHei", sans-serif';
  ctx.textAlign = 'right';
  ctx.textBaseline = 'middle';
  [0, 0.5, 1].forEach(frac => {
    const y = padT + ih - frac * ih;
    ctx.strokeStyle = border;
    ctx.lineWidth = 1;
    ctx.setLineDash([3, 4]);
    ctx.beginPath();
    ctx.moveTo(padL, y);
    ctx.lineTo(w - padR, y);
    ctx.stroke();
    ctx.setLineDash([]);
    ctx.fillStyle = muted;
    ctx.fillText(fmt(Math.round(max * frac)), padL - 8, y);
  });

  // 面积渐变填充（主色 → 透明）
  const grad = ctx.createLinearGradient(0, padT, 0, padT + ih);
  grad.addColorStop(0, hexToRgba(primary, 0.20));
  grad.addColorStop(1, hexToRgba(primary, 0));
  ctx.beginPath();
  ctx.moveTo(pts[0].x, padT + ih);
  pts.forEach(p => ctx.lineTo(p.x, p.y));
  ctx.lineTo(pts[pts.length - 1].x, padT + ih);
  ctx.closePath();
  ctx.fillStyle = grad;
  ctx.fill();

  // 折线
  ctx.beginPath();
  pts.forEach((p, k) => k ? ctx.lineTo(p.x, p.y) : ctx.moveTo(p.x, p.y));
  ctx.strokeStyle = primary;
  ctx.lineWidth = 2;
  ctx.lineJoin = 'round';
  ctx.lineCap = 'round';
  ctx.stroke();

  const label = i => opts.showTokens ? fmt((i.prompt_tokens || 0) + (i.completion_tokens || 0))
    : opts.showLatency ? fmtLatency(i.avg_latency_ms) : fmt(i.count || 0);

  ctx.font = font;
  ctx.textAlign = 'center';
  ctx.textBaseline = 'alphabetic';
  pts.forEach(p => {
    const bad = !opts.showTokens && (p.i.failed || 0) > 0;
    // 数据点
    ctx.beginPath();
    ctx.arc(p.x, p.y, 4, 0, Math.PI * 2);
    ctx.fillStyle = bad ? err : panel;
    ctx.strokeStyle = bad ? err : primary;
    ctx.lineWidth = 2;
    ctx.fill();
    ctx.stroke();
    // 数值（点上方）
    ctx.fillStyle = muted;
    ctx.fillText(label(p.i), p.x, p.y - 9);
    // 名称（底部）
    ctx.fillStyle = muted;
    ctx.fillText(String(p.i.name || '').slice(0, 10), p.x, h - 8);
  });
}

// hex(#rrggbb) → rgba(r,g,b,a)，用于把主题色转成半透明面积填充
function hexToRgba(hex, a) {
  const m = /^#?([0-9a-f]{6})$/i.exec((hex || '').trim());
  if (!m) return 'rgba(47,129,247,' + a + ')';
  const n = parseInt(m[1], 16);
  return 'rgba(' + ((n >> 16) & 255) + ',' + ((n >> 8) & 255) + ',' + (n & 255) + ',' + a + ')';
}

/* ===== 趋势图（按小时的时间序列折线图） ===== */
const _trendCharts = {};

// renderTrend 渲染最近若干小时的趋势（X 轴为时间）。
// series: [{ts, requests, success, failed, prompt_tokens, completion_tokens}]
// opts.metric: 'requests'（默认，请求次数）| 'tokens'（输入+输出 token）
function renderTrend(elId, series, opts = {}) {
  const el = $(elId);
  if (!el) return;
  if (!series || !series.length) {
    el.innerHTML = '<div class="muted" style="padding:12px 0">暂无趋势数据（用量统计按小时累积）</div>';
    delete _trendCharts[elId];
    return;
  }
  _trendCharts[elId] = { series, opts };
  drawTrend(el, series, opts);
}

function drawTrend(el, series, opts) {
  const val = i => opts.metric === 'tokens'
    ? (i.prompt_tokens || 0) + (i.completion_tokens || 0)
    : (i.requests || 0);
  const max = Math.max(...series.map(val), 1);
  const n = series.length;

  let cv = el.querySelector('canvas');
  if (!cv) { cv = document.createElement('canvas'); el.innerHTML = ''; el.appendChild(cv); }
  const dpr = window.devicePixelRatio || 1;
  const w = Math.max(el.clientWidth, 240);
  const h = 200;
  cv.width = Math.round(w * dpr);
  cv.height = Math.round(h * dpr);
  cv.style.width = w + 'px';
  cv.style.height = h + 'px';
  const ctx = cv.getContext('2d');
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  ctx.clearRect(0, 0, w, h);

  const cs = getComputedStyle(document.documentElement);
  const primary = cs.getPropertyValue('--primary').trim();
  const muted = cs.getPropertyValue('--muted').trim();
  const err = cs.getPropertyValue('--err').trim();
  const border = cs.getPropertyValue('--border').trim();

  const padL = 50, padR = 14, padT = 18, padB = 26;
  const iw = w - padL - padR, ih = h - padT - padB;
  const pts = series.map((i, idx) => ({
    x: n > 1 ? padL + idx * iw / (n - 1) : padL + iw / 2,
    y: padT + ih - (val(i) / max) * ih,
    i
  }));
  const font = '10px -apple-system, "Segoe UI", Roboto, "Microsoft YaHei", sans-serif';

  // Y 轴网格 + 刻度（0 / ½max / max）
  ctx.font = font;
  ctx.textAlign = 'right';
  ctx.textBaseline = 'middle';
  [0, 0.5, 1].forEach(f => {
    const y = padT + ih - f * ih;
    ctx.strokeStyle = border;
    ctx.lineWidth = 1;
    ctx.setLineDash([3, 4]);
    ctx.beginPath();
    ctx.moveTo(padL, y);
    ctx.lineTo(w - padR, y);
    ctx.stroke();
    ctx.setLineDash([]);
    ctx.fillStyle = muted;
    ctx.fillText(fmt(Math.round(max * f)), padL - 8, y);
  });

  // 面积渐变填充
  const grad = ctx.createLinearGradient(0, padT, 0, padT + ih);
  grad.addColorStop(0, hexToRgba(primary, 0.22));
  grad.addColorStop(1, hexToRgba(primary, 0));
  ctx.beginPath();
  ctx.moveTo(pts[0].x, padT + ih);
  pts.forEach(p => ctx.lineTo(p.x, p.y));
  ctx.lineTo(pts[pts.length - 1].x, padT + ih);
  ctx.closePath();
  ctx.fillStyle = grad;
  ctx.fill();

  // 折线
  ctx.beginPath();
  pts.forEach((p, k) => k ? ctx.lineTo(p.x, p.y) : ctx.moveTo(p.x, p.y));
  ctx.strokeStyle = primary;
  ctx.lineWidth = 2;
  ctx.lineJoin = 'round';
  ctx.stroke();

  // 失败点标红：这些小时里有请求失败
  pts.forEach(p => {
    if (!p.i.failed) return;
    ctx.beginPath();
    ctx.arc(p.x, p.y, 3, 0, Math.PI * 2);
    ctx.fillStyle = err;
    ctx.fill();
  });

  // 峰值数值标注
  const peak = pts.reduce((a, b) => (val(b.i) > val(a.i) ? b : a), pts[0]);
  if (val(peak.i) > 0) {
    ctx.font = font;
    ctx.textAlign = 'center';
    ctx.textBaseline = 'alphabetic';
    ctx.fillStyle = muted;
    ctx.fillText(fmt(val(peak.i)), peak.x, Math.max(peak.y - 7, 11));
  }

  // X 轴：小时标签，隔点显示避免拥挤
  ctx.font = font;
  ctx.textAlign = 'center';
  ctx.textBaseline = 'alphabetic';
  const step = Math.max(1, Math.ceil(n / 8));
  pts.forEach((p, idx) => {
    if (idx % step && idx !== n - 1) return;
    const d = new Date(p.i.ts * 1000);
    ctx.fillStyle = muted;
    ctx.fillText(String(d.getHours()).padStart(2, '0') + ':00', p.x, h - 8);
  });
}

// 窗口缩放时按新宽度重绘所有折线图（测试沙箱可能没有 addEventListener，加保护）
if (window.addEventListener) {
  window.addEventListener('resize', () => {
    Object.keys(_lineCharts).forEach(id => {
      const el = $(id);
      if (el && _lineCharts[id]) drawLineChart(el, _lineCharts[id].items, _lineCharts[id].opts);
    });
    Object.keys(_trendCharts).forEach(id => {
      const el = $(id);
      if (el && _trendCharts[id]) drawTrend(el, _trendCharts[id].series, _trendCharts[id].opts);
    });
  });
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
