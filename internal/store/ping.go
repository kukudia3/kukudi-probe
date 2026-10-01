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
	// TickBaseSec 是 X 轴的**基准间隔**（秒），与资源图的 Range.TickBaseSec 是
	// 同一张用户定稿的表（按**档位**定，与桶宽无关）：延迟图的桶宽比资源图粗得多
	// （1h 档 60 秒 vs 10 秒），但"这一档的刻度语义"是同一件事 —— 1h 档无论画 CPU
	// 还是画延迟，都是每 1 分钟一根。
	//
	// 两张表各带一份（它们的 key/window 本来就各有一份），由 store 的测试
	// （TestPingRangeTickBaseMatchesResourceRanges）钉住两者不许漂移。
	// 屏幕上真实的标签间隔由前端按宽度自动稀疏，服务端不抽稀 —— 见 chart.js 的
	// xLabelStep。
	TickBaseSec int64
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

// pingRangeSpecs 是六档的桶宽（秒）与 X 轴基准间隔（秒）。用户定稿的对应关系，测试锁死。
//
// 基准间隔与资源图同表（1m/1m/1m/2m/5m/15m），比延迟图自己的桶宽细得多：
// 6h 档一个点代表 5 分钟，而基准刻度是 1 分钟 —— 屏幕上放不下的部分由前端
// 按标签实际宽度自动稀疏，服务端不做这件事。
var pingRangeSpecs = []struct {
	key      string
	window   time.Duration
	bucket   int64
	tickBase int64
}{
	{"1h", time.Hour, 60, 60},
	{"6h", 6 * time.Hour, 300, 60},
	{"12h", 12 * time.Hour, 600, 60},
	{"1d", 24 * time.Hour, 900, 120},
	{"3d", 72 * time.Hour, 1800, 300},
	{"7d", 7 * 24 * time.Hour, 3600, 900},
}

