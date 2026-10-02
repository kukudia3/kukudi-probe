package e2e

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"probe/internal/config"
	"probe/internal/protocol"
	"probe/internal/store"
)

// 首页卡片在四种状态下的视觉表现（真浏览器 + 真服务端）。
//
// 为什么要真跑一遍：这一轮改的全是**计算样式** —— 环是不是真的画上去了、被模糊的
// 到底是哪一块、覆盖层有没有把卡片的点击吃掉。静态断言只能证明"CSS 里有这条规则"，
// 证明不了"它落在这张卡片上、而且没糊到不该糊的地方"。
//
// 现场的四种状态是这样造出来的（不 mock 任何接口、也不改被测代码）：
//
//	在线   挂一个**真 Agent**，每秒上报；
//	抖动   服务端重启前，直接往 node_runtime 里播种一条 120 秒前的 last_seen
//	       （StaleAfter=10s < 120s < OfflineAfter=600s）；
//	离线   同样播种，但 last_seen 是 700 秒前（> OfflineAfter）；
//	未知   一个从来没连过的节点（node_runtime.last_seen = 0，启动时根本不会被恢复）。
//
// 为什么必须重启一次服务端：内存里的 last_seen 只有 Agent 上报这一条路能改，
// 而 seedFromRuntime 会在启动时把"上次退出前的最后状态"读回来（见 pipeline.go）。
// 抖动那台因此有一个 **590 秒** 的稳定窗口，浏览器慢慢量都来得及 —— 靠"停掉
// Agent 再等阈值"那种做法只有几秒窗口，量到一半状态就翻成离线了。
const (
	cardStaleAfter   = 10 * time.Second
	cardOfflineAfter = 600 * time.Second
	cardStaleAge     = 120 * time.Second
	cardOfflineAge   = 700 * time.Second
)

// 四台机器的名字。首页按 sort_order 排（= 创建顺序），所以顺序就是下面这个顺序。
const (
	cardOnlineName  = "online-01"
	cardStaleName   = "stale-01"
	cardOfflineName = "offline-01"
	cardUnknownName = "unknown-01"
)

