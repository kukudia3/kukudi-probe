package e2e

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 设置页改版（层级阶梯 + 分组 + 动作条）的真浏览器验证。
//
// 为什么必须用真浏览器：这次改版的**唯一硬指标**是"六种不同层级的东西不再挤在
// 同一档字号/颜色里"。那是一个 computed style 的问题 —— 静态断言只能证明 CSS 文件里
// 写着 13px，证明不了浏览器真的算出了 13px（选择器被更靠后的规则盖掉、元素根本没被
// 匹配上、媒体查询在 1500px 下把值改了，三种情况静态断言全是绿的）。
//
// 做法与仓库里其它浏览器用例完全一致：真服务端 + 反代注入自检脚本 + --headless=new
// + 结果 POST 回 mock。**不用** --dump-dom --virtual-time-budget：页面挂着 SSE 长连接，
// 虚拟时间会被一直暂停。
const settingsHarnessJS = `(function () {
  'use strict';
  var CFG = window.__TZCFG || {};
  var NODES = CFG.nodes || {};
  var rawFetch = window.fetch.bind(window);
  var R = {
    errs: [], steps: [], fatal: '', scenarioErrs: {},
    panes: {}, levels: {}, levelsDark: {}, darkContrast: {}, bugs: {},
    days: {}, routes: [], viewport: 0, narrow: { panes: {} },
    hints: {},
    hintWrites: { 'ping-hint': [], 'ping-interval-sec': [], 'tg-token-hint': [],
                  'empty': [], 'empty-admin-hint': [] },
    hiddenWrites: { 'empty': [], 'empty-admin-hint': [] }
  };
  window.__SETTINGSRESULT = R;

  // ---- ★「那句话只有一个来源」的量测 ------------------------------------------
  // #ping-hint / #tg-token-hint / #empty 在 index.html 里各有**一份**静态文案。
  // v1.6.5 的 bug 是 app.js 里还有第二份（用 + 拼出来的），settings 回调一回来就把
  // HTML 那份**整句**盖掉 —— 页面先显示新的、几十~几百毫秒后变成旧的，用户看到的就是
  // "闪一下"。v1.6.6 的 #empty 是同一个形状的最后一份（两份今天逐字相同）。
  //
  // 打桩必须装在 app.js **之前**：本脚本是 </body> 前的普通内联脚本，而 app.js/chart.js
  // 是 defer 的，所以这里就是"最早的钩子"，那次覆盖一定会被记下来。
  // 只记录、不改写：setter 原样穿过去。
  (function patchTextContent() {
    var d = Object.getOwnPropertyDescriptor(Node.prototype, 'textContent');
    if (!d || !d.set) { R.errs.push('打桩失败：Node.prototype.textContent 没有 setter'); return; }
    Object.defineProperty(Node.prototype, 'textContent', {
      configurable: true, enumerable: d.enumerable,
      get: function () { return d.get.call(this); },
      set: function (v) {
        if (this.nodeType === 1 && Object.prototype.hasOwnProperty.call(R.hintWrites, this.id)) {
          R.hintWrites[this.id].push({
            from: d.get.call(this),
            to: String(v),
            stack: String(new Error().stack || '').split('\n').slice(1, 4).join(' | ')
          });
        }
        return d.set.call(this, v);
      }
    });
  })();

  // #empty 的那句话分两半：后半句（只有管理员用得上）靠 <span id="empty-admin-hint">
  // 的 **hidden** 属性切换，不是靠 textContent —— 所以同一个钩子也要盯 hidden 的写入，
  // 否则"占位没人驱动"（访客看到一句让他去点不存在按钮的话）谁也发现不了。
  (function patchHidden() {
    var owner = HTMLElement.prototype, d = null;
    while (owner && !(d = Object.getOwnPropertyDescriptor(owner, 'hidden'))) owner = Object.getPrototypeOf(owner);
    if (!owner || !d || !d.set) { R.errs.push('打桩失败：找不到 hidden 的 setter'); return; }
    Object.defineProperty(owner, 'hidden', {
      configurable: true, enumerable: d.enumerable,
      get: function () { return d.get.call(this); },
      set: function (v) {
        if (this.nodeType === 1 && Object.prototype.hasOwnProperty.call(R.hiddenWrites, this.id)) {
          R.hiddenWrites[this.id].push({
            from: !!d.get.call(this),
            to: !!v,
            stack: String(new Error().stack || '').split('\n').slice(1, 4).join(' | ')
          });
        }
        return d.set.call(this, v);
      }
    });
  })();

  window.addEventListener('error', function (e) { R.errs.push('error: ' + (e.message || e.type)); });
  window.addEventListener('unhandledrejection', function (e) {
    var r = e.reason;
    R.errs.push('rejection: ' + (r && r.message ? r.message : String(r)));
  });

  function node(id) { return document.getElementById(id); }
  function textOf(id) { var n = node(id); return n ? n.textContent : ''; }
  function shown(id) { var n = node(id); return !!n && !n.hidden; }
  // hiddenOf 读某个元素自己的 hidden（读不到时回 null —— Go 侧按"三态"断言，
  // 免得"没读到"被当成"读到了 false"）。
  function hiddenOf(id) { var n = node(id); return n ? !!n.hidden : null; }
  function sleep(ms) { return new Promise(function (r) { setTimeout(r, ms); }); }
  function waitFor(what, cond, ms) {
    var deadline = Date.now() + (ms || 20000);
    return new Promise(function (resolve, reject) {
      (function poll() {
        var ok = false;
        try { ok = cond(); } catch (e) { reject(new Error(what + ' 判定抛错: ' + e.message)); return; }
        if (ok) { R.steps.push(what); resolve(true); return; }
        if (Date.now() > deadline) { reject(new Error('等待超时: ' + what)); return; }
        setTimeout(poll, 50);
      })();
    });
  }

  // 每个场景各自记一次"这一段里新增了几条 JS 报错"：合起来看总数会掩盖
  // "前一个场景的错被后面的场景背了锅"。
  var errBaseline = 0;
  function endScenario(name) {
    R.scenarioErrs[name] = R.errs.length - errBaseline;
    errBaseline = R.errs.length;
  }

  function loggedIn() { return shown('view-home') || shown('view-detail') || shown('view-settings'); }
  function login() {
    return waitFor('出现登录视图', function () { return shown('view-login') || loggedIn(); }, 30000)
      .then(function () {
        if (loggedIn()) return true;
        node('login-user').value = CFG.user;
        node('login-pass').value = CFG.pass;
        node('login-submit').click();
        return waitFor('登录后进入应用', loggedIn, 30000);
      });
  }

  function paneSection(name) {
    var box = node('settings-panes');
    return box ? box.querySelector('section[data-pane="' + name + '"]') : null;
  }
  // 设置页把服务端值回填完的标志：服务端信息那 10 格是最后一个 Promise.all 里填的。
  function settingsLoaded() {
    var box = node('server-info');
    return !!box && box.children.length > 0;
  }
  function openPane(name) {
    window.location.hash = '#/settings/' + name;
    return waitFor('栏目 ' + name + ' 打开', function () {
      if (!shown('view-settings')) return false;
      var sec = paneSection(name);
      return !!sec && !sec.hidden;
    }, 30000)
      .then(function () { return waitFor('栏目 ' + name + ' 已回填', settingsLoaded, 15000); })
      .then(function () { return sleep(200); });
  }

  // 八栏各自的骨架。ids 是 app.js 依赖的元素（少一个就是"这一栏打开是空的"或
  // 绑定直接抛错），sel 是这次改版立起来的结构（少一个就是层级没落地）。
  var PANES = [
    { name: 'notify', ids: ['tg-enabled', 'tg-token', 'tg-token-hint', 'tg-chat',
        'notify-daily', 'notify-weekly', 'notify-monthly', 'settings-test',
        'notify-save', 'notify-ok', 'notify-error'],
      sel: ['.pane-head .pane-title', '.pane-head .pane-sub', '.pane-body .group',
        '.group-head .group-title', '.check-grid .check-card', '.actions-bar .btn.primary'] },
    { name: 'alert', ids: ['alert-cooldown', 'alert-grace', 'alert-debounce', 'alert-recover',
        'alert-save', 'alert-ok', 'alert-error'],
      sel: ['.pane-head .pane-title', '.pane-body .fields.cols-4', '.fields .field-label',
        '.fields .field-hint', '.actions-bar .btn.primary'] },
    { name: 'dashboard', ids: ['chart-toggles', 'dashboard-save', 'dashboard-ok', 'dashboard-error'],
      sel: ['.pane-head .pane-title', '.pane-body .group', '.check-grid .check-card',
        '.actions-bar .btn.primary'] },
    { name: 'ping', ids: ['ping-hint', 'ping-list', 'ping-add', 'ping-limit', 'ping-interval',
        'ping-save', 'ping-ok', 'ping-error'],
      sel: ['.pane-head .pane-title', '.ping-thead', '.ping-list .ping-row',
        '.ping-row .btn.danger', '.ping-actions .btn', '.actions-bar .btn.primary'] },
    { name: 'nodes', ids: ['nodes-list', 'nodes-add', 'nodes-empty', 'nodes-error'],
      sel: ['.pane-head .pane-title', '.pane-head .btn', '.pane-body .node-list'] },
    { name: 'security', ids: ['pw-current', 'pw-new', 'pw-new2', 'pw-submit',
        'security-ok', 'security-error'],
      sel: ['.pane-head .pane-title', '.pane-body .group-title', '.pane-body .warn-box',
        '.actions-bar .btn.primary'] },
    { name: 'server', ids: ['server-info', 'fx-info'],
      sel: ['.pane-head .pane-title', '.pane-body .kv-grid .kv-cell',
        '.pane-body .kv-rows .kv-row'] },
    { name: 'audit', ids: ['audit-table', 'audit-body', 'audit-more', 'audit-empty',
        'audit-time-head'],
      sel: ['.pane-head .pane-title', '.pane-body .table th', '.pane-body .table-wrap'] }
  ];

  function inspectPane(spec) {
    var sec = paneSection(spec.name);
    var out = { ids: {}, sel: {} };
    spec.ids.forEach(function (id) {
      var el = node(id);
      // 元素必须**在这一栏里**：别处有个同名 id 一样能让上面的查找通过，
      // 而 getElementById 只认第一个（仓库里踩过这个坑，见 webui_test 的注释）。
      out.ids[id] = !!el && !!sec && sec.contains(el);
    });
    spec.sel.forEach(function (s) { out.sel[s] = sec ? sec.querySelectorAll(s).length : 0; });
    // 空提示位不再占位：error/ok 各自空着时高度必须是 0。
    // 这是"保存按钮孤零零飘着"的物理原因（两个 <p> 各占一行 18px）。
    var hints = {};
    ['.actions-bar .error', '.actions-bar .ok'].forEach(function (s) {
      var el = sec ? sec.querySelector(s) : null;
      if (!el) { hints[s] = null; return; }
      hints[s] = { text: el.textContent, height: el.getBoundingClientRect().height };
    });
    out.hints = hints;
    var bar = sec ? sec.querySelector('.actions-bar') : null;
    out.actionsBarHeight = bar ? bar.getBoundingClientRect().height : -1;
    // 文本里的 Markdown 星号（bug #1）：全栏扫一遍，不是只看「安全」那一句。
    out.asterisks = sec ? (sec.textContent.split('**').length - 1) : 0;
    return out;
  }

  // 层级阶梯：每一档取一个**真实存在**的元素，读它的计算样式。
  // 期望值写在 Go 那边（设计稿的 18/15/13.5/13/12.5/12/11.5），这里只负责观测。
  var LEVELS = [
    { name: 'paneTitle',  pane: 'notify', sel: '.pane-head .pane-title' },
    { name: 'groupTitle', pane: 'notify', sel: '.pane-body .group-title' },
    { name: 'paneSub',    pane: 'notify', sel: '.pane-head .pane-sub' },
    { name: 'groupDesc',  pane: 'notify', sel: '.pane-body .group-desc' },
    { name: 'fieldLabel', pane: 'notify', sel: '.pane-body .field-label' },
    { name: 'fieldHint',  pane: 'notify', sel: '.pane-body .field-hint' },
    { name: 'control',    pane: 'notify', sel: '.pane-body .field input' },
    { name: 'checkText',  pane: 'notify', sel: '.pane-body .check-card .ck-main' },
    { name: 'pingThead',  pane: 'ping',   sel: '.pane-body .ping-thead' },
    { name: 'kvKey',      pane: 'server', sel: '.pane-body .kv-k' },
    { name: 'kvValue',    pane: 'server', sel: '.pane-body .kv-v' },
    { name: 'tableTh',    pane: 'audit',  sel: '.pane-body .table th' }
  ];

  function readLevels(paneName, into) {
    LEVELS.forEach(function (lv) {
      if (lv.pane !== paneName) return;
      var sec = paneSection(lv.pane);
      var el = sec ? sec.querySelector(lv.sel) : null;
      if (!el) { into[lv.name] = null; return; }
      var cs = getComputedStyle(el);
      into[lv.name] = { fontSize: cs.fontSize, fontWeight: cs.fontWeight, color: cs.color };
    });
  }

  // ---- 深色主题：断言"没有元素变成不可读" --------------------------------
  // 判据是**前景与背景的亮度差**：同色（差 0）就是彻底看不见，
  // 而"深色下把 --fg 写死成 #1b1f24"这类错误正好落在这里。
  function parseColor(s) {
    var m = /rgba?\(([^)]+)\)/.exec(s || '');
    if (!m) return null;
    var p = m[1].split(',').map(function (x) { return parseFloat(x); });
    return { r: p[0], g: p[1], b: p[2], a: p.length > 3 ? p[3] : 1 };
  }
  function lum(c) {
    function f(v) { v = v / 255; return v <= 0.03928 ? v / 12.92 : Math.pow((v + 0.055) / 1.055, 2.4); }
    return 0.2126 * f(c.r) + 0.7152 * f(c.g) + 0.0722 * f(c.b);
  }
  function bgOf(el) {
    var n = el;
    while (n && n.nodeType === 1) {
      var c = parseColor(getComputedStyle(n).backgroundColor);
      if (c && c.a > 0.5) return c;
      n = n.parentElement;
    }
    return parseColor(getComputedStyle(document.body).backgroundColor);
  }
  function contrastOf(el) {
    var fg = parseColor(getComputedStyle(el).color);
    var bg = bgOf(el);
    if (!fg || !bg) return null;
    return Math.abs(lum(fg) - lum(bg));
  }
  // 深色下逐栏看的元素：正文、弱化文字、按钮文字、只读信息的标签与值、
  // 表头、复选框文字、危险按钮。少一类就等于那一类没被验过。
  var READABLE = [
    '.pane-head .pane-title', '.pane-head .pane-sub', '.pane-body .group-title',
    '.pane-body .group-desc', '.pane-body .field-label', '.pane-body .field-hint',
    '.pane-body .field input', '.pane-body .check-card .ck-main',
    '.actions-bar .btn.primary', '.actions-bar .btn:not(.primary)',
    '.pane-body .kv-k', '.pane-body .kv-v', '.pane-body .table th', '.pane-body .table td',
    '.pane-body .node-item-name', '.pane-body .node-item-meta',
    '.pane-body .ping-thead', '.pane-body .ping-enable span',
    '.pane-body .empty-box', '.pane-body .warn-box'
  ];
  function readDark(paneName, into) {
    var sec = paneSection(paneName);
    if (!sec) return;
    READABLE.forEach(function (s) {
      var el = sec.querySelector(s);
      if (!el) return;
      var text = (el.textContent || '').trim();
      if (text === '') return;
      var cs = getComputedStyle(el);
      if (cs.display === 'none' || cs.visibility === 'hidden') return;
      into[paneName + ' ' + s] = {
        color: cs.color,
        bg: (function () { var c = bgOf(el); return c ? 'rgb(' + c.r + ', ' + c.g + ', ' + c.b + ')' : ''; })(),
        contrast: contrastOf(el)
      };
    });
  }

  // ---- 顺手修掉的三个 bug -------------------------------------------------
  function readBugs() {
    var out = {};
    // #2：.btn.danger 必须有**看得见的红边框**，而不是"与普通按钮不同"就行。
    // 判据三条（上一版只查了第一条，于是"透明边框"也能过 —— 用户看到的正是
    // "删除按钮没有外框，像渲染坏了"）：
    //   ① 颜色与普通按钮不同；② 边框宽度与普通按钮**一致且非 0**；
    //   ③ 边框颜色不是全透明（浏览器对 transparent 报 rgba(0, 0, 0, 0)）。
    var plain = node('settings-back');
    var danger = paneSection('ping').querySelector('.ping-row .btn.danger');
    if (plain && danger) {
      var a = getComputedStyle(plain);
      var b = getComputedStyle(danger);
      out.danger = {
        plainColor: a.color, dangerColor: b.color,
        plainBorder: a.borderTopColor, dangerBorder: b.borderTopColor,
        plainBorderWidth: a.borderTopWidth, dangerBorderWidth: b.borderTopWidth,
        dangerBg: b.backgroundColor
      };
    } else {
      out.danger = null;
    }
    // #3：动作条左侧那两个提示位空着时必须完全不占位。
    out.hintHeights = {};
    ['notify', 'alert', 'dashboard', 'ping', 'security'].forEach(function (name) {
      var sec = paneSection(name);
      var bar = sec ? sec.querySelector('.actions-bar') : null;
      if (!bar) return;
      var err = bar.querySelector('.error');
      var ok = bar.querySelector('.ok');
      out.hintHeights[name] = {
        error: err ? err.getBoundingClientRect().height : -1,
        errorText: err ? err.textContent : '',
        ok: ok ? ok.getBoundingClientRect().height : -1,
        okText: ok ? ok.textContent : ''
      };
    });
    return out;
  }

  // ---- 天数文案：<24h 与已过期都不能出现「0 天」 ---------------------------
  function costLineOf(id) {
    var card = document.querySelector('#grid .card[data-node-id="' + id + '"]');
    if (!card) return null;
    var lines = card.querySelectorAll('.line');
    for (var i = 0; i < lines.length; i++) {
      var label = lines[i].querySelector('.line-label');
      if (label && label.textContent === '费用') {
        var v = lines[i].querySelector('.line-value');
        return v ? v.textContent : '';
      }
    }
    return '';
  }
  function readDays() {
    var order = ['soon', 'gone', 'ok'];
    return order.reduce(function (chain, key) {
      var id = NODES[key];
      if (!id) return chain;
      return chain.then(function () {
        window.location.hash = '#/n/' + id;
        return waitFor('详情页 ' + key + ' 打开', function () {
          return shown('view-detail') && textOf('detail-name') === key;
        }, 30000).then(function () { return sleep(250); });
      }).then(function () {
        R.days[key] = { nodeID: id, detail: textOf('stat-left'), card: null };
        window.location.hash = '#/';
        return waitFor('回到首页', function () { return shown('view-home'); }, 20000);
      }).then(function () {
        return waitFor('首页卡片就绪 ' + key, function () { return costLineOf(id) !== null; }, 20000);
      }).then(function () {
        R.days[key].card = costLineOf(id);
      });
    }, Promise.resolve());
  }

  function visit(hash, view, pane, what) {
    window.location.hash = hash;
    return waitFor(what + ' 打开', function () {
      if (!shown(view)) return false;
      if (view === 'view-detail' && textOf('detail-name') !== CFG.nodeName) return false;
      if (pane) {
        var sec = paneSection(pane);
        if (!sec || sec.hidden) return false;
      }
      return true;
    }, 30000).then(function () { return sleep(300); });
  }

  // readHints 读「那句话」稳定之后的现场，并验一次"只改数字、句子一个字不动"。
  //
  // 期望值**不在这里写**：Go 那边从服务端真正发出去的那份 HTML 里取（唯一来源）。
  // 这里只负责观测 —— 读了什么就回传什么，判定留给 Go。
  function readHints() {
    R.hints.ping = textOf('ping-hint');
    R.hints.pingSec = textOf('ping-interval-sec');
    R.hints.tg = textOf('tg-token-hint');
    // 首页空态那句：整句同样只该在 index.html 里有一份。这里读的是"稳定之后"的现场
    // （期望值在 Go 侧从服务端真正发出去的那份 HTML 里取，不在这里写第二份）。
    R.hints.empty = textOf('empty');
    R.hints.emptyAdminHidden = hiddenOf('empty-admin-hint');
    var input = node('ping-interval');
    if (!input) { R.errs.push('找不到 #ping-interval'); return; }
    R.hints.intervalValue = input.value;
    var keep = input.value;
    // 改「探测间隔」：句子的结构必须一个字符都不变，只有那个数字占位跟着走。
    input.value = '300';
    input.dispatchEvent(new Event('input', { bubbles: true }));
    R.hints.pingAfterEdit = textOf('ping-hint');
    R.hints.pingSecAfterEdit = textOf('ping-interval-sec');
    input.value = keep;   // 还原现场：后面的步骤还要用它
    input.dispatchEvent(new Event('input', { bubbles: true }));
  }

  // ---- 窄屏：八栏都不许横向溢出，多列网格必须退回单列 ---------------------
  function columnsOf(el) {
    return getComputedStyle(el).gridTemplateColumns.split(' ').length;
  }
  function narrowPass() {
    R.viewport = document.documentElement.clientWidth;
    var order = PANES.map(function (p) { return p.name; });
    return order.reduce(function (chain, name) {
      return chain.then(function () { return openPane(name); }).then(function () {
        var sec = paneSection(name);
        var rec = {
          docOverflow: document.documentElement.scrollWidth - document.documentElement.clientWidth,
          paneOverflow: sec.scrollWidth - sec.clientWidth,
          grid: {}
        };
        [['.fields', '.fields'], ['.check-grid', '.check-grid'], ['.kv-grid', '.kv-grid']]
          .forEach(function (pair) {
            var el = sec.querySelector(pair[1]);
            if (el) rec.grid[pair[0]] = columnsOf(el);
          });
        R.narrow.panes[name] = rec;
        endScenario('窄屏 ' + name);
      });
    }, Promise.resolve());
  }

  function run() {
    return waitFor('页面脚本就绪', function () { return !!node('view-login') && !!node('form-login'); }, 30000)
      .then(login)
      .then(function () { endScenario('登录'); })
      .then(function () {
        // 从设置页进来时 enterApp() 会直接路由到 #/settings/<栏>；
        // 窄屏那一遍只关心布局，走另一条路。
        if (CFG.scenario === 'narrow') return narrowPass();
        return fullPass();
      });
  }

  function fullPass() {
    var order = PANES.map(function (p) { return p.name; });
    return order.reduce(function (chain, name) {
      return chain.then(function () {
        var spec = null;
        PANES.forEach(function (p) { if (p.name === name) spec = p; });
        return openPane(name).then(function () {
          R.panes[name] = inspectPane(spec);
          // ★ 这两句读的是「settings 回调**之后**」的现场：openPane 等的是
          //   settingsLoaded()（服务端信息那 10 格填好），而它在 renderPingEditor 之后。
          //   回调之前读到的永远是 HTML 的原文案 —— 那正是这个 bug 能藏住的原因。
          if (name === 'ping') { R.hints.pingAtOpen = textOf('ping-hint'); }
          if (name === 'notify') { R.hints.tgAtOpen = textOf('tg-token-hint'); }
          readLevels(name, R.levels);
          endScenario('浅色 ' + name);
        });
      });
    }, Promise.resolve())
      .then(function () { readHints(); endScenario('两句提示的现场读数'); })
      .then(function () { R.bugs = readBugs(); endScenario('三个 bug 的界面上证据'); })
      .then(readDays)
      .then(function () { endScenario('天数文案'); })
      // 深色：切成深色再把八栏各开一遍，逐栏读对比度。
      .then(function () {
        document.documentElement.setAttribute('data-theme', 'dark');
        return sleep(200);
      })
      .then(function () {
        return order.reduce(function (chain, name) {
          return chain.then(function () { return openPane(name); }).then(function () {
            readLevels(name, R.levelsDark);
            readDark(name, R.darkContrast);
            endScenario('深色 ' + name);
          });
        }, Promise.resolve());
      })
      // 深色下的危险按钮也要再量一遍：边框颜色跟着主题走（--bad-border 有自己
      // 一组值），所以"浅色下看得见"证明不了"深色下也看得见"。
      .then(function () { R.bugsDark = readBugs(); endScenario('深色危险按钮的计算样式'); })
      .then(function () {
        document.documentElement.removeAttribute('data-theme');
        return sleep(120);
      })
      // 回归：四条既有入口各走一遍，每条都要没有新增 JS 报错。
      .then(function () { return visit('#/', 'view-home', '', '首页'); })
      .then(function () {
        // ★ 首页空态那句话：在**它自己的页面上**再读一次（同一份期望值）。
        //   这条用例的库里有 3 台机器 ⇒ #empty 必须是收起状态（它只在没有节点时出现），
        //   所以这一读验的是"元素自己的显示条件没被这次改动碰坏"。
        R.hints.emptyAtHome = textOf('empty');
        R.hints.emptyAtHomeShown = shown('empty');
        R.hints.emptyAdminHiddenAtHome = hiddenOf('empty-admin-hint');
        endScenario('回归 #/');
      })
      .then(function () { return visit('#/n/' + CFG.nodeID, 'view-detail', '', '详情页'); })
      .then(function () { endScenario('回归 #/n/<id>'); })
      .then(function () { return visit('#/settings/nodes', 'view-settings', 'nodes', '服务器列表栏'); })
      .then(function () { endScenario('回归 #/settings/nodes'); })
      .then(function () { return visit('#/settings/alert', 'view-settings', 'alert', '告警参数栏'); })
      .then(function () {
        R.routes.push({ hash: '#/', view: 'view-home' });
        R.routes.push({ hash: '#/n/' + CFG.nodeID, view: 'view-detail' });
        R.routes.push({ hash: '#/settings/nodes', view: 'view-settings' });
        R.routes.push({ hash: '#/settings/alert', view: 'view-settings' });
        endScenario('回归 #/settings/alert');
      });
  }

  function finish() {
    return rawFetch('/__result', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(R)
    }).then(function () {
      document.title = 'DUMPMARK:' + JSON.stringify({ errs: R.errs.length, fatal: R.fatal });
    });
  }

  // 截图模式：只把界面开到某一栏就停住（给人核对用），不跑完整流程、也不回传结果。
  // EventSource 换成不联网的替身：挂着的 SSE 长连接会让 --virtual-time-budget
  // 永远耗不完，Chrome 就不截图了（与仓库里其它截图用例同一个理由）。
  function shot() {
    window.EventSource = function () {
      var listeners = {};
      this.addEventListener = function (name, fn) { (listeners[name] = listeners[name] || []).push(fn); };
      this.close = function () {};
    };
    if (CFG.theme === 'dark') document.documentElement.setAttribute('data-theme', 'dark');
    login().then(function () { return openPane(CFG.shot); }).then(function () {
      document.title = 'SHOT-READY:' + CFG.shot;
    }).catch(function (err) { R.fatal = String(err && err.message ? err.message : err); });
  }

  window.addEventListener('load', function () {
    if (CFG.shot) { shot(); return; }
    run().catch(function (err) {
      R.fatal = String(err && err.message ? err.message : err);
    }).then(finish, finish);
  });
})();`

