package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"probe/internal/store"
)

// 这一份用例钉住的是第三阶段修掉的六处**前端状态**缺陷（审计 06-F1/F2/F3/F4/F6 与
// 07-发现 1/2/3/4）。它们有一个共同点：**只有真浏览器说得清**。
//
//   - "登出之后 DOM 里还剩什么"是元素在不在文档里，不是接口给没给（F1/F4）；
//   - "迟到的响应会不会写回 DOM"需要真的把请求拖慢、在它回来之前登出/离开（F2/F3/F6/发现2）；
//   - "实时流永久失败之后页面会不会复查身份"依赖 EventSource 的 CLOSED 语义（发现1）；
//   - "探测器那一行的 title 会不会跟着脱敏结果走"依赖"指纹相同就提前 return"这条
//     优化（发现4）。
//
// 做法与仓库里其它浏览器用例完全一致：真服务端（Server.Run 完整路径）+ 反代注入自检
// 脚本 + 页面把观测值 POST 回 mock，**不用** --dump-dom + --virtual-time-budget
// （首页挂着 SSE 长连接，虚拟时间会一直暂停）。
//
// 两个用例各跑一次 Chrome：
//  1. TestFrontendStateFixesInRealBrowser —— 06-F1/F2/F3/F4/F6 + 07-发现 1/2/3；
//  2. TestFrontendProbeRowTitleFollowsGuestMask —— 07-发现 4（要开着访客开关、
//     并且真的把身份从"页面内的管理员"降级成访客）。
const (
	frontStateNameA = "frontstate-A"
	frontStateNameB = "frontstate-B"
	// frontStatePass 必须与 setupAdmin 里那个密码一致（harness 用它登录）。
	frontStatePass = "a-very-good-password"
	// frontStatePass2 是"在别处改密码"之后的新密码：改密会注销其它会话并撤销全部
	// 管理员实时流，这正是审计里"会话在别处失效"的触发条件。
	frontStatePass2 = "a-very-good-password-2"
)

// frontFixResult 是浏览器回传的观测值。
type frontFixResult struct {
	Errs   []string       `json:"errs"`
	Steps  []string       `json:"steps"`
	Fatal  string         `json:"fatal"`
	Checks map[string]any `json:"checks"`
	Notes  map[string]any `json:"notes"`
}

// revokeProxy 在真反代前面加一个用例专用的小口子：POST /__revoke。
//
// 页面用它来请求"在**别处**把这个会话注销掉"（真实部署里对应管理员在另一台设备上
// 改密码）——由 Go 侧用**另一个会话**去调 /api/v1/auth/password。为什么不让页面自己
// 调：自己改密码只会注销**其它**会话，自己的会话反而留着，那就复现不出"这个页面
// 的会话在别处失效"。其余路径（含注入过脚本的首页与 /__result）原样交给 tzProxy。
type revokeProxy struct {
	inner  *tzProxy
	revoke func() error
}

