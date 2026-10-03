package store

import (
	"context"
	"math"
	"testing"
	"time"
)

func TestInsertBucketsIsIdempotent(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()
	ts := time.Now().Unix() - 100

	bucket := SampleBucket{
		NodeID: 1, TS: ts,
		CPUAvg: 10, CPUMax: 20, MemAvg: 30, MemMax: 40, SwapAvg: 1,
		DiskAvg: 50, DiskMax: 51, LoadAvg: 0.5,
		RxRate: 100, RxMax: 200, TxRate: 50, TxMax: 60,
		LatAvg: 20, LatMin: 18, LatMax: 25,
		Up: 10, All: 10,
	}
	if err := db.InsertBuckets(ctx, TableSamples10s, []SampleBucket{bucket}); err != nil {
		t.Fatalf("首次写入: %v", err)
	}

	// 同一个 (node_id, ts) 再写一次：值被覆盖，行数不变。
	bucket.CPUAvg = 99
	if err := db.InsertBuckets(ctx, TableSamples10s, []SampleBucket{bucket}); err != nil {
		t.Fatalf("重复写入: %v", err)
	}

	var count int
	if err := db.Reader().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM samples_10s WHERE node_id = 1`).Scan(&count); err != nil {
		t.Fatalf("统计: %v", err)
	}
	if count != 1 {
		t.Fatalf("行数 = %d，期望 1（幂等写入）", count)
	}

	var cpuAvg float64
	var up, all int64
	if err := db.Reader().QueryRowContext(ctx,
		`SELECT cpu_avg, up_cnt, all_cnt FROM samples_10s WHERE node_id = 1 AND ts = ?`, ts).
		Scan(&cpuAvg, &up, &all); err != nil {
		t.Fatalf("读取: %v", err)
	}
	if cpuAvg != 99 {
		t.Fatalf("覆盖写入未生效：cpu_avg = %v", cpuAvg)
	}
	if up != 10 || all != 10 {
		t.Fatalf("up/all = %d/%d", up, all)
	}
}

func TestInsertBucketsRejectsUnknownTable(t *testing.T) {
	db := openTemp(t)
	err := db.InsertBuckets(context.Background(), "nodes; DROP TABLE nodes", []SampleBucket{{NodeID: 1, TS: 1}})
	if err == nil {
		t.Fatal("非法表名应当被拒绝")
	}
}

func TestRollupSamplesAggregatesAndIsIdempotent(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()

	// 一分钟的起点对齐到 60 秒网格。
	base := time.Now().Add(-10 * time.Minute).Unix()
	base -= base % 60

	buckets := make([]SampleBucket, 0, 6)
	for i := int64(0); i < 6; i++ {
		buckets = append(buckets, SampleBucket{
			NodeID: 7,
			TS:     base + i*10,
			CPUAvg: float64(10 + i), CPUMax: float64(20 + i),
			MemAvg: 50, MemMax: 55, DiskAvg: 60, DiskMax: 61,
			RxRate: float64(100 * (i + 1)), RxMax: float64(200 * (i + 1)),
			TxRate: 10, TxMax: 20, LatAvg: float64(30 + i), LatMin: float64(25 + i), LatMax: float64(40 + i),
			Up: 10, All: 10,
		})
	}
	if err := db.InsertBuckets(ctx, TableSamples10s, buckets); err != nil {
		t.Fatalf("写入 10 秒桶: %v", err)
	}

	until := time.Unix(base+120, 0)
	n, err := db.RollupSamples(ctx, TableSamples10s, TableSamples1m, 60, []int64{7}, time.Unix(base-60, 0), until)
	if err != nil {
		t.Fatalf("聚合: %v", err)
	}
	if n == 0 {
		t.Fatal("应当至少聚合出一行")
	}

	var cpuAvg, cpuMax, rxAvg, rxMax, latMin, latMax float64
	var up, all int64
	if err := db.Reader().QueryRowContext(ctx,
		`SELECT cpu_avg, cpu_max, rx_rate, rx_max, lat_min, lat_max, up_cnt, all_cnt
		 FROM samples_1m WHERE node_id = 7 AND ts = ?`, base).
		Scan(&cpuAvg, &cpuMax, &rxAvg, &rxMax, &latMin, &latMax, &up, &all); err != nil {
		t.Fatalf("读取聚合结果: %v", err)
	}
	// cpu_avg = (10+11+12+13+14+15)/6 = 12.5，cpu_max = 25
	if cpuAvg != 12.5 || cpuMax != 25 {
		t.Fatalf("cpu_avg/cpu_max = %v/%v，期望 12.5/25", cpuAvg, cpuMax)
	}
	if rxAvg != 350 || rxMax != 1200 {
		t.Fatalf("rx 聚合 = %v/%v，期望 350/1200", rxAvg, rxMax)
	}
	if latMin != 25 || latMax != 45 {
		t.Fatalf("lat 最值 = %v/%v，期望 25/45", latMin, latMax)
	}
	if up != 60 || all != 60 {
		t.Fatalf("up/all = %d/%d，期望 60/60", up, all)
	}

	// 再跑一次：结果不变（幂等）。
	if _, err := db.RollupSamples(ctx, TableSamples10s, TableSamples1m, 60, []int64{7}, time.Unix(base-60, 0), until); err != nil {
		t.Fatalf("重复聚合: %v", err)
	}
	var count int
	if err := db.Reader().QueryRowContext(ctx, `SELECT COUNT(*) FROM samples_1m WHERE node_id = 7`).Scan(&count); err != nil {
		t.Fatalf("统计: %v", err)
	}
	if count != 1 {
		t.Fatalf("重复聚合后行数 = %d，期望 1", count)
	}
}

func TestRollupSkipsUnclosedBuckets(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()
	now := time.Now()

	// 刚写入的桶还没结束（ts + 60 > now），不应被聚合。
	ts := now.Unix() - (now.Unix() % 60)
	if err := db.InsertBuckets(ctx, TableSamples10s, []SampleBucket{{NodeID: 1, TS: ts, CPUAvg: 5}}); err != nil {
		t.Fatalf("写入: %v", err)
	}
	if _, err := db.RollupSamples(ctx, TableSamples10s, TableSamples1m, 60, []int64{1}, now.Add(-time.Hour), now); err != nil {
		t.Fatalf("聚合: %v", err)
	}
	var count int
	if err := db.Reader().QueryRowContext(ctx, `SELECT COUNT(*) FROM samples_1m`).Scan(&count); err != nil {
		t.Fatalf("统计: %v", err)
	}
	if count != 0 {
		t.Fatalf("未封闭的桶被聚合了：%d 行", count)
	}
}

// TestRollupCoversLastClosedBucket 覆盖一个 off-by-one：
// 已经结束的最后一个桶必须被聚合（曾经因为多减了一个 width 而被整桶跳过）。
func TestRollupCoversLastClosedBucket(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()

	base := time.Now().Add(-10 * time.Minute).Unix()
	base -= base % 60 // 对齐到分钟

	buckets := make([]SampleBucket, 0, 6)
	for i := int64(0); i < 6; i++ {
		buckets = append(buckets, SampleBucket{NodeID: 1, TS: base + i*10, CPUAvg: 10, Up: 1, All: 1})
	}
	if err := db.InsertBuckets(ctx, TableSamples10s, buckets); err != nil {
		t.Fatalf("写入: %v", err)
	}

	// until = 桶结束时刻 + 30 秒：这一分钟已经封闭，必须被聚合。
	until := time.Unix(base+60, 0).Add(30 * time.Second)
	if _, err := db.RollupSamples(ctx, TableSamples10s, TableSamples1m, 60, []int64{1}, time.Unix(base-60, 0), until); err != nil {
		t.Fatalf("聚合: %v", err)
	}

	var count int
	if err := db.Reader().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM samples_1m WHERE node_id = 1 AND ts = ?`, base).Scan(&count); err != nil {
		t.Fatalf("统计: %v", err)
	}
	if count != 1 {
		t.Fatal("已经封闭的最后一分钟没有被聚合（rollup 少算了一个桶）")
	}
}

