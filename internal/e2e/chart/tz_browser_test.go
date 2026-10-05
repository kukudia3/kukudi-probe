package chart

import (
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"probe/internal/e2e/harness"
	"sort"
	"strings"
	"testing"
	"time"

	// 内嵌时区数据库：Windows 上没有系统 tzdata，time.LoadLocation("Asia/Tokyo")
	// 会直接失败，而这条用例的全部意义就是"服务端时区 != 浏览器时区"。
	// （生产侧由 cmd/probe-server 自己 import，这里只是让测试二进制也能加载。）
	_ "time/tzdata"

	"probe/internal/store"
)

// 跨时区端到端：服务端按 --timezone 切天，浏览器却在自己那个时区里，
// 页面上每一个时间都必须仍然按**服务端时区**渲染。
//
// 为什么必须用真浏览器 + 真服务端：
//   - 渲染路径整条都在浏览器里（Intl.DateTimeFormat 的实际行为、canvas 上真正
//     fillText 出来的字、对话框回填的 value），静态断言只能证明"代码里有 Intl"；
//   - 数据侧的切天口径在服务端（store.CycleStart / api_traffic.go 的 startDay），
//     mock 一份假数据等于把要验证的那一半也一起 mock 掉了。
//
// 所以这里既不 mock 接口也不 mock 数据：起一个**真服务端**（--timezone 指定成
// 与浏览器不同的时区），接口原样反代给浏览器，只在 index.html 里注入一段自检脚本
// （收集 __errs、钩 canvas 上的 fillText、驱动真实的点击/输入）。
// 断言用的期望值由 **Go 的 time 包**算出来 —— Intl 与 zoneinfo 是两套彼此独立的
// 实现，用后者核对前者才说明"渲染用的确实是服务端时区"，而不是两边一起错。
const (
	// 服务端时区（TestZone）搬到了 harness 包：跨时区的几条用例（tz / axis /
	// cardstate）与轴标签截图用例共用同一个"服务端时区 ≠ 浏览器时区"的前提，
	// 而它们现在分在不同的包里。原来的理由照旧：故意选一个与本机浏览器
	// （Asia/Shanghai，UTC+8）不同的 —— 两边一样时，页面上的时间无论按哪一边
	// 渲染都长得一样，测了等于没测。

	// 到期日：手写死的输入。东九区下这一天的零点是 2026-09-30T15:00:00Z，
	// 对应的 epoch 见 tzTestExpiresEpoch（手算，见下面用例里的核对）。
	tzTestDate = "2026-10-01"
	// 1790780400 = 2026-10-01T00:00:00+09:00。
	// 旧代码（浏览器本地零点写、UTC 读）在 UTC+8 下会存成 1790784000
	// （2026-10-01T00:00+08:00），两者差 3600 —— 这条断言分得开对错。
	tzTestExpiresEpoch = int64(1790780400)
)

// tzResult 是浏览器回传的全部观测值。断言全部在 Go 这边做：
// 期望值要用 Go 的 time 包按服务端时区算，浏览器只负责"把它看到的说出来"。
type tzResult struct {
	Errs      []string `json:"errs"`
	Fatal     string   `json:"fatal"`
	Steps     []string `json:"steps"`
	BrowserTZ string   `json:"browserTZ"`

	// 「更新于」：首帧取样 + 变化轨迹 + SSE 给的原始 ts。
	UpdatedAfterNodesFetch *string  `json:"updatedAfterNodesFetch"`
	UpdatedLogAtSSE        []string `json:"updatedLogAtSSE"`
	UpdatedLog             []string `json:"updatedLog"`
	SSETSList              []int64  `json:"sseTSList"`
	NodesServerTime        string   `json:"nodesServerTime"`

	ServerInfoTZ  string   `json:"serverInfoTZ"`
	AuditHeader   string   `json:"auditHeader"`
	AuditRendered []string `json:"auditRendered"`
	AuditRawTS    []int64  `json:"auditRawTS"`

	Traffic tzChart `json:"traffic"`
	CPU     tzChart `json:"cpu"`

	HoverTraffic tzHover `json:"hoverTraffic"`
	HoverCPU     tzHover `json:"hoverCpu"`

	DialogRounds []tzRound `json:"dialogRounds"`
}