// settingsLevel 是一档层级在浏览器里实际算出来的样式。
type settingsLevel struct {
	FontSize   string `json:"fontSize"`
	FontWeight string `json:"fontWeight"`
	Color      string `json:"color"`
}

type settingsHintSample struct {
	Text   string  `json:"text"`
	Height float64 `json:"height"`
}

// settingsHintWrite 是一次对被观测元素 textContent 的写入（从谁写的、写成什么）。
type settingsHintWrite struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Stack string `json:"stack"`
}

// settingsHiddenWrite 是一次对被观测元素 hidden 属性的写入 —— #empty 的"后半句"
// 占位（#empty-admin-hint）靠它切换：它没有 textContent 写入，只有显隐。
type settingsHiddenWrite struct {
	From  bool   `json:"from"`
	To    bool   `json:"to"`
	Stack string `json:"stack"`
}

// settingsHints 是那几句提示的现场读数：
//   - Ping/Tg/Empty 是"稳定之后"（settings 回调已经回来、又等过 200ms）读到的整句；
//   - PingAtOpen/TgAtOpen 是各自那一栏**刚打开**时读的（同样在回调之后）；
//   - EmptyAtHome 是回到首页时再读一次的 #empty（同一份期望值）；
//   - EmptyAdminHidden / EmptyAdminHiddenAtHome 是"只有管理员那半句"占位的显隐；
//   - PingAfterEdit/PingSecAfterEdit 是把「探测间隔」改成 300 之后读的。
type settingsHints struct {
	Ping                   string `json:"ping"`
	PingSec                string `json:"pingSec"`
	PingAtOpen             string `json:"pingAtOpen"`
	PingAfterEdit          string `json:"pingAfterEdit"`
	PingSecAfterEdit       string `json:"pingSecAfterEdit"`
	Tg                     string `json:"tg"`
	TgAtOpen               string `json:"tgAtOpen"`
	Empty                  string `json:"empty"`
	EmptyAtHome            string `json:"emptyAtHome"`
	EmptyAtHomeShown       bool   `json:"emptyAtHomeShown"`
	EmptyAdminHidden       *bool  `json:"emptyAdminHidden"`
	EmptyAdminHiddenAtHome *bool  `json:"emptyAdminHiddenAtHome"`
	IntervalValue          string `json:"intervalValue"`
}

