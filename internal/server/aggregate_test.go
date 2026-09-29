package server

import (
	"testing"
	"time"

	"probe/internal/protocol"
)

func sampleMetrics(cpu, rx, lat float64) protocol.Metrics {
	return protocol.Metrics{
		CPUPct: cpu,
		Mem:    protocol.Mem{Total: 100, Used: 50, Pct: 50},
		Swap:   protocol.Mem{Pct: 5},
		Disk:   []protocol.Disk{{Mount: "/", Pct: 60}},
		Load:   protocol.Load{L1: 0.5},
		Net:    protocol.Net{Iface: "eth0", RxRate: rx, TxRate: 10},
		LatMS:  lat,
	}
}

func TestAccumulatorAggregatesOneBucket(t *testing.T) {
	a := newAccumulator(bucketWidth)
	t0 := time.Unix(1_700_000_000, 0) // 能被 10 整除

	for i := 0; i < 3; i++ {
		a.add(1, 1, sampleMetrics(float64(10*(i+1)), float64(100*(i+1)), float64(20+i)),
			t0.Add(time.Duration(i)*time.Second))
	}

	// 桶还没结束：不落盘。
	if got := a.flushClosed(t0.Add(5 * time.Second)); got != nil {
		t.Fatalf("未结束的桶不该落盘，实际 %d 个", len(got))
	}

	buckets := a.flushClosed(t0.Add(10 * time.Second))
	if len(buckets) != 1 {
		t.Fatalf("落盘桶数 = %d，期望 1", len(buckets))
	}
	b := buckets[0]
	if b.TS != t0.Unix() {
		t.Fatalf("桶起点 = %d，期望 %d", b.TS, t0.Unix())
	}
	if b.CPUAvg != 20 || b.CPUMax != 30 {
		t.Fatalf("CPU avg/max = %v/%v，期望 20/30", b.CPUAvg, b.CPUMax)
	}
	if b.RxRate != 200 || b.RxMax != 300 {
		t.Fatalf("rx avg/max = %v/%v，期望 200/300", b.RxRate, b.RxMax)
	}
	if b.LatAvg != 21 || b.LatMin != 20 || b.LatMax != 22 {
		t.Fatalf("延迟 avg/min/max = %v/%v/%v，期望 21/20/22", b.LatAvg, b.LatMin, b.LatMax)
	}
	if b.DiskAvg != 60 || b.MemAvg != 50 || b.SwapAvg != 5 || b.LoadAvg != 0.5 {
		t.Fatalf("其它指标聚合不对: %+v", b)
	}
	// up/all 都乘了 uptimeScale（600），所以这里断言"比值"与缩放后的绝对值：
	// 3 个样本 / 1 秒间隔 → 10 个应有样本 → 3*600 / 10*600。
	if b.Up != 3*uptimeScale || b.All != 10*uptimeScale {
		t.Fatalf("up/all = %d/%d，期望 %d/%d", b.Up, b.All, 3*uptimeScale, 10*uptimeScale)
	}
	if pct := float64(b.Up) / float64(b.All) * 100; pct < 29.9 || pct > 30.1 {
		t.Fatalf("可用率 = %v%%，期望 30%%", pct)
	}

	// 落盘后队列为空。
	if got := a.flushClosed(t0.Add(20 * time.Second)); got != nil {
		t.Fatalf("落盘后不该再有数据: %d", len(got))
	}
}

func TestAccumulatorRespectsInterval(t *testing.T) {
	a := newAccumulator(bucketWidth)
	t0 := time.Unix(1_700_000_000, 0)

	// 5 秒上报一次：10 秒桶里应有 2 个样本，收到 2 个 → 100%。
	a.add(2, 5, sampleMetrics(10, 100, 20), t0)
	a.add(2, 5, sampleMetrics(20, 200, 30), t0.Add(5*time.Second))

	buckets := a.flushClosed(t0.Add(10 * time.Second))
	if len(buckets) != 1 {
		t.Fatalf("桶数 = %d", len(buckets))
	}
	if buckets[0].Up != 2*uptimeScale || buckets[0].All != 2*uptimeScale {
		t.Fatalf("up/all = %d/%d，期望 %d/%d",
			buckets[0].Up, buckets[0].All, 2*uptimeScale, 2*uptimeScale)
	}
}

