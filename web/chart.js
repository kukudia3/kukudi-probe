'use strict';

/* 极简 VPS 探针 —— 图表引擎
 *
 * 自己写的 canvas 折线图，不引入任何第三方图表库：
 * 需要的能力只有"定时序折线 + 固定刻度 + 悬浮读数"，为此拉一个库进来不划算。
 * 对外只暴露 ProbeChart.create(canvas, options)。
 *
 * options:
 *   series:   [{label, color, points: [[ts, avg, max], ...], showMax: bool,
 *               bars: {valueIndex, max, color}}]
 *   tickBaseSec: X 轴**基准**间隔（秒）——标签锚点之间的间隔，由后端按档位给。
 *             它只是起点：屏幕上实际画多少个标签由本引擎按标签文本的真实宽度
 *             自动按**整齐倍数**放大（见 X_STEP_MULTIPLIERS / xLabelStep）。
 *             这里曾经还有一个 tickLabelSec（"实际标签 + 竖网格线"的间隔），
 *             现在两个概念合并成一个：竖网格线已经整条删掉，服务端也不再预先抽稀。
 *   yMax:     固定 Y 轴上限（百分比图传 100）；不传则自动取"好看的刻度"
 *   yFormat:  刻度与读数的格式化函数
 *   xFormat:  X 轴标签格式化函数
 *   timeZone: 渲染时间用的 **IANA 时区名**（如 "Asia/Shanghai"），由服务端下发。
 *             引擎自己格式化时间的地方（桶宽 ≥ 1 小时时悬浮读数里的区间端点、
 *             以及"两端是不是同一天"的判定）全部按它渲染；X 轴标签的格式由
 *             xFormat 决定，而调用方给的那个 xFormat 也必须按同一个时区渲染
 *             —— 见文件末尾「时区」那一段的说明。
 *             留空（老接口没下发、或名字当前浏览器不认识）时退回浏览器本地时区。
 *   unit:     读数单位（tooltip 用）
 *   showMean: 画不画"主曲线"（valueIndex 1）。默认画。延迟图的「延迟」开关用它。
 *   showMax:  画不画峰值淡线（valueIndex 2），**并且**决定 Y 轴要不要把峰值算进去
 *             （见 bounds）。默认画。延迟图的「峰值线」开关用它 —— 关掉它之后
 *             轴只按主曲线的高度自适应，这正是用户要的"取消峰值线后 Y 轴自适应"。
 *   smooth:   true 时主曲线与峰值线都画成**单调三次**平滑曲线（见 drawRun），
 *             默认 false（折线）。延迟图的「平滑曲线」开关用它。
 *   bucketSec: 桶宽（秒）。两个用处：判断"两点之间缺了多少个桶"（见 linkedWithPrev），
 *             以及悬浮读数里那段时间区间（见 hoverStampText）。0 表示未知 ——
 *             那时只按"值是不是 null"断线，悬浮也只写一个时刻。
 *
 * bars 是可选的"竖条"描述：从绘图区底边往上画（延迟图用它画每个桶的丢包率）。
 * valueIndex 指向点数组里的第几个元素，max 是满格对应的值。不传 bars 的 series
 * 与以前完全一致 —— CPU/内存/磁盘/网络/流量五张图都不受影响。
 *
 * 这里曾经还有第三个可选描述 slow（"超过阈值的那一段画成红色"）：用户明确不要
 * "慢"这个概念了，红线、阈值、判定规则与那个选项一起删干净了（连带前端那些
 * SLOW_* 常量）。丢包竖条留着 —— 那是真丢包，与延迟高低是两件事。
 */

