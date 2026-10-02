package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"probe/internal/protocol"
)

// almostEqual 比较两个浮点数（倍数换算会带出最后一位的误差，不能直接 ==）。
func almostEqual(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// 0003 是"给已发布的库加一张表"的迁移，只有真的从 v2 库升级上来才算测到。
//
// 与 0002 的用例同样的思路：手工造一个 v1+0002 的库（user_version=2），
// 再让 Open 去补 0003 —— 直接新建的库是 0001~0003 一次跑完的，
// 盖不住"老库缺表"这条路径。
func TestMigration0003UpgradesExistingDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "probe.db")

	raw, err := sql.Open("sqlite", dsn(path, true))
	if err != nil {
		t.Fatalf("打开原始数据库: %v", err)
	}
	for _, stmt := range schemaV1 {
		if _, err := raw.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("执行 0001 建表语句: %v", err)
		}
	}
	for _, stmt := range migrations[1].stmts {
		if _, err := raw.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("执行 0002 语句: %v", err)
		}
	}
	// 老库里已有数据：升级后必须原样还在。
	if _, err := raw.ExecContext(ctx,
		`INSERT INTO nodes (name, token_hash, created_at, updated_at) VALUES (?, ?, ?, ?)`,
		"legacy-01", []byte{0x01, 0x02}, 100, 100); err != nil {
		t.Fatalf("插入老节点: %v", err)
	}
	if _, err := raw.ExecContext(ctx, `PRAGMA user_version = 2`); err != nil {
		t.Fatalf("设置 schema 版本: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("关闭原始数据库: %v", err)
	}

	db, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("升级打开: %v", err)
	}
	defer func() { _ = db.Close() }()

	version, err := db.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	// 只断言"补到了最新版本、且至少包含 0003"：后面每加一条迁移都会让这个数字变大，
	// 把 3 写死会让每次加迁移都要回来改这个用例（0002 的用例同样只钉下限）。
	if version != len(migrations) || version < 3 {
		t.Fatalf("升级后版本 = %d，期望 %d（0003_ping_samples 及以后的全部迁移）", version, len(migrations))
	}
	assertTables(t, db)

	if _, err := db.NodeByID(ctx, 1); err != nil {
		t.Fatalf("老节点在升级后丢了: %v", err)
	}

	// 新表能写能读（迁移真的建了表，而不是只改了版本号）。
	bucket := NewPingBucket(1, 7, 1_700_000_000, 23.4, 20.1, 31.2, 0)
	if err := db.UpsertPingBuckets(ctx, []PingBucket{bucket}); err != nil {
		t.Fatalf("写入探测桶: %v", err)
	}
	var avg float64
	if err := db.Reader().QueryRowContext(ctx,
		`SELECT avg_ms FROM ping_samples_1m WHERE node_id = 1 AND target_id = 7 AND ts = ?`,
		bucket.TS).Scan(&avg); err != nil {
		t.Fatalf("读取探测桶: %v", err)
	}
	if avg != 23.4 {
		t.Fatalf("读回的 avg_ms = %v，期望 23.4", avg)
	}
}

func TestUpsertPingBucketsIsIdempotent(t *testing.T) {
	ctx := context.Background()
	db := openTemp(t)
	ts := int64(1_700_000_000) - int64(1_700_000_000)%60

	first := NewPingBucket(1, 1, ts, 20, 18, 25, 0)
	if err := db.UpsertPingBuckets(ctx, []PingBucket{first}); err != nil {
		t.Fatalf("首次写入: %v", err)
	}
	// 同一个 (node, target, ts) 再写一次：覆盖而不是插出第二行。
	second := NewPingBucket(1, 1, ts, 30, 28, 35, 33.333)
	if err := db.UpsertPingBuckets(ctx, []PingBucket{second, second}); err != nil {
		t.Fatalf("重复写入: %v", err)
	}

	var n int
	if err := db.Reader().QueryRowContext(ctx,
		`SELECT count(*) FROM ping_samples_1m WHERE node_id = 1 AND target_id = 1`).Scan(&n); err != nil {
		t.Fatalf("统计行数: %v", err)
	}
	if n != 1 {
		t.Fatalf("幂等写入后行数 = %d，期望 1", n)
	}
	var avg, loss float64
	var up, all int64
	if err := db.Reader().QueryRowContext(ctx,
		`SELECT avg_ms, loss_pct, up_cnt, all_cnt FROM ping_samples_1m WHERE node_id = 1 AND target_id = 1`).
		Scan(&avg, &loss, &up, &all); err != nil {
		t.Fatalf("读取: %v", err)
	}
	if avg != 30 || loss != 33.333 || all != 100 {
		t.Fatalf("覆盖后的值不对: avg=%v loss=%v up=%d all=%d", avg, loss, up, all)
	}
	if up != 67 { // 100 - 33.333 四舍五入
		t.Fatalf("up_cnt = %d，期望 67（loss 33.333%% 换算成 1/100 单位）", up)
	}
}

