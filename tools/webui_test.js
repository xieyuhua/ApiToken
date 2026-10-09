/**
 * webui_test.js —— 前端零依赖回归测试（Node ≥ 16，无需 npm install）
 *
 * 覆盖 WebUI 里最容易回归、又无法靠 Go 测试覆盖的交互逻辑：
 *   1. Combo 组件：过滤 / 高亮 / 分组 / 截断提示 / 键盘导航 / 卸载
 *   2. 自由输入（对外模型名可自定义）：零命中采纳、有命中不误改
 *   3. 路由弹窗：编辑保存的是更新而非新增、改名需确认、标题随状态变化
 *
 * 运行：node tools/webui_test.js
 */
'use strict';

const fs = require('fs');
const path = require('path');

const ROOT = path.resolve(__dirname, '..');
const read = p => fs.readFileSync(path.join(ROOT, p), 'utf8');

let failed = 0;
let passed = 0;
function assert(cond, msg) {
  if (cond) { passed++; console.log('  ok   ' + msg); }
  else { failed++; console.error('  FAIL ' + msg); }
}
function section(name) { console.log('\n== ' + name); }

/* ------------------------------------------------------------------ *
 * 极简 DOM stub：只实现 combo 与路由弹窗用到的 API
 * ------------------------------------------------------------------ */
class ClassList {
  constructor() { this.s = new Set(); }
  add(...c) { c.forEach(x => x && this.s.add(x)); }
  remove(...c) { c.forEach(x => this.s.delete(x)); }
  contains(c) { return this.s.has(c); }
  toggle(c) { this.s.has(c) ? this.s.delete(c) : this.s.add(c); }
}
class El {
  constructor(tag) {
    this.tagName = (tag || 'div').toUpperCase();
    this.children = []; this.attrs = {}; this.dataset = {}; this.style = {};
    this._html = ''; this._text = ''; this.value = ''; this.title = '';
    this.classList = new ClassList(); this.handlers = {}; this.parentElement = null;
  }
  get innerHTML() { return this._html; }
  set innerHTML(v) {
    this._html = String(v);
    this.children = [];
    // 把渲染出的 combo-item 变成可被 querySelector 命中的伪节点
    const re = /<div class="(combo-item[^"]*)"([^>]*)>/g;
    let m;
    while ((m = re.exec(this._html))) {
      const el = new El('div');
      el.className = m[1];
      const dv = /data-value="([^"]*)"/.exec(m[2]);
      const dl = /data-label="([^"]*)"/.exec(m[2]);
      if (dv) el.setAttribute('data-value', dv[1]);
      if (dl) el.setAttribute('data-label', dl[1]);
      el.parentElement = this;
      this.children.push(el);
    }
  }
  set textContent(v) { this._text = String(v); this._html = ''; }
  get textContent() { return this._text; }
  set className(v) { this.classList.s = new Set(String(v).split(/\s+/).filter(Boolean)); }
  get className() { return Array.from(this.classList.s).join(' '); }
  appendChild(c) { this.children.push(c); c.parentElement = this; return c; }
  insertBefore(n) { this.children.unshift(n); return n; }
  removeChild() {} remove() {}
  setAttribute(k, v) { this.attrs[k] = v; }
  getAttribute(k) { return k in this.attrs ? this.attrs[k] : null; }
  closest() { return null; }
  contains(n) { return n === this || this.children.some(c => c.contains && c.contains(n)); }
  querySelector(sel) { return this.querySelectorAll(sel)[0] || null; }
  querySelectorAll(sel) {
    const out = [];
    const want = sel.replace(/^\./, '').split('.');
    const walk = n => (n.children || []).forEach(c => {
      const cls = String(c.className || '').split(/\s+/);
      if (want.every(w => cls.includes(w))) out.push(c);
      walk(c);
    });
    walk(this);
    return out;
  }
  insertAdjacentHTML(pos, html) { this.innerHTML += html; }
  addEventListener(t, fn) { (this.handlers[t] = this.handlers[t] || []).push(fn); }
  removeEventListener(t, fn) {
    const a = this.handlers[t];
    if (!a) return;
    const i = a.indexOf(fn);
    if (i >= 0) a.splice(i, 1);
  }
  dispatch(t, ev) { (this.handlers[t] || []).slice().forEach(fn => fn(ev || {})); }
  focus() { this.dispatch('focus'); }
  showModal() { this.open = true; }
  close() { this.open = false; }
}
const docHandlers = {};
const store = {};
function el(id) { return (store[id] = store[id] || new El('div')); }

