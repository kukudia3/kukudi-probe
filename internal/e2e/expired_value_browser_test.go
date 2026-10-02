package e2e

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"probe/internal/config"
)

// 已过期的机器，「剩余价值」那一段**整段不出现** —— 在真浏览器里的验收。
//
// 为什么必须真跑一遍：这一轮改的是"某一段显不显示"，而它坏掉的方式全是静默的 ——
// 页面照渲染、别的字段照显示，只是那一行多了一句废话（「剩余价值 ¥0.00」），
// 或者 hidden 属性设上了、CSS 却把格子照样画出来（.stat 自己写了 display:flex，
// 会盖掉 hidden 属性那条 display:none）。静态断言看不出后者：属性是对的，
// 只有量一下 offsetWidth 才知道它到底显示了没有。
//
// 一台机器一个场景（四台，同一份夹具）：
//
//	exp-01      填了价格 + 已过期 3 天   → 「剩余价值」整段/整格不出现
//	live-01     填了价格 + 还剩 28 天    → 照旧显示金额
//	soon-01     填了价格 + 还剩 5 小时   → **照旧显示 ¥0.00**（没过期，只是整天数归零）
//	noprice-01  没填价格 + 还剩 28 天    → 不显示剩余价值，但详情页那一格照旧是 —
//
// 第三台是这一轮最容易写错的判据：用 remaining_days <= 0 判断"过期"的话，
// 它会被当成过期机器（整天数同样是 0）而整段消失 —— 而它其实没过期。
// 第四台挡的是另一个方向：把"没填价格"和"已过期"混成一种（两者的详情页表现必须不同）。
func TestExpiredNodeOmitsRemainingValueInRealBrowser(t *testing.T) {
	chrome := findChrome()
	if chrome == "" {
		t.Skip("找不到 Chrome：设置 PROBE_CHROME 或把它装到默认位置后这条用例会自动跑起来")
	}

	h, br, ids := startExpiryFixture(t)

	// 期望值在**跑浏览器之前**从 API 取：页面渲染的就是这一份数据
	// （快照与页面之间只差几百毫秒，而这几台机器的口径在几小时内不会变）。
	apiNodes := fetchNodes(t, br)
	exp := nodeByName(t, apiNodes, "exp-01")
	live := nodeByName(t, apiNodes, "live-01")
	soon := nodeByName(t, apiNodes, "soon-01")
	noprice := nodeByName(t, apiNodes, "noprice-01")

	// 服务端下发的标志位：页面上的三种表现都建立在这几个事实上。
	for _, c := range []struct {
		name string
		node map[string]any
		want bool
	}{
		{"exp-01", exp, true},
		{"live-01", live, false},
		{"soon-01", soon, false},
		{"noprice-01", noprice, false},
	} {
		if got := c.node["expired"]; got != c.want {
			t.Fatalf("%s 的 expired = %v，期望 %v：夹具没造对，后面的断言无从谈起", c.name, got, c.want)
		}
	}
	// 夹具的另外三条前提（不成立的话下面的期望值会变成"随便什么都能过"）。
	if got := intField(t, live, "remaining_value_cents"); got <= 0 {
		t.Fatalf("live-01 的剩余价值 = %d，期望是正数（不然「照旧显示金额」这条断言是空的）", got)
	}
	if got := intField(t, soon, "remaining_value_cents"); got != 0 {
		t.Fatalf("soon-01 的剩余价值 = %d，期望 0（「还剩 5 小时」正是整天数归零但没过期的那种）", got)
	}
	if got := intField(t, noprice, "price_cents"); got != 0 {
		t.Fatalf("noprice-01 的价格 = %d，期望 0", got)
	}

	proxy := newHarnessProxy(t, "http://"+h.addr, tzHarnessConfig{
		Scenario: "expiry", User: "admin", Pass: "a-very-good-password",
		Nodes: ids, Focus: "exp-01",
	}, expiryHarnessJS)
	mock := newMockServer(t, proxy)
	raw := runChromeForResult(t, chrome, mock.URL+"/", proxy.result, 180*time.Second, "1500,1100")

	var res expiryResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("解析浏览器回传的结果: %v\n原始内容：%s", err, tail(string(raw), 800))
	}
	if res.Fatal != "" {
		t.Fatalf("浏览器里的自检流程没跑完：%s", res.Fatal)
	}
	if len(res.Errs) != 0 {
		t.Fatalf("浏览器里有 %d 条 JS 报错：%v", len(res.Errs), res.Errs)
	}
	t.Logf("自检脚本走过的步骤 = %v", res.Steps)

	// ---------------------------------------------------------------- 服务器列表
	//
	// 1) 已过期：那一段**整段不出现**（不是 ¥0.00，也不是 —）。
	expRow := res.List["exp-01"]
	if !expRow.RowFound {
		t.Fatal("服务器列表里没有 exp-01 那一行")
	}
	if expRow.VisibleValueTexts != 0 {
		t.Errorf("过期机器的服务器列表那一行还有可见的「剩余价值」：%q", expRow.Text)
	}
	// 行本身要还在（"少了一段"与"整行没渲染出来"是两回事）：到期文案照旧显示。
	wantExpLeft := stringField(t, exp, "expires_text")
	if !strings.Contains(expRow.Text, wantExpLeft) {
		t.Errorf("过期机器那一行的信息 = %q，期望还带着到期文案 %q（那一行只是少了剩余价值这一段）",
			expRow.Text, wantExpLeft)
	}

	// 2) 没过期：照旧显示金额（口径与详情页一致：由服务端算好、折成人民币）。
	wantLive := "剩余价值 " + cnyText(intField(t, live, "remaining_value_cents"))
	if liveRow := res.List["live-01"]; !strings.Contains(liveRow.Text, wantLive) {
		t.Errorf("没过期机器的服务器列表 = %q，期望包含 %q", liveRow.Text, wantLive)
	}

	// 3) 还剩 5 小时（整天数为 0、但**没过期**）：照旧显示 ¥0.00。
	//
	//    这一条挡的是"拿 remaining_days <= 0 当过期判据"的写法 —— 那样写，
	//    这台机器会跟过期机器一样整段消失，而它只是"今天到期"。
	wantSoon := "剩余价值 " + cnyText(0)
	if soonRow := res.List["soon-01"]; !strings.Contains(soonRow.Text, wantSoon) {
		t.Errorf("还剩 5 小时的机器（没过期）服务器列表 = %q，期望包含 %q："+
			"整天数为 0 不等于过期，判据要用服务端的 expired", soonRow.Text, wantSoon)
	}

	// 4) 没填价格：同样不显示剩余价值（改动之前就是这样），但**与过期不是一种**：
	//    它的详情页那一格照旧显示 —（见下面）。
	nopriceRow := res.List["noprice-01"]
	if nopriceRow.VisibleValueTexts != 0 {
		t.Errorf("没填价格的机器不该显示剩余价值：%q", nopriceRow.Text)
	}
	if !strings.Contains(nopriceRow.Text, stringField(t, noprice, "expires_text")) {
		t.Errorf("没填价格那一行的信息 = %q，期望还带着到期文案（没填价格不等于整行没有信息）", nopriceRow.Text)
	}

	// ---------------------------------------------------------------- 详情页
	//
	// 5) 已过期：那一格（含「剩余价值」这个标题）整格不显示，而且**真的没有占位**。
	expDetail := res.Detail["exp-01"]
	if !expDetail.ValueHidden {
		t.Error("过期机器的详情页那一格没有被隐藏（应当是整格不显示，含标题）")
	}
	if expDetail.ValueWidth != 0 {
		t.Errorf("过期机器那一格还占着 %dpx：hidden 设上了但 CSS 没兜住"+
			"（.stat 自己写了 display:flex，会盖掉 hidden 属性那条 display:none）", expDetail.ValueWidth)
	}
	if expDetail.VisibleValueTexts != 0 {
		t.Error("过期机器的详情页上还有可见的「剩余价值」字样：整段省略应当是「页面上看不到这段字」")
	}
	if expDetail.VisibleCells != 3 {
		t.Errorf("过期机器的汇总排显示了 %d 格，期望 3 格（只少「剩余价值」那一格）", expDetail.VisibleCells)
	}
	// 另外三格必须照旧：过期只与"剩余价值"有关，价格/月均/到期都是事实。
	for _, cell := range []struct{ name, got string }{
		{"节点价格", expDetail.Price},
		{"月均支出", expDetail.Monthly},
	} {
		if cell.got == "" || cell.got == "—" {
			t.Errorf("过期机器的详情页「%s」= %q，期望照旧显示（只该少「剩余价值」那一格）", cell.name, cell.got)
		}
	}
	if !strings.Contains(expDetail.Left, "已过期") {
		t.Errorf("过期机器的详情页「到期」= %q，期望写着已过期多久", expDetail.Left)
	}
	t.Logf("过期机器 exp-01：列表 %q / 详情 剩余价值 hidden=%v 宽=%d 可见格数=%d / 到期 %q",
		expRow.Text, expDetail.ValueHidden, expDetail.ValueWidth, expDetail.VisibleCells, expDetail.Left)

	// 6) 没过期：那一格照旧显示金额（同一口径、同一个数）。
	liveDetail := res.Detail["live-01"]
	if liveDetail.ValueHidden || liveDetail.ValueWidth == 0 || liveDetail.VisibleCells != 4 {
		t.Errorf("没过期机器的详情页那一格不该变：hidden=%v 宽=%d 可见格数=%d",
			liveDetail.ValueHidden, liveDetail.ValueWidth, liveDetail.VisibleCells)
	}
	if want := cnyText(intField(t, live, "remaining_value_cents")); liveDetail.ValueText != want {
		t.Errorf("没过期机器的详情页剩余价值 = %q，期望 %q", liveDetail.ValueText, want)
	}

	// 7) 还剩 5 小时：那一格在、写着 ¥0.00（"没过期就照旧显示"，哪怕是 0）。
	soonDetail := res.Detail["soon-01"]
	if soonDetail.ValueHidden || soonDetail.VisibleCells != 4 {
		t.Errorf("还剩 5 小时的机器不该被当成过期：hidden=%v 可见格数=%d",
			soonDetail.ValueHidden, soonDetail.VisibleCells)
	}
	if soonDetail.ValueText != cnyText(0) {
		t.Errorf("还剩 5 小时的机器剩余价值 = %q，期望 %q（整天数归零，但没过期）",
			soonDetail.ValueText, cnyText(0))
	}

	// 8) 没填价格：那一格在、写着 —。**这就是它与"过期"的区别** ——
	//    过期是整格消失，没填价格是留着格子说"这里没填"。
	nopriceDetail := res.Detail["noprice-01"]
	if nopriceDetail.ValueHidden || nopriceDetail.VisibleCells != 4 {
		t.Errorf("没填价格的机器那一格应当照旧留着（显示 —）：hidden=%v 可见格数=%d",
			nopriceDetail.ValueHidden, nopriceDetail.VisibleCells)
	}
	if nopriceDetail.ValueText != "—" {
		t.Errorf("没填价格的机器剩余价值 = %q，期望 —", nopriceDetail.ValueText)
	}
	t.Logf("没过期 live-01：详情 %q（%d 格） / 还剩 5 小时 soon-01：列表 %q 详情 %q / 没填价格 noprice-01：列表 %q 详情 %q",
		liveDetail.ValueText, liveDetail.VisibleCells,
		res.List["soon-01"].Text, soonDetail.ValueText,
		nopriceRow.Text, nopriceDetail.ValueText)

	// ---------------------------------------------------------------- 首页卡片
	//
	// 9) 首页卡片的「费用」行讲的是**节点价格**（+ 周期 + 到期文案），本来就**没有**
	//    剩余价值这一项：所以那一处无需改动，这条断言是"此处没有同类显示"的证据。
	//    （用户说的"首页卡片同处也一致"，落到代码里就是这个事实。）
	for _, name := range []string{"exp-01", "live-01", "soon-01", "noprice-01"} {
		card := res.Cards[name]
		if strings.Contains(card.Cost, "剩余价值") {
			t.Errorf("首页卡片 %s 的费用行 = %q：卡片上不该有剩余价值（那是价格行）", name, card.Cost)
		}
	}
	if res.Cards["noprice-01"].Hidden != true {
		t.Error("没填价格的机器在首页不该有费用行（改动前后都一样）")
	}
	if !strings.Contains(res.Cards["live-01"].Cost, cnyText(intField(t, live, "price_cents"))) {
		t.Errorf("首页卡片 live-01 的费用行 = %q，期望包含价格 %q",
			res.Cards["live-01"].Cost, cnyText(intField(t, live, "price_cents")))
	}
	t.Logf("首页卡片费用行：exp-01 %q / live-01 %q / soon-01 %q / noprice-01 hidden=%v",
		res.Cards["exp-01"].Cost, res.Cards["live-01"].Cost,
		res.Cards["soon-01"].Cost, res.Cards["noprice-01"].Hidden)

	// 10) 首页总览区那一格讲的是"所有机器加起来"，**不是某一台的状态**：
	//     过期机器贡献 0 是事实，所以这一格的口径不动（见交付说明里的取舍）。
	//     这里只确认它照旧渲染得出来（回归）。
	if !res.Overview.Found || res.Overview.Text == "" {
		t.Errorf("首页总览区的「剩余价值」那一格没渲染出来：found=%v text=%q",
			res.Overview.Found, res.Overview.Text)
	}
	t.Logf("首页总览区剩余价值 = %q", res.Overview.Text)

	// 11) 四条路由的回归：每条都要 errs=0，并且真的停在那个视图上。
	assertRoutes(t, res.Routes, map[string]string{
		"#/":               "view-home",
		"#/n/1":            "view-detail",
		"#/settings/nodes": "view-settings:nodes",
		"#/settings/alert": "view-settings:alert",
	})
}

