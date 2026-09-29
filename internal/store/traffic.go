package store

import (
	"context"
	"fmt"
	"time"
)

// 流量记账日期的格式（服务器本地时区的 YYYY-MM-DD）。
const trafficDayFormat = "2006-01-02"

// FormatDay 把时刻格式化成记账用的日期字符串。
func FormatDay(t time.Time) string { return t.Format(trafficDayFormat) }

// CycleStart 返回 t 所在的计费周期起点。
//
// 周期起点是每个月的 resetDay 号 00:00（服务器本地时区）；resetDay 大于当月天数时
// 钳到当月最后一天（例如 31 号在 2 月按 28/29 号算）。
//
// 这里刻意不做"清零"动作：月用量 = 周期内 traffic_daily 求和，
// 因此改配置立刻生效，历史也永远不会丢。
func CycleStart(t time.Time, resetDay int, loc *time.Location) time.Time {
	if loc == nil {
		loc = time.UTC
	}
	resetDay = clampResetDay(resetDay)
	local := t.In(loc)
	year, month, day := local.Date()

	thisMonth := time.Date(year, month, minInt(resetDay, daysInMonth(year, month, loc)), 0, 0, 0, 0, loc)
	if day >= thisMonth.Day() {
		return thisMonth
	}
	py, pm := previousMonth(year, month)
	return time.Date(py, pm, minInt(resetDay, daysInMonth(py, pm, loc)), 0, 0, 0, 0, loc)
}

// NextCycleStart 返回 from 所在周期的下一个周期起点（也就是本周期结束时刻）。
func NextCycleStart(from time.Time, resetDay int, loc *time.Location) time.Time {
	if loc == nil {
		loc = time.UTC
	}
	start := CycleStart(from, resetDay, loc)
	ny, nm := nextMonth(start.Year(), start.Month())
	return time.Date(ny, nm, minInt(clampResetDay(resetDay), daysInMonth(ny, nm, loc)), 0, 0, 0, 0, loc)
}

func clampResetDay(resetDay int) int {
	if resetDay < 1 {
		return 1
	}
	if resetDay > 31 {
		return 31
	}
	return resetDay
}

func daysInMonth(year int, month time.Month, loc *time.Location) int {
	// 下个月的第 0 天 = 本月最后一天。
	return time.Date(year, month+1, 0, 0, 0, 0, 0, loc).Day()
}

func previousMonth(year int, month time.Month) (int, time.Month) {
	if month == time.January {
		return year - 1, time.December
	}
	return year, month - 1
}

func nextMonth(year int, month time.Month) (int, time.Month) {
	if month == time.December {
		return year + 1, time.January
	}
	return year, month + 1
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TrafficUpdate 是一次流量落盘：增量与"新基线"必须在同一个事务里提交。
type TrafficUpdate struct {
	NodeID  int64
	Day     string
	RxDelta int64
	TxDelta int64
	// RxTotal / TxTotal 是本次采纳的 Agent 累计值（下一次增量就从这个基线算起）。
	RxTotal uint64
	TxTotal uint64
}

// FlushTraffic 把流量增量写进 traffic_daily，同时推进 node_runtime 里的基线。
//
// 两者原子提交：不可能出现"加了增量但基线没动"（会重复计）或
// "基线动了但增量没加"（会丢流量）——这是流量统计不乱的核心保证。
func (d *DB) FlushTraffic(ctx context.Context, updates []TrafficUpdate, now time.Time) error {
	if len(updates) == 0 {
		return nil
	}
	tx, err := d.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO traffic_daily (node_id, day, rx, tx) VALUES (?, ?, ?, ?)
		ON CONFLICT(node_id, day) DO UPDATE SET
			rx = rx + excluded.rx,
			tx = tx + excluded.tx`)
	if err != nil {
		return fmt.Errorf("准备写入日流量失败: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	baseline, err := tx.PrepareContext(ctx,
		`UPDATE node_runtime SET rx_total = ?, tx_total = ?, updated_at = ? WHERE node_id = ?`)
	if err != nil {
		return fmt.Errorf("准备更新流量基线失败: %w", err)
	}
	defer func() { _ = baseline.Close() }()

	for _, u := range updates {
		if u.RxDelta != 0 || u.TxDelta != 0 {
			if _, err := stmt.ExecContext(ctx, u.NodeID, u.Day, u.RxDelta, u.TxDelta); err != nil {
				return fmt.Errorf("写入节点 %d 的日流量失败: %w", u.NodeID, err)
			}
		}
		if _, err := baseline.ExecContext(ctx, u.RxTotal, u.TxTotal, now.Unix(), u.NodeID); err != nil {
			return fmt.Errorf("更新节点 %d 的流量基线失败: %w", u.NodeID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交流量落盘失败: %w", err)
	}
	return nil
}

// DailyTraffic 是一天的流量。
type DailyTraffic struct {
	NodeID int64
	Day    string
	Rx     int64
	Tx     int64
}

// TrafficDailySince 返回 sinceDate（含）之后的全部日流量记录。
//
// 调用方拿到的是"每个节点每天一行"，可以自己按各自的周期起点汇总，
// 因此 N 个节点只需要 1 次查询。
func (d *DB) TrafficDailySince(ctx context.Context, sinceDate string) ([]DailyTraffic, error) {
	rows, err := d.r.QueryContext(ctx,
		`SELECT node_id, day, rx, tx FROM traffic_daily WHERE day >= ? ORDER BY node_id, day`, sinceDate)
	if err != nil {
		return nil, fmt.Errorf("查询日流量失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []DailyTraffic
	for rows.Next() {
		var r DailyTraffic
		if err := rows.Scan(&r.NodeID, &r.Day, &r.Rx, &r.Tx); err != nil {
			return nil, fmt.Errorf("读取日流量失败: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历日流量失败: %w", err)
	}
	return out, nil
}

// TrafficTotals 返回每个节点的累计流量（全表分组，也是 1 次查询）。
func (d *DB) TrafficTotals(ctx context.Context) (map[int64][2]int64, error) {
	rows, err := d.r.QueryContext(ctx,
		`SELECT node_id, SUM(rx), SUM(tx) FROM traffic_daily GROUP BY node_id`)
	if err != nil {
		return nil, fmt.Errorf("查询累计流量失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[int64][2]int64)
	for rows.Next() {
		var (
			id     int64
			rx, tx int64
		)
		if err := rows.Scan(&id, &rx, &tx); err != nil {
			return nil, fmt.Errorf("读取累计流量失败: %w", err)
		}
		out[id] = [2]int64{rx, tx}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历累计流量失败: %w", err)
	}
	return out, nil
}

// TrafficBaselines 读回每个节点的流量基线（服务端启动时载入内存）。
func (d *DB) TrafficBaselines(ctx context.Context) (map[int64][2]uint64, error) {
	rows, err := d.r.QueryContext(ctx, `SELECT node_id, rx_total, tx_total FROM node_runtime`)
	if err != nil {
		return nil, fmt.Errorf("查询流量基线失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[int64][2]uint64)
	for rows.Next() {
		var (
			id     int64
			rx, tx int64
		)
		if err := rows.Scan(&id, &rx, &tx); err != nil {
			return nil, fmt.Errorf("读取流量基线失败: %w", err)
		}
		out[id] = [2]uint64{uint64(maxInt64(rx, 0)), uint64(maxInt64(tx, 0))}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历流量基线失败: %w", err)
	}
	return out, nil
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
