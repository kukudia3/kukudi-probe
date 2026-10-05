package protocol

// 这一份用例钉的是首页那两格「实时上行 / 实时下行」改成**跟着推送帧每秒更新**之后的
// 行为（用户报的"会变 ✓ 但不是一秒一变 ✗"）。
//
// 改造前：那两格的唯一来源是 60 秒一次的 GET /api/v1/overview 轮询；SSE 每秒都在推，
// 但帧里只有每个节点的速率，两格合计要等下一轮轮询才动。改造后：帧里带上服务端算好的
// 全量合计（streamPayload.Totals，口径 = overviewTotals → totalsOf，见
// internal/server/overview.go），前端只把它格式化 —— **前端一个加法都不做**。
//
// 两条用例分工：
//
//  1. TestStreamFrameTotalsMatchOverview —— 协议那一层（不用浏览器，几秒钟）：
//     帧里的 totals 与 GET /api/v1/overview 的 totals 必须是同一份数字，
//     而且"实时上下行只累加在线节点"这条口径在两条路上都成立。
//  2. TestRealtimeRatesFollowStreamInRealBrowser —— 用户在屏幕上看得见的那一层：
//     数字在几秒内跟着推送变（不是等 60 秒）、上下线跟着变、推送断了之后状态栏
//     不再写「实时」、恢复之后又写回来。
//
// 做法与仓库里其它浏览器用例完全一致：真服务端（Server.Run 完整路径）+ 反代注入
// 自检脚本 + 页面把观测值 POST 回 mock，**不用** --dump-dom + --virtual-time-budget
// （首页挂着 SSE 长连接，虚拟时间会一直暂停）。

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"path/filepath"
	"probe/internal/e2e/harness"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"probe/internal/protocol"
)

// 现场的数字（字节/秒）。三个节点故意用**互不相同**的值：方向搞反、少算一台、
// 多算一台离线机器，都会让屏幕上那一个字符串与期望值对不上。
//
//   - A 在线：初始 200.0/100.0 KB/s，换速率之后 600.0/300.0 KB/s；
//   - B 在线且恒定：50.0/20.0 KB/s；
//   - C **离线**（最后一次通信在一小时前），最后已知速率 999.0/999.0 KB/s。
//
// C 是这条用例判别力的来源：它的 DTO 里 rx_rate/tx_rate 是大数，但"实时上下行"
// 只算在线节点（见 overview.go 的 overviewTotals），所以 A+B 的合计里不许有它。
const (
	rtTxA   = 200000
	rtRxA   = 100000
	rtTxA2  = 600000
	rtRxA2  = 300000
	rtTxB   = 50000
	rtRxB   = 20000
	rtTxOff = 999000
	rtRxOff = 999000
)

// rtRatesMetrics 造一帧只带网络速率的指标（其余字段留零值）。
func rtRatesMetrics(tx, rx int) protocol.Metrics {
	return protocol.Metrics{Net: protocol.Net{
		Iface: "eth0", RxRate: float64(rx), TxRate: float64(tx), BootID: "boot-rt",
	}}
}

// rtFmtRate 复刻 app.js 的 fmtRate（十进制、≥10 一位小数、<10 两位）：期望值必须由
// "两个在线节点的速率之和"推出来，而不是把屏幕上那串字再抄一遍 —— 抄一遍的话，
// 实现把方向搞反（上行写成 rx）或者把离线那台算进来时，用例照样是绿的。
func rtFmtRate(n int) string {
	if n <= 0 {
		return "0 B/s"
	}
	units := []string{"B", "KB", "MB", "GB", "TB", "PB"}
	v := float64(n)
	i := 0
	for v >= 1000 && i < len(units)-1 {
		v /= 1000
		i++
	}
	var s string
	switch {
	case i == 0:
		s = strconv.FormatFloat(v, 'f', 0, 64)
	case v < 10:
		s = strconv.FormatFloat(v, 'f', 2, 64)
	default:
		s = strconv.FormatFloat(v, 'f', 1, 64)
	}
	return s + " " + units[i] + "/s"
}

// rtParseRate 把屏幕上那串 "↑ 250.0 KB/s" 读回成字节/秒（false = 读不出来）。
// 容差判据要用它：显示只保留一位小数（0.1 KB = 100 字节一步）。
func rtParseRate(text string) (int, bool) {
	text = strings.TrimPrefix(strings.TrimSpace(text), "↑")
	text = strings.TrimPrefix(strings.TrimSpace(text), "↓")
	text = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(text), "/s"))
	fields := strings.Fields(text)
	if len(fields) != 2 {
		return 0, false
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, false
	}
	scale := map[string]float64{"B": 1, "KB": 1e3, "MB": 1e6, "GB": 1e9, "TB": 1e12, "PB": 1e15}[fields[1]]
	if scale == 0 {
		return 0, false
	}
	return int(v*scale + 0.5), true
}

// rtWaitTotals 从 SSE 帧里等一份满足条件的 totals（超时如实报错，不吞）。
func rtWaitTotals(t *testing.T, events chan map[string]any, what string, ok func(map[string]any) bool, timeout time.Duration) map[string]any {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case payload, open := <-events:
			if !open {
				t.Fatalf("等 %s 时 SSE 流已关闭", what)
			}
			totals, isMap := payload["totals"].(map[string]any)
			if !isMap {
				t.Fatalf("帧里没有 totals（老服务端不发这个键，前端会退化成 60 秒轮询，"+
					"但这条用例的现场必须是新服务端）：%v", payload)
			}
			if ok(totals) {
				return totals
			}
		case <-time.After(200 * time.Millisecond):
		}
	}
	t.Fatalf("%s：%s 内没有等到符合条件的帧", what, timeout)
	return nil
}