// TestRollupNeverShrinksExistingBucket 覆盖"保留策略先删掉部分源行"的场景：
// 残缺的重算结果不能覆盖掉已经算好的 1 分钟行。
func TestRollupNeverShrinksExistingBucket(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()

	base := time.Now().Add(-10 * time.Minute).Unix()
	base -= base % 60

	buckets := make([]SampleBucket, 0, 6)
	for i := int64(0); i < 6; i++ {
		buckets = append(buckets, SampleBucket{
			NodeID: 1, TS: base + i*10, CPUAvg: 10, CPUMax: 30, RxMax: 1000, Up: 60, All: 60,
		})
	}
	if err := db.InsertBuckets(ctx, TableSamples10s, buckets); err != nil {
		t.Fatalf("写入: %v", err)
	}
	until := time.Unix(base+120, 0)
	if _, err := db.RollupSamples(ctx, TableSamples10s, TableSamples1m, 60, []int64{1}, time.Unix(base-60, 0), until); err != nil {
		t.Fatalf("首次聚合: %v", err)
	}

	var upBefore, allBefore int64
	var maxBefore float64
	if err := db.Reader().QueryRowContext(ctx,
		`SELECT up_cnt, all_cnt, cpu_max FROM samples_1m WHERE node_id = 1 AND ts = ?`, base).
		Scan(&upBefore, &allBefore, &maxBefore); err != nil {
		t.Fatalf("读取: %v", err)
	}

	// 模拟保留策略：删掉这一分钟里的 5 个 10 秒源桶，只剩 1 个。
	if _, err := db.Writer().ExecContext(ctx,
		`DELETE FROM samples_10s WHERE node_id = 1 AND ts > ? AND ts < ?`, base, base+50); err != nil {
		t.Fatalf("删除源行: %v", err)
	}
	if _, err := db.RollupSamples(ctx, TableSamples10s, TableSamples1m, 60, []int64{1}, time.Unix(base-60, 0), until); err != nil {
		t.Fatalf("重算: %v", err)
	}

	var upAfter, allAfter int64
	var maxAfter float64
	if err := db.Reader().QueryRowContext(ctx,
		`SELECT up_cnt, all_cnt, cpu_max FROM samples_1m WHERE node_id = 1 AND ts = ?`, base).
		Scan(&upAfter, &allAfter, &maxAfter); err != nil {
		t.Fatalf("读取: %v", err)
	}
	if upAfter != upBefore || allAfter != allBefore {
		t.Fatalf("残缺重算覆盖了完整结果：up/all %d/%d → %d/%d",
			upBefore, allBefore, upAfter, allAfter)
	}
	if maxAfter != maxBefore {
		t.Fatalf("残缺重算改写了峰值：cpu_max %v → %v", maxBefore, maxAfter)
	}
}

