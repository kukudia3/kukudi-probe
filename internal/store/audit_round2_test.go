package store

// 第二轮安全审计（SECURITY-AUDIT-ROUND2.md）里属于存储 / SQL / 迁移 / 并发的
// confirmed 条目在此逐条钉住。每条都写了"撤掉修复会红在哪一处"。

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 04-S-2：user_version 为负数时 migrate 越界 panic / 现在是一条能读懂的报错
// ---------------------------------------------------------------------------

func TestMigrateRejectsNegativeUserVersion(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "probe.db")

	// 一个"版本号是负数"的库：手工改库写错、头页损坏、或把别的软件的 SQLite
	// 文件当成本程序的库。本程序自己的 migrate 永远写正数，所以这不是正常路径。
	raw, err := sql.Open("sqlite", dsn(path, true))
	if err != nil {
		t.Fatalf("打开原始数据库: %v", err)
	}
	for _, stmt := range schemaV1 {
		if _, err := raw.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("执行 0001 建表语句: %v", err)
		}
	}
	if _, err := raw.ExecContext(ctx, `PRAGMA user_version = -1`); err != nil {
		t.Fatalf("设置负数版本号: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("关闭原始数据库: %v", err)
	}

	// 关键断言：**不能 panic**（越界崩溃会让 systemd 反复重启、日志里只有 Go 栈），
	// 而是一条说清"版本号是负数、库可能坏了"的错误。
	db, err := Open(ctx, path)
	if err == nil {
		_ = db.Close()
		t.Fatal("负数 schema 版本应当拒绝启动")
	}
	if !strings.Contains(err.Error(), "-1") {
		t.Errorf("错误消息里应当带上那个版本号本身，实际: %v", err)
	}

	// 拒绝启动不该顺手改库：版本号必须还是 -1（否则操作员再也看不出文件被谁动过）。
	raw2, err := sql.Open("sqlite", dsn(path, true))
	if err != nil {
		t.Fatalf("重新打开原始数据库: %v", err)
	}
	defer func() { _ = raw2.Close() }()
	var got int
	if err := raw2.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&got); err != nil {
		t.Fatalf("读取版本号: %v", err)
	}
	if got != -1 {
		t.Errorf("拒绝启动之后 user_version = %d，期望原样保留 -1", got)
	}
}

// ---------------------------------------------------------------------------
// 02·M3：0007 补的 ts 覆盖索引（三种库都要能过 + 索引真的被用上）
// ---------------------------------------------------------------------------

const pingIndexName = "idx_ping_samples_1m_ts"

