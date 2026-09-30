package server

import (
	"context"
	"net/http"
	"testing"
	"time"

	"probe/internal/protocol"
	"probe/internal/store"
)

// TestUptimeWorksForSlowIntervals 覆盖一个曾经的静默错误：
// 上报间隔大于桶宽（10 秒）时，all_cnt 的整数除法结果是 0，
// 可用率永远算不出来（界面一直显示"—"）。
func TestUptimeWorksForSlowIntervals(t *testing.T) {
	for _, interval := range []int{1, 3, 7, 15, 30, 60, 300} {
		h := newAuthHarness(t)
		nodeID, _ := createNodeOverHTTP(t, h, "slow-01")
		ctx := context.Background()

		// 模拟"节点按 interval 正常上报 10 分钟"：每 10 秒的桶里按间隔落 0 或 1 帧。
		now := time.Now()
		base := now.Add(-10 * time.Minute).Unix()
		base -= base % 10
		frames := int64(0)
		for i := int64(0); i < 600; i += int64(interval) {
			at := time.Unix(base+i, 0)
			h.srv.agg.add(nodeID, interval, testMetricsForDetail(), at)
			frames++
		}
		// 推进到"现在"之后，让所有桶都封闭并落盘。
		buckets := h.srv.agg.flushClosed(now.Add(time.Minute))
		if err := h.srv.db.InsertBuckets(ctx, store.TableSamples10s, buckets); err != nil {
			t.Fatalf("写入桶: %v", err)
		}

		rg, _ := store.RangeByKey("1h")
		pct, has, err := h.srv.db.QueryUptime(ctx, nodeID, rg, now)
		if err != nil {
			t.Fatalf("间隔 %d：查询可用率失败: %v", interval, err)
		}
		if !has {
			t.Fatalf("间隔 %d：应当能算出可用率（曾经因为 all_cnt=0 而算不出来）", interval)
		}
		if pct < 95 || pct > 100 {
			t.Fatalf("间隔 %d：一直正常上报的节点可用率 = %.2f%%，期望接近 100%%", interval, pct)
		}
	}
}

// TestUptimeReflectsMissingReports 验证"少报一半"会被如实反映，而不是被算成 100%。
func TestUptimeReflectsMissingReports(t *testing.T) {
	h := newAuthHarness(t)
	nodeID, _ := createNodeOverHTTP(t, h, "half-01")
	ctx := context.Background()

	now := time.Now()
	base := now.Add(-10 * time.Minute).Unix()
	base -= base % 10

	// 每 10 秒的桶里只上报 5 次（应有 10 次）→ 50%。
	for i := int64(0); i < 600; i += 10 {
		at := time.Unix(base+i, 0)
		for j := 0; j < 5; j++ {
			h.srv.agg.add(nodeID, 1, testMetricsForDetail(), at.Add(time.Duration(j)*time.Second))
		}
	}
	buckets := h.srv.agg.flushClosed(now.Add(time.Minute))
	if err := h.srv.db.InsertBuckets(ctx, store.TableSamples10s, buckets); err != nil {
		t.Fatalf("写入桶: %v", err)
	}

	rg, _ := store.RangeByKey("1h")
	pct, has, err := h.srv.db.QueryUptime(ctx, nodeID, rg, now)
	if err != nil || !has {
		t.Fatalf("查询可用率: %v has=%v", err, has)
	}
	if pct < 49 || pct > 51 {
		t.Fatalf("可用率 = %.2f%%，期望约 50%%", pct)
	}
}

