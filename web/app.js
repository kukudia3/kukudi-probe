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
    timer: null
  };
  var DETAIL_REFRESH_MS = 30000;
  // 手机端（窄屏）用服务端给的二次聚合目标，PC 不聚合。
  var MOBILE_QUERY = '(max-width: 640px)';

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
    el.viewAudit.hidden = name !== 'audit';
    el.btnAdd.hidden = name !== 'home';
    el.btnSettings.hidden = name === 'setup' || name === 'login';
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
    [['net', '网络'], ['traffic', '本月'], ['lat', '延迟'], ['up', 'Uptime'], ['seen', '最后通信']].forEach(function (item) {
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

  function renderDetailInfo() {
    var node = detail.node;
    if (!node) return;
    var dl = el.detailInfo;
    dl.textContent = '';

    el.detailName.textContent = node.name;
    el.detailDot.className = 'dot ' + node.status;
    el.detailStatus.textContent = STATUS_TEXT[node.status] || node.status;

    infoRow(dl, '状态', statusSpan(node.status));
    infoRow(dl, '最后通信', node.last_seen ? fmtAgo(node.last_seen) : '从未');
    infoRow(dl, '实时网络', '↑ ' + fmtRate(node.tx_rate) + '  ↓ ' + fmtRate(node.rx_rate));
    infoRow(dl, '累计流量', '↑ ' + fmtBytes(node.tx_total) + '  ↓ ' + fmtBytes(node.rx_total));
    infoRow(dl, '延迟', node.lat_ms > 0 ? node.lat_ms.toFixed(1) + ' ms' : '—');
    infoRow(dl, 'Uptime', fmtUptime(node.uptime_sec));
    var u24 = detail.uptime['1d'];
    var u7 = detail.uptime['7d'];
    infoRow(dl, '可用率 24h', u24 && u24.has_data ? u24.pct.toFixed(2) + '%' : '—');
    infoRow(dl, '可用率 7d', u7 && u7.has_data ? u7.pct.toFixed(2) + '%' : '—');
    infoRow(dl, '今日流量', '↓ ' + fmtBytes(node.traffic_today_rx) + '  ↑ ' + fmtBytes(node.traffic_today_tx));
    infoRow(dl, '本周期流量', trafficCycleText(node));
    infoRow(dl, '累计流量', '↓ ' + fmtBytes(node.traffic_total_rx) + '  ↑ ' + fmtBytes(node.traffic_total_tx));
    if (node.cycle_start) {
      infoRow(dl, '计费周期', node.cycle_start + ' → ' + node.cycle_end + '（每月 ' + node.reset_day + ' 日重置）');
    }
    infoRow(dl, 'CPU', fmtPct(node.cpu_pct) + (node.cpu_cores ? '（' + node.cpu_cores + ' 核）' : ''));
    infoRow(dl, '内存', fmtPct(node.mem_pct) + '（swap ' + fmtPct(node.swap_pct) + '）');
    infoRow(dl, '磁盘', diskSummary(node));
    infoRow(dl, '负载 1 分钟', node.load1 ? node.load1.toFixed(2) : '—');
    infoRow(dl, '系统', node.os_name || '—');
    infoRow(dl, '内核', node.kernel || '—');
    infoRow(dl, 'CPU 型号', node.cpu_model || '—');
    infoRow(dl, '监控网卡', node.iface || '—');
    infoRow(dl, '出口地址', node.observed_ip || '—');
    infoRow(dl, 'Agent 版本', node.agent_version || '—');
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
    var metrics = ['cpu', 'mem', 'disk', 'net_down', 'net_up', 'lat'];

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
      var latOpts = { yMax: 0, unit: ' ms', yFormat: function (v) { return v.toFixed(0); }, tickLabelSec: tick, xFormat: xFormat, showMax: true };

      setChart('cpu', 'chart-cpu', byMetric.cpu, [{ label: 'CPU', color: '#2563eb' }], pctOpts);
      setChart('mem', 'chart-mem', byMetric.mem, [{ label: '内存', color: '#7c3aed' }], pctOpts);
      setChart('disk', 'chart-disk', byMetric.disk, [{ label: '磁盘', color: '#0891b2' }], pctOpts);
      setChart('net', 'chart-net', null, null, rateOpts, [
        { label: '下行', color: '#2563eb', data: byMetric.net_down },
        { label: '上行', color: '#16a34a', data: byMetric.net_up }
      ]);
      setChart('lat', 'chart-lat', byMetric.lat, [{ label: '延迟', color: '#d97706' }], latOpts);
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

  function openDetail(id) {
    detail.id = id;
    setView('detail');
    el.detailName.textContent = '加载中…';
    el.detailInfo.textContent = '';
    el.detailRanges.textContent = '';

    api('/api/v1/nodes/' + id).then(function (data) {
      detail.node = data.node;
      detail.uptime = data.uptime || {};
      detail.ranges = data.ranges || [];
      if (!detail.range && detail.ranges.length) detail.range = detail.ranges[0].key;
      renderDetailInfo();
      renderRangeButtons();
      return Promise.all([loadSeries(), loadTrafficChart()]);
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
      traffic_limit: Math.max(0, Math.round((parseFloat(el.nodeTraffic.value) || 0) * 1024 * 1024 * 1024)),
      traffic_warn_pct: Math.min(100, Math.max(1, parseInt(el.nodeWarn.value, 10) || 80)),
      reset_day: Math.min(31, Math.max(1, parseInt(el.nodeReset.value, 10) || 1)),
      expires_at: expiresAt,
      enabled: el.nodeEnabled.checked
    };
  }

  function submitNodeForm(event) {
    event.preventDefault();
    el.nodeError.textContent = '';
    el.nodeSubmit.disabled = true;
    var payload = nodeFormPayload();
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

  function openAudit() {
    el.auditBody.textContent = '';
    el.auditEmpty.hidden = true;
    el.auditMore.hidden = true;
    auditBeforeID = 0;
    setView('audit');
    loadAudit();
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

  function openSettings() {
    el.settingsError.textContent = '';
    el.settingsOk.textContent = '';
    el['tg-token'].value = '';
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

      el.dlgSettings.showModal();
    }).catch(function (err) {
      toast(err.message);
    });
  }

  function saveSettings() {
    el.settingsError.textContent = '';
    el.settingsOk.textContent = '';
    el.settingsSave.disabled = true;

    var telegram = {
      enabled: el.tgEnabled.checked,
      bot_token: el['tg-token'].value.trim(),
      chat_id: el.tgChat.value.trim()
    };
    var alertCfg = {
      cooldown: el.alertCooldown.value.trim(),
      startup_grace: el.alertGrace.value.trim(),
      debounce: el.alertDebounce.value.trim(),
      recover_stable: el.alertRecover.value.trim()
    };

    api('/api/v1/settings/telegram', { method: 'PUT', body: telegram }).then(function (cfg) {
      el['tg-token'].value = '';
      el.tgTokenHint.textContent = cfg.has_token ? '已经保存过 Token；留空表示不修改。' : '还没有保存 Token。';
      return api('/api/v1/settings/alert', { method: 'PUT', body: alertCfg });
    }).then(function (data) {
      var alertCfg2 = data.alert || {};
      el.alertCooldown.value = alertCfg2.cooldown || '';
      el.alertGrace.value = alertCfg2.startup_grace || '';
      el.alertDebounce.value = alertCfg2.debounce || '';
      el.alertRecover.value = alertCfg2.recover_stable || '';
      el.settingsOk.textContent = '已保存';
      toast('设置已保存');
    }).catch(function (err) {
      el.settingsError.textContent = err.message;
    }).then(function () {
      el.settingsSave.disabled = false;
    });
  }

  function changePassword() {
    el.settingsError.textContent = '';
    el.settingsOk.textContent = '';
    var current = el.pwCurrent.value;
    var next = el.pwNew.value;
    var again = el.pwNew2.value;
    if (!current || !next) {
      el.settingsError.textContent = '请填写当前密码与新密码';
      return;
    }
    if (next !== again) {
      el.settingsError.textContent = '两次输入的新密码不一致';
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
      el.settingsOk.textContent = '密码已修改（其它设备已退出登录：' + (data.revoked_sessions || 0) + ' 个会话）';
      toast('密码已修改');
    }).catch(function (err) {
      el.settingsError.textContent = err.message;
    }).then(function () {
      el.pwSubmit.disabled = false;
    });
  }

  function testTelegram() {
    el.settingsError.textContent = '';
    el.settingsOk.textContent = '';
    el.settingsTest.disabled = true;
    api('/api/v1/settings/telegram/test', { method: 'POST' }).then(function () {
      el.settingsOk.textContent = '测试消息已发出，请查看 Telegram。';
    }).catch(function (err) {
      el.settingsError.textContent = err.message;
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
    if (hash === '#/audit') {
      closeDetail();
      openAudit();
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
      route();
      return true;
    });
  }

  // enterApp 在登录/初始化成功后调用：把地址栏归一化，然后按路由渲染。
  function enterApp() {
    if (!window.location.hash || window.location.hash === '#') {
      window.location.hash = '#/';
    }
    route();
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

    el.btnSettings.addEventListener('click', openSettings);
    el.settingsCancel.addEventListener('click', function () {
      el.dlgSettings.close();
    });
    el.settingsSave.addEventListener('click', saveSettings);
    el.settingsTest.addEventListener('click', testTelegram);
    el.pwSubmit.addEventListener('click', changePassword);
    el.settingsAudit.addEventListener('click', function () {
      el.dlgSettings.close();
      window.location.hash = '#/audit';
    });

    el.auditBack.addEventListener('click', function () {
      window.location.hash = '#/';
    });
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