// explainOpensRootPage 报告 EXPLAIN 的字节码里有没有一次 OpenRead 打在这个根页上。
//
// 为什么不用 EXPLAIN QUERY PLAN：这个驱动/SQLite 版本下它的文本不可信 ——
// 走索引的扫描照样打印成 SCAN（审计员的方法学提醒）。P2 是 b 树的根页号，
// 与 sqlite_master.rootpage 一比就是"这条计划到底开了哪棵树"。
func explainOpensRootPage(t *testing.T, db *DB, root int64) bool {
	t.Helper()
	rows, err := db.Reader().QueryContext(context.Background(), `
		EXPLAIN SELECT node_id, target_id, (ts - ?) / ? AS bucket,
			SUM(avg_ms * up_cnt), SUM(up_cnt), SUM(all_cnt)
		FROM `+TablePing1m+`
		WHERE ts >= ? AND ts < ?
		GROUP BY node_id, target_id, bucket
		ORDER BY node_id, target_id, bucket`,
		1, 60, 0, 1)
	if err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var (
		addr, p1, p2, p3 int64
		opcode           string
		p4               any
		p5               int64
		comment          any
	)
	for rows.Next() {
		if err := rows.Scan(&addr, &opcode, &p1, &p2, &p3, &p4, &p5, &comment); err != nil {
			t.Fatalf("扫描 EXPLAIN 结果: %v", err)
		}
		if opcode == "OpenRead" && p2 == root {
			return true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历 EXPLAIN 结果: %v", err)
	}
	return false
}

func pingIndexRootPage(t *testing.T, db *DB) int64 {
	t.Helper()
	var root int64
	err := db.Reader().QueryRowContext(context.Background(),
		`SELECT rootpage FROM sqlite_master WHERE type = 'index' AND name = ?`, pingIndexName).Scan(&root)
	if errors.Is(err, sql.ErrNoRows) {
		return 0
	}
	if err != nil {
		t.Fatalf("查询索引根页: %v", err)
	}
	return root
}

// seedPingRows 灌一段"够长、窗口只覆盖其中一小段"的数据：窗口相对表足够小，
// 代价模型才会真的选索引（几行的表上两种计划都能跑完，测不出区别）。
func seedPingRows(t *testing.T, db *DB, nodes, targets, days int) (start, end int64) {
	t.Helper()
	ctx := context.Background()
	end = time.Now().Unix()
	end -= end % 60
	start = end - int64(days)*24*3600

	buckets := make([]PingBucket, 0, nodes*targets*days*24*60)
	for n := 1; n <= nodes; n++ {
		for tg := 1; tg <= targets; tg++ {
			for ts := start; ts < end; ts += 60 {
				buckets = append(buckets, NewPingBucket(int64(n), int64(tg), ts, 12.5, 10, 30, 0))
			}
		}
	}
	// 分批写：一次全灌也行，分批更容易在失败时定位是哪一批。
	for len(buckets) > 0 {
		n := min(len(buckets), 20000)
		if err := db.UpsertPingBuckets(ctx, buckets[:n]); err != nil {
			t.Fatalf("灌探测数据: %v", err)
		}
		buckets = buckets[n:]
	}
	return start, end
}

func TestMigration0007AddsPingIndexUsedByOverviewQuery(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "probe.db")

	// 1) 全新库：0001~0007 一次跑完，索引必须在。
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	version, err := db.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if version != len(migrations) {
		t.Fatalf("schema 版本 = %d，期望 %d", version, len(migrations))
	}
	root := pingIndexRootPage(t, db)
	if root == 0 {
		t.Fatalf("%s 不存在：0007 没有跑（QueryOverviewPing 会退化成整表扫描）", pingIndexName)
	}

	// 2) 索引必须真的被那条查询用上（不是"建了但计划不选它"）。
	_, end := seedPingRows(t, db, 20, 3, 3)
	window := int64(3600)
	bucketSec := window / 10
	end -= end % bucketSec
	start := end - window
	if _, err := db.QueryOverviewPing(ctx, start, end, bucketSec, 10); err != nil {
		t.Fatalf("QueryOverviewPing: %v", err)
	}
	if !explainOpensRootPage(t, db, root) {
		t.Errorf("查询计划里没有打开索引 %s（根页 %d）—— 又退回整表扫描了", pingIndexName, root)
	}

	// 3) 重复打开（幂等）：版本不变、不重复建索引、不报错。
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	again, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("再次 Open: %v", err)
	}
	defer func() { _ = again.Close() }()
	if v, err := again.SchemaVersion(ctx); err != nil || v != version {
		t.Fatalf("重复打开后版本 = %d（err=%v），期望 %d", v, err, version)
	}
	if got := pingIndexRootPage(t, again); got != root {
		t.Errorf("重复打开后索引根页 = %d，期望 %d（不该被重建）", got, root)
	}
}