type settingsPaneInspection struct {
	IDs              map[string]bool                `json:"ids"`
	Sel              map[string]int                 `json:"sel"`
	Hints            map[string]*settingsHintSample `json:"hints"`
	ActionsBarHeight float64                        `json:"actionsBarHeight"`
	Asterisks        int                            `json:"asterisks"`
}

type settingsDarkSample struct {
	Color    string  `json:"color"`
	Bg       string  `json:"bg"`
	Contrast float64 `json:"contrast"`
}

type settingsNarrowPane struct {
	DocOverflow  int            `json:"docOverflow"`
	PaneOverflow int            `json:"paneOverflow"`
	Grid         map[string]int `json:"grid"`
}

// settingsBrowserResult 是自检脚本回传的全部观测值。
type settingsBrowserResult struct {
	Errs         []string                          `json:"errs"`
	Fatal        string                            `json:"fatal"`
	Steps        []string                          `json:"steps"`
	ScenarioErrs map[string]int                    `json:"scenarioErrs"`
	Panes        map[string]settingsPaneInspection `json:"panes"`
	Levels       map[string]*settingsLevel         `json:"levels"`
	LevelsDark   map[string]*settingsLevel         `json:"levelsDark"`
	DarkContrast map[string]settingsDarkSample     `json:"darkContrast"`
	Bugs         map[string]json.RawMessage        `json:"bugs"`
	// BugsDark 是同一批观测值在深色主题下再量一遍（危险按钮的边框颜色跟着主题走，
	// 浅色下看得见证明不了深色下也看得见）。
	BugsDark map[string]json.RawMessage `json:"bugsDark"`
	Days     map[string]struct {
		NodeID int64  `json:"nodeID"`
		Detail string `json:"detail"`
		Card   string `json:"card"`
	} `json:"days"`
	Routes   []notifyBrowserRoute `json:"routes"`
	Viewport int                  `json:"viewport"`
	// Hints / HintWrites 是「提示句只有一个来源」那组观测值：整句的现场读数，
	// 以及"谁把整句写了一遍"的逐次记录（期望：对 #ping-hint / #tg-token-hint /
	// #empty 的写入次数为 0，只有 #ping-interval-sec 那个占位被写）。
	Hints      settingsHints                  `json:"hints"`
	HintWrites map[string][]settingsHintWrite `json:"hintWrites"`
	// HiddenWrites 是 hidden 属性的写入记录：#empty 的后半句占位靠它切换
	// （期望：对 #empty-admin-hint 至少写过一次，否则那半句没人管）。
	HiddenWrites map[string][]settingsHiddenWrite `json:"hiddenWrites"`
	Narrow       struct {
		Panes map[string]settingsNarrowPane `json:"panes"`
	} `json:"narrow"`
}

