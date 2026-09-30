package store

import (
	"context"
	"fmt"
	"math"
	"time"
)

// TablePing1m 是延迟探测的历史表名。
const TablePing1m = "ping_samples_1m"

// lossScale 是 loss_pct 与 up_cnt/all_cnt 之间的换算比例。
//
// 为什么把丢包率同时写成计数：表结构（冻结的 0003 DDL）只有 up_cnt/all_cnt 两列，
// 而 ping 的一行代表"一次探测"。把 loss_pct 放大 100 倍写成
// up = 100-loss、all = 100，任意范围的整体丢包率就能直接用 SUM(up)/SUM(all) 算出来，
// 不必在 SQL 里做浮点平均（也就不会有"按桶平均"与"按探测平均"的歧义）。
const lossScale int64 = 100

// PingBucket 是 ping_samples_1m 的一行：某节点某目标在某一分钟内的最新探测结果。
type PingBucket struct {
	NodeID   int64
	TargetID int64
	TS       int64

	AvgMS   float64
	MinMS   float64
	MaxMS   float64
	LossPct float64

	Up  int64
	All int64
}

// NewPingBucket 按协议结果组装一行。
//
// up_cnt/all_cnt 用 1/100 为单位承载 loss_pct（见 lossScale）：
// 丢包率 33.3% → up=67、all=100。这样"任意范围的整体丢包率"就是 SUM(up)/SUM(all)，
// 而不是"各桶丢包率的算术平均"（后者在探测次数不同的桶之间会被稀释）。
func NewPingBucket(nodeID, targetID, ts int64, avgMS, minMS, maxMS, lossPct float64) PingBucket {
	up := int64(math.Round(float64(lossScale) * (100 - lossPct) / 100))
	if up < 0 {
		up = 0
	}
	if up > lossScale {
		up = lossScale
	}
	return PingBucket{
		NodeID: nodeID, TargetID: targetID, TS: ts,
		AvgMS: avgMS, MinMS: minMS, MaxMS: maxMS, LossPct: lossPct,
		Up: up, All: lossScale,
	}
}

