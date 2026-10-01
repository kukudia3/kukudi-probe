package e2e

import (
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"probe/internal/store"
)

// 窄屏（手机）下的延迟图：**必须还是一条线**，不能退化成一串孤立圆点。
//
// 背景（两个 bug 叠在一起，都只有 web/ 能修）：
//
//  1. 桶宽改细之后（1h/6h/12h 都是 60 秒），手机端的二次聚合目标反而更大
//     （6h 档 120 秒 > 1.5 × 60 秒）。而 chart.js 的断线判据拿的是**服务端桶宽**
//     （linkedWithPrev：相邻两点间隔 > 1.5 × bucketSec 即断开）——
//     于是每一对相邻点都被判成缺口，每条 run 只剩一个点。
//  2. 聚合用的档位取的是 detail.range（资源卡），而延迟图跟的是 detail.pingRange：
//     两张卡停在不同档位时，1h 的延迟曲线会被按 7d 的聚合目标（1800 秒）合并。
//
// 为什么必须在真浏览器里量：这两条都只在"窄屏 + 有这个档位的数据"时成立，
// 静态断言能钉住代码形状，钉不住"画出来到底是一串点还是一条线"。
// 这里直接钩 canvas，数一帧里画了几个孤立圆点（arc）与几段线段（lineTo）：
// 正常情况线段占绝对多数，坏掉时反过来（lineTo = 0、arc = 点数）。
func TestMobileLatencyChartStaysConnected(t *testing.T) {
	chrome := findChrome()
	if chrome == "" {
		t.Skip("找不到 Chrome：设置 PROBE_CHROME 或把它装到默认位置后这条用例会自动跑起来")
	}

	logs := &captureHandler{}
	h := startServerAt(t, "127.0.0.1:0", filepath.Join(t.TempDir(), "probe.db"), time.UTC, slog.New(logs), nil)
	br := newBrowser(t, "http://"+h.addr)
	setupAdmin(t, br, waitSetupCode(t, logs))
	nodeID, _ := createNodeViaAPI(t, br, "lat-01")

	// 配置一个探测目标（走真接口），再按**真实的 1 分钟桶**喂一段曲线。
	targetID := createPingTarget(t, br, "CF", "1.1.1.1", 443, 60)
	seedPingBuckets(t, h, nodeID, targetID, 60, 5*time.Hour)

	// ---- 场景组一：手机端二次聚合 + 两卡档位不同 + 悬浮区间阈值 ----------
	agg := runLatHarness(t, chrome, h, tzHarnessConfig{
		NodeID: nodeID, NodeName: "lat-01", Scenario: "agg",
		User: "admin", Pass: "a-very-good-password",
	}, "600,1400")
	if !agg.Mobile {
		t.Fatalf("视口宽度没有落进 (max-width: 640px)：手机端聚合根本没生效，这条用例什么也没验证到（%s）", agg.Steps)
	}
	checkLatencyLineConnected(t, agg)
	checkPeakLineDefaultOff(t, agg)
	check7dHoverShowsInterval(t, agg)

	// ---- 场景组二：探测间隔（300 秒）> 桶宽（6h 档 60 秒）----------------
	//
	// 探测间隔是可配的，改大之后"每 5 个桶里只有 1 个有行"，相邻两点的实际间距
	// 变成 300 秒 —— 断线判据必须按这个间距算，否则整条曲线又是一串孤立点。
	// 用另一个节点：它的 ping_samples_1m 是独立的，不会被上面那批每分钟一行的数据盖住。
	targetID2 := createPingTarget(t, br, "CF", "1.1.1.1", 443, 300)
	node2, _ := createNodeViaAPI(t, br, "lat-02")
	seedPingBuckets(t, h, node2, targetID2, 300, 350*time.Minute)

	sparse := runLatHarness(t, chrome, h, tzHarnessConfig{
		NodeID: node2, NodeName: "lat-02", Scenario: "interval",
		User: "admin", Pass: "a-very-good-password",
	}, "600,1400")
	checkLatencyLineConnected(t, sparse)
	checkProbeIntervalRespected(t, sparse)
}

