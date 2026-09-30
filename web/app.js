'use strict';

/* 极简 VPS 探针 —— 前端
 *
 * 没有框架、没有构建步骤、没有外部请求。
 * 渲染只用 DOM API 与 textContent（绝不拼接 HTML，防 XSS）。
 * 实时数据走 SSE：服务端每秒只推"变了的节点"，这里按 id 局部更新卡片。
 */

(function () {
  var POLL_SESSION_MS = 30000;

  // 一键安装脚本所在的仓库与分支：新增节点后要拿它拼出给用户的安装命令。
  // 必须与 deploy/install-remote.sh 里的 DEFAULT_GITHUB / DEFAULT_REF 一致，
  // 否则页面给出的命令会直接 404 —— 而 Token 只显示一次，关掉就只能重新生成
  // （deploy/deploy_test.go 里有测试盯着这两处不许漂移）。
  var INSTALL_REPO = 'kukudia3/kukudi-probe';
  var INSTALL_REF = 'main';

  var el = {};
  function $(id) { return document.getElementById(id); }

  // ---------------------------------------------------------------- 状态

  var session = { authenticated: false, needs_setup: false, username: '', csrf_token: '' };
  var nodes = new Map();     // id -> dto
  var cards = new Map();     // id -> { root, refs }
  var source = null;         // EventSource
  var streamOk = false;

  // 详情页状态
  var detail = {
    id: 0,
    node: null,
    uptime: {},
    ranges: [],
    range: '1h',
    charts: new Map(),   // key -> chart 实例
    timer: null,
    // 延迟图的探测目标列表：进入详情页时随节点详情一起取一次。
    // null = 还没拿到（这时不请求 /ping），[] = 确实一个都没配（显示空态）。
    pingTargets: null,
    pingSeries: []       // 上一次 /ping 画出来的全部曲线（勾选过滤前）
  };
  var DETAIL_REFRESH_MS = 30000;
  // 手机端（窄屏）用服务端给的二次聚合目标，PC 不聚合。
  var MOBILE_QUERY = '(max-width: 640px)';

  // 延迟图的曲线颜色：沿用项目既有的那几个色（与网络图/流量图同一套取色），
  // 按目标顺序循环取 —— 颜色只是"哪条线是哪条"，不引入新的 CSS 变量。
  var PING_COLORS = ['#2563eb', '#16a34a', '#7c3aed', '#0891b2', '#d97706', '#dc2626'];

  // 延迟图上"隐藏了哪些目标"存 localStorage：这是"本浏览器想看哪几条线"的偏好，
  // 与服务端的探测目标配置无关，所以不进服务端（主题切换也是同样的做法）。
  var PING_HIDDEN_KEY = 'probe-ping-hidden';

  // 要显示哪些图表。null = 还没从服务端拿到，此时先按"全部显示"（不能因为一次
  // 设置接口慢半拍就让首屏少画几张图）；拿到之后就是一个可能为空的键数组。
  var visibleCharts = null;

  // 操作记录相关状态
  var auditBeforeID = 0;
  var confirmAction = null;
  var ACTION_TEXT = {
    node_create: '新增节点',
    node_update: '修改节点',
    node_delete: '删除节点',
    node_token_rotate: '重新生成 Token',
    telegram_settings: '修改通知设置',
    settings_update: '修改告警参数',
    setup: '初始化管理员',
    login: '登录',
    logout: '退出登录'
  };

  // ---------------------------------------------------------------- 工具

  function fmtBytes(n) {
    if (!n || n < 0) return '0 B';
    var units = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB'];
    var i = 0;
    var v = n;
    while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
    return (i === 0 ? v.toFixed(0) : v.toFixed(v < 10 ? 2 : 1)) + ' ' + units[i];
  }

  function fmtRate(n) {
    if (!n || n < 0) return '0 B/s';
    return fmtBytes(n) + '/s';
  }

  function fmtPct(n) {
    if (typeof n !== 'number' || isNaN(n)) return '—';
    return n.toFixed(n < 10 ? 1 : 0) + '%';
  }

  function fmtUptime(sec) {
    if (!sec) return '—';
    var d = Math.floor(sec / 86400);
    var h = Math.floor((sec % 86400) / 3600);
    var m = Math.floor((sec % 3600) / 60);
    if (d > 0) return d + ' 天 ' + h + ' 小时';
    if (h > 0) return h + ' 小时 ' + m + ' 分';
    if (m > 0) return m + ' 分';
    return sec + ' 秒';
  }

  function fmtAgo(unixSec) {
    if (!unixSec) return '从未';
    var diff = Math.max(0, Math.floor(Date.now() / 1000) - unixSec);
    if (diff < 5) return '刚刚';
    if (diff < 60) return diff + ' 秒前';
    if (diff < 3600) return Math.floor(diff / 60) + ' 分钟前';
    if (diff < 86400) return Math.floor(diff / 3600) + ' 小时前';
    return Math.floor(diff / 86400) + ' 天前';
  }

  function fmtClock(unixSec) {
    if (!unixSec) return '—';
    var d = new Date(unixSec * 1000);
    var p = function (n) { return (n < 10 ? '0' : '') + n; };
    return p(d.getHours()) + ':' + p(d.getMinutes()) + ':' + p(d.getSeconds());
  }

  // fmtTime 给操作记录用：审计是跨天翻的，只给时分秒分不清是不是今天。
  //
  // 之前这张表调的是一个根本不存在的 fmtTime()，ReferenceError 被 loadAudit 的
  // catch 吞成一句 toast —— 表现是"表格永远空的"，控制台之外看不出哪里错了。
  function fmtTime(unixSec) {
    if (!unixSec) return '—';
    var d = new Date(unixSec * 1000);
    var p = function (n) { return (n < 10 ? '0' : '') + n; };
    return p(d.getMonth() + 1) + '-' + p(d.getDate()) + ' ' +
      p(d.getHours()) + ':' + p(d.getMinutes()) + ':' + p(d.getSeconds());
  }

  var STATUS_TEXT = { online: '在线', stale: '抖动', offline: '离线', unknown: '未知' };

  function barClass(pct) {
    if (pct >= 90) return 'bad';
    if (pct >= 70) return 'warn';
    return '';
  }

  function toast(message) {
    el.toast.textContent = message;
    el.toast.hidden = false;
    window.clearTimeout(toast._timer);
    toast._timer = window.setTimeout(function () { el.toast.hidden = true; }, 2600);
  }

  // ---------------------------------------------------------------- API

  // BASE 是"面板被部署在哪个路径下"，用于支持子路径反代
  // （例如域名后面挂一层 /probe/，而不是单独开子域名）。
  //
  // 所有资源与接口都用它拼前缀：不能出现写死的绝对接口路径
  // （那会让子路径部署时 HTML 能打开、但脚本与接口全 404，页面一片空白）。
  var BASE = (function () {
    var p = window.location.pathname || '/';
    if (p.charAt(p.length - 1) !== '/') {
      var i = p.lastIndexOf('/');
      p = i >= 0 ? p.slice(0, i + 1) : '/';
    }
    return p;
  })();

  function apiURL(path) {
    return BASE + String(path).replace(/^\//, '');
  }

  function api(path, options) {
    var opts = options || {};
    var headers = { 'Accept': 'application/json' };
    if (opts.body !== undefined) headers['Content-Type'] = 'application/json';
    if (opts.method && opts.method !== 'GET') headers['X-CSRF-Token'] = session.csrf_token;
    return fetch(apiURL(path), {
      method: opts.method || 'GET',
      headers: headers,
      credentials: 'same-origin',
      cache: 'no-store',
      body: opts.body === undefined ? undefined : JSON.stringify(opts.body)
    }).then(function (res) {
      var isJSON = (res.headers.get('Content-Type') || '').indexOf('application/json') >= 0;
      return (isJSON ? res.json().catch(function () { return {}; }) : Promise.resolve({}))
        .then(function (data) {
          if (!res.ok) {
            var err = new Error((data.error && data.error.message) || ('请求失败（HTTP ' + res.status + '）'));
            err.status = res.status;
            err.code = data.error && data.error.code;
            throw err;
          }
          return data;
        });
    });
  }

  // ---------------------------------------------------------------- 视图切换

  function setView(name) {
    el.viewSetup.hidden = name !== 'setup';
    el.viewLogin.hidden = name !== 'login';
    el.viewHome.hidden = name !== 'home';
    el.viewDetail.hidden = name !== 'detail';
    el.viewSettings.hidden = name !== 'settings';
    el.btnAdd.hidden = name !== 'home';
    // 设置页自带「← 返回」与左栏导航，顶栏那个「设置」在设置页上只会把人从
    // 当前栏弹回第一栏，所以它在设置页里也藏起来。
    el.btnSettings.hidden = name === 'setup' || name === 'login' || name === 'settings';
    el.btnLogout.hidden = name === 'setup' || name === 'login';
    el.live.hidden = name === 'setup' || name === 'login';
  }

  // ---------------------------------------------------------------- 首页渲染

  function createCard(dto) {
    var root = document.createElement('div');
    root.className = 'card clickable';
    root.addEventListener('click', function () {
      window.location.hash = '#/n/' + dto.id;
    });

    var head = document.createElement('div');
    head.className = 'card-head';
    var left = document.createElement('div');
    left.style.minWidth = '0';
    var name = document.createElement('div');
    name.className = 'card-name';
    var sub = document.createElement('div');
    sub.className = 'card-sub';
    left.appendChild(name);
    left.appendChild(sub);

    var status = document.createElement('span');
    status.className = 'status';
    var dot = document.createElement('span');
    dot.className = 'dot';
    var statusText = document.createElement('span');
    status.appendChild(dot);
    status.appendChild(statusText);
    head.appendChild(left);
    head.appendChild(status);

    var bars = document.createElement('div');
    bars.className = 'bars';
    var barRefs = {};
    [['cpu', 'CPU'], ['mem', '内存'], ['disk', '磁盘'], ['quota', '流量']].forEach(function (item) {
      var row = document.createElement('div');
      row.className = 'bar-row';
      if (item[0] === 'quota') row.hidden = true; // 没设额度就不占位置
      var label = document.createElement('span');
      label.className = 'bar-label';
      label.textContent = item[1];
      var track = document.createElement('div');
      track.className = 'bar';
      var fill = document.createElement('i');
      track.appendChild(fill);
      var value = document.createElement('span');
      value.className = 'bar-value';
      row.appendChild(label);
      row.appendChild(track);
      row.appendChild(value);
      bars.appendChild(row);
      barRefs[item[0]] = { row: row, fill: fill, value: value };
    });

    var foot = document.createElement('div');
    foot.className = 'card-foot';
    var refs = {};
    // 「面板延迟」而不是「延迟」：这个值测的是 Agent 到面板本身的 WebSocket 往返
    // （走 Cloudflare 隧道时恒为 ~100ms），跟详情页那张延迟图里的探测结果不是一回事，
    // 用同一个名字会让人以为卡片上的数字就是到目标的延迟。
    [['net', '网络'], ['traffic', '本月'], ['lat', '面板延迟'], ['up', 'Uptime'], ['seen', '最后通信']].forEach(function (item) {
      var span = document.createElement('span');
      var label = document.createTextNode(item[1] + ' ');
      var b = document.createElement('b');
      span.appendChild(label);
      span.appendChild(b);
      foot.appendChild(span);
      refs[item[0]] = b;
    });

    root.appendChild(head);
    root.appendChild(bars);
    root.appendChild(foot);

    var card = {
      root: root,
      refs: {
        name: name, sub: sub, dot: dot, status: status,
        cpu: barRefs.cpu, mem: barRefs.mem, disk: barRefs.disk, quota: barRefs.quota,
        net: refs.net, traffic: refs.traffic, lat: refs.lat, up: refs.up, seen: refs.seen
      }
    };
    return card;
  }

  function updateCard(card, dto) {
    var r = card.refs;
    r.name.textContent = dto.name;

    var subParts = [];
    if (dto.group_name) subParts.push(dto.group_name);
    if (dto.region) subParts.push(dto.region);
    r.sub.textContent = subParts.join(' · ');

    var status = dto.status || 'unknown';
    r.dot.className = 'dot ' + status;
    r.status.textContent = STATUS_TEXT[status] || status;

    [['cpu', dto.cpu_pct], ['mem', dto.mem_pct], ['disk', dto.disk_pct]].forEach(function (item) {
      var ref = r[item[0]];
      var pct = typeof item[1] === 'number' ? Math.max(0, Math.min(100, item[1])) : 0;
      ref.fill.style.width = pct + '%';
      ref.fill.className = barClass(pct);
      ref.value.textContent = fmtPct(item[1]);
    });

    r.net.textContent = '↑ ' + fmtRate(dto.tx_rate) + ' ↓ ' + fmtRate(dto.rx_rate);
    r.lat.textContent = dto.lat_ms > 0 ? dto.lat_ms.toFixed(1) + ' ms' : '—';
    r.up.textContent = fmtUptime(dto.uptime_sec);
    r.seen.textContent = dto.last_seen ? fmtAgo(dto.last_seen) : '从未';

    // 本周期流量（有额度时额外显示一条额度进度）
    var cycleUsed = (dto.traffic_cycle_rx || 0) + (dto.traffic_cycle_tx || 0);
    if (dto.traffic_limit > 0) {
      r.traffic.textContent = fmtBytes(cycleUsed) + ' / ' + fmtBytes(dto.traffic_limit);
      var pct = Math.max(0, Math.min(999, dto.traffic_pct || 0));
      r.quota.row.hidden = false;
      r.quota.fill.style.width = Math.min(100, pct) + '%';
      r.quota.fill.className = pct >= 100 ? 'bad'
        : (dto.traffic_warn_pct > 0 && pct >= dto.traffic_warn_pct ? 'warn' : '');
      r.quota.value.textContent = pct.toFixed(pct < 10 ? 1 : 0) + '%';
    } else {
      r.traffic.textContent = fmtBytes(cycleUsed);
      r.quota.row.hidden = true;
    }

    card.root.title = dto.name + (dto.observed_ip ? ' · ' + dto.observed_ip : '');
  }

  function renderNode(dto) {
    var card = cards.get(dto.id);
    if (!card) {
      card = createCard(dto);
      cards.set(dto.id, card);
      el.grid.appendChild(card.root);
    }
    updateCard(card, dto);
  }

  function renderSummary(summary, ts) {
    el.sumOnline.textContent = summary.online;
    el.sumTotal.textContent = summary.total;
    el.sumStale.textContent = summary.stale;
    el.sumOffline.textContent = summary.offline;
    el.sumUnknown.textContent = summary.unknown;
    el.sumUnknownWrap.hidden = !summary.unknown;
    el.updated.textContent = ts ? '更新于 ' + fmtClock(ts) : '—';
    el.empty.hidden = summary.total > 0;
  }

  function applyPayload(payload) {
    if (!payload || !payload.nodes) return;
    payload.nodes.forEach(function (dto) {
      nodes.set(dto.id, dto);
      renderNode(dto);
      if (detail.id && dto.id === detail.id) {
        // 详情页的实时字段跟着一起变（图表按 30 秒整段刷新，不在每秒里重画）。
        detail.node = dto;
        if (!el.viewDetail.hidden) renderDetailInfo();
      }
    });
    renderSummary(payload.summary, payload.ts);
  }

  // ---------------------------------------------------------------- 实时通道

  function setLive(ok, text) {
    streamOk = ok;
    el.liveDot.className = 'dot ' + (ok ? 'online' : 'offline');
    el.liveText.textContent = text;
  }

  function connectStream() {
    if (source) source.close();
    var errors = 0;
    source = new EventSource(apiURL('/api/v1/stream'));
    source.addEventListener('open', function () { setLive(true, '实时'); });
    source.addEventListener('nodes', function (event) {
      errors = 0;
      setLive(true, '实时');
      try {
        applyPayload(JSON.parse(event.data));
      } catch (err) {
        setLive(false, '数据异常');
      }
    });
    source.addEventListener('error', function () {
      // EventSource 会自动重连；这里只把状态显示出来。
      errors++;
      setLive(false, '已断开，重连中');
      if (errors >= 3) {
        errors = 0;
        // 重连一直失败：可能是会话过期或被别处登出，复查一次会话状态。
        refreshSession().then(function (loggedIn) {
          if (!loggedIn) resetHome();
        }).catch(function () { /* 服务端不可达，继续重连 */ });
      }
    });
  }

  function stopStream() {
    if (source) { source.close(); source = null; }
    setLive(false, '未连接');
  }

  function resetHome() {
    stopStream();
    closeDetail();
    session = { authenticated: false, needs_setup: false, username: '', csrf_token: '' };
    // 图表可见性是"当前登录者"的设置，退出后必须丢掉：
    // 否则下一位登录者在自己那份设置到位之前会看到上一位的图表组合。
    visibleCharts = null;
    cards.forEach(function (card) { card.root.remove(); });
    cards.clear();
    nodes.clear();
  }

  // ---------------------------------------------------------------- 加载数据

  function loadNodes() {
    return api('/api/v1/nodes').then(function (data) {
      var seen = new Set();
      data.nodes.forEach(function (dto) {
        seen.add(dto.id);
        nodes.set(dto.id, dto);
        renderNode(dto);
      });
      nodes.forEach(function (_dto, id) {
        if (!seen.has(id)) {
          var card = cards.get(id);
          if (card) { card.root.remove(); cards.delete(id); }
          nodes.delete(id);
        }
      });
      renderSummary(data.summary, Math.floor(Date.now() / 1000));
    });
  }

  // ---------------------------------------------------------------- 节点详情

  var RANGE_X_FORMAT = {
    '1h': function (ts) { return clockOf(ts); },
    '6h': function (ts) { return clockOf(ts); },
    '12h': function (ts) { return clockOf(ts); },
    '1d': function (ts) { return clockOf(ts); },
    '3d': function (ts) { return dateOf(ts); },
    '7d': function (ts) { return dateOf(ts); }
  };

  function clockOf(ts) {
    var d = new Date(ts * 1000);
    var p = function (n) { return (n < 10 ? '0' : '') + n; };
    return p(d.getHours()) + ':' + p(d.getMinutes());
  }

  function dateOf(ts) {
    var d = new Date(ts * 1000);
    var p = function (n) { return (n < 10 ? '0' : '') + n; };
    return p(d.getMonth() + 1) + '-' + p(d.getDate());
  }

  function fmtAxisBytes(v) {
    if (v >= 1048576) return (v / 1048576).toFixed(v < 10485760 ? 1 : 0) + 'M';
    if (v >= 1024) return (v / 1024).toFixed(0) + 'K';
    return v.toFixed(0);
  }

  // aggregate 按目标间隔把点合并到整齐的时间网格上（手机端用，绝不插值造点）。
  function aggregate(points, targetSec) {
    if (!targetSec || targetSec <= 0 || points.length === 0) return points;
    var out = [];
    var cur = null;
    points.forEach(function (p) {
      var bucket = Math.floor(p[0] / targetSec) * targetSec;
      if (!cur || cur[0] !== bucket) {
        cur = [bucket, 0, 0, 0];
        out.push(cur);
      }
      cur[1] += p[1];
      cur[2] = Math.max(cur[2], p[2]);
      cur[3] += 1;
    });
    return out.map(function (c) { return [c[0], c[1] / c[3], c[2]]; });
  }

  function seriesFor(points) {
    var meta = rangeMeta(detail.range);
    var mobile = window.matchMedia(MOBILE_QUERY).matches;
    return aggregate(points, mobile ? meta.mobile_agg_sec : 0);
  }

  function rangeMeta(rangeKey) {
    for (var i = 0; i < detail.ranges.length; i++) {
      if (detail.ranges[i].key === rangeKey) return detail.ranges[i];
    }
    return { key: rangeKey, tick_base_sec: 600, tick_label_sec: 600, mobile_agg_sec: 0 };
  }

  function chartFor(key, canvasId) {
    var chart = detail.charts.get(key);
    if (chart) return chart;
    var canvas = $(canvasId);
    if (!canvas) return null;
    chart = window.ProbeChart.create(canvas, {
      xFormat: RANGE_X_FORMAT[detail.range] || clockOf
    });
    detail.charts.set(key, chart);
    return chart;
  }

  function renderRangeButtons() {
    el.detailRanges.textContent = '';
    detail.ranges.forEach(function (r) {
      var b = document.createElement('button');
      b.type = 'button';
      b.className = 'range-btn' + (r.key === detail.range ? ' active' : '');
      b.textContent = r.key;
      b.addEventListener('click', function () {
        if (detail.range === r.key) return;
        detail.range = r.key;
        renderRangeButtons();
        loadSeries();
        // 延迟图的范围由 /ping 自己按 range 聚合，所以要跟着档位重新请求一次
        // （目标列表不重取：它是配置，跟时间范围无关）。
        loadPingChart();
      });
      el.detailRanges.appendChild(b);
    });
  }

  function infoRow(dl, label, value) {
    var dt = document.createElement('dt');
    dt.textContent = label;
    var dd = document.createElement('dd');
    if (value instanceof Node) {
      dd.appendChild(value);
    } else {
      dd.textContent = value;
    }
    dl.appendChild(dt);
    dl.appendChild(dd);
  }

  function statusSpan(status) {
    var span = document.createElement('span');
    span.className = 'status';
    var dot = document.createElement('span');
    dot.className = 'dot ' + status;
    var text = document.createElement('span');
    text.textContent = STATUS_TEXT[status] || status;
    span.appendChild(dot);
    span.appendChild(text);
    return span;
  }

  // localIPText 把 Agent 自报的本机地址拼成一行文本。
  //
  // 两个字段都是可选的：只有 IPv6 的机器只有 local_ip6，反之亦然；两个都没有
  // （Agent 取不到路由、或对端是旧版 Agent 不带这两个字段）时显示 — 而不是
  // 空字符串 —— 空白会被读成"界面没渲染出来"。
  function localIPText(node) {
    var parts = [];
    if (node.local_ip) parts.push(node.local_ip);
    if (node.local_ip6) parts.push(node.local_ip6);
    return parts.length ? parts.join(' / ') : '—';
  }

  // 币种符号表。只映射这几种常见币种，其余一律按"金额 + 代码"显示：
  // 猜一个符号出来（比如给 CHF 配个 $）比不猜更容易误读金额。
  var CURRENCY_SYMBOL = { CNY: '¥', USD: '$', EUR: '€', JPY: '¥', GBP: '£' };
  var BILLING_TEXT = { 1: '/ 月', 3: '/ 季', 6: '/ 半年', 12: '/ 年' };

  function fmtMoney(cents, currency) {
    var amount = (Math.max(0, cents || 0) / 100).toFixed(2);
    var code = String(currency || '').toUpperCase();
    if (!code) return amount; // 没填货币就只显示数字，不硬塞一个符号
    var symbol = CURRENCY_SYMBOL[code];
    return symbol ? symbol + amount + ' ' + code : amount + ' ' + code;
  }

  function billingText(months) {
    if (!months) return '';
    return BILLING_TEXT[months] || '/ ' + months + ' 个月';
  }

  // renderDetailInfo 按"硬件 / 系统 / 存储 / 网络 / 流量"分组渲染。
  //
  // 以前所有字段挤在一个 <dl> 里：四十多行连成一片，找一个值要滚很久，
  // 而且"内存（含 swap）""CPU（含核数）"这种合并行在窄屏上会折成两行更难认。
  function renderDetailInfo() {
    var node = detail.node;
    if (!node) return;

    el.detailName.textContent = node.name;
    el.detailDot.className = 'dot ' + node.status;
    el.detailStatus.textContent = STATUS_TEXT[node.status] || node.status;

    var hw = el.infoHardware;
    var sys = el.infoSystem;
    var sto = el.infoStorage;
    var net = el.infoNetwork;
    var tra = el.infoTraffic;
    [hw, sys, sto, net, tra].forEach(function (dl) { dl.textContent = ''; });

    // 硬件信息
    infoRow(hw, 'CPU 使用率', fmtPct(node.cpu_pct));
    infoRow(hw, 'CPU 型号', node.cpu_model || '—');
    infoRow(hw, '核心数', node.cpu_cores ? node.cpu_cores + ' 核' : '—');
    infoRow(hw, '负载 1 分钟', node.load1 ? node.load1.toFixed(2) : '—');

    // 系统信息
    infoRow(sys, '状态', statusSpan(node.status));
    infoRow(sys, '最后通信', node.last_seen ? fmtAgo(node.last_seen) : '从未');
    infoRow(sys, '操作系统', node.os_name || '—');
    infoRow(sys, '内核', node.kernel || '—');
    infoRow(sys, '运行时间（Uptime）', fmtUptime(node.uptime_sec));
    infoRow(sys, 'Agent 版本', node.agent_version || '—');

    // 存储信息
    infoRow(sto, '内存', fmtPct(node.mem_pct));
    infoRow(sto, '内存交换', fmtPct(node.swap_pct));
    infoRow(sto, '磁盘', diskSummary(node));

    // 网络信息
    infoRow(net, '实时网络', '↑ ' + fmtRate(node.tx_rate) + '  ↓ ' + fmtRate(node.rx_rate));
    infoRow(net, '累计流量', '↑ ' + fmtBytes(node.tx_total) + '  ↓ ' + fmtBytes(node.rx_total));
    // 这一格以前叫「延迟」，但它的值是 Agent 到**面板自身**的 WebSocket ping/pong
    // 往返（走 Cloudflare 隧道时恒为 ~100ms），不是到任何探测目标的延迟。
    // 标签写清楚，免得和下面延迟图里的探测结果当成一回事。
    infoRow(net, '面板延迟', node.lat_ms > 0 ? node.lat_ms.toFixed(1) + ' ms' : '—');
    infoRow(net, '监控网卡', node.iface || '—');
    // 「本机地址」与「来源 IP」是两回事，标签不能含糊：
    // 前者是 Agent 自己算出来的机器地址（UDP connect 取源地址），后者是服务端
    // 看到的 TCP 来源。Agent 与探针同机、或走 Cloudflare Tunnel 时，后者恒为
    // 127.0.0.1；节点在 NAT/代理后面时，只有前者才是机器自己的地址。
    infoRow(net, '本机地址', localIPText(node));
    infoRow(net, '来源 IP', node.observed_ip || '—');

    // 流量信息（今日/本周期/历史累计是三个不同口径，标签写清楚免得看串）
    var u24 = detail.uptime['1d'];
    var u7 = detail.uptime['7d'];
    infoRow(tra, '今日流量', '↓ ' + fmtBytes(node.traffic_today_rx) + '  ↑ ' + fmtBytes(node.traffic_today_tx));
    infoRow(tra, '本周期流量', trafficCycleText(node));
    infoRow(tra, '历史累计流量', '↓ ' + fmtBytes(node.traffic_total_rx) + '  ↑ ' + fmtBytes(node.traffic_total_tx));
    if (node.cycle_start) {
      infoRow(tra, '计费周期', node.cycle_start + ' → ' + node.cycle_end + '（每月 ' + node.reset_day + ' 日重置）');
    }
    infoRow(tra, '可用率 24h', u24 && u24.has_data ? u24.pct.toFixed(2) + '%' : '—');
    infoRow(tra, '可用率 7d', u7 && u7.has_data ? u7.pct.toFixed(2) + '%' : '—');

    renderDetailStats();
  }

  // renderDetailStats 填顶部四张汇总卡。
  //
  // 没填价格时价格三格显示 —（而不是 ¥0.00）：这一排讲的是"这台机器花了多少钱、
  // 还剩多少"，写 0 会被读成"免费"，比留白更容易误判。
  // 「剩余时间」只看到期日，与填没填价格无关，所以它单独判断。
  function renderDetailStats() {
    var node = detail.node;
    if (!node) return;

    el.statLeft.textContent = node.expires_at > 0 ? node.remaining_days + ' 天' : '—';

    if (!(node.price_cents > 0)) {
      el.statPrice.textContent = '—';
      el.statMonthly.textContent = '—';
      el.statValue.textContent = '—';
      return;
    }

    var currency = node.currency || '';
    // 剩余天数与剩余价值都由服务端算好（前端不做算术，PC 与手机看到的一定一致）。
    el.statPrice.textContent = [fmtMoney(node.price_cents, currency), billingText(node.billing_months)].join(' ').trim();
    el.statMonthly.textContent = [fmtMoney(node.monthly_cents, currency), '/ 月'].join(' ');
    el.statValue.textContent = fmtMoney(node.remaining_value_cents, currency);
  }

  // 详情页的内容分散在汇总排与 5 张信息卡里，切换节点时必须整块清空，
  // 否则新节点的数据到位之前会一直显示上一个节点的数字。
  function clearDetailPanels() {
    [el.infoHardware, el.infoSystem, el.infoStorage, el.infoNetwork, el.infoTraffic]
      .forEach(function (dl) { dl.textContent = ''; });
    [el.statPrice, el.statMonthly, el.statLeft, el.statValue]
      .forEach(function (node) { node.textContent = '—'; });
  }

  // ---------------------------------------------------------------- 图表可见性

  function chartVisible(key) {
    return visibleCharts === null || visibleCharts.indexOf(key) >= 0;
  }

  // applyChartVisibility 只切换 chart-block 的显隐，不销毁图表实例：
  // 勾回来的时候还能复用同一个 canvas 与事件监听。
  //
  // 六张全被取消勾选时，连外层的图表卡片一起收起来 —— 否则页面上会留一个
  // 只有标题的空边框，看着像加载失败。
  function applyChartVisibility() {
    if (!el.detailCharts) return;
    var shown = 0;
    Array.prototype.forEach.call(el.detailCharts.querySelectorAll('.chart-block'), function (block) {
      var on = chartVisible(block.dataset.chart);
      block.hidden = !on;
      if (on) shown++;
    });
    el.detailCharts.hidden = shown === 0;
  }

  function setChartVisibility(visible) {
    if (!visible || typeof visible.length !== 'number') return; // 服务端没给就保持"全部显示"
    visibleCharts = Array.prototype.slice.call(visible);
    applyChartVisibility();
  }

  // loadChartVisibility 拉一次可见性。失败不报错：宁可多画几张图，
  // 也不能因为一个附加设置让整个页面停在"加载中"。
  function loadChartVisibility() {
    return api('/api/v1/settings').then(function (data) {
      setChartVisibility(data.charts && data.charts.visible);
    }).catch(function () { /* 保持全部显示 */ });
  }

  function diskSummary(node) {
    var disks = node.disks || [];
    if (disks.length === 0) return '—';
    var parts = disks.slice(0, 3).map(function (d) {
      return d.mount + ' ' + fmtPct(d.pct);
    });
    if (disks.length > 3) parts.push('+' + (disks.length - 3));
    return parts.join('，');
  }

  // trafficCycleText 描述本周期用量；设了额度时带上额度与百分比。
  function trafficCycleText(node) {
    var used = (node.traffic_cycle_rx || 0) + (node.traffic_cycle_tx || 0);
    var text = '↓ ' + fmtBytes(node.traffic_cycle_rx) + '  ↑ ' + fmtBytes(node.traffic_cycle_tx)
      + '（共 ' + fmtBytes(used) + '）';
    if (node.traffic_limit > 0) {
      text += ' / ' + fmtBytes(node.traffic_limit) + '（' + (node.traffic_pct || 0).toFixed(1) + '%）';
    }
    return text;
  }

  // loadTrafficChart 画"近 7 天流量"：流量天生按天统计，所以它不跟随六档范围。
  function loadTrafficChart() {
    if (!detail.id) return Promise.resolve();
    // 隐藏的图表不发请求：服务端也就省下一次按天聚合的查询。
    if (!chartVisible('traffic')) return Promise.resolve();
    return api('/api/v1/nodes/' + detail.id + '/traffic?days=7').then(function (data) {
      var chart = chartFor('traffic', 'chart-traffic');
      if (!chart) return;
      var down = [];
      var up = [];
      (data.points || []).forEach(function (p) {
        // 每天两种方向各画一条；X 轴仍在本地零点。
        down.push([p[0], p[1], p[1]]);
        up.push([p[0], p[2], p[2]]);
      });
      chart.setData([
        { label: '下行', color: '#2563eb', points: down },
        { label: '上行', color: '#16a34a', points: up }
      ], {
        yMax: 0,
        unit: '',
        yFormat: fmtAxisBytes,
        xFormat: dateOf,
        tickBaseSec: 86400,
        tickLabelSec: 86400,
        showMax: false
      });
    }).catch(function () { /* 忽略：详情页其它内容照常显示 */ });
  }

  function loadSeries() {
    if (!detail.id) return Promise.resolve();
    var meta = rangeMeta(detail.range);
    var base = '/api/v1/nodes/' + detail.id + '/series?range=' + encodeURIComponent(detail.range) + '&metric=';
    // 只请求勾选了的图表：被隐藏的图谁也不看，为它查库+传数据是纯浪费
    // （网络图是上下行两条曲线，要么都取要么都不取）。
    //
    // 延迟图不在这个列表里：它画的不是 Agent 上报的 lat_ms，而是配置好的探测目标，
    // 数据来自 /ping（见 loadPingChart）。
    var metrics = [];
    if (chartVisible('cpu')) metrics.push('cpu');
    if (chartVisible('mem')) metrics.push('mem');
    if (chartVisible('disk')) metrics.push('disk');
    if (chartVisible('net')) metrics.push('net_down', 'net_up');
    if (metrics.length === 0) return Promise.resolve();

    return Promise.all(metrics.map(function (m) {
      return api(base + m).then(function (data) { return { metric: m, data: data }; })
        .catch(function () { return null; });
    })).then(function (results) {
      var byMetric = {};
      results.forEach(function (r) { if (r) byMetric[r.metric] = r.data; });

      var xFormat = RANGE_X_FORMAT[detail.range] || clockOf;
      var tick = meta.tick_label_sec || 600;

      var pctOpts = { yMax: 100, unit: '%', yFormat: function (v) { return v.toFixed(0) + '%'; }, tickLabelSec: tick, xFormat: xFormat, showMax: true };
      var rateOpts = { yMax: 0, unit: '/s', yFormat: fmtAxisBytes, tickLabelSec: tick, xFormat: xFormat, showMax: true };

      if (chartVisible('cpu')) setChart('cpu', 'chart-cpu', byMetric.cpu, [{ label: 'CPU', color: '#2563eb' }], pctOpts);
      if (chartVisible('mem')) setChart('mem', 'chart-mem', byMetric.mem, [{ label: '内存', color: '#7c3aed' }], pctOpts);
      if (chartVisible('disk')) setChart('disk', 'chart-disk', byMetric.disk, [{ label: '磁盘', color: '#0891b2' }], pctOpts);
      if (chartVisible('net')) {
        setChart('net', 'chart-net', null, null, rateOpts, [
          { label: '下行', color: '#2563eb', data: byMetric.net_down },
          { label: '上行', color: '#16a34a', data: byMetric.net_up }
        ]);
      }
    });
  }

  // setChart 既支持"一个指标一条线"，也支持"一个图多条线"（网络图）。
  function setChart(key, canvasId, data, seriesSpec, options, multiSpec) {
    var chart = chartFor(key, canvasId);
    if (!chart) return;
    var specs = multiSpec || (data ? seriesSpec.map(function (s) {
      return { label: s.label, color: s.color, points: seriesFor(data.points) };
    }) : []);
    var series = specs.map(function (s) {
      return {
        label: s.label,
        color: s.color,
        points: s.points || (s.data ? seriesFor(s.data.points) : [])
      };
    });
    chart.setData(series, options);
  }

  // ---------------------------------------------------------------- 延迟图（探测目标）
  //
  // 这里的"延迟"是用户配置的探测目标（Agent 每 N 秒探一次，服务端下发），
  // 不是 Agent 到面板自身的 WebSocket 往返 —— 后者走 Cloudflare 隧道时恒为
  // ~100ms，画成曲线没有任何参考价值（它现在仍在「网络信息」卡里，叫「面板延迟」）。

  // pingTargetLabel 是曲线与勾选框上显示的名字：名称允许留空，留空就用地址。
  function pingTargetLabel(t) {
    return t.label || t.host || ('目标 #' + t.id);
  }

  function pingColor(index) {
    return PING_COLORS[index % PING_COLORS.length];
  }

  // latHiddenSet 把 localStorage 里"被取消勾选的目标 id"读成一张查询表。
  //
  // 用表而不是数组：目标被删掉之后，存着的旧 id 在新数据里根本不会出现，
  // 查不到就等于没隐藏 —— 不用专门清理，也不会因此报错。
  function latHiddenSet() {
    var set = {};
    try {
      var parsed = JSON.parse(localStorage.getItem(PING_HIDDEN_KEY) || '[]');
      if (Object.prototype.toString.call(parsed) === '[object Array]') {
        parsed.forEach(function (id) { set[String(id)] = true; });
      }
    } catch (err) { /* 隐私模式或内容被改坏：当作一条都没隐藏 */ }
    return set;
  }

  function setLatHidden(ids) {
    try {
      localStorage.setItem(PING_HIDDEN_KEY, JSON.stringify(ids));
    } catch (err) { /* 存不下就只在本次会话里生效 */ }
  }

  // setLatEmpty 在 canvas 与空态提示之间切换：两者互斥。
  // 只藏提示不藏 canvas 的话，页面上会同时有一张"暂无数据"的空白图和一句提示。
  function setLatEmpty(message) {
    if (!el.latEmpty) return;
    el.latEmpty.textContent = message || '';
    el.latEmpty.hidden = !message;
    if (el.chartLat) el.chartLat.hidden = !!message;
  }

  // renderLatToggles 在延迟图上方渲染一行勾选框：每个**配置过的**目标一个，
  // 默认全选（没数据的目标也在，只是它的线画不出来）。
  function renderLatToggles(targets) {
    var box = el.latTargets;
    if (!box) return;
    box.textContent = '';
    if (!targets.length) {
      box.hidden = true;
      return;
    }
    var hidden = latHiddenSet();
    targets.forEach(function (t, i) {
      var label = document.createElement('label');
      label.className = 'check';
      var input = document.createElement('input');
      input.type = 'checkbox';
      input.checked = !hidden[String(t.id)];
      input.addEventListener('change', function () { toggleLatTarget(t.id, !input.checked); });
      var swatch = document.createElement('span');
      swatch.className = 'swatch';
      swatch.style.background = pingColor(i);
      var text = document.createElement('span');
      text.textContent = pingTargetLabel(t) + (t.has_data ? '' : '（暂无数据）');
      label.appendChild(input);
      label.appendChild(swatch);
      label.appendChild(text);
      box.appendChild(label);
    });
    box.hidden = false;
  }

  function toggleLatTarget(id, hide) {
    var hidden = latHiddenSet();
    if (hide) hidden[String(id)] = true;
    else delete hidden[String(id)];
    var ids = Object.keys(hidden).map(function (key) {
      return parseInt(key, 10);
    }).filter(function (n) { return !isNaN(n); });
    setLatHidden(ids);
    applyLatSeries();
  }

  function latChartOptions() {
    var meta = rangeMeta(detail.range);
    return {
      yMax: 0,
      unit: ' ms',
      yFormat: function (v) { return v.toFixed(0); },
      tickLabelSec: meta.tick_label_sec || 600,
      xFormat: RANGE_X_FORMAT[detail.range] || clockOf,
      showMax: true
    };
  }

  // applyLatSeries 按当前勾选状态把曲线重新塞进图表实例（不销毁重建：
  // 重建会连 canvas 上的鼠标监听与悬浮读数一起丢掉）。
  //
  // 每条 series 都带 targetId：勾选状态按 id 存，与曲线顺序无关，
  // 换个时间档位重画也不会错位。
  function applyLatSeries() {
    var hidden = latHiddenSet();
    var shown = detail.pingSeries.filter(function (s) {
      return !hidden[String(s.targetId)];
    });
    setChart('lat', 'chart-lat', null, null, latChartOptions(), shown);
  }

  // loadPingTargets 进详情页时取一次"配置了哪些探测目标"。
  //
  // 为什么不能只看 /ping 的返回：一个目标都没配时要**连请求都不发**、
  // 直接显示空态提示，而"到底有没有配"只有设置接口知道。
  function loadPingTargets() {
    if (!chartVisible('lat')) {
      detail.pingTargets = null;
      return Promise.resolve();
    }
    return api('/api/v1/settings').then(function (data) {
      detail.pingTargets = (data.ping && data.ping.targets) || [];
    }).catch(function () {
      // 取不到就当作"不知道"：宁可不画，也不要退回 Agent 自己上报的 lat_ms ——
      // 那个数是到面板自身的往返，跟探测目标毫无关系，画上去就是误导。
      detail.pingTargets = null;
    });
  }

  // loadPingChart 画延迟图：一个探测目标一条线，取点的 avg（与 /series 一致，
  // [ts, avg, max] 里只用前两个值；max 交给图表的峰值淡线）。
  function loadPingChart() {
    if (!detail.id) return Promise.resolve();
    if (!chartVisible('lat')) return Promise.resolve();
    if (detail.pingTargets === null) return Promise.resolve();
    if (detail.pingTargets.length === 0) {
      detail.pingSeries = [];
      renderLatToggles([]);
      applyLatSeries();
      setLatEmpty('还没有配置探测目标 —— 去「设置 → 延迟探测」添加。');
      return Promise.resolve();
    }
    return api('/api/v1/nodes/' + detail.id + '/ping?range=' + encodeURIComponent(detail.range)).then(function (data) {
      var targets = data.targets || [];
      setLatEmpty('');
      var series = [];
      targets.forEach(function (t, i) {
        // has_data:false 的目标不画线（服务端也会把它列出来），
        // 但下面的勾选框里仍然要有它 —— "这个目标一个点都没有"本身就是信息。
        if (!t.has_data) return;
        var points = t.points || [];
        if (points.length === 0) return;
        series.push({
          targetId: t.id,
          label: pingTargetLabel(t),
          color: pingColor(i),
          points: seriesFor(points)
        });
      });
      detail.pingSeries = series;
      renderLatToggles(targets);
      applyLatSeries();
    }).catch(function () { /* 忽略：详情页其它内容照常显示 */ });
  }

  function openDetail(id) {
    detail.id = id;
    setView('detail');
    el.detailName.textContent = '加载中…';
    clearDetailPanels();
    el.detailRanges.textContent = '';
    // 延迟图的状态一并清空：曲线、勾选框、空态都不能留着上一个节点的。
    // pingTargets 置 null 表示"还不知道有没有配目标"，这时不请求 /ping。
    detail.pingTargets = null;
    detail.pingSeries = [];
    renderLatToggles([]);
    setLatEmpty('');
    // 先按可见性把图表块藏好，再去请求数据：隐藏的图连一次请求都不发。
    applyChartVisibility();
    if (chartVisible('lat')) applyLatSeries();

    api('/api/v1/nodes/' + id).then(function (data) {
      detail.node = data.node;
      detail.uptime = data.uptime || {};
      detail.ranges = data.ranges || [];
      if (!detail.range && detail.ranges.length) detail.range = detail.ranges[0].key;
      renderDetailInfo();
      renderRangeButtons();
      // 目标列表与节点详情一起取（只取这一次），拿到之后才决定要不要请求 /ping。
      return Promise.all([
        loadSeries(),
        loadTrafficChart(),
        loadPingTargets().then(loadPingChart)
      ]);
    }).then(function () {
      // 图表容器尺寸只有在显示之后才有效，这里补一次重绘。
      detail.charts.forEach(function (chart) { chart.redraw(); });
      if (detail.timer) window.clearInterval(detail.timer);
      detail.timer = window.setInterval(function () {
        if (document.hidden) return;
        api('/api/v1/nodes/' + detail.id).then(function (data) {
          detail.node = data.node;
          detail.uptime = data.uptime || {};
          renderDetailInfo();
        }).catch(function () { /* 忽略瞬时错误 */ });
        loadSeries();
        loadTrafficChart();
        loadPingChart();
      }, DETAIL_REFRESH_MS);
    }).catch(function (err) {
      toast(err.message);
      window.location.hash = '#/';
    });
  }

  function closeDetail() {
    if (detail.timer) {
      window.clearInterval(detail.timer);
      detail.timer = null;
    }
    detail.id = 0;
    detail.node = null;
    detail.pingTargets = null;
    detail.pingSeries = [];
  }

  // ---------------------------------------------------------------- 节点编辑 / 删除

  // 新增与编辑共用同一个对话框，靠这个变量区分。
  var nodeDialogMode = 'create';

  function openNodeDialog(mode, dto) {
    nodeDialogMode = mode;
    el.nodeError.textContent = '';
    el.nodeTitle.textContent = mode === 'edit' ? '编辑节点' : '新增节点';
    el.nodeSubmit.textContent = mode === 'edit' ? '保存' : '创建';
    el.nodeEnabledWrap.hidden = mode !== 'edit';

    var d = dto || {};
    el['node-name'].value = d.name || '';
    el.nodeGroup.value = d.group_name || '';
    el.nodeRegion.value = d.region || '';
    el.nodeInterval.value = d.interval_sec || 1;
    // 金额在库里是"分"，表单里是"元"：只有这一处换算是必要的，其余地方一律用分。
    el['node-price'].value = d.price_cents ? (d.price_cents / 100).toFixed(2) : '';
    el.nodeCurrency.value = d.currency || '';
    el.nodeBilling.value = String(d.billing_months || 0);
    el.nodeTraffic.value = d.traffic_limit ? Math.round(d.traffic_limit / (1024 * 1024 * 1024)) : 0;
    el.nodeWarn.value = d.traffic_warn_pct || 80;
    el.nodeReset.value = d.reset_day || 1;
    el.nodeNote.value = d.note || '';
    el.nodeEnabled.checked = d.enabled === undefined ? true : !!d.enabled;
    el['node-expires'].value = d.expires_at ? new Date(d.expires_at * 1000).toISOString().slice(0, 10) : '';
    el.dlgNode.showModal();
  }

  function nodeFormPayload() {
    var expires = el['node-expires'].value;
    // 到期日按服务器本地零点处理，避免时区差一天。
    var expiresAt = 0;
    if (expires) {
      expiresAt = Math.floor(new Date(expires + 'T00:00:00').getTime() / 1000);
    }
    return {
      name: el['node-name'].value.trim(),
      group_name: el.nodeGroup.value.trim(),
      region: el.nodeRegion.value.trim(),
      note: el.nodeNote.value.trim(),
      interval_sec: parseInt(el.nodeInterval.value, 10) || 1,
      // 元 → 分：先四舍五入到整数分，避免 71.21 变成 7120.999999 再被截断成 7120。
      price_cents: Math.round((parseFloat(el['node-price'].value) || 0) * 100) || 0,
      currency: el.nodeCurrency.value.trim().toUpperCase(),
      billing_months: parseInt(el.nodeBilling.value, 10) || 0,
      traffic_limit: Math.max(0, Math.round((parseFloat(el.nodeTraffic.value) || 0) * 1024 * 1024 * 1024)),
      traffic_warn_pct: Math.min(100, Math.max(1, parseInt(el.nodeWarn.value, 10) || 80)),
      reset_day: Math.min(31, Math.max(1, parseInt(el.nodeReset.value, 10) || 1)),
      expires_at: expiresAt,
      enabled: el.nodeEnabled.checked
    };
  }

  // validateNodePricing 在提交前挡一次"填了价格没填周期"。
  //
  // 服务端也会拒（同一条规则），但那要等一个来回；这里立刻提示，用户不用
  // 盯着一个转圈的按钮猜哪里填错了。
  function validateNodePricing(payload) {
    if (payload.price_cents > 0 && payload.billing_months <= 0) {
      return '填了价格就要选计费周期';
    }
    return '';
  }

  function submitNodeForm(event) {
    event.preventDefault();
    el.nodeError.textContent = '';
    var payload = nodeFormPayload();
    var problem = validateNodePricing(payload);
    if (problem) {
      el.nodeError.textContent = problem;
      return;
    }
    el.nodeSubmit.disabled = true;
    var editing = nodeDialogMode === 'edit';
    var path = editing ? '/api/v1/nodes/' + detail.id : '/api/v1/nodes';

    api(path, { method: editing ? 'PATCH' : 'POST', body: payload }).then(function (data) {
      el.dlgNode.close();
      if (editing) {
        toast('已保存');
        // 详情页与首页一起刷新。
        return api('/api/v1/nodes/' + detail.id).then(function (fresh) {
          detail.node = fresh.node;
          detail.uptime = fresh.uptime || {};
          detail.ranges = fresh.ranges || [];
          renderDetailInfo();
          loadNodes();
        });
      }
      showToken(data.token, payload.name);
      return loadNodes();
    }).catch(function (err) {
      el.nodeError.textContent = err.message;
    }).then(function () {
      el.nodeSubmit.disabled = false;
    });
  }

  // confirmDialog 是删除节点 / 换 Token 共用的二次确认。
  function confirmDialog(title, text, warn, onOK) {
    el.confirmTitle.textContent = title;
    el.confirmText.textContent = text;
    el.confirmWarn.textContent = warn || '';
    el.confirmWarn.hidden = !warn;
    confirmAction = onOK;
    el.dlgConfirm.showModal();
  }

  function requestDeleteNode(id) {
    var name = detail.node ? detail.node.name : '该节点';
    confirmDialog('删除节点', '确定要删除「' + name + '」吗？',
      '该节点的全部历史数据、流量记录与告警状态会一起删除，无法恢复。',
      function () {
        api('/api/v1/nodes/' + id, { method: 'DELETE' }).then(function () {
          toast('节点已删除');
          window.location.hash = '#/';
        }).catch(function (err) { toast(err.message); });
      });
  }

  function requestRotateToken(id) {
    confirmDialog('重新生成 Token', '确定要重新生成该节点的 Token 吗？',
      '旧 Token 会立即失效，节点当前的连接会被断开；需要在 VPS 上更新 Token 后才会重新上报。',
      function () {
        api('/api/v1/nodes/' + id + '/token', { method: 'POST' }).then(function (data) {
          showToken(data.token, detail.node ? detail.node.name : '');
        }).catch(function (err) { toast(err.message); });
      });
  }

  // ---------------------------------------------------------------- 操作记录

  // resetAudit 把操作记录退回到"第一页之前"。
  //
  // 操作记录原本是一个独立页面，每次进入都从零拉；现在它是设置页里的一栏，
  // 设置页可以反复进出、也可以反复切栏回来 —— 不清空的话上一轮的行会跟新一轮
  // 叠在一起，"加载更多"还会带着上一轮的 before_id 继续往下翻。
  function resetAudit() {
    el.auditBody.textContent = '';
    el.auditEmpty.hidden = true;
    el.auditMore.hidden = true;
    auditBeforeID = 0;
  }

  function loadAudit() {
    var path = '/api/v1/audit?limit=50' + (auditBeforeID ? '&before_id=' + auditBeforeID : '');
    return api(path).then(function (data) {
      var entries = data.entries || [];
      entries.forEach(function (entry) {
        var tr = document.createElement('tr');
        [fmtTime(entry.ts), ACTION_TEXT[entry.action] || entry.action,
          entry.node_id ? '#' + entry.node_id : '—', entry.ip || '—', entry.detail
        ].forEach(function (value) {
          var td = document.createElement('td');
          td.textContent = value;
          tr.appendChild(td);
        });
        el.auditBody.appendChild(tr);
      });
      if (entries.length > 0) {
        auditBeforeID = entries[entries.length - 1].id;
      }
      el.auditMore.hidden = entries.length < 50;
      el.auditEmpty.hidden = el.auditBody.childNodes.length > 0;
    }).catch(function (err) {
      toast(err.message);
    });
  }

  // ---------------------------------------------------------------- 设置
  //
  // 设置是一个**整页视图**（#/settings/<栏>），不是对话框：六类设置挤在一个
  // <dialog> 里时只有一个「保存」，它串行 PUT 三个接口，哪一段失败都落到同一个
  // 提示上；整页之后每一栏各自保存、各自提示，还能深链到某一栏。

  // 栏名清单同时是导航与内容的顺序来源（HTML 里 7 个 data-pane 必须与它一致）。
  // 顺序即左栏从上到下的顺序：延迟探测排在仪表盘之后（都是"画什么"的设置）。
  var SETTINGS_PANES = ['notify', 'alert', 'dashboard', 'ping', 'security', 'server', 'audit'];

  // settingsPane 把栏名归一化：未知值（含空串）一律回落到第一栏。
  // 这样 #/settings/nope 这种手改/过期的地址不会打开一个六栏全隐藏的空白页。
  function settingsPane(name) {
    return SETTINGS_PANES.indexOf(name) >= 0 ? name : SETTINGS_PANES[0];
  }

  // settingsPaneFromHash 解析 #/settings/<栏>；不是设置路由时返回 null。
  function settingsPaneFromHash(hash) {
    var m = /^#\/settings(?:\/([^\/?#]+))?\/?$/.exec(hash);
    if (!m) return null;
    return m[1] ? decodeURIComponent(m[1]) : '';
  }

  // showSettingsPane 只切 hidden 与高亮，不重建 DOM：切栏是纯显示操作，
  // 重建会连带丢掉用户没保存的输入。
  function showSettingsPane(name) {
    var pane = settingsPane(name);
    Array.prototype.forEach.call(el.settingsNav.querySelectorAll('.nav-item'), function (btn) {
      // 用 classList 而不是重写 className：className 一旦整体赋值，
      // 以后往按钮上加的其它类名会被悄悄抹掉。
      var on = btn.dataset.pane === pane;
      btn.classList.toggle('active', on);
      btn.setAttribute('aria-current', on ? 'true' : 'false');
    });
    Array.prototype.forEach.call(el.settingsPanes.querySelectorAll('.pane'), function (sec) {
      sec.hidden = sec.dataset.pane !== pane;
    });
    return pane;
  }

  // clearSettingsHints 清掉各栏的提示（每次进设置页时用：上一轮遗留的"已保存"
  // 会让人以为这次也已经存过了）。提示为什么必须按栏分开，见 paneErrorNode()。
  function clearSettingsHints() {
    [el.notifyError, el.notifyOk, el.alertError, el.alertOk,
      el.dashboardError, el.dashboardOk, el.pingError, el.pingOk,
      el.securityError, el.securityOk]
      .forEach(function (node) { node.textContent = ''; });
  }

  // openSettings 打开设置页：切视图 → 选中栏 → 拉数据。
  //
  // 已经在设置页里时只切栏、不重新拉数据：用户可能在 A 栏填了一半再去看 B 栏，
  // 每切一次就把输入框回填成服务端的值，等于把没保存的编辑悄悄吃掉。
  // 反过来，"从别的视图进来"必须重新拉（服务器信息、Token 提示都可能变了）。
  function openSettings(pane) {
    var target = settingsPane(pane);
    if (!el.viewSettings.hidden) {
      showSettingsPane(target);
      return;
    }
    setView('settings');
    showSettingsPane(target);
    clearSettingsHints();
    el['tg-token'].value = '';

    // 操作记录跟着设置页一起进场：它是设置里的一栏，不是独立页面了。
    resetAudit();
    loadAudit();

    Promise.all([api('/api/v1/settings/telegram'), api('/api/v1/settings')]).then(function (results) {
      var cfg = results[0];
      var all = results[1];
      el.tgEnabled.checked = !!cfg.enabled;
      el.tgChat.value = cfg.chat_id || '';
      el.tgTokenHint.textContent = cfg.has_token
        ? '已经保存过 Token；留空表示不修改。'
        : '还没有保存 Token。';

      var alertCfg = all.alert || {};
      el.alertCooldown.value = alertCfg.cooldown || '';
      el.alertGrace.value = alertCfg.startup_grace || '';
      el.alertDebounce.value = alertCfg.debounce || '';
      el.alertRecover.value = alertCfg.recover_stable || '';

      // 图表勾选：先按服务端的值更新全局可见性，再照着它勾选复选框，
      // 两边不会出现"设置里勾着、详情页却关着"的不一致。
      setChartVisibility(all.charts && all.charts.visible);
      Array.prototype.forEach.call(el.chartToggles.querySelectorAll('input[data-chart]'), function (box) {
        box.checked = chartVisible(box.dataset.chart);
      });

      // 延迟探测的目标列表整块重建：进设置页时以服务端的值为准。
      renderPingEditor(all.ping);

      var info = all.server || {};
      el.serverInfo.textContent = '';
      [
        ['版本', (info.version || '—') + (info.commit && info.commit !== 'unknown' ? ' (' + info.commit + ')' : '')],
        ['监听地址', info.listen || '—'],
        ['时区', info.timezone || '—'],
        ['TLS', info.tls ? '已启用' : '未启用（建议用 Caddy/nginx 反代）'],
        ['历史保留', '10 秒桶 ' + (info.retention_10s || '—') + '，1 分钟桶 ' + (info.retention_1m || '—')],
        ['落盘周期', info.flush_interval || '—'],
        ['状态判定', '抖动 ' + (info.stale_after || '—') + '，离线 ' + (info.offline_after || '—')],
        ['流量增量上限', info.traffic_delta_max || '—'],
        ['节点数量', String(info.node_count == null ? '—' : info.node_count)],
        ['已运行', fmtUptime(info.uptime_sec)]
      ].forEach(function (row) {
        var dt = document.createElement('dt');
        dt.textContent = row[0];
        var dd = document.createElement('dd');
        dd.textContent = row[1];
        el.serverInfo.appendChild(dt);
        el.serverInfo.appendChild(dd);
      });
    }).catch(function (err) {
      // 拉不到设置时把错误落在当前栏里，而不是只弹一个转瞬即逝的 toast：
      // 用户需要知道"现在这些框里显示的不是服务端的值"。
      var node = paneErrorNode(target);
      if (node) node.textContent = '读取设置失败：' + err.message;
      toast(err.message);
    });
  }

  // paneErrorNode 返回某一栏的错误提示元素（"服务器信息""操作记录"两栏是只读的，
  // 没有提示位，返回 null）。
  //
  // 各栏的提示元素必须分开：以前六类设置共用 #settings-error/#settings-ok，
  // 一栏保存失败会在另一栏里冒出红字，看起来像是那一栏出了问题。
  function paneErrorNode(pane) {
    if (pane === 'notify') return el.notifyError;
    if (pane === 'alert') return el.alertError;
    if (pane === 'dashboard') return el.dashboardError;
    if (pane === 'ping') return el.pingError;
    if (pane === 'security') return el.securityError;
    return null;
  }

  // saveNotify 只保存 Telegram 这一栏。三个保存函数互不依赖：
  // 以前是一个按钮串行 PUT 三个接口，第一个失败后面两个就不会发出去，
  // 用户以为"全没存上"，其实只是其中一段的参数写错了。
  function saveNotify() {
    el.notifyError.textContent = '';
    el.notifyOk.textContent = '';
    el.notifySave.disabled = true;

    var telegram = {
      enabled: el.tgEnabled.checked,
      bot_token: el['tg-token'].value.trim(),
      chat_id: el.tgChat.value.trim()
    };

    api('/api/v1/settings/telegram', { method: 'PUT', body: telegram }).then(function (cfg) {
      // 保存成功后立刻清空 Token 输入框并把 hint 换成最新的：
      // Token 只用于"覆盖"，留在页面上等于把密钥摊在屏幕上。
      el['tg-token'].value = '';
      el.tgTokenHint.textContent = cfg.has_token
        ? '已经保存过 Token；留空表示不修改。'
        : '还没有保存 Token。';
      el.notifyOk.textContent = '已保存';
      toast('通知设置已保存');
    }).catch(function (err) {
      el.notifyError.textContent = err.message;
    }).then(function () {
      el.notifySave.disabled = false;
    });
  }

  function saveAlert() {
    el.alertError.textContent = '';
    el.alertOk.textContent = '';
    el.alertSave.disabled = true;

    var alertCfg = {
      cooldown: el.alertCooldown.value.trim(),
      startup_grace: el.alertGrace.value.trim(),
      debounce: el.alertDebounce.value.trim(),
      recover_stable: el.alertRecover.value.trim()
    };

    api('/api/v1/settings/alert', { method: 'PUT', body: alertCfg }).then(function (data) {
      // 回填服务端归一化后的值（例如 "1h" → "1h0m0s"）：不回填的话，
      // 框里留着用户写法的同时实际生效的是另一个值，下次打开又会跳一下。
      var saved = data.alert || {};
      el.alertCooldown.value = saved.cooldown || '';
      el.alertGrace.value = saved.startup_grace || '';
      el.alertDebounce.value = saved.debounce || '';
      el.alertRecover.value = saved.recover_stable || '';
      el.alertOk.textContent = '已保存';
      toast('告警参数已保存');
    }).catch(function (err) {
      el.alertError.textContent = err.message;
    }).then(function () {
      el.alertSave.disabled = false;
    });
  }

  function saveDashboard() {
    el.dashboardError.textContent = '';
    el.dashboardOk.textContent = '';
    el.dashboardSave.disabled = true;

    var charts = {
      visible: Array.prototype.filter.call(
        el.chartToggles.querySelectorAll('input[data-chart]'),
        function (box) { return box.checked; }
      ).map(function (box) { return box.dataset.chart; })
    };

    api('/api/v1/settings/charts', { method: 'PUT', body: charts }).then(function (data) {
      // 存完立刻把全局可见性同步过来：回到详情页时按新设置决定画哪几张、发哪些请求。
      // 详情页此刻必然是隐藏的（设置是整页视图），所以不用在这里重画图表 ——
      // openDetail() 进页时会按新的可见性重新拉数据并重绘。
      setChartVisibility(data.visible);
      el.dashboardOk.textContent = '已保存';
      toast('图表设置已保存');
    }).catch(function (err) {
      el.dashboardError.textContent = err.message;
    }).then(function () {
      el.dashboardSave.disabled = false;
    });
  }

  // ---------------------------------------------------------------- 延迟探测（设置栏）

  // 目标行是动态生成的（数量可变），所以控件的引用存在行对象里、放进 pingRows 数组，
  // 不去拼 id 字符串再 getElementById：拼出来的 id 既容易撞车，也用不上 main() 里
  // 那份从 DOM 自动登记的 el（拿不到就会静默变成 null）。
  var pingRows = [];
  var pingMaxTargets = 16;

  // pingCell 造一个"小标题 + 控件"的格子。用 <label> 把控件包起来做隐式关联，
  // 这样不必为每个动态控件生成 id。
  function pingCell(caption, control) {
    var cell = document.createElement('label');
    cell.className = 'ping-cell';
    var cap = document.createElement('span');
    cap.className = 'ping-cap';
    cap.textContent = caption;
    cell.appendChild(cap);
    cell.appendChild(control);
    return cell;
  }

  // newPingRow 造一行目标编辑器，返回行对象（id + 各控件引用 + 行元素）。
  function newPingRow(target) {
    var t = target || {};
    var row = { id: t.id || 0, wrap: null, refs: null };

    var wrap = document.createElement('div');
    wrap.className = 'ping-row';

    var labelInput = document.createElement('input');
    labelInput.type = 'text';
    labelInput.maxLength = 64;
    labelInput.spellcheck = false;
    labelInput.placeholder = '留空则显示地址';
    labelInput.value = t.label || '';

    var typeSelect = document.createElement('select');
    [['tcp', 'TCP'], ['icmp', 'ICMP']].forEach(function (item) {
      var opt = document.createElement('option');
      opt.value = item[0];
      opt.textContent = item[1];
      typeSelect.appendChild(opt);
    });
    typeSelect.value = t.type === 'icmp' ? 'icmp' : 'tcp';

    var hostInput = document.createElement('input');
    hostInput.type = 'text';
    hostInput.spellcheck = false;
    hostInput.maxLength = 255;
    hostInput.placeholder = '1.1.1.1 或 example.com';
    hostInput.value = t.host || '';

    var portInput = document.createElement('input');
    portInput.type = 'number';
    portInput.min = '1';
    portInput.max = '65535';
    portInput.value = t.port ? String(t.port) : '';

    var enabledBox = document.createElement('input');
    enabledBox.type = 'checkbox';
    enabledBox.checked = t.enabled === undefined ? true : !!t.enabled;
    var enabledLabel = document.createElement('label');
    enabledLabel.className = 'check';
    var enabledText = document.createElement('span');
    enabledText.textContent = '启用';
    enabledLabel.appendChild(enabledBox);
    enabledLabel.appendChild(enabledText);

    var removeBtn = document.createElement('button');
    removeBtn.type = 'button';
    removeBtn.className = 'btn danger';
    removeBtn.textContent = '删除';
    removeBtn.addEventListener('click', function () {
      pingRows = pingRows.filter(function (r) { return r !== row; });
      wrap.remove();
      syncPingEditor();
    });

    // ICMP 没有端口：输入框禁用并置灰。值留在框里 —— 用户切回 TCP 时
    // 不用重新输一遍，而提交时按类型决定发不发端口（见 pingPayload）。
    function syncPortState() {
      var tcp = typeSelect.value === 'tcp';
      portInput.disabled = !tcp;
      portInput.placeholder = tcp ? '443' : '—';
    }
    typeSelect.addEventListener('change', syncPortState);
    syncPortState();

    wrap.appendChild(pingCell('名称', labelInput));
    wrap.appendChild(pingCell('类型', typeSelect));
    wrap.appendChild(pingCell('地址', hostInput));
    wrap.appendChild(pingCell('端口', portInput));
    wrap.appendChild(enabledLabel);
    wrap.appendChild(removeBtn);

    row.wrap = wrap;
    row.refs = {
      label: labelInput, type: typeSelect, host: hostInput,
      port: portInput, enabled: enabledBox
    };
    return row;
  }

  // addPingRow 往编辑器尾部加一行（新目标 id 传 0，服务端会分配）。
  function addPingRow(target) {
    if (pingRows.length >= pingMaxTargets) return null;
    var row = newPingRow(target);
    pingRows.push(row);
    el.pingList.appendChild(row.wrap);
    syncPingEditor();
    return row;
  }

  // syncPingEditor 更新"最多 N 个"的提示与添加按钮的可用状态。
  function syncPingEditor() {
    var full = pingRows.length >= pingMaxTargets;
    el.pingAdd.disabled = full;
    el.pingLimit.textContent = full
      ? '已达上限（最多 ' + pingMaxTargets + ' 个目标，要再加请先删掉一个）'
      : '最多 ' + pingMaxTargets + ' 个目标，当前 ' + pingRows.length + ' 个';
  }

  function updatePingHint() {
    var sec = parseInt(el.pingInterval.value, 10) || 60;
    el.pingHint.textContent = 'Agent 每隔 ' + sec + ' 秒对每个目标探测一次，' +
      '结果显示在节点详情页的延迟图里（一个目标一条曲线）。';
  }

  // renderPingEditor 按服务端返回的配置整块重建编辑器。
  //
  // 为什么要整块重建：保存后新增的目标才拿到服务端分配的 id，不重建的话下一次
  // 保存会把它们当成新目标再发一次，曲线身份变了 —— 历史就断在这里。
  function renderPingEditor(cfg) {
    var ping = cfg || {};
    pingMaxTargets = ping.max_targets > 0 ? ping.max_targets : 16;
    pingRows = [];
    el.pingList.textContent = '';
    // 这里不走 addPingRow 的上限判断：服务端返回多少就渲染多少 ——
    // 万一它给的比 max_targets 还多，静默丢掉几行会在下次保存时把它们删掉。
    (ping.targets || []).forEach(function (t) {
      var row = newPingRow(t);
      pingRows.push(row);
      el.pingList.appendChild(row.wrap);
    });
    el.pingInterval.value = ping.interval_sec || 60;
    updatePingHint();
    syncPingEditor();
  }

  // pingPayload 把编辑器读成请求体。编辑已有目标必须回传它的 id（id 是曲线身份），
  // 新增的传 0 由服务端分配。
  function pingPayload() {
    return {
      targets: pingRows.map(function (row) {
        var tcp = row.refs.type.value === 'tcp';
        return {
          id: row.id,
          label: row.refs.label.value.trim(),
          type: tcp ? 'tcp' : 'icmp',
          host: row.refs.host.value.trim(),
          port: tcp ? (parseInt(row.refs.port.value, 10) || 0) : 0,
          enabled: row.refs.enabled.checked
        };
      }),
      interval_sec: parseInt(el.pingInterval.value, 10) || 0
    };
  }

  // validatePing 在提交前挡一道：服务端也会拒同样的规则，但那要等一个来回，
  // 而且这里能指出是第几行填错了。返回空串表示没问题。
  function validatePing(payload) {
    if (payload.targets.length > pingMaxTargets) {
      return '最多只能配置 ' + pingMaxTargets + ' 个探测目标';
    }
    for (var i = 0; i < payload.targets.length; i++) {
      var t = payload.targets[i];
      var at = '第 ' + (i + 1) + ' 个目标：';
      if (!t.host) return at + '地址不能为空';
      if (/[\s/]/.test(t.host)) return at + '地址不能包含空格或斜杠';
      if (t.type === 'tcp' && !(t.port >= 1 && t.port <= 65535)) {
        return at + 'TCP 端口必须是 1-65535 之间的整数';
      }
    }
    if (!(payload.interval_sec >= 10 && payload.interval_sec <= 3600)) {
      return '探测间隔必须是 10-3600 之间的整数秒';
    }
    return '';
  }

  function savePing() {
    el.pingError.textContent = '';
    el.pingOk.textContent = '';
    var payload = pingPayload();
    var problem = validatePing(payload);
    if (problem) {
      el.pingError.textContent = problem;   // 就地提示，不白跑一个来回
      return;
    }
    el.pingSave.disabled = true;

    api('/api/v1/settings/ping', { method: 'PUT', body: payload }).then(function (data) {
      // 用服务端返回的列表重建编辑器：这时才知道新增目标的 id 与归一化后的间隔。
      renderPingEditor(data);
      el.pingOk.textContent = '已保存';
      toast('延迟探测设置已保存');
    }).catch(function (err) {
      el.pingError.textContent = err.message;
    }).then(function () {
      el.pingSave.disabled = false;
    });
  }

  function changePassword() {
    el.securityError.textContent = '';
    el.securityOk.textContent = '';
    var current = el.pwCurrent.value;
    var next = el.pwNew.value;
    var again = el.pwNew2.value;
    if (!current || !next) {
      el.securityError.textContent = '请填写当前密码与新密码';
      return;
    }
    if (next !== again) {
      el.securityError.textContent = '两次输入的新密码不一致';
      return;
    }
    el.pwSubmit.disabled = true;
    api('/api/v1/auth/password', {
      method: 'POST',
      body: { current_password: current, new_password: next, new_password2: again }
    }).then(function (data) {
      el.pwCurrent.value = '';
      el.pwNew.value = '';
      el.pwNew2.value = '';
      el.securityOk.textContent = '密码已修改（其它设备已退出登录：' + (data.revoked_sessions || 0) + ' 个会话）';
      toast('密码已修改');
    }).catch(function (err) {
      el.securityError.textContent = err.message;
    }).then(function () {
      el.pwSubmit.disabled = false;
    });
  }

  function testTelegram() {
    el.notifyError.textContent = '';
    el.notifyOk.textContent = '';
    el.settingsTest.disabled = true;
    api('/api/v1/settings/telegram/test', { method: 'POST' }).then(function () {
      el.notifyOk.textContent = '测试消息已发出，请查看 Telegram。';
    }).catch(function (err) {
      el.notifyError.textContent = err.message;
    }).then(function () {
      el.settingsTest.disabled = false;
    });
  }

  // ---------------------------------------------------------------- 路由

  function route() {
    var hash = window.location.hash || '#/';
    var nodeMatch = /^#\/n\/(\d+)$/.exec(hash);
    if (!session.authenticated) {
      // 未登录时任何路由都回到登录/初始化视图。
      if (session.needs_setup) setView('setup');
      else setView('login');
      return;
    }
    if (nodeMatch) {
      openDetail(parseInt(nodeMatch[1], 10));
      return;
    }
    // 操作记录已经并进设置页。老地址（#/audit，别人可能收藏了）换成新地址再渲染，
    // 用 replace 是为了不在后退历史里插一条 —— 否则按后退会在两个地址之间弹。
    if (hash === '#/audit') {
      window.location.replace('#/settings/audit');
      openSettings('audit');
      return;
    }
    var pane = settingsPaneFromHash(hash);
    if (pane !== null) {
      closeDetail();
      openSettings(pane);
      return;
    }
    closeDetail();
    setView('home');
    if (!source) startHome();
  }

  // ---------------------------------------------------------------- 会话

  function refreshSession() {
    return api('/api/v1/session').then(function (data) {
      session = data;
      if (data.needs_setup) {
        setView('setup');
        return false;
      }
      if (!data.authenticated) {
        setView('login');
        return false;
      }
      // 先取图表可见性再进路由：直接进详情页（#/n/1）时，晚一步就会先按
      // "全部显示"把六张图的数据都请求一遍，用户还会看到图表闪一下。
      return loadChartVisibility().then(function () {
        route();
        return true;
      });
    });
  }

  // enterApp 在登录/初始化成功后调用：把地址栏归一化，然后按路由渲染。
  function enterApp() {
    if (!window.location.hash || window.location.hash === '#') {
      window.location.hash = '#/';
    }
    // 登录后才拿得到设置，所以这里补一次可见性（拿到之前按全部显示）。
    loadChartVisibility().then(route);
  }

  function startHome() {
    return loadNodes().then(function () {
      connectStream();
    }).catch(function (err) {
      toast(err.message);
    });
  }

  // ---------------------------------------------------------------- 主题

  function applyTheme(theme) {
    if (theme) {
      document.documentElement.dataset.theme = theme;
    } else {
      delete document.documentElement.dataset.theme;
    }
    try {
      if (theme) localStorage.setItem('probe-theme', theme);
      else localStorage.removeItem('probe-theme');
    } catch (err) { /* 隐私模式下 localStorage 可能不可用 */ }
  }

  // ---------------------------------------------------------------- 事件绑定

  function bind() {
    el.btnTheme.addEventListener('click', function () {
      var current = document.documentElement.dataset.theme;
      var isDark = current ? current === 'dark'
        : window.matchMedia('(prefers-color-scheme: dark)').matches;
      applyTheme(isDark ? 'light' : 'dark');
      // 图表颜色来自 CSS 变量之外的固定色，切主题后重画一次即可。
      detail.charts.forEach(function (chart) { chart.redraw(); });
    });

    el.detailBack.addEventListener('click', function () {
      window.location.hash = '#/';
    });
    window.addEventListener('hashchange', route);

    // 页面切到后台就断开实时流：50 个节点全变时每秒约 40 KB，
    // 让手机后台标签页一直收这些数据是纯粹的浪费（回到前台会重新拉一次全量快照）。
    document.addEventListener('visibilitychange', function () {
      if (!session.authenticated) return;
      if (document.hidden) {
        stopStream();
      } else if (!source && !el.viewHome.hidden) {
        connectStream();
      }
    });

    // 顶栏的「设置」只改 hash，剩下的交给 route()：点按钮、点左栏导航、手改地址、
    // 按前进/后退因此走的是同一条路径，不会出现"高亮了但内容没换"。
    el.btnSettings.addEventListener('click', function () {
      window.location.hash = '#/settings';
    });
    el.settingsBack.addEventListener('click', function () {
      window.location.hash = '#/';
    });
    Array.prototype.forEach.call(el.settingsNav.querySelectorAll('.nav-item'), function (btn) {
      btn.addEventListener('click', function () {
        window.location.hash = '#/settings/' + btn.dataset.pane;
      });
    });
    el.notifySave.addEventListener('click', saveNotify);
    el.alertSave.addEventListener('click', saveAlert);
    el.dashboardSave.addEventListener('click', saveDashboard);
    el.pingSave.addEventListener('click', savePing);
    el.pingAdd.addEventListener('click', function () {
      // 新行只给类型与端口留空：地址必须用户自己填，端口也宁可让他显式写一个
      // —— 预填 443 会让人以为"不填端口也能用"。
      addPingRow({ type: 'tcp', enabled: true });
    });
    // 间隔改了就把 hint 里的秒数一起改：否则提示里写 60、实际生效 300。
    el.pingInterval.addEventListener('input', updatePingHint);
    el.settingsTest.addEventListener('click', testTelegram);
    el.pwSubmit.addEventListener('click', changePassword);

    el.auditMore.addEventListener('click', loadAudit);

    el.btnAdd.addEventListener('click', function () {
      openNodeDialog('create', null);
    });
    el.formNode.addEventListener('submit', submitNodeForm);
    el.nodeCancel.addEventListener('click', function () {
      el.dlgNode.close();
    });
    el.detailEdit.addEventListener('click', function () {
      if (detail.node) openNodeDialog('edit', detail.node);
    });
    el.detailToken.addEventListener('click', function () {
      if (detail.id) requestRotateToken(detail.id);
    });
    el.detailDelete.addEventListener('click', function () {
      if (detail.id) requestDeleteNode(detail.id);
    });
    el.confirmCancel.addEventListener('click', function () {
      el.dlgConfirm.close();
      confirmAction = null;
    });
    el.confirmOk.addEventListener('click', function () {
      var action = confirmAction;
      confirmAction = null;
      el.dlgConfirm.close();
      if (action) action();
    });

    el.formLogin.addEventListener('submit', function (event) {
      event.preventDefault();
      el.loginError.textContent = '';
      el.loginSubmit.disabled = true;
      api('/api/v1/auth/login', {
        method: 'POST',
        body: { username: $('login-user').value, password: $('login-pass').value }
      }).then(function (data) {
        session = { authenticated: true, username: data.username, csrf_token: data.csrf_token, needs_setup: false };
        $('login-pass').value = '';
        enterApp();
        return null;
      }).catch(function (err) {
        el.loginError.textContent = err.message;
      }).then(function () {
        el.loginSubmit.disabled = false;
      });
    });

    el.formSetup.addEventListener('submit', function (event) {
      event.preventDefault();
      el.setupError.textContent = '';
      var pass = $('setup-pass').value;
      if (pass !== $('setup-pass2').value) {
        el.setupError.textContent = '两次输入的密码不一致';
        return;
      }
      el.setupSubmit.disabled = true;
      api('/api/v1/setup', {
        method: 'POST',
        body: {
          code: $('setup-code').value.trim(),
          username: $('setup-user').value.trim(),
          password: pass
        }
      }).then(function (data) {
        session = { authenticated: true, username: data.username, csrf_token: data.csrf_token, needs_setup: false };
        $('setup-pass').value = '';
        $('setup-pass2').value = '';
        $('setup-code').value = '';
        toast('管理员已创建');
        enterApp();
        return null;
      }).catch(function (err) {
        el.setupError.textContent = err.message;
      }).then(function () {
        el.setupSubmit.disabled = false;
      });
    });

    el.btnLogout.addEventListener('click', function () {
      api('/api/v1/auth/logout', { method: 'POST' }).catch(function () { /* 忽略 */ }).then(function () {
        resetHome();
        setView('login');
      });
    });

    el.tokenCopy.addEventListener('click', function () {
      var text = el.tokenValue.textContent;
      if (navigator.clipboard && navigator.clipboard.writeText) {
        navigator.clipboard.writeText(text).then(function () { toast('已复制 Token'); },
          function () { toast('复制失败，请手动选择'); });
      } else {
        toast('浏览器不支持自动复制，请手动选择');
      }
    });
    el.tokenClose.addEventListener('click', function () { el.dlgToken.close(); });
  }

  // showToken 显示一次性 Token，并给出**从零开始的完整安装命令**。
  //
  // 以前这里打印的是 `probe-agent --server … --token-file …`，那是"装好之后
  // 服务怎么启动"的命令（--token-file 指向的 /etc/probe-agent/token 由安装脚本
  // 写入）。第一次用的人照着敲只会得到 `probe-agent: command not found`，
  // 而 Token 只显示这一次，关掉对话框就得重新生成 —— 所以必须给安装命令。
  function showToken(token, node) {
    el.tokenValue.textContent = token;
    var cmd = 'curl -fsSL https://raw.githubusercontent.com/' + INSTALL_REPO + '/' + INSTALL_REF +
      '/deploy/install-remote.sh \\\n' +
      '  | sh -s -- agent \\\n' +
      '  --server ' + window.location.origin + ' \\\n' +
      '  --token ' + token;
    el.tokenCmd.textContent = cmd;
    el.dlgToken.showModal();
    void node;
  }

  // ---------------------------------------------------------------- 启动

  // camelID 把 HTML 里的短横线 id 转成代码里惯用的驼峰写法。
  function camelID(id) {
    return id.replace(/-([a-z0-9])/g, function (m, ch) { return ch.toUpperCase(); });
  }

  function main() {
    // 把所有带 id 的元素登记进 el，"原样"与"驼峰"两种键都登记。
    //
    // 为什么不能手写名单：HTML 的 id 是短横线式（view-setup、btn-theme），代码里
    // 习惯写驼峰（el.viewSetup、el.btnTheme）。手写名单一旦和 HTML 脱节，
    // getElementById 会返回 null，bind() 第一行就抛 TypeError，refreshSession()
    // 永远执行不到 —— 所有视图一直 hidden，页面全白，而且除了控制台毫无线索。
    // 从 DOM 直接推导就不可能再对不上。
    Array.prototype.forEach.call(document.querySelectorAll('[id]'), function (node) {
      el[node.id] = node;
      el[camelID(node.id)] = node;
    });

    try {
      var saved = localStorage.getItem('probe-theme');
      if (saved) applyTheme(saved);
    } catch (err) { /* 忽略 */ }

    bind();

    refreshSession().then(function (loggedIn) {
      if (loggedIn) return null;
      // 未登录时定期复查：管理员在别处初始化完成后，这个页面能自动切到登录。
      window.setInterval(function () {
        if (!session.authenticated) refreshSession();
      }, POLL_SESSION_MS);
      return null;
    }).catch(function (err) {
      el.loginError.textContent = '无法连接服务端：' + err.message;
      setView('login');
    });
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', main);
  } else {
    main();
  }
})();
