package server

import (
	"context"
	"math"
	"net/http"
	"strconv"
	"testing"
	"time"

	"probe/internal/protocol"
	"probe/internal/store"
)

// createNodeOverHTTP 用已登录的会话建一个节点，返回 ID 与一次性 Token。
func createNodeOverHTTP(t *testing.T, h *authHarness, name string) (int64, string) {
	t.Helper()
	status, body := h.post(t, "/api/v1/nodes", map[string]any{
		"name": name, "interval_sec": 1, "reset_day": 19, "traffic_limit": 1 << 30,
	}, nil)
	if status != http.StatusCreated {
		t.Fatalf("创建节点失败: %d %v", status, body)
	}
	token, _ := body["token"].(string)
	node, _ := body["node"].(map[string]any)
	id, _ := node["id"].(float64)
	return int64(id), token
}

// TestTrafficAccountingEndToEnd 走真实 WebSocket 帧 → 内存增量 → 落盘 → 浏览器 API。
func TestTrafficAccountingEndToEnd(t *testing.T) {
	h := newAuthHarness(t)
	nodeID, token := createNodeOverHTTP(t, h, "traffic-01")
	// 这条用例要连发多帧，先把每秒限流放大（限流本身由专门的用例覆盖）。
	h.srv.agents.msgPerSecond = 100

	conn := mustDialAgent(t, h.ts, token)
	sendFrame(t, conn, helloFrame(t, testHello()))
	readHandshake(t, conn)

	// 5 帧：第一帧只建基线，后面 4 帧各 +1 MiB / +0.5 MiB。
	const mib = 1 << 20
	for i := 1; i <= 5; i++ {
		m := testMetrics()
		m.Net.RxTotal = uint64(i) * mib
		m.Net.TxTotal = uint64(i) * mib / 2
		frame, err := protocol.New(protocol.TypeMetrics, m)
		if err != nil {
			t.Fatalf("构造帧: %v", err)
		}
		frame.Seq = uint64(i)
		sendFrame(t, conn, frame)
	}
	waitFor(t, 3*time.Second, "5 帧全部被处理", func() bool {
		n, ok := h.srv.State().Get(nodeID)
		return ok && n.Seq >= 5
	})

	// 生产里这一步由每分钟的 ticker 调用。
	h.srv.flushTraffic(context.Background())

	status, body := h.get(t, "/api/v1/nodes")
	if status != http.StatusOK {
		t.Fatalf("查询节点失败: %d", status)
	}
	dto := firstNodeFromList(t, body)

	if got := int64(dto["traffic_today_rx"].(float64)); got != 4*mib {
		t.Fatalf("今日下行 = %d，期望 %d（第一帧只建基线）", got, 4*mib)
	}
	if got := int64(dto["traffic_today_tx"].(float64)); got != 2*mib {
		t.Fatalf("今日上行 = %d，期望 %d", got, 2*mib)
	}
	if dto["traffic_cycle_rx"] != dto["traffic_today_rx"] || dto["traffic_total_rx"] != dto["traffic_today_rx"] {
		t.Fatalf("周期/累计应当与今日一致（只有一天的数据）: %v", dto)
	}
	// 今日/本周/本周期三个总和都由服务端给（前端不做算术），而且这三个口径
	// 在"只有今天有数据"时必然相等 —— 本周是从本周一算起的，今天就在本周里。
	for _, key := range []string{"traffic_today_total", "traffic_week_total", "traffic_cycle_total"} {
		if got := int64(dto[key].(float64)); got != 6*mib {
			t.Fatalf("%s = %d，期望 %d（4 MiB 下 + 2 MiB 上）", key, got, 6*mib)
		}
	}
	if dto["traffic_week_rx"] != dto["traffic_today_rx"] || dto["traffic_week_tx"] != dto["traffic_today_tx"] {
		t.Fatalf("只有今天的数据时本周应当等于今日: %v", dto)
	}
	if dto["cycle_start"] == "" || dto["cycle_end"] == "" {
		t.Fatalf("周期窗口缺失: %v", dto)
	}
	if dto["traffic_limit"] != float64(1<<30) {
		t.Fatalf("流量额度 = %v", dto["traffic_limit"])
	}
	// 4 MiB + 2 MiB = 6 MiB，额度 1 GiB → 0.59%
	if pct := dto["traffic_pct"].(float64); pct < 0.5 || pct > 0.7 {
		t.Fatalf("额度使用率 = %v%%，期望约 0.59%%", pct)
	}
	// 今日/本周的占比分母也是**月额度**：只有今天有数据时三者必须一致。
	for _, key := range []string{"traffic_today_pct", "traffic_week_pct"} {
		if pct := dto[key].(float64); pct < 0.5 || pct > 0.7 {
			t.Fatalf("%s = %v%%，期望约 0.59%%（分母是月额度）", key, pct)
		}
	}

	// 再上报一帧（模拟"服务端已落盘、Agent 继续跑"）：增量继续累计。
	m := testMetrics()
	m.Net.RxTotal = 6 * mib
	m.Net.TxTotal = 3 * mib
	frame, err := protocol.New(protocol.TypeMetrics, m)
	if err != nil {
		t.Fatalf("构造帧: %v", err)
	}
	frame.Seq = 6
	sendFrame(t, conn, frame)
	waitFor(t, 3*time.Second, "第 6 帧被处理", func() bool {
		n, ok := h.srv.State().Get(nodeID)
		return ok && n.Seq >= 6
	})
	h.srv.flushTraffic(context.Background())

	status, body = h.get(t, "/api/v1/nodes")
	if status != http.StatusOK {
		t.Fatalf("查询节点失败: %d", status)
	}
	dto = firstNodeFromList(t, body)
	if got := int64(dto["traffic_today_rx"].(float64)); got != 5*mib {
		t.Fatalf("第二次落盘后今日下行 = %d，期望 %d（同一天累加）", got, 5*mib)
	}
}