// runLatHarness 起一层反代 + 注入，跑一次 Chrome，把页面回传的观测值解出来。
func runLatHarness(t *testing.T, chrome string, h *harness, cfg tzHarnessConfig, windowSize string) latResult {
	t.Helper()
	proxy := newHarnessProxy(t, "http://"+h.addr, cfg, latHarnessJS)
	mock := newMockServer(t, proxy)
	raw := runChromeForResult(t, chrome, mock.URL+"/", proxy.result, 90*time.Second, windowSize)

	var res latResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("解析浏览器回传的结果: %v\n原始内容：%s", err, tail(string(raw), 600))
	}
	if res.Fatal != "" {
		t.Fatalf("浏览器里的自检流程没跑完（%s）：%s", res.Scenario, res.Fatal)
	}
	if len(res.Errs) != 0 {
		t.Fatalf("浏览器里有 %d 条 JS 报错：%v", len(res.Errs), res.Errs)
	}
	return res
}

// checkLatencyLineConnected 逐个场景核对"延迟曲线是线、不是一串点"。
func checkLatencyLineConnected(t *testing.T, res latResult) {
	t.Helper()
	if len(res.Phases) == 0 {
		t.Fatal("一个场景都没量到")
	}
	for _, p := range res.Phases {
		// 源点数、桶宽与点距都来自**服务端**（同一时刻的 /ping 响应）：
		// 有了它才能说清"应画多少段线"，而不是只看一个绝对数。
		t.Logf("场景 %s：资源档位=%s 延迟档位=%s；服务端 %d 个点（桶宽 %d 秒、点距 %.0f 秒）→ 这一帧画了 %d 个孤立圆点(arc)、%d 段线(lineTo)",
			p.Name, p.ResourceKey, p.LatKey, p.SourcePoints, p.SourceBucket, p.SourceSpacing, p.Arc, p.LineTo)
		if p.SourcePoints < 20 {
			t.Fatalf("场景 %s 的服务端点数只有 %d：数据没铺够，这条用例没有意义", p.Name, p.SourcePoints)
		}
		// 正常：孤立圆点几乎为 0（只有真的断开的那些桶才会画点）。
		// 坏掉：每个点各自成一段 —— arc ≈ 点数、lineTo 只剩几条横网格线。
		if p.Arc > 2 {
			t.Errorf("场景 %s 画了 %d 个孤立圆点（应当是 0~2 个）：断线判据把正常的相邻点判成了缺口 —— "+
				"曲线退化成一串点，而线上一个线段都没有（lineTo=%d）", p.Name, p.Arc, p.LineTo)
		}
		if min := p.SourcePoints / 4; p.LineTo < min {
			t.Errorf("场景 %s 只画了 %d 段线（至少要 %d 段，源数据 %d 个点）：曲线没连起来",
				p.Name, p.LineTo, min, p.SourcePoints)
		}
	}
}

// checkProbeIntervalRespected 核对"探测间隔 > 桶宽"那一组：数据确实是稀疏的
// （点距 = 探测间隔），而曲线仍然是连着的。
//
// 只说"连成线"是不够的：如果数据本身每分钟一行，那这条断言在探测间隔这件事上
// 什么都没证明。所以要先钉住"点距 300 秒、桶宽 60 秒"这个前提。
func checkProbeIntervalRespected(t *testing.T, res latResult) {
	t.Helper()
	if len(res.Phases) == 0 {
		t.Fatal("探测间隔那一组一个场景都没量到")
	}
	p := res.Phases[0]
	if p.SourceBucket != 60 {
		t.Errorf("6h 档桶宽 = %d 秒，期望 60：前提不对，这条用例验证的不是「探测间隔 > 桶宽」",
			p.SourceBucket)
	}
	if p.SourceSpacing != 300 {
		t.Errorf("服务端相邻两点的间距 = %.0f 秒，期望 300（= 探测间隔）：数据不是按 300 秒一行播撒的",
			p.SourceSpacing)
	}
	if p.SourceSpacing <= float64(p.SourceBucket)*1.5 {
		t.Errorf("点距 %.0f 秒没有超过 1.5 × 桶宽（%d 秒）：这组数据不会触发「探测间隔 > 桶宽」的 bug",
			p.SourceSpacing, p.SourceBucket)
	}
	t.Logf("探测间隔 %0.f 秒 > 桶宽 %d 秒：%d 个点仍然画出了 %d 段线、%d 个孤立圆点",
		p.SourceSpacing, p.SourceBucket, p.SourcePoints, p.LineTo, p.Arc)
}

