package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

func TestCycleStart(t *testing.T) {
	utc := time.UTC
	shanghai, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Skipf("缺少时区数据: %v", err)
	}

	cases := []struct {
		name     string
		now      time.Time
		resetDay int
		loc      *time.Location
		want     string // 期望的周期起点（本地日期）
	}{
		{"重置日当天", time.Date(2026, 9, 19, 10, 0, 0, 0, utc), 19, utc, "2026-09-19"},
		{"重置日之后", time.Date(2026, 9, 25, 10, 0, 0, 0, utc), 19, utc, "2026-09-19"},
		{"重置日之前", time.Date(2026, 9, 10, 10, 0, 0, 0, utc), 19, utc, "2026-08-19"},
		{"跨年：1 月初", time.Date(2026, 1, 3, 10, 0, 0, 0, utc), 19, utc, "2025-12-19"},
		{"重置日为 1 号", time.Date(2026, 9, 30, 10, 0, 0, 0, utc), 1, utc, "2026-09-01"},
		{"31 号在 2 月钳到最后一天", time.Date(2026, 3, 5, 10, 0, 0, 0, utc), 31, utc, "2026-02-28"},
		{"31 号在闰年 2 月", time.Date(2024, 3, 5, 10, 0, 0, 0, utc), 31, utc, "2024-02-29"},
		{"时区：UTC 18 号晚上已经是上海 19 号", time.Date(2026, 9, 18, 20, 0, 0, 0, utc), 19, shanghai, "2026-09-19"},
		{"时区：北京时间还是 18 号", time.Date(2026, 9, 18, 10, 0, 0, 0, utc), 19, shanghai, "2026-08-19"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := CycleStart(tc.now, tc.resetDay, tc.loc)
			if got.Format("2006-01-02") != tc.want {
				t.Fatalf("周期起点 = %s，期望 %s", got.Format("2006-01-02"), tc.want)
			}
			if got.Hour() != 0 || got.Minute() != 0 {
				t.Fatalf("周期起点必须是本地零点，实际 %s", got)
			}
			if got.Location() != tc.loc {
				t.Fatalf("周期起点时区 = %s，期望 %s", got.Location(), tc.loc)
			}
		})
	}
}

// WeekStart：「本周」从**周一 00:00** 起（按传入的时区切天，与"今日"同一套规则）。
//
// 为什么单独钉它：周界的算法看起来"怎么写都对"，但按周日切与按周一切在
// 周一早上会差整整一天（面板上写着"本周流量"却是昨天的量），而页面上完全
// 看不出哪里错了。跨月、跨年、以及"时区不同导致本地还停在上一天"都要一起钉住。
func TestWeekStart(t *testing.T) {
	utc := time.UTC
	shanghai, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Skipf("缺少时区数据: %v", err)
	}

	cases := []struct {
		name string
		now  time.Time
		loc  *time.Location
		want string // 期望的周一（本地日期）
	}{
		{"周三", time.Date(2026, 9, 23, 12, 0, 0, 0, utc), utc, "2026-09-21"},
		{"周一当天（零点起就是本周）", time.Date(2026, 9, 21, 0, 30, 0, 0, utc), utc, "2026-09-21"},
		{"周日属于**上一个**周一", time.Date(2026, 9, 20, 23, 0, 0, 0, utc), utc, "2026-09-14"},
		{"跨月：周三回退到上个月", time.Date(2026, 10, 1, 9, 0, 0, 0, utc), utc, "2026-09-28"},
		{"跨年：元旦回退到上一年", time.Date(2026, 1, 1, 9, 0, 0, 0, utc), utc, "2025-12-29"},
		{"时区：UTC 周日晚上已是上海周一", time.Date(2026, 9, 20, 20, 0, 0, 0, utc), shanghai, "2026-09-21"},
		{"时区：上海还是周日", time.Date(2026, 9, 20, 10, 0, 0, 0, utc), shanghai, "2026-09-14"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := WeekStart(tc.now, tc.loc)
			if got.Format("2006-01-02") != tc.want {
				t.Fatalf("周一 = %s，期望 %s", got.Format("2006-01-02"), tc.want)
			}
			if got.Weekday() != time.Monday {
				t.Fatalf("结果必须是周一，实际 %s", got.Weekday())
			}
			if got.Hour() != 0 || got.Minute() != 0 || got.Second() != 0 {
				t.Fatalf("周起点必须是本地零点，实际 %s", got)
			}
			if got.Location() != tc.loc {
				t.Fatalf("周起点时区 = %s，期望 %s", got.Location(), tc.loc)
			}
			if got.After(tc.now.In(tc.loc)) {
				t.Fatalf("周起点不能晚于当前时刻: %s > %s", got, tc.now.In(tc.loc))
			}
		})
	}

	// loc 为 nil 时按 UTC 处理（与 CycleStart 同一套兜底），不能 panic。
	if got := WeekStart(time.Date(2026, 9, 23, 12, 0, 0, 0, utc), nil); got.Format("2006-01-02") != "2026-09-21" {
		t.Fatalf("loc 为 nil 时应当按 UTC 算，实际 %s", got.Format("2006-01-02"))
	}
}

