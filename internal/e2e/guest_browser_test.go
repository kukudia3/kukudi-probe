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
// 四个场景各跑一次 Chrome：
//  1. TestGuestReadOnlyInRealBrowser：开关**打开**时，未登录的浏览器能看首页与
//     详情页（价格可见），但 DOM 里**不存在**「新增节点/设置/退出」，
//     也**不存在**本机地址/来源 IP 那两行、以及探测目标的地址；直接 fetch 写接口
//     拿到 401；随后在同一页面里登录，确认一切照旧（三个入口回来、IP 行回来、
//     探测目标卡片上重新显示地址、能进设置页），并把四条常用路由各走一遍记录 JS 报错。
//  2. TestGuestSwitchOffKeepsLoginPage：开关**关**着时（默认），未登录只看到登录页
//     —— 与这个功能加进来之前完全一样。
//  3. TestGuestLogoutLeavesNoPrivateValues：登录 → 把详情页/设置页/Token 弹窗都开过
//     一遍 → **退出登录** → 页面文本里不许再有任何 IP（DOM 里的私有值要清掉，
//     不是靠 CSS 藏起来）。
//  4. TestTokenDialogCopyButtonsInRealBrowser：创建成功对话框里的两个复制按钮各
//     复制各的，命令那个复制到的是**完整命令**（与命令元素的 textContent 逐字符相同）。
//
// 三条"活着"的用例共用 startGuestBrowserFixture 的同一份现场：现场一旦各搭各的，
// "访客看不到的东西"就可能在另一条用例里被悄悄搭进去，而那条用例还是绿的。

// guestPrivateValues 是服务端无论如何都不该下发给访客的**值**（见下面 Seed 的部分）。
// 断言"页面文本里不出现这些串"是最贴近用户感受的一条：它不关心实现怎么脱敏，
// 只关心"别人打开页面能不能看见这台机器的地址"。
var guestPrivateValues = []string{"203.0.113.9", "10.0.0.5", "fd00::5"}

// guestPingHosts 是 fixture 配的三个探测目标的**地址**。第三个刻意不写名字
// （label 留空）：存储层会用 host 兜底填 label，于是"公开字段 label 的值就是地址"
// —— 这正是审计里漏掉的那条路径（见 internal/server/guest.go 的 guestTargetLabel）。
//
// 它们同样一个都不许出现在访客页面上：详情页的探测目标卡片与延迟图的名字都取自
// label，访客那一份的派生 label 已被抹成空串，卡片上显示的是「目标 #id」。
var guestPingHosts = []string{"1.1.1.1", "nas.home.lan", "9.9.9.9"}

// guestAllPrivateValues 是"页面上一个都不许出现"的全集（节点地址 + 探测目标地址）。
func guestAllPrivateValues() []string {
	return append(append([]string{}, guestPrivateValues...), guestPingHosts...)
}