// tzChart 是一张图上"画出来的字"的观测值。
//
// Labels 只收 X 轴那一行（画在绘图区下方），并且带上画它的像素坐标 ——
// Go 那边按坐标反推出它代表哪一刻，再用 time 包格式化，两边才能逐字比对。
type tzChart struct {
	Drawn     int       `json:"drawn"`
	Width     float64   `json:"width"`
	Height    float64   `json:"height"`
	PlotLeft  float64   `json:"plotLeft"`
	PlotRight float64   `json:"plotRight"`
	T0        float64   `json:"t0"`
	T1        float64   `json:"t1"`
	Points    []int64   `json:"points"`
	Labels    []tzLabel `json:"labels"`
}

type tzLabel struct {
	Text string  `json:"text"`
	X    float64 `json:"x"`
	Y    float64 `json:"y"`
}

type tzHover struct {
	TargetTS float64  `json:"targetTS"`
	Rows     []string `json:"rows"`
	Time     string   `json:"time"`
}

type tzRound struct {
	Round     int    `json:"round"`
	Before    string `json:"before"`
	Readback  string `json:"readback"`
	Stored    int64  `json:"stored"`
	Remaining int64  `json:"remaining"`
}

// TestTimezoneRenderingInRealBrowser 是本仓库里唯一一条"跨时区"的端到端用例。
func TestTimezoneRenderingInRealBrowser(t *testing.T) {
	chrome := harness.FindChrome()
	if chrome == "" {
		t.Skip("找不到 Chrome：设置 PROBE_CHROME 或把它装到默认位置后这条用例会自动跑起来")
	}
	loc, err := time.LoadLocation(harness.TestZone)
	if err != nil {
		t.Fatalf("加载时区 %s: %v", harness.TestZone, err)
	}
	now := time.Now()
	_, serverOff := now.In(loc).Zone()
	_, browserOff := now.In(time.Local).Zone()
	if serverOff == browserOff {
		t.Skipf("本机时区与服务端时区 %s 的偏移相同（%d 秒），跨时区渲染无从验证：请换一台非 UTC+9 的机器或改 tzTestZone", harness.TestZone, serverOff)
	}

	// 手工核对的锚点：2026-10-01T00:00:00+09:00 就是 1790780400。
	// 写死它有两个用处：一是把"手算的期望值"钉在用例里（不依赖运行时环境），
	// 二是防止上面那个常量被谁"顺手改一改"之后整条断言跟着一起漂。
	if got := time.Date(2026, 10, 1, 0, 0, 0, 0, loc).Unix(); got != tzTestExpiresEpoch {
		t.Fatalf("手算的期望值对不上：time.Date(2026-10-01, %s) = %d，期望 %d", harness.TestZone, got, tzTestExpiresEpoch)
	}

	logs := &harness.CaptureHandler{}
	h := harness.StartServerAt(t, "127.0.0.1:0", filepath.Join(t.TempDir(), "probe.db"), loc, slog.New(logs), nil)
	br := harness.NewBrowser(t, "http://"+h.Addr)
	harness.SetupAdmin(t, br, harness.WaitSetupCode(t, logs))
	nodeID, _ := harness.CreateNodeViaAPI(t, br, "tz-01")

	wantTrafficDays := seedTrafficDays(t, h, nodeID, loc)
	wantCPUTicks := harness.SeedCPUSamples(t, h, nodeID)

	// 反代 + 注入：接口一条都不 mock，浏览器拿到的仍是真服务端的真数据。
	proxy := harness.NewTZProxy(t, "http://"+h.Addr, harness.HarnessConfig{
		NodeID:   nodeID,
		NodeName: "tz-01",
		Date:     tzTestDate,
		User:     "admin",
		Pass:     "a-very-good-password",
	})
	mock := harness.NewMockServer(t, proxy)

	raw := harness.RunChromeForResult(t, chrome, mock.URL+"/", proxy.Result, 90*time.Second, "")

	var res tzResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("解析浏览器回传的结果: %v\n原始内容：%s", err, harness.Tail(string(raw), 600))
	}
	if res.Fatal != "" {
		t.Fatalf("浏览器里的自检流程没跑完：%s", res.Fatal)
	}
	// 每个场景都不许有 JS 报错：白屏、绑定失败、null 取属性全都落在这里。
	if len(res.Errs) != 0 {
		t.Fatalf("浏览器里有 %d 条 JS 报错：%v", len(res.Errs), res.Errs)
	}

	t.Logf("浏览器时区 = %s；服务端时区 = %s（--timezone）", res.BrowserTZ, res.ServerInfoTZ)
	if res.ServerInfoTZ != harness.TestZone {
		t.Errorf("设置页「服务器信息 · 时区」= %q，期望 %q", res.ServerInfoTZ, harness.TestZone)
	}

	checkSummaryClock(t, res, loc)
	checkAuditClock(t, res, loc)
	checkTrafficAxis(t, res, loc, wantTrafficDays)
	checkCPUAxis(t, res, loc, wantCPUTicks)
	checkHovers(t, res, loc)
	checkExpiresRoundTrip(t, res, loc)
}

