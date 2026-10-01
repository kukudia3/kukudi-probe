'use strict';

/* 极简 VPS 探针 —— 图表引擎
 *
 * 自己写的 canvas 折线图，不引入任何第三方图表库：
 * 需要的能力只有"定时序折线 + 固定刻度 + 悬浮读数"，为此拉一个库进来不划算。
 * 对外只暴露 ProbeChart.create(canvas, options)。
 *
 * options:
 *   series:   [{label, color, points: [[ts, avg, max], ...], showMax: bool,
 *               bars: {valueIndex, max, color},
 *               slow: {valueIndex, threshold, color}}]
 *   tickBaseSec / tickLabelSec: X 轴基础刻度与实际标签间隔（秒）
 *   yMax:     固定 Y 轴上限（百分比图传 100）；不传则自动取"好看的刻度"
 *   yFormat:  刻度与读数的格式化函数
 *   xFormat:  X 轴标签格式化函数
 *   unit:     读数单位（tooltip 用）
 *   showMean: 画不画"主曲线"（valueIndex 1）。默认画。延迟图的「延迟」开关用它。
 *   showMax:  画不画峰值淡线（valueIndex 2），**并且**决定 Y 轴要不要把峰值算进去
 *             （见 bounds）。默认画。延迟图的「峰值线」开关用它 —— 关掉它之后
 *             轴只按主曲线的高度自适应，这正是用户要的"取消峰值线后 Y 轴自适应"。
 *   smooth:   true 时主曲线与峰值线都画成**单调三次**平滑曲线（见 drawRun），
 *             默认 false（折线）。延迟图的「平滑曲线」开关用它。
 *   bucketSec: 桶宽（秒），用来判断"两点之间缺了多少个桶"（见 linkedWithPrev）。
 *             0 表示未知 —— 那时只按"值是不是 null"断线。
 *
 * bars 是可选的"竖条"描述：从绘图区底边往上画（延迟图用它画每个桶的丢包率）。
 * valueIndex 指向点数组里的第几个元素，max 是满格对应的值。不传 bars 的 series
 * 与以前完全一致 —— CPU/内存/磁盘/网络/流量五张图都不受影响。
 *
 * slow 是可选的"慢"（超过阈值）描述：把超过 threshold 的那部分曲线画成红色。
 * threshold 由**服务端**算好（基线中位数 × 3，下限 100ms，见
 * internal/store/ping.go 的 SlowStatsOf）—— 前端不做算术是本项目的原则，
 * 而且阈值一旦两处各算一遍，"图例写着慢 0%、线却是红的"这种自相矛盾迟早出现。
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

  // 孤立慢点的红点半径。
  //
  // 为什么孤立点必须画成点：drawLine 的"单点"路径只 moveTo 不 lineTo，
  // stroke 之后什么也画不出来 —— 一个孤立的尖峰（前后邻居都正常）就消失了，
  // 而"偶发一根 2203ms"恰恰是最该被看见的那种慢。
  var SLOW_DOT_R = 2.6;

  // 慢段红线的线宽：比曲线本身（1.6）略粗，压在上面才分得清"线是红的"
  // 与"这条线本身是红的"。
  var SLOW_LINE_W = 1.8;

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

  function defaultXFormat(ts) {
    var d = new Date(ts * 1000);
    return pad2(d.getHours()) + ':' + pad2(d.getMinutes());
  }

  function create(canvas, options) {
    var opts = {
      series: [],
      tickBaseSec: 600,
      tickLabelSec: 600,
      yMax: 0,
      yFormat: function (v) { return String(Math.round(v)); },
      xFormat: defaultXFormat,
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

      // X 轴刻度：钉在绝对时间网格上（切换范围时标签位置稳定）
      var step = Math.max(1, opts.tickLabelSec || opts.tickBaseSec || 600);
      ctx.textAlign = 'center';
      ctx.textBaseline = 'top';
      var first = Math.ceil(t0 / step) * step;
      for (var ts = first; ts <= t1; ts += step) {
        var px = Math.round(x(ts)) + 0.5;
        ctx.strokeStyle = COLORS.grid;
        ctx.beginPath();
        ctx.moveTo(px, g.top);
        ctx.lineTo(px, g.top + plotH);
        ctx.stroke();
        ctx.fillStyle = textColor();
        ctx.fillText(opts.xFormat(ts), px, g.top + plotH + 4);
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
        // 有意义的画面 —— 它就是"最慢那一次"的轨迹。
        if (opts.showMean !== false) {
          drawLine(ctx, pts, 1, x, y, s.color, 1, opts.smooth);
        }
        // 慢段画在曲线**之后**：红色要压在正常段上面，反过来的话后画的曲线
        // 会把红线盖掉一半，看起来像"这条线只是有点泛红"。
        if (s.slow) drawSlow(ctx, pts, s.slow, x, y);
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
    // 分段是所有绘制路径的公共前提：断线、平滑、慢段标红三者必须用同一套分段，
    // 否则会出现"线断开了、平滑却跨过缺口画了过去"或者"红线横跨一段没有样本的
    // 时间"这种自相矛盾的画面。
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
    // 延迟是有物理下限（> 0）的量 —— 画出一条负延迟、或者一个"比最慢那一次还慢"
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

    // drawSlow 把**超过阈值**的那部分曲线画成红色。
    //
    // 为什么不能"把超阈值的点单独当成一条 series 交给 drawLine"：drawLine 是
    // 把点**依次连起来**的，两个不相邻的尖峰之间会被拉出一条跨过正常区间的红线
    // —— 图上看起来那一整段都在慢，而中间其实是好的。这是最容易写错的一条，
    // 所以这里按**连续性**分段，一段一段画：
    //
    //   连续两个及以上都超阈值 → 把它们连成一段红线；
    //   孤立的单个超阈值点     → 画一个红点（见 SLOW_DOT_R）；
    //   中间隔着一个正常点     → 断开，红线绝不跨过正常区间。
    //
    // 头尾同样按这个规则处理：第一段可以从 pts[0] 开始、最后一段可以结束在
    // pts[pts.length-1]，不需要任何越界保护（下标全部来自 pts 自己）。
    //
    // 「连续性」的判据与曲线**完全一致**（linkedWithPrev：null 或 ts 间隔超过
    // 1.5 个桶宽都算断）。这一条必须跟着断线一起改：曲线断开了、红线却跨过缺口
    // 连过去的话，图上会有一段"横跨关机两小时"的红线 —— 那段时间根本没有样本，
    // 却被画成"一直很慢"，比不标红还糟。
    function drawSlow(ctx, pts, spec, x, y) {
      // 阈值 <= 0 表示服务端算不出来（没数据 / 整段全丢，见 app.js 的注释）：
      // 没有判据就不标红，而不是拿 0 当阈值把所有点涂红。
      if (!(spec.threshold > 0)) return;
      var color = spec.color || COLORS.text;
      ctx.save();
      ctx.strokeStyle = color;
      ctx.fillStyle = color;
      ctx.lineWidth = SLOW_LINE_W;
      ctx.lineJoin = 'round';
      ctx.lineCap = 'round';

      var run = [];
      function flush() {
        if (run.length === 1) {
          var only = pts[run[0]];
          ctx.beginPath();
          ctx.arc(x(only[0]), y(only[spec.valueIndex]), SLOW_DOT_R, 0, Math.PI * 2);
          ctx.fill();
        } else if (run.length > 1) {
          ctx.beginPath();
          for (var i = 0; i < run.length; i++) {
            var px = x(pts[run[i]][0]);
            var py = y(pts[run[i]][spec.valueIndex]);
            if (i === 0) ctx.moveTo(px, py); else ctx.lineTo(px, py);
          }
          ctx.stroke();
        }
        run = [];
      }

      for (var i = 0; i < pts.length; i++) {
        // 缺口（值是 null，或者与上一点之间缺了桶）先把当前段收掉：
        // 红线因此与曲线一样在缺口处断开。i = 0 时 linkedWithPrev 为 false，
        // 这里 flush 的是一个空段，什么也不做。
        if (!linkedWithPrev(pts, i, spec.valueIndex, opts.bucketSec)) flush();
        var v = pts[i][spec.valueIndex];
        if (typeof v === 'number' && isFinite(v) && v > spec.threshold) run.push(i);
        else flush();
      }
      flush();
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

    function nearestIndex(hoverX, g) {
      var b = bounds();
      if (!isFinite(b.t0)) return -1;
      var plotW = g.w - g.left - g.right;
      var ratio = (hoverX - g.left) / plotW;
      var ts = b.t0 + ratio * Math.max(b.t1 - b.t0, 1);
      var best = -1;
      var bestDist = Infinity;
      var first = opts.series[0] && opts.series[0].points ? opts.series[0].points : [];
      for (var i = 0; i < first.length; i++) {
        var d = Math.abs(first[i][0] - ts);
        if (d < bestDist) { bestDist = d; best = i; }
      }
      return best;
    }

    function drawHover(g, hoverX, x, y, plotH) {
      var index = nearestIndex(hoverX, g);
      if (index < 0) return;
      var first = opts.series[0].points;
      var ts = first[index][0];
      var px = x(ts);

      g.ctx.save();
      g.ctx.strokeStyle = COLORS.axis;
      g.ctx.beginPath();
      g.ctx.moveTo(px, g.top);
      g.ctx.lineTo(px, g.top + plotH);
      g.ctx.stroke();

      var rows = [opts.xFormat(ts)];
      opts.series.forEach(function (s) {
        var p = s.points[index];
        g.ctx.fillStyle = s.color;
        if (p) {
          // p[1] 可能是 null：延迟图里那表示"这一桶没有任何成功的探测"
          // （见 app.js 的 latencySeriesFor），**不是** 0ms。这一支不能画点、
          // 也不能格式化它：y(null) 会被当成 0 落到绘图区底边（画出一个假的
          // "0 ms" 顶点），yFormat 里的 toFixed 更是直接抛 TypeError，把整张图
          // 连悬浮一起画挂。读数显示 —（破折号）而不是 0：0 ms 是**合法读数**
          // （这一桶很快），与"一个样本都没有"正好相反，写 0 就是误导。
          var hasValue = typeof p[1] === 'number' && isFinite(p[1]);
          if (hasValue) {
            g.ctx.beginPath();
            g.ctx.arc(px, y(p[1]), 2.5, 0, Math.PI * 2);
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