// 设计稿的阶梯（值来自 _settings_mock/after.css 的注释，单位 px）。
// 键名与自检脚本里的 LEVELS 一一对应。
var settingsLadder = map[string]float64{
	"paneTitle":  18,
	"groupTitle": 15,
	"control":    13.5,
	"checkText":  13.5,
	"fieldLabel": 13,
	"paneSub":    12.5,
	"groupDesc":  12.5,
	"fieldHint":  12,
	"tableTh":    11.5,
	"pingThead":  11.5,
	"kvKey":      11.5,
	"kvValue":    13.5,
}

// 必须**两两不同**的五个层级：栏目标题 / 分组标题 / 字段 label / 字段说明 / 控件。
// 这正是这次改版的核心指标 —— 改版前它们是 14 / 13 / 12 / 12 / 14，两两撞车。
var settingsDistinctLevels = []string{"paneTitle", "groupTitle", "fieldLabel", "fieldHint", "control"}

// hintTextFromHTML 从**服务端真正发出去的那份 index.html**里取出某个提示元素的可见文案
// （去掉内部标签），liveSec >= 0 时把秒数占位的默认值换成 liveSec —— 也就是"那个 span
// 被 JS 写完之后，用户应该看到的整句"。
//
// 为什么不在这条用例里手写一遍句子：手写等于又造一个副本 —— HTML 与 app.js 一起漂、
// 而用例里那句没跟着变的时候，它仍然是绿的。这正是这个 bug 藏了这么久的形状。
func hintTextFromHTML(t *testing.T, page, id string, liveSec int) string {
	t.Helper()
	at := strings.Index(page, `id="`+id+`"`)
	if at < 0 {
		t.Fatalf("服务端发出去的 HTML 里找不到 id=%q 的元素", id)
	}
	open := strings.Index(page[at:], ">")
	if open < 0 {
		t.Fatalf("id=%q 的开始标签没有闭合", id)
	}
	rest := page[at+open+1:]
	end := strings.Index(rest, "</p>")
	if end < 0 {
		t.Fatalf("id=%q 的元素后面没有 </p>", id)
	}
	inner := rest[:end]
	if liveSec >= 0 {
		re := regexp.MustCompile(`<span[^>]*id="ping-interval-sec"[^>]*>([^<]*)</span>`)
		m := re.FindStringSubmatch(inner)
		if m == nil {
			t.Fatalf("id=%q 的句子里没有秒数占位 span（app.js 要写的那个元素）", id)
		}
		inner = strings.Replace(inner, m[0], strconv.Itoa(liveSec), 1)
	}
	return strings.TrimSpace(regexp.MustCompile(`<[^>]*>`).ReplaceAllString(inner, ""))
}

