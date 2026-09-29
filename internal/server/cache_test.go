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
