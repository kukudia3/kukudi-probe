package e2e

import (
	"encoding/json"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
	"time"
)

// 改动 A 的真浏览器验收：六档的 X 轴标签**在真实画布上**到底是什么样。
//
// 为什么不能只做静态断言：静态断言能证明"代码里有 measureText 与一个间距常量"，
// 证明不了"这一档画出来是 12 个标签、间距 100px、跨天那几格写的是 10-01"。
// 而这次改动的全部内容就是观感 —— 数量、间距、内容，三样都只有把 canvas 上
// 真正 fillText 出来的字读出来才算验证过。
//
// 四条断言（每条都对应一种"页面上看得出来、测试却抓不到"的坏法）：
//  1. **间距**：相邻标签的空白 ≥ chart.js 里的 X_LABEL_MIN_GAP（判定用的是
//     标签边缘，所以这里也按 measureText 的宽度还原成边缘距离来比）；
//  2. **个数**：落在用户定稿的目标区间里（1h/6h/12h/1d 8~12、3d 6~10、7d 7~10）；
//  3. **跨天**：1d/3d/7d 必须有日期形式的标签，而且日期恰好落在"当地那一天的
//     第一个刻度"上（判据见下）；
//  4. **时区**：每一个标签都等于"它代表的那一刻在**服务端时区**下的写法" ——
//     期望值用 Go 的 time 包按 --timezone 现算（Intl 与 zoneinfo 是两套彼此
//     独立的实现），浏览器本地时区比服务端慢一小时，用本地时区渲染必然对不上。
//
// 数据造的是**跨 7 天**的一段探测曲线（300 秒一个点）：1d/3d/7d 三档因此真的
// 跨了天，而不是"窗口里只有一个白天"。
func TestXAxisLabelsMatchTargetsInRealBrowser(t *testing.T) {
	chrome := findChrome()
	if chrome == "" {
		t.Skip("找不到 Chrome：设置 PROBE_CHROME 或把它装到默认位置后这条用例会自动跑起来")
	}
	loc, err := time.LoadLocation(tzTestZone)
	if err != nil {
		t.Fatalf("加载时区 %s: %v", tzTestZone, err)
	}
	now := time.Now()
	if _, serverOff := now.In(loc).Zone(); func() bool { _, b := now.In(time.Local).Zone(); return b == serverOff }() {
		t.Skipf("本机时区与服务端时区 %s 的偏移相同：时区那条断言没有区分度", tzTestZone)
	}

	h, nodeID := startAxisFixture(t, loc)

	proxy := newHarnessProxy(t, "http://"+h.addr, tzHarnessConfig{
		NodeID: nodeID, NodeName: "axis-01", Scenario: "axis",
		User: "admin", Pass: "a-very-good-password",
	}, axisHarnessJS)
	mock := newMockServer(t, proxy)

	raw := runChromeForResult(t, chrome, mock.URL+"/", proxy.result, 180*time.Second, "1500,1100")

	var res axisResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("解析浏览器回传的结果: %v\n原始内容：%s", err, tail(string(raw), 600))
	}
	if res.Fatal != "" {
		t.Fatalf("浏览器里的自检流程没跑完：%s", res.Fatal)
	}
	if len(res.Errs) != 0 {
		t.Fatalf("浏览器里有 %d 条 JS 报错：%v", len(res.Errs), res.Errs)
	}
	if res.ServerTZ != tzTestZone {
		t.Errorf("接口下发的服务端时区 = %q，期望 %q", res.ServerTZ, tzTestZone)
	}
	t.Logf("浏览器时区 = %s；服务端时区 = %s；画布宽 %.0f", res.BrowserTZ, res.ServerTZ, res.CanvasW)
	if len(res.Ranges) != 6 {
		t.Fatalf("只量到 %d 档（期望 6 档）：%s", len(res.Ranges), res.Steps)
	}

	gap := xLabelMinGap(t)
	bands := map[string][2]int{
		"1h": {8, 12}, "6h": {8, 12}, "12h": {8, 12},
		"1d": {8, 12}, "3d": {6, 10}, "7d": {7, 10},
	}
	dateRanges := map[string]bool{"1d": true, "3d": true, "7d": true}
	discriminating := 0

	for _, r := range res.Ranges {
		discriminating += checkAxisRange(t, r, loc, gap, bands[r.Key], dateRanges[r.Key])
	}
	// 时区那条断言必须真的有区分度：服务端与浏览器时区不同时，至少要有一个标签
	// 在两边的写法不一样（全一样的话，这条用例对"用的是哪个时区"什么都没证明）。
	if discriminating == 0 {
		t.Error("没有任何一个标签能区分服务端时区与浏览器本地时区：这条时区断言是空的，请换一个时区或数据")
	}
	t.Logf("有 %d 个标签在「服务端时区」与「浏览器本地时区」下写法不同（时区断言的区分度）", discriminating)
}

