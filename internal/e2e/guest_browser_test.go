package e2e

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"probe/internal/protocol"
)

// 「允许访客查看」（访客只读模式）的真浏览器验收。
//
// 为什么必须真跑一遍：这个功能做错了在页面上**看不出来** ——
// 卡片照渲染、曲线照画，只是访客能看到他不该看到的东西、或者能点到不该点的按钮。
// 而"能不能看到"这件事只有真浏览器说得清：DOM 里到底有没有那个元素、
// 那行文案有没有出现、直接 fetch 一个写接口会拿到什么。做法与仓库里其它浏览器
// 用例完全一致：真服务端 + 反代注入自检脚本 + --headless=new + 结果 POST 回 mock。
// **不用** --dump-dom --virtual-time-budget（页面挂着 SSE 长连接）。
//
// 两个场景各跑一次 Chrome：
//  1. TestGuestReadOnlyInRealBrowser：开关**打开**时，未登录的浏览器能看首页与
//     详情页（价格可见），但 DOM 里**不存在**「新增节点/设置/退出」，
//     也**不存在**本机地址/来源 IP 那两行；直接 fetch 写接口拿到 401；
//     随后在同一页面里登录，确认一切照旧（三个入口回来、IP 行回来、能进设置页），
//     并把四条常用路由各走一遍记录 JS 报错。
//  2. TestGuestSwitchOffKeepsLoginPage：开关**关**着时（默认），未登录只看到登录页
//     —— 与这个功能加进来之前完全一样。

// guestPrivateValues 是服务端无论如何都不该下发给访客的**值**（见下面 Seed 的部分）。
// 断言"页面文本里不出现这些串"是最贴近用户感受的一条：它不关心实现怎么脱敏，
// 只关心"别人打开页面能不能看见这台机器的地址"。
var guestPrivateValues = []string{"203.0.113.9", "10.0.0.5", "fd00::5"}

// seedGuestNodeState 往内存状态里塞一份"带三个地址字段"的现场
// （真实部署里它们由 Agent 的 hello 带上来）。
//
// 为什么必须塞：不塞的话管理员页面上也不会出现这些地址，
// 而"访客页面上没有它们"这条断言在任何实现下都成立 —— 那就成了一条空断言。
func seedGuestNodeState(t *testing.T, h *harness, nodeID int64) {
	t.Helper()
	h.srv.State().Attach(nodeID, 1, protocol.Info{
		OS:           protocol.OSInfo{Name: "Debian", Kernel: "6.1.0"},
		CPU:          protocol.CPUInfo{Model: "EPYC", Cores: 2},
		AgentVersion: "1.0.42",
	}, guestPrivateValues[0], guestPrivateValues[1], guestPrivateValues[2], time.Now())
	h.srv.State().Update(nodeID, 1, protocol.Metrics{
		CPUPct: 12.5, Mem: protocol.Mem{Total: 1 << 30, Used: 1 << 29, Pct: 50},
		Net:       protocol.Net{Iface: "eth0", BootID: "boot-abc", RxRate: 1024, TxRate: 2048},
		UptimeSec: 3600,
	}, 0, time.Now())
}

type guestResult struct {
	Errs  []string `json:"errs"`
	Fatal string   `json:"fatal"`
	Steps []string `json:"steps"`

	// 访客（未登录）这一遍。
	GuestBarShown  bool     `json:"guestBarShown"`
	GuestBarText   string   `json:"guestBarText"`
	LoginEntry     bool     `json:"loginEntry"`
	AdminEntries   []string `json:"adminEntries"`
	HomeCards      int      `json:"homeCards"`
	HomeCardTitles []string `json:"homeCardTitles"`
	HomeCardText   string   `json:"homeCardText"`
	HomeLeaks      []string `json:"homeLeaks"`
	HomeLoginView  bool     `json:"homeLoginView"`

	DetailOpened  bool     `json:"detailOpened"`
	DetailEntries []string `json:"detailEntries"`
	DetailNetwork []string `json:"detailNetwork"`
	// SettingsRedirect 是"访客把地址改成 #/settings/nodes 之后停在哪"：
	// 期望是首页（设置接口在服务端就是 401，把用户丢在只会报错的地址上没有意义）。
	SettingsRedirect string   `json:"settingsRedirect"`
	DetailStats      []string `json:"detailStats"`
	DetailText       string   `json:"detailText"`
	DetailLeaks      []string `json:"detailLeaks"`
	DetailHasChart   bool     `json:"detailHasChart"`

	WriteStatus  int `json:"writeStatus"`
	WriteStatus2 int `json:"writeStatus2"`

	// 登录之后那一遍。
	AfterLoginEntries  []string `json:"afterLoginEntries"`
	AfterLoginDetail   []string `json:"afterLoginDetail"`
	AfterLoginNetwork  []string `json:"afterLoginNetwork"`
	AfterLoginStats    []string `json:"afterLoginStats"`
	AfterLoginLeaks    []string `json:"afterLoginLeaks"`
	AfterLoginLoginBar bool     `json:"afterLoginLoginBar"`
	SettingsOpened     bool     `json:"settingsOpened"`
	SettingsNodes      int      `json:"settingsNodes"`
	GuestSwitchChecked bool     `json:"guestSwitchChecked"`

	Routes []guestRouteResult `json:"routes"`
	Debug  any                `json:"debug"`
}