func TestSettingsPanesRedesignInRealBrowser(t *testing.T) {
	chrome := findChrome()
	if chrome == "" {
		t.Skip("找不到 Chrome：设置 PROBE_CHROME 或把它装到默认位置后这条用例会自动跑起来")
	}

	logs := &captureHandler{}
	h := startServerAt(t, "127.0.0.1:0", filepath.Join(t.TempDir(), "probe.db"), time.UTC, slog.New(logs), nil)
	br := newBrowser(t, "http://"+h.addr)
	setupAdmin(t, br, waitSetupCode(t, logs))

	// 三台机器：还差 5 小时到期、已经过期 3 天、正常还剩 28 天。
	// 都要填价格 —— 首页卡片的「费用」行只在填过价格时才显示，
	// 而到期文案正是拼在那一行里的。
	//
	// 为什么每台都往"整点"再多给半小时 / 半天：到期文案里的天数是**向下取整**的
	// （"剩余不足 1 天（约 5 小时）"里的小时数同理）。刚好卡在 5h0m0s 的话，
	// 服务端算出这句话与浏览器读到它之间隔了一秒，就会从"约 5 小时"掉成"约 4 小时"
	// —— 那是**测试**在抖，不是页面错了。留出余量之后，期望值在整个用例运行期间
	// 都是同一个字符串。
	now := time.Now()
	soonID := createPricedNode(t, br, "soon", 3000, "CNY", 1, now.Add(5*time.Hour+30*time.Minute).Unix())
	goneID := createPricedNode(t, br, "gone", 3000, "CNY", 1, now.Add(-72*time.Hour-time.Minute).Unix())
	okID := createPricedNode(t, br, "ok", 3000, "CNY", 1, now.Add(28*24*time.Hour+12*time.Hour).Unix())

	// 延迟探测至少要有**一个目标**：否则 .ping-row 是空的，那一栏的行式布局
	// 与「删除」按钮（.btn.danger）根本没有元素可看。
	if status, body := br.do(http.MethodPut, "/api/v1/settings/ping", map[string]any{
		"targets": []map[string]any{
			{"id": 0, "label": "香港出口", "type": "tcp", "host": "hk.example.com", "port": 443, "enabled": true},
		},
		"interval_sec": 60,
	}, true); status != http.StatusOK {
		t.Fatalf("预置探测目标失败: %d %v", status, body)
	}

	// 期望的到期文案从**服务端自己的响应**里取（不要在测试里手写一份）：
	// 这条用例要证的是"面板显示的就是服务端算的那句话"，
	// 手写字符串会把"两边算的不是同一件事"这种 bug 放过去。
	//
	// 同时也钉住这三句话本身：它们是"与告警文案同一口径"这条要求的字面证据
	// （「0 天」在两种情况下都不许出现）。
	wantLiteral := map[string]string{
		"soon": "剩余不足 1 天（约 5 小时）",
		"gone": "已过期 3 天",
		"ok":   "剩余 28 天",
	}
	wantText := map[string]string{}
	for key, id := range map[string]int64{"soon": soonID, "gone": goneID, "ok": okID} {
		n := nodeByID(t, fetchNodes(t, br), id)
		text, _ := n["expires_text"].(string)
		if text != wantLiteral[key] {
			t.Fatalf("节点 %s 的 expires_text = %q，期望 %q", key, text, wantLiteral[key])
		}
		if strings.Contains(text, "0 天") {
			t.Fatalf("服务端给的到期文案里出现了「0 天」：%q", text)
		}
		wantText[key] = text
	}
	t.Logf("服务端给的到期文案：还差 5 小时 = %q；已过期 3 天 = %q；还剩 28 天 = %q",
		wantText["soon"], wantText["gone"], wantText["ok"])

	cfg := tzHarnessConfig{
		NodeID: okID, NodeName: "ok", User: "admin", Pass: "a-very-good-password",
		Nodes: map[string]int64{"soon": soonID, "gone": goneID, "ok": okID},
	}
	proxy := newHarnessProxy(t, "http://"+h.addr, cfg, settingsHarnessJS)
	mock := newMockServer(t, proxy)

	raw := runChromeForResult(t, chrome, mock.URL+"/#/settings/notify", proxy.result, 240*time.Second, "1500,1100")
	var res settingsBrowserResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("解析浏览器回传的结果: %v\n原始内容：%s", err, tail(string(raw), 600))
	}
	if res.Fatal != "" {
		t.Fatalf("浏览器里的自检流程没跑完：%s（已完成步骤：%v）", res.Fatal, res.Steps)
	}

	// 1) 每一步都必须 errs=[]：白屏、绑定失败、null 取属性全都落在
	//    window.onerror 与 unhandledrejection 上。
	if len(res.Errs) != 0 {
		t.Fatalf("浏览器里有 %d 条 JS 报错：%v", len(res.Errs), res.Errs)
	}
	for name, n := range res.ScenarioErrs {
		if n != 0 {
			t.Errorf("场景「%s」里有 %d 条 JS 报错", name, n)
		}
	}
	t.Logf("各场景 JS 报错数：%v", res.ScenarioErrs)

	// 2) 八栏逐个：id 齐全、关键结构存在。
	wantPanes := []string{"notify", "alert", "dashboard", "ping", "nodes", "security", "server", "audit"}
	if len(res.Panes) != len(wantPanes) {
		t.Fatalf("只打开了 %d 栏，期望 %d 栏：%v", len(res.Panes), len(wantPanes), res.Panes)
	}
	for _, name := range wantPanes {
		got, ok := res.Panes[name]
		if !ok {
			t.Errorf("「%s」栏没有被打开过", name)
			continue
		}
		for id, exists := range got.IDs {
			if !exists {
				t.Errorf("「%s」栏里缺少（或不在这一栏里的）id=%s", name, id)
			}
		}
		for sel, count := range got.Sel {
			if count == 0 {
				t.Errorf("「%s」栏里没有匹配 %s 的元素（改版的骨架没落地）", name, sel)
			}
		}
		// bug #1：Markdown 星号不许出现在任何一栏的可见文本里。
		if got.Asterisks != 0 {
			t.Errorf("「%s」栏里有 %d 处字面量 **（Markdown 语法没被渲染）", name, got.Asterisks)
		}
	}

	// 3) 层级阶梯：这一条是"改完还是挤在一起"的唯一硬指标。
	checkLevels(t, "浅色", res.Levels)
	t.Logf("层级阶梯（浅色实测）→ %s", formatLevels(res.Levels))

	// 4) 深色主题：换成深色再跑一遍，阶梯不变、且没有元素变成不可读。
	if len(res.LevelsDark) == 0 {
		t.Fatal("深色主题下没有读到任何层级值：那一遍没跑起来")
	}
	checkLevels(t, "深色", res.LevelsDark)
	t.Logf("层级阶梯（深色实测）→ %s", formatLevels(res.LevelsDark))
	worst, worstKey := 1.0, ""
	for key, sample := range res.DarkContrast {
		if sample.Contrast < worst {
			worst, worstKey = sample.Contrast, key
		}
	}
	if worstKey == "" {
		t.Fatal("深色主题下没有采到任何对比度样本")
	}
	t.Logf("深色下对比度最低的一处 = %s（前景 %s / 背景 %s，亮度差 %.3f）",
		worstKey, res.DarkContrast[worstKey].Color, res.DarkContrast[worstKey].Bg, worst)
	// 0.1 是个很宽的兜底线（正文与弱化文字的亮度差实测都在 0.3 以上）：
	// 它专门用来抓"前景与背景同色"这一类彻底看不见的错误。
	for key, sample := range res.DarkContrast {
		if sample.Contrast < 0.1 {
			t.Errorf("深色下 %s 几乎看不见：前景 %s / 背景 %s，亮度差 %.3f",
				key, sample.Color, sample.Bg, sample.Contrast)
		}
	}

	// 5) bug #2：.btn.danger 默认就有一圈看得见的红边 —— 浅色与深色各量一遍。
	checkDangerButton(t, res.Bugs, "浅色")
	if len(res.BugsDark) == 0 {
		t.Error("没有回传深色下 .btn.danger 的观测值：边框颜色跟着主题走，深色必须单独验")
	} else {
		checkDangerButton(t, res.BugsDark, "深色")
	}

	// 6) bug #3：空的提示位不再占位；动作条本身也不会因此变高。
	checkEmptyHints(t, res.Panes)

	// 7) ★★ 提示句只有一个来源：屏幕上稳定显示的那句必须**逐字等于 index.html 里那句**。
	//
	// 这是「延迟探测文案闪一下」（v1.6.5）的闸门。当时的形状：HTML 里是新句子，
	// app.js 的 updatePingHint() 里另有一份用 + 拼出来的旧句子，settings 回调回来时
	// 把它**整句**盖掉 —— 页面先显示新的、几十~几百毫秒后变成旧的。旧句子是拼出来的，
	// 按整句 grep 抓不到；两条既有用例又只断言"这个 id 存在"，所以一路全绿。
	//
	// 期望值从**服务端真正发出去的那份 HTML**里取（proxy.page），不在这里手写第二份：
	// 手写等于又造一个副本，HTML 与 JS 一起漂的时候这条断言仍然是绿的。
	page := string(proxy.page)
	wantPing60 := hintTextFromHTML(t, page, "ping-hint", 60)
	wantPing300 := hintTextFromHTML(t, page, "ping-hint", 300)
	if res.Hints.IntervalValue != "60" {
		t.Errorf("探测间隔输入框的值 = %q，期望 \"60\"（用例预置的就是 60，下面的期望值以它为准）",
			res.Hints.IntervalValue)
	}
	if res.Hints.PingAtOpen != wantPing60 {
		t.Errorf("「延迟探测」栏刚打开（settings 回调已回来）时 #ping-hint = %q，期望 %q",
			res.Hints.PingAtOpen, wantPing60)
	}
	if res.Hints.Ping != wantPing60 {
		t.Errorf("#ping-hint 稳定后 = %q，期望 %q（index.html 里那句，秒数占位换成实际值）",
			res.Hints.Ping, wantPing60)
	}
	if res.Hints.PingSec != "60" {
		t.Errorf("秒数占位 #ping-interval-sec = %q，期望 \"60\"", res.Hints.PingSec)
	}
	// ★ 整句写入次数必须为 0：这句话在 index.html 里只有一份，任何一次
	//   el.pingHint.textContent = … 都是第二份文案又回来了。
	if n := len(res.HintWrites["ping-hint"]); n != 0 {
		t.Errorf("有 %d 次对 #ping-hint 的整句写入（这句话只该在 index.html 里有一份）：%+v",
			n, res.HintWrites["ping-hint"])
	}
	// 反过来，秒数占位必须**真的**被写过：否则"只改数字"就成了"谁都不改"。
	if n := len(res.HintWrites["ping-interval-sec"]); n == 0 {
		t.Error("没有任何一次对 #ping-interval-sec 的写入：秒数占位是死的，用户改了间隔提示里还是旧数字")
	}
	// 改「探测间隔」：句子必须一个字符都不变，只有那个数字跟着走。
	if res.Hints.PingAfterEdit != wantPing300 || res.Hints.PingSecAfterEdit != "300" {
		t.Errorf("把探测间隔改成 300 之后：整句 = %q（期望 %q）、占位 = %q（期望 \"300\"）",
			res.Hints.PingAfterEdit, wantPing300, res.Hints.PingSecAfterEdit)
	}
	// #tg-token-hint 同理：状态在 label 右边的徽章里，句子是纯静态的 ⇒ 一次写入都不许有。
	wantTgHint := hintTextFromHTML(t, page, "tg-token-hint", -1)
	if res.Hints.TgAtOpen != wantTgHint || res.Hints.Tg != wantTgHint {
		t.Errorf("#tg-token-hint = %q（另一处读到 %q），期望 %q",
			res.Hints.TgAtOpen, res.Hints.Tg, wantTgHint)
	}
	if n := len(res.HintWrites["tg-token-hint"]); n != 0 {
		t.Errorf("有 %d 次对 #tg-token-hint 的写入（那句话是纯静态的，状态由 #tg-token-tag 徽章回答）：%+v",
			n, res.HintWrites["tg-token-hint"])
	}
	t.Logf("提示句实测：#ping-hint = %q（改间隔成 300 后仍是这句，只有占位变 %q）；"+
		"#tg-token-hint = %q；对这两个元素的整句写入 = %d 次",
		res.Hints.Ping, res.Hints.PingSecAfterEdit, res.Hints.Tg,
		len(res.HintWrites["ping-hint"])+len(res.HintWrites["tg-token-hint"]))

	// 7b) ★★ #empty（首页空态）同理：整句只有 index.html 一份，"只有管理员用得上"的
	//     后半句由 <span id="empty-admin-hint"> 的 hidden 切换 —— JS 一个字的整句都不许写。
	//
	// 与 #ping-hint 同一个形状（v1.6.6 的 app.js 里是
	// `el.empty.textContent = guestMode() ? … : …`），所以闸门也同一套：
	// 期望值从服务端真正发出去的那份 HTML 里取，写入次数必须是 0。
	wantEmpty := hintTextFromHTML(t, page, "empty", -1)
	if wantEmpty == "" {
		t.Fatal("从服务端发出去的 HTML 里读出来的 #empty 是空的：那句话的「唯一来源」没了")
	}
	if res.Hints.Empty == "" || res.Hints.EmptyAtHome == "" {
		t.Errorf("#empty 没读到（settings 现场 %q / 首页 %q）", res.Hints.Empty, res.Hints.EmptyAtHome)
	} else if res.Hints.Empty != wantEmpty || res.Hints.EmptyAtHome != wantEmpty {
		t.Errorf("#empty = %q（回到首页时 %q），期望 %q（index.html 里那句，唯一来源）",
			res.Hints.Empty, res.Hints.EmptyAtHome, wantEmpty)
	}
	// ★ 整句写入次数必须为 0：任何一次 el.empty.textContent = … 都是第二份文案又回来了。
	if n := len(res.HintWrites["empty"]); n != 0 {
		t.Errorf("有 %d 次对 #empty 的整句写入（这句话只该在 index.html 里有一份）：%+v",
			n, res.HintWrites["empty"])
	}
	// 占位里的文本同样不许被写（写了就是把第二份文案挪进了 span 里）。
	if n := len(res.HintWrites["empty-admin-hint"]); n != 0 {
		t.Errorf("有 %d 次对 #empty-admin-hint 的文本写入（JS 只许切换它的显隐）：%+v",
			n, res.HintWrites["empty-admin-hint"])
	}
	// 反过来，占位必须**真的**被切换过，否则"只有管理员那半句"就成了谁都不管的死文案：
	// 访客会看到一句让他去点一个根本不存在的按钮的话。
	if n := len(res.HiddenWrites["empty-admin-hint"]); n == 0 {
		t.Error("没有任何一次对 #empty-admin-hint 的 hidden 写入：管理员那半句没人切换，" +
			"访客会照着「点右上角『新增节点』」去找一个不存在的按钮")
	}
	// 这条用例是**管理员**：完整那句必须都在（后半句不许被藏起来）。
	if res.Hints.EmptyAdminHidden == nil {
		t.Error("没有读到 #empty-admin-hint 的 hidden（占位元素不在？）")
	} else if *res.Hints.EmptyAdminHidden {
		t.Error("管理员看到的 #empty 缺了后半句（#empty-admin-hint.hidden = true）")
	}
	if res.Hints.EmptyAdminHiddenAtHome == nil {
		t.Error("回到首页时没有读到 #empty-admin-hint 的 hidden")
	} else if *res.Hints.EmptyAdminHiddenAtHome {
		t.Error("管理员回到首页时 #empty 缺了后半句（#empty-admin-hint.hidden = true）")
	}
	// 元素自己的显示条件没被这次改动碰坏：库里有 3 台机器 ⇒ 空态必须收起来。
	if res.Hints.EmptyAtHomeShown {
		t.Error("库里有 3 台机器，#empty 却是显示状态（空态的显示条件坏了）")
	}
	adminHidden := "没读到"
	if res.Hints.EmptyAdminHidden != nil {
		adminHidden = strconv.FormatBool(*res.Hints.EmptyAdminHidden)
	}
	t.Logf("首页空态实测：#empty = %q（回到首页 %q）；对它的整句写入 = %d 次；"+
		"#empty-admin-hint.hidden = %s（管理员，期望 false），对它的 hidden 写入 = %d 次",
		res.Hints.Empty, res.Hints.EmptyAtHome, len(res.HintWrites["empty"]),
		adminHidden, len(res.HiddenWrites["empty-admin-hint"]))

	// 8) 天数文案：详情页与首页卡片都不能出现「0 天」。
	for _, key := range []string{"soon", "gone", "ok"} {
		got, ok := res.Days[key]
		if !ok {
			t.Errorf("没有读到 %s 那一台机器的到期文案", key)
			continue
		}
		if got.Detail != wantText[key] {
			t.Errorf("%s 详情页「到期」= %q，期望 %q（与服务端 expires_text 同源）",
				key, got.Detail, wantText[key])
		}
		if !strings.Contains(got.Card, wantText[key]) {
			t.Errorf("%s 首页卡片的费用行 = %q，里面没有到期文案 %q", key, got.Card, wantText[key])
		}
		for _, text := range []string{got.Detail, got.Card} {
			if strings.Contains(text, "0 天") {
				t.Errorf("%s 的界面上出现了「0 天」：%q", key, text)
			}
		}
	}
	t.Logf("天数文案：soon 详情=%q 卡片=%q；gone 详情=%q 卡片=%q",
		res.Days["soon"].Detail, res.Days["soon"].Card, res.Days["gone"].Detail, res.Days["gone"].Card)

	// 9) 回归：四条既有入口照旧能打开。
	wantRoutes := []struct{ hash, view string }{
		{"#/", "view-home"},
		{"#/n/" + strconv.FormatInt(okID, 10), "view-detail"},
		{"#/settings/nodes", "view-settings"},
		{"#/settings/alert", "view-settings"},
	}
	if len(res.Routes) != len(wantRoutes) {
		t.Fatalf("回归路由只跑了 %d 个：%+v", len(res.Routes), res.Routes)
	}
	for i, want := range wantRoutes {
		if res.Routes[i].Hash != want.hash || res.Routes[i].View != want.view {
			t.Errorf("第 %d 个回归入口 = %+v，期望 %s → %s", i+1, res.Routes[i], want.hash, want.view)
		}
	}

	// 10) 窄屏：八栏都不横向溢出，多列网格退回单列。
	narrow := runSettingsNarrow(t, chrome, "http://"+h.addr, cfg)
	if len(narrow.Narrow.Panes) != len(wantPanes) {
		t.Fatalf("窄屏只量到 %d 栏，期望 %d 栏：%+v", len(narrow.Narrow.Panes), len(wantPanes), narrow.Narrow.Panes)
	}
	t.Logf("窄屏实测：CSS 视口 %dpx（--window-size 传的是 560）", narrow.Viewport)
	// 760 是 style.css 里"窄屏兜底"那一档的断点，不是某个平台上量到的观测值：
	// 视口一旦宽过它，下面"多列网格必须退回单列"的断言就失去了前提（在 800px 的
	// 视口上量"窄屏会不会溢出"是自欺）。这条断言同时充当**环境自检**：哪个平台的
	// Chrome 最小窗宽超过 760，就会在这里明确报出来，而不是让后面那条网格断言
	// 给出一句看不懂的失败。
	if narrow.Viewport > 760 {
		t.Errorf("窄屏那一遍的 CSS 视口是 %dpx，没落进 style.css 的 (max-width: 760px) 那一档："+
			"这个宽度下多列网格本来就不该退回单列，后面的断言等于没验", narrow.Viewport)
	}
	for _, name := range wantPanes {
		rec, ok := narrow.Narrow.Panes[name]
		if !ok {
			t.Errorf("窄屏下没有量到「%s」栏", name)
			continue
		}
		if rec.DocOverflow > 0 {
			t.Errorf("窄屏下「%s」栏把整页顶宽了 %dpx（出现了横向滚动条）", name, rec.DocOverflow)
		}
		if rec.PaneOverflow > 0 {
			t.Errorf("窄屏下「%s」栏自己溢出了 %dpx", name, rec.PaneOverflow)
		}
		for sel, cols := range rec.Grid {
			if cols != 1 {
				t.Errorf("窄屏下「%s」栏的 %s 是 %d 列，期望退回单列", name, sel, cols)
			}
		}
	}
}