func TestNextCycleStart(t *testing.T) {
	utc := time.UTC
	cases := []struct {
		now      time.Time
		resetDay int
		want     string
	}{
		{time.Date(2026, 9, 25, 0, 0, 0, 0, utc), 19, "2026-10-19"},
		{time.Date(2026, 12, 25, 0, 0, 0, 0, utc), 19, "2027-01-19"},
		{time.Date(2026, 1, 31, 0, 0, 0, 0, utc), 31, "2026-02-28"},
	}
	for _, tc := range cases {
		got := NextCycleStart(tc.now, tc.resetDay, utc)
		if got.Format("2006-01-02") != tc.want {
			t.Errorf("NextCycleStart(%s, %d) = %s，期望 %s",
				tc.now.Format("2006-01-02"), tc.resetDay, got.Format("2006-01-02"), tc.want)
		}
		if !got.After(CycleStart(tc.now, tc.resetDay, utc)) {
			t.Errorf("周期结束必须晚于周期起点")
		}
	}
}

func TestFlushTrafficAccumulatesAndMovesBaseline(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()
	now := time.Now()
	day := FormatDay(now)

	if _, _, err := db.CreateNode(ctx, validNewNode(), now); err != nil {
		t.Fatalf("创建节点: %v", err)
	}

	// 第一次：加 1 MiB / 512 KiB，基线推进到 2000/1000。
	if err := db.FlushTraffic(ctx, []TrafficUpdate{{
		NodeID: 1, Day: day, RxDelta: 1024, TxDelta: 512, RxTotal: 2000, TxTotal: 1000,
	}}, now); err != nil {
		t.Fatalf("第一次落盘: %v", err)
	}
	// 第二次：同一天继续累加。
	if err := db.FlushTraffic(ctx, []TrafficUpdate{{
		NodeID: 1, Day: day, RxDelta: 2048, TxDelta: 1024, RxTotal: 4000, TxTotal: 2000,
	}}, now.Add(time.Minute)); err != nil {
		t.Fatalf("第二次落盘: %v", err)
	}

	var rx, tx int64
	if err := db.Reader().QueryRowContext(ctx,
		`SELECT rx, tx FROM traffic_daily WHERE node_id = 1 AND day = ?`, day).Scan(&rx, &tx); err != nil {
		t.Fatalf("读取日流量: %v", err)
	}
	if rx != 3072 || tx != 1536 {
		t.Fatalf("日流量 = %d/%d，期望 3072/1536（同一天累加）", rx, tx)
	}

	baselines, err := db.TrafficBaselines(ctx)
	if err != nil {
		t.Fatalf("读取基线: %v", err)
	}
	if baselines[1] != [2]uint64{4000, 2000} {
		t.Fatalf("基线 = %v，期望 [4000 2000]", baselines[1])
	}

	// 只有基线变化（增量为 0，例如 Agent 重装后重设基线）时不应产生新的日流量行。
	if err := db.FlushTraffic(ctx, []TrafficUpdate{{
		NodeID: 1, Day: day, RxTotal: 10, TxTotal: 20,
	}}, now.Add(2*time.Minute)); err != nil {
		t.Fatalf("重设基线: %v", err)
	}
	if err := db.Reader().QueryRowContext(ctx,
		`SELECT rx, tx FROM traffic_daily WHERE node_id = 1 AND day = ?`, day).Scan(&rx, &tx); err != nil {
		t.Fatalf("读取日流量: %v", err)
	}
	if rx != 3072 || tx != 1536 {
		t.Fatalf("重设基线不该改变日流量：%d/%d", rx, tx)
	}
	baselines, _ = db.TrafficBaselines(ctx)
	if baselines[1] != [2]uint64{10, 20} {
		t.Fatalf("基线未重设: %v", baselines[1])
	}
}