// guestRouteResult 是一条路由走一遍之后的观测值（与仓库里其它用例同构）。
type guestRouteResult struct {
	Hash string `json:"hash"`
	Errs int    `json:"errs"`
	View string `json:"view"`
}

// TestGuestReadOnlyInRealBrowser 是主验收。
func TestGuestReadOnlyInRealBrowser(t *testing.T) {
	chrome := findChrome()
	if chrome == "" {
		t.Skip("找不到 Chrome：设置 PROBE_CHROME 或把它装到默认位置后这条用例会自动跑起来")
	}

	logs := &captureHandler{}
	h := startServerAt(t, "127.0.0.1:0", filepath.Join(t.TempDir(), "probe.db"), time.UTC, slog.New(logs), nil)

	br := newBrowser(t, "http://"+h.addr)
	setupAdmin(t, br, waitSetupCode(t, logs))

	// 一台"什么都有"的机器：价格 / 到期日 / 三个地址字段。
	// 地址是**内存状态**里的（真实部署里由 Agent 的 hello 带上来）：
	// 这样管理员页面上会真的出现这三个串，而访客页面上一个都不许有 ——
	// 断言"页面上不出现这些值"比断言"响应里没有某个键"更接近用户感受。
	nodeID, _ := createNodeViaAPI(t, br, "guest-01")
	status, body := br.do(http.MethodPatch, "/api/v1/nodes/"+strconv.FormatInt(nodeID, 10), map[string]any{
		"name": "guest-01", "group_name": "香港", "region": "HK", "interval_sec": 1,
		"note":        "备注里可以写任何东西：root@203.0.113.7:22",
		"price_cents": 7121, "currency": "CNY", "billing_months": 12,
		"expires_at": time.Now().Add(30 * 24 * time.Hour).Unix(),
		"reset_day":  1, "traffic_warn_pct": 80,
	}, true)
	if status != http.StatusOK {
		t.Fatalf("改节点失败: %d %v", status, body)
	}
	seedGuestNodeState(t, h, nodeID)

	// 打开「允许访客查看」（走真实接口：这也是它该被使用的方式）。
	status, body = br.do(http.MethodPut, "/api/v1/settings/guest", map[string]any{"enabled": true}, true)
	if status != http.StatusOK {
		t.Fatalf("打开访客开关失败: %d %v", status, body)
	}

	cfg := tzHarnessConfig{
		NodeID: nodeID, NodeName: "guest-01",
		User: "admin", Pass: "a-very-good-password",
		Scenario: "guest",
		// 私有值随配置一起注入脚本：它不该在页面上出现，脚本要拿它去搜。
		Date: strings.Join(guestPrivateValues, ","),
	}
	proxy := newHarnessProxy(t, "http://"+h.addr, cfg, guestHarnessJS)
	mock := newMockServer(t, proxy)
	raw := runChromeForResult(t, chrome, mock.URL+"/", proxy.result, 240*time.Second, "1500,1100")

	var res guestResult
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

	// ---- 访客看首页 ----
	if !res.GuestBarShown {
		t.Error("访客页面上应当有一条「只读」提示（否则看不出这是只读的）")
	}
	if !strings.Contains(res.GuestBarText, "只读") {
		t.Errorf("只读提示的文案 = %q，应当写着「只读」", res.GuestBarText)
	}
	if !res.LoginEntry {
		t.Error("访客页面上必须保留登录入口（不然管理员自己也没法登录）")
	}
	if len(res.AdminEntries) != 0 {
		t.Errorf("访客的 DOM 里不该有这些管理员入口：%v（要求是**根本不画**，不是 display:none）",
			res.AdminEntries)
	}
	if res.HomeCards < 1 {
		t.Fatalf("访客应当能看到节点卡片，实际 %d 张", res.HomeCards)
	}
	if len(res.HomeLeaks) != 0 {
		t.Errorf("访客首页上出现了私有值 %v（卡片文本：%q，卡片标题：%v）\n出现在这些元素里：%v",
			res.HomeLeaks, res.HomeCardText, res.HomeCardTitles, res.Debug)
	}
	// 价格是**公开**的（用户明确要求访客能看到）：没看到价格反而是错的。
	if !strings.Contains(res.HomeCardText, "71.21") {
		t.Errorf("访客卡片上应当能看到价格（¥71.21），实际：%q", res.HomeCardText)
	}
	t.Logf("访客首页卡片 = %q；卡片 title = %v", res.HomeCardText, res.HomeCardTitles)

	// ---- 访客看详情页 ----
	if !res.DetailOpened {
		t.Fatal("访客应当能打开节点详情页")
	}
	if !res.DetailHasChart {
		t.Error("访客的详情页应当照常画出图表（只是少了两个地址字段）")
	}
	if len(res.DetailEntries) != 0 {
		t.Errorf("访客的详情页头部不该有这些入口：%v（编辑/换 Token/删除都会 401，留着只会让人点了报错）",
			res.DetailEntries)
	}
	if len(res.DetailLeaks) != 0 {
		t.Errorf("访客详情页上出现了私有值 %v（页面文本：%q）", res.DetailLeaks, res.DetailText)
	}
	for _, label := range res.DetailNetwork {
		if label == "本机地址" || label == "来源 IP" {
			t.Errorf("访客详情页的「网络信息」卡里不该有 %q 这一行（整行不画，不是显示 —）：%v",
				label, res.DetailNetwork)
		}
	}
	// 汇总排四格**都在**（价格公开），而且显示的是人民币金额。
	if got := strings.Join(res.DetailStats, "|"); !strings.Contains(got, "71.21") {
		t.Errorf("访客详情页顶部应当能看到价格 71.21，实际四格 = %v", res.DetailStats)
	}
	t.Logf("访客详情页：网络卡标签 = %v；汇总四格 = %v", res.DetailNetwork, res.DetailStats)

	// ---- 访客直接打写接口：401 ----
	if res.WriteStatus != http.StatusUnauthorized {
		t.Errorf("访客 POST /api/v1/nodes 必须 401，实际 %d", res.WriteStatus)
	}
	if res.WriteStatus2 != http.StatusUnauthorized {
		t.Errorf("访客 PUT /api/v1/settings/guest 必须 401（否则他能自己把开关关掉），实际 %d", res.WriteStatus2)
	}
	// 访客手改地址进设置页：停在首页，而不是一个只会报错的空页面。
	if res.SettingsRedirect != "view-home" {
		t.Errorf("访客访问 #/settings/nodes 之后应当停在首页，实际停在 %q", res.SettingsRedirect)
	}

	// ---- 登录之后：一切照旧 ----
	if got := strings.Join(res.AfterLoginEntries, ","); got != "btn-add,btn-settings,btn-logout" {
		t.Errorf("登录后顶栏三个入口应当回到文档里，实际 %v", res.AfterLoginEntries)
	}
	if got := strings.Join(res.AfterLoginDetail, ","); got != "detail-edit,detail-token,detail-delete" {
		t.Errorf("登录后详情页头部的三个入口应当回来，实际 %v", res.AfterLoginDetail)
	}
	if res.AfterLoginLoginBar {
		t.Error("登录后不该再显示「只读」提示条")
	}
	var hasLocal, hasObserved bool
	for _, label := range res.AfterLoginNetwork {
		if label == "本机地址" {
			hasLocal = true
		}
		if label == "来源 IP" {
			hasObserved = true
		}
	}
	if !hasLocal || !hasObserved {
		t.Errorf("管理员详情页的「网络信息」卡里必须有「本机地址」与「来源 IP」两行，实际 %v",
			res.AfterLoginNetwork)
	}
	if len(res.AfterLoginLeaks) == 0 {
		t.Errorf("管理员页面上应当能看到这些地址 %v（脱敏只该对访客生效；看不到说明误伤了管理员）",
			guestPrivateValues)
	}
	if !strings.Contains(strings.Join(res.AfterLoginStats, "|"), "71.21") {
		t.Errorf("管理员详情页顶部应当能看到价格，实际 = %v", res.AfterLoginStats)
	}
	if !res.SettingsOpened || res.SettingsNodes != 1 {
		t.Errorf("登录后应当能进设置页并看到 1 台机器的服务器列表（opened=%v rows=%d）",
			res.SettingsOpened, res.SettingsNodes)
	}
	if !res.GuestSwitchChecked {
		t.Error("设置页「访客访问」那一栏的开关应当勾着（服务端此刻是开着的）")
	}

	// ---- 四条常用路由：登录态下不许有 JS 报错 ----
	if len(res.Routes) != 4 {
		t.Fatalf("应当走完 4 条路由，实际 %d：%+v", len(res.Routes), res.Routes)
	}
	for _, r := range res.Routes {
		if r.Errs != 0 {
			t.Errorf("%s 这条路由上有 %d 条 JS 报错（停在 %s）", r.Hash, r.Errs, r.View)
		}
	}
	t.Logf("四条路由 = %+v", res.Routes)
}

