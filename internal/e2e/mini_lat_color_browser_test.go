package e2e

import (
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"probe/internal/store"
)

// 首页迷你条的延迟格子配色：**绝对阈值与本机倍数取严**（用户定的"两者取严"）。
//
// 为什么必须在真浏览器里量：分级回答的是"哪一段该被注意到"，它坏掉的方式全是静默的
// —— 格子照画、颜色照有，只是颜色不再与规则一致；而基准（行首那个数）一旦与格子用的
// 不是同一个，同一张卡片上就会自相矛盾（数字写着 205ms，格子却按别的基准判色）。
// 静态断言只能钉住代码形状（见 TestFrontendMiniLatencyGradesByOwnAverage），
// "画出来到底是黄还是绿"只能读 DOM。
//
// 两个节点、两份数据（都喂在 /overview 默认的 1h ÷ 10 段 = 每段 6 分钟上）：
//
//	slow-205（基准 ≈205ms）：197.9ms 必须**黄**（绝对命中 >180 —— 这正是用户报的
//	            那个例子：只看相对倍数时它是绿的）、250ms 必须**红**（绝对 >240）、
//	            150ms 必须**绿**（两个口径都不命中）；
//	fast-34（基准 ≈34ms）：60ms / 45ms 必须**黄**（相对命中：>1.2×，且都 ≤2×）、
//	            22ms 必须**绿**。
//
// 关于 fast-34 的基准为什么是 ~34ms 而不是用户举例的 20ms：按最终规则
// （bad = value > 240 **或** value > 2× 均值），均值 20ms 时 45ms 与 60ms 都会命中
// **红**档（> 2×20 = 40），与"期望是黄"矛盾。要让"22 绿 / 45 黄 / 60 黄"同时成立，
// 基准必须落在 [30, 37.5)：45 > 1.2a（黄）且 60 ≤ 2a（不红）。规则是最终裁决，
// 数据按规则反推 —— 这条用例证明的是"相对口径没丢"（两个值都远低于绝对阈值 180，
// 却都被标了黄），而不是那个具体基准值。
func TestMiniLatencyColorsInRealBrowser(t *testing.T) {
	chrome := findChrome()
	if chrome == "" {
		t.Skip("找不到 Chrome：设置 PROBE_CHROME 或把它装到默认位置后这条用例会自动跑起来")
	}

	logs := &captureHandler{}
	h := startServerAt(t, "127.0.0.1:0", filepath.Join(t.TempDir(), "probe.db"), time.UTC, slog.New(logs), nil)
	br := newBrowser(t, "http://"+h.addr)
	setupAdmin(t, br, waitSetupCode(t, logs))

	// 探测目标是**全局配置**（不是每节点一份）：只配一次，两个节点各喂自己那份数据。
	// 这里必须先把两个节点建完再配目标、再喂数据：设置接口按 (type,host,port) 匹配，
	// 重复配置可能换掉目标 id，而 ping_samples_1m 里的行是按 target_id 落的 ——
	// id 换了，数据就成了"已删除目标的历史"，卡片上那一行会显示 —（而不是报错）。
	slowNode, _ := createNodeViaAPI(t, br, "slow-205")
	fastNode, _ := createNodeViaAPI(t, br, "fast-34")
	targetID := createPingTarget(t, br, "CF", "1.1.1.1", 443, 60)

	// 一台"常年两百毫秒"的机器：绝对口径就是为它准备的（相对它自己一切正常）。
	seedOverviewBuckets(t, h, slowNode, targetID,
		[]float64{207.4, 207.4, 207.4, 197.9, 250, 150, 207.4, 207.4, 207.4, 207.4})

	// 一台"平常二十几毫秒"的机器：相对口径为它准备（60ms 在绝对分档里仍是绿）。
	seedOverviewBuckets(t, h, fastNode, targetID,
		[]float64{30, 30, 30, 30, 22, 45, 60, 30, 30, 30})
	t.Logf("探测目标 id = %d（节点 %d 与 %d 共用）", targetID, slowNode, fastNode)

	cfg := tzHarnessConfig{Scenario: "mini", User: "admin", Pass: "a-very-good-password"}
	proxy := newHarnessProxy(t, "http://"+h.addr, cfg, miniHarnessJS)
	mock := newMockServer(t, proxy)
	raw := runChromeForResult(t, chrome, mock.URL+"/", proxy.result, 120*time.Second, "1500,1100")

	var res miniResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("解析浏览器回传的结果: %v\n原始内容：%s", err, tail(string(raw), 600))
	}
	if res.Fatal != "" {
		t.Fatalf("浏览器里的自检流程没跑完：%s", res.Fatal)
	}
	if len(res.Errs) != 0 {
		t.Fatalf("浏览器里有 %d 条 JS 报错：%v", len(res.Errs), res.Errs)
	}
	t.Logf("自检脚本走过的步骤 = %v", res.Steps)
	for _, c := range res.Cards {
		t.Logf("卡片 %s：迷你条行首 %q；每格（值 → 颜色）= %v", c.Name, c.Lat.Head, c.Lat.Pairs())
	}

	slow := res.card(t, "slow-205")
	fast := res.card(t, "fast-34")

	// ---- 1) 绝对口径：一台均值 205ms 的机器上，197.9ms 不能是绿的 ----
	//
	// 这是用户报的那个例子：只看相对倍数（197.9 ≤ 205）时它是绿的，
	// 而按 Komari 的绝对分档 180 < 197.9 ≤ 240 应当是琥珀色。
	slowCase := func(v float64) string { return res.cellGrade(t, slow, v) }
	if got := slowCase(197.9); got != "warn" {
		t.Errorf("均值 %s 的机器上，197.9ms 的格子是 %q，期望 warn：绝对口径（>180）必须把它标黄 —— "+
			"这正是「只看相对倍数时它显示绿色」那个问题的反面", slow.Lat.Head, got)
	}
	if got := slowCase(250); got != "bad" {
		t.Errorf("均值 %s 的机器上，250ms 的格子是 %q，期望 bad（绝对 >240）", slow.Lat.Head, got)
	}
	if got := slowCase(150); got != "ok" {
		t.Errorf("均值 %s 的机器上，150ms 的格子是 %q，期望 ok（150 ≤ 180 且 ≤ 1.2× 均值）", slow.Lat.Head, got)
	}

	// ---- 2) 相对口径：绝对阈值之下的抖动（22~60ms）也要被标出来 ----
	fastCase := func(v float64) string { return res.cellGrade(t, fast, v) }
	if got := fastCase(60); got != "warn" {
		t.Errorf("均值 %s 的机器上，60ms 的格子是 %q，期望 warn：60ms 远低于绝对阈值 180，"+
			"但它超过本机均值的 1.2 倍 —— 相对口径没丢", fast.Lat.Head, got)
	}
	if got := fastCase(45); got != "warn" {
		t.Errorf("均值 %s 的机器上，45ms 的格子是 %q，期望 warn（同上：相对命中，"+
			"而且没有到 2× 均值的红档）", fast.Lat.Head, got)
	}
	if got := fastCase(22); got != "ok" {
		t.Errorf("均值 %s 的机器上，22ms 的格子是 %q，期望 ok（两个口径都不命中）", fast.Lat.Head, got)
	}

	// ---- 3) 「探测」那一行与迷你条格子**同一套判据** ----
	//
	// 两行颜色互相矛盾比"哪一档更准"严重得多：用户没法判断该信哪一个。
	// 期望值由 Go 按同一条规则算（数据来自 /overview 的原始数字），
	// 与实际画在 DOM 上的类比对 —— 前端若偷偷换了一套口径，这里就对不上。
	src, ok := res.Src.Nodes[strconv.FormatInt(slowNode, 10)]
	if !ok || len(src.Targets) == 0 {
		t.Fatalf("没有从 /overview 拿到节点 %d 的探测数据：%+v", slowNode, res.Src.Nodes)
	}
	if len(slow.Probes) == 0 {
		t.Fatalf("卡片 %s 的「探测」那一行没有读数：%+v", slow.Name, slow)
	}
	wantProbe := miniGradeOf(src.Targets[0].LatMS, src.Targets[0].AvgMS)
	if got := gradeOf(slow.Probes[0].Cls); got != wantProbe {
		t.Errorf("「探测」那一行的颜色是 %q，按同一套规则算出来应当是 %q（当前值 %.1fms / 均值 %.1fms）："+
			"两行必须共用同一个判据", got, wantProbe, src.Targets[0].LatMS, src.Targets[0].AvgMS)
	}
	t.Logf("「探测」那一行：%q → %q（Go 侧按同一规则算出的期望是 %q）",
		slow.Probes[0].Text, gradeOf(slow.Probes[0].Cls), wantProbe)

	// 回归：丢包格子还在（这次只动延迟那一套阈值）。
	if len(slow.Loss.Cells) != len(slow.Lat.Cells) {
		t.Errorf("丢包那一行的格子数 (%d) 与延迟行 (%d) 不一致：这次改动不该碰它",
			len(slow.Loss.Cells), len(slow.Lat.Cells))
	}
}