// checkSummaryClock 验证总览条「更新于」只认服务端时钟。
//
// 为什么这条要单独测：改之前首帧用的是浏览器 Date.now()、之后用 SSE 的 payload.ts
// —— 两个时钟源来回切，两端差几分钟页面就会跳一下。现在首帧必须是 —（明确占位），
// 之后的每一个值都必须等于"某个服务端 ts 在服务端时区下的写法"。
func checkSummaryClock(t *testing.T, res tzResult, loc *time.Location) {
	t.Helper()
	if res.UpdatedAfterNodesFetch == nil {
		t.Fatal("没有取到首帧渲染后的「更新于」：自检脚本没钩上 /api/v1/nodes")
	}
	if *res.UpdatedAfterNodesFetch != "—" {
		t.Errorf("首帧「更新于」= %q，期望 —（拿浏览器时钟冒充服务端时间就是这次要修的 bug）",
			*res.UpdatedAfterNodesFetch)
	}
	if len(res.SSETSList) == 0 {
		t.Fatal("一帧 SSE 都没收到：总览条的「更新于」没有服务端 ts 可用")
	}
	t.Logf("首帧原始 ts = 无（只显示占位 —）；/nodes 里服务端自报的时间 = %s；SSE 原始 ts = %v",
		res.NodesServerTime, res.SSETSList)

	// 每一个「更新于 X」都必须是某个 SSE ts 在服务端时区下的写法。
	// 浏览器时区比服务端慢 1 小时，用浏览器时钟渲染出来的值在这里必然对不上。
	allowed := map[string]int64{}
	for _, ts := range res.SSETSList {
		allowed["更新于 "+time.Unix(ts, 0).In(loc).Format("15:04:05")] = ts
	}
	// 第一条轨迹必须是占位 —：这就是"首帧没有服务端 ts 时不许拿浏览器时钟冒充"。
	// （给 textContent 赋值一定会产生一次 childList 变更，所以这条轨迹抓得到。）
	if len(res.UpdatedLog) == 0 || res.UpdatedLog[0] != "—" {
		t.Errorf("「更新于」的第一条轨迹 = %v，期望第一条是 —（首帧只能用占位，不能用浏览器时钟）", res.UpdatedLog)
	}
	firstClock := ""
	for _, v := range res.UpdatedLog {
		if !strings.HasPrefix(v, "更新于 ") {
			continue
		}
		if firstClock == "" {
			firstClock = v
		}
		if _, ok := allowed[v]; !ok {
			t.Errorf("「更新于」出现过 %q：它不是任何一帧服务端 ts 在 %s 下的写法（合法的只有 %v）",
				v, harness.TestZone, sortedKeys(allowed))
		}
	}
	if firstClock == "" {
		t.Fatalf("「更新于」一次都没有按服务端 ts 渲染过：轨迹 = %v", res.UpdatedLog)
	}
	want := "更新于 " + time.Unix(res.SSETSList[0], 0).In(loc).Format("15:04:05")
	if firstClock != want {
		t.Errorf("第一帧 SSE 之后「更新于」= %q，期望 %q（= 服务端 ts %d 在 %s 下的写法）",
			firstClock, want, res.SSETSList[0], harness.TestZone)
	}
	t.Logf("「更新于」的完整轨迹 = %v", res.UpdatedLog)
	t.Logf("第一帧 SSE 的原始 ts = %d → 页面显示 %q（Go 用 %s 算出来也是这个）",
		res.SSETSList[0], firstClock, harness.TestZone)
}

