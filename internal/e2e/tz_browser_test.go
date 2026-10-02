package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
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
	// 服务端时区。故意选一个与本机浏览器（Asia/Shanghai，UTC+8）不同的：
	// 两边一样时，页面上的时间无论按哪一边渲染都长得一样，测了等于没测。
	tzTestZone = "Asia/Tokyo" // UTC+9

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
	chrome := findChrome()
	if chrome == "" {
		t.Skip("找不到 Chrome：设置 PROBE_CHROME 或把它装到默认位置后这条用例会自动跑起来")
	}
	loc, err := time.LoadLocation(tzTestZone)
	if err != nil {
		t.Fatalf("加载时区 %s: %v", tzTestZone, err)
	}
	now := time.Now()
	_, serverOff := now.In(loc).Zone()
	_, browserOff := now.In(time.Local).Zone()
	if serverOff == browserOff {
		t.Skipf("本机时区与服务端时区 %s 的偏移相同（%d 秒），跨时区渲染无从验证：请换一台非 UTC+9 的机器或改 tzTestZone", tzTestZone, serverOff)
	}

	// 手工核对的锚点：2026-10-01T00:00:00+09:00 就是 1790780400。
	// 写死它有两个用处：一是把"手算的期望值"钉在用例里（不依赖运行时环境），
	// 二是防止上面那个常量被谁"顺手改一改"之后整条断言跟着一起漂。
	if got := time.Date(2026, 10, 1, 0, 0, 0, 0, loc).Unix(); got != tzTestExpiresEpoch {
		t.Fatalf("手算的期望值对不上：time.Date(2026-10-01, %s) = %d，期望 %d", tzTestZone, got, tzTestExpiresEpoch)
	}

	logs := &captureHandler{}
	h := startServerAt(t, "127.0.0.1:0", filepath.Join(t.TempDir(), "probe.db"), loc, slog.New(logs), nil)
	br := newBrowser(t, "http://"+h.addr)
	setupAdmin(t, br, waitSetupCode(t, logs))
	nodeID, _ := createNodeViaAPI(t, br, "tz-01")

	wantTrafficDays := seedTrafficDays(t, h, nodeID, loc)
	wantCPUTicks := seedCPUSamples(t, h, nodeID)

	// 反代 + 注入：接口一条都不 mock，浏览器拿到的仍是真服务端的真数据。
	proxy := newTZProxy(t, "http://"+h.addr, tzHarnessConfig{
		NodeID:   nodeID,
		NodeName: "tz-01",
		Date:     tzTestDate,
		User:     "admin",
		Pass:     "a-very-good-password",
	})
	mock := newMockServer(t, proxy)

	raw := runChromeForResult(t, chrome, mock.URL+"/", proxy.result, 90*time.Second, "")

	var res tzResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("解析浏览器回传的结果: %v\n原始内容：%s", err, tail(string(raw), 600))
	}
	if res.Fatal != "" {
		t.Fatalf("浏览器里的自检流程没跑完：%s", res.Fatal)
	}
	// 每个场景都不许有 JS 报错：白屏、绑定失败、null 取属性全都落在这里。
	if len(res.Errs) != 0 {
		t.Fatalf("浏览器里有 %d 条 JS 报错：%v", len(res.Errs), res.Errs)
	}

	t.Logf("浏览器时区 = %s；服务端时区 = %s（--timezone）", res.BrowserTZ, res.ServerInfoTZ)
	if res.ServerInfoTZ != tzTestZone {
		t.Errorf("设置页「服务器信息 · 时区」= %q，期望 %q", res.ServerInfoTZ, tzTestZone)
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
				v, tzTestZone, sortedKeys(allowed))
		}
	}
	if firstClock == "" {
		t.Fatalf("「更新于」一次都没有按服务端 ts 渲染过：轨迹 = %v", res.UpdatedLog)
	}
	want := "更新于 " + time.Unix(res.SSETSList[0], 0).In(loc).Format("15:04:05")
	if firstClock != want {
		t.Errorf("第一帧 SSE 之后「更新于」= %q，期望 %q（= 服务端 ts %d 在 %s 下的写法）",
			firstClock, want, res.SSETSList[0], tzTestZone)
	}
	t.Logf("「更新于」的完整轨迹 = %v", res.UpdatedLog)
	t.Logf("第一帧 SSE 的原始 ts = %d → 页面显示 %q（Go 用 %s 算出来也是这个）",
		res.SSETSList[0], firstClock, tzTestZone)
}

