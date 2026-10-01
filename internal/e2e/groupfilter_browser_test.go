package e2e

import (
	"encoding/json"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 改动 B 的真浏览器验收：首页顶部那一排分组 chip。
//
// 为什么必须真跑一遍：这一排东西"坏掉"的方式全是静默的 —— chip 照渲染、点击照有，
// 只是分组名单漏了一台机器（没填分组的那些），或者筛完之后上面的「在线 X/Y」
// 变成了本组的数字（那是**另一个**问题：总览条讲的是整个面板），
// 或者刷新之后回到「全部」而用户以为自己的选择还在。这些在静态断言里都看不出来。
//
// 三个场景：
//  1. 默认（1500×1100）：chip 名单/台数/顺序 → 点「香港」只剩香港的卡片 →
//     总览条数字**不变** → 「未分组」也能筛 → 切回「香港」后刷新，选择还在；
//  2. 同一次运行的续跑（刷新之后）：停在「香港」时，设置页的拖动把手是灰的、
//     合成一次拖动也**不发** PUT /nodes/order；切回「全部」再拖，PUT 正常发出；
//  3. 窄屏（380×900）：chip 折行、页面没有横向溢出、点一组之后卡片数正确。
func TestHomeGroupFilterInRealBrowser(t *testing.T) {
	chrome := findChrome()
	if chrome == "" {
		t.Skip("找不到 Chrome：设置 PROBE_CHROME 或把它装到默认位置后这条用例会自动跑起来")
	}

	logs := &captureHandler{}
	h := startServerAt(t, "127.0.0.1:0", filepath.Join(t.TempDir(), "probe.db"), time.UTC, slog.New(logs), nil)
	br := newBrowser(t, "http://"+h.addr)
	setupAdmin(t, br, waitSetupCode(t, logs))

	// 四台机器：香港 2、美国 1、没填分组 1。
	//
	// 顺序**故意**与分组名的字典序不同（香港 → 美国 → 未分组 → 香港）：
	// 分组顺序应当按"卡片在首页的显示顺序里首次出现"排，而不是按名字排。
	// 没填分组那台排在中间，因此「未分组」必须出现在「香港」之后、「美国」之后 ——
	// 按字典序它会在最后，两者分得开。
	hkID := createNodeWithGroup(t, br, "hk-01", "香港")
	createNodeWithGroup(t, br, "us-01", "美国")
	createNodeWithGroup(t, br, "none-01", "")
	createNodeWithGroup(t, br, "hk-02", "香港")

	cfg := tzHarnessConfig{NodeID: hkID, NodeName: "hk-01", Scenario: "filter", User: "admin", Pass: "a-very-good-password"}
	proxy := newHarnessProxy(t, "http://"+h.addr, cfg, groupHarnessJS)
	mock := newMockServer(t, proxy)
	raw := runChromeForResult(t, chrome, mock.URL+"/", proxy.result, 180*time.Second, "1500,1100")

	var res groupResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("解析浏览器回传的结果: %v\n原始内容：%s", err, tail(string(raw), 600))
	}
	if res.Fatal != "" {
		t.Fatalf("浏览器里的自检流程没跑完：%s", res.Fatal)
	}
	if len(res.Errs) != 0 {
		t.Fatalf("浏览器里有 %d 条 JS 报错：%v", len(res.Errs), res.Errs)
	}
	t.Logf("chip 列表 = %v（共 %d 台机器）", res.Chips, res.Total)
	t.Logf("自检脚本走过的步骤 = %v", res.Steps)
	t.Logf("第二遍开始时的状态 = %+v", res.Debug)

	// 1) 名单与台数：全部 + 三个分组，顺序按首次出现（香港 → 美国 → 未分组）。
	wantChips := []struct {
		text  string
		count string
	}{
		{"全部", "4"}, {"香港", "2"}, {"美国", "1"}, {"未分组", "1"},
	}
	if len(res.Chips) != len(wantChips) {
		t.Fatalf("chip 数量 = %d，期望 %d：%v", len(res.Chips), len(wantChips), res.Chips)
	}
	for i, want := range wantChips {
		if !strings.HasPrefix(res.Chips[i].Text, want.text) {
			t.Errorf("第 %d 个 chip = %q，期望以 %q 开头（顺序 = 分组在首页首次出现的顺序）", i+1, res.Chips[i].Text, want.text)
		}
		if !strings.HasSuffix(res.Chips[i].Text, " "+want.count) {
			t.Errorf("第 %d 个 chip = %q，期望带台数 %s（分组后面直接写「香港 2」）", i+1, res.Chips[i].Text, want.count)
		}
		if i > 0 && res.Chips[i].Active {
			t.Errorf("默认应当停在「全部」，第 %d 个 chip（%q）却是高亮的", i+1, res.Chips[i].Text)
		}
		if i > 0 && res.Chips[i].Pressed != "false" {
			t.Errorf("第 %d 个 chip（%q）的 aria-pressed = %q，期望 false", i+1, res.Chips[i].Text, res.Chips[i].Pressed)
		}
	}
	if res.Chips[0].Pressed != "true" {
		t.Errorf("「全部」的 aria-pressed = %q，期望 true（默认选中）", res.Chips[0].Pressed)
	}

	// 2) 点「香港」：只剩香港那两张卡片；总览条**一个数字都不变**。
	if got := strings.Join(res.AfterPickVisible, ","); got != "hk-01,hk-02" {
		t.Errorf("点「香港」后可见的卡片 = %q，期望 hk-01,hk-02", got)
	}
	if !strings.HasPrefix(res.AfterPickChip, "香港") {
		t.Errorf("点完之后高亮的 chip = %q，期望「香港 2」", res.AfterPickChip)
	}
	if res.SummaryBefore != res.SummaryAfter {
		t.Errorf("总览条在筛选前后变了：%q → %q（它讲的是整个面板，不能跟着筛）",
			res.SummaryBefore, res.SummaryAfter)
	}
	if res.SummaryAfter != "0/4" {
		t.Errorf("总览条 = %q，期望 0/4（四台机器一台都没上线）", res.SummaryAfter)
	}
	// 选择存进了 localStorage（键名带版本前缀）。
	if !strings.Contains(res.Stored, "香港") {
		t.Errorf("localStorage 里的选择 = %q，期望存着「香港」", res.Stored)
	}
	t.Logf("点「香港」后：可见卡片 = %v；总览条 %s → %s；localStorage = %s",
		res.AfterPickVisible, res.SummaryBefore, res.SummaryAfter, res.Stored)

	// 3) 「未分组」入口：没填分组的机器不能"消失"。
	if got := strings.Join(res.UngroupedVisible, ","); got != "none-01" {
		t.Errorf("点「未分组」后可见的卡片 = %q，期望 none-01", got)
	}
	// 键盘路径（聚焦 + 激活）：整排重画之后焦点必须还在这一组上。
	if !strings.HasPrefix(res.KeyboardChipFocus, "BUTTON[香港") {
		t.Errorf("用键盘激活一个分组之后焦点 = %q，期望仍停在那一枚 chip 上："+
			"那一排是整块重画的，键盘用户按一次回车就要重新 Tab 一遍", res.KeyboardChipFocus)
	}

	// 4) 刷新之后选择还在（localStorage 读回来）。
	if !strings.HasPrefix(res.AfterReloadChip, "香港") {
		t.Errorf("刷新之后高亮的 chip = %q，期望仍停在「香港」", res.AfterReloadChip)
	}
	if got := strings.Join(res.AfterReloadVisible, ","); got != "hk-01,hk-02" {
		t.Errorf("刷新之后可见的卡片 = %q，期望 hk-01,hk-02（筛选状态跟着选择一起保留）", got)
	}
	t.Logf("刷新之后：高亮 = %q；可见卡片 = %v", res.AfterReloadChip, res.AfterReloadVisible)

	// 5) 筛选时拖动排序被禁用：把手变灰、title 写明原因，而且**一个 PUT 都不发**。
	if !strings.Contains(res.LockedHandle.Cls, "off") {
		t.Errorf("筛选时把手应当带 .off（变灰）：class = %q", res.LockedHandle.Cls)
	}
	if res.LockedHandle.Disabled != "true" {
		t.Errorf("筛选时把手应当 aria-disabled=true，实际 %q", res.LockedHandle.Disabled)
	}
	if !strings.Contains(res.LockedHandle.Title, "全部") {
		t.Errorf("筛选时把手的 title 应当写明「切回「全部」再拖」，实际 %q", res.LockedHandle.Title)
	}
	if res.LockedHandle.Cursor != "not-allowed" {
		t.Errorf("筛选时把手的光标 = %q，期望 not-allowed", res.LockedHandle.Cursor)
	}
	if res.LockedHandle.Opacity == "" || res.LockedHandle.Opacity == "1" {
		t.Errorf("筛选时把手的不透明度 = %q，期望明显变淡（opacity < 1）", res.LockedHandle.Opacity)
	}
	if res.OrderPutsLocked != 0 {
		t.Errorf("筛选状态下合成一次拖动竟然发出了 %d 次 PUT /nodes/order：落点是子集里的位置，写回去会打乱没显示的机器",
			res.OrderPutsLocked)
	}
	t.Logf("筛选时把手：class=%q title=%q cursor=%s opacity=%s；拖动后 PUT /nodes/order 次数 = %d",
		res.LockedHandle.Cls, res.LockedHandle.Title, res.LockedHandle.Cursor, res.LockedHandle.Opacity, res.OrderPutsLocked)

	// 6) 切回「全部」之后拖动照常可用（同一个把手、同一段合成事件）。
	if strings.Contains(res.UnlockedHandle.Cls, "off") {
		t.Errorf("「全部」时把手不该是灰的：class = %q", res.UnlockedHandle.Cls)
	}
	if res.UnlockedHandle.Disabled != "false" {
		t.Errorf("「全部」时把手应当 aria-disabled=false，实际 %q", res.UnlockedHandle.Disabled)
	}
	if res.OrderPutsUnlocked < 1 {
		t.Error("「全部」时拖动一下应当真的发出 PUT /nodes/order（一条都没发：拖动排序被误伤了）")
	}
	t.Logf("「全部」时把手：class=%q aria-disabled=%s；拖动后 PUT /nodes/order 次数 = %d",
		res.UnlockedHandle.Cls, res.UnlockedHandle.Disabled, res.OrderPutsUnlocked)

	// 7) 窄屏：chip 折行、页面不横向溢出、点一组之后卡片数正确。
	narrow := runGroupNarrow(t, chrome, "http://"+h.addr)
	if narrow.OverflowBefore > 0 || narrow.OverflowAfter > 0 {
		t.Errorf("窄屏（视口 %dpx）出现了横向溢出：筛选前 %dpx、筛选后 %dpx",
			narrow.Viewport, narrow.OverflowBefore, narrow.OverflowAfter)
	}
	if narrow.FilterScroll > 0 {
		t.Errorf("窄屏下 chip 那一行自己溢出了 %dpx（应当折行，而不是把容器顶宽）", narrow.FilterScroll)
	}
	if narrow.Chips < 4 {
		t.Errorf("窄屏下只看到 %d 个 chip，期望 4 个（分组多了要折行，不能藏起来）", narrow.Chips)
	}
	if narrow.VisibleCards != 2 {
		t.Errorf("窄屏下点「香港」之后可见卡片 = %d，期望 2", narrow.VisibleCards)
	}
	t.Logf("窄屏（视口 %dpx）：%d 个 chip、筛选后 %d 张卡片；页面横向溢出 %dpx → %dpx；chip 行自身溢出 %dpx",
		narrow.Viewport, narrow.Chips, narrow.VisibleCards,
		narrow.OverflowBefore, narrow.OverflowAfter, narrow.FilterScroll)

	// 8) 回归：四条路由各走一遍，每条都要 **errs=0**、而且真的切到了那个视图。
	//
	// 分组筛选动的是共用的卡片与首页那一块；改坏了详情页/设置页往往一点征兆都没有
	// （报错只落在控制台里）。这里把用户点得最多的四条路由串起来走一遍。
	wantView := map[string]string{
		"#/":                                 "view-home",
		"#/n/" + strconv.FormatInt(hkID, 10): "view-detail",
		"#/settings/nodes":                   "view-settings:nodes",
		"#/settings/alert":                   "view-settings:alert",
	}
	if len(res.Routes) != len(wantView) {
		t.Fatalf("只走了 %d 条路由，期望 %d 条：%+v", len(res.Routes), len(wantView), res.Routes)
	}
	for _, r := range res.Routes {
		t.Logf("路由 %s → 视图 %s（这一段里有 %d 条 JS 报错）", r.Hash, r.View, r.Errs)
		if r.Errs != 0 {
			t.Errorf("路由 %s 上有 %d 条 JS 报错", r.Hash, r.Errs)
		}
		want, ok := wantView[r.Hash]
		if !ok {
			t.Errorf("浏览器走了一条没登记的路由 %s", r.Hash)
			continue
		}
		if r.View != want {
			t.Errorf("路由 %s 最后停在 %q，期望 %q", r.Hash, r.View, want)
		}
	}
}