// TestRollupOnlyTouchesGivenNodes 验证聚合是按给定节点做的（既省扫描，也不会误伤别的节点）。
func TestRollupOnlyTouchesGivenNodes(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()

	base := time.Now().Add(-10 * time.Minute).Unix()
	base -= base % 60
	buckets := []SampleBucket{
		{NodeID: 1, TS: base, CPUAvg: 10, Up: 1, All: 1},
		{NodeID: 2, TS: base, CPUAvg: 20, Up: 1, All: 1},
	}
	if err := db.InsertBuckets(ctx, TableSamples10s, buckets); err != nil {
		t.Fatalf("写入: %v", err)
	}
	if _, err := db.RollupSamples(ctx, TableSamples10s, TableSamples1m, 60, []int64{1},
		time.Unix(base-60, 0), time.Unix(base+120, 0)); err != nil {
		t.Fatalf("聚合: %v", err)
	}

	n1, err := db.CountSamples(ctx, TableSamples1m, 1, time.Unix(0, 0), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("统计节点 1: %v", err)
	}
	n2, err := db.CountSamples(ctx, TableSamples1m, 2, time.Unix(0, 0), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("统计节点 2: %v", err)
	}
	if n1 != 1 {
		t.Fatalf("节点 1 应当被聚合出 1 行，实际 %d", n1)
	}
	if n2 != 0 {
		t.Fatalf("节点 2 不在给定列表里，不该被聚合，实际 %d 行", n2)
	}
}

