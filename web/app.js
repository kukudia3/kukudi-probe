'use strict';

/* 极简 VPS 探针 —— 前端
 *
 * 没有框架、没有构建步骤、没有外部请求。
 * 渲染只用 DOM API 与 textContent（绝不拼接 HTML，防 XSS）。
 * 实时数据走 SSE：服务端每秒只推"变了的节点"，这里按 id 局部更新卡片。
 */

(function () {
  var POLL_SESSION_MS = 30000;

  // 总览（首页顶部那块合计 + 卡片上的迷你条）的刷新周期。
  //
  // 为什么不跟 SSE 走：SSE 是每秒级的实时数据，而总览是**分钟级**的
  // （内存/硬盘的桶是每分钟落盘、累计流量每分钟算一次、探测结果每分钟一行），
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

  // 总览状态。
  //
  // overviewNodes 是最近一次 /api/v1/overview 里"每节点探测分桶"那部分（后端按
  // id 的字符串做键）。它必须留在模块状态里而不是随手用一次：卡片可能在这之后才
  // 被 SSE 创建出来（新节点上线、或这一轮 /nodes 里刚出现），那时 updateCard
  // 要能从这份数据里把它那张卡片的迷你条补上。
  var overviewNodes = {};
  var overviewTimer = null;

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

  // fmtPct1 是资源格与丢包用的百分比写法：**一律一位小数**。
  //
  // 与 fmtPct 不同（它 ≥10% 时取整）。两个理由：
  //   - 四格是两列并排的，"2.0%" 与 "25%" 混在一起时小数点对不齐；
  //   - 25.04% 与 25.96% 都写成 "25%"，看不出它在涨。
  // 丢包的浮层读数也是这个写法（与延迟格的一位小数对称），只有"一点没丢"
  // 仍然写 0%（那是实测值，写 0.0% 反而像没测到）。
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
  // 标签是用户给机器挂的短文字（最多 8 个，每个最长 16 字），用来把"这台是干嘛的"
  // 一眼标出来。它有两个显示位置：首页卡片底部与设置页的「服务器列表」，
  // 两处的颜色必须是同一个 —— 所以取色只由下面这一个哈希函数决定。

  // 上限与服务端的 store.NormalizeTags 保持一致。前端这一道只是"少一个来回"，
  // 真正生效的是服务端那一道（curl 可以绕过这里）。
  var TAG_MAX_COUNT = 8;
  var TAG_MAX_LEN = 16;

  // 色板大小对应 style.css 里的 .tag-c0 … .tag-c9。
  var TAG_COLOR_COUNT = 10;

  // tagHash 是一个**稳定**的小哈希：同一个字符串永远得到同一个数。
  //
  // 为什么按文字哈希，而不是随机数或"第几个标签"：
  //   - 随机数 / 序号：同一个标签在首页、设置页、两次加载之间会换颜色，
  //     而颜色唯一的用处就是"认出这是哪个标签"——颜色会变就等于没有颜色；
  //   - 文字哈希：重载、换页面、换浏览器看到的都一致，服务端也不必存颜色，
  //     改标签名字自然就换了颜色（名字才是它的身份）。
  // 这里不需要抗碰撞（10 个色板，撞色无所谓），只需要稳定与分布均匀。
  function tagHash(text) {
    var h = 0;
    for (var i = 0; i < text.length; i++) {
      // 31 是 Java String.hashCode 的乘子：够短，对短字符串分布也够均匀。
      // >>> 0 把结果固定成无符号 32 位整数，避免负号让取模出现负下标。
      h = (h * 31 + text.charCodeAt(i)) >>> 0;
    }
    return h;
  }

  // tagClass 返回这个标签的色板类名。
  function tagClass(text) {
    return 'tag-c' + (tagHash(text) % TAG_COLOR_COUNT);
  }

  // tagChip 造一个标签徽章。颜色一律走 tagClass：首页卡片、服务器列表、
  // 编辑标签对话框里的同一个标签因此必然是同一个颜色。
  function tagChip(text) {
    var span = document.createElement('span');
    span.className = 'tag ' + tagClass(text);
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
    // 切视图时收掉迷你条的悬停浮层：首页被 hidden 之后格子还在 DOM 里，
    // 浏览器不会为"祖先被藏起来"补发 mouseout —— 不收的话浮层会一直停在半空中
    // （它是 position:fixed，跟着视口走），点进详情页还能看见上一个页面的读数。
    hideMiniTip();
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
  //   点线引导行（速率 / 在线 / 最后通信 / 费用 / 探测）
  //   延迟 / 丢包迷你条
  //   标签行
  //
  // 「面板延迟」（Agent 到面板自身的 WebSocket 往返）**不在卡片上**：它测的是
  // 隧道往返（走 Cloudflare 恒为 ~100ms），与「探测」那一行的探测结果不是一回事，
  // 摆在同一张卡片上必然被当成同一个数。它仍然在详情页的「网络信息」卡里
  // （见 renderDetailInfo），那里有足够的上下文写清楚它是什么。

  // 四格资源：键（updateCard 按它取引用）→ 标题。数组顺序就是格子顺序，
  // 2×2 网格按行填充 —— 前两个一行（CPU / 内存），后两个一行（硬盘 / 流量）。
  var CARD_RES = [['cpu', 'CPU'], ['mem', '内存'], ['disk', '硬盘'], ['quota', '流量']];

  // 点线引导行：键 → 标题。顺序就是卡片上的顺序。
  // 「费用」在没填价格时整行不显示（见 updateCard），其余四行一直在。
  var CARD_LINES = [
    ['net', '速率'], ['online', '在线'], ['seen', '最后通信'], ['cost', '费用'], ['probe', '探测']
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

    // 「延迟 / 丢包」迷你条：完全复用原有实现（格子、悬停浮层、配色阈值都不动）。
    var mini = createMiniBar();

    // 标签行：默认隐藏，等 updateCard 按节点数据决定显隐（没有标签的卡片
    // 不该留一条空白）。
    var tags = document.createElement('div');
    tags.className = 'card-tags';
    tags.hidden = true;

    root.appendChild(head);
    root.appendChild(res);
    root.appendChild(lines);
    root.appendChild(mini.root);
    // 标签行排在**最后**：上面五行是"这台机器的实时读数"（且每秒都在变），
    // 标签是"这台机器是什么"，属于补充信息，放最后不打断读数的节奏。
    root.appendChild(tags);

    var card = {
      root: root,
      // 上一次画出来的标签（拼成一个字符串比对）。卡片每秒都会被 SSE 重画一次，
      // 而标签是分钟级才变一次的东西 —— 不比对的话，每秒都要把徽章拆了重建。
      tagKey: null,
      // 「探测」那一行同理：它的读数是分钟级的，取整后不变就不重建那些 span。
      probeKey: null,
      refs: {
        name: name, sub: sub, dot: dot, status: status,
        cpu: resRefs.cpu, mem: resRefs.mem, disk: resRefs.disk, quota: resRefs.quota,
        net: lineRefs.net.value, online: lineRefs.online.value, seen: lineRefs.seen.value,
        cost: lineRefs.cost, probe: lineRefs.probe.value,
        tags: tags, mini: mini
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
    // Agent 一次都没连上过 —— 这正是本项目一直在修的那类问题（"没探测到就留浅灰"
    // 是同一条原则）。离线但报过数据的节点照旧显示最后一次读数：状态点已经写明"离线"。
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

    // 「探测」那一行与迷你条**同源**（同一份 /overview 数据，同一张卡片一次渲染），
    // 只是它按探测目标拆开，所以放在迷你条前面一起画。
    renderProbeLine(card, overviewNodes[String(dto.id)]);

    // 迷你条的数据来自 /overview（分钟级），与这里的每秒实时字段不是一个来源。
    // 每帧都重画一次是有意的：卡片可能是刚建出来的（新节点上线），
    // 那时只有这一次机会能把迷你条补上。
    renderMiniBar(card, overviewNodes[String(dto.id)]);

    // 标签同理：卡片可能是刚建出来的（新节点上线），这一帧要能把标签补上
    // （renderCardTags 内部按"变没变"跳过重建，所以每秒调用不会重建 DOM）。
    renderCardTags(card, dto);
  }

  // renderProbeLine 画「探测」那一行：每个**配置过的**探测目标一个当前延迟，
  // 用 · 分隔，按"该目标这一小时的平均值"着色。
  //
  // 阈值与迷你条的格子完全共用（miniLatClass：≤1.2× 绿、≤2× 黄、>2× 红）：
  // 同一份数据、同一套判断，两处颜色因此不可能互相打脸。
  // 顺序就是服务端给的配置顺序（与详情页延迟图的图例一致）。
  //
  // 这里显示的是"最近一段（默认 6 分钟）的平均"而不是某一秒的瞬时值：探测结果
  // 落库是 1 分钟粒度、卡片 60 秒刷新一次，没有更细的数据可取。
  function renderProbeLine(card, mini) {
    var box = card.refs.probe;
    var targets = (mini && mini.targets) || [];
    // 先比"取整后的读数变没变"再决定要不要重建 DOM：卡片每秒都会被 SSE 重画一次，
    // 而探测结果是分钟级的 —— 不比对的话，每个目标的 span 每秒都要拆了重建。
    var key = targets.map(function (t) {
      return t.id + ':' + Math.round(t.lat_ms || 0) + '/' + Math.round(t.avg_ms || 0);
    }).join('|');
    if (key === card.probeKey) return;
    card.probeKey = key;

    box.textContent = '';
    if (targets.length === 0) {
      // 没配探测目标、或这个节点这一小时没有任何探测结果：写 — 而不是留空
      // （空白会被读成"界面没渲染出来"）。
      box.textContent = '—';
      return;
    }
    targets.forEach(function (t, i) {
      if (i > 0) {
        var sep = document.createElement('span');
        sep.className = 'line-sep';
        sep.textContent = ' · ';
        box.appendChild(sep);
      }
      var num = document.createElement('span');
      // 没有数据（lat_ms = 0）或整段没有有效均值时不加颜色类，保持灰色的 —：
      // 没有比较基准就不做判断（同 miniLatClass）。
      var cls = miniLatClass(t.lat_ms, t.avg_ms);
      num.className = cls ? 'line-num ' + cls : 'line-num';
      num.textContent = miniLatText(t.lat_ms);
      // 悬停标题写清是哪个目标：一行里好几个毫秒数，光看数字认不出谁是谁。
      // 名称留空时回落到 host（与详情页的曲线名同一条规则，见 pingTargetLabel）。
      num.title = pingTargetLabel(t);
      box.appendChild(num);
    });
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

  // loadOverview 取一次总览，并把结果同时铺到总览区与各卡片的迷你条上。
  //
  // 失败只忽略（不弹 toast）：总览是一个附加区块，节点卡片与实时流不该
  // 因为它拉不到就停摆；下一次轮询（60 秒后）自然会重试。
  function loadOverview() {
    return api('/api/v1/overview?window=1h&buckets=10').then(function (data) {
      overviewNodes = data.nodes || {};
      // 每格的起始时间与桶宽都从响应里读（见 miniRangeText）：前端不推桶边界。
      overviewBucketTS = data.bucket_ts || [];
      overviewBucketSec = data.bucket_sec || 0;
      renderOverview(data.totals || {});
      // 「探测」那一行与迷你条同源（都来自这次 /overview）也同节奏，一起刷新：
      // 只刷迷你条的话，那些上报间隔很长的节点（比如 60 秒一帧）要等到下一次
      // SSE 推送才会更新探测行，而它其实刚刚就拿到了新数据。
      cards.forEach(function (card, id) {
        renderProbeLine(card, overviewNodes[String(id)]);
        renderMiniBar(card, overviewNodes[String(id)]);
      });
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

  // ---------------------------------------------------------------- 卡片迷你条
  //
  // 每张节点卡片在「流量」那行下面画两块 10 格的迷你条：最近一小时的延迟与丢包，
  // 每格 6 分钟。数据来自 /api/v1/overview（一次请求带回所有节点，
  // 不是每张卡片各查一次）。

  // 悬停浮层的状态。
  //
  // 浮层整页**只有一个**（第一次悬停时才建）：一张卡片 20 格、几十张卡片就是
  // 上千个格子的量级，而同一时刻最多只有一格能被悬停；每格建一个既浪费内存，
  // 又要给每次数据刷新同步一份浮层内容。
  var miniTip = null;        // 浮层元素（挂在 <body> 上，position:fixed）
  var miniTipTime = null;    // 第一行：时间段
  var miniTipValue = null;   // 第二行：数值
  var miniTipCell = null;    // 当前高亮的格子（null = 没有悬停）
  var miniTipRow = null;     // 当前高亮那格所属的行（高亮同时记在行的 hoverIndex 上）

  // 每格的起始时间（Unix 秒）与桶宽：都由 /api/v1/overview 下发。
  // 前端一格都不自己切、也不拿"当前时间 − 窗口 + i×桶宽"去推 —— 桶边界是服务端
  // 按 bucket_sec 对齐后算的（桶号 = (ts-start)/bucketSec），推出来的边界会与真实桶
  // 错开最多一整格，浮层上的时间段就不是那一格数据的实际区间了。
  var overviewBucketTS = [];
  var overviewBucketSec = 0;

  // 浮层与视口边缘之间至少留这么多像素。
  var MINI_TIP_MARGIN = 4;
  // 浮层与格子之间的空隙：贴太紧会压住格子自己的描边，高亮反而看不出来。
  var MINI_TIP_GAP = 8;

  // 两套配色阈值分开写、分开注释：它们回答的是两个不同的问题。
  //
  // 丢包率用**绝对**阈值 —— 丢包有客观含义：0% 就是没丢，(0, 5%] 已经能感觉出来，
  // 超过 5% 就该去查线路了。这个判断与"这条线路本身多快"完全无关。
  var MINI_LOSS_WARN_PCT = 5;
  //
  // 延迟用**相对该节点整小时均值**的倍数 —— 不同线路的基线差很多（香港 20ms 与
  // 美西 180ms 都可能是完全正常的），用绝对毫秒数会把整条线路涂成同一种颜色，
  // 反而看不出"这段时间变差了"，而那才是迷你条要回答的问题。
  // 均值（lat_ms）由后端给；这里只拿它乘一个显示用的常量做分级，
  // 不在前端重新聚合任何原始数据。
  var MINI_LAT_WARN_RATIO = 1.2;  // ≤ 1.2× 绿
  var MINI_LAT_BAD_RATIO = 2;     // ≤ 2× 黄，> 2× 红

  // 丢包格子：0% 绿、(0, 5%] 黄、> 5% 红；没数据（null）留浅灰底。
  function miniLossClass(value) {
    if (typeof value !== 'number') return '';
    if (value > MINI_LOSS_WARN_PCT) return 'bad';
    if (value > 0) return 'warn';
    return 'ok';
  }

  // 延迟格子：≤ 1.2× 绿、≤ 2× 黄、> 2× 红；没数据（null）留浅灰底。
  function miniLatClass(value, avg) {
    if (typeof value !== 'number') return '';
    // 整小时没有有效均值（每一段都全丢）：没有比较基准，不做判断 ——
    // 拿 0 当基准会把所有有值的格子都判成红的。
    if (!(avg > 0)) return '';
    if (value > avg * MINI_LAT_BAD_RATIO) return 'bad';
    if (value > avg * MINI_LAT_WARN_RATIO) return 'warn';
    return 'ok';
  }

  // 0 ms / 0% 都是"没测到"而不是"快得没有延迟"，显示成 — 而不是 0。
  function miniLatText(ms) {
    return ms > 0 ? Math.round(ms) + ' ms' : '—';
  }

  // 丢包读数：一位小数（与延迟的一位小数对称），0% 是实测值。
  function miniLossText(pct) {
    return pct > 0 ? fmtPct1(pct) : '0%';
  }

  // miniRangeText 拼出浮层第一行的 "HH:MM – HH:MM"。
  //
  // 两端的时刻**全部来自后端**：bucket_ts[i] 是这一格的起点，bucket_ts[i+1] 就是
  // 它的终点（接口保证 bucket_ts 严格升序、相邻差正好是一格）；只有最后一格没有
  // "下一格"，才用同样由后端给的 bucket_sec 收尾。这里做的是格式化，不是分桶。
  function miniRangeText(ts, sec, index) {
    var start = ts[index];
    var end = index + 1 < ts.length ? ts[index + 1] : start + (sec > 0 ? sec : 0);
    return clockOf(start) + ' – ' + clockOf(end);
  }

  // miniCellText 把一段的原始值变成浮层第二行的文本。
  //
  // null（后端明确说"这一段没有数据"）一律是「无数据」：写 0 ms / 0% 会被读成
  // "那 6 分钟真的没有延迟、真的没丢包"，比留白更容易误判 —— 与浅灰格子同一条口径。
  function miniCellText(row, index) {
    var value = row.cellValues[index];
    if (typeof value !== 'number' || !isFinite(value)) return '无数据';
    return row.cellTextOf ? row.cellTextOf(value) : String(value);
  }

  // ensureMiniTip 造（或取回）那个共用的浮层。
  //
  // 浮层挂在 <body> 上而不是卡片里：卡片的祖先有圆角与 overflow，放进去会被裁掉；
  // 挂在 body 上配 position:fixed，坐标直接就是视口坐标，夹取逻辑才能简单地拿
  // documentElement 的可见宽高来比较（见 placeMiniTip）。
  function ensureMiniTip() {
    if (miniTip) return miniTip;
    var tip = document.createElement('div');
    tip.className = 'mini-tip';
    // .mini-tip 自己写了 display，会盖掉 hidden 那条 display:none，
    // 所以样式里还有一条 .mini-tip[hidden] { display: none }。
    tip.hidden = true;
    var time = document.createElement('span');
    time.className = 'mini-tip-time';
    var value = document.createElement('b');
    value.className = 'mini-tip-value';
    tip.appendChild(time);
    tip.appendChild(value);
    document.body.appendChild(tip);
    miniTip = tip;
    miniTipTime = time;
    miniTipValue = value;
    return tip;
  }

  // placeMiniTip 把浮层摆到格子上方，并**夹进视口**。
  //
  // 夹取用的是 documentElement.clientWidth/clientHeight（视口里真正可见的那块，
  // 已经扣掉滚动条），所以"右边界 ≤ 可见宽度、下边界 ≤ 可见高度"是硬保证：
  // 最右一列卡片、贴着屏幕底部的一行都不会把浮层推出屏幕外。
  //   水平：默认以格子中线对齐；右边放不下就左移到"可见宽度 − 浮层宽 − 边距"，
  //         左边同理兜到边距（窄视口里浮层比视口还宽时，优先保住左边界）。
  //   垂直：默认贴在格子上方；上方放不下（第一行卡片）就翻到格子下方，
  //         翻下去仍然超出底边时再上移到"可见高度 − 浮层高 − 边距" ——
  //         这时浮层可能压住格子，但绝不越出视口。
  //
  // 位置一律由**格子的矩形**算出，不用 clientX/clientY：格子只有 9px 高，
  // 跟着鼠标纵向走会让浮层在格子里上下抖（快速划过时看起来就是闪烁）。
  function placeMiniTip(cell) {
    var tip = ensureMiniTip();
    var rect = cell.getBoundingClientRect();
    var vw = document.documentElement.clientWidth;
    var vh = document.documentElement.clientHeight;
    var w = tip.offsetWidth;
    var h = tip.offsetHeight;

    var left = rect.left + rect.width / 2 - w / 2;
    var top = rect.top - h - MINI_TIP_GAP;
    if (top < MINI_TIP_MARGIN) top = rect.bottom + MINI_TIP_GAP;

    var maxLeft = vw - w - MINI_TIP_MARGIN;
    var maxTop = vh - h - MINI_TIP_MARGIN;
    if (left > maxLeft) left = maxLeft;
    if (left < MINI_TIP_MARGIN) left = MINI_TIP_MARGIN;
    if (top > maxTop) top = maxTop;
    if (top < MINI_TIP_MARGIN) top = MINI_TIP_MARGIN;

    tip.style.left = left + 'px';
    tip.style.top = top + 'px';
  }

  // showMiniTip 显示浮层：第一行时间段、第二行这一段的值，同时高亮这一格。
  function showMiniTip(row, index, cell) {
    if (index >= overviewBucketTS.length) return;
    var tip = ensureMiniTip();
    miniTipTime.textContent = miniRangeText(overviewBucketTS, overviewBucketSec, index);
    miniTipValue.textContent = miniCellText(row, index);
    setMiniHighlight(row, index, cell);
    tip.hidden = false;
    // 先显示再量尺寸：hidden 的元素 offsetWidth 恒为 0，夹取会算错。
    placeMiniTip(cell);
  }

  // setMiniHighlight 把高亮从上一格挪到这一格。
  //
  // 高亮同时记在行的 hoverIndex 上，而不是只加一个类名到 DOM：卡片每秒都会被 SSE
  // 重画一次，renderMiniRow 会整体重写 className —— 只加 DOM 类的话，鼠标停在
  // 格子上不动时高亮会每秒闪一下。
  function setMiniHighlight(row, index, cell) {
    if (miniTipCell && miniTipCell !== cell) miniTipCell.classList.remove('hover');
    if (miniTipRow && miniTipRow !== row) miniTipRow.hoverIndex = -1;
    miniTipRow = row;
    miniTipCell = cell;
    row.hoverIndex = index;
    cell.classList.add('hover');
  }

  // hideMiniTip 收起浮层并清掉高亮（鼠标移出格子、数据重画、退出登录时调用）。
  function hideMiniTip() {
    if (miniTipCell) {
      miniTipCell.classList.remove('hover');
      miniTipCell = null;
    }
    if (miniTipRow) {
      miniTipRow.hoverIndex = -1;
      miniTipRow = null;
    }
    if (miniTip) miniTip.hidden = true;
  }

  // bindMiniCell 给一个格子绑悬停事件（只在格子被创建时绑一次，不随数据刷新叠加）。
  //
  // 用 mouseover/mouseout 这一对：mouseenter/mouseleave 不冒泡、也没法由脚本的
  // dispatchEvent 合成，而格子本身就是最内层元素，两者在行为上没有区别 ——
  // 选能在浏览器验证里走同一条路径的那一对。
  function bindMiniCell(row, cell, index) {
    cell.addEventListener('mouseover', function () { showMiniTip(row, index, cell); });
    // 同一格内移动也重算一次位置：视口可能刚被滚动或缩放，重算的成本只有一次
    // getBoundingClientRect，比"浮层停在旧位置"划算。
    cell.addEventListener('mousemove', function () { showMiniTip(row, index, cell); });
    cell.addEventListener('mouseout', hideMiniTip);
  }

  // miniRow 造一行：标题 + 数值（一行），下面一条格子。
  // 格子在第一次拿到数据时按数组长度铺（长度由后端定，见 renderMiniRow）。
  function miniRow(title) {
    var row = document.createElement('div');
    row.className = 'mini-row';

    var head = document.createElement('div');
    head.className = 'mini-head';
    var label = document.createElement('span');
    label.className = 'mini-label';
    label.textContent = title;
    var value = document.createElement('b');
    value.className = 'mini-value';
    value.textContent = '—';
    head.appendChild(label);
    head.appendChild(value);

    var cells = document.createElement('div');
    cells.className = 'mini-cells';

    row.appendChild(head);
    row.appendChild(cells);
    return {
      row: row, value: value, cells: cells, cellNodes: [],
      // 每格的原始值（后端数组的引用）与格式化函数：悬停时按需格式化，
      // 不是每次刷新都拼 10 个字符串。
      cellValues: [], cellTextOf: null,
      // 鼠标停在第几格（-1 = 没有）。每秒重画时要靠它把高亮类补回来。
      hoverIndex: -1
    };
  }

  // createMiniBar 造整块迷你条（延迟一行、丢包一行）。
  //
  // 默认 hidden：这块要不要显示取决于"有没有配探测目标、这个节点有没有数据"，
  // 而那要等 /overview 回来才知道 —— 先摆一个空框出来，就成了"探针坏了"的观感。
  function createMiniBar() {
    var root = document.createElement('div');
    root.className = 'card-mini';
    root.hidden = true;
    var lat = miniRow('延迟');
    var loss = miniRow('丢包');
    root.appendChild(lat.row);
    root.appendChild(loss.row);
    return { root: root, lat: lat, loss: loss };
  }

  // renderMiniRow 更新一行：数字 + 每个格子的颜色。
  //
  // 格子数按 values 的长度铺（后端默认给 10 段）。为什么按长度而不是写死 10：
  // 段数是接口参数，后端改了 buckets 前端要能自动跟上 —— 写死 10 的话
  // 多出来的段会被静默丢掉，看起来"数据少了一段"。
  //
  // cellText 把一段的原始值格式化成悬停浮层第二行的文本（延迟一位小数、丢包带 %），
  // 每一行各自的写法不同，所以由调用方传进来。
  function renderMiniRow(ref, values, text, classify, cellText) {
    ref.value.textContent = text;
    var list = values || [];
    if (ref.cellNodes.length !== list.length) {
      // 格子的数量变了：旧格子连同它们的悬停状态一起作废（浮层可能正指着其中一格）。
      if (miniTipRow === ref) hideMiniTip();
      ref.cells.textContent = '';
      ref.cellNodes = [];
      for (var i = 0; i < list.length; i++) {
        var cell = document.createElement('span');
        cell.className = 'mini-cell';
        // 事件在格子创建时绑一次：格子是复用的（长度不变就不重建），
        // 放在这里就不会每秒叠一层监听。
        bindMiniCell(ref, cell, i);
        ref.cells.appendChild(cell);
        ref.cellNodes.push(cell);
      }
    }
    ref.cellValues = list;
    ref.cellTextOf = cellText;
    for (var j = 0; j < list.length; j++) {
      var cls = classify(list[j]);
      // 没有数据的那一段保持 .mini-cell 的浅灰底色（不加颜色类）：
      // 画成 0 会让人以为"那 6 分钟延迟为零"，比留白更容易误判。
      //
      // 悬停高亮（hover）必须在这里补回来：卡片每秒都被 SSE 重画一次，
      // 只往 DOM 上加类的话，鼠标停在格子上不动时高亮会每秒闪一下。
      ref.cellNodes[j].className = 'mini-cell' + (cls ? ' ' + cls : '') +
        (ref.hoverIndex === j ? ' hover' : '');
    }
    // 数字每秒刷新一次：鼠标停着不动时浮层里的值也要跟着走，
    // 否则它会一直显示悬停那一刻的旧值（看起来像"卡住了"）。
    if (ref.hoverIndex >= 0 && miniTipRow === ref && miniTipValue) {
      miniTipValue.textContent = miniCellText(ref, ref.hoverIndex);
    }
  }

  // renderMiniBar 画一张卡片的迷你条。
  //
  // mini 为空（这个节点这一小时一个探测结果都没有，或整个集群都没配探测目标）
  // 时整块藏起来，而不是画 20 个灰格子：一片灰看起来像探针坏了，
  // 而实际只是这台机器没参与探测。
  function renderMiniBar(card, mini) {
    var bar = card.refs.mini;
    if (!bar) return;
    if (!mini) {
      bar.root.hidden = true;
      // 整块藏起来时浮层要跟着收：鼠标停在格子上时数据刷新、这一段变成"没有数据"，
      // 格子会消失，而 mouseout 不一定还会派发（元素已经不可见）。
      if (miniTipRow === bar.lat || miniTipRow === bar.loss) hideMiniTip();
      return;
    }
    bar.root.hidden = false;
    renderMiniRow(bar.lat, mini.lat, miniLatText(mini.lat_ms), function (value) {
      return miniLatClass(value, mini.lat_ms);
    }, function (value) {
      // 一位小数：与卡片脚注、详情页的「面板延迟」写法一致。
      return value.toFixed(1) + ' ms';
    });
    // 丢包的格子沿用行首那份写法（0% 是实测值，写成 0.0% 反而像没测到）。
    renderMiniRow(bar.loss, mini.loss, miniLossText(mini.loss_pct), miniLossClass, miniLossText);
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
    // 浮层与高亮先收掉：卡片马上要被移除，鼠标停过的那一格会跟着消失，
    // 而 mouseout 对一个已经不在文档里的元素不会再来。
    hideMiniTip();
    // 总览是"上一位登录者那一屏"的数据：不清掉的话，下一位登录进来、
    // 新的 /overview 还没回来的那一瞬会看到别人的集群合计。
    overviewNodes = {};
    overviewBucketTS = [];
    overviewBucketSec = 0;
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
  // 第 4 位（丢包率，只有延迟图的点有）也一起带过去：合并后的桶取这几个桶的
  // **平均**。前端手里没有"每个桶探测了多少次"（服务端算桶丢包率时用的权重），
  // 取平均既不会像取最大值那样把偶发的一次丢包说成整段都在丢，也不会像取
  // 最小值那样把它抹掉 —— 而"这里丢过包"正是竖条要传达的信息。
  function aggregate(points, targetSec) {
    if (!targetSec || targetSec <= 0 || points.length === 0) return points;
    var out = [];
    var cur = null;
    points.forEach(function (p) {
      var bucket = Math.floor(p[0] / targetSec) * targetSec;
      if (!cur || cur[0] !== bucket) {
        // [桶起点, 值累加, 峰值, 点数, 丢包累加, 有丢包的点数]
        cur = [bucket, 0, 0, 0, 0, 0];
        out.push(cur);
      }
      cur[1] += p[1];
      cur[2] = Math.max(cur[2], p[2]);
      cur[3] += 1;
      if (typeof p[3] === 'number' && isFinite(p[3])) {
        cur[4] += p[3];
        cur[5] += 1;
      }
    });
    return out.map(function (c) {
      var merged = [c[0], c[1] / c[3], c[2]];
      if (c[5] > 0) merged.push(c[4] / c[5]);
      return merged;
    });
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
    infoRow(tra, '今日流量', '↓ ' + fmtBytesDec(node.traffic_today_rx) + '  ↑ ' + fmtBytesDec(node.traffic_today_tx));
    infoRow(tra, '本周期流量', trafficCycleText(node));
    infoRow(tra, '历史累计流量', '↓ ' + fmtBytesDec(node.traffic_total_rx) + '  ↑ ' + fmtBytesDec(node.traffic_total_tx));
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

  // chartCards 返回装了图表的**两张卡片**（「资源与网络」与「延迟」，见 index.html）。
  //
  // 用 el 上的键现取，而不是把元素缓存在模块顶部的数组里：el 是 main() 启动时
  // 从 DOM 自动登记的（那时才存在），在这里现取既拿得到最新引用，也让"键必须
  // 能由 id 推导"这条不变量继续由测试盯着。
  function chartCards() {
    return [el.chartsResources, el.chartsLatency].filter(function (node) { return !!node; });
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
  // 两张卡片**各自**判断：资源/网络/流量的五张全被取消勾选时只收起卡片 A，
  // 延迟图还在的话卡片 B 照常显示 —— 反过来也一样。两边都收起来时页面上
  // 不会留下任何空块（只有标题的空边框看着像加载失败）。
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

  // trafficCycleText 描述本周期用量；设了额度时带上额度与百分比。
  // 流量一律 1000 进制（GB/TB），与「月流量额度（GB）」输入框的口径一致。
  function trafficCycleText(node) {
    var used = (node.traffic_cycle_rx || 0) + (node.traffic_cycle_tx || 0);
    var text = '↓ ' + fmtBytesDec(node.traffic_cycle_rx) + '  ↑ ' + fmtBytesDec(node.traffic_cycle_tx)
      + '（共 ' + fmtBytesDec(used) + '）';
    if (node.traffic_limit > 0) {
      text += ' / ' + fmtBytesDec(node.traffic_limit) + '（' + (node.traffic_pct || 0).toFixed(1) + '%）';
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

      // 流量图的 Y 轴是字节（每天的量）：轴自带单位（GB/TB），所以 unit 留空。
      var pctOpts = { yMax: 100, unit: '%', yFormat: function (v) { return v.toFixed(0) + '%'; }, tickLabelSec: tick, xFormat: xFormat, showMax: true };
      // 速率图的 Y 轴是"每秒多少字节"：yFormat 直接给 fmtRate（KB/s、MB/s，
      // 1000 进制），单位已经写在刻度里，unit 必须留空 —— 否则读数会变成 "MB/s/s"。
      var rateOpts = { yMax: 0, unit: '', yFormat: fmtRate, tickLabelSec: tick, xFormat: xFormat, showMax: true };

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
        // bars 原样透传（只有延迟图会带）：这里一旦漏掉，丢包竖条就画不出来，
        // 而且看不出哪里错了 —— 数据、图例、勾选框全都是对的。
        bars: s.bars,
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

  // latTargetText 是勾选框上的文案：名称（+ 整段平均延迟 / 丢包率）。
  //
  // 区间聚合值挂在图例上，是因为它们没有别的去处：整段丢了多少、平均多少毫秒
  // 是"这个目标靠不靠谱"的第一手结论，而曲线只讲"什么时候慢"。两个后缀都按
  // "没有就不写"处理 —— 探针绝大多数时间既不丢包、延迟也正常，给每个目标都
  // 挂一句「丢包 0%」会把真正丢包的那个目标淹掉。
  //
  // 平均延迟由后端给（/ping 的 avg_ms，按成功探测次数加权）：前端手里只有画曲线用的
  // 分桶点，自己平均一遍就等于把服务端的加权规则再实现一次，两处口径迟早分叉。
  function latTargetText(t) {
    var text = pingTargetLabel(t);
    if (!t.has_data) return text + '（暂无数据）';
    // 0 表示没有有效的延迟样本（整段全丢），此时不写延迟后缀。
    if (t.avg_ms > 0) text += ' · ' + Math.round(t.avg_ms) + ' ms';
    if (t.loss_pct > 0) text += ' · 丢包 ' + fmtPct(t.loss_pct);
    return text;
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
      text.textContent = latTargetText(t);
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
  // [ts, avg, max, loss] 里前两个画曲线；max 交给图表的峰值淡线）。
  //
  // 第 4 位（该桶丢包率）走 series.bars：从绘图区底边往上画一条半透明的竖条。
  // 丢包是稀疏事件，画成第二条曲线的话 1% 与 0% 在图上几乎重合。
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
          points: seriesFor(points),
          // valueIndex 指向点里的第 4 位（丢包率），max=100 表示满格。
          // 颜色不传：图表默认用该 series 自己的线色（半透明填充）。
          bars: { valueIndex: 3, max: 100 }
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
      el.nodeError.textContent = err.message;
    }).then(function () {
      el.nodeSubmit.disabled = false;
    });
  }

  // refreshNodeViews 把"所有显示节点的地方"刷新一遍：首页卡片（loadNodes）
  // 与设置页的「服务器列表」（它自己有一份快照）。新增/编辑/改标签之后都要调，
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

  // 栏名清单同时是导航与内容的顺序来源（HTML 里 8 个 data-pane 必须与它一致）。
  // 顺序即左栏从上到下的顺序：延迟探测排在仪表盘之后（都是"画什么"的设置），
  // 服务器列表排在延迟探测之后（都是"有哪些机器"，紧挨着看）。
  var SETTINGS_PANES = ['notify', 'alert', 'dashboard', 'ping', 'nodes', 'security', 'server', 'audit'];

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
    if (pane === 'ping') return el.pingError;
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

  // ---------------------------------------------------------------- 服务器列表（设置栏）
  //
  // 一行一台机器：状态点 + 名称 + 地区徽章 + 「编辑标签」「编辑节点」，
  // 下面一行是**从已有字段自动拼出来**的信息（IP · 分组 · 剩余价值 · 到期天数），
  // 再下面是标签行。这一栏不新增任何输入项：要看什么都在节点数据里。

  // settingsNodes 是这一栏自己的一份快照。
  //
  // 为什么不直接用首页那份 nodes：这一栏可以被**直接深链**打开
  // （#/settings/nodes），那时首页还从来没加载过，nodes 是空的 ——
  // 表现是"服务器列表永远空着，只有先去一趟首页才正常"。
  var settingsNodes = [];

  // rowButton 造行尾的小按钮（两个按钮长得一样，只有回调不同）。
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
  // 结构是「左侧内容 + 右侧操作」两列（见 style.css 的 .node-item 网格）：
  // 两个按钮必须和**整行**垂直居中，而不是跟名称挤在第一行 —— 机器信息有三行，
  // 按钮跟在第一行里会随着内容变长越来越"飘在顶上"。
  function settingsNodeRow(node) {
    var row = document.createElement('div');
    row.className = 'node-item';

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

    // 右侧操作列：只要这两个按钮：删除、换 Token 之类都在详情页里，这一栏是"看与轻改"。
    var acts = document.createElement('div');
    acts.className = 'node-item-acts';
    acts.appendChild(rowButton('编辑标签', function () { openTagDialog(node); }));
    acts.appendChild(rowButton('编辑节点', function () { openNodeDialog('edit', node); }));
    row.appendChild(acts);

    return row;
  }

  // ---------------------------------------------------------------- 编辑标签对话框

  var tagDraft = [];    // 正在编辑的标签（保存前只在这里改）
  var tagNode = null;   // 打开对话框时那台机器的 DTO（保存时要带全它的字段）

  // openTagDialog 打开「编辑标签」：显示当前标签 + 别的机器用过的标签。
  function openTagDialog(node) {
    tagNode = node;
    // 复制一份再改：直接改 node.tags 会把首页卡片手里的同一个数组一起改掉，
    // 点「取消」就恢复不回来了。
    tagDraft = (node.tags || []).slice();
    el.tagsNode.textContent = node.name;
    el.tagsError.textContent = '';
    el.tagsInput.value = '';
    renderTagEditor();
    renderTagSuggestions();
    el.dlgTags.showModal();
  }

  // renderTagEditor 重建编辑器里的徽章。
  //
  // 徽章一律插在输入框**前面**（insertBefore）：输入框始终是最后一个子节点，
  // 加完一个标签光标还停在原处，可以接着打下一個。
  function renderTagEditor() {
    Array.prototype.forEach.call(el.tagsEditor.querySelectorAll('.tag'), function (chip) {
      chip.remove();
    });
    tagDraft.forEach(function (tag) {
      var chip = tagChip(tag);
      var remove = document.createElement('button');
      remove.type = 'button';
      remove.className = 'tag-x';
      remove.textContent = '×';
      remove.title = '删除标签 ' + tag;
      remove.addEventListener('click', function () { removeTag(tag); });
      chip.appendChild(remove);
      el.tagsEditor.insertBefore(chip, el.tagsInput);
    });
    syncTagEditor();
  }

  // syncTagEditor 更新提示，并按上限决定输入框能不能用。
  //
  // 到上限就禁用输入框（而不是让他打完再报错）：这是"再打也没用"的状态，
  // 禁用它同时也是一个提示 —— 文案里写明要先删一个。
  function syncTagEditor() {
    var full = tagDraft.length >= TAG_MAX_COUNT;
    el.tagsInput.disabled = full;
    el.tagsHint.textContent = full
      ? '已达上限（最多 ' + TAG_MAX_COUNT + ' 个标签），要再加请先删掉一个'
      : '最多 ' + TAG_MAX_COUNT + ' 个，每个最长 ' + TAG_MAX_LEN + ' 字';
  }

  // addTag 把一段文字加成标签；返回是否加进去了（加进去才清空输入框）。
  //
  // 校验顺序与服务端一致：去空白 → 空串丢弃 → 长度 → 重复 → 数量。
  // 这里拦一道只是省一个来回，服务端那道才是真正生效的。
  function addTag(raw) {
    var tag = String(raw || '').trim();
    if (!tag) return false;
    if (tagRuneLen(tag) > TAG_MAX_LEN) {
      el.tagsError.textContent = '标签「' + tag + '」有 ' + tagRuneLen(tag) +
        ' 个字符，超过 ' + TAG_MAX_LEN + ' 字上限';
      return false;
    }
    if (tagDraft.indexOf(tag) >= 0) {
      el.tagsError.textContent = '「' + tag + '」已经加过了';
      return false;
    }
    if (tagDraft.length >= TAG_MAX_COUNT) {
      el.tagsError.textContent = '最多 ' + TAG_MAX_COUNT + ' 个标签，要再加请先删掉一个';
      syncTagEditor();
      return false;
    }
    tagDraft.push(tag);
    el.tagsError.textContent = '';
    renderTagEditor();
    renderTagSuggestions();
    return true;
  }

  function removeTag(tag) {
    tagDraft = tagDraft.filter(function (item) { return item !== tag; });
    el.tagsError.textContent = '';
    renderTagEditor();
    renderTagSuggestions();
  }

  // renderTagSuggestions 列出"别的机器用过的标签"，点一下就能加进来。
  //
  // 为什么要有这一块：同一批标签（探针 / 搜索 / 中转）会在多台机器上重复出现，
  // 手打就一定会打出「探针」和「探针 」这种看着一样、哈希却不同的两个标签 ——
  // 那样它们的颜色也会不一样。
  //
  // 候选 = 全部节点用过的标签 − 这台机器已有的。一个候选都没有（整个集群还没用过
  // 标签，或者全被这台机器占着）时整块（含标题）不显示。
  function renderTagSuggestions() {
    var used = {};
    settingsNodes.forEach(function (node) {
      (node.tags || []).forEach(function (tag) { used[tag] = true; });
    });
    tagDraft.forEach(function (tag) { delete used[tag]; });
    var list = Object.keys(used);

    el.tagsExisting.textContent = '';
    el.tagsExistingWrap.hidden = list.length === 0;
    list.forEach(function (tag) {
      // 徽章本身就是按钮：底色/字色仍然由 .tag-cN 决定，与别处完全同色。
      var btn = document.createElement('button');
      btn.type = 'button';
      btn.className = 'tag ' + tagClass(tag);
      btn.textContent = tag;
      btn.addEventListener('click', function () { addTag(tag); });
      el.tagsExisting.appendChild(btn);
    });
  }

  // tagFormPayload 拼出保存标签的请求体：**带上这台机器的全部字段**。
  //
  // 节点更新接口是"整体替换"语义（与详情页「编辑」用的是同一个处理函数），
  // 只发 tags 会把名称、分组、价格一起冲成空值，服务端会直接 400。
  // 所以这里把 DTO 里的字段原样抄回去，一个都不做转换：金额在库里就是"分"，
  // 与请求体的约定一致（见 dto.go 的 nodeDTO）。
  function tagFormPayload() {
    return {
      name: tagNode.name,
      group_name: tagNode.group_name || '',
      region: tagNode.region || '',
      note: tagNode.note || '',
      interval_sec: tagNode.interval_sec || 1,
      traffic_limit: tagNode.traffic_limit || 0,
      traffic_warn_pct: tagNode.traffic_warn_pct || 80,
      reset_day: tagNode.reset_day || 1,
      expires_at: tagNode.expires_at || 0,
      price_cents: tagNode.price_cents || 0,
      currency: tagNode.currency || '',
      billing_months: tagNode.billing_months || 0,
      enabled: tagNode.enabled !== false,
      tags: tagDraft.slice()
    };
  }

  function saveTags() {
    if (!tagNode) return;
    el.tagsError.textContent = '';
    el.tagsSave.disabled = true;

    api('/api/v1/nodes/' + tagNode.id, { method: 'PUT', body: tagFormPayload() }).then(function () {
      el.dlgTags.close();
      toast('标签已保存');
      // 首页卡片与设置页的服务器列表一起刷新（不等下一次 SSE）。
      return refreshNodeViews();
    }).catch(function (err) {
      el.tagsError.textContent = err.message;
    }).then(function () {
      el.tagsSave.disabled = false;
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

    // 浮层用的是 fixed 定位（视口坐标），页面一滚动或窗口一改大小，格子就不在原处了，
    // 而这两种情况下浏览器不会给格子派发 mouseout —— 不收起来浮层会停在半空中指着空气。
    // 用捕获阶段：设置页里那些自己可滚动的容器（如操作记录表）滚动时也要收。
    window.addEventListener('scroll', hideMiniTip, true);
    window.addEventListener('resize', hideMiniTip);

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

    // 服务器列表：添加节点复用顶栏那套流程（同一个对话框、同一份校验）。
    el.nodesAdd.addEventListener('click', function () {
      openNodeDialog('create', null);
    });

    // 编辑标签：回车加一个、点 × 删一个、点候选徽章加进去。
    el.tagsInput.addEventListener('keydown', function (event) {
      // 只认回车。对话框里没有 <form>，但回车在有的浏览器里会触发默认按钮，
      // 所以照样 preventDefault，免得"加标签"顺手把对话框存了。
      if (event.key !== 'Enter') return;
      event.preventDefault();
      if (addTag(el.tagsInput.value)) el.tagsInput.value = '';
    });
    el.tagsCancel.addEventListener('click', function () {
      // 取消不写库：草稿只存在于 tagDraft 里，关掉即作废（原数据一个字节没动）。
      el.dlgTags.close();
    });
    el.tagsSave.addEventListener('click', saveTags);
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