// 上报间隔大于桶宽（10 秒）时，每个桶"应有"的帧数是分数，
// 整数计数必须靠 uptimeScale 才不丢精度——否则 all_cnt 会恒为 0，
// 可用率永远算不出来（只能显示"—"）。
func TestAccumulatorHandlesIntervalLargerThanBucket(t *testing.T) {
	for _, interval := range []int{1, 2, 5, 10, 15, 30, 60, 300} {
		a := newAccumulator(bucketWidth)
		t0 := time.Unix(1_700_000_000, 0)
		a.add(1, interval, sampleMetrics(10, 100, 20), t0)

		buckets := a.flushClosed(t0.Add(10 * time.Second))
		if len(buckets) != 1 {
			t.Fatalf("间隔 %d：桶数 = %d", interval, len(buckets))
		}
		b := buckets[0]
		if b.All <= 0 {
			t.Fatalf("间隔 %d：all_cnt = %d，必须大于 0（否则可用率算不出来）", interval, b.All)
		}
		// all 应当等于 10 秒 / 间隔 × 缩放，误差不超过 1/600。
		want := float64(uptimeScale) * float64(bucketWidth) / float64(interval)
		if diff := float64(b.All) - want; diff > 1 || diff < -1 {
			t.Fatalf("间隔 %d：all_cnt = %d，期望约 %.1f", interval, b.All, want)
		}
	}
}

func TestAccumulatorRollsOverToNewBucket(t *testing.T) {
	a := newAccumulator(bucketWidth)
	t0 := time.Unix(1_700_000_000, 0)

	a.add(1, 1, sampleMetrics(10, 100, 20), t0)
	a.add(1, 1, sampleMetrics(90, 900, 90), t0.Add(11*time.Second)) // 进入下一个桶

	// t0+19 时第二个桶还没结束（它的结束时刻是 t0+20）。
	buckets := a.flushClosed(t0.Add(19 * time.Second))
	if len(buckets) != 1 {
		t.Fatalf("桶数 = %d，期望 1（新桶还没结束）", len(buckets))
	}
	if buckets[0].CPUAvg != 10 {
		t.Fatalf("落盘的应当是旧桶，实际 cpu_avg = %v", buckets[0].CPUAvg)
	}
	if buckets[0].TS != t0.Unix() {
		t.Fatalf("旧桶起点 = %d", buckets[0].TS)
	}

	// 到了 t0+20，第二个桶也结束了。
	buckets = a.flushClosed(t0.Add(20 * time.Second))
	if len(buckets) != 1 || buckets[0].CPUAvg != 90 {
		t.Fatalf("第二个桶落盘不对: %+v", buckets)
	}
}

func TestAccumulatorSeparatesNodes(t *testing.T) {
	a := newAccumulator(bucketWidth)
	t0 := time.Unix(1_700_000_000, 0)

	a.add(1, 1, sampleMetrics(10, 100, 20), t0)
	a.add(2, 1, sampleMetrics(90, 900, 20), t0)

	buckets := a.flushClosed(t0.Add(10 * time.Second))
	if len(buckets) != 2 {
		t.Fatalf("桶数 = %d，期望 2（每个节点一个）", len(buckets))
	}
	seen := map[int64]float64{}
	for _, b := range buckets {
		seen[b.NodeID] = b.CPUAvg
	}
	if seen[1] != 10 || seen[2] != 90 {
		t.Fatalf("节点数据串了: %v", seen)
	}
}