// TestGuestSwitchOffKeepsLoginPage 开关关着（默认）时，未登录只看到登录页。
//
// 这条钉的是"关着的时候行为与以前**完全一样**"：改动没有把登录页挤掉，
// 也没有让面板在默认状态下多露出任何东西。
func TestGuestSwitchOffKeepsLoginPage(t *testing.T) {
	chrome := findChrome()
	if chrome == "" {
		t.Skip("找不到 Chrome：设置 PROBE_CHROME 或把它装到默认位置后这条用例会自动跑起来")
	}

	logs := &captureHandler{}
	h := startServerAt(t, "127.0.0.1:0", filepath.Join(t.TempDir(), "probe.db"), time.UTC, slog.New(logs), nil)
	br := newBrowser(t, "http://"+h.addr)
	setupAdmin(t, br, waitSetupCode(t, logs))
	createNodeViaAPI(t, br, "closed-01")
	// 刻意**不**打开开关。

	cfg := tzHarnessConfig{NodeID: 1, NodeName: "closed-01", User: "admin", Pass: "a-very-good-password", Scenario: "closed"}
	proxy := newHarnessProxy(t, "http://"+h.addr, cfg, guestHarnessJS)
	mock := newMockServer(t, proxy)
	raw := runChromeForResult(t, chrome, mock.URL+"/", proxy.result, 120*time.Second, "1500,1100")

	var res guestResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("解析浏览器回传的结果: %v\n原始内容：%s", err, string(raw))
	}
	if res.Fatal != "" {
		t.Fatalf("浏览器里的自检流程没跑完：%s", res.Fatal)
	}
	if len(res.Errs) != 0 {
		t.Fatalf("浏览器里有 %d 条 JS 报错：%v", len(res.Errs), res.Errs)
	}
	if !res.HomeLoginView {
		t.Error("开关关着时，未登录的浏览器应当停在登录页（与以前完全一样）")
	}
	if res.GuestBarShown {
		t.Error("开关关着时不该出现「只读」提示条")
	}
	if res.HomeCards != 0 {
		t.Errorf("开关关着时未登录不该看到任何节点卡片，实际 %d 张", res.HomeCards)
	}
}

