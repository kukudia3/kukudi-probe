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

// 「本周」= 本周一 00:00 到现在（按配置时区切天，见 store.WeekStart）。
//
// 边界钉在这里：**上周日**的记录一分都不能算进本周，本周一的记录必须算进去。
// 这条线一旦画错（比如按周日切周），表现是"周一早上打开面板，本周流量 = 昨天的量"，
// 而这种错在页面上看起来完全正常 —— 只有一个数字偏了，没有别的线索。
func TestBuildTrafficAggWeekBoundary(t *testing.T) {
	loc := time.UTC
	// 2026-09-23 是星期三，本周一是 2026-09-21。
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, loc)
	if now.Weekday() != time.Wednesday {
		t.Fatalf("用例前提不成立：2026-09-23 是 %s", now.Weekday())
	}

	nodes := []store.Node{{ID: 1, ResetDay: 1}}
	daily := []store.DailyTraffic{
		{NodeID: 1, Day: "2026-09-20", Rx: 1000, Tx: 100}, // 上周日：不属于本周
		{NodeID: 1, Day: "2026-09-21", Rx: 200, Tx: 20},   // 本周一 00:00 起
		{NodeID: 1, Day: "2026-09-22", Rx: 30, Tx: 3},     // 本周二
		{NodeID: 1, Day: "2026-09-23", Rx: 4, Tx: 5},      // 今天
	}
	totals := map[int64][2]int64{1: {1234, 128}}

	agg := buildTrafficAgg(now, loc, nodes, daily, totals)[1]

	if agg.WeekRx != 234 || agg.WeekTx != 28 {
		t.Fatalf("本周 = %d/%d，期望 234/28（上周日的 1000/100 不能算进来）",
			agg.WeekRx, agg.WeekTx)
	}
	if agg.TodayRx != 4 || agg.TodayTx != 5 {
		t.Fatalf("今日 = %d/%d，期望 4/5", agg.TodayRx, agg.TodayTx)
	}
	// 本周期（1 号重置）包含这四天里的每一天 —— 与「本周」是两个不同口径。
	if agg.CycleRx != 1234 || agg.CycleTx != 128 {
		t.Fatalf("本周期 = %d/%d，期望 1234/128", agg.CycleRx, agg.CycleTx)
	}
}

// 今天正好是周一时，「本周」必须等于「今日」（同一个公式的自然结果，不是特例）。
func TestBuildTrafficAggWeekEqualsTodayOnMonday(t *testing.T) {
	loc := time.UTC
	now := time.Date(2026, 9, 21, 9, 0, 0, 0, loc) // 星期一
	if now.Weekday() != time.Monday {
		t.Fatalf("用例前提不成立：2026-09-21 是 %s", now.Weekday())
	}

	nodes := []store.Node{{ID: 1, ResetDay: 1}}
	daily := []store.DailyTraffic{
		{NodeID: 1, Day: "2026-09-20", Rx: 777, Tx: 777}, // 周日：上一周
		{NodeID: 1, Day: "2026-09-21", Rx: 11, Tx: 22},   // 今天 = 本周一
	}

	agg := buildTrafficAgg(now, loc, nodes, daily, nil)[1]
	if agg.WeekRx != agg.TodayRx || agg.WeekTx != agg.TodayTx {
		t.Fatalf("周一时本周(%d/%d)应当等于今日(%d/%d)",
			agg.WeekRx, agg.WeekTx, agg.TodayRx, agg.TodayTx)
	}
	if agg.WeekRx != 11 || agg.WeekTx != 22 {
		t.Fatalf("本周 = %d/%d，期望 11/22", agg.WeekRx, agg.WeekTx)
	}
}

