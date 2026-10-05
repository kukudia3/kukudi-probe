package ui

import (
	"encoding/json"
	"log/slog"
	"path/filepath"
	"probe/internal/e2e/harness"
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
	chrome := harness.FindChrome()
	if chrome == "" {
		t.Skip("找不到 Chrome：设置 PROBE_CHROME 或把它装到默认位置后这条用例会自动跑起来")
	}

	logs := &harness.CaptureHandler{}
	h := harness.StartServerAt(t, "127.0.0.1:0", filepath.Join(t.TempDir(), "probe.db"), time.UTC, slog.New(logs), nil)
	br := harness.NewBrowser(t, "http://"+h.Addr)
	harness.SetupAdmin(t, br, harness.WaitSetupCode(t, logs))

	// 四台机器：香港 2、美国 1、没填分组 1。
	//
	// 顺序**故意**与分组名的字典序不同（香港 → 美国 → 未分组 → 香港）：
	// 分组顺序应当按"卡片在首页的显示顺序里首次出现"排，而不是按名字排。
	// 没填分组那台排在中间，因此「未分组」必须出现在「香港」之后、「美国」之后 ——
	// 按字典序它会在最后，两者分得开。
	hkID := harness.CreateNodeWithGroup(t, br, "hk-01", "香港")
	harness.CreateNodeWithGroup(t, br, "us-01", "美国")
	harness.CreateNodeWithGroup(t, br, "none-01", "")
	harness.CreateNodeWithGroup(t, br, "hk-02", "香港")

	cfg := harness.HarnessConfig{NodeID: hkID, NodeName: "hk-01", Scenario: "filter", User: "admin", Pass: "a-very-good-password"}
	proxy := harness.NewHarnessProxy(t, "http://"+h.Addr, cfg, harness.GroupHarnessJS)
	mock := harness.NewMockServer(t, proxy)
	raw := harness.RunChromeForResult(t, chrome, mock.URL+"/", proxy.Result, 180*time.Second, "1500,1100")

	var res groupResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("解析浏览器回传的结果: %v\n原始内容：%s", err, harness.Tail(string(raw), 600))
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
	narrow := runGroupNarrow(t, chrome, "http://"+h.Addr)
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
	cfg := harness.HarnessConfig{Scenario: "narrow", User: "admin", Pass: "a-very-good-password"}
	proxy := harness.NewHarnessProxy(t, base, cfg, harness.GroupHarnessJS)
	mock := harness.NewMockServer(t, proxy)
	raw := harness.RunChromeForResult(t, chrome, mock.URL+"/", proxy.Result, 120*time.Second, "380,900")

	var res groupResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("解析窄屏场景的结果: %v\n原始内容：%s", err, harness.Tail(string(raw), 600))
	}
	if res.Fatal != "" {
		t.Fatalf("窄屏场景没跑完：%s", res.Fatal)
	}
	if len(res.Errs) != 0 {
		t.Fatalf("窄屏场景里有 %d 条 JS 报错：%v", len(res.Errs), res.Errs)
	}
	return res.Narrow
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