// guestHarnessJS 是注入页面里的自检脚本。
//
// 它按用户的真实操作驱动界面，断言全在 Go 那边：
//  1. 未登录进来：只读提示条在不在、三个管理员入口在不在 DOM 里、
//     卡片有没有、卡片上有没有价格、**页面上有没有出现那三个私有地址**；
//  2. 直接 fetch 两个写接口，记下状态码；
//  3. 从提示条里的登录入口登录，再看一遍（入口回来、地址行回来、设置页能进）；
//  4. 四条常用路由各走一遍，记录每条上有几条 JS 报错。
//
// 场景 closed（开关关着）只做第 1 步的"应当停在登录页"那一半。
const guestHarnessJS = `(function () {
  'use strict';
  var CFG = window.__TZCFG || {};
  var rawFetch = window.fetch.bind(window);
  var PRIVATE = String(CFG.date || '').split(',').filter(function (s) { return !!s; });
  var R = {
    errs: [], fatal: '', steps: [], adminEntries: [], homeLeaks: [], homeCardTitles: [],
    homeCardText: '', detailRows: [], detailNetwork: [], detailStats: [], detailLeaks: [],
    detailText: '', afterLoginEntries: [], afterLoginNetwork: [], afterLoginStats: [],
    afterLoginLeaks: [], routes: []
  };
  window.__GUESTRESULT = R;

  // 截图模式：把 EventSource 换成一个不联网的替身。
  // 理由见仓库里其它截图用例：挂着的 SSE 长连接会让 --virtual-time-budget
  // 永远耗不完，Chrome 也就不截图了。
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

  function node(id) { return document.getElementById(id); }
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

  // ---- 观测工具 -----------------------------------------------------------

  // 管理员入口是否**存在于 DOM**：不是看 hidden，是看元素在不在。
  function adminEntries() {
    return ['btn-add', 'btn-settings', 'btn-logout'].filter(function (id) { return !!node(id); });
  }
  // 详情页头部那三个入口（编辑 / 换 Token / 删除）同理：访客能不能看到它们，
  // 决定了他会不会去点一排"点了只会报错"的按钮。
  function detailEntries() {
    return ['detail-edit', 'detail-token', 'detail-delete'].filter(function (id) { return !!node(id); });
  }
  function leaks(text) {
    var out = [];
    PRIVATE.forEach(function (s) { if (text.indexOf(s) >= 0) out.push(s); });
    return out;
  }
  // pageText 取页面上**给人看**的文字。
  //
  // 必须排除 <script>：注入的自检脚本本身带着那三个私有值（它要拿它们去搜），
  // 而它是测试脚手架、不是页面数据 —— 不排除的话这条断言永远红。
  function pageText() {
    var clone = document.body.cloneNode(true);
    Array.prototype.forEach.call(clone.querySelectorAll('script'), function (s) {
      if (s.parentNode) s.parentNode.removeChild(s);
    });
    return clone.textContent;
  }
  // leakSpots 找出"到底是哪个元素"把私有值写进了页面（只报前若干个）。
  // 断言的报错信息里带上它，排查时不必再猜一遍。
  function leakSpots() {
    var out = [];
    var all = document.querySelectorAll('*');
    Array.prototype.forEach.call(all, function (el) {
      if (out.length >= 12 || el.tagName === 'SCRIPT') return;
      var own = '';
      Array.prototype.forEach.call(el.childNodes, function (c) {
        if (c.nodeType === 3) own += c.nodeValue;
      });
      var hit = leaks(own);
      if (!hit.length) return;
      var id = el.id ? '#' + el.id : '';
      var cls = el.className && typeof el.className === 'string' ? '.' + el.className.split(' ')[0] : '';
      out.push(el.tagName + id + cls + ' → ' + hit.join(',') + ' :: ' + own.slice(0, 80));
    });
    return out;
  }
  // 一个 <dl> 里的全部行标签（infoRow 生成的是 dt/dd 交替）。
  function dlLabels(id) {
    var dl = node(id);
    if (!dl) return [];
    var out = [];
    Array.prototype.forEach.call(dl.querySelectorAll('dt'), function (dt) { out.push(dt.textContent); });
    return out;
  }
  function dlText(id) { var dl = node(id); return dl ? dl.textContent : ''; }
  function statTexts() {
    return ['stat-price', 'stat-monthly', 'stat-left', 'stat-value'].map(function (id) {
      var n = node(id);
      return id + '=' + (n ? n.textContent : '(缺)');
    });
  }
  function cardText() {
    var grid = node('grid');
    if (!grid || !grid.children.length) return '';
    return grid.children[0].textContent;
  }
  function cardTitles() {
    var grid = node('grid');
    if (!grid) return [];
    var out = [];
    Array.prototype.forEach.call(grid.children, function (c) { out.push(c.title || ''); });
    return out;
  }
  function currentView() {
    var views = ['view-home', 'view-detail', 'view-settings', 'view-login', 'view-setup'];
    for (var i = 0; i < views.length; i++) {
      if (shown(views[i])) return views[i];
    }
    return '（没有可见视图）';
  }

  // 首页：等卡片或（开关关着时）登录页出现。两个场景都从这里开始。
  function settle() {
    return waitFor('页面脚本就绪', function () { return !!node('view-login'); }, 30000)
      .then(function () { return sleep(1200); });
  }

  // ---- 访客那一遍 ---------------------------------------------------------

  function guestPass() {
    return waitFor('访客看到首页卡片', function () {
      return shown('view-home') && node('grid') && node('grid').children.length >= 1;
    }, 30000).then(function () { return sleep(500); })
      .then(function () {
        R.guestBarShown = shown('guest-bar');
        R.guestBarText = node('guest-bar') ? node('guest-bar').textContent : '';
        R.loginEntry = !!node('btn-login-entry');
        R.adminEntries = adminEntries();
        R.homeCards = node('grid').children.length;
        R.homeCardText = cardText();
        R.homeCardTitles = cardTitles();
        R.homeLeaks = leaks(pageText()).concat(leaks(R.homeCardTitles.join(' ')));
        R.Debug = leakSpots();
        R.steps.push('访客首页观测完成');

        // 直接打写接口：访客必须拿到 401。
        return rawFetch('/api/v1/nodes', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ name: 'evil', interval_sec: 1 })
        });
      }).then(function (res) {
        R.writeStatus = res.status;
        return rawFetch('/api/v1/settings/guest', {
          method: 'PUT',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ enabled: false })
        });
      }).then(function (res) {
        R.writeStatus2 = res.status;
        R.steps.push('访客写接口观测完成');

        // 进详情页。
        window.location.hash = '#/n/' + CFG.nodeID;
        return waitFor('访客打开详情页', function () {
          return shown('view-detail') && node('detail-name') && node('detail-name').textContent === CFG.nodeName;
        }, 30000);
      }).then(function () { return sleep(1200); })
      .then(function () {
        R.detailOpened = true;
        R.detailEntries = detailEntries();
        R.detailNetwork = dlLabels('info-network');
        R.detailStats = statTexts();
        R.detailText = dlText('info-network') + ' ' + dlText('info-hardware') + ' ' + dlText('info-system') + ' ' + dlText('info-traffic');
        R.detailLeaks = leaks(pageText());
        R.detailHasChart = shown('charts-resources') && !!node('chart-cpu');
        R.steps.push('访客详情页观测完成');

        // 访客把地址改成设置页：必须被送回首页（设置接口在服务端就是 401）。
        window.location.hash = '#/settings/nodes';
        return sleep(900).then(function () { R.settingsRedirect = currentView(); });
      });
  }

  // ---- 登录之后那一遍 -----------------------------------------------------

  function login() {
    // 访客唯一的登录入口就在那条只读提示里。
    node('btn-login-entry').click();
    return waitFor('登录页出现', function () { return shown('view-login'); }, 15000)
      .then(function () {
        node('login-user').value = CFG.user;
        node('login-pass').value = CFG.pass;
        node('login-submit').click();
        return waitFor('登录后回到应用', function () {
          return shown('view-home') || shown('view-detail');
        }, 30000);
      });
  }

  function adminPass() {
    return login()
      .then(function () {
        window.location.hash = '#/n/' + CFG.nodeID;
        return waitFor('管理员打开详情页', function () {
          return shown('view-detail') && node('detail-name') && node('detail-name').textContent === CFG.nodeName;
        }, 30000);
      })
      .then(function () { return sleep(1200); })
      .then(function () {
        R.afterLoginEntries = adminEntries();
        R.afterLoginDetail = detailEntries();
        R.afterLoginNetwork = dlLabels('info-network');
        R.afterLoginStats = statTexts();
        R.afterLoginLeaks = leaks(pageText());
        R.afterLoginLoginBar = shown('guest-bar');
        R.steps.push('登录后详情页观测完成');
        // 设置页：能进、服务器列表有那一台机器、访客开关是勾着的。
        window.location.hash = '#/settings/guest';
        return waitFor('访客访问栏打开', function () {
          return shown('view-settings') && !!node('guest-enabled');
        }, 30000);
      })
      .then(function () { return sleep(1200); })
      .then(function () {
        R.GuestSwitchChecked = !!(node('guest-enabled') && node('guest-enabled').checked);
        window.location.hash = '#/settings/nodes';
        return waitFor('服务器列表就绪', function () {
          var list = node('nodes-list');
          return shown('view-settings') && list && list.children.length === 1;
        }, 30000);
      })
      .then(function () { return sleep(400); })
      .then(function () {
        R.SettingsOpened = true;
        R.SettingsNodes = node('nodes-list').children.length;
        R.steps.push('设置页观测完成');
        return true;
      });
  }

  // ---- 四条常用路由 -------------------------------------------------------

  function routePass() {
    var routes = ['#/', '#/n/' + CFG.nodeID, '#/settings/nodes', '#/settings/alert'];
    var out = [];
    return routes.reduce(function (chain, hash) {
      return chain.then(function () {
        var before = R.errs.length;
        window.location.hash = hash;
        return sleep(1200).then(function () {
          out.push({ hash: hash, errs: R.errs.length - before, view: currentView() });
        });
      });
    }, Promise.resolve()).then(function () { R.routes = out; return true; });
  }

  function run() {
    if (CFG.scenario === 'closed') {
      // 开关关着：只记"未登录停在哪儿"。
      return settle().then(function () {
        R.homeLoginView = shown('view-login');
        R.guestBarShown = shown('guest-bar');
        R.homeCards = node('grid') ? node('grid').children.length : 0;
        R.steps.push('关着开关时的未登录状态观测完成');
        return true;
      });
    }
    return settle().then(guestPass).then(adminPass).then(routePass);
  }

  function finish() {
    return rawFetch('/__result', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(R)
    }).then(function () { document.title = 'DUMPMARK:guest'; });
  }

  // shot 模式（人工截图用）：把界面开到指定状态就停住，不回传结果。
  //   guest-home    → 未登录的首页
  //   guest-detail  → 未登录的详情页（价格还在、地址那两行没有）
  //   admin-detail  → 登录后的同一个详情页（对比：地址两行回来了）
  //   guest-switch  → 设置页「访客访问」那一栏（新开关）
  // CFG.theme 非空时先切成指定主题（dark）：危险按钮的边框颜色跟着主题走，
  // 深色下"看不看得见"必须单独看一眼 —— 无头浏览器没法可靠地模拟
  // prefers-color-scheme，所以走手动切换那条分支（顺带也验了它）。
  function shot() {
    var mode = CFG.shot;
    if (CFG.theme) document.documentElement.setAttribute('data-theme', CFG.theme);
    var chain;
    if (mode === 'guest-home') {
      chain = waitFor('访客首页就绪', function () {
        return shown('view-home') && node('grid') && node('grid').children.length >= 1;
      }, 30000).then(function () { return sleep(600); });
    } else if (mode === 'guest-detail') {
      chain = waitFor('访客详情页就绪', function () {
        return shown('view-detail') && node('detail-name') && node('detail-name').textContent === CFG.nodeName;
      }, 30000).then(function () { return sleep(1000); });
    } else {
      // 管理员那两张：先从只读提示里的登录入口登录，再按地址栏里的路由走。
      //
      // 注意登录那条路会把地址栏改到 #/login（登录入口就在那儿），登完之后
      // app 会回到首页 —— 所以想要"登录后的详情页"必须自己再把 hash 设回去，
      // 否则截出来的是首页（这正是第一版截图截错的原因）。
      chain = login().then(function () { return sleep(900); });
      if (mode === 'admin-detail') {
        chain = chain.then(function () {
          window.location.hash = '#/n/' + CFG.nodeID;
          return waitFor('管理员详情页就绪', function () {
            return shown('view-detail') && node('detail-name') && node('detail-name').textContent === CFG.nodeName;
          }, 30000);
        }).then(function () { return sleep(1000); });
      }
      if (mode === 'guest-switch') {
        chain = chain.then(function () {
          window.location.hash = '#/settings/guest';
          return waitFor('访客访问栏打开', function () {
            return shown('view-settings') && !!node('guest-enabled');
          }, 30000);
        }).then(function () { return sleep(600); });
      }
    }
    return chain.then(function () { document.title = 'SHOT-READY:' + mode; });
  }

  window.addEventListener('load', function () {
    if (CFG.shot) {
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
