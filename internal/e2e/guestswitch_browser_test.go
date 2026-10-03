package e2e

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 「访客模式下登录管理员」这条路上的实时通道身份切换。
//
// 用户实测报告（原话）：「从只读模式下，我登录了之后点开小鸡详情，里面的
// 本机地址、来源 ip 会闪一下然后消失，刷新下网页倒是有了」。
//
// 机制（见 _audit/SECURITY-AUDIT-ROUND2.md 区域 07 的「发现 5」）：
//  1. 访客身份回到只读面板（#/）时，前端建起一条 SSE 实时流 —— 它的身份是
//     **建连那一刻**由服务端判定的（internal/server/api_stream.go:63/79）；
//  2. 登录之后那条流**还是访客身份的**：route() 重开实时流的判据是 `if (!source)`
//     （见 web/app.js 的 route），它只问"有没有流"，不问"这条流是谁建的"，
//     而 enterApp() 也不碰 source；
//  3. 于是详情页拉的是管理员数据（「本机地址 / 来源 IP」两行画出来了），
//     紧接着那条访客流推来一帧**脱敏 DTO**（白名单里没有这两个字段），
//     applyPayload 把它写进 detail.node 并重画「网络信息」卡 ⇒ 两行消失；
//  4. 刷新整页之所以正常：深链到 #/n/<id> 时路由在 openDetail 就返回了，
//     **根本不建流**，也就没有帧来覆盖它。
//
// 为什么必须真浏览器 + 真 Agent：
//   - 断言的是"DOM 里那两行还在不在"，只有真浏览器说得清；
//   - 帧必须**真的在推**。没有 Agent 持续上报时，服务端的变更集里根本没有这台
//     机器（internal/server/api_stream.go:269 的 seq 判据），覆盖不会发生 ——
//     既有用例 adminPass（guest_browser_test.go）一直是绿的正是这个原因。
//
// 断言用一串连续采样（每 150 毫秒一次、共 6 秒）而不是两个点：用户报告的是
// "闪一下然后消失"，抹掉发生在中间某一拍，两个采样点有可能正好错开。
func TestGuestLoginKeepsDetailAddressRows(t *testing.T) {
	chrome := findChrome()
	if chrome == "" {
		t.Skip("找不到 Chrome：设置 PROBE_CHROME 或把它装到默认位置后这条用例会自动跑起来")
	}

	h, nodeID, token := startGuestSwitchFixture(t)

	// 真 Agent：节点每秒钟上报一拍，服务端每秒的变更集里因此总带着这台机器。
	// 没有它，这条用例会"绿得毫无意义"（没有任何帧来覆盖详情页）。
	client, _ := newClient(t, "http://"+h.addr, token)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = client.Run(ctx) }()
	waitFor(t, 15*time.Second, "Agent 上报两拍", func() bool {
		n, ok := h.srv.State().Get(nodeID)
		return ok && n.Seq >= 2
	})

	cfg := tzHarnessConfig{
		NodeID: nodeID, NodeName: "switch-01",
		User: "admin", Pass: "a-very-good-password",
		Scenario: "switch",
	}
	proxy := newHarnessProxy(t, "http://"+h.addr, cfg, guestHarnessJS)
	mock := newMockServer(t, proxy)
	raw := runChromeForResult(t, chrome, mock.URL+"/", proxy.result, 240*time.Second, "1500,1100")

	var res guestSwitchResult
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

	// ---- 现场自检 ①：访客那一遍看不到这两行（脱敏仍然生效，这条用例不空） ----
	if len(res.GuestNetwork) == 0 {
		t.Fatal("访客详情页的「网络信息」卡是空的 —— 现场没搭起来，下面的断言会变成空断言")
	}
	for _, label := range res.GuestNetwork {
		if label == "本机地址" || label == "来源 IP" {
			t.Errorf("访客详情页不该有 %q 这一行（服务端白名单里没有这两个字段）：%v",
				label, res.GuestNetwork)
		}
	}
	for _, title := range res.GuestCardTitles {
		if strings.Contains(title, "127.0.0.1") {
			t.Errorf("访客首页卡片的 title 里不该有 observed_ip：%q", title)
		}
	}
	t.Logf("访客详情页：「网络信息」卡标签 = %v；首页卡片 title = %v",
		res.GuestNetwork, res.GuestCardTitles)

	// ---- 现场自检 ②：登录后首页卡片带上 observed_ip（管理员那一份数据到了） ----
	// 这一条与详情页那两行是同一个根因的两处表现：访客流不清掉的话，首页卡片
	// 永远是访客那一份（title 里没有地址），而详情页拉的是管理员数据 ——
	// 两处自相矛盾（审计 07-发现 5 的原话）。
	if !anyContains(res.AdminCardTitles, "127.0.0.1") {
		t.Errorf("登录后首页卡片的 title 里应当有 observed_ip（管理员数据）：%v", res.AdminCardTitles)
	}

	// ---- 实时通道的身份：登录之后不该再收到访客脱敏帧 ----
	//
	// 这是根因那一环的直接证据（比"两行地址消失"更贴近原因）：服务端按**建连
	// 那一刻**的身份决定这条流推脱敏版还是完整版（internal/server/api_stream.go:63/79），
	// 所以"登录之后还在收脱敏帧"就等于"这条流还是访客身份的"。
	//
	// 先要两条"不空"的证据：登录**之前**确实收到过脱敏帧（仪表真的在记东西）、
	// 登录**之后**确实还有数据在推（否则"没有脱敏帧"可能只是因为没帧）。
	if res.StreamLog.MaskedFrames == 0 {
		t.Error("登录之前一帧脱敏数据都没收到 —— 仪表没起作用，下面那条断言会是空的")
	}
	if res.StreamLog.FramesAfterLogin < 3 {
		t.Errorf("登录之后只收到 %d 帧实时数据，这条用例说明不了问题（没有数据在推）",
			res.StreamLog.FramesAfterLogin)
	}
	if res.StreamLog.MaskedAfterLogin != 0 {
		t.Errorf("登录之后仍然收到 %d 条**访客脱敏**帧（共 %d 帧，其中 %d 帧带私有字段）："+
			"那条访客身份的实时流没有被换掉", res.StreamLog.MaskedAfterLogin,
			res.StreamLog.FramesAfterLogin, res.StreamLog.PrivateFrames)
	}
	// 重建一次就够：登录只发生一次，不该出现"反复重连"（访客一条 + 管理员一条 = 2）。
	if res.StreamLog.Created > 3 {
		t.Errorf("这一轮里一共建了 %d 条实时流，像是重连风暴", res.StreamLog.Created)
	}
	t.Logf("实时通道：建流 %d 条；收到 %d 帧（脱敏 %d、带私有字段 %d）；登录后 %d 帧（脱敏 %d）",
		res.StreamLog.Created, res.StreamLog.Frames, res.StreamLog.MaskedFrames,
		res.StreamLog.PrivateFrames, res.StreamLog.FramesAfterLogin, res.StreamLog.MaskedAfterLogin)

	// ---- 采样序列：那两行出现过吗？稳定吗？ ----
	if len(res.Samples) < 20 {
		t.Fatalf("连续采样点太少（%d 个），这条用例说明不了稳定性", len(res.Samples))
	}
	withRows, firstMissing := 0, -1
	for i, s := range res.Samples {
		if hasAddressRows(s.Labels) {
			withRows++
			continue
		}
		if firstMissing < 0 {
			firstMissing = i
		}
	}
	last := res.Samples[len(res.Samples)-1]
	t.Logf("采样 %d 个点：带两行地址的 %d 个；最后一个采样点（t=%dms）标签 = %v",
		len(res.Samples), withRows, last.T, last.Labels)

	// ① 管理员数据确实画出来过 —— 这一条把"两条守卫把响应丢了"那种解释排除掉：
	//    真丢了响应的话，这两行**一次都不会出现**。
	if withRows == 0 {
		t.Fatalf("登录后详情页的「本机地址 / 来源 IP」一次都没出现过（%d 个采样点全是 %v）"+
			" —— 管理员那份详情数据没有落地，不是被脱敏帧覆盖", len(res.Samples), last.Labels)
	}
	// ② 稳定：采样结束（6 秒、约 6 拍实时数据）时那两行还在。用户报告的就是这一条。
	if !hasAddressRows(last.Labels) {
		t.Errorf("登录后详情页的「本机地址 / 来源 IP」在 6 秒内消失了：第 %d 个采样点（t=%dms）起"+
			"「网络信息」卡里只剩 %v —— 访客身份的那条实时流推来的脱敏帧把它覆盖掉了"+
			"（最后一条采样 t=%dms）", firstMissing, res.Samples[firstMissing].T, last.Labels, last.T)
	}
	// ③ 两行不只是"标签在"：值也要在（真实 Agent 的 local_ip / observed_ip 都是
	//    回环地址 —— 服务端地址就是 127.0.0.1）。
	if !strings.Contains(res.StableText, "127.0.0.1") {
		t.Errorf("采样结束时「网络信息」卡的文本里看不到 Agent 的地址：%q", res.StableText)
	}
	if firstMissing >= 0 {
		t.Logf("注意：第 %d 个采样点（t=%dms）起那两行不见了", firstMissing, res.Samples[firstMissing].T)
	}
}

