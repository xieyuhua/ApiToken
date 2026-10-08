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
      $('model').innerHTML = state.models.map(m => {
        const ups = (m.upstreams || []).map(u => u.channel_id).join(', ');
        return '<option value="' + esc(m.id) + '">' + esc(m.id) + (ups ? '  ← ' + esc(ups) : '') + '</option>';
      }).join('') || '<option value="">（网关未返回模型）</option>';
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
  $('model').addEventListener('change', () => { $('title').textContent = $('model').value; });

  restore();
  setStatus('off', '未连接');
  loadModels();
})();