func TestQueryPingSeriesBucketsAndLoss(t *testing.T) {
	ctx := context.Background()
	db := openTemp(t)
	now := time.Unix(1_700_000_000, 0)
	rg, ok := PingRangeByKey("1h")
	if !ok {
		t.Fatal("1h 档位不存在")
	}
	// 落在窗口内的 5 个连续分钟（桶宽 60 秒 → 每个桶一个点）。
	base := now.Unix() - now.Unix()%60 - 5*60
	for i := int64(0); i < 5; i++ {
		loss := 0.0
		if i == 4 {
			loss = 100
		}
		if err := db.UpsertPingBuckets(ctx, []PingBucket{
			NewPingBucket(1, 1, base+i*60, float64(20+i), float64(18+i), float64(25+i), loss),
			NewPingBucket(1, 2, base+i*60, float64(50+i), float64(48+i), float64(55+i), 0),
			NewPingBucket(2, 1, base+i*60, 999, 999, 999, 0), // 别的节点，不该被查到
		}); err != nil {
			t.Fatalf("写入: %v", err)
		}
	}
	// 窗口外的数据（比 1h 更早）不该出现。
	if err := db.UpsertPingBuckets(ctx, []PingBucket{
		NewPingBucket(1, 1, base-3600, 123, 123, 123, 0),
	}); err != nil {
		t.Fatalf("写入窗口外数据: %v", err)
	}

	got, err := db.QueryPingSeries(ctx, 1, 1, rg, now)
	if err != nil {
		t.Fatalf("查询: %v", err)
	}
	if !got.HasData || len(got.Points) != 5 {
		t.Fatalf("点数 = %d（has_data=%v），期望 5", len(got.Points), got.HasData)
	}
	if got.Points[0].TS != base || got.Points[0].Avg != 20 || got.Points[0].Max != 25 {
		t.Fatalf("第一个点不对: %+v", got.Points[0])
	}
	if got.Points[1].TS != base+60 {
		t.Fatalf("点没有按时间排序: %+v", got.Points)
	}
	if got.Points[0].Avg == 999 {
		t.Fatal("查到了别的节点的数据")
	}
	// 逐点丢包率：每个桶自己那一段的丢包率，画丢包竖条用的就是它。
	if got.Points[0].Loss != 0 || got.Points[1].Loss != 0 || got.Points[2].Loss != 0 ||
		got.Points[3].Loss != 0 {
		t.Fatalf("前四个桶没有丢包，逐点丢包率应当为 0: %+v", got.Points)
	}
	if got.Points[4].Loss != 100 {
		t.Fatalf("最后一个桶全丢，丢包率 = %v，期望 100", got.Points[4].Loss)
	}
	// 整体丢包率 = 5 个探测里丢了 1 个 = 20%。
	if got.LossPct < 19.9 || got.LossPct > 20.1 {
		t.Fatalf("整体丢包率 = %v，期望约 20", got.LossPct)
	}

	// 另一个目标自己一条曲线；不存在的目标返回空但不算错。
	other, err := db.QueryPingSeries(ctx, 1, 2, rg, now)
	if err != nil || len(other.Points) != 5 || other.LossPct != 0 {
		t.Fatalf("目标 2 的曲线不对: %+v (err=%v)", other, err)
	}
	empty, err := db.QueryPingSeries(ctx, 1, 99, rg, now)
	if err != nil {
		t.Fatalf("查询空目标不该报错: %v", err)
	}
	if empty.HasData || len(empty.Points) != 0 || empty.LossPct != 0 {
		t.Fatalf("空目标应当返回空曲线: %+v", empty)
	}
	if _, err := db.QueryPingSeries(ctx, 1, 1, PingRange{Key: "bad"}, now); err == nil {
		t.Fatal("非法档位应当报错")
	}
}

// 桶**平均延迟**也必须按成功探测次数加权，不能把"整分钟全丢"的那一行
// （avg_ms = 0、up_cnt = 0）当成一个 0ms 的样本平均进去。
//
// 场景：同一个 1 小时桶里两行 —— 一行正常（200ms、100 次成功），
// 一行整分钟全丢（avg_ms = 0、0 次成功、100 次探测）。
//
//	正确（按 up_cnt 加权）：(200×100 + 0×0) / (100 + 0) = 200ms
//	错误（AVG(avg_ms)）  ：(200 + 0) / 2 = 100ms     ← 直接砍半
//
// 表现就是"线路越丢包、图例上的延迟越低" —— 明明只是丢了几分钟，
// 整段平均延迟却掉下来了。这个测试就是钉住这一条。
func TestQueryPingSeriesAvgMSIgnoresFullyLostBuckets(t *testing.T) {
	ctx := context.Background()
	db := openTemp(t)
	now := time.Unix(1_700_000_000, 0)
	rg, ok := PingRangeByKey("7d")
	if !ok {
		t.Fatal("7d 档位不存在")
	}
	base := now.Unix() - now.Unix()%3600 - 7200

	if err := db.UpsertPingBuckets(ctx, []PingBucket{
		{NodeID: 1, TargetID: 1, TS: base, AvgMS: 200, MaxMS: 250, Up: 100, All: 100},
		// 整分钟全丢：没有延迟样本，avg_ms 与 up_cnt 都是 0。
		{NodeID: 1, TargetID: 1, TS: base + 60, AvgMS: 0, MaxMS: 0, Up: 0, All: 100},
		// 另一行部分丢包：60 次成功、平均 300ms。权重是 60 而不是 100。
		{NodeID: 1, TargetID: 1, TS: base + 120, AvgMS: 300, MaxMS: 400, Up: 60, All: 100},
	}); err != nil {
		t.Fatalf("写入: %v", err)
	}

	got, err := db.QueryPingSeries(ctx, 1, 1, rg, now)
	if err != nil {
		t.Fatalf("查询: %v", err)
	}
	if len(got.Points) != 1 {
		t.Fatalf("点数 = %d，期望 1", len(got.Points))
	}
	// 桶内加权：(200×100 + 300×60) / (100 + 60) = 38000 / 160 = 237.5
	// 若用 AVG(avg_ms) 会得到 (200 + 0 + 300) / 3 = 166.7（少了 30%）。
	if want := 237.5; got.Points[0].Avg < want-0.5 || got.Points[0].Avg > want+0.5 {
		t.Fatalf("桶平均延迟 = %v，期望约 %v（整分钟全丢的行不该参与平均；"+
			"AVG(avg_ms) 会得到约 166.7）", got.Points[0].Avg, want)
	}
	// 整段平均延迟与桶同源，也是同一个数（只有一个桶）。
	if got.AvgMS < 237.0 || got.AvgMS > 238.0 {
		t.Fatalf("整段平均延迟 = %v，期望约 237.5（图例显示的就是它）", got.AvgMS)
	}
	// 丢包照旧：三行分别是 0/100/40 次丢，合计 140 / 300 = 46.7%。
	// （注意第一行是 0% 丢包，别顺手把它也算成丢的。）
	if got.LossPct < 46.6 || got.LossPct > 46.8 {
		t.Fatalf("丢包率 = %v，期望约 46.7", got.LossPct)
	}

	// 极端情形：整段全丢时算不出平均延迟，必须是 0（前端据此不写延迟后缀），
	// 不能因为"没有成功样本"就退化成 0/0 或把 0 当延迟。
	if err := db.UpsertPingBuckets(ctx, []PingBucket{
		{NodeID: 2, TargetID: 1, TS: base, AvgMS: 0, MaxMS: 0, Up: 0, All: 100},
		{NodeID: 2, TargetID: 1, TS: base + 60, AvgMS: 0, MaxMS: 0, Up: 0, All: 100},
	}); err != nil {
		t.Fatalf("写入: %v", err)
	}
	allLost, err := db.QueryPingSeries(ctx, 2, 1, rg, now)
	if err != nil {
		t.Fatalf("查询: %v", err)
	}
	if len(allLost.Points) != 1 || allLost.Points[0].Avg != 0 {
		t.Fatalf("整段全丢时桶平均延迟应当是 0，实际 %+v", allLost.Points)
	}
	if allLost.AvgMS != 0 {
		t.Fatalf("整段全丢时平均延迟应当是 0（不是 NaN/Inf），实际 %v", allLost.AvgMS)
	}
	if allLost.LossPct != 100 {
		t.Fatalf("整段全丢时丢包率应当是 100，实际 %v", allLost.LossPct)
	}
}