function installDom() {
  global.document = {
    documentElement: new El('html'),
    body: new El('body'),
    createElement: t => new El(t),
    getElementById: id => el(id),
    querySelector: () => null,
    addEventListener: (t, fn) => { (docHandlers[t] = docHandlers[t] || []).push(fn); },
    removeEventListener: (t, fn) => {
      const a = docHandlers[t];
      if (!a) return;
      const i = a.indexOf(fn);
      if (i >= 0) a.splice(i, 1);
    },
  };
  global.window = global;
  global.location = { origin: 'http://127.0.0.1:8080' };
  global.localStorage = { getItem: () => '', setItem: () => {} };
  global.setInterval = () => 0;
  global.setTimeout = () => 0;
  global.clearTimeout = () => {};
  global.confirm = () => true;
}

/* ------------------------------------------------------------------ *
 * 1. Combo 组件
 * ------------------------------------------------------------------ */
function testCombo() {
  section('Combo 组件');
  installDom();

  const names = [];
  for (let i = 0; i < 469; i++) names.push('space-bunny/model-' + i);
  ['gpt-4o', 'gpt-4o-mini', 'claude-3-5-sonnet', 'deepseek-chat'].forEach(n => names.push(n));

  const box = new El('div');
  box.className = 'combo';
  const input = new El('input');
  box.appendChild(input);
  const picked = [];
  const ctl = Combo.attach(input, {
    allowFree: true,
    getOptions: () => names.map(m => ({
      value: m, label: m, hint: m.indexOf('space-bunny/') === 0 ? '渠道 sb' : '',
    })),
    onPick: (v, o) => picked.push([v, !!(o && o.free)]),
  });

  const type = v => { input.value = v; input.dataset.kw = v; input.dispatch('input'); };
  const key = k => input.dispatch('keydown', { key: k, preventDefault() {} });

  // 过滤 + hint（渠道名）匹配
  type('space-bunny/model-1');
  assert(/model-1\b/.test(ctl.list.innerHTML), '按模型名过滤');
  type('渠道 sb');
  assert(/space-bunny\/model-0/.test(ctl.list.innerHTML), '按渠道名（hint）反查模型');
  assert(/仅显示前 300 个/.test(ctl.list.innerHTML), '超量截断有提示');

  // 命中高亮
  type('gpt-4o');
  assert(/<mark>gpt-4o<\/mark>/.test(ctl.list.innerHTML), '命中关键词高亮');

  // 分组
  const gbox = new El('div');
  gbox.className = 'combo';
  const ginput = new El('input');
  gbox.appendChild(ginput);
  const gctl = Combo.attach(ginput, {
    getOptions: () => [
      { value: 'a', label: 'a', group: 'G1' },
      { value: 'b', label: 'b', group: 'G2' },
    ],
  });
  ginput.value = '';
  ginput.dispatch('input');
  assert(/combo-group">G1/.test(gctl.list.innerHTML) && /combo-group">G2/.test(gctl.list.innerHTML),
    '按 group 输出分组标题');

  // 键盘导航
  ginput.dispatch('focus');
  const press = (target, k) => target.dispatch('keydown', { key: k, preventDefault() {} });
  press(ginput, 'ArrowDown');
  assert(!!gctl.list.querySelector('.combo-item.active'), 'ArrowDown 选中第一项');
  press(ginput, 'ArrowDown');
  const act = gctl.list.querySelectorAll('.combo-item').findIndex(el =>
    el.className.split(/\s+/).includes('active'));
  assert(act === 1, 'ArrowDown 移动到第二项，实际第 ' + (act + 1) + ' 项');
  press(ginput, 'ArrowUp');
  assert(gctl.list.querySelectorAll('.combo-item').findIndex(el =>
    el.className.split(/\s+/).includes('active')) === 0, 'ArrowUp 回到第一项');
  press(ginput, 'Escape');
  assert(ginput.value === '' && !gctl.isOpen(), 'Esc 收起并清空未选中项');

  // 自由输入：零命中才自动采纳
  type('my-brand-v2');
  assert(/combo-item free/.test(ctl.list.innerHTML), '零命中时置顶出现「使用自定义名称」');
  assert(ctl.list.innerHTML.indexOf('combo-item free') < 40, '自定义项在列表最顶部');
  assert(input.dataset.free === '1', '零命中标记可自动采纳');
  ctl.close();
  assert(Combo.value(input) === 'my-brand-v2', '零命中关闭时采纳自定义名');
  assert(picked.length === 1 && picked[0][1] === true, '自定义名触发 onPick(free:true)');

  // 自由输入：有命中（正在搜索）不得改值 —— 否则编辑路由会凭空多一条
  picked.length = 0;
  ctl.setValue('gpt-4o', 'gpt-4o');
  type('gpt-4');
  assert(!input.dataset.free, '搜索中不标记自动采纳');
  input.value = 'gpt-4';
  ctl.close();
  assert(Combo.value(input) === 'gpt-4o', '搜索半截词后关闭保持原值：' + Combo.value(input));
  assert(picked.length === 0, '搜索过程不触发 onPick');

  // 搜索中可主动点自定义项
  type('gpt-4');
  key('Enter');
  assert(Combo.value(input) === 'gpt-4', 'Enter 采纳置顶的自定义项');

  // 精确命中 → 无自定义项，Enter 选候选
  picked.length = 0;
  type('gpt-4o-mini');
  assert(!/combo-item free/.test(ctl.list.innerHTML), '精确命中时不出现自定义项');
  key('Enter');
  assert(Combo.value(input) === 'gpt-4o-mini' && picked[0][1] === false, 'Enter 选中候选');

  // 未输入时关闭，提示文案不得被当成值
  ctl.setValue('', '— 选择模型 —');
  ctl.open();
  ctl.close();
  assert(Combo.value(input) === '' && input.value === '', '空值关闭后仍为空');

  // 重复挂载不泄漏 document 监听器
  section('Combo 卸载 / 重复挂载');
  const before = (docHandlers.click || []).length;
  for (let i = 0; i < 5; i++) Combo.attach(input, { getOptions: () => names.map(m => ({ value: m, label: m })) });
  const after = (docHandlers.click || []).length;
  assert(after === before, '重复 attach 后 document click 监听器不增长（' + before + ' → ' + after + '）');

  const stale = [];
  const c2 = Combo.attach(input, {
    getOptions: () => [{ value: 'x1', label: 'x1' }],
    onPick: v => stale.push(v),
  });
  assert((docHandlers.click || []).length === before, '再次挂载仍只保留一个监听器');
  c2.list.innerHTML = '';
  type('x1');   // 旧实例若还在，会往同一个 list 写内容
  assert(/combo-item/.test(c2.list.innerHTML), '只有最新实例在渲染列表');
  assert(stale.length === 0, '旧实例回调不再触发');
  Combo.detach(input);
  assert((docHandlers.click || []).length === before - 1, 'detach 后监听器被摘除');
}