// checkAuditClock 逐行核对操作记录的时间：页面上写的，必须等于后端下发的那个 ts
// 在服务端时区下的写法。
//
// 为什么用 Go 算期望值而不是在 JS 里再算一遍：那样只是"同一套 Intl 自己跟自己对"，
// 证明不了服务端时区真的被用上了。这里两套实现（浏览器 Intl / Go zoneinfo）
// 逐秒比对，才说明用户看到的时间能直接跟服务器上的 date、日志对上。
func checkAuditClock(t *testing.T, res tzResult, loc *time.Location) {
	t.Helper()
	if !strings.Contains(res.AuditHeader, tzTestZone) {
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
			t.Errorf("审计第 %d 行时间 = %q，期望 %q（ts=%d 在 %s 下）", i+1, got, wantServer, ts, tzTestZone)
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
				lb.Text, lb.X, anchor, tzTestZone, want)
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
				lb.Text, lb.X, anchor, tzTestZone, want, wantBrowser)
		}
	}
	if bad == 0 {
		t.Logf("CPU X 轴 %d 个标签全部是服务端时区（%s）的写法；后端曲线共 %d 个点（第一个 ts=%d）",
			len(res.CPU.Labels), tzTestZone, len(wantTicks), wantTicks[0])
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
			t.Errorf("流量图悬浮浮层时间 = %q，期望 %q（ts=%d 在 %s 下）", res.HoverTraffic.Time, want, ts, tzTestZone)
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
			t.Errorf("CPU 图悬浮浮层时间 = %q，期望 %q（ts=%d 在 %s 下）", res.HoverCPU.Time, want, ts, tzTestZone)
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
				r.Round, r.Stored, tzTestExpiresEpoch, tzTestZone)
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
func seedTrafficDays(t *testing.T, h *harness, nodeID int64, loc *time.Location) []string {
	t.Helper()
	ctx := context.Background()
	today := time.Now().In(loc)
	day := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, loc)
	out := make([]string, 0, 7)
	for i := 6; i >= 0; i-- {
		d := day.AddDate(0, 0, -i)
		if _, err := h.db.Writer().ExecContext(ctx,
			`INSERT INTO traffic_daily (node_id, day, rx, tx) VALUES (?, ?, ?, ?)`,
			nodeID, store.FormatDay(d), int64(7-i)*1_000_000_000, int64(14-i)*1_000_000_000); err != nil {
			t.Fatalf("写入日流量 %s: %v", store.FormatDay(d), err)
		}
		out = append(out, d.Format("01-02"))
	}
	return out
}