// check7dHoverShowsInterval 核对"7d 档（桶宽 900 秒）的悬浮必须写成时间段"。
//
// 这是被一次真实的回归逼出来的：悬浮区间的阈值原来写死 3600 秒（当时的 7d 桶宽），
// 桶宽改细成 900 之后 7d 档的悬浮退回了单个时刻 —— 用户要的"看得到这一段有多长"
// 被悄悄抵消一半，而页面上一点异常都没有。静态断言只能钉住那个常量，
// "画出来到底有没有那一段区间"必须在真浏览器里读浮层。
func check7dHoverShowsInterval(t *testing.T, res latResult) {
	t.Helper()
	h := res.Hover7d
	if h.RangeKey == "" {
		t.Fatal("没有取到 7d 档的悬浮观测")
	}
	t.Logf("7d 档悬浮：点 ts=%d（桶宽 %d 秒）→ 浮层第一行 %q；这一帧画在浮层里的行 = %v",
		h.TargetTS, h.Bucket, h.Time, h.Rows)
	if h.Time == "" {
		t.Fatalf("7d 档的悬浮浮层没有写成时间段（拿到的行：%v）—— "+
			"桶宽 900 秒 > 最细的历史桶 60 秒，就该显示 08:30–08:45 这样的区间", h.Rows)
	}
	// 区间的右端必须正好是"起点 + 桶宽"（开区间）：用页面上那个 ts 反推。
	// 这里只钉形状（HH:MM–HH:MM），具体几分钟由上面的 bucket 与 Go 侧的阈值共同保证。
	if !regexp.MustCompile(`^([0-9]{2}-[0-9]{2} )?[0-9]{2}:[0-9]{2}–([0-9]{2}-[0-9]{2} )?[0-9]{2}:[0-9]{2}$`).MatchString(h.Time) {
		t.Errorf("7d 档悬浮的时间行 = %q，不是「起点–终点」的形状", h.Time)
	}
}

// checkPeakLineDefaultOff 核对「峰值线」默认关闭、打开后 Y 轴明显变高、再关掉能回去。
//
// 为什么用 Y 轴上限来验：峰值线开着时 chart.js 的 bounds() 会把峰值算进轴范围，
// 关掉之后轴只按平均线自适应 —— 这是"默认值改了"在画面上唯一可观测的差别
// （只数线条数量的话，两种状态下都是"一条线"）。
func checkPeakLineDefaultOff(t *testing.T, res latResult) {
	t.Helper()
	if res.Peak.InitialPressed != "false" {
		t.Errorf("首次打开时「峰值线」chip 的 aria-pressed = %q，期望 false（默认必须是关的）",
			res.Peak.InitialPressed)
	}
	t.Logf("峰值线默认状态：aria-pressed=%q；默认 Y 轴上限 = %d；打开后 = %d；再关掉 = %d",
		res.Peak.InitialPressed, res.Peak.YTopOff, res.Peak.YTopOn, res.Peak.YTopBackOff)
	if res.Peak.YTopOff <= 0 || res.Peak.YTopOn <= 0 {
		t.Fatalf("没有量到 Y 轴刻度（off=%d on=%d）：轴上的数字一个都没抓到", res.Peak.YTopOff, res.Peak.YTopOn)
	}
	if res.Peak.YTopOn <= res.Peak.YTopOff {
		t.Errorf("打开峰值线之后 Y 轴上限 = %d，没有高过关闭时的 %d —— 说明 showMax 没有把峰值算进轴范围",
			res.Peak.YTopOn, res.Peak.YTopOff)
	}
	// 数据里峰值是平均值的 6 倍以上，轴的差距必须看得出来（不是差一两个刻度）。
	if res.Peak.YTopOn < res.Peak.YTopOff*3/2 {
		t.Errorf("打开峰值线后 Y 轴上限只从 %d 涨到 %d，差距太小：轴多半没把峰值算进去",
			res.Peak.YTopOff, res.Peak.YTopOn)
	}
	if res.Peak.YTopBackOff != res.Peak.YTopOff {
		t.Errorf("再关掉峰值线之后 Y 轴上限 = %d，期望回到 %d（开关要能来回切）",
			res.Peak.YTopBackOff, res.Peak.YTopOff)
	}
}

