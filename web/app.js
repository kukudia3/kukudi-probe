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
    // 时间档位有**两份**，因为两张图表卡各有一组按钮、互相独立：
    //   range     ——「资源与网络」卡那一组（#detail-ranges），控制 CPU/内存/磁盘/网络；
    //   pingRange ——「延迟」卡那一组（#lat-ranges），只控制延迟图。
    // 分开之后"资源图看 1 天、延迟图看 1 小时"可以同时成立。以前只有一个 range，
    // 两组按钮共用一个状态，切哪一边都会把两边一起重拉 —— 这正是本次要拆掉的。
    // 默认值两边相同（1h）：第一次打开时两张卡看起来一致，不会让人以为延迟图没跟着切。
    range: '1h',
    pingRange: '1h',
    charts: new Map(),   // key -> chart 实例
    timer: null,
    // 延迟图的探测目标列表：进入详情页时随节点详情一起取一次。
    // null = 还没拿到（这时不请求 /ping），[] = 确实一个都没配（显示空态）。
    pingTargets: null,
    pingSeries: [],      // 上一次 /ping 画出来的全部曲线（隐藏过滤前）
    // 延迟图的桶宽（秒），来自 /ping 响应的 meta.bucket_sec。
    // 断线要用它（两点间隔超过 1.5 个桶宽就说明中间那些桶根本不存在），
    // 而**不能**拿 /series 的 bucket_sec：同一个档位下两者桶宽不同（1h 是 60 与 10），
    // 拿错了会把一条正常的曲线切得一段一段。
    pingBucketSec: 0,
    // 延迟图的 X 轴**基准**间隔（秒），来自 /ping 响应的 meta.tick_base_sec。
    //
    // 为什么延迟图不跟资源图共用 /nodes/{id} 里那份刻度：两者是两个接口、两张
    // 档位表（桶宽不同）。刻度按**档位**定、两表的值本来就该一样，但"该读谁的就
    // 读谁的"才不会在某一处改了另一处没改时画出错位的刻度（0 表示还没拿到，
    // 那时退回 ranges 里同档位的基准间隔）。
    pingTickBaseSec: 0,
    // 探测间隔（秒），来自 /api/v1/settings 的 ping.interval_sec（也可在延迟探测
    // 设置里改）。断线判据要用它：**探测间隔 > 桶宽**时，桶里只有一部分有行，
    // 相邻两点的实际间距是探测间隔而不是桶宽（见 latBucketSec）。
    pingIntervalSec: 0
  };
  var DETAIL_REFRESH_MS = 30000;
  // 手机端（窄屏）用服务端给的二次聚合目标，PC 不聚合。
  var MOBILE_QUERY = '(max-width: 640px)';

  // 延迟图的曲线颜色：沿用项目既有的那几个色（与网络图/流量图同一套取色），
  // 按目标顺序循环取 —— 颜色只是"哪条线是哪条"，不引入新的 CSS 变量。
  var PING_COLORS = ['#2563eb', '#16a34a', '#7c3aed', '#0891b2', '#d97706', '#dc2626'];

  // 这里曾经有一个 SLOW_COLOR（'#ef4444'）—— 延迟图上"超过阈值的那一段"用它画红线，
  // 图例里的「· 慢 X%」也用它上色。用户明确不要"慢"这个概念了：红线、阈值、慢占比、
  // 以及这个常量一起删干净（后端的 baseline_ms / threshold_ms / slow_pct 也没了）。
  // 延迟图上现在只有一条平均线与底部的丢包竖条：峰值淡线已经不再画（用户要求），
  // 峰值本身只在悬浮读数与目标卡片的那行统计里出现。

  // 延迟图上"隐藏了哪些目标"存 localStorage：这是"本浏览器想看哪几条线"的偏好，
  // 与服务端的探测目标配置无关，所以不进服务端（主题切换也是同样的做法）。
  var PING_HIDDEN_KEY = 'probe-ping-hidden';

  // 延迟图那三个开关（延迟 / 丢包 / 平滑曲线）存 localStorage。
  //
  // 键名带 -v1：以后改这三个开关的结构（加一个、把布尔改成三态）就换成 -v2，
  // 老浏览器里存着的旧结构不会被读成一个"字段对不上"的畸形对象 ——
  // 那类错最难查：开关点了像是没反应，而控制台一声不吭。
  // 这里**不换**版本号：删掉「峰值线」之后老结构里多出的 peak 键会被安全忽略
  // （见 LAT_VIEW_DEFAULT 的说明），换了反而把用户另外三个开关的选择一起清掉。
  var PING_VIEW_KEY = 'probe-ping-view-v1';

  // 要显示哪些图表。null = 还没从服务端拿到，此时先按"全部显示"（不能因为一次
  // 设置接口迟半步就让首屏少画几张图）；拿到之后就是一个可能为空的键数组。
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

  // ---------------------------------------------------------------- 时区渲染层
  //
  // 页面上**所有**"把 epoch 渲染成人类可读时间"的地方都走这一层，
  // 而且一律按**服务端时区**（--timezone 下发给前端的那个 IANA 名）渲染。
  //
  // 为什么不按浏览器本地时区：后端的切天口径**全部**是服务端时区 ——
  // 「今日 / 本周 / 计费周期」由 s.loc 算出来（store.CycleStart / WeekStart）、
  // 「近 7 天流量」的日轴是服务端本地零点（api_traffic.go 的 startDay）、
  // 审计日志与 SSE 的 ts 也都是服务端进程的时间。切天用服务端时区、
  // 渲染却用浏览器时区的话，同一个时刻在页面上就是另一个日期：
  // 服务端 UTC+8、浏览器 UTC+0 时，服务端记在 10-02 这一天的流量会被画在
  // 10-01 16:00 的位置上，看起来就是"日轴差了一天"。统一到服务端时区之后，
  // "页面上写的日期"与"后端切天的依据"必然是同一个 —— 这也正是运维看面板时的
  // 期望：面板上的时间要能直接跟服务器上的 `date`、日志、SSH 对上。
  //
  // 拿不到 timezone（老服务端没下发、字段缺失、或这个名字当前浏览器不认识）时
  // **优雅退回浏览器本地**：那正是这次改动之前的行为，页面照常可用，不会白屏。
  var serverTZ = { name: '', fmt: false };

  // TZ_SPEC 一次取全（年月日时分秒），于是**一个时区只需要一个
  // Intl.DateTimeFormat 实例** —— 构造它很贵，而这些函数在每帧绘制路径上
  // （图表悬浮时每次 mousemove 都会重画）。缓存键就是时区名，所有格式共用它。
  var TZ_SPEC = {
    year: 'numeric', month: '2-digit', day: '2-digit',
    hour: '2-digit', minute: '2-digit', second: '2-digit',
    // hourCycle 显式写 h23：hour12:false 在个别实现/语言下会给出 "24:00"，
    // 那会把 00:15 写成 "24:15"，日期也跟着错一天。
    hourCycle: 'h23'
  };

  function pad2(n) { return (n < 10 ? '0' : '') + n; }

  // tzFormatter 取当前时区的格式化器。
  //   false = 还没建；null = 建不出来（名字非法 / 没有 Intl）；对象 = 可用。
  // 建不出来也缓存下来：否则每次格式化都要重新构造并抛一次异常。
  function tzFormatter() {
    if (serverTZ.fmt !== false) return serverTZ.fmt;
    var fmt = null;
    if (serverTZ.name) {
      try {
        fmt = new Intl.DateTimeFormat('en-US', {
          timeZone: serverTZ.name,
          year: TZ_SPEC.year, month: TZ_SPEC.month, day: TZ_SPEC.day,
          hour: TZ_SPEC.hour, minute: TZ_SPEC.minute, second: TZ_SPEC.second,
          hourCycle: TZ_SPEC.hourCycle
        });
      } catch (e) {
        fmt = null;
      }
    }
    serverTZ.fmt = fmt;
    return fmt;
  }

  // localFields 是"拿不到服务端时区"时的退路：浏览器本地时区的各字段。
  // 字段名与 tzFields 完全一致，所以调用点不需要写两套分支。
  function localFields(d) {
    return {
      year: String(d.getFullYear()),
      month: pad2(d.getMonth() + 1),
      day: pad2(d.getDate()),
      hour: pad2(d.getHours()),
      minute: pad2(d.getMinutes()),
      second: pad2(d.getSeconds())
    };
  }

  // tzFields 把 epoch（秒）拆成"服务端时区下的年月日时分秒"，全是补零后的字符串。
  function tzFields(unixSec) {
    var d = new Date(unixSec * 1000);
    var fmt = tzFormatter();
    if (!fmt) return localFields(d);
    var parts = fmt.formatToParts(d);
    var out = {};
    for (var i = 0; i < parts.length; i++) {
      var t = parts[i].type;
      if (t === 'year' || t === 'month' || t === 'day' ||
          t === 'hour' || t === 'minute' || t === 'second') {
        out[t] = parts[i].value;
      }
    }
    // h23 之下不该出现 24；真出现了就按 00 处理（否则日期会跟着错一天）。
    if (out.hour === '24') out.hour = '00';
    return out;
  }

  function fmtClock(unixSec) {
    if (!unixSec) return '—';
    var f = tzFields(unixSec);
    return f.hour + ':' + f.minute + ':' + f.second;
  }

  // fmtTime 给操作记录用：审计是跨天翻的，只给时分秒分不清是不是今天。
  //
  // 之前这张表调的是一个根本不存在的 fmtTime()，ReferenceError 被 loadAudit 的
  // catch 吞成一句 toast —— 表现是"表格永远空的"，控制台之外看不出哪里错了。
  //
  // 时间按**服务端时区**渲染（理由见上面那一段）：审计是拿去跟服务端日志、SSH 里
  // 的 `date` 对时间的，按浏览器时区渲染的话，两端时区不一致时怎么也对不上。
  function fmtTime(unixSec) {
    if (!unixSec) return '—';
    var f = tzFields(unixSec);
    return f.month + '-' + f.day + ' ' + f.hour + ':' + f.minute + ':' + f.second;
  }

  // setServerTimezone 记下服务端下发的 IANA 时区名（三个接口都带这个字段：
  // /api/v1/nodes 与 /api/v1/nodes/{id} 的 server.timezone、/api/v1/settings 的
  // server.timezone）。值没变时什么都不做 —— 它每次取数都会被调到。
  function setServerTimezone(raw) {
    var name = String(raw == null ? '' : raw).trim();
    if (name === serverTZ.name) return;
    serverTZ.name = name;
    // 换时区就让旧的格式化器作废，下次格式化时按需重建。
    serverTZ.fmt = false;
    refreshTimezoneViews();
  }

  // applyAuditHead 在操作记录的表头上写清"这一列的时间按哪个时区"。
  //
  // 为什么必须写出来：审计时间是给人跟服务端日志、SSH 对时间用的。不写时区，
  // 用户会默认它是自己浏览器的时间 —— 差一个时区时怎么也对不上，而页面上
  // 看不出任何异常（日期甚至可能还是对的）。
  function applyAuditHead() {
    if (!el.auditTimeHead) return;
    el.auditTimeHead.textContent = serverTZ.name
      ? '时间（服务端时区 ' + serverTZ.name + '）'
      : '时间（本机时区）';
  }

  // refreshTimezoneViews 在"时区刚从无到有 / 变了"时把已经画出来的东西重画一遍。
  //
  // 为什么需要：时区是跟着接口回来的，可能比第一帧渲染晚（例如直接深链到
  // #/settings/audit 时，操作记录会先按浏览器本地时区画出来）。不重画的话，
  // 同一个页面上会同时存在两种时区的时间 —— 那比"整页都差一个时区"更难发现。
  function refreshTimezoneViews() {
    applyAuditHead();
    // 操作记录整表重取：条数有上限、重取比逐行改文本简单，也不会漏行。
    if (el.auditBody && el.auditBody.childNodes.length > 0) {
      resetAudit();
      loadAudit();
    }
    // 总览条「更新于」用同一个 ts 重渲染一次（ts 仍是服务端给的那个）。
    if (summaryState.summary) renderSummary(summaryState.summary, summaryState.ts);
    // 图表：xFormat 是每次调用现读时区的，这里只要把新时区交给引擎并重画。
    detail.charts.forEach(function (chart) {
      chart.setOptions({ timeZone: serverTZ.name });
    });
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
  // 会把每秒的推送一起撑大、首屏也拖得更久。64 是个现实中碰不到的数。
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
    // 卡片上记着自己是哪个节点：分组筛选那一排是按**卡片的显示顺序**分组的
    // （见 groupEntries），而 DOM 里只有元素、没有 id，不记一笔就只能回去翻
    // nodes（那是个 Map，顺序与列表当前顺序未必一致）。
    root.dataset.nodeId = String(dto.id);
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
    //
    // 到期文案由服务端算好（expires_text）而不是前端拼 "N 天"：拼出来的写法在
    // 不足 24 小时和已过期两种情况下都是「0 天」（见 internal/server 的 applyPricing）。
    // 服务端那份与告警文案共用同一个函数，所以面板与 Telegram 消息逐字一致。
    var hasPrice = dto.price_cents > 0;
    r.cost.root.hidden = !hasPrice;
    if (hasPrice) {
      // 金额走 nodeMoney：**一律人民币**（换算不了时如实退回原币种，见该函数），
      // 与详情页、设置页服务器列表同一口径。
      r.cost.value.textContent = [nodeMoney(dto, 'price_cents', 'price_cny_cents'), billingText(dto.billing_months)]
        .join(' ').trim() +
        (dto.expires_at > 0 && dto.expires_text ? ' · ' + dto.expires_text : '');
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
  // 着色走**同一个** latGrade（绝对阈值与本机倍数取严），与迷你条的延迟格子
  // 完全同源：同一台机器上两行颜色互相矛盾比"哪一档更准"严重得多。
  // 差别只在**基准**——这里比的是"当前这一段 vs 这个目标自己的整窗口均值"，
  // 迷你条比的是"这一段 vs 该节点整窗口均值"（跨目标）。
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
      // 没有比较基准就不做判断（同 latGrade 的 avg <= 0 分支）。
      var cls = latGrade(t.lat_ms, t.avg_ms);
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
    // 分组筛选正在生效时，新画出来的卡片也要立刻守规矩（否则新上线的机器会
    // 无视筛选冒出来，而它的分组其实不匹配）。
    card.root.hidden = !groupMatches(dto);
    updateCard(card, dto);
  }

  // ---------------------------------------------------------------- 首页分组筛选
  //
  // 节点网格上面那一排 chip：[全部 5] [香港 3] [美国 2] [未分组 1] —— 点一下只剩
  // 这一组的卡片。
  //
  // 选项**从实际存在的分组里生成**：节点本来就带 group_name（「编辑节点」里填的），
  // 这里只是拿它当筛选用，不新增任何字段，也没有第二个地方维护分组名单；
  // 没有节点的分组不会出现在这一排里（那只会是一个点开就空的按钮）。
  // 没填分组的节点归到「未分组」这个入口下 —— 不给入口的话，那些机器一筛选就
  // "消失"了，而用户完全看不出是为什么。
  //
  // 只影响**卡片区**：上面的「在线 3/5」与总览区是整个面板的健康度，
  // 筛出一组机器时它们仍然是全量数字（用户要的正是"这一组怎么样、整体又怎么样"）。
  var GROUP_FILTER_KEY = 'probe-group-filter-v1';

  // GROUP_UNGROUPED 是"未分组"的键。分组名是用户随便填的，"未分组"这四个字完全
  // 可能被真的当成组名，所以哨兵用一个正常输入框里打不出来的字符（NUL）：
  // 空串留给"全部"（默认值），真实组名与两者都不会撞。
  var GROUP_UNGROUPED = '\u0000';

  // groupFilter 是当前选中的键（'' = 全部）。
  var groupFilter = '';

  // groupFilterKey 是上一次画出来的那排 chip 的"内容指纹"：卡片每秒都会被 SSE
  // 重画一次，而分组是分钟级才变一次的东西 —— 不比对的话，每秒都要把按钮拆了
  // 重建，键盘焦点（Tab 到某个分组上）会跟着丢。
  var groupFilterKey = null;

  function groupKey(name) {
    var text = String(name == null ? '' : name).trim();
    return text === '' ? GROUP_UNGROUPED : text;
  }

  function groupLabel(key) {
    return key === GROUP_UNGROUPED ? '未分组' : key;
  }

  // groupMatches 判断一台机器在当前筛选下要不要显示。
  function groupMatches(dto) {
    if (!groupFilter) return true;
    return groupKey(dto.group_name) === groupFilter;
  }

  // loadGroupFilter / saveGroupFilter 把选择存进 localStorage（键名带版本前缀，
  // 与延迟图那几个开关同一套做法、同一条理由：以后结构变了而键名不变的话，
  // 老浏览器里存着的旧结构会被读成一个字段对不上的东西）。
  function loadGroupFilter() {
    try {
      var raw = localStorage.getItem(GROUP_FILTER_KEY);
      if (raw === null) return '';
      var parsed = JSON.parse(raw);
      // 只认字符串：读不懂的值一律当「全部」—— 拿一个坏值去筛，页面会变成
      // "一台机器都没有"，而那是用户最不可能想到的原因。
      return typeof parsed === 'string' ? parsed : '';
    } catch (err) {
      return '';   // 隐私模式下 localStorage 不可用：退回「全部」
    }
  }

  function saveGroupFilter(key) {
    try {
      localStorage.setItem(GROUP_FILTER_KEY, JSON.stringify(key));
    } catch (err) { /* 存不下就只在这次会话里生效，不影响筛选本身 */ }
  }

  // groupEntries 按**卡片在首页的显示顺序**列出分组：一个分组排在哪里，取决于
  // 它的第一台机器排在哪里（顺序就是拖动排序后的顺序）。按名字排序看着整齐，
  // 但用户拖完顺序之后 chip 的顺序会与卡片对不上，反而像"分组乱序了"。
  function groupEntries() {
    var order = [];
    var counts = new Map();
    Array.prototype.forEach.call(el.grid.children, function (card) {
      var dto = nodes.get(Number(card.dataset.nodeId));
      if (!dto) return;
      var key = groupKey(dto.group_name);
      if (!counts.has(key)) { counts.set(key, 0); order.push(key); }
      counts.set(key, counts.get(key) + 1);
    });
    return order.map(function (key) {
      return { key: key, label: groupLabel(key), count: counts.get(key) };
    });
  }

  function renderGroupFilter() {
    var entries = groupEntries();
    // 选中的那一组没了（最后一台机器被删掉、或者改了分组）：退回「全部」——
    // 否则页面会停在一个筛不出任何卡片的空状态上，而选择状态还"记着"。
    if (groupFilter && !entries.some(function (e) { return e.key === groupFilter; })) {
      groupFilter = '';
      saveGroupFilter(groupFilter);
    }
    var key = groupFilter + '\u0000' + entries.map(function (e) {
      return e.key + ':' + e.count;
    }).join('\u0001');
    if (key === groupFilterKey) return;   // 没变就不动 DOM（每秒都会走到这里）
    groupFilterKey = key;

    // 分组只有一个（或者一台机器都没有）时整排不显示：那时唯一的选项与「全部」
    // 完全等价，摆一排按钮只是白占一行 —— 首页此时不该多出一块控件。
    if (entries.length <= 1) {
      el.groupFilter.textContent = '';
      el.groupFilter.hidden = true;
      return;
    }

    var total = 0;
    entries.forEach(function (e) { total += e.count; });
    var items = [{ key: '', label: '全部', count: total }].concat(entries);
    el.groupFilter.textContent = '';
    items.forEach(function (item) {
      var btn = document.createElement('button');
      btn.type = 'button';
      btn.className = 'chip' + (item.key === groupFilter ? ' active' : '');
      // 台数直接写在文案里（"香港 3"）：不用点进去就知道这一组有几台机器 ——
      // 这排按钮同时也是"我有哪些分组"的一览。
      btn.textContent = item.label + ' ' + item.count;
      // 可键盘操作交给 <button> 本身（Tab 到、回车/空格触发都是浏览器自带的），
      // 选中状态同时给 aria-pressed（读屏与样式各取一份）。
      btn.setAttribute('aria-pressed', item.key === groupFilter ? 'true' : 'false');
      // 键记在 dataset 上：重画之后要能把焦点放回选中的那一枚（见 focusChip）。
      btn.dataset.group = item.key;
      btn.addEventListener('click', function () { setGroupFilter(item.key); });
      el.groupFilter.appendChild(btn);
    });
    el.groupFilter.hidden = false;
  }

  // focusChip 把键盘焦点放回选中的那一枚 chip。
  //
  // 为什么需要：点一下（或用键盘激活）会重画整排按钮，旧按钮被丢掉 ——
  // 焦点随之掉到 <body> 上，用键盘的人按一次回车就"跟丢"了，得重新 Tab 一遍。
  // 只在**用户操作之后**调它（setGroupFilter）：秒级的重画（分组/台数变了）
  // 也走 renderGroupFilter，那会儿抢焦点只会更烦人。
  function focusChip(key) {
    Array.prototype.forEach.call(el.groupFilter.querySelectorAll('button'), function (btn) {
      if (btn.dataset.group === key) btn.focus();
    });
  }

  // applyGroupFilter 把筛选结果落到卡片上：不匹配的整张 hidden。
  //
  // 卡片是**留着**的，不删 DOM：筛选是一种查看方式，不是数据状态 ——
  // 删掉的话，每秒一次的 SSE 重画还得把它们重新建出来，而且顺序、焦点、
  // 悬停状态全都会乱。
  function applyGroupFilter() {
    cards.forEach(function (card, id) {
      var dto = nodes.get(id);
      card.root.hidden = !dto || !groupMatches(dto);
    });
  }

  function setGroupFilter(key) {
    if (groupFilter === key) return;
    groupFilter = key;
    saveGroupFilter(key);
    renderGroupFilter();   // 重画这一排：高亮与 aria-pressed 都要跟着变
    applyGroupFilter();
    // 重画把按钮全换掉了：把焦点放回选中的那一枚，键盘用户不会"跟丢"。
    focusChip(key);
  }

  // reorderLocked 说明"现在能不能拖动排序"：按分组筛选时不能。
  //
  // 为什么：拖动松手时算出来的落点是**当前这一屏里**的位置（见 onNodeDragEnd），
  // 而 PUT /api/v1/nodes/order 收的是**完整**顺序 —— 拿筛选后的子集位置去写整份
  // 顺序，没显示出来的那些机器的次序会被搅乱，而页面上一点异常都看不出来
  // （只有下次不筛选时才会发现顺序变了）。
  // 界面上的表现见 settingsNodeRow：把手变灰 + title 写明原因。
  function reorderLocked() {
    return !!groupFilter;
  }

  // summaryState 记着"最近一次汇总 + 它对应的那个 ts"，只为了一件事：
  // 时区晚一步才拿到时，能把「更新于」按新时区重渲染一遍（见 refreshTimezoneViews）。
  var summaryState = { summary: null, ts: 0 };

  // renderSummary 里的 ts 必须是**服务端给的那个**（SSE 的 payload.ts）。
  //
  // 这里曾经在首帧用 Math.floor(Date.now() / 1000)（浏览器时钟）、之后用 SSE 的
  // payload.ts（服务端时钟）：两个时钟源来回切，两端差几分钟就会在页面上跳一下，
  // 而"更新于"本来是用来判断数据新不新的。现在首帧没有服务端 ts 就显示 —，
  // 等第一帧 SSE 到达再写上服务端的时间 —— 全页只有一个时钟源。
  function renderSummary(summary, ts) {
    summaryState.summary = summary;
    summaryState.ts = ts;
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
    // 分组与台数可能刚变了（新节点上线、分组被改过）：这里是"卡片刚重画完"的
    // 唯一出口，放在它后面才能按最新的一组卡片算顺序与台数。
    renderGroupFilter();
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

  // 值的两种写法：单行（大部分格子）与多行（见 overviewValueLines 的退路）。
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

  // overviewValueLines 拼总览区「剩余价值」那一格的内容。
  //
  // 正常情况下它只有**一行**：一个折好的人民币金额（用户要的"直接显示剩余价值
  // 多少￥"）。金额由服务端算（见 overview.go 的 remainingValueCNY）：
  //   - 能换算的币种（含 CNY，以及"没填币种"——服务端按人民币处理）拢成一个 ¥ 合计；
  //   - 换算不了的币种**绝不按 1:1 并进去**（那等于凭空编一个汇率，而用户会拿这个
  //     数当真实资产去决策），跟在后面单独列出来："¥1234.56 + 182.51 XYZ"；
  //   - 一个能换算的都没有时**连 ¥ 都不显示**：¥0.00 会被读成"这些机器一文不值"，
  //     而事实是"这些币种都换不了"。
  //
  // 退路：服务端没下发 remaining_value_cny 时退回改动之前的"按币种分行"。
  // 前端资源是嵌在二进制里一起发的，正常不会对不上；但反代缓存住一份新的 app.js
  // 配一台旧服务端时就会出现"新前端读不到新字段"——那时至少还能看见数字，
  // 而不是把一格本来就有信息的地方变成 —（与"拿不到 timezone 就退回本机时区"
  // 是同一条原则）。
  function overviewValueLines(t) {
    var cny = t.remaining_value_cny;
    if (!cny) {
      var byCurrency = (t.remaining_value || []).map(function (item) {
        return fmtMoney(item.cents, item.currency);
      });
      return byCurrency.length ? byCurrency : ['—'];
    }
    var parts = [];
    // converted=false 表示一个能换算的分组都没有 —— 这时不能写 ¥0.00。
    if (cny.converted) parts.push(fmtMoney(cny.cents, CNY_CODE));
    // 换算不了的各自带着自己的币种代码（fmtMoney 会写清是哪种钱）。
    (cny.unconverted || []).forEach(function (item) {
      parts.push(fmtMoney(item.cents, item.currency));
    });
    // 一台带价格的机器都没有（或全都没了剩余价值）：显示 — 而不是留空
    // ——空白会被读成"界面没渲染出来"。
    return parts.length ? [parts.join(' + ')] : ['—'];
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

    // 剩余价值：折成人民币显示（口径与退路见 overviewValueLines）。
    setOverviewLines('value', overviewValueLines(t));
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
  // 延迟着色（迷你条的延迟格子与「探测」那一行**共用**这一组阈值）。
  //
  // 两个口径**取严**（任一命中更坏的那一档就取那一档）：
  //
  //   bad  (红)：value > 240  或  value > avg × 2
  //   warn (黄)：value > 180  或  value > avg × 1.2
  //   ok   (绿)：其余
  //
  // 为什么要**绝对**阈值（LAT_ABS_*）：跨机器可比 —— "200ms 算不算慢"不该先去看
  // 这台机器的历史。一台常年 205ms 的机器上，197.9ms 相对它自己只是 0.97 倍
  // （相对口径判绿），但绝对值上已经落在该看一眼的区间（180 < 197.9 ≤ 240 → 黄）。
  // 分档出处：Komari Emerald 主题的 ≤60 / ≤120 / ≤180 / ≤240 / >240 —— 我们只有
  // 三档，取后两条边界（180 / 240）；边界一律"严格大于"（与它的 `<=` 写法一致）。
  //
  // 为什么要**相对**倍数（MINI_LAT_*_RATIO）：反过来的那一头同样真实 ——
  // 一条常年 20ms 的线路抖到 60ms，绝对分档里还是"绿"，可那是 3 倍。
  // warn 的 1.2× 不是随手取的：写成"裸均值"（value > avg）时按定义**大约一半的值
  // 都在均值之上**，每一行会有一半格子变黄，图就没有信息量了；1.2 这个数就是为此
  // 存在的。bad 的 2× 是旧口径，保持不变。
  var LAT_ABS_WARN_MS = 180;
  var LAT_ABS_BAD_MS = 240;
  var MINI_LAT_WARN_RATIO = 1.2;
  var MINI_LAT_BAD_RATIO = 2;

  // 丢包格子：0% 绿、(0, 5%] 黄、> 5% 红；没数据（null）留浅灰底。
  function miniLossClass(value) {
    if (typeof value !== 'number') return '';
    if (value > MINI_LOSS_WARN_PCT) return 'bad';
    if (value > 0) return 'warn';
    return 'ok';
  }

  // latGrade 是延迟着色**唯一**的口径：迷你条的延迟格子与「探测」那一行都走它。
  //
  // 两行必须共用同一个函数（不是各写一份同样的判断）：同一台机器上"格子绿着、
  // 探测行黄着"这种自相矛盾，比"哪一档更准"严重得多 —— 用户没法判断该信哪一个。
  // 它们的**基准**仍然是各自的（格子比该节点整窗口均值、探测行比该目标整窗口均值），
  // 变的只是"拿什么尺子量这个差"。
  //
  // avg <= 0（没有基准：没配目标 / 整段全丢 / 一个样本都没有）时返回 ''（浅灰）：
  // 0 ms 是"没测到"，不是"很快" —— 拿 0 当基准会把所有有值的格子判成红的。
  function latGrade(value, avg) {
    if (typeof value !== 'number') return '';
    if (!(avg > 0)) return '';
    if (value > LAT_ABS_BAD_MS || value > avg * MINI_LAT_BAD_RATIO) return 'bad';
    if (value > LAT_ABS_WARN_MS || value > avg * MINI_LAT_WARN_RATIO) return 'warn';
    return 'ok';
  }

  // MINI_LAT_HINT 把上面那条规则讲给用户听（挂在迷你条「延迟」那一行的标题上，
  // 悬停可见）。数字**全部由常量拼出来**：写死"180 / 240 / 1.2"的话，以后调阈值
  // 就会留下一句骗人的说明 —— 而颜色这种事，说明与规则不一致比没有说明更糟。
  //
  // 为什么挂 label 而不是整行/格子：格子自己有自建浮层（悬停显示那一段的读数），
  // 再叠一个原生 title 会两个浮层一起冒出来。
  var MINI_LAT_HINT = '格子颜色：延迟 > ' + LAT_ABS_WARN_MS + 'ms，或 > 本机这一小时均值的 ' +
    MINI_LAT_WARN_RATIO + ' 倍 → 黄；> ' + LAT_ABS_BAD_MS + 'ms，或 > ' + MINI_LAT_BAD_RATIO +
    ' 倍 → 红（两条判据取更严的那个）。';

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
      row: row, label: label, value: value, cells: cells, cellNodes: [],
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
    // 颜色的规则不是一眼能猜出来的（两个口径取严），说明挂在离格子最近的标题上。
    lat.label.title = MINI_LAT_HINT;
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
    // 延迟格子按该节点这一小时的窗口平均值分级（见 latGrade）：那个均值
    // 就是行首印着的 mini.lat_ms，格子与数字用同一个基准。
    renderMiniRow(bar.lat, mini.lat, miniLatText(mini.lat_ms), function (value) {
      return latGrade(value, mini.lat_ms);
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
    // 分组筛选那一排跟着卡片一起清掉（**选择本身留着**：它是本浏览器的偏好，
    // 与延迟图那几个开关同一条约定）。指纹也要清 —— 不清的话，下次登录时
    // "内容没变"会让 renderGroupFilter 直接返回，而 DOM 里其实已经空了。
    groupFilterKey = null;
    el.groupFilter.textContent = '';
    el.groupFilter.hidden = true;
  }

  // ---------------------------------------------------------------- 加载数据

  function loadNodes() {
    return api('/api/v1/nodes').then(function (data) {
      // 服务端时区随这个接口一起来（server.timezone），先记下来再渲染：
      // 下面这些卡片与总览条上的每一个时间都要用它。
      setServerTimezone(data.server && data.server.timezone);
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
      // 分组那一排按**卡片现在的顺序**重排（见 groupEntries）：拖完顺序之后，
      // chip 的顺序也该跟着变得符合直觉。放在重排卡片之后。
      renderGroupFilter();
      applyGroupFilter();
      // 首帧不给 ts：这时手上只有浏览器时钟，而它是**另一个时钟源**，
      // 拿它冒充服务端时间就是"更新于"跳来跳去的根源（见 renderSummary）。
      // 第一帧 SSE 到达后自然会被填上。之后每一次全量取数（改完节点、
      // 拖完顺序都会走到这里）沿用**上一个服务端 ts**，而不是把它清成 —：
      // 那会让"更新于"在每次保存之后闪一下，而这几秒里时间其实一直在往前走。
      renderSummary(data.summary, summaryState.ts);
    });
  }

  // ---------------------------------------------------------------- 节点详情

  // 3d/7d 的格式要看**实际标签间隔**（第二个参数，由图表抽稀后传进来）：
  // 抽稀后的间隔可能小于一天（例如 3d 档实际每 12 小时一个），那时只用 MM-DD 会画出
  // 「09-29 09-29 09-29」一串长得一模一样的标签 —— 加上时分才分得清是哪一天里的哪一刻。
  // 间隔 ≥ 一天时保持 MM-DD：标签更短，同样宽度下能放下更多个。
  //
  // 3d 档还看**上一个标签**（第三个参数，prevTS）：每天的**第一个**标签写日期，
  // 其余写时分（与 1d 同一套判据）。这样一天的边界由日期标出来，中间的刻度只写
  // 时分就够 —— 六个标签里三个是日期，比"每一格都写 MM-DD HH:MM"窄一半，
  // 半宽的资源图上因此能保住 6 个标签（写全的话只放得下 3 个）。
  //
  // 1d 档的日期同样看上一个标签：窗口有 24 小时，全是 "13:10" 这种纯时分就分不清
  // 哪段是今天、哪段是昨天。规则是"每天的第一个标签写日期（10-01），其余写时分"。
  //
  // 为什么判据是"跨天"而不是"时刻等于 00:00"：刻度锚在**绝对时间网格**上
  // （见 chart.js 的 labelFits），而服务端时区的偏移可以是任意分钟数
  // （+05:30、+09:00…）—— 偏移不是步长整数倍时，零点根本不落在刻度上，
  // "等于 00:00"这条判据会一个日期都标不出来。按"这一格与上一格不是同一天"
  // 判定，则每一天必然有且只有一个标签带日期：零点正好落在刻度上时就是零点本身，
  // 否则是零点之后的第一个刻度。
  var RANGE_X_FORMAT = {
    '1h': function (ts) { return clockOf(ts); },
    '6h': function (ts) { return clockOf(ts); },
    '12h': function (ts) { return clockOf(ts); },
    '1d': function (ts, step, prev) { return startsNewDay(ts, prev) ? dateOf(ts) : clockOf(ts); },
    '3d': function (ts, step, prev) { return (startsNewDay(ts, prev) || step >= 86400) ? dateOf(ts) : clockOf(ts); },
    '7d': function (ts, step) { return step >= 86400 ? dateOf(ts) : dateTimeOf(ts); }
  };

  // RANGE_X_LABEL_MAX 是每一档"适合观察"的标签个数**上限**（用户定稿的目标区间
  // 取上界）：1h/6h/12h/1d → 8~12 个，取 12；3d → 6~10 个，取 10；7d → 7~10 个，取 10。
  //
  // 交给图表引擎的是上限，不是下限：下限由画布宽度决定（半宽的资源图放不下 8 个
  // "HH:MM"），硬塞只会把标签挤回去 —— 而"先看得清"是这次改动的前提
  // （见 chart.js 的 X_LABEL_MIN_GAP）。它和 chart.js 的 X_STEP_LADDER 一起
  // 决定每一档最终的间隔，实测值见 e2e 的 X 轴用例。
  var RANGE_X_LABEL_MAX = { '1h': 12, '6h': 12, '12h': 12, '1d': 12, '3d': 10, '7d': 10 };

  // dayKeyCache 缓存"某个时刻属于哪一天"（**服务端时区**）：做日期判定时每次绘制
  // 都要问一遍，而 tzFields 走的是 Intl.formatToParts —— 渲染路径上最贵的一步。
  // 窗口在滑动，键只增不减，所以缓存满了整体丢掉重建（不做 LRU：这里只需要挡住
  // "同一次绘制里把同一批刻度问好几遍"）。
  var dayKeyCache = {};
  var dayKeyCount = 0;

  function dayKeyOf(ts) {
    var key = String(ts);
    if (Object.prototype.hasOwnProperty.call(dayKeyCache, key)) return dayKeyCache[key];
    if (dayKeyCount > 512) { dayKeyCache = {}; dayKeyCount = 0; }
    dayKeyCache[key] = dayOf(ts);
    dayKeyCount++;
    return dayKeyCache[key];
  }

  // startsNewDay 判断这一格是不是**这一天的第一个标签**（prev 为 null 表示它是
  // 整排标签里的第一个，也算）。判据按服务端时区比"哪一天"，不是比小时数。
  function startsNewDay(ts, prev) {
    if (prev === null || prev === undefined) return true;
    return dayKeyOf(ts) !== dayKeyOf(prev);
  }

  // clockOf / dateOf / dateTimeOf / dayOf 都是**按服务端时区**渲染的
  // （理由见文件上方「时区渲染层」那一段），与 fmtClock / fmtTime 同一口径。

  // clockOf "HH:MM"：图表 X 轴、迷你条浮层的区间端点、计费周期的时刻。
  function clockOf(ts) {
    var f = tzFields(ts);
    return f.hour + ':' + f.minute;
  }

  // dateOf "MM-DD"：流量图的日轴（服务端本地零点 → 服务端时区的日期）。
  function dateOf(ts) {
    var f = tzFields(ts);
    return f.month + '-' + f.day;
  }

  function dateTimeOf(ts) {
    var f = tzFields(ts);
    return f.month + '-' + f.day + ' ' + f.hour + ':' + f.minute;
  }

  // dayOf "YYYY-MM-DD"：到期日输入框要的那个形状。
  function dayOf(ts) {
    var f = tzFields(ts);
    return f.year + '-' + f.month + '-' + f.day;
  }

  // ---------------------------------------------------------------- 日期 ↔ epoch
  //
  // <input type="date"> 里的是**日历上的一天**（"2026-10-01"），而库里存的是一个
  // epoch。这一对换算必须与后端切天用**同一把尺子**，否则同一个日期在两端的归属日
  // 不同：东八区下若按 UTC 零点存，"10-01 到期"会被后端读成 09-30。
  // 所以这里求的是"**服务端时区**下那一天的零点"，读回时也按服务端时区取日期 ——
  // 往返因此恒等（存进去什么日期，读出来还是那个日期，反复编辑不漂移）。
  //
  // 为什么不干脆用 UTC 做纯日期换算（Date.UTC 写、toISOString 读）：那样往返也是
  // 恒等的，但它把到期日变成"另一种时间戳"—— 全页只有这一个字段按 UTC 解释，
  // 而且**已经存在库里的历史数据会继续错**：旧代码写进去的就是"浏览器本地零点"，
  // 在"管理员浏览器时区 == 服务端时区"这种最常见的部署里它正好等于服务端零点，
  // 按服务端时区读回来是对的，按 UTC 读回来仍然差一天。按服务端时区换算才能把
  // 存量数据一起修正过来。

  // zoneOffsetSec 返回服务端时区在 utcSec 这一刻的偏移（秒，东为正）。
  // 拿不到时区时退回浏览器本地的偏移 —— 与 tzFields 的退路保持一致。
  function zoneOffsetSec(utcSec) {
    if (!tzFormatter()) return -new Date(utcSec * 1000).getTimezoneOffset() * 60;
    var f = tzFields(utcSec);
    return Date.UTC(+f.year, +f.month - 1, +f.day, +f.hour, +f.minute, +f.second) / 1000 - utcSec;
  }

  // dayStartEpoch 求"服务端时区下 y-mo-d 这一天的零点"对应的 epoch（秒）。
  function dayStartEpoch(y, mo, d) {
    var want = Date.UTC(y, mo - 1, d, 0, 0, 0) / 1000;
    var t = want;
    // 偏移量随时刻变（夏令时）：先当成 UTC 零点猜一个，再按那一刻的偏移校正。
    // 最多三轮 —— 同一时区的偏移只在切换的那一刻跳一次，两轮必然稳定。
    for (var i = 0; i < 3; i++) {
      var next = want - zoneOffsetSec(t);
      if (next === t) break;
      t = next;
    }
    // 有些时区在夏令时开始那天根本没有"零点"（当地 00:00 直接跳到 01:00），
    // 上面校正出来的时刻可能落在前一天 23:00。往后再找最多两小时，直到它在
    // 服务端时区里确实属于输入的那一天 —— **往返恒等**优先于"正好是零点"。
    var wantDay = String(y) + '-' + pad2(mo) + '-' + pad2(d);
    for (var j = 0; j < 3 && dayOf(t) !== wantDay; j++) {
      t += 3600;
    }
    return t;
  }

  // DATE_INPUT_RE 匹配 <input type="date"> 的值（浏览器保证是 yyyy-mm-dd）。
  var DATE_INPUT_RE = /^(\d{4})-(\d{2})-(\d{2})$/;

  // parseDateInput 把输入框里的 yyyy-mm-dd 变成"服务端时区那一天的零点"epoch（秒）。
  // 不自己拼 new Date(字符串)：那个会被当成 UTC 或浏览器本地，正是这次要修的问题。
  function parseDateInput(value) {
    var m = DATE_INPUT_RE.exec(String(value == null ? '' : value).trim());
    if (!m) return 0;
    return dayStartEpoch(Number(m[1]), Number(m[2]), Number(m[3]));
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
  //
  // 平均延迟只累加**正样本**（p[1] > 0）。约定是：延迟数据里 avg == 0 表示
  // "这一桶没有任何成功的探测"（没有延迟样本），**不是** 0 毫秒（后端就是这么
  // 约定的：ping_samples_1m 里整分钟全丢的行 avg_ms = 0、up_cnt = 0，服务端按
  // up_cnt 加权，见 store.QueryPingSeries）。把 0 也累加进去、还把它算进分母，
  // 就是后端刚修掉的那个错误 —— 结果**方向是反的**：线路越是丢包，合并出来的
  // 平均延迟越低（一个 200ms 的桶旁边挂三个全丢的桶，均值就被拽到 50ms），
  // 用户看到的是"越丢包越快"。所以分子与分母都只用正样本。
  //
  // 这个合并桶里一个正样本都没有时，合并后的 avg 是 **0** —— 保持"没有样本"
  // 这个语义，而不是 0 / 0 得到的 NaN（NaN 会让 Y 轴范围、悬浮读数、红线判定
  // 全部失去意义）。画线前 latencySeriesFor 会把这个 0 规范化成 null（缺失）。
  function aggregate(points, targetSec) {
    if (!targetSec || targetSec <= 0 || points.length === 0) return points;
    var out = [];
    var cur = null;
    points.forEach(function (p) {
      var bucket = Math.floor(p[0] / targetSec) * targetSec;
      if (!cur || cur[0] !== bucket) {
        // [桶起点, 正样本值累加, 峰值, 正样本个数, 丢包累加, 有丢包的点数]
        cur = [bucket, 0, 0, 0, 0, 0];
        out.push(cur);
      }
      // 只有正样本才进分子与分母（见上面那段注释）。
      if (p[1] > 0) {
        cur[1] += p[1];
        cur[3] += 1;
      }
      // max 照旧对**所有**点取最大值：它是"这一桶最高多少"，口径不变。
      // （全丢的桶 max 也是 0，最终会和 avg 一起被规范化成 null。）
      cur[2] = Math.max(cur[2], p[2]);
      if (typeof p[3] === 'number' && isFinite(p[3])) {
        cur[4] += p[3];
        cur[5] += 1;
      }
    });
    return out.map(function (c) {
      // c[3] === 0（整个合并桶一个正样本都没有）时 avg 给 0，不能写 c[1] / c[3]。
      var merged = [c[0], c[3] > 0 ? c[1] / c[3] : 0, c[2]];
      if (c[5] > 0) merged.push(c[4] / c[5]);
      return merged;
    });
  }

  // mobileAggSec 返回"这张图的档位在手机端要不要再聚合一次"，以及聚合目标（秒）。
  //
  // 档位必须由调用方指定，而且必须是**这张图自己的**那一份：
  //   - 资源图（CPU/内存/磁盘/网络/流量）跟的是资源卡的 detail.range；
  //   - 延迟图跟的是延迟卡的 detail.pingRange。
  // 两张卡的档位是独立的，拿错的那一份去聚合就会得到一张完全不该那样的图
  // （例如延迟图停在 1h、资源图停在 7d 时，1h 的延迟曲线被按 1800 秒合并）。
  function mobileAggSec(rangeKey) {
    if (!window.matchMedia(MOBILE_QUERY).matches) return 0;
    return rangeMeta(rangeKey).mobile_agg_sec || 0;
  }

  // seriesFor 是**通用**取点：把接口给的点按"这张图自己的档位"做一次手机端二次聚合。
  // CPU / 内存 / 磁盘 / 网络与流量图都走它 —— 那里的 0 是实打实的读数
  // （0% CPU、0 B/s 都是合法的），所以这里**一个字都不能动**，绝不能把 0
  // 当成缺失（见下面的 latencySeriesFor）。
  //
  // rangeKey 是**必传**的（这里曾经写死 detail.range）：延迟图正是走它，
  // 而延迟图的档位是 detail.pingRange —— 写死资源档位时，两张卡停在不同档位
  // 就会拿另一张表的聚合目标去合并这张图的点。
  function seriesFor(points, rangeKey) {
    return aggregate(points, mobileAggSec(rangeKey));
  }

  // latencySeriesFor 是**延迟图（/ping）专用**的取点：在 seriesFor 之上把
  // "这一桶没有有效读数"的点规范化成 null（缺失）。
  //
  // 为什么必须规范化：chart.js 的 drawLine 只跳过**非数字**的值，0 会被原样
  // 画成 0ms —— 正好落在画布底边（绘图区底部就是 0）。而算 Y 轴范围的 bounds()
  // 只看 > 0 的读数，两边口径不一致，画出来的就是"轴的范围里根本没有 0、
  // 线却扎到了 0"：那条假线会让人以为"丢包的时候延迟反而最低"。
  //
  // 为什么选"规范化成 null"而不是给 drawLine 加一个"0 也算缺失"的开关：
  // 开关是按序列生效的全局行为，任何一处忘了关（或以后新加一张图忘了传），
  // CPU 0%、内存 0%、速率为 0 这些**合法读数**就会被静默丢掉 —— 那是比现在
  // 这个 bug 更隐蔽的一类错。null 只会出现在延迟序列里，其余序列的点一个都不碰，
  // 而 chart.js 本来就有一条"值不是数字就跳过"的路径（drawLine / drawBars），
  // 不需要新增任何开关。桌面端与手机端走的是同一条路：手机端聚合出来的 0
  // 也在这里被规范化。
  function latencySeriesFor(points) {
    return seriesFor(points, detail.pingRange).map(function (p) {
      var has = p[1] > 0;   // 这一桶有没有成功的探测（0 = 没有样本，不是 0ms）
      // max 与 avg 是同一批样本算出来的：没有样本时两者都是 0，都要当缺失。
      // max 单独再判一次 0：峰值虽然不再画线了，但悬浮读数里那一行（chart.js 的
      // hoverPeak）读的就是它 —— 留一个 0 在那儿会被格式化成 "峰值 0 ms"，
      // 而 0 ms 是一个**不可能的**峰值（延迟有物理下限），写出来就是误导。
      return [p[0], has ? p[1] : null, has && p[2] > 0 ? p[2] : null, p[3]];
    });
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
      xFormat: RANGE_X_FORMAT[detail.range] || clockOf,
      // 这一档适合观察的标签个数上限（见 RANGE_X_LABEL_MAX）：建实例时就带上，
      // 免得第一帧（数据还没到）与后面几帧的稀疏规则不一样。
      xLabelMax: RANGE_X_LABEL_MAX[detail.range] || 0,
      // 图上所有由**引擎自己**格式化时间的地方（桶宽 ≥ 1 小时时悬浮读数里的
      // 区间端点、跨天判定）都用这个时区 —— 与 X 轴标签、与页面其它时间同一口径。
      timeZone: serverTZ.name
    });
    detail.charts.set(key, chart);
    return chart;
  }

  // renderRangeButtons 渲染**两组**时间档位按钮（1h…7d）。
  //
  // 两组（「资源与网络」卡的 #detail-ranges 与「延迟」卡的 #lat-ranges）共用这一个
  // 渲染函数：按钮文案、样式、active 高亮规则必须一模一样，写两份迟早有一处忘同步。
  // 但它们的状态与作用范围是**分开**的（见 detail.range / detail.pingRange）：
  // 资源组切完只重取 /series，延迟组切完只重取 /ping，互不牵连。
  // 两个按钮组的高亮也各算各的（activeKey 分别传进去），所以资源卡停在 1h 而延迟卡
  // 停在 6h 时，两边显示的高亮就是两个不同的按钮 —— 不会被看成一整组控件。
  function renderRangeButtons() {
    renderRangeGroup(el.detailRanges, detail.range, setResourceRange);
    renderRangeGroup(el.latRanges, detail.pingRange, setPingRange);
  }

  // renderRangeGroup 画一组档位按钮：清空容器的旧按钮、按 detail.ranges 重建、
  // 把 activeKey 那一个高亮出来。点击交给 onPick（每组一个，见下面两个 set*）。
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

  // setResourceRange 切「资源与网络」那一组档位：只重取资源序列（CPU/内存/磁盘/网络）。
  //
  // 它**不碰延迟图**：延迟图跟的是 detail.pingRange 与 /ping 接口，与这里无关。
  // 这是拆分档位时最容易写错的一处（以前这里就是一句 loadPingChart() 把延迟图
  // 一起重拉），所以注释留在这里。
  function setResourceRange(key) {
    if (detail.range === key) return;
    detail.range = key;
    renderRangeButtons();
    loadSeries();
  }

  // setPingRange 切「延迟」那一组档位：**只**重新请求 /ping。
  //
  // 同理不重取资源序列：那五张图跟的是 detail.range，切延迟档位时它们一个字节都不该动。
  // 目标列表（detail.pingTargets）也不重取 —— 它是配置，与时间范围无关。
  function setPingRange(key) {
    if (detail.pingRange === key) return;
    detail.pingRange = key;
    renderRangeButtons();
    loadPingChart();
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

  // CNY_CODE 是人民币的币种代码，同时也是**"没填币种"时服务端采用的口径**
  // （见 internal/fx 的 ToCNY：币种为空按人民币原值返回，绝不返回 0）。
  // 界面判断"这个金额是不是人民币口径"只认这一个常量，不再散写字面量。
  var CNY_CODE = 'CNY';
  var CNY_SYMBOL = '¥';

  function fmtAmount(cents) {
    // 金额一律两位小数：分 → 元。
    return (Math.max(0, cents || 0) / 100).toFixed(2);
  }

  // fmtMoney 按**原币种**渲染一个金额（"$100.00 USD"、"123.45 XYZ"）。
  //
  // 三处价格已经不走它了（一律人民币，见 nodeMoney）。它剩下的用处只有两个，
  // 都是"原币种才是实话"的场合：
  //   1. 汇率取不到时的退路 —— 那个数没换算过，套一个 ¥ 就是撒谎（见 nodeMoney）；
  //   2. 首页总览区「剩余价值」按币种分行的几行：服务端按币种分组下发，前端照原样
  //      渲染（见 renderOverview），那里出现美元是正常的。
  //
  // 人民币**不再跟一个 CNY**：¥ 本身已经说明是人民币，再写一遍代码是废话，
  // 还会把卡片上只有一格宽的「费用」行撑长。"币种没填"与 CNY 是同一条路
  // （服务端按人民币处理），所以它也写成 ¥金额 —— 写成一个光秃秃的数字的话，
  // 同一台机器在卡片与总览区就会一个带 ¥、一个不带。
  function fmtMoney(cents, currency) {
    var amount = fmtAmount(cents);
    var code = String(currency || '').toUpperCase();
    if (!code || code === CNY_CODE) return CNY_SYMBOL + amount;
    var symbol = CURRENCY_SYMBOL[code];
    return symbol ? symbol + amount + ' ' + code : amount + ' ' + code;
  }

  // nodeMoney 取一个节点某个金额字段的**显示文本**：一律人民币。
  //
  // 三个显示位置（首页卡片的「费用」行、详情页顶部那三格、设置页「服务器列表」的
  // 剩余价值）都走这一个函数：同一个概念在三处必须长得一样，否则会被当成两个值。
  // 前端**不重算汇率**（前端不做算术）：换算结果、以及"到底换算过没有"，都由服务端下发。
  //
  // 只有两条规则，分岔口是服务端的 cny_converted（这个金额真的换算过没有）：
  //
  //   1. 真的换算过（外币 + 有可用汇率）→ 显示**人民币口径**「¥500.00」，
  //      两位小数，不跟币种代码。
  //   2. 没换算过 → 原币种金额就是这个数**唯一的事实**，如实显示它：
  //        · 币种是 CNY、或压根没填（服务端按人民币处理）→「¥31.00」；
  //        · 别的币种（老数据里的 XYZ、或这一份汇率表里没有它）→「123.45 XYZ」。
  //          **绝不**写成 ¥123.45：服务端"退回原值"的语义正是"这个数没换算过"，
  //          套一个 ¥ 会让用户以为它是人民币，拿去对账就错了一整截。
  //          也绝不留白、不写 ¥—：那两种写法都看不出"这里本来有一个金额"。
  function nodeMoney(node, origField, cnyField) {
    if (node.cny_converted) return CNY_SYMBOL + fmtAmount(node[cnyField]);
    var code = String(node.currency || '').trim().toUpperCase();
    if (code === '' || code === CNY_CODE) return CNY_SYMBOL + fmtAmount(node[origField]);
    return fmtMoney(node[origField], code);
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
    // 「入库以来累计流量」而不是「历史累计流量」：这个数来自
    // SELECT SUM(rx), SUM(tx) FROM traffic_daily（后端 TrafficTotals），也就是
    // **这个数据库开始记录以来**的累计 —— 删掉节点会把它一起级联清掉。
    // 写成「历史累计」会被读成"这台机器开机以来的总流量"（那个数只有 Agent 自己
    // 的网卡计数才有），两者能差出好几倍，而页面上看不出是哪一个。
    infoRow(tra, '入库以来累计流量', '↓ ' + fmtBytesDec(node.traffic_total_rx) + '  ↑ ' + fmtBytesDec(node.traffic_total_tx));
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
  // 「到期」只看到期日，与填没填价格无关，所以它单独判断。
  function renderDetailStats() {
    var node = detail.node;
    if (!node) return;

    // 到期文案一律用服务端给的 expires_text（与首页卡片、服务器列表、告警消息
    // 同一个函数算出来的）：前端拼 "N 天" 时，不足 24 小时与已过期都会显示「0 天」。
    el.statLeft.textContent = node.expires_at > 0 && node.expires_text ? node.expires_text : '—';

    // 已过期的机器**整格**不显示「剩余价值」（含标题）——不是显示 ¥0.00，也不是
    // 显示 —：机器一过期，剩余天数就归零，价值必然是 0，写出来是废话。
    //
    // 判据是服务端的 node.expired，不是 remaining_days <= 0：那个整天数在
    // "还剩 5 小时"时同样是 0，而那不是过期（见 dto.go 的 Expired）。
    // 也**不带**"没填价格"那一路：没填价格的机器照旧显示 —（那是"没填"，
    // 与"过期"是两回事，混在一起就再也分不出"这台机器没记价格"了）。
    var expired = node.price_cents > 0 && node.expired;
    var valueCell = el.statValue.parentNode;   // <b> 的父节点就是那一格（.stat）
    valueCell.hidden = expired;
    // 整条汇总排的列数跟着格数走：少一格却还是四列的话，右边会空出一块容器底色，
    // 看着像没渲染完（与"折成两行"的 span-full 同一套做法）。
    if (valueCell.parentNode) valueCell.parentNode.classList.toggle('cols-3', expired);

    if (!(node.price_cents > 0)) {
      el.statPrice.textContent = '—';
      el.statMonthly.textContent = '—';
      el.statValue.textContent = '—';
      return;
    }

    // 剩余价值由服务端算好（前端不做算术，PC 与手机看到的一定一致）。
    // 金额走 nodeMoney：一律人民币（换算不了时如实退回原币种），与首页卡片、
    // 服务器列表同一口径。
    el.statPrice.textContent = [nodeMoney(node, 'price_cents', 'price_cny_cents'), billingText(node.billing_months)]
      .join(' ').trim();
    el.statMonthly.textContent = [nodeMoney(node, 'monthly_cents', 'monthly_cny_cents'), '/ 月'].join(' ');
    el.statValue.textContent = nodeMoney(node, 'remaining_value_cents', 'remaining_value_cny_cents');
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

  // 这里原本有一个 placeRangeButtons()：仪表盘里把五张资源图全部取消勾选、只留延迟时，
  // 卡片 A 整张被收起，那一组时间档位会跟着消失，于是它把档位按钮**挪**到卡片 B 的
  // 标题行去兜底。现在**删掉了**，理由是它已经没有要解决的问题：
  //   - 延迟卡自带一组档位（#lat-ranges，见 renderRangeButtons），资源卡收起时
  //     延迟图照样能换档位 —— 兜底原本要保证的那件事已经由结构本身保证；
  //   - 再挪过去的话，卡片 B 的标题行里会同时出现两组档位（资源那组 + 延迟那组），
  //     两组按钮长得一模一样却控制不同的图，比"按钮暂时不见"糟糕得多；
  //   - 五张资源图都被取消勾选时，那一组档位本来也没有可控制的图（页面上一张
  //     资源图都没有），留着它反而是个能点却没反应的控件。
  // 卡片 A 重新可见（用户把某张资源图勾回来）时，档位按钮跟着卡片一起回来 ——
  // 按钮容器一直在卡片自己的标题行里，不需要任何"挪回去"的逻辑。

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
    // 这里原本还会调用 placeRangeButtons()，把档位按钮挪到"当前可见的那张卡"。
    // 延迟卡自带档位之后那个函数已经删掉（理由见上面那段注释）：档位按钮就留在
    // 自己那张卡的标题行里，卡片收起时它跟着一起消失，不需要也不该被挪走。
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
      // 这一份设置里也带 server.timezone，而且它是**进页面后最早**回来的那一份
      // （refreshSession 里排在路由之前），先记下来，首屏的时间就是对的。
      setServerTimezone(data.server && data.server.timezone);
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
        // 每天两种方向各画一条。X 轴上的点是**服务端本地零点**（api_traffic.go 的
        // startDay），所以标签必须按服务端时区渲染（dateOf 就是干这个的）——
        // 按浏览器本地渲染时，服务端 UTC+8 / 浏览器 UTC+0 会把 10-02 的点画成
        // 10-01 16:00、标签写成 10-01，整条日轴差一天。
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
      // 这一档适合观察的标签个数上限（1h~1d 12 个、3d/7d 10 个，见 RANGE_X_LABEL_MAX）：
      // 没有它，整行宽的图上会稀出十七八个标签 —— 不挤，但也没人会逐个读。
      var xLabelMax = RANGE_X_LABEL_MAX[detail.range] || 0;
      // tick_base_sec 是这一档的 X 轴**基准**间隔（1h/6h/12h = 1 分钟、1d = 2 分钟、
      // 3d = 5 分钟、7d = 15 分钟）。它比屏幕上能放下的密得多，实际标签间隔由
      // chart.js 按标签文本宽度自动稀疏（整齐刻度阶梯）。tick_label_sec 那个字段
      // 已经不再影响画面（它过去是"标签 + 竖网格线"的步长），所以这里不再读它。
      var tickBase = meta.tick_base_sec || 60;

      // 流量图的 Y 轴是字节（每天的量）：轴自带单位（GB/TB），所以 unit 留空。
      // 百分比图的 unit 也必须留空：读数是 yFormat(值) + unit 拼出来的，而下面的
      // yFormat 已经带上了 '%'（刻度轴用的也是它），再给 unit 一个 '%' 会拼成 "0%%"。
      // 四个图表里只有这一处重复过 —— 其余三个（字节/速率/延迟）都是"单位只出现在
      // 一处"：要么在 yFormat 里，要么在 unit 里。
      var pctOpts = { yMax: 100, unit: '', yFormat: function (v) { return v.toFixed(0) + '%'; }, tickBaseSec: tickBase, xFormat: xFormat, xLabelMax: xLabelMax, showMax: true };
      // 速率图的 Y 轴是"每秒多少字节"：yFormat 直接给 fmtRate（KB/s、MB/s，
      // 1000 进制），单位已经写在刻度里，unit 必须留空 —— 否则读数会变成 "MB/s/s"。
      var rateOpts = { yMax: 0, unit: '', yFormat: fmtRate, tickBaseSec: tickBase, xFormat: xFormat, xLabelMax: xLabelMax, showMax: true };

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
  //
  // 这里现取的档位是**资源卡**的 detail.range：喂给它的都是资源图
  // （CPU/内存/磁盘/网络）。延迟图虽然也走这个函数，但它的点已经由
  // loadPingChart 按 detail.pingRange 聚合好、走 multiSpec 的 points 传进来，
  // 不会在这里被二次聚合（见 seriesFor 的说明）。
  function setChart(key, canvasId, data, seriesSpec, options, multiSpec) {
    var chart = chartFor(key, canvasId);
    if (!chart) return;
    var specs = multiSpec || (data ? seriesSpec.map(function (s) {
      return { label: s.label, color: s.color, points: seriesFor(data.points, detail.range) };
    }) : []);
    var series = specs.map(function (s) {
      return {
        label: s.label,
        color: s.color,
        // bars 原样透传（只有延迟图会带）：这里一旦漏掉，丢包竖条就画不出来，
        // 而且看不出哪里错了 —— 数据、图例、勾选框全都是对的。
        bars: s.bars,
        points: s.points || (s.data ? seriesFor(s.data.points, detail.range) : [])
      };
    });
    chart.setData(series, options);
  }

  // ---------------------------------------------------------------- 延迟图（探测目标）
  //
  // 这里的"延迟"是用户配置的探测目标（Agent 每 N 秒探一次，服务端下发），
  // 不是 Agent 到面板自身的 WebSocket 往返 —— 后者走 Cloudflare 隧道时恒为
  // ~100ms，画成曲线没有任何参考价值（它现在仍在「网络信息」卡里，叫「面板延迟」）。

  // pingTargetLabel 是曲线与目标卡片上显示的名字：名称允许留空，留空就用地址。
  function pingTargetLabel(t) {
    return t.label || t.host || ('目标 #' + t.id);
  }

  // ---------------------------------------------------------- 延迟图的三个开关
  //
  // 卡片下面那一行 chip（延迟 / 丢包 / 平滑曲线）控制"这张图上画什么"。
  // 它是**本浏览器**的看图偏好，与服务端的探测配置无关 —— 所以和「哪些目标被隐藏」
  // 一样存 localStorage，不进服务端。
  //
  // 这里曾经还有第四个 chip「峰值线」。用户要求去掉它：峰值线永远不画（图上不再有
  // 那条 0.28 的淡线），Y 轴也不再为峰值留空间（"图表上面的留白因为峰值延迟的缘故
  // 变得太多了"），**但悬浮读数里的「峰值 N ms」那一行要保留**。三件事现在是：
  // 画线/轴（chart.js 的 showMax，延迟图写死 false）、悬浮那一行（hoverPeak，开）——
  // 一个没有开关的功能不该留一个 chip，所以 chip 与它的存储键一起删掉。

  // LAT_VIEW_ITEMS 是三个开关：键（存进 localStorage）→ 文案。顺序就是页面顺序。
  // 键名与 chart.js 的选项不是一一对应（丢包那个是逐 series 的，见 applyLatSeries），
  // 所以这里用一组自己的短名，别把图表的选项名直接当存储键用。
  var LAT_VIEW_ITEMS = [
    ['mean', '延迟'],
    ['loss', '丢包'],
    ['smooth', '平滑曲线']
  ];

  // LAT_VIEW_DEFAULT 是**没有存过**时的默认值：延迟（平均线）开、丢包竖条开、平滑关。
  //
  // 平滑关着：折线是本项目一直以来的画法，"平滑"是后加的选项，不该悄悄改掉
  // 所有人的默认视图。
  //
  // 这里曾经有一个 peak 项（"峰值线默认关"）。它随着那个 chip 一起删了 ——
  // 于是老浏览器 localStorage 里存着的对象会多出一个 `peak` 键，而 latView() 只
  // 遍历 LAT_VIEW_DEFAULT 的键去取值：**多出来的键被安全忽略**，少掉的键落回默认值，
  // 两头都不会抛错、也不会把开关读成 undefined（所以不需要换 PING_VIEW_KEY 的版本号，
  // 也用不着迁移代码）。下一次用户切任何一个开关时，写回去的就是这份三键对象，
  // 那个残留的 peak 键顺手就没了。
  var LAT_VIEW_DEFAULT = { mean: true, loss: true, smooth: false };

  // 三个 chip 的 DOM 引用（键 → <button>）：切换时只改高亮，不重建整行
  // —— 重建会把键盘焦点一起丢掉（用户按空格切一个开关，焦点就没了）。
  var latChipRefs = {};

  // LAT_CARD_HINT / LAT_CHIPS_HINT 是两个 ⓘ 的说明文本。
  //
  // 为什么用 title 属性而不是自建浮层：这两段是静态文案，不需要定位/夹取/跟随
  // 鼠标那一整套（迷你条的浮层是自建的，因为它要显示**动态读数**）。
  // title 还自带无障碍支持：键盘 Tab 到卡片或 chip 上时读屏会念出来。
  var LAT_CARD_HINT = '卡片上那一行数字的含义（全部由服务端算好）：\n' +
    '平均延迟：这一段里所有成功探测的加权平均（按成功次数加权；整分钟全丢的桶没有样本，不算进去）。\n' +
    '峰值：这一段里延迟最高的那一次探测。图上不再画峰值线，把鼠标移到曲线附近可以在\n' +
    '悬浮读数里看到每个桶的峰值。\n' +
    '丢包率：没能在超时时间内回来的探测，占全部探测的比例。\n' +
    '丢包率为 0 时不显示（绝大多数时候都是 0，每个目标都挂一句会把真有问题的那个淹掉）。\n' +
    '点这张卡片可以隐藏 / 显示这条曲线。';

  var LAT_CHIPS_HINT = '这三个开关决定「延迟」这张图上画什么：\n' +
    '延迟：画每个目标的平均延迟曲线。关掉后平均线不画（丢包竖条还在）。\n' +
    '丢包：在图底部画丢包竖条，越高丢得越多。\n' +
    '平滑曲线：把平均线画成单调三次平滑曲线（Fritsch–Carlson，不会过冲）；关掉就是折线。\n' +
    '峰值不在这三个开关里：峰值线不再画（Y 轴因此只按平均线缩放，曲线更撑满绘图区），\n' +
    '但把鼠标移到图上时，悬浮读数里仍然会列出每个桶的峰值。\n' +
    '开关状态存在本浏览器里，下次打开还是这个样子。';

  // latView 读当前开关状态。
  //
  // 逐个键落回默认值，而不是整份信任存着的那一坨：以后加了新开关，老浏览器里
  // 存下的对象缺那个键，这里要给默认值而不是 undefined（undefined 传进图表选项
  // 里会让 `opts.showMean !== false` 这类判断全部失效，行为随实现细节漂移）。
  //
  // 反过来说，存着的对象里**多出来的键**（比如删掉「峰值线」之前存下的 peak）
  // 根本不会被遍历到 —— 它不会被读进 view，也不会传进图表选项：老浏览器不会因为
  // 这个残留键报错或者白屏。setLatView 写回去的是这份三键对象，残留键就此消失。
  function latView() {
    var view = {};
    Object.keys(LAT_VIEW_DEFAULT).forEach(function (key) {
      view[key] = LAT_VIEW_DEFAULT[key];
    });
    try {
      var parsed = JSON.parse(localStorage.getItem(PING_VIEW_KEY) || 'null');
      if (parsed && typeof parsed === 'object') {
        Object.keys(LAT_VIEW_DEFAULT).forEach(function (key) {
          // 只认真正的布尔：存进去的是字符串 "false" 时不能当成真值。
          if (typeof parsed[key] === 'boolean') view[key] = parsed[key];
        });
      }
    } catch (err) { /* 隐私模式或内容被改坏：当作全是默认值 */ }
    return view;
  }

  function setLatView(view) {
    try {
      localStorage.setItem(PING_VIEW_KEY, JSON.stringify(view));
    } catch (err) { /* 存不下就只在本次会话里生效 */ }
  }

  // syncLatChips 把三个 chip 的高亮与 aria-pressed 同步成当前状态。
  function syncLatChips(view) {
    Object.keys(latChipRefs).forEach(function (key) {
      var on = !!view[key];
      latChipRefs[key].classList.toggle('active', on);
      latChipRefs[key].setAttribute('aria-pressed', on ? 'true' : 'false');
    });
  }

  function toggleLatView(key) {
    var view = latView();
    view[key] = !view[key];
    setLatView(view);
    syncLatChips(view);
    // 只重画曲线，不重建卡片：卡片只跟"哪个目标被隐藏"有关，与这三个开关无关。
    applyLatSeries();
  }

  // ---------------------------------------------------------- 目标卡片
  //
  // 每个探测目标一张小卡片，替代原来的勾选框：竖色条（该目标的线色）+ 名称 +
  // ⓘ + 一行统计。点卡片切换这条曲线的显示/隐藏，隐藏时整张卡片明显变灰
  // —— 只把勾去掉的话，扫一眼分不出"这条线被我关了"还是"这个目标没数据"。

  // latTargetText 是卡片上那一行统计：平均 · 峰值 · 丢包。
  //
  // 名字已经移到卡片标题行，所以这里不含名称。
  //
  // 区间聚合值由后端给：/ping 的 avg_ms（按成功探测次数加权）与 peak_ms
  // （曲线用的那批桶里 max 的最大值）。前端手里只有画曲线用的分桶点，自己算一遍
  // 就等于把服务端的口径再实现一次，两处迟早分叉 —— 而且"卡片上写的峰值"与
  // "悬浮读数里那一行峰值"必须是同一个数（都是这一批桶的 max）。
  //
  // 0 一律写「—」而不是 0：延迟有物理下限，0 只可能是"这一桶没有成功探测"
  // （没有样本），写 0 ms 会被读成"快得没有延迟"，与事实正好相反。
  function latTargetText(t) {
    if (!t.has_data) return '暂无数据';
    var text = t.avg_ms > 0 ? Math.round(t.avg_ms) + ' ms' : '—';
    text += ' · 峰值 ' + (t.peak_ms > 0 ? Math.round(t.peak_ms) + ' ms' : '—');
    // 丢包率用 fmtPct1（恒定一位小数）而不是 fmtPct（≥10% 会取整）：迷你条的
    // 丢包浮层也是 fmtPct1，两个百分比一个小数位一个整数位会看着像两种口径。
    if (t.loss_pct > 0) text += ' · 丢包 ' + fmtPct1(t.loss_pct);
    return text;
  }

  function pingColor(index) {
    return PING_COLORS[index % PING_COLORS.length];
  }

  // setLatCardOff 把一张卡片的"被隐藏"外观同步出来：整张变灰（CSS 的
  // .lat-card.off）**加上**无障碍状态。两个都要改 —— "变灰"对读屏用户是不可见的，
  // 而 aria-pressed 对看得见的人也是不可见的。
  function setLatCardOff(btn, off) {
    btn.classList.toggle('off', off);
    btn.setAttribute('aria-pressed', off ? 'false' : 'true');
  }

  // latCard 造一张目标卡片。
  //
  // 整张卡片是一个 <button>：它是"可点 + 可键盘操作（Tab 到、回车/空格触发）"
  // 的标准做法，不必自己接 keydown 去模拟。里面的元素只能是 <span>（button 的
  // 内容模型是短语内容），布局交给 CSS 的 flex/grid。
  function latCard(t, index, hidden) {
    var btn = document.createElement('button');
    btn.type = 'button';   // 显式写死：默认的 submit 在表单里会提交整个表单
    btn.className = 'lat-card';
    btn.title = pingTargetLabel(t);   // 名称可能被 CSS 截断（省略号），悬停看全称
    setLatCardOff(btn, !!hidden);

    var head = document.createElement('span');
    head.className = 'lat-card-head';

    // 左侧竖色条：用该目标自己的线色（与图上那条线同色）—— 目标多了靠颜色认人，
    // 这也是参考图里最显眼的那一笔。
    var bar = document.createElement('span');
    bar.className = 'lat-bar';
    bar.style.background = pingColor(index);

    var name = document.createElement('span');
    name.className = 'lat-card-name';
    name.textContent = pingTargetLabel(t);

    var info = document.createElement('span');
    info.className = 'lat-card-info';
    info.textContent = 'ⓘ';
    // 说明挂在这个 span 的 title 上（不做浮层组件）：ⓘ 是"这里有解释"的通用记号，
    // 悬停即可看到那一行四个数字各自是什么。
    info.title = LAT_CARD_HINT;

    head.appendChild(bar);
    head.appendChild(name);
    head.appendChild(info);

    var stats = document.createElement('span');
    stats.className = 'lat-card-stats';
    stats.textContent = latTargetText(t);

    btn.appendChild(head);
    btn.appendChild(stats);
    btn.addEventListener('click', function () {
      // 以**存储**为准决定这次是藏还是显示（而不是读 DOM 上的类）：
      // 存储是唯一的事实来源，DOM 只是它的投影。
      var willHide = !latHiddenSet()[String(t.id)];
      toggleLatTarget(t.id, willHide);
      setLatCardOff(btn, willHide);
    });
    return btn;
  }

  // latChips 造那一行全局开关：三个 chip + 一个说明的 ⓘ。
  function latChips() {
    var row = document.createElement('div');
    row.className = 'lat-chips';
    var view = latView();
    LAT_VIEW_ITEMS.forEach(function (item) {
      var key = item[0];
      var btn = document.createElement('button');
      btn.type = 'button';
      btn.className = 'chip';
      btn.textContent = item[1];
      btn.setAttribute('aria-pressed', view[key] ? 'true' : 'false');
      btn.classList.toggle('active', !!view[key]);
      btn.addEventListener('click', function () { toggleLatView(key); });
      latChipRefs[key] = btn;
      row.appendChild(btn);
    });
    var info = document.createElement('span');
    info.className = 'lat-chips-info';
    info.textContent = 'ⓘ';
    info.title = LAT_CHIPS_HINT;
    row.appendChild(info);
    return row;
  }

  // latHiddenSet 把 localStorage 里"被隐藏的目标 id"读成一张查询表。
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

  // renderLatToggles 渲染延迟图上方的控制区：**每个配置过的目标一张卡片**，
  // 下面跟着一行四个全局开关。默认全部显示（没数据的目标也在，只是它的线画不出来）。
  //
  // 函数名沿用旧名（原来是"一行勾选框"）：调用点与静态断言都按它找。
  function renderLatToggles(targets) {
    var box = el.latTargets;
    if (!box) return;
    box.textContent = '';
    latChipRefs = {};
    if (!targets.length) {
      // 一个目标都没配：整块收起（空态提示由 setLatEmpty 给）。
      box.hidden = true;
      return;
    }
    var hidden = latHiddenSet();
    var cards = document.createElement('div');
    cards.className = 'lat-cards';
    targets.forEach(function (t, i) {
      cards.appendChild(latCard(t, i, !!hidden[String(t.id)]));
    });
    box.appendChild(cards);
    // 开关排在卡片**下面**（参考图的顺序）：卡片是"看哪几条线"，开关是"这几条线
    // 怎么画"，先挑目标再调画法。
    box.appendChild(latChips());
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

  // LAT_SCALE_MIN_DELTAS 是"实测点距"至少要有几个差值才算得出来。
  //
  // 为什么是 3：中位数要能抗离群值，至少得让其中一个差值有可能被挡在外面。
  // 只有一个差值时"中位数"就是那个差值本身 —— 万一它正好落在一个缺口上，
  // 整条曲线就按缺口的宽度判，正常的点反而全被误判成断开；两个差值取中间两个的
  // 平均，同样没有抗性（见 latMeasuredSpacingSec）。所以少于 3 个就老实退回
  // "已知的三种原因"，不猜。
  var LAT_SCALE_MIN_DELTAS = 3;

  // latShownSeries 返回"这一帧真正要画的那几条曲线"（被隐藏的目标已经滤掉）。
  //
  // 为什么单独拎出来：断线判据要量的点距必须与**画出来的那串点**是同一批
  // （见 latSpacingDeltas），而 applyLatSeries 也要做同一件事 —— 两处各自维护
  // 一份"哪些目标被隐藏"的判断，迟早会让判据按一条根本没画的曲线去算。
  function latShownSeries() {
    var hidden = latHiddenSet();
    return detail.pingSeries.filter(function (s) {
      return !hidden[String(s.targetId)];
    });
  }

  // latSpacingDeltas 把"要画的这串点"里相邻两点的 ts 差值收集成一个数组。
  //
  // 必须是**手机端二次聚合之后**的点：detail.pingSeries 里存的就是
  // latencySeriesFor 聚合完的点数组，而 /ping 回来的原始点一个代表 60 秒、
  // 聚合之后可能代表 120 秒甚至 1800 秒。在聚合之前量，量到的是"服务端桶距"，
  // 比实际点距小 —— 聚合后的那些点就会被逐对判成缺口，曲线碎成一串圆点。
  //
  // 多目标时把所有目标的差值放进**同一个数组**（并集）再求中位数：同一档位下
  // 各目标的点距本该一致（同一份聚合目标、同一个探测间隔），不一致就说明这份
  // 数据有问题（某个 Agent 中途掉过线、上报节奏不齐）。并集的中位数比"挑一条
  // 曲线来量"稳（挑到谁全看运气），也比"逐目标算完再取某个极值"稳（取最小会被
  // 稀疏目标拽小 → 曲线碎成点；取最大会把真正的掉线连起来）。
  //
  // 只收 > 0 的有限差值：万一同一个桶里出现重复 ts（后端 GROUP BY 理论上不会，
  // 这里防御性地挡一下），差值是 0，混进去会把中位数往小里拽 —— 而"判据偏小"
  // 正是"曲线退化成一串孤立圆点"的那个方向。
  function latSpacingDeltas() {
    var deltas = [];
    latShownSeries().forEach(function (s) {
      var pts = s.points || [];
      for (var i = 1; i < pts.length; i++) {
        var d = pts[i][0] - pts[i - 1][0];
        if (isFinite(d) && d > 0) deltas.push(d);
      }
    });
    return deltas;
  }

  // latScaleMemo 缓存"实测点距中位数"。
  //
  // 为什么要缓存：latBucketSec() 的调用点（latChartOptions → 悬浮重绘那一帧）
  // 可能每帧都会走到，而每一次都要扫一遍全部曲线的点、再排序 —— 一条 7d 曲线
  // 上千个点时那就是每帧一次上千元素的排序。缓存的键就是**三件会影响这个数的
  // 事**，而且都是常数级比较（两个小值 + 一个引用）：
  //
  //   - series：detail.pingSeries 的**数组引用**。数据每次变化（/ping 回来、
  //     切档位、换节点、关详情页）都是整份新数组赋给它，引用一变缓存立刻作废，
  //     所以"数据刷新"不需要谁记得来通知这里；
  //   - hidden：隐藏了哪些目标（短签名）。用户点一下目标卡片就变 —— 要画的点
  //     换了，量出来的间距自然可能跟着变；
  //   - agg：手机端的二次聚合目标（mobileAggSec）。窗口跨过 640px 时它会变，
  //     点距也跟着变。
  var latScaleMemo = { series: null, hidden: '', agg: -1, value: 0 };

  // latHiddenSignature 是"当前隐藏了哪些目标"的短签名（缓存键的一部分）。
  // 排序是为了让键与顺序无关：{1:true,2:true} 与 {2:true,1:true} 是同一件事。
  function latHiddenSignature() {
    return Object.keys(latHiddenSet()).sort().join(',');
  }

  // latMeasuredSpacingSec 返回"实测的相邻点距中位数"（秒），测不出来时返回 0。
  //
  // 为什么取中位数而**不是平均值**：平均值会被离群值带偏 —— 一段 6 小时的曲线里
  // 只要有一个两小时的洞，平均值就被抬到几十分钟，于是真正的掉线（比如半小时）
  // 反倒小于"平均点距"，被判成正常间距连了起来，线横跨过那个洞。中位数对少数
  // 极端值免疫，它回答的是"大多数相邻点之间隔多久"。
  //
  // 返回 0 表示"没有实测值"（差值少于 LAT_SCALE_MIN_DELTAS 个：刚进详情页、
  // /ping 还没回来、数据只有一个点、或者目标全被隐藏了），交给 latBucketSec
  // 退回已知的那三项。**绝不猜一个数**：猜小了会把正常曲线切碎，猜大了会把掉线
  // 连起来，两种都比"不测"更糟。
  function latMeasuredSpacingSec() {
    var series = detail.pingSeries;
    var hidden = latHiddenSignature();
    var agg = mobileAggSec(detail.pingRange);
    if (latScaleMemo.series === series && latScaleMemo.hidden === hidden && latScaleMemo.agg === agg) {
      return latScaleMemo.value;
    }
    var deltas = latSpacingDeltas();
    var value = 0;
    if (deltas.length >= LAT_SCALE_MIN_DELTAS) {
      deltas.sort(function (a, b) { return a - b; });
      var mid = deltas.length >> 1;
      // 偶数个差值取中间两个的平均（中位数的标准定义）。只有两个差值时走不到
      // 这里（前面已经按 3 个起算），所以这条分支不会退化成"平均值"。
      value = deltas.length % 2 ? deltas[mid] : (deltas[mid - 1] + deltas[mid]) / 2;
    }
    latScaleMemo.series = series;
    latScaleMemo.hidden = hidden;
    latScaleMemo.agg = agg;
    latScaleMemo.value = value;
    return value;
  }

  // latBucketSec 返回延迟曲线断线判据（以及悬浮"附近"判据）该用的**时间尺度**（秒）。
  //
  // 它取四个值的最大者，因为四者都会让"相邻两个点之间隔多久"变大：
  //
  //   1. 服务端桶宽（detail.pingBucketSec，来自 /ping 的 meta.bucket_sec）——
  //      一个点代表多长时间；
  //   2. 手机端二次聚合目标（mobileAggSec）—— 聚合之后一个点代表的是聚合目标，
  //      比桶宽还大（6h 档 120 秒 > 桶宽 60 秒）；
  //   3. **探测间隔**（detail.pingIntervalSec，来自 /ping 设置的 interval_sec）——
  //      桶是"分钟格子"，但 Agent 是每隔 interval_sec 才探一次：间隔 300 秒而
  //      桶宽 60 秒时，每 5 个桶里只有 1 个有行，相邻两点的实际间距就是 300 秒；
  //   4. **实测的相邻点距中位数**（latMeasuredSpacingSec）——
  //      前三项都是"别人自报的数"，只能覆盖**已知**的三种原因。实测中位数不看
  //      任何自报值，直接量画出来的那串点，因此能兜住"没想到的第四种"：
  //      后端自报的桶宽与实际不符、Agent 上报节奏不规则、某个目标中途换了间隔……
  //      （Komari 当前版本的主题就是这么做的：拿相邻点时间戳相减取中位数。）
  //
  // 为什么必须取最大者：断线判据是"相邻两点间隔 > 1.5 × bucketSec 即断开"
  // （chart.js 的 linkedWithPrev）。拿一个比实际点距小的尺度去比，**每一对相邻点
  // 都会被判成缺口** —— 曲线退化成一串孤立圆点，一根线都画不出来，悬浮读数也
  // 大多落空（slack 太小）。实测值即使可用也只做"抬高"这一件事，不取代任何一项：
  // 保守优先，实测一旦偏小（比如点太少、中位数落在窄间距上），已知的三项仍然兜底。
  //
  // 退回路径：实测项在"点还不够"时是 0（见 latMeasuredSpacingSec），此时这个
  // 函数与加入实测之前**逐字等价** —— 不会因为"还没有数据"而崩掉或者返回 0
  // （探测间隔是可配的 10~3600 秒，这三项必然把它托住）。
  function latBucketSec() {
    var scale = mobileAggSec(detail.pingRange);
    if (detail.pingBucketSec > scale) scale = detail.pingBucketSec;
    if (detail.pingIntervalSec > scale) scale = detail.pingIntervalSec;
    var measured = latMeasuredSpacingSec();
    if (measured > scale) scale = measured;
    return scale > 0 ? scale : 0;
  }

  // latChartOptions 把三个开关翻译成图表选项（对应关系见 chart.js 顶部的说明）。
  //
  //   延迟     → showMean：平均线不画
  //   平滑     → smooth：平均线走单调三次插值（峰值线已经不画了）
  //   丢包     → 不在这里：竖条是逐 series 的（applyLatSeries 直接不给 bars）
  //   峰值线   → **没有开关了**：showMax 写死 false —— 淡线不画、Y 轴也不为峰值
  //              留空间（用户要的"曲线撑满绘图区"就是这一半）；但悬浮读数里的
  //              「峰值 N ms」那一行仍然要给，那是 hoverPeak（默认开）。
  function latChartOptions() {
    var meta = rangeMeta(detail.pingRange);
    var view = latView();
    return {
      yMax: 0,
      unit: ' ms',
      yFormat: function (v) { return v.toFixed(0); },
      // X 轴基准间隔优先取 /ping 自己的 meta.tick_base_sec（延迟图有自己那张档位表：
      // 桶宽与资源图不同），还没拿到时退回 /nodes 的 ranges 里同档位的那一份 ——
      // 两张表的基准间隔按同一张用户定稿的表，值相同（store 的测试钉住了这一点）。
      tickBaseSec: detail.pingTickBaseSec || meta.tick_base_sec || 60,
      // 档位与格式都取**延迟卡自己**的 detail.pingRange，不是资源卡的 detail.range：
      // 资源图停在 1d 而延迟图停在 1h 时，这里的刻度格式必须是 1h 那一套（HH:MM），
      // 否则一条一小时的曲线会按"1d"的格式每隔几分钟画一个 "09-29"。
      xFormat: RANGE_X_FORMAT[detail.pingRange] || clockOf,
      // 标签个数上限同理：取延迟卡自己那一档的（与 xFormat 同一个来源）。
      xLabelMax: RANGE_X_LABEL_MAX[detail.pingRange] || 0,
      showMean: view.mean,
      // 峰值线**永远不画**，Y 轴也不再为它留空间（chart.js 的 bounds 里就是这一句：
      // `opts.showMax && p[2] > vMax`）。这里不是"默认关"，是写死 —— 谁把它改回
      // 一个可配置项，用户抱怨的那片留白就会原样回来（峰值能到均值的 6 倍以上，
      // 轴一被顶上去，平均线那点起伏就压平了）。
      showMax: false,
      // 但悬浮读数里的峰值那一行**要留着**（用户原话："我需要保留移到上面的时候
      // 也能显示峰值"）。它与 showMax 在 chart.js 里是两个选项 —— 别合回去，
      // 合起来就只能二选一：要么轴上有留白，要么悬浮里看不到峰值。
      hoverPeak: true,
      smooth: view.smooth,
      // 断线判据要的是**相邻两点真正的间距**：桶宽 / 聚合目标 / 探测间隔 / 实测
      // 点距中位数，四者里最大的那个，理由见 latBucketSec。
      bucketSec: latBucketSec()
    };
  }

  // applyLatSeries 按当前"隐藏了哪些目标 + 三个开关"把曲线重新塞进图表实例
  // （不销毁重建：重建会连 canvas 上的鼠标监听与悬浮读数一起丢掉）。
  //
  // 每条 series 都带 targetId：显示/隐藏按 id 存，与曲线顺序无关，
  // 换个时间档位重画也不会错位。
  //
  // "要画哪几条"交给 latShownSeries()：断线判据量的就是它的这串点
  // （latMeasuredSpacingSec → latSpacingDeltas），两处用同一个筛选，
  // 判据才不可能按一条没画出来的曲线去算。
  function applyLatSeries() {
    var view = latView();
    var shown = [];
    latShownSeries().forEach(function (s) {
      // 「丢包」关掉时把 bars 摘掉：竖条是**逐 series** 的描述（丢包率只对探测目标
      // 有意义），与其在图表引擎里再加一个全局开关（引擎就要同时维护两套"画不画"
      // 的语义），不如在这里就不交给它 —— 引擎那边的约定保持"有 bars 就画"。
      shown.push(view.loss ? s : {
        targetId: s.targetId, label: s.label, color: s.color,
        points: s.points
      });
    });
    setChart('lat', 'chart-lat', null, null, latChartOptions(), shown);
  }

  // loadPingTargets 进详情页时取一次"配置了哪些探测目标"。
  //
  // 为什么不能只看 /ping 的返回：一个目标都没配时要**连请求都不发**、
  // 直接显示空态提示，而"到底有没有配"只有设置接口知道。
  //
  // 顺带把**探测间隔**也记下来：断线判据要用它（见 latBucketSec）。
  // 它与目标列表在同一个响应里，不必再请求一次（pingPayload 只做展示与编辑）。
  function loadPingTargets() {
    if (!chartVisible('lat')) {
      detail.pingTargets = null;
      return Promise.resolve();
    }
    return api('/api/v1/settings').then(function (data) {
      var ping = data.ping || {};
      detail.pingTargets = ping.targets || [];
      detail.pingIntervalSec = ping.interval_sec > 0 ? ping.interval_sec : 0;
    }).catch(function () {
      // 取不到就当作"不知道"：宁可不画，也不要退回 Agent 自己上报的 lat_ms ——
      // 那个数是到面板自身的往返，跟探测目标毫无关系，画上去就是误导。
      detail.pingTargets = null;
    });
  }

  // loadPingChart 画延迟图：一个探测目标一条线，取点的 avg（与 /series 一致，
  // [ts, avg, max, loss] 里前两个画曲线；max **只**用于悬浮读数里的峰值那一行，
  // 图上不再画峰值线）。
  //
  // 取点走 latencySeriesFor 而不是 seriesFor：延迟数据里 avg == 0 是"这一桶
  // 没有成功的探测"（见那里的注释），必须规范化成 null，否则曲线会在丢包处
  // 扎到 0ms。
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
    return api('/api/v1/nodes/' + detail.id + '/ping?range=' + encodeURIComponent(detail.pingRange)).then(function (data) {
      var targets = data.targets || [];
      setLatEmpty('');
      // 断线判据要用的桶宽只在这里拿得到（/ping 响应的 meta.bucket_sec）：Agent
      // 离线时根本不会有 ping_samples_1m 行，那些桶连点都不存在，光看"值是不是
      // null"是查不出来的 —— 必须拿桶宽去比相邻两点的 ts 间隔（见 chart.js 的
      // linkedWithPrev）。注意**不能**用 /nodes/{id} 里 ranges 的 bucket_sec：
      // 同一个档位下两者桶宽不同（1h 档分别是 60 与 10 秒），拿错了会把一条正常的
      // 曲线切得一段一段。
      var meta = data.meta || {};
      detail.pingBucketSec = meta.bucket_sec > 0 ? meta.bucket_sec : 0;
      // X 轴基准间隔也来自这一份 meta（延迟图自己那张档位表）：见 latChartOptions。
      detail.pingTickBaseSec = meta.tick_base_sec > 0 ? meta.tick_base_sec : 0;
      var series = [];
      targets.forEach(function (t, i) {
        // has_data:false 的目标不画线（服务端也会把它列出来），
        // 但下面的卡片里仍然要有它 —— "这个目标一个点都没有"本身就是信息。
        if (!t.has_data) return;
        var points = t.points || [];
        if (points.length === 0) return;
        series.push({
          targetId: t.id,
          label: pingTargetLabel(t),
          color: pingColor(i),
          points: latencySeriesFor(points),
          // valueIndex 指向点里的第 4 位（丢包率），max=100 表示满格。
          // 颜色不传：图表默认用该 series 自己的线色（半透明填充）。
          // 「丢包」开关关掉时 applyLatSeries 会把这一项摘掉再交出去。
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
    // 两组档位按钮都清掉：留着上一个节点的按钮会让人以为档位已经生效了
    // （档位表要等 /nodes/{id} 回来才知道，见下面的 renderRangeButtons）。
    el.detailRanges.textContent = '';
    el.latRanges.textContent = '';
    // 延迟图的状态一并清空：曲线、目标卡片、空态都不能留着上一个节点的。
    // pingTargets 置 null 表示"还不知道有没有配目标"，这时不请求 /ping。
    detail.pingTargets = null;
    detail.pingSeries = [];
    detail.pingBucketSec = 0;
    detail.pingTickBaseSec = 0;
    detail.pingIntervalSec = 0;
    renderLatToggles([]);
    setLatEmpty('');
    // 先按可见性把图表块藏好，再去请求数据：隐藏的图连一次请求都不发。
    applyChartVisibility();
    if (chartVisible('lat')) applyLatSeries();

    api('/api/v1/nodes/' + id).then(function (data) {
      // 详情接口同样带 server.timezone：直接深链到 #/n/<id> 时也能立刻拿到。
      setServerTimezone(data.server && data.server.timezone);
      detail.node = data.node;
      detail.uptime = data.uptime || {};
      detail.ranges = data.ranges || [];
      if (!detail.range && detail.ranges.length) detail.range = detail.ranges[0].key;
      // 延迟档位是**另一份**状态，同样要落回服务端给的档位表里（不在表里的话
      // 按钮高亮不出来，rangeMeta 也只能退回默认那档）。它是空值时跟着资源档位走
      // —— 默认一致，用户第一次打开不会以为"延迟图的档位没跟着切"。
      if (!detail.pingRange && detail.ranges.length) detail.pingRange = detail.range;
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
    detail.pingBucketSec = 0;
    detail.pingTickBaseSec = 0;
    detail.pingIntervalSec = 0;
  }

  // ---------------------------------------------------------------- 节点编辑 / 删除

  // 新增与编辑共用同一个对话框，靠这个变量区分。
  var nodeDialogMode = 'create';

  // CURRENCY_DEFAULT 是**新建**节点时货币下拉的默认值：人民币。
  //
  // 为什么默认值放在这里（JS 初始化）而不是 index.html 的 <option selected>：
  // 新增与编辑共用同一个对话框，而这是个有状态的控件 —— 上一次编辑一台美元机器
  // 会把它留在 USD 上，showModal() 并不会把它重置回 HTML 里那个默认值。
  // 只有"每次打开对话框都显式设一次"才能保证新建时看到的一定是人民币。
  // 编辑时则**原样**用节点自己的币种（含空值与怪币种，见 setCurrencyValue）。
  //
  // 它必须是"人民币的代码"本身（而不是另写一遍字面量）：默认币种就是人民币，
  // 两处各写一遍早晚会漂移 —— 改了这里忘了那里，新建出来的节点就默认成别的币种了。
  var CURRENCY_DEFAULT = CNY_CODE;

  // nodeDialogID 是"这次编辑的是哪个节点"。
  //
  // 以前保存时直接用 detail.id，而设置页的「服务器列表」也能打开这个对话框 ——
  // 那时详情页是关着的（detail.id = 0），保存会打到 /api/v1/nodes/0 上，
  // 表现是"保存按钮点了没反应，只有一行红字"。目标 id 必须跟着对话框自己走。
  var nodeDialogID = 0;

  // setCurrencyValue 把某个节点当前的币种回填到货币下拉（<select id="node-currency">）。
  //
  // 下拉里**没有空选项**（币种只有"选一个"，默认人民币），可库里确实存在两种
  // 下拉装不下的值：老数据里的怪币种（XYZ、USDT、被手工改过的值…），以及
  // **压根没填币种**的空串。两者的处理办法完全一样 —— 临时补一个选项让它显示出来，
  // 并且原样保存回去：
  //   ✗ 静默改成 CNY —— 那是改坏用户的数据（他只是打开编辑框看了一眼、点了保存，
  //     库里的空值/USD/XYZ 就变成人民币了，而且没有任何提示）；
  //   ✗ 直接 select.value = '' 了事 —— 没有匹配的选项时浏览器会把 selectedIndex
  //     设成 -1，下拉变成**一片空白**：用户既看不出它原来是"没设置"，保存时还会
  //     把一个空 value 当成他的选择。补一个「（未设置）」选项才看得见原值；
  //   ✗ 回退成文本框 —— 同一个字段两种控件，保存路径要分叉，样式也不一致。
  // 临时选项带 data-legacy 标记，下次打开对话框时先清掉，不会一条条攒起来。
  function setCurrencyValue(select, code) {
    Array.prototype.forEach.call(select.querySelectorAll('option[data-legacy]'), function (opt) {
      opt.parentNode.removeChild(opt);
    });
    var want = String(code || '').trim().toUpperCase();
    var found = null;
    for (var i = 0; i < select.options.length; i++) {
      if (select.options[i].value === want) { found = select.options[i]; break; }
    }
    if (!found) {
      found = document.createElement('option');
      found.value = want;
      // 文案写明它是库里的原值：用户看到"XYZ（库里的原值）"就知道这个币种不在
      // 常用列表里，而不是以为界面把它选错了；空值写「（未设置）」——
      // 让"这台机器没填过币种"这件事在编辑框里看得见，而不是一片空白。
      found.textContent = want === '' ? '（未设置）' : want + '（库里的原值）';
      found.dataset.legacy = '1';
      select.appendChild(found);
    }
    select.value = want;
  }

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
    // 货币是下拉。**新建**时默认人民币（CURRENCY_DEFAULT）；**编辑**时把库里的原值
    // 原样回填 —— 空值与下拉里没有的币种都会临时补一个选项（见 setCurrencyValue），
    // 绝不在这里把它们改成别的币种。
    setCurrencyValue(el.nodeCurrency, mode === 'create' ? CURRENCY_DEFAULT : (d.currency || ''));
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
    // 到期日回填：按**服务端时区**把 epoch 取成 yyyy-mm-dd。
    // 这里曾经是 new Date(...).toISOString().slice(0, 10)（UTC 日期）：
    // 东八区下存进去的"本地零点"被当成 UTC 读回来，每打开一次编辑框就往前挪一天，
    // 保存之后越存越早 —— 一条会改坏数据的 bug。
    el['node-expires'].value = d.expires_at ? dayOf(d.expires_at) : '';
    el.dlgNode.showModal();
  }

  function nodeFormPayload() {
    var expires = el['node-expires'].value;
    // 到期日是"日历上的一天"，存进库的是一个 epoch。这一对换算必须与后端切天用
    // **同一个时区**（这里求的是服务端时区下那一天的零点），否则同一个日期在两端的
    // 归属日不同。曾经写的是 new Date(expires + 'T00:00:00')（**浏览器本地零点**），
    // 而读回时用的是 UTC 日期 —— 东半球下"输入 2026-10-01、回读 2026-09-30"。
    // 详见上面「日期 ↔ epoch」那一整段。
    var expiresAt = 0;
    if (expires) {
      expiresAt = parseDateInput(expires);
    }
    // 元 → 分：先四舍五入到整数分，避免 71.21 变成 7120.999999 再被截断成 7120。
    var priceCents = Math.round((parseFloat(el['node-price'].value) || 0) * 100) || 0;
    var billingMonths = parseInt(el.nodeBilling.value, 10) || 0;
    return {
      name: el['node-name'].value.trim(),
      group_name: el.nodeGroup.value.trim(),
      region: el.nodeRegion.value.trim(),
      note: el.nodeNote.value.trim(),
      interval_sec: parseInt(el.nodeInterval.value, 10) || 1,
      price_cents: priceCents,
      // 货币：下拉的 value 就是币种代码（空串 = 库里没填）。下拉里装不下的值 ——
      // 老币种（XYZ）与空值 —— 由 setCurrencyValue 临时补成一个选项，这里**原样**
      // 发回去：用户只是打开编辑框看了一眼再保存，库里的币种不该被改成别的
      // （尤其不能变成 CNY），空值也不该被顺手填上一个。
      //
      // 唯一的例外：**没有计费周期**时一律发空串。服务端有一条硬规则 ——
      // "没有计费周期时，价格与货币都必须留空"（见 store.Node.Validate）——
      // 而下拉现在默认选人民币，不做这个判断的话，新建一台**不填价格**的机器
      // 会被服务端直接拒掉（用户什么都没做错，却建不出来）。
      // 没有价格就没有币种可言，这也不是改写用户数据：这种机器库里存的本来就是空串
      // （改动之前下拉的默认值就是空，两条路的结果完全一样）。
      currency: billingMonths > 0 ? el.nodeCurrency.value.trim().toUpperCase() : '',
      billing_months: billingMonths,
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
    // 整条一起收（不只是按钮）：没有「加载更多」时它只剩一条多余的分隔线 +
    // 一句"每次 50 条"，在空列表下面看着像是页面出了错。
    el.auditFoot.hidden = true;
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
      // 「加载更多」在 .list-foot 里，整条跟着它一起显示/隐藏（见 resetAudit）。
      el.auditFoot.hidden = el.auditMore.hidden;
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
      // 三个定时流量报告的开关：与 Telegram 配置同一个响应（同一个接口），
      // 所以进设置页时它们和上面那几个框是同一时刻的值，不会出现"半新半旧"。
      el.notifyDaily.checked = !!cfg.daily_report;
      el.notifyWeekly.checked = !!cfg.weekly_report;
      el.notifyMonthly.checked = !!cfg.monthly_report;
      el.tgTokenHint.textContent = cfg.has_token
        ? '已经保存过 Token；留空表示不修改。'
        : '还没有保存 Token。';
      // 「有没有存过 Token」从说明句子里提出来，变成 label 右边的状态徽章：
      // 说明句只留"我该怎么做"，状态由徽章回答（见 index.html 的 .field-label .tag）。
      el.tgTokenTag.hidden = !cfg.has_token;

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
      // 服务端信息卡上的「时区」就是页面时间用的那个时区，两边必须同源。
      setServerTimezone(info.timezone);
      // 「服务端参数」是 5 列 × 2 行的网格，每格是"小标签在上 + 值在下"的 .kv-cell。
      // 为什么不再成对追加 dt/dd：.kv-grid 是 grid，**每个子元素各占一格** ——
      // dt 与 dd 会被拆到相邻两格里，标签和它的值直接分家。
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
        el.serverInfo.appendChild(kvCell(row[0], row[1]));
      });

      // 汇率单独一组：它的值是一整句话（"内置兜底（从没成功取到过实时汇率…）"），
      // 塞进 5 列网格里每格只剩 250px、一句话要折三行 —— 所以改用"标签定宽在左、
      // 值在右"的行式布局（.kv-row）。
      el.fxInfo.textContent = '';
      fxRows(all.fx).forEach(function (row) {
        el.fxInfo.appendChild(kvRow(row[0], row[1]));
      });
    }).catch(function (err) {
      // 拉不到设置时把错误落在当前栏里，而不是只弹一个转瞬即逝的 toast：
      // 用户需要知道"现在这些框里显示的不是服务端的值"。
      var node = paneErrorNode(target);
      if (node) node.textContent = '读取设置失败：' + err.message;
      toast(err.message);
    });
  }

  // fxRows 把服务端下发的汇率元信息（GET /api/v1/settings 的 fx）拼成「服务器信息」
  // 里的几行。
  //
  // 为什么必须让用户看见这三件事：价格上凭空多出来的那个 ¥ 数字如果不写清出处
  // （哪一天的、从哪取的），没人敢拿它对账；而"兜底"两个字更重要 —— 内置兜底表是
  // **写死在程序里的量级估计，不是实时值**，拿它当实时汇率去比价会得出错误结论。
  function fxRows(fx) {
    fx = fx || {};
    var state;
    if (fx.enabled === false) {
      state = '已关闭自动获取（--fx=false）：一直用上次取到的值，没有则用内置兜底';
    } else if (fx.is_default) {
      state = '内置兜底（从没成功取到过实时汇率，换算结果仅供参考）';
    } else {
      state = '每天自动获取';
    }
    var rows = [
      ['汇率数据', state],
      ['汇率日期', fx.date || '—'],
      ['汇率来源', fx.source || '内置兜底表（写死在程序里，不是实时值）']
    ];
    // 取得时间只在真取到过时才显示（兜底表的 fetched_at 是 0，写"50 年前"很荒唐）。
    if (fx.fetched_at > 0) rows.push(['汇率取得于', fmtAgo(fx.fetched_at)]);
    return rows;
  }

  // kvCell 造只读信息网格里的一格：**小标签在上、值在下**。
  //
  // 为什么不是一个 <dl> 里的 dt/dd：.kv-grid 是 grid 布局，每个子元素各占一格，
  // dt 与 dd 会被排到相邻的两格里 —— 标签和它的值就分家了。
  // 一对值包进同一个 .kv-cell，网格才排得对。
  function kvCell(label, value) {
    var cell = document.createElement('div');
    cell.className = 'kv-cell';
    cell.appendChild(kvText('kv-k', label));
    cell.appendChild(kvText('kv-v', value));
    return cell;
  }

  // kvRow 造"标签定宽在左 + 值在右"的一行，给一整句话的长值用（汇率那几项）。
  function kvRow(label, value) {
    var row = document.createElement('div');
    row.className = 'kv-row';
    row.appendChild(kvText('kv-k', label));
    row.appendChild(kvText('kv-v', value));
    return row;
  }

  // kvText 造上面两种格子里共用的一小段文字（一律 textContent 赋值，
  // 与全站其它渲染路径一样，不用任何 HTML 注入 API）。
  function kvText(className, text) {
    var node = document.createElement('span');
    node.className = className;
    node.textContent = text;
    return node;
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

  // saveNotify 只保存「通知」这一栏。三个保存函数互不依赖：
  // 以前是一个按钮串行 PUT 三个接口，第一个失败后面两个就不会发出去，
  // 用户以为"全没存上"，其实只是其中一段的参数写错了。
  //
  // 定时流量报告的三个开关也在这个请求里：报告走的就是这条 Telegram 渠道，
  // 拆成第二个请求的话这一栏就得有两个保存按钮（或者又回到"一个按钮发两次"）。
  function saveNotify() {
    el.notifyError.textContent = '';
    el.notifyOk.textContent = '';
    el.notifySave.disabled = true;

    var telegram = {
      enabled: el.tgEnabled.checked,
      bot_token: el['tg-token'].value.trim(),
      chat_id: el.tgChat.value.trim(),
      daily_report: el.notifyDaily.checked,
      weekly_report: el.notifyWeekly.checked,
      monthly_report: el.notifyMonthly.checked
    };

    api('/api/v1/settings/telegram', { method: 'PUT', body: telegram }).then(function (cfg) {
      // 保存成功后立刻清空 Token 输入框并把 hint 换成最新的：
      // Token 只用于"覆盖"，留在页面上等于把密钥摊在屏幕上。
      el['tg-token'].value = '';
      el.tgTokenHint.textContent = cfg.has_token
        ? '已经保存过 Token；留空表示不修改。'
        : '还没有保存 Token。';
      el.tgTokenTag.hidden = !cfg.has_token;
      // 三个开关按服务端**回显**的值回填：服务端读回来的是什么就显示什么，
      // 免得界面上勾着而库里其实是另一个值。
      el.notifyDaily.checked = !!cfg.daily_report;
      el.notifyWeekly.checked = !!cfg.weekly_report;
      el.notifyMonthly.checked = !!cfg.monthly_report;
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
    // 类名是 ping-enable 而不是通用的 check：设置页那一行现在是 grid 的**一格**
    // （六列 = 名称 / 类型 / 地址 / 端口 / 启用 / 删除），要跟 34px 高的输入框
    // 在同一格里垂直居中；通用 .check 自带的下外边距会把它顶偏。
    // 复选框本身的行为一点没变，只是外观归 .ping-row .ping-enable 管。
    var enabledLabel = document.createElement('label');
    enabledLabel.className = 'ping-enable';
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
    // 探测间隔在这里也记一份（详情页的断线判据要用，见 latBucketSec）：
    // 用户刚在设置里把它从 60 改成 300，延迟图不该等到下次进详情页才反应过来。
    detail.pingIntervalSec = ping.interval_sec > 0 ? ping.interval_sec : 0;
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
    //
    // 主页按分组筛选时这个把手停用（把手上加 .off 变灰 + title 写明原因）：
    // 松手时算出来的落点是**筛选后那一屏里**的位置，而 PUT /nodes/order 收的是
    // 完整顺序 —— 写回去会把没显示的机器顺序搅乱（详见 reorderLocked）。
    // 停用必须在**界面上看得出来**：一个还能按、按了没反应的把手，用户只会
    // 以为拖动坏了。
    var handle = document.createElement('div');
    var locked = reorderLocked();
    handle.className = locked ? 'node-drag off' : 'node-drag';
    handle.title = locked
      ? '已按分组「' + groupLabel(groupFilter) + '」筛选，拖动排序暂时停用：这时算出的落点是筛选结果里的位置，写回去会打乱没显示出来的机器。切回「全部」再拖。'
      : '按住拖动调整顺序';
    handle.setAttribute('aria-label', locked ? '拖动排序（已停用）' : '拖动排序');
    handle.setAttribute('aria-disabled', locked ? 'true' : 'false');
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
    // 剩余价值只在**填过价格、而且还没过期**时才有意义，两个条件缺一不可：
    //   · 没填价格：remaining_value_cents 恒为 0，显示成「剩余价值 ¥0.00」会被读成
    //     "这台机器一文不值"；
    //   · 已过期：剩余天数归零 ⇒ 价值必然是 0，写出来同样是废话（用户要的就是
    //     这种情况**整段省略**，不是显示 ¥0.00、也不是显示 —）。
    // 判据是服务端的 node.expired，不是 remaining_days <= 0 —— 后者在"还剩 5 小时"
    // 时也是 0，而那台机器没过期（见 dto.go 的 Expired）。这一段的显隐与「分组」
    // 一样：不显示就是整段不出现，不留占位符。
    // 金额走 nodeMoney（与首页卡片的「费用」行、详情页顶部那三格同一口径）：一律人民币。
    if (node.price_cents > 0 && !node.expired) {
      pieces.push(document.createTextNode('剩余价值 ' + nodeMoney(node, 'remaining_value_cents', 'remaining_value_cny_cents')));
    }
    // 到期文案用服务端给的 expires_text（与首页卡片、详情页「到期」那一格、
    // 告警消息共用同一个函数）：前端拼 "N 天后到期" 时，不足 24 小时与已过期
    // 都会写成「0 天后到期」，那正是这一轮要修的 bug。
    if (node.expires_at > 0 && node.expires_text) {
      pieces.push(document.createTextNode(node.expires_text));
    }

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
    // 分组筛选生效时不许拖（见 reorderLocked）。界面那边已经把把手画成灰的，
    // 这里是同一道闸门的代码一侧：合成事件、键盘或以后新加的入口都绕不过去。
    if (reorderLocked()) return;

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

    // 分组筛选的选择也是本浏览器的偏好（与主题、延迟图那几个开关一样）：
    // 在第一帧渲染之前读出来，刷新后停在原来那一组上。
    groupFilter = loadGroupFilter();

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