/* ------------------------------------------------------------------ *
 * 2. 路由弹窗（index.html 内联脚本 + combo.js）
 * ------------------------------------------------------------------ */
function testRouteDialog() {
  section('路由弹窗：编辑必须落在同名路由上');
  installDom();

  const CHANNELS = [{
    id: 'ds1', name: 'DeepSeek', base_url: 'https://api.deepseek.com',
    models: ['gpt-4o', 'deepseek-chat', 'claude-3'],
  }];
  const ROUTES = [{ model: 'gpt-4o', channels: ['ds1'], model_map: { ds1: 'gpt-4o' }, enabled: true }];
  const posted = [];
  function respond(p) {
    if (p.startsWith('/admin/channels')) return CHANNELS;
    if (p.startsWith('/admin/routes')) return ROUTES;
    if (p.startsWith('/admin/logs/facets')) return { models: ['gpt-4o', 'deepseek-chat', 'custom-9'] };
    if (p.startsWith('/admin/api-info')) return { version: 'v1', uptime: '1s', routing: { strategy: 'order' } };
    if (p.startsWith('/admin/stats')) return {
      total_requests: 1, total_success: 1, total_failed: 0, prompt_tokens: 1, completion_tokens: 1,
      avg_latency_ms: 10, enabled_channels: 1, channel_count: 1, models: {}, uptime_seconds: 60,
      started_at: new Date().toISOString(),
    };
    return {};
  }
  global.fetch = (p, opts) => {
    if (opts && opts.method === 'POST') posted.push({ p, body: opts.body });
    const data = p.includes('/admin/routes') && opts && opts.method === 'POST'
      ? JSON.parse(opts.body) : respond(p);
    return Promise.resolve({ ok: true, status: 200, text: () => Promise.resolve(JSON.stringify(data)) });
  };

  const html = read('internal/webui/index.html');
  const inline = html.split('<script>').pop().split('</script>')[0];
  // app.js + combo.js + 页面内联脚本必须放在同一次 eval 里（共享 const 作用域）
  (0, eval)([read('internal/webui/static/app.js'), read('internal/webui/static/combo.js'), inline].join('\n;\n'));

  const settle = async (n) => { for (let i = 0; i < (n || 12); i++) await new Promise(r => setImmediate(r)); };
  const clickOutside = () => (docHandlers.click || []).forEach(fn => fn({ target: el('toast') }));
  const save = async () => { clickOutside(); await saveRoute(); await settle(2); };
  const models = () => posted.map(x => JSON.parse(x.body).model);

  return (async () => {
    await settle(4);

    // 编辑：原样保存 → 同名 upsert（= 更新）
    posted.length = 0;
    await openRouteDlg('gpt-4o');
    await settle();
    assert(routeModelValue() === 'gpt-4o', '编辑弹窗回填原对外模型名');
    assert(el('r_model').value === 'gpt-4o', '输入框显示原名');
    await save();
    assert(models().length === 1 && models()[0] === 'gpt-4o',
      '原样保存提交同名路由（=更新），实际 ' + JSON.stringify(models()));

    // 编辑：只搜索不确认 → 不能变成改名
    posted.length = 0;
    await openRouteDlg('gpt-4o');
    await settle();
    const inp = el('r_model');
    inp.value = 'gpt-4';
    inp.dataset.kw = 'gpt-4';
    inp.dispatch('input');
    await settle();
    inp.value = 'gpt-4';
    clickOutside();
    assert(routeModelValue() === 'gpt-4o', '搜索半截词后关闭不改名：' + routeModelValue());
    await save();
    assert(models().length === 1 && models()[0] === 'gpt-4o',
      '搜索误触后保存仍是更新，实际 ' + JSON.stringify(models()));

    // 编辑：真输入列表外的名字 → 采纳自定义名，且需确认改名
    posted.length = 0;
    let confirmed = '';
    global.confirm = msg => { confirmed = msg; return true; };
    await openRouteDlg('gpt-4o');
    await settle();
    inp.value = 'brand-new-model';
    inp.dataset.kw = 'brand-new-model';
    inp.dispatch('input');
    clickOutside();
    assert(routeModelValue() === 'brand-new-model', '零命中输入被采纳为自定义名');
    await save();
    assert(/新增一条/.test(confirmed), '改名会二次确认（说明后果）');
    assert(models().length === 1 && models()[0] === 'brand-new-model', '确认后提交新名字');

    // 取消确认 → 不提交
    posted.length = 0;
    global.confirm = () => false;
    await openRouteDlg('gpt-4o');
    await settle();
    inp.value = 'another-name';
    inp.dataset.kw = 'another-name';
    inp.dispatch('input');
    await save();
    assert(posted.length === 0, '改名取消确认时不提交');

    // 添加：自定义名直接可用（先加一个渠道，否则后端会拒绝保存）
    posted.length = 0;
    global.confirm = () => true;
    await openRouteDlg();
    await settle();
    assert(routeModelValue() === '', '添加弹窗默认留空');
    const pick2 = el('r_channelPick');
    pick2.value = 'DeepSeek';
    pick2.dispatch('input');
    pick2.dispatch('keydown', { key: 'Enter', preventDefault() {} });
    addRouteChannel();   // 选中后需点「添加渠道」
    inp.value = 'my-gateway-model';
    inp.dataset.kw = 'my-gateway-model';
    inp.dispatch('input');
    inp.dispatch('keydown', { key: 'Enter', preventDefault() {} });
    await save();
    assert(models().length === 1 && models()[0] === 'my-gateway-model',
      '添加时可直接输入自定义名，实际 ' + JSON.stringify(models()));

    // 标题随状态变化
    await openRouteDlg('gpt-4o');
    await settle();
    assert(/更新路由/.test(el('rtDlgHead').textContent), '编辑时标题标明「更新路由」');
    await openRouteDlg();
    await settle();
    assert(el('rtDlgHead').textContent === '添加路由', '添加时标题为「添加路由」');
    await copyRoute('gpt-4o');
    await settle();
    assert(/复制自/.test(el('rtDlgHead').textContent), '复制时标题标明来源');
    assert(routeModelValue() === '', '复制时对外模型名留空');
  })();
}