// createPingTarget 走真接口配一个探测目标（顺带把探测间隔设成 intervalSec），
// 返回目标的 ID。
//
// 第二次调用时目标按 (type, host, port) 复用同一个 id（服务端就是这么匹配的），
// 所以只改间隔、不会把曲线身份换掉。
func createPingTarget(t *testing.T, br *browser, label, host string, port, intervalSec int) int64 {
	t.Helper()
	status, body := br.do("PUT", "/api/v1/settings/ping", map[string]any{
		"interval_sec": intervalSec,
		"targets": []map[string]any{
			{"label": label, "type": "tcp", "host": host, "port": port, "enabled": true},
		},
	}, true)
	if status != 200 {
		t.Fatalf("保存探测目标失败: %d %v", status, body)
	}
	targets, _ := body["targets"].([]any)
	if len(targets) != 1 {
		t.Fatalf("探测目标数量 = %d，期望 1", len(targets))
	}
	first, _ := targets[0].(map[string]any)
	id, _ := first["id"].(float64)
	if id <= 0 {
		t.Fatalf("探测目标 ID 不合法: %v", first)
	}
	if got, _ := body["interval_sec"].(float64); int(got) != intervalSec {
		t.Fatalf("探测间隔 = %v，期望 %d", body["interval_sec"], intervalSec)
	}
	return int64(id)
}

// seedPingBuckets 按 step 秒的间距喂一段延迟曲线（默认场景就是**真实的 1 分钟桶**）。
//
// 平均值在 20~39ms 之间小幅起伏，峰值给到平均值的 6 倍以上 ——
// 这样"轴要不要算峰值"在画面上一眼可辨（默认关掉之后轴会矮一大截）。
//
// step 就是"探测间隔"：300 秒一行时，60 秒宽的桶里每 5 个才有 1 个有数据 ——
// 这正是"探测间隔 > 桶宽"要复现的情形。
func seedPingBuckets(t *testing.T, h *harness, nodeID, targetID int64, step int, span time.Duration) {
	t.Helper()
	now := time.Now()
	end := now.Unix() - now.Unix()%int64(step) - int64(step)
	start := end - int64(span.Seconds())
	var buckets []store.PingBucket
	for ts := start; ts <= end; ts += int64(step) {
		i := (ts / int64(step)) % 20
		avg := float64(20 + i) // 20…39 ms
		peak := avg*6 + 30     // 150…264 ms
		buckets = append(buckets, store.NewPingBucket(nodeID, targetID, ts, avg, avg-5, peak, 0))
	}
	if len(buckets) < 20 {
		t.Fatalf("播撒的探测桶只有 %d 个，太少", len(buckets))
	}
	if err := h.db.UpsertPingBuckets(context.Background(), buckets); err != nil {
		t.Fatalf("写入探测桶: %v", err)
	}
	t.Logf("播撒探测桶 %d 个（节点 %d，ts %d … %d，步长 %d 秒）", len(buckets), nodeID, start, end, step)
}

// latResult 是窄屏延迟图那条用例的观测值。
type latResult struct {
	Errs     []string `json:"errs"`
	Fatal    string   `json:"fatal"`
	Steps    []string `json:"steps"`
	Mobile   bool     `json:"mobile"`
	Scenario string   `json:"scenario"`

	Phases  []latPhase `json:"phases"`
	Peak    latPeak    `json:"peak"`
	Hover7d latHover   `json:"hover7d"`
}

type latPhase struct {
	Name          string  `json:"name"`
	ResourceKey   string  `json:"resourceKey"`
	LatKey        string  `json:"latKey"`
	SourcePoints  int     `json:"sourcePoints"`
	SourceBucket  int     `json:"sourceBucket"`
	SourceSpacing float64 `json:"sourceSpacing"`
	Arc           int     `json:"arc"`
	LineTo        int     `json:"lineTo"`
	MoveTo        int     `json:"moveTo"`
}

// latHover 记一次悬浮观测：鼠标停在哪个点上、浮层第一行写的是什么。
type latHover struct {
	RangeKey string   `json:"rangeKey"`
	TargetTS int64    `json:"targetTS"`
	Bucket   int      `json:"bucket"`
	Time     string   `json:"time"`
	Rows     []string `json:"rows"`
}

type latPeak struct {
	InitialPressed string `json:"initialPressed"`
	YTopOff        int    `json:"yTopOff"`
	YTopOn         int    `json:"yTopOn"`
	YTopBackOff    int    `json:"yTopBackOff"`
}