// 老库升级路径：v6（0006 跑完、没有 0007）→ Open → 补上索引。
//
// 直接新建的库是 0001~0007 一次跑完的，盖不住"老部署升级"这条路。
func TestMigration0007UpgradesExistingV6Database(t *testing.T) {
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
	for _, m := range migrations[1:6] { // 0002..0006，刻意不含 0007
		for _, stmt := range m.stmts {
			if _, err := raw.ExecContext(ctx, stmt); err != nil {
				t.Fatalf("执行 %s 语句: %v", m.name, err)
			}
		}
	}
	// 老库里已经有探测数据（升级时索引要建在它们上面）。
	if _, err := raw.ExecContext(ctx,
		`INSERT INTO ping_samples_1m (node_id, target_id, ts, avg_ms, min_ms, max_ms, loss_pct, up_cnt, all_cnt)
		 VALUES (1, 1, 100, 12.5, 10, 30, 0, 100, 100)`); err != nil {
		t.Fatalf("插入老数据: %v", err)
	}
	if _, err := raw.ExecContext(ctx, `PRAGMA user_version = 6`); err != nil {
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

	if v, err := db.SchemaVersion(ctx); err != nil || v != len(migrations) {
		t.Fatalf("升级后版本 = %d（err=%v），期望 %d", v, err, len(migrations))
	}
	if pingIndexRootPage(t, db) == 0 {
		t.Fatalf("老库升级后 %s 不存在", pingIndexName)
	}
	// 老数据还在，而且查询能正常跑。
	out, err := db.QueryOverviewPing(ctx, 0, 200, 60, 10)
	if err != nil {
		t.Fatalf("升级后查询: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("升级后探测数据 = %+v，期望 1 个节点", out)
	}
}

// ---------------------------------------------------------------------------
// 04-X-1：UpsertPingBuckets 的防御性校验（不整批失败、每个节点的目标数有上限）
// ---------------------------------------------------------------------------

// 坏行（NaN / 非法 id）以前会把整批毒掉：NaN 被 SQLite 写成 NULL，撞上
// avg_ms REAL NOT NULL 之后整个事务回滚 —— 一条坏数据让所有节点这一分钟的
// 探测结果一起丢。现在跳过它，其余照写。
func TestUpsertPingBucketsSkipsBadRowsInsteadOfPoisoningBatch(t *testing.T) {
	ctx := context.Background()
	db := openTemp(t)
	ts := time.Now().Unix() - time.Now().Unix()%60

	rows := []PingBucket{
		NewPingBucket(1, 1, ts, 12.5, 10, 30, 0),
		{NodeID: 1, TargetID: 2, TS: ts, AvgMS: math.NaN(), MinMS: 1, MaxMS: 2, Up: 100, All: 100},
		{NodeID: -1, TargetID: 3, TS: ts, AvgMS: 5, MinMS: 1, MaxMS: 6, Up: 100, All: 100},
		{NodeID: 1, TargetID: -2, TS: ts, AvgMS: 5, MinMS: 1, MaxMS: 6, Up: 100, All: 100},
		NewPingBucket(2, 1, ts, 20, 18, 22, 0),
	}
	if err := db.UpsertPingBuckets(ctx, rows); err != nil {
		t.Fatalf("坏行不该让整批失败: %v", err)
	}

	var n int
	if err := db.Reader().QueryRowContext(ctx, `SELECT count(*) FROM `+TablePing1m).Scan(&n); err != nil {
		t.Fatalf("统计行数: %v", err)
	}
	if n != 2 {
		t.Errorf("落库行数 = %d，期望 2（两条合法行，三条坏行被跳过）", n)
	}
	for _, want := range []struct{ node, target int64 }{{1, 1}, {2, 1}} {
		var got int
		if err := db.Reader().QueryRowContext(ctx,
			`SELECT count(*) FROM `+TablePing1m+` WHERE node_id = ? AND target_id = ?`,
			want.node, want.target).Scan(&got); err != nil {
			t.Fatalf("查询合法行: %v", err)
		}
		if got != 1 {
			t.Errorf("合法行 (node=%d,target=%d) 没写进去", want.node, want.target)
		}
	}
}

// 存储层的兜底：一个节点无论被塞多少伪造 target_id，落库的目标数都被
// maxTargetsPerNode 卡住（审计 04-X-1；主防线在协议侧，见 03-A-1）。
func TestUpsertPingBucketsCapsTargetsPerNode(t *testing.T) {
	ctx := context.Background()
	db := openTemp(t)
	ts := time.Now().Unix() - time.Now().Unix()%60

	rows := make([]PingBucket, 0, 501)
	for i := 1; i <= 500; i++ {
		rows = append(rows, NewPingBucket(1, int64(i), ts, 10, 9, 11, 0))
	}
	// 另一个节点照常写：上限是按节点算的，不该连累别人。
	rows = append(rows, NewPingBucket(2, 1, ts, 10, 9, 11, 0))
	if err := db.UpsertPingBuckets(ctx, rows); err != nil {
		t.Fatalf("写入: %v", err)
	}

	count := func(node int64) int {
		t.Helper()
		var n int
		if err := db.Reader().QueryRowContext(ctx,
			`SELECT count(DISTINCT target_id) FROM `+TablePing1m+` WHERE node_id = ?`, node).Scan(&n); err != nil {
			t.Fatalf("统计目标数: %v", err)
		}
		return n
	}
	if got := count(1); got != maxTargetsPerNode {
		t.Errorf("节点 1 落库的目标数 = %d，期望被卡在 %d（伪造 target_id 不能无限膨胀这张表）",
			got, maxTargetsPerNode)
	}
	if got := count(2); got != 1 {
		t.Errorf("节点 2 落库的目标数 = %d，期望 1（上限按节点各算各的）", got)
	}

	// 正常规模不受影响：一个节点配上协议上限（16）个真实目标，一条都不掉。
	rows = rows[:0]
	for i := 1; i <= 16; i++ {
		rows = append(rows, NewPingBucket(3, int64(i), ts, 10, 9, 11, 0))
	}
	if err := db.UpsertPingBuckets(ctx, rows); err != nil {
		t.Fatalf("写入正常规模: %v", err)
	}
	if got := count(3); got != 16 {
		t.Errorf("节点 3 落库的目标数 = %d，期望 16（协议上限内的目标一个都不能少）", got)
	}
}

// testNewNode 造一个"其余字段都合法"的新节点输入：CreateNode 会校验上报间隔、
// 重置日这些字段，用例只关心名字/地区/额度时不必每次都写全。
func testNewNode(name string) NewNode {
	return NewNode{Name: name, IntervalSec: 1, TrafficWarnPct: 80, ResetDay: 1}
}

// ---------------------------------------------------------------------------
// 04-S-4：批次里混进已删节点时，不该让整批回滚
// ---------------------------------------------------------------------------

// 造几个节点，删掉其中一个，然后灌一个"合法、合法、已删"的批次。
//
// ⚠️ 复现口径（审计员踩过的坑）：CreateNode **自己就会往 node_runtime 插一行**，
// 所以判别必须看**内容**（last_seen 有没有被写成批次里的值），不能只看行数；
// 而且合法行要排在非法行**之前**，否则"整批回滚"与"一条都没插"在计数上不可区分。
func TestUpsertRuntimeSkipsDeletedNodeWithoutLosingBatch(t *testing.T) {
	ctx := context.Background()
	db := openTemp(t)
	now := time.Now()

	mk := func(name string) Node {
		t.Helper()
		node, _, err := db.CreateNode(ctx, testNewNode(name), now)
		if err != nil {
			t.Fatalf("创建节点 %s: %v", name, err)
		}
		return node
	}
	a, b, gone := mk("rt-a"), mk("rt-b"), mk("rt-gone")
	if err := db.DeleteNode(ctx, gone.ID); err != nil {
		t.Fatalf("删除节点: %v", err)
	}
	if _, err := db.Writer().ExecContext(ctx, `DELETE FROM node_runtime`); err != nil {
		t.Fatalf("清空运行态: %v", err)
	}

	rows := []RuntimeRow{
		{NodeID: a.ID, LastSeen: 1111, Status: "online"},
		{NodeID: b.ID, LastSeen: 2222, Status: "online"},
		{NodeID: gone.ID, LastSeen: 3333, Status: "online"},
	}
	if err := db.UpsertRuntime(ctx, rows, now); err != nil {
		t.Fatalf("批次里有一个已删节点，不该让整批失败: %v", err)
	}

	lastSeen := func(id int64) int64 {
		t.Helper()
		var got int64
		err := db.Reader().QueryRowContext(ctx, `SELECT last_seen FROM node_runtime WHERE node_id = ?`, id).Scan(&got)
		if errors.Is(err, sql.ErrNoRows) {
			return -1
		}
		if err != nil {
			t.Fatalf("读取节点 %d 的运行态: %v", id, err)
		}
		return got
	}
	if got := lastSeen(a.ID); got != 1111 {
		t.Errorf("节点 A 的运行态 = %d，期望 1111（已删节点不该拖垮整批）", got)
	}
	if got := lastSeen(b.ID); got != 2222 {
		t.Errorf("节点 B 的运行态 = %d，期望 2222", got)
	}
	if got := lastSeen(gone.ID); got != -1 {
		t.Errorf("已删节点不该有运行态行，实际 last_seen = %d", got)
	}
}

func TestUpsertAlertStatesSkipsDeletedNodeWithoutLosingBatch(t *testing.T) {
	ctx := context.Background()
	db := openTemp(t)
	now := time.Now()

	a, _, err := db.CreateNode(ctx, testNewNode("al-a"), now)
	if err != nil {
		t.Fatalf("创建节点 A: %v", err)
	}
	gone, _, err := db.CreateNode(ctx, testNewNode("al-gone"), now)
	if err != nil {
		t.Fatalf("创建节点 gone: %v", err)
	}
	if err := db.DeleteNode(ctx, gone.ID); err != nil {
		t.Fatalf("删除节点: %v", err)
	}

	rows := []AlertStateRow{
		{NodeID: a.ID, Rule: "offline", State: "firing", Since: 100, LastNotify: 100, NotifyCnt: 1},
		{NodeID: gone.ID, Rule: "offline", State: "firing", Since: 200, LastNotify: 200, NotifyCnt: 1},
	}
	if err := db.UpsertAlertStates(ctx, rows); err != nil {
		t.Fatalf("批次里有一个已删节点，不该让整批失败: %v", err)
	}

	var n int
	if err := db.Reader().QueryRowContext(ctx,
		`SELECT count(*) FROM alert_state WHERE node_id = ?`, a.ID).Scan(&n); err != nil {
		t.Fatalf("统计告警状态: %v", err)
	}
	if n != 1 {
		t.Errorf("合法节点的告警状态 = %d 行，期望 1 行（已删节点不该拖垮整批）", n)
	}
}

// ---------------------------------------------------------------------------
// 04-S-3：UpdateNodeIfUnchanged 的并发判据（只发现、不改变"后提交者赢"）
// ---------------------------------------------------------------------------

func TestUpdateNodeIfUnchangedDetectsConcurrentEdit(t *testing.T) {
	ctx := context.Background()
	db := openTemp(t)
	now := time.Now()

	node, _, err := db.CreateNode(ctx, func() NewNode {
		in := testNewNode("cas-01")
		in.Region = "HK"
		in.TrafficLimit = 1000
		return in
	}(), now)
	if err != nil {
		t.Fatalf("创建节点: %v", err)
	}

	// 会话 A 读到的快照（编辑框里那份）。
	snapshot, err := db.NodeByID(ctx, node.ID)
	if err != nil {
		t.Fatalf("NodeByID: %v", err)
	}

	// 会话 B 先保存：改地区、改额度。
	byB := snapshot
	byB.Region = "SG"
	byB.TrafficLimit = 2000
	if err := db.UpdateNode(ctx, byB, now.Add(time.Second)); err != nil {
		t.Fatalf("会话 B 保存: %v", err)
	}

	// 会话 A 用**陈旧快照**保存：必须被判成 stale，而且一个字段都没写进去。
	stale := snapshot
	stale.Region = "JP"
	stale.TrafficLimit = 999
	err = db.UpdateNodeIfUnchanged(ctx, stale, snapshot.UpdatedAt, now.Add(2*time.Second))
	if !errors.Is(err, ErrNodeStale) {
		t.Fatalf("并发修改应当返回 ErrNodeStale，实际 %v", err)
	}
	got, err := db.NodeByID(ctx, node.ID)
	if err != nil {
		t.Fatalf("读回: %v", err)
	}
	if got.Region != "SG" || got.TrafficLimit != 2000 {
		t.Errorf("被判 stale 的那次写入不该落库：%+v", got)
	}

	// 用**当前**的 updated_at 保存：正常写入，与 UpdateNode 完全一样。
	fresh := got
	fresh.Region = "JP"
	if err := db.UpdateNodeIfUnchanged(ctx, fresh, got.UpdatedAt, now.Add(3*time.Second)); err != nil {
		t.Fatalf("没有并发修改时应当正常写入: %v", err)
	}
	got, err = db.NodeByID(ctx, node.ID)
	if err != nil {
		t.Fatalf("再读回: %v", err)
	}
	if got.Region != "JP" {
		t.Errorf("正常写入没生效：%+v", got)
	}

	// 值完全没变时 RowsAffected 也可能为 0：那种情况下不能误报 stale。
	if err := db.UpdateNodeIfUnchanged(ctx, got, got.UpdatedAt, now.Add(4*time.Second)); err != nil {
		t.Errorf("内容没变的保存应当成功（不能因为 RowsAffected=0 就报 stale）: %v", err)
	}

	// 节点不存在时仍然是 ErrNodeNotFound，而不是 stale。
	ghost := got
	ghost.ID = 99999
	if err := db.UpdateNodeIfUnchanged(ctx, ghost, 0, now); !errors.Is(err, ErrNodeNotFound) {
		t.Errorf("不存在的节点应当返回 ErrNodeNotFound，实际 %v", err)
	}
}

// 幂等性没被防御性校验改掉：同一 (node,target,ts) 仍然覆盖而不是新增。
func TestUpsertPingBucketsStillOverwritesSameBucket(t *testing.T) {
	ctx := context.Background()
	db := openTemp(t)
	ts := time.Now().Unix() - time.Now().Unix()%60

	if err := db.UpsertPingBuckets(ctx, []PingBucket{NewPingBucket(1, 1, ts, 12.5, 10, 30, 0)}); err != nil {
		t.Fatalf("首次写入: %v", err)
	}
	if err := db.UpsertPingBuckets(ctx, []PingBucket{NewPingBucket(1, 1, ts, 99.5, 90, 100, 25)}); err != nil {
		t.Fatalf("重复写入: %v", err)
	}

	var (
		n     int
		avgMS float64
	)
	if err := db.Reader().QueryRowContext(ctx,
		`SELECT count(*), max(avg_ms) FROM `+TablePing1m).Scan(&n, &avgMS); err != nil {
		t.Fatalf("读回: %v", err)
	}
	if n != 1 || avgMS != 99.5 {
		t.Errorf("同一 (node,target,ts) 应当覆盖而不是新增：行数=%d avg=%v", n, avgMS)
	}
}