// 覆盖层时间行的形状：YYYY-MM-DD HH:mm:ss。
var cardStampPattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}$`)

// TestHomeCardStatesInRealBrowser 是这一轮的主验收：四种状态各一张卡片，
// 逐张读计算样式（环 / 模糊 / 覆盖层），再走一遍四条路由回归。
func TestHomeCardStatesInRealBrowser(t *testing.T) {
	chrome := findChrome()
	if chrome == "" {
		t.Skip("找不到 Chrome：设置 PROBE_CHROME 或把它装到默认位置后这条用例会自动跑起来")
	}
	loc, err := time.LoadLocation(tzTestZone)
	if err != nil {
		t.Fatalf("加载时区 %s: %v", tzTestZone, err)
	}
	// 服务端时区与浏览器时区必须不同：一样的话，覆盖层上那行时间按哪边渲染都长得一样，
	// 时区断言等于没测（与 tz_browser_test.go 同一条理由）。
	if _, serverOff := time.Now().In(loc).Zone(); serverOff == browserOffset() {
		t.Skipf("本机时区与服务端时区 %s 的偏移相同（%d 秒），覆盖层时间的时区无从验证：请换一台机器或改 tzTestZone",
			tzTestZone, serverOff)
	}

	f := startCardStateFixture(t, loc)

	// 期望值由 **Go 的 time 包**按服务端时区算出来：浏览器只负责"把它看到的说出来"。
	// Intl 与 zoneinfo 是两套彼此独立的实现，两边逐秒对上才说明渲染用的确实是服务端时区。
	seen := time.Unix(f.offlineSeen, 0)
	wantOfflineTime := seen.In(loc).Format("2006-01-02 15:04:05")
	wantBrowserTime := seen.In(time.Local).Format("2006-01-02 15:04:05")
	if wantOfflineTime == wantBrowserTime {
		t.Fatalf("这条用例不具区分度（两种时区都写成 %q），请换一个 last_seen", wantOfflineTime)
	}

	cfg := tzHarnessConfig{
		NodeID: f.ids[cardOnlineName], NodeName: cardOnlineName, Scenario: "state",
		User: "admin", Pass: "a-very-good-password",
	}
	proxy := newHarnessProxy(t, "http://"+f.h.addr, cfg, cardStateHarnessJS)
	mock := newMockServer(t, proxy)
	raw := runChromeForResult(t, chrome, mock.URL+"/", proxy.result, 180*time.Second, "1500,1100")

	var res cardStateResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("解析浏览器回传的结果: %v\n原始内容：%s", err, tail(string(raw), 600))
	}
	if res.Fatal != "" {
		t.Fatalf("浏览器里的自检流程没跑完：%s", res.Fatal)
	}
	if len(res.Errs) != 0 {
		t.Fatalf("浏览器里有 %d 条 JS 报错：%v", len(res.Errs), res.Errs)
	}
	t.Logf("浏览器时区 = %s；服务端时区 = %s（--timezone）", res.BrowserTZ, res.ServerTZ)
	t.Logf("自检脚本走过的步骤 = %v", res.Steps)

	if res.ServerTZ != tzTestZone {
		t.Errorf("服务端下发的时区 = %q，期望 %q", res.ServerTZ, tzTestZone)
	}
	if len(res.Cards) != 4 {
		t.Fatalf("首页上量到 %d 张卡片，期望 4 张：%+v", len(res.Cards), res.Cards)
	}
	// 四态的实际计算样式逐张打出来（出了问题看这一段最快）。
	for _, c := range res.Cards {
		t.Logf("卡片 %s（%s）：box-shadow=%q filter=%q opacity=%s pointer-events=%s；"+
			"覆盖层 display=%s 大字=%q 时间=%q；标签行 filter=%q；四格资源 filter=%q",
			c.Name, c.Status, c.Ring, c.FadeFilter, c.FadeOpacity, c.FadePointer,
			c.OverlayDisplay, c.OverlayState, c.OverlayTime, c.TagsFilter, strings.Join(c.ResFilters, " | "))
	}

	// ---- ① 状态点：在名字**左边**，而且真的在 DOM 里 -------------------------
	//
	// 这一条同时证明那个 bug 修好了：以前 updateCard 给 .status（圆点的**父**元素）
	// 写 textContent，圆点被整个删掉，DOM 里 dotFound:false。
	for _, c := range res.Cards {
		if !c.DotFound {
			t.Errorf("%s：卡片上找不到状态点（.card-name-row 里没有 .dot）—— "+
				"这就是「给父元素写 textContent 把圆点删掉」那个 bug 的症状", c.Name)
		}
		if !c.DotChildOfNameRow {
			t.Errorf("%s：状态点不在名字那一行里（parent 不是 .card-name-row）", c.Name)
		}
		if !c.DotLeftOfName {
			t.Errorf("%s：状态点不在名字**左边**（几何判据没通过）", c.Name)
		}
		if c.HeadHasStatus {
			t.Errorf("%s：卡片头部右上角还留着一块 .status（状态应当只在名字左边）；"+
				"那块里现在有 %d 个圆点 —— 0 个就是「给父元素写 textContent 把圆点删掉」那个老 bug 又回来了",
				c.Name, c.HeadStatusDots)
		}
		if c.DotCount != 1 {
			t.Errorf("%s：整张卡片上有 %d 个状态点，期望恰好 1 个（名字左边那个）", c.Name, c.DotCount)
		}
		if c.DotTitle == "" {
			t.Errorf("%s：状态点上没有悬停说明（在线/抖动没有覆盖层，不留一句话这两种状态就没有文字了）", c.Name)
		}
	}
	t.Logf("四张卡片的状态点：%s", strings.Join(cardDotLog(res.Cards), "；"))
	// 四种状态的点颜色也两两不同（绿 / 黄 / 红 / 灰）：环没被看到时（比如窄屏上
	// 只有一列卡片、环细到几乎看不见），点仍然要能分开这四种状态。
	seenDot := map[string]string{}
	for _, c := range res.Cards {
		if prev, dup := seenDot[c.DotBg]; dup {
			t.Errorf("%s 与 %s 的状态点颜色完全一样（%q）：四种状态必须一眼能分开", c.Name, prev, c.DotBg)
		}
		seenDot[c.DotBg] = c.Name
	}

	// ---- ② 边框环：四态各不相同，在线没有环 --------------------------------
	ringOf := map[string]string{}
	for _, c := range res.Cards {
		ringOf[c.Name] = normalizeCSS(c.Ring)
	}
	wantRings := map[string]string{
		cardOnlineName:  "none",
		cardStaleName:   normalizeCSS("rgba(217, 119, 6, 0.25) 0px 0px 0px 1px"),
		cardOfflineName: normalizeCSS("rgba(228, 0, 20, 0.2) 0px 0px 0px 1px"),
		cardUnknownName: normalizeCSS("rgba(103, 112, 124, 0.3) 0px 0px 0px 1px"),
	}
	for name, want := range wantRings {
		got, ok := ringOf[name]
		if !ok {
			t.Fatalf("没有量到 %s 这张卡片的环", name)
		}
		if got != want {
			t.Errorf("%s 的环 = %q，期望 %q", name, got, want)
		}
	}
	// 四种状态的环两两不同（在线是 none，另外三个是三种颜色）。
	seenRing := map[string]string{}
	for name, ring := range ringOf {
		if prev, dup := seenRing[ring]; dup {
			t.Errorf("%s 与 %s 的环完全一样（%q）：四种状态必须一眼能分开", name, prev, ring)
		}
		seenRing[ring] = name
	}
	t.Logf("四态环（computed box-shadow）：%s", strings.Join(cardRingLog(res.Cards), "；"))

	// ---- ③ 模糊：只有离线/未知那一块被糊 ------------------------------------
	blurOf := map[string]string{}
	for _, c := range res.Cards {
		blurOf[c.Name] = normalizeCSS(c.FadeFilter)
	}
	for _, name := range []string{cardOfflineName, cardUnknownName} {
		c := res.card(t, name)
		if !strings.Contains(c.FadeFilter, "blur(4px)") {
			t.Errorf("%s：被模糊那一块的 filter = %q，期望含 blur(4px)", name, c.FadeFilter)
		}
		if c.FadeOpacity != "0.6" {
			t.Errorf("%s：被模糊那一块的 opacity = %q，期望 0.6", name, c.FadeOpacity)
		}
		if c.FadePointer != "none" {
			t.Errorf("%s：被模糊那一块的 pointer-events = %q，期望 none（糊掉的内容不该还能点/悬停）", name, c.FadePointer)
		}
	}
	for _, name := range []string{cardOnlineName, cardStaleName} {
		c := res.card(t, name)
		if strings.Contains(c.FadeFilter, "blur") {
			t.Errorf("%s：那一块**不该**被模糊，实际 filter = %q", name, c.FadeFilter)
		}
		if c.FadeOpacity != "1" {
			t.Errorf("%s：没被模糊时 opacity = %q，期望 1", name, c.FadeOpacity)
		}
		if c.FadePointer == "none" {
			t.Errorf("%s：没被模糊时不该关掉 pointer-events（迷你条的悬停浮层会失灵）", name)
		}
	}

	// 被模糊的内容仍然留在 DOM 里（不删、不 display:none）：离线/未知那两张卡片上，
	// 五行引导行与两条迷你条的格子照旧数得出来。
	for _, name := range []string{cardOfflineName, cardUnknownName} {
		c := res.card(t, name)
		if c.LineRows != 5 {
			t.Errorf("%s：被模糊的那一块里有 %d 行引导行，期望 5 行（内容仍须留在 DOM 里）", name, c.LineRows)
		}
		if c.MiniCells != 20 {
			t.Errorf("%s：被模糊的那一块里有 %d 个迷你条格子，期望 20 个（两条各 10 格）", name, c.MiniCells)
		}
		if !c.MiniShown {
			t.Errorf("%s：迷你条是收起来的 —— 这一条没验证到「迷你条也在被模糊的那一块里」", name)
		}
	}

	// ---- ④ 覆盖层：只有离线/未知有，文案与时间都对 --------------------------
	stateOf := map[string]string{}
	for _, c := range res.Cards {
		stateOf[c.Name] = c.OverlayState
	}
	if stateOf[cardOfflineName] != "离线" {
		t.Errorf("离线卡片的覆盖层大字 = %q，期望「离线」", stateOf[cardOfflineName])
	}
	if stateOf[cardUnknownName] != "未知" {
		t.Errorf("未知卡片的覆盖层大字 = %q，期望「未知」", stateOf[cardUnknownName])
	}
	for _, name := range []string{cardOnlineName, cardStaleName} {
		c := res.card(t, name)
		if c.OverlayDisplay != "none" {
			t.Errorf("%s：覆盖层不该显示，实际 display = %q", name, c.OverlayDisplay)
		}
		if c.OverlayState != "" || c.OverlayTime != "" {
			t.Errorf("%s：不该有覆盖层文案，实际 %q / %q（抖动不是「出事了」，不给大字）",
				name, c.OverlayState, c.OverlayTime)
		}
	}
	for _, name := range []string{cardOfflineName, cardUnknownName} {
		c := res.card(t, name)
		if c.OverlayDisplay != "flex" {
			t.Errorf("%s：覆盖层 display = %q，期望 flex", name, c.OverlayDisplay)
		}
		if c.OverlayPosition != "absolute" || c.OverlayZ != "10" {
			t.Errorf("%s：覆盖层应当是 absolute + z-index:10（被模糊内容的兄弟节点），实际 %q / %q",
				name, c.OverlayPosition, c.OverlayZ)
		}
		if !c.OverlayCoversFade {
			t.Errorf("%s：覆盖层没有铺满被模糊的那一块（inset: 0）", name)
		}
		if c.OverlayStateSize != "14px" {
			t.Errorf("%s：覆盖层大字字号 = %q，期望 14px", name, c.OverlayStateSize)
		}
		if c.OverlayStateWeight != "400" {
			t.Errorf("%s：覆盖层大字字重 = %q，期望 400（**不加粗**）", name, c.OverlayStateWeight)
		}
		if c.OverlayTimeSize != "11px" {
			t.Errorf("%s：覆盖层时间字号 = %q，期望 11px", name, c.OverlayTimeSize)
		}
	}
	// 大字**分状态**取色：离线 = --bad（故障），未知 = --fg-muted（灰环配红字看着像
	// "出事了"，而"未知"只是"还没有过消息"）；浅色与深色各量一遍（深色那两档在
	// 下面 TestHomeCardStatesDarkAndNarrow 里量）。
	checkOverlayStateColors(t, res, "浅色", cardOvBadLight, cardOvMutedLight)

	// ---- ⑤ 时间：格式对、而且是**服务端时区** -------------------------------
	offline := res.card(t, cardOfflineName)
	if !cardStampPattern.MatchString(offline.OverlayTime) {
		t.Errorf("离线卡片的时间 = %q，形状不是 YYYY-MM-DD HH:mm:ss", offline.OverlayTime)
	}
	if offline.OverlayTime != wantOfflineTime {
		t.Errorf("离线卡片的时间 = %q，期望 %q（播种的 last_seen=%d 在 %s 下的写法）——"+
			"按浏览器本地时区渲染的话会是 %q", offline.OverlayTime, wantOfflineTime, f.offlineSeen,
			tzTestZone, wantBrowserTime)
	}
	if unknown := res.card(t, cardUnknownName); unknown.OverlayTime != "—" {
		t.Errorf("未知卡片的时间 = %q，期望 —（它从来没有上报过，没有 last_seen）", unknown.OverlayTime)
	}
	t.Logf("覆盖层时间：离线 = %q（Go 按 %s 算出来也是这个；按浏览器本地时区则是 %q）；未知 = %q",
		offline.OverlayTime, tzTestZone, wantBrowserTime, res.card(t, cardUnknownName).OverlayTime)

	// ---- ⑥ 不该被模糊的地方：标签行与四格资源 ------------------------------
	for _, c := range res.Cards {
		if strings.Contains(c.TagsFilter, "blur") {
			t.Errorf("%s：标签行被模糊了（filter = %q）—— 标签讲的是「这台机器是什么」，不该糊", c.Name, c.TagsFilter)
		}
		if !c.TagsShown || c.TagCount == 0 {
			t.Errorf("%s：标签行没显示出来，这一条没验证到「标签行没被模糊」", c.Name)
		}
		if len(c.ResFilters) != 4 {
			t.Fatalf("%s：量到 %d 格资源，期望 4 格", c.Name, len(c.ResFilters))
		}
		for i, f := range c.ResFilters {
			if strings.Contains(f, "blur") {
				t.Errorf("%s：第 %d 格资源被模糊了（filter = %q）—— 最后一次读数不该糊", c.Name, i+1, f)
			}
		}
	}

	// ---- ⑦ 卡片的点击仍然可用（覆盖层与模糊都不吃鼠标）---------------------
	if !strings.Contains(res.ClickTarget, "card-fade") {
		t.Errorf("被模糊那一块的中心点上，命中测试拿到的是 %q —— "+
			"覆盖层/模糊块应当让鼠标穿透到卡片上（不该被 .card-overlay 吃掉）", res.ClickTarget)
	}
	if res.ClickHash != "#/n/"+strconv.FormatInt(f.ids[cardOfflineName], 10) {
		t.Errorf("点离线卡片被模糊的那一块之后 hash = %q，期望进到它的详情页 %q",
			res.ClickHash, "#/n/"+strconv.FormatInt(f.ids[cardOfflineName], 10))
	}
	if !res.ClickBackHome {
		t.Error("点完卡片之后回不到首页")
	}
	t.Logf("点离线卡片被模糊那一块：命中元素 = %q → hash = %q → 回到首页 ok",
		res.ClickTarget, res.ClickHash)

	// ---- ⑧ 分组筛选之后，状态的表现一个都不变 ------------------------------
	//
	// 筛选动的是卡片的 hidden，而状态样式是 data-status 驱动的：两者不该互相影响
	// （筛出来一张卡片，它的环/模糊/覆盖层必须还是原来那一套）。
	if len(res.Filtered.Visible) != 1 || res.Filtered.Visible[0] != cardOfflineName {
		t.Errorf("点「香港」之后可见的卡片 = %v，期望只有 %s", res.Filtered.Visible, cardOfflineName)
	}
	if !strings.HasPrefix(res.Filtered.Chip, "香港") {
		t.Errorf("点完之后高亮的 chip = %q，期望「香港」", res.Filtered.Chip)
	}
	plain := res.card(t, cardOfflineName)
	filtered := res.Filtered.Offline
	if normalizeCSS(filtered.Ring) != normalizeCSS(plain.Ring) {
		t.Errorf("筛选之后离线卡片的环变了：%q → %q", plain.Ring, filtered.Ring)
	}
	if filtered.FadeFilter != plain.FadeFilter || filtered.FadeOpacity != plain.FadeOpacity {
		t.Errorf("筛选之后离线卡片的模糊变了：%q/%s → %q/%s",
			plain.FadeFilter, plain.FadeOpacity, filtered.FadeFilter, filtered.FadeOpacity)
	}
	if !strings.Contains(filtered.FadeFilter, "blur(4px)") {
		t.Errorf("筛选之后离线卡片不再被模糊：filter = %q", filtered.FadeFilter)
	}
	if filtered.OverlayState != "离线" || filtered.OverlayTime != plain.OverlayTime || !filtered.OverlayCoversFade {
		t.Errorf("筛选之后离线卡片的覆盖层变了：%q / %q / 铺满=%v",
			filtered.OverlayState, filtered.OverlayTime, filtered.OverlayCoversFade)
	}
	if strings.Contains(filtered.TagsFilter, "blur") {
		t.Errorf("筛选之后标签行被模糊了：filter = %q", filtered.TagsFilter)
	}
	t.Logf("点「香港」之后：可见卡片 = %v；高亮的 chip = %q；离线卡片的环 = %q、filter = %q",
		res.Filtered.Visible, res.Filtered.Chip, normalizeCSS(filtered.Ring), filtered.FadeFilter)

	// ---- ⑨ 回归：四条路由各走一遍，每条都要 errs=0 -------------------------
	wantView := map[string]string{
		"#/": "view-home",
		"#/n/" + strconv.FormatInt(f.ids[cardOnlineName], 10): "view-detail",
		"#/settings/nodes": "view-settings:nodes",
		"#/settings/alert": "view-settings:alert",
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

// TestHomeCardStatesDarkAndNarrow 换两种环境再量一遍同一批样式。
//
// 为什么深色要单独跑：环的色值在三个主题块里各定义了一份（20% 的环在深色底上
// 根本看不见），量一遍才知道深色那一份真的生效了；而且 `:root[data-theme="dark"]`
// 与 `prefers-color-scheme` 是**两条不同的规则**，无头浏览器没法可靠地模拟后者。
//
// 为什么窄屏要单独跑：卡片只有 320px 宽时，覆盖层那两行字会不会溢出去、
// 环会不会被裁掉，只有真按那个宽度排版才知道。
func TestHomeCardStatesDarkAndNarrow(t *testing.T) {
	chrome := findChrome()
	if chrome == "" {
		t.Skip("找不到 Chrome：设置 PROBE_CHROME 或把它装到默认位置后这条用例会自动跑起来")
	}
	loc, err := time.LoadLocation(tzTestZone)
	if err != nil {
		t.Fatalf("加载时区 %s: %v", tzTestZone, err)
	}
	f := startCardStateFixture(t, loc)

	dark := cardStateRun(t, chrome, "http://"+f.h.addr, tzHarnessConfig{
		NodeID: f.ids[cardOnlineName], Scenario: "dark", Theme: "dark",
		User: "admin", Pass: "a-very-good-password",
	}, "1500,1100")

	// 深色那一份环：与浅色**不是同一组值**，而且四态仍然各不相同。
	wantDark := map[string]string{
		cardOnlineName:  "none",
		cardStaleName:   normalizeCSS("rgba(251, 191, 36, 0.4) 0px 0px 0px 1px"),
		cardOfflineName: normalizeCSS("rgba(248, 113, 113, 0.45) 0px 0px 0px 1px"),
		cardUnknownName: normalizeCSS("rgba(152, 161, 173, 0.4) 0px 0px 0px 1px"),
	}
	lightSeen := map[string]string{cardStaleName: "rgba(217,119,6,0.25)", cardOfflineName: "rgba(228,0,20,0.2)", cardUnknownName: "rgba(103,112,124,0.3)"}
	for name, want := range wantDark {
		got := normalizeCSS(dark.card(t, name).Ring)
		if got != want {
			t.Errorf("[深色] %s 的环 = %q，期望 %q", name, got, want)
		}
		if prev, ok := lightSeen[name]; ok && got == prev {
			t.Errorf("[深色] %s 的环与浅色那一份完全一样（%q）：深色主题必须给独立的值", name, got)
		}
	}
	// 深色下四态同样互不相同。
	seen := map[string]string{}
	for _, c := range dark.Cards {
		ring := normalizeCSS(c.Ring)
		if prev, dup := seen[ring]; dup {
			t.Errorf("[深色] %s 与 %s 的环完全一样（%q）", c.Name, prev, ring)
		}
		seen[ring] = c.Name
	}
	// 模糊/覆盖层的判定与浅色一致（这两条本来就与主题无关，再确认一遍没被覆盖掉）。
	checkStateSemantics(t, dark, f, "深色")
	// 大字颜色在深色下量第二遍：同一个 --fg-muted 变量在这里是另一档灰（#98a1ad），
	// 而"未知 ≠ 离线"这条判据两种主题下都得成立。
	checkOverlayStateColors(t, dark, "深色", cardOvBadDark, cardOvMutedDark)

	narrow := cardStateRun(t, chrome, "http://"+f.h.addr, tzHarnessConfig{
		NodeID: f.ids[cardOnlineName], Scenario: "narrow",
		User: "admin", Pass: "a-very-good-password",
	}, "380,900")
	if narrow.OverflowX > 0 {
		t.Errorf("[窄屏] 页面横向溢出了 %dpx（视口 %dpx）—— 覆盖层或卡片被撑宽了",
			narrow.OverflowX, narrow.Narrow.Viewport)
	}
	// Chrome 无头模式有最小窗口宽度（要 380 拿到的是 485），所以这里不钉具体数字，
	// 只确认真的落在"手机那一档"（<= 640px）—— 否则这条用例名不副实。
	if narrow.Narrow.Viewport <= 0 || narrow.Narrow.Viewport > 640 {
		t.Fatalf("[窄屏] 视口 = %dpx，没有落进移动那一档（<= 640px）：这条用例没验证到窄屏",
			narrow.Narrow.Viewport)
	}
	if narrow.Narrow.CardWidth <= 0 || narrow.Narrow.CardWidth > narrow.Narrow.Viewport {
		t.Errorf("[窄屏] 卡片宽度 = %dpx，视口 = %dpx", narrow.Narrow.CardWidth, narrow.Narrow.Viewport)
	}
	// 覆盖层那两行字必须在卡片里（左右各留 1px 容差）。
	for _, name := range []string{cardOfflineName, cardUnknownName} {
		c := narrow.card(t, name)
		if !c.OverlayCoversFade {
			t.Errorf("[窄屏] %s：覆盖层没有铺满被模糊的那一块", name)
		}
		if c.OverlayTextWidth <= 0 || c.OverlayTextWidth > narrow.Narrow.CardWidth {
			t.Errorf("[窄屏] %s：覆盖层文字宽 %dpx，卡片只有 %dpx —— 那两行字溢出去了",
				name, c.OverlayTextWidth, narrow.Narrow.CardWidth)
		}
	}
	checkStateSemantics(t, narrow, f, "窄屏")
	// 窄屏这一次是浅色主题：大字颜色按浅色那一档再量一遍（窄屏不改变颜色，
	// 但"量到的是哪一档"要说清楚）。
	checkOverlayStateColors(t, narrow, "窄屏", cardOvBadLight, cardOvMutedLight)
	t.Logf("[窄屏] 视口 %dpx、卡片 %dpx、页面横向溢出 %dpx", narrow.Narrow.Viewport, narrow.Narrow.CardWidth, narrow.OverflowX)
	t.Logf("[深色] 四态环：%s", strings.Join(cardRingLog(dark.Cards), "；"))
}

// checkStateSemantics 把"与主题无关"的那几条再钉一遍（深色/窄屏两次运行各来一次）：
// 点还在名字左边、只有离线/未知被模糊、覆盖层文案与时间格式不变。
func checkStateSemantics(t *testing.T, res cardStateResult, f *cardStateFixture, tag string) {
	t.Helper()
	if res.Fatal != "" {
		t.Fatalf("[%s] 浏览器里的自检流程没跑完：%s", tag, res.Fatal)
	}
	if len(res.Errs) != 0 {
		t.Fatalf("[%s] 浏览器里有 %d 条 JS 报错：%v", tag, len(res.Errs), res.Errs)
	}
	if len(res.Cards) != 4 {
		t.Fatalf("[%s] 首页上量到 %d 张卡片，期望 4 张", tag, len(res.Cards))
	}

	wantStatus := map[string]string{
		cardOnlineName: "online", cardStaleName: "stale",
		cardOfflineName: "offline", cardUnknownName: "unknown",
	}
	for _, c := range res.Cards {
		if c.Status != wantStatus[c.Name] {
			t.Errorf("[%s] %s 的状态 = %q，期望 %q", tag, c.Name, c.Status, wantStatus[c.Name])
		}
		if !c.DotFound || !c.DotLeftOfName || c.HeadHasStatus {
			t.Errorf("[%s] %s：状态点必须在名字左边、右上角不许有 .status（dotFound=%v leftOfName=%v headHasStatus=%v）",
				tag, c.Name, c.DotFound, c.DotLeftOfName, c.HeadHasStatus)
		}
	}
	for _, name := range []string{cardOfflineName, cardUnknownName} {
		c := res.card(t, name)
		if !strings.Contains(c.FadeFilter, "blur(4px)") || c.FadeOpacity != "0.6" {
			t.Errorf("[%s] %s：没有被模糊（filter=%q opacity=%q）", tag, name, c.FadeFilter, c.FadeOpacity)
		}
		if c.OverlayDisplay != "flex" || c.OverlayState == "" {
			t.Errorf("[%s] %s：覆盖层没出来（display=%q text=%q）", tag, name, c.OverlayDisplay, c.OverlayState)
		}
		if strings.Contains(c.TagsFilter, "blur") {
			t.Errorf("[%s] %s：标签行被模糊了", tag, name)
		}
		for i, ff := range c.ResFilters {
			if strings.Contains(ff, "blur") {
				t.Errorf("[%s] %s：第 %d 格资源被模糊了", tag, name, i+1)
			}
		}
	}
	// 时间：离线那台是 YYYY-MM-DD HH:mm:ss；未知那台从来没有上报过，只能是 —。
	if got := res.card(t, cardOfflineName).OverlayTime; !cardStampPattern.MatchString(got) {
		t.Errorf("[%s] 离线卡片的时间 = %q，形状不是 YYYY-MM-DD HH:mm:ss", tag, got)
	}
	if got := res.card(t, cardUnknownName).OverlayTime; got != "—" {
		t.Errorf("[%s] 未知卡片的时间 = %q，期望 —（没有 last_seen）", tag, got)
	}
	for _, name := range []string{cardOnlineName, cardStaleName} {
		c := res.card(t, name)
		if strings.Contains(c.FadeFilter, "blur") {
			t.Errorf("[%s] %s：不该被模糊，实际 filter=%q", tag, name, c.FadeFilter)
		}
		if c.OverlayDisplay != "none" || c.OverlayState != "" {
			t.Errorf("[%s] %s：不该有覆盖层（display=%q text=%q）", tag, name, c.OverlayDisplay, c.OverlayState)
		}
	}
	// 离线那台的时间必须是服务端时区下的写法（两次运行都要对）。
	want := time.Unix(f.offlineSeen, 0).In(f.loc).Format("2006-01-02 15:04:05")
	if got := res.card(t, cardOfflineName).OverlayTime; got != want {
		t.Errorf("[%s] 离线卡片的时间 = %q，期望 %q（服务端时区 %s）", tag, got, want, tzTestZone)
	}
}

// 覆盖层大字的颜色：三套主题里 --bad / --fg-muted 各自的 rgb（见 style.css 的三个
// 变量块）。离线 = --bad：浅色 #dc2626、深色 #f87171；未知 = --fg-muted：
// 浅色 #67707c、深色 #98a1ad。
const (
	cardOvBadLight   = "rgb(220, 38, 38)"
	cardOvMutedLight = "rgb(103, 112, 124)"
	cardOvBadDark    = "rgb(248, 113, 113)"
	cardOvMutedDark  = "rgb(152, 161, 173)"
)

// checkOverlayStateColors 量"覆盖层大字分状态取色"这一条：离线必须是 --bad 的红、
// 未知必须是 --fg-muted 的灰，而且**两者必须不同** —— 灰环配红字看着像"出事了"，
// 而"未知"只是"这台机器还没有过消息"。浅色 / 深色 / 窄屏三次运行各调一次
// （同一个 --fg-muted 变量在三套主题里落在不同的 rgb 上，所以要按主题给期望值）。
func checkOverlayStateColors(t *testing.T, res cardStateResult, tag, wantBad, wantMuted string) {
	t.Helper()
	offline := normalizeCSS(res.card(t, cardOfflineName).OverlayStateColor)
	unknown := normalizeCSS(res.card(t, cardUnknownName).OverlayStateColor)
	if offline != normalizeCSS(wantBad) {
		t.Errorf("[%s] 离线大字颜色 = %q，期望 --bad（%q）", tag, offline, wantBad)
	}
	if unknown != normalizeCSS(wantMuted) {
		t.Errorf("[%s] 未知大字颜色 = %q，期望 --fg-muted（%q）", tag, unknown, wantMuted)
	}
	if offline == unknown {
		t.Errorf("[%s] 离线与未知的大字颜色完全一样（%q）：未知不是故障，必须与离线分开", tag, offline)
	}
	t.Logf("[%s] 覆盖层大字颜色：离线 = %q（--bad 的红）；未知 = %q（--fg-muted 的灰）", tag, offline, unknown)
}

// cardStateRun 跑一次 Chrome 并回传观测值（深色/窄屏共用）。
func cardStateRun(t *testing.T, chrome, base string, cfg tzHarnessConfig, window string) cardStateResult {
	t.Helper()
	proxy := newHarnessProxy(t, base, cfg, cardStateHarnessJS)
	mock := newMockServer(t, proxy)
	raw := runChromeForResult(t, chrome, mock.URL+"/", proxy.result, 180*time.Second, window)
	var res cardStateResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("[%s] 解析浏览器回传的结果: %v\n原始内容：%s", cfg.Scenario, err, tail(string(raw), 600))
	}
	return res
}

func browserOffset() int {
	_, off := time.Now().In(time.Local).Zone()
	return off
}

// ---------------------------------------------------------------- 现场

// cardStateFixture 是"四种状态各一台"的现场。
type cardStateFixture struct {
	h           *harness
	loc         *time.Location
	ids         map[string]int64
	offlineSeen int64
}

// startCardStateFixture 造出四种状态齐全的一个现场（在线一台 + 抖动/离线/未知各一台）。
//
// 顺序很重要：起服务端 → 建四台机器 → 只给在线那台挂真 Agent → **停掉服务端** →
// 直接往库里播种 last_seen 与探测数据 → 用同一个地址、同一个库重启。
// 播种必须发生在服务端停掉之后：服务端在跑的时候会把内存里的最后状态写回
// node_runtime，早播的那两行会被覆盖。
func startCardStateFixture(t *testing.T, loc *time.Location) *cardStateFixture {
	t.Helper()
	logs := &captureHandler{}
	dbPath := filepath.Join(t.TempDir(), "probe.db")
	mutate := func(cfg *config.Server) {
		cfg.StaleAfter = cardStaleAfter
		cfg.OfflineAfter = cardOfflineAfter
	}

	h := startServerAt(t, "127.0.0.1:0", dbPath, loc, slog.New(logs), mutate)
	addr := h.addr
	br := newBrowser(t, "http://"+addr)
	setupAdmin(t, br, waitSetupCode(t, logs))

	ids := map[string]int64{}
	// 分组**故意**分成三组：下面要验证"分组筛选之后，筛出来的那张卡片的环/模糊/
	// 覆盖层一样正确"—— 四台全在同一组的话，筛选根本筛不掉任何东西。
	groupOf := map[string]string{
		cardOnlineName: "测试", cardStaleName: "测试",
		cardOfflineName: "香港", cardUnknownName: "美国",
	}
	var onlineToken string
	for _, name := range []string{cardOnlineName, cardStaleName, cardOfflineName, cardUnknownName} {
		id, token := createNodeViaAPI(t, br, name)
		ids[name] = id
		if name == cardOnlineName {
			onlineToken = token
		}
		// 价格与标签：卡片上「费用」那一行与最下面的标签行都要有东西，
		// 否则"这两处没被模糊"就没验证到（空行本来就是 hidden 的）。
		status, body := br.do(http.MethodPatch, "/api/v1/nodes/"+strconv.FormatInt(id, 10), map[string]any{
			"name": name, "group_name": groupOf[name], "region": "HK", "interval_sec": 1,
			"price_cents": 1999, "currency": "CNY", "billing_months": 1,
			"traffic_limit": 1_000_000_000_000, "traffic_warn_pct": 80, "reset_day": 1,
			"tags": []string{"hk", "生产"}, "enabled": true,
		}, true)
		if status != http.StatusOK {
			t.Fatalf("预置 %s 的价格/标签失败: %d %v", name, status, body)
		}
	}

	// 在线那台挂一个**真 Agent**（其余三台一位 Agent 都没有）。
	client, _ := newClient(t, "http://"+addr, onlineToken)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = client.Run(ctx) }()
	waitFor(t, 30*time.Second, "在线那台开始上报", func() bool {
		n, ok := h.srv.State().Get(ids[cardOnlineName])
		return ok && n.Connected && n.Seq >= 1
	})

	// 停服务端（Run 返回时后台循环已经全部退出，库也关干净了），再直接播种。
	h.stop(t)

	now := time.Now()
	offlineSeen := now.Add(-cardOfflineAge).Unix()
	seedSeen := map[string]int64{
		cardStaleName:   now.Add(-cardStaleAge).Unix(),
		cardOfflineName: offlineSeen,
	}
	seedCardStateDB(t, dbPath, ids, seedSeen, addr, loc)

	h2 := startServerAt(t, addr, dbPath, loc, slog.New(logs), mutate)
	if h2.addr != addr {
		t.Fatalf("重启后的地址变了：%s → %s", addr, h2.addr)
	}

	// Agent 会自动重连（见 TestAgentReconnectsAfterServerRestart）。
	waitFor(t, 60*time.Second, "重启后在线那台重新连上", func() bool {
		n, ok := h2.srv.State().Get(ids[cardOnlineName])
		return ok && n.Connected && n.Seq >= 1
	})

	// 起浏览器之前先自己核一遍四态：状态不对的话，浏览器里那四条断言全是噪音。
	wantStatus := map[string]string{
		cardOnlineName: "online", cardStaleName: "stale",
		cardOfflineName: "offline", cardUnknownName: "unknown",
	}
	status, body := br.do(http.MethodGet, "/api/v1/nodes", nil, false)
	if status != http.StatusOK {
		t.Fatalf("取节点列表失败: %d", status)
	}
	nodes, _ := body["nodes"].([]any)
	if len(nodes) != 4 {
		t.Fatalf("节点列表里有 %d 台机器，期望 4 台", len(nodes))
	}
	for _, rawNode := range nodes {
		n, _ := rawNode.(map[string]any)
		name, _ := n["name"].(string)
		got, _ := n["status"].(string)
		want, ok := wantStatus[name]
		if !ok {
			t.Fatalf("节点列表里出现了一台没登记的机器 %q", name)
		}
		if got != want {
			t.Fatalf("%s 的状态 = %q，期望 %q（现场没造对，后面的浏览器断言没有意义）", name, got, want)
		}
		t.Logf("现场：%s = %s（last_seen=%v）", name, got, n["last_seen"])
	}
	return &cardStateFixture{h: h2, loc: loc, ids: ids, offlineSeen: offlineSeen}
}

// seedCardStateDB 在服务端停着的时候打开库做四件事：
//
//  1. 把抖动/离线那两台的 last_seen 改成一个"过去"的时刻（重启时由 seedFromRuntime 读回内存）；
//  2. 配两个探测目标（迷你条与「探测」那一行要有东西可画）；
//  3. 给四台机器各铺一小时的探测桶（10 段迷你条的每一格都有数）；
//  4. 关库，交还给服务端。
//
// 未知那台**故意不碰**：node_runtime 那一行的 last_seen 保持 0，
// LoadRuntime 的 `WHERE last_seen > 0` 会把它滤掉 → 内存里没有它的状态 → 未知。
func seedCardStateDB(t *testing.T, dbPath string, ids map[string]int64, seen map[string]int64, addr string, loc *time.Location) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("打开数据库播种: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Fatalf("关闭数据库: %v", err)
		}
	}()

	for name, ts := range seen {
		res, err := db.Writer().ExecContext(ctx,
			`UPDATE node_runtime SET last_seen = ?, status = 'offline', updated_at = ? WHERE node_id = ?`,
			ts, ts, ids[name])
		if err != nil {
			t.Fatalf("播种 %s 的 last_seen: %v", name, err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			t.Fatalf("播种 %s 的 last_seen 影响了 %d 行，期望 1 行", name, n)
		}
	}

	// 探测目标：TCP 到**面板自己**（127.0.0.1:<端口>）。真 Agent 会照着这份配置去探测，
	// 打到面板自己的监听口上必然立刻成功 —— 用外网地址的话，测试机上可能一路超时。
	//
	// 只要**一个**目标：卡片上那两条迷你条（延迟 / 丢包）同源于同一个目标的桶。
	// （同地址同端口的目标会被存储层按"重复"去重，写两个只会剩一个。）
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("解析面板地址 %s: %v", addr, err)
	}
	port, _ := strconv.Atoi(portStr)
	saved, err := db.SetPingSettings(ctx, []store.PingTarget{
		{Label: "面板", Type: protocol.PingTypeTCP, Host: "127.0.0.1", Port: port, Enabled: true},
	}, 60)
	if err != nil {
		t.Fatalf("配置探测目标: %v", err)
	}
	if len(saved.Targets) != 1 {
		t.Fatalf("配了 1 个探测目标，存下来 %d 个", len(saved.Targets))
	}
	targetID := saved.Targets[0].ID

	// 一小时的探测桶：每分钟一行，四台机器各一份。10 段（每段 6 分钟）因此都有数。
	// 丢包那一行也要有颜色差：每 17 分钟来一段全丢（前端按段着色）。
	now := time.Now().Unix()
	base := now - now%60 - 55*60
	buckets := make([]store.PingBucket, 0, 4*55)
	for i := int64(0); i < 55; i++ {
		for _, nodeID := range ids {
			avg := 18 + float64(i%7)
			loss := 0.0
			if i%17 == 3 {
				loss = 100
			}
			buckets = append(buckets, store.NewPingBucket(nodeID, targetID, base+i*60,
				avg, avg-2, avg+4, loss))
		}
	}
	if err := db.UpsertPingBuckets(ctx, buckets); err != nil {
		t.Fatalf("铺探测桶: %v", err)
	}
	t.Logf("已播种：%d 台机器的 last_seen、1 个探测目标、%d 行探测桶", len(seen), len(buckets))
}

// ---------------------------------------------------------------- 观测值

type cardStateResult struct {
	Errs  []string `json:"errs"`
	Fatal string   `json:"fatal"`
	Steps []string `json:"steps"`

	BrowserTZ string `json:"browserTZ"`
	ServerTZ  string `json:"serverTZ"`

	Cards []cardObs `json:"cards"`

	ClickTarget   string      `json:"clickTarget"`
	ClickHash     string      `json:"clickHash"`
	ClickBackHome bool        `json:"clickBackHome"`
	OverflowX     int         `json:"overflowX"`
	Narrow        cardNarrow  `json:"narrow"`
	Filtered      cardFilter  `json:"filtered"`
	Routes        []cardRoute `json:"routes"`
}

// cardFilter 是"点一个分组 chip 之后"的观测值：筛出来的那张卡片，
// 环 / 模糊 / 覆盖层必须**一模一样**（筛选只是显隐，不该改变状态的表现）。
type cardFilter struct {
	Chip    string   `json:"chip"`
	Visible []string `json:"visible"`
	Offline cardObs  `json:"offline"`
}

// card 按名字取一张卡片的观测值。
func (r cardStateResult) card(t *testing.T, name string) cardObs {
	t.Helper()
	for _, c := range r.Cards {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("观测值里没有 %s 这张卡片（量到的有 %v）", name, cardNamesOf(r.Cards))
	return cardObs{}
}

func cardNamesOf(cards []cardObs) []string {
	out := make([]string, 0, len(cards))
	for _, c := range cards {
		out = append(out, c.Name)
	}
	return out
}

// cardObs 是一张卡片上量到的**计算样式**（浏览器只负责说出来，断言在 Go 这边）。
type cardObs struct {
	Name   string `json:"name"`
	Status string `json:"status"`

	DotFound          bool   `json:"dotFound"`
	DotChildOfNameRow bool   `json:"dotChildOfNameRow"`
	DotLeftOfName     bool   `json:"dotLeftOfName"`
	DotTitle          string `json:"dotTitle"`
	DotBg             string `json:"dotBg"`
	HeadHasStatus     bool   `json:"headHasStatus"`
	DotCount          int    `json:"dotCount"`
	HeadStatusDots    int    `json:"headStatusDots"`

	Ring       string `json:"ring"`
	CardBorder string `json:"cardBorder"`

	FadeFilter  string `json:"fadeFilter"`
	FadeOpacity string `json:"fadeOpacity"`
	FadePointer string `json:"fadePointer"`

	OverlayDisplay     string `json:"overlayDisplay"`
	OverlayPosition    string `json:"overlayPosition"`
	OverlayZ           string `json:"overlayZ"`
	OverlayState       string `json:"overlayState"`
	OverlayTime        string `json:"overlayTime"`
	OverlayStateSize   string `json:"overlayStateSize"`
	OverlayStateWeight string `json:"overlayStateWeight"`
	OverlayStateColor  string `json:"overlayStateColor"`
	OverlayTimeSize    string `json:"overlayTimeSize"`
	OverlayCoversFade  bool   `json:"overlayCoversFade"`
	OverlayTextWidth   int    `json:"overlayTextWidth"`

	TagsFilter string `json:"tagsFilter"`
	TagsShown  bool   `json:"tagsShown"`
	TagCount   int    `json:"tagCount"`

	ResFilters []string `json:"resFilters"`

	LineRows   int    `json:"lineRows"`
	MiniCells  int    `json:"miniCells"`
	MiniShown  bool   `json:"miniShown"`
	MiniFilter string `json:"miniFilter"`
}

type cardRoute struct {
	Hash string `json:"hash"`
	Errs int    `json:"errs"`
	View string `json:"view"`
}

type cardNarrow struct {
	Viewport  int `json:"viewport"`
	CardWidth int `json:"cardWidth"`
}

// normalizeCSS 把计算样式里的空白压掉再比：Chrome 的序列化里逗号/空格怎么写
// 不该让断言变红（`rgba(228, 0, 20, 0.2) 0px 0px 0px 1px` 与它的紧凑写法是一回事）。
func normalizeCSS(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(s))), "")
}

func cardRingLog(cards []cardObs) []string {
	out := make([]string, 0, len(cards))
	for _, c := range cards {
		out = append(out, c.Name+"="+normalizeCSS(c.Ring))
	}
	return out
}

func cardDotLog(cards []cardObs) []string {
	out := make([]string, 0, len(cards))
	for _, c := range cards {
		out = append(out, c.Name+"{点="+c.DotBg+" 在名字左边="+
			strconv.FormatBool(c.DotLeftOfName)+" 文案="+c.DotTitle+"}")
	}
	return out
}

// ---------------------------------------------------------------- 浏览器里的自检脚本

// cardStateHarnessJS 注入到首页里（与其它浏览器用例同一套做法：只装钩子、只把
// 量到的计算样式说出来，一句断言都不在这里做）。
const cardStateHarnessJS = `(function () {
  'use strict';
  var CFG = window.__TZCFG || {};
  var rawFetch = window.fetch.bind(window);
  var R = {
    errs: [], fatal: '', steps: [], browserTZ: '', serverTZ: '',
    cards: [], clickTarget: '', clickHash: '', clickBackHome: false,
    overflowX: 0, narrow: {}, filtered: {}, routes: []
  };
  window.__CARDRESULT = R;

  // 截图模式：把 EventSource 换成不联网的替身（挂着的 SSE 会让 --virtual-time-budget
  // 永远耗不完，Chrome 就不会截图）。
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
  try { R.browserTZ = Intl.DateTimeFormat().resolvedOptions().timeZone; } catch (e) { R.browserTZ = '?'; }

  // 深色走**手动切换**那条分支（[data-theme="dark"]）：无头浏览器没法可靠地模拟
  // prefers-color-scheme，而这两条规则里的环色必须各自正确。
  if (CFG.theme) document.documentElement.setAttribute('data-theme', CFG.theme);

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

  function cards() {
    var grid = node('grid');
    return grid ? Array.prototype.slice.call(grid.children) : [];
  }
  function inner(el) { return el ? el.textContent : ''; }
  function filterOf(el) { return el ? getComputedStyle(el).filter : ''; }

  // observe 读一张卡片上"与状态有关"的全部计算样式。
  function observe(card) {
    var nameEl = card.querySelector('.card-name');
    var row = card.querySelector('.card-name-row');
    var dot = row ? row.querySelector('.dot') : null;
    var body = card.querySelector('.card-fade-body');
    var overlay = card.querySelector('.card-overlay');
    var ovState = overlay ? overlay.querySelector('.card-ov-state') : null;
    var ovTime = overlay ? overlay.querySelector('.card-ov-time') : null;
    var tags = card.querySelector('.card-tags');
    var mini = card.querySelector('.card-mini');
    var cs = getComputedStyle(card);
    var bs = body ? getComputedStyle(body) : null;
    var os = overlay ? getComputedStyle(overlay) : null;

    // "点在名字左边"是**几何**判据：光有元素、顺序对，也可能被布局挤到别处去。
    var dotLeftOfName = false;
    if (dot && nameEl) {
      var dr = dot.getBoundingClientRect();
      var nr = nameEl.getBoundingClientRect();
      dotLeftOfName = dr.width > 0 && dr.height > 0 && dr.right <= nr.left + 0.5;
    }

    var resFilters = [];
    Array.prototype.forEach.call(card.querySelectorAll('.res-cell'), function (cell) {
      resFilters.push(getComputedStyle(cell).filter);
    });
    var tagCount = card.querySelectorAll('.card-tags .tag').length;

    var fr = body ? body.getBoundingClientRect() : { left: 0, top: 0, width: 0, height: 0 };
    var covers = false;
    var ovTextWidth = 0;
    if (overlay && body && os.display !== 'none') {
      var or = overlay.getBoundingClientRect();
      covers = Math.abs(or.left - fr.left) <= 1 && Math.abs(or.top - fr.top) <= 1 &&
        Math.abs(or.width - fr.width) <= 1 && Math.abs(or.height - fr.height) <= 1;
      var sr = ovState ? ovState.getBoundingClientRect() : null;
      var tr = ovTime ? ovTime.getBoundingClientRect() : null;
      ovTextWidth = Math.max(sr ? sr.width : 0, tr ? tr.width : 0);
    }

    return {
      name: inner(nameEl),
      status: card.getAttribute('data-status') || '',
      dotFound: !!dot,
      dotChildOfNameRow: !!(row && dot && dot.parentNode === row),
      dotLeftOfName: dotLeftOfName,
      dotTitle: dot ? dot.title : '',
      dotBg: dot ? getComputedStyle(dot).backgroundColor : '',
      headHasStatus: !!card.querySelector('.card-head .status'),
      // 卡片上所有的圆点、以及头部那块 .status 里的圆点数：
      // 老代码给 .status（圆点的**父**元素）写 textContent，圆点会被整个删掉 ——
      // 那时 headHasStatus 仍是 true、而 headStatusDots 是 0。
      dotCount: card.querySelectorAll('.dot').length,
      headStatusDots: card.querySelectorAll('.card-head .status .dot').length,
      ring: cs.boxShadow,
      cardBorder: cs.borderColor,
      fadeFilter: bs ? bs.filter : '',
      fadeOpacity: bs ? bs.opacity : '',
      fadePointer: bs ? bs.pointerEvents : '',
      overlayDisplay: os ? os.display : '',
      overlayPosition: os ? os.position : '',
      overlayZ: os ? os.zIndex : '',
      overlayState: inner(ovState),
      overlayTime: inner(ovTime),
      overlayStateSize: ovState ? getComputedStyle(ovState).fontSize : '',
      overlayStateWeight: ovState ? getComputedStyle(ovState).fontWeight : '',
      overlayStateColor: ovState ? getComputedStyle(ovState).color : '',
      overlayTimeSize: ovTime ? getComputedStyle(ovTime).fontSize : '',
      overlayCoversFade: covers,
      overlayTextWidth: Math.round(ovTextWidth),
      tagsFilter: filterOf(tags),
      tagsShown: !!tags && !tags.hidden,
      tagCount: tagCount,
      resFilters: resFilters,
      lineRows: card.querySelectorAll('.card-lines .line').length,
      miniCells: card.querySelectorAll('.card-mini .mini-cell').length,
      miniShown: !!mini && !mini.hidden,
      miniFilter: filterOf(mini),
      clickX: fr.left + fr.width / 2,
      clickY: fr.top + fr.height / 2
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
      })
      .then(function () {
        if (!shown('view-home')) {
          window.location.hash = '#/';
          return waitFor('回到首页', function () { return shown('view-home'); }, 15000);
        }
        return true;
      });
  }

  // 四种状态**同时**在页面上：少一种就再等一会儿（状态是服务端算的，
  // 在线那台要等 Agent 重连上来）。
  function home() {
    return waitFor('首页四张卡片、四种状态齐备', function () {
      if (!shown('view-home')) return false;
      var list = cards();
      if (list.length !== 4) return false;
      var want = ['online', 'stale', 'offline', 'unknown'];
      for (var i = 0; i < want.length; i++) {
        var c = document.querySelector('.card[data-status="' + want[i] + '"]');
        if (!c) return false;
      }
      // 被模糊的那两张要已经写上覆盖层文案了（写完才说明这一帧画全了）。
      var off = document.querySelector('.card[data-status="offline"] .card-ov-state');
      var unk = document.querySelector('.card[data-status="unknown"] .card-ov-state');
      if (!off || !off.textContent || !unk || !unk.textContent) return false;
      // 迷你条也要画出来（下面要断言"迷你条也在被模糊的那一块里"）。
      var mini = document.querySelector('.card[data-status="offline"] .card-mini');
      return !!mini && !mini.hidden && mini.querySelectorAll('.mini-cell').length === 20;
    }, 30000).then(function () { return sleep(600); });
  }

  function serverTZ() {
    return rawFetch('/api/v1/nodes', {
      headers: { 'Accept': 'application/json' }, credentials: 'same-origin', cache: 'no-store'
    }).then(function (res) { return res.json(); }).then(function (data) {
      R.serverTZ = (data.server && data.server.timezone) || '';
      return true;
    });
  }

  // 点一下**被模糊那一块的中心**：命中测试 + 真点击。
  // 覆盖层与模糊块都是 pointer-events: none，所以点必须落到卡片上、进详情页。
  function clickBlurred(id) {
    var target = document.querySelector('.card[data-status="offline"]');
    if (!target) throw new Error('页面上没有离线的卡片');
    var info = observe(target);
    var el = document.elementFromPoint(info.clickX, info.clickY);
    R.clickTarget = el ? (el.className || el.tagName) : '（没有命中任何元素）';
    if (!el) return false;
    el.click();
    return sleep(600).then(function () {
      R.clickHash = window.location.hash;
      window.location.hash = '#/';
      return waitFor('点完卡片回到首页', function () { return shown('view-home'); }, 15000);
    }).then(function () { R.clickBackHome = true; return true; });
  }

  // clickChip 点一枚分组 chip（分组筛选那一排是 app.js 从实际存在的分组生成的）。
  function clickChip(prefix) {
    var box = node('group-filter');
    if (!box) throw new Error('找不到分组筛选那一排 #group-filter');
    var btns = box.querySelectorAll('button');
    for (var i = 0; i < btns.length; i++) {
      if (btns[i].textContent.indexOf(prefix) === 0) { btns[i].click(); return true; }
    }
    throw new Error('chip 列表里没有以「' + prefix + '」开头的分组');
  }

  // filterPass 验证"分组筛选之后状态的表现不变"：筛出只有离线那一张的那一组，
  // 再量一遍它的环/模糊/覆盖层 —— 筛选只是显隐，不该把状态样式一起改掉。
  function filterPass() {
    waitFor('分组 chip 就绪', function () {
      var box = node('group-filter');
      return !!box && box.querySelectorAll('button').length >= 2;
    }, 15000);
    clickChip('香港');
    return sleep(500).then(function () {
      R.filtered.chip = (function () {
        var b = node('group-filter').querySelector('button.active');
        return b ? b.textContent : '';
      })();
      R.filtered.visible = cards().filter(function (c) { return !c.hidden; }).map(function (c) {
        var n = c.querySelector('.card-name');
        return n ? n.textContent : '?';
      });
      var off = document.querySelector('.card[data-status="offline"]');
      R.filtered.offline = observe(off);
      clickChip('全部');
      return sleep(300);
    });
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
    var routes = ['#/', '#/n/' + CFG.nodeID, '#/settings/nodes', '#/settings/alert'];
    var out = [];
    return routes.reduce(function (chain, hash) {
      return chain.then(function () {
        var before = R.errs.length;
        window.location.hash = hash;
        return sleep(1200).then(function () {
          out.push({ hash: hash, errs: R.errs.length - before, view: currentView() });
          return true;
        });
      });
    }, Promise.resolve()).then(function () { R.routes = out; return true; });
  }

  function measure() {
    R.cards = cards().map(observe);
    var doc = document.documentElement;
    R.overflowX = doc.scrollWidth - doc.clientWidth;
    var first = document.querySelector('.card');
    R.narrow = {
      viewport: doc.clientWidth,
      cardWidth: first ? Math.round(first.getBoundingClientRect().width) : 0
    };
    return true;
  }

  function run() {
    return waitFor('页面脚本就绪', function () { return !!node('view-login'); }, 30000)
      .then(login)
      .then(home)
      .then(serverTZ)
      .then(measure)
      .then(function () {
        if (CFG.scenario === 'state') return filterPass();
        return true;
      })
      .then(function () {
        if (CFG.scenario === 'state') {
          R.cards = cards().map(observe);   // 筛回「全部」之后再量一遍（下面用它对比）
          return clickBlurred();
        }
        return true;
      })
      .then(function () {
        if (CFG.scenario === 'dark') return true;   // 深色只看样式，不点
        return routePass();
      });
  }

  // shot 模式（人工截图用）：把首页开到四种状态齐备就停住，不回传结果。
  function shot() {
    return login().then(home).then(function () {
      document.title = 'SHOT-READY:cardstate';
      return true;
    });
  }

  function finish() {
    return rawFetch('/__result', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(R)
    }).then(function () { document.title = 'DUMPMARK:cardstate'; });
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
