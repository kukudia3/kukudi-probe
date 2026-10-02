package store

import (
	"context"
	"fmt"
	"time"
)

// 两级历史桶的表名。只允许出现在这里的名字，任何拼接都先过 checkTable。
const (
	TableSamples10s = "samples_10s"
	TableSamples1m  = "samples_1m"
)

func checkTable(table string) error {
	if table != TableSamples10s && table != TableSamples1m {
		return fmt.Errorf("非法的历史表名 %q", table)
	}
	return nil
}

// SampleBucket 是一个聚合桶（10s 或 1m）。
//
// 每个桶都保存 avg 与 max：降采样时不会把短时间的高负载抹平。
type SampleBucket struct {
	NodeID int64
	TS     int64

	CPUAvg  float64
	CPUMax  float64
	MemAvg  float64
	MemMax  float64
	SwapAvg float64
	DiskAvg float64
	DiskMax float64
	LoadAvg float64

	RxRate float64
	RxMax  float64
	TxRate float64
	TxMax  float64

	LatAvg float64
	LatMin float64
	LatMax float64

	Up  int64
	All int64
}

const bucketColumns = `node_id, ts, cpu_avg, cpu_max, mem_avg, mem_max, swap_avg, disk_avg, disk_max,
	load1_avg, rx_rate, rx_max, tx_rate, tx_max, lat_avg, lat_min, lat_max, up_cnt, all_cnt`