// UpsertPingBuckets 批量写入探测桶。幂等：同一 (node_id, target_id, ts) 重复写入会覆盖，
// 因此服务端重启后补写、重复 flush 都不会产生重复数据。
func (d *DB) UpsertPingBuckets(ctx context.Context, buckets []PingBucket) error {
	if len(buckets) == 0 {
		return nil
	}

	tx, err := d.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO `+TablePing1m+` (node_id, target_id, ts, avg_ms, min_ms, max_ms, loss_pct, up_cnt, all_cnt)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(node_id, target_id, ts) DO UPDATE SET
			avg_ms = excluded.avg_ms, min_ms = excluded.min_ms, max_ms = excluded.max_ms,
			loss_pct = excluded.loss_pct, up_cnt = excluded.up_cnt, all_cnt = excluded.all_cnt`)
	if err != nil {
		return fmt.Errorf("准备写入 %s 失败: %w", TablePing1m, err)
	}
	defer func() { _ = stmt.Close() }()

	for _, b := range buckets {
		if _, err := stmt.ExecContext(ctx,
			b.NodeID, b.TargetID, b.TS, b.AvgMS, b.MinMS, b.MaxMS, b.LossPct, b.Up, b.All); err != nil {
			return fmt.Errorf("写入 %s 桶失败: %w", TablePing1m, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交 %s 桶失败: %w", TablePing1m, err)
	}
	return nil
}

// DeleteOldPingSamples 清理过期探测桶。
//
// 与 samples 一样按节点逐条删除：主键是 (node_id, target_id, ts)，
// 带 node_id 的等值条件才能走主键区间，不会为了清理做全表扫描。
func (d *DB) DeleteOldPingSamples(ctx context.Context, before time.Time, nodeIDs []int64) (int64, error) {
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
		res, err := tx.ExecContext(ctx,
			`DELETE FROM `+TablePing1m+` WHERE node_id = ? AND ts < ?`, id, before.Unix())
		if err != nil {
			return total, fmt.Errorf("清理 %s（节点 %d）失败: %w", TablePing1m, id, err)
		}
		if n, err := res.RowsAffected(); err == nil {
			total += n
		}
	}
	if err := tx.Commit(); err != nil {
		return total, fmt.Errorf("提交清理 %s 失败: %w", TablePing1m, err)
	}
	return total, nil
}

// PingRange 是延迟曲线的一个时间档位。
//
// 刻意不复用 Range：那套桶宽是给 10s/1m 源表 + "点数 ≤1000" 设计的，
// 1h 档只有 10 秒桶 —— 对 1 分钟粒度的 ping 表意味着 6/7 的桶是空的，
// 图上会出现大片空洞。这里按"60~170 个点"重新定一套桶宽（见 pingRangeSpecs）。
type PingRange struct {
	Key    string
	Window time.Duration
	// Bucket 是聚合桶宽（秒）。
	Bucket int64
}

// Points 返回该档位的桶数量。
func (r PingRange) Points() int {
	if r.Bucket <= 0 {
		return 0
	}
	return int(r.Window.Seconds()) / int(r.Bucket)
}

// window 返回查询窗口 [start, end)，两端对齐到桶网格。
//
// 与 Range.window 同样的理由：只有对齐了，每个点才代表一个完整的桶；
// 代价是最新点最多滞后一个桶（1h 档 60 秒），对历史曲线完全可接受。
func (r PingRange) window(now time.Time) (int64, int64) {
	if r.Bucket <= 0 {
		return now.Unix(), now.Unix()
	}
	end := now.Unix() - (now.Unix() % r.Bucket)
	return end - int64(r.Window.Seconds()), end
}

// pingRangeSpecs 是六档的桶宽（秒）。用户定稿的对应关系，测试锁死。
var pingRangeSpecs = []struct {
	key    string
	window time.Duration
	bucket int64
}{
	{"1h", time.Hour, 60},
	{"6h", 6 * time.Hour, 300},
	{"12h", 12 * time.Hour, 600},
	{"1d", 24 * time.Hour, 900},
	{"3d", 72 * time.Hour, 1800},
	{"7d", 7 * 24 * time.Hour, 3600},
}

// PingRanges 返回六档（顺序固定：从短到长）。
func PingRanges() []PingRange {
	out := make([]PingRange, 0, len(pingRangeSpecs))
	for _, spec := range pingRangeSpecs {
		out = append(out, PingRange{Key: spec.key, Window: spec.window, Bucket: spec.bucket})
	}
	return out
}

// PingRangeByKey 按 key 取档位。
func PingRangeByKey(key string) (PingRange, bool) {
	for _, r := range PingRanges() {
		if r.Key == key {
			return r, true
		}
	}
	return PingRange{}, false
}

// PingSeries 是一个目标在某个档位下的曲线。
type PingSeries struct {
	Points []SeriesPoint
	// LossPct 是该档位内按探测次数加权的整体丢包率（无数据时为 0）。
	LossPct float64
	HasData bool
}

// QueryPingSeries 查询一个目标的曲线。
//
// 一次查询同时拿到三类信息：每个桶的 avg/max（画图）、以及 SUM(up)/SUM(all)
// （整体丢包率）—— 后者不需要再扫一遍表。
func (d *DB) QueryPingSeries(ctx context.Context, nodeID, targetID int64, r PingRange, now time.Time) (PingSeries, error) {
	if r.Bucket <= 0 {
		return PingSeries{}, fmt.Errorf("非法的延迟曲线档位 %q", r.Key)
	}
	startTS, endTS := r.window(now)

	rows, err := d.r.QueryContext(ctx, `
		SELECT (ts / ?) * ? AS bucket, AVG(avg_ms), MAX(max_ms), SUM(up_cnt), SUM(all_cnt)
		FROM `+TablePing1m+`
		WHERE node_id = ? AND target_id = ? AND ts >= ? AND ts < ?
		GROUP BY bucket ORDER BY bucket`,
		r.Bucket, r.Bucket, nodeID, targetID, startTS, endTS)
	if err != nil {
		return PingSeries{}, fmt.Errorf("查询延迟曲线失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out PingSeries
	var up, all int64
	for rows.Next() {
		var (
			p  SeriesPoint
			u  int64
			a  int64
			ts int64
		)
		if err := rows.Scan(&ts, &p.Avg, &p.Max, &u, &a); err != nil {
			return PingSeries{}, fmt.Errorf("读取延迟曲线点失败: %w", err)
		}
		p.TS = ts
		out.Points = append(out.Points, p)
		up += u
		all += a
	}
	if err := rows.Err(); err != nil {
		return PingSeries{}, fmt.Errorf("遍历延迟曲线点失败: %w", err)
	}
	out.HasData = len(out.Points) > 0
	if all > 0 {
		if up > all {
			up = all
		}
		// 丢包率 = 丢掉的次数 / 总探测次数（up_cnt 是成功次数，见 lossScale）。
		out.LossPct = float64(all-up) / float64(all) * 100
	}
	return out, nil
}
