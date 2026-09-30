package server

import (
	"context"
	"net/http"
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