// seedCPUSamples 写一段 CPU 曲线（10 秒桶）。返回这些点的 ts（升序）。
//
// 只写窗口**中间**的一段（25 分钟前到 10 分钟前）：1h 档的查询窗口两端会对齐到
// 桶网格，写满整小时的话首尾点会随着 now 往前走而变，断言就飘了。
func seedCPUSamples(t *testing.T, h *harness, nodeID int64) []int64 {
	t.Helper()
	end := time.Now().Add(-10 * time.Minute).Truncate(10 * time.Second)
	start := time.Now().Add(-25 * time.Minute).Truncate(10 * time.Second)
	buckets := make([]store.SampleBucket, 0, 90)
	ticks := make([]int64, 0, 90)
	for ts := start; !ts.After(end); ts = ts.Add(10 * time.Second) {
		i := len(buckets)
		buckets = append(buckets, store.SampleBucket{
			NodeID: nodeID, TS: ts.Unix(),
			CPUAvg: float64(10 + i%20), CPUMax: float64(12 + i%20),
			MemAvg: 20, MemMax: 22, Up: 10, All: 10,
		})
		ticks = append(ticks, ts.Unix())
	}
	if len(buckets) < 30 {
		t.Fatalf("播撒的 CPU 桶只有 %d 个，太少", len(buckets))
	}
	if err := h.db.InsertBuckets(context.Background(), store.TableSamples10s, buckets); err != nil {
		t.Fatalf("写入 CPU 桶: %v", err)
	}
	t.Logf("播撒 CPU 桶 %d 个（ts %d … %d，步长 10 秒）", len(buckets), ticks[0], ticks[len(ticks)-1])
	return ticks
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

type tzHarnessConfig struct {
	NodeID   int64  `json:"nodeID"`
	NodeName string `json:"nodeName"`
	Date     string `json:"date"`
	User     string `json:"user"`
	Pass     string `json:"pass"`
	// Shot 非空时脚本只把界面开到指定状态就停住（给人截图核对用），
	// 不跑完整流程、也不回传结果。取值：audit / chart / dialog。
	Shot string `json:"shot"`
	// Scenario 是自检脚本要跑哪一套流程（不同用例观测的东西不同）：
	// 延迟图那条用 agg / interval，跨时区那条不用。
	Scenario string `json:"scenario"`
	// Nodes 是"节点名字 → id"（汇率那条用例要逐台机器看三处显示口径，
	// 名字用来在首页卡片与服务器列表里找到那一行，id 用来直接开它的详情页）。
	Nodes map[string]int64 `json:"nodes,omitempty"`
	// Focus 是"这条用例主要看哪台机器"（汇率那条用例的期望值是 Go 侧按它算的，
	// 详情页路由也必须走同一台；map 序列化之后键是字典序，不能靠"第一个"猜）。
	Focus string `json:"focus,omitempty"`
	// Theme 只在截图模式下用：取 "dark" 时页面进来就切到深色
	// （深色是 [data-theme="dark"] 而不是系统偏好 —— 无头浏览器没法可靠地
	// 模拟 prefers-color-scheme，走手动切换那条分支同时也验了它）。
	Theme string `json:"theme,omitempty"`
}

// newHarnessProxy 起一个"反代 + 只改首页"的层，首页里注入调用方给的脚本。
func newHarnessProxy(t *testing.T, base string, cfg tzHarnessConfig, harnessJS string) *tzProxy {
	t.Helper()
	target, err := url.Parse(base)
	if err != nil {
		t.Fatalf("解析服务端地址 %s: %v", base, err)
	}
	rp := httputil.NewSingleHostReverseProxy(target)
	// SSE 要立刻往下灌：总览条与卡片都靠它（Go 对 text/event-stream 本来就会
	// 立即 flush，这里显式写一遍是不依赖那个隐式行为）。
	rp.FlushInterval = 50 * time.Millisecond
	// 用例收尾时 Chrome 被强杀，长连接会带着 context canceled 报错回来 ——
	// 那是我们自己杀的，不该混进测试输出里。
	rp.ErrorLog = log.New(io.Discard, "", 0)

	resp, err := http.Get(base + "/")
	if err != nil {
		t.Fatalf("取首页: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("读首页: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("取首页状态码 = %d", resp.StatusCode)
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("序列化自检配置: %v", err)
	}
	return &tzProxy{
		proxy:  rp,
		page:   []byte(injectHarness(string(body), string(raw), harnessJS)),
		result: make(chan []byte, 1),
	}
}

// tzProxy 是"反向代理 + 只改首页"的一层。
//
//   - "/" → 从**真服务端**取 index.html，注入自检脚本后返回（注入必须发生在
//     index.html 上，所以需要这一层）；
//   - "/__result" → 浏览器把观测结果 POST 回这里，测试从 channel 里取走；
//   - 其它一切 → 原样转发给真服务端（Cookie / CSRF / SSE 一个都不碰）。
//
// 为什么不把 Chrome 直接指向服务端：注入不进去。为什么不干脆 mock 接口：
// 那等于把"后端按 --timezone 切天"这一半也 mock 掉了，而这正是要验证的东西。
type tzProxy struct {
	proxy  *httputil.ReverseProxy
	page   []byte
	result chan []byte
	once   sync.Once
}

func newTZProxy(t *testing.T, base string, cfg tzHarnessConfig) *tzProxy {
	t.Helper()
	return newHarnessProxy(t, base, cfg, tzHarnessJS)
}

func (p *tzProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/__result":
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		p.once.Do(func() { p.result <- body })
		w.WriteHeader(http.StatusNoContent)
	case r.URL.Path == "/" || r.URL.Path == "/index.html":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(p.page)
	default:
		p.proxy.ServeHTTP(w, r)
	}
}

// injectHarness 把自检脚本插在 </body> 之前：脚本是普通（非 defer）的内联脚本，
// 会在 app.js/chart.js 这些 defer 脚本**之前**执行，钩子因此一定装得上。
//
// harnessJS 由调用方给：跨时区那条用例与延迟图那条用例要观测的东西完全不同，
// 各带一份脚本比在一个脚本里塞两套分支清楚。
func injectHarness(html, cfgJSON, harnessJS string) string {
	const marker = "</body>"
	at := strings.LastIndex(html, marker)
	if at < 0 {
		panic("index.html 里没有 </body>：注入点找不到")
	}
	script := "<script>window.__TZCFG = " + cfgJSON + ";</script>\n" +
		"<script>\n" + harnessJS + "\n</script>\n"
	return html[:at] + script + html[at:]
}

// ---------------------------------------------------------------- 跑 Chrome

// runChromeForResult 打开 pageURL，等页面自己把结果 POST 回来（或超时）。
// windowSize 为空时用默认窗口；窄屏用例靠它落进 (max-width: 640px) 那一档。
func runChromeForResult(t *testing.T, chrome, pageURL string, result chan []byte, timeout time.Duration, windowSize string) []byte {
	t.Helper()
	// 画像目录单独开在系统临时目录里：Chrome 被强杀时子进程可能还握着里面的文件，
	// 放在 t.TempDir() 里会让"清理失败"变成一条测试失败（与被测代码无关）。
	profile, err := os.MkdirTemp("", "probe-tz-chrome-")
	if err != nil {
		t.Fatalf("创建 Chrome 画像目录: %v", err)
	}
	defer func() { _ = os.RemoveAll(profile) }()
	if windowSize == "" {
		windowSize = "1500,1100"
	}
	args := []string{
		"--headless=new",
		"--no-proxy-server",
		"--disable-gpu",
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-extensions",
		"--user-data-dir=" + profile,
		"--window-size=" + windowSize,
		pageURL,
	}
	cmd := exec.Command(chrome, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动 Chrome: %v", err)
	}
	// 这里**故意不用 --dump-dom + --virtual-time-budget**：页面连着一路 SSE，
	// 长连接会让虚拟时间一直暂停，预算可能永远耗不完（整套流程就卡死了）。
	// 结果通道改成"页面自己 POST 回 mock"（也就是本仓库惯用的那一套），
	// 拿到结果之后由我们结束 Chrome。
	killed := false
	defer func() {
		if !killed {
			killChrome(cmd)
		}
		_ = cmd.Wait()
	}()

	select {
	case raw := <-result:
		killed = true
		killChrome(cmd)
		return raw
	case <-time.After(timeout):
		t.Fatalf("等浏览器回传结果超时（%s）。Chrome 输出尾部：\n%s", timeout, tail(out.String(), 1500))
		return nil
	}
}

// killChrome 结束 Chrome。Windows 上只杀主进程会留下渲染/GPU 子进程攥着画像目录，
// 所以优先用 taskkill /T 整棵树一起收。
func killChrome(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	if cmd.Process.Pid > 0 {
		kill := exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(cmd.Process.Pid))
		if err := kill.Run(); err == nil {
			return
		}
	}
	_ = cmd.Process.Kill()
}