// InsertBuckets 批量写入桶。幂等：同一 (node_id, ts) 重复写入会覆盖，
// 因此服务端重启后补算、重复 flush 都不会产生重复数据。
func (d *DB) InsertBuckets(ctx context.Context, table string, buckets []SampleBucket) error {
	if err := checkTable(table); err != nil {
		return err
	}
	if len(buckets) == 0 {
		return nil
	}

	tx, err := d.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO `+table+` (`+bucketColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(node_id, ts) DO UPDATE SET
			cpu_avg = excluded.cpu_avg, cpu_max = excluded.cpu_max,
			mem_avg = excluded.mem_avg, mem_max = excluded.mem_max,
			swap_avg = excluded.swap_avg,
			disk_avg = excluded.disk_avg, disk_max = excluded.disk_max,
			load1_avg = excluded.load1_avg,
			rx_rate = excluded.rx_rate, rx_max = excluded.rx_max,
			tx_rate = excluded.tx_rate, tx_max = excluded.tx_max,
			lat_avg = excluded.lat_avg, lat_min = excluded.lat_min, lat_max = excluded.lat_max,
			up_cnt = excluded.up_cnt, all_cnt = excluded.all_cnt`)
	if err != nil {
		return fmt.Errorf("准备写入 %s 失败: %w", table, err)
	}
	defer func() { _ = stmt.Close() }()

	for _, b := range buckets {
		if _, err := stmt.ExecContext(ctx,
			b.NodeID, b.TS, b.CPUAvg, b.CPUMax, b.MemAvg, b.MemMax, b.SwapAvg,
			b.DiskAvg, b.DiskMax, b.LoadAvg, b.RxRate, b.RxMax, b.TxRate, b.TxMax,
			b.LatAvg, b.LatMin, b.LatMax, b.Up, b.All); err != nil {
			return fmt.Errorf("写入 %s 桶失败: %w", table, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交 %s 桶失败: %w", table, err)
	}
	return nil
}

// RollupSamples 把细层聚合到粗层。
//
// 只处理**已封闭**的桶（桶结束时刻 <= until），因此可以安全地重复执行：
// 服务端异常退出后重跑最近一段窗口即可补齐。
//
// 按节点逐条执行：samples 表主键是 (node_id, ts)，带 node_id 的等值条件才能
// 走主键区间；只按 ts 过滤会退化成整表扫描（12 小时的 10 秒桶有几十万行）。
//
// 覆盖写是**只增不减**的：up_cnt 对同一个桶单调不减（源行只会被清理、不会被改写），
// 所以当保留策略先把部分 10 秒源行删掉时，残缺的重算结果不会覆盖掉已经算好的
// 1 分钟行。
func (d *DB) RollupSamples(ctx context.Context, from, to string, width int64, nodeIDs []int64, since, until time.Time) (int64, error) {
	if err := checkTable(from); err != nil {
		return 0, err
	}
	if err := checkTable(to); err != nil {
		return 0, err
	}
	if width <= 0 {
		return 0, fmt.Errorf("聚合宽度必须为正")
	}
	if len(nodeIDs) == 0 {
		return 0, nil
	}

	align := func(t time.Time) int64 { return t.Unix() - (t.Unix() % width) }
	fromTS := align(since)
	// toTS 是"源行 ts"的排他上界：ts < toTS 的源行所在的桶都已经结束。
	toTS := align(until)
	if toTS <= fromTS {
		return 0, nil
	}

	query := `
		INSERT INTO ` + to + ` (` + bucketColumns + `)
		SELECT node_id, (ts / ?) * ? AS bucket,
			AVG(cpu_avg), MAX(cpu_max),
			AVG(mem_avg), MAX(mem_max),
			AVG(swap_avg),
			AVG(disk_avg), MAX(disk_max),
			AVG(load1_avg),
			AVG(rx_rate), MAX(rx_max),
			AVG(tx_rate), MAX(tx_max),
			AVG(lat_avg), MIN(lat_min), MAX(lat_max),
			SUM(up_cnt), SUM(all_cnt)
		FROM ` + from + `
		WHERE node_id = ? AND ts >= ? AND ts < ?
		GROUP BY bucket
		ON CONFLICT(node_id, ts) DO UPDATE SET
			cpu_avg = excluded.cpu_avg, cpu_max = excluded.cpu_max,
			mem_avg = excluded.mem_avg, mem_max = excluded.mem_max,
			swap_avg = excluded.swap_avg,
			disk_avg = excluded.disk_avg, disk_max = excluded.disk_max,
			load1_avg = excluded.load1_avg,
			rx_rate = excluded.rx_rate, rx_max = excluded.rx_max,
			tx_rate = excluded.tx_rate, tx_max = excluded.tx_max,
			lat_avg = excluded.lat_avg, lat_min = excluded.lat_min, lat_max = excluded.lat_max,
			up_cnt = excluded.up_cnt, all_cnt = excluded.all_cnt
		WHERE excluded.up_cnt >= ` + to + `.up_cnt`

	tx, err := d.w.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, query)
	if err != nil {
		return 0, fmt.Errorf("准备聚合语句失败: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	var total int64
	for _, id := range nodeIDs {
		res, err := stmt.ExecContext(ctx, width, width, id, fromTS, toTS)
		if err != nil {
			return total, fmt.Errorf("%s → %s 聚合失败（node %d）: %w", from, to, id, err)
		}
		if n, err := res.RowsAffected(); err == nil {
			total += n
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("提交聚合失败: %w", err)
	}
	return total, nil
}

// DeleteOldSamples 清理过期桶。
//
// 按节点逐条删除：samples 表的主键是 (node_id, ts)，这样每条语句都走索引，
// 不会为了清理去做全表扫描（也就不用给 ts 单独建索引拖慢写入）。
func (d *DB) DeleteOldSamples(ctx context.Context, table string, before time.Time, nodeIDs []int64) (int64, error) {
	if err := checkTable(table); err != nil {
		return 0, err
	}
	if len(nodeIDs) == 0 {
		return 0, nil
	}

	tx, err := d.w.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	var total int64
	for _, id := range nodeIDs {
		res, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE node_id = ? AND ts < ?`, id, before.Unix())
		if err != nil {
			return total, fmt.Errorf("清理 %s（节点 %d）失败: %w", table, id, err)
		}
		if n, err := res.RowsAffected(); err == nil {
			total += n
		}
	}
	if err := tx.Commit(); err != nil {
		return total, fmt.Errorf("提交清理 %s 失败: %w", table, err)
	}
	return total, nil
}

// SeriesPoint 是一个绘图点：时间、平均值、最大值。
//
// 不返回 min：图表上 avg + max 已经能看出趋势与尖峰，
// 多一个字段只会让前端更难读（延迟的 min 仍留在库里备用）。
type SeriesPoint struct {
	TS  int64
	Avg float64
	Max float64
}

// metricColumns 是允许查询的指标与它们的列名（白名单，杜绝拼接注入）。
var metricColumns = map[string][2]string{
	"cpu":      {"cpu_avg", "cpu_max"},
	"mem":      {"mem_avg", "mem_max"},
	"disk":     {"disk_avg", "disk_max"},
	"net_up":   {"tx_rate", "tx_max"},
	"net_down": {"rx_rate", "rx_max"},
	"lat":      {"lat_avg", "lat_max"},
}

// MetricNames 返回所有可查询的指标名（顺序固定，便于前端与测试使用）。
func MetricNames() []string {
	return []string{"cpu", "mem", "disk", "net_up", "net_down", "lat"}
}

// QuerySeries 按档位查询一条曲线。
func (d *DB) QuerySeries(ctx context.Context, nodeID int64, metric string, r Range, now time.Time) ([]SeriesPoint, error) {
	cols, ok := metricColumns[metric]
	if !ok {
		return nil, fmt.Errorf("不支持的指标 %q", metric)
	}
	if err := checkTable(r.Source); err != nil {
		return nil, err
	}

	query := `SELECT (ts / ?) * ? AS bucket, AVG(` + cols[0] + `), MAX(` + cols[1] + `)
		FROM ` + r.Source + `
		WHERE node_id = ? AND ts >= ? AND ts < ?
		GROUP BY bucket ORDER BY bucket`

	startTS, endTS := r.window(now)
	rows, err := d.r.QueryContext(ctx, query, r.Bucket, r.Bucket, nodeID, startTS, endTS)
	if err != nil {
		return nil, fmt.Errorf("查询 %s 曲线失败: %w", metric, err)
	}
	defer func() { _ = rows.Close() }()

	points := make([]SeriesPoint, 0, r.Points())
	for rows.Next() {
		var p SeriesPoint
		if err := rows.Scan(&p.TS, &p.Avg, &p.Max); err != nil {
			return nil, fmt.Errorf("读取曲线点失败: %w", err)
		}
		points = append(points, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历曲线点失败: %w", err)
	}
	return points, nil
}

// QueryUptime 返回某个档位内的可用率（0-100）。
//
// up_cnt 是实际收到的上报次数，all_cnt 是按上报间隔应有的次数（两者都乘了同一个
// 缩放因子，见 server/aggregate.go 的 uptimeScale），因此"掉线率"直接从同一批桶里
// 算出来，不需要额外存 uptime 数据。
//
// 语义说明：分母只累计**真实存在的桶**。也就是说这是"在探针观测到的这段时间里，
// 上报有多完整"，而不是"墙钟时间里节点在线多久"——服务端自己停机、或节点还没被
// 创建的那段时间没有任何桶，不应该记到节点头上。
func (d *DB) QueryUptime(ctx context.Context, nodeID int64, r Range, now time.Time) (float64, bool, error) {
	if err := checkTable(r.Source); err != nil {
		return 0, false, err
	}
	var up, all int64
	startTS, endTS := r.window(now)
	err := d.r.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(up_cnt), 0), COALESCE(SUM(all_cnt), 0) FROM `+r.Source+`
		 WHERE node_id = ? AND ts >= ? AND ts < ?`,
		nodeID, startTS, endTS).Scan(&up, &all)
	if err != nil {
		return 0, false, fmt.Errorf("查询可用率失败: %w", err)
	}
	if all <= 0 {
		return 0, false, nil
	}
	if up > all {
		up = all
	}
	return float64(up) / float64(all) * 100, true, nil
}

// CountSamples 统计某个节点在时间范围内的桶数量（只被测试用）。
func (d *DB) CountSamples(ctx context.Context, table string, nodeID int64, since, until time.Time) (int64, error) {
	if err := checkTable(table); err != nil {
		return 0, err
	}
	var n int64
	err := d.r.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM `+table+` WHERE node_id = ? AND ts >= ? AND ts < ?`,
		nodeID, since.Unix(), until.Unix()).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("统计 %s 失败: %w", table, err)
	}
	return n, nil
}

// RuntimeRow 是 node_runtime 里的一行（服务端退出前的最后状态）。
type RuntimeRow struct {
	NodeID       int64
	LastSeen     int64
	Status       string
	CPUPct       float64
	MemPct       float64
	SwapPct      float64
	DiskPct      float64
	Load1        float64
	LatMS        float64
	UptimeSec    int64
	BootID       string
	Iface        string
	RxRaw        uint64
	TxRaw        uint64
	AgentVersion string
	Kernel       string
	OSName       string
	CPUModel     string
	// OnlineSince 是「最近一次进入在线状态的时刻」（Unix 秒，迁移 0005）。
	//
	// 0 表示不在线（或这台机器还没有过在线记录）。它不是时长而是**起点**：
	// 时长由服务端用"现在 − 起点"现算，重启后只要能读回起点就能接着累加
	// （见 internal/server/online.go）。
	OnlineSince int64
}

// UpsertRuntime 批量写出节点的最后状态。
//
// 注意：rx_total / tx_total 是**流量基线**，由 Phase 7 的流量统计负责写入，
// 这里刻意不碰——在它接手之前这两列保持 0，表示"还没有基线"。
func (d *DB) UpsertRuntime(ctx context.Context, rows []RuntimeRow, now time.Time) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := d.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO node_runtime (node_id, last_seen, status, cpu_pct, mem_pct, swap_pct, disk_pct,
			load1, lat_ms, uptime_sec, boot_id, iface, rx_raw, tx_raw, agent_version, kernel, os_name,
			cpu_model, online_since, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(node_id) DO UPDATE SET
			last_seen = excluded.last_seen, status = excluded.status,
			cpu_pct = excluded.cpu_pct, mem_pct = excluded.mem_pct, swap_pct = excluded.swap_pct,
			disk_pct = excluded.disk_pct, load1 = excluded.load1, lat_ms = excluded.lat_ms,
			uptime_sec = excluded.uptime_sec, boot_id = excluded.boot_id, iface = excluded.iface,
			rx_raw = excluded.rx_raw, tx_raw = excluded.tx_raw,
			agent_version = excluded.agent_version, kernel = excluded.kernel,
			os_name = excluded.os_name, cpu_model = excluded.cpu_model,
			online_since = excluded.online_since,
			updated_at = excluded.updated_at`)
	if err != nil {
		return fmt.Errorf("准备写入运行态失败: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	ts := now.Unix()
	for _, r := range rows {
		if _, err := stmt.ExecContext(ctx,
			r.NodeID, r.LastSeen, r.Status, r.CPUPct, r.MemPct, r.SwapPct, r.DiskPct, r.Load1, r.LatMS,
			r.UptimeSec, r.BootID, r.Iface, r.RxRaw, r.TxRaw, r.AgentVersion, r.Kernel, r.OSName,
			r.CPUModel, r.OnlineSince, ts); err != nil {
			return fmt.Errorf("写入运行态（节点 %d）失败: %w", r.NodeID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交运行态失败: %w", err)
	}
	return nil
}

// LoadRuntime 读回全部节点的最后状态（服务端重启后恢复界面用）。
func (d *DB) LoadRuntime(ctx context.Context) ([]RuntimeRow, error) {
	rows, err := d.r.QueryContext(ctx, `
		SELECT node_id, last_seen, status, cpu_pct, mem_pct, swap_pct, disk_pct, load1, lat_ms,
			uptime_sec, boot_id, iface, rx_raw, tx_raw, agent_version, kernel, os_name, cpu_model,
			online_since
		FROM node_runtime WHERE last_seen > 0`)
	if err != nil {
		return nil, fmt.Errorf("读取运行态失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []RuntimeRow
	for rows.Next() {
		var r RuntimeRow
		if err := rows.Scan(&r.NodeID, &r.LastSeen, &r.Status, &r.CPUPct, &r.MemPct, &r.SwapPct,
			&r.DiskPct, &r.Load1, &r.LatMS, &r.UptimeSec, &r.BootID, &r.Iface, &r.RxRaw, &r.TxRaw,
			&r.AgentVersion, &r.Kernel, &r.OSName, &r.CPUModel, &r.OnlineSince); err != nil {
			return nil, fmt.Errorf("读取运行态失败: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历运行态失败: %w", err)
	}
	return out, nil
}
