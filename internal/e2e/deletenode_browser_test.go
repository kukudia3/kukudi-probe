package e2e

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 删除节点的真浏览器验收：**删完之后那张卡片/那一行必须自己消失，不用刷新**。
//
// 用户报的 bug："我在小鸡详情页面点了删除按钮之后，主页上还留存，但是刷新之后没有了"。
// 所以这一条的关键断言全都带一个"不用刷新"的前提：
//
//  1. 详情页头部点「删除」→ 弹二次确认 → 确认 → 回首页时那张卡片**已经不在 DOM 里**
//     （并且总览条与分组 chip 的台数跟着变了）；
//  2. 设置页「服务器列表」每一行行尾是「编辑节点」+「删除」两个按钮，点删除 →
//     二次确认 → 那一行消失；
//  3. **别人删的**（Go 侧直接调 DELETE，浏览器停在首页上什么都不做）：SSE 那一帧
//     必须点名说这台没了，卡片自己消失 —— 这条是 SSE 删除名单存在的全部理由
//     （变更集里"没有它"与"它没变"是同一个形状，前端分不开）；
//  4. 全程 `performance.getEntriesByType('navigation').length === 1`：一次整页刷新
//     都没有发生过（"刷新之后就没有了"这句话里的那个刷新）。
//
// 全程没有任何 mock：真服务端 + 注入自检脚本 + 结果 POST 回来（与仓库里其它
// 浏览器用例同一套做法，见 groupfilter_browser_test.go 顶部的说明）。
func TestNodeDeleteInRealBrowser(t *testing.T) {
	chrome := findChrome()
	if chrome == "" {
		t.Skip("找不到 Chrome：设置 PROBE_CHROME 或把它装到默认位置后这条用例会自动跑起来")
	}

	logs := &captureHandler{}
	h := startServerAt(t, "127.0.0.1:0", filepath.Join(t.TempDir(), "probe.db"), time.UTC, slog.New(logs), nil)
	br := newBrowser(t, "http://"+h.addr)
	setupAdmin(t, br, waitSetupCode(t, logs))

	// 四台机器，其中三台会被三种不同的方式删掉：
	//   · us-01   —— 在自己的详情页上点「删除」（用户报的那条路）；
	//   · hk-01   —— 在设置页服务器列表那一行点「删除」（这一轮新加的按钮）；
	//   · hk-02   —— **别人删的**：Go 侧直接 DELETE，浏览器只看不点；
	//   · keep-01 —— 一台都不删（路由回归要有一台活着的机器可开）。
	hk1ID := createNodeWithGroup(t, br, "hk-01", "香港")
	hk2ID := createNodeWithGroup(t, br, "hk-02", "香港")
	usID := createNodeWithGroup(t, br, "us-01", "美国")
	keepID := createNodeWithGroup(t, br, "keep-01", "")

	cfg := tzHarnessConfig{
		NodeID: usID, NodeName: "us-01",
		User: "admin", Pass: "a-very-good-password",
		Scenario: "delete",
		Nodes:    map[string]int64{"hk-01": hk1ID, "hk-02": hk2ID, "us-01": usID, "keep-01": keepID},
	}
	proxy := newHarnessProxy(t, "http://"+h.addr, cfg, deleteHarnessJS)
	gate := newPhaseGate(proxy)
	mock := newMockServer(t, gate)

	// 「别人删的」与「断线期间被删的」两幕都由浏览器报到之后 Go 才动手 ——
	// 两边必须对齐时机（见 deleteHarnessJS 里那两次 /__phase）。
	phasesDone := make(chan string, 1)
	go func() {
		pending := []struct {
			phase string
			id    int64
		}{
			{"remote", hk2ID},
			{"hidden", keepID},
		}
		for _, p := range pending {
			select {
			case name := <-gate.phases:
				if name != p.phase {
					phasesDone <- "期待阶段 " + p.phase + "，收到 " + name
					return
				}
			case <-time.After(300 * time.Second):
				phasesDone <- "没等到浏览器的阶段信号 " + p.phase
				return
			}
			// 这里不能用 br.do：它出错时会 t.Fatalf，而 t.Fatalf 只能从测试 goroutine 调。
			if msg := deleteNodeOverHTTP(br.client, "http://"+h.addr, br.csrf, p.id); msg != "" {
				phasesDone <- msg
				return
			}
		}
		phasesDone <- ""
	}()

	raw := runChromeForResult(t, chrome, mock.URL+"/", proxy.result, 300*time.Second, "1500,1100")

	var res deleteResult
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
	// Chrome 已经跑完了：两个阶段信号要么早就到过，要么永远不会来。
	// 给一小段宽限（信号 → Go 删除 → 浏览器看到结果 → 回传，这条链就差了最后几微秒）。
	var phaseMsg string
	select {
	case phaseMsg = <-phasesDone:
	case <-time.After(60 * time.Second):
		phaseMsg = "等 Go 侧的删除动作超时"
	}
	if phaseMsg != "" {
		t.Fatalf("Go 侧的删除动作没走通：%s", phaseMsg)
	}

	checkHomeBeforeDelete(t, res)
	checkDetailDelete(t, res)
	checkSettingsRowDelete(t, res)
	checkRemoteDelete(t, res)
	checkReconnectDelete(t, res)

	// 全程只该有 1 条 navigation：一次整页刷新都没有发生过。
	// 少了这一条，"卡片消失了"完全可能是"页面被刷新过"—— 而用户报的正是
	// "刷新之后就没有了"（那不算修好）。
	if res.NavigationCount != 1 {
		t.Errorf("全程 navigation 条数 = %d，期望 1（= 一次整页刷新都没发生："+
			"卡片消失必须是 DOM 局部更新，不是刷新出来的）", res.NavigationCount)
	}
	// 浏览器自己发出去了两次 DELETE（详情页一次、设置页行尾一次）——
	// "别人删的"那一幕是 Go 发的，不该算在浏览器头上。
	if res.DeleteCalls != 2 {
		t.Errorf("浏览器自己发出的 DELETE 请求 = %d 次，期望 2（详情页 1 + 设置页行尾 1）", res.DeleteCalls)
	}

	// 路由回归：四条路由各走一遍，每条都要 errs=0、而且要真的切到那个视图。
	wantView := map[string]string{
		"#/":                                   "view-home",
		"#/n/" + strconv.FormatInt(keepID, 10): "view-detail",
		"#/settings/nodes":                     "view-settings:nodes",
		"#/settings/alert":                     "view-settings:alert",
	}
	if len(res.Routes) != len(wantView) {
		t.Fatalf("只走了 %d 条路由，期望 %d 条：%+v", len(res.Routes), len(wantView), res.Routes)
	}
	for _, r := range res.Routes {
		t.Logf("路由 %s → 视图 %s（这一段里有 %d 条 JS 报错）", r.Hash, r.View, r.Errs)
		if r.Errs != 0 {
			t.Errorf("路由 %s 上有 %d 条 JS 报错", r.Hash, r.Errs)
		}
		if want, ok := wantView[r.Hash]; !ok {
			t.Errorf("浏览器走了一条没登记的路由 %s", r.Hash)
		} else if r.View != want {
			t.Errorf("路由 %s 最后停在 %q，期望 %q", r.Hash, r.View, want)
		}
	}
}

