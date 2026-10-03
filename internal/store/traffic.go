package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
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

// DayStart 返回 t 所在自然日的零点（按 loc 切天）。
//
// 它就是"切天"这件事的唯一实现：定时流量报告的区间边界（昨天 / 上周 / 上月）
// 全部由它再往前推，服务器上因此只有一套"一天从几点开始"的口径
// （--timezone）。WeekStart / CycleStart 里那句 time.Date(y, m, d, 0, 0, 0, 0, loc)
// 是同一个意思，这里单独提出来是为了让调用方不必自己再写一遍 —— 时区相关的
// 算术多写一处就多一处"忘了带 location"的机会。
func DayStart(t time.Time, loc *time.Location) time.Time {
	if loc == nil {
		loc = time.UTC
	}
	local := t.In(loc)
	year, month, day := local.Date()
	return time.Date(year, month, day, 0, 0, 0, 0, loc)
}

// WeekStart 返回 t 所在自然周的起点：**周一 00:00**（按 loc 切天，与"今日"同一套时区）。
//
// 为什么是周一而不是周日：中文语境里「本周」指的就是"这周一到今天"，
// 按周日切会让周一早上打开面板的人看到"本周流量 = 昨天（周日）的量"。
//
// 与 CycleStart 一样，这里**不做任何"清零"动作**：周用量 = 周内 traffic_daily 求和，
// 所以跨周那一刻它自然从 0 重新开始，不需要定时任务，改代码也不会丢历史。
//
// 今天正好是周一时，本函数返回的就是今天的零点 —— 「本周」因此等于「今日」。
// 这不是需要特判的边界，而是同一个公式的自然结果。
func WeekStart(t time.Time, loc *time.Location) time.Time {
	if loc == nil {
		loc = time.UTC
	}
	local := t.In(loc)
	year, month, day := local.Date()
	// Go 的 Weekday 里周日是 0，先把编号挪成"周一 = 0"，再往回退那么多天。
	// 直接 day-offset 交给 time.Date 归一化：跨月、跨年都不用自己算。
	offset := (int(local.Weekday()) + 6) % 7
	return time.Date(year, month, day-offset, 0, 0, 0, 0, loc)
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

	// 上界保护（04-2 / B2）：`rx = rx + excluded.rx` 一旦越出 int64，SQLite 会把结果
	// 算成 REAL，驱动随后报 `cannot store REAL value in INTEGER column` —— 一句英文、
	// 不带节点名与日期，运维看不出是"这台机器的流量计数炸了"，只看到落盘一直失败。
	//
	// 所以在写之前先读一次当前累计值，越界的那一步**根本不发给 SQLite**，改为返回
	// 一句能读懂的中文错。读用主键（node_id, day）、只对有增量的行走一次，而落盘
	// 本身是每 --flush-interval（默认 10 秒）一批、每批每个有增量的节点一次。
	//
	// 为什么不在 upsert 的 SQL 里加 CASE 守卫：那会改动**正常路径**的语句文本 —— 而
	// 它今天已经是对的；越界需要的是"说清哪个节点、哪一天、差多少"，那件事在 Go 里
	// 写比在 SQL 里写清楚得多。
	cur, err := tx.PrepareContext(ctx,
		`SELECT rx, tx FROM traffic_daily WHERE node_id = ? AND day = ?`)
	if err != nil {
		return fmt.Errorf("准备读取日流量失败: %w", err)
	}
	defer func() { _ = cur.Close() }()

	for _, u := range updates {
		if u.RxDelta != 0 || u.TxDelta != 0 {
			var rx, tx int64
			switch err := cur.QueryRowContext(ctx, u.NodeID, u.Day).Scan(&rx, &tx); {
			case errors.Is(err, sql.ErrNoRows):
				// 这一天还没有行：写进去的就是增量本身（本来就是 int64），不会越界。
			case err != nil:
				return fmt.Errorf("读取节点 %d 的日流量失败: %w", u.NodeID, err)
			default:
				if trafficSumOverflow(rx, u.RxDelta) || trafficSumOverflow(tx, u.TxDelta) {
					return trafficOverflowError(u, rx, tx)
				}
			}
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

// trafficSumOverflow 报告 cur+delta 是否越过 int64 —— 也就是 SQLite 的 INTEGER 列上界。
//
// 两个方向分开判：写成 `cur > MaxInt64-delta` 就不必先算出溢出结果再看它一眼，
// 而"先算出结果"正是 SQLite 把它变成 REAL 的原因。
func trafficSumOverflow(cur, delta int64) bool {
	switch {
	case delta > 0:
		return cur > math.MaxInt64-delta
	case delta < 0:
		return cur < math.MinInt64-delta
	default:
		return false
	}
}

// trafficOverflowError 是"这一行已经顶到 int64 上界"时给运维看的那句话。
//
// 两侧的当前值与增量都写出来：运维要判断的是"是这台机器真跑了这么多，还是有人
// 手工灌了一个天文数字"，而"已到上限"这一句本身答不了这个问题。范围写成闭区间
// 而不是只写上界：负增量把值推到下界以下同样装不下（只在有人手工调用 API 时才会出现）。
func trafficOverflowError(u TrafficUpdate, rx, tx int64) error {
	return fmt.Errorf("节点 %d 在 %s 的流量计数超上限：累计 rx=%d / tx=%d，本次增量 rx=%d / tx=%d，"+
		"再累加会越过 int64（SQLite 整数列）能表示的范围 -9223372036854775808 … 9223372036854775807（约 9.2 EB）；"+
		"本次整批不写入，需要人工把这一天的这两个数清零后才能继续记账",
		u.NodeID, u.Day, rx, tx, u.RxDelta, u.TxDelta)
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