// 没填月额度时三个占比一律是 0，而三个总和照旧有值。
//
// 前端据此**整段不显示占比**：0% 会被读成"这个月一点没用"，而事实是没填额度、
// 根本无从判断。这条口径在服务端钉死，前端才敢用 "pct > 0 才显示" 这种简单判断。
func TestTrafficPctIsZeroWithoutLimit(t *testing.T) {
	h := newAuthHarness(t)
	status, body := h.post(t, "/api/v1/nodes", map[string]any{
		"name": "no-limit", "interval_sec": 1, "reset_day": 1,
	}, nil)
	if status != http.StatusCreated {
		t.Fatalf("创建节点失败: %d %v", status, body)
	}
	node, _ := body["node"].(map[string]any)
	id := int64(node["id"].(float64))

	day := store.FormatDay(time.Now().In(h.srv.loc))
	if err := h.srv.db.FlushTraffic(context.Background(), []store.TrafficUpdate{
		{NodeID: id, Day: day, RxDelta: 4096, TxDelta: 2048, RxTotal: 4096, TxTotal: 2048},
	}, time.Now()); err != nil {
		t.Fatalf("写入流量: %v", err)
	}
	h.srv.trafficCache.invalidate()

	status, body = h.get(t, "/api/v1/nodes")
	if status != http.StatusOK {
		t.Fatalf("查询节点失败: %d", status)
	}
	dto := firstNodeFromList(t, body)
	if dto["traffic_limit"].(float64) != 0 {
		t.Fatalf("这条用例的节点不该有额度: %v", dto["traffic_limit"])
	}
	for _, key := range []string{"traffic_pct", "traffic_today_pct", "traffic_week_pct"} {
		if got := dto[key].(float64); got != 0 {
			t.Errorf("没填额度时 %s 应当是 0，实际 %v", key, got)
		}
	}
	for _, key := range []string{"traffic_today_total", "traffic_week_total", "traffic_cycle_total"} {
		if got := int64(dto[key].(float64)); got != 6144 {
			t.Errorf("%s = %d，期望 6144（总和与占比互不影响）", key, got)
		}
	}
}