// TestTrafficCommitKeepsConcurrentFrames 覆盖一个流量少算的竞态：
// snapshot 与 commit 之间（一次写事务的时间）到达的帧，不能被 commit 清零丢掉。
func TestTrafficCommitKeepsConcurrentFrames(t *testing.T) {
	tr := newTrafficTracker(1 << 40)
	tr.observe(1, trafficMetrics(0, 0))      // 建基线
	tr.observe(1, trafficMetrics(1000, 500)) // +1000/+500
	updates := tr.snapshot("2026-09-29")
	if len(updates) != 1 || updates[0].RxDelta != 1000 {
		t.Fatalf("快照内容不对: %+v", updates)
	}

	// 落盘期间又来了两帧（+600/+300）。
	tr.observe(1, trafficMetrics(1600, 800))

	tr.commit(updates)

	rx, tx, _, _ := tr.pending(1)
	if rx != 600 || tx != 300 {
		t.Fatalf("落盘期间的增量被丢了：pending = %d/%d，期望 600/300", rx, tx)
	}

	// 下一轮必须把这部分写出去（而不是补 0）。
	second := tr.snapshot("2026-09-29")
	if len(second) != 1 || second[0].RxDelta != 600 || second[0].TxDelta != 300 {
		t.Fatalf("第二轮快照 = %+v，期望 600/300", second)
	}
	if second[0].RxTotal != 1600 || second[0].TxTotal != 800 {
		t.Fatalf("第二轮基线 = %d/%d，期望 1600/800", second[0].RxTotal, second[0].TxTotal)
	}
	tr.commit(second)
	if rx, tx, _, _ := tr.pending(1); rx != 0 || tx != 0 {
		t.Fatalf("两轮之后 pending 应当清零: %d/%d", rx, tx)
	}
}

// TestTrafficIgnoresUnknownNodes 验证"已删除节点"的残留帧会被忽略：
// 否则外键约束会让整批流量落盘失败，拖垮所有节点的记账。
func TestTrafficIgnoresUnknownNodes(t *testing.T) {
	tr := newTrafficTracker(1 << 40)
	tr.setKnown([]int64{1})
	tr.observe(7, trafficMetrics(0, 0))
	tr.observe(7, trafficMetrics(1<<20, 0))

	if updates := tr.snapshot("2026-09-29"); len(updates) != 0 {
		t.Fatalf("不存在的节点不该产生待落盘数据: %+v", updates)
	}

	// 节点被删掉之后，它的残留状态也要一起清掉。
	tr.observe(1, trafficMetrics(0, 0))
	tr.setKnown([]int64{2})
	if _, _, _, ok := tr.pending(1); ok {
		t.Fatal("已删除节点的状态应当被清掉")
	}
}

// TestDeleteNodeDisconnectsAgent 覆盖"删了还在上报"：
// 已删除节点继续上报会让流量落盘因外键约束失败。
func TestDeleteNodeDisconnectsAgent(t *testing.T) {
	h := newAuthHarness(t)
	nodeID, token := createNodeOverHTTP(t, h, "del-01")
	h.srv.traffic.setKnown([]int64{nodeID})

	conn := mustDialAgent(t, h.ts, token)
	sendFrame(t, conn, helloFrame(t, testHello()))
	readHandshake(t, conn)
	waitFor(t, 3*time.Second, "连接登记", func() bool { return h.srv.agents.activeCount() == 1 })

	if status, body, _ := h.do(t, http.MethodDelete, "/api/v1/nodes/1", nil, true, nil); status != http.StatusOK {
		t.Fatalf("删除节点失败: %d %v", status, body)
	}

	// 连接被服务端断开。
	waitFor(t, 3*time.Second, "连接被断开", func() bool { return h.srv.agents.activeCount() == 0 })

	// 再喂一帧到内存聚合/流量记账：不该产生任何待落盘数据。
	h.srv.agg.add(nodeID, 1, testMetricsForDetail(), time.Now())
	if updates := h.srv.traffic.snapshot(store.FormatDay(time.Now())); len(updates) != 0 {
		t.Fatalf("已删除节点不该有待落盘流量: %+v", updates)
	}
	buckets := h.srv.agg.flushClosed(time.Now().Add(time.Minute))
	for _, b := range buckets {
		if b.NodeID == nodeID {
			t.Fatalf("已删除节点不该再产生历史桶: %+v", b)
		}
	}
}