// 桶丢包率按**探测次数**加权（SUM(up_cnt)/SUM(all_cnt)），不是各分钟
// loss_pct 的算术平均。
//
// 场景：同一个查询桶里两行 —— 一行"折算 100 次探测丢 1 次"（1%），一行
// "折算 10 次探测丢 4 次"（40%）。加权 = 5/110 ≈ 4.55%，算术平均 = 20.5%。
// 两者相差四倍多，所以这个测试能真正分辨实现用的是哪一种。
func TestQueryPingSeriesBucketLossIsProbeWeighted(t *testing.T) {
	ctx := context.Background()
	db := openTemp(t)
	now := time.Unix(1_700_000_000, 0)
	rg, ok := PingRangeByKey("7d") // 桶宽 900 秒：一个桶里放得下多行
	if !ok {
		t.Fatal("7d 档位不存在")
	}
	base := now.Unix() - now.Unix()%3600 - 7200 // 对齐到小时，稳稳落在窗口内（也是 900 秒桶的整数倍）

	if err := db.UpsertPingBuckets(ctx, []PingBucket{
		// 直接给计数列：NewPingBucket 只能写"100 次"这一种权重，
		// 而要验证的正是"权重不同的两行不能等同对待"。
		{NodeID: 1, TargetID: 1, TS: base, AvgMS: 20, MaxMS: 25, Up: 99, All: 100},
		{NodeID: 1, TargetID: 1, TS: base + 60, AvgMS: 30, MaxMS: 35, Up: 6, All: 10},
		{NodeID: 1, TargetID: 1, TS: base + 3600, AvgMS: 40, MaxMS: 45, Up: 100, All: 100},
	}); err != nil {
		t.Fatalf("写入: %v", err)
	}

	got, err := db.QueryPingSeries(ctx, 1, 1, rg, now)
	if err != nil {
		t.Fatalf("查询: %v", err)
	}
	if len(got.Points) != 2 {
		t.Fatalf("点数 = %d，期望 2（两个小时桶）", len(got.Points))
	}
	// 加权：(1 + 4) / (100 + 10) = 4.545…%
	if got.Points[0].Loss < 4.5 || got.Points[0].Loss > 4.6 {
		t.Fatalf("桶丢包率 = %v，期望约 4.55（按探测次数加权；算术平均会是 20.5）",
			got.Points[0].Loss)
	}
	if got.Points[1].Loss != 0 {
		t.Fatalf("第二个桶没有丢包，丢包率 = %v，期望 0", got.Points[1].Loss)
	}
	// 整段聚合与桶口径一致：5 / 210 ≈ 2.38%。
	if got.LossPct < 2.3 || got.LossPct > 2.5 {
		t.Fatalf("整体丢包率 = %v，期望约 2.38（与桶口径同源）", got.LossPct)
	}

	// 权重不同但丢包率相同的两行：加权结果应当等于那个共同的丢包率。
	if err := db.UpsertPingBuckets(ctx, []PingBucket{
		{NodeID: 2, TargetID: 1, TS: base, AvgMS: 20, MaxMS: 20, Up: 90, All: 100}, // 10%
		{NodeID: 2, TargetID: 1, TS: base + 60, AvgMS: 20, MaxMS: 20, Up: 9, All: 10},
	}); err != nil {
		t.Fatalf("写入: %v", err)
	}
	same, err := db.QueryPingSeries(ctx, 2, 1, rg, now)
	if err != nil || len(same.Points) != 1 {
		t.Fatalf("查询: %+v (err=%v)", same, err)
	}
	if same.Points[0].Loss < 9.9 || same.Points[0].Loss > 10.1 {
		t.Fatalf("两行都是 10%% 丢包时，加权结果 = %v，期望 10", same.Points[0].Loss)
	}
}