func TestDeleteOldSamples(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()
	now := time.Now()

	old := now.Add(-48 * time.Hour).Unix()
	fresh := now.Add(-time.Minute).Unix()
	buckets := []SampleBucket{
		{NodeID: 1, TS: old}, {NodeID: 1, TS: fresh},
		{NodeID: 2, TS: old}, {NodeID: 2, TS: fresh},
	}
	if err := db.InsertBuckets(ctx, TableSamples10s, buckets); err != nil {
		t.Fatalf("写入: %v", err)
	}

	// 只清理节点 1，且只清理 24 小时以前的。
	n, err := db.DeleteOldSamples(ctx, TableSamples10s, now.Add(-24*time.Hour), []int64{1})
	if err != nil {
		t.Fatalf("清理: %v", err)
	}
	if n != 1 {
		t.Fatalf("删除行数 = %d，期望 1", n)
	}

	var node1, node2 int64
	if err := db.Reader().QueryRowContext(ctx, `SELECT COUNT(*) FROM samples_10s WHERE node_id = 1`).Scan(&node1); err != nil {
		t.Fatalf("统计节点 1: %v", err)
	}
	if err := db.Reader().QueryRowContext(ctx, `SELECT COUNT(*) FROM samples_10s WHERE node_id = 2`).Scan(&node2); err != nil {
		t.Fatalf("统计节点 2: %v", err)
	}
	if node1 != 1 {
		t.Fatalf("节点 1 剩余 %d 行，期望 1（新的那行要保留）", node1)
	}
	if node2 != 2 {
		t.Fatalf("节点 2 剩余 %d 行，期望 2（没让它清理）", node2)
	}
}

func TestQuerySeriesBucketsAndLimits(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()
	now := time.Now()

	// 往 1 分钟表里放 2000 个点（约 33 小时），验证会被聚合而不是原样返回。
	rows := make([]SampleBucket, 0, 2000)
	start := now.Add(-33 * time.Hour)
	for i := int64(0); i < 2000; i++ {
		rows = append(rows, SampleBucket{
			NodeID: 3,
			TS:     start.Unix() + i*60,
			CPUAvg: float64(i % 100), CPUMax: float64(i%100) + 5,
			Up: 60, All: 60,
		})
	}
	if err := db.InsertBuckets(ctx, TableSamples1m, rows); err != nil {
		t.Fatalf("写入: %v", err)
	}

	for _, r := range Ranges() {
		points, err := db.QuerySeries(ctx, 3, "cpu", r, now)
		if err != nil {
			t.Fatalf("%s 查询失败: %v", r.Key, err)
		}
		if len(points) > maxPoints {
			t.Errorf("%s 返回 %d 个点，超过上限 %d", r.Key, len(points), maxPoints)
		}
		for i := 1; i < len(points); i++ {
			if points[i].TS <= points[i-1].TS {
				t.Fatalf("%s 的点没有按时间递增", r.Key)
			}
			if points[i].TS%r.Bucket != 0 {
				t.Fatalf("%s 的点没有落在 %d 秒网格上: %d", r.Key, r.Bucket, points[i].TS)
			}
		}
		// 7d 档必须把 1 分钟数据聚合到 15 分钟。
		if r.Key == "7d" && r.Bucket != 900 {
			t.Fatalf("7d 桶宽 = %d，期望 900", r.Bucket)
		}
	}

	// 数据都在 1 分钟表里，所以 1h 档（源是 10 秒表）应当没有点。
	hour, _ := RangeByKey("1h")
	points, err := db.QuerySeries(ctx, 3, "cpu", hour, now)
	if err != nil {
		t.Fatalf("查询 1h: %v", err)
	}
	if len(points) != 0 {
		t.Fatalf("1h 档源表是 samples_10s，不该读到 1 分钟表的数据：%d 个点", len(points))
	}
}

func TestQuerySeriesRejectsUnknownMetric(t *testing.T) {
	db := openTemp(t)
	r, _ := RangeByKey("1h")
	for _, metric := range []string{"", "cpu; DROP TABLE samples_10s", "unknown", "cpu_avg"} {
		if _, err := db.QuerySeries(context.Background(), 1, metric, r, time.Now()); err == nil {
			t.Errorf("指标 %q 应当被拒绝", metric)
		}
	}
}

func TestQueryUptime(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()
	now := time.Now()

	// 10 个桶里只有 6 个收到满额上报，另外 4 个各少一半 → 可用率 80%。
	rows := make([]SampleBucket, 0, 10)
	start := now.Add(-30 * time.Minute)
	for i := int64(0); i < 10; i++ {
		up := int64(10)
		if i >= 6 {
			up = 5
		}
		rows = append(rows, SampleBucket{NodeID: 5, TS: start.Unix() + i*10, Up: up, All: 10})
	}
	if err := db.InsertBuckets(ctx, TableSamples10s, rows); err != nil {
		t.Fatalf("写入: %v", err)
	}

	r, _ := RangeByKey("1h")
	pct, has, err := db.QueryUptime(ctx, 5, r, now)
	if err != nil {
		t.Fatalf("查询可用率: %v", err)
	}
	if !has {
		t.Fatal("应当有数据")
	}
	if pct < 79.9 || pct > 80.1 {
		t.Fatalf("可用率 = %v，期望 80", pct)
	}

	// 没有数据的节点：has=false。
	if _, has, err := db.QueryUptime(ctx, 999, r, now); err != nil || has {
		t.Fatalf("无数据时应当 has=false（err=%v has=%v）", err, has)
	}
}

