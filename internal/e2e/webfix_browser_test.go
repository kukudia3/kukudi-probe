package e2e

import (
	"encoding/json"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"probe/internal/config"
)

// 这一份用例钉住的是第二阶段修掉的五处前端缺陷。它们全都有一个共同点：
// **静态看代码看不出来**（CSS 的层叠、异步响应链的先后、hidden 属性的视觉残留），
// 所以必须在真浏览器里、按用户的真实操作走一遍才算验证过。
//
// 用的仍然是本仓库惯用的那一套：真服务端（Server.Run 完整路径）+ 反代注入自检脚本
// + 页面把观测值 POST 回 /__result。**不用** --dump-dom + --virtual-time-budget：
// 首页连着 SSE 长连接，虚拟时间会一直暂停（见 tz_browser_test.go 的说明）。

const (
	webFixNameA = "webfix-A"
	webFixNameB = "webfix-B"
)

// webFixResult 是浏览器回传的观测值。
type webFixResult struct {
	Errs   []string       `json:"errs"`
	Steps  []string       `json:"steps"`
	Fatal  string         `json:"fatal"`
	Checks map[string]any `json:"checks"`
	Notes  map[string]any `json:"notes"`
}

func TestWebFixesInRealBrowser(t *testing.T) {
	chrome := findChrome()
	if chrome == "" {
		t.Skip("找不到 Chrome：设置 PROBE_CHROME 或把它装到默认位置后这条用例会自动跑起来")
	}

	logs := &captureHandler{}
	h := startServerAt(t, "127.0.0.1:0", filepath.Join(t.TempDir(), "probe.db"), time.UTC, slog.New(logs),
		func(cfg *config.Server) { cfg.FX = false })
	br := newBrowser(t, "http://"+h.addr)
	setupAdmin(t, br, waitSetupCode(t, logs))

	// 两台机器：A 是"先看的那一台"，B 是"切过去的那一台"。两台都要有 CPU 数据，
	// 否则图表走的是「暂无数据」那条路，断言就失去区分度了。
	idA, _ := createNodeViaAPI(t, br, webFixNameA)
	idB, _ := createNodeViaAPI(t, br, webFixNameB)
	seedCPUSamples(t, h, idA)
	seedCPUSamples(t, h, idB)
	// 一个探测目标：延迟那一块的开关行（卡片 + chip）只有配了目标才渲染得出来，
	// 而"重建之后焦点还在不在"必须在那块 DOM 上才验得了。不用喂数据：卡片上的
	// 「暂无数据」也是有卡片、有 chip 的。
	createPingTarget(t, br, "webfix-ping", "127.0.0.1", 80, 60)

	cfg := tzHarnessConfig{
		NodeID:   idA,
		NodeName: webFixNameA,
		Nodes:    map[string]int64{webFixNameA: idA, webFixNameB: idB},
		// Focus = B：脚本里凡是要用"另一台机器"的地方都用它。
		Focus:    webFixNameB,
		User:     "admin",
		Pass:     "a-very-good-password",
		Scenario: "webfix",
	}
	proxy := newHarnessProxy(t, "http://"+h.addr, cfg, webFixHarnessJS)
	mock := newMockServer(t, proxy)

	// 超时给足：脚本里有一段 35 秒的"野定时器窗口"（见 waitWildWindow）。
	raw := runChromeForResult(t, chrome, mock.URL+"/#/", proxy.result, 300*time.Second, "1500,1100")

	var res webFixResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("解析浏览器回传的结果: %v\n原始内容：%s", err, tail(string(raw), 600))
	}
	if res.Fatal != "" {
		t.Fatalf("浏览器里的自检流程没跑完：%s（已完成步骤：%v）", res.Fatal, res.Steps)
	}
	if len(res.Errs) != 0 {
		t.Fatalf("浏览器里有 %d 条 JS 报错：%v", len(res.Errs), res.Errs)
	}
	t.Logf("已完成步骤：%v", res.Steps)
	t.Logf("观测到的中间值：%v", res.Notes)

	boolCheck := func(name string) bool {
		v, _ := res.Checks[name].(bool)
		return v
	}
	strCheck := func(name string) string {
		v, _ := res.Checks[name].(string)
		return v
	}

	// ---- ① label.check 缺 [hidden] 兜底：新增节点对话框里多出一个「启用」勾选框 ----
	//
	// 断言的是**渲染结果**（getComputedStyle），而不是 hidden 属性：属性一直是对的，
	// 错的是它压不过 label.check 的 display:flex。
	if !boolCheck("addHiddenAttr") {
		t.Error("「新增节点」对话框里 #node-enabled-wrap 的 hidden 属性应当是 true")
	}
	if got := strCheck("addDisplay"); got != "none" {
		t.Errorf("「新增节点」对话框里「启用」勾选框的 display = %q，期望 none —— 作者样式的 display:flex 盖掉了 [hidden]", got)
	}
	// 反向的一半：编辑模式下它必须**真的看得见**，否则上面那条断言是空的
	// （一个永远不显示的勾选框也能让 display 是 none）。
	if !boolCheck("editVisible") {
		t.Error("「编辑节点」对话框里 #node-enabled-wrap 不应当是 hidden")
	}
	if got := strCheck("editDisplay"); got != "flex" {
		t.Errorf("「编辑节点」对话框里「启用」勾选框的 display = %q，期望 flex（它必须真的显示出来）", got)
	}

	// ---- ② openDetail 的过期守卫：离开详情页后不许再建 30 秒定时器 ----
	if boolCheck("wildTimerCreated") {
		t.Errorf("离开详情页之后又建出了一个 30 秒定时器（interval 个数 %v → %v）：迟到的响应链没有过期守卫，它会一直打 GET /api/v1/nodes/0",
			res.Notes["intervalsBefore"], res.Notes["intervalsAfter"])
	}
	// 同一个 bug 的最终症状：回到首页之后，浏览器真的去请求了 /api/v1/nodes/0。
	// 这一段要在首页**静置 35 秒**才观测得到（30 秒的定时器 + 1.8 秒的迟到响应），
	// 所以它被安排在整轮脚本的最后几段里，期间不做任何导航。
	if !boolCheck("noZeroNodeRequest") {
		t.Errorf("回到首页后出现了 GET /api/v1/nodes/0：%v", res.Notes["zeroNodeRequests"])
	}
	// 同一处守卫的另一半：A→B 直接切节点时，A 的迟到响应不许把 B 的页面覆盖掉。
	if !boolCheck("staleChainKeptB") {
		t.Errorf("切到 B 之后，A 那条迟到的响应把页面改成了 %q（地址仍是 %v）",
			res.Notes["detailNameAfterStale"], res.Notes["hashAfterStale"])
	}

	// ---- ③ 切到后台再回前台：只要还在应用里就要重连实时流 ----
	if !boolCheck("stoppedInBackground") {
		t.Error("切到后台时应当断开实时流（省流量那一半不能被改掉）")
	}
	if !boolCheck("reconnectedOnForeground") {
		t.Errorf("停在详情页时回到前台没有重连实时流（EventSource 创建次数 %v → %v）：实时数据会一直是断的",
			res.Notes["esAfterBackground"], res.Notes["esAfterForeground"])
	}
	if !boolCheck("reconnectedOnHome") {
		t.Error("停在首页时回到前台也必须重连实时流（这条是原有行为，不能被改坏）")
	}

	// ---- ④ closeDetail / openDetail 清空图表数据（而不是销毁实例） ----
	if !boolCheck("chartClearedOnSwitch") {
		t.Errorf("A→B 切节点之后，CPU 画布没有被清空成「暂无数据」（这一瞬画上画的是：%v）",
			res.Notes["chartTextAfterSwitch"])
	}
	if !boolCheck("chartClearedOnLeave") {
		t.Errorf("离开详情页之后，CPU 画布上仍留着上一个节点的曲线（这一瞬画上画的是：%v）",
			res.Notes["chartTextAfterLeave"])
	}

	// ---- chart.js：极窄画布（隐藏的图表块）上那一帧必须画得出来 ----
	//
	// 这一条同时是"清空图表数据"那一改的护栏：clearDetailCharts 会在图表块隐藏时
	// 也调一次 setData，而隐藏的 canvas 量出来是 0 宽 —— layout() 会把它夹到 120px。
	// 极窄 + 抽稀边界是一组很容易退化的输入，这里把它扫一遍。
	if !boolCheck("chartDrewOnTinyCanvas") {
		t.Errorf("极窄画布（120px，图表块隐藏时就是这一档）上画图抛错或什么都没画：%v",
			res.Notes["chartFailures"])
	}
	// destroy() 必须把挂上去的监听一个不漏地摘掉（含 touchcancel 那一个）。
	if !boolCheck("touchcancelRegistered") {
		t.Errorf("canvas 没有注册 touchcancel（挂上的监听：%v）：触摸被系统接管时浮层会停在屏幕上",
			res.Notes["chartAdded"])
	}
	if !boolCheck("destroyRemovedEveryListener") {
		t.Errorf("destroy() 之后还有监听留在 canvas 上：%v（挂上 %v / 摘掉 %v）",
			res.Notes["chartLeaks"], res.Notes["chartAdded"], res.Notes["chartRemoved"])
	}

	// ---- 延迟开关行：数据刷新重建之后，键盘焦点要回到同一个控件 ----
	if !boolCheck("cardFocusKept") {
		t.Errorf("延迟卡片重建之后焦点没回到同一张卡片：重建前 %q（真的是焦点吗 %v）→ 重建后 %q",
			res.Notes["focusCardKey"], res.Notes["focusBeforeCard"], res.Notes["focusAfterCard"])
	}
	if !boolCheck("chipFocusKept") {
		t.Errorf("延迟 chip 重建之后焦点没回到同一枚 chip：重建前 %v → 重建后 %q",
			res.Notes["focusBeforeChip"], res.Notes["focusAfterChip"])
	}

	// ---- ⑤ 退出登录：两步验证那一组私有值必须从 DOM 里消失 ----
	if !boolCheck("twofaPremise") {
		t.Fatalf("前置条件没成立：待确认密钥/otpauth 链接/二维码没有渲染出来（secret=%q url=%q qr=%q）",
			res.Notes["secretBefore"], res.Notes["urlBefore"], res.Notes["qrBefore"])
	}
	if !boolCheck("twofaClearedAfterLogout") {
		t.Errorf("退出登录后 2FA 那一组还留在 DOM 里：secret=%q url=%q qr=%q 恢复码条数=%v",
			res.Notes["secretAfter"], res.Notes["urlAfter"], res.Notes["qrAfter"], res.Notes["codesAfter"])
	}
}