func TestDeleteOldPingSamples(t *testing.T) {
	ctx := context.Background()
	db := openTemp(t)
	now := time.Now()
	db.UpsertPingBuckets(ctx, nil) // 空批次应当是无操作

	old := now.Add(-9 * 24 * time.Hour).Unix()
	old -= old % 60
	fresh := now.Add(-time.Hour).Unix()
	fresh -= fresh % 60
	for _, ts := range []int64{old, fresh} {
		for _, node := range []int64{1, 2} {
			if err := db.UpsertPingBuckets(ctx, []PingBucket{NewPingBucket(node, 1, ts, 20, 20, 20, 0)}); err != nil {
				t.Fatalf("写入: %v", err)
			}
		}
	}

	deleted, err := db.DeleteOldPingSamples(ctx, now.Add(-8*24*time.Hour), []int64{1, 2})
	if err != nil {
		t.Fatalf("清理: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("清理行数 = %d，期望 2（两个节点各一行过期的）", deleted)
	}
	var n int
	if err := db.Reader().QueryRowContext(ctx, `SELECT count(*) FROM ping_samples_1m`).Scan(&n); err != nil {
		t.Fatalf("统计: %v", err)
	}
	if n != 2 {
		t.Fatalf("清理后剩余 %d 行，期望 2（只删过期的）", n)
	}
	// 一个节点都不给时不做任何事（与 DeleteOldSamples 一致，避免无条件全表删除）。
	if deleted, err := db.DeleteOldPingSamples(ctx, now, nil); err != nil || deleted != 0 {
		t.Fatalf("空节点列表应当是无操作: deleted=%d err=%v", deleted, err)
	}
}

func TestDeleteNodeRemovesPingSamples(t *testing.T) {
	ctx := context.Background()
	db := openTemp(t)
	now := time.Now()
	node, _, err := db.CreateNode(ctx, NewNode{
		Name: "ping-del", IntervalSec: 1, TrafficWarnPct: 80, ResetDay: 1,
	}, now)
	if err != nil {
		t.Fatalf("创建节点: %v", err)
	}
	if err := db.UpsertPingBuckets(ctx, []PingBucket{
		NewPingBucket(node.ID, 1, now.Unix()-now.Unix()%60, 20, 20, 20, 0),
	}); err != nil {
		t.Fatalf("写入探测桶: %v", err)
	}
	if err := db.DeleteNode(ctx, node.ID); err != nil {
		t.Fatalf("删除节点: %v", err)
	}
	var n int
	if err := db.Reader().QueryRowContext(ctx,
		`SELECT count(*) FROM ping_samples_1m WHERE node_id = ?`, node.ID).Scan(&n); err != nil {
		t.Fatalf("统计: %v", err)
	}
	if n != 0 {
		t.Fatalf("删除节点后仍残留 %d 行探测数据", n)
	}
}

// 六档桶宽与 X 轴基准间隔是用户定稿的表格，测试锁死：改动它必须先改需求。
//
// 这一版的**分辨率表**是 1m/1m/1m/2m/5m/15m，桶宽与基准间隔逐档相同（一个柱子 = 一个
// 刻度单位）：6h/12h 从"一根柱子 5/10 分钟"变成 1 分钟，代价是点数从 72 涨到 360/720。
//
// 表格本身只说明"现在是这几个数"，另有两条不变量说明"允许的范围"：
//   - 桶宽 ≥ 60 秒：ping_samples_1m 只有 1 分钟一级粒度（见 schema.go），
//     桶比它细只会得到"1 个有值的桶 + N 个空桶"，图上是大片空洞；
//   - 点数 ≤ maxPoints（1000）：与资源图同一条硬上限（见 ranges.go），
//     超了前端画不动、响应也大。本表最大是 3d 档的 864。
//
// 顺带钉住"窗口能被桶宽整除、对齐后的窗口恰好容纳 Points() 个完整桶"：
// 不整除就会出现残缺的首尾桶（一个 60 秒的点其实只统计了 30 秒）。
func TestPingRangesBucketTable(t *testing.T) {
	want := []struct {
		key      string
		sec      int64
		bucket   int64
		tickBase int64
		points   int
	}{
		{"1h", 3600, 60, 60, 60},
		{"6h", 21600, 60, 60, 360},
		{"12h", 43200, 60, 60, 720},
		{"1d", 86400, 120, 120, 720},
		{"3d", 259200, 300, 300, 864},
		{"7d", 604800, 900, 900, 672},
	}
	ranges := PingRanges()
	if len(ranges) != len(want) {
		t.Fatalf("档位数 = %d，期望 %d", len(ranges), len(want))
	}
	for i, w := range want {
		r := ranges[i]
		if r.Key != w.key || int64(r.Window.Seconds()) != w.sec || r.Bucket != w.bucket {
			t.Errorf("第 %d 档 = %s/%ds/%ds，期望 %s/%ds/%ds",
				i, r.Key, int64(r.Window.Seconds()), r.Bucket, w.key, w.sec, w.bucket)
		}
		if r.TickBaseSec != w.tickBase {
			t.Errorf("%s 档基准间隔 = %d 秒，期望 %d 秒", r.Key, r.TickBaseSec, w.tickBase)
		}
		// 点数 = 窗口 ÷ 桶宽，逐档写死：它就是用户看到的数据量。
		if got := r.Points(); got != w.points {
			t.Errorf("%s 档点数 = %d，期望 %d（%d 秒 ÷ %d 秒）",
				r.Key, got, w.points, w.sec, w.bucket)
		}
		if r.Bucket < 60 {
			t.Errorf("%s 档桶宽 = %d 秒，比源表粒度（1 分钟）还细：桶里大部分是空的",
				r.Key, r.Bucket)
		}
		if p := r.Points(); p > maxPoints {
			t.Errorf("%s 档点数 = %d，超过上限 %d", r.Key, p, maxPoints)
		}
		if p := r.Points(); p < 60 {
			t.Errorf("%s 档点数 = %d，太少：看不出曲线的形状", r.Key, p)
		}
		if int64(r.Window.Seconds())%r.Bucket != 0 {
			t.Errorf("%s 档：桶宽 %d 不能整除窗口 %d 秒，首尾会出现残缺桶",
				r.Key, r.Bucket, int64(r.Window.Seconds()))
		}
		start, end := r.window(time.Now())
		if end%r.Bucket != 0 || start%r.Bucket != 0 {
			t.Errorf("%s 档：查询窗口两端没有落在桶网格上（%d, %d）", r.Key, start, end)
		}
		if count := int(end-start) / int(r.Bucket); count != r.Points() {
			t.Errorf("%s 档：对齐窗口能容纳 %d 个桶，期望 %d", r.Key, count, r.Points())
		}
	}
	if _, ok := PingRangeByKey("2h"); ok {
		t.Error("2h 不是合法档位")
	}
}

// openTempB 与 openTemp 相同，给基准用（testing.TB 同时覆盖 *testing.T 与 *testing.B）。
func openTempB(b *testing.B) *DB {
	b.Helper()
	db, err := Open(context.Background(), filepath.Join(b.TempDir(), "probe.db"))
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	b.Cleanup(func() { _ = db.Close() })
	return db
}

// seedPingWeek 铺一份"数据铺满"的最坏情况：节点 1 / 目标 1，每分钟一行、整整 7 天
// （10080 行 × 60 秒 = 604800 秒 = 一个 7d 档窗口），末尾正好落在 now 对齐到的分钟上。
//
// 六个档位的窗口都落在这一份数据里，所以它们**都**应当被填满。
func seedPingWeek(tb testing.TB, db *DB, now time.Time) {
	tb.Helper()
	const minutes = 7 * 24 * 60
	base := now.Unix() - now.Unix()%60 - minutes*60
	rows := make([]PingBucket, 0, minutes)
	for i := int64(0); i < minutes; i++ {
		rows = append(rows, NewPingBucket(1, 1, base+i*60,
			float64(20+i%7), 18, float64(25+i%9), 0))
	}
	if err := db.UpsertPingBuckets(context.Background(), rows); err != nil {
		tb.Fatalf("铺数据: %v", err)
	}
}

// 数据铺满时，每个档位返回的点数必须**恰好**等于 Points()（窗口 ÷ 桶宽）：
// 这是把分辨率表落到真实 SQL 上的验证 —— 组数由 GROUP BY (ts/bucket)*bucket 数出来，
// 少一个点说明有桶是空的（桶宽比源粒度细）或首尾桶残缺（窗口没对齐桶网格）。
//
// 首尾两点的时刻也钉住：第一个点 = 窗口起点，最后一个点 = 终点 − 桶宽。
func TestQueryPingSeriesReturnsExactlyRangePoints(t *testing.T) {
	ctx := context.Background()
	db := openTemp(t)
	now := time.Now()
	seedPingWeek(t, db, now)

	for _, r := range PingRanges() {
		series, err := db.QueryPingSeries(ctx, 1, 1, r, now)
		if err != nil {
			t.Fatalf("查询 %s 档: %v", r.Key, err)
		}
		if want := r.Points(); len(series.Points) != want {
			t.Errorf("%s 档返回 %d 个点，期望 %d（窗口 %d 秒 ÷ 桶宽 %d 秒）",
				r.Key, len(series.Points), want, int64(r.Window.Seconds()), r.Bucket)
			continue
		}
		start, end := r.window(now)
		if got := series.Points[0].TS; got != start {
			t.Errorf("%s 档第一个点 ts = %d，期望窗口起点 %d", r.Key, got, start)
		}
		if got := series.Points[len(series.Points)-1].TS; got != end-r.Bucket {
			t.Errorf("%s 档最后一个点 ts = %d，期望 %d（终点 − 桶宽）", r.Key, got, end-r.Bucket)
		}
	}
}

// BenchmarkQueryPingSeriesTiers 量一遍六个档位在同一条 7 天数据上的查询代价。
//
// 为什么要留这个基准：分辨率表把 6h/12h 从 72 点抬到 360/720、1d/3d/7d 也各涨了一截，
// 而 /ping 是**每个探测目标一条查询**（最多 16 条，见 server/api_ping.go），
// 返回的点数就是响应体大小。数据用 seedPingWeek（最坏情况：每分钟一行、7 天）。
//
// 它**不在** 1 Hz 的实时循环里（那里只有 currentNodes()，见 server/api_stream.go）：
// 只有打开详情页、切延迟档位、切目标开关时才查，所以看的是"单次查询绝对耗时"，
// 而不是每秒预算。
func BenchmarkQueryPingSeriesTiers(b *testing.B) {
	ctx := context.Background()
	db := openTempB(b)
	now := time.Now()
	seedPingWeek(b, db, now)

	for _, r := range PingRanges() {
		b.Run(r.Key, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				series, err := db.QueryPingSeries(ctx, 1, 1, r, now)
				if err != nil {
					b.Fatalf("查询 %s 档: %v", r.Key, err)
				}
				if len(series.Points) > maxPoints {
					b.Fatalf("%s 档返回 %d 个点，超过上限 %d", r.Key, len(series.Points), maxPoints)
				}
			}
		})
	}
}