// TestStreamFrameTotalsMatchOverview 钉住协议那一层：推送帧里的 totals 与
// GET /api/v1/overview 的 totals 是**同一份口径**（同一个函数算出来的），
// 而且"实时上下行只累加在线节点"在两条路上都成立。
//
// 为什么不带浏览器也值得单独一条：它几秒钟跑完，断言的是数字本身；浏览器那条验的是
// "用户在屏幕上看得见的行为"，两者互补。
func TestStreamFrameTotalsMatchOverview(t *testing.T) {
	logs := &harness.CaptureHandler{}
	h := harness.StartServerAt(t, "127.0.0.1:0", filepath.Join(t.TempDir(), "probe.db"), time.UTC, slog.New(logs), nil)
	br := harness.NewBrowser(t, "http://"+h.Addr)
	harness.SetupAdmin(t, br, harness.WaitSetupCode(t, logs))

	onlineID, _ := harness.CreateNodeViaAPI(t, br, "totals-online")
	offlineID, _ := harness.CreateNodeViaAPI(t, br, "totals-offline")

	// 在线节点：刚上报过。
	h.Srv.State().Update(onlineID, 1, rtRatesMetrics(rtTxA, rtRxA), 0, time.Now())
	// 离线节点：最后一次通信在一小时前，**最后已知速率是大数**。
	h.Srv.State().Update(offlineID, 1, rtRatesMetrics(rtTxOff, rtRxOff), 0, time.Now().Add(-time.Hour))

	events, stop := br.SSE("/api/v1/stream")
	defer stop()

	// ① 连上时那份全量快照里就有 totals，而且只算了在线那一台。
	// 先原样收下第一帧再判值（而不是"等一个符合条件的帧"）：不合期望时那条红要说得清
	// 到底读到了什么（例如"tx_rate = 0"就一眼看出合计是空的）。
	snapshot := rtWaitTotals(t, events, "连上之后的第一帧（全量快照）", func(map[string]any) bool { return true }, 5*time.Second)
	if got := snapshot["tx_rate"]; got != float64(rtTxA) {
		t.Errorf("快照 totals.tx_rate = %v，期望 %d（只算在线节点；离线那台的最后速率是 %d，不许算进来）",
			got, rtTxA, rtTxOff)
	}
	if got := snapshot["rx_rate"]; got != float64(rtRxA) {
		t.Errorf("快照 totals.rx_rate = %v，期望 %d", got, rtRxA)
	}
	if snapshot["nodes_online"] != float64(1) || snapshot["nodes_total"] != float64(2) {
		t.Errorf("快照 totals 的台数 = %v 在线 / %v 总数，期望 1 / 2",
			snapshot["nodes_online"], snapshot["nodes_total"])
	}

	// ② 换一次速率：下一帧的 totals 必须跟着变（它就是"每秒更新"的数据源）。
	h.Srv.State().Update(onlineID, 1, rtRatesMetrics(rtTxA2, rtRxA2), 0, time.Now())
	frame := rtWaitTotals(t, events, "换速率之后的 totals", func(totals map[string]any) bool {
		return totals["tx_rate"] == float64(rtTxA2)
	}, 5*time.Second)
	if got := frame["rx_rate"]; got != float64(rtRxA2) {
		t.Errorf("换速率之后 totals.rx_rate = %v，期望 %d", got, rtRxA2)
	}

	// ③ 同一时刻的 /api/v1/overview 必须是同一份数字（两条路同一个函数）。
	status, body := br.Do(http.MethodGet, "/api/v1/overview?window=1h&buckets=10", nil, false)
	if status != http.StatusOK {
		t.Fatalf("GET /api/v1/overview = %d", status)
	}
	ov, isMap := body["totals"].(map[string]any)
	if !isMap {
		t.Fatalf("/overview 的响应里没有 totals：%v", body)
	}
	if ov["tx_rate"] != frame["tx_rate"] || ov["rx_rate"] != frame["rx_rate"] {
		t.Errorf("推送帧与 /overview 的实时上下行对不上：帧 %v/%v，接口 %v/%v"+
			"（口径必须复用同一个函数，见 overview.go 的 totalsOf）",
			frame["tx_rate"], frame["rx_rate"], ov["tx_rate"], ov["rx_rate"])
	}
	// 方向也要对：上行是 tx、下行是 rx（与卡片上的 ↑/↓ 同一套，见 overviewTotals 的注释）。
	if ov["tx_rate"] != float64(rtTxA2) || ov["rx_rate"] != float64(rtRxA2) {
		t.Errorf("/overview 的 totals = tx %v / rx %v，期望 tx %d / rx %d（方向搞反了页面一样能显示）",
			ov["tx_rate"], ov["rx_rate"], rtTxA2, rtRxA2)
	}
	// ④ 对照：**所有**节点的 DTO 里都还带着各自的速率（离线的也在），
	//    所以"算错成全部节点"确实是这条用例挡得住的一种实现。
	_, nodesBody := br.Do(http.MethodGet, "/api/v1/nodes", nil, false)
	rawNodes, err := json.Marshal(nodesBody)
	if err != nil {
		t.Fatalf("序列化 /nodes: %v", err)
	}
	if !strings.Contains(string(rawNodes), strconv.Itoa(rtTxOff)) {
		t.Errorf("离线的节点在 /nodes 里也应当带着它最后的速率 %d：没有它，"+
			"上面那条「只算在线」的断言在任何实现下都成立（空断言）", rtTxOff)
	}
}

// ---------------------------------------------------------------- 真浏览器

// rtRatesAgent 是假 Agent：按拍把速率写进服务端内存状态（真 Agent 那条路是
// WebSocket → state.Update，这里直接用同一个入口）。
type rtRatesAgent struct {
	mu     sync.Mutex
	id     int64
	tx, rx int
	paused bool
}

func (a *rtRatesAgent) setRate(tx, rx int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.tx, a.rx = tx, rx
}