// seedOverviewBuckets 按总览的段宽（默认 1h ÷ 10 段 = 360 秒）喂一个节点的探测数据。
//
// 每段只写一行、放在该段**正中**：窗口是"对齐到段宽"的（见 overview.go 的
// end -= end % bucketSec），写正中既不会跨段，也不怕测试跑着跑着窗口往前滑一格。
func seedOverviewBuckets(t *testing.T, h *harness, nodeID, targetID int64, values []float64) {
	t.Helper()
	const bucketSec = 360
	now := time.Now().Unix()
	start := now - now%bucketSec - int64(len(values))*bucketSec
	buckets := make([]store.PingBucket, 0, len(values))
	for i, v := range values {
		ts := start + int64(i)*bucketSec + bucketSec/2
		// Min/Avg/Max 都写同一个值：这一段的读数就是它，不受"取哪个口径"影响。
		buckets = append(buckets, store.PingBucket{
			NodeID: nodeID, TargetID: targetID, TS: ts,
			AvgMS: v, MinMS: v, MaxMS: v,
			Up: 100, All: 100,
		})
	}
	if err := h.db.UpsertPingBuckets(context.Background(), buckets); err != nil {
		t.Fatalf("写入探测桶（节点 %d）: %v", nodeID, err)
	}
	t.Logf("播撒探测桶 %d 个（节点 %d，段宽 %d 秒，值 %v）", len(buckets), nodeID, bucketSec, values)
}