// runGroupNarrow 再跑一次 Chrome（380×900），只看"窄屏能不能用"。
func runGroupNarrow(t *testing.T, chrome, base string) groupNarrow {
	t.Helper()
	cfg := tzHarnessConfig{Scenario: "narrow", User: "admin", Pass: "a-very-good-password"}
	proxy := newHarnessProxy(t, base, cfg, groupHarnessJS)
	mock := newMockServer(t, proxy)
	raw := runChromeForResult(t, chrome, mock.URL+"/", proxy.result, 120*time.Second, "380,900")

	var res groupResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("解析窄屏场景的结果: %v\n原始内容：%s", err, tail(string(raw), 600))
	}
	if res.Fatal != "" {
		t.Fatalf("窄屏场景没跑完：%s", res.Fatal)
	}
	if len(res.Errs) != 0 {
		t.Fatalf("窄屏场景里有 %d 条 JS 报错：%v", len(res.Errs), res.Errs)
	}
	return res.Narrow
}

// createNodeWithGroup 建一台带分组的节点（group 传空串就是"没填分组"）。
func createNodeWithGroup(t *testing.T, br *browser, name, group string) int64 {
	t.Helper()
	status, body := br.do("POST", "/api/v1/nodes", map[string]any{
		"name": name, "group_name": group, "region": "HK", "interval_sec": 1,
	}, true)
	if status != 201 {
		t.Fatalf("创建节点 %s 失败: %d %v", name, status, body)
	}
	node, _ := body["node"].(map[string]any)
	id, _ := node["id"].(float64)
	if id <= 0 {
		t.Fatalf("创建节点 %s 没有返回 id: %v", name, body)
	}
	return int64(id)
}

