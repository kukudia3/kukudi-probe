package server

import (
	"context"
	"net/http"
	"testing"
	"time"

	"probe/internal/store"
)

// TestTrafficAggregatesAreCachedAndInvalidated 验证 1 Hz 循环每秒要用的流量汇总
// 走缓存（它每分钟才变一次），同时"落盘/节点增删"后必须立刻失效。
func TestTrafficAggregatesAreCachedAndInvalidated(t *testing.T) {
	h := newAuthHarness(t)
	ctx := context.Background()
	nodeID, _ := createNodeOverHTTP(t, h, "cache-01")

	nodes, err := h.srv.db.ListNodes(ctx)
	if err != nil {
		t.Fatalf("读取节点: %v", err)
	}
	now := time.Now()

	first, err := h.srv.trafficAggregates(ctx, nodes, now)
	if err != nil {
		t.Fatalf("汇总: %v", err)
	}
	if len(first) != 1 {
		t.Fatalf("汇总条目 = %d，期望 1", len(first))
	}

	// 直接往库里加流量：缓存还在（30 秒 TTL），所以看不出变化——
	// 这正是"每秒不再查库"的代价，正常路径靠落盘后失效来保证新鲜度。
	day := store.FormatDay(now.In(h.srv.loc))
	if err := h.srv.db.FlushTraffic(ctx, []store.TrafficUpdate{
		{NodeID: nodeID, Day: day, RxDelta: 4096, TxDelta: 1024, RxTotal: 4096, TxTotal: 1024},
	}, now); err != nil {
		t.Fatalf("写入流量: %v", err)
	}
	cached, err := h.srv.trafficAggregates(ctx, nodes, now.Add(time.Second))
	if err != nil {
		t.Fatalf("汇总: %v", err)
	}
	if cached[nodeID].TodayRx != first[nodeID].TodayRx {
		t.Fatal("缓存应当生效（同一分钟内不该重复查库）")
	}

	// 服务端自己落盘后必须失效，用户立刻看到新流量。
	h.srv.trafficCache.invalidate()
	fresh, err := h.srv.trafficAggregates(ctx, nodes, now.Add(2*time.Second))
	if err != nil {
		t.Fatalf("汇总: %v", err)
	}
	if fresh[nodeID].TodayRx != 4096 || fresh[nodeID].TodayTx != 1024 {
		t.Fatalf("失效后应当读到新值: %+v", fresh[nodeID])
	}

	// 超过 TTL 也应当自动刷新。
	stale, err := h.srv.trafficAggregates(ctx, nodes, now.Add(2*time.Second+trafficCacheTTL+time.Second))
	if err != nil {
		t.Fatalf("汇总: %v", err)
	}
	if stale[nodeID].TodayRx != 4096 {
		t.Fatalf("TTL 之后应当重新查库: %+v", stale[nodeID])
	}
}

// TestTrafficCacheInvalidatedOnNodeChanges 验证增删节点后缓存立刻失效，
// 否则新节点的流量会显示为 0 直到 TTL 过期。
func TestTrafficCacheInvalidatedOnNodeChanges(t *testing.T) {
	h := newAuthHarness(t)
	ctx := context.Background()
	createNodeOverHTTP(t, h, "c1")

	if _, err := h.srv.currentNodes(ctx); err != nil { // 先填充缓存
		t.Fatalf("读取节点: %v", err)
	}
	if _, ok := h.srv.trafficCache.get(time.Now()); !ok {
		t.Fatal("第一次读取后应当有缓存")
	}

	// 新增节点 → 缓存失效。
	createNodeOverHTTP(t, h, "c2")
	if _, ok := h.srv.trafficCache.get(time.Now()); ok {
		t.Fatal("新增节点后缓存应当失效")
	}

	if _, err := h.srv.currentNodes(ctx); err != nil {
		t.Fatalf("读取节点: %v", err)
	}
	if _, ok := h.srv.trafficCache.get(time.Now()); !ok {
		t.Fatal("应当重新填充缓存")
	}

	// 删除节点 → 也失效。
	if status, body, _ := h.do(t, http.MethodDelete, "/api/v1/nodes/2", nil, true, nil); status != http.StatusOK {
		t.Fatalf("删除节点失败: %d %v", status, body)
	}
	if _, ok := h.srv.trafficCache.get(time.Now()); ok {
		t.Fatal("删除节点后缓存应当失效")
	}
}