// startAxisFixture 起一个真服务端并铺一段**跨 7 天**的探测曲线，返回服务端与节点 id。
//
// 探测间隔 300 秒：断线判据（1.5 × 桶宽）不会把这段曲线切成孤立点，
// 而 300 秒一点正好让六档的窗口里都有足够的点（1h 档十几个），
// 同时 1d/3d/7d 三档真的跨了好几天（日期标签必须有东西可标）。
func startAxisFixture(t *testing.T, loc *time.Location) (*harness, int64) {
	t.Helper()
	logs := &captureHandler{}
	h := startServerAt(t, "127.0.0.1:0", filepath.Join(t.TempDir(), "probe.db"), loc, slog.New(logs), nil)
	br := newBrowser(t, "http://"+h.addr)
	setupAdmin(t, br, waitSetupCode(t, logs))
	nodeID, _ := createNodeViaAPI(t, br, "axis-01")
	targetID := createPingTarget(t, br, "CF", "1.1.1.1", 443, 300)
	seedPingBuckets(t, h, nodeID, targetID, 300, 7*24*time.Hour)
	return h, nodeID
}

// xLabelMinGap 从 chart.js 里读出最小间距常量：断言必须用**代码里那个数**，
// 不能在这里写第二份（写两份的话，改了代码而没改测试，这条用例就成了摆设）。
func xLabelMinGap(t *testing.T) float64 {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "web", "chart.js"))
	if err != nil {
		t.Fatalf("读取 web/chart.js: %v", err)
	}
	m := regexp.MustCompile(`var X_LABEL_MIN_GAP = (\d+);`).FindStringSubmatch(string(data))
	if m == nil {
		t.Fatal("web/chart.js 里找不到 X_LABEL_MIN_GAP")
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("解析 X_LABEL_MIN_GAP=%q: %v", m[1], err)
	}
	return float64(n)
}

var (
	axisHHMM     = regexp.MustCompile(`^\d{2}:\d{2}$`)
	axisMMDD     = regexp.MustCompile(`^\d{2}-\d{2}$`)
	axisMMDDHHMM = regexp.MustCompile(`^\d{2}-\d{2} \d{2}:\d{2}$`)
)

