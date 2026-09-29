package server

import (
	"testing"
	"time"

	"probe/internal/protocol"
	"probe/internal/store"
)

func trafficMetrics(rx, tx uint64) protocol.Metrics {
	m := testMetrics()
	m.Net.RxTotal = rx
	m.Net.TxTotal = tx
	return m
}

func TestTrafficTrackerFirstSeenOnlySetsBaseline(t *testing.T) {
	tr := newTrafficTracker(1 << 40)

	// 首次观测：只建基线，绝不把"安装以来的累计值"当成流量。
	if reason := tr.observe(1, trafficMetrics(5<<40, 3<<40)); reason != resetFirstSeen {
		t.Fatalf("首次观测原因 = %q，期望 %q", reason, resetFirstSeen)
	}
	rx, tx, base, _ := tr.pending(1)
	if rx != 0 || tx != 0 {
		t.Fatalf("首次观测不该产生增量: %d/%d", rx, tx)
	}
	if base != [2]uint64{5 << 40, 3 << 40} {
		t.Fatalf("基线 = %v", base)
	}
}

func TestTrafficTrackerAccumulatesIdempotently(t *testing.T) {
	tr := newTrafficTracker(1 << 40)
	tr.observe(1, trafficMetrics(1000, 500)) // first_seen

	if reason := tr.observe(1, trafficMetrics(2000, 800)); reason != "" {
		t.Fatalf("正常上报不该重设基线: %q", reason)
	}
	rx, tx, _, _ := tr.pending(1)
	if rx != 1000 || tx != 300 {
		t.Fatalf("增量 = %d/%d，期望 1000/300", rx, tx)
	}

	// 重复帧（同样的累计值）→ 增量为 0，不会重复计。
	if reason := tr.observe(1, trafficMetrics(2000, 800)); reason != "" {
		t.Fatalf("重复帧不该重设基线: %q", reason)
	}
	rx, tx, _, _ = tr.pending(1)
	if rx != 1000 || tx != 300 {
		t.Fatalf("重复帧改变了增量: %d/%d", rx, tx)
	}

	// 跳帧（Agent 中间丢了几拍）→ 增量按差值补齐，不丢流量。
	tr.observe(1, trafficMetrics(5000, 900))
	rx, tx, _, _ = tr.pending(1)
	if rx != 4000 || tx != 400 {
		t.Fatalf("跳帧后的增量 = %d/%d，期望 4000/400", rx, tx)
	}
}

func TestTrafficTrackerAgentResetCountsNothing(t *testing.T) {
	tr := newTrafficTracker(1 << 40)
	tr.observe(1, trafficMetrics(900, 900))
	tr.observe(1, trafficMetrics(1000, 1000))

	// Agent 重装 / checkpoint 丢失：累计值变小 → 重设基线，加 0。
	if reason := tr.observe(1, trafficMetrics(10, 20)); reason != resetAgentReset {
		t.Fatalf("原因 = %q，期望 %q", reason, resetAgentReset)
	}
	rx, tx, base, _ := tr.pending(1)
	if rx != 100 || tx != 100 {
		t.Fatalf("重设前已有的增量应当保留: %d/%d", rx, tx)
	}
	if base != [2]uint64{10, 20} {
		t.Fatalf("基线未重设: %v", base)
	}

	// 重设之后继续正常累加。
	tr.observe(1, trafficMetrics(110, 40))
	rx, tx, _, _ = tr.pending(1)
	if rx != 200 || tx != 120 {
		t.Fatalf("重设后的增量 = %d/%d，期望 200/120", rx, tx)
	}
}

func TestTrafficTrackerRejectsAbsurdDelta(t *testing.T) {
	tr := newTrafficTracker(1 << 20) // 上限 1 MiB
	tr.observe(1, trafficMetrics(0, 0))

	if reason := tr.observe(1, trafficMetrics(2<<20, 0)); reason != resetTooLarge {
		t.Fatalf("原因 = %q，期望 %q", reason, resetTooLarge)
	}
	rx, _, base, _ := tr.pending(1)
	if rx != 0 {
		t.Fatalf("可疑增量不该被计入: %d", rx)
	}
	if base[0] != 2<<20 {
		t.Fatalf("基线未重设: %v", base)
	}
}