// ---- 设置项 ----

func TestPingSettingsDefaultsToEmpty(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()

	targets, err := db.PingTargets(ctx)
	if err != nil {
		t.Fatalf("读取目标: %v", err)
	}
	if len(targets) != 0 {
		t.Fatalf("缺省应当是空列表，实际 %+v", targets)
	}
	if targets == nil {
		t.Fatal("缺省应当返回空切片（不是 nil）：接口里要是 [] 而不是 null")
	}
	interval, err := db.PingIntervalSec(ctx)
	if err != nil {
		t.Fatalf("读取间隔: %v", err)
	}
	if interval != protocol.DefaultPingIntervalSec {
		t.Fatalf("缺省间隔 = %d，期望 %d", interval, protocol.DefaultPingIntervalSec)
	}
}

func TestPingSettingsFallBackOnDirtyValue(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()

	dirtyTargets := []string{
		"", "not json", `{"id":1}`, `"1.1.1.1"`, `[1,2]`, `[{"id":1,"type":"udp","host":"1.1.1.1"}]`,
		`[{"id":1,"type":"tcp","host":"1.1.1.1","port":0}]`,
		`[{"id":1,"type":"tcp","host":"","port":443}]`,
		`[{"id":0,"type":"tcp","host":"1.1.1.1","port":443}]`,
		`[{"id":1,`, // 写到一半断电
	}
	for _, dirty := range dirtyTargets {
		if err := db.SetSettings(ctx, map[string]string{KeyPingTargets: dirty}); err != nil {
			t.Fatalf("写入脏数据 %q: %v", dirty, err)
		}
		got, err := db.PingTargets(ctx)
		if err != nil {
			t.Fatalf("脏数据 %q 不该返回错误: %v", dirty, err)
		}
		if len(got) != 0 {
			t.Errorf("脏数据 %q 应当回退到空列表，实际 %+v", dirty, got)
		}
	}

	// 间隔：缺省、非数字、越界都回退到 60。
	for _, dirty := range []string{"", "abc", "0", "9", "3601", "-60", "60.5"} {
		if err := db.SetSettings(ctx, map[string]string{KeyPingIntervalSec: dirty}); err != nil {
			t.Fatalf("写入脏数据 %q: %v", dirty, err)
		}
		got, err := db.PingIntervalSec(ctx)
		if err != nil {
			t.Fatalf("脏数据 %q 不该返回错误: %v", dirty, err)
		}
		if got != protocol.DefaultPingIntervalSec {
			t.Errorf("脏数据 %q 应当回退到 %d，实际 %d", dirty, protocol.DefaultPingIntervalSec, got)
		}
	}

	// 混合场景：列表里有一条好的、一条坏的 → 只丢坏的那条（不能让整页白掉）。
	mixed := `[{"id":1,"type":"tcp","host":"1.1.1.1","port":443,"label":"CF","enabled":true},` +
		`{"id":2,"type":"udp","host":"9.9.9.9"},{"id":3,"type":"icmp","host":"1.0.0.1","port":1234,"enabled":false}]`
	if err := db.SetSettings(ctx, map[string]string{KeyPingTargets: mixed}); err != nil {
		t.Fatalf("写入混合数据: %v", err)
	}
	got, err := db.PingTargets(ctx)
	if err != nil {
		t.Fatalf("读取: %v", err)
	}
	if len(got) != 2 || got[0].Host != "1.1.1.1" || got[1].Host != "1.0.0.1" {
		t.Fatalf("混合数据应当只丢掉坏的那条，实际 %+v", got)
	}
	if got[1].Port != 0 {
		t.Fatalf("icmp 目标的端口应当被清零，实际 %d", got[1].Port)
	}
}

