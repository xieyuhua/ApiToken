/**
 * combo.js —— 通用「可搜索下拉」组件。
 *
 * 解决原生 <select> 在选项过多（几百个模型）时无法检索的问题。
 * 支持：关键词过滤（匹配标题/副标题/分组）、命中高亮、键盘上下选择、Enter 确认、
 *      Esc 收起、点击外部收起、收起时恢复已选值、超量截断提示、选项分组、
 *      allowFree 自由输入（可直接键入候选之外的名称，并在列表顶部置顶提示）。
 *
 * 用法：
 *   const c = Combo.attach($('r_model'), {
 *     getOptions: () => [{value:'gpt-4o', label:'gpt-4o', hint:'deepseek-1'}],
 *     onPick: v => { ... },
 *   });
 *   c.refresh();                 // 选项数据变化后刷新已展开的列表
 *   Combo.value($('r_model'));   // 读取真实值（不是显示文本）
 *
 * 自由输入（对外模型名这类「可以用自定义名字」的字段）：
 *   Combo.attach($('r_model'), { getOptions, allowFree: true, onPick });
 *   —— 输入的文本与任何候选都不完全一致时，列表顶部出现「使用自定义名称 xxx」；
 *      直接 Enter、失焦关闭列表都会把输入文本作为值（onPick 第二参数带 free:true）。
 */