func (a *rtRatesAgent) setPaused(paused bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.paused = paused
}

// push 写一拍；at 是这次通信的时刻（把它倒拨就等于"这台机器很久没说话了"）。
// paused 时**什么都不写** —— 那是"这台机器的 Agent 掉了"。
func (a *rtRatesAgent) push(h *harness.Harness, at time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.paused {
		return
	}
	a.writeLocked(h, at)
}

// writeBackdated 不管 paused，**硬写**一拍（时刻由调用方给）。
//
// 为什么要单独一个：让 A"立刻离线"要的正是"暂停上报 + 最后一次通信倒拨一小时"这两件
// 事一起生效 —— 只 setPaused 的话它上一次通信还是刚刚，状态仍然是在线（30 秒内都不会
// 变），这条用例就白等了。
func (a *rtRatesAgent) writeBackdated(h *harness.Harness, at time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.writeLocked(h, at)
}

func (a *rtRatesAgent) writeLocked(h *harness.Harness, at time.Time) {
	h.Srv.State().Update(a.id, 1, rtRatesMetrics(a.tx, a.rx), 0, at)
}

// rtRatesProxy 在真反代前面加几个用例专用的小口子：
//
//	POST /__rate?tx=..&rx=..  —— 把 A 的上报速率换掉，并立刻补一拍；
//	POST /__offline-a         —— A 停止上报 + 把最后一次通信**倒拨一小时**（立刻离线）；
//	POST /__online-a          —— A 恢复上报（立刻补一拍，于是立刻回到在线）；
//	POST /__freeze /__thaw    —— 把 SSE 的响应体吞掉 / 放行（见 rtGatedWriter）。
//
// 其余路径原样交给 harness.Proxy（首页注入、/__result、以及一切接口）。
type rtRatesProxy struct {
	inner *harness.Proxy
	h     *harness.Harness
	agent *rtRatesAgent
	// open 为真时 SSE 正常放行；为假时响应体被**攒住**（连接照旧开着）。
	open atomic.Bool
	// active 是当前那条 SSE 的闸门（/__thaw 要靠它把攒下的字节吐出去）。
	active atomic.Pointer[rtGatedWriter]
	// streams 数一共建过几条 SSE 连接（浏览器自己重连也会 +1）：诊断用 ——
	// "解冻之后那几秒"到底是等下一帧，还是浏览器把连接重连了一次。
	streams atomic.Int64
}

// rtGatedWriter 是 SSE 那一条响应的闸门：open 为假时把响应体**攒在本地**
// （一个字节都不往浏览器送），连接本身照旧开着 —— 这正是中间层缓冲住 SSE 时
// （反代 proxy_buffering）的样子：字节一个不少，只是迟到。
//
// 为什么不直接**丢掉**冻结期间的那些字节：那样会从中间截断一条 SSE 事件，浏览器解析
// 到半截 JSON（页面按"数据异常"处理），接着连接被重连 —— 那就同时验了三条路径，
// 而且"解冻之后多久恢复"变得不确定。真实的反代缓冲不会丢字节，只会攒着。
//
// 为什么不干脆掐断连接：掐断会派发 error、状态栏会写「已断开，重连中」，那是**另一条**
// 路径（连接确实断了）。这条用例要验的是"连接活着、数据不来" —— 恰恰是只看连接状态的
// 旧实现会撒谎的那种情况。
type rtGatedWriter struct {
	http.ResponseWriter
	open *atomic.Bool
	mu   sync.Mutex
	buf  bytes.Buffer
}

// rtGateBufferMax 是攒字节的上限：真实反代也有缓冲区上限（proxy_buffer_size），
// 攒不下时会丢。本用例的冻结窗口由看门狗决定（6~7 秒、每秒一两帧），离它很远。
const rtGateBufferMax = 1 << 20

func (w *rtGatedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.open.Load() {
		if w.buf.Len() < rtGateBufferMax {
			_, _ = w.buf.Write(p)
		}
		// 报告"写成功了"：让反代继续往下灌，服务端那边感觉不到任何异常。
		return len(p), nil
	}
	w.drainLocked()
	return w.ResponseWriter.Write(p)
}

// flush 把攒下的字节一次写出去（/__thaw 调它）。
func (w *rtGatedWriter) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.drainLocked()
}