func TestSetPingSettingsAssignsAndKeepsIDs(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()

	// 1) 全新列表：服务端分配 ID。
	saved, err := db.SetPingSettings(ctx, []PingTarget{
		{Label: "Cloudflare", Type: protocol.PingTypeTCP, Host: "1.1.1.1", Port: 443, Enabled: true},
		{Label: "Google", Type: protocol.PingTypeICMP, Host: "8.8.8.8", Enabled: true},
	}, 60)
	if err != nil {
		t.Fatalf("保存: %v", err)
	}
	if len(saved.Targets) != 2 || saved.Targets[0].ID != 1 || saved.Targets[1].ID != 2 {
		t.Fatalf("新目标的 ID 应当从 1 开始分配: %+v", saved.Targets)
	}
	if saved.IntervalSec != 60 {
		t.Fatalf("间隔没被保存: %+v", saved)
	}

	// 2) 回传既有 ID（改名）+ 一个新目标：老 ID 保留，新目标拿下一个号。
	saved, err = db.SetPingSettings(ctx, []PingTarget{
		{ID: 2, Label: "Google DNS", Type: protocol.PingTypeICMP, Host: "8.8.8.8", Enabled: true},
		{Label: "Quad9", Type: protocol.PingTypeTCP, Host: "9.9.9.9", Port: 853, Enabled: true},
	}, 30)
	if err != nil {
		t.Fatalf("保存: %v", err)
	}
	if saved.Targets[0].ID != 2 || saved.Targets[0].Label != "Google DNS" {
		t.Fatalf("改名不该换 ID: %+v", saved.Targets[0])
	}
	if saved.Targets[1].ID != 3 {
		t.Fatalf("新目标应当拿到下一个 ID（3），实际 %d", saved.Targets[1].ID)
	}

	// 3) 删掉最大的那个再新建：ID 不能回收（否则历史曲线会被接错）。
	saved, err = db.SetPingSettings(ctx, []PingTarget{
		{ID: 2, Label: "Google DNS", Type: protocol.PingTypeICMP, Host: "8.8.8.8", Enabled: true},
	}, 30)
	if err != nil {
		t.Fatalf("保存: %v", err)
	}
	saved, err = db.SetPingSettings(ctx, []PingTarget{
		{ID: 2, Label: "Google DNS", Type: protocol.PingTypeICMP, Host: "8.8.8.8", Enabled: true},
		{Label: "Cloudflare", Type: protocol.PingTypeTCP, Host: "1.1.1.1", Port: 443, Enabled: true},
	}, 30)
	if err != nil {
		t.Fatalf("保存: %v", err)
	}
	if saved.Targets[1].ID <= 3 {
		t.Fatalf("删掉的目标 ID 不该被复用，实际新 ID = %d", saved.Targets[1].ID)
	}

	// 4) 带一个当前不存在的 ID（伪造/过期）→ 重新分配，绝不接受客户端的 ID。
	saved, err = db.SetPingSettings(ctx, []PingTarget{
		{ID: 999, Label: "Bogus", Type: protocol.PingTypeTCP, Host: "203.0.113.9", Port: 80, Enabled: true},
	}, 30)
	if err != nil {
		t.Fatalf("保存: %v", err)
	}
	if saved.Targets[0].ID == 999 {
		t.Fatalf("不存在的 ID 应当被重新分配，实际仍是 %d", saved.Targets[0].ID)
	}

	// 5) 同一份请求里两个目标带同一个既有 ID：只有一个能保留它，另一个必须重新分配
	//    （否则两条曲线会共用一个身份，历史会串在一起）。
	kept := saved.Targets[0]
	saved, err = db.SetPingSettings(ctx, []PingTarget{
		{ID: kept.ID, Label: "A", Type: protocol.PingTypeTCP, Host: "198.51.100.1", Port: 80, Enabled: true},
		{ID: kept.ID, Label: "B", Type: protocol.PingTypeTCP, Host: "198.51.100.2", Port: 80, Enabled: true},
	}, 30)
	if err != nil {
		t.Fatalf("保存: %v", err)
	}
	if saved.Targets[0].ID != kept.ID {
		t.Fatalf("第一个目标应当保留既有 ID %d，实际 %d", kept.ID, saved.Targets[0].ID)
	}
	if saved.Targets[1].ID == kept.ID {
		t.Fatalf("同一个 ID 不该落到两个目标身上: %+v", saved.Targets)
	}
}

func TestSetPingSettingsIDsSurviveReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "probe.db")
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("打开: %v", err)
	}
	saved, err := db.SetPingSettings(ctx, []PingTarget{
		{Type: protocol.PingTypeTCP, Host: "1.1.1.1", Port: 443, Enabled: true},
		{Type: protocol.PingTypeTCP, Host: "8.8.4.4", Port: 53, Enabled: true},
	}, 60)
	if err != nil {
		t.Fatalf("保存: %v", err)
	}
	if saved.Targets[1].ID != 2 {
		t.Fatalf("准备数据失败: %+v", saved.Targets)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("关闭: %v", err)
	}

	// 重启后再新建一个目标：必须接着 3 往下发，不能回到 1（否则会与已有曲线撞号）。
	db2, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("重新打开: %v", err)
	}
	defer func() { _ = db2.Close() }()
	saved, err = db2.SetPingSettings(ctx, []PingTarget{
		{ID: 1, Type: protocol.PingTypeTCP, Host: "1.1.1.1", Port: 443, Enabled: true},
		{ID: 2, Type: protocol.PingTypeTCP, Host: "8.8.4.4", Port: 53, Enabled: true},
		{Type: protocol.PingTypeICMP, Host: "9.9.9.9", Enabled: true},
	}, 60)
	if err != nil {
		t.Fatalf("保存: %v", err)
	}
	if saved.Targets[2].ID != 3 {
		t.Fatalf("重启后新 ID = %d，期望 3", saved.Targets[2].ID)
	}
}

func TestSetPingSettingsDedupesAndNormalizes(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()

	saved, err := db.SetPingSettings(ctx, []PingTarget{
		{Label: "  Cloudflare  ", Type: "TCP", Host: " 1.1.1.1 ", Port: 443, Enabled: true},
		{Label: "重复的", Type: protocol.PingTypeTCP, Host: "1.1.1.1", Port: 443, Enabled: true},
		{Type: protocol.PingTypeICMP, Host: "8.8.8.8", Port: 1234, Enabled: true},
		{Type: protocol.PingTypeICMP, Host: "8.8.8.8", Enabled: true},
	}, 60)
	if err != nil {
		t.Fatalf("保存: %v", err)
	}
	if len(saved.Targets) != 2 {
		t.Fatalf("相同 type+host+port 应当自动去重，实际 %+v", saved.Targets)
	}
	if saved.Targets[0].Label != "Cloudflare" || saved.Targets[0].Type != protocol.PingTypeTCP ||
		saved.Targets[0].Host != "1.1.1.1" {
		t.Fatalf("归一化没生效: %+v", saved.Targets[0])
	}
	// icmp 没有端口：端口被清零，因此"8.8.8.8:1234"与"8.8.8.8"是同一个目标。
	if saved.Targets[1].Port != 0 {
		t.Fatalf("icmp 的端口应当被清零: %+v", saved.Targets[1])
	}
	// label 留空时用 host 兜底，图例上不会出现一条没有名字的曲线。
	saved, err = db.SetPingSettings(ctx, []PingTarget{
		{Type: protocol.PingTypeTCP, Host: "example.com", Port: 80, Enabled: true},
	}, 60)
	if err != nil {
		t.Fatalf("保存: %v", err)
	}
	if saved.Targets[0].Label != "example.com" {
		t.Fatalf("空 label 应当用 host 兜底，实际 %q", saved.Targets[0].Label)
	}
	// 兜底出来的 label 必须能被 LabelDerivedFromHost 认出来 ——
	// 面板的访客出口就是靠它把"公开字段 label == 私有值 host"抹掉的
	// （见 internal/server/guest.go 的 guestTargetLabel）。两处一旦分叉，
	// 地址就会顺着公开字段漏给访客，而页面上完全看不出来。
	if !LabelDerivedFromHost(saved.Targets[0].Label, saved.Targets[0].Host) {
		t.Errorf("兜底出来的 label %q 没被认成派生值（host %q）",
			saved.Targets[0].Label, saved.Targets[0].Host)
	}
	// 读回来的那一份也要认得出来：库里的 label 已经是地址了，读取路同样走
	// normalized()，所以判定必须对**存量数据**成立。
	targets, err := db.PingTargets(ctx)
	if err != nil {
		t.Fatalf("读取探测目标: %v", err)
	}
	if len(targets) != 1 || !LabelDerivedFromHost(targets[0].Label, targets[0].Host) {
		t.Fatalf("从库里读回来的目标也要认得出来: %+v", targets)
	}
}

// TestLabelDerivedFromHost 钉住判定本身：它必须与 normalized() 的兜底**严格同源**。
//
// 判定偏松的代价是"访客那边少个名字"（前端回落到「目标 #id」），偏紧的代价是
// "地址漏给访客" —— 两个方向不对称，所以判定宁可宽，但宽也有边界：
// 与 host 无关的名字一个都不能被误判成派生值（那会把用户自己起的名字也抹掉）。
func TestLabelDerivedFromHost(t *testing.T) {
	longHost := strings.Repeat("a", protocol.MaxPingLabelLen) + "b.example.com"

	cases := []struct {
		name  string
		label string
		host  string
		want  bool
	}{
		{"留空时 normalized 的兜底结果", derivedLabel("nas.home.lan"), "nas.home.lan", true},
		{"空 label 原样传进来（还没兜底）", "", "nas.home.lan", false},
		{"超长 host 被截断后的那一段", derivedLabel(longHost), longHost, true},
		{"用户自己起的名字", "内网 NAS", "nas.home.lan", false},
		{"名字与地址只是碰巧有点像", "nas.home", "nas.home.lan", false},
		{"host 为空（没有可派生的东西）", "随便", "", false},
		{"host 为空且 label 也为空", "", "", false},
		// 用户故意拿地址当名字：也判成派生。抹掉的代价只是名字没了，
		// 而漏掉的代价是把地址发给了不该看到它的人 —— 方向不对称。
		{"用户把 label 写成与 host 一样", "1.1.1.1", "1.1.1.1", true},
	}
	for _, tc := range cases {
		if got := LabelDerivedFromHost(tc.label, tc.host); got != tc.want {
			t.Errorf("%s：LabelDerivedFromHost(%q, %q) = %v，期望 %v",
				tc.name, tc.label, tc.host, got, tc.want)
		}
	}

	// 判定认的必须就是 normalized() 真正填进去的那个值（同一套去空白 + 截断）。
	// 少了这一步，"两处各写一遍、慢慢分叉"就还是会溜过去。
	for _, host := range []string{"nas.home.lan", longHost, "  1.1.1.1  "} {
		normalized := PingTarget{Type: protocol.PingTypeTCP, Host: host, Port: 80}.normalized()
		if !LabelDerivedFromHost(normalized.Label, normalized.Host) {
			t.Errorf("normalized() 对 host %q 填出来的 label %q 没被认出来",
				host, normalized.Label)
		}
	}
}