// ---------------------------------------------------------------- 浏览器里的自检脚本

// tzHarnessJS 是注入到页面里的自检脚本。
//
// 它只做三件事：**装钩子**（收集 JS 报错、把 canvas 上真正画出来的字记下来、
// 记下 SSE 给的原始 ts）、**按用户的真实操作驱动界面**（登录 → 进详情页 →
// 开编辑框 → 改到期日 → 保存 → 重新打开，连着来三轮）、**把观测值 POST 回去**。
// 一句断言都不在这里做：期望值要用 Go 的 time 包算（见文件顶部说明）。
const tzHarnessJS = `(function () {
  'use strict';
  var CFG = window.__TZCFG || {};
  var rawFetch = window.fetch.bind(window);
  var R = {
    errs: [], steps: [], browserTZ: '', fatal: '',
    nodesServerTime: '', updatedAfterNodesFetch: null, updatedLog: [], updatedLogAtSSE: [],
    sseTSList: [], serverInfoTZ: '', auditHeader: '', auditRendered: [], auditRawTS: [],
    traffic: {}, cpu: {}, hoverTraffic: {}, hoverCpu: {}, dialogRounds: []
  };
  window.__TZRESULT = R;

  window.addEventListener('error', function (e) { R.errs.push('error: ' + (e.message || e.type)); });
  window.addEventListener('unhandledrejection', function (e) {
    var r = e.reason;
    R.errs.push('rejection: ' + (r && r.message ? r.message : String(r)));
  });
  try { R.browserTZ = Intl.DateTimeFormat().resolvedOptions().timeZone; } catch (e) { R.browserTZ = '?'; }

  // ---- 钩 canvas：把真正 fillText 出来的字按画布 id 记下来 ------------------
  // 只静态看代码是证明不了"轴上写的是哪一天"的，必须看它画了什么。
  var drawn = {};
  (function () {
    var proto = CanvasRenderingContext2D.prototype;
    var orig = proto.fillText;
    proto.fillText = function (text, x, y) {
      var id = this.canvas ? this.canvas.id : '';
      if (id === 'chart-traffic' || id === 'chart-cpu') {
        var list = drawn[id] || (drawn[id] = []);
        list.push({ text: String(text), x: x, y: y });
      }
      return orig.apply(this, arguments);
    };
  })();
  function markOf(id) { return (drawn[id] || []).length; }
  function since(id, mark) { return (drawn[id] || []).slice(mark); }
  function uniqSince(id, mark, minY) {
    var out = [], seen = {};
    since(id, mark).forEach(function (r) {
      if (minY !== undefined && r.y < minY) return;
      if (seen[r.text]) return;
      seen[r.text] = 1;
      out.push({ text: r.text, x: r.x, y: r.y });
    });
    return out;
  }

  // ---- 钩 fetch：记下 /api/v1/nodes 那一帧渲染完之后「更新于」是什么 --------
  // 首帧必须显示占位 —（而不是浏览器时钟算出来的时间），这一条只有在这里取样
  // 才抓得到：它一瞬间就被随后到达的 SSE 覆盖了。
  window.fetch = function (input, init) {
    var url = typeof input === 'string' ? input : ((input && input.url) || '');
    var method = (init && init.method) || 'GET';
    return rawFetch(input, init).then(function (res) {
      if (method === 'GET' && /\/api\/v1\/nodes$/.test(url)) {
        var origJSON = res.json.bind(res);
        res.json = function () {
          return origJSON().then(function (data) {
            R.nodesServerTime = (data.server && data.server.time) || '';
            if (R.updatedAfterNodesFetch === null) {
              // 微任务跑完之后才轮到定时器：这时 app.js 那一帧已经渲染完了。
              setTimeout(function () {
                if (R.updatedAfterNodesFetch === null) {
                  R.updatedAfterNodesFetch = textOf('updated');
                }
              }, 0);
            }
            return data;
          });
        };
      }
      return res;
    });
  };

  // ---- 钩 EventSource：记下服务端给的原始 ts -------------------------------
  var OrigES = window.EventSource;
  window.EventSource = function (url, cfg) {
    var es = new OrigES(url, cfg);
    es.addEventListener('nodes', function (ev) {
      try {
        var p = JSON.parse(ev.data);
        if (p && typeof p.ts === 'number') R.sseTSList.push(p.ts);
      } catch (e) { R.errs.push('SSE 解析失败: ' + e.message); }
    });
    return es;
  };
  window.EventSource.prototype = OrigES.prototype;

  // ---- 「更新于」的变化轨迹 -------------------------------------------------
  var updatedEl = null;
  function watchUpdated() {
    updatedEl = document.getElementById('updated');
    if (!updatedEl) return;
    if (window.MutationObserver) {
      new MutationObserver(function () {
        var v = updatedEl.textContent;
        if (R.updatedLog[R.updatedLog.length - 1] !== v) R.updatedLog.push(v);
      }).observe(updatedEl, { childList: true, characterData: true, subtree: true });
    }
  }

  // ---- 小工具 --------------------------------------------------------------
  function node(id) { return document.getElementById(id); }
  function textOf(id) { var n = node(id); return n ? n.textContent : ''; }
  function shown(id) { var n = node(id); return !!n && !n.hidden; }
  function sleep(ms) { return new Promise(function (r) { setTimeout(r, ms); }); }
  function getJSON(url) {
    return rawFetch(url, { headers: { 'Accept': 'application/json' }, credentials: 'same-origin', cache: 'no-store' })
      .then(function (res) { return res.json(); });
  }
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
  // 「服务器信息」栏里的只读网格：每一项是一格 .kv-cell（小标签 .kv-k + 值 .kv-v）。
  // 改版前是 <dl> 里成对的 dt/dd，所以这里按"相邻两个子元素"读；
  // 现在一对值包在同一格里（.kv-grid 是 grid，dt/dd 会被拆到相邻两格）。
  function serverInfoValue(label) {
    var box = node('server-info');
    if (!box) return '';
    var cells = box.querySelectorAll('.kv-cell');
    for (var i = 0; i < cells.length; i++) {
      var k = cells[i].querySelector('.kv-k');
      if (k && k.textContent === label) {
        var v = cells[i].querySelector('.kv-v');
        return v ? v.textContent : '';
      }
    }
    return '';
  }

  // ---- 0. 登录（走真实表单：不注入 Cookie，整条链路与用户手点一样）--------
  // 登录成功的判据不能只看首页：带 #/n/<id> 或 #/settings/audit 进来时，
  // enterApp() 会直接路由到详情页/设置页，首页自始至终是 hidden 的。
  function loggedIn() {
    return shown('view-home') || shown('view-detail') || shown('view-settings');
  }
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

  // ---- 1. 首页：等第一帧 SSE，并把「更新于」的轨迹快照下来 -------------------
  function home() {
    return waitFor('收到第一帧 SSE', function () { return R.sseTSList.length > 0; }, 30000)
      .then(function () {
        return waitFor('「更新于」已按服务端 ts 渲染', function () {
          return /^更新于 /.test(textOf('updated'));
        }, 15000);
      })
      .then(function () {
        R.updatedLogAtSSE = R.updatedLog.slice();
        if (R.updatedLogAtSSE.length === 0) R.updatedLogAtSSE = [textOf('updated')];
        return true;
      });
  }

  // ---- 2. 详情页：等两张图都画出来 -----------------------------------------
  function openDetail() {
    window.location.hash = '#/n/' + CFG.nodeID;
    return waitFor('详情页打开', function () {
      return shown('view-detail') && textOf('detail-name') === CFG.nodeName;
    }, 30000).then(function () {
      return waitFor('流量图已绘制', function () { return markOf('chart-traffic') > 20; }, 30000);
    }).then(function () {
      return waitFor('CPU 图已绘制', function () { return markOf('chart-cpu') > 20; }, 30000);
    });
  }

  function canvasRect(id) { return node(id).getBoundingClientRect(); }

  // 强制重画一次（图表引擎监听 window 的 resize，80ms 防抖之后整张重画）：
  // 这样"要分析的那一帧"是我们自己触发的，几何尺寸也已经稳定。
  function forceRedraw(id) {
    var mark = markOf(id);
    window.dispatchEvent(new Event('resize'));
    return sleep(500).then(function () { return mark; });
  }

  // 把一张图的 X 轴标签（画在绘图区下方那一行）连同像素坐标一起收下来。
  function captureAxis(id, slot, t0, t1, points) {
    return forceRedraw(id).then(function (mark) {
      var rect = canvasRect(id);
      var h = Math.round(rect.height);
      slot.drawn = markOf(id);
      slot.width = Math.round(rect.width);
      slot.height = h;
      slot.plotLeft = 64;   // 与 chart.js 的 layout() 一致
      slot.plotRight = 8;
      slot.t0 = t0;
      slot.t1 = t1;
      slot.points = points;
      slot.labels = uniqSince(id, mark, h - 30).filter(function (r) {
        return /^[0-9]{2}[-:][0-9]{2}/.test(r.text);
      });
      return true;
    });
  }

  // 悬浮浮层那几行是**这一帧最后画的**东西（chart.js 的 drawHover 排在 draw() 末尾），
  // 而且行距恒为 14px（见 chart.js 里 lineH）。从末尾按行距往回走，就能把浮层的行
  // 原样取出来 —— 不能按文字去重：浮层里那个日期完全可能和 X 轴上某个标签同字
  // （"09-30" 既是轴标签、又是某一天的读数），按文字筛会把它吞掉。
  function tooltipRows(id, mark) {
    var list = since(id, mark);
    var tail = [];
    for (var i = list.length - 1; i >= 0; i--) {
      if (tail.length === 0) { tail.unshift(list[i]); continue; }
      if (Math.abs((tail[0].y - list[i].y) - 14) <= 2) { tail.unshift(list[i]); continue; }
      break;
    }
    return tail.map(function (r) { return r.text; });
  }

  // 悬浮：按图表引擎的线性映射算出"目标点"的像素位置，把鼠标停上去，
  // 再把浮层里新画出来的那几行字收下来。
  function hoverPoint(id, slot, t0, t1, ts) {
    var canvas = node(id);
    var rect = canvasRect(id);
    var w = Math.round(rect.width);
    var plotW = w - 64 - 8;
    var px = 64 + (ts - t0) / Math.max(t1 - t0, 1) * plotW;
    if (px > w - 10) px = w - 10;
    if (px < 70) px = 70;
    var mark = markOf(id);
    canvas.dispatchEvent(new MouseEvent('mousemove', {
      clientX: rect.left + px, clientY: rect.top + 60, bubbles: true
    }));
    slot.targetTS = ts;
    slot.rows = tooltipRows(id, mark);
    slot.time = slot.rows.filter(function (t) { return /^[0-9]{2}[-:][0-9]{2}/.test(t); })[0] || '';
    canvas.dispatchEvent(new MouseEvent('mouseleave', { bubbles: true }));
    return true;
  }

  function charts() {
    return getJSON('/api/v1/nodes/' + CFG.nodeID + '/series?range=1h&metric=cpu')
      .then(function (data) {
        var pts = (data.points || []).map(function (p) { return p[0]; });
        if (pts.length < 5) throw new Error('CPU 曲线点太少：' + pts.length);
        var mid = pts[Math.floor(pts.length / 2)];
        return captureAxis('chart-cpu', R.cpu, pts[0], pts[pts.length - 1], pts)
          .then(function () { return hoverPoint('chart-cpu', R.hoverCpu, pts[0], pts[pts.length - 1], mid); });
      })
      .then(function () {
        return getJSON('/api/v1/nodes/' + CFG.nodeID + '/traffic?days=7');
      })
      .then(function (data) {
        var pts = (data.points || []).map(function (p) { return p[0]; });
        if (pts.length < 7) throw new Error('日流量点太少：' + pts.length);
        var last = pts[pts.length - 1];
        return captureAxis('chart-traffic', R.traffic, pts[0], last, pts)
          .then(function () { return hoverPoint('chart-traffic', R.hoverTraffic, pts[0], last, last); });
      });
  }

  // ---- 3. 设置页 · 操作记录 ------------------------------------------------
  function audit() {
    window.location.hash = '#/settings/audit';
    return waitFor('操作记录已渲染', function () {
      return shown('view-settings') && node('audit-body').childNodes.length > 0 && serverInfoValue('时区') !== '';
    }, 30000).then(function () {
      R.auditHeader = textOf('audit-time-head');
      R.serverInfoTZ = serverInfoValue('时区');
      var rows = node('audit-body').childNodes;
      for (var i = 0; i < rows.length && i < 6; i++) {
        R.auditRendered.push(rows[i].childNodes[0].textContent);
      }
      return getJSON('/api/v1/audit?limit=50');
    }).then(function (data) {
      (data.entries || []).slice(0, 6).forEach(function (e) { R.auditRawTS.push(e.ts); });
      return true;
    });
  }

  // ---- 4. 到期日往返：打开编辑 → 填日期 → 保存 → 重新打开，连着三轮 --------
  function openEdit() {
    return waitFor('详情页就绪', function () {
      return shown('view-detail') && textOf('detail-name') === CFG.nodeName;
    }, 30000).then(function () {
      return waitFor('编辑按钮可用', function () { return node('detail-edit') && !node('detail-edit').disabled; }, 10000);
    }).then(function () {
      node('detail-edit').click();
      return waitFor('节点对话框打开', function () { return node('dlg-node').open === true; }, 15000);
    });
  }

  function roundTrip(n) {
    var rec = { round: n, before: '', readback: '', stored: 0, remaining: 0 };
    window.location.hash = '#/n/' + CFG.nodeID;
    return openEdit().then(function () {
      rec.before = node('node-expires').value;
      node('node-expires').value = CFG.date;
      return waitFor('提交按钮可用', function () { return !node('node-submit').disabled; }, 15000);
    }).then(function () {
      node('node-submit').click();
      return waitFor('保存返回（对话框关闭）', function () { return node('dlg-node').open === false; }, 30000);
    }).then(function () {
      return getJSON('/api/v1/nodes/' + CFG.nodeID);
    }).then(function (data) {
      rec.stored = data.node.expires_at;
      rec.remaining = data.node.remaining_days;
      return openEdit();
    }).then(function () {
      rec.readback = node('node-expires').value;
      node('node-cancel').click();
      R.dialogRounds.push(rec);
      return true;
    });
  }

  function run() {
    return waitFor('页面脚本就绪', function () { return !!node('view-login') && !!node('form-login'); }, 30000)
      .then(function () { watchUpdated(); return true; })
      .then(login)
      .then(home)
      .then(openDetail)
      .then(charts)
      .then(audit)
      .then(function () { return roundTrip(1); })
      .then(function () { return roundTrip(2); })
      .then(function () { return roundTrip(3); });
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
    if (CFG.shot) { shot(); return; }
    run().catch(function (err) {
      R.fatal = String(err && err.message ? err.message : err);
    }).then(finish, finish);
  });

  // shot 模式：只把界面开到指定状态（给人工截图核对用），不跑完整流程。
  function shot() {
    var mark = function (what) { document.title = 'SHOT-READY:' + what; };
    login().then(function () {
      if (CFG.shot === 'audit') {
        window.location.hash = '#/settings/audit';
        return waitFor('操作记录已渲染', function () {
          return shown('view-settings') && node('audit-body').childNodes.length > 0;
        }, 30000).then(function () { mark('audit'); });
      }
      window.location.hash = '#/n/' + CFG.nodeID;
      return waitFor('详情页打开', function () {
        return shown('view-detail') && textOf('detail-name') === CFG.nodeName;
      }, 30000).then(function () {
        if (CFG.shot === 'dialog') {
          return openEdit().then(function () { mark('dialog'); });
        }
        return waitFor('流量图已绘制', function () { return markOf('chart-traffic') > 20; }, 30000)
          .then(function () { mark('chart'); });
      });
    }).catch(function (err) { R.fatal = String(err && err.message ? err.message : err); });
  }
})();`

// ---------------------------------------------------------------- mock 服务器

// newMockServer 起一个只服务注入过自检脚本的首页 + 反代 + 结果回收的 httptest 服务。
func newMockServer(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}