func (w *rtGatedWriter) drainLocked() {
	if w.buf.Len() == 0 {
		return
	}
	// 先复制一份再写：net/http 的 Write 会同步拷走，但这里不依赖那个细节。
	pending := append([]byte(nil), w.buf.Bytes()...)
	w.buf.Reset()
	_, _ = w.ResponseWriter.Write(pending)
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *rtGatedWriter) Flush() {
	if !w.open.Load() {
		return
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (p *rtRatesProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/__rate":
		tx, errTX := strconv.Atoi(r.URL.Query().Get("tx"))
		rx, errRX := strconv.Atoi(r.URL.Query().Get("rx"))
		if errTX != nil || errRX != nil {
			http.Error(w, "tx/rx 必须是整数", http.StatusBadRequest)
			return
		}
		p.agent.setRate(tx, rx)
		// 立刻补一拍：不必等下一次心跳，状态栏那两格下一秒就会跟着变。
		p.agent.push(p.h, time.Now())
		w.WriteHeader(http.StatusNoContent)
		return
	case "/__offline-a":
		// 先停止上报（否则下一拍就把它救回在线），再把时间倒拨：默认
		// stale-after=10s / offline-after=30s（见 config.Default），一小时前的那一拍
		// 立刻就是"离线"，而它最后已知的速率仍然留在状态里。
		p.agent.setPaused(true)
		p.agent.writeBackdated(p.h, time.Now().Add(-time.Hour))
		w.WriteHeader(http.StatusNoContent)
		return
	case "/__online-a":
		p.agent.setPaused(false)
		p.agent.push(p.h, time.Now())
		w.WriteHeader(http.StatusNoContent)
		return
	case "/__freeze":
		p.open.Store(false)
		w.WriteHeader(http.StatusNoContent)
		return
	case "/__thaw":
		p.open.Store(true)
		// 把冻结期间攒下的字节一次交给浏览器（真实的反代缓冲也是这么吐出来的）。
		if gw := p.active.Load(); gw != nil {
			gw.flush()
		}
		w.WriteHeader(http.StatusNoContent)
		return
	case "/api/v1/stream":
		p.streams.Add(1)
		gw := &rtGatedWriter{ResponseWriter: w, open: &p.open}
		p.active.Store(gw)
		p.inner.ServeHTTP(gw, r)
		return
	}
	p.inner.ServeHTTP(w, r)
}

// rtRatesResult 是浏览器回传的原始观测值。
//
// 脚本**一句断言都不做**（时限只是"在页面里等一等"），期望值全部由 Go 侧算：
// 屏幕上那串字与 /api/v1/overview 的数字都原样带回来，谁对谁错在这里判。
type rtRatesResult struct {
	Errs  []string       `json:"errs"`
	Steps []string       `json:"steps"`
	Fatal string         `json:"fatal"`
	Notes map[string]any `json:"notes"`
}

func (r *rtRatesResult) noteStr(key string) string {
	v, _ := r.Notes[key].(string)
	return v
}

func (r *rtRatesResult) noteNum(key string) float64 {
	v, _ := r.Notes[key].(float64)
	return v
}

// TestRealtimeRatesFollowStreamInRealBrowser 是真浏览器那一遍（用户看得见的行为）。
func TestRealtimeRatesFollowStreamInRealBrowser(t *testing.T) {
	chrome := harness.FindChrome()
	if chrome == "" {
		t.Skip("找不到 Chrome：设置 PROBE_CHROME 或把它装到默认位置后这条用例会自动跑起来")
	}

	logs := &harness.CaptureHandler{}
	h := harness.StartServerAt(t, "127.0.0.1:0", filepath.Join(t.TempDir(), "probe.db"), time.UTC, slog.New(logs), nil)
	br := harness.NewBrowser(t, "http://"+h.Addr)
	harness.SetupAdmin(t, br, harness.WaitSetupCode(t, logs))

	idA, _ := harness.CreateNodeViaAPI(t, br, "rt-a")
	idB, _ := harness.CreateNodeViaAPI(t, br, "rt-b")
	idC, _ := harness.CreateNodeViaAPI(t, br, "rt-offline")

	agentA := &rtRatesAgent{id: idA, tx: rtTxA, rx: rtRxA}
	agentB := &rtRatesAgent{id: idB, tx: rtTxB, rx: rtRxB}
	// C 是那台"离线但最后速率很大"的机器：只写一拍、时间倒拨一小时，之后不再上报。
	agentC := &rtRatesAgent{id: idC, tx: rtTxOff, rx: rtRxOff, paused: true}
	agentC.push(h, time.Now().Add(-time.Hour))

	// A 与 B 每秒各报一拍（与真 Agent 的默认 interval_sec=1 同节奏）。
	agentA.push(h, time.Now())
	agentB.push(h, time.Now())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-tick.C:
				agentA.push(h, now)
				agentB.push(h, now)
			}
		}
	}()

	cfg := harness.HarnessConfig{
		NodeID:   idA,
		NodeName: "rt-a",
		User:     "admin",
		Pass:     harness.AdminPass,
		Scenario: "rtrates",
	}
	proxy := &rtRatesProxy{
		inner: harness.NewHarnessProxy(t, "http://"+h.Addr, cfg, rtRatesHarnessJS()),
		h:     h,
		agent: agentA,
	}
	proxy.open.Store(true)
	mock := harness.NewMockServer(t, proxy)

	raw := harness.RunChromeForResult(t, chrome, mock.URL+"/", proxy.inner.Result, 150*time.Second, "1500,1100")

	var res rtRatesResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("解析浏览器回传的结果: %v\n原始内容：%s", err, harness.Tail(string(raw), 600))
	}
	t.Logf("已完成步骤：%v", res.Steps)
	t.Logf("观测值：%v", res.Notes)
	t.Logf("浏览器一共建过 %d 条 SSE 连接（诊断用：解冻迟到的原因）", proxy.streams.Load())
	if res.Fatal != "" {
		t.Fatalf("浏览器里的自检流程没跑完：%s（已完成步骤：%v）", res.Fatal, res.Steps)
	}
	if len(res.Errs) != 0 {
		t.Fatalf("浏览器里有 %d 条 JS 报错：%v", len(res.Errs), res.Errs)
	}

	// ---- ① 起点：两个在线节点之和，**不含**那台离线机器 ----
	// 这本身就是口径断言：C 的最后已知速率是 999.0 KB/s，算进去两格会变成 1.25 MB/s
	// 那个量级，与期望值差一个数量级。
	wantUp0 := "↑ " + rtFmtRate(rtTxA+rtTxB)
	wantDown0 := "↓ " + rtFmtRate(rtRxA+rtRxB)
	if got := res.noteStr("upInitial"); got != wantUp0 {
		t.Fatalf("起点「实时上行」= %q，期望 %q（A %s + B %s；离线的 C 是 %s，算进来就不对了）",
			got, wantUp0, rtFmtRate(rtTxA), rtFmtRate(rtTxB), rtFmtRate(rtTxOff))
	}
	if got := res.noteStr("downInitial"); got != wantDown0 {
		t.Fatalf("起点「实时下行」= %q，期望 %q", got, wantDown0)
	}

	// ---- ② 口径对齐：同一时刻的屏幕值 vs GET /api/v1/overview ----
	// 容差 100 字节：显示只保留一位小数（0.1 KB = 100 字节一步 ⇒ 取整误差 ≤ 50），
	// 再加上"读屏幕"与"取接口"之间那几毫秒里速率本身的抖动 —— 本用例的假 Agent 把
	// 速率钉死，所以这一项实际上是 0，容差留的是余量（见下面打印的相隔毫秒数）。
	const tolerance = 100
	gotUp, okUp := rtParseRate(res.noteStr("cellUpAtAlign"))
	gotDown, okDown := rtParseRate(res.noteStr("cellDownAtAlign"))
	if !okUp || !okDown {
		t.Fatalf("读不出屏幕上的速率：%q / %q", res.noteStr("cellUpAtAlign"), res.noteStr("cellDownAtAlign"))
	}
	ovTx := int(res.noteNum("ovTx"))
	ovRx := int(res.noteNum("ovRx"))
	t.Logf("口径对齐：屏幕 ↑%d B/s / ↓%d B/s，接口 tx=%d / rx=%d，两次读数相隔 %.0f ms",
		gotUp, gotDown, ovTx, ovRx, res.noteNum("alignGapMs"))
	if diff := gotUp - ovTx; diff > tolerance || diff < -tolerance {
		t.Errorf("屏幕上的上行 %d 与同一时刻 /api/v1/overview 的 %d 差 %d 字节（容差 %d）",
			gotUp, ovTx, diff, tolerance)
	}
	if diff := gotDown - ovRx; diff > tolerance || diff < -tolerance {
		t.Errorf("屏幕上的下行 %d 与同一时刻 /api/v1/overview 的 %d 差 %d 字节（容差 %d）",
			gotDown, ovRx, diff, tolerance)
	}
	if ovTx != rtTxA+rtTxB || ovRx != rtRxA+rtRxB {
		t.Errorf("接口那份合计 = tx %d / rx %d，期望 %d / %d（只算在线节点）",
			ovTx, ovRx, rtTxA+rtTxB, rtRxA+rtRxB)
	}

	// ---- ③ 数字在几秒内跟着推送变（不是等 60 秒那轮轮询）----
	wantUp := "↑ " + rtFmtRate(rtTxA2+rtTxB)
	wantDown := "↓ " + rtFmtRate(rtRxA2+rtRxB)
	changedAfter := res.noteNum("changedAfterMs")
	if got := res.noteStr("upAfterBump"); got != wantUp {
		t.Errorf("换速率 %.0f ms 之后「实时上行」= %q，期望 %q", changedAfter, got, wantUp)
	}
	if got := res.noteStr("downAfterBump"); got != wantDown {
		t.Errorf("换速率 %.0f ms 之后「实时下行」= %q，期望 %q", changedAfter, got, wantDown)
	}
	if changedAfter <= 0 || changedAfter >= 6000 {
		t.Errorf("数字在 %.0f ms 内没跟上（期望 6 秒内）：它应当由每秒的推送驱动，"+
			"而不是 60 秒的轮询", changedAfter)
	}
	// 这段时间里**一次 /overview 都不许落地** —— 否则"变了"可能只是轮询正好赶上。
	if a, b := res.noteNum("overviewDoneBeforeBump"), res.noteNum("overviewDoneAfterBump"); b != a {
		t.Errorf("换速率到读数之间落地了 %.0f 次 /api/v1/overview（之前 %.0f 次）："+
			"这条断言要的是**推送**把它改掉的", b-a, a)
	}

	// ---- ④ 节点下线 / 上线，合计跟着变 ----
	wantOffUp := "↑ " + rtFmtRate(rtTxB)
	wantOffDown := "↓ " + rtFmtRate(rtRxB)
	if got := res.noteStr("upAfterOffline"); got != wantOffUp {
		t.Errorf("A 离线之后「实时上行」= %q，期望 %q（只剩 B）—— 离线那台的速率不许留在合计里",
			got, wantOffUp)
	}
	if got := res.noteStr("downAfterOffline"); got != wantOffDown {
		t.Errorf("A 离线之后「实时下行」= %q，期望 %q", got, wantOffDown)
	}
	if ms := res.noteNum("offlineMs"); ms <= 0 || ms >= 8000 {
		t.Errorf("A 离线之后合计在 %.0f ms 内没跟着变（默认 offline-after=30s，这里靠倒拨时间触发）", ms)
	}
	if got := res.noteStr("upAfterOnline"); got != wantUp {
		t.Errorf("A 回到在线之后「实时上行」= %q，期望 %q", got, wantUp)
	}
	if got := res.noteStr("downAfterOnline"); got != wantDown {
		t.Errorf("A 回到在线之后「实时下行」= %q，期望 %q", got, wantDown)
	}
	if ms := res.noteNum("onlineMs"); ms <= 0 || ms >= 8000 {
		t.Errorf("A 回到在线之后合计在 %.0f ms 内没跟着变", ms)
	}

	// ---- ⑤ 指示灯：连接活着、但数据不来时不许再写「实时」 ----
	if got := res.noteStr("liveBeforeFreeze"); got != "实时" {
		t.Fatalf("冻结之前状态栏 = %q，期望「实时」（前置条件：这时数据是新鲜的）", got)
	}
	// 前置条件：冻结期间**真的**一帧都没到（攒在反代里，不是"到了但页面没理"）。
	if a, b := res.noteNum("framesBeforeFreeze"), res.noteNum("framesAtFreezeEnd"); b != a {
		t.Fatalf("冻结期间浏览器还收到了 %.0f 帧（冻结前 %.0f 帧）：现场没搭成，下面那条断言就没有意义", b-a, a)
	}
	if a, b := res.noteNum("framesAtFreezeEnd"), res.noteNum("framesAfterThaw"); b <= a {
		t.Fatalf("解冻之后一帧都没到（%.0f → %.0f）：攒下的字节没有吐出来", a, b)
	}
	if es := res.noteNum("esStateDuringFreeze"); es != 1 {
		t.Errorf("冻结期间 EventSource.readyState = %.0f，期望 1（OPEN）：这条用例要的正是"+
			"「连接开着、数据不来」；连接真断了的话验的就是另一条路径了", es)
	}
	if got := res.noteStr("liveDuringFreeze"); got == "实时" {
		t.Errorf("推送被攒住 %.0f ms 之后状态栏仍然写着「实时」（连接是 OPEN 的，"+
			"所以只看连接状态的实现会一直这么写）：数据不新鲜时必须改口", res.noteNum("freezeMs"))
	}
	if got := res.noteStr("liveDuringFreeze"); got != "数据延迟" {
		t.Errorf("推送被攒住之后状态栏 = %q，期望「数据延迟」（文案是这次唯一的可观测变化）", got)
	}
	if got := res.noteStr("liveAfterThaw"); got != "实时" {
		t.Errorf("推送恢复之后状态栏 = %q，期望「实时」", got)
	}
	// 解冻的过程里不许出现「已断开」：这条用例验的是"连接一直开着"，
	// 中间真断过一次的话，恢复靠的就是重连拿快照，而不是"数据又来了"。
	if tl := res.noteStr("thawTimeline"); strings.Contains(tl, "已断开") {
		t.Errorf("解冻过程中状态栏出现过「已断开」（文案轨迹：%s）：冻结必须只影响**数据**，"+
			"不许把连接弄断 —— 否则验的就不是这条路径了", tl)
	}
	if ms := res.noteNum("thawMs"); ms <= 0 || ms >= 3000 {
		t.Errorf("推送恢复之后 %.0f ms 内状态栏没写回「实时」（攒下的字节是解冻时一次吐出来的）", ms)
	}
}

