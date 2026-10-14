/* apitoken 对话测试页逻辑：直接调用网关对外的 OpenAI 兼容接口 */
(function () {
  const $ = id => document.getElementById(id);
  const state = { messages: [], controller: null, models: [] };

  function base() {
    let b = ($('base').value || '').trim().replace(/\/+$/, '');
    if (!b) b = location.origin;
    return b;
  }
  /** 地址里已含 completions 视为「对话全路径」，直接使用、不再拼接 */
  function isChatFullPath(b) {
    return b.indexOf('completions') !== -1;
  }
  /** 拼接 OpenAI 兼容路径：base 已带 /v1 时不重复加 */
  function apiUrl(suffix) {
    const s = '/' + suffix.replace(/^\/+/, '');
    return /\/v\d+$/i.test(base()) ? base() + s : base() + '/v1' + s;
  }
  /** 拉模型用：全路径时剥掉尾部 /chat/completions（兼容 /completions）再拼 models */
  function modelsUrl() {
    if (!isChatFullPath(base())) return apiUrl('/models');
    return base().replace(/\/(chat\/)?completions$/i, '') + '/models';
  }
  function proto() { return ($('proto') && $('proto').value) || 'openai'; }
  /** 按协议返回请求路径；OpenAI 保留「填完整 completions 地址」的旧用法 */
  function protoUrl() {
    const b = base();
    if (proto() === 'openai' && isChatFullPath(b)) return b;
    const suffix = proto() === 'openai' ? '/chat/completions' : proto() === 'anthropic' ? '/messages' : '/responses';
    return /\/v\d+$/i.test(b) ? b + suffix : b + '/v1' + suffix;
  }
  /** 协议切换：更新说明文案 */
  function onProtoChange() {
    const p = proto();
    const hint = $('protoHint');
    if (!hint) return;
    hint.textContent = p === 'openai' ? '通用对话协议，绝大多数 SDK 默认支持。'
      : p === 'anthropic' ? 'Anthropic 协议，网关自动与上游 OpenAI 端点互转。'
      : 'Codex 协议（Responses），供 OpenAI Codex CLI 等 coding agent 使用。';
  }
  function key() { return ($('key').value || '').trim(); }
  function headers(extra) {
    const h = Object.assign({ 'Content-Type': 'application/json' }, extra || {});
    if (key()) h['Authorization'] = 'Bearer ' + key();
    if (proto() === 'anthropic') h['anthropic-version'] = '2023-06-01';
    return h;
  }

  function setStatus(kind, text) {
    $('status').innerHTML = '<span class="dot ' + kind + '"></span>' + esc(text);
  }

  /* ===== 连接配置历史（仅存浏览器 localStorage，不上服务端） ===== */
  const CONN_KEY = 'apitoken_conns';
  const CONN_MAX = 20;

  function loadConns() {
    try {
      const list = JSON.parse(localStorage.getItem(CONN_KEY) || '[]');
      return Array.isArray(list) ? list : [];
    } catch (e) { return []; }
  }
  function storeConns(list) {
    localStorage.setItem(CONN_KEY, JSON.stringify(list.slice(0, CONN_MAX)));
  }
  /** 按地址去重保存一条连接（最近的排最前），key 随之更新 */
  function upsertConn(b, k) {
    if (!b) return;
    const list = loadConns().filter(c => c.base !== b);
    list.unshift({ base: b, key: k || '', ts: Date.now() });
    storeConns(list);
  }
  function removeConn(b) {
    storeConns(loadConns().filter(c => c.base !== b));
  }
  /** 下拉文案：去掉协议头，过长截断 */
  function connLabel(b) {
    const s = b.replace(/^https?:\/\//i, '');
    return s.length > 32 ? s.slice(0, 32) + '…' : s;
  }
  function renderConns() {
    const sel = $('connSel');
    if (!sel) return;
    const list = loadConns();
    const cur = base();
    let html = '<option value="">历史连接（' + list.length + '）</option>';
    html += list.map(c =>
      '<option value="' + esc(c.base) + '"' + (c.base === cur ? ' selected' : '') + '>' +
      esc(connLabel(c.base)) + '</option>'
    ).join('');
    sel.innerHTML = html;
  }
  /** 选中历史连接：回填输入框、保存为当前连接并立即拉取模型 */
  function pickConn(v) {
    if (!v) return;
    const c = loadConns().find(x => x.base === v);
    if (!c) return;
    $('base').value = c.base;
    $('key').value = c.key || '';
    saveConn();
    loadModels();
  }
  function delConn() {
    const sel = $('connSel');
    const v = sel ? sel.value : '';
    if (!v) { toast('先在下拉里选中要删除的历史连接', true); return; }
    removeConn(v);
    renderConns();
    toast('已删除该历史连接');
  }

  function saveConn() {
    localStorage.setItem('apitoken_base', $('base').value.trim());
    localStorage.setItem('apitoken_key', key());
    upsertConn(base(), key());
    renderConns();
    toast('连接信息已保存到本地');
  }

  function restore() {
    $('base').value = localStorage.getItem('apitoken_base') || location.origin;
    $('key').value = localStorage.getItem('apitoken_key') || '';
  }

  /* ===== 模型下拉（复用 /static/combo.js，与控制台同一套组件） ===== */
  function modelChannelText(m) {
    return (m.upstreams || []).map(u => u.channel_id).join(', ');
  }

  function modelOptions() {
    return (state.models || []).map(m => ({
      value: m.id,
      label: m.id,
      hint: modelChannelText(m),     // 副标题=可用渠道，可用渠道名反查模型
    }));
  }

  /** 搜索计数提示（Combo 只负责渲染，统计文案由页面自己维护） */
  function updateModelHint(kw, hit, total) {
    const hint = $('modelHint');
    if (!hint) return;
    hint.textContent = kw
      ? '匹配 ' + hit + ' / ' + total + ' 个模型'
      : (total ? '共 ' + total + ' 个模型，输入关键词可快速筛选' : '');
  }

  let modelCombo = null;

  function initModelCombo() {
    const inp = $('model');
    if (!inp || !window.Combo) return null;
    modelCombo = Combo.attach(inp, {
      getOptions: modelOptions,
      onRender: updateModelHint,
      onPick: function (id) {
        if (!id) return;
        $('title').textContent = id;
      },
      emptyText: '网关未返回模型，请先点「拉取模型」',
    });
    // 回填页面里预置的值（通常是空）
    modelCombo.setValue(inp.value || '', inp.value || '');
    modelCombo.refresh();
    return modelCombo;
  }

  async function loadModels() {
    setStatus('wait', '连接中…');
    try {
      const res = await fetch(modelsUrl(), { headers: headers() });
      const data = await res.json().catch(() => null);
      if (!res.ok) {
        const msg = (data && data.error && data.error.message) || ('HTTP ' + res.status);
        throw new Error(msg);
      }
      state.models = data.data || [];
      // 默认沿用当前选择；没有则选第一个
      const cur = Combo.value($('model'));
      if (!state.models.some(m => m.id === cur)) {
        const first = state.models.length ? state.models[0].id : '';
        if (modelCombo) modelCombo.setValue(first, first);
        else $('model').value = first;
      } else if (modelCombo) {
        modelCombo.refresh();
      }
      setStatus('on', '已连接 · ' + state.models.length + ' 个模型');
      $('title').textContent = state.models.length ? $('model').value : '未获取到模型';
      // 连接成功即记入历史，下次可从下拉一键切换
      upsertConn(base(), key());
      renderConns();
      toast('已拉取 ' + state.models.length + ' 个模型');
    } catch (e) {
      setStatus('off', '连接失败');
      toast('拉取模型失败：' + e.message, true);
    }
  }

  function quick(text) {
    $('input').value = text;
    send();
  }

  function addMsg(role, content) {
    const msg = { role, content, meta: null, el: null };
    state.messages.push(msg);
    renderMsg(msg);
    scrollBottom();
    return msg;
  }

  function renderMsg(msg) {
    const wrap = document.createElement('div');
    wrap.className = 'msg ' + (msg.role === 'user' ? 'user' : msg.role === 'error' ? 'err' : 'bot');
    const who = msg.role === 'user' ? '我' : msg.role === 'error' ? '错误' : '助手';
    wrap.innerHTML = '<div class="who">' + who + '</div><div class="bubble"></div><div class="meta"></div>';
    wrap.querySelector('.bubble').textContent = msg.content;
    $('body').appendChild(wrap);
    msg.el = wrap;
    return wrap;
  }

  function setBubble(msg, text) {
    msg.content = text;
    msg.el.querySelector('.bubble').textContent = text;
    scrollBottom();
  }

  function setMeta(msg, items) {
    msg.meta = items;
    msg.el.querySelector('.meta').innerHTML = items.filter(Boolean)
      .map(i => '<span>' + i + '</span>').join('');
  }

  function scrollBottom() {
    const b = $('body');
    b.scrollTop = b.scrollHeight;
  }

  function clearChat() {
    state.messages = [];
    $('body').innerHTML = '';
    $('lastMeta').textContent = '';
  }

  function stopStream() {
    if (state.controller) {
      state.controller.abort();
      state.controller = null;
    }
  }

  function buildBody() {
    const sys = $('system').value.trim();
    const stream = $('mode').value === '1';
    const t = parseFloat($('temp').value);
    const mt = parseInt($('maxTok').value, 10);
    const hist = state.messages.filter(m => (m.role === 'user' || m.role === 'assistant') && m.content);

    if (proto() === 'anthropic') {
      const body = {
        model: $('model').value,
        max_tokens: (isNaN(mt) || mt <= 0) ? 1024 : mt,
        messages: hist.map(m => ({ role: m.role, content: m.content })),
        stream
      };
      if (sys) body.system = sys;
      if (!isNaN(t)) body.temperature = t;
      return body;
    }

    if (proto() === 'responses') {
      const body = {
        model: $('model').value,
        input: hist.map(m => m.role === 'user'
          ? { type: 'message', role: 'user', content: [{ type: 'input_text', text: m.content }] }
          : { type: 'message', role: 'assistant', content: [{ type: 'output_text', text: m.content }] }),
        stream
      };
      if (sys) body.instructions = sys;
      if (!isNaN(t)) body.temperature = t;
      if (!isNaN(mt) && mt > 0) body.max_output_tokens = mt;
      return body;
    }

    // OpenAI
    const msgs = [];
    if (sys) msgs.push({ role: 'system', content: sys });
    hist.forEach(m => msgs.push({ role: m.role, content: m.content }));
    const body = { model: $('model').value, messages: msgs, stream };
    if (!isNaN(t)) body.temperature = t;
    if (!isNaN(mt) && mt > 0) body.max_tokens = mt;
    return body;
  }

  /** 非流式：按协议提取正文 */
  function parseNonStream(data, p) {
    if (p === 'anthropic') {
      return (data.content || []).filter(b => b.type === 'text').map(b => b.text).join('');
    }
    if (p === 'responses') {
      let text = '';
      for (const item of (data.output || [])) {
        if (item.type !== 'message') continue;
        for (const c of (item.content || [])) {
          if (c.type === 'output_text') text += c.text;
        }
      }
      return text;
    }
    return (data.choices && data.choices[0] && data.choices[0].message && data.choices[0].message.content) || '';
  }

  /** 流式：按协议从一个 data 块里提取文本增量 */
  function streamDelta(j, p) {
    if (p === 'anthropic') {
      return (j.type === 'content_block_delta' && j.delta && j.delta.type === 'text_delta') ? (j.delta.text || '') : '';
    }
    if (p === 'responses') {
      return j.type === 'response.output_text.delta' ? (j.delta || '') : '';
    }
    return (j.choices && j.choices[0] && j.choices[0].delta && j.choices[0].delta.content) || '';
  }

  /** 流式：按协议提取 usage */
  function streamUsage(j, p) {
    if (p === 'responses') return j.response && j.response.usage || null;
    return j.usage || null;
  }

  /** 统一成 {prompt_tokens, completion_tokens} 结构 */
  function normalizeUsage(u) {
    if (!u) return null;
    return {
      prompt_tokens: (u.prompt_tokens ?? u.input_tokens ?? 0),
      completion_tokens: (u.completion_tokens ?? u.output_tokens ?? 0)
    };
  }

  async function send() {
    const text = $('input').value.trim();
    if (!text) return;
    if (!$('model').value) {
      toast('请先拉取并选择模型', true);
      return;
    }
    $('input').value = '';
    addMsg('user', text);
    const bot = addMsg('assistant', '');
    bot.el.querySelector('.bubble').classList.add('cursor');

    const t0 = performance.now();
    let first = 0;
    const body = buildBody();
    state.controller = new AbortController();

    try {
      const res = await fetch(protoUrl(), {
        method: 'POST',
        headers: headers(),
        body: JSON.stringify(body),
        signal: state.controller.signal
      });
      const rid = res.headers.get('X-Request-Id') || '';
      const ch = res.headers.get('X-Gateway-Channel') || '';
      const chName = res.headers.get('X-Gateway-Channel-Name') || '';
      const upModel = res.headers.get('X-Gateway-Upstream-Model') || '';

      if (!res.ok) {
        const data = await res.json().catch(() => null);
        const msg = (data && data.error && data.error.message) || ('HTTP ' + res.status);
        bot.el.className = 'msg err';
        setBubble(bot, msg);
        setMeta(bot, [rid ? 'request_id ' + rid : '', ch ? '渠道 ' + ch : '', Math.round(performance.now() - t0) + ' ms']);
        state.controller = null;
        return;
      }

      if (body.stream) {
        const reader = res.body.getReader();
        const dec = new TextDecoder();
        let buf = '', out = '', usage = null;
        for (; ;) {
          const { done, value } = await reader.read();
          if (done) break;
          buf += dec.decode(value, { stream: true });
          const chunks = buf.split('\n\n');
          buf = chunks.pop();
          for (const c of chunks) {
            for (const line of c.split('\n')) {
              if (!line.startsWith('data:')) continue;
              const payload = line.slice(5).trim();
              if (!payload || payload === '[DONE]') continue;
              let j;
              try { j = JSON.parse(payload); } catch (e) { continue; }
              if (!first) first = performance.now();
              const d = streamDelta(j, proto());
              if (d) { out += d; setBubble(bot, out); }
              const u = streamUsage(j, proto());
              if (u) usage = normalizeUsage(u);
            }
          }
        }
        if (!out) setBubble(bot, '（上游没有返回内容）');
        bot.el.querySelector('.bubble').classList.remove('cursor');
        setMeta(bot, metaItems(rid, ch || chName, upModel, usage, t0, first));
      } else {
        const data = await res.json();
        const msg = parseNonStream(data, proto()) || '（空回复）';
        setBubble(bot, msg);
        bot.el.querySelector('.bubble').classList.remove('cursor');
        setMeta(bot, metaItems(rid, ch || chName, upModel, normalizeUsage(data.usage), t0, 0));
      }
      $('title').textContent = $('model').value;
    } catch (e) {
      bot.el.className = 'msg err';
      setBubble(bot, e.name === 'AbortError' ? '（已停止）' + bot.content : (e.message || String(e)));
      bot.el.querySelector('.bubble').classList.remove('cursor');
      setMeta(bot, [Math.round(performance.now() - t0) + ' ms']);
    } finally {
      state.controller = null;
      setStatus('on', '已连接 · ' + state.models.length + ' 个模型');
    }
  }

  function metaItems(rid, ch, upModel, usage, t0, first) {
    const total = usage ? (usage.prompt_tokens || 0) + (usage.completion_tokens || 0) : 0;
    return [
      rid ? 'request_id ' + rid : '',
      ch ? '渠道 ' + ch : '',
      upModel ? '上游模型 ' + upModel : '',
      total ? 'token ' + fmt(usage.prompt_tokens || 0) + '→' + fmt(usage.completion_tokens || 0) : '',
      first ? '首字 ' + Math.round(first - t0) + ' ms' : '',
      '总耗时 ' + Math.round(performance.now() - t0) + ' ms'
    ];
  }

  // 暴露给 HTML 内联按钮
  window.saveConn = saveConn;
  window.loadModels = loadModels;
  window.quick = quick;
  window.send = send;
  window.stopStream = stopStream;
  window.clearChat = clearChat;
  window.pickConn = pickConn;
  window.delConn = delConn;
  window.onProtoChange = onProtoChange;

  $('input').addEventListener('keydown', e => {
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault();
      send();
    }
  });
  initModelCombo();
  onProtoChange();

  restore();
  renderConns();
  setStatus('off', '未连接');
  loadModels();
})();