// guestSwitchSample 是「网络信息」卡在某一时刻的行标签。
type guestSwitchSample struct {
	T      int      `json:"t"`
	Labels []string `json:"labels"`
}

// guestSwitchResult 是「身份切换」用例从浏览器里带回来的观测值（见 guestHarnessJS
// 的 switchPass）。
type guestSwitchResult struct {
	Errs  []string `json:"errs"`
	Fatal string   `json:"fatal"`
	Steps []string `json:"steps"`

	GuestCards      int      `json:"guestCards"`
	GuestCardTitles []string `json:"guestCardTitles"`
	GuestNetwork    []string `json:"guestNetwork"`

	AdminCardTitles []string `json:"adminCardTitles"`

	// StreamLog 是页面里那个实时通道仪表（见 guestHarnessJS）：建了几条流、
	// 收了几帧、其中多少帧是访客脱敏的。
	StreamLog struct {
		Created          int `json:"created"`
		Frames           int `json:"frames"`
		MaskedFrames     int `json:"maskedFrames"`
		PrivateFrames    int `json:"privateFrames"`
		FramesAfterLogin int `json:"framesAfterLogin"`
		MaskedAfterLogin int `json:"maskedAfterLogin"`
	} `json:"streamLog"`

	Samples    []guestSwitchSample `json:"samples"`
	StableText string              `json:"stableText"`
}