(function () {
  // currentDPR 现读当前的物理倍率，**不在模块加载时缓存**。
  //
  // 把窗口从 1× 屏拖到 2× 屏、改系统缩放、或只改本窗口的浏览器缩放，这个值都会变；
  // 缓存下来的那一份会让图表继续按旧倍率分配位图 —— 表现是"换到另一块屏之后又糊了，
  // 刷新一下就好"，属于最难查的那类问题。
  function currentDPR() {
    var dpr = window.devicePixelRatio || 1;
    return dpr > 0 ? dpr : 1;
  }

  // 丢包竖条的三条硬指标。
  //
  // 为什么单独拎出来：丢包是"稀疏且数值很小"的信号，条太窄/太淡就等于没画。
  // 尤其是 1% 的丢包 —— 按比例算出来不到 1px，而"这里丢过包"恰恰是
  // 最需要一眼看到的信息，所以给一个 3px 的最小可见高度。
  var BAR_ALPHA = 0.45;
  var BAR_MAX_W = 14;
  var BAR_MIN_W = 2;
  var BAR_MIN_H = 3;

  // 孤立数据点的半径。
  //
  // 断线之后"孤立的一点"不再罕见：一段缺口的两侧可能各只剩一个桶有样本
  // （Agent 刚上线一分钟又断了）。这种点用 moveTo + stroke 什么都画不出来
  // （路径里只有一个点，没有线段可描边），必须单独画成一个点，
  // 否则那条真实存在的读数就被静默丢掉了。
  var SOLO_DOT_R = 1.8;

  // 断线的间隔判据：相邻两点的 ts 间隔超过 **1.5 个桶宽** 就算断开。
  //
  // 为什么是 1.5 而不是 1：点的时间戳都落在服务端的桶网格上，正常相邻两点的间隔
  // 正好是一个桶宽；留半个桶的余量是为了容忍对齐/取整带来的秒级偏差 ——
  // 掐着 1 倍写，某次对齐差 1 秒就会把整条曲线碎成一段一段。
  var GAP_BUCKET_RATIO = 1.5;

  // ---------------------------------------------------------------- X 轴标签间隔
  //
  // 标签锚点固定在**绝对时间网格**上（ts 取整到间隔的倍数），窗口滑动时标签
  // 只会有 ±1 的差别；间隔本身从后端给的"基准间隔"出发，放不下就自动稀疏。

  // X_LABEL_MIN_GAP 是相邻两个标签之间**至少要留**的像素。
  //
  // 为什么不是"不重叠就行"：紧紧挨着的两个 "12:34" 看上去是一串数字，读的人得先
  // 猜哪里断开；11px 的小字本身就需要一点空白才扫得快。一个 "HH:MM" 标签大约
  // 30px 宽，留 28px 的空白（差不多与标签本身一样宽）读起来才不费劲。
  //
  // 这个数还决定了"宽画布上会稀出多少个标签"：延迟图那张卡片是整行宽的
  // （约 1500px 绘图区），间距给到 8px 时它会稀出 30~36 个标签 —— 虽然没有重叠，
  // 但已经没人会去逐个读了；给到 28px 就自然收在 24 个以内。
  var X_LABEL_MIN_GAP = 28;

  // X_STEP_MULTIPLIERS 是稀疏时允许用的**整齐倍数**：实际间隔 = 基准间隔 × 其中之一。
  //
  // 为什么必须是这几个数：读图的人要能一眼换算"相邻两根刻度差多久"。1/2/5/10/15/30/60
  // 是钟表上本来就有的分档（15 分钟、半小时、1 小时），60 之后沿 1-2-5 继续
  // （120、300…），仍然是整数小时。
  //
  // 为什么不能按整数倍递增（1,2,3,4…）：那样会冒出"每 7 分钟"这种刻度 ——
  // 钟表上没有这一档，读者要心算才知道两条线之间是多久；换个基准间隔又会得到
  // 另一批同样别扭的数字。
  //
  // 为什么用倍数而不是写死一串秒数：基准间隔是后端按档位给的（1h 档 60 秒、
  // 7d 档 900 秒）。倍数化之后，无论基准是多少，"稀疏出来"的都是它的整数倍，
  // 标签因此仍然落在绝对时间网格上 —— 锚点稳定这条设计不会被破坏。
  var X_STEP_MULTIPLIERS = [1, 2, 5, 10, 15, 30, 60, 120, 300, 600, 1200, 3000];

  var COLORS = {
    grid: 'rgba(128,128,128,0.22)',
    axis: 'rgba(128,128,128,0.45)',
    // text / empty 是刻度与空态文字的**兜底色**，实际取值见 textColor()：
    // 原来硬编码成 #8b939f，它在白色面板上对比度只有约 3.1:1（无障碍标准要 4.5:1），
    // 11px 的小字用这个颜色看上去就是"发虚、糊"。而且它不跟随主题 ——
    // 深色下这个灰正好，浅色下就太浅。改成读 CSS 的 --fg-muted：
    // 浅色 #67707c（4.86:1）、深色 #98a1ad，两边都达标。
    text: '#8b939f',
    tooltipBg: 'rgba(20,22,26,0.92)',
    tooltipFg: '#f2f4f7',
    empty: '#8b939f'
  };

  // textColor 返回当前主题下刻度文字的用色。
  //
  // 为什么要缓存：读取 CSS 自定义属性要走 getComputedStyle，会强制一次样式重算，
  // 而 draw() 在悬浮时会跟着 mousemove 每次重画 —— 每帧都读一次太浪费。
  // 这里只在"主题真的换了"时才重读。主题有两个来源：显式的 data-theme 属性，
  // 以及跟随系统的 prefers-color-scheme（见 style.css），两者都要认。
  var themeCache = { key: null, color: COLORS.text };

  function currentThemeKey() {
    var attr = document.documentElement.getAttribute('data-theme');
    if (attr === 'dark' || attr === 'light') return attr;
    if (window.matchMedia && window.matchMedia('(prefers-color-scheme: dark)').matches) {
      return 'dark';
    }
    return 'light';
  }

  function textColor() {
    var key = currentThemeKey();
    if (key === themeCache.key) return themeCache.color;
    var value = '';
    try {
      value = getComputedStyle(document.documentElement)
        .getPropertyValue('--fg-muted').trim();
    } catch (e) {
      value = ''; // 极老的浏览器没有 getComputedStyle 的这条路径，退回硬编码色
    }
    themeCache.key = key;
    themeCache.color = value || COLORS.text;
    return themeCache.color;
  }

  function niceCeil(value) {
    if (!(value > 0)) return 1;
    var exp = Math.floor(Math.log10(value));
    var base = Math.pow(10, exp);
    var norm = value / base;
    var step = norm <= 1 ? 1 : norm <= 2 ? 2 : norm <= 2.5 ? 2.5 : norm <= 5 ? 5 : 10;
    return step * base;
  }

  function pad2(n) { return (n < 10 ? '0' : '') + n; }

  // ---------------------------------------------------------------- 时区
  //
  // 图上的时间**一律按服务端时区渲染**，不用浏览器本地时区。
  //
  // 为什么：后端的时间桶是按 --timezone 切的 —— 尤其是"近 7 天流量"那种按天的点，
  // 时间戳就是**服务端时区的本地零点**（见 server/api_traffic.go 的 startDay）。
  // 拿浏览器本地时区去渲染它，两端时区不一致时整条日轴就会差一天：
  // 服务端 UTC+8、浏览器 UTC+0 时，服务端 10-02 那个点会被画在 10-01 16:00 的位置上，
  // 轴标签跟着写成 10-01，而这一天的数字其实来自 10-02。
  // 图上的每一个时刻（X 轴标签、悬浮区间、跨天判定）因此共用同一个时区，
  // 与页面上的审计时间、总览条「更新于」保持同一口径。
  //
  // 缓存：Intl.DateTimeFormat 的构造很贵，而这段代码在**每帧绘制路径**上
  // （悬浮时每次 mousemove 都会重画一次）。这里一次取全（年月日时分秒），
  // 所以"一个时区只需要一个实例"—— 缓存键就是时区名，所有格式共用它。
  // 建不出来（时区名非法 / 老浏览器没有 Intl）时把 null 也缓存下来：
  // 不缓存的话每次格式化都要重新构造+抛一次异常。
  var FORMAT_SPEC = {
    year: 'numeric', month: '2-digit', day: '2-digit',
    hour: '2-digit', minute: '2-digit', second: '2-digit',
    // hourCycle 显式写 h23：hour12:false 在个别实现/语言下会给出 "24:00"，
    // 那会让 00:15 这类标签写成 "24:15"，并且把日期也带错一天。
    hourCycle: 'h23'
  };

  var formatterCache = {};

  function formatterFor(timeZone) {
    var key = timeZone || '-';
    if (key in formatterCache) return formatterCache[key];
    var fmt = null;
    try {
      var spec = {
        year: FORMAT_SPEC.year, month: FORMAT_SPEC.month, day: FORMAT_SPEC.day,
        hour: FORMAT_SPEC.hour, minute: FORMAT_SPEC.minute, second: FORMAT_SPEC.second,
        hourCycle: FORMAT_SPEC.hourCycle
      };
      // 不传 timeZone 就是"浏览器本地"，那正是拿不到时区时的退路。
      if (timeZone) spec.timeZone = timeZone;
      fmt = new Intl.DateTimeFormat('en-US', spec);
    } catch (e) {
      fmt = null;
    }
    formatterCache[key] = fmt;
    return fmt;
  }

  // localFields 是"拿不到服务端时区"时的退路：浏览器本地时区的各字段。
  // 字段名与 zoneFields 完全一致，调用点因此不需要写两套分支。
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

  // zoneFields 把 epoch（秒）拆成"该时区下的年月日时分秒"，全部是补零后的字符串。
  function zoneFields(timeZone, ts) {
    var d = new Date(ts * 1000);
    var fmt = formatterFor(timeZone);
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

  // HOVER_INTERVAL_MIN_SEC 是"悬浮读数要写成 [起点, 终点) 区间"的桶宽下限（秒）。
  //
  // 为什么是 60：一条读数代表的是**一段时间的平均**，只写一个时刻会被读成
  // "这一刻的值"；桶越宽，这个误读越离谱。这条线以前划在 3600（1 小时）——
  // 那是拿延迟图**当时**那张桶宽表的最大一档（7d = 3600 秒）当的上界。
  // 桶宽改细之后 7d 变成 900，它就掉到线外面去了：**7d 档的悬浮从区间退回了
  // 单个时刻** —— 用户要的"看得到这一段有多长"被悄悄抵消一半，而页面上
  // 一点异常都看不出来（读数照样有，只是少了一半信息）。
  //
  // 教训是"阈值不能钉在某一版桶宽表上"。所以这里改成钉在**本项目最细的历史桶**
  // （1 分钟，见 ping_samples_1m：探测结果一行就是 1 分钟）上：
  // 比 1 分钟还粗的桶一律写成区间。这样
  //   - 1h/6h/12h（桶宽 = 60 秒，正好一格）仍然是简洁的单个 "12:34"；
  //   - 1d(120) / 3d(300) / 7d(900)，以及手机端聚合后的任意更大点距，都写区间；
  //   - 以后把某一档的桶宽又改细/改粗，只要它比 1 分钟粗，这个功能就不会再丢。
  // 代价只是浮层第一行多了几个字符（"12:34–12:35"），换来的是一句不含糊的话。
  var HOVER_INTERVAL_MIN_SEC = 60;

  function create(canvas, options) {
    var opts = {
      series: [],
      tickBaseSec: 600,
      yMax: 0,
      yFormat: function (v) { return String(Math.round(v)); },
      // 默认标签格式：HH:MM，**按服务端时区**渲染（见文件末尾「时区」那一段）。
      // 调用方自己给了 xFormat 时用它的 —— 那个也必须按同一个时区渲染。
      xFormat: function (ts) { return clockHM(ts); },
      timeZone: '',
      unit: '',
      showMean: true,
      showMax: true,
      smooth: false,
      bucketSec: 0
    };
    apply(options);

    var state = { hoverX: null };

    function apply(next) {
      if (!next) return;
      for (var key in next) {
        if (Object.prototype.hasOwnProperty.call(next, key)) opts[key] = next[key];
      }
    }

    // layout 是**唯一**处理 DPR 的地方：量 CSS 尺寸 → 按物理倍率分配位图 →
    // 把坐标系缩回 CSS 像素。这一处做对了，下面所有绘制代码（坐标、线宽、字号）
    // 一个字都不用改，因为 setTransform 之后坐标系就是 CSS 像素。
    //
    // 不写 canvas.style.width / height 是**刻意**的：显示尺寸由 style.css 的
    // canvas.chart（width:100% + 媒体查询里的高度）决定，而行内样式会盖掉它 ——
    // 一旦把"当前像素宽"写进行内样式，canvas 就再也不跟着容器缩放了：下次
    // getBoundingClientRect() 读到的还是那个写死的值，等于自己把自己钉住。
    // 位图按 devicePixelRatio 放大不会改变显示尺寸（CSS 尺寸与它无关），所以
    // 高分屏清晰的代价只是显存，不是布局。
    function layout() {
      var rect = canvas.getBoundingClientRect();
      var width = Math.max(120, Math.round(rect.width));
      var height = Math.max(80, Math.round(rect.height));
      var dpr = currentDPR();
      var bitmapW = Math.round(width * dpr);
      var bitmapH = Math.round(height * dpr);
      // 只在真的变了时才赋值：给 canvas.width 赋值（哪怕值一模一样）会重新分配
      // 整张位图并把它清空，而 layout() 每次 draw() 都要跑 —— 悬浮读数时每动一下
      // 鼠标就是一次 draw()，每帧重新分配一张几 MB 的位图是白白的开销。
      // 代价是"尺寸没变时位图里的旧内容还在"，所以 draw() 里必须显式清屏。
      if (canvas.width !== bitmapW) canvas.width = bitmapW;
      if (canvas.height !== bitmapH) canvas.height = bitmapH;
      var ctx = canvas.getContext('2d');
      // 倍率用**刚刚读到的同一个 dpr**，不要拿 bitmapW / width 反算：位图尺寸是
      // Math.round 过的，反算出来的倍率会差一点点，坐标系的偏移正好落在整像素
      // 边缘上，线又糊回去了。
      ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
      return {
        ctx: ctx,
        w: width,
        h: height,
        // 位图尺寸（物理像素）：清屏必须按它来，理由见 draw()。
        bitmapW: canvas.width,
        bitmapH: canvas.height,
        // left 是 Y 轴刻度的留白。42 曾经够用（那时刻度写的是 "1.0M" 这种光秃秃的
        // 缩写），但字节刻度现在要把单位写全：速率图是 "500.0 KB/s"、"2.00 MB/s"，
        // 流量图是 "465.0 GB"、"1.0 TB"。刻度是右对齐画在 left-6 处的，留白不够
        // 就会把左边的字裁掉（实测 42 时 "500.0 KB/s" 左边界到 -15px）。
        // 64 是按 11px 字体下最长的刻度（"500.0 KB/s" ≈ 51px）留出余量。
        left: 64,
        right: 8,
        top: 8,
        bottom: 20
      };
    }

    function bounds() {
      var t0 = Infinity;
      var t1 = -Infinity;
      var vMax = 0;
      opts.series.forEach(function (s) {
        (s.points || []).forEach(function (p) {
          if (p[0] < t0) t0 = p[0];
          if (p[0] > t1) t1 = p[0];
          // p[1] 可能是 null（延迟图里"这一桶没有成功探测"，见 app.js 的
          // latencySeriesFor）：比较运算把 null 与 0 一样看待，两者都不会撑高
          // Y 轴上限 —— 这正是"没有读数就不该影响轴范围"的语义，所以这里
          // 不需要额外分支（曲线那条路径才会把 0 画成 0ms，见 drawLine）。
          if (p[1] > vMax) vMax = p[1];
          if (opts.showMax && p[2] > vMax) vMax = p[2];
        });
      });
      return { t0: t0, t1: t1, vMax: vMax };
    }

    function yTop(vMax) {
      if (opts.yMax > 0) return opts.yMax;
      if (vMax <= 0) return 1;
      return niceCeil(vMax * 1.1);
    }

    function drawEmpty(g) {
      // 「暂无数据」也是文字，用同一套跟随主题的色（理由见 textColor）。
      g.ctx.fillStyle = textColor();
      g.ctx.font = '12px -apple-system, "Segoe UI", sans-serif';
      g.ctx.textAlign = 'center';
      g.ctx.textBaseline = 'middle';
      g.ctx.fillText('暂无数据', (g.left + g.w - g.right) / 2, (g.top + g.h - g.bottom) / 2);
    }

    // labelFits 算出按 step 铺标签时，屏幕上**真正能画出**的标签是哪些。
    //
    // 宽度取标签的**实际文本宽度**（ctx.measureText），而不是"最多画 N 个"：
    // "09:05" 与 "12-31" 不一样宽，窄画布与宽画布不一样宽，字号也会变 ——
    // 拍一个 N 出来必然在某个组合上失手，而失手的表现只是"看起来有点挤"，
    // 不会报错。调用前 ctx.font 必须已经是画标签用的那个字号（否则量出来的是别的字号）。
    //
    // 返回 {step, count, ok, ticks}：ticks 是这个间隔下**会画出来**的那些锚点
    // （绝对时间网格上的 ts），count 是它的条数，ok 表示相邻两个之间都留够了
    // X_LABEL_MIN_GAP，step 原样带回（标签格式取决于实际间隔，见下面的说明）。
    //
    // 为什么判定要把"画到哪儿"一起算出来、而不是只回一句放得下放不下：
    // 判定与绘制分成两套逻辑时，两边迟早对不上 —— 判定说"放得下"，画的时候
    // 才发现最左边那个压在 Y 轴刻度上、最右边那个被画布裁掉半个字。现在
    // draw() 直接遍历这里给出的 ticks，两条边界规则只写一遍，对不上是不可能的。
    //
    // count 为 0 有两种情况：窗口里一个锚点都没有（刚上线、只有几分钟数据），
    // 或者仅有的锚点因为边界规则被丢掉了 —— 调用方都要按"没有标签可画"处理。
    function labelFits(ctx, step, t0, t1, g) {
      var plotW = g.w - g.left - g.right;
      var span = Math.max(1, t1 - t0);
      var first = Math.ceil(t0 / step) * step;
      var ticks = [];
      var prevRight = 0;
      for (var ts = first; ts <= t1; ts += step) {
        var px = (ts - t0) / span * plotW;
        // 量宽度时要**连 step 一起传**：有些档位的格式取决于实际间隔
        // （3d/7d 间隔 ≥ 一天用 MM-DD，否则用 MM-DD HH:MM）。这里在试的正是候选
        // step，用它自己的格式去量才自洽 —— 否则量的是短格式、画的是长格式，
        // 抽稀判定会偏乐观，两个标签就叠上了。
        //
        // half 是标签宽度的一半：X 标签是**居中**锚定的（draw() 里 textAlign =
        // 'center'），所以下面比边界时用的是标签的**边缘**（left = px - half、
        // right = px + half），不是锚点 px 本身 —— 拿锚点去比，判定会宽松
        // 半个标签，最左边那两个就先压上去了。
        var half = ctx.measureText(opts.xFormat(ts, step)).width / 2;
        var left = g.left + px - half;    // 标签左边缘（画布坐标）
        var right = g.left + px + half;   // 标签右边缘

        // 左边界：左边缘越过绘图区左缘，说明这个标签会压到 Y 轴刻度文字上 ——
        // Y 轴刻度是右对齐画在 g.left - 6 处的，g.left 左边那一条横向空间归它。
        // 这里**既不夹取也不硬塞**：直接跳过这个锚点，让整排标签往后挪一个间隔
        // （于是第一个标签落在第二个锚点上）。夹取（把它推到贴着 g.left）会让
        // 标签不再落在它代表的时刻上，读出来的时间是错的；硬塞就是原来那个
        // "和 Y 轴刻度叠在一起"的样子。
        // 画布左缘（0）不必单独判：g.left > 0，"越过 g.left"必然也越过了画布左缘。
        // 锚点单调递增，所以会越界的只可能是最前面那几个。
        if (left < g.left) continue;

        // 右边界：右边缘越过**画布**右缘时这个标签会被裁掉半个字，宁可不画它。
        // 比的是画布宽度 g.w，不是绘图区右缘（g.w - g.right）：X 轴标签本来就画在
        // 绘图区之外（横向与绘图区对齐，右端还有 g.right 那点余量），拿绘图区
        // 右缘去比会把本来画得下的整条标签丢掉。
        // 不往左挤的理由与左边那条一样（挤了就不在真实时刻上了）。
        // 锚点单调递增，一旦越界，后面的只会更靠右 —— 直接结束。
        if (right > g.w) break;

        // 与上一个标签的右边界比：紧紧挨着的两个 "12:34" 看上去是一串数字。
        // 被左边界跳过的标签不参与这一比（prevRight 不动），所以「往后挪一格」
        // 之后的第一个标签不会被当成「和前一个挨得太近」。
        if (ticks.length > 0 && left - prevRight < X_LABEL_MIN_GAP) {
          return { step: step, count: ticks.length, ok: false, ticks: null };
        }
        prevRight = right;
        ticks.push(ts);
      }
      return { step: step, count: ticks.length, ok: true, ticks: ticks };
    }

    // xLabelStep 返回这一帧的**标签计划**（就是 labelFits 的结果）：从基准间隔出发，
    // 按整齐倍数逐级放大，直到相邻标签之间的空隙够 X_LABEL_MIN_GAP、且两端的标签
    // 都不会被 Y 轴区或画布边缘裁掉为止。
    //
    // 函数名沿用旧名（它原来只返回一个"间隔秒数"）：间隔仍是这份计划里最主要的东西，
    // 只是顺带把"哪些锚点真的画得下"一起带出来交给 draw() —— 判定与绘制共用一份结果。
    //
    // 为什么不能直接按基准间隔画标签：基准间隔是"刻度语义"（1h 档每 1 分钟一根），
    // 它比一些档位的**数据桶宽**还细（6h 档一个点代表 5 分钟），而且远细于屏幕能
    // 放下的量 —— 1h 档 60 个标签铺在约 900px 上，一个 "HH:MM" 就占约 32px，
    // 画出来是一片糊在一起的黑块（这也是"必须按整齐倍数稀疏"的由来，
    // 整齐的理由见 X_STEP_MULTIPLIERS）。
    function xLabelStep(ctx, t0, t1, g) {
      var base = Math.max(1, opts.tickBaseSec || 600);
      for (var i = 0; i < X_STEP_MULTIPLIERS.length; i++) {
        var step = base * X_STEP_MULTIPLIERS[i];
        var fit = labelFits(ctx, step, t0, t1, g);
        // 窗口比基准间隔还窄（刚上线、只有几分钟数据）：一个标签都放不下，
        // 就按基准间隔走（与"没数据"时的表现一致），不要越级把间隔放大。
        if (fit.count === 0) return labelFits(ctx, base, t0, t1, g);
        if (fit.ok) return fit;
      }
      // 梯级用完了还是放不下（极窄的画布）：用最大的那一档 —— 宁可只剩一个标签，
      // 也不要退回"密密麻麻"那种没法读的画面。
      return labelFits(ctx, base * X_STEP_MULTIPLIERS[X_STEP_MULTIPLIERS.length - 1], t0, t1, g);
    }

    function draw() {
      var g = layout();
      var ctx = g.ctx;
      // 清屏按**位图**尺寸清，而不是 CSS 尺寸。canvas.width = Math.round(w*dpr)
      // 在 w*dpr 的小数部分 ≥ 0.5 时会向上取整，比"CSS 尺寸 × dpr"多出最多半个
      // 物理像素 —— 那几列/行像素不在 CSS 坐标系覆盖的范围内，只清 CSS 尺寸的话
      // 右/下边缘会留下上一帧的残影（2× 屏上就是一条脏边）。
      // 这里临时切回单位变换把整张位图清干净，再切回来继续按 CSS 像素画。
      ctx.save();
      ctx.setTransform(1, 0, 0, 1, 0, 0);
      ctx.clearRect(0, 0, g.bitmapW, g.bitmapH);
      ctx.restore();

      var b = bounds();
      var hasData = isFinite(b.t0) && isFinite(b.t1) && b.t1 > 0;
      var plotW = g.w - g.left - g.right;
      var plotH = g.h - g.top - g.bottom;

      if (!hasData) {
        drawEmpty(g);
        return;
      }

      var top = yTop(b.vMax);
      var t0 = b.t0;
      var t1 = Math.max(b.t1, b.t0 + 1);
      var x = function (ts) { return g.left + (ts - t0) / (t1 - t0) * plotW; };
      var y = function (v) { return g.top + plotH - Math.min(v, top) / top * plotH; };

      // 横向网格 + Y 轴刻度
      ctx.font = '11px -apple-system, "Segoe UI", sans-serif';
      ctx.textAlign = 'right';
      ctx.textBaseline = 'middle';
      var lines = 4;
      for (var i = 0; i <= lines; i++) {
        var value = top * i / lines;
        // 线用 round(v)+0.5（1px 的线落在像素中心才不会被摊成 2px 灰边）。
        // 文字也共用这一个坐标：试过把文字单独改成整数坐标，实测在 DPR=1 与 DPR=2
        // 下锐度指标都**略微下降**（DPR=1 下 1255→1156、DPR=2 下 444→418），
        // 说明这个位置本来就落在字形栅格上，改了反而错开。别再"顺手修"它。
        var py = Math.round(y(value)) + 0.5;
        ctx.strokeStyle = i === 0 ? COLORS.axis : COLORS.grid;
        ctx.beginPath();
        ctx.moveTo(g.left, py);
        ctx.lineTo(g.w - g.right, py);
        ctx.stroke();
        ctx.fillStyle = textColor();
        ctx.fillText(opts.yFormat(value), g.left - 6, py);
      }

      // X 轴标签：锚点仍然钉在**绝对时间网格**上（ts 取整到间隔的倍数，
      // 切档位/滑窗口时位置稳定 —— 这是既有设计），但间隔不是基准间隔本身，
      // 而是按标签实际宽度自动稀疏出来的（见 xLabelStep）。
      //
      // 这里遍历的是 xLabelStep 给出的**计划**里那几个锚点，而不是自己从
      // Math.ceil(t0 / step) 重新铺一遍：判定（放不放得下、会不会压到 Y 轴刻度、
      // 会不会被画布右缘裁）与绘制因此共用同一份结果，不可能出现"判定说放得下、
      // 画出来却是挤的 / 被裁掉半个字"。自己重算一遍就等于把两条边界规则写第二遍，
      // 迟早只改一处。
      //
      // 这里**不画竖网格线**。原来每个刻度位置都有一条从绘图区顶部到底部的浅灰竖线
      // （ctx.moveTo(px, g.top); ctx.lineTo(px, g.top + plotH);），整段删掉了：
      //   - 它对读数没有任何帮助 —— 读数值靠的是横向网格线与左侧刻度；
      //   - 刻度位置本来就由标签自己表达（标签就画在那条线上）；
      //   - 档位越短、标签越密，竖线越像一层网罩在曲线上（1h 档原本几十条）。
      // 悬浮时那条竖线是**交互反馈**（"鼠标停在哪一点"），仍然画，见 drawHover ——
      // 两者不是一回事，别一起删。
      //
      // 顺带说明：竖线用的步长曾是 opts.tickLabelSec，这个字段已经随竖线一起消失，
      // 现在 X 轴只认 tickBaseSec（基准间隔）—— 它不再影响画面上的任何一条线。
      ctx.textAlign = 'center';
      ctx.textBaseline = 'top';
      var plan = xLabelStep(ctx, t0, t1, g);
      for (var k = 0; k < plan.ticks.length; k++) {
        var ts = plan.ticks[k];
        ctx.fillStyle = textColor();
        // 0.5 的偏移与横网格线同一个理由（1px 的线落在像素中心）；文字沿用同一个
        // 锚点，改了就会与悬浮竖线错开半个像素。
        // step 传进去：格式随实际间隔变（见 labelFits 里的说明），
        // 画的这一份必须与量宽度的那一份用同一个 step，否则两边格式不一致。
        ctx.fillText(opts.xFormat(ts, plan.step), Math.round(x(ts)) + 0.5, g.top + plotH + 4);
      }

      // 竖条（丢包）先全部画完，再画曲线：半透明的条压在曲线下面时曲线仍然清楚，
      // 反过来的话后画的条会盖住先画的线（多个目标叠在一起时尤其乱）。
      opts.series.forEach(function (s) {
        if (!s.bars || !s.points || s.points.length === 0) return;
        drawBars(ctx, s.points, s.bars, x, g.top + plotH, plotH, s.color);
      });

      // 曲线
      opts.series.forEach(function (s) {
        var pts = s.points || [];
        if (pts.length === 0) return;

        // 峰值淡线先画，均值线后画压在它上面。
        //
        // showMax 同时管两件事：画不画这条淡线、以及 Y 轴要不要把峰值算进去
        // （见 bounds 里那句 `opts.showMax && p[2] > vMax`）—— 用户要的
        // "取消峰值线之后 Y 轴自适应"就是后面那一半：关掉之后轴只按均值的高度算。
        if (opts.showMax && s.showMax !== false) {
          drawLine(ctx, pts, 2, x, y, s.color, 0.28, opts.smooth);
        }
        // 「延迟」开关关掉时平均线不画（峰值线若开着照旧画）：只留峰值也是
        // 有意义的画面 —— 它就是"延迟最高的那一次"的轨迹。
        if (opts.showMean !== false) {
          drawLine(ctx, pts, 1, x, y, s.color, 1, opts.smooth);
        }
      });

      // 悬浮读数
      if (state.hoverX !== null && state.hoverX >= g.left && state.hoverX <= g.w - g.right) {
        drawHover(g, state.hoverX, x, y, plotH);
      }
    }

    // linkedWithPrev 判断 pts[i] 能不能与 pts[i-1] 连起来（同一条子路径里）。
    //
    // 断线的两条判据都在这里，**两条都要**（少一条就会画出假的直线）：
    //
    //   1. 值是 null（或任何非数字）：这一桶整段丢包，一个成功的探测都没有。
    //      点还在数组里（见 app.js 的 latencySeriesFor），只是没有读数；
    //   2. 相邻两点的 ts 间隔 > 1.5 × 桶宽：**那一整段连点都没有**。
    //      Agent 离线时服务端根本不会往 ping_samples_1m 里写行，查询扫出来的点
    //      自然跳过那几十分钟甚至几小时 —— 只按 null 判断是**查不出来**的，
    //      而这恰恰是最该断开的一种：不断的话曲线会用一条直线横跨关机的那两小时，
    //      看图的人会以为那段时间延迟很稳。
    function linkedWithPrev(pts, i, valueIndex, bucketSec) {
      if (i <= 0) return false;
      var v = pts[i][valueIndex];
      if (typeof v !== 'number' || !isFinite(v)) return false;
      var prev = pts[i - 1][valueIndex];
      if (typeof prev !== 'number' || !isFinite(prev)) return false;
      // 桶宽未知（调用方没给）时只按 null 断开：宁可少断，也不要按一个猜出来的
      // 桶宽把正常的曲线切碎。
      if (!(bucketSec > 0)) return true;
      return (pts[i][0] - pts[i - 1][0]) <= bucketSec * GAP_BUCKET_RATIO;
    }

    // runsOf 把点切成若干**连续段**（每段是一串下标）。
    //
    // 分段是所有绘制路径的公共前提：断线、平滑两者必须用同一套分段，
    // 否则会出现"线断开了、平滑却跨过缺口画了过去"这种自相矛盾的画面。
    function runsOf(pts, valueIndex, bucketSec) {
      var runs = [];
      var run = [];
      for (var i = 0; i < pts.length; i++) {
        var v = pts[i][valueIndex];
        if (typeof v !== 'number' || !isFinite(v)) {
          if (run.length) runs.push(run);
          run = [];
          continue;
        }
        // run 非空时它的最后一个元素必然是 i-1（下标是逐个 push 进去的），
        // 所以这里比的就是相邻两点。
        if (run.length && !linkedWithPrev(pts, i, valueIndex, bucketSec)) {
          runs.push(run);
          run = [];
        }
        run.push(i);
      }
      if (run.length) runs.push(run);
      return runs;
    }

    // monotoneTangents 算**单调三次插值**（Fritsch–Carlson）在每个点上的切线斜率。
    //
    // 为什么不能用普通的 Catmull-Rom / 自然三次样条：它们都会**过冲**。
    // 数据在 0 附近时过冲会把曲线画到负数去；尖峰两侧则会鼓出比真实峰值还高的包。
    // 延迟是有物理下限（> 0）的量 —— 画出一条负延迟、或者一个"比真实峰值还高"
    // 的鼓包，等于凭空造了一个不存在的读数，而看图的人只会以为线路真的那样。
    //
    // Fritsch–Carlson 的两步：
    //   1. 初始切线取相邻斜率的平均（极值点处压成 0，否则那里必然鼓包）；
    //   2. 把切线夹进"不会破坏单调性"的范围（|m| ≤ 3|Δ|）——
    //      于是每一段都是单调的，曲线必然落在两端点之间，一个控制点都不会跑出
    //      该段数据的 [min, max]（见 drawRun 里控制点的取法）。
    //
    // 传进来的 xs/ys 已经是**像素**坐标：单调性是仿射变换的不变量，在像素空间
    // 里做与在数值空间里做等价，但省掉了来回换算。
    function monotoneTangents(xs, ys) {
      var n = xs.length;
      var m = new Array(n);
      if (n < 2) { m[0] = 0; return m; }

      var delta = new Array(n - 1);
      for (var i = 0; i < n - 1; i++) {
        var dx = xs[i + 1] - xs[i];
        // 两点重合（理论上不会有：桶的时间戳严格升序）时按 0 斜率处理，
        // 避免除零得到 Infinity 之后把整段曲线带飞。
        delta[i] = dx > 0 ? (ys[i + 1] - ys[i]) / dx : 0;
      }

      m[0] = delta[0];
      m[n - 1] = delta[n - 2];
      for (var j = 1; j < n - 1; j++) {
        // 极值点（左右斜率反号）：切线必须是 0，否则曲线会在峰/谷处鼓出去。
        m[j] = delta[j - 1] * delta[j] <= 0 ? 0 : (delta[j - 1] + delta[j]) / 2;
      }

      for (var k = 0; k < n - 1; k++) {
        if (delta[k] === 0) { m[k] = 0; m[k + 1] = 0; continue; }
        var a = m[k] / delta[k];
        var b = m[k + 1] / delta[k];
        var s = a * a + b * b;
        // 圆以外的切线会被"拉回圆上"（半径 3）—— 这一步就是防过冲的那道夹子。
        if (s > 9) {
          var tau = 3 / Math.sqrt(s);
          m[k] = tau * a * delta[k];
          m[k + 1] = tau * b * delta[k];
        }
      }
      return m;
    }

    // drawRun 画一个连续段：smooth 关着是折线，开着是单调三次曲线。
    //
    // 平滑**逐段**做（每个连续段各自算切线），绝不跨过缺口 —— 跨过去的话，
    // 缺口两侧的点会被"平滑"成一条穿过缺口的曲线，那正是断线要避免的事。
    function drawRun(ctx, pts, run, valueIndex, x, y, smooth) {
      if (run.length === 0) return;

      // 孤立的一点：路径里只有一个点，stroke 什么都画不出来（没有线段可描边）。
      // 断线之后这种点不再罕见（缺口两侧各剩一个桶有样本），必须单独画成点。
      if (run.length === 1) {
        var only = pts[run[0]];
        ctx.beginPath();
        ctx.arc(x(only[0]), y(only[valueIndex]), SOLO_DOT_R, 0, Math.PI * 2);
        ctx.fill();
        return;
      }

      var xs = [];
      var ys = [];
      for (var i = 0; i < run.length; i++) {
        xs.push(x(pts[run[i]][0]));
        ys.push(y(pts[run[i]][valueIndex]));
      }

      ctx.beginPath();
      if (!smooth) {
        for (var j = 0; j < xs.length; j++) {
          if (j === 0) ctx.moveTo(xs[j], ys[j]);
          else ctx.lineTo(xs[j], ys[j]);
        }
        ctx.stroke();
        return;
      }

      var m = monotoneTangents(xs, ys);
      ctx.moveTo(xs[0], ys[0]);
      for (var k = 0; k < xs.length - 1; k++) {
        // 三次贝塞尔的控制点取在 1/3 处、纵坐标偏移 m×dx/3（标准 Hermite → Bézier
        // 换算）。因为 |m| ≤ 3|Δ|（见 monotoneTangents），控制点必然落在这一段的
        // 两端点之间 —— 这就是"曲线不会过冲"的算术依据。
        var h = (xs[k + 1] - xs[k]) / 3;
        ctx.bezierCurveTo(
          xs[k] + h, ys[k] + m[k] * h,
          xs[k + 1] - h, ys[k + 1] - m[k + 1] * h,
          xs[k + 1], ys[k + 1]);
      }
      ctx.stroke();
    }

    // drawLine 画一条曲线（valueIndex 指向点里的第几个元素）。
    //
    // 现在它按**连续段**逐段画（见 runsOf）：缺口处断开，不再用一条直线把缺口
    // 两端"桥"过去 —— 那样画出来的是一条不存在的读数，看图的人会以为那段时间
    // 延迟正常。线的颜色/线宽/透明度与以前完全一致，只有"缺数据的地方不再连线"。
    function drawLine(ctx, pts, valueIndex, x, y, color, alpha, smooth) {
      ctx.save();
      ctx.globalAlpha = alpha;
      ctx.strokeStyle = color;
      // fillStyle 只有"孤立点画成圆点"那一条路径用得到；一起设上，免得圆点
      // 捡到上一条竖条/文字的填充色。
      ctx.fillStyle = color;
      ctx.lineWidth = valueIndex === 1 ? 1.6 : 1;
      ctx.lineJoin = 'round';
      var runs = runsOf(pts, valueIndex, opts.bucketSec);
      for (var i = 0; i < runs.length; i++) {
        drawRun(ctx, pts, runs[i], valueIndex, x, y, smooth);
      }
      ctx.restore();
    }

    // barWidth 让竖条宽度跟随桶间距：桶是固定时间网格上的格子，
    // "首尾两点的像素距离 / 间隔数"就是桶间距（点数>1 时必然均分）。
    // 单点时没有间距可言（spacing 为 Infinity，自然落到 BAR_MAX_W）；
    // 间距算出 0 或 NaN（极窄的容器）时有 BAR_MIN_W 兜底，不会画出 0 宽的条。
    function barWidth(pts, x) {
      var spacing = Infinity;
      if (pts.length > 1) {
        spacing = Math.abs(x(pts[pts.length - 1][0]) - x(pts[0][0])) / (pts.length - 1);
      }
      var w = Math.min(spacing * 0.7, BAR_MAX_W);
      if (!(w > BAR_MIN_W)) w = BAR_MIN_W;
      return w;
    }

    // drawBars 从绘图区底边往上画竖条（延迟图用它画"这一桶丢了多少包"）。
    //
    // 为什么是竖条而不是第二条曲线：丢包是"某一段时间里丢了多少"，
    // 视觉语言天然是"这一格有多高"；画成折线的话 1% 与 0% 在图上几乎重合，
    // 而这两者的区别正是用户要看的。
    function drawBars(ctx, pts, spec, x, bottom, plotH, fallbackColor) {
      var max = spec.max > 0 ? spec.max : 100;
      var width = barWidth(pts, x);
      ctx.save();
      ctx.globalAlpha = BAR_ALPHA;
      // 默认用该 series 自己的线色：色块与曲线一眼能对上（app.js 不传 color）。
      ctx.fillStyle = spec.color || fallbackColor || COLORS.text;
      for (var i = 0; i < pts.length; i++) {
        var v = pts[i][spec.valueIndex];
        // v <= 0 不画：没丢包的桶画一条"贴地"的边毫无信息量，反而像噪点。
        if (typeof v !== 'number' || !isFinite(v) || v <= 0) continue;
        var h = Math.min(v, max) / max * plotH;
        if (h < BAR_MIN_H) h = BAR_MIN_H;   // 见 BAR_MIN_H：不满足最小高度就看不见
        var px = x(pts[i][0]);
        ctx.fillRect(px - width / 2, bottom - h, width, h);
      }
      ctx.restore();
    }

    // fmtLoss 把丢包率格式化成 "0.3%" / "12%"。
    //
    // 小于 10 保留一位小数："0.3%" 与 "0%" 的区别正是这里要传达的信息，
    // 一并四舍五入成整数就把它抹掉了。
    function fmtLoss(v) {
      return (v < 10 ? v.toFixed(1) : v.toFixed(0)) + '%';
    }

    // nearestPoint 在**一条曲线自己**的点里找离 ts 最近的那个（返回下标；没有点时 -1）。
    //
    // 为什么必须逐条曲线各找各的，而不是像以前那样"用第 0 条曲线的下标去索引所有
    // 曲线"：点数组里只有**存在**的桶（服务端 GROUP BY bucket，某个目标中途没数据
    // 时它的数组就比别的短），共用一个下标等于拿"第 0 条曲线的第 37 个点"去读
    // 第 1 条曲线的第 37 个点 —— 那是**另一个时间**的读数，而浮层上只写一个时间，
    // 用户根本看不出来（两条线的值都"有数"，只是不在同一刻）。
    function nearestPoint(points, ts) {
      var best = -1;
      var bestDist = Infinity;
      for (var i = 0; i < points.length; i++) {
        var d = Math.abs(points[i][0] - ts);
        if (d < bestDist) { bestDist = d; best = i; }
      }
      return best;
    }

    // HOVER_SLACK_RATIO 是"这个点还算不算在鼠标附近"的判据：最近的点离鼠标超过
    // 1.5 个桶宽，就算"这条曲线在这个时间附近没有点"。
    //
    // 为什么与断线判据（GAP_BUCKET_RATIO）用同一个数：那里说"相邻两点隔了 1.5 个
    // 桶以上就说明中间那些桶根本不存在"，这里问的是"鼠标指的那段时间有没有点" ——
    // 两处尺度必须一致，否则会出现"线是断开的，浮层却从缺口另一头拿了个读数"，
    // 而那个读数被写在这个时间点上，读起来就是"缺口里其实有数据"。
    var HOVER_SLACK_RATIO = 1.5;

    // hoverStampText 把"这个读数代表的那一段时间"写出来。
    //
    // 一个点不是一个瞬间，而是一个**桶**（1h 档 1 分钟、7d 档 1 小时）：只写起点
    // 会被当成"这一刻的读数"—— 桶宽一小时时，那等于把一小时的平均值读成某一秒的值。
    //
    // 规则（用户定稿，阈值见 HOVER_INTERVAL_MIN_SEC）：
    //   - 桶宽 ≤ 1 分钟（最细的历史桶）：只写一个时刻（交给 opts.xFormat，
    //     各档位自己的格式）—— 一格就是一分钟，写 "12:34" 不含糊也不啰嗦；
    //   - 比 1 分钟粗：写 [桶起点, 桶起点 + 桶宽)。右端是**开**区间，所以直接写
    //     "起点 + 桶宽"（08:30–09:30，而不是 08:30–09:29）；
    //   - 两端跨天时两端都带日期（09-29 23:30–09-30 00:30）：只写时分的话
    //     "23:30–00:30" 看上去像倒着走。
    function hoverStampText(ts) {
      var width = opts.bucketSec > 0 ? opts.bucketSec : 0;
      if (width <= HOVER_INTERVAL_MIN_SEC) return opts.xFormat(ts);
      var end = ts + width;
      if (dayKey(ts) === dayKey(end)) return clockHM(ts) + '–' + clockHM(end);
      return dayHM(ts) + '–' + dayHM(end);
    }

    function clockHM(ts) {
      var f = zoneFields(opts.timeZone, ts);
      return f.hour + ':' + f.minute;
    }

    function dayHM(ts) {
      var f = zoneFields(opts.timeZone, ts);
      return f.month + '-' + f.day + ' ' + f.hour + ':' + f.minute;
    }

    // dayKey 是"这是哪一天"的串（**按服务端时区**），只用来比较两端是不是同一天。
    //
    // 为什么不能拿浏览器本地日期来比：桶的边界是服务端切的，跨天判定也必须用
    // 同一把尺子 —— 否则会出现"服务端认为跨天了、图上却写成同一天的两个小时"，
    // 或者反过来，把同一个桶的起止写成一前一后两天。
    function dayKey(ts) {
      var f = zoneFields(opts.timeZone, ts);
      return f.year + '-' + f.month + '-' + f.day;
    }

    function drawHover(g, hoverX, x, y, plotH) {
      var b = bounds();
      if (!isFinite(b.t0)) return;
      var plotW = g.w - g.left - g.right;
      var hoverTS = b.t0 + (hoverX - g.left) / plotW * Math.max(b.t1 - b.t0, 1);
      // 桶宽未知（不传 bucketSec 的图）时不设"附近"这条判据：那些图的点落在固定
      // 时间网格上，任何位置都有最近点。
      var slack = opts.bucketSec > 0 ? opts.bucketSec * HOVER_SLACK_RATIO : Infinity;

      // 每条曲线**各自**按 ts 找最近的点（见 nearestPoint）。匹配到的点各画各的
      // 圆点、各显示各的值；离得太远的记 null，那一行写 —（不拿别的点充数）。
      var marks = [];
      var baseTS = null;
      var baseDist = Infinity;
      opts.series.forEach(function (s) {
        var pts = s.points || [];
        if (pts.length === 0) return;
        var i = nearestPoint(pts, hoverTS);
        if (i < 0) return;
        var p = pts[i];
        var dist = Math.abs(p[0] - hoverTS);
        if (dist > slack) {
          marks.push({ series: s, point: null });
          return;
        }
        marks.push({ series: s, point: p });
        // 竖线画在**基准时间**上：取所有曲线里离鼠标最近的那个匹配点的时间
        // （并列时先出现的曲线赢，结果稳定）。每条曲线的圆点仍然画在它自己
        // 匹配到的时间上 —— 两个时间可能差几十秒，那正是"各自的桶"。
        if (dist < baseDist) { baseDist = dist; baseTS = p[0]; }
      });
      if (marks.length === 0) return;
      if (baseTS === null) {
        // 所有曲线在这个位置附近都没有点（鼠标停在缺口里）：竖线仍然跟着鼠标走，
        // 时间取鼠标所在的那个桶的起点 —— 交互反馈不该整块消失。
        baseTS = opts.bucketSec > 0
          ? Math.floor(hoverTS / opts.bucketSec) * opts.bucketSec
          : hoverTS;
      }
      var px = x(baseTS);

      g.ctx.save();
      g.ctx.strokeStyle = COLORS.axis;
      g.ctx.beginPath();
      g.ctx.moveTo(px, g.top);
      g.ctx.lineTo(px, g.top + plotH);
      g.ctx.stroke();

      var rows = [hoverStampText(baseTS)];
      marks.forEach(function (m) {
        var s = m.series;
        var p = m.point;
        g.ctx.fillStyle = s.color;
        if (!p) {
          // 这条曲线在鼠标附近没有点。写 — 而不是拿别的时间的点顶上：
          // 那会让两条曲线的读数看起来是同一刻的（见 nearestPoint 的说明）。
          rows.push(s.label + ' —');
          return;
        }
        // p[1] 可能是 null：延迟图里那表示"这一桶没有任何成功的探测"
        // （见 app.js 的 latencySeriesFor），**不是** 0ms。这一支不能画点、
        // 也不能格式化它：y(null) 会被当成 0 落到绘图区底边（画出一个假的
        // "0 ms" 顶点），yFormat 里的 toFixed 更是直接抛 TypeError，把整张图
        // 连悬浮一起画挂。读数显示 —（破折号）而不是 0：0 ms 是**合法读数**
        // （这一桶很快），与"一个样本都没有"正好相反，写 0 就是误导。
        var hasValue = typeof p[1] === 'number' && isFinite(p[1]);
        if (hasValue) {
          g.ctx.beginPath();
          g.ctx.arc(x(p[0]), y(p[1]), 2.5, 0, Math.PI * 2);
          g.ctx.fill();
          rows.push(s.label + ' ' + opts.yFormat(p[1]) + opts.unit);
          if (opts.showMax && typeof p[2] === 'number' && isFinite(p[2]) && p[2] > p[1]) {
            rows.push('  峰值 ' + opts.yFormat(p[2]) + opts.unit);
          }
        } else {
          // 这一行要留着：下面那行「丢包 X%」得说清是谁的 —— 整桶全丢时
          // 丢包率恰恰是 100%，那正是用户要看的那一行。
          rows.push(s.label + ' —');
        }
        // 带竖条的 series（延迟图）把这一桶的丢包率也列出来：图上能看出
        // "这里丢过包"，但看不出具体丢了多少。
        // 这一段**不在** hasValue 分支里：丢包与延迟是两件事，没有延迟读数
        // 的时候更要把丢包写出来（整桶全丢 = 丢包 100%）。
        if (s.bars) {
          var loss = p[s.bars.valueIndex];
          // 只在真有丢包时列：0% 是绝大多数桶的常态，每行都写一遍会把
          // 工具提示撑长，也会把真正丢包的那一行淹掉。
          if (typeof loss === 'number' && isFinite(loss) && loss > 0) {
            rows.push('  丢包 ' + fmtLoss(loss));
          }
        }
      });

      g.ctx.font = '11px -apple-system, "Segoe UI", sans-serif';
      var pad = 7;
      var lineH = 14;
      var width = 0;
      rows.forEach(function (row) {
        width = Math.max(width, g.ctx.measureText(row).width);
      });
      var boxW = width + pad * 2;
      var boxH = rows.length * lineH + pad * 1.4;
      var boxX = px + 10;
      if (boxX + boxW > g.w - g.right) boxX = px - boxW - 10;
      if (boxX < g.left) boxX = g.left;
      var boxY = g.top + 6;

      g.ctx.fillStyle = COLORS.tooltipBg;
      g.ctx.beginPath();
      if (g.ctx.roundRect) {
        g.ctx.roundRect(boxX, boxY, boxW, boxH, 6);
      } else {
        g.ctx.rect(boxX, boxY, boxW, boxH);
      }
      g.ctx.fill();

      g.ctx.fillStyle = COLORS.tooltipFg;
      g.ctx.textAlign = 'left';
      g.ctx.textBaseline = 'top';
      rows.forEach(function (row, i) {
        g.ctx.fillText(row, boxX + pad, boxY + pad * 0.7 + i * lineH);
      });
      g.ctx.restore();
    }

    function pointerX(event) {
      var rect = canvas.getBoundingClientRect();
      var clientX = event.touches && event.touches.length ? event.touches[0].clientX : event.clientX;
      return clientX - rect.left;
    }

    function onMove(event) {
      state.hoverX = pointerX(event);
      draw();
    }

    function onLeave() {
      state.hoverX = null;
      draw();
    }

    canvas.addEventListener('mousemove', onMove);
    canvas.addEventListener('mouseleave', onLeave);
    canvas.addEventListener('touchstart', onMove, { passive: true });
    canvas.addEventListener('touchmove', onMove, { passive: true });
    canvas.addEventListener('touchend', onLeave);

    var resizeTimer = null;
    function onResize() {
      if (resizeTimer) window.clearTimeout(resizeTimer);
      resizeTimer = window.setTimeout(function () { draw(); }, 80);
    }
    window.addEventListener('resize', onResize);

    // DPR 自己变了也要重画，不能只等 resize。
    //
    // 窗口从 1× 屏拖到 2× 屏时视口的 CSS 尺寸会变，resize 事件通常跟着来；但
    // 系统缩放被改、或者只改了本窗口的浏览器缩放时，未必有 resize —— 而倍率已经
    // 变了，位图还按旧倍率分配，屏幕拉伸显示，图就又糊了。
    // 媒体查询 (resolution: <当前倍率>dppx) 专门盯这个值：它一旦不再匹配，
    // change 事件就来了。查询串里带着倍率，所以**每次变化后都要重新注册**
    // （旧查询永远为假，再也不会触发第二次）。
    var dprQuery = null;
    function onDPRChange() {
      watchDPR();
      draw();
    }
    function unwatchDPR() {
      if (!dprQuery) return;
      if (dprQuery.removeEventListener) dprQuery.removeEventListener('change', onDPRChange);
      else if (dprQuery.removeListener) dprQuery.removeListener(onDPRChange);  // Safari < 14
      dprQuery = null;
    }
    function watchDPR() {
      if (typeof window.matchMedia !== 'function') return;
      unwatchDPR();
      dprQuery = window.matchMedia('(resolution: ' + currentDPR() + 'dppx)');
      if (dprQuery.addEventListener) dprQuery.addEventListener('change', onDPRChange);
      else if (dprQuery.addListener) dprQuery.addListener(onDPRChange);
    }
    watchDPR();

    return {
      setData: function (series, extra) {
        opts.series = series || [];
        apply(extra);
        draw();
      },
      setOptions: function (extra) { apply(extra); draw(); },
      redraw: draw,
      destroy: function () {
        window.removeEventListener('resize', onResize);
        canvas.removeEventListener('mousemove', onMove);
        canvas.removeEventListener('mouseleave', onLeave);
        // 媒体查询的监听也要摘掉：它挂在 MediaQueryList 上、不属于 canvas，
        // 图表实例销毁后还会一直活着并继续触发重画。
        unwatchDPR();
      }
    };
  }

  window.ProbeChart = { create: create };
})();
