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
 *   tickBaseSec / tickLabelSec: X 轴基础刻度与实际标签间隔（秒）
 *   yMax:     固定 Y 轴上限（百分比图传 100）；不传则自动取"好看的刻度"
 *   yFormat:  刻度与读数的格式化函数
 *   xFormat:  X 轴标签格式化函数
 *   unit:     读数单位（tooltip 用）
 *
 * bars 是可选的"竖条"描述：从绘图区底边往上画（延迟图用它画每个桶的丢包率）。
 * valueIndex 指向点数组里的第几个元素，max 是满格对应的值。不传 bars 的 series
 * 与以前完全一致 —— CPU/内存/磁盘/网络/流量五张图都不受影响。
 */

(function () {
  var DPR = window.devicePixelRatio || 1;

  // 丢包竖条的三条硬指标。
  //
  // 为什么单独拎出来：丢包是"稀疏且数值很小"的信号，条太窄/太淡就等于没画。
  // 尤其是 1% 的丢包 —— 按比例算出来不到 1px，而"这里丢过包"恰恰是
  // 最需要一眼看到的信息，所以给一个 3px 的最小可见高度。
  var BAR_ALPHA = 0.45;
  var BAR_MAX_W = 14;
  var BAR_MIN_W = 2;
  var BAR_MIN_H = 3;

  var COLORS = {
    grid: 'rgba(128,128,128,0.22)',
    axis: 'rgba(128,128,128,0.45)',
    text: '#8b939f',
    tooltipBg: 'rgba(20,22,26,0.92)',
    tooltipFg: '#f2f4f7',
    empty: '#8b939f'
  };

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

    function layout() {
      var rect = canvas.getBoundingClientRect();
      var width = Math.max(120, Math.round(rect.width));
      var height = Math.max(80, Math.round(rect.height));
      var dpr = window.devicePixelRatio || DPR;
      canvas.width = Math.round(width * dpr);
      canvas.height = Math.round(height * dpr);
      var ctx = canvas.getContext('2d');
      ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
      return {
        ctx: ctx,
        w: width,
        h: height,
        left: 42,
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
      g.ctx.fillStyle = COLORS.empty;
      g.ctx.font = '12px -apple-system, "Segoe UI", sans-serif';
      g.ctx.textAlign = 'center';
      g.ctx.textBaseline = 'middle';
      g.ctx.fillText('暂无数据', (g.left + g.w - g.right) / 2, (g.top + g.h - g.bottom) / 2);
    }

    function draw() {
      var g = layout();
      var ctx = g.ctx;
      ctx.clearRect(0, 0, g.w, g.h);

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
        var py = Math.round(y(value)) + 0.5;
        ctx.strokeStyle = i === 0 ? COLORS.axis : COLORS.grid;
        ctx.beginPath();
        ctx.moveTo(g.left, py);
        ctx.lineTo(g.w - g.right, py);
        ctx.stroke();
        ctx.fillStyle = COLORS.text;
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
        ctx.fillStyle = COLORS.text;
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

        if (opts.showMax && s.showMax !== false) {
          drawLine(ctx, pts, 2, x, y, s.color, 0.28);
        }
        drawLine(ctx, pts, 1, x, y, s.color, 1);
      });

      // 悬浮读数
      if (state.hoverX !== null && state.hoverX >= g.left && state.hoverX <= g.w - g.right) {
        drawHover(g, state.hoverX, x, y, plotH);
      }
    }

    function drawLine(ctx, pts, valueIndex, x, y, color, alpha) {
      ctx.save();
      ctx.globalAlpha = alpha;
      ctx.strokeStyle = color;
      ctx.lineWidth = valueIndex === 1 ? 1.6 : 1;
      ctx.lineJoin = 'round';
      ctx.beginPath();
      var started = false;
      for (var i = 0; i < pts.length; i++) {
        var v = pts[i][valueIndex];
        if (typeof v !== 'number' || !isFinite(v)) continue;
        var px = x(pts[i][0]);
        var py = y(v);
        if (!started) { ctx.moveTo(px, py); started = true; } else { ctx.lineTo(px, py); }
      }
      if (started) ctx.stroke();
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
          g.ctx.beginPath();
          g.ctx.arc(px, y(p[1]), 2.5, 0, Math.PI * 2);
          g.ctx.fill();
          rows.push(s.label + ' ' + opts.yFormat(p[1]) + opts.unit);
          if (opts.showMax && p[2] > p[1]) {
            rows.push('  峰值 ' + opts.yFormat(p[2]) + opts.unit);
          }
          // 带竖条的 series（延迟图）把这一桶的丢包率也列出来：图上能看出
          // "这里丢过包"，但看不出具体丢了多少。
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
      }
    };
  }

  window.ProbeChart = { create: create };
})();