// 流量计费口径在**接口层**的闭环：创建时写进去 → 汇总时按口径算 → JSON 里给出来。
//
// 三台机器喂一模一样的数据（收 4096 / 发 2048），只有计费方式不同：
// 双向 6144、只算出站 2048、只算入站 4096。而 traffic_cycle_total 三台都是 6144
// —— 详情页「本周期流量」那一行写的是"这一周期真实的收 + 发"，它不跟额度比，
// 与"额度用了多少"是两个问题（用户逐条确认过的口径）。
//
// 三个百分比（本周期 / 今日 / 本周）的**分子都跟口径走**：只算出站的机器上，
// 今日那一行不能给出一个比卡片更大的占比 —— 那会让同一台机器在同一屏上出现
// 两个"额度用了多少"（用户拍板：今日 / 本周的百分比也要跟）。
//
// 这条用例里三台机器的流量都落在今天（今日 ⊆ 本周 ⊆ 本周期），所以三个百分比
// 恰好相等；"三个窗口各算各的"由 TestTrafficTodayWeekPctUseOwnWindow 单独钉。
func TestTrafficBillingModeOverHTTP(t *testing.T) {
	h := newAuthHarness(t)

	// 同一批数据：收 4096、发 2048（和 = 6144）。
	const rx, tx = 4096, 2048
	modes := []struct {
		name         string
		billing      int
		wantUsed     int64
		wantTotal    int64
		wantCyclePct float64
		wantTodayPct float64
	}{
		{"billing-both", store.BillingBoth, rx + tx, rx + tx, 100 * float64(rx+tx) / (1 << 30), 100 * float64(rx+tx) / (1 << 30)},
		{"billing-tx", store.BillingTxOnly, tx, rx + tx, 100 * float64(tx) / (1 << 30), 100 * float64(tx) / (1 << 30)},
		{"billing-rx", store.BillingRxOnly, rx, rx + tx, 100 * float64(rx) / (1 << 30), 100 * float64(rx) / (1 << 30)},
	}

	day := store.FormatDay(time.Now().In(h.srv.loc))
	ids := make(map[int64]int, len(modes)) // 节点 id → modes 下标
	created := make([]int64, 0, len(modes))
	for i, m := range modes {
		status, body := h.post(t, "/api/v1/nodes", map[string]any{
			"name": m.name, "interval_sec": 1, "reset_day": 1,
			"traffic_limit": int64(1 << 30), "traffic_billing": m.billing,
		}, nil)
		if status != http.StatusCreated {
			t.Fatalf("创建节点 %s 失败: %d %v", m.name, status, body)
		}
		node, _ := body["node"].(map[string]any)
		id := int64(node["id"].(float64))
		ids[id] = i
		created = append(created, id)
		// 请求里的值必须原样回来（创建响应就是前端拿到的那一份）。
		if got, _ := node["traffic_billing"].(float64); int(got) != m.billing {
			t.Errorf("%s 创建响应里的 traffic_billing = %v，期望 %d", m.name, node["traffic_billing"], m.billing)
		}
		if err := h.srv.db.FlushTraffic(context.Background(), []store.TrafficUpdate{
			{NodeID: id, Day: day, RxDelta: rx, TxDelta: tx, RxTotal: rx, TxTotal: tx},
		}, time.Now()); err != nil {
			t.Fatalf("写入流量: %v", err)
		}
	}
	// 测试直接往库里写（没走每分钟落盘），汇总缓存要手动失效（同 TestTrafficAPI）。
	h.srv.trafficCache.invalidate()

	status, list := h.get(t, "/api/v1/nodes")
	if status != http.StatusOK {
		t.Fatalf("查询节点失败: %d", status)
	}
	nodes, _ := list["nodes"].([]any)
	if len(nodes) != len(modes) {
		t.Fatalf("节点数 = %d，期望 %d", len(nodes), len(modes))
	}
	for _, raw := range nodes {
		node, _ := raw.(map[string]any)
		id := int64(node["id"].(float64))
		i, ok := ids[id]
		if !ok {
			t.Fatalf("列表里出现了没登记过的节点 %v", id)
		}
		m := modes[i]
		if got := int64(node["traffic_cycle_used"].(float64)); got != m.wantUsed {
			t.Errorf("%s：traffic_cycle_used = %d，期望 %d", m.name, got, m.wantUsed)
		}
		// 今日 / 本周那一行的「计费 Y」也是服务端按同一个口径算的（详情页三行现在
		// 逐字同构）：前端拿它直接拼串，不许自己把收 + 发加起来。
		if got := int64(node["traffic_today_used"].(float64)); got != m.wantUsed {
			t.Errorf("%s：traffic_today_used = %d，期望 %d（今日那一行的「计费」跟同一个口径）",
				m.name, got, m.wantUsed)
		}
		if got := int64(node["traffic_week_used"].(float64)); got != m.wantUsed {
			t.Errorf("%s：traffic_week_used = %d，期望 %d（本周那一行的「计费」跟同一个口径）",
				m.name, got, m.wantUsed)
		}
		if got := int64(node["traffic_cycle_total"].(float64)); got != m.wantTotal {
			t.Errorf("%s：traffic_cycle_total = %d，期望 %d（这一行始终是收 + 发）", m.name, got, m.wantTotal)
		}
		if got := node["traffic_pct"].(float64); math.Abs(got-m.wantCyclePct) > 1e-9 {
			t.Errorf("%s：traffic_pct = %v，期望 %v（按计费口径）", m.name, got, m.wantCyclePct)
		}
		if got := node["traffic_today_pct"].(float64); math.Abs(got-m.wantTodayPct) > 1e-9 {
			t.Errorf("%s：traffic_today_pct = %v，期望 %v（今日也跟计费口径走）",
				m.name, got, m.wantTodayPct)
		}
		if got := node["traffic_week_pct"].(float64); math.Abs(got-m.wantTodayPct) > 1e-9 {
			t.Errorf("%s：traffic_week_pct = %v，期望 %v（本周也跟计费口径走）",
				m.name, got, m.wantTodayPct)
		}
		if got := int64(node["traffic_cycle_rx"].(float64)); got != rx {
			t.Errorf("%s：traffic_cycle_rx = %d，期望 %d", m.name, got, rx)
		}
		if got := int64(node["traffic_cycle_tx"].(float64)); got != tx {
			t.Errorf("%s：traffic_cycle_tx = %d，期望 %d", m.name, got, tx)
		}
	}

	// 改口径：PUT 一台机器（只算出站 → 只算入站），再读一次列表，已用量必须换过来。
	//
	// 为什么改完要**再读一次列表**而不是看 PUT 的响应：PUT/POST 的响应是
	// dtoFor（配置 + 内存状态），它本来就不带流量汇总（那要另查 traffic_daily）——
	// 前端保存完也是靠 SSE / 下一次列表刷新拿到流量那一格的。这条断言因此走的是
	// 用户真正读数字的那条路。
	status, body := h.put(t, "/api/v1/nodes/"+strconv.FormatInt(created[1], 10), map[string]any{
		"name": "billing-tx", "interval_sec": 1, "reset_day": 1,
		"traffic_limit": int64(1 << 30), "traffic_billing": store.BillingRxOnly,
	})
	if status != http.StatusOK {
		t.Fatalf("修改节点失败: %d %v", status, body)
	}
	status, list = h.get(t, "/api/v1/nodes")
	if status != http.StatusOK {
		t.Fatalf("改完再查失败: %d", status)
	}
	nodes, _ = list["nodes"].([]any)
	seen := false
	for _, raw := range nodes {
		node, _ := raw.(map[string]any)
		if int64(node["id"].(float64)) != created[1] {
			continue
		}
		seen = true
		if got := int64(node["traffic_cycle_used"].(float64)); got != rx {
			t.Errorf("改成只算入站后 traffic_cycle_used = %d，期望 %d", got, rx)
		}
		if got := node["traffic_billing"].(float64); int(got) != store.BillingRxOnly {
			t.Errorf("改成只算入站后 traffic_billing = %v，期望 %d", node["traffic_billing"], store.BillingRxOnly)
		}
	}
	// 没填额度的机器：百分比一律 0（前端据此不显示），但按口径算出来的**已用量**
	// 照给 —— 卡片那一格在没额度时只写已用量，那个数同样要按口径。
	status, body = h.post(t, "/api/v1/nodes", map[string]any{
		"name": "billing-no-limit", "interval_sec": 1, "reset_day": 1,
		"traffic_limit": 0, "traffic_billing": store.BillingTxOnly,
	}, nil)
	if status != http.StatusCreated {
		t.Fatalf("创建无额度节点失败: %d %v", status, body)
	}
	noLimitNode, _ := body["node"].(map[string]any)
	noLimitID := int64(noLimitNode["id"].(float64))
	if err := h.srv.db.FlushTraffic(context.Background(), []store.TrafficUpdate{
		{NodeID: noLimitID, Day: day, RxDelta: 700, TxDelta: 300, RxTotal: 700, TxTotal: 300},
	}, time.Now()); err != nil {
		t.Fatalf("写入流量: %v", err)
	}
	h.srv.trafficCache.invalidate()
	status, list = h.get(t, "/api/v1/nodes")
	if status != http.StatusOK {
		t.Fatalf("改完再查失败: %d", status)
	}
	nodes, _ = list["nodes"].([]any)
	seen = false
	for _, raw := range nodes {
		node, _ := raw.(map[string]any)
		if int64(node["id"].(float64)) != noLimitID {
			continue
		}
		seen = true
		if got := node["traffic_pct"].(float64); got != 0 {
			t.Errorf("没填额度时 traffic_pct = %v，期望 0（前端据此整段不显示占比）", got)
		}
		if got := int64(node["traffic_cycle_used"].(float64)); got != 300 {
			t.Errorf("没填额度时 traffic_cycle_used = %d，期望 300（只算出站 ⇒ 用发）", got)
		}
		if got := int64(node["traffic_cycle_total"].(float64)); got != 1000 {
			t.Errorf("没填额度时 traffic_cycle_total = %d，期望 1000", got)
		}
	}
	if !seen {
		t.Fatalf("列表里找不到节点 %d", noLimitID)
	}
}