func TestTrafficDailySinceAndTotals(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()
	now := time.Now()
	if _, _, err := db.CreateNode(ctx, validNewNode(), now); err != nil {
		t.Fatalf("创建节点: %v", err)
	}
	second := validNewNode()
	second.Name = "hk-02"
	if _, _, err := db.CreateNode(ctx, second, now); err != nil {
		t.Fatalf("创建节点: %v", err)
	}

	updates := []TrafficUpdate{
		{NodeID: 1, Day: "2026-09-01", RxDelta: 100, TxDelta: 10, RxTotal: 100, TxTotal: 10},
		{NodeID: 1, Day: "2026-09-19", RxDelta: 200, TxDelta: 20, RxTotal: 300, TxTotal: 30},
		{NodeID: 2, Day: "2026-09-19", RxDelta: 300, TxDelta: 30, RxTotal: 300, TxTotal: 30},
	}
	if err := db.FlushTraffic(ctx, updates, now); err != nil {
		t.Fatalf("落盘: %v", err)
	}

	daily, err := db.TrafficDailySince(ctx, "2026-09-19")
	if err != nil {
		t.Fatalf("查询日流量: %v", err)
	}
	if len(daily) != 2 {
		t.Fatalf("9-19 之后应当有 2 行，实际 %d", len(daily))
	}
	for _, d := range daily {
		if d.Day != "2026-09-19" {
			t.Fatalf("日期过滤没生效: %+v", d)
		}
	}

	totals, err := db.TrafficTotals(ctx)
	if err != nil {
		t.Fatalf("查询累计: %v", err)
	}
	if totals[1] != [2]int64{300, 30} || totals[2] != [2]int64{300, 30} {
		t.Fatalf("累计流量不对: %v", totals)
	}
}

func TestFlushTrafficRejectsNothingForEmptyInput(t *testing.T) {
	db := openTemp(t)
	if err := db.FlushTraffic(context.Background(), nil, time.Now()); err != nil {
		t.Fatalf("空输入不该报错: %v", err)
	}
}