// checkAuditClock 逐行核对操作记录的时间：页面上写的，必须等于后端下发的那个 ts
// 在服务端时区下的写法。
//
// 为什么用 Go 算期望值而不是在 JS 里再算一遍：那样只是"同一套 Intl 自己跟自己对"，
// 证明不了服务端时区真的被用上了。这里两套实现（浏览器 Intl / Go zoneinfo）
// 逐秒比对，才说明用户看到的时间能直接跟服务器上的 date、日志对上。
func checkAuditClock(t *testing.T, res tzResult, loc *time.Location) {
	t.Helper()
	if !strings.Contains(res.AuditHeader, harness.TestZone) {
		t.Errorf("操作记录表头 = %q，没有写明时间按哪个时区（必须写出来，否则用户会默认它是自己浏览器的时间）",
			res.AuditHeader)
	}
	if len(res.AuditRendered) == 0 || len(res.AuditRawTS) == 0 {
		t.Fatal("操作记录是空的：这一栏没有渲染出来，时间对不对无从验证")
	}
	n := len(res.AuditRendered)
	if len(res.AuditRawTS) < n {
		n = len(res.AuditRawTS)
	}
	for i := 0; i < n; i++ {
		ts := res.AuditRawTS[i]
		wantServer := time.Unix(ts, 0).In(loc).Format("01-02 15:04:05")
		wantBrowser := time.Unix(ts, 0).In(time.Local).Format("01-02 15:04:05")
		got := res.AuditRendered[i]
		t.Logf("审计第 %d 行：后端 ts=%d → 页面 %q；服务端时区算法 %q；浏览器本地算法 %q",
			i+1, ts, got, wantServer, wantBrowser)
		if got != wantServer {
			t.Errorf("审计第 %d 行时间 = %q，期望 %q（ts=%d 在 %s 下）", i+1, got, wantServer, ts, harness.TestZone)
		}
		if wantServer == wantBrowser {
			t.Logf("  （这条恰好两种算法一样，不具区分度）")
		}
	}
	t.Logf("表头 = %q", res.AuditHeader)
}

// checkTrafficAxis 核对「近 7 天流量」的日轴。
//
// 轴上的字是按**绝对时间网格**锚定的（chart.js 里 first = ceil(t0/step)*step，
// step 恒为 86400 的整数倍），所以这里按像素坐标把锚点反推出来，
// 再用 Go 按服务端时区格式化 —— 页面上的日期必须与"这一刻在服务端时区是哪一天"一致。
func checkTrafficAxis(t *testing.T, res tzResult, loc *time.Location, wantDays []string) {
	t.Helper()
	if len(res.Traffic.Labels) == 0 {
		t.Fatalf("流量图上一个字都没量到（画布 %vx%v，点了 %d 个）",
			res.Traffic.Width, res.Traffic.Height, len(res.Traffic.Points))
	}
	seen := map[string]bool{}
	for _, lb := range res.Traffic.Labels {
		anchor := snapToGrid(anchorTS(res.Traffic, lb.X), 86400)
		want := time.Unix(anchor, 0).In(loc).Format("01-02")
		seen[lb.Text] = true
		if lb.Text != want {
			t.Errorf("流量日轴标签 %q（x=%.1f）反推出的锚点 ts=%d 在 %s 下应当是 %q —— 说明轴上的日期不是按服务端时区渲染的",
				lb.Text, lb.X, anchor, harness.TestZone, want)
		}
	}
	t.Logf("流量日轴的标签：%v（后端给的 7 个服务端日是 %v）", sortedKeys(seen), wantDays)

	// 7 天里至少要有 4 个日期被标出来，否则"轴对不对"这件事没被真正验证到。
	if len(seen) < 4 {
		t.Errorf("流量日轴只画出了 %d 个日期标签（%v），太少，不足以验证日轴", len(seen), sortedKeys(seen))
	}
	// 轴的右端必须落到"最后一个服务端日"附近：labelFits 会把越界的标签丢掉，
	// 所以允许它落在最后两天里，但不允许整条轴停在更早的地方。
	last := wantDays[len(wantDays)-1]
	prev := wantDays[len(wantDays)-2]
	if !seen[last] && !seen[prev] {
		t.Errorf("流量日轴上既没有 %s 也没有 %s（实际画出 %v）：轴与后端给的日期对不上",
			prev, last, sortedKeys(seen))
	}
}