(function () {
  'use strict';

  const MAX_LIST = 300;   // 最多渲染多少行，避免选项极多时卡顿
  const all = [];         // 已挂载的控制器，便于 refreshAll

  const esc = s => String(s == null ? '' : s).replace(/[&<>"]/g, c =>
    ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c]));

  /** 关键词高亮 */
  function hl(text, kw) {
    if (!kw) return esc(text);
    const i = String(text).toLowerCase().indexOf(kw);
    if (i < 0) return esc(text);
    return esc(text.slice(0, i)) + '<mark>' + esc(text.slice(i, i + kw.length)) +
      '</mark>' + esc(text.slice(i + kw.length));
  }

  const registry = new WeakMap();

  function attach(input, opts) {
    if (!input) return { refresh: function () {}, close: function () {}, open: function () {} };
    opts = opts || {};
    const box = (input.closest && input.closest('.combo')) || input.parentElement || input;
    let list = box.querySelector('.combo-list');
    if (!list) {
      list = document.createElement('div');
      list.className = 'combo-list hidden';
      box.appendChild(list);
    }
    input.classList.add('combo-input');
    input.autocomplete = 'off';
    input.setAttribute('autocapitalize', 'off');
    input.setAttribute('spellcheck', 'false');

    const ctl = {
      input: input,
      list: list,
      options: [],
      refresh: refresh,
      open: open,
      close: close,
      setValue: setValue,
      isOpen: function () { return !list.classList.contains('hidden'); },
    };

    function currentValue() { return input.dataset.value || ''; }

    function setValue(value, label) {
      input.dataset.value = value == null ? '' : String(value);
      input.value = label != null ? label : (value == null ? '' : String(value));
      input.title = opts.title || input.value;
    }

    function collect() {
      ctl.options = (opts.getOptions ? opts.getOptions() : []).filter(Boolean);
      return ctl.options;
    }

    function render(kw) {
      const optsAll = collect();
      const k = (kw || '').toLowerCase();
      const hit = k ? optsAll.filter(o =>
        String(o.label || o.value || '').toLowerCase().includes(k) ||
        String(o.hint || '').toLowerCase().includes(k) ||
        String(o.group || '').toLowerCase().includes(k)) : optsAll;

      // 自由输入：输入文本与任何候选都不完全一致时，置顶一条「使用自定义名称」
      // （置顶渲染，因此不会像普通选项那样被 MAX_LIST 截断后点不到）
      let free = null;
      if (opts.allowFree && k) {
        const exact = optsAll.some(o => String(o.label || o.value || '').toLowerCase() === k);
        if (!exact) free = { value: kw, label: kw, hint: opts.freeHint || '自定义名称（不在候选列表中）', free: true };
      }

      const cur = currentValue();
      const rows = free ? [free].concat(hit) : hit;
      const shown = rows.slice(0, MAX_LIST);

      if (!shown.length) {
        list.innerHTML = optsAll.length
          ? '<div class="combo-empty">没有匹配「' + esc(kw) + '」的选项' +
            (optsAll.length > 20 ? '（共 ' + optsAll.length + ' 个，请换个关键词）' : '') + '</div>'
          : '<div class="combo-empty">' + esc(opts.emptyText || '暂无可选项') + '</div>';
      } else {
        let lastGroup = null;
        let html = '';
        shown.forEach(function (o) {
          if (o.free) {
            html += '<div class="combo-item free' + (String(o.value) === String(cur) ? ' on' : '') +
              '" data-value="' + esc(o.value) + '" data-label="' + esc(o.label) +
              '" title="' + esc(o.label) + '">' +
              '<span class="mid">使用自定义名称「' + esc(o.label) + '」</span>' +
              '<span class="mups">' + hl(o.hint, k) + '</span>' +
              '</div>';
            return;
          }
          const g = o.group || '';
          if (g && g !== lastGroup) html += '<div class="combo-group">' + esc(g) + '</div>';
          lastGroup = g;
          html += '<div class="combo-item' + (String(o.value) === String(cur) ? ' on' : '') +
            '" data-value="' + esc(o.value) + '" data-label="' + esc(o.label || o.value) +
            '" title="' + esc(o.title || o.label || o.value) + '">' +
            '<span class="mid">' + hl(o.label || o.value, k) + '</span>' +
            (o.hint ? '<span class="mups">' + hl(o.hint, k) + '</span>' : '') +
            '</div>';
        });
        list.innerHTML = html;
      }
      if (rows.length > shown.length) {
        list.insertAdjacentHTML('beforeend',
          '<div class="combo-empty">仅显示前 ' + shown.length + ' 个（共 ' + rows.length +
          ' 个命中），请继续输入以缩小范围</div>');
      }
    }

    function refresh(kw) {
      render(kw != null ? kw : (input.dataset.kw || ''));
    }

    function open() {
      if (typeof opts.onOpen === 'function') opts.onOpen();
      list.classList.remove('hidden');
      refresh();
    }

    function close() {
      const typed = input.value.trim();
      const typedKw = input.dataset.kw || '';   // 用户确实敲过字才允许当作自定义值
      const cur = ctl.options.find(function (o) { return String(o.value) === String(currentValue()); });
      list.classList.add('hidden');
      input.dataset.kw = '';
      // 自由输入：用户直接键入的文本与当前值不同 → 以输入文本作为自定义值
      if (opts.allowFree && typedKw && typed && typed !== currentValue() &&
        (!cur || typed !== (cur.label || cur.value))) {
        setValue(typed, typed);
        if (typeof opts.onPick === 'function') opts.onPick(typed, { value: typed, label: typed, free: true });
        return;
      }
      input.value = cur ? (cur.label || cur.value) : (opts.allowFree ? currentValue() : '');
      input.title = opts.title || input.value;
    }

    function pick(value) {
      const o = ctl.options.find(function (x) { return String(x.value) === String(value); });
      const label = o ? (o.label || o.value) : String(value == null ? '' : value);
      setValue(value, label);
      close();
      if (typeof opts.onPick === 'function') {
        opts.onPick(value, o || { value: value, label: label, free: true });
      }
    }

    function move(step) {
      const items = Array.prototype.slice.call(list.querySelectorAll('.combo-item'));
      if (!items.length) return;
      let i = items.findIndex(function (el) { return el.classList.contains('active'); });
      if (i < 0) i = step > 0 ? 0 : items.length - 1;
      else i = Math.min(items.length - 1, Math.max(0, i + step));
      items.forEach(function (el) { el.classList.remove('active'); });
      items[i].classList.add('active');
      if (items[i].scrollIntoView) items[i].scrollIntoView({ block: 'nearest' });
    }

    input.addEventListener('focus', function () { open(); });
    input.addEventListener('input', function () {
      input.dataset.kw = input.value.trim();
      list.classList.remove('hidden');
      render(input.dataset.kw);
    });
    input.addEventListener('keydown', function (e) {
      if (e.key === 'ArrowDown') { e.preventDefault(); move(1); }
      else if (e.key === 'ArrowUp') { e.preventDefault(); move(-1); }
      else if (e.key === 'Enter') {
        if (list.classList.contains('hidden')) return;
        const el = list.querySelector('.combo-item.active') || list.querySelector('.combo-item');
        if (el) { e.preventDefault(); pick(el.getAttribute('data-value')); }
        else if (opts.allowFree && input.value.trim()) {
          e.preventDefault();
          const t = input.value.trim();
          setValue(t, t);
          close();
          if (typeof opts.onPick === 'function') opts.onPick(t, { value: t, label: t, free: true });
        }
      } else if (e.key === 'Escape') {
        close();
      }
    });

    // 用 mousedown 抢先，避免输入框先 blur 导致列表收起
    list.addEventListener('mousedown', function (e) {
      const it = e.target.closest ? e.target.closest('.combo-item') : null;
      if (!it) return;
      e.preventDefault();
      pick(it.getAttribute('data-value'));
    });

    document.addEventListener('click', function (e) {
      if (box.contains && box.contains(e.target)) return;
      if (list.classList.contains('hidden')) return;
      close();
    });

    registry.set(input, ctl);
    all.push(ctl);
    return ctl;
  }

  /** 读取组合框的真实值（而非显示文本） */
  function value(input) {
    if (!input) return '';
    const ctl = registry.get(input);
    if (ctl) return ctl.input.dataset.value || '';
    return (input.dataset && input.dataset.value) || input.value || '';
  }

  /** 读取组合框的显示文本 */
  function text(input) {
    return input ? (input.value || '') : '';
  }

  function refreshAll() {
    all.forEach(function (c) { try { c.refresh(); } catch (e) { /* 忽略已卸载节点 */ } });
  }

  window.Combo = { attach: attach, value: value, text: text, hl: hl, refreshAll: refreshAll, MAX_LIST: MAX_LIST };
})();