// TestTrafficTodayWeekPctUseOwnWindow 钉住"三个百分比各算各的窗口"。
//
// 上一条用例把流量全灌在今天，三个窗口恰好重合（今日 ⊆ 本周 ⊆ 本周期），
// 于是"今日的百分比用了本周期那一坨字节"这种错也照样能绿。这一条把三个窗口
// **拆开**：昨天一行（多数时候在本周与本周期里）、今天一行（只落在今日）。
// 只算出站（口径 = 发）时三个百分比因此分别是三个不同的数：
//
//	今日 = 今天的发；本周 = 今天 + 昨天的发；本周期 = 今天 + 昨天的发（默认重置日 1）。
//
// 昨天算不算"本周 / 本周期"由 store 的窗口函数决定（今天是周一 / 每月 1 号时
// 昨天落在这个窗口之外），所以期望值用**同一批窗口函数**现算 —— 这不是把实现
// 抄一遍：窗口边界（周一起算、重置日切月）本来就是 store 的公开口径，用例要的是
// "服务端把哪个窗口的字节喂给了哪个百分比"。
func TestTrafficTodayWeekPctUseOwnWindow(t *testing.T) {
	h := newAuthHarness(t)

	const limit = 1000
	const yRx, yTx = 99999, 400 // 昨天：收一大坨、发一点（口径只认发）
	const tRx, tTx = 1, 200     // 今天

	status, body := h.post(t, "/api/v1/nodes", map[string]any{
		"name": "billing-window", "interval_sec": 1, "reset_day": 1,
		"traffic_limit": int64(limit), "traffic_billing": store.BillingTxOnly,
	}, nil)
	if status != http.StatusCreated {
		t.Fatalf("创建节点失败: %d %v", status, body)
	}
	node, _ := body["node"].(map[string]any)
	id := int64(node["id"].(float64))

	now := time.Now().In(h.srv.loc)
	today := store.DayStart(now, h.srv.loc)
	yesterday := today.AddDate(0, 0, -1)
	if err := h.srv.db.FlushTraffic(context.Background(), []store.TrafficUpdate{
		{NodeID: id, Day: store.FormatDay(yesterday), RxDelta: yRx, TxDelta: yTx,
			RxTotal: yRx, TxTotal: yTx},
		{NodeID: id, Day: store.FormatDay(today), RxDelta: tRx, TxDelta: tTx,
			RxTotal: tRx, TxTotal: tTx},
	}, time.Now()); err != nil {
		t.Fatalf("写入流量: %v", err)
	}
	h.srv.trafficCache.invalidate()

	// 昨天落没落在这两个窗口里，由 store 的窗口函数说了算。
	yInWeek := !yesterday.Before(store.WeekStart(now, h.srv.loc))
	yInCycle := !yesterday.Before(store.CycleStart(now, 1, h.srv.loc))
	t.Logf("今天 = %s（周一 = %v / 每月 1 号 = %v）：昨天在本周 = %v，在本周期 = %v",
		store.FormatDay(today), now.Weekday() == time.Monday, today.Day() == 1, yInWeek, yInCycle)

	status, list := h.get(t, "/api/v1/nodes")
	if status != http.StatusOK {
		t.Fatalf("查询节点失败: %d", status)
	}
	nodes, _ := list["nodes"].([]any)
	if len(nodes) != 1 {
		t.Fatalf("节点数 = %d，期望 1", len(nodes))
	}
	got, _ := nodes[0].(map[string]any)

	// 百分比：分子是各自窗口里**按口径**的字节（只算出站 ⇒ 用发），分母是月额度。
	wantToday := 100 * float64(tTx) / limit
	wantWeek := wantToday
	if yInWeek {
		wantWeek += 100 * float64(yTx) / limit
	}
	wantCycle := wantToday
	if yInCycle {
		wantCycle += 100 * float64(yTx) / limit
	}
	for _, c := range []struct {
		key  string
		want float64
	}{
		{"traffic_today_pct", wantToday},
		{"traffic_week_pct", wantWeek},
		{"traffic_pct", wantCycle},
	} {
		if v := got[c.key].(float64); math.Abs(v-c.want) > 1e-9 {
			t.Errorf("%s = %v，期望 %v（各自窗口里按计费口径的字节 ÷ 月额度）", c.key, v, c.want)
		}
	}
	// 三个合计仍然是收 + 发的事实，一个都不跟口径走。
	for _, c := range []struct {
		key  string
		want int64
	}{
		{"traffic_today_total", tRx + tTx},
		{"traffic_week_total", tRx + tTx + boolBytes(yInWeek, yRx+yTx)},
		{"traffic_cycle_total", tRx + tTx + boolBytes(yInCycle, yRx+yTx)},
	} {
		if v := int64(got[c.key].(float64)); v != c.want {
			t.Errorf("%s = %d，期望 %d（合计始终是收 + 发）", c.key, v, c.want)
		}
	}
	// 三行右边写的「计费 Y」= 各自窗口里按口径的字节（不是收 + 发），而且与上面
	// 那三个百分比是同一批字节（同一处 store.BilledBytes 算出来的）。
	for _, c := range []struct {
		key  string
		want int64
	}{
		{"traffic_today_used", tTx},
		{"traffic_week_used", tTx + boolBytes(yInWeek, yTx)},
		{"traffic_cycle_used", tTx + boolBytes(yInCycle, yTx)},
	} {
		if v := int64(got[c.key].(float64)); v != c.want {
			t.Errorf("%s = %d，期望 %d（各自窗口里按计费口径的字节，只算出站 ⇒ 只数发）",
				c.key, v, c.want)
		}
	}
}