// miniGradeOf 是前端 latGrade() 在 Go 侧的**复刻**，只用来算期望值。
//
// 判据本身在前端（app.js 的 latGrade）：这里复刻一份是为了让"两行同源"这条断言
// 有独立的期望值可对 —— 若哪天前端改了口径，这条断言会先红。
// 改规则时两边都要动，但**规则的定义处**始终是 app.js（注释里写明了出处）。
func miniGradeOf(value, avg float64) string {
	if !(avg > 0) {
		return ""
	}
	if value > 240 || value > avg*2 {
		return "bad"
	}
	if value > 180 || value > avg*1.2 {
		return "warn"
	}
	return "ok"
}

// gradeOf 从 DOM 上的 className 里取出颜色档（"" = 浅灰/没有数据）。
//
// 先判 bad 再判 warn：className 里出现 "bad" 时不能再因为别的原因判成 ok
// （这里用 Contains 而不是等值比较，因为类名是 "mini-cell warn" / "line-num ok" 这种组合）。
func gradeOf(cls string) string {
	for _, g := range []string{"bad", "warn", "ok"} {
		if strings.Contains(cls, g) {
			return g
		}
	}
	return ""
}

// parseMS 解析浮层里的读数文本（"197.9 ms" → 197.9）。
func parseMS(text string) (float64, bool) {
	text = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(text), "ms"))
	v, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// ---------------------------------------------------------------- 观测值

type miniResult struct {
	Errs  []string   `json:"errs"`
	Fatal string     `json:"fatal"`
	Steps []string   `json:"steps"`
	Cards []miniCard `json:"cards"`
	Src   miniSrc    `json:"src"`
}

