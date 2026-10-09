/* apitoken 对话测试页逻辑：直接调用网关对外的 OpenAI 兼容接口 */
(function () {
  const $ = id => document.getElementById(id);
  const state = { messages: [], controller: null, models: [] };

  function base() {
    let b = ($('base').value || '').trim().replace(/\/+$/, '');
    if (!b) b = location.origin;
    return b;
  }
  function key() { return ($('key').value || '').trim(); }
  function headers(extra) {
    const h = Object.assign({ 'Content-Type': 'application/json' }, extra || {});
    if (key()) h['Authorization'] = 'Bearer ' + key();
    return h;
  }

  function setStatus(kind, text) {
    $('status').innerHTML = '<span class="dot ' + kind + '"></span>' + esc(text);
  }

  function saveConn() {
    localStorage.setItem('apitoken_base', $('base').value.trim());
    localStorage.setItem('apitoken_key', key());
    toast('连接信息已保存到本地');
  }

  function restore() {
    $('base').value = localStorage.getItem('apitoken_base') || location.origin;
    $('key').value = localStorage.getItem('apitoken_key') || '';
  }

  /* ===== 模型下拉（可搜索） ===== */
  const MODEL_LIST_MAX = 300;   // 最多渲染多少行，避免模型极多时卡顿

  function modelChannelText(m) {
    return (m.upstreams || []).map(u => u.channel_id).join(', ');
  }

  /** 渲染模型列表；kw 为搜索关键词（模型名或渠道名模糊匹配） */
  function renderModelList(kw) {
    kw = (kw || '').trim().toLowerCase();
    const all = state.models || [];
    const hit = kw
      ? all.filter(m => m.id.toLowerCase().includes(kw) || modelChannelText(m).toLowerCase().includes(kw))
      : all;
    const shown = hit.slice(0, MODEL_LIST_MAX);

    if (!all.length) {
      $('modelList').innerHTML = '<div class="combo-empty">网关未返回模型</div>';
    } else if (!shown.length) {
      $('modelList').innerHTML = '<div class="combo-empty">没有匹配「' + esc(kw) + '」的模型</div>';
    } else {
      const cur = $('model').value;
      $('modelList').innerHTML = shown.map(m => {
        const ups = modelChannelText(m);
        return '<div class="combo-item' + (m.id === cur ? ' on' : '') + '" data-id="' + esc(m.id) + '" title="' + esc(m.id) + '">' +
          '<span class="mid">' + hl(m.id, kw) + '</span>' +
          (ups ? '<span class="mups" title="' + esc(ups) + '">' + esc(ups) + '</span>' : '') +
          '</div>';
      }).join('');
    }
    if (hit.length > shown.length) {
      $('modelList').innerHTML += '<div class="combo-empty">仅显示前 ' + shown.length +
        ' 个（共 ' + hit.length + ' 个命中），请继续输入以缩小范围</div>';
    }

    const hint = $('modelHint');
    if (hint) {
      hint.textContent = kw
        ? '匹配 ' + hit.length + ' / ' + all.length + ' 个模型'
        : (all.length ? '共 ' + all.length + ' 个模型，输入关键词可快速筛选' : '');
    }
  }

  /** 关键词高亮 */
  function hl(text, kw) {
    if (!kw) return esc(text);
    const i = text.toLowerCase().indexOf(kw);
    if (i < 0) return esc(text);
    return esc(text.slice(0, i)) + '<mark>' + esc(text.slice(i, i + kw.length)) + '</mark>' + esc(text.slice(i + kw.length));
  }

  function openModelList() {
    $('modelList').classList.remove('hidden');
    renderModelList($('model').dataset.kw || '');
  }

  function closeModelList() {
    $('modelList').classList.add('hidden');
    // 收起时把输入框还原为已选模型，避免残留搜索词
    const m = (state.models || []).find(x => x.id === $('model').dataset.picked);
    $('model').value = m ? m.id : '';
  }

  function pickModel(id) {
    if (!id) return;
    const inp = $('model');
    inp.value = id;
    inp.dataset.picked = id;
    inp.dataset.kw = '';
    inp.title = id;              // 名称较长时，悬停输入框可看完整模型名
    $('title').textContent = id;
    closeModelList();
  }

  function moveModelHighlight(step) {
    const items = Array.from($('modelList').querySelectorAll('.combo-item'));
    if (!items.length) return;
    let i = items.findIndex(el => el.classList.contains('active'));
    i = i < 0 ? (step > 0 ? 0 : items.length - 1) : Math.min(items.length - 1, Math.max(0, i + step));
    items.forEach(el => el.classList.remove('active'));
    items[i].classList.add('active');
    items[i].scrollIntoView({ block: 'nearest' });
  }

  function initModelCombo() {
    const inp = $('model'), list = $('modelList');
    if (!inp || !list) return;

    inp.dataset.picked = inp.value || '';
    inp.addEventListener('focus', openModelList);
    inp.addEventListener('input', function () {
      inp.dataset.kw = inp.value.trim();
      renderModelList(inp.dataset.kw);
      list.classList.remove('hidden');
    });
    inp.addEventListener('keydown', function (e) {
      if (e.key === 'ArrowDown') { e.preventDefault(); moveModelHighlight(1); }
      else if (e.key === 'ArrowUp') { e.preventDefault(); moveModelHighlight(-1); }
      else if (e.key === 'Enter') {
        const active = list.querySelector('.combo-item.active') || list.querySelector('.combo-item');
        if (active && !list.classList.contains('hidden')) { e.preventDefault(); pickModel(active.dataset.id); }
      } else if (e.key === 'Escape') {
        closeModelList();
      }
    });

    // 用 mousedown 而非 click，避免输入框先 blur 导致列表收起
    list.addEventListener('mousedown', function (e) {
      const it = e.target.closest('.combo-item');
      if (!it) return;
      e.preventDefault();
      pickModel(it.dataset.id);
    });
    document.addEventListener('click', function (e) {
      if (!$('modelBox').contains(e.target)) closeModelList();
    });
  }
  async function loadModels() {
    setStatus('wait', '连接中…');
    try {
      const res = await fetch(base() + '/v1/models', { headers: headers() });
      const data = await res.json().catch(() => null);
      if (!res.ok) {
        const msg = (data && data.error && data.error.message) || ('HTTP ' + res.status);
        throw new Error(msg);
      }
      state.models = data.data || [];
      // 默认沿用当前选择；没有则选第一个
      if (!state.models.some(m => m.id === $('model').value)) {
        $('model').value = state.models.length ? state.models[0].id : '';
      }
      renderModelList('');
      setStatus('on', '已连接 · ' + state.models.length + ' 个模型');
      $('title').textContent = state.models.length ? $('model').value : '未获取到模型';
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
    const msgs = [];
    const sys = $('system').value.trim();
    if (sys) msgs.push({ role: 'system', content: sys });
    state.messages.forEach(m => {
      if (m.role === 'user' || m.role === 'assistant') {
        if (m.content) msgs.push({ role: m.role, content: m.content });
      }
    });
    const body = {
      model: $('model').value,
      messages: msgs,
      stream: $('mode').value === '1'
    };
    const t = parseFloat($('temp').value);
    if (!isNaN(t)) body.temperature = t;
    const mt = parseInt($('maxTok').value, 10);
    if (!isNaN(mt) && mt > 0) body.max_tokens = mt;
    return body;
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
      const res = await fetch(base() + '/v1/chat/completions', {
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
              const d = j.choices && j.choices[0] && j.choices[0].delta && j.choices[0].delta.content;
              if (d) { out += d; setBubble(bot, out); }
              if (j.usage) usage = j.usage;
            }
          }
        }
        if (!out) setBubble(bot, '（上游没有返回内容）');
        bot.el.querySelector('.bubble').classList.remove('cursor');
        setMeta(bot, metaItems(rid, ch || chName, upModel, usage, t0, first));
      } else {
        const data = await res.json();
        const msg = (data.choices && data.choices[0] && data.choices[0].message && data.choices[0].message.content) || '（空回复）';
        setBubble(bot, msg);
        bot.el.querySelector('.bubble').classList.remove('cursor');
        setMeta(bot, metaItems(rid, ch || chName, upModel, data.usage, t0, 0));
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
      total ? 'token ' + (usage.prompt_tokens || 0) + '→' + (usage.completion_tokens || 0) : '',
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

  $('input').addEventListener('keydown', e => {
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault();
      send();
    }
  });
  initModelCombo();

  restore();
  setStatus('off', '未连接');
  loadModels();
})();