// seedGuestPingTargets 配三个探测目标（第三个名字留空）。
//
// 为什么必须配：不配的话页面上根本不会出现这些地址，"访客页面上没有它们"这条断言
// 在任何实现下都成立 —— 那就成了一条空断言。
func seedGuestPingTargets(t *testing.T, br *browser) {
	t.Helper()
	status, body := br.do(http.MethodPut, "/api/v1/settings/ping", map[string]any{
		"interval_sec": 60,
		"targets": []map[string]any{
			{"label": "Cloudflare", "type": "tcp", "host": guestPingHosts[0], "port": 443, "enabled": true},
			{"label": "内网 NAS", "type": "icmp", "host": guestPingHosts[1], "port": 0, "enabled": true},
			{"label": "", "type": "icmp", "host": guestPingHosts[2], "port": 0, "enabled": true},
		},
	}, true)
	if status != http.StatusOK {
		t.Fatalf("配置探测目标失败: %d %v", status, body)
	}
}

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
	HomeIPv4       []string `json:"homeIPv4"`
	HomeLoginView  bool     `json:"homeLoginView"`

	DetailOpened  bool     `json:"detailOpened"`
	DetailEntries []string `json:"detailEntries"`
	DetailNetwork []string `json:"detailNetwork"`
	// DetailTargets 是详情页「延迟」卡上那一排探测目标卡片的文字：
	// 访客看到的应当是「目标 #id」（派生 label 被服务端抹成空串之后的回落），
	// 而不是地址。
	DetailTargets []string `json:"detailTargets"`
	// DetailIPv4 是详情页文本里所有"看着像 IPv4"的串：访客页面上一个都不该有
	// （只查我们塞进去的那几个值的话，"地址换了个字段漏出来"就查不到了）。
	DetailIPv4 []string `json:"detailIPv4"`
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
	AfterLoginEntries []string `json:"afterLoginEntries"`
	AfterLoginDetail  []string `json:"afterLoginDetail"`
	AfterLoginNetwork []string `json:"afterLoginNetwork"`
	AfterLoginStats   []string `json:"afterLoginStats"`
	AfterLoginLeaks   []string `json:"afterLoginLeaks"`
	// AfterLoginTargets 是登录后详情页上探测目标卡片的名字：管理员看到的应当
	// **仍然是地址**（存储层对空名称目标的兜底是管理员侧的既有行为，
	// 抹派生 label 只该发生在访客出口）。
	AfterLoginTargets  []string `json:"afterLoginTargets"`
	AfterLoginLoginBar bool     `json:"afterLoginLoginBar"`
	SettingsOpened     bool     `json:"settingsOpened"`
	SettingsNodes      int      `json:"settingsNodes"`
	GuestSwitchChecked bool     `json:"guestSwitchChecked"`

	Routes []guestRouteResult `json:"routes"`
	Debug  any                `json:"debug"`

	// ---- 退出登录那一遍（scenario=logout）----
	//
	// 断言的是"退出之后 DOM 里还剩什么"：这一屏在退出前是**到处都有私有值**的
	// （详情页的两个地址、探测目标卡片上的地址、设置页的审计表与服务端信息、
	// Token 弹窗里的命令），退出后必须一处都不剩。
	LogoutBeforeLeaks     []string `json:"logoutBeforeLeaks"`
	LogoutBeforeTargets   string   `json:"logoutBeforeTargets"`
	LogoutOpenedToken     bool     `json:"logoutOpenedToken"`
	LogoutCmdLen          int      `json:"logoutCmdLen"`
	LogoutView            string   `json:"logoutView"`
	LogoutText            string   `json:"logoutText"`
	LogoutLeaks           []string `json:"logoutLeaks"`
	LogoutIPv4            []string `json:"logoutIPv4"`
	LogoutDetailDrained   bool     `json:"logoutDetailDrained"`
	LogoutTargetsDrained  bool     `json:"logoutTargetsDrained"`
	LogoutTokenDrained    bool     `json:"logoutTokenDrained"`
	LogoutSettingsDrained bool     `json:"logoutSettingsDrained"`

	// ---- 复制按钮那一遍（scenario=copy）----
	CmdBtnPresent   bool   `json:"cmdBtnPresent"`
	TokenBtnPresent bool   `json:"tokenBtnPresent"`
	CmdText         string `json:"cmdText"`
	CmdTextLen      int    `json:"cmdTextLen"`
	CmdCopied       string `json:"cmdCopied"`
	CmdCopiedLen    int    `json:"cmdCopiedLen"`
	TokenValue      string `json:"tokenValue"`
	TokenCopied     string `json:"tokenCopied"`
	// CmdScrolls 是"命令块确实比可视区域宽"的证据（否则"没被裁切"这条断言
	// 测的是一个不会发生的问题）。
	CmdScrolls bool `json:"cmdScrolls"`
	// ClipboardRead 是旁证：真剪贴板能读就读一次（读不到记原因，不做断言）。
	ClipboardRead    string `json:"clipboardRead"`
	ClipboardReadErr string `json:"clipboardReadErr"`
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
	// 三个探测目标（第三个**不写名字**）：存储层会用 host 兜底填 label，
	// 于是"公开字段 label 的值就是地址"。它是这次审计漏掉的那条路径，
	// 必须出现在现场里 —— 否则"访客页面上看不到地址"这条断言是空的。
	seedGuestPingTargets(t, br)

	cfg := tzHarnessConfig{
		NodeID: nodeID, NodeName: "guest-01",
		User: "admin", Pass: "a-very-good-password",
		Scenario: "guest",
		// 私有值随配置一起注入脚本：一个都不该在页面上出现，脚本要拿它去搜。
		Private: guestAllPrivateValues(),
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
	if len(res.HomeIPv4) != 0 {
		t.Errorf("访客首页文本里出现了看着像 IPv4 的串 %v（页面文本：%q）",
			res.HomeIPv4, res.HomeCardText)
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
	if len(res.DetailIPv4) != 0 {
		t.Errorf("访客详情页文本里出现了看着像 IPv4 的串 %v —— 地址必须一个都不露", res.DetailIPv4)
	}
	// 探测目标卡片：**名字来自 label，而空名称目标的 label 在服务端已被抹成空串**，
	// 所以卡片上应当是「目标 #id」，不是地址。名字留空的目标恰好是 default 用法
	// （前端的 placeholder 就写着「留空则显示地址」）。
	if len(res.DetailTargets) != len(guestPingHosts) {
		t.Fatalf("访客详情页应当列出 %d 个探测目标卡片，实际 %v", len(guestPingHosts), res.DetailTargets)
	}
	for _, name := range res.DetailTargets {
		for _, host := range guestPingHosts {
			if strings.Contains(name, host) {
				t.Errorf("探测目标卡片上出现了地址 %q（卡片名 %q）—— 派生 label 应当在服务端被抹掉", host, name)
			}
		}
	}
	if !strings.Contains(strings.Join(res.DetailTargets, "|"), "目标 #") {
		t.Errorf("空名称目标在访客页面上应当显示成「目标 #id」，实际卡片名 = %v", res.DetailTargets)
	}
	t.Logf("访客详情页的探测目标卡片 = %v", res.DetailTargets)
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
	// 派生 label 只抹在**访客出口**：管理员看到的仍然是他在配置里"没填名字"
	// 所对应的那个地址（存储层兜底的结果）。这条断言把"别去动存储层"钉住。
	if got := strings.Join(res.AfterLoginTargets, "|"); !strings.Contains(got, guestPingHosts[2]) {
		t.Errorf("管理员详情页的探测目标卡片上应当能看到地址 %q（存储层对空名称目标的兜底），实际 %v",
			guestPingHosts[2], res.AfterLoginTargets)
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

// head 取字符串开头最多 n 个**字节**（只用于日志与报错信息：够看清是什么就行）。
func head(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// tailRunes 取字符串末尾最多 n 个**字符**（回报里要贴"后 60 字符"，按字符切才不会
// 把多字节字符切坏）。
func tailRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[len(runes)-n:])
}

// startGuestBrowserFixture 是"访客/管理员浏览器用例"共用的现场：
// 一台地址齐全、价格齐全的机器 + 三个探测目标（第三个名字留空）+ 打开的访客开关。
//
// 为什么把现场抽出来：下面三条用例（看到什么 / 退出后还剩什么 / 复制按钮）
// 观察的是三件不同的事，但**必须站在同一个现场上** —— 现场一旦各搭各的，
// "访客看不到的东西"就可能在另一条用例里被悄悄搭进去，而那条用例还是绿的。
func startGuestBrowserFixture(t *testing.T) (*harness, int64) {
	t.Helper()
	logs := &captureHandler{}
	h := startServerAt(t, "127.0.0.1:0", filepath.Join(t.TempDir(), "probe.db"), time.UTC, slog.New(logs), nil)
	br := newBrowser(t, "http://"+h.addr)
	setupAdmin(t, br, waitSetupCode(t, logs))

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
	seedGuestPingTargets(t, br)

	if status, body := br.do(http.MethodPut, "/api/v1/settings/guest", map[string]any{"enabled": true}, true); status != http.StatusOK {
		t.Fatalf("打开访客开关失败: %d %v", status, body)
	}
	return h, nodeID
}

// TestGuestLogoutLeavesNoPrivateValues 钉住必修 6：
// 退出登录之后，DOM 里不能再留着上一位登录者的私有值。
//
// 为什么只有真浏览器说得清：这些值不是"接口还给了没给"，而是"**元素还在文档里**"。
// resetHome 清了首页卡片与服务器列表，但详情页的两行地址、探测目标卡片、
// 设置页的审计表 / 服务端信息 / Telegram 输入框、Token 弹窗里的命令都没清 ——
// 页面文本里读得到，而这个项目对"看不见但还在"的态度是明确的：摘掉，不是藏起来。
//
// 断言同时用两种口径：
//   - 我们塞进去的那些**具体值**（能指名道姓地说出漏了哪个地址）；
//   - 任何"看着像 IPv4"的串（防止地址换了个字段漏出来）。
func TestGuestLogoutLeavesNoPrivateValues(t *testing.T) {
	chrome := findChrome()
	if chrome == "" {
		t.Skip("找不到 Chrome：设置 PROBE_CHROME 或把它装到默认位置后这条用例会自动跑起来")
	}
	h, nodeID := startGuestBrowserFixture(t)

	cfg := tzHarnessConfig{
		NodeID: nodeID, NodeName: "guest-01",
		User: "admin", Pass: "a-very-good-password",
		Scenario: "logout",
		Private:  guestAllPrivateValues(),
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

	// 现场自检：退出**之前**页面上必须到处是私有值。
	// 少了这一条，"退出后没有"可能只是因为现场本来就没有。
	if len(res.LogoutBeforeLeaks) == 0 {
		t.Fatalf("退出之前页面上就看不到任何私有值 %v —— 这条用例会退化成空断言"+
			"（详情页文本：%q）", guestAllPrivateValues(), res.LogoutBeforeTargets)
	}
	for _, host := range guestPingHosts {
		// 只有**名字留空**的那个目标会在页面上显示成地址（存储层用 host 兜底填了
		// label）；另外两个有用户起的名字，它们的地址本来就不该出现在页面上。
		want := host == guestPingHosts[2]
		if got := strings.Contains(res.LogoutBeforeTargets, host); got != want {
			t.Fatalf("退出之前详情页的探测目标卡片里，地址 %q 的出现情况 = %v，期望 %v（卡片文本 %q）",
				host, got, want, res.LogoutBeforeTargets)
		}
	}
	if !res.LogoutOpenedToken || res.LogoutCmdLen == 0 {
		t.Fatalf("退出之前应当打开过 Token 弹窗并看到安装命令（opened=%v len=%d）",
			res.LogoutOpenedToken, res.LogoutCmdLen)
	}

	// 退出之后：三种口径都必须干净。
	if len(res.LogoutLeaks) != 0 {
		t.Errorf("退出登录之后页面上还能读到私有值 %v（停在 %s）\n页面文本：%q",
			res.LogoutLeaks, res.LogoutView, res.LogoutText)
	}
	if len(res.LogoutIPv4) != 0 {
		t.Errorf("退出登录之后页面上还有看着像 IPv4 的串 %v —— DOM 里的私有值必须清掉，"+
			"而不是靠 CSS 藏起来（停在 %s）", res.LogoutIPv4, res.LogoutView)
	}
	if !res.LogoutDetailDrained {
		t.Error("退出之后详情页的信息卡还留着内容（「本机地址 / 来源 IP」那两行在 clearDetailPanels 里）")
	}
	if !res.LogoutTargetsDrained {
		t.Error("退出之后详情页的探测目标卡片还在（名字可能正是探测目标的地址）")
	}
	if !res.LogoutTokenDrained {
		t.Error("退出之后 Token 弹窗里的 Token 还在")
	}
	if !res.LogoutSettingsDrained {
		t.Error("退出之后设置页的容器（服务端信息 / 审计表 / 探测目标）还留着内容")
	}
}

// TestTokenDialogCopyButtonsInRealBrowser 是真浏览器里的"两个复制按钮"验收。
//
// 用户报的是：Token 旁边的「复制」只复制 Token，而他真正要复制的是下面那条
// 安装命令（命令块可横向滚动，手动选中容易漏字符）。
//
// 剪贴板怎么读：**拦截 navigator.clipboard.writeText**，把应用交给它的字符串
// 原样记下来。为什么不用 navigator.clipboard.readText()：无头 Chrome 里剪贴板
// 权限与文档焦点都不保证（readText/writeText 都可能 reject），那样这条断言就变成
// 在测浏览器而不是在测我们的代码；而"点这个按钮时应用把什么字符串交给了剪贴板"
// 才是这段代码要保证的契约。真剪贴板仍然试读一次，结果只当旁证（读不到记原因）。
func TestTokenDialogCopyButtonsInRealBrowser(t *testing.T) {
	chrome := findChrome()
	if chrome == "" {
		t.Skip("找不到 Chrome：设置 PROBE_CHROME 或把它装到默认位置后这条用例会自动跑起来")
	}
	h, nodeID := startGuestBrowserFixture(t)

	cfg := tzHarnessConfig{
		NodeID: nodeID, NodeName: "guest-01",
		User: "admin", Pass: "a-very-good-password",
		Scenario: "copy",
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

	// 两个按钮都在（Token 那个不许被删掉）。
	if !res.CmdBtnPresent {
		t.Fatal("Token 对话框里没有命令块的复制按钮（id=token-cmd-copy）")
	}
	if !res.TokenBtnPresent {
		t.Fatal("Token 自己的复制按钮不见了：两个都有用，别删掉任何一个")
	}
	if !res.CmdScrolls {
		t.Log("注意：这次命令块没有横向溢出（命令比较短），" +
			"「没有被裁切」这条断言的说服力会弱一些")
	}
	if res.CmdText == "" || res.CmdCopied == "" {
		t.Fatalf("命令的复制按钮没有把任何东西交给剪贴板（命令文本长度 %d，复制到 %d 个字符串）",
			res.CmdTextLen, len(res.CmdCopied))
	}

	// ---- 命令按钮复制到的东西 ----
	if !strings.HasPrefix(res.CmdCopied, "curl -fsSL ") {
		t.Errorf("复制到的字符串应当以 `curl -fsSL ` 开头，实际前 80 字符：%q", head(res.CmdCopied, 80))
	}
	if !strings.Contains(res.CmdCopied, "--token pba_") {
		t.Errorf("复制到的命令里必须有 `--token pba_…`，实际：%q", head(res.CmdCopied, 200))
	}
	if res.TokenValue == "" || !strings.Contains(res.CmdCopied, res.TokenValue) {
		t.Errorf("复制到的命令里必须带着这个 Token（%q），实际：%q",
			res.TokenValue, head(res.CmdCopied, 200))
	}
	// 长度与命令元素的 textContent **逐字符相同**：这一条直接钉住"没有被裁切"。
	if res.CmdCopiedLen != res.CmdTextLen {
		t.Errorf("复制到的字符串长度 %d 与命令元素的 textContent 长度 %d 不一致 —— "+
			"说明取的是别的东西（可视区域/选区）", res.CmdCopiedLen, res.CmdTextLen)
	}
	if res.CmdCopied != res.CmdText {
		t.Errorf("复制到的字符串必须与命令元素的 textContent **完全一致**。\n"+
			"复制到的（%d 字符）：%q\n命令元素里的（%d 字符）：%q",
			len(res.CmdCopied), res.CmdCopied, res.CmdTextLen, res.CmdText)
	}
	// 不含换行装饰/省略号：必须是一条能直接在 VPS 上粘贴执行的命令。
	if strings.Contains(res.CmdCopied, "…") {
		t.Errorf("复制到的命令里有省略号：%q", res.CmdCopied)
	}
	lines := strings.Split(res.CmdCopied, "\n")
	if len(lines) != 4 {
		t.Errorf("命令应当是 4 行（3 个续行符），实际 %d 行：%q", len(lines), lines)
	} else {
		for i, line := range lines[:3] {
			if !strings.HasSuffix(line, `\`) {
				t.Errorf("命令第 %d 行应当以续行符 \\ 结尾，实际 %q", i+1, line)
			}
		}
	}
	if !strings.HasPrefix(lines[len(lines)-1], "  --token ") {
		t.Errorf("命令最后一行应当单独是 `  --token <token>`，实际 %q", lines[len(lines)-1])
	}
	// 粘贴执行的命令里不能有回车（Windows 换行会让 shell 把它当成命令的一部分）。
	if strings.Contains(res.CmdCopied, "\r") {
		t.Errorf("复制到的命令里带了 \\r：%q", res.CmdCopied)
	}

	// ---- Token 那个按钮仍然只复制 Token ----
	if res.TokenCopied != res.TokenValue {
		t.Errorf("Token 的复制按钮应当复制 Token 本身（%q），实际 %q",
			res.TokenValue, res.TokenCopied)
	}

	// 把复制到的命令原样贴出来（回报里要用）。
	t.Logf("复制到的安装命令：前 80 字符 = %q；后 60 字符 = %q；总长度 = %d 字符",
		head(res.CmdCopied, 80), tailRunes(res.CmdCopied, 60), res.CmdCopiedLen)
	if res.ClipboardReadErr != "" {
		t.Logf("真剪贴板试读没成功（只当旁证，不做断言）：%s", res.ClipboardReadErr)
	} else {
		t.Logf("真剪贴板读回来的一致 = %v", res.ClipboardRead == res.CmdCopied)
	}
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
  var PRIVATE = (CFG.private || []).filter(function (s) { return !!s; });
  var R = {
    errs: [], fatal: '', steps: [], adminEntries: [], homeLeaks: [], homeCardTitles: [],
    homeCardText: '', detailRows: [], detailNetwork: [], detailStats: [], detailLeaks: [],
    detailText: '', afterLoginEntries: [], afterLoginNetwork: [], afterLoginStats: [],
    afterLoginLeaks: [], routes: []
  };
  window.__GUESTRESULT = R;

  // copiedLog 记下"应用交给剪贴板的那串字符"。
  //
  // 为什么用拦截而不是 navigator.clipboard.readText()：无头 Chrome 里的剪贴板
  // 权限与文档焦点都不保证（readText/writeText 都可能 reject），那样这条断言就变成
  // 在测浏览器而不是在测我们的代码。这里断言的契约是"点这个按钮时，应用把**什么
  // 字符串**交给了 navigator.clipboard.writeText"—— 它才是这段代码要保证的东西。
  // 真剪贴板在 copy 场景里另外试读一次，结果只当旁证（读不到就记原因，不断言）。
  var copiedLog = [];
  function stubClipboardWrite() {
    var real = navigator.clipboard;
    if (!real || typeof real.writeText !== 'function') return false;
    try {
      Object.defineProperty(real, 'writeText', {
        configurable: true,
        value: function (text) { copiedLog.push(String(text)); return Promise.resolve(); }
      });
      return true;
    } catch (e) { return false; }
  }
  stubClipboardWrite();

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
  // ipv4s 找出文本里所有"看着像 IPv4"的串（四段、每段 0-255）。
  //
  // 为什么除了"我们塞进去的那几个值"还要查这个：只查已知值的话，
  // "地址换了个字段漏出来"就永远查不到。这条不关心实现，
  // 只关心"退出之后页面上还有没有地址"。
  function ipv4s(text) {
    var out = [];
    var re = /\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b/g;
    var m;
    while ((m = re.exec(text)) !== null) {
      var ok = true;
      m[0].split('.').forEach(function (p) { if (parseInt(p, 10) > 255) ok = false; });
      if (ok && out.indexOf(m[0]) < 0) out.push(m[0]);
    }
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
  // latTargetNames 取延迟卡上每个探测目标卡片的名字（卡片是 app.js 生成的）。
  function latTargetNames() {
    var box = node('lat-targets');
    if (!box) return [];
    var out = [];
    Array.prototype.forEach.call(box.querySelectorAll('.lat-card-name'), function (n) {
      out.push(n.textContent);
    });
    return out;
  }
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
        R.homeIPv4 = ipv4s(pageText());
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
        R.detailTargets = latTargetNames();
        R.detailText = dlText('info-network') + ' ' + dlText('info-hardware') + ' ' + dlText('info-system') + ' ' + dlText('info-traffic');
        R.detailLeaks = leaks(pageText());
        R.detailIPv4 = ipv4s(pageText());
        R.detailHasChart = shown('charts-resources') && !!node('chart-cpu');
        R.steps.push('访客详情页观测完成');

        // 访客把地址改成设置页：必须被送回首页（设置接口在服务端就是 401）。
        window.location.hash = '#/settings/nodes';
        return sleep(900).then(function () { R.settingsRedirect = currentView(); });
      });
  }

  // ---- 登录之后那一遍 -----------------------------------------------------

  function login() {
    // 访客模式下唯一的登录入口就在那条只读提示里；管理员模式下
    // （copy 场景）直接在地址栏里进登录页 —— 两条路最终都落到 #/login。
    if (node('btn-login-entry')) node('btn-login-entry').click();
    else window.location.hash = '#/login';
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
        R.afterLoginTargets = latTargetNames();
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

  // ---- 退出登录那一遍 -----------------------------------------------------

  // 目的：退出之后 DOM 里不能再留着上一位登录者的私有值。
  //
  // 做法：先把详情页（本机地址/来源 IP/探测目标卡片）、设置页（服务器列表、
  // 探测目标、审计表、服务端信息）与 Token 弹窗（Token + 带 Token 的安装命令）
  // 都开过一遍，攒出一屏"到处是私有值"的 DOM，然后点退出，再看页面文本里还剩什么。
  // 退出前先把 leaks 记一笔：没有它，"退出后没有"这条断言可能是空的
  // （现场本来就没有地址，任何实现都能过）。
  function logoutPass() {
    return login().then(function () {
      window.location.hash = '#/n/' + CFG.nodeID;
      return waitFor('详情页打开', function () {
        return shown('view-detail') && node('detail-name') && node('detail-name').textContent === CFG.nodeName;
      }, 30000);
    }).then(function () { return sleep(1200); })
      .then(function () {
        // 现场自检：退出之前页面上**必须**有私有值（两个地址 + 探测目标的地址）。
        R.logoutBeforeLeaks = leaks(pageText());
        R.logoutBeforeTargets = node('lat-targets') ? node('lat-targets').textContent : '';
        // 设置页：把审计表、服务端信息、探测目标编辑器都填上。
        window.location.hash = '#/settings/ping';
        return waitFor('设置页打开', function () {
          return shown('view-settings') && node('ping-list') && node('ping-list').children.length >= 1;
        }, 30000);
      })
      .then(function () { return sleep(1000); })
      .then(function () {
        // Token 弹窗：Token 与安装命令（命令里也带着 Token）。
        window.location.hash = '#/n/' + CFG.nodeID;
        return waitFor('详情页再次打开', function () {
          return shown('view-detail') && !!node('detail-token');
        }, 30000);
      })
      .then(function () {
        node('detail-token').click();
        return waitFor('二次确认出现', function () {
          return node('dlg-confirm') && node('dlg-confirm').open;
        }, 15000);
      })
      .then(function () {
        node('confirm-ok').click();
        return waitFor('Token 对话框出现', function () {
          return node('dlg-token') && node('dlg-token').open &&
            node('token-value') && node('token-value').textContent.length > 0 &&
            node('token-cmd') && node('token-cmd').textContent.length > 0;
        }, 20000);
      })
      .then(function () {
        R.logoutOpenedToken = true;
        R.logoutCmdLen = node('token-cmd').textContent.length;
        node('dlg-token').close();
        // 退出登录：这是这一步唯一真正要驱动的事情。
        node('btn-logout').click();
        return waitFor('退出后不再是管理员', function () {
          return !node('btn-logout');
        }, 30000);
      })
      .then(function () { return sleep(1500); })
      .then(function () {
        R.logoutView = currentView();
        var text = pageText();
        R.logoutText = text;
        R.logoutLeaks = leaks(text);
        R.logoutIPv4 = ipv4s(text);
        R.logoutDetailDrained = dlText('info-network') === '' && dlText('info-hardware') === '';
        R.logoutTargetsDrained = !node('lat-targets') || node('lat-targets').textContent === '';
        R.logoutTokenDrained = !node('token-value') || node('token-value').textContent === '';
        R.logoutSettingsDrained = (!node('server-info') || node('server-info').textContent === '') &&
          (!node('audit-body') || node('audit-body').textContent === '') &&
          (!node('ping-list') || node('ping-list').textContent === '');
        R.steps.push('退出登录后的 DOM 观测完成');
        return true;
      });
  }

  // ---- 复制按钮那一遍 -----------------------------------------------------

  // 目的：创建成功那个对话框里，**命令块自己的**复制按钮复制的是完整命令
  // （不是 Token、也不是被裁掉的那一段）。
  //
  // 走真实路径：点「新增节点」→ 填名字 → 提交 → 对话框自动弹出。
  function copyPass() {
    return login().then(function () { return sleep(600); })
      .then(function () {
        node('btn-add').click();
        return waitFor('新增节点对话框打开', function () {
          return node('dlg-node') && node('dlg-node').open && !!node('node-name');
        }, 15000);
      })
      .then(function () {
        node('node-name').value = 'copy-01';
        node('node-submit').click();
        return waitFor('Token 对话框出现', function () {
          return node('dlg-token') && node('dlg-token').open &&
            node('token-value') && node('token-value').textContent.length > 0 &&
            node('token-cmd') && node('token-cmd').textContent.length > 0;
        }, 25000);
      })
      .then(function () { return sleep(300); })
      .then(function () {
        R.cmdBtnPresent = !!node('token-cmd-copy');
        R.tokenBtnPresent = !!node('token-copy');
        R.cmdText = node('token-cmd').textContent;
        R.cmdTextLen = R.cmdText.length;
        // 命令块确实比可视区域宽 —— 这正是"用户手动选中容易漏字符"的原因，
        // 也是"必须按 textContent 取"的理由。
        R.cmdScrolls = node('token-cmd').scrollWidth > node('token-cmd').clientWidth;
        R.tokenValue = node('token-value').textContent;
        if (!R.cmdBtnPresent || !R.tokenBtnPresent) return true;
        node('token-cmd-copy').click();
        return sleep(200);
      })
      .then(function () {
        R.cmdCopied = copiedLog.length ? copiedLog[copiedLog.length - 1] : '';
        R.cmdCopiedLen = R.cmdCopied.length;
        node('token-copy').click();
        return sleep(200);
      })
      .then(function () {
        R.tokenCopied = copiedLog.length ? copiedLog[copiedLog.length - 1] : '';
        R.steps.push('两个复制按钮各点了一次');
        // 旁证：真剪贴板能读就读一次（无头环境里常常读不到，读不到只记原因）。
        if (!navigator.clipboard || typeof navigator.clipboard.readText !== 'function') {
          R.clipboardReadErr = 'navigator.clipboard.readText 不可用';
          return true;
        }
        return navigator.clipboard.readText().then(function (text) { R.clipboardRead = text; },
          function (err) { R.clipboardReadErr = String(err && err.message ? err.message : err); });
      })
      .then(function () { return true; });
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
    if (CFG.scenario === 'logout') {
      return settle().then(logoutPass);
    }
    if (CFG.scenario === 'copy') {
      return settle().then(copyPass);
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
    } else if (mode === 'guest-latency') {
      // 访客详情页的「延迟」卡：要看清探测目标卡片上写的是「目标 #id」而不是地址。
      // guest-detail 那张的取景在页面顶部，卡片在折叠线以下，所以单独截一张。
      chain = waitFor('访客详情页就绪', function () {
        return shown('view-detail') && node('detail-name') && node('detail-name').textContent === CFG.nodeName;
      }, 30000).then(function () {
        return waitFor('探测目标卡片出现', function () {
          return node('lat-targets') && node('lat-targets').querySelectorAll('.lat-card-name').length >= 1;
        }, 20000);
      }).then(function () { return sleep(900); })
        .then(function () {
          if (node('charts-latency') && node('charts-latency').scrollIntoView) {
            node('charts-latency').scrollIntoView({ block: 'center' });
          }
          return sleep(400);
        });
    } else if (mode === 'logout-done') {
      // 退出登录**之后**的那一屏：详情页/设置页都开过一遍，然后退出。
      // 人工核对的是"页面上还看不看得见上一位登录者的东西"。
      chain = login().then(function () { return sleep(600); })
        .then(function () {
          window.location.hash = '#/n/' + CFG.nodeID;
          return waitFor('详情页就绪', function () {
            return shown('view-detail') && node('detail-name') && node('detail-name').textContent === CFG.nodeName;
          }, 30000);
        })
        .then(function () { return sleep(900); })
        .then(function () {
          window.location.hash = '#/settings/ping';
          return waitFor('设置页就绪', function () {
            return shown('view-settings') && node('ping-list') && node('ping-list').children.length >= 1;
          }, 30000);
        })
        .then(function () { return sleep(700); })
        .then(function () {
          window.location.hash = '#/n/' + CFG.nodeID;
          return waitFor('详情页再次就绪', function () { return shown('view-detail'); }, 30000);
        })
        .then(function () { return sleep(700); })
        .then(function () {
          node('detail-token').click();
          return waitFor('二次确认出现', function () { return node('dlg-confirm') && node('dlg-confirm').open; }, 15000);
        })
        .then(function () {
          node('confirm-ok').click();
          return waitFor('Token 对话框出现', function () {
            return node('dlg-token') && node('dlg-token').open && node('token-cmd') && node('token-cmd').textContent.length > 0;
          }, 20000);
        })
        .then(function () {
          node('dlg-token').close();
          node('btn-logout').click();
          return waitFor('退出完成', function () { return !node('btn-logout'); }, 30000);
        })
        .then(function () { return sleep(1200); });
    } else if (mode === 'token-dialog') {
      // 创建成功那个对话框：Token 的复制按钮 + 命令块自己的复制按钮 + 命令块。
      // 走真实路径建一台机器（对话框是提交成功之后自动弹出来的）。
      chain = login().then(function () { return sleep(800); })
        .then(function () {
          node('btn-add').click();
          return waitFor('新增节点对话框打开', function () {
            return node('dlg-node') && node('dlg-node').open && !!node('node-name');
          }, 15000);
        })
        .then(function () {
          // 名字必须按 shot 模式区分：所有截图共用同一个服务端，固定名字的话
          // 第二张就会撞上"节点名称已存在"，对话框根本弹不出来（截图截到的是
          // 新增对话框 + 一行红字）。
          node('node-name').value = 'shot-' + mode + (CFG.theme ? '-' + CFG.theme : '');
          node('node-submit').click();
          return waitFor('Token 对话框出现', function () {
            return node('dlg-token') && node('dlg-token').open &&
              node('token-cmd') && node('token-cmd').textContent.length > 0;
          }, 25000);
        })
        .then(function () { return sleep(600); });
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
