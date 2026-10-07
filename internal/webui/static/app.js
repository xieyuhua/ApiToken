/* apitoken 管理端公共脚本 */
const $ = id => document.getElementById(id);

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