// rtRatesHarnessJS 把现场算出来的期望值填进模板（见 rtRatesHarnessJSTemplate 里的
// __X__ 占位符）。
//
// 为什么走文本替换而不是往 harness.HarnessConfig 上加字段：期望值全部由上面那组场景常量
// 推出来（见 rtFmtRate），放在这里一眼看得出"屏幕上该出现什么"是怎么来的，
// 也不会去动别的用例共用的结构体。
func rtRatesHarnessJS() string {
	return strings.NewReplacer(
		"__WANT_UP0__", "↑ "+rtFmtRate(rtTxA+rtTxB),
		"__WANT_DOWN0__", "↓ "+rtFmtRate(rtRxA+rtRxB),
		"__WANT_UP_FAST__", "↑ "+rtFmtRate(rtTxA2+rtTxB),
		"__WANT_DOWN_FAST__", "↓ "+rtFmtRate(rtRxA2+rtRxB),
		"__WANT_UP_OFF__", "↑ "+rtFmtRate(rtTxB),
		"__WANT_DOWN_OFF__", "↓ "+rtFmtRate(rtRxB),
		"__FAST_TX__", strconv.Itoa(rtTxA2),
		"__FAST_RX__", strconv.Itoa(rtRxA2),
	).Replace(rtRatesHarnessJSTemplate)
}