// webFixHarnessJS 是注入到页面里的自检脚本。
//
// 它只做两件事：**装钩子**（fetch / setInterval / EventSource / canvas.fillText /
// document.hidden）与**按用户的真实操作驱动界面**（点新增节点、点编辑、切节点、
// 拨前后台开关、点启用两步验证、点退出登录），然后把观测值 POST 回去。
// 一句断言都不在这里做：期望值全部在 Go 侧。
//
// document.hidden 必须自己伪造：无头窗口永远是"可见"的，不拨这个开关就永远走不到
// visibilitychange 那两条分支。它在 Document.prototype 上是取值器，所以用
// defineProperty 在实例上加一个同名的自身属性把它盖掉即可。
const webFixHarnessJS = `(function () {
  'use strict';
  var CFG = window.__TZCFG || {};
  var IDA = CFG.nodeID;
  var IDB = (CFG.nodes || {})[CFG.focus] || 0;

  var R = { errs: [], steps: [], fatal: '', checks: {}, notes: {}, fetchLog: [], intervalLog: [] };
  window.__WEBFIXRESULT = R;
  R.notes.zeroNodeRequests = [];

  window.addEventListener('error', function (e) { R.errs.push('error: ' + (e.message || e.type)); });
  window.addEventListener('unhandledrejection', function (e) {
    var r = e.reason;
    R.errs.push('rejection: ' + (r && r.message ? r.message : String(r)));
  });

  function node(id) { return document.getElementById(id); }
  function textOf(id) { var n = node(id); return n ? n.textContent : ''; }
  function shown(id) { var n = node(id); return !!n && !n.hidden; }
  function sleep(ms) { return new Promise(function (r) { setTimeout(r, ms); }); }
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

  // ---- 钩 fetch：记下每一次请求；delayMap 让指定的接口"迟到" ----------------
  var rawFetch = window.fetch.bind(window);
  var delayMap = {};
  window.fetch = function (input, init) {
    var url = typeof input === 'string' ? input : ((input && input.url) || '');
    R.fetchLog.push(url);
    if (url.indexOf('/api/v1/nodes/0') >= 0) R.notes.zeroNodeRequests.push(url);
    var p = rawFetch(input, init);
    var ms = 0;
    Object.keys(delayMap).forEach(function (needle) {
      if (ms === 0 && url.indexOf(needle) >= 0) ms = delayMap[needle];
    });
    if (ms > 0) {
      return p.then(function (res) {
        return sleep(ms).then(function () {
          // delayedDone 是"一条被拖慢的响应真的交回给 app.js 了"的计数。
          // 用例靠它来等"迟到的响应落地"，而不是拍一个固定的 sleep：
          // 固定 sleep 会在边界上取样（响应刚到、那条链还没跑完），断言时红时绿。
          R.notes.delayedDone = (R.notes.delayedDone || 0) + 1;
          return res;
        });
      });
    }
    return p;
  };
  function waitDelayed(mark, what) {
    return waitFor(what, function () { return (R.notes.delayedDone || 0) > mark; }, 30000);
  }

  // ---- 钩 setInterval：数一数"每 30 秒一次"的定时器有没有多出来 -------------
  // app.js 里 30000ms 的定时器只有两处：详情页刷新（DETAIL_REFRESH_MS）与
  // "未登录时的会话复查"（登录之后它只是个空转的壳）。所以登录后基线恒为 1。
  var origSetInterval = window.setInterval.bind(window);
  window.setInterval = function (fn, ms) {
    R.intervalLog.push(ms);
    return origSetInterval(fn, ms);
  };
  function count30s() {
    var n = 0;
    R.intervalLog.forEach(function (v) { if (v === 30000) n++; });
    return n;
  }

  // ---- 钩 EventSource：数创建与关闭 ----------------------------------------
  var OrigES = window.EventSource;
  var origClose = OrigES.prototype.close;
  OrigES.prototype.close = function () {
    R.notes.esClosed = (R.notes.esClosed || 0) + 1;
    return origClose.apply(this, arguments);
  };
  function WrappedES(url, cfg) {
    R.notes.esCreated = (R.notes.esCreated || 0) + 1;
    return new OrigES(url, cfg);
  }
  WrappedES.prototype = OrigES.prototype;
  window.EventSource = WrappedES;

  // ---- 钩 canvas 的 fillText：图上真正画出来的字按画布 id 记下来 ------------
  // 只静态看代码证明不了"画布上现在是什么"，必须看它画了什么。
  var drawn = {};
  (function () {
    var proto = CanvasRenderingContext2D.prototype;
    var orig = proto.fillText;
    proto.fillText = function (text) {
      var id = this.canvas ? this.canvas.id : '';
      (drawn[id] || (drawn[id] = [])).push(String(text));
      return orig.apply(this, arguments);
    };
  })();
  function drawCount(id) { return (drawn[id] || []).length; }
  function since(id, mark) { return (drawn[id] || []).slice(mark); }

  // 帧计数器：chart.js 的 draw() 每一帧都以一次 clearRect 开头（chart.js:612），
  // 所以"清了几次屏"就是"画了几帧"——比数 fillText 靠谱（每一帧的字数会变）。
  // waitCPUCurve 靠它区分「这一帧画的是真曲线」与「这一帧是空态」。
  var frames = {};
  (function () {
    var proto = CanvasRenderingContext2D.prototype;
    var orig = proto.clearRect;
    proto.clearRect = function () {
      var id = this.canvas ? this.canvas.id : '';
      frames[id] = (frames[id] || 0) + 1;
      return orig.apply(this, arguments);
    };
  })();
  function frameCount(id) { return frames[id] || 0; }
  // clockLabelsSince 数"自 mark 以来画出来的时刻标签"（HH:MM 形状）：
  // 有它就说明这一帧画的是真曲线，一个都没有就只画了「暂无数据」。
  function clockLabelsSince(id, mark) {
    var n = 0;
    since(id, mark).forEach(function (t) {
      if (/^[0-9]{2}:[0-9]{2}$/.test(t)) n++;
    });
    return n;
  }
  function waitCPUCurve(what) {
    var mark = drawCount('chart-cpu');
    return waitFor(what, function () {
      return frameCount('chart-cpu') > 0 && clockLabelsSince('chart-cpu', mark) > 0;
    }, 30000);
  }

  // ---- 伪造 document.hidden ------------------------------------------------
  var hiddenFlag = false;
  try {
    Object.defineProperty(document, 'hidden', { configurable: true, get: function () { return hiddenFlag; } });
    Object.defineProperty(document, 'visibilityState', {
      configurable: true, get: function () { return hiddenFlag ? 'hidden' : 'visible'; }
    });
  } catch (e) { R.errs.push('无法覆盖 document.hidden: ' + e.message); }
  function setHidden(v) {
    hiddenFlag = v;
    document.dispatchEvent(new Event('visibilitychange'));
  }

  // ---- 登录与首页（与 tz_browser_test.go 同一套） --------------------------
  function loggedIn() { return shown('view-home') || shown('view-detail') || shown('view-settings'); }
  function login() {
    return waitFor('出现登录视图', function () { return shown('view-login') || loggedIn(); }, 30000)
      .then(function () {
        if (loggedIn()) return true;
        node('login-user').value = CFG.user;
        node('login-pass').value = CFG.pass;
        node('login-submit').click();
        return waitFor('登录后进入应用', loggedIn, 30000);
      });
  }
  function home() {
    window.location.hash = '#/';
    return waitFor('首页就绪', function () { return shown('view-home'); }, 30000)
      .then(function () {
        return waitFor('首页两张卡片出现', function () { return document.querySelectorAll('#grid .card').length >= 2; }, 30000);
      });
  }
  function openDetail(id, name) {
    window.location.hash = '#/n/' + id;
    return waitFor('详情页打开: ' + name, function () {
      return shown('view-detail') && textOf('detail-name') === name;
    }, 30000);
  }
  // ---- ① 新增/编辑节点对话框里的「启用」勾选框 -----------------------------
  function dialogCheck() {
    node('btn-add').click();
    return waitFor('新增节点对话框打开', function () { return node('dlg-node').open === true; }, 15000)
      .then(function () {
        R.checks.addHiddenAttr = node('node-enabled-wrap').hidden === true;
        R.checks.addDisplay = getComputedStyle(node('node-enabled-wrap')).display;
        node('node-cancel').click();
        return waitFor('新增对话框关闭', function () { return node('dlg-node').open === false; }, 15000);
      })
      .then(function () { return openDetail(IDA, CFG.nodeName); })
      .then(function () {
        return waitFor('编辑按钮可用', function () { return node('detail-edit') && !node('detail-edit').disabled; }, 15000);
      })
      .then(function () {
        node('detail-edit').click();
        return waitFor('编辑对话框打开', function () { return node('dlg-node').open === true; }, 15000);
      })
      .then(function () {
        R.checks.editVisible = node('node-enabled-wrap').hidden === false;
        R.checks.editDisplay = getComputedStyle(node('node-enabled-wrap')).display;
        node('node-cancel').click();
        return waitFor('编辑对话框关闭', function () { return node('dlg-node').open === false; }, 15000);
      });
  }

  // ---- ④ A→B 直接切节点：画布上不能留着 A 的曲线 ---------------------------
  // 这是一条**只在浏览器里**才成立的断言：进入详情页时 setView 让画布重新可见，
  // 而 chart.js 在尺寸没变时不会重新分配位图，所以只要没人清，旧曲线就还在。
  function chartSwitchCheck() {
    // 前提要先成立：A 的曲线**真的画出来了**（画布上出现 HH:MM 形状的 X 轴标签），
    // 否则"切走之后它还留着吗"这句话没有意义。
    return openDetail(IDA, CFG.nodeName)
      .then(function () { return waitCPUCurve('A 的曲线画出来了（画布上有 X 轴时刻标签）'); })
      .then(function () {
        R.notes.cpuFramesBeforeSwitch = frameCount('chart-cpu');
        // B 的数据故意迟到：留下的这段窗口期正是"旧曲线还在不在"能被看见的时刻。
        delayMap['/api/v1/nodes/' + IDB] = 1800;
        R.notes.cpuMarkBeforeSwitch = drawCount('chart-cpu');
        window.location.hash = '#/n/' + IDB;
        return sleep(400);
      })
      .then(function () {
        var after = since('chart-cpu', R.notes.cpuMarkBeforeSwitch);
        R.notes.chartTextAfterSwitch = after;
        R.checks.chartClearedOnSwitch = after.length > 0 && after.indexOf('暂无数据') >= 0;
        return waitFor('B 的详情页渲染完成', function () { return textOf('detail-name') === CFG.focus; }, 30000);
      })
      .then(function () { return waitCPUCurve('B 的曲线画出来了'); })
      .then(function () {
        // 离开详情页（回首页）：closeDetail 也要清，否则上一个节点的点数组一直可达。
        R.notes.cpuMarkBeforeLeave = drawCount('chart-cpu');
        window.location.hash = '#/';
        return waitFor('回到首页', function () { return shown('view-home'); }, 15000);
      })
      .then(function () {
        var after = since('chart-cpu', R.notes.cpuMarkBeforeLeave);
        R.notes.chartTextAfterLeave = after;
        R.checks.chartClearedOnLeave = after.length > 0 && after.indexOf('暂无数据') >= 0;
        delayMap = {};
        return true;
      });
  }

  // ---- ② 过期守卫：离开详情页之后，迟到的响应不许再建 30 秒定时器 -----------
  function wildTimerCheck() {
    return sleep(300)
      .then(function () {
        R.notes.intervalsBefore = count30s();
        delayMap['/api/v1/nodes/' + IDA] = 1800;
        R.notes.wildMark = R.notes.delayedDone || 0;
        window.location.hash = '#/n/' + IDA;
        return sleep(300);   // 请求已经发出、还挂在路上
      })
      .then(function () {
        window.location.hash = '#/';
        R.notes.wildWindowStart = Date.now();
        return waitFor('离开详情页回到首页', function () { return shown('view-home'); }, 15000);
      })
      .then(function () { return waitDelayed(R.notes.wildMark, 'A 的迟到响应落地'); })
      .then(function () { return sleep(800); })   // 那条链跑完（含 Promise.all 与第二个 .then）
      .then(function () {
        delayMap = {};
        R.notes.intervalsAfter = count30s();
        R.checks.wildTimerCreated = R.notes.intervalsAfter > R.notes.intervalsBefore;
        return true;
      });
  }

  // waitWildWindow 在首页**静置**到"那个野定时器本该响过一次"之后再检查。
  //
  // 30 秒的定时器 + 1.8 秒的迟到响应：从离开详情页算起，它最早会在 31.5 秒左右
  // 打出第一条 GET /api/v1/nodes/0。窗口给 35 秒是留 3.5 秒余量。
  function waitWildWindow() {
    var start = R.notes.wildWindowStart || Date.now();
    var left = 35000 - (Date.now() - start);
    if (left < 0) left = 0;
    return sleep(left).then(function () {
      R.checks.noZeroNodeRequest = R.notes.zeroNodeRequests.length === 0;
      return true;
    });
  }

  // ---- ②（另一半）A→B 直接切节点：A 的迟到响应不许覆盖 B 的页面 ------------
  function staleChainCheck() {
    delayMap['/api/v1/nodes/' + IDA] = 3600;
    var mark = R.notes.delayedDone || 0;
    window.location.hash = '#/n/' + IDA;
    return sleep(250)
      .then(function () {
        window.location.hash = '#/n/' + IDB;
        return waitFor('B 的详情页渲染完成（第二轮）', function () { return textOf('detail-name') === CFG.focus; }, 30000);
      })
      .then(function () { return waitDelayed(mark, 'A 的迟到响应落地（切节点那条）'); })
      .then(function () { return sleep(600); })   // 那条链跑完（renderDetailInfo 就在里面）
      .then(function () {
        delayMap = {};
        R.notes.detailNameAfterStale = textOf('detail-name');
        R.notes.hashAfterStale = window.location.hash;
        R.checks.staleChainKeptB = textOf('detail-name') === CFG.focus && window.location.hash === ('#/n/' + IDB);
        return true;
      });
  }

  // ---- ③ 前后台切换：详情页与首页都要重连实时流 ----------------------------
  function visibilityCheck() {
    return waitFor('停在详情页', function () { return shown('view-detail'); }, 15000)
      .then(function () {
        R.notes.esBeforeBackground = R.notes.esCreated || 0;
        var closedBefore = R.notes.esClosed || 0;
        setHidden(true);
        return sleep(300).then(function () {
          R.checks.stoppedInBackground = (R.notes.esClosed || 0) > closedBefore;
          R.notes.esAfterBackground = R.notes.esCreated || 0;
          setHidden(false);
          return sleep(800);
        });
      })
      .then(function () {
        R.notes.esAfterForeground = R.notes.esCreated || 0;
        R.checks.reconnectedOnForeground = R.notes.esAfterForeground > R.notes.esAfterBackground;
        // 首页那一档是**原有行为**：同一条用例里一起钉住，免得修详情页时把它改坏。
        window.location.hash = '#/';
        return waitFor('回到首页（前后台那条）', function () { return shown('view-home'); }, 15000);
      })
      .then(function () {
        var closedBefore = R.notes.esClosed || 0;
        R.notes.esHomeBefore = R.notes.esCreated || 0;
        setHidden(true);
        return sleep(300).then(function () {
          if ((R.notes.esClosed || 0) <= closedBefore) R.errs.push('首页切到后台没有断开实时流');
          setHidden(false);
          return sleep(800);
        });
      })
      .then(function () {
        R.checks.reconnectedOnHome = (R.notes.esCreated || 0) > R.notes.esHomeBefore;
        hiddenFlag = false;
        return true;
      });
  }

  // ---- ⑤ 退出登录：2FA 那一组私有值必须从 DOM 里消失 ----------------------
  function twofaCheck() {
    window.location.hash = '#/settings/security';
    return waitFor('安全栏就绪', function () { return shown('view-settings') && shown('twofa-start'); }, 30000)
      .then(function () {
        node('twofa-enable').click();
        return waitFor('待确认密钥已显示', function () {
          return shown('twofa-setup') && textOf('twofa-secret').length > 0;
        }, 30000);
      })
      .then(function () {
        R.notes.secretBefore = textOf('twofa-secret');
        R.notes.urlBefore = textOf('twofa-url');
        R.notes.qrBefore = node('twofa-qr').getAttribute('src') || '';
        R.notes.codesBefore = node('twofa-codes-list').childNodes.length;
        R.checks.twofaPremise =
          R.notes.secretBefore.length > 0 &&
          R.notes.urlBefore.indexOf('otpauth') >= 0 &&
          R.notes.qrBefore.indexOf('/api/v1/twofa/qr') >= 0;
        node('btn-logout').click();
        return waitFor('退出登录回到登录页', function () { return shown('view-login'); }, 30000);
      })
      .then(function () {
        R.notes.secretAfter = textOf('twofa-secret');
        R.notes.urlAfter = textOf('twofa-url');
        R.notes.qrAfter = node('twofa-qr').getAttribute('src') || '';
        R.notes.codesAfter = node('twofa-codes-list').childNodes.length;
        R.checks.twofaClearedAfterLogout =
          R.notes.secretAfter === '' && R.notes.urlAfter === '' &&
          R.notes.qrAfter === '' && R.notes.codesAfter === 0;
        return true;
      });
  }

  // ---- chart.js 的合成帧：极窄画布 + destroy 的监听台账 --------------------
  //
  // 这一条不经过用户操作：直接调 ProbeChart.create / setData / destroy，用**真浏览器
  // 里的真 chart.js** 去撞那几组最容易退化的输入（画布被 layout() 夹到 120px）。
  // 为什么必须这么撞：clearDetailCharts 会在图表块**隐藏**时也画一次，而隐藏的
  // canvas 量出来就是 0 宽 —— 也就是说 120px 那一档现在真的会走到。
  function chartSyntheticCheck() {
    var canvas = document.createElement('canvas');
    canvas.id = 'chart-synthetic';
    canvas.style.display = 'block';
    canvas.style.width = '120px';
    canvas.style.height = '80px';
    document.body.appendChild(canvas);

    // 5 个字符的标签（"12:34" 那个形状，11px 下约 28px 宽）：标签宽度直接决定抽稀
    // 判定，拿一个字母去撞是撞不出边界情况的。
    function xFormat(ts) {
      var m = Math.floor(ts / 60) % 60;
      return (m < 10 ? '0' : '') + m + ':00';
    }

    var failures = [];
    [[120, 60, 420, 86], [120, 60, 420, 0], [120, 60, 420, 59],
      [120, 300, 1800, 37], [140, 60, 420, 86], [160, 60, 600, 7],
      [120, 900, 3600, 11]].forEach(function (c) {
      var w = c[0], base = c[1], span = c[2], phase = c[3];
      canvas.style.width = w + 'px';
      var chart = window.ProbeChart.create(canvas, {
        tickBaseSec: base, xLabelMax: 12, yMax: 100,
        yFormat: function (v) { return v.toFixed(0) + '%'; },
        xFormat: xFormat
      });
      var points = [];
      for (var i = 0; i < 8; i++) points.push([phase + i * (span / 7), 10 + i, 12 + i]);
      var mark = drawCount('chart-synthetic');
      try {
        chart.setData([{ label: 'a', color: '#2563eb', points: points }], {});
      } catch (e) {
        failures.push('w=' + w + ' base=' + base + ' phase=' + phase + ' → ' + (e && e.message));
        return;
      }
      if (drawCount('chart-synthetic') <= mark) {
        failures.push('w=' + w + ' base=' + base + ' phase=' + phase + ' → 一个像素都没画');
      }
    });
    R.notes.chartFailures = failures;
    R.checks.chartDrewOnTinyCanvas = failures.length === 0;
    if (canvas.parentNode) canvas.parentNode.removeChild(canvas);

    // destroy 的台账用**一张干净画布**：上面那七张图都没销毁（它们的监听本来就该
    // 留着），混在一起数会把"漏摘"和"没销毁"算成同一件事。
    var canvas2 = document.createElement('canvas');
    canvas2.id = 'chart-synthetic-destroy';
    canvas2.style.display = 'block';
    canvas2.style.width = '300px';
    canvas2.style.height = '80px';
    document.body.appendChild(canvas2);
    // 页面脚本读不到 DevTools 的 getEventListeners，所以把这张画布自己的
    // add/removeEventListener 换成记账版（chart.js 是现取 canvas.addEventListener 的）。
    var added = {}, removed = {};
    var origAdd = canvas2.addEventListener.bind(canvas2);
    var origRemove = canvas2.removeEventListener.bind(canvas2);
    canvas2.addEventListener = function (type) {
      added[type] = (added[type] || 0) + 1;
      return origAdd.apply(null, arguments);
    };
    canvas2.removeEventListener = function (type) {
      removed[type] = (removed[type] || 0) + 1;
      return origRemove.apply(null, arguments);
    };
    var chart2 = window.ProbeChart.create(canvas2, {
      tickBaseSec: 60, yFormat: xFormat, xFormat: xFormat
    });
    chart2.setData([{ label: 'a', color: '#2563eb', points: [[0, 1, 1], [60, 2, 2]] }], {});
    chart2.destroy();
    var leaks = [];
    Object.keys(added).forEach(function (type) {
      if ((removed[type] || 0) < added[type]) leaks.push(type);
    });
    R.notes.chartAdded = added;
    R.notes.chartRemoved = removed;
    R.notes.chartLeaks = leaks;
    R.checks.destroyRemovedEveryListener = leaks.length === 0;
    R.checks.touchcancelRegistered = (added.touchcancel || 0) >= 1;
    if (canvas2.parentNode) canvas2.parentNode.removeChild(canvas2);
    return Promise.resolve(true);
  }

  // ---- 延迟开关行：30 秒一次的重建不能把键盘焦点甩掉 ------------------------
  //
  // 触发方式用**切延迟档位**（setPingRange → loadPingChart → renderLatToggles），
  // 它与 30 秒定时器走的是同一条重建路径，但不用等半分钟。
  function clickRange(boxId, key) {
    var btns = node(boxId).querySelectorAll('button');
    for (var i = 0; i < btns.length; i++) {
      if (btns[i].textContent === key) { btns[i].click(); return true; }
    }
    return false;
  }
  function waitCardsRebuilt(oldNode) {
    return waitFor('延迟卡片已重建', function () {
      var now = document.querySelector('#lat-targets .lat-card');
      return !!now && now !== oldNode;
    }, 30000);
  }
  function activeKey() {
    var active = document.activeElement;
    return (active && active.dataset && active.dataset.key) || '';
  }
  function latFocusCheck() {
    return openDetail(IDA, CFG.nodeName)
      .then(function () {
        // 前提只按**类名**判：data-key 是要验的东西之一，拿它当等待条件的话，
        // 少了它这条用例会以"超时"结束，而不是以"焦点没回到同一个控件"结束。
        return waitFor('延迟开关行已渲染', function () {
          return !!(node('lat-targets') &&
            document.querySelector('#lat-targets .lat-card') &&
            document.querySelector('#lat-targets .chip'));
        }, 30000);
      })
      .then(function () {
        // ① 目标卡片：Tab 上去之后切档位 —— 整块重建，焦点必须回到同一张卡片。
        var card = document.querySelector('#lat-targets .lat-card');
        card.focus();
        R.notes.focusCardKey = (card.dataset && card.dataset.key) || '';
        R.notes.focusBeforeCard = document.activeElement === card;
        clickRange('lat-ranges', '6h');
        return waitCardsRebuilt(card);
      })
      .then(function () {
        R.notes.focusAfterCard = activeKey();
        R.checks.cardFocusKept =
          R.notes.focusBeforeCard === true && R.notes.focusAfterCard === R.notes.focusCardKey && R.notes.focusAfterCard !== '';
        // ② 开关 chip：同一块 DOM，同一个道理。
        var chip = null;
        Array.prototype.forEach.call(document.querySelectorAll('#lat-targets .chip'), function (c) {
          if (c.textContent === '丢包') chip = c;
        });
        if (!chip) { R.errs.push('没找到「丢包」这枚 chip'); return true; }
        chip.focus();
        R.notes.focusBeforeChip = document.activeElement === chip;
        clickRange('lat-ranges', '1h');
        return waitCardsRebuilt(document.querySelector('#lat-targets .lat-card'));
      })
      .then(function () {
        R.notes.focusAfterChip = activeKey();
        R.checks.chipFocusKept = R.notes.focusBeforeChip === true && R.notes.focusAfterChip === 'chip:loss';
        return true;
      });
  }

  function run() {
    return waitFor('页面脚本就绪', function () { return !!node('view-login') && !!node('form-login'); }, 30000)
      .then(login)
      .then(home)
      .then(chartSyntheticCheck)
      .then(dialogCheck)
      .then(chartSwitchCheck)
      .then(wildTimerCheck)
      .then(waitWildWindow)
      .then(staleChainCheck)
      .then(visibilityCheck)
      .then(latFocusCheck)
      .then(twofaCheck);
  }

  function finish() {
    return rawFetch('/__result', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(R)
    }).then(function () {
      document.title = 'DUMPMARK:' + JSON.stringify({ errs: R.errs.length, fatal: R.fatal });
    });
  }

  window.addEventListener('load', function () {
    run().catch(function (err) {
      R.fatal = String(err && err.message ? err.message : err);
    }).then(finish, finish);
  });
})();`