// checkHomeBeforeDelete 是删除之前的基线：四张卡片、总览条 0/4、四个 chip。
func checkHomeBeforeDelete(t *testing.T, res deleteResult) {
	t.Helper()
	if got := strings.Join(res.VisibleBefore, ","); got != "hk-01,hk-02,us-01,keep-01" {
		t.Fatalf("首页卡片 = %q，期望四台都在（后面的断言都要跟这个基线比）", got)
	}
	if res.SummaryBefore != "0/4" {
		t.Fatalf("总览条 = %q，期望 0/4", res.SummaryBefore)
	}
	wantChips := []string{"全部 4", "香港 2", "美国 1", "未分组 1"}
	if got := strings.Join(res.ChipsBefore, "|"); got != strings.Join(wantChips, "|") {
		t.Fatalf("分组 chip = %v，期望 %v", res.ChipsBefore, wantChips)
	}
	t.Logf("删除前：卡片 = %v；总览条 = %s；chip = %v", res.VisibleBefore, res.SummaryBefore, res.ChipsBefore)
}

// checkDetailDelete 是用户报的那条路：详情页点「删除」→ 二次确认 → 回首页时卡片已经没了。
func checkDetailDelete(t *testing.T, res deleteResult) {
	t.Helper()
	if !res.DetailDialogOpen {
		t.Fatal("点详情页的「删除」没有弹出二次确认框")
	}
	// 确认框里说的必须是**这一台**（名字取自详情页正在看的那一台）。
	if !strings.Contains(res.DetailDialogTitle, "删除") {
		t.Errorf("二次确认框标题 = %q，应当写明是删除", res.DetailDialogTitle)
	}
	if !strings.Contains(res.DetailDialogText, "us-01") {
		t.Errorf("二次确认框正文 = %q，应当指名道姓写「us-01」", res.DetailDialogText)
	}
	if !strings.Contains(res.DetailDialogWarn, "无法恢复") {
		t.Errorf("二次确认框没有写明后果（%q）", res.DetailDialogWarn)
	}
	if !res.DetailBackHome {
		t.Fatal("删完之后没有回到首页")
	}
	if !res.DetailCardGone {
		t.Errorf("回到首页时 us-01 那张卡片还在 DOM 里 —— 这正是用户报的「主页上还留存」"+
			"（等了 %d 毫秒，全程只刷新过 %d 次页面）", res.DetailDeleteMs, res.NavigationCount-1)
	}
	if res.DetailDeleteMs > 3000 {
		t.Errorf("从点确认到卡片消失用了 %d 毫秒，太久（这条路径不该等 SSE：删完会立刻重新拉一次节点视图）",
			res.DetailDeleteMs)
	}
	if res.DetailStillInGrid {
		t.Error("#grid 里仍然留着 us-01 的卡片元素（卡片必须整个摘掉，不是 hidden）")
	}
	if got := strings.Join(res.VisibleAfterDetail, ","); got != "hk-01,hk-02,keep-01" {
		t.Errorf("删完之后首页可见卡片 = %q，期望 hk-01,hk-02,keep-01", got)
	}
	// 总览条与分组 chip 的台数都要跟着变（服务端汇总 + 按卡片重算的分组）。
	if res.SummaryAfterDetail != "0/3" {
		t.Errorf("删完之后总览条 = %q，期望 0/3（台数要跟着变）", res.SummaryAfterDetail)
	}
	if got := strings.Join(res.ChipsAfterDetail, "|"); got != "全部 3|香港 2|未分组 1" {
		t.Errorf("删完之后分组 chip = %v，期望 [全部 3 香港 2 未分组 1]（美国那一组要整组消失）",
			res.ChipsAfterDetail)
	}
	t.Logf("详情页删除：确认框 = %q / %q / %q；%d 毫秒后卡片消失；总览条 %s → %s；chip = %v",
		res.DetailDialogTitle, res.DetailDialogText, res.DetailDialogWarn,
		res.DetailDeleteMs, res.SummaryBefore, res.SummaryAfterDetail, res.ChipsAfterDetail)
}