/* ------------------------------------------------------------------ *
 * 3. 静态约束（与 Go 测试互补：这里查文件，Go 那边查 HTTP 响应）
 * ------------------------------------------------------------------ */
function testStatic() {
  section('静态约束');
  const chatJs = read('internal/webui/static/chat.js');
  const chatHtml = read('internal/webui/chat.html');
  const idx = read('internal/webui/index.html');

  assert(/static\/combo\.js/.test(chatHtml), 'chat 页已引入 combo.js');
  assert(!/function renderModelList|function pickModel|MODEL_LIST_MAX/.test(chatJs),
    'chat.js 不再自带一份下拉实现');
  assert(/Combo\.attach/.test(chatJs), 'chat.js 复用 Combo 组件');

  assert(!/__custom__|r_model_custom/.test(idx), '已移除 __custom__ / r_model_custom 占位方案');
  assert(/allowFree: true/.test(idx), '对外模型名启用自由输入');
  assert((idx.match(/allowFree: true/g) || []).length >= 3, '路由弹窗内 3 个可输入下拉都启用自由输入');
  assert(/id="rtDlgHead"/.test(idx) && /routeEditingModel/.test(idx), '更新/新建语义已区分');
}

/* ------------------------------------------------------------------ */
(async () => {
  installDom();
  (0, eval)(read('internal/webui/static/combo.js'));
  testCombo();
  await testRouteDialog();
  testStatic();
  console.log('\n通过 ' + passed + ' 项，失败 ' + failed + ' 项');
  process.exit(failed ? 1 : 0);
})();