func (p *revokeProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/__revoke" {
		if err := p.revoke(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	p.inner.ServeHTTP(w, r)
}

// revokeSessionFromElsewhere 用浏览器用例自己的会话（br）改管理员密码。
//
// 服务端在改密成功后会注销**其它**会话并撤销**全部**管理员实时流
// （见 auth.HandleChangePassword 与 Server.revokeStreams）——也就是说 Chrome 里
// 那条会话与那条 SSE 会同时失效，而页面自己并不知道。这就是审计里
// "会话被撤销（别处登出 / 改密 / 重置两步验证 / 过期）"那条触发条件。
//
// 这个函数会在 httptest 的 handler goroutine 里被调用，所以**不能**用 browser.do
// （它在传输错误时会调 t.Fatalf，而 Fatalf 只能从测试 goroutine 调）：这里只用
// 标准库发一次请求，把错误如实返回给页面（页面会把 5xx 记进 errs）。
func revokeSessionFromElsewhere(br *browser, oldPass, newPass string) error {
	body, err := json.Marshal(map[string]any{
		"current_password": oldPass,
		"new_password":     newPass,
		"new_password2":    newPass,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, br.base+"/api/v1/auth/password", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-CSRF-Token", br.csrf)
	// 复用 br 的 Cookie jar：里面装的是**另一个**会话（与 Chrome 那条无关）。
	resp, err := br.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("在别处改密码返回 %d: %s", resp.StatusCode, raw)
	}
	return nil
}

// startFrontStateFixture 搭"前端状态"用例的现场：两台机器（其中 A 带备注与来源 IP）、
// 一个探测目标（延迟卡片与 /ping 那条链要用它）、访客开关保持默认的**关**。
func startFrontStateFixture(t *testing.T) (*harness, *browser, int64, int64) {
	t.Helper()
	logs := &captureHandler{}
	h := startServerAt(t, "127.0.0.1:0", filepath.Join(t.TempDir(), "probe.db"), time.UTC, slog.New(logs), nil)
	br := newBrowser(t, "http://"+h.addr)
	setupAdmin(t, br, waitSetupCode(t, logs))

	idA, _ := createNodeViaAPI(t, br, frontStateNameA)
	idB, _ := createNodeViaAPI(t, br, frontStateNameB)

	// A 的备注：#node-note 的内容来自服务端回填（openNodeDialog），它是 F4 那条
	// 主路径的现场 —— 备注里写一个"能反推出主机"的串，正是服务端把 note 列为
	// 访客私有的理由。
	status, body := br.do(http.MethodPatch, fmt.Sprintf("/api/v1/nodes/%d", idA), map[string]any{
		"name": frontStateNameA, "group_name": "测试", "region": "HK", "interval_sec": 1,
		"note":             "root@203.0.113.7:22",
		"reset_day":        1,
		"traffic_warn_pct": 80,
	}, true)
	if status != http.StatusOK {
		t.Fatalf("改节点失败: %d %v", status, body)
	}
	// 让 A 带上来源 IP 与本机地址：F2 断言的是"登出后卡片 title 不该再出现 observed_ip"，
	// 现场没有它的话那条断言在任何实现下都成立。
	seedGuestNodeState(t, h, idA)
	// 一个探测目标：F3 要验"迟到的 /ping 会不会重建（隐藏的）延迟目标卡片"，
	// 没有目标的话那一块根本不会被建出来。
	createPingTarget(t, br, "frontstate-ping", "127.0.0.1", 80, 60)
	return h, br, idA, idB
}

// TestFrontendStateFixesInRealBrowser 一条 Chrome 走完六处前端状态修复的验收。
func TestFrontendStateFixesInRealBrowser(t *testing.T) {
	chrome := findChrome()
	if chrome == "" {
		t.Skip("找不到 Chrome：设置 PROBE_CHROME 或把它装到默认位置后这条用例会自动跑起来")
	}
	h, br, idA, idB := startFrontStateFixture(t)

	cfg := tzHarnessConfig{
		NodeID:   idA,
		NodeName: frontStateNameA,
		Nodes:    map[string]int64{frontStateNameA: idA, frontStateNameB: idB},
		Focus:    frontStateNameB,
		User:     "admin",
		Pass:     frontStatePass,
		Scenario: "frontstate",
	}
	proxy := newHarnessProxy(t, "http://"+h.addr, cfg, frontFixHarnessJS)
	wrapped := &revokeProxy{inner: proxy, revoke: func() error {
		return revokeSessionFromElsewhere(br, frontStatePass, frontStatePass2)
	}}
	mock := newMockServer(t, wrapped)

	raw := runChromeForResult(t, chrome, mock.URL+"/", proxy.result, 300*time.Second, "1500,1100")

	var res frontFixResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("解析浏览器回传的结果: %v\n原始内容：%s", err, tail(string(raw), 600))
	}
	// 先把观测值打出来再判 fatal：自检流程中途卡住时，这些值就是唯一的线索。
	t.Logf("已完成步骤：%v", res.Steps)
	t.Logf("观测到的中间值：%v", res.Notes)
	if res.Fatal != "" {
		t.Fatalf("浏览器里的自检流程没跑完：%s（已完成步骤：%v）", res.Fatal, res.Steps)
	}
	if len(res.Errs) != 0 {
		t.Fatalf("浏览器里有 %d 条 JS 报错：%v", len(res.Errs), res.Errs)
	}

	boolCheck := func(name string) bool {
		v, _ := res.Checks[name].(bool)
		return v
	}
	noteStr := func(name string) string {
		v, _ := res.Notes[name].(string)
		return v
	}
	noteNum := func(name string) float64 {
		v, _ := res.Notes[name].(float64)
		return v
	}

	// ---- 06-F1：改密表单里的明文密码不能在登出后留在 DOM 里 ----
	if !boolCheck("passwordFormsFilled") {
		t.Fatalf("前置条件没成立：改密表单三个框没有被填上（%v）", res.Notes["pwFilled"])
	}
	if !boolCheck("logoutClearsPasswordForms") {
		t.Errorf("退出登录后改密表单里还留着明文密码：%v（应当全被清空）", res.Notes["pwAfter"])
	}

	// ---- 06-F4：节点对话框与登录页用户名的残留 ----
	if noteStr("noteBefore") == "" {
		t.Fatalf("前置条件没成立：#node-note 打开编辑时没有被服务端回填（%v）", res.Notes["noteBefore"])
	}
	if !boolCheck("logoutClearsNodeForm") {
		t.Errorf("退出登录后 #node-note 还留着上一位登录者的备注：%q", noteStr("noteAfter"))
	}
	if noteStr("loginUserBefore") == "" {
		t.Fatalf("前置条件没成立：登录页用户名为空（登出清理那条断言会退化成空断言）")
	}
	if !boolCheck("logoutClearsLoginUser") {
		t.Errorf("退出登录后登录页的用户名还留着：%q", noteStr("loginUserAfter"))
	}

	// ---- 06-F2：登出之后迟到的 /api/v1/nodes 不许把卡片写回 #grid ----
	if got := noteNum("gridAfterLate"); got != 0 {
		t.Errorf("登出之后首页网格里又出现了 %.0f 张卡片：%v（卡片的 title 上带着来源 IP）",
			got, res.Notes["cardTitlesAfterLate"])
	}
	if !boolCheck("homeStaysEmptyAfterLogout") {
		t.Error("登出之后首页网格必须是空的（迟到的节点列表响应被世代守卫丢弃）")
	}
	// 同一处守卫的另一半：那条被丢弃的响应之后不许再建实时连接。
	if !boolCheck("noStreamAfterLogout") {
		t.Errorf("登出之后又新建了实时连接（EventSource 创建次数 %v → %v）：那条连接属于上一位登录者的页面",
			res.Notes["esBefore"], res.Notes["esAfter"])
	}

	// ---- 06-F6 / 07-发现 2：设置页四条在飞链（节点列表、操作记录、设置、Telegram）----
	if !boolCheck("settingsNodesStaysEmpty") {
		t.Errorf("退出登录后设置页「服务器列表」又被写回了 %.0f 行（里面是本机地址/来源 IP）",
			noteNum("nodesListChildren"))
	}
	if !boolCheck("auditStaysEmpty") {
		t.Errorf("退出登录后「操作记录」又被写回了 %.0f 行（每一行都带着来源 IP）",
			noteNum("auditBodyChildren"))
	}
	if !boolCheck("telegramPrefsStayEmpty") {
		t.Errorf("退出登录后 Telegram 的 chat_id 又被写回输入框：%q", noteStr("tgChatAfter"))
	}
	if !boolCheck("serverInfoStaysEmpty") {
		t.Errorf("退出登录后「服务端参数」又被写回：%q", noteStr("serverInfoAfter"))
	}

	// ---- 06-F3：详情页子请求（/series、/traffic、/ping）的过期守卫 ----
	if noteNum("latTargetsOnDetail") <= 0 {
		t.Fatalf("前置条件没成立：详情页的延迟目标卡片一个都没建出来（%v）——"+
			"迟到的 /ping 就没有东西可重建", res.Notes["latTargetsOnDetail"])
	}
	if !boolCheck("latePingDoesNotRebuildDetail") {
		t.Errorf("离开详情页之后，迟到的 /ping 又把（隐藏的）延迟目标卡片重建了出来：%v 个子节点",
			res.Notes["latTargetsAfterLatePing"])
	}
	if !boolCheck("lateTrafficDoesNotRedraw") {
		t.Errorf("离开详情页之后，迟到的 /traffic 又在流量画布上画了一帧（帧数 %v → %v）",
			res.Notes["trafficFramesMark"], res.Notes["trafficFramesAfter"])
	}

	// ---- 07-发现 3：畸形的设置地址不许把已登录的管理员丢到登录页 ----
	if !boolCheck("malformedHashKeepsSettings") {
		t.Errorf("畸形设置地址之后停在了 %q 视图（期望 settings）——已登录的管理员被留在了错误的页面",
			noteStr("malformedView"))
	}
	if !boolCheck("malformedHashNoNewErrors") {
		t.Errorf("畸形设置地址抛出了新的 JS 错误（%v → %v）：那条分支必须把解码失败当成「认不出的栏名」"+
			"处理，而不是让异常冒出去", res.Notes["errsBeforeMalformed"], res.Notes["errsAfterMalformed"])
	}
	if noteStr("loginErrorText") != "" {
		t.Errorf("登录页出现了一句与服务端无关的误导性错误：%q", noteStr("loginErrorText"))
	}
	if !boolCheck("legalHashStillWorks") {
		t.Error("修完之后合法的栏名（#/settings/audit）必须照常打开")
	}

	// ---- 07-发现 1：实时流永久失败（会话被撤销后的 401）要复查身份 ----
	if noteNum("cardsBeforeRevoke") < 2 {
		t.Fatalf("前置条件没成立：撤销之前首页只有 %.0f 张卡片（没有可清的数据）", res.Notes["cardsBeforeRevoke"])
	}
	if !boolCheck("revokedSessionRechecksIdentity") {
		t.Errorf("会话在别处失效之后，页面没有复查身份（/api/v1/session 请求数 %v → %v）",
			res.Notes["sessionFetchesBeforeRevoke"], res.Notes["sessionFetchesAfterRevoke"])
	}
	if !boolCheck("revokedSessionClearsHome") {
		t.Errorf("会话在别处失效之后，首页仍然留着 %.0f 张卡片（上一个身份的私有数据）",
			res.Notes["cardsAfterRevoke"])
	}
}

// TestFrontendProbeRowTitleFollowsGuestMask 钉住 07-发现 4。
//
// 现场：一个"名字留空、由 host 兜底"的探测目标（存储层会把它的 label 填成地址），
// 加上一段**数值恒定**的探测数据 —— 数值恒定是关键：管理员那一帧与访客那一帧的
// 取整读数完全相同，于是"指纹里有没有 label"成了唯一变量。
//
// 走法：管理员在首页看到那一格的 title 是地址 → 会话在别处失效（改密码），而
// 「允许访客查看」开着 ⇒ 实时流以**访客身份**重连成功（200），页面不会收到 CLOSED、
// 也就不会清空 —— 它会继续以访客身份收数据（这正是发现 4 的场景）。此时回一次详情页
// 再回首页会立刻取一份**访客** /overview：派生 label 已被服务端抹成空串。
// 指纹里带上 label 之后，那一格会重建，title 变成「目标 #id」；不带 label 的话
// 指纹与管理员那一帧相同 ⇒ 提前 return ⇒ title 里继续留着**被白名单抹掉的地址**。
func TestFrontendProbeRowTitleFollowsGuestMask(t *testing.T) {
	chrome := findChrome()
	if chrome == "" {
		t.Skip("找不到 Chrome：设置 PROBE_CHROME 或把它装到默认位置后这条用例会自动跑起来")
	}

	logs := &captureHandler{}
	h := startServerAt(t, "127.0.0.1:0", filepath.Join(t.TempDir(), "probe.db"), time.UTC, slog.New(logs), nil)
	br := newBrowser(t, "http://"+h.addr)
	setupAdmin(t, br, waitSetupCode(t, logs))

	nodeID, _ := createNodeViaAPI(t, br, "frontstate-probe")
	seedGuestNodeState(t, h, nodeID)

	// 目标的名字留空、host 用 guestPingHosts[2]（9.9.9.9）：存储层会用 host 兜底填
	// label，于是"公开字段 label 的值就是地址"——访客那一份必须在出口抹掉它。
	targetID := createPingTarget(t, br, "", guestPingHosts[2], 80, 60)
	seedFlatPingBuckets(t, h, nodeID, targetID, 25, 60, 50*time.Minute)

	// 访客开关打开：撤销之后实时流会以访客身份重连成功（200），页面因此"不知道"
	// 自己已经不是管理员了 —— 这正是发现 4 与发现 1 的分界。
	if status, body := br.do(http.MethodPut, "/api/v1/settings/guest", map[string]any{"enabled": true}, true); status != http.StatusOK {
		t.Fatalf("打开访客开关失败: %d %v", status, body)
	}

	cfg := tzHarnessConfig{
		NodeID:   nodeID,
		NodeName: "frontstate-probe",
		User:     "admin",
		Pass:     frontStatePass,
		Scenario: "probelabel",
		// Nodes 的键在别的用例里是"节点名字 → id"，这里额外借一个键把探测目标的 id
		// 交给脚本（脚本要拿它拼期望的「目标 #id」）。不改共享结构体是为了不动
		// 别的用例的现场配置。
		Nodes: map[string]int64{"probe-target": targetID},
		// Private 的语义就是"页面上不该出现的私有值"：第一个是那个派生 label 的地址
		// （降级之后必须从 title 上消失），第二个是来源 IP（用来判断身份真的降级了）。
		Private: []string{guestPingHosts[2], guestPrivateValues[0]},
	}
	proxy := newHarnessProxy(t, "http://"+h.addr, cfg, probeLabelHarnessJS)
	wrapped := &revokeProxy{inner: proxy, revoke: func() error {
		return revokeSessionFromElsewhere(br, frontStatePass, frontStatePass2)
	}}
	mock := newMockServer(t, wrapped)

	raw := runChromeForResult(t, chrome, mock.URL+"/", proxy.result, 240*time.Second, "1500,1100")

	var res frontFixResult
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
	noteStr := func(name string) string {
		v, _ := res.Notes[name].(string)
		return v
	}

	// 现场自检：降级**之前**那一格的 title 必须就是那个地址（否则"降级之后它没了"
	// 这条断言在任何实现下都成立）。
	if got := noteStr("titleBefore"); got != guestPingHosts[2] {
		t.Fatalf("前置条件没成立：降级之前探测行的 title = %q，期望 %q（行文本 %q）",
			got, guestPingHosts[2], noteStr("textBefore"))
	}
	// 数值没变：取整读数完全相同，所以"要不要重建"只取决于指纹里有没有 label。
	if noteStr("textAfter") != noteStr("textBefore") {
		t.Fatalf("读数变了（%q → %q）：这条用例的区分度建立在「数值没变、只有 label 变了」之上",
			noteStr("textBefore"), noteStr("textAfter"))
	}
	if !boolCheck("probeTitleFollowedGuestLabel") {
		t.Errorf("身份降级成访客之后，探测行的 title 仍然是被白名单抹掉的地址：%q（期望「目标 #…」）",
			noteStr("titleAfter"))
	}
}

// seedFlatPingBuckets 喂一段**数值恒定**的探测数据（与 latency_browser_test.go 里
// 那条有起伏的 seedPingBuckets 不同：这里要的是"取整读数前后完全一样"）。
func seedFlatPingBuckets(t *testing.T, h *harness, nodeID, targetID int64, value float64, step int, span time.Duration) {
	t.Helper()
	now := time.Now()
	end := now.Unix() - now.Unix()%int64(step) - int64(step)
	start := end - int64(span.Seconds())
	buckets := make([]store.PingBucket, 0, 64)
	for ts := start; ts <= end; ts += int64(step) {
		buckets = append(buckets, store.NewPingBucket(nodeID, targetID, ts, value, value-1, value+1, 0))
	}
	if len(buckets) < 20 {
		t.Fatalf("播撒的探测桶只有 %d 个，太少", len(buckets))
	}
	if err := h.db.UpsertPingBuckets(context.Background(), buckets); err != nil {
		t.Fatalf("写入探测桶: %v", err)
	}
	t.Logf("播撒恒值探测桶 %d 个（节点 %d，目标 %d，值 %.0f ms，步长 %d 秒）",
		len(buckets), nodeID, targetID, value, step)
}

// frontFixHarnessJS 是注入到页面里的自检脚本（06-F1/F2/F3/F4/F6 + 07-发现 1/2/3）。
//
// 它只做两件事：**装钩子**（fetch / EventSource / canvas 帧计数 / JS 报错）与
// **按用户的真实操作驱动界面**（登录 → 进首页/详情页/设置页 → 填表 → 退出登录），
// 然后把观测值 POST 回去。一句断言都不在这里做：期望值全部在 Go 侧。
//
// 六段流程按顺序跑，每段自己把现场恢复好（要登录的先登录）：
//
//	① 登录时那条 /api/v1/nodes 还在路上就登出（F2 的触发条件）；
//	② 打开编辑对话框（服务端回填 note）→ 填改密表单 → 登出（F1/F4）；
//	③ 进设置页后立刻登出（四条在飞链：F6 + 发现 2）；
//	④ 进详情页后立刻离开（三条子请求：F3）；
//	⑤ 畸形设置地址（发现 3）；
//	⑥ 在别处注销会话（发现 1）。
const frontFixHarnessJS = `(function () {
  'use strict';
  var CFG = window.__TZCFG || {};
  var IDA = CFG.nodeID;

  var R = { errs: [], steps: [], fatal: '', checks: {}, notes: {}, fetchLog: [] };
  window.__FRONTFIXRESULT = R;
  R.notes.sessionFetches = 0;

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
  function loggedIn() { return shown('view-home') || shown('view-detail') || shown('view-settings'); }
  function cardCount() { return document.querySelectorAll('#grid .card').length; }
  function paneShown(name) {
    var sec = document.querySelector('.pane[data-pane="' + name + '"]');
    return !!sec && !sec.hidden;
  }
  function childrenOf(id) { var n = node(id); return n ? n.childNodes.length : -1; }

  // ---- 钩 fetch：按**精确路径**拖慢指定接口，并给"响应真的落地了"计数 ----
  //
  // 为什么要精确路径而不是子串：'/api/v1/nodes' 是子串的话，/api/v1/nodes/1 与
  // /api/v1/nodes/1/ping 都会被一起拖慢，几段流程就分不开了。
  var rawFetch = window.fetch.bind(window);
  var delayMap = {};   // 精确路径 → 延迟毫秒
  var doneCount = {};  // 精确路径 → 已经交回给 app.js 的响应数
  function pathOf(url) {
    var s = String(url || '');
    var q = s.indexOf('?'); if (q >= 0) s = s.slice(0, q);
    var h = s.indexOf('#'); if (h >= 0) s = s.slice(0, h);
    var i = s.indexOf('/api/');
    return i >= 0 ? s.slice(i) : s;
  }
  window.fetch = function (input, init) {
    var url = typeof input === 'string' ? input : ((input && input.url) || '');
    var p = pathOf(url);
    R.fetchLog.push(p);
    if (p === '/api/v1/session') R.notes.sessionFetches = (R.notes.sessionFetches || 0) + 1;
    var pr = rawFetch(input, init);
    var ms = delayMap[p] || 0;
    return pr.then(function (res) {
      if (ms <= 0) { doneCount[p] = (doneCount[p] || 0) + 1; return res; }
      return sleep(ms).then(function () {
        doneCount[p] = (doneCount[p] || 0) + 1;
        return res;
      });
    });
  };
  function doneOf(p) { return doneCount[p] || 0; }
  function waitDone(p, mark, what) {
    return waitFor(what, function () { return doneOf(p) > mark; }, 30000);
  }

  // ---- 钩 EventSource：数创建与关闭（F2 的"不许再建流"要用）----------------
  // instances 留着每一条连接的 readyState：发现 1 的断言（会话撤销后重连吃 401 →
  // CLOSED）如果没在浏览器里发生，这几个数字是唯一能说明"卡在哪一步"的证据。
  var OrigES = window.EventSource;
  var instances = [];
  var origClose = OrigES.prototype.close;
  OrigES.prototype.close = function () {
    R.notes.esClosed = (R.notes.esClosed || 0) + 1;
    return origClose.apply(this, arguments);
  };
  function WrappedES(url, cfg) {
    R.notes.esCreated = (R.notes.esCreated || 0) + 1;
    var inst = new OrigES(url, cfg);
    instances.push(inst);
    return inst;
  }
  WrappedES.prototype = OrigES.prototype;
  // 三个常量必须一起搬过来：app.js 的永久失败判据是
  // "es.readyState === EventSource.CLOSED"，而换掉构造函数之后 EventSource.CLOSED
  // 会变成 undefined —— 那样这条用例会以"app.js 没反应"的样子红，实际是脚本自己
  // 把常量弄丢了（真实页面里 EventSource 永远是原生那个，常量必然存在）。
  WrappedES.CONNECTING = OrigES.CONNECTING;
  WrappedES.OPEN = OrigES.OPEN;
  WrappedES.CLOSED = OrigES.CLOSED;
  window.EventSource = WrappedES;
  function esStates() { return instances.map(function (es) { return es.readyState; }); }

  // ---- 钩 canvas 的 clearRect：chart.js 每一帧都以它开头，帧数=画了几次 ----
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

  // ---- 登录（用户名框里会留下 admin：F4 的一条触发路径）-------------------
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

  // ---- ① 06-F2：登录时那条 /api/v1/nodes 还在路上就退出登录 ----------------
  function inFlightHomeCheck() {
    node('btn-logout').click();
    return waitFor('退出登录回到登录页（在飞那条）', function () { return shown('view-login'); }, 30000)
      .then(function () { return waitDone('/api/v1/nodes', R.notes.nodesMark, '登录时那条节点列表响应落地'); })
      .then(function () { return sleep(600); })
      .then(function () {
        delayMap = {};
        R.notes.esAfter = R.notes.esCreated || 0;
        R.notes.gridAfterLate = cardCount();
        R.notes.cardTitlesAfterLate = Array.prototype.map.call(
          document.querySelectorAll('#grid .card'), function (c) { return c.title || ''; });
        R.checks.homeStaysEmptyAfterLogout = R.notes.gridAfterLate === 0;
        R.checks.noStreamAfterLogout = R.notes.esAfter === R.notes.esBefore;
        return true;
      });
  }

  // ---- ② 06-F1 / 06-F4：编辑对话框 + 改密表单，然后退出登录 ----------------
  function logoutClearsFormsCheck() {
    return login()
      .then(function () { return waitFor('首页卡片出现（表单那条）', function () { return cardCount() >= 2; }, 30000); })
      .then(function () {
        window.location.hash = '#/n/' + IDA;
        return waitFor('详情页打开（表单那条）', function () {
          return shown('view-detail') && textOf('detail-name') === CFG.nodeName;
        }, 30000);
      })
      .then(function () { return waitFor('编辑按钮可用', function () { return node('detail-edit') && !node('detail-edit').disabled; }, 15000); })
      .then(function () {
        // 延迟目标卡片：F3 要验"迟到的 /ping 会不会把它重建出来"，所以先证明
        // 它**本来**建得出来。
        return waitFor('延迟目标卡片渲染出来', function () { return childrenOf('lat-targets') > 0; }, 20000);
      })
      .then(function () {
        R.notes.latTargetsOnDetail = childrenOf('lat-targets');
        node('detail-edit').click();
        return waitFor('编辑对话框打开', function () { return node('dlg-node').open === true; }, 15000);
      })
      .then(function () {
        R.notes.noteBefore = node('node-note').value;
        R.notes.tagsBefore = node('node-tags').value;
        node('node-cancel').click();
        return waitFor('编辑对话框关闭', function () { return node('dlg-node').open === false; }, 15000);
      })
      .then(function () {
        window.location.hash = '#/settings/security';
        return waitFor('安全栏就绪', function () { return shown('view-settings') && paneShown('security'); }, 30000);
      })
      .then(function () {
        // 与用户逐字输入等价：三个密码框里就是明文，而且**不提交**（审计 06-F1 的
        // 触发条件之一：填了不提交，或提交失败）。
        node('pw-current').value = 'plain-old-secret';
        node('pw-new').value = 'plain-new-secret-1';
        node('pw-new2').value = 'plain-new-secret-1';
        R.checks.passwordFormsFilled =
          node('pw-current').value.length > 0 && node('pw-new').value.length > 0 && node('pw-new2').value.length > 0;
        R.notes.loginUserBefore = node('login-user').value;
        node('btn-logout').click();
        return waitFor('退出登录回到登录页（表单那条）', function () { return shown('view-login'); }, 30000);
      })
      .then(function () {
        R.notes.pwAfter = [node('pw-current').value, node('pw-new').value, node('pw-new2').value];
        R.notes.noteAfter = node('node-note').value;
        R.notes.loginUserAfter = node('login-user').value;
        R.checks.logoutClearsPasswordForms =
          node('pw-current').value === '' && node('pw-new').value === '' && node('pw-new2').value === '';
        R.checks.logoutClearsNodeForm = R.notes.noteAfter === '';
        R.checks.logoutClearsLoginUser = R.notes.loginUserAfter === '';
        return true;
      });
  }

  // ---- ③ 06-F6 / 07-发现 2：进设置页后立刻退出登录（四条在飞链）------------
  function settingsInFlightCheck() {
    return login()
      .then(function () { return waitFor('首页卡片出现（设置页那条）', function () { return cardCount() >= 2; }, 30000); })
      .then(function () {
        R.notes.nodesMark = doneOf('/api/v1/nodes');
        R.notes.auditMark = doneOf('/api/v1/audit');
        R.notes.settingsMark = doneOf('/api/v1/settings');
        R.notes.telegramMark = doneOf('/api/v1/settings/telegram');
        delayMap['/api/v1/nodes'] = 2500;
        delayMap['/api/v1/audit'] = 2500;
        delayMap['/api/v1/settings'] = 2500;
        delayMap['/api/v1/settings/telegram'] = 2500;
        window.location.hash = '#/settings/notify';
        return waitFor('设置页打开（通知栏）', function () { return shown('view-settings') && paneShown('notify'); }, 30000);
      })
      .then(function () { return sleep(250); })   // 四条请求已经在路上
      .then(function () {
        node('btn-logout').click();
        return waitFor('退出登录回到登录页（设置页那条）', function () { return shown('view-login'); }, 30000);
      })
      .then(function () { return waitDone('/api/v1/nodes', R.notes.nodesMark, '设置页那条节点列表落地'); })
      .then(function () { return waitDone('/api/v1/settings', R.notes.settingsMark, '设置页那条设置响应落地'); })
      .then(function () { return waitDone('/api/v1/settings/telegram', R.notes.telegramMark, 'Telegram 那条响应落地'); })
      .then(function () { return waitDone('/api/v1/audit', R.notes.auditMark, '操作记录那条响应落地'); })
      .then(function () { return sleep(600); })
      .then(function () {
        delayMap = {};
        R.notes.nodesListChildren = childrenOf('nodes-list');
        R.notes.auditBodyChildren = childrenOf('audit-body');
        R.notes.tgChatAfter = node('tg-chat') ? node('tg-chat').value : '';
        R.notes.serverInfoAfter = textOf('server-info');
        R.checks.settingsNodesStaysEmpty = R.notes.nodesListChildren === 0;
        R.checks.auditStaysEmpty = R.notes.auditBodyChildren === 0;
        R.checks.telegramPrefsStayEmpty = R.notes.tgChatAfter === '';
        R.checks.serverInfoStaysEmpty = R.notes.serverInfoAfter === '';
        return true;
      });
  }

  // ---- ④ 06-F3：进详情页后立刻回首页（/traffic 与 /ping 还在路上）----------
  function detailInFlightCheck() {
    return login()
      .then(function () { return waitFor('首页卡片出现（详情页那条）', function () { return cardCount() >= 2; }, 30000); })
      .then(function () {
        R.notes.pingMark = doneOf('/api/v1/nodes/' + IDA + '/ping');
        R.notes.trafficMark = doneOf('/api/v1/nodes/' + IDA + '/traffic');
        delayMap['/api/v1/nodes/' + IDA + '/ping'] = 3000;
        delayMap['/api/v1/nodes/' + IDA + '/traffic'] = 3000;
        window.location.hash = '#/n/' + IDA;
        return waitFor('详情页打开（迟到那条）', function () {
          return shown('view-detail') && textOf('detail-name') === CFG.nodeName;
        }, 30000);
      })
      .then(function () { return sleep(300); })
      .then(function () {
        window.location.hash = '#/';
        return waitFor('回到首页（离开详情页）', function () { return shown('view-home'); }, 15000);
      })
      .then(function () {
        // 离开之后 closeDetail 已经把那一块清空（renderLatToggles([])）与画布清成
        // 「暂无数据」：从这里开始计数，迟到的响应再画一笔/再建一个节点都算数。
        R.notes.latTargetsAfterLeave = childrenOf('lat-targets');
        R.notes.trafficFramesMark = frameCount('chart-traffic');
        return waitDone('/api/v1/nodes/' + IDA + '/ping', R.notes.pingMark, '详情页那条 /ping 落地');
      })
      .then(function () { return waitDone('/api/v1/nodes/' + IDA + '/traffic', R.notes.trafficMark, '详情页那条 /traffic 落地'); })
      .then(function () { return sleep(700); })
      .then(function () {
        delayMap = {};
        R.notes.latTargetsAfterLatePing = childrenOf('lat-targets');
        R.notes.trafficFramesAfter = frameCount('chart-traffic');
        R.checks.latePingDoesNotRebuildDetail = R.notes.latTargetsAfterLatePing === 0;
        R.checks.lateTrafficDoesNotRedraw = R.notes.trafficFramesAfter === R.notes.trafficFramesMark;
        return true;
      });
  }

  // ---- ⑤ 07-发现 3：畸形设置地址（把链接发给已登录的管理员）---------------
  function malformedHashCheck() {
    R.notes.errsBeforeMalformed = R.errs.length;
    window.location.hash = '#/settings/%E0%A4%A';
    return sleep(900)
      .then(function () {
        R.notes.malformedView = shown('view-settings') ? 'settings'
          : (shown('view-login') ? 'login' : (shown('view-home') ? 'home' : '?'));
        R.notes.loginErrorText = textOf('login-error');
        R.notes.errsAfterMalformed = R.errs.length;
        R.checks.malformedHashKeepsSettings = R.notes.malformedView === 'settings';
        R.checks.malformedHashNoNewErrors = R.notes.errsAfterMalformed === R.notes.errsBeforeMalformed;
        window.location.hash = '#/settings/audit';
        return waitFor('合法栏名仍然能打开', function () { return shown('view-settings') && paneShown('audit'); }, 15000);
      })
      .then(function () {
        R.checks.legalHashStillWorks = paneShown('audit');
        return true;
      });
  }

  // ---- ⑥ 07-发现 1：会话在别处失效（改密码）→ 实时流 401 → 必须复查身份 ----
  function streamRevokedCheck() {
    window.location.hash = '#/';
    return waitFor('回到首页（断流那条）', function () { return shown('view-home'); }, 15000)
      .then(function () { return waitFor('首页卡片出现（断流那条）', function () { return cardCount() >= 2; }, 30000); })
      .then(function () {
        R.notes.cardsBeforeRevoke = cardCount();
        R.notes.sessionFetchesBeforeRevoke = R.notes.sessionFetches || 0;
        // 让 Go 侧用**另一个会话**改密码：服务端会注销这条会话并撤销全部管理员实时流。
        return rawFetch('/__revoke', { method: 'POST' });
      })
      .then(function (res) {
        if (!res || !res.ok) throw new Error('在别处注销会话失败: ' + (res && res.status));
        // 诊断：会话是不是真的没了？用 /nodes 探一下（故意不碰 /session 的计数，
        // 那个计数是"页面自己有没有复查身份"的证据）。
        return rawFetch('/api/v1/nodes').then(function (r) {
          R.notes.nodesStatusAfterRevoke = r.status;
          return true;
        });
      })
      .then(function () {
        // 会话没了 ⇒ 重连吃 401 ⇒ EventSource 永久失败（CLOSED，不再重连）。
        // 页面必须自己复查一次身份：这是发现 1 唯一的行为变化。
        // 这里**不**让超时冒成 fatal：红了也要把诊断值带回去（见下面那组 notes）。
        return waitFor('身份失效被复查到（回到登录页）', function () { return shown('view-login'); }, 25000)
          .then(function () { return true; }, function () { return false; });
      })
      .then(function (ok) { return sleep(300).then(function () { return ok; }); })
      .then(function (ok) {
        R.notes.loginViewAfterRevoke = ok === true;
        R.notes.liveTextAfterRevoke = textOf('live-text');
        R.notes.esStatesAfterRevoke = esStates();
        R.notes.cardsAfterRevoke = cardCount();
        R.notes.sessionFetchesAfterRevoke = R.notes.sessionFetches || 0;
        R.checks.loginViewAfterRevoke = ok === true;
        R.checks.revokedSessionClearsHome = R.notes.cardsAfterRevoke === 0;
        R.checks.revokedSessionRechecksIdentity =
          R.notes.sessionFetchesAfterRevoke > R.notes.sessionFetchesBeforeRevoke;
        return true;
      });
  }

  function run() {
    return waitFor('页面脚本就绪', function () { return !!node('view-login') && !!node('form-login'); }, 30000)
      // ① 的钩子必须在登录**之前**装好：那条 /api/v1/nodes 是登录成功后立刻发出的。
      .then(function () {
        R.notes.nodesMark = doneOf('/api/v1/nodes');
        R.notes.esBefore = R.notes.esCreated || 0;
        delayMap['/api/v1/nodes'] = 2500;
        return true;
      })
      .then(login)
      .then(inFlightHomeCheck)
      .then(logoutClearsFormsCheck)
      .then(settingsInFlightCheck)
      .then(detailInFlightCheck)
      .then(malformedHashCheck)
      .then(streamRevokedCheck);
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

// probeLabelHarnessJS 是 07-发现 4 的自检脚本。
//
// 只做一件事：把"管理员那一帧"与"访客那一帧"的**探测行 title 与文本**取下来对比。
// 期望值在 Go 侧给（CFG.private[0] 是那个派生 label 的地址，CFG.nodes['probe-target']
// 是目标 id）。
const probeLabelHarnessJS = `(function () {
  'use strict';
  var CFG = window.__TZCFG || {};
  var WANT_HOST = (CFG.private || [])[0] || '';
  var TARGET_ID = (CFG.nodes || {})['probe-target'] || 0;

  var R = { errs: [], steps: [], fatal: '', checks: {}, notes: {}, fetchLog: [] };
  window.__PROBELABELRESULT = R;

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
  // 探测行那一格：卡片上的毫秒数（title 就是目标的名字）
  function probeCell() { return document.querySelector('#grid .card .line-num'); }
  function probeTitle() { var c = probeCell(); return c ? (c.title || '') : ''; }
  function probeText() { var c = probeCell(); return c ? c.textContent : ''; }

  var rawFetch = window.fetch.bind(window);
  var doneCount = {};
  function pathOf(url) {
    var s = String(url || '');
    var q = s.indexOf('?'); if (q >= 0) s = s.slice(0, q);
    var h = s.indexOf('#'); if (h >= 0) s = s.slice(0, h);
    var i = s.indexOf('/api/');
    return i >= 0 ? s.slice(i) : s;
  }
  window.fetch = function (input, init) {
    var url = typeof input === 'string' ? input : ((input && input.url) || '');
    var p = pathOf(url);
    R.fetchLog.push(p);
    return rawFetch(input, init).then(function (res) {
      doneCount[p] = (doneCount[p] || 0) + 1;
      return res;
    });
  };
  function doneOf(p) { return doneCount[p] || 0; }

  // isAdmin 用"顶栏那三个管理员入口在不在"判身份。
  //
  // 不能拿"首页可见"当"已登录"：这条用例的现场**开着访客开关**，未登录的浏览器
  // 会直接落在只读面板上（view-home 一开始就是可见的，见 refreshSession 的访客分支），
  // 拿它当判据的话整段流程会以访客身份跑完 —— 那样"降级之前"的 title 本来就已经是
  // 「目标 #id」，这条用例会以"前置条件不成立"红，而实际是脚本没登录。
  // applyAdminChrome 在未登录时把这三个按钮**从 DOM 里摘掉**（不是 hidden），
  // 所以"取不到 btn-logout"就是访客。
  function isAdmin() { return !!node('btn-logout'); }
  function login() {
    return waitFor('页面出现登录页或只读面板', function () { return shown('view-login') || shown('view-home'); }, 30000)
      .then(function () {
        if (isAdmin()) return true;                      // 已经是管理员
        if (!shown('view-login')) {                      // 只读面板：先点「管理员登录」
          node('btn-login-entry').click();
          return waitFor('登录页出现（从只读面板过来）', function () { return shown('view-login'); }, 15000);
        }
        return true;
      })
      .then(function () {
        if (isAdmin()) return true;
        node('login-user').value = CFG.user;
        node('login-pass').value = CFG.pass;
        node('login-submit').click();
        return waitFor('登录后进入应用（管理员）', isAdmin, 30000);
      });
  }

  function run() {
    return waitFor('页面脚本就绪', function () { return !!node('view-login') && !!node('form-login'); }, 30000)
      .then(login)
      .then(function () { return waitFor('首页卡片出现', function () { return document.querySelectorAll('#grid .card').length >= 1; }, 30000); })
      // ⚠️ 必须等**管理员那一帧**，不能只等"title 非空"：这条用例的现场开着访客开关，
      // 页面在登录**之前**就已经落在只读面板上，那一份 /overview 里"名字留空、由 host
      // 兜底"的目标被服务端抹成空 label（guestTargetLabel）⇒ 探测行的 title 是
      // 「目标 #id」。只等"title 非空"会被那一帧立刻满足（实测：titleBefore =
      // "目标 #1"、行文本 "25 ms"，而期望 "9.9.9.9"），于是 Go 侧的前置断言必红 ——
      // 而它红的原因不是产品提前降级，是**基准帧取错了**。这里等的是"管理员那一帧"，
      // 期望值由 Go 侧通过 CFG.private[0] 给（就是那个派生 label 的地址）。
      .then(function () { return waitFor('管理员那一帧出现（探测行 title 是地址）', function () { return probeTitle() === WANT_HOST; }, 30000); })
      // ⚠️ 地址要先归一到 #/：上面是从只读面板点「管理员登录」进来的，地址栏停在
      // #/login（app.js 的 enterApp 只在地址为空时才补 #/）。C6 修好之后，身份失效
      // 会让页面**按当前地址**重新渲染一次 —— 访客停在 #/login 上本来就该看到登录页
      // （与手动刷新同一地址的结果一致），那样这条用例要看的"那一格跟着访客那一份
      // 重建"就永远不会发生了。所以先把地址归零，让降级之后的页面落在首页面板上。
      //
      // 诚实记一笔：C6 修好之后，这一格是被"整张卡片按访客身份重建"带对的，而**不再**
      // 经过 renderProbeLine 的指纹（卡片都被 resetHome 摘掉、重新建过了）。也就是说
      // "指纹里必须带 label"那条守卫**不再由这条用例覆盖** —— 它现在的判别力是"降级
      // 之后 title 必须是访客该看到的那一个"。要重新覆盖指纹，需要一个"卡片还在、
      // 只收到脱敏 label"的现场，而那正是 C6 修掉的那个形态。
      .then(function () {
        window.location.hash = '#/';
        return waitFor('停在首页且 hash = #/', function () {
          return shown('view-home') && window.location.hash === '#/';
        }, 15000);
      })
      .then(function () {
        R.notes.titleBefore = probeTitle();
        R.notes.textBefore = probeText();
        // 让 Go 侧用**另一个会话**改密码：这条会话与它的实时流一起失效；而
        // 「允许访客查看」开着，所以重连会以**访客**身份成功（200）——页面不会收到
        // CLOSED，也就不会清空，它会继续以访客身份收脱敏数据（发现 4 的场景）。
        return rawFetch('/__revoke', { method: 'POST' });
      })
      .then(function (res) {
        if (!res || !res.ok) throw new Error('在别处注销会话失败: ' + (res && res.status));
        // 降级的证据：卡片 title 上的来源 IP 消失了（访客那一份没有 observed_ip）。
        return waitFor('身份已经降级（卡片 title 上不再有来源 IP）', function () {
          var c = document.querySelector('#grid .card');
          var ip = (CFG.private || [])[1] || '';
          return !!c && ip !== '' && (c.title || '').indexOf(ip) < 0;
        }, 40000);
      })
      .then(function () {
        // 回一次详情页再回首页：setView('home') → syncOverviewTimer → startOverview
        // 会立刻取一次 /overview —— 身份已经是访客，那一份里的派生 label 被服务端
        // 抹成了空串（host 键也不下发）。
        window.location.hash = '#/n/' + CFG.nodeID;
        return waitFor('详情页打开（降级之后）', function () { return shown('view-detail'); }, 30000);
      })
      .then(function () {
        window.location.hash = '#/';
        return waitFor('回到首页（降级之后）', function () { return shown('view-home'); }, 15000);
      })
      .then(function () {
        // 等那一格真的跟着访客那一份走（上限 15 秒：红了不会一直挂着）。
        var want = '目标 #' + TARGET_ID;
        return waitFor('探测行跟着访客那一份重建', function () { return probeTitle() === want; }, 15000)
          .then(function () { return true; }, function () { return false; });
      })
      .then(function (ok) {
        R.notes.titleAfter = probeTitle();
        R.notes.textAfter = probeText();
        R.checks.probeTitleFollowedGuestLabel =
          ok === true && R.notes.titleAfter === ('目标 #' + TARGET_ID);
        // 读数一个字符都没变：这条用例的区分度就建立在这上面。
        R.checks.probeNumbersUnchanged = R.notes.textAfter === R.notes.textBefore;
        R.notes.wantHost = WANT_HOST;
        return true;
      });
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