// checkLevels 核对一整套层级值：与设计稿一致，且五个关键层级两两不同。
func checkLevels(t *testing.T, theme string, levels map[string]*settingsLevel) {
	t.Helper()
	if len(levels) == 0 {
		t.Fatalf("%s：一档层级都没读到", theme)
	}
	for name, want := range settingsLadder {
		got, ok := levels[name]
		if !ok || got == nil {
			t.Errorf("%s：没有读到 %s 这一档", theme, name)
			continue
		}
		if wantFont := formatPX(want); got.FontSize != wantFont {
			t.Errorf("%s：%s 的 font-size = %s，设计稿是 %s", theme, name, got.FontSize, wantFont)
		}
	}
	// 关键指标：栏目标题 / 分组标题 / 字段 label / 字段说明 / 控件
	// 五者的字号必须互不相同 —— 改版前它们是 14 / 13 / 12 / 12 / 14。
	seen := map[string]string{}
	for _, name := range settingsDistinctLevels {
		got := levels[name]
		if got == nil {
			t.Errorf("%s：%s 这一档没读到，无法比较", theme, name)
			continue
		}
		if prev, dup := seen[got.FontSize]; dup {
			t.Errorf("%s：%s 与 %s 的字号都是 %s（层级又挤在一起了）", theme, name, prev, got.FontSize)
		}
		seen[got.FontSize] = name
	}
	// 字段说明必须与字段 label **颜色也不同**：改版前两者都是 12px + --fg-muted，
	// 那是"说明和标题挤在一起"的根子。字号已经拉开了，颜色这一维也不能省。
	label, hint := levels["fieldLabel"], levels["fieldHint"]
	if label != nil && hint != nil && label.Color == hint.Color {
		t.Errorf("%s：字段 label 与字段说明的颜色相同（%s）—— 两者必须一眼能分开", theme, label.Color)
	}
}