// applyTraffic 的口径：三行流量各自的总和、以及**以月额度为分母**的占比。
//
// 今日/本周的分母也是月额度：它们回答的是"这个月用了多少"，不是"今天的用量里
// 收占多少、发占多少"。没填额度时三个占比一律 0，前端据此整段不显示占比。
func TestApplyTrafficTotalsAndPct(t *testing.T) {
	loc := time.UTC
	cycleStart := time.Date(2026, 9, 1, 0, 0, 0, 0, loc)
	cycleEnd := time.Date(2026, 10, 1, 0, 0, 0, 0, loc)
	agg := trafficAgg{
		TodayRx: 4, TodayTx: 6,
		WeekRx: 40, WeekTx: 60,
		CycleRx: 400, CycleTx: 600,
		TotalRx: 4000, TotalTx: 6000,
		CycleStart: cycleStart, CycleEnd: cycleEnd,
	}

	// 有额度：1 GB 额度，今日 10 B / 本周 100 B / 本周期 1000 B。
	dto := nodeDTO{TrafficLimit: 1e9}
	applyTraffic(&dto, agg, loc)
	if dto.TrafficTodayTotal != 10 || dto.TrafficWeekTotal != 100 || dto.TrafficCycleTotal != 1000 {
		t.Fatalf("三个总和不对: today=%d week=%d cycle=%d",
			dto.TrafficTodayTotal, dto.TrafficWeekTotal, dto.TrafficCycleTotal)
	}
	if dto.TrafficTodayRx != 4 || dto.TrafficTodayTx != 6 ||
		dto.TrafficWeekRx != 40 || dto.TrafficWeekTx != 60 ||
		dto.TrafficCycleRx != 400 || dto.TrafficCycleTx != 600 ||
		dto.TrafficTotalRx != 4000 || dto.TrafficTotalTx != 6000 {
		t.Fatalf("收/发没有原样透传: %+v", dto)
	}
	for name, got := range map[string]float64{
		"today": dto.TrafficTodayPct, "week": dto.TrafficWeekPct, "cycle": dto.TrafficPct,
	} {
		// 10/1e9、100/1e9、1000/1e9 都是极小的数，只断言"算过且一致"：
		// 1000/1e9*100 = 1e-4 %。
		if got <= 0 {
			t.Errorf("%s 占比应当大于 0，实际 %v", name, got)
		}
	}
	// 本周期占比 = 总和 / 额度 × 100（口径与原来完全一致）。
	// 用容差比而不是 == ：1000/1e9×100 的浮点结果是 9.999999999999999e-05，
	// 而字面量常量 1e-4 在编译期是按精确值取整的，两者差 1 个 ULP。
	if diff := dto.TrafficPct - 1e-4; diff > 1e-12 || diff < -1e-12 {
		t.Errorf("本周期占比 = %v，期望约 1e-4%%", dto.TrafficPct)
	}
	if dto.CycleStart != "2026-09-01" || dto.CycleEnd != "2026-10-01" {
		t.Errorf("周期窗口 = %s → %s", dto.CycleStart, dto.CycleEnd)
	}

	// 没额度：三个占比都是 0（前端据此不显示占比），总和照旧给。
	noLimit := nodeDTO{}
	applyTraffic(&noLimit, agg, loc)
	if noLimit.TrafficPct != 0 || noLimit.TrafficTodayPct != 0 || noLimit.TrafficWeekPct != 0 {
		t.Fatalf("没填额度时三个占比都应当是 0: cycle=%v today=%v week=%v",
			noLimit.TrafficPct, noLimit.TrafficTodayPct, noLimit.TrafficWeekPct)
	}
	if noLimit.TrafficTodayTotal != 10 || noLimit.TrafficCycleTotal != 1000 {
		t.Fatalf("没填额度不该影响总和: %+v", noLimit)
	}
}

// 占比上限 999：超额很多时百分比会到几千，那个位置放不下四位数。
func TestTrafficPctOfCapsAndZeroLimit(t *testing.T) {
	if got := trafficPctOf(500, 0); got != 0 {
		t.Errorf("额度为 0 时应当返回 0，实际 %v", got)
	}
	if got := trafficPctOf(500, -1); got != 0 {
		t.Errorf("额度为负时应当返回 0，实际 %v", got)
	}
	if got := trafficPctOf(50, 100); got != 50 {
		t.Errorf("50/100 = %v，期望 50", got)
	}
	if got := trafficPctOf(1<<40, 1); got != 999 {
		t.Errorf("超额时应当封顶在 999，实际 %v", got)
	}
}