// checkSettingsRowDelete 是这一轮新加的那个按钮：行尾「编辑节点」+「删除」，删完那一行消失。
func checkSettingsRowDelete(t *testing.T, res deleteResult) {
	t.Helper()
	if res.RowsBefore != 3 {
		t.Fatalf("设置页服务器列表有 %d 行，期望 3（us-01 已经删了）", res.RowsBefore)
	}
	if len(res.RowButtons) != 3 {
		t.Fatalf("只读到 %d 行的按钮，期望 3 行", len(res.RowButtons))
	}
	for _, row := range res.RowButtons {
		if len(row.Buttons) != 2 {
			t.Errorf("「%s」那一行行尾有 %d 个按钮，期望 2 个（编辑节点 + 删除）：%+v",
				row.Name, len(row.Buttons), row.Buttons)
			continue
		}
		edit, del := row.Buttons[0], row.Buttons[1]
		if edit.Text != "编辑节点" {
			t.Errorf("「%s」那一行第一个按钮 = %q，期望「编辑节点」", row.Name, edit.Text)
		}
		if del.Text != "删除" {
			t.Errorf("「%s」那一行第二个按钮 = %q，期望「删除」（用户要的就是它跟在编辑后面）",
				row.Name, del.Text)
		}
		if edit.Cls != "btn" {
			t.Errorf("「%s」的编辑按钮 class = %q，期望就是 btn", row.Name, edit.Cls)
		}
		// 危险色：与详情页那个删除按钮同一个 .btn.danger（行尾并排两个按钮，
		// 长得一模一样的话点错只是迟早的事）。
		if del.Cls != "btn danger" {
			t.Errorf("「%s」的删除按钮 class = %q，期望「btn danger」", row.Name, del.Cls)
		}
		if del.Color == edit.Color {
			t.Errorf("「%s」的删除按钮颜色（%s）与编辑按钮（%s）一模一样：危险色没生效",
				row.Name, del.Color, edit.Color)
		}
		if del.BorderColor == edit.BorderColor {
			t.Errorf("「%s」的删除按钮边框色（%s）与编辑按钮（%s）一样：危险色没生效",
				row.Name, del.BorderColor, edit.BorderColor)
		}
		// 无障碍：行尾空间小，按钮上只有两个字，全称留给 title / aria-label。
		if !strings.Contains(del.Title, "删除") || !strings.Contains(del.Title, row.Name) {
			t.Errorf("「%s」的删除按钮 title = %q，应当写明删的是哪一台", row.Name, del.Title)
		}
		if !strings.Contains(del.Aria, "删除") || !strings.Contains(del.Aria, row.Name) {
			t.Errorf("「%s」的删除按钮 aria-label = %q，应当写明删的是哪一台", row.Name, del.Aria)
		}
		t.Logf("行 %q：编辑按钮 class=%q color=%s；删除按钮 class=%q color=%s border=%s title=%q aria-label=%q",
			row.Name, edit.Cls, edit.Color, del.Cls, del.Color, del.BorderColor, del.Title, del.Aria)
	}

	if !res.SettingsDialogOpen {
		t.Fatal("点行尾的「删除」没有弹出二次确认框")
	}
	if !strings.Contains(res.SettingsDialogText, "hk-01") {
		t.Errorf("行尾删除的确认框正文 = %q，应当写「hk-01」——"+
			"从设置页删的时候 detail.node 可能是上一次打开的详情页，拿它拼文案就会问错机器",
			res.SettingsDialogText)
	}
	if res.RowsAfter != 2 {
		t.Errorf("删完之后服务器列表有 %d 行，期望 2（那一行要消失）", res.RowsAfter)
	}
	if strings.Contains(strings.Join(res.RowsAfterNames, ","), "hk-01") {
		t.Errorf("删完之后 hk-01 那一行还在：%v", res.RowsAfterNames)
	}
	if res.SettingsDeleteMs > 3000 {
		t.Errorf("从点确认到那一行消失用了 %d 毫秒，太久", res.SettingsDeleteMs)
	}
	if res.NodesError != "" {
		t.Errorf("服务器列表那一栏报了错：%q", res.NodesError)
	}
	t.Logf("设置页删除：确认框 = %q；%d 毫秒后那一行消失；剩余行 = %v",
		res.SettingsDialogText, res.SettingsDeleteMs, res.RowsAfterNames)
}

// checkRemoteDelete 是"别人删的"：浏览器停在首页上不动，卡片必须自己消失（SSE 删除名单）。
func checkRemoteDelete(t *testing.T, res deleteResult) {
	t.Helper()
	if !res.RemoteCardGone {
		t.Errorf("别人删掉 hk-02 之后，浏览器上那张卡片还在（等了 %d 毫秒，%s）——"+
			"SSE 的变更集里「没有它」同时意味着「它没变」，只有服务端点名（payload.deleted）"+
			"前端才能知道它没了；少了它，卡片会一直留到整页刷新（用户报的就是这个）",
			res.RemoteDeleteMs, res.RemoteError)
	}
	if res.RemoteCardGone && res.RemoteDeleteMs > 5000 {
		t.Errorf("卡片消失用了 %d 毫秒：SSE 是 1 Hz，超过 5 秒说明它不是被推送收掉的", res.RemoteDeleteMs)
	}
	if got := strings.Join(res.VisibleAfterRemote, ","); got != "keep-01" {
		t.Errorf("别人删完之后首页可见卡片 = %q，期望只剩 keep-01", got)
	}
	if res.SummaryAfterRemote != "0/1" {
		t.Errorf("别人删完之后总览条 = %q，期望 0/1（汇总始终是全量的，跟着那一帧一起更新）",
			res.SummaryAfterRemote)
	}
	t.Logf("别人删的：卡片自己消失了 = %v（耗时 %d 毫秒，%s）；总览条 %s → %s；chip = %v",
		res.RemoteCardGone, res.RemoteDeleteMs, res.RemoteError,
		res.SummaryAfterDetail, res.SummaryAfterRemote, res.ChipsAfterRemote)
}

// checkReconnectDelete 是"断线期间被删的"：页面切后台时 app.js 会主动断开实时流，
// 这期间被删掉的节点**一次都没被推过** —— 只能靠重连拿到的那份全量快照做差集收掉
// （payload.full 那条路）。少了它，那张卡片会一直留到整页刷新。
func checkReconnectDelete(t *testing.T, res deleteResult) {
	t.Helper()
	if !res.HiddenCardGone {
		t.Errorf("断线期间被删掉的 keep-01（等了 %d 毫秒，%s）卡片还在：重连拿到的全量快照"+
			"（payload.full）必须照它做差集 —— 断线时那个 id 从未出现在任何一帧变更集里，"+
			"变更集的删除名单覆盖不到它", res.HiddenDeleteMs, res.HiddenError)
	}
	if res.HiddenCardGone && res.HiddenDeleteMs > 8000 {
		t.Errorf("重连到卡片消失用了 %d 毫秒，太久（重连的第一帧就是全量快照，不该等这么久）",
			res.HiddenDeleteMs)
	}
	if res.LiveText != "实时" {
		t.Errorf("切回前台之后实时状态 = %q，期望「实时」（app.js 应当自己重连上了）", res.LiveText)
	}
	if got := strings.Join(res.VisibleAfterHidden, ","); got != "" {
		t.Errorf("最后一台被收掉之后首页可见卡片 = %q，期望一张都不剩", got)
	}
	if !res.EmptyShown {
		t.Error("一台机器都没有时应当显示空态（#empty）—— 卡片收干净了，空态要跟上")
	}
	if res.SummaryAfterHidden != "0/0" {
		t.Errorf("最后一台被收掉之后总览条 = %q，期望 0/0", res.SummaryAfterHidden)
	}
	t.Logf("断线期间被删的：卡片自己消失了 = %v（耗时 %d 毫秒，%s）；实时状态 = %q；"+
		"总览条 %s → %s；可见卡片 = %v；空态 = %v",
		res.HiddenCardGone, res.HiddenDeleteMs, res.HiddenError, res.LiveText,
		res.SummaryAfterRemote, res.SummaryAfterHidden, res.VisibleAfterHidden, res.EmptyShown)
}