// checkCPUAxis 核对资源图的 X 轴（HH:MM）。
//
// 这一条是**真正有区分度**的那一条：日轴/资源轴的锚点都钉在绝对时间网格上
// （同一批 ts），但 HH:MM 这种写法直接暴露渲染时区 —— 服务端 UTC+9、浏览器 UTC+8，
// 同一个锚点按两边渲染会正好差一个小时，谁渲染的一眼可辨。
func checkCPUAxis(t *testing.T, res tzResult, loc *time.Location, wantTicks []int64) {
	t.Helper()
	if len(res.CPU.Labels) == 0 {
		t.Fatalf("CPU 图上一个字都没量到（画布 %vx%v，点了 %d 个）",
			res.CPU.Width, res.CPU.Height, len(res.CPU.Points))
	}
	bad := 0
	for _, lb := range res.CPU.Labels {
		anchor := snapToGrid(anchorTS(res.CPU, lb.X), 60)
		want := time.Unix(anchor, 0).In(loc).Format("15:04")
		wantBrowser := time.Unix(anchor, 0).In(time.Local).Format("15:04")
		if lb.Text != want {
			bad++
			t.Errorf("CPU X 轴标签 %q（x=%.1f）反推出的锚点 ts=%d：%s 下应当是 %q，浏览器本地时区下才是 %q",
				lb.Text, lb.X, anchor, harness.TestZone, want, wantBrowser)
		}
	}
	if bad == 0 {
		t.Logf("CPU X 轴 %d 个标签全部是服务端时区（%s）的写法；后端曲线共 %d 个点（第一个 ts=%d）",
			len(res.CPU.Labels), harness.TestZone, len(wantTicks), wantTicks[0])
	}
}

// checkHovers 核对悬浮浮层：图上读到的那个点，浮层里写的时间必须是**服务端时区**下
// 那个点的写法。流量图这条尤其关键 —— 日轴上的点就是"服务端本地零点"，
// 按浏览器本地渲染会整整差一天（服务端 10-02 的点写成 10-01 23:00）。
func checkHovers(t *testing.T, res tzResult, loc *time.Location) {
	t.Helper()
	if res.HoverTraffic.Time == "" {
		t.Errorf("流量图的悬浮浮层没有量到时间行（rows=%v）", res.HoverTraffic.Rows)
	} else {
		ts := int64(res.HoverTraffic.TargetTS)
		want := time.Unix(ts, 0).In(loc).Format("01-02")
		wantBrowser := time.Unix(ts, 0).In(time.Local).Format("01-02")
		t.Logf("流量图悬浮：后端日点 ts=%d → 浮层 %q；服务端时区算法 %q；浏览器本地算法 %q",
			ts, res.HoverTraffic.Time, want, wantBrowser)
		if res.HoverTraffic.Time != want {
			t.Errorf("流量图悬浮浮层时间 = %q，期望 %q（ts=%d 在 %s 下）", res.HoverTraffic.Time, want, ts, harness.TestZone)
		}
		if want == wantBrowser {
			t.Errorf("这条悬浮用例不具区分度（两种时区都写成 %q），请换一个 ts", want)
		}
	}
	if res.HoverCPU.Time == "" {
		t.Errorf("CPU 图的悬浮浮层没有量到时间行（rows=%v）", res.HoverCPU.Rows)
	} else {
		ts := int64(res.HoverCPU.TargetTS)
		want := time.Unix(ts, 0).In(loc).Format("15:04")
		wantBrowser := time.Unix(ts, 0).In(time.Local).Format("15:04")
		t.Logf("CPU 图悬浮：点 ts=%d → 浮层 %q；服务端时区算法 %q；浏览器本地算法 %q",
			ts, res.HoverCPU.Time, want, wantBrowser)
		if res.HoverCPU.Time != want {
			t.Errorf("CPU 图悬浮浮层时间 = %q，期望 %q（ts=%d 在 %s 下）", res.HoverCPU.Time, want, ts, harness.TestZone)
		}
		if want == wantBrowser {
			t.Errorf("这条悬浮用例不具区分度（两种时区都写成 %q），请换一个 ts", want)
		}
	}
}