// rtRatesHarnessJSTemplate 是注入到页面里的自检脚本（①~⑤ 五段）。
//
// 它只做两件事：**钩 fetch**（数 /api/v1/overview 到底落地了几次）与**按用户的真实
// 操作驱动界面**（等首页两格出现 → 读值 → 让 Go 侧换速率 / 让 A 上下线 / 冻结推送 →
// 再读值），然后把观测值 POST 回去。期望值在 Go 侧。
const rtRatesHarnessJSTemplate = `(function () {
  'use strict';
  var CFG = window.__TZCFG || {};
  var WANT_UP0 = '__WANT_UP0__', WANT_DOWN0 = '__WANT_DOWN0__';
  var WANT_UP = '__WANT_UP_FAST__', WANT_DOWN = '__WANT_DOWN_FAST__';
  var WANT_OFF_UP = '__WANT_UP_OFF__', WANT_OFF_DOWN = '__WANT_DOWN_OFF__';
  var FAST_TX = __FAST_TX__, FAST_RX = __FAST_RX__;

  var R = { errs: [], steps: [], fatal: '', notes: {} };
  window.__RTRATESRESULT = R;

  window.addEventListener('error', function (e) { R.errs.push('error: ' + (e.message || e.type)); });
  window.addEventListener('unhandledrejection', function (e) {
    var r = e.reason;
    R.errs.push('rejection: ' + (r && r.message ? r.message : String(r)));
  });

  function node(id) { return document.getElementById(id); }
  function textOf(id) { var n = node(id); return n ? n.textContent : ''; }
  function shown(id) { var n = node(id); return !!n && !n.hidden; }
  function waitFor(what, cond, ms) {
    var deadline = Date.now() + (ms || 20000);
    return new Promise(function (resolve) {
      (function poll() {
        var ok = false;
        try { ok = cond(); } catch (e) { ok = false; }
        if (ok) { R.steps.push(what); resolve(true); return; }
        if (Date.now() > deadline) { resolve(false); return; }
        setTimeout(poll, 50);
      })();
    });
  }

  // ovText 按**格子自己的标签**取值，不按位置猜（顺序见 app.js 的 OVERVIEW_ITEMS）。
  function ovText(label) {
    var ov = node('overview');
    if (!ov) return '';
    for (var i = 0; i < ov.children.length; i++) {
      var cell = ov.children[i];
      var lab = cell.querySelector('.ov-label');
      if (!lab || lab.textContent !== label) continue;
      var val = cell.querySelector('.ov-value');
      return val ? val.textContent : '';
    }
    return '';
  }
  function upText() { return ovText('实时上行'); }
  function downText() { return ovText('实时下行'); }
  function liveText() { return textOf('live-text'); }

  // ---- 钩 fetch：数"响应真的交回给 app.js"的次数（轮询有没有插手全靠它）----
  var rawFetch = window.fetch.bind(window);

  function sleep(ms) { return new Promise(function (r) { setTimeout(r, ms); }); }

  // ---- 自检自己的取数：**单次 20 秒上限**（读请求失败重发一次；写请求不重发）----
  //
  // 这里原来是裸 fetch，**没有任何上限**：只要有一条响应永远不回来（半开连接、
  // keep-alive 竞态、中转层把这一笔丢了、服务端那一条 handler 挂住 —— 都不是
  // 页面能控制的），自检就永远停在那个 then 上：不 reject（于是不写 R.fatal）、
  // 不报错、也不回传。而 Go 侧等的是"页面 POST 回结果"这**一个**事件，它对
  // "页面还活着吗 / 走到哪一步了"零观测 ⇒ 只能干等到外层上限（这条用例 150 秒），
  // 失败信息里连卡在哪一步都没有。
  //
  // 现在单次给 20 秒上限：读请求失败换一条新连接重发一次（单笔丢了最常见的形态）；
  // 写请求不重发（重发一次写可能把被测状态改两遍）；持续卡死就抛错 → run() 的
  // catch 写进 R.fatal → 页面照常 finish() 回传 ⇒ Go 侧拿到"取数失败 <url>"，
  // 而不是静默超时。判据与观测值一个都没动。
  function boundedFetch(url, init, retry) {
    if (retry === undefined) retry = true;
    var left = retry ? 2 : 1;
    function once() {
      var ctl = (typeof AbortController === 'function') ? new AbortController() : null;
      var timedOut = false;
      var timer = ctl ? setTimeout(function () { timedOut = true; ctl.abort(); }, 20000) : null;
      var opts = {};
      for (var k in (init || {})) {
        if (Object.prototype.hasOwnProperty.call(init, k)) opts[k] = init[k];
      }
      if (ctl) opts.signal = ctl.signal;
      return rawFetch(url, opts).then(function (res) {
        if (timer) clearTimeout(timer);
        return res;
      }, function (err) {
        if (timer) clearTimeout(timer);
        if (--left > 0) {
          R.steps.push('取数没回来（20 秒上限），换一条请求重发一次: ' + url);
          return sleep(200).then(once);
        }
        var why = timedOut ? '20 秒没有响应（已 abort）' : (err && err.message ? err.message : String(err));
        throw new Error('取数失败（单次 20 秒上限' + (retry ? '、已重发一次' : '') + '）' + url + '：' + why);
      });
    }
    return once();
  }
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
    return rawFetch(input, init).then(function (res) {
      doneCount[p] = (doneCount[p] || 0) + 1;
      return res;
    });
  };
  function doneOf(p) { return doneCount[p] || 0; }

  // ---- 钩 EventSource：只为了留下实例读 readyState ----
  var OrigES = window.EventSource;
  var instances = [];
  var closes = 0;
  var origClose = OrigES.prototype.close;
  OrigES.prototype.close = function () {
    closes++;
    return origClose.apply(this, arguments);
  };
  function WrappedES(url, cfg) {
    var inst = new OrigES(url, cfg);
    instances.push(inst);
    // 记下每一帧到达的时刻与帧数：冻结前后"到底还有没有帧"全靠它（app.js 自己也听着
    // 同一个事件，多挂一个监听不影响它）。
    inst.addEventListener('nodes', function () {
      R.notes.frameCount = (R.notes.frameCount || 0) + 1;
      R.notes.lastFrameAt = Date.now();
    });
    return inst;
  }
  WrappedES.prototype = OrigES.prototype;
  WrappedES.CONNECTING = OrigES.CONNECTING;
  WrappedES.OPEN = OrigES.OPEN;
  WrappedES.CLOSED = OrigES.CLOSED;
  window.EventSource = WrappedES;
  function lastESState() {
    return instances.length ? instances[instances.length - 1].readyState : -1;
  }
  function esTrace() {
    return instances.length + '/' + closes;
  }

  function login() {
    return waitFor('出现登录视图', function () { return shown('view-login') || shown('view-home'); }, 30000)
      .then(function () {
        if (shown('view-home')) return true;
        node('login-user').value = CFG.user;
        node('login-pass').value = CFG.pass;
        node('login-submit').click();
        return waitFor('登录后进入应用', function () { return shown('view-home'); }, 30000);
      });
  }

  function post(path) {
    return boundedFetch(path, { method: 'POST' }, false).then(function (res) {
      if (!res || !res.ok) throw new Error(path + ' 失败: ' + (res && res.status));
      return Date.now();
    });
  }

  function run() {
    return waitFor('页面脚本就绪', function () { return !!node('view-login') && !!node('form-login'); }, 30000)
      .then(login)
      .then(function () { return waitFor('首页可见', function () { return shown('view-home'); }, 30000); })
      .then(function () {
        return waitFor('总览两格已渲染', function () { return upText() !== '' && downText() !== ''; }, 30000);
      })
      // ① 起点：两个在线节点之和（不含那台离线机器）
      .then(function () {
        return waitFor('起点读数 = 两个在线节点之和', function () {
          return upText() === WANT_UP0 && downText() === WANT_DOWN0;
        }, 20000);
      })
      .then(function () {
        R.notes.upInitial = upText();
        R.notes.downInitial = downText();
        R.notes.liveBeforeFreeze = liveText();
        R.notes.esTraceInitial = esTrace();
        // ② 口径对齐：读完屏幕**立刻**取一次 /overview（两次读数相隔几毫秒）
        R.notes.cellUpAtAlign = upText();
        R.notes.cellDownAtAlign = downText();
        R.notes.cellAt = Date.now();
        return boundedFetch('/api/v1/overview?window=1h&buckets=10').then(function (res) { return res.json(); });
      })
      .then(function (data) {
        R.notes.overviewAt = Date.now();
        R.notes.alignGapMs = R.notes.overviewAt - R.notes.cellAt;
        var t = (data && data.totals) || {};
        R.notes.ovTx = t.tx_rate;
        R.notes.ovRx = t.rx_rate;
        // ③ 换速率：6 秒内必须动，而且这 6 秒里不许有 /overview 落地
        R.notes.overviewDoneBeforeBump = doneOf('/api/v1/overview');
        R.notes.upBeforeBump = upText();
        R.notes.downBeforeBump = downText();
        return post('/__rate?tx=' + FAST_TX + '&rx=' + FAST_RX);
      })
      .then(function (at) {
        R.notes.bumpAt = at;
        return waitFor('数字跟着推送变了', function () {
          return upText() === WANT_UP && downText() === WANT_DOWN;
        }, 6000);
      })
      .then(function () {
        R.notes.changedAfterMs = Date.now() - R.notes.bumpAt;
        R.notes.overviewDoneAfterBump = doneOf('/api/v1/overview');
        R.notes.upAfterBump = upText();
        R.notes.downAfterBump = downText();
        R.notes.esTraceAfterBump = esTrace();
        // ④ 下线：A 停止上报 + 最后一次通信倒拨一小时（立刻离线，最后速率留在状态里）
        R.notes.offlineAt = Date.now();
        return post('/__offline-a');
      })
      .then(function () {
        return waitFor('合计里不再算离线的 A', function () {
          return upText() === WANT_OFF_UP && downText() === WANT_OFF_DOWN;
        }, 8000);
      })
      .then(function () {
        R.notes.offlineMs = Date.now() - R.notes.offlineAt;
        R.notes.upAfterOffline = upText();
        R.notes.downAfterOffline = downText();
        // ④' 上线：恢复上报（立刻补一拍）
        R.notes.onlineAt = Date.now();
        return post('/__online-a');
      })
      .then(function () {
        return waitFor('合计里又算上在线的 A', function () {
          return upText() === WANT_UP && downText() === WANT_DOWN;
        }, 8000);
      })
      .then(function () {
        R.notes.onlineMs = Date.now() - R.notes.onlineAt;
        R.notes.upAfterOnline = upText();
        R.notes.downAfterOnline = downText();
        R.notes.esTraceAfterOnline = esTrace();
        R.notes.liveBeforeFreeze = liveText();
        R.notes.framesBeforeFreeze = R.notes.frameCount || 0;
        // ⑤ 冻结推送：连接照旧开着（EventSource 不派发 error），但一个字节都收不到
        R.notes.freezeAt = Date.now();
        return post('/__freeze');
      })
      .then(function () {
        return waitFor('状态栏不再写「实时」', function () { return liveText() !== '实时'; }, 15000);
      })
      .then(function () {
        R.notes.freezeMs = Date.now() - R.notes.freezeAt;
        R.notes.liveDuringFreeze = liveText();
        R.notes.framesAtFreezeEnd = R.notes.frameCount || 0;
        R.notes.esStateDuringFreeze = lastESState();
        R.notes.esInstancesDuringFreeze = instances.length;
        // ⑤' 解冻：下一帧就该把它写回「实时」
        R.notes.thawAt = Date.now();
        return post('/__thaw');
      })
      .then(function () {
        // 解冻之后记下状态栏文案的变化过程（每 100 ms 采一次，只在变化时记）：
        // "为什么等了几秒"在红的时候必须一眼看得出（是不是中间写过「已断开，重连中」）。
        var seen = [];
        var deadline = Date.now() + 8000;
        return new Promise(function (resolve) {
          (function sample() {
            var t = liveText();
            if (seen[seen.length - 1] !== t) seen.push(t);
            if (t === '实时' || Date.now() > deadline) {
              R.notes.thawTimeline = seen.join(' > ');
              resolve(true);
              return;
            }
            setTimeout(sample, 100);
          })();
        });
      })
      .then(function () {
        R.notes.thawMs = Date.now() - R.notes.thawAt;
        R.notes.liveAfterThaw = liveText();
        R.notes.framesAfterThaw = R.notes.frameCount || 0;
        R.notes.lastFrameGapMs = R.notes.lastFrameAt ? Date.now() - R.notes.lastFrameAt : -1;
        R.notes.esStateAfterThaw = lastESState();
        // 这条连接**从头到尾没有重建过**才算数：中间掉过线的话，验到的就是"重连拿快照"
        // 那条路，而不是"推送恢复"（重连次数一起带回去，好让红的时候一眼看出区别）。
        R.notes.esInstancesAfterThaw = instances.length;
        R.notes.upAtEnd = upText();
        R.notes.downAtEnd = downText();
        return true;
      });
  }

  function finish() {
    return rawFetch('/__result', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(R)
    }).then(function () {
      document.title = 'DUMPMARK:rtrates:' + JSON.stringify({ errs: R.errs.length, fatal: R.fatal });
    });
  }

  // 起跑**不再等 load 事件**：load 要等所有子资源（style.css / chart.js / app.js…）
  // 全部就绪，而自检真正需要的只是 DOM —— 它第一步本来就是轮询等界面出现
  // （waitFor，30 秒上限）。把整条自检挂在一个与它无关的事件上，等于白白多一处
  // "事件不来就整条静默"的窗口：某个 defer 脚本慢/挂住时 load 永远不触发，页面
  // 既不自检也不回传，Go 侧只能干等到外层上限（连卡在哪一步都没有）。起跑提前
  // 只改变"从哪一刻开始轮询 DOM"，步骤名与顺序、判据、观测值一个都不变。
  (function () {
    run().catch(function (err) {
      R.fatal = String(err && err.message ? err.message : err);
    }).then(finish, finish);
  })();;
})();`