// deleteNodeOverHTTP 用管理员的 Cookie + CSRF 直接删一台机器（"别人删的"那两幕）。
//
// 返回空串表示成功，否则是一句人话：调用方在 goroutine 里（t.Fatalf 只能从测试
// goroutine 调），所以这里只把结果交回去。
func deleteNodeOverHTTP(client *http.Client, base, csrf string, id int64) string {
	req, err := http.NewRequest(http.MethodDelete,
		base+"/api/v1/nodes/"+strconv.FormatInt(id, 10), nil)
	if err != nil {
		return "构造 DELETE 请求失败: " + err.Error()
	}
	req.Header.Set("X-CSRF-Token", csrf)
	resp, err := client.Do(req)
	if err != nil {
		return "DELETE 请求失败: " + err.Error()
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "DELETE /api/v1/nodes/" + strconv.FormatInt(id, 10) + " 状态码 = " + resp.Status
	}
	return ""
}

// TestGuestHasNoDeleteButtonInRealBrowser 钉住"访客的 DOM 里根本没有删除按钮"。
//
// 要求是**根本不画**，不是 display:none / hidden：藏起来的按钮仍然在文档里，
// 脚本、读屏、以及"以后改代码的人"都看得见它们（与 applyAdminChrome 那条注释同一个
// 理由）。所以这里断言的是 DOM 里一个 `.btn.danger` 都没有、服务器列表一行都没建，
// 而且访客连设置页都进不去（那是服务端的 401，前端不该把人丢过去）。
func TestGuestHasNoDeleteButtonInRealBrowser(t *testing.T) {
	chrome := findChrome()
	if chrome == "" {
		t.Skip("找不到 Chrome：设置 PROBE_CHROME 或把它装到默认位置后这条用例会自动跑起来")
	}

	logs := &captureHandler{}
	h := startServerAt(t, "127.0.0.1:0", filepath.Join(t.TempDir(), "probe.db"), time.UTC, slog.New(logs), nil)
	br := newBrowser(t, "http://"+h.addr)
	setupAdmin(t, br, waitSetupCode(t, logs))

	nodeID := createNodeWithGroup(t, br, "guest-01", "香港")
	status, body := br.do(http.MethodPut, "/api/v1/settings/guest", map[string]any{"enabled": true}, true)
	if status != http.StatusOK {
		t.Fatalf("打开访客开关失败: %d %v", status, body)
	}

	cfg := tzHarnessConfig{NodeID: nodeID, NodeName: "guest-01", Scenario: "guest"}
	proxy := newHarnessProxy(t, "http://"+h.addr, cfg, deleteHarnessJS)
	mock := newMockServer(t, proxy)
	// **不登录**：这一遍要的就是一个没有会话的浏览器（服务端开着访客查看）。
	raw := runChromeForResult(t, chrome, mock.URL+"/", proxy.result, 180*time.Second, "1500,1100")

	var res deleteResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("解析浏览器回传的结果: %v\n原始内容：%s", err, tail(string(raw), 600))
	}
	if res.Fatal != "" {
		t.Fatalf("访客场景没跑完：%s", res.Fatal)
	}
	if len(res.Errs) != 0 {
		t.Fatalf("访客场景里有 %d 条 JS 报错：%v", len(res.Errs), res.Errs)
	}
	t.Logf("访客场景走过的步骤 = %v", res.Steps)

	if !res.GuestBarShown {
		t.Error("访客页面上应当有一条「只读」提示（否则看不出这是只读的）")
	}
	if len(res.DangerButtons) != 0 {
		t.Errorf("访客的 DOM 里有这些节点删除入口：%v —— 要求是**根本不画**，不是藏起来", res.DangerButtons)
	}
	if res.RowDangerButtons != 0 {
		t.Errorf("访客的服务器列表里建了 %d 个行尾的危险色按钮：那一栏本来就要登录，"+
			"行（连同按钮）都不该建出来", res.RowDangerButtons)
	}
	if res.NodesListChildren != 0 {
		t.Errorf("访客的服务器列表里建了 %d 行：那一栏本来就要登录，行都不该建出来", res.NodesListChildren)
	}
	if res.DetailDeleteInDOM {
		t.Error("访客的详情页头部不该有「删除」按钮（点了只会 401）")
	}
	if res.GuestSettingsHash != "#/" {
		t.Errorf("访客访问 #/settings/nodes 之后停在 %q，应当被弹回首页", res.GuestSettingsHash)
	}
	t.Logf("访客：节点删除入口 = %v；行尾危险色按钮 = %d 个；服务器列表 %d 行；"+
		"详情页有删除按钮 = %v；访问 #/settings/nodes 之后停在 %q",
		res.DangerButtons, res.RowDangerButtons, res.NodesListChildren,
		res.DetailDeleteInDOM, res.GuestSettingsHash)
}

