package store

import (
	"context"
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