// TestRuntimeRoundTripRawKernelCounterBoundary 钉住 raw 那一列的存储边界。
//
// validateNet 现在允许 rx_raw/tx_raw 到 2^63-1（内核 64 位计数快照，理由见
// internal/protocol/validate.go 的 maxInt64 注释），这条用例证明这一层真的存得下、
// 读得回；同时它也是"界为什么是 2^63 而不是 MaxUint64"的另一半：>= 2^63 时
// database/sql 会报 "uint64 values with high bit set are not supported"，
// 并且**整批**运行态落盘一起回滚（实测见 _audit/ROUND5-RAWCOUNT.md §3.2）。
func TestRuntimeRoundTripRawKernelCounterBoundary(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()
	now := time.Now()

	if _, _, err := db.CreateNode(ctx, validNewNode(), now); err != nil {
		t.Fatalf("创建节点: %v", err)
	}

	rows := []RuntimeRow{{
		NodeID: 1, LastSeen: now.Unix(), Status: "online", CPUPct: 12.5, MemPct: 50,
		SwapPct: 1, DiskPct: 60, Load1: 0.4, LatMS: 20, UptimeSec: 3600,
		BootID: "boot-1", Iface: "eth0", RxRaw: math.MaxInt64, TxRaw: 1 << 53,
		AgentVersion: "0.1.0", Kernel: "6.1.0", OSName: "Debian", CPUModel: "Xeon",
	}}
	if err := db.UpsertRuntime(ctx, rows, now); err != nil {
		t.Fatalf("写入 2^63-1 的 raw: %v", err)
	}

	loaded, err := db.LoadRuntime(ctx)
	if err != nil {
		t.Fatalf("读取运行态: %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("运行态行数 = %d，期望 1", len(loaded))
	}
	if loaded[0].RxRaw != math.MaxInt64 || loaded[0].TxRaw != 1<<53 {
		t.Fatalf("raw 往返不一致: rx=%d tx=%d", loaded[0].RxRaw, loaded[0].TxRaw)
	}
}

func TestRuntimeRoundTrip(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()
	now := time.Now()

	if _, _, err := db.CreateNode(ctx, validNewNode(), now); err != nil {
		t.Fatalf("创建节点: %v", err)
	}

	rows := []RuntimeRow{{
		NodeID: 1, LastSeen: now.Unix(), Status: "online",
		CPUPct: 12.5, MemPct: 50, SwapPct: 1, DiskPct: 60, Load1: 0.4, LatMS: 20,
		UptimeSec: 3600, BootID: "boot-1", Iface: "eth0", RxRaw: 100, TxRaw: 200,
		AgentVersion: "0.1.0", Kernel: "6.1.0", OSName: "Debian", CPUModel: "Xeon",
	}}
	if err := db.UpsertRuntime(ctx, rows, now); err != nil {
		t.Fatalf("写入运行态: %v", err)
	}
	// 覆盖写。
	rows[0].CPUPct = 42
	if err := db.UpsertRuntime(ctx, rows, now.Add(time.Minute)); err != nil {
		t.Fatalf("重复写入运行态: %v", err)
	}

	loaded, err := db.LoadRuntime(ctx)
	if err != nil {
		t.Fatalf("读取运行态: %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("运行态行数 = %d，期望 1", len(loaded))
	}
	got := loaded[0]
	if got.CPUPct != 42 || got.Iface != "eth0" || got.UptimeSec != 3600 || got.OSName != "Debian" {
		t.Fatalf("运行态内容不对: %+v", got)
	}
	// 流量基线列在 Phase 7 之前必须保持 0（表示"还没有基线"）。
	var rxTotal, txTotal int64
	if err := db.Reader().QueryRowContext(ctx,
		`SELECT rx_total, tx_total FROM node_runtime WHERE node_id = 1`).Scan(&rxTotal, &txTotal); err != nil {
		t.Fatalf("读取基线: %v", err)
	}
	if rxTotal != 0 || txTotal != 0 {
		t.Fatalf("流量基线不该被运行态写入碰过: rx=%d tx=%d", rxTotal, txTotal)
	}
}