// checkAxisRange 核对一档：间距、个数、标签内容（服务端时区）、跨天日期。
// 返回值是"这一档里有多少个标签在服务端时区与浏览器本地时区下写法不同"
// （0 = 这一档对时区没有区分度）。
func checkAxisRange(t *testing.T, r axisRange, loc *time.Location, gap float64, band [2]int, dateRange bool) int {
	t.Helper()
	discriminating := 0
	if len(r.Labels) < 2 {
		t.Fatalf("%s 档只画了 %d 个 X 轴标签（t0=%d t1=%d）：这一档没东西可验", r.Key, len(r.Labels), int64(r.T0), int64(r.T1))
	}
	step := axisStep(t, r)

	// 1) 反推每个标签代表哪一刻，并用 Go 的 time 包按服务端时区核对它的写法。
	anchors := make([]int64, 0, len(r.Labels))
	texts := make([]string, 0, len(r.Labels))
	for i, lb := range r.Labels {
		anchor := anchorOf(r, lb, step)
		anchors = append(anchors, anchor)
		texts = append(texts, lb.Text)
		tm := time.Unix(anchor, 0)
		wantServer := ""
		switch {
		case axisHHMM.MatchString(lb.Text):
			wantServer = tm.In(loc).Format("15:04")
		case axisMMDD.MatchString(lb.Text):
			wantServer = tm.In(loc).Format("01-02")
		case axisMMDDHHMM.MatchString(lb.Text):
			wantServer = tm.In(loc).Format("01-02 15:04")
		default:
			t.Errorf("%s 档的第 %d 个标签 %q 不是 HH:MM / MM-DD / MM-DD HH:MM 中的任何一种", r.Key, i+1, lb.Text)
			continue
		}
		if lb.Text != wantServer {
			t.Errorf("%s 档的第 %d 个标签 = %q（锚点 ts=%d）：服务端时区 %s 下应当是 %q，浏览器本地时区下才是 %q",
				r.Key, i+1, lb.Text, anchor, tzTestZone, wantServer, tm.In(time.Local).Format(labelLayoutOf(lb.Text)))
			continue
		}
		if wantServer != tm.In(time.Local).Format(labelLayoutOf(lb.Text)) {
			discriminating++
		}
		// 锚点必须落在刻度网格上（不是被"挤"到某个位置上的）：
		if anchor%int64(step) != 0 {
			t.Errorf("%s 档的标签 %q 反推出的锚点 ts=%d 不在 %d 秒的网格上", r.Key, lb.Text, anchor, int64(step))
		}
	}

	// 2) 间距：标签是居中锚定的，所以比的是**边缘**之间的空白（与 chart.js 一致）。
	minGap := math.Inf(1)
	minGapAt := -1
	for i := 1; i < len(r.Labels); i++ {
		left := r.Labels[i].X - 0.5 - r.Labels[i].W/2
		prevRight := r.Labels[i-1].X - 0.5 + r.Labels[i-1].W/2
		d := left - prevRight
		if d < minGap {
			minGap = d
			minGapAt = i
		}
	}
	t.Logf("%s 档：%d 个标签（目标 %d~%d），间隔 %d 秒，最小空白 %.1fpx（要求 ≥ %.0f，最窄处在第 %d 个标签前）；标签 = %v",
		r.Key, len(r.Labels), band[0], band[1], int64(step), minGap, gap, minGapAt+1, texts)
	// 半个像素是 fillText 的 Math.round 带来的（chart.js 里就是 +0.5）。
	if minGap < gap-1 {
		t.Errorf("%s 档相邻标签的最小空白只有 %.1fpx（chart.js 要求 ≥ %.0fpx）：还是挤在一起",
			r.Key, minGap, gap)
	}

	// 3) 个数落在用户定稿的目标区间里。
	if len(r.Labels) < band[0] || len(r.Labels) > band[1] {
		t.Errorf("%s 档画了 %d 个标签，落在目标区间 %d~%d 之外（画布宽 %.0f）",
			r.Key, len(r.Labels), band[0], band[1], r.CanvasW)
	}

	// 4) 跨天档：必须有日期标签，而且**日期只出现在跨天的那一格上**
	//    （或者整排的第一个标签 —— 按设计它也要写明"这一段是哪天的"）。
	//
	//    为什么不要求"零点本身"：刻度锚在绝对时间网格上，服务端时区的偏移可以是
	//    任意分钟数（+09:00 时两小时的网格落在当地的奇数小时上），零点根本不落在
	//    刻度上。要守的不变量是"每一天有且只有一个标签带日期"，判据就是"它与上一个
	//    标签不是同一天"（见 app.js 的 startsNewDay）。
	//
	//    带时分的 MM-DD HH:MM（3d/7d 间隔细于一天时的格式）不算"日期标记"：
	//    那里日期只是时间戳的一部分，每一格都有。
	dates := 0
	distinct := map[string]bool{}
	for i, lb := range r.Labels {
		if !axisMMDD.MatchString(lb.Text) && !axisMMDDHHMM.MatchString(lb.Text) {
			continue
		}
		dates++
		anchor := anchors[i]
		day := time.Unix(anchor, 0).In(loc).Format("2006-01-02")
		distinct[day] = true
		if !axisMMDD.MatchString(lb.Text) || i == 0 {
			continue
		}
		prevDay := time.Unix(anchors[i-1], 0).In(loc).Format("2006-01-02")
		if day == prevDay {
			t.Errorf("%s 档的第 %d 个标签写成了日期 %q（ts=%d），但它与上一个标签是同一天：日期只该出现在每天的第一个标签上",
				r.Key, i+1, lb.Text, anchor)
		}
	}
	if dateRange {
		if dates == 0 {
			t.Errorf("%s 档一个日期标签都没有（%v）：跨天的窗口里分不清哪段是哪天", r.Key, texts)
		}
		if len(distinct) < 2 {
			t.Errorf("%s 档只标出了 %d 个不同的日期（数据跨了好几天）：日期没有真的跟着天走", r.Key, len(distinct))
		}
	} else if dates != 0 {
		t.Errorf("%s 档出现了 %d 个日期标签：窗口 ≤ 12 小时时应当全是时分", r.Key, dates)
	}
	return discriminating
}

