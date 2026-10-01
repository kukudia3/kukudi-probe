'use strict';

/* 极简 VPS 探针 —— 图表引擎
 *
 * 自己写的 canvas 折线图，不引入任何第三方图表库：
 * 需要的能力只有"定时序折线 + 固定刻度 + 悬浮读数"，为此拉一个库进来不划算。
 * 对外只暴露 ProbeChart.create(canvas, options)。
 *
 * options:
 *   series:   [{label, color, points: [[ts, avg, max], ...]}]
 *   tickBaseSec: X 轴**基准**间隔（秒）——标签锚点之间的间隔，由后端按档位给。
 *             它只是起点：屏幕上实际画多少个标签由本引擎按标签文本的真实宽度
 *             自动按**整齐倍数**放大（见 X_STEP_MULTIPLIERS / xLabelStep）。
 *             这里曾经还有一个 tickLabelSec（"实际标签 + 竖网格线"的间隔），
 *             现在两个概念合并成一个：竖网格线已经整条删掉，服务端也不再预先抽稀。
 *   yMax:     固定 Y 轴上限（百分比图传 100）；不传则自动取"好看的刻度"
 *   yFormat:  刻度与读数的格式化函数
 *   xFormat:  X 轴标签格式化函数
 *   unit:     读数单位（tooltip 用；单位已经写在 yFormat 里时必须留空）
 *   showMax:  画不画峰值淡线（valueIndex 2），**并且**决定 Y 轴要不要把峰值算进去
 *             （见 bounds）。默认画。流量图传 false（它一天一个点，没有"峰值"可言）。
 *
 * 关于"这张图引擎还支不支持别的画法"：**不支持了**。丢包竖条、超阈值标红、
 * 平滑曲线（单调三次插值）、"只画峰值不画均值"这四个开关都属于已经删掉的
 * 「延迟探测」功能，随功能一起从引擎里移除；现在只画折线 + 峰值淡线。
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

  // 孤立数据点的半径。
  //
  // 只有一个数据点的图不再罕见：节点刚建、或刚好只有一个桶有样本（Agent 上线
  // 一分钟又断了）。这种点用 moveTo + stroke 什么都画不出来（路径里只有一个点，
  // 没有线段可描边），必须单独画成一个点，否则那条真实存在的读数就被静默丢掉了。
  var SOLO_DOT_R = 1.8;

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

  function defaultXFormat(ts) {
    var d = new Date(ts * 1000);
    return pad2(d.getHours()) + ':' + pad2(d.getMinutes());
  }

  function create(canvas, options) {
    var opts = {
      series: [],
      tickBaseSec: 600,
      yMax: 0,
      yFormat: function (v) { return String(Math.round(v)); },
      xFormat: defaultXFormat,
      unit: '',
      showMax: true
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
          // p[1] 可能是 null（"这一桶没有读数"）：比较运算把 null 与 0 一样看待，
          // 两者都不会撑高 Y 轴上限 —— 这正是"没有读数就不该影响轴范围"的语义，
          // 所以这里不需要额外分支（曲线那条路径会把非数字的值断开，见 runsOf）。
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

      // 曲线
      opts.series.forEach(function (s) {
        var pts = s.points || [];
        if (pts.length === 0) return;

        // 峰值淡线先画，均值线后画压在它上面。
        //
        // showMax 同时管两件事：画不画这条淡线、以及 Y 轴要不要把峰值算进去
        // （见 bounds 里那句 `opts.showMax && p[2] > vMax`）。流量图关掉它是因为
        // 它一天只有一个点，avg 与 max 恒等，"峰值"没有信息量。
        if (opts.showMax) {
          drawLine(ctx, pts, 2, x, y, s.color, 0.28);
        }
        drawLine(ctx, pts, 1, x, y, s.color, 1);
      });

      // 悬浮读数
      if (state.hoverX !== null && state.hoverX >= g.left && state.hoverX <= g.w - g.right) {
        drawHover(g, state.hoverX, x, y, plotH);
      }
    }

    // linkedWithPrev 判断 pts[i] 能不能与 pts[i-1] 连起来（同一条子路径里）。
    //
    // 判据只有一条：**值不是数字**（null / undefined / NaN）就不连。
    // 这条路径现在没有调用方会真的走到（服务端给的三种曲线点都是数字），
    // 但引擎保留它有两个具体的理由：
    //   - bounds / drawHover 本来就把"非数字"当成"没有读数"，绘制侧不认这条
    //     判据的话，同一个点在三处会有两种含义；
    //   - 这里曾经还有第二条判据（相邻两点的 ts 间隔超过 1.5 个桶宽就断开），
    //     它需要调用方把桶宽传进来 —— 那是「延迟探测」功能的一部分，已随功能
    //     一起删除（见文件头）。不要把桶宽判据再"顺手加回来"：手机端会把点
    //     二次聚合成更宽的桶，桶宽传错时整条曲线会碎成一颗颗孤立的点。
    function linkedWithPrev(pts, i, valueIndex) {
      if (i <= 0) return false;
      var v = pts[i][valueIndex];
      if (typeof v !== 'number' || !isFinite(v)) return false;
      var prev = pts[i - 1][valueIndex];
      return typeof prev === 'number' && isFinite(prev);
    }

    // runsOf 把点切成若干**连续段**（每段是一串下标）。
    //
    // 分段是所有绘制路径的公共前提：断线、孤立点两条路径必须用同一套分段，
    // 否则会出现"线断开了、单点却按整段画"这种自相矛盾的画面。
    function runsOf(pts, valueIndex) {
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
        if (run.length && !linkedWithPrev(pts, i, valueIndex)) {
          runs.push(run);
          run = [];
        }
        run.push(i);
      }
      if (run.length) runs.push(run);
      return runs;
    }

    // drawRun 画一个连续段（折线）。
    function drawRun(ctx, pts, run, valueIndex, x, y) {
      if (run.length === 0) return;

      // 孤立的一点：路径里只有一个点，stroke 什么都画不出来（没有线段可描边）。
      // 节点刚建、或只有一个桶有样本时就是这种情况，必须单独画成点。
      if (run.length === 1) {
        var only = pts[run[0]];
        ctx.beginPath();
        ctx.arc(x(only[0]), y(only[valueIndex]), SOLO_DOT_R, 0, Math.PI * 2);
        ctx.fill();
        return;
      }

      ctx.beginPath();
      for (var j = 0; j < run.length; j++) {
        var px = x(pts[run[j]][0]);
        var py = y(pts[run[j]][valueIndex]);
        if (j === 0) ctx.moveTo(px, py);
        else ctx.lineTo(px, py);
      }
      ctx.stroke();
    }

    // drawLine 画一条曲线（valueIndex 指向点里的第几个元素）。
    //
    // 它按**连续段**逐段画（见 runsOf）：值不是数字的地方断开，不把两端用一条
    // 直线"桥"过去 —— 那样画出来的是一条不存在的读数。
    function drawLine(ctx, pts, valueIndex, x, y, color, alpha) {
      ctx.save();
      ctx.globalAlpha = alpha;
      ctx.strokeStyle = color;
      // fillStyle 只有"孤立点画成圆点"那一条路径用得到；一起设上，免得圆点
      // 捡到上一条文字/网格线的填充色。
      ctx.fillStyle = color;
      ctx.lineWidth = valueIndex === 1 ? 1.6 : 1;
      ctx.lineJoin = 'round';
      var runs = runsOf(pts, valueIndex);
      for (var i = 0; i < runs.length; i++) {
        drawRun(ctx, pts, runs[i], valueIndex, x, y);
      }
      ctx.restore();
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
          // p[1] 理论上一定是数字（服务端给的曲线点都是数字），但这里仍然守着
          // "不是数字就没有读数"这条路：y(null) 会被当成 0 落到绘图区底边
          // （画出一个假的 0 顶点），yFormat 里那些 toFixed 更是直接抛 TypeError，
          // 把整张图连悬浮一起画挂。没有读数时读数行写 —（破折号）而不是 0：
          // 0 是**合法读数**（这一项真的是 0），与"没有样本"正好相反。
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
            rows.push(s.label + ' —');
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