// PingRanges 返回六档（顺序固定：从短到长）。
func PingRanges() []PingRange {
	out := make([]PingRange, 0, len(pingRangeSpecs))
	for _, spec := range pingRangeSpecs {
		out = append(out, PingRange{
			Key: spec.key, Window: spec.window,
			Bucket: spec.bucket, TickBaseSec: spec.tickBase,
		})
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

// PingPoint 是延迟曲线上的一个桶：时间、平均/最大延迟，以及这个桶的丢包率。
//
// 为什么不复用 SeriesPoint：丢包率只对探测目标有意义（CPU/内存那些曲线没有这个
// 概念），塞进通用点类型会让 /series 的返回里多出一个恒为 0 的字段。
type PingPoint struct {
	TS   int64
	Avg  float64
	Max  float64
	Loss float64 // 该桶的丢包率（0-100）
}

// lossPctOf 把一对"成功次数 / 总探测次数"换算成丢包率（百分比）。
//
// 桶丢包率与整段丢包率必须走同一个函数：两处口径一旦分叉，图上那些竖条加起来
// 就对不上图例里的总丢包率，而"对不上"正是最容易被当成 bug 的现象。
func lossPctOf(up, all int64) float64 {
	if all <= 0 {
		return 0
	}
	// 计数列理论上不会越界（写入时已经夹过一次），但读的是历史数据，
	// 越界会让丢包率变成负数（图上就是"负的竖条"），所以这里再夹一次。
	if up > all {
		up = all
	}
	if up < 0 {
		up = 0
	}
	return float64(all-up) / float64(all) * 100
}

// PingSeries 是一个目标在某个档位下的曲线。
type PingSeries struct {
	Points []PingPoint
	// LossPct 是该档位内按探测次数加权的整体丢包率（无数据时为 0）。
	LossPct float64
	// AvgMS 是该档位内按**成功**探测次数加权的整体平均延迟（无数据时为 0）。
	//
	// 为什么不把各桶的 avg 直接平均：桶里的 avg_ms 已经是"那一分钟的平均"，
	// 但不同分钟的成功探测次数可以差很多（Agent 重启、间隔调整、部分丢包），
	// 直接平均会让"探通 3 次的一分钟"与"探通 60 次的一分钟"权重相同。
	//
	// 权重用 up_cnt（成功次数）而不是 all_cnt：全丢的那一分钟根本没有延迟样本
	// （avg_ms 是 0），把它算进分母会把整段平均延迟拉低 —— 表现是"线路越丢包、图例上的延迟越低"。
	AvgMS float64
	// PeakMS 是该档位内的**峰值延迟**：曲线用的那批桶里 MaxMS 的最大值（无数据时为 0）。
	//
	// 为什么是"桶最大值的最大"而不是"整段的总体最大"：表里落库的本来就只有每分钟的
	// 最大值（max_ms），更细的原始探测不保留，所以这就是手里能拿到的最坏值 ——
	// 它同时也是前端峰值淡线画的那个数（points[i][2]），两处必须是同一批点。
	//
	// 与 AvgMS 的加权口径**刻意不同**：平均值按成功次数加权（见上），峰值取最大值。
	// 一个"丢了一半包但回来的都特别高"的桶，它的 900ms 照样该被算进峰值 ——
	// 峰值回答的是"最坏有多坏"，不是"典型有多坏"。
	// 全丢的桶 max_ms 也是 0（没有样本），所以整段全丢 / 完全没有行时这里都是 0；
	// 0 是前端认的"没有峰值"哨兵（延迟有物理下限，不可能是 0ms 的真实峰值）。
	PeakMS  float64
	HasData bool
}

// QueryPingSeries 查询一个目标的曲线。
//
// 一次查询同时拿到三类信息：每个桶的 avg/max 与**该桶的丢包率**（画图 + 画丢包竖条）、
// 以及 SUM(up)/SUM(all)（整体丢包率）—— 后者不需要再扫一遍表。
//
// 丢包率按探测次数加权（SUM(up_cnt)/SUM(all_cnt)），不是各分钟 loss_pct 的算术平均：
// 一个小时里"探测 60 次丢 1 次"和"探测 6 次丢 1 次"不是一回事，算术平均会把它们
// 等同对待（见 lossScale）。
func (d *DB) QueryPingSeries(ctx context.Context, nodeID, targetID int64, r PingRange, now time.Time) (PingSeries, error) {
	if r.Bucket <= 0 {
		return PingSeries{}, fmt.Errorf("非法的延迟曲线档位 %q", r.Key)
	}
	startTS, endTS := r.window(now)

	rows, err := d.r.QueryContext(ctx, `
		SELECT (ts / ?) * ? AS bucket,
			COALESCE(SUM(avg_ms * up_cnt) / NULLIF(SUM(up_cnt), 0), 0),
			MAX(max_ms), SUM(up_cnt), SUM(all_cnt)
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
	var latWeighted float64
	// peak 顺手在同一个循环里取最大值：曲线用的就是这批桶，再单独发一条
	// MAX(max_ms) 查询等于把同一段区间扫第二遍（见 PingSeries.PeakMS）。
	var peak float64
	for rows.Next() {
		var (
			p  PingPoint
			u  int64
			a  int64
			ts int64
		)
		if err := rows.Scan(&ts, &p.Avg, &p.Max, &u, &a); err != nil {
			return PingSeries{}, fmt.Errorf("读取延迟曲线点失败: %w", err)
		}
		p.TS = ts
		p.Loss = lossPctOf(u, a)
		out.Points = append(out.Points, p)
		up += u
		all += a
		if p.Max > peak {
			peak = p.Max
		}
		// 整体平均延迟的加权和：权重是这一分钟的成功探测次数（见 PingSeries.AvgMS）。
		latWeighted += p.Avg * float64(u)
	}
	if err := rows.Err(); err != nil {
		return PingSeries{}, fmt.Errorf("遍历延迟曲线点失败: %w", err)
	}
	out.HasData = len(out.Points) > 0
	out.PeakMS = peak
	// 丢包率 = 丢掉的次数 / 总探测次数（up_cnt 是成功次数，见 lossScale）。
	out.LossPct = lossPctOf(up, all)
	if up > 0 {
		out.AvgMS = latWeighted / float64(up)
	}
	return out, nil
}

// OverviewPing 是一个节点在"总览"窗口内的探测概览。
//
// 这个类型直接带 JSON 标签：与 PingTarget 同样的理由 —— 它就是接口返回的形状本身
// （总览接口 nodes 里的一项），中间再套一层 server 侧的 DTO 只会多一处要同步改的字段。
//
// 桶是**跨目标**的：一段（默认 6 分钟）里的数值 = 该节点所有探测目标在那一段的合计。
// 首页卡片上的迷你条讲的是"这台机器的线路整体怎么样"，不区分是到哪个目标。
type OverviewPing struct {
	// LatMS / LossPct 是**整段**（默认一小时）的聚合值，也就是迷你条左边那两个数字。
	LatMS   float64 `json:"lat_ms"`
	LossPct float64 `json:"loss_pct"`
	// Lat / Loss 的长度恒为 buckets，没有数据的那一段是 null。
	//
	// 用 null 而不是 0：0 ms / 0% 丢包都是**有意义的实测值**，
	// 与"这一段压根没探到"必须能分辨 —— 前端把 null 画成浅灰底，把 0 画成绿格。
	Lat  []*float64 `json:"lat"`
	Loss []*float64 `json:"loss"`
	// Targets 是**按目标**拆开的聚合，给卡片上「探测」那一行用（每个目标一个当前延迟）。
	//
	// 永远是数组（哪怕空）：JSON 里的 null 会让前端多一条判断。
	// 顺序在这里不保证，由 server 层按探测目标配置的顺序排好（前端按顺序取色/显示，
	// 与详情页延迟图的图例顺序一致）。
	Targets []OverviewPingTarget `json:"targets"`
}

// OverviewPingTarget 是某个节点的一个探测目标在总览窗口内的聚合。
//
// 为什么不塞进 nodeDTO（每分钟才变一次的探测结果没必要挤进每秒推送的 SSE）：
// 见 internal/server/overview.go 的说明。
type OverviewPingTarget struct {
	ID int64 `json:"id"`
	// Label / Host 由 server 层从探测目标**配置**里补上：探测结果是数据（只记
	// target_id，目标删掉后历史还在），名字是配置。库这边不猜名字。
	Label string `json:"label"`
	Host  string `json:"host"`
	// LatMS 是"当前"延迟，也就是最近一个有延迟样本的桶的加权平均（默认 6 分钟一段）。
	//
	// 表是 1 分钟粒度、卡片 60 秒刷新一次，所以这就是能拿到的最新鲜的值；
	// 用整窗口的平均值当"当前"会让卡片永远显示一小时前那个数。
	LatMS float64 `json:"lat_ms"`
	// AvgMS 是整窗口的平均延迟（与 PingSeries.AvgMS 同一套口径：按成功探测次数加权）。
	// 卡片上「探测」那一行的着色就是拿 LatMS 与它比（≤1.2× 绿、≤2× 黄、>2× 红）：
	// 问的是"这台机器现在是不是比它自己这一小时的平均水平差"。
	// 首页迷你条的延迟格子用的是**同一个**窗口平均值（见 app.js 的 miniLatClass）——
	// 两处昨天还各有一套阈值口径，现在都落回"与自己的平均水平比"这一条，
	// 差别只在倍数（格子 2×、探测行 1.2×），不再有第二种基准。
	AvgMS float64 `json:"avg_ms"`
	// LossPct 是整窗口按探测次数加权的丢包率。
	LossPct float64 `json:"loss_pct"`
	// HasData 表示这个目标在窗口里有没有任何探测记录。
	// false 的目标照样出现在数组里（"这个目标一个点都没有"本身就是信息），
	// 只是前端画成 —。
	HasData bool `json:"has_data"`
}

// targetAcc 是扫描过程中"某节点某目标"的累加器。
type targetAcc struct {
	latWeighted float64
	up, all     int64
	// lastLat 是**最近一个有延迟样本的桶**的值。行按 (node, target, bucket) 升序
	// 到达，所以最后写进去的就是最新的那一段。
	lastLat float64
}

// bucketAcc 是扫描过程中"某节点某一段"的累加器（跨目标）。
type bucketAcc struct {
	latWeighted float64
	up, all     int64
}

// nodeAcc 是一个节点的全部累加器。
type nodeAcc struct {
	latWeighted float64
	up, all     int64
	buckets     []bucketAcc
	targets     map[int64]*targetAcc
}

// QueryOverviewPing 一次取回**所有节点**在 [start, end) 内的分桶与分目标探测概览。
//
// 为什么是一条 SQL 而不是每个节点查一次：首页每张卡片都要画迷你条、每张卡片还要
// 显示每个目标的当前延迟，节点一多，"按节点循环"就是典型的 N+1
// （50 个节点 = 50 次查询 + 50 次语句准备）。这里按 (node_id, target_id, 桶号) 分组，
// 一次扫描就把两种口径（跨目标的分桶、跨分桶的分目标）都带回来 —— 再把它们拆成
// 两条 SQL 就等于把同一段数据读两遍。
//
// 为什么分组维度里必须有 target_id：分桶值要的是"这台机器整体"，目标值要的是
// "到某个目标"。一条 GROUP BY 只能有一个分组维度，所以取最细的那个
// (node_id, target_id, 桶号)，两种口径在 Go 侧各自累加（见 nodeAcc）。
// 内存与节点数×（段数+目标数）成正比，与"目标数×段数"无关。
//
// 桶号在 SQL 里算成 (ts - start) / bucketSec：窗口长度恒为 buckets × bucketSec，
// 所以桶号一定落在 [0, buckets)；越界的脏数据在这里丢掉，而不是让某一格错位。
func (d *DB) QueryOverviewPing(ctx context.Context, start, end, bucketSec int64, buckets int) (map[int64]OverviewPing, error) {
	if bucketSec <= 0 || buckets <= 0 || end <= start {
		return nil, fmt.Errorf("非法的总览窗口: start=%d end=%d bucket=%d buckets=%d",
			start, end, bucketSec, buckets)
	}

	rows, err := d.r.QueryContext(ctx, `
		SELECT node_id, target_id, (ts - ?) / ? AS bucket,
			SUM(avg_ms * up_cnt), SUM(up_cnt), SUM(all_cnt)
		FROM `+TablePing1m+`
		WHERE ts >= ? AND ts < ?
		GROUP BY node_id, target_id, bucket
		ORDER BY node_id, target_id, bucket`,
		start, bucketSec, start, end)
	if err != nil {
		return nil, fmt.Errorf("查询总览探测数据失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	accs := make(map[int64]*nodeAcc)
	for rows.Next() {
		var (
			nodeID      int64
			targetID    int64
			index       int64
			latWeighted float64
			up, all     int64
		)
		if err := rows.Scan(&nodeID, &targetID, &index, &latWeighted, &up, &all); err != nil {
			return nil, fmt.Errorf("读取总览探测数据失败: %w", err)
		}
		if index < 0 || index >= int64(buckets) {
			continue
		}

		acc := accs[nodeID]
		if acc == nil {
			acc = &nodeAcc{
				buckets: make([]bucketAcc, buckets),
				targets: make(map[int64]*targetAcc),
			}
			accs[nodeID] = acc
		}
		// 节点级：整窗口的加权和单独累加，不与各桶的均值混在一起 ——
		// 各桶的探测次数不同，把桶均值再平均一遍就等于给每一段投了相同的一票。
		acc.latWeighted += latWeighted
		acc.up += up
		acc.all += all

		b := &acc.buckets[index]
		b.latWeighted += latWeighted
		b.up += up
		b.all += all

		t := acc.targets[targetID]
		if t == nil {
			t = &targetAcc{}
			acc.targets[targetID] = t
		}
		t.latWeighted += latWeighted
		t.up += up
		t.all += all
		// 全丢的那一段没有延迟样本（up=0）：它不该覆盖"当前延迟"。
		if up > 0 {
			t.lastLat = latWeighted / float64(up)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历总览探测数据失败: %w", err)
	}

	// 收尾：所有换算走与 QueryPingSeries 完全相同的两个函数（up 加权、lossPctOf）。
	// 两处口径一旦分叉，左边写着 198ms、格子却按另一个均值分级，谁看都是 bug。
	out := make(map[int64]OverviewPing, len(accs))
	for nodeID, acc := range accs {
		p := OverviewPing{
			Lat:     make([]*float64, buckets),
			Loss:    make([]*float64, buckets),
			Targets: make([]OverviewPingTarget, 0, len(acc.targets)),
		}
		for i := range acc.buckets {
			b := &acc.buckets[i]
			// 全丢的那一段没有延迟样本（up=0）：留 null，而不是画成 0 ms。
			if b.up > 0 {
				value := b.latWeighted / float64(b.up)
				p.Lat[i] = &value
			}
			if b.all > 0 {
				value := lossPctOf(b.up, b.all)
				p.Loss[i] = &value
			}
		}
		if acc.up > 0 {
			p.LatMS = acc.latWeighted / float64(acc.up)
		}
		p.LossPct = lossPctOf(acc.up, acc.all)
		for targetID, t := range acc.targets {
			item := OverviewPingTarget{
				ID:      targetID,
				LatMS:   t.lastLat,
				LossPct: lossPctOf(t.up, t.all),
				HasData: t.all > 0,
			}
			if t.up > 0 {
				item.AvgMS = t.latWeighted / float64(t.up)
			}
			p.Targets = append(p.Targets, item)
		}
		out[nodeID] = p
	}
	return out, nil
}