// labelLayoutOf 给出"同一种标签在浏览器本地时区下该长什么样"的布局，
// 只用于把区分度打进日志（服务端与本地时区不同时，两个值必然不同）。
func labelLayoutOf(text string) string {
	switch {
	case axisMMDDHHMM.MatchString(text):
		return "01-02 15:04"
	case axisMMDD.MatchString(text):
		return "01-02"
	default:
		return "15:04"
	}
}

// axisStep 从相邻标签的间距反推这一档实际用的刻度（秒）。
//
// 标签锚点都在同一张网格上、而且相邻两个之间只差一个刻度（越界的那几个在
// labelFits 里就被丢掉了，不会在中间留下空洞），所以"最小的那个差"就是刻度。
func axisStep(t *testing.T, r axisRange) float64 {
	t.Helper()
	raw := make([]float64, 0, len(r.Labels))
	plotW := r.CanvasW - r.PlotLeft - r.PlotRight
	for _, lb := range r.Labels {
		raw = append(raw, r.T0+(lb.X-0.5-r.PlotLeft)/plotW*(r.T1-r.T0))
	}
	minDiff := math.Inf(1)
	for i := 1; i < len(raw); i++ {
		if d := raw[i] - raw[i-1]; d < minDiff {
			minDiff = d
		}
	}
	// 吸附到最近的整齐刻度（chart.js 的 X_STEP_LADDER 就是这些值）。
	//
	// 容差按**像素**折算：标签的 x 被 Math.round 到半个像素，反推回时间就是
	// "半个像素代表多久"（7d 档画布 1400px、跨度 7 天时约 226 秒），所以容差必须
	// 跟着跨度走；而阶梯里相邻两级至少差一倍，这点误差不会让吸附跳级。
	tol := (r.T1-r.T0)/plotW*1.5 + 1
	ladder := []float64{60, 120, 300, 600, 900, 1800, 3600, 7200, 10800, 21600, 43200, 86400, 172800, 604800}
	best := ladder[0]
	for _, v := range ladder {
		if math.Abs(v-minDiff) < math.Abs(best-minDiff) {
			best = v
		}
	}
	if math.Abs(best-minDiff) > tol {
		t.Errorf("%s 档相邻标签的间距 ≈ %.0f 秒，不属于任何一级整齐刻度（X_STEP_LADDER）：标签网格不是整齐分档", r.Key, minDiff)
	}
	return best
}

// anchorOf 把"标签画在哪个像素上"反推回它代表的时刻（chart.js 的线性映射，
// x(ts) = left + (ts-t0)/(t1-t0) * (w-left-right)，画的时候 x 被 Math.round + 0.5）。
func anchorOf(r axisRange, lb axisLabel, step float64) int64 {
	plotW := r.CanvasW - r.PlotLeft - r.PlotRight
	raw := r.T0 + (lb.X-0.5-r.PlotLeft)/plotW*(r.T1-r.T0)
	return int64(math.Round(raw/step)) * int64(math.Round(step))
}

// ---------------------------------------------------------------- 观测值

type axisResult struct {
	Errs      []string    `json:"errs"`
	Fatal     string      `json:"fatal"`
	Steps     []string    `json:"steps"`
	BrowserTZ string      `json:"browserTZ"`
	ServerTZ  string      `json:"serverTZ"`
	CanvasW   float64     `json:"canvasW"`
	Ranges    []axisRange `json:"ranges"`
}

type axisRange struct {
	Key       string      `json:"key"`
	CanvasW   float64     `json:"canvasW"`
	CanvasH   float64     `json:"canvasH"`
	PlotLeft  float64     `json:"plotLeft"`
	PlotRight float64     `json:"plotRight"`
	BottomY   float64     `json:"bottomY"`
	T0        float64     `json:"t0"`
	T1        float64     `json:"t1"`
	Points    int         `json:"points"`
	Labels    []axisLabel `json:"labels"`
}

type axisLabel struct {
	Text string  `json:"text"`
	X    float64 `json:"x"`
	W    float64 `json:"w"`
}