// hasAddressRows 判断一份「网络信息」卡标签里有没有那两行（它们的存在与否就是
// 身份：服务端的访客白名单里没有 local_ip / observed_ip / local_ip6）。
func hasAddressRows(labels []string) bool {
	var local, observed bool
	for _, l := range labels {
		if l == "本机地址" {
			local = true
		}
		if l == "来源 IP" {
			observed = true
		}
	}
	return local && observed
}

// anyContains 判断一组串里有没有一个含 needle（报错信息里要能直接看出实际值）。
func anyContains(list []string, needle string) bool {
	for _, s := range list {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}

// startGuestSwitchFixture 是这条用例的现场：一台节点 + 打开的访客开关。
//
// 与 startGuestBrowserFixture 的区别只有一处：**把节点的 Token 交出来** ——
// 这条用例必须再起一个真 Agent（见文件头说明）。其余现场（价格、探测目标、
// 播撒的地址）这条用例都用不上，索性不搭：少一份现场就少一处可能与断言无关的干扰。
func startGuestSwitchFixture(t *testing.T) (*harness, int64, string) {
	t.Helper()
	logs := &captureHandler{}
	h := startServerAt(t, "127.0.0.1:0", filepath.Join(t.TempDir(), "probe.db"), time.UTC, slog.New(logs), nil)
	br := newBrowser(t, "http://"+h.addr)
	setupAdmin(t, br, waitSetupCode(t, logs))

	nodeID, token := createNodeViaAPI(t, br, "switch-01")

	// 打开「允许访客查看」（走真实接口：这也是它该被使用的方式）。
	if status, body := br.do(http.MethodPut, "/api/v1/settings/guest", map[string]any{"enabled": true}, true); status != http.StatusOK {
		t.Fatalf("打开访客开关失败: %d %v", status, body)
	}
	if token == "" {
		t.Fatal("建节点没有返回 Token：这条用例起不了 Agent")
	}
	return h, nodeID, token
}