// ---------------------------------------------------------------- 观测值

type groupResult struct {
	Errs  []string `json:"errs"`
	Fatal string   `json:"fatal"`
	Steps []string `json:"steps"`

	Chips         []groupChip `json:"chips"`
	Total         int         `json:"total"`
	SummaryBefore string      `json:"summaryBefore"`
	SummaryAfter  string      `json:"summaryAfter"`

	AfterPickChip      string   `json:"afterPickChip"`
	AfterPickVisible   []string `json:"afterPickVisible"`
	UngroupedVisible   []string `json:"ungroupedVisible"`
	Stored             string   `json:"stored"`
	AfterReloadChip    string   `json:"afterReloadChip"`
	AfterReloadVisible []string `json:"afterReloadVisible"`
	KeyboardChipFocus  string   `json:"keyboardChipFocus"`

	LockedHandle      groupHandle `json:"lockedHandle"`
	UnlockedHandle    groupHandle `json:"unlockedHandle"`
	OrderPutsLocked   int         `json:"orderPutsLocked"`
	OrderPutsUnlocked int         `json:"orderPutsUnlocked"`

	Narrow groupNarrow  `json:"narrow"`
	Routes []groupRoute `json:"routes"`
	Debug  any          `json:"debug"`
}

// groupRoute 是一条路由走一遍之后的观测值：这条路由上新增了几条 JS 报错、
// 最后停在哪个视图（"页面没崩，而且真的切过去了"两件事都要看得见）。
type groupRoute struct {
	Hash string `json:"hash"`
	Errs int    `json:"errs"`
	View string `json:"view"`
}

