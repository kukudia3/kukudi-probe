'use strict';

/* 极简 VPS 探针 —— 前端
 *
 * 没有框架、没有构建步骤、没有外部请求。
 * 渲染只用 DOM API 与 textContent（绝不拼接 HTML，防 XSS）。
 * 实时数据走 SSE：服务端每秒只推"变了的节点"，这里按 id 局部更新卡片。
 */

(function () {
  var POLL_SESSION_MS = 30000;

  // 总览（首页顶部那块合计）的刷新周期。
  //
  // 为什么不跟 SSE 走：SSE 是每秒级的实时数据，而总览是**分钟级**的
  // （内存/硬盘的桶是每分钟落盘、累计流量每分钟算一次），
  // 跟着每秒重取等于每秒让服务端做一次全库聚合，换来的只是完全相同的数字。
  var OVERVIEW_POLL_MS = 60000;

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

  var overviewTimer = null;

  // 详情页状态
  var detail = {
    id: 0,
    node: null,
    uptime: {},
    ranges: [],
    // 时间档位：只控制「资源与网络」卡里的五张图（CPU/内存/磁盘/网络/流量）。
    //
    // 这里曾经还有第二份档位状态（pingRange）与第二组按钮，属于已经删掉的
    // 「延迟探测」功能，随功能一起移除。
    range: '1h',
    charts: new Map(),   // key -> chart 实例
    timer: null
  };
  var DETAIL_REFRESH_MS = 30000;
  // 手机端（窄屏）用服务端给的二次聚合目标，PC 不聚合。
  var MOBILE_QUERY = '(max-width: 640px)';

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
    node_order: '调整节点顺序',
    node_token_rotate: '重新生成 Token',
    telegram_settings: '修改通知设置',
    settings_update: '修改告警参数',
    setup: '初始化管理员',
    login: '登录',
    logout: '退出登录'
  };

  // ---------------------------------------------------------------- 工具

  // 字节有**两套单位口径**，按"这个量商家是怎么卖的"分开用，绝不混：
  //
  //   - 内存 → fmtBytesBin（1024 进制，KiB/MiB/GiB/TiB）。内存条物理上就是 2 的幂，
  //     商家说的"1 GB 内存"给的其实是 1024³ 字节；写成 GiB 才是实话。
  //   - 硬盘 / 流量 / 速率 → fmtBytesDec（1000 进制，KB/MB/GB/TB）。商家卖硬盘与
  //     流量就是按 10 的幂（20 GB 的盘 = 20×10⁹ 字节，1 TB 流量 = 10¹² 字节），
  //     与「月流量额度（GB）」输入框同一口径。
  //
  // 混用不是"看着别扭"，是会把额度算错：输入框按 1024³ 存、标签却写 GB 时，用户填
  // 2000 以为买了 2000 GB，实际入库 2147 GB —— 80% 预警要等真实用量到 86% 才响。
  //
  // fmtScaledBytes 是两套口径共用的实现：逐级降幂，小值两位小数、大值一位。
  function fmtScaledBytes(n, base, units) {
    if (!n || n < 0) return '0 B';
    var i = 0;
    var v = n;
    while (v >= base && i < units.length - 1) { v /= base; i++; }
    return (i === 0 ? v.toFixed(0) : v.toFixed(v < 10 ? 2 : 1)) + ' ' + units[i];
  }

  // fmtBytesBin：**只给内存用**（1024 进制，带 i 的 KiB/MiB/GiB/TiB）。
  function fmtBytesBin(n) {
    return fmtScaledBytes(n, 1024, ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB']);
  }

  // fmtBytesDec：内存以外全部用它 —— 硬盘、流量、速率（1000 进制，不带 i）。
  function fmtBytesDec(n) {
    return fmtScaledBytes(n, 1000, ['B', 'KB', 'MB', 'GB', 'TB', 'PB']);
  }

  // 速率是网络惯例的 1000 进制（KB/s、MB/s），与流量额度同一口径。
  function fmtRate(n) {
    if (!n || n < 0) return '0 B/s';
    return fmtBytesDec(n) + '/s';
  }

  function fmtPct(n) {
    if (typeof n !== 'number' || isNaN(n)) return '—';
    return n.toFixed(n < 10 ? 1 : 0) + '%';
  }

  // fmtPct1 是资源格与流量用的百分比写法：**一律一位小数**。
  //
  // 与 fmtPct 不同（它 ≥10% 时取整）。两个理由：
  //   - 四格是两列并排的，"2.0%" 与 "25%" 混在一起时小数点对不齐；
  //   - 25.04% 与 25.96% 都写成 "25%"，看不出它在涨。
  function fmtPct1(n) {
    if (typeof n !== 'number' || isNaN(n)) return '—';
    return n.toFixed(1) + '%';
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

  // ---------------------------------------------------------------- 标签
  //
  // 标签是用户给机器挂的短文字（最多 64 个，每个最长 32 字），用来把"这台是干嘛的"
  // 一眼标出来。它有两个显示位置：首页卡片底部与设置页的「服务器列表」，
  // 两处长得完全一样 —— 都是低调的元数据样式（见 style.css 的 .tag）。
  //
  // 编辑入口只有一个：节点对话框里的「标签」输入框（多个标签用 ; 分隔）。
  // 以前它有一个独立的标签对话框，那样一台机器的配置就有两个入口，
  // 而两处保存的都是同一份数据（整体替换），用户会以为改的是两样东西。
  //
  // 标签**不按文字取色**：既然只有一种样式，就不需要"文字哈希 → 色板"那一套，
  // 也就没有"同一个标签在两个页面上颜色不同"的可能。

  // 上限与服务端的 store.NormalizeTags 保持一致。前端这一道只是"少一个来回"，
  // 真正生效的是服务端那一道（curl 可以绕过这里）。
  //
  // 为什么留上界（而不是真的不限制）：标签跟着节点 DTO 每秒走 SSE，几十个长标签
  // 会把每秒的推送一起撑大、首屏也变慢。64 是个现实中碰不到的数。
  var TAG_MAX_COUNT = 64;
  var TAG_MAX_LEN = 32;

  // TAG_SEP 是标签之间的分隔符：半角 ; 与全角 ；都收。
  //
  // 中文输入法下打出来的是全角 ；，这两个字符在输入框里长得几乎一样 ——
  // 只认半角的话，用户会以为"明明输了却不生效"，而这属于最难自查的一类 bug。
  var TAG_SEP = /[;；]/;

  // tagChip 造一个标签徽章。
  //
  // 样式一律交给 CSS 的 .tag（淡边框 + 弱化色 + 小字号）：首页卡片与设置页的
  // 服务器列表因此必然长得一模一样 —— 同一个标签在两处不同样是只有盯着屏幕
  // 才看得出来的问题。看上去应该像"附注"而不是"按钮"：标签是补充信息，
  // 不是可以点的操作。
  function tagChip(text) {
    var span = document.createElement('span');
    span.className = 'tag';
    span.textContent = text;
    return span;
  }

  // tagRuneLen 数标签的**字符**数（不是 UTF-16 长度）。
  //
  // 服务端按 rune 算长度，而 '👍'.length 在 JS 里是 2 —— 直接用 .length 会出现
  // "前端放行、服务端 400"这种前后不一致。Array.from 按码点切分，与 rune 基本一致。
  function tagRuneLen(text) {
    return Array.from(text).length;
  }

  // splitTags 把输入框里的文本切成标签列表：按 ; / ； 切、去首尾空白、丢空串、去重。
  //
  // 规则与服务端的 store.NormalizeTags 一模一样（顺序保持首次出现）：
  // 前端这一道只是让用户立刻看到结果，服务端那道才是真正生效的。
  // 丢空串是必须的 —— 末尾多打一个分隔符、或者连打两个 ;; 都会切出一个空串。
  function splitTags(text) {
    var out = [];
    var seen = {};
    String(text === undefined || text === null ? '' : text).split(TAG_SEP).forEach(function (piece) {
      var tag = piece.trim();
      if (!tag) return;
      if (seen[tag]) return;
      seen[tag] = true;
      out.push(tag);
    });
    return out;
  }

  // tagsToInputValue 把标签列表回填成输入框里的文本：用「; 」连接。
  //
  // 分号后面跟一个空格是刻意的：与提示里写的一致，读起来也不挤成一片，
  // 而且回填出来的文本与用户手打时的写法一样 —— 保存时按同一个分隔符切分。
  function tagsToInputValue(list) {
    return (list || []).join('; ');
  }

  // tagProblem 校验一份标签列表，返回错误消息（空串表示没问题）。
  //
  // 唯一调用点是节点对话框的提交前校验（见 validateNodeTags）。顺序与服务端一致：
  // 先逐个看长度，再看总数。消息里必须带上出问题的那个标签（一次可能有几十个，
  // 只说"某个标签太长"用户得自己一个个数过去）。
  function tagProblem(list) {
    for (var i = 0; i < list.length; i++) {
      var n = tagRuneLen(list[i]);
      if (n > TAG_MAX_LEN) {
        return '标签「' + list[i] + '」有 ' + n + ' 个字符，超过 ' + TAG_MAX_LEN + ' 字上限';
      }
    }
    if (list.length > TAG_MAX_COUNT) {
      return '标签数量 ' + list.length + ' 个，超过上限 ' + TAG_MAX_COUNT + ' 个';
    }
    return '';
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
    // 总览的定时器跟着首页视图走。放在这里而不是各个路由分支里：
    // setView 是切视图的唯一出口，路由有几条分支、以后还会不会加分支，
    // 都不会漏掉"离开首页要停表"这件事（见 syncOverviewTimer）。
    syncOverviewTimer();
  }

  // ---------------------------------------------------------------- 首页渲染
  //
  // 一张卡片自上而下：
  //   头部（名称 / 分组·地区 + 状态点）
  //   四格资源（CPU / 内存 / 硬盘 / 流量，2×2）
  //   点线引导行（速率 / 在线 / 最后通信 / 费用）
  //   标签行
  //
  // 这里曾经还有「探测」一行与「延迟 / 丢包」两块迷你条（都来自已删除的
  // 「延迟探测」功能），随功能一起移除。
  //
  // 「面板延迟」（Agent 到面板自身的 WebSocket 往返）**不在卡片上**：它测的是
  // 隧道往返（走 Cloudflare 恒为 ~100ms），混在卡片上必然被当成别的数。
  // 它仍然在详情页的「网络信息」卡里（见 renderDetailInfo），那里有足够的上下文
  // 写清楚它是什么。

  // 四格资源：键（updateCard 按它取引用）→ 标题。数组顺序就是格子顺序，
  // 2×2 网格按行填充 —— 前两个一行（CPU / 内存），后两个一行（硬盘 / 流量）。
  var CARD_RES = [['cpu', 'CPU'], ['mem', '内存'], ['disk', '硬盘'], ['quota', '流量']];

  // 点线引导行：键 → 标题。顺序就是卡片上的顺序。
  // 「费用」在没填价格时整行不显示（见 updateCard），其余三行一直在。
  var CARD_LINES = [
    ['net', '速率'], ['online', '在线'], ['seen', '最后通信'], ['cost', '费用']
  ];

  // resCell 造一格资源：标题行（标签 + 百分比）、进度条、小字副值。
  // 三部分在竖直方向对齐，四格因此在两列里各自对齐成一条线（见 style.css 的 .card-res）。
  function resCell(label) {
    var root = document.createElement('div');
    root.className = 'res-cell';

    var head = document.createElement('div');
    head.className = 'res-head';
    var name = document.createElement('span');
    name.className = 'res-label';
    name.textContent = label;
    var pct = document.createElement('b');
    pct.className = 'res-pct';
    pct.textContent = '—';
    head.appendChild(name);
    head.appendChild(pct);

    var track = document.createElement('div');
    track.className = 'bar';
    var fill = document.createElement('i');
    track.appendChild(fill);

    var sub = document.createElement('div');
    sub.className = 'res-sub';
    sub.textContent = '—';

    root.appendChild(head);
    root.appendChild(track);
    root.appendChild(sub);
    return { root: root, pct: pct, fill: fill, sub: sub };
  }

  // lineRow 造一条点线引导行：左标签、中间虚线、右值。
  //
  // 中间那条虚线交给 CSS（.line-lead 的下边框），不在这里拼一串 · 字符：字符画的
  // 线会随字体/字号/缩放变样，而且复制卡片文本时会带上一长串句点。
  function lineRow(label) {
    var root = document.createElement('div');
    root.className = 'line';
    var name = document.createElement('span');
    name.className = 'line-label';
    name.textContent = label;
    var lead = document.createElement('span');
    lead.className = 'line-lead';
    var value = document.createElement('span');
    value.className = 'line-value';
    root.appendChild(name);
    root.appendChild(lead);
    root.appendChild(value);
    return { root: root, value: value };
  }

  // setRes 画一格资源：百分比 + 进度条 + 副值。
  //
  // pct 传 null 表示"这个口径没有"（没填流量额度、节点还没上报硬盘）：百分比写 —、
  // 进度条留空 —— 写 0% 会被读成"实测就是 0"，那是另一回事。
  // cls 是进度条的颜色类；不传就按通用阈值（barClass），流量格自己传一套
  // （它按节点配置的告警阈值变色，与 CPU/内存/硬盘不同）。
  function setRes(cell, pct, sub, cls) {
    var has = typeof pct === 'number' && isFinite(pct);
    var shown = has ? Math.max(0, Math.min(100, pct)) : 0;
    cell.pct.textContent = has ? fmtPct1(pct) : '—';
    cell.fill.style.width = shown + '%';
    cell.fill.className = cls === undefined ? barClass(shown) : cls;
    cell.sub.textContent = sub;
  }

  // fmtLoad 写负载：两位小数。取整会把 0.04 与 0.004 显示成同一个数，
  // 而"负载到底降下来没有"正是这三个数要回答的。
  function fmtLoad(v) {
    return (typeof v === 'number' && isFinite(v) ? v : 0).toFixed(2);
  }

  // pairTextBin / pairTextDec 拼「已用 / 总量」。总量为 0（还没上报）时给一个 —：
  // 写 "0 B / 0 B" 会被读成"这台机器是空的"。
  //
  // 为什么是两个而不是一个：内存走 1024 进制、硬盘走 1000 进制（见 fmtBytesBin），
  // 合用一个就没法各写各的 —— 结果必然是其中一格用错单位。
  function pairTextBin(used, total) {
    if (!(total > 0)) return '—';
    return fmtBytesBin(used) + ' / ' + fmtBytesBin(total);
  }

  // pairTextDec：硬盘用（1000 进制）。
  function pairTextDec(used, total) {
    if (!(total > 0)) return '—';
    return fmtBytesDec(used) + ' / ' + fmtBytesDec(total);
  }

  // rootDiskOf 挑出**根挂载点**。
  //
  // 与后端 overview.go 的 rootDisk 是同一条口径（集群合计也只算 /）：一个节点可能
  // 上报多个挂载点，其中很多其实是同一个文件系统（同一分区的多个挂载点、bind mount、
  // overlay 的上层），全加起来会把同一块盘算好几遍 —— 数字变大但并不荒谬，
  // 只有拿 df 对一遍才会发现。挂载点数组由服务端整体给出（详情页要用全部），
  // 挑哪一个由前端决定，服务端不替前端挑。
  function rootDiskOf(disks) {
    var list = disks || [];
    if (list.length === 0) return null;
    for (var i = 0; i < list.length; i++) {
      if (list[i].mount === '/') return list[i];
    }
    // 找不到 / 就退回第一项：protocol.Disk 约定第一项是 --disk 指定的主文件系统
    // （容器或老 Agent 上报的可能是别的路径）。
    return list[0];
  }

  // onlineText 写「连续在线时长」：不在线时是 —。
  //
  // 服务端只在这个节点处于 online 时才给非零值（见 internal/server/online.go），
  // 所以 0 一律是"不在线"：写 "0 秒" 会被读成"刚上线"，与事实正好相反。
  //
  // 它与「开机时长」（uptime_sec，详情页里那一行）是两个概念：机器重启会让开机时长
  // 归零而节点一秒没掉线；反过来机器一年没重启、中途断网三天，开机时长照样是一年。
  function onlineText(sec) {
    return sec > 0 ? fmtUptime(sec) : '—';
  }

  // quotaBarClass 决定流量进度条的颜色：按**节点自己配的**告警阈值
  // （traffic_warn_pct）变色，而不是 CPU/内存/硬盘那套固定阈值 ——
  // 流量讲的是"额度用掉多少"，用户在创建节点时就为它定过一个百分比。
  function quotaBarClass(dto) {
    var pct = Math.max(0, Math.min(999, dto.traffic_pct || 0));
    if (pct >= 100) return 'bad';
    if (dto.traffic_warn_pct > 0 && pct >= dto.traffic_warn_pct) return 'warn';
    return '';
  }

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

    // 四格资源（2×2）。
    var res = document.createElement('div');
    res.className = 'card-res';
    var resRefs = {};
    CARD_RES.forEach(function (item) {
      var cell = resCell(item[1]);
      res.appendChild(cell.root);
      resRefs[item[0]] = cell;
    });

    // 点线引导行。
    var lines = document.createElement('div');
    lines.className = 'card-lines';
    var lineRefs = {};
    CARD_LINES.forEach(function (item) {
      var row = lineRow(item[1]);
      lines.appendChild(row.root);
      lineRefs[item[0]] = row;
    });

    // 标签行：默认隐藏，等 updateCard 按节点数据决定显隐（没有标签的卡片
    // 不该留一条空白）。
    var tags = document.createElement('div');
    tags.className = 'card-tags';
    tags.hidden = true;

    root.appendChild(head);
    root.appendChild(res);
    root.appendChild(lines);
    // 标签行排在**最后**：上面几行是"这台机器的实时读数"（且每秒都在变），
    // 标签是"这台机器是什么"，属于补充信息，放最后不打断读数的节奏。
    root.appendChild(tags);

    var card = {
      root: root,
      // 上一次画出来的标签（拼成一个字符串比对）。卡片每秒都会被 SSE 重画一次，
      // 而标签是分钟级才变一次的东西 —— 不比对的话，每秒都要把徽章拆了重建。
      tagKey: null,
      refs: {
        name: name, sub: sub, dot: dot, status: status,
        cpu: resRefs.cpu, mem: resRefs.mem, disk: resRefs.disk, quota: resRefs.quota,
        net: lineRefs.net.value, online: lineRefs.online.value, seen: lineRefs.seen.value,
        cost: lineRefs.cost,
        tags: tags
      }
    };
    return card;
  }

  // renderCardTags 画卡片底部的标签行：没有标签就整行不显示（不留一条空行）。
  //
  // 标签多了由 CSS 折行（.card-tags 是 flex-wrap），卡片高度自己长，
  // 不会撑破卡片、也不会出现横向滚动。
  function renderCardTags(card, dto) {
    var list = dto.tags || [];
    var key = list.join('\u0000');
    if (key === card.tagKey) return;   // 没变就不动 DOM（每秒都会走到这里）
    card.tagKey = key;
    card.refs.tags.textContent = '';
    card.refs.tags.hidden = list.length === 0;
    list.forEach(function (tag) {
      card.refs.tags.appendChild(tagChip(tag));
    });
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

    // 四格资源。副值里的"已用/总量"是服务端给的原始字节，这里只做单位换算与拼串
    // —— 百分比与聚合一律由服务端算好（前端不做算术是本项目的原则）。
    //
    // 从未上报过的节点（last_seen 为 0）**不能显示 0.0%**：那是"没有数据"，不是
    // "这台机器很闲"。0.0% 配上一排 0.00 的负载会被读成"空载的正常机器"，而真相是
    // Agent 一次都没连上过 —— 这正是本项目一直在修的那类问题（"没有数据就留浅灰、
    // 绝不画成 0"是同一条原则）。离线但报过数据的节点照旧显示最后一次读数：
    // 状态点已经写明"离线"。
    var neverReported = !dto.last_seen;
    if (neverReported) {
      setRes(r.cpu, null, '—');
      setRes(r.mem, null, '—');
      setRes(r.disk, null, '—');
      setRes(r.quota, null, '—');
    } else {
      // CPU 的副值是三个负载（1/5/15 分钟）：只看 1 分钟分不清"刚刚抖了一下"
      // 与"已经压了半小时"。
      setRes(r.cpu, dto.cpu_pct, [fmtLoad(dto.load1), fmtLoad(dto.load5), fmtLoad(dto.load15)].join(', '));
      // 内存：已用 / 总量。**1024 进制**（MiB/GiB），见 fmtBytesBin。
      setRes(r.mem, dto.mem_pct, pairTextBin(dto.mem_used, dto.mem_total));
      // 硬盘：只取根挂载点（见 rootDiskOf）。**1000 进制**（MB/GB）。
      var disk = rootDiskOf(dto.disks);
      setRes(r.disk, disk ? disk.pct : null, disk ? pairTextDec(disk.used, disk.total) : '—');
      // 流量：本周期已用 / 月额度。本周期用量 = 收 + 发（与详情页「本周期流量（共 …）」
      // 同一个算法，早就在这里了）。
      var cycleUsed = (dto.traffic_cycle_rx || 0) + (dto.traffic_cycle_tx || 0);
      if (dto.traffic_limit > 0) {
        // 设了额度：百分比由服务端的 traffic_pct 给（前端不拿两个字节数去除）。
        setRes(r.quota, dto.traffic_pct, fmtBytesDec(cycleUsed) + ' / ' + fmtBytesDec(dto.traffic_limit),
          quotaBarClass(dto));
      } else {
        // 没填额度：**只写已用量**。写 "/ 0" 会被读成"额度用光了"，而事实是没填额度；
        // 百分比那一格写 —（没有分母），进度条留空。
        setRes(r.quota, null, fmtBytesDec(cycleUsed));
      }
    }

    // 速率：⬆ 是上行（tx_rate）、⬇ 是下行（rx_rate）—— 与总览区、详情页同一套口径。
    // 从未上报过的节点同理写 —：0 B/s 会被读成"链路闲着"。
    r.net.textContent = neverReported
      ? '—'
      : '⬆ ' + fmtRate(dto.tx_rate) + '  ⬇ ' + fmtRate(dto.rx_rate);
    // 在线：连续在线时长，不是开机时长（那两个数经常差很多，见 onlineText）。
    r.online.textContent = onlineText(dto.online_sec);
    r.seen.textContent = dto.last_seen ? fmtAgo(dto.last_seen) : '从未';

    // 费用：没填价格时**整行不显示**（留一行 "0.00" 会被读成"这台机器免费"）。
    // 拼接方式与详情页顶部那一格一致（[金额, 周期].join(' ')）：
    // 同一个概念在两处必须长得一样，否则会以为是两个不同的值。
    // 剩余天数由服务端算好（remaining_days，与详情页那一格同源）。
    var hasPrice = dto.price_cents > 0;
    r.cost.root.hidden = !hasPrice;
    if (hasPrice) {
      r.cost.value.textContent = [fmtMoney(dto.price_cents, dto.currency), billingText(dto.billing_months)]
        .join(' ').trim() +
        (dto.expires_at > 0 ? ' · +' + dto.remaining_days + ' 天' : '');
    }

    card.root.title = dto.name + (dto.observed_ip ? ' · ' + dto.observed_ip : '');

    // 标签同理：卡片可能是刚建出来的（新节点上线），这一帧要能把标签补上
    // （renderCardTags 内部按"变没变"跳过重建，所以每秒调用不会重建 DOM）。
    renderCardTags(card, dto);
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

  // ---------------------------------------------------------------- 首页总览
  //
  // 总览区讲的是"所有机器加起来"：内存、硬盘、实时上下行、累计流量、剩余价值、在线数。
  // 每个数字都由 /api/v1/overview 算好（口径见 internal/server/overview.go）：
  // 前端不做算术是项目原则，何况"哪些节点算在线""硬盘算哪个挂载点""币种怎么分组"
  // 这些口径一旦在两端各写一遍，PC 与手机、首页与详情页迟早会对不上。

  // 总览区的格子：键（对应 renderOverview 里读的字段）→ 标题。数组顺序就是页面顺序。
  //
  // 这里**没有**"在线情况"：页面顶部那条汇总栏已经写着「在线 2 / 3 抖动 1 …」，
  // 再摆一格只会重复；而且七格在宽屏下会折成"六格 + 一格"，第二行右边空出五格，
  // 看着像没渲染完。六格正好一行排满。
  var OVERVIEW_ITEMS = [
    ['mem', '总内存用量'],
    ['disk', '硬盘用量'],
    ['up', '实时上行'],
    ['down', '实时下行'],
    ['traffic', '累计流量'],
    ['value', '剩余价值']
  ];
  var overviewRefs = null;   // 键 -> 那一格的值容器（null = 还没建）

  // buildOverview 按 OVERVIEW_ITEMS 把格子建出来。只建一次，之后只改文本。
  function buildOverview() {
    el.overview.textContent = '';
    overviewRefs = {};
    OVERVIEW_ITEMS.forEach(function (item) {
      var cell = document.createElement('div');
      cell.className = 'ov-item';
      var label = document.createElement('span');
      label.className = 'ov-label';
      label.textContent = item[1];
      var value = document.createElement('div');
      value.className = 'ov-value';
      cell.appendChild(label);
      cell.appendChild(value);
      el.overview.appendChild(cell);
      overviewRefs[item[0]] = value;
    });
  }

  // 值的两种写法：单行（大部分格子）与多行（剩余价值按币种分行）。
  // 多行也不拼 HTML 字符串，而是逐行 createElement + textContent（防 XSS）。
  function setOverviewText(key, text) {
    var box = overviewRefs[key];
    if (box) box.textContent = text;
  }

  function setOverviewLines(key, lines) {
    var box = overviewRefs[key];
    if (!box) return;
    box.textContent = '';
    lines.forEach(function (line) {
      var span = document.createElement('span');
      span.className = 'ov-line';
      span.textContent = line;
      box.appendChild(span);
    });
  }

  function renderOverview(t) {
    if (!el.overview) return;
    if (!overviewRefs) buildOverview();
    el.overview.hidden = false;

    // 没有在线节点（或还没上报过）时显示 —：写 "0 B / 0 B (0%)" 会被读成
    // "机器都是空的"，而事实是"一台都没在线，压根没有实时数据"。
    // 内存是 1024 进制（GiB）、硬盘与流量是 1000 进制（GB/TB）：口径见 fmtBytesBin。
    setOverviewText('mem', t.mem_total > 0
      ? fmtBytesBin(t.mem_used) + ' / ' + fmtBytesBin(t.mem_total) + ' (' + fmtPct(t.mem_pct) + ')'
      : '—');
    setOverviewText('disk', t.disk_total > 0
      ? fmtBytesDec(t.disk_used) + ' / ' + fmtBytesDec(t.disk_total) + ' (' + fmtPct(t.disk_pct) + ')'
      : '—');

    // 方向与卡片上的 ↑/↓ 一致：↑ 是上行（tx），↓ 是下行（rx）。
    setOverviewText('up', '↑ ' + fmtRate(t.tx_rate));
    setOverviewText('down', '↓ ' + fmtRate(t.rx_rate));

    // 累计流量来自数据库里的历史账（不是 Agent 的实时快照），两个方向分开显示。
    setOverviewText('traffic', '↑ ' + fmtBytesDec(t.traffic_tx_total) + '  ↓ ' + fmtBytesDec(t.traffic_rx_total));

    // 剩余价值**按币种分行**：不同币种不能相加（见 overview.go 的 overviewCurrency）。
    // 只有一种币种时就是一行，与多币种走的是同一条路径。
    var values = (t.remaining_value || []).map(function (item) {
      return fmtMoney(item.cents, item.currency);
    });
    setOverviewLines('value', values.length ? values : ['—']);
  }

  // clearOverview 把总览区退回"没有数据"的样子（退出登录时用）。
  function clearOverview() {
    if (!el.overview) return;
    el.overview.hidden = true;
    el.overview.textContent = '';
    overviewRefs = null;
  }

  // loadOverview 取一次总览并铺到总览区。
  //
  // 失败只忽略（不弹 toast）：总览是一个附加区块，节点卡片与实时流不该
  // 因为它拉不到就停摆；下一次轮询（60 秒后）自然会重试。
  function loadOverview() {
    return api('/api/v1/overview?window=1h&buckets=10').then(function (data) {
      renderOverview(data.totals || {});
    }).catch(function () { /* 忽略：下一次轮询会重试 */ });
  }

  function startOverview() {
    loadOverview();
    if (overviewTimer) return;   // 已经在跑：绝不重复叠加（见 syncOverviewTimer）
    overviewTimer = window.setInterval(function () {
      // 后台标签页不请求：总览是分钟级数据，用户看不见的时候取回来只是白花流量。
      if (document.hidden) return;
      loadOverview();
    }, OVERVIEW_POLL_MS);
  }

  function stopOverview() {
    if (overviewTimer) {
      window.clearInterval(overviewTimer);
      overviewTimer = null;
    }
  }

  // syncOverviewTimer 让总览的定时器跟着首页视图走：在首页就（确保）在跑，
  // 不在首页就停掉。
  //
  // 为什么必须"确保"而不是每次新建：setView 会被 hashchange、登录、会话刷新
  // 反复调用，每调一次就 setInterval 一个的话，定时器会越叠越多 ——
  // 表现是 /overview 的请求数随时间成倍增长，而页面上看不出任何异常。
  // 反过来也一样重要：离开首页后还在每 60 秒请求一次，详情页与设置页根本没人看。
  function syncOverviewTimer() {
    if (el.viewHome.hidden) {
      stopOverview();
      return;
    }
    startOverview();
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
    stopOverview();
    // 总览是"上一位登录者那一屏"的数据：不清掉的话，下一位登录进来、
    // 新的 /overview 还没回来的那一瞬会看到别人的集群合计。
    clearOverview();
    // 设置页的服务器列表同样是"上一位登录者那一屏"的数据，一起清掉
    // （下次进设置页会重新取）。
    settingsNodes = [];
    el.nodesList.textContent = '';
    el.nodesEmpty.hidden = true;
    el.nodesError.textContent = '';
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
      // 顺序也要跟着接口走：renderNode 对**已经存在**的卡片只更新内容、不移动
      // DOM 位置，所以拖完顺序后光"重新取一次数"是不够的 —— 卡片会留在原处，
      // 看起来像首页没跟着变。appendChild 对已经排在末尾的节点是空操作，
      // 因此这次全量取数之外的每秒 SSE 推送（applyPayload，只改变更集）
      // 不会碰顺序：顺序只在 GET /api/v1/nodes 里有意义。
      data.nodes.forEach(function (dto) {
        var card = cards.get(dto.id);
        if (card && card.root.parentNode === el.grid) el.grid.appendChild(card.root);
      });
      renderSummary(data.summary, Math.floor(Date.now() / 1000));
    });
  }

  // ---------------------------------------------------------------- 节点详情

  // 3d/7d 的格式要看**实际标签间隔**（第二个参数，由图表抽稀后传进来）：
  // 抽稀后的间隔可能小于一天（例如 3d 档实际每 10 小时一个），那时只用 MM-DD 会画出
  // 「09-29 09-29 09-29」一串长得一模一样的标签 —— 加上时分才分得清是哪一天里的哪一刻。
  // 间隔 ≥ 一天时保持 MM-DD：标签更短，同样宽度下能放下更多个。
  var RANGE_X_FORMAT = {
    '1h': function (ts) { return clockOf(ts); },
    '6h': function (ts) { return clockOf(ts); },
    '12h': function (ts) { return clockOf(ts); },
    '1d': function (ts) { return clockOf(ts); },
    '3d': function (ts, step) { return step >= 86400 ? dateOf(ts) : dateTimeOf(ts); },
    '7d': function (ts, step) { return step >= 86400 ? dateOf(ts) : dateTimeOf(ts); }
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

  function dateTimeOf(ts) {
    return dateOf(ts) + ' ' + clockOf(ts);
  }

  // 图表轴上的字节刻度：**1000 进制**，与 fmtBytesDec 同一口径（流量图用它画
  // 每天的量）。单位后缀写全（KB/MB/GB/TB）而不是只写 K/M/G —— 只写 K/M/G 的话，
  // 看图的人没法判断这是 1000 进制还是 1024 进制，而这正是本次要分开的两套口径。
  // 速率图的刻度走 fmtRate（那样才会带上 "/s"），不共用这个函数。
  function fmtAxisBytes(v) {
    if (v >= 1000000000000) return (v / 1000000000000).toFixed(v < 10000000000000 ? 1 : 0) + ' TB';
    if (v >= 1000000000) return (v / 1000000000).toFixed(v < 10000000000 ? 1 : 0) + ' GB';
    if (v >= 1000000) return (v / 1000000).toFixed(v < 10000000 ? 1 : 0) + ' MB';
    if (v >= 1000) return (v / 1000).toFixed(0) + ' KB';
    return v.toFixed(0) + ' B';
  }

  // aggregate 按目标间隔把点合并到整齐的时间网格上（手机端用，绝不插值造点）。
  //
  // 合并后的桶取这几个点的**算术平均**（avg）与最大值（max）。
  //
  // 这里刻意**不**跳过 0：三条资源曲线上的 0 都是实打实的读数（0% CPU、0% 内存、
  // 0 B/s），把它们排除在分母之外会让"一直闲着"的那一段被算成"平均下来很忙"。
  // 这个坑以前真的踩过：为了「延迟探测」那张图（那里 avg == 0 表示"没有样本"，
  // 与 0ms 正好相反）加过一次"只累加正样本"，而它是**按序列生效**的 —— 五张资源
  // 图跟着一起被改了。延迟图随功能删除后，那条特殊规则也一起删掉。
  function aggregate(points, targetSec) {
    if (!targetSec || targetSec <= 0 || points.length === 0) return points;
    var out = [];
    var cur = null;
    points.forEach(function (p) {
      var bucket = Math.floor(p[0] / targetSec) * targetSec;
      if (!cur || cur[0] !== bucket) {
        // [桶起点, avg 累加, 峰值, 样本个数]
        cur = [bucket, 0, 0, 0];
        out.push(cur);
      }
      cur[1] += p[1];
      cur[3] += 1;
      cur[2] = Math.max(cur[2], p[2]);
    });
    return out.map(function (c) {
      // c[3] 恒 >= 1（每个合并桶至少来自一个原始点），不会出现 0/0 的 NaN。
      return [c[0], c[1] / c[3], c[2]];
    });
  }

  // seriesFor 是**通用**取点：把接口给的点按档位做一次手机端二次聚合。
  // CPU / 内存 / 磁盘 / 网络与流量图都走它 —— 那里的 0 是实打实的读数
  // （0% CPU、0 B/s 都是合法的），所以绝不能把 0 当成缺失。
  function seriesFor(points) {
    var meta = rangeMeta(detail.range);
    var mobile = window.matchMedia(MOBILE_QUERY).matches;
    return aggregate(points, mobile ? meta.mobile_agg_sec : 0);
  }

  // rangeMeta 按档位取后端给的图表参数（六档都在 detail.ranges 里）。
  //
  // 兜底对象只带**真正会被读到**的字段：tick_base_sec 是 X 轴基准间隔（1h 档 1 分钟），
  // mobile_agg_sec 是手机端的二次聚合目标。这里原样抄过一个 tick_label_sec ——
  // 那个字段（"实际标签 + 竖网格线"的间隔）已经不再影响任何一条线，抄过来只会让人
  // 以为它还在用（见 chart.js 顶部关于 tickBaseSec 的说明）。
  function rangeMeta(rangeKey) {
    for (var i = 0; i < detail.ranges.length; i++) {
      if (detail.ranges[i].key === rangeKey) return detail.ranges[i];
    }
    return { key: rangeKey, tick_base_sec: 60, mobile_agg_sec: 0 };
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

  // renderRangeButtons 渲染时间档位按钮（1h…7d）。
  //
  // 它曾经渲染**两组**（「资源与网络」卡与「延迟」卡各一组）。延迟卡随功能一起
  // 删除之后只剩下一组，但"渲染"与"状态"仍然是分开的两件事：renderRangeGroup
  // 只负责画，当前选中的档位在 detail.range 里（见 setResourceRange）。
  function renderRangeButtons() {
    renderRangeGroup(el.detailRanges, detail.range, setResourceRange);
  }

  // renderRangeGroup 画一组档位按钮：清空容器的旧按钮、按 detail.ranges 重建、
  // 把 activeKey 那一个高亮出来。点击交给 onPick。
  function renderRangeGroup(box, activeKey, onPick) {
    box.textContent = '';
    detail.ranges.forEach(function (r) {
      var b = document.createElement('button');
      b.type = 'button';
      b.className = 'range-btn' + (r.key === activeKey ? ' active' : '');
      b.textContent = r.key;
      b.addEventListener('click', function () { onPick(r.key); });
      box.appendChild(b);
    });
  }

  // setResourceRange 切「资源与网络」卡的档位：只重取资源序列（CPU/内存/磁盘/网络）。
  function setResourceRange(key) {
    if (detail.range === key) return;
    detail.range = key;
    renderRangeButtons();
    loadSeries();
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
    // 「开机时长」而不是「运行时间（Uptime）」：它来自 /proc/uptime，是机器自上次重启
    // 以来的时长，不是"在线了多久"（在线率看流量卡里的可用率）。
    infoRow(sys, '开机时长', fmtUptime(node.uptime_sec));
    infoRow(sys, 'Agent 版本', node.agent_version || '—');

    // 存储信息
    infoRow(sto, '内存', fmtPct(node.mem_pct));
    infoRow(sto, '内存交换', fmtPct(node.swap_pct));
    infoRow(sto, '磁盘', diskSummary(node));

    // 网络信息
    infoRow(net, '实时网络', '↑ ' + fmtRate(node.tx_rate) + '  ↓ ' + fmtRate(node.rx_rate));
    infoRow(net, '累计流量', '↑ ' + fmtBytesDec(node.tx_total) + '  ↓ ' + fmtBytesDec(node.rx_total));
    // 这一格叫「面板延迟」而不是「延迟」：它的值是 Agent 到**面板自身**的
    // WebSocket ping/pong 往返（走 Cloudflare 隧道时恒为 ~100ms）。标签写清楚，
    // 免得被当成"到某个外部目标的探测延迟"（那一整块功能已删除，见 README/docs）。
    infoRow(net, '面板延迟', node.lat_ms > 0 ? node.lat_ms.toFixed(1) + ' ms' : '—');
    infoRow(net, '监控网卡', node.iface || '—');
    // 「本机地址」与「来源 IP」是两回事，标签不能含糊：
    // 前者是 Agent 自己算出来的机器地址（UDP connect 取源地址），后者是服务端
    // 看到的 TCP 来源。Agent 与探针同机、或走 Cloudflare Tunnel 时，后者恒为
    // 127.0.0.1；节点在 NAT/代理后面时，只有前者才是机器自己的地址。
    infoRow(net, '本机地址', localIPText(node));
    infoRow(net, '来源 IP', node.observed_ip || '—');

    // 流量信息（今日 / 本周 / 本周期 / 历史累计是四个不同口径，标签写清楚免得看串）。
    //
    // 「本周」的口径：**本周一 00:00 到现在**，按服务端配置的 --timezone 切天
    // （与「今日」用的是同一套切法，见后端的 store.WeekStart）。今天是周一时
    // 「本周」就等于「今日」—— 这不是特例，而是同一个公式的自然结果。
    //
    // 每一行的"总和"与"占比"都由服务端算好（见 dto.go 的 applyTraffic）：
    // 前端只做单位换算与拼串。今日/本周的分母也是**月额度**，所以它们说的是
    // "占月额度的百分之多少"，不是"占今天用的那点流量的百分之多少"。
    infoRow(tra, '今日流量', trafficCell(node.traffic_today_rx, node.traffic_today_tx,
      trafficSumText(node.traffic_today_total, node.traffic_today_pct, 0)));
    infoRow(tra, '本周流量', trafficCell(node.traffic_week_rx, node.traffic_week_tx,
      trafficSumText(node.traffic_week_total, node.traffic_week_pct, 0)));
    infoRow(tra, '本周期流量', trafficCell(node.traffic_cycle_rx, node.traffic_cycle_tx,
      trafficSumText(node.traffic_cycle_total, node.traffic_pct, node.traffic_limit)));
    infoRow(tra, '历史累计流量', '↓ ' + fmtBytesDec(node.traffic_total_rx) + '  ↑ ' + fmtBytesDec(node.traffic_total_tx));
    if (node.cycle_start) {
      infoRow(tra, '计费周期', node.cycle_start + ' → ' + node.cycle_end + '（每月 ' + node.reset_day + ' 日重置）');
    }
    // 可用率那两行（24h / 7d）已经删掉：那是**面板看到的在线率**，混在流量卡里
    // 既容易与"流量"读成一件事，也与「开机时长」那一行重复。
    // 后端 /nodes/{id} 仍然返回 uptime（detail.uptime），留着是为了不改接口契约。

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

  // chartCard 返回装了图表的卡片（「资源与网络」，见 index.html）。
  //
  // 用 el 上的键现取，而不是把元素缓存在模块顶部的数组里：el 是 main() 启动时
  // 从 DOM 自动登记的（那时才存在），在这里现取既拿得到最新引用，也让"键必须
  // 能由 id 推导"这条不变量继续由测试盯着。
  //
  // 返回值是数组：这里以前有第二张卡（「延迟」），函数是按"多张卡各自判断显隐"
  // 写的；现在只有一张，但 applyChartVisibility 仍然按数组遍历 —— 保留这个形状
  // 是为了让"以后再加一张图表卡"不必重写那段逻辑。
  function chartCards() {
    return [el.chartsResources].filter(function (node) { return !!node; });
  }

  // spanFullRow 让"最后一个可见图块"在可见个数为奇数时横跨两列。
  //
  // 为什么不用 CSS 的 :last-child:nth-child(odd)：被隐藏的图块仍然是子节点，
  // 只留 CPU 时 :last-child 落在被隐藏的「近 7 天流量」上，那条规则根本不生效 ——
  // 图排进左半格、右半格空一块。CSS 也没法数"可见的兄弟"（能数的
  // :nth-child(… of S) 要 Chrome 111+，这里不赌浏览器版本）。
  function spanFullRow(blocks) {
    var odd = blocks.length % 2 === 1;
    blocks.forEach(function (block, i) {
      block.classList.toggle('span-full', odd && i === blocks.length - 1);
    });
  }

  // applyChartVisibility 只切换 chart-block 的显隐，不销毁图表实例：
  // 勾回来的时候还能复用同一个 canvas 与事件监听。
  //
  // 每张图表卡**各自**判断：五张资源图全被取消勾选时整张卡收起，页面上不会留下
  // 一个只有标题的空边框（那看着像加载失败）。
  function applyChartVisibility() {
    chartCards().forEach(function (card) {
      var shown = [];
      Array.prototype.forEach.call(card.querySelectorAll('.chart-block'), function (block) {
        var on = chartVisible(block.dataset.chart);
        block.hidden = !on;
        if (on) shown.push(block);
      });
      spanFullRow(shown);
      card.hidden = shown.length === 0;
    });
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

  // trafficCell 拼一行流量：左边是方向读数（↓ 收 / ↑ 发），右边是"总和（占比）"。
  //
  // 两段之间**不堆空格字符**，而是两个 span + CSS 的 gap（见 style.css 的
  // .traffic-cell）：空格在对齐上不可控（等宽字体与比例字体里宽度不同），
  // 而且在窄屏折行时会被留在行首/行尾，看起来像多了一个缩进。
  //
  // 流量一律 1000 进制（GB/TB），与「月流量额度（GB）」输入框的口径一致。
  function trafficCell(rx, tx, sumText) {
    var box = document.createElement('span');
    box.className = 'traffic-cell';
    var dir = document.createElement('span');
    dir.className = 'traffic-dir';
    dir.textContent = '↓ ' + fmtBytesDec(rx) + '  ↑ ' + fmtBytesDec(tx);
    box.appendChild(dir);
    // 没有总和可写（数据还没到）时右边整段不出现：留一个空 span 会多出一段空隙。
    if (sumText) {
      var sum = document.createElement('span');
      sum.className = 'traffic-sum';
      sum.textContent = sumText;
      box.appendChild(sum);
    }
    return box;
  }

  // trafficSumText 拼「总和（占比）」那一段。三个数全部来自服务端，前端只格式化。
  //
  //   - total 是服务端算好的收 + 发；
  //   - pct 的分母是**月额度**（今日/本周也一样），所以它是"占月额度"的百分比；
  //   - limit 只有"本周期"那一行才传：那一行要写清分母是哪个额度
  //     （15.0 GB / 1.00 TB（1.5%））；今日/本周写「15.0 GB（1.5%）」就够了，
  //     每行都重复一遍额度只会把这一列撑得很长。
  //
  // 没填额度（limit <= 0，服务端的 pct 也会是 0）时**不显示占比**：
  // 没有分母的百分比是没有意义的，写 0% 会被读成"这个月一点没用"。
  function trafficSumText(total, pct, limit) {
    var text = fmtBytesDec(total);
    if (limit > 0) text += ' / ' + fmtBytesDec(limit);
    if (!(limit > 0)) return text;
    return text + '（' + (pct || 0).toFixed(1) + '%）';
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
        // 流量图一天一个点，基准间隔就是 1 天。屏幕上放不下时由图表引擎按标签
        // 实际宽度自动稀疏（放大到 2 天、5 天…这几个整齐倍数），前端不操心。
        tickBaseSec: 86400,
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
      // tick_base_sec 是这一档的 X 轴**基准**间隔（1h/6h/12h = 1 分钟、1d = 2 分钟、
      // 3d = 5 分钟、7d = 15 分钟）。它比屏幕上能放下的密得多，实际标签间隔由
      // chart.js 按标签文本宽度自动稀疏（整齐倍数）。tick_label_sec 那个字段已经
      // 不再影响画面（它过去是"标签 + 竖网格线"的步长），所以这里不再读它。
      var tickBase = meta.tick_base_sec || 60;

      // 流量图的 Y 轴是字节（每天的量）：轴自带单位（GB/TB），所以 unit 留空。
      // 百分比图的 unit 也必须留空：读数是 yFormat(值) + unit 拼出来的，而下面的
      // yFormat 已经带上了 '%'（刻度轴用的也是它），再给 unit 一个 '%' 会拼成 "0%%"。
      // 这两个图里只有这一处重复过 —— 其余（字节/速率）都是"单位只出现在一处"：
      // 要么在 yFormat 里，要么在 unit 里。
      var pctOpts = { yMax: 100, unit: '', yFormat: function (v) { return v.toFixed(0) + '%'; }, tickBaseSec: tickBase, xFormat: xFormat, showMax: true };
      // 速率图的 Y 轴是"每秒多少字节"：yFormat 直接给 fmtRate（KB/s、MB/s，
      // 1000 进制），单位已经写在刻度里，unit 必须留空 —— 否则读数会变成 "MB/s/s"。
      var rateOpts = { yMax: 0, unit: '', yFormat: fmtRate, tickBaseSec: tickBase, xFormat: xFormat, showMax: true };

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

  function openDetail(id) {
    detail.id = id;
    setView('detail');
    el.detailName.textContent = '加载中…';
    clearDetailPanels();
    // 档位按钮先清掉：留着上一个节点的按钮会让人以为档位已经生效了
    // （档位表要等 /nodes/{id} 回来才知道，见下面的 renderRangeButtons）。
    el.detailRanges.textContent = '';
    // 先按可见性把图表块藏好，再去请求数据：隐藏的图连一次请求都不发。
    applyChartVisibility();

    api('/api/v1/nodes/' + id).then(function (data) {
      detail.node = data.node;
      detail.uptime = data.uptime || {};
      detail.ranges = data.ranges || [];
      if (!detail.range && detail.ranges.length) detail.range = detail.ranges[0].key;
      renderDetailInfo();
      renderRangeButtons();
      return Promise.all([
        loadSeries(),
        loadTrafficChart()
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

  // nodeDialogID 是"这次编辑的是哪个节点"。
  //
  // 以前保存时直接用 detail.id，而设置页的「服务器列表」也能打开这个对话框 ——
  // 那时详情页是关着的（detail.id = 0），保存会打到 /api/v1/nodes/0 上，
  // 表现是"保存按钮点了没反应，只有一行红字"。目标 id 必须跟着对话框自己走。
  var nodeDialogID = 0;

  function openNodeDialog(mode, dto) {
    nodeDialogMode = mode;
    nodeDialogID = mode === 'edit' && dto ? dto.id : 0;
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
    // 月流量额度在表单里是 **GB（10⁹ 字节）**，与标签「月流量额度（GB，0 表示不限）」
    // 一字不差地对应。曾经这里除以 1024³（GiB）：用户填 2000 以为买了 2000 GB，
    // 实际入库 2147 GB，80% 预警要等真实用量到 86% 才响 —— 用户可能在收到预警前
    // 就已经超了商家的额度。硬盘/流量的口径见 fmtBytesDec，内存才是 1024 进制。
    el.nodeTraffic.value = d.traffic_limit ? Math.round(d.traffic_limit / 1e9) : 0;
    el.nodeWarn.value = d.traffic_warn_pct || 80;
    el.nodeReset.value = d.reset_day || 1;
    el.nodeNote.value = d.note || '';
    // 标签回填成「a; b; c」（分号 + 空格）：用户看到的这一串，就是保存时会被
    // splitTags 切分、也是接口最终会收到的那份列表 —— 中间没有第二套草稿要同步。
    el.nodeTags.value = tagsToInputValue(d.tags);
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
      // GB → 字节：1 GB = 10⁹ 字节（与输入框标签、fmtBytesDec 同一口径）。
      traffic_limit: Math.max(0, Math.round((parseFloat(el.nodeTraffic.value) || 0) * 1e9)),
      traffic_warn_pct: Math.min(100, Math.max(1, parseInt(el.nodeWarn.value, 10) || 80)),
      reset_day: Math.min(31, Math.max(1, parseInt(el.nodeReset.value, 10) || 1)),
      expires_at: expiresAt,
      enabled: el.nodeEnabled.checked,
      // 标签：请求体里是**现切**的列表，文本框是唯一的事实来源。
      // 与其它字段一样是"整体替换"语义 —— 这里带上它，保存节点时标签就跟着一起存，
      // 不需要第二个对话框（旧版那个标签对话框正是为此存在的）。
      tags: splitTags(el.nodeTags.value)
    };
  }

  // validateNodeTags 在提交前挡一次超长/超量的标签。
  //
  // 服务端也会拒（同一条规则，见 store.NormalizeTags），但那要等一个来回；
  // 这里立刻把消息显示在对话框的错误位上，用户不用盯着一个转圈的按钮猜哪里填错了。
  // 上限常量与提示文案同源（TAG_MAX_COUNT / TAG_MAX_LEN），改一处就够。
  function validateNodeTags(payload) {
    return tagProblem(payload.tags || []);
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
    var problem = validateNodePricing(payload) || validateNodeTags(payload);
    if (problem) {
      el.nodeError.textContent = problem;
      return;
    }
    el.nodeSubmit.disabled = true;
    var editing = nodeDialogMode === 'edit';
    var path = editing ? '/api/v1/nodes/' + nodeDialogID : '/api/v1/nodes';

    api(path, { method: editing ? 'PATCH' : 'POST', body: payload }).then(function (data) {
      el.dlgNode.close();
      if (editing) {
        toast('已保存');
        // 详情页与首页一起刷新。
        return api('/api/v1/nodes/' + nodeDialogID).then(function (fresh) {
          // 从设置页打开这个对话框时详情页是关着的，刷它没有意义也无害
          // （detail.id 为 0 时下面的赋值不会影响任何视图）。
          if (detail.id === nodeDialogID) {
            detail.node = fresh.node;
            detail.uptime = fresh.uptime || {};
            detail.ranges = fresh.ranges || [];
            renderDetailInfo();
          }
          return refreshNodeViews();
        });
      }
      showToken(data.token, payload.name);
      return refreshNodeViews();
    }).catch(function (err) {
      // 失败**不关对话框**：名称重复、标签超限这类错误全都要用户就地改一改再重试，
      // 关掉的话他填了十几个字段的表单就没了（只剩一句转瞬即逝的 toast）。
      el.nodeError.textContent = err.message;
    }).then(function () {
      el.nodeSubmit.disabled = false;
    });
  }

  // refreshNodeViews 把"所有显示节点的地方"刷新一遍：首页卡片（loadNodes）
  // 与设置页的「服务器列表」（它自己有一份快照）。新增/编辑之后都要调，
  // 否则刚改完的那一处还是旧数据 —— 用户会以为没保存成功。
  function refreshNodeViews() {
    var jobs = [loadNodes()];
    if (!el.viewSettings.hidden) jobs.push(loadSettingsNodes());
    return Promise.all(jobs);
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
  // 顺序即左栏从上到下的顺序。
  var SETTINGS_PANES = ['notify', 'alert', 'dashboard', 'nodes', 'security', 'server', 'audit'];

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
      el.dashboardError, el.dashboardOk,
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
    // 服务器列表也一样：每次进设置页都重新取一次节点（机器可能在别处刚被改过）。
    loadSettingsNodes();

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
        // 这一栏是"面板自己的信息"，所以这个值是**面板进程**启动至今的时长，
        // 不是任何一台被监控节点的。写「面板已运行」免得跟节点卡片的「开机时长」混淆。
        ['面板已运行', fmtUptime(info.uptime_sec)]
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
    if (pane === 'nodes') return el.nodesError;
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

  // ---------------------------------------------------------------- 服务器列表（设置栏）
  //
  // 一行一台机器，横着依次是：拖拽把手 → 状态点 + 名称 + 地区徽章 →（从已有字段
  // 自动拼出来的信息：IP · 分组 · 剩余价值 · 到期天数）→ 标签 → 「编辑节点」按钮。
  //
  // 这几段在宽屏下**排在同一行**（见 style.css 的 .node-item / .node-item-body：
  // 允许折行的单行 flex），宽度不够时按上面的顺序折行 —— 一台机器的标签因此
  // 能跟它的名称并排，而不是永远各占一行。这一栏不新增任何输入项：
  // 要看什么都在节点数据里，改配置走「编辑节点」那个对话框。
  //
  // 行的顺序（以及首页卡片的顺序）由服务端的 sort_order 决定：拖动把手松手后
  // 把整份顺序 PUT 给 /api/v1/nodes/order（见下面的拖动排序一节）。

  // settingsNodes 是这一栏自己的一份快照。
  //
  // 为什么不直接用首页那份 nodes：这一栏可以被**直接深链**打开
  // （#/settings/nodes），那时首页还从来没加载过，nodes 是空的 ——
  // 表现是"服务器列表永远空着，只有先去一趟首页才正常"。
  var settingsNodes = [];

  // rowButton 造行尾的小按钮（现在只剩「编辑节点」一个，这里保留工厂函数）。
  function rowButton(text, onClick) {
    var btn = document.createElement('button');
    btn.type = 'button';
    btn.className = 'btn';
    btn.textContent = text;
    btn.addEventListener('click', onClick);
    return btn;
  }

  // loadSettingsNodes 取一次节点并整块重画这一栏。
  function loadSettingsNodes() {
    return api('/api/v1/nodes').then(function (data) {
      settingsNodes = data.nodes || [];
      el.nodesError.textContent = '';
      renderSettingsNodes();
    }).catch(function (err) {
      // 读取失败落在这一栏里，而不是只弹一个转瞬即逝的 toast：
      // 否则页面上是一片空白，看不出是"没有节点"还是"没读出来"。
      el.nodesError.textContent = '读取节点列表失败：' + err.message;
    });
  }

  function renderSettingsNodes() {
    el.nodesList.textContent = '';
    el.nodesEmpty.hidden = settingsNodes.length > 0;
    settingsNodes.forEach(function (node) {
      el.nodesList.appendChild(settingsNodeRow(node));
    });
  }

  // settingsNodeRow 造一台机器的那一行。
  //
  // 结构是「把手 + 内容 + 右侧操作」三列（见 style.css 的 .node-item 网格）：
  // 两个按钮必须和**整行**垂直居中，而不是跟名称挤在第一行 —— 机器信息有三行，
  // 按钮跟在第一行里会随着内容变长越来越"飘在顶上"。
  function settingsNodeRow(node) {
    var row = document.createElement('div');
    row.className = 'node-item';

    // 拖拽把手：**只有**它按下才可能开始拖（见 startNodeDrag）。
    // 整行都能拖的话，想按住名称选一段文字、或者在标签上划一下看看全称，
    // 都会变成"把这一行拖走了"；而这一行里还有两个按钮，手一抖就误触。
    // 六个点由 CSS 画（radial-gradient 平铺），不引入任何图标资源。
    var handle = document.createElement('div');
    handle.className = 'node-drag';
    handle.title = '按住拖动调整顺序';
    handle.setAttribute('aria-label', '拖动排序');
    handle.addEventListener('pointerdown', function (event) {
      startNodeDrag(event, node, row, handle);
    });
    row.appendChild(handle);

    var body = document.createElement('div');
    body.className = 'node-item-body';

    var head = document.createElement('div');
    head.className = 'node-item-head';
    // 状态点沿用首页卡片那套配色变量（.dot.online/.stale/.offline）：
    // 同一个状态在首页与设置页必须是同一个颜色。
    var dot = document.createElement('span');
    dot.className = 'dot ' + (node.status || 'unknown');
    var name = document.createElement('span');
    name.className = 'node-item-name';
    name.textContent = node.name;
    head.appendChild(dot);
    head.appendChild(name);
    // 地区徽章：只有填了地区才显示（没填就不摆一个空框）。
    if (node.region) {
      var region = document.createElement('span');
      region.className = 'node-item-region';
      region.textContent = node.region;
      head.appendChild(region);
    }
    body.appendChild(head);

    // 第二行：IP · 分组 · 剩余价值 · 到期天数。
    //
    // 每一段都是"有才显示"：没填分组就不写「分组：—」这种占位 ——
    // 一堆破折号比少一行更难读，也会把真正缺失的信息淹掉。
    var pieces = [];
    // IP 优先用 local_ip（Agent 自报的本机地址），没有就退回 observed_ip
    // （服务端看到的来源地址；走隧道或 NAT 时它可能是 127.0.0.1，所以只当兜底）。
    var ip = node.local_ip || node.observed_ip || '';
    if (ip) {
      var ipSpan = document.createElement('span');
      ipSpan.className = 'node-item-ip';
      ipSpan.textContent = ip;
      pieces.push(ipSpan);
    }
    if (node.group_name) pieces.push(document.createTextNode('分组：' + node.group_name));
    // 剩余价值只在**填过价格**时才有意义：没价格时 remaining_value_cents 恒为 0，
    // 显示成「剩余价值 ¥0.00」会被读成"这台机器一文不值"。
    if (node.price_cents > 0) {
      pieces.push(document.createTextNode('剩余价值 ' + fmtMoney(node.remaining_value_cents, node.currency)));
    }
    if (node.expires_at > 0) pieces.push(document.createTextNode(node.remaining_days + ' 天后到期'));

    var meta = document.createElement('div');
    meta.className = 'node-item-meta';
    pieces.forEach(function (piece, i) {
      if (i > 0) meta.appendChild(document.createTextNode(' · '));
      meta.appendChild(piece);
    });
    meta.hidden = pieces.length === 0;
    body.appendChild(meta);

    // 第三行：标签（没有标签就整行不显示）。
    var tags = document.createElement('div');
    tags.className = 'node-item-tags';
    var tagList = node.tags || [];
    tags.hidden = tagList.length === 0;
    tagList.forEach(function (tag) { tags.appendChild(tagChip(tag)); });
    body.appendChild(tags);

    row.appendChild(body);

    // 右侧操作列：只剩「编辑节点」一个按钮。
    // 标签以前有一个自己的按钮与对话框，现在并进了这个对话框的
    // 「标签」输入框 —— 一台机器的配置应该在一个地方改完，两个入口迟早会出现
    // "这里改了那里没改"的错觉（而保存都是整体替换，两处其实改的是同一份数据）。
    var acts = document.createElement('div');
    acts.className = 'node-item-acts';
    acts.appendChild(rowButton('编辑节点', function () { openNodeDialog('edit', node); }));
    row.appendChild(acts);

    return row;
  }

  // ---------------------------------------------------------------- 拖动排序
  //
  // 用 Pointer Events（pointerdown/pointermove/pointerup + setPointerCapture），
  // 不用 HTML5 那套原生拖放属性：它在触摸屏上根本不触发 dragstart，而这一栏
  // 是要在手机上看的。Pointer Events 一套代码同时覆盖鼠标、触摸与触控笔。
  //
  // 拖动时必须同时给出三个视觉要素，缺一个用户就不知道发生了什么：
  //   1. 被拖的那一行跟着指针走，半透明 + 抬起来的阴影（.dragging）；
  //   2. 目标位置有一条插入线（.drop-before/.drop-after 的伪元素）；
  //   3. 整页禁止选中文字（body.drag-active），否则指针扫过的地方会被选成一片蓝。

  // dragRow 是当前正在拖的那一行（null = 没在拖）。
  //
  // 多指触摸时只认第一根手指：第二根手指的 pointerdown 会因为 dragRow 非空
  // 直接返回，否则两行会跟着两根手指各跑各的。
  var dragRow = null;

  // suppressRowClick 用来吞掉"拖动结束时浏览器补发的那一次 click"。
  //
  // 指针扫过按钮、松手又落在按钮上时，浏览器会按"最近的公共祖先"补一个 click ——
  // 那一下会直接打开「编辑节点」对话框。所以：真的拖动过就吞掉紧跟着的这一次
  // click；下一次按下指针时（见 bind）清掉这个标记，免得"拖动在窗口外结束、
  // 没补 click"时把用户之后的第一次点击也吃掉。
  var suppressRowClick = false;

  // DRAG_SLOP 是"算不算在拖"的像素阈值：手指按下时哪怕只抖 1 像素，
  // 没有阈值也会立刻进入拖动状态（行跟着手指走、插入线乱闪），
  // 于是"点一下把手"永远点不出想要的结果。
  var DRAG_SLOP = 4;

  function startNodeDrag(event, node, row, handle) {
    // 只认主键：鼠标右键/中键按下不该开始拖（那是"另存为/新标签页"的入口）。
    if (event.button !== undefined && event.button !== 0) return;
    if (dragRow) return;

    // 阻止默认行为：不选中文字，也不发起浏览器的原生拖拽（把行里的文字拖出
    // 页面会生成一个跟随光标的 ghost 图像，松手还可能触发导航）。
    event.preventDefault();

    var order = settingsNodes.map(function (item) { return item.id; });
    var startIndex = order.indexOf(node.id);
    // 这一行不在当前快照里（数据正在被重新拉取）：宁可什么都不做，
    // 也不要把一个陌生 id 混进发给服务端的顺序里（那会整份请求 400）。
    if (startIndex < 0) return;
    dragRow = {
      id: node.id,
      row: row,
      handle: handle,
      pointerId: event.pointerId,
      startY: event.clientY,
      startIndex: startIndex,
      orderBefore: order,   // 失败回滚要用的"拖动前顺序"
      moved: false
    };
    row.classList.add('dragging');
    document.body.classList.add('drag-active');
    // 上一次的失败提示不该留在一个正在重新拖的页面上（它会让人以为这次也要失败）。
    el.nodesError.textContent = '';

    // 指针捕获：指针离开这一行、甚至离开窗口之后，pointermove/up 仍然送回
    // handle，拖动不会半路"丢了"。
    //
    // 合成事件（自动化测试、部分老浏览器）里没有对应的活动指针，
    // setPointerCapture 会抛 NotFoundError —— 捕获失败不影响拖动：下面几个
    // 监听本来就挂在 document 上，少掉的只是"事件目标锁定"这一层。
    try {
      handle.setPointerCapture(event.pointerId);
    } catch (err) { /* 见上：捕获失败照样能拖 */ }

    document.addEventListener('pointermove', onNodeDragMove);
    document.addEventListener('pointerup', onNodeDragEnd);
    document.addEventListener('pointercancel', onNodeDragCancel);
  }

  // nodeDragTargetIndex 算出"松手后这行会插到第几位"（在"去掉被拖那一行"
  // 的序列里的下标）。
  //
  // 判据是**其它行的竖直中点**：指针越过哪一行的中线，就排到它后面。
  // 不用行的上下边界：行高不一样（标签行可有可无），按边界算会出现
  // "指针明明还在这一行里，却已经算到下一行去了"的错觉。
  //
  // 被拖的那一行只用 transform 跟随指针，**不改变布局**，所以其它行的位置在
  // 整个拖动过程中是稳定的 —— 否则"会落到第几位"每动一下都要重算，越拖越乱。
  function nodeDragTargetIndex(clientY) {
    var rows = el.nodesList.children;
    var at = 0;
    for (var i = 0; i < rows.length; i++) {
      if (rows[i] === dragRow.row) continue;
      var rect = rows[i].getBoundingClientRect();
      if (clientY <= rect.top + rect.height / 2) break;
      at++;
    }
    return at;
  }

  function clearDropLines() {
    Array.prototype.forEach.call(el.nodesList.children, function (item) {
      item.classList.remove('drop-before', 'drop-after');
    });
  }

  // showDropLine 在"会落到的那一行"上画插入线（伪元素，见 style.css）。
  //
  // 用伪元素而不是往列表里插一个占位元素：占位元素会把其它行挤动，
  // 而行的位置一动，nodeDragTargetIndex 算出来的插入位置就会跟着跳。
  function showDropLine(at) {
    clearDropLines();
    var others = [];
    Array.prototype.forEach.call(el.nodesList.children, function (item) {
      if (item !== dragRow.row) others.push(item);
    });
    if (others.length === 0) return;
    if (at >= others.length) others[others.length - 1].classList.add('drop-after');
    else others[at].classList.add('drop-before');
  }

  function onNodeDragMove(event) {
    // 只认开始拖的那根手指：多指时别的手指不动这一行。
    if (!dragRow || event.pointerId !== dragRow.pointerId) return;
    var dy = event.clientY - dragRow.startY;
    if (!dragRow.moved) {
      if (Math.abs(dy) < DRAG_SLOP) return;   // 还没到阈值：先当成一次点击
      dragRow.moved = true;
    }
    // 只做竖直位移：横向一个像素都不动，所以拖动期间不会多出横向滚动条。
    dragRow.row.style.transform = 'translateY(' + dy + 'px)';
    showDropLine(nodeDragTargetIndex(event.clientY));
  }

  // orderWithMoved 把 id 从 before 里摘出来插到第 at 位。
  function orderWithMoved(before, id, at) {
    var rest = before.filter(function (item) { return item !== id; });
    rest.splice(at, 0, id);
    return rest;
  }

  function sameOrder(a, b) {
    if (a.length !== b.length) return false;
    for (var i = 0; i < a.length; i++) {
      if (a[i] !== b[i]) return false;
    }
    return true;
  }

  function onNodeDragEnd(event) {
    if (!dragRow || event.pointerId !== dragRow.pointerId) return;
    var state = dragRow;
    // 没超过阈值（只是点了一下把手）→ 位置不动。
    var at = state.moved ? nodeDragTargetIndex(event.clientY) : state.startIndex;
    var order = orderWithMoved(state.orderBefore, state.id, at);
    var moved = state.moved;
    endNodeDrag();
    if (!moved || sameOrder(order, state.orderBefore)) {
      // 原地放下：本地顺序没变，就不发请求 —— 一次没有信息量的 PUT 只会让
      // 别的浏览器跟着重画一次列表。
      return;
    }
    suppressRowClick = true;
    // 先本地重排（立刻看到结果，不等网络），再发请求。
    applyNodeOrder(order);
    saveNodeOrder(order, state.orderBefore);
  }

  function onNodeDragCancel() {
    // 系统把指针收走了（来电、手势返回、切到别的应用）：**不发请求**，
    // 顺序保持拖动前的样子 —— 用户并没有"放下"。
    endNodeDrag();
  }

  // endNodeDrag 收拾拖动状态：样式、监听、指针捕获。
  function endNodeDrag() {
    if (!dragRow) return;
    var handle = dragRow.handle;
    var pointerId = dragRow.pointerId;
    document.removeEventListener('pointermove', onNodeDragMove);
    document.removeEventListener('pointerup', onNodeDragEnd);
    document.removeEventListener('pointercancel', onNodeDragCancel);
    try {
      if (handle.hasPointerCapture && handle.hasPointerCapture(pointerId)) {
        handle.releasePointerCapture(pointerId);
      }
    } catch (err) { /* 捕获本来就没成功（合成事件）：没什么可释放的 */ }
    dragRow.row.style.transform = '';
    dragRow.row.classList.remove('dragging');
    clearDropLines();
    document.body.classList.remove('drag-active');
    dragRow = null;
  }

  // applyNodeOrder 按 id 顺序重排这一栏的快照并重画（纯本地，不等网络）。
  //
  // 松手后必须先做这一步：等 PUT 回来再重画的话，松手到重画之间页面还是旧顺序，
  // 用户会以为"没拖成功"而再拖一次。
  function applyNodeOrder(ids) {
    var byID = {};
    settingsNodes.forEach(function (item) { byID[item.id] = item; });
    var next = [];
    ids.forEach(function (id) { if (byID[id]) next.push(byID[id]); });
    // 数量对不上说明这份快照与这批 id 不同源（理论上不会发生）：宁可什么都不做，
    // 也不能把某一台机器从列表里弄丢。
    if (next.length !== settingsNodes.length) return false;
    settingsNodes = next;
    renderSettingsNodes();
    return true;
  }

  // saveNodeOrder 把新顺序发给服务端，成功后重新拉一次节点列表。
  //
  // 为什么成功之后必须**显式重拉**、不能等 SSE：SSE 推的是"哪些节点的实时数据
  // 变了"这一变更集，payload 里没有顺序这个概念 —— 首页网格的卡片顺序只有
  // GET /api/v1/nodes 才拿得到。refreshNodeViews() 一次把首页与这一栏都刷新，
  // 两边不会出现"设置页已是新顺序、首页还是旧顺序"。
  function saveNodeOrder(ids, before) {
    api('/api/v1/nodes/order', { method: 'PUT', body: { ids: ids } }).then(function () {
      return refreshNodeViews();
    }).catch(function (err) {
      // 失败要退回**拖动前**的顺序，并把错误留在这栏的错误位上（不是一个转瞬
      // 即逝的 toast）：页面停在用户拖出来的顺序、而服务端其实没接受的话，
      // 下次刷新顺序又跳回去，用户完全不知道哪一次生效了。
      applyNodeOrder(before);
      el.nodesError.textContent = '调整顺序失败：' + err.message + '（已恢复原来的顺序）';
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

    // 服务器列表：添加节点复用顶栏那套流程（同一个对话框、同一份校验）。
    el.nodesAdd.addEventListener('click', function () {
      openNodeDialog('create', null);
    });
    // 拖动排序收尾时浏览器可能补发一次 click（详情见 suppressRowClick）：
    // 在容器上以**捕获阶段**拦下来，这样它不会落到行里的按钮上。
    // 捕获阶段必须早于按钮自己的处理器 —— 冒泡阶段拦已经晚了。
    el.nodesList.addEventListener('click', function (event) {
      if (!suppressRowClick) return;
      suppressRowClick = false;
      event.preventDefault();
      event.stopPropagation();
    }, true);
    // 下一次按下指针就把"补发 click"的标记清掉：拖动如果在窗口外结束（没有
    // 补发 click），这个标记会一直留着，把用户接下来的第一次点击也吞掉。
    document.addEventListener('pointerdown', function () { suppressRowClick = false; }, true);

    // 节点对话框里的「标签」输入框：一个普通文本框，多个标签用 ; 分隔
    // （保存时才切分，见 nodeFormPayload）。提示里的上限由常量拼出来，
    // HTML 里那份只写分隔符 —— 两处都写死数字的话，改了上限而提示还留着旧数字，
    // 用户会照着错的数字删标签。
    el.nodeTagsHint.textContent = '多个标签用 ; 分隔（半角 ; 与全角 ；都行），最多 ' +
      TAG_MAX_COUNT + ' 个，每个最长 ' + TAG_MAX_LEN + ' 字';
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