// boolBytes 在条件成立时返回 n，否则 0（用例里算"这一天算不算在这个窗口里"）。
func boolBytes(ok bool, n int64) int64 {
	if ok {
		return n
	}
	return 0
}

func TestTrafficAPI(t *testing.T) {
	h := newAuthHarness(t)
	nodeID, _ := createNodeOverHTTP(t, h, "traffic-02")

	// 没有任何流量时也必须是 200 + 补齐 7 天的 0。
	status, body := h.get(t, "/api/v1/nodes/1/traffic")
	if status != http.StatusOK {
		t.Fatalf("流量接口失败: %d %v", status, body)
	}
	if body["days"] != float64(7) {
		t.Fatalf("默认天数 = %v", body["days"])
	}
	points, _ := body["points"].([]any)
	if len(points) != 7 {
		t.Fatalf("点数 = %d，期望 7（缺的日期补 0）", len(points))
	}
	for i, raw := range points {
		p, _ := raw.([]any)
		if len(p) != 3 {
			t.Fatalf("第 %d 个点结构 = %v，期望 [ts, rx, tx]", i, p)
		}
		if p[1].(float64) != 0 || p[2].(float64) != 0 {
			t.Fatalf("第 %d 个点应当为 0: %v", i, p)
		}
	}
	today, _ := body["today"].(map[string]any)
	if today["rx"] != float64(0) {
		t.Fatalf("今日流量应当为 0: %v", today)
	}

	// 写入一天的数据后应当出现在最后一个点上。
	day := store.FormatDay(time.Now().In(h.srv.loc))
	if err := h.srv.db.FlushTraffic(context.Background(), []store.TrafficUpdate{
		{NodeID: nodeID, Day: day, RxDelta: 1234, TxDelta: 567, RxTotal: 1234, TxTotal: 567},
	}, time.Now()); err != nil {
		t.Fatalf("写入流量: %v", err)
	}
	// 测试是直接往库里写（没走服务端的每分钟落盘），所以要手动让汇总缓存失效——
	// 生产路径里 flushTraffic 成功后会自己失效。
	h.srv.trafficCache.invalidate()
	status, body = h.get(t, "/api/v1/nodes/1/traffic?days=3")
	if status != http.StatusOK {
		t.Fatalf("流量接口失败: %d %v", status, body)
	}
	points, _ = body["points"].([]any)
	if len(points) != 3 {
		t.Fatalf("days=3 时点数 = %d", len(points))
	}
	last, _ := points[2].([]any)
	if last[1].(float64) != 1234 || last[2].(float64) != 567 {
		t.Fatalf("今天的数据不对: %v", last)
	}
	today, _ = body["today"].(map[string]any)
	if today["rx"] != float64(1234) || today["tx"] != float64(567) {
		t.Fatalf("今日汇总不对: %v", today)
	}

	// 参数校验。
	for _, days := range []string{"0", "91", "abc", "-1"} {
		status, _ := h.get(t, "/api/v1/nodes/1/traffic?days="+days)
		if status != http.StatusBadRequest {
			t.Errorf("days=%s 应当 400，实际 %d", days, status)
		}
	}

	// 不存在的节点 / 未登录。
	if status, _ := h.get(t, "/api/v1/nodes/999/traffic"); status != http.StatusNotFound {
		t.Errorf("不存在的节点应当 404，实际 %d", status)
	}
}