func TestSetPingSettingsRejectsInvalid(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()
	longHost := ""
	for i := 0; i < protocol.MaxPingHostLen+1; i++ {
		longHost += "a"
	}
	longLabel := ""
	for i := 0; i < protocol.MaxPingLabelLen+1; i++ {
		longLabel += "字"
	}

	tooMany := make([]PingTarget, protocol.MaxPingTargets+1)
	for i := range tooMany {
		tooMany[i] = PingTarget{Type: protocol.PingTypeTCP, Host: fmt.Sprintf("10.0.0.%d", i), Port: 80, Enabled: true}
	}

	cases := []struct {
		name     string
		targets  []PingTarget
		interval int
	}{
		{"类型非法", []PingTarget{{Type: "udp", Host: "1.1.1.1"}}, 60},
		{"类型为空", []PingTarget{{Host: "1.1.1.1"}}, 60},
		{"tcp 缺端口", []PingTarget{{Type: protocol.PingTypeTCP, Host: "1.1.1.1"}}, 60},
		{"端口越界", []PingTarget{{Type: protocol.PingTypeTCP, Host: "1.1.1.1", Port: 65536}}, 60},
		{"端口为负", []PingTarget{{Type: protocol.PingTypeTCP, Host: "1.1.1.1", Port: -1}}, 60},
		{"主机为空", []PingTarget{{Type: protocol.PingTypeTCP, Host: "  ", Port: 80}}, 60},
		{"主机过长", []PingTarget{{Type: protocol.PingTypeTCP, Host: longHost, Port: 80}}, 60},
		{"主机含空格", []PingTarget{{Type: protocol.PingTypeTCP, Host: "1.1.1.1 x", Port: 80}}, 60},
		{"名称过长", []PingTarget{{Label: longLabel, Type: protocol.PingTypeTCP, Host: "1.1.1.1", Port: 80}}, 60},
		{"目标过多", tooMany, 60},
		{"间隔过小", []PingTarget{{Type: protocol.PingTypeTCP, Host: "1.1.1.1", Port: 80}}, 9},
		{"间隔过大", []PingTarget{{Type: protocol.PingTypeTCP, Host: "1.1.1.1", Port: 80}}, 3601},
		{"间隔为 0", []PingTarget{{Type: protocol.PingTypeTCP, Host: "1.1.1.1", Port: 80}}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := db.SetPingSettings(ctx, tc.targets, tc.interval)
			if err == nil {
				t.Fatalf("非法输入被接受: %+v interval=%d", tc.targets, tc.interval)
			}
			if !errors.Is(err, ErrInvalidPing) {
				t.Fatalf("错误应当包装 ErrInvalidPing（服务端据此回 400），实际 %v", err)
			}
		})
	}

	// 恰好到上限应当接受。
	atLimit := make([]PingTarget, protocol.MaxPingTargets)
	for i := range atLimit {
		atLimit[i] = PingTarget{Type: protocol.PingTypeTCP, Host: fmt.Sprintf("10.0.0.%d", i), Port: 80, Enabled: true}
	}
	if _, err := db.SetPingSettings(ctx, atLimit, protocol.MaxPingIntervalSec); err != nil {
		t.Fatalf("%d 个目标应当被接受: %v", protocol.MaxPingTargets, err)
	}
	if _, err := db.SetPingSettings(ctx, nil, protocol.MinPingIntervalSec); err != nil {
		t.Fatalf("清空目标应当被接受: %v", err)
	}
	empty, err := db.PingTargets(ctx)
	if err != nil || len(empty) != 0 {
		t.Fatalf("清空后应当读到空列表: %+v (err=%v)", empty, err)
	}
	// 清空必须真的写成 []（而不是留下一个 null）。
	raw, ok, err := db.GetSetting(ctx, KeyPingTargets)
	if err != nil || !ok {
		t.Fatalf("读取原始设置: %v ok=%v", err, ok)
	}
	if raw != "[]" {
		t.Fatalf("清空后应当存成 []，实际 %q", raw)
	}
}

// 设置写入必须落库（不是只改了内存）。
func TestPingSettingsRoundTripThroughDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "probe.db")
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("打开: %v", err)
	}
	want := []PingTarget{
		{Label: "Cloudflare", Type: protocol.PingTypeTCP, Host: "1.1.1.1", Port: 443, Enabled: true},
		{Label: "内网", Type: protocol.PingTypeICMP, Host: "2001:db8::1", Enabled: false},
	}
	if _, err := db.SetPingSettings(ctx, want, 120); err != nil {
		t.Fatalf("保存: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("关闭: %v", err)
	}

	db2, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("重新打开: %v", err)
	}
	defer func() { _ = db2.Close() }()
	settings, err := db2.PingSettings(ctx)
	if err != nil {
		t.Fatalf("读取: %v", err)
	}
	if settings.IntervalSec != 120 || len(settings.Targets) != 2 {
		t.Fatalf("读回设置不对: %+v", settings)
	}
	if settings.Targets[0].Label != "Cloudflare" || settings.Targets[0].Port != 443 ||
		!settings.Targets[0].Enabled {
		t.Fatalf("第一个目标不对: %+v", settings.Targets[0])
	}
	if settings.Targets[1].Enabled {
		t.Fatalf("停用状态没被保存: %+v", settings.Targets[1])
	}

	// JSON 形状必须与接口契约一致（前端按这些字段名解析）。
	encoded, err := json.Marshal(settings.Targets[0])
	if err != nil {
		t.Fatalf("序列化: %v", err)
	}
	for _, needle := range []string{`"id":`, `"label":"Cloudflare"`, `"type":"tcp"`, `"host":"1.1.1.1"`, `"port":443`, `"enabled":true`} {
		if !strings.Contains(string(encoded), needle) {
			t.Errorf("目标的 JSON 缺少 %s：%s", needle, encoded)
		}
	}
}