// TestSingleNodeTrafficRequestsDoNotPoisonCache 是「单节点调用污染全量缓存」的回归用例。
//
// 详情页与流量接口传的是**单元素**节点列表，它们的结果不能落进那份"全量节点"缓存：
// 缓存没有「覆盖了哪些节点」这个维度，一旦被单节点结果覆盖，之后最多 30 秒内
// 其它节点（以及 /nodes、/overview、SSE）的流量会全部显示成 0，并让告警引擎
// 误判"流量已超额"已经恢复、从而多推一条通知。
func TestSingleNodeTrafficRequestsDoNotPoisonCache(t *testing.T) {
	h := newAuthHarness(t)
	ctx := context.Background()
	first, _ := createNodeOverHTTP(t, h, "pollute-01")
	second, _ := createNodeOverHTTP(t, h, "pollute-02")

	// 两台机器都有真实用量：只有"全量"这条路径才该同时看到它们。
	day := store.FormatDay(time.Now().In(h.srv.loc))
	if err := h.srv.db.FlushTraffic(ctx, []store.TrafficUpdate{
		{NodeID: first, Day: day, RxDelta: 4096, TxDelta: 1024, RxTotal: 4096, TxTotal: 1024},
		{NodeID: second, Day: day, RxDelta: 8192, TxDelta: 2048, RxTotal: 8192, TxTotal: 2048},
	}, time.Now()); err != nil {
		t.Fatalf("写入流量: %v", err)
	}

	// ① 详情页（单节点）：它自己的那份汇总必须照旧正确。
	status, body := h.get(t, "/api/v1/nodes/1")
	if status != http.StatusOK {
		t.Fatalf("详情页失败: %d %v", status, body)
	}
	if got := nodeTodayTotal(t, body["node"]); got != 5120 {
		t.Fatalf("详情页今日流量 = %d，期望 5120", got)
	}
	// ② 流量接口（单节点）：同上。
	status, body = h.get(t, "/api/v1/nodes/2/traffic")
	if status != http.StatusOK {
		t.Fatalf("流量接口失败: %d %v", status, body)
	}
	today, _ := body["today"].(map[string]any)
	if got := int64(today["rx"].(float64) + today["tx"].(float64)); got != 10240 {
		t.Fatalf("流量接口今日流量 = %d，期望 10240", got)
	}

	// ③ 这两个请求都不该往那份全量缓存里写东西。
	if _, ok := h.srv.trafficCache.get(time.Now()); ok {
		t.Fatal("单节点请求污染了全量流量缓存")
	}

	// ④ 紧接着读全量列表：两台机器的今日流量都必须是真实值（而不是被污染的 0）。
	status, body = h.get(t, "/api/v1/nodes")
	if status != http.StatusOK {
		t.Fatalf("查询节点列表失败: %d %v", status, body)
	}
	want := map[int64]int64{first: 5120, second: 10240}
	seen := 0
	for _, raw := range body["nodes"].([]any) {
		node, _ := raw.(map[string]any)
		id := int64(node["id"].(float64))
		seen++
		if got := nodeTodayTotal(t, node); got != want[id] {
			t.Errorf("节点 %d 的今日流量 = %d，期望 %d（全量视图被单节点请求污染）", id, got, want[id])
		}
	}
	if seen != 2 {
		t.Fatalf("节点列表条目 = %d，期望 2", seen)
	}
}

// nodeTodayTotal 从节点 DTO 里读出"今日流量合计"。
func nodeTodayTotal(t *testing.T, raw any) int64 {
	t.Helper()
	node, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("节点 DTO 结构不对: %T", raw)
	}
	rx, _ := node["traffic_today_rx"].(float64)
	tx, _ := node["traffic_today_tx"].(float64)
	return int64(rx + tx)
}