// TestFlushTrafficStopsAtInt64Ceiling 覆盖 B2（round5）：流量累加越过 int64 上界时，
// 返回一句**能读懂的**错，而不是驱动层那句
// `constraint failed: cannot store REAL value in INTEGER column traffic_daily.rx (3091)`。
//
// 为什么要有这条用例：round4 的 04-2 已经实测了越界那一刻的**现状**（报错、整事务回滚、
// 数据不脏），但现状的问题是**那句话**——它不带节点名、不带日期，运维看不出是这台机器的
// 流量计数炸了。本用例钉住四件事：
//
//	① 边界是闭区间：恰好累加到 MaxInt64 必须成功（守卫不能把正常值也挡掉）；
//	② 越界那一步返回中文错（带节点、日期、上限），且**不是**驱动层那句英文；
//	③ 被拒的那次一个字节都不许落库：日流量行与 node_runtime 基线都保持原值（回滚）；
//	④ 正常路径逐字节不变：被拒之后同一行继续正常累加、新行仍可写入、界内批次照常成功。
//
// 反向验证（已实测，见 ROUND5-BACKEND.md）：把 traffic.go 的 trafficSumOverflow 调用
// 撤掉 → 红在 ② 段（报错变回那句英文，`strings.Contains(msg, "REAL")` 命中且缺"上限"）。
func TestFlushTrafficStopsAtInt64Ceiling(t *testing.T) {
	ctx := context.Background()
	db := openTemp(t)
	now := time.Now()
	const day = "2026-10-03"

	mkNode := func(name string) int64 {
		t.Helper()
		node, _, err := db.CreateNode(ctx, testNewNode(name), now)
		if err != nil {
			t.Fatalf("创建节点 %s: %v", name, err)
		}
		return node.ID
	}
	rxOf := func(nodeID int64) (int64, bool) {
		t.Helper()
		var rx int64
		err := db.Reader().QueryRowContext(ctx,
			`SELECT rx FROM traffic_daily WHERE node_id = ? AND day = ?`, nodeID, day).Scan(&rx)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, false
		}
		if err != nil {
			t.Fatalf("读取节点 %d 的日流量: %v", nodeID, err)
		}
		return rx, true
	}
	baseRxOf := func(nodeID int64) uint64 {
		t.Helper()
		baselines, err := db.TrafficBaselines(ctx)
		if err != nil {
			t.Fatalf("读取流量基线: %v", err)
		}
		return baselines[nodeID][0]
	}
	// assertReadableOverflow 断言"这是一句人话"，而不是驱动层那句英文。
	assertReadableOverflow := func(err error, nodeID int64) {
		t.Helper()
		if err == nil {
			t.Fatalf("越界时必须报错")
		}
		msg := err.Error()
		t.Logf("越界报错（实测）：%v", err)
		for _, want := range []string{fmt.Sprintf("节点 %d", nodeID), day, "上限"} {
			if !strings.Contains(msg, want) {
				t.Errorf("报错里缺少 %q，运维据此认不出是哪台机器/哪一天：%v", want, err)
			}
		}
		if strings.Contains(msg, "REAL") || strings.Contains(msg, "cannot store") {
			t.Errorf("报错还是驱动层那句英文（上界保护没生效）：%v", err)
		}
	}

	// ① 边界是闭区间：MaxInt64-10 与 +10 各写一次，必须都成功。
	ceil := mkNode("traffic-ceiling")
	if err := db.FlushTraffic(ctx, []TrafficUpdate{
		{NodeID: ceil, Day: day, RxDelta: math.MaxInt64 - 10, RxTotal: 100},
	}, now); err != nil {
		t.Fatalf("写入 MaxInt64-10: %v", err)
	}
	if err := db.FlushTraffic(ctx, []TrafficUpdate{
		{NodeID: ceil, Day: day, RxDelta: 10, RxTotal: 200},
	}, now); err != nil {
		t.Fatalf("恰好累加到 int64 上界应当成功（边界是闭区间）: %v", err)
	}
	if got, ok := rxOf(ceil); !ok || got != math.MaxInt64 {
		t.Fatalf("恰好到上界后读回 %d（ok=%v），期望 %d", got, ok, int64(math.MaxInt64))
	}

	// ② 再加 1 字节：必须是一句能读懂的错。
	baseBefore := baseRxOf(ceil)
	assertReadableOverflow(db.FlushTraffic(ctx, []TrafficUpdate{
		{NodeID: ceil, Day: day, RxDelta: 1, RxTotal: 999},
	}, now), ceil)

	// ③ 被拒的那次不许留下任何痕迹：日流量行与基线都保持原值。
	if got, ok := rxOf(ceil); !ok || got != math.MaxInt64 {
		t.Errorf("被拒的那次改动了日流量：读回 %d（ok=%v），期望仍然是 %d", got, ok, int64(math.MaxInt64))
	}
	if got := baseRxOf(ceil); got != baseBefore {
		t.Errorf("被拒的那次推进了流量基线：%d → %d（整个事务必须回滚）", baseBefore, got)
	}

	// ④ 反方向（下溢）也要拦住：累加值不能掉到 int64 下界以下。
	//    先把累计值压到 0 以下（增量是 int64，API 允许负值），再减 MinInt64 才会下溢：
	//    非负的累计值 + MinInt64 本身是装得下的（MinInt64 + 5 还在范围内）。
	floor := mkNode("traffic-floor")
	if err := db.FlushTraffic(ctx, []TrafficUpdate{
		{NodeID: floor, Day: day, RxDelta: -5, RxTotal: 5},
	}, now); err != nil {
		t.Fatalf("写入 -5: %v", err)
	}
	assertReadableOverflow(db.FlushTraffic(ctx, []TrafficUpdate{
		{NodeID: floor, Day: day, RxDelta: math.MinInt64, RxTotal: 6},
	}, now), floor)
	if got, ok := rxOf(floor); !ok || got != -5 {
		t.Errorf("被拒的下溢改动了日流量：读回 %d（ok=%v），期望仍然是 -5", got, ok)
	}

	// ④' 正常路径逐字节不变：被拒之后同一行继续正常累加（-5 + 12 = 7）。
	if err := db.FlushTraffic(ctx, []TrafficUpdate{
		{NodeID: floor, Day: day, RxDelta: 12, TxDelta: 2, RxTotal: 12, TxTotal: 2},
	}, now); err != nil {
		t.Fatalf("被拒之后同一行应当还能正常累加: %v", err)
	}
	if got, ok := rxOf(floor); !ok || got != 7 {
		t.Errorf("正常累加后读回 %d（ok=%v），期望 7", got, ok)
	}

	// ④'' insert 路径不受守卫影响：新的一行直接写入 MaxInt64 也不越界（值本身就装得下）。
	fresh := mkNode("traffic-fresh")
	if err := db.FlushTraffic(ctx, []TrafficUpdate{
		{NodeID: fresh, Day: day, RxDelta: math.MaxInt64, RxTotal: 1},
	}, now); err != nil {
		t.Fatalf("新行直接写入 MaxInt64 应当成功（这是插入，不是累加）: %v", err)
	}
	if got, ok := rxOf(fresh); !ok || got != math.MaxInt64 {
		t.Errorf("新行读回 %d（ok=%v），期望 %d", got, ok, int64(math.MaxInt64))
	}

	// ⑤ 一批里混进越界的节点：整批回滚（**现状语义**，round5 刻意没改）。一个节点把
	// 计数撑到上界，会让同批其他节点的流量这一分钟也落不了盘 —— 这是 B2 报告里
	// 记下的放大效应，写成断言是为了让它将来被改掉时是一次**有意的**决定。
	ok2 := mkNode("traffic-same-batch")
	err := db.FlushTraffic(ctx, []TrafficUpdate{
		{NodeID: ok2, Day: day, RxDelta: 1024, RxTotal: 1024},
		{NodeID: ceil, Day: day, RxDelta: 1, RxTotal: 1000},
	}, now)
	assertReadableOverflow(err, ceil)
	if got, ok := rxOf(ok2); ok {
		t.Errorf("同批正常节点的流量没有被回滚：读回 %d（现状是整批一个事务）", got)
	}
	// 同批正常节点自己单独再写一次，仍然正常。
	if err := db.FlushTraffic(ctx, []TrafficUpdate{
		{NodeID: ok2, Day: day, RxDelta: 1024, RxTotal: 1024},
	}, now); err != nil {
		t.Fatalf("单独写入同批那个正常节点: %v", err)
	}
	if got, ok := rxOf(ok2); !ok || got != 1024 {
		t.Errorf("正常节点单独写入后读回 %d（ok=%v），期望 1024", got, ok)
	}
}