// latHarnessJS 是窄屏延迟图那条用例的自检脚本。
//
// 它只做两件事：**按用户的真实操作切档位/点开关**，以及**把 canvas 上真正发生的
// 绘制调用数出来**（arc = 孤立圆点、lineTo = 线段、以及 Y 轴刻度上的数字）。
// 断言全在 Go 那边：什么算"连成一条线"是测试的判断，不是页面的。
const latHarnessJS = `(function () {
  'use strict';
  var CFG = window.__TZCFG || {};
  var rawFetch = window.fetch.bind(window);
  var R = { errs: [], fatal: '', steps: [], mobile: false, scenario: CFG.scenario || 'agg',
            phases: [], peak: {}, hover7d: {} };
  window.__LATRESULT = R;

  window.addEventListener('error', function (e) { R.errs.push('error: ' + (e.message || e.type)); });
  window.addEventListener('unhandledrejection', function (e) {
    var r = e.reason;
    R.errs.push('rejection: ' + (r && r.message ? r.message : String(r)));
  });

  // ---- 钩 canvas：这一帧画了几个点、几段线、Y 轴最高刻度是多少 ----------
  var counts = {};
  var yLabels = [];
  var allTexts = [];   // 悬浮浮层的行也在这里面（浮层是这一帧最后画的东西）
  function bucketOf(canvas) {
    var id = canvas && canvas.id ? canvas.id : '?';
    return counts[id] || (counts[id] = { arc: 0, lineTo: 0, moveTo: 0 });
  }
  (function () {
    var proto = CanvasRenderingContext2D.prototype;
    function wrap(name, hit) {
      var orig = proto[name];
      proto[name] = function () {
        if (this.canvas && this.canvas.id === 'chart-lat') hit(this, arguments);
        return orig.apply(this, arguments);
      };
    }
    wrap('arc', function (ctx) { bucketOf(ctx.canvas).arc++; });
    wrap('lineTo', function (ctx) { bucketOf(ctx.canvas).lineTo++; });
    wrap('moveTo', function (ctx) { bucketOf(ctx.canvas).moveTo++; });
    wrap('fillText', function (ctx, args) {
      var text = String(args[0]);
      allTexts.push(text);
      // Y 轴刻度是右对齐画在 g.left - 6 = 58 处的纯数字（延迟图的 yFormat 就是 v.toFixed(0)）。
      // 悬浮浮层里的数字带单位（"12 ms"），过不了这个正则。
      if (Math.abs(args[1] - 58) < 1.5 && /^[0-9]+$/.test(text)) yLabels.push(parseInt(text, 10));
    });
  })();
  function resetCounts() {
    counts['chart-lat'] = { arc: 0, lineTo: 0, moveTo: 0 };
    yLabels = [];
    allTexts = [];
  }
  function snapshot() {
    var c = bucketOf(document.getElementById('chart-lat'));
    var top = 0;
    yLabels.forEach(function (v) { if (v > top) top = v; });
    return { arc: c.arc, lineTo: c.lineTo, moveTo: c.moveTo, yTop: top };
  }

  // ---- 小工具 -------------------------------------------------------------
  function node(id) { return document.getElementById(id); }
  function textOf(id) { var n = node(id); return n ? n.textContent : ''; }
  function shown(id) { var n = node(id); return !!n && !n.hidden; }
  function sleep(ms) { return new Promise(function (r) { setTimeout(r, ms); }); }
  function getJSON(url) {
    return rawFetch(url, { headers: { 'Accept': 'application/json' }, credentials: 'same-origin', cache: 'no-store' })
      .then(function (res) { return res.json(); });
  }
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

  // ---- 取数完成的通知：/ping 每次回来都记一笔 ----------------------------
  var pingDone = 0;
  window.fetch = function (input, init) {
    var url = typeof input === 'string' ? input : ((input && input.url) || '');
    return rawFetch(input, init).then(function (res) {
      if (/\/ping\?range=/.test(url)) pingDone++;
      return res;
    });
  };

  function clickRange(containerId, key) {
    var box = node(containerId);
    if (!box) throw new Error('找不到档位容器 ' + containerId);
    var btns = box.querySelectorAll('button');
    for (var i = 0; i < btns.length; i++) {
      if (btns[i].textContent === key) { btns[i].click(); return true; }
    }
    throw new Error(containerId + ' 里没有档位按钮 ' + key);
  }

  function peakChip() {
    var row = document.querySelector('#lat-targets .lat-chips');
    if (!row) throw new Error('找不到四个开关那一行');
    var btns = row.querySelectorAll('button');
    for (var i = 0; i < btns.length; i++) {
      if (btns[i].textContent === '峰值线') return btns[i];
    }
    throw new Error('开关行里没有「峰值线」');
  }

  // redrawAndMeasure 强制重画一帧，再数这一帧的绘制调用。
  // 触发方式是 window 的 resize（图表引擎监听它，80ms 防抖后整张重画）——
  // 这样"被数的那一帧"是我们自己触发的，不会数到半张图。
  function redrawAndMeasure() {
    resetCounts();
    window.dispatchEvent(new Event('resize'));
    return sleep(600).then(snapshot);
  }

  // 切**延迟卡**的档位：等 /ping 回来 + 等页面把这一帧画完，再数。
  function switchLatRange(key) {
    var want = pingDone + 1;
    clickRange('lat-ranges', key);
    return waitFor('延迟曲线重新取数（' + key + '）', function () { return pingDone >= want; }, 20000)
      .then(function () { return sleep(300); });
  }

  // 切**资源卡**的档位：它只重取 /series，不发 /ping —— 等一小会儿就够了。
  function switchResourceRange(key) {
    clickRange('detail-ranges', key);
    return sleep(500);
  }

  // measure 记录一个场景：这一档位下服务端给了多少点（以及点距）、这一帧画了什么。
  function measure(name, resourceKey, latKey) {
    return redrawAndMeasure().then(function (m) {
      return getJSON('/api/v1/nodes/' + CFG.nodeID + '/ping?range=' + latKey).then(function (data) {
        var meta = data.meta || {};
        var targets = data.targets || [];
        var pts = (targets[0] && targets[0].points) || [];
        // 服务端相邻两点的实际间距：它才是断线判据要比的那个数
        // （探测间隔 > 桶宽时，点距是探测间隔而不是桶宽）。
        var spacing = pts.length > 1 ? pts[1][0] - pts[0][0] : 0;
        R.phases.push({
          name: name, resourceKey: resourceKey, latKey: latKey,
          sourcePoints: pts.length, sourceBucket: meta.bucket_sec || 0,
          sourceSpacing: spacing,
          arc: m.arc, lineTo: m.lineTo, moveTo: m.moveTo
        });
        return true;
      });
    });
  }

  // hoverMid 把鼠标停在这条曲线的中间那个点上，把浮层第一行（时间）读出来。
  //
  // 横坐标按图表引擎的线性映射算：x = left(64) + (ts-t0)/(t1-t0) * (w-left-right)。
  function hoverMid(rangeKey, bucket) {
    var canvas = node('chart-lat');
    var rect = canvas.getBoundingClientRect();
    var w = Math.round(rect.width);
    return getJSON('/api/v1/nodes/' + CFG.nodeID + '/ping?range=' + rangeKey).then(function (data) {
      var pts = (data.targets && data.targets[0] && data.targets[0].points) || [];
      if (pts.length < 3) throw new Error(rangeKey + ' 档只有 ' + pts.length + ' 个点，没法悬浮');
      var t0 = pts[0][0], t1 = pts[pts.length - 1][0];
      var mid = pts[Math.floor(pts.length / 2)][0];
      var plotW = w - 64 - 8;
      var px = 64 + (mid - t0) / Math.max(t1 - t0, 1) * plotW;
      if (px > w - 10) px = w - 10;
      if (px < 70) px = 70;
      var before = allTexts.length;
      canvas.dispatchEvent(new MouseEvent('mousemove', {
        clientX: rect.left + px, clientY: rect.top + 60, bubbles: true
      }));
      var rows = allTexts.slice(before);
      canvas.dispatchEvent(new MouseEvent('mouseleave', { bubbles: true }));
      R.hover7d = {
        rangeKey: rangeKey, targetTS: mid, bucket: bucket, rows: rows,
        // 浮层第一行就是时间（chart.js 的 drawHover 里 rows[0]）。
        // 区间两端跨天时形如 "09-29 23:30–09-30 00:30"，否则是 "16:30–16:45"。
        time: rows.filter(function (t) { return /^([0-9]{2}-[0-9]{2} )?[0-9]{2}:[0-9]{2}–/.test(t); })[0] || ''
      };
      return true;
    });
  }

  function login() {
    function loggedIn() {
      return shown('view-home') || shown('view-detail') || shown('view-settings');
    }
    return waitFor('出现登录视图', function () { return shown('view-login') || loggedIn(); }, 30000)
      .then(function () {
        if (loggedIn()) return true;
        node('login-user').value = CFG.user;
        node('login-pass').value = CFG.pass;
        node('login-submit').click();
        return waitFor('登录后进入应用', loggedIn, 30000);
      });
  }

  function openDetail() {
    window.location.hash = '#/n/' + CFG.nodeID;
    return waitFor('详情页打开', function () {
      return shown('view-detail') && textOf('detail-name') === CFG.nodeName;
    }, 30000)
      .then(function () {
        // 延迟卡自己那组档位要等 /nodes/{id} 回来才渲染出来。
        return waitFor('延迟档位按钮就绪', function () {
          return node('lat-ranges') && node('lat-ranges').querySelectorAll('button').length > 0;
        }, 30000);
      })
      .then(function () {
        // 探测目标的卡片与那一行开关是 /ping 回来之后才渲染的 —— 它出现了就说明
        // 数据已经到手（此时 canvas 上至少已经有横网格线，lineTo > 0）。
        return waitFor('探测目标与开关就绪', function () {
          return !!document.querySelector('#lat-targets .lat-chips');
        }, 30000);
      })
      .then(function () { return sleep(400); });
  }

  // scenario 'agg'：手机端二次聚合 + 断线判据（两卡档位不同、聚合目标大于桶宽）。
  function runAgg() {
    return openDetail()
      // ---- 场景 0：峰值线默认关闭，打开后 Y 轴明显变高 ---------------------
      .then(function () {
        R.peak.initialPressed = peakChip().getAttribute('aria-pressed');
        return redrawAndMeasure().then(function (m) {
          R.peak.yTopOff = m.yTop;
          peakChip().click();               // 打开峰值线
          return sleep(300);
        }).then(redrawAndMeasure).then(function (m) {
          R.peak.yTopOn = m.yTop;
          peakChip().click();               // 再关掉（回到默认视图）
          return sleep(300);
        }).then(redrawAndMeasure).then(function (m) {
          R.peak.yTopBackOff = m.yTop;
          return true;
        });
      })
      // ---- 场景 1：资源图与延迟图都停在 6h（手机端聚合目标 120 秒 > 桶宽 60 秒）
      .then(function () { return switchResourceRange('6h'); })
      .then(function () { return switchLatRange('6h'); })
      .then(function () { return measure('两卡都 6h', '6h', '6h'); })
      // ---- 场景 2：资源图切到 7d，延迟图仍停在 6h（聚合目标必须还是 6h 那一份）
      .then(function () { return switchResourceRange('7d'); })
      .then(function () { return switchLatRange('12h'); })
      .then(function () { return switchLatRange('6h'); })
      .then(function () { return measure('资源 7d + 延迟 6h', '7d', '6h'); })
      // ---- 场景 3：7d 档（桶宽 900 秒）的悬浮必须写成时间段，不是一个时刻
      .then(function () { return switchLatRange('7d'); })
      .then(function () { return hoverMid('7d', 900); });
  }

  // scenario 'interval'：探测间隔（300 秒）比桶宽（6h 档 60 秒）大 5 倍时，
  // 每个桶里只有 1/5 有行 —— 曲线仍然必须连成一条线。
  function runInterval() {
    return openDetail()
      .then(function () { return switchLatRange('6h'); })
      .then(function () { return measure('探测间隔 300 秒 + 6h 档', '1h', '6h'); });
  }

  function run() {
    R.mobile = window.matchMedia('(max-width: 640px)').matches;
    return waitFor('页面脚本就绪', function () { return !!node('view-login'); }, 30000)
      .then(login)
      .then(function () { return CFG.scenario === 'interval' ? runInterval() : runAgg(); });
  }

  function finish() {
    return rawFetch('/__result', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(R)
    }).then(function () { document.title = 'DUMPMARK:lat'; });
  }

  window.addEventListener('load', function () {
    run().catch(function (err) {
      R.fatal = String(err && err.message ? err.message : err);
    }).then(finish, finish);
  });
})();`