type groupChip struct {
	Text    string `json:"text"`
	Pressed string `json:"pressed"`
	Active  bool   `json:"active"`
}

type groupHandle struct {
	Cls      string `json:"cls"`
	Title    string `json:"title"`
	Disabled string `json:"disabled"`
	Cursor   string `json:"cursor"`
	Opacity  string `json:"opacity"`
}

type groupNarrow struct {
	Viewport       int `json:"viewport"`
	Chips          int `json:"chips"`
	VisibleCards   int `json:"visibleCards"`
	OverflowBefore int `json:"overflowBefore"`
	OverflowAfter  int `json:"overflowAfter"`
	FilterScroll   int `json:"filterScroll"`
}

// groupHarnessJS 是注入到页面里的自检脚本（与其它浏览器用例同一套做法）。
//
// 它按用户的真实操作驱动界面：等首页 → 读 chip → 点分组 → 数还看得见的卡片 →
// 刷新一次验证选择留下来了 → 进设置页**合成**一次拖动（Pointer Events）→
// 切回「全部」再拖一次。断言全在 Go 那边。
//
// 刷新之后脚本会再跑一遍：用 sessionStorage 记一个阶段标记，第二遍接着往下走
// （localStorage 里的选择正好是这一遍要验证的东西）。
const groupHarnessJS = `(function () {
  'use strict';
  var CFG = window.__TZCFG || {};
  var rawFetch = window.fetch.bind(window);
  var R = {
    errs: [], fatal: '', steps: [], chips: [], total: 0,
    summaryBefore: '', summaryAfter: '', afterPickChip: '', afterPickVisible: [],
    ungroupedVisible: [], stored: '', afterReloadChip: '', afterReloadVisible: [],
    keyboardChipFocus: '',
    lockedHandle: {}, unlockedHandle: {}, orderPutsLocked: 0, orderPutsUnlocked: 0,
    narrow: {}
  };
  window.__GROUPRESULT = R;

  // 截图模式：把 EventSource 换成一个不联网的替身（理由见 axis_browser_test.go：
  // 挂着的 SSE 请求会让 --virtual-time-budget 永远耗不完，Chrome 就不截图）。
  if (CFG.shot) {
    window.EventSource = function () {
      var listeners = {};
      this.addEventListener = function (name, fn) { (listeners[name] = listeners[name] || []).push(fn); };
      this.close = function () {};
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

  // ---- 记 PUT /api/v1/nodes/order 发了几次（拖动排序的唯一出口）-------------
  var orderPuts = 0;
  window.fetch = function (input, init) {
    var url = typeof input === 'string' ? input : ((input && input.url) || '');
    var method = ((init && init.method) || 'GET').toUpperCase();
    return rawFetch(input, init).then(function (res) {
      if (method === 'PUT' && url.indexOf('/nodes/order') >= 0) orderPuts++;
      return res;
    });
  };

  // ---- 小工具 -------------------------------------------------------------
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

  function chips() {
    var box = node('group-filter');
    if (!box) return [];
    var out = [];
    Array.prototype.forEach.call(box.querySelectorAll('button'), function (b) {
      out.push({
        text: b.textContent,
        pressed: b.getAttribute('aria-pressed'),
        active: b.classList.contains('active')
      });
    });
    return out;
  }

  function activeChipText() {
    var box = node('group-filter');
    if (!box) return '';
    var btn = box.querySelector('button.active');
    return btn ? btn.textContent : '';
  }

  // 现在**看得见**的卡片名字（hidden 的卡片不占位置，等于被筛掉了）。
  function cardNames() {
    var grid = node('grid');
    var out = [];
    if (!grid) return out;
    Array.prototype.forEach.call(grid.children, function (card) {
      if (card.hidden) return;
      var name = card.querySelector('.card-name');
      out.push(name ? name.textContent : '?');
    });
    return out;
  }

  function clickChip(prefix) {
    var box = node('group-filter');
    if (!box) throw new Error('找不到分组筛选那一排 #group-filter');
    var btns = box.querySelectorAll('button');
    for (var i = 0; i < btns.length; i++) {
      if (btns[i].textContent.indexOf(prefix) === 0) { btns[i].click(); return true; }
    }
    throw new Error('chip 列表里没有以「' + prefix + '」开头的分组');
  }

  // 键盘路径：把焦点放到某一枚 chip 上再激活它（真实键盘按回车走的就是这条路），
  // 返回"激活之后焦点落在哪儿"的描述 —— 那一排是整块重画的，很容易把焦点丢掉。
  function activateChipByKeyboard(prefix) {
    var box = node('group-filter');
    var btns = box.querySelectorAll('button');
    for (var i = 0; i < btns.length; i++) {
      if (btns[i].textContent.indexOf(prefix) !== 0) continue;
      btns[i].focus();
      btns[i].click();
      var active = document.activeElement;
      if (!active) return '（没有 activeElement）';
      var pressed = active.getAttribute ? active.getAttribute('aria-pressed') : null;
      // 只取前 40 个字符：焦点掉到 <body> 上时 textContent 是整个页面。
      return active.tagName + '[' + (active.textContent || '').slice(0, 40) + '] pressed=' + pressed;
    }
    throw new Error('chip 列表里没有以「' + prefix + '」开头的分组');
  }

  function home() {
    return waitFor('首页四张卡片都画出来', function () {
      return shown('view-home') && node('grid') && node('grid').children.length === 4;
    }, 30000)
      .then(function () {
        return waitFor('分组 chip 就绪', function () { return chips().length >= 4; }, 15000);
      })
      .then(function () { return sleep(300); });
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
      })
      .then(function () {
        // 刷新之后 hash 可能还停在 #/settings/nodes：拉回首页再继续。
        if (!shown('view-home')) {
          window.location.hash = '#/';
          return waitFor('回到首页', function () { return shown('view-home'); }, 15000);
        }
        return true;
      });
  }

  function goSettings() {
    window.location.hash = '#/settings/nodes';
    return waitFor('服务器列表就绪', function () {
      var list = node('nodes-list');
      return shown('view-settings') && list && list.children.length === 4;
    }, 30000).then(function () { return sleep(300); });
  }

  function handleInfo() {
    var handle = node('nodes-list').querySelector('.node-drag');
    if (!handle) throw new Error('服务器列表里找不到拖动把手');
    var cs = getComputedStyle(handle);
    return {
      cls: handle.className,
      title: handle.title,
      disabled: handle.getAttribute('aria-disabled'),
      cursor: cs.cursor,
      opacity: cs.opacity
    };
  }

  // synthDrag 合成一次真实的拖动：把最后一行拖到最上面（落点会变 → 顺序会变）。
  // 用 Pointer Events（与页面自己监听的是同一套），pointerId 固定 1。
  function synthDrag() {
    var rows = node('nodes-list').children;
    if (rows.length < 2) throw new Error('服务器列表里没有两行');
    var last = rows[rows.length - 1];
    var handle = last.querySelector('.node-drag');
    var hr = handle.getBoundingClientRect();
    var first = rows[0].getBoundingClientRect();
    var x = hr.left + hr.width / 2;
    var y = hr.top + hr.height / 2;
    var endY = first.top + 1;
    var common = { bubbles: true, cancelable: true, pointerId: 1, pointerType: 'mouse', isPrimary: true, clientX: x };
    handle.dispatchEvent(new PointerEvent('pointerdown', Object.assign({}, common, { button: 0, buttons: 1, clientY: y })));
    document.dispatchEvent(new PointerEvent('pointermove', Object.assign({}, common, { buttons: 1, clientY: endY })));
    document.dispatchEvent(new PointerEvent('pointerup', Object.assign({}, common, { button: 0, buttons: 0, clientY: endY })));
  }

  // restore 把刷新前那一遍的观测值搬回来：重载会新建一份 R，而第一遍量的东西
  // （chip 名单、总览条前后、被筛掉的卡片…）在第二遍里拿不到了。
  // 只搬"第一遍才有的"那些键，第二遍自己会重新赋值的不受影响。
  function restore() {
    var prev = null;
    try { prev = JSON.parse(sessionStorage.getItem('probe-group-result') || 'null'); } catch (e) { prev = null; }
    if (!prev) return;
    ['chips', 'total', 'summaryBefore', 'summaryAfter', 'afterPickChip', 'afterPickVisible',
      'ungroupedVisible', 'stored', 'keyboardChipFocus'].forEach(function (k) {
      if (prev[k] !== undefined) R[k] = prev[k];
    });
    // errs / steps 是两遍各自的记录，接在一起（第二遍的报错同样要报出来）。
    if (prev.errs && prev.errs.length) R.errs = prev.errs.concat(R.errs);
    if (prev.steps && prev.steps.length) R.steps = prev.steps.concat(R.steps);
    if (prev.fatal) R.fatal = prev.fatal;
  }

  // ---- 场景一：名单、台数、筛选、总览条、刷新保留 -------------------------
  function firstPass() {
    R.total = node('grid').children.length;
    R.chips = chips();
    R.summaryBefore = textOf('sum-online') + '/' + textOf('sum-total');
    clickChip('香港');
    return sleep(300).then(function () {
      R.afterPickChip = activeChipText();
      R.afterPickVisible = cardNames();
      R.summaryAfter = textOf('sum-online') + '/' + textOf('sum-total');
      try { R.stored = localStorage.getItem('probe-group-filter-v1') || ''; } catch (e) { R.stored = ''; }
      // 「未分组」那一组也要能筛：没填分组的机器不能在筛选里消失。
      clickChip('未分组');
      return sleep(300);
    }).then(function () {
      R.ungroupedVisible = cardNames();
      // 切回「香港」，刷新一次：验证选择是**存下来**的，而不是只在内存里。
      // 这一次走**键盘路径**（先聚焦再激活）：那一排是整块重画的，
      // 焦点要是丢了，键盘用户按一次回车就得重新 Tab 一遍。
      R.keyboardChipFocus = activateChipByKeyboard('香港');
      return sleep(200);
    }).then(function () {
      sessionStorage.setItem('probe-group-phase', '1');
      // 把这一遍的观测值交给第二遍（重载之后 R 是新的）。
      try { sessionStorage.setItem('probe-group-result', JSON.stringify(R)); } catch (e) { /* 存不下就少几条观测值 */ }
      window.location.reload();
      // 这一遍**不回传结果**：页面马上要重载，回传的会是第一遍的观测值
      // （mock 只收第一条），第二遍就永远等不到了。返回一个永不 resolve 的
      // promise，让下面的 finish 不执行 —— 重载会自己把这条链带走。
      return new Promise(function () {});
    });
  }

  // ---- 场景二：刷新之后接着看（选择保留 + 拖动闸门）-----------------------
  function secondPass() {
    R.debug = {
      phase: sessionStorage.getItem('probe-group-phase') || '',
      stored: localStorage.getItem('probe-group-filter-v1') || '<null>',
      chips: chips(),
      gridChildren: node('grid') ? node('grid').children.length : -1,
      hiddenFlags: (function () {
        var out = [];
        if (!node('grid')) return out;
        Array.prototype.forEach.call(node('grid').children, function (c) { out.push(c.hidden ? 'hidden' : 'shown'); });
        return out;
      })(),
      filterHidden: node('group-filter') ? node('group-filter').hidden : null
    };
    R.afterReloadChip = activeChipText();
    R.afterReloadVisible = cardNames();
    return goSettings().then(function () {
      R.lockedHandle = handleInfo();
      synthDrag();
      return sleep(800);
    }).then(function () {
      R.orderPutsLocked = orderPuts;
      // 切回「全部」再拖一次：这一次必须真的发 PUT。
      window.location.hash = '#/';
      return waitFor('回到首页', function () { return shown('view-home'); }, 15000);
    }).then(function () { return sleep(300); })
      .then(function () { clickChip('全部'); return sleep(200); })
      .then(goSettings)
      .then(function () {
        R.unlockedHandle = handleInfo();
        var before = orderPuts;
        synthDrag();
        return sleep(1000).then(function () { R.orderPutsUnlocked = orderPuts - before; });
      });
  }

  // ---- 场景三：窄屏 -------------------------------------------------------
  function narrowPass() {
    var doc = document.documentElement;
    R.narrow.viewport = doc.clientWidth;
    R.narrow.chips = chips().length;
    R.narrow.overflowBefore = doc.scrollWidth - doc.clientWidth;
    clickChip('香港');
    return sleep(300).then(function () {
      R.narrow.visibleCards = cardNames().length;
      R.narrow.overflowAfter = doc.scrollWidth - doc.clientWidth;
      var box = node('group-filter');
      R.narrow.filterScroll = box.scrollWidth - box.clientWidth;
      return true;
    });
  }

  // ---- 场景四：四条路由各走一遍，记下每一条上有没有 JS 报错 ----------------
  //
  // 分组筛选改的是首页那一块，但它是**共用的卡片与路由**的一部分：改坏了
  // 详情页/设置页往往一点征兆都没有（报错只落在控制台里）。四条路由是用户点得
  // 最多的那几条：首页 / 节点详情 / 服务器列表 / 告警参数。
  function currentView() {
    var views = ['view-home', 'view-detail', 'view-settings', 'view-login', 'view-setup'];
    for (var i = 0; i < views.length; i++) {
      if (shown(views[i])) {
        if (views[i] !== 'view-settings') return views[i];
        var pane = node('settings-panes').querySelector('.pane:not([hidden])');
        return 'view-settings:' + (pane ? pane.dataset.pane : '?');
      }
    }
    return '（没有可见视图）';
  }

  function routePass() {
    var routes = ['#/', '#/n/' + CFG.nodeID, '#/settings/nodes', '#/settings/alert'];
    var out = [];
    return routes.reduce(function (chain, hash) {
      return chain.then(function () {
        R.routeErrsBefore = R.errs.length;
        window.location.hash = hash;
        return sleep(1000);
      }).then(function () {
        out.push({ hash: hash, errs: R.errs.length - R.routeErrsBefore, view: currentView() });
        return true;
      });
    }, Promise.resolve()).then(function () { R.routes = out; return true; });
  }

  function run() {
    var scenario = CFG.scenario || 'filter';
    restore();
    return waitFor('页面脚本就绪', function () { return !!node('view-login'); }, 30000)
      .then(login)
      .then(home)
      .then(function () {
        if (scenario === 'narrow') return narrowPass();
        var phase = '';
        try { phase = sessionStorage.getItem('probe-group-phase') || ''; } catch (e) { phase = ''; }
        return phase === '1' ? secondPass() : firstPass();
      })
      .then(function () { return routePass(); });
  }

  // shot 模式（人工截图用）：把首页开到指定状态就停住，不回传结果。
  //   group-all → 默认（全部）；group-hk → 选中「香港」；group-none → 选中「未分组」。
  function shot() {
    var mode = CFG.shot;
    return login().then(home).then(function () {
      if (mode === 'group-hk') clickChip('香港');
      else if (mode === 'group-none') clickChip('未分组');
      return sleep(400);
    }).then(function () {
      document.title = 'SHOT-READY:' + mode;
      return true;
    });
  }

  function finish() {
    return rawFetch('/__result', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(R)
    }).then(function () { document.title = 'DUMPMARK:group'; });
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