// TestNodeDeleteScreenshots 产出三张人工核对的 PNG（默认跳过，与仓库里其它截图
// 用例一样 —— CI 上不该往磁盘里写 PNG）：
//
//		$env:PROBE_SHOT_DIR = "$env:TEMP\probe-shots"
//		go test ./internal/e2e/ -run TestNodeDeleteScreenshots -v
//
//	  - node-delete-rows    —— 设置页「服务器列表」：每一行行尾「编辑节点」+「删除」；
//	  - node-delete-confirm —— 点行尾「删除」之后的二次确认框；
//	  - node-delete-after   —— 详情页删完回到首页：卡片没了、总览条台数变了。
//
// 为什么要有图：自动化用例能证明"两个按钮都在、class 是 btn danger、点完那一行
// 消失了"，证明不了"两个按钮并排看着顺不顺、那圈红边是不是像渲染坏了、确认框里
// 那句话读起来够不够清楚" —— 那些只有看图才知道。
func TestNodeDeleteScreenshots(t *testing.T) {
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

	logs := &captureHandler{}
	h := startServerAt(t, "127.0.0.1:0", filepath.Join(t.TempDir(), "probe.db"), time.UTC, slog.New(logs), nil)
	br := newBrowser(t, "http://"+h.addr)
	setupAdmin(t, br, waitSetupCode(t, logs))
	hk1ID := createNodeWithGroup(t, br, "hk-01", "香港")
	hk2ID := createNodeWithGroup(t, br, "hk-02", "香港")
	usID := createNodeWithGroup(t, br, "us-01", "美国")
	keepID := createNodeWithGroup(t, br, "keep-01", "")

	// 顺序有讲究：前两张都不真的删（确认框只是弹出来），第三张才走完删除 ——
	// 三张共用同一个服务端，反过来的话 rows/confirm 里就会少一台机器。
	shots := []struct {
		name string
		mode string
		w, h int
	}{
		{"node-delete-rows", "rows", 1664, 620},
		{"node-delete-confirm", "confirm", 1664, 620},
		{"node-delete-after", "after", 1664, 900},
	}
	for _, s := range shots {
		cfg := tzHarnessConfig{
			NodeID: usID, NodeName: "us-01",
			User: "admin", Pass: "a-very-good-password",
			Shot:  s.mode,
			Nodes: map[string]int64{"hk-01": hk1ID, "hk-02": hk2ID, "us-01": usID, "keep-01": keepID},
		}
		mock := newMockServer(t, newShotProxyWith(t, "http://"+h.addr, cfg, deleteHarnessJS))
		out := filepath.Join(outDir, s.name+".png")

		// --virtual-time-budget：等页面里的自检脚本把界面开到目标状态。
		// （自动化断言那条路径故意不用它：长连接 SSE 会让虚拟时间暂停。
		// 截图模式下 EventSource 已经被换成不联网的替身，见 deleteHarnessJS。）
		args := []string{
			"--headless=new", "--no-proxy-server", "--disable-gpu", "--no-first-run",
			"--hide-scrollbars",
			"--user-data-dir=" + t.TempDir(),
			"--window-size=" + strconv.Itoa(s.w) + "," + strconv.Itoa(s.h),
			"--force-device-scale-factor=1.5",
			"--virtual-time-budget=60000",
			"--screenshot=" + out,
			mock.URL + "/",
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

// phaseGate 是"浏览器告诉 Go 该动手了"的一条小通道（见 TestNodeDeleteInRealBrowser
// 里"别人删的"那一幕）：两边必须对齐时机 —— 浏览器先报到，Go 再去删。
type phaseGate struct {
	inner  http.Handler
	phases chan string
}

func newPhaseGate(inner http.Handler) *phaseGate {
	return &phaseGate{inner: inner, phases: make(chan string, 4)}
}

func (g *phaseGate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/__phase" {
		select {
		case g.phases <- r.URL.Query().Get("name"):
		default: // 没人接就丢掉：这一步不该把浏览器卡住
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	g.inner.ServeHTTP(w, r)
}

// ---------------------------------------------------------------- 观测值

type deleteResult struct {
	Errs  []string `json:"errs"`
	Fatal string   `json:"fatal"`
	Steps []string `json:"steps"`

	// 全程总共发生了几次整页导航（= 1 表示一次刷新都没有）。
	NavigationCount int `json:"navigationCount"`
	// 浏览器自己发出去了几次 DELETE（远程那一幕是 Go 发的，不算在内）。
	DeleteCalls int `json:"deleteCalls"`

	VisibleBefore []string `json:"visibleBefore"`
	SummaryBefore string   `json:"summaryBefore"`
	ChipsBefore   []string `json:"chipsBefore"`

	DetailDialogOpen   bool     `json:"detailDialogOpen"`
	DetailDialogTitle  string   `json:"detailDialogTitle"`
	DetailDialogText   string   `json:"detailDialogText"`
	DetailDialogWarn   string   `json:"detailDialogWarn"`
	DetailBackHome     bool     `json:"detailBackHome"`
	DetailCardGone     bool     `json:"detailCardGone"`
	DetailStillInGrid  bool     `json:"detailStillInGrid"`
	DetailDeleteMs     int      `json:"detailDeleteMs"`
	SummaryAfterDetail string   `json:"summaryAfterDetail"`
	ChipsAfterDetail   []string `json:"chipsAfterDetail"`
	VisibleAfterDetail []string `json:"visibleAfterDetail"`

	RowsBefore         int         `json:"rowsBefore"`
	RowButtons         []deleteRow `json:"rowButtons"`
	SettingsDialogOpen bool        `json:"settingsDialogOpen"`
	SettingsDialogText string      `json:"settingsDialogText"`
	SettingsDeleteMs   int         `json:"settingsDeleteMs"`
	RowsAfter          int         `json:"rowsAfter"`
	RowsAfterNames     []string    `json:"rowsAfterNames"`
	NodesError         string      `json:"nodesError"`

	RemoteDeleteMs     int      `json:"remoteDeleteMs"`
	RemoteCardGone     bool     `json:"remoteCardGone"`
	RemoteError        string   `json:"remoteError"`
	SummaryAfterRemote string   `json:"summaryAfterRemote"`
	ChipsAfterRemote   []string `json:"chipsAfterRemote"`
	VisibleAfterRemote []string `json:"visibleAfterRemote"`

	// 断线期间被删（重连后拿全量帧收掉）那一幕的观测值。
	HiddenCardGone     bool     `json:"hiddenCardGone"`
	HiddenError        string   `json:"hiddenError"`
	HiddenDeleteMs     int      `json:"hiddenDeleteMs"`
	EmptyShown         bool     `json:"emptyShown"`
	LiveText           string   `json:"liveText"`
	SummaryAfterHidden string   `json:"summaryAfterHidden"`
	VisibleAfterHidden []string `json:"visibleAfterHidden"`

	// 访客场景的观测值。
	GuestBarShown     bool     `json:"guestBarShown"`
	DangerButtons     []string `json:"dangerButtons"`
	RowDangerButtons  int      `json:"rowDangerButtons"`
	NodesListChildren int      `json:"nodesListChildren"`
	DetailDeleteInDOM bool     `json:"detailDeleteInDOM"`
	GuestSettingsHash string   `json:"guestSettingsHash"`

	Routes []groupRoute `json:"routes"`
	Debug  any          `json:"debug"`
}

// deleteRow 是设置页一行行尾的按钮（名字 + 每个按钮的文本 / class / 无障碍属性 / 算出来的颜色）。
type deleteRow struct {
	Name    string            `json:"name"`
	Buttons []deleteRowButton `json:"buttons"`
}

type deleteRowButton struct {
	Text        string `json:"text"`
	Cls         string `json:"cls"`
	Title       string `json:"title"`
	Aria        string `json:"aria"`
	Color       string `json:"color"`
	BorderColor string `json:"borderColor"`
}

// ---------------------------------------------------------------- 浏览器里的自检脚本

// deleteHarnessJS 是注入到页面里的自检脚本（与仓库里其它浏览器用例同一套做法）。
//
// 它只做两件事：按用户的真实操作驱动界面（登录 → 详情页点删除 → 设置页点删除 →
// 停在首页等别人删），以及把看到的原样记下来。**一句断言都不在这里做** ——
// 期望值全在 Go 那边（见 checkDetailDelete 等几个函数）。
const deleteHarnessJS = `(function () {
  'use strict';
  var CFG = window.__TZCFG || {};
  var rawFetch = window.fetch.bind(window);
  var R = {
    errs: [], fatal: '', steps: [], navigationCount: 0, deleteCalls: 0,
    visibleBefore: [], summaryBefore: '', chipsBefore: [],
    detailDialogOpen: false, detailDialogTitle: '', detailDialogText: '', detailDialogWarn: '',
    detailBackHome: false, detailCardGone: false, detailStillInGrid: false, detailDeleteMs: -1,
    summaryAfterDetail: '', chipsAfterDetail: [], visibleAfterDetail: [],
    rowsBefore: 0, rowButtons: [], settingsDialogOpen: false, settingsDialogText: '',
    settingsDeleteMs: -1, rowsAfter: 0, rowsAfterNames: [], nodesError: '',
    remoteDeleteMs: -1, remoteCardGone: false, remoteError: '', summaryAfterRemote: '',
    chipsAfterRemote: [], visibleAfterRemote: [],
    hiddenCardGone: false, hiddenError: '', hiddenDeleteMs: -1, emptyShown: false,
    liveText: '', summaryAfterHidden: '', visibleAfterHidden: [],
    guestBarShown: false, dangerButtons: [], rowDangerButtons: 0, nodesListChildren: 0,
    detailDeleteInDOM: false, guestSettingsHash: '', routes: []
  };
  window.__DELETERESULT = R;

  // 截图模式：把 EventSource 换成不联网的替身（理由见 groupfilter_browser_test.go：
  // 挂着的 SSE 请求会让 --virtual-time-budget 永远耗不完，Chrome 就不截图了）。
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

  // ---- 记 DELETE 请求次数（浏览器自己发的那两次）---------------------------
  window.fetch = function (input, init) {
    var url = typeof input === 'string' ? input : ((input && input.url) || '');
    var method = ((init && init.method) || 'GET').toUpperCase();
    return rawFetch(input, init).then(function (res) {
      if (method === 'DELETE') R.deleteCalls++;
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
  function hashOf() { return window.location.hash || '#/'; }

  // 现在**看得见**的卡片名字（顺序就是 DOM 顺序）。
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
  // 卡片元素还在不在 DOM 里（不管 hidden）——这条才分得清"整张摘掉了"与"只是藏起来"。
  function cardInGrid(id) {
    var grid = node('grid');
    if (!grid) return false;
    return !!grid.querySelector('.card[data-node-id="' + id + '"]');
  }
  function cardGone(id) {
    var grid = node('grid');
    if (!grid) return false;
    for (var i = 0; i < grid.children.length; i++) {
      if (grid.children[i].dataset.nodeId === String(id)) return false;
    }
    return true;
  }
  function chips() {
    var box = node('group-filter');
    if (!box || box.hidden) return [];
    var out = [];
    Array.prototype.forEach.call(box.querySelectorAll('button'), function (b) { out.push(b.textContent); });
    return out;
  }
  function summary() { return textOf('sum-online') + '/' + textOf('sum-total'); }
  function showHome() {
    return waitFor('首页四张卡片都画出来', function () {
      return shown('view-home') && node('grid') && node('grid').children.length === 4;
    }, 30000).then(function () {
      return waitFor('分组 chip 就绪', function () { return chips().length >= 4; }, 15000);
    }).then(function () { return sleep(300); });
  }

  // 设置页每一行行尾的按钮（文本 / class / 无障碍属性 / 算出来的颜色）。
  function rowButtonInfo(row) {
    var out = { name: '', buttons: [] };
    var nameEl = row.querySelector('.node-item-name');
    out.name = nameEl ? nameEl.textContent : '';
    Array.prototype.forEach.call(row.querySelectorAll('.node-item-acts button'), function (b) {
      var cs = getComputedStyle(b);
      out.buttons.push({
        text: b.textContent, cls: b.className, title: b.title,
        aria: b.getAttribute('aria-label') || '',
        color: cs.color, borderColor: cs.borderTopColor
      });
    });
    return out;
  }
  function rowNames() {
    var list = node('nodes-list');
    var out = [];
    if (!list) return out;
    Array.prototype.forEach.call(list.children, function (row) {
      var nameEl = row.querySelector('.node-item-name');
      out.push(nameEl ? nameEl.textContent : '?');
    });
    return out;
  }
  function rowByNodeName(name) {
    var list = node('nodes-list');
    var rows = list ? list.children : [];
    for (var i = 0; i < rows.length; i++) {
      var nameEl = rows[i].querySelector('.node-item-name');
      if (nameEl && nameEl.textContent === name) return rows[i];
    }
    return null;
  }

  // 确认框：等它真的打开（<dialog>.open），把三个字段读下来。
  function confirmOpen() { var d = node('dlg-confirm'); return !!d && d.open === true; }
  function readConfirm() {
    return {
      title: textOf('confirm-title'), text: textOf('confirm-text'), warn: textOf('confirm-warn')
    };
  }
  function clickConfirm() {
    node('confirm-ok').click();
    return waitFor('确认框关闭', function () { return !confirmOpen(); }, 15000);
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

  // 等访客面板就绪（不登录：服务端开着访客查看时，未登录的浏览器进的就是只读面板）。
  function guestHome() {
    return waitFor('访客首页出现', function () { return shown('view-home'); }, 30000)
      .then(function () {
        return waitFor('访客卡片画出来', function () {
          return node('grid') && node('grid').children.length === 1;
        }, 30000);
      })
      .then(function () { return sleep(300); });
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
    var routes = ['#/', '#/n/' + CFG.nodes['keep-01'], '#/settings/nodes', '#/settings/alert'];
    var out = [];
    return routes.reduce(function (chain, hash) {
      return chain.then(function () {
        var before = R.errs.length;
        window.location.hash = hash;
        return sleep(1000).then(function () {
          out.push({ hash: hash, errs: R.errs.length - before, view: currentView() });
        });
      });
    }, Promise.resolve()).then(function () { R.routes = out; });
  }

  // ---- 场景一：详情页删除（用户报的那条路）--------------------------------
  function detailDelete() {
    window.location.hash = '#/n/' + CFG.nodeID;
    return waitFor('详情页打开', function () {
      return shown('view-detail') && textOf('detail-name') === CFG.nodeName;
    }, 30000).then(function () {
      return waitFor('详情页的删除按钮可用', function () {
        return node('detail-delete') && !node('detail-delete').disabled;
      }, 10000);
    }).then(function () {
      node('detail-delete').click();
      return waitFor('弹出二次确认', confirmOpen, 15000);
    }).then(function () {
      R.detailDialogOpen = true;
      var c = readConfirm();
      R.detailDialogTitle = c.title;
      R.detailDialogText = c.text;
      R.detailDialogWarn = c.warn;
      var started = Date.now();
      return clickConfirm().then(function () {
        return waitFor('回到首页', function () { return shown('view-home'); }, 15000);
      }).then(function () {
        R.detailBackHome = true;
        return waitFor('us-01 的卡片从 DOM 里消失', function () { return cardGone(CFG.nodeID); }, 15000);
      }).then(function () {
        R.detailDeleteMs = Date.now() - started;
        R.detailCardGone = true;
        R.detailStillInGrid = cardInGrid(CFG.nodeID);
      });
    }).then(function () { return sleep(500); })
      .then(function () {
        R.visibleAfterDetail = cardNames();
        R.summaryAfterDetail = summary();
        R.chipsAfterDetail = chips();
      });
  }

  // ---- 场景二：设置页行尾的删除按钮 ---------------------------------------
  function settingsDelete() {
    window.location.hash = '#/settings/nodes';
    return waitFor('服务器列表就绪', function () {
      var list = node('nodes-list');
      return shown('view-settings') && list && list.children.length === 3;
    }, 30000).then(function () { return sleep(300); })
      .then(function () {
        R.rowsBefore = node('nodes-list').children.length;
        Array.prototype.forEach.call(node('nodes-list').children, function (row) {
          R.rowButtons.push(rowButtonInfo(row));
        });
        var row = rowByNodeName('hk-01');
        if (!row) throw new Error('服务器列表里找不到 hk-01 那一行');
        var del = null;
        Array.prototype.forEach.call(row.querySelectorAll('.node-item-acts button'), function (b) {
          if (b.textContent === '删除') del = b;
        });
        if (!del) throw new Error('hk-01 那一行行尾没有「删除」按钮');
        var started = Date.now();
        del.click();
        return waitFor('弹出二次确认', confirmOpen, 15000).then(function () {
          R.settingsDialogOpen = true;
          R.settingsDialogText = readConfirm().text;
          return clickConfirm();
        }).then(function () {
          return waitFor('hk-01 那一行消失', function () {
            var list = node('nodes-list');
            return list && list.children.length === 2 && rowNames().indexOf('hk-01') < 0;
          }, 15000);
        }).then(function () {
          R.settingsDeleteMs = Date.now() - started;
          R.rowsAfter = node('nodes-list').children.length;
          R.rowsAfterNames = rowNames();
          R.nodesError = textOf('nodes-error');
        });
      });
  }

  // ---- 场景三：别人删的（浏览器停在首页，什么都不点）----------------------
  function remoteDelete() {
    window.location.hash = '#/';
    return waitFor('回到首页', function () { return shown('view-home'); }, 15000)
      .then(function () { return sleep(300); })
      .then(function () {
        if (node('grid').children.length !== 2) {
          throw new Error('这一拍首页该有两张卡片，实际 ' + node('grid').children.length);
        }
        // 报到：Go 那边收到之后才去删 hk-02（见 TestNodeDeleteInRealBrowser）。
        return rawFetch('/__phase?name=remote', { method: 'POST' }).then(function () { return true; });
      })
      .then(function () {
        var started = Date.now();
        // 超时**不**当作致命错误往上抛：这一条要的是"卡片没消失"这个观测值，
        // 让 Go 那边的断言把它连同原因一起说出来（见 checkRemoteDelete）。
        return waitFor('hk-02 的卡片自己消失', function () { return cardGone(CFG.nodes['hk-02']); }, 20000)
          .then(function () { R.remoteCardGone = true; })
          .catch(function (err) { R.remoteError = String(err && err.message ? err.message : err); })
          .then(function () { R.remoteDeleteMs = Date.now() - started; });
      })
      .then(function () { return sleep(500); })
      .then(function () {
        R.visibleAfterRemote = cardNames();
        R.summaryAfterRemote = summary();
        R.chipsAfterRemote = chips();
        R.navigationCount = performance.getEntriesByType('navigation').length;
      });
  }

  // 文档里"节点删除"这一类入口：详情页头部的 #detail-delete、设置页行尾的删除，
  // 以及任何写着「删除」两个字的按钮。
  //
  // 为什么按"文本 + id"找而不是数 .btn.danger：文档里本来就有别的危险色按钮
  // （共用确认框里的「确认」、设置页两步验证那栏的「关闭两步验证」），它们都跟
  // 节点无关。这里要问的是"访客能不能看到一个删节点的入口"。
  function deleteEntryPoints() {
    var entries = [];
    Array.prototype.forEach.call(document.querySelectorAll('button'), function (b) {
      if (b.closest('dialog')) return;
      var text = (b.textContent || '').trim();
      if (text === '删除' || b.id === 'detail-delete') {
        entries.push((b.id || '(' + b.className + ')') + ':' + text);
      }
    });
    return {
      entries: entries,
      rowDanger: document.querySelectorAll('.node-item-acts .btn.danger').length
    };
  }

  // ---- 场景四：访客（没有会话）--------------------------------------------
  function guestPass() {
    // 先等访客面板真的画出来：applyAdminChrome 会把管理员入口从 DOM 里摘掉，
    // 在那之前采样会数到还没被摘掉的按钮（等于什么都没验到）。
    return guestHome().then(function () {
      var d = deleteEntryPoints();
      R.guestBarShown = shown('guest-bar');
      R.dangerButtons = d.entries;
      R.rowDangerButtons = d.rowDanger;
      R.nodesListChildren = node('nodes-list') ? node('nodes-list').children.length : 0;
      // 直接去设置页：访客不该被丢到那一栏上（那几个接口在服务端就是 401）。
      window.location.hash = '#/settings/nodes';
      return sleep(800);
    }).then(function () {
      R.guestSettingsHash = hashOf();
      var d = deleteEntryPoints();
      R.dangerButtons = d.entries;
      R.rowDangerButtons = d.rowDanger;
      R.nodesListChildren = node('nodes-list') ? node('nodes-list').children.length : 0;
      // 详情页头部的三个管理员入口（含删除）也不该在文档里。
      window.location.hash = '#/n/' + CFG.nodeID;
      return waitFor('访客详情页打开', function () {
        return shown('view-detail') && textOf('detail-name') === CFG.nodeName;
      }, 30000);
    }).then(function () { return sleep(300); })
      .then(function () {
        var d = deleteEntryPoints();
        R.detailDeleteInDOM = !!node('detail-delete');
        R.dangerButtons = d.entries;
        R.rowDangerButtons = d.rowDanger;
      });
  }

  // ---- 场景五：断线期间被删的节点（重连后拿全量帧收掉）--------------------
  //
  // 走的是真实那条路：页面切到后台 → app.js 自己断开实时流（见它的 visibilitychange）
  // → 这期间节点被删（**它从未出现在任何一帧变更集里**）→ 切回前台自动重连 →
  // 服务端发全量快照（payload.full）→ 前端照它做差集，卡片收掉。
  //
  // 为什么单独有这一幕："变更集里的删除名单"那条路覆盖不了它 —— 断线时那个 id
  // 一次都没被推过。少了全量帧的差集，这张卡片会一直留到整页刷新。
  function hidePage(hidden) {
    // app.js 只读 document.hidden（那是只读属性），这里盖一个可配置的值出来，
    // 再派发一次真的 visibilitychange —— 与用户切到别的标签页走的是同一段代码。
    try {
      Object.defineProperty(document, 'hidden', { value: hidden, configurable: true });
    } catch (e) { /* 盖不上就直说，下面的等待会超时 */ }
    document.dispatchEvent(new Event('visibilitychange'));
  }

  function reconnectDelete() {
    window.location.hash = '#/';
    return waitFor('回到首页', function () { return shown('view-home'); }, 15000)
      .then(function () { return sleep(300); })
      .then(function () {
        if (node('grid').children.length !== 1) {
          throw new Error('这一拍首页该只剩 1 张卡片，实际 ' + node('grid').children.length);
        }
        hidePage(true);
        return waitFor('实时流已断开', function () { return textOf('live-text') === '未连接'; }, 10000);
      })
      .then(function () {
        // 报到：Go 收到之后才去删 keep-01（浏览器此刻是"后台标签页"，收不到推送）。
        return rawFetch('/__phase?name=hidden', { method: 'POST' }).then(function () { return true; });
      })
      .then(function () { return sleep(1200); })   // 等服务端真的把那一台删掉
      .then(function () {
        var started = Date.now();
        hidePage(false);   // 切回前台：app.js 自己重连，重连的第一帧就是全量快照
        return waitFor('重连后 keep-01 的卡片消失', function () {
          return shown('view-home') && cardGone(CFG.nodes['keep-01']);
        }, 20000)
          .then(function () { R.hiddenCardGone = true; })
          .catch(function (err) { R.hiddenError = String(err && err.message ? err.message : err); })
          .then(function () { R.hiddenDeleteMs = Date.now() - started; });
      })
      .then(function () { return sleep(800); })
      .then(function () {
        R.liveText = textOf('live-text');
        R.emptyShown = shown('empty');
        R.summaryAfterHidden = summary();
        R.visibleAfterHidden = cardNames();
        R.navigationCount = performance.getEntriesByType('navigation').length;
      });
  }

  function run() {
    var scenario = CFG.scenario || 'delete';
    return waitFor('页面脚本就绪', function () { return !!node('view-login'); }, 30000)
      .then(function () {
        if (scenario === 'guest') return guestPass();
        return login().then(showHome).then(function () {
          R.visibleBefore = cardNames();
          R.summaryBefore = summary();
          R.chipsBefore = chips();
          R.navigationCount = performance.getEntriesByType('navigation').length;
        }).then(detailDelete).then(settingsDelete).then(remoteDelete)
          .then(routePass).then(reconnectDelete);
      });
  }

  // shot 模式（人工截图用）：把界面开到指定状态就停住，不回传结果。
  //   rows     → 设置页服务器列表（行尾两个按钮）
  //   confirm  → 那一行的删除二次确认框
  //   after    → 删完之后回到首页（卡片没了、总览条数字变了）
  function shot() {
    var mode = CFG.shot;
    return login().then(showHome).then(function () {
      if (mode === 'rows') {
        window.location.hash = '#/settings/nodes';
        return waitFor('服务器列表就绪', function () {
          return shown('view-settings') && node('nodes-list') && node('nodes-list').children.length === 4;
        }, 30000);
      }
      if (mode === 'confirm') {
        window.location.hash = '#/settings/nodes';
        return waitFor('服务器列表就绪', function () {
          return shown('view-settings') && node('nodes-list') && node('nodes-list').children.length === 4;
        }, 30000).then(function () {
          var row = rowByNodeName('hk-02');
          if (!row) throw new Error('找不到 hk-02 那一行');
          Array.prototype.forEach.call(row.querySelectorAll('.node-item-acts button'), function (b) {
            if (b.textContent === '删除') b.click();
          });
          return waitFor('弹出二次确认', confirmOpen, 15000);
        });
      }
      // after：走一遍详情页删除，停在首页（卡片已经没了）。
      return detailDelete();
    }).then(function () {
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
    }).then(function () { document.title = 'DUMPMARK:delete'; });
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