// checkExpiresRoundTrip 核对到期日：输入 2026-10-01 → 存下来的 epoch 必须是
// "服务端时区下 2026-10-01 的零点"，重新打开编辑框必须还是 2026-10-01，
// 连存三次不许漂移。
func checkExpiresRoundTrip(t *testing.T, res tzResult, loc *time.Location) {
	t.Helper()
	if len(res.DialogRounds) != 3 {
		t.Fatalf("只完成了 %d 轮「打开编辑 → 保存 → 重新打开」，期望 3 轮：%+v",
			len(res.DialogRounds), res.DialogRounds)
	}
	for _, r := range res.DialogRounds {
		t.Logf("第 %d 轮：打开时输入框 = %q；保存后库里的 expires_at = %d（剩余 %d 天）；重新打开 = %q",
			r.Round, r.Before, r.Stored, r.Remaining, r.Readback)
		if r.Stored != tzTestExpiresEpoch {
			t.Errorf("第 %d 轮保存后的 expires_at = %d，期望 %d（= %s 00:00:00 的 epoch）",
				r.Round, r.Stored, tzTestExpiresEpoch, harness.TestZone)
		}
		if r.Readback != tzTestDate {
			t.Errorf("第 %d 轮重新打开编辑框读到 %q，期望 %q —— 往返不恒等（每存一次就漂一天）",
				r.Round, r.Readback, tzTestDate)
		}
	}
	// 三轮必须完全一致：一次漂一天的话第二轮、第三轮就露馅了。
	first := res.DialogRounds[0]
	for _, r := range res.DialogRounds[1:] {
		if r.Stored != first.Stored || r.Readback != first.Readback {
			t.Errorf("第 %d 轮与第 1 轮不一致（expires_at %d vs %d，回读 %q vs %q）：日期在漂",
				r.Round, r.Stored, first.Stored, r.Readback, first.Readback)
		}
	}
	t.Logf("到期日往返恒等：输入 %q → expires_at=%d（%s）→ 重新打开仍是 %q；连存 3 次没有漂移",
		tzTestDate, first.Stored, time.Unix(first.Stored, 0).In(loc).Format(time.RFC3339), first.Readback)
}

// ---------------------------------------------------------------- 数据播种

// seedTrafficDays 写 7 天日流量（服务端时区下的今天与前 6 天），返回它们的 MM-DD。
//
// 直接写库而不是走接口：日流量是 Agent 上报累加出来的，没有"灌一天的量"这种接口。
// 这里要验的是**渲染**，不是统计口径，所以直接给 traffic_daily 塞行最直接。
func seedTrafficDays(t *testing.T, h *harness.Harness, nodeID int64, loc *time.Location) []string {
	t.Helper()
	ctx := context.Background()
	today := time.Now().In(loc)
	day := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, loc)
	out := make([]string, 0, 7)
	for i := 6; i >= 0; i-- {
		d := day.AddDate(0, 0, -i)
		if _, err := h.DB.Writer().ExecContext(ctx,
			`INSERT INTO traffic_daily (node_id, day, rx, tx) VALUES (?, ?, ?, ?)`,
			nodeID, store.FormatDay(d), int64(7-i)*1_000_000_000, int64(14-i)*1_000_000_000); err != nil {
			t.Fatalf("写入日流量 %s: %v", store.FormatDay(d), err)
		}
		out = append(out, d.Format("01-02"))
	}
	return out
}

// ---------------------------------------------------------------- 像素 → 时刻

// anchorTS 把"标签画在哪个像素上"反推回它代表的时刻。
//
// chart.js 的横坐标是线性映射：x(ts) = g.left + (ts-t0)/(t1-t0) * (w-left-right)，
// 标签画在 Math.round(x)+0.5 上，所以反推的误差只有半个像素（一个屏幕上不到一秒）。
// 再用 snapToGrid 吸附到刻度网格上，误差就被彻底消掉了。
func anchorTS(c tzChart, x float64) float64 {
	plotW := c.Width - c.PlotLeft - c.PlotRight
	if plotW <= 0 {
		return c.T0
	}
	return c.T0 + (x-c.PlotLeft)/plotW*(c.T1-c.T0)
}

// snapToGrid 把反推出来的时刻吸附到刻度网格（标签锚点恒为网格的整数倍：
// 分钟档是 60 秒的倍数，日轴是 86400 秒的倍数）。
func snapToGrid(ts, grid float64) int64 {
	return int64(ts/grid+0.5) * int64(grid)
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------- 反代 + 注入

// ---------------------------------------------------------------- 跑 Chrome

// ---------------------------------------------------------------- 浏览器里的自检脚本

// ---------------------------------------------------------------- mock 服务器