// axisHarnessJS 是注入到页面里的自检脚本（与 tz/latency 那两条同一套做法：
// 真服务端 + 反代注入 + 结果 POST 回 mock）。
//
// 它只做两件事：**按用户的真实操作切档位**（点详情页延迟卡的那排按钮），
// 以及**把 canvas 上真正画出来的字记下来**（X 轴那一行的文案、x 坐标、
// 用同一个 ctx 现量的文本宽度）。一句断言都不在这里做。
const axisHarnessJS = `(function () {
  'use strict';
  var CFG = window.__TZCFG || {};
  var rawFetch = window.fetch.bind(window);
  var R = { errs: [], fatal: '', steps: [], browserTZ: '', serverTZ: '', canvasW: 0, ranges: [] };
  window.__AXISRESULT = R;

  // 截图模式：把 EventSource 换成一个不联网的替身。
  // 为什么必须换：--virtual-time-budget 在"还有网络请求挂着"时会**暂停虚拟时间**，
  // 而实时流（SSE）正是一条永远挂着的请求 —— 预算永远耗不完，Chrome 也就不截图
  // （只能被杀掉）。截图要的是一屏静态画面，首页那些数字仍然由 /api/v1/nodes
  // 一次取回来，不依赖实时流。
  if (CFG.shot) {
    window.EventSource = function () {
      var listeners = {};
      this.addEventListener = function (name, fn) { (listeners[name] = listeners[name] || []).push(fn); };
      this.close = function () {};
      // 照常报一次 open：右上角的「实时」标记要亮着，截图才与真机一致。
      setTimeout(function () {
        (listeners['open'] || []).forEach(function (fn) { fn({ type: 'open' }); });
      }, 0);
    };
  }

  window.addEventListener('error', function (e) { R.errs.push('error: ' + (e.message || e.type)); });
  window.addEventListener('unhandledrejection', function (e) {
    var r = e.reason;
    R.errs.push('rejection: ' + (r && r.message ? r.message : String(r)));
  });
  try { R.browserTZ = Intl.DateTimeFormat().resolvedOptions().timeZone; } catch (e) { R.browserTZ = '?'; }

  // ---- 钩 canvas：延迟图这一帧画在 X 轴那一行（y = 画布高 - 16）的字 ----
  // 宽度必须用**同一个 ctx** 现量：字号是 chart.js 在画标签前设的 11px，
  // Go 那边靠它把"锚点"还原成"标签左右边缘"，才能算真实的空白。
  var drawn = [];
  (function () {
    var proto = CanvasRenderingContext2D.prototype;
    var origFill = proto.fillText;
    proto.fillText = function (text, x, y) {
      if (this.canvas && this.canvas.id === 'chart-lat') {
        var w = 0;
        try { w = this.measureText(String(text)).width; } catch (e) { w = 0; }
        drawn.push({ text: String(text), x: x, y: y, w: w });
      }
      return origFill.apply(this, arguments);
    };
  })();

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

  // ---- 取数完成的通知：/ping 每次回来都记一笔 ------------------------------
  var pingDone = 0;
  window.fetch = function (input, init) {
    var url = typeof input === 'string' ? input : ((input && input.url) || '');
    return rawFetch(input, init).then(function (res) {
      if (/\/ping\?range=/.test(url)) pingDone++;
      return res;
    });
  };

  function clickRange(key) {
    var box = node('lat-ranges');
    if (!box) throw new Error('找不到延迟卡的档位容器 #lat-ranges');
    var btns = box.querySelectorAll('button');
    for (var i = 0; i < btns.length; i++) {
      if (btns[i].textContent === key) { btns[i].click(); return true; }
    }
    throw new Error('档位按钮里没有 ' + key);
  }

  function activeRange() {
    var box = node('lat-ranges');
    if (!box) return '';
    var btn = box.querySelector('button.active');
    return btn ? btn.textContent : '';
  }

  function switchLatRange(key) {
    // 已经停在这一档（默认就是 1h）时点它不会重新取数 —— 直接量眼前这一帧。
    if (activeRange() === key) return sleep(300);
    var want = pingDone + 1;
    clickRange(key);
    return waitFor('延迟曲线重新取数（' + key + '）', function () { return pingDone >= want; }, 20000)
      .then(function () { return sleep(300); });
  }

  // captureAxis 触发一次**完整重画**（resize → chart.js 80ms 防抖后整张重画），
  // 再把这一次重画里落在 X 轴那一行的字全部收下来。
  function captureAxis(key, t0, t1, points) {
    var canvas = node('chart-lat');
    var rect = canvas.getBoundingClientRect();
    var h = Math.round(rect.height);
    var bottomY = h - 16;   // chart.js: g.top + plotH + 4，而 g.top+plotH = h - 20
    var mark = drawn.length;
    window.dispatchEvent(new Event('resize'));
    return sleep(600).then(function () {
      var labels = [];
      for (var i = mark; i < drawn.length; i++) {
        if (Math.abs(drawn[i].y - bottomY) < 0.51) labels.push(drawn[i]);
      }
      R.canvasW = Math.round(rect.width);
      R.ranges.push({
        key: key, canvasW: Math.round(rect.width), canvasH: h,
        plotLeft: 64, plotRight: 8, bottomY: bottomY,
        t0: t0, t1: t1, points: points, labels: labels
      });
      return true;
    });
  }

  function measure(key) {
    return switchLatRange(key)
      .then(function () { return getJSON('/api/v1/nodes/' + CFG.nodeID + '/ping?range=' + key); })
      .then(function (data) {
        var pts = (data.targets && data.targets[0] && data.targets[0].points) || [];
        if (pts.length < 3) throw new Error(key + ' 档只有 ' + pts.length + ' 个点，画不出轴');
        return captureAxis(key, pts[0][0], pts[pts.length - 1][0], pts.length);
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
        return waitFor('延迟卡的档位按钮就绪', function () {
          return node('lat-ranges') && node('lat-ranges').querySelectorAll('button').length === 6;
        }, 30000);
      })
      .then(function () {
        // 探测目标卡片与那一行开关是 /ping 回来之后才渲染的：它出现了就说明
        // 数据已经到手（此时 canvas 上至少画过一帧）。
        return waitFor('延迟图已绘制一帧', function () { return drawn.length > 5; }, 30000);
      })
      .then(function () { return sleep(400); });
  }

  function run() {
    var ranges = ['1h', '6h', '12h', '1d', '3d', '7d'];
    return waitFor('页面脚本就绪', function () { return !!node('view-login'); }, 30000)
      .then(login)
      .then(function () {
        return getJSON('/api/v1/nodes').then(function (data) {
          R.serverTZ = (data.server && data.server.timezone) || '';
          return true;
        });
      })
      .then(openDetail)
      .then(function () {
        return ranges.reduce(function (chain, key) {
          return chain.then(function () { return measure(key); });
        }, Promise.resolve());
      });
  }

  // shot 模式（人工截图用）：把延迟图切到指定档位、只留这一张卡（近景），
  // 然后停住不回传结果，只把标题改成 SHOT-READY（截图用例靠 --virtual-time-budget 等它）。
  //
  // 近景用**隐藏其它块**而不是滚动定位：图表是异步加载的，页面高度在加载过程中
  // 会变，scrollIntoView 算出来的位置到截图时已经不对了（第一版就滚到了一片空白上）。
  function shot() {
    var key = CFG.shot;
    return login().then(openDetail).then(function () {
      return switchLatRange(key);
    }).then(function () {
      var css = document.createElement('style');
      css.textContent =
        'header.top, footer { display: none !important; }' +
        '.detail-head, .stat-row, .info-grid, #charts-resources { display: none !important; }' +
        '.detail-wrap { padding-top: 0 !important; margin-top: 0 !important; }' +
        '#charts-latency { margin: 0 !important; }';
      document.head.appendChild(css);
      window.scrollTo(0, 0);
      return sleep(500);
    }).then(function () {
      window.scrollTo(0, 0);
      document.title = 'SHOT-READY:axis-' + key;
      return true;
    });
  }

  function finish() {
    return rawFetch('/__result', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(R)
    }).then(function () { document.title = 'DUMPMARK:axis'; });
  }

  window.addEventListener('load', function () {
    if (CFG.shot) {
      // 截图模式：跑完就停（不回传结果 —— 截图靠 --virtual-time-budget + --screenshot）。
      shot().catch(function (err) {
        document.title = 'SHOT-FAILED:' + String(err && err.message ? err.message : err);
      });
      return;
    }
    run().catch(function (err) {
      R.fatal = String(err && err.message ? err.message : err);
    }).then(finish, finish);
  });
})();`