// card 按名字取一张卡片（找不到就直接失败：说明首页根本没渲染出来）。
func (r miniResult) card(t *testing.T, name string) miniCard {
	t.Helper()
	for _, c := range r.Cards {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("首页上没有找到卡片 %q（读到的是 %v）", name, r.cardNames())
	return miniCard{}
}

func (r miniResult) cardNames() []string {
	out := make([]string, 0, len(r.Cards))
	for _, c := range r.Cards {
		out = append(out, c.Name)
	}
	return out
}

// cellGrade 在"值 → 颜色"里按数值找一个格子（值来自浮层文本，如 "197.9 ms"）。
func (r miniResult) cellGrade(t *testing.T, card miniCard, want float64) string {
	t.Helper()
	for _, c := range card.Lat.Cells {
		v, ok := parseMS(c.Value)
		if !ok {
			continue
		}
		if math.Abs(v-want) < 0.05 {
			return gradeOf(c.Cls)
		}
	}
	t.Fatalf("卡片 %s 的迷你条里没有值为 %.1f ms 的格子（读到的是 %v）", card.Name, want, card.Lat.Cells)
	return ""
}

type miniCard struct {
	Name   string    `json:"name"`
	Lat    miniRow   `json:"lat"`
	Loss   miniRow   `json:"loss"`
	Probes []miniNum `json:"probes"`
}

type miniRow struct {
	// Head 是行首那个数（延迟行就是 /overview 的 lat_ms）。
	Head  string     `json:"head"`
	Cells []miniCell `json:"cells"`
}

// Pairs 把这一行读成 "值→颜色" 的列表（日志用）。
func (r miniRow) Pairs() []string {
	out := make([]string, 0, len(r.Cells))
	for _, c := range r.Cells {
		cls := gradeOf(c.Cls)
		if cls == "" {
			cls = "（无）"
		}
		out = append(out, c.Value+"→"+cls)
	}
	return out
}

type miniCell struct {
	Value string `json:"value"`
	Cls   string `json:"cls"`
}

type miniNum struct {
	Text string `json:"text"`
	Cls  string `json:"cls"`
}

// miniSrc 是 /overview 里我们关心的那一部分（算「探测」行的期望颜色用）。
type miniSrc struct {
	Nodes map[string]struct {
		LatMS   float64 `json:"lat_ms"`
		Targets []struct {
			LatMS float64 `json:"lat_ms"`
			AvgMS float64 `json:"avg_ms"`
		} `json:"targets"`
	} `json:"nodes"`
}

// miniHarnessJS 是迷你条配色那条用例的自检脚本。
//
// 它只做两件事：**把每一格的值与颜色读出来**（格子的值只能靠合成 mouseover 打开
// 页面自己的浮层拿到 —— DOM 上没有 title），以及把 /overview 的原始数字带回来
// （Go 侧要按同一条规则算「探测」行的期望颜色）。一句断言都不在这里做。
const miniHarnessJS = `(function () {
  'use strict';
  var CFG = window.__TZCFG || {};
  var rawFetch = window.fetch.bind(window);
  var R = { errs: [], fatal: '', steps: [], cards: [], src: {} };
  window.__MINIRESULT = R;

  window.addEventListener('error', function (e) { R.errs.push('error: ' + (e.message || e.type)); });
  window.addEventListener('unhandledrejection', function (e) {
    var r = e.reason;
    R.errs.push('rejection: ' + (r && r.message ? r.message : String(r)));
  });

  function node(id) { return document.getElementById(id); }
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

  // readRow 读一行迷你条：行首那个数 + 每一格的（值, className）。
  //
  // 格子的值在 DOM 上没有：它只在悬停浮层里（.mini-tip-value）。绑的是
  // mouseover（见 app.js 的 bindMiniCell），所以合成一个 mouseover 就能走到
  // 与真人一样的那条路径 —— 不是绕过页面自己算一遍。
  function readRow(card, index) {
    var rows = card.querySelectorAll('.card-mini .mini-row');
    if (rows.length <= index) return { head: '', cells: [] };
    var row = rows[index];
    var head = row.querySelector('.mini-value');
    var out = { head: head ? head.textContent : '', cells: [] };
    var cells = row.querySelectorAll('.mini-cell');
    for (var i = 0; i < cells.length; i++) {
      cells[i].dispatchEvent(new MouseEvent('mouseover', { bubbles: true }));
      var tip = document.querySelector('.mini-tip-value');
      var text = tip ? tip.textContent : '';
      cells[i].dispatchEvent(new MouseEvent('mouseout', { bubbles: true }));
      out.cells.push({ value: text, cls: cells[i].className });
    }
    return out;
  }

  // readCard 读一张卡片：名字、延迟行、丢包行、「探测」那一行的读数与颜色。
  function readCard(card) {
    var name = card.querySelector('.card-name');
    var probes = [];
    var lines = card.querySelectorAll('.line');
    for (var i = 0; i < lines.length; i++) {
      var label = lines[i].querySelector('.line-label');
      if (!label || label.textContent !== '探测') continue;
      var nums = lines[i].querySelectorAll('.line-num');
      for (var j = 0; j < nums.length; j++) {
        probes.push({ text: nums[j].textContent, cls: nums[j].className });
      }
    }
    return {
      name: name ? name.textContent : '?',
      lat: readRow(card, 0),
      loss: readRow(card, 1),
      probes: probes
    };
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

  function run() {
    return waitFor('页面脚本就绪', function () { return !!node('view-login'); }, 30000)
      .then(login)
      // 卡片由 SSE 建、迷你条由 /overview 填：等到"卡片有格子、且格子有颜色"再读。
      .then(function () {
        return waitFor('首页迷你条就绪', function () {
          var grid = node('grid');
          if (!grid || grid.children.length < 2) return false;
          var cells = grid.querySelectorAll('.card-mini .mini-cell');
          if (cells.length < 20) return false;
          for (var i = 0; i < cells.length; i++) {
            if (cells[i].className !== 'mini-cell') return true;
          }
          return false;
        }, 30000);
      })
      .then(function () { return sleep(600); })
      .then(function () {
        var grid = node('grid');
        Array.prototype.forEach.call(grid.children, function (card) {
          if (card.hidden) return;
          R.cards.push(readCard(card));
        });
        R.steps.push('读完 ' + R.cards.length + ' 张卡片的迷你条');
        return getJSON('/api/v1/overview');
      })
      .then(function (data) { R.src = data; return true; });
  }

  function finish() {
    return rawFetch('/__result', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(R)
    }).then(function () { document.title = 'DUMPMARK:mini'; });
  }

  window.addEventListener('load', function () {
    run().catch(function (err) {
      R.fatal = String(err && err.message ? err.message : err);
    }).then(finish, finish);
  });
})();`