// formatPX 把设计稿里的数值写成浏览器 computed style 的写法（13.5px / 18px）。
func formatPX(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64) + "px"
}

func formatLevels(levels map[string]*settingsLevel) string {
	order := []string{"paneTitle", "groupTitle", "control", "checkText", "fieldLabel",
		"paneSub", "groupDesc", "fieldHint", "tableTh", "pingThead", "kvKey", "kvValue"}
	parts := make([]string, 0, len(order))
	for _, name := range order {
		got := levels[name]
		if got == nil {
			parts = append(parts, name+"=(缺失)")
			continue
		}
		parts = append(parts, name+"="+got.FontSize+"/"+got.FontWeight+"/"+got.Color)
	}
	return strings.Join(parts, "  ")
}

// checkDangerButton 核对 bug #2：`.btn.danger` 默认就有一圈**看得见的红边**。
//
// 这条断言被收紧过一次：上一版只要求"与普通按钮的计算样式不同"，于是
// `border-color: transparent`（边框在、但完全透明）照样能通过 —— 用户看到的
// 就是详情页那一排里「删除」没有外框、像渲染坏了。现在钉的是**观感**本身：
// 粗细与普通按钮一致（不是 0、也不是更粗），颜色是一个真的、且不同的颜色。
func checkDangerButton(t *testing.T, raw map[string]json.RawMessage, theme string) {
	t.Helper()
	body, ok := raw["danger"]
	if !ok {
		t.Fatalf("[%s] 没有回传 .btn.danger 的观测值", theme)
	}
	if string(body) == "null" {
		t.Fatalf("[%s] 延迟探测那一栏里找不到 .btn.danger（探测目标没建出来？）", theme)
	}
	var got struct {
		PlainColor        string `json:"plainColor"`
		DangerColor       string `json:"dangerColor"`
		PlainBorder       string `json:"plainBorder"`
		DangerBorder      string `json:"dangerBorder"`
		PlainBorderWidth  string `json:"plainBorderWidth"`
		DangerBorderWidth string `json:"dangerBorderWidth"`
		DangerBg          string `json:"dangerBg"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("[%s] 解析 .btn.danger 的观测值: %v", theme, err)
	}
	t.Logf("[%s] .btn.danger：普通按钮 color=%s border=%s/%s；危险按钮 color=%s border=%s/%s bg=%s",
		theme, got.PlainColor, got.PlainBorder, got.PlainBorderWidth,
		got.DangerColor, got.DangerBorder, got.DangerBorderWidth, got.DangerBg)

	if got.DangerColor == got.PlainColor && got.DangerBorder == got.PlainBorder {
		t.Errorf("[%s] .btn.danger 与普通按钮的计算样式完全一样：style.css 里那条规则没生效", theme)
	}
	// ① 颜色必须真的不同（"不同"是这条样式存在的理由）。
	if got.DangerBorder == got.PlainBorder {
		t.Errorf("[%s] .btn.danger 的边框颜色与普通按钮一样（%q）：看不出这是危险操作",
			theme, got.DangerBorder)
	}
	// ② 粗细必须与普通按钮一致 —— 危险靠颜色区分，不靠"更粗"、更不靠"没有"。
	if got.DangerBorderWidth != got.PlainBorderWidth {
		t.Errorf("[%s] .btn.danger 的边框宽度 = %q，与普通按钮的 %q 不一致（应当只换颜色、不换粗细）",
			theme, got.DangerBorderWidth, got.PlainBorderWidth)
	}
	if got.DangerBorderWidth == "" || got.DangerBorderWidth == "0px" {
		t.Errorf("[%s] .btn.danger 的边框宽度 = %q，等于没有边框（默认就要有一圈看得见的红边）",
			theme, got.DangerBorderWidth)
	}
	// ③ 颜色不能是全透明的：`border-color: transparent` 的计算值是 rgba(0,0,0,0)，
	// 宽度照样是 1px —— 上面两条都拦不住它，只有这一条能。
	if isTransparentColor(got.DangerBorder) {
		t.Errorf("[%s] .btn.danger 的边框颜色 = %q 是全透明的：危险按钮必须默认就有一圈看得见的红边",
			theme, got.DangerBorder)
	}
	if isTransparentColor(got.PlainBorder) {
		t.Errorf("[%s] 普通 .btn 的边框颜色 = %q 是全透明的（基础按钮不该这样）", theme, got.PlainBorder)
	}
}

// isTransparentColor 判断浏览器给出的计算颜色是不是"看不见"（全透明）。
//
// 浏览器对 `border-color: transparent` 报的是 `transparent` 或 `rgba(0, 0, 0, 0)`，
// 两种都要认。注意不能用"以 ,0) 结尾"这种偷懒判据：`rgb(0, 0, 0)`（纯黑边框）
// 也会撞上它。所以这里只对 rgb/rgba/hsl/hsla 里的**第 4 个分量**（alpha）下手。
func isTransparentColor(value string) bool {
	c := strings.ToLower(strings.TrimSpace(value))
	if c == "" || c == "transparent" || c == "none" {
		return true
	}
	open := strings.Index(c, "(")
	if open < 0 || !strings.HasSuffix(c, ")") {
		return false // #rrggbb、颜色关键字等等都是不透明的
	}
	parts := strings.Split(c[open+1:len(c)-1], ",")
	if len(parts) != 4 {
		return false // rgb(...) / hsl(...) 没有 alpha 分量
	}
	alpha := strings.TrimSpace(parts[3])
	percent := strings.HasSuffix(alpha, "%")
	alpha = strings.TrimSuffix(alpha, "%")
	v, err := strconv.ParseFloat(alpha, 64)
	if err != nil {
		return false
	}
	if percent {
		v /= 100
	}
	return v <= 0
}

// checkEmptyHints 核对 bug #3：动作条左侧那两个常驻空元素不再各占一行。
func checkEmptyHints(t *testing.T, panes map[string]settingsPaneInspection) {
	t.Helper()
	checked := 0
	for name, pane := range panes {
		for sel, sample := range pane.Hints {
			if sample == nil {
				continue
			}
			checked++
			if strings.TrimSpace(sample.Text) != "" {
				continue
			}
			if sample.Height > 0.5 {
				t.Errorf("「%s」栏里空着的 %s 仍占 %.1fpx 高（保存按钮就是被它推远的）",
					name, sel, sample.Height)
			}
		}
	}
	if checked == 0 {
		t.Error("一栏的提示位都没量到：动作条里没有 .error/.ok")
	}
}

// runSettingsNarrow 用窄视口再跑一遍。
//
// 视口宽度必须**显式钉住**，不能靠"Chrome 会把它夹到多宽"来定 —— 那是平台相关的：
//
//   - `--window-size` 的单位是 DIP（也就是 CSS 像素），而 Windows 上 Chrome 的
//     浏览器窗口有 **500 DIP 的最小宽度**：本机实测 `--window-size=380` 拿到的
//     clientWidth 是 500（页面带竖向滚动条时 485）—— 要 380、给 500；
//   - Linux 上没有这条最小宽度限制（CI 就是 Linux），同一个 380 会真的给出 ~380。
//
// 于是同一个用例在 Windows 与 CI 上量的**根本不是同一个视口**，断言也就不是同一件事
// ——原来这句提示语里的"485px 那一档"正是那个被夹出来的偶然值。
//
// 560 这个取值同时满足两件事：**大于 500 DIP 的平台下限**（两边都不会被夹，拿到我们
// 真正要的那个宽度），又**落在 style.css 的 `(max-width: 760px)` 那一档里**（"多列
// 网格退回单列""动作条折行"这些规则只在那一档生效，而那正是这条用例要验的东西）。
// 之所以贴着下限取（而不是随手写个 700），是为了让本机量到的视口尽量接近原来那个
// 485px —— 窄屏断言越窄越有意义，但前提是这个宽度是**我们指定的**、不是平台给的。
//
// 顺带记一笔：`--force-device-scale-factor` **改不了 CSS 视口**（本机实测
// `--window-size=760 --force-device-scale-factor=2` 的 clientWidth 是 744，不是
// 380），所以不能靠缩放去"造"一个窄屏。
func runSettingsNarrow(t *testing.T, chrome, base string, cfg tzHarnessConfig) settingsBrowserResult {
	t.Helper()
	cfg.Scenario = "narrow"
	proxy := newHarnessProxy(t, base, cfg, settingsHarnessJS)
	mock := newMockServer(t, proxy)
	raw := runChromeForResult(t, chrome, mock.URL+"/#/settings/notify", proxy.result, 240*time.Second, "560,900")
	var res settingsBrowserResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("解析窄屏那一遍的结果: %v\n原始内容：%s", err, tail(string(raw), 600))
	}
	if res.Fatal != "" {
		t.Fatalf("窄屏那一遍没跑完：%s（已完成步骤：%v）", res.Fatal, res.Steps)
	}
	if len(res.Errs) != 0 {
		t.Fatalf("窄屏那一遍有 %d 条 JS 报错：%v", len(res.Errs), res.Errs)
	}
	return res
}