// TestExpiredValueScreenshots 产出人工核对用的 PNG：服务器列表（过期 + 正常 +
// 只剩 5 小时 + 没填价格四台机器同一屏）与过期机器的详情页。
//
// 默认**跳过**（CI 上不该往磁盘里写 PNG）：要看图时设
//
//	$env:PROBE_SHOT_DIR = "$env:TEMP\probe-shots"; go test ./internal/e2e/ -run TestExpiredValueScreenshots -v
func TestExpiredValueScreenshots(t *testing.T) {
	outDir := os.Getenv("PROBE_SHOT_DIR")
	if outDir == "" {
		t.Skip("没设 PROBE_SHOT_DIR：截图用例默认不跑（它只产出人工核对的 PNG）")
	}
	chrome := findChrome()
	if chrome == "" {
		t.Skip("找不到 Chrome：设置 PROBE_CHROME 或把它装到默认位置")
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("创建截图目录: %v", err)
	}

	h, _, ids := startExpiryFixture(t)

	shots := []struct {
		name  string
		shot  string
		focus string
		w, h  int
		scale int
	}{
		{"expired-nodes", "nodes", "exp-01", 1500, 900, 1},
		{"expired-detail", "detail", "exp-01", 1500, 900, 1},
		{"expired-detail-live", "detail", "live-01", 1500, 900, 1},
		// 窄屏（CSS 视口 500 上下，落在 (max-width: 640px) 那一档）：
		// 少一格时那一排会退回两列、第三格铺满整行 —— 只有看图才知道有没有
		// 空出一块容器底色（Windows 上窗口有最小宽度，直接传 380 拿不到）。
		{"expired-detail-narrow", "detail", "exp-01", 500, 1500, 2},
	}
	for _, s := range shots {
		if only := os.Getenv("PROBE_SHOT_ONLY"); only != "" && !strings.Contains(only, s.name) {
			continue
		}
		cfg := tzHarnessConfig{
			Scenario: "expiry", User: "admin", Pass: "a-very-good-password",
			Nodes: ids, Focus: s.focus, Shot: s.shot,
		}
		mock := newMockServer(t, newShotProxyWith(t, "http://"+h.addr, cfg, expiryHarnessJS))
		out := filepath.Join(outDir, s.name+".png")

		// --virtual-time-budget：等页面里的自检脚本把界面开到目标状态
		// （自动化断言那条路径故意不用它：长连接 SSE 会让虚拟时间暂停）。
		args := []string{
			"--headless=new", "--no-proxy-server", "--disable-gpu", "--no-first-run",
			"--hide-scrollbars",
			"--user-data-dir=" + t.TempDir(),
			"--window-size=" + strconv.Itoa(s.w) + "," + strconv.Itoa(s.h),
			"--virtual-time-budget=60000",
			"--screenshot=" + out,
			mock.URL + "/",
		}
		if s.scale > 1 {
			args = append(args, "--force-device-scale-factor="+strconv.Itoa(s.scale))
		}
		cmd := exec.Command(chrome, args...)
		var buf bytes.Buffer
		cmd.Stdout = &buf
		cmd.Stderr = &buf
		if err := cmd.Start(); err != nil {
			t.Fatalf("启动 Chrome: %v", err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(120 * time.Second):
			killChrome(cmd)
			t.Fatalf("%s: Chrome 超时。输出尾部：\n%s", s.name, tail(buf.String(), 800))
		}
		st, err := os.Stat(out)
		if err != nil {
			t.Fatalf("%s: 截图没有生成: %v\nChrome 输出尾部：\n%s", s.name, err, tail(buf.String(), 800))
		}
		t.Logf("%s → %s（%d 字节）", s.name, out, st.Size())
	}
}

// startExpiryFixture 起一个装了四台机器的服务端：一台已过期、一台正常、
// 一台只剩 5 小时（整天数为 0 但没过期）、一台没填价格。
//
// 价格全用人民币：这一轮验的是"那一段显不显示"，不该被汇率换算那一层干扰
// （汇率那条链路由 fx_browser_test.go 盯着）。CFG.FX 也关掉：夹具不该依赖外网。
//
// 第一台建出来的机器（exp-01）id 必然是 1，路由回归里的 #/n/1 就是它。
func startExpiryFixture(t *testing.T) (*harness, *browser, map[string]int64) {
	t.Helper()
	logs := &captureHandler{}
	h := startServerAt(t, "127.0.0.1:0", filepath.Join(t.TempDir(), "probe.db"), time.UTC, slog.New(logs),
		func(cfg *config.Server) { cfg.FX = false })
	br := newBrowser(t, "http://"+h.addr)
	setupAdmin(t, br, waitSetupCode(t, logs))

	now := time.Now()
	// 到期日都避开"整天数"的边界（多给 6 小时）：剩余天数在整场用例里不会跳变，
	// 页面渲染与 Go 侧算期望值时不会因为跨过一个整天的边界而对不上。
	later := now.Add(28*24*time.Hour + 6*time.Hour).Unix()
	ids := map[string]int64{
		// 已过期 3 天多 1 分钟：`remaining_days` 是 0、`expired` 必须是 true。
		"exp-01":     createPricedNode(t, br, "exp-01", 3100, "CNY", 1, now.Add(-3*24*time.Hour-time.Minute).Unix()),
		"live-01":    createPricedNode(t, br, "live-01", 4200, "CNY", 1, later),
		"soon-01":    createPricedNode(t, br, "soon-01", 5000, "CNY", 1, now.Add(5*time.Hour+30*time.Minute).Unix()),
		"noprice-01": createPricedNode(t, br, "noprice-01", 0, "", 0, later),
	}
	if ids["exp-01"] != 1 {
		t.Fatalf("exp-01 的 id = %d，期望 1（路由回归里的 #/n/1 指着它）", ids["exp-01"])
	}
	return h, br, ids
}

// ---------------------------------------------------------------- 观测值

type expiryResult struct {
	Errs  []string `json:"errs"`
	Fatal string   `json:"fatal"`
	Steps []string `json:"steps"`

	Cards  map[string]expiryCard   `json:"cards"`
	Detail map[string]expiryDetail `json:"detail"`
	List   map[string]expiryRow    `json:"list"`

	Overview expiryOverview `json:"overview"`
	Routes   []groupRoute   `json:"routes"`
}

// expiryCard 是首页卡片「费用」那一行的观测值（卡片上**没有**剩余价值这一项）。
type expiryCard struct {
	Cost   string `json:"cost"`
	Hidden bool   `json:"hidden"`
}

// expiryDetail 是详情页顶部汇总排的观测值。
//
// ValueWidth 是关键：hidden 属性只是"要求隐藏"，而 offsetWidth === 0 才是
// "真的没有占位"。.stat 自己写了 display:flex，少一条 [hidden] 兜底就会出现
// "属性设上了、格子照样显示"——那种坏法只有量一下才知道。
type expiryDetail struct {
	ValueHidden bool   `json:"valueHidden"`
	ValueWidth  int    `json:"valueWidth"`
	ValueText   string `json:"valueText"`
	// VisibleCells 是汇总排里**真的显示了**的格子数（过期时应当是 3）。
	VisibleCells int `json:"visibleCells"`
	// VisibleValueTexts 是这一页上还**看得见**的、写着「剩余价值」的元素个数。
	// 整段省略的意思就是它是 0（DOM 里留着但不可见的元素不算"出现"）。
	VisibleValueTexts int `json:"visibleValueTexts"`

	Price   string `json:"price"`
	Monthly string `json:"monthly"`
	Left    string `json:"left"`
}

// expiryRow 是设置页服务器列表里一行的观测值。
type expiryRow struct {
	Text     string `json:"text"`
	RowFound bool   `json:"rowFound"`
	// VisibleValueTexts 同 expiryDetail：这一行里还看得见的「剩余价值」字样有几个。
	VisibleValueTexts int `json:"visibleValueTexts"`
}

// expiryOverview 是首页总览区那一格的观测值（口径这一轮不动，只做回归）。
type expiryOverview struct {
	Found bool   `json:"found"`
	Text  string `json:"text"`
}

func stringField(t *testing.T, node map[string]any, key string) string {
	t.Helper()
	v, ok := node[key].(string)
	if !ok {
		t.Fatalf("节点字段 %s 不是字符串: %v", key, node[key])
	}
	return v
}

// ---------------------------------------------------------------- 自检脚本

// expiryHarnessJS 是注入到页面里的自检脚本（与其它浏览器用例同一套做法：
// 真服务端 + 反代注入 + 结果 POST 回 mock，断言全部在 Go 那边做）。
//
// 它按用户的真实操作驱动界面：登录 → 首页读四张卡片的「费用」行 → 逐台进详情页
// 读汇总排（含"那一格到底占了多宽"）→ 进设置页读服务器列表 → 四条路由各走一遍。
const expiryHarnessJS = `(function () {
  'use strict';
  var CFG = window.__TZCFG || {};
  var NODES = CFG.nodes || {};
  var NAMES = Object.keys(NODES);
  var rawFetch = window.fetch.bind(window);
  var R = {
    errs: [], steps: [], fatal: '',
    cards: {}, detail: {}, list: {}, overview: {}, routes: []
  };
  window.__EXPIRYRESULT = R;

  // 截图模式：把 EventSource 换成不联网的替身（与其它截图用例同一个理由：
  // 挂着的 SSE 长连接会让 --virtual-time-budget 永远耗不完，Chrome 就不截图了）。
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

  // visibleTexts 数一数 root 里**看得见**的、写着这段字的元素有几个。
  //
  // "整段不出现"必须是这个意思：DOM 里留着但不可见的元素不算出现；
  // 反过来，hidden 属性设了而 CSS 没兜住（offsetWidth > 0）会在这里露出来 ——
  // 这正是这一轮最容易漏的那种坏法。
  function visibleTexts(root, text) {
    if (!root) return -1;
    var out = 0;
    Array.prototype.forEach.call(root.querySelectorAll('*'), function (el) {
      if (el.children.length === 0 && el.textContent.indexOf(text) >= 0 && el.offsetWidth > 0) out++;
    });
    return out;
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

  function goHome() {
    window.location.hash = '#/';
    return waitFor('首页卡片都画出来', function () {
      return shown('view-home') && node('grid') && node('grid').children.length >= NAMES.length;
    }, 30000).then(function () { return sleep(400); });
  }

  function cardOf(name) {
    var grid = node('grid');
    if (!grid) return null;
    var found = null;
    Array.prototype.forEach.call(grid.children, function (card) {
      var n = card.querySelector('.card-name');
      if (n && n.textContent === name) found = card;
    });
    return found;
  }

  // 首页卡片的「费用」行：这一轮要确认的是**卡片上根本没有剩余价值这一项**
  // （卡片讲的是价格 + 周期 + 到期文案）。
  function readCards() {
    NAMES.forEach(function (name) {
      var card = cardOf(name);
      if (!card) { R.cards[name] = { cost: '（没有这张卡片）', hidden: true }; return; }
      var cost = '', hidden = true;
      Array.prototype.forEach.call(card.querySelectorAll('.line'), function (line) {
        var label = line.querySelector('.line-label');
        if (label && label.textContent === '费用') {
          var value = line.querySelector('.line-value');
          cost = value ? value.textContent : '';
          hidden = !!line.hidden;
        }
      });
      R.cards[name] = { cost: cost, hidden: hidden };
    });
  }

  function goDetail(name) {
    window.location.hash = '#/n/' + NODES[name];
    return waitFor('详情页打开：' + name, function () {
      return shown('view-detail') && textOf('detail-name') === name;
    }, 30000).then(function () { return sleep(400); });
  }

  // 详情页顶部汇总排：那一格的 hidden / 真实宽度 / 文本，以及这一排还剩几格。
  function readDetail(name) {
    var value = node('stat-value');
    var cell = value ? value.parentNode : null;
    var row = cell ? cell.parentNode : null;
    var visibleCells = 0;
    if (row) {
      Array.prototype.forEach.call(row.children, function (c) {
        if (c.offsetWidth > 0) visibleCells++;
      });
    }
    R.detail[name] = {
      valueHidden: cell ? !!cell.hidden : false,
      valueWidth: cell ? cell.offsetWidth : -1,
      valueText: value ? value.textContent : '',
      visibleCells: visibleCells,
      visibleValueTexts: visibleTexts(node('view-detail'), '剩余价值'),
      price: textOf('stat-price'),
      monthly: textOf('stat-monthly'),
      left: textOf('stat-left')
    };
  }

  function goSettings(pane, wantRows) {
    window.location.hash = '#/settings/' + pane;
    return waitFor('设置页打开：' + pane, function () { return shown('view-settings'); }, 30000)
      .then(function () {
        if (!wantRows) return sleep(400);
        return waitFor('服务器列表就绪', function () {
          return node('nodes-list') && node('nodes-list').children.length === NAMES.length;
        }, 30000).then(function () { return sleep(400); });
      });
  }

  function rowOf(name) {
    var list = node('nodes-list');
    if (!list) return null;
    var found = null;
    Array.prototype.forEach.call(list.children, function (row) {
      var n = row.querySelector('.node-item-name');
      if (n && n.textContent === name) found = row;
    });
    return found;
  }

  function readList() {
    NAMES.forEach(function (name) {
      var row = rowOf(name);
      var meta = row ? row.querySelector('.node-item-meta') : null;
      R.list[name] = {
        text: meta ? meta.textContent : '（没有这一行）',
        rowFound: !!row,
        visibleValueTexts: row ? visibleTexts(row, '剩余价值') : -1
      };
    });
  }

  // 总览区那一格：口径这一轮不动，只确认它照旧渲染得出来。
  function readOverview() {
    var overview = node('overview');
    var box = null;
    if (overview) {
      Array.prototype.forEach.call(overview.children, function (cell) {
        var label = cell.querySelector('.ov-label');
        if (label && label.textContent === '剩余价值') box = cell.querySelector('.ov-value');
      });
    }
    R.overview = { found: !!box, text: box ? box.textContent : '' };
  }

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
    var focus = CFG.focus || NAMES[0];
    var routes = ['#/', '#/n/' + NODES[focus], '#/settings/nodes', '#/settings/alert'];
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

  function fullPass() {
    return goHome().then(function () { readCards(); })
      .then(function () {
        return NAMES.reduce(function (chain, name) {
          return chain.then(function () { return goDetail(name); })
            .then(function () { readDetail(name); });
        }, Promise.resolve());
      })
      .then(function () { return goHome(); })
      .then(function () {
        return waitFor('总览区已渲染', function () {
          return node('overview') && !node('overview').hidden;
        }, 20000).then(function () { return sleep(300); });
      })
      .then(function () { readOverview(); })
      .then(function () { return goSettings('nodes', true); })
      .then(function () { readList(); });
  }

  function finish() {
    return rawFetch('/__result', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(R)
    }).then(function () { document.title = 'DUMPMARK:expiry'; });
  }

  // 截图模式（人工核对用）：把界面开到指定状态就停住，不回传结果。
  //   nodes  → 设置页「服务器列表」（四台机器同一屏）
  //   detail → CFG.focus 那台机器的详情页（看汇总排少不少那一格）
  function shot() {
    var mode = CFG.shot;
    var focus = CFG.focus || NAMES[0];
    return login().then(function () {
      if (mode === 'detail') return goDetail(focus);
      return goSettings('nodes', true);
    }).then(function () { return sleep(600); })
      .then(function () { document.title = 'SHOT-READY:' + mode; });
  }

  window.addEventListener('load', function () {
    if (CFG.shot) {
      shot().catch(function (err) {
        document.title = 'SHOT-FAILED:' + String(err && err.message ? err.message : err);
      });
      return;
    }
    waitFor('页面脚本就绪', function () { return !!node('view-login'); }, 30000)
      .then(login)
      .then(fullPass)
      .then(routePass)
      .catch(function (err) { R.fatal = String(err && err.message ? err.message : err); })
      .then(finish, finish);
  });
})();`