// TestDisableNodeDisconnectsAgent 验证"停用"是立刻生效的，而不是等下一条连接。
func TestDisableNodeDisconnectsAgent(t *testing.T) {
	h := newAuthHarness(t)
	nodeID, token := createNodeOverHTTP(t, h, "disable-01")

	conn := mustDialAgent(t, h.ts, token)
	sendFrame(t, conn, helloFrame(t, testHello()))
	readHandshake(t, conn)
	waitFor(t, 3*time.Second, "连接登记", func() bool { return h.srv.agents.activeCount() == 1 })

	status, body, _ := h.do(t, http.MethodPatch, "/api/v1/nodes/1", map[string]any{
		"name": "disable-01", "interval_sec": 1, "reset_day": 1, "enabled": false,
	}, true, nil)
	if status != http.StatusOK {
		t.Fatalf("停用失败: %d %v", status, body)
	}
	waitFor(t, 3*time.Second, "连接被断开", func() bool { return h.srv.agents.activeCount() == 0 })

	// 节点不再显示"在线"（连接断开后 Detach 会把 connected 置 false）。
	nodes, err := h.srv.currentNodes(context.Background())
	if err != nil {
		t.Fatalf("读取节点: %v", err)
	}
	if len(nodes) != 1 || nodes[0].Connected {
		t.Fatalf("停用后不该还显示已连接: %+v", nodes)
	}
	_ = nodeID
}

// TestAccumulatorRequeueIsBounded 验证"写库失败后放回队列"是安全的、
// 而且不会在库长期不可写时无限吃内存。
func TestAccumulatorRequeueIsBounded(t *testing.T) {
	a := newAccumulator(bucketWidth)
	batch := make([]store.SampleBucket, 0, 10)
	for i := 0; i < 10; i++ {
		batch = append(batch, store.SampleBucket{NodeID: 1, TS: int64(i * 10)})
	}
	if dropped := a.requeue(batch); dropped != 0 {
		t.Fatalf("第一批不该被丢弃: %d", dropped)
	}
	if got := a.flushClosed(time.Now().Add(time.Hour)); len(got) != 10 {
		t.Fatalf("放回的桶应当能再次取出: %d", len(got))
	}

	// 灌满超过上限的数据：超出的部分被丢弃，但队列本身有界。
	huge := make([]store.SampleBucket, maxReadyBuckets+500)
	if dropped := a.requeue(huge); dropped != 500 {
		t.Fatalf("超出上限应当丢弃 500 个，实际 %d", dropped)
	}
	if got := a.flushClosed(time.Now().Add(time.Hour)); len(got) != maxReadyBuckets {
		t.Fatalf("队列应当封顶在 %d，实际 %d", maxReadyBuckets, len(got))
	}
}

// TestFlushSamplesWritesAllClosedBuckets 验证写入路径把一批桶整批落库（供失败重试依赖）。
func TestFlushSamplesWritesAllClosedBuckets(t *testing.T) {
	h := newAuthHarness(t)
	nodeID, _ := createNodeOverHTTP(t, h, "flush-01")
	ctx := context.Background()

	now := time.Now()
	base := now.Add(-5 * time.Minute).Unix()
	base -= base % 10
	for i := int64(0); i < 6; i++ {
		h.srv.agg.add(nodeID, 1, protocol.Metrics{CPUPct: float64(i), Mem: protocol.Mem{Pct: 50},
			Disk: []protocol.Disk{{Mount: "/", Pct: 10}}, Net: protocol.Net{Iface: "eth0"}}, time.Unix(base+i*10, 0))
	}
	h.srv.flushSamples(ctx)
	count, err := h.srv.db.CountSamples(ctx, store.TableSamples10s, nodeID, time.Unix(base-60, 0), now)
	if err != nil {
		t.Fatalf("统计: %v", err)
	}
	if count != 6 {
		t.Fatalf("落盘桶数 = %d，期望 6", count)
	}
}