func TestTrafficTrackerSnapshotThenCommit(t *testing.T) {
	tr := newTrafficTracker(1 << 40)
	tr.observe(1, trafficMetrics(0, 0))
	tr.observe(1, trafficMetrics(500, 100))

	updates := tr.snapshot("2026-09-29")
	if len(updates) != 1 {
		t.Fatalf("待落盘条数 = %d，期望 1", len(updates))
	}
	if updates[0].RxDelta != 500 || updates[0].Day != "2026-09-29" || updates[0].RxTotal != 500 {
		t.Fatalf("待落盘内容不对: %+v", updates[0])
	}

	// 还没 commit：可以再取一次（落盘失败要能重试）。
	if again := tr.snapshot("2026-09-29"); len(again) != 1 || again[0].RxDelta != 500 {
		t.Fatalf("未提交前应当能重复取到同一批数据: %+v", again)
	}

	tr.commit(updates)
	if again := tr.snapshot("2026-09-29"); len(again) != 0 {
		t.Fatalf("提交后不该还有待落盘数据: %+v", again)
	}
	rx, tx, base, _ := tr.pending(1)
	if rx != 0 || tx != 0 || base != [2]uint64{500, 100} {
		t.Fatalf("提交后状态不对: rx=%d tx=%d base=%v", rx, tx, base)
	}

	// 只有基线变化（重设）时也要能被 flush 出去。
	tr.observe(1, trafficMetrics(10, 10))
	updates = tr.snapshot("2026-09-29")
	if len(updates) != 1 || updates[0].RxDelta != 0 || updates[0].RxTotal != 10 {
		t.Fatalf("重设基线后应当有待落盘项: %+v", updates)
	}
}

// 「Server 重启」场景：基线来自数据库，重启期间的流量不会丢、也不会重复计。
func TestTrafficTrackerSurvivesServerRestart(t *testing.T) {
	first := newTrafficTracker(1 << 40)
	first.observe(1, trafficMetrics(1000, 500))
	first.observe(1, trafficMetrics(2000, 800))
	updates := first.snapshot("2026-09-29")
	first.commit(updates)

	// 重启：新的 tracker 从数据库载入基线（这里模拟载入后的状态）。
	second := newTrafficTracker(1 << 40)
	second.seed(map[int64][2]uint64{1: {updates[0].RxTotal, updates[0].TxTotal}})

	// 重启期间 Agent 又涨了 500/200：这笔流量必须补上。
	if reason := second.observe(1, trafficMetrics(2500, 1000)); reason != "" {
		t.Fatalf("重启后不该重设基线: %q", reason)
	}
	rx, tx, _, _ := second.pending(1)
	if rx != 500 || tx != 200 {
		t.Fatalf("重启后的增量 = %d/%d，期望 500/200", rx, tx)
	}
}

func TestBuildTrafficAggSplitsTodayCycleTotal(t *testing.T) {
	loc := time.UTC
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, loc)

	nodes := []store.Node{
		{ID: 1, ResetDay: 19, TrafficLimit: 1000},
		{ID: 2, ResetDay: 1, TrafficLimit: 0},
	}
	daily := []store.DailyTraffic{
		{NodeID: 1, Day: "2026-08-20", Rx: 100, Tx: 10}, // 上个周期
		{NodeID: 1, Day: "2026-09-19", Rx: 200, Tx: 20}, // 本周期
		{NodeID: 1, Day: "2026-09-25", Rx: 300, Tx: 30}, // 今天
		{NodeID: 2, Day: "2026-09-25", Rx: 5, Tx: 5},    // 今天（周期 1 号起）
		{NodeID: 2, Day: "2026-08-31", Rx: 7, Tx: 7},    // 上个周期
	}
	totals := map[int64][2]int64{1: {600, 60}, 2: {12, 12}}

	aggs := buildTrafficAgg(now, loc, nodes, daily, totals)

	one := aggs[1]
	if one.TodayRx != 300 || one.TodayTx != 30 {
		t.Fatalf("节点 1 今日 = %d/%d，期望 300/30", one.TodayRx, one.TodayTx)
	}
	if one.CycleRx != 500 || one.CycleTx != 50 {
		t.Fatalf("节点 1 本周期 = %d/%d，期望 500/50（8-20 不算）", one.CycleRx, one.CycleTx)
	}
	if one.TotalRx != 600 || one.TotalTx != 60 {
		t.Fatalf("节点 1 累计 = %d/%d", one.TotalRx, one.TotalTx)
	}
	if one.CycleStart.Format("2006-01-02") != "2026-09-19" {
		t.Fatalf("节点 1 周期起点 = %s", one.CycleStart.Format("2006-01-02"))
	}
	if one.CycleEnd.Format("2006-01-02") != "2026-10-19" {
		t.Fatalf("节点 1 周期结束 = %s", one.CycleEnd.Format("2006-01-02"))
	}

	two := aggs[2]
	if two.CycleRx != 5 || two.TotalRx != 12 {
		t.Fatalf("节点 2 周期/累计 = %d/%d，期望 5/12", two.CycleRx, two.TotalRx)
	}
	if two.CycleStart.Format("2006-01-02") != "2026-09-01" {
		t.Fatalf("节点 2 周期起点 = %s", two.CycleStart.Format("2006-01-02"))
	}
}
