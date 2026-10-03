package store_test

// 第三轮验证（D:\DEEPSEEK\_audit\ROUND3-VERIFY-2.md）⑤：ping_samples_1m 的
// **磁盘增长真实量级**。
//
// 第二遍审计（03-A-1 / 09-F1）对这条只有结构上界："单连接 4800 行/分钟、
// 20 条连接约 9.6 万行/分钟、8 天 60~90 GB"，并明确写着"全是静态推算"、
// "SQLite 单写者能否吃下约 1600 行/秒"**未实测**。这里把两件事量出来：
//
//  1. 一行占多少字节（含索引与页开销，用真实文件增长量）；
//  2. 存储层一次能吞多少行（96,000 行要多久写进去）。
//
// 为什么必须用真实文件大小而不是"列宽相加"：SQLite 的页填充率、B-tree 分裂、
// WAL 复用都会改变实际占用；本用例每次都先做 `wal_checkpoint(TRUNCATE)`
// 再 stat 主库，量到的是"净落盘字节"。
//
// ⚠️ 两条用例都是**测量**，断言只钉"行真的写进去了 + 量级合理"，
// 具体数字由 t.Logf 打到 -v 输出里，报告里的表格就是从这些输出抄的。

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"probe/internal/store"
)

// fileSize 返回文件大小（不存在记 0）。
func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi.Size()
}

// checkpointAndSizes 强制把 WAL 折回主库，返回 (主库字节, WAL 字节)。
func checkpointAndSizes(t *testing.T, db *store.DB, path string) (int64, int64) {
	t.Helper()
	if _, err := db.Writer().ExecContext(context.Background(), `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatalf("wal_checkpoint: %v", err)
	}
	return fileSize(t, path), fileSize(t, path+"-wal")
}

// pingRows 数一个节点在 ping_samples_1m 里的行数。
func pingRows(t *testing.T, db *store.DB, nodeID int64) int64 {
	t.Helper()
	var n int64
	if err := db.Reader().QueryRowContext(context.Background(),
		`SELECT count(*) FROM ping_samples_1m WHERE node_id = ?`, nodeID).Scan(&n); err != nil {
		t.Fatalf("统计行数: %v", err)
	}
	return n
}

// pingBatch 造一批 (node, 32 个目标 × minutes 个分钟) 的桶。
//
// 为什么按 32 分组：存储层有一次调用最多 32 个目标/节点的兜底
// （store/ping.go 的 maxTargetsPerNode），一次调用塞超过 32 个不同 target_id
// 会被静默丢掉 —— 那会让"字节/行"这个测量失真。
func pingBatch(nodeID int64, baseTS int64, minutes int) []store.PingBucket {
	out := make([]store.PingBucket, 0, 32*minutes)
	for target := int64(1); target <= 32; target++ {
		for m := 0; m < minutes; m++ {
			out = append(out, store.NewPingBucket(nodeID, target, baseTS+int64(m)*60,
				12.5, 10, 20, 0))
		}
	}
	return out
}

// TestPingBucketDiskGrowthPerRow 量"一行 ping_samples_1m 占多少净字节"。
//
// 方法：空库 → 写 12,800 行（32 目标 × 400 分钟，4 次调用）→ checkpoint → stat。
func TestPingBucketDiskGrowthPerRow(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "probe.db")
	db, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	defer func() { _ = db.Close() }()

	pageSize, autoCkpt := 0, 0
	if err := db.Reader().QueryRowContext(ctx, `PRAGMA page_size`).Scan(&pageSize); err != nil {
		t.Fatalf("读 page_size: %v", err)
	}
	if err := db.Reader().QueryRowContext(ctx, `PRAGMA wal_autocheckpoint`).Scan(&autoCkpt); err != nil {
		t.Fatalf("读 wal_autocheckpoint: %v", err)
	}

	before, _ := checkpointAndSizes(t, db, path)

	const nodeID = 1001
	const perCall = 32 * 100 // 32 目标 × 100 分钟 = 3200 行/次
	const calls = 4
	const wantRows = perCall * calls

	base := time.Now().Add(-time.Duration(wantRows) * time.Minute).Unix()
	base -= base % 60
	start := time.Now()
	for i := 0; i < calls; i++ {
		batch := pingBatch(nodeID, base+int64(i*100)*60, 100)
		if len(batch) != perCall {
			t.Fatalf("批次行数 = %d，期望 %d", len(batch), perCall)
		}
		if err := db.UpsertPingBuckets(ctx, batch); err != nil {
			t.Fatalf("写入第 %d 批: %v", i+1, err)
		}
	}
	elapsed := time.Since(start)

	after, wal := checkpointAndSizes(t, db, path)
	rows := pingRows(t, db, nodeID)
	if rows != wantRows {
		t.Fatalf("落库行数 = %d，期望 %d（测量失真：有行被 32 目标上限丢掉了）", rows, wantRows)
	}

	grew := after - before
	bytesPerRow := float64(grew) / float64(wantRows)
	pagesPerRow := float64(grew) / float64(pageSize) / float64(wantRows)

	t.Logf("① 字节/行：主库 +%d B / %d 行 = **%.1f B/行**（page_size=%d ⇒ %.4f 页/行）；"+
		"写出耗时 %s（%.0f 行/秒）；checkpoint 后 -wal=%d B；wal_autocheckpoint=%d 页(%d KiB)",
		grew, wantRows, bytesPerRow, pageSize, pagesPerRow,
		elapsed.Round(time.Millisecond), float64(wantRows)/elapsed.Seconds(), wal,
		autoCkpt, autoCkpt*pageSize/1024)

	// 只钉"真的落盘了 + 量级不是荒唐值"：具体数字随 SQLite 版本/页大小浮动，
	// 钉死会变成一条与实现无关的脆弱断言。
	if grew <= 0 {
		t.Fatal("主库一个字节都没长：行没有真的落盘")
	}
	if bytesPerRow < 8 || bytesPerRow > 512 {
		t.Fatalf("字节/行 = %.1f，超出合理区间 [8, 512]（测量方法或存储实现变了）", bytesPerRow)
	}
}

// TestPingBucketPreFixAmplificationRate 量"修复前那条放大路径"在存储层能跑多快。
//
// 修复前的形态（报告 03-A-1）：一枚有效 Token 开 20 条连接、每帧 16 个新
// target_id、每秒 5 帧 ⇒ 约 96,000 行/分钟。报告没验证的是
// "SQLite 单写者能不能吃下这个速率"。
//
// 现在的存储层有一次调用最多 32 个目标/节点的兜底，所以要**分成 30 次调用**
// （32 目标 × 100 分钟 = 3200 行/次）才能写出 96,000 行 —— 这正是"修复前
// 没有这道兜底时会一次性灌进来"的量，只是被拆成了 30 个事务。
//
// 结论用法：把实测"行/秒"与放大路径需要的 1600 行/秒对比。
func TestPingBucketPreFixAmplificationRate(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "probe.db")
	db, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	defer func() { _ = db.Close() }()

	before, _ := checkpointAndSizes(t, db, path)

	const nodeID = 2002
	const total = 96000 // 报告里"一枚 Token 一分钟"的量级
	// 30 次调用 × 3200 行；每次的 target 集合都是 1..32（不同分钟不冲突），
	// 所以 32 目标上限不会丢行 —— 只是把"一秒钟灌进来"摊成 30 个事务。
	const calls = 30
	const perCall = total / calls
	base := time.Now().Add(-time.Duration(total/32) * time.Minute).Unix()
	base -= base % 60

	start := time.Now()
	for i := 0; i < calls; i++ {
		batch := pingBatch(nodeID, base+int64(i*100)*60, 100)
		if len(batch) != perCall {
			t.Fatalf("批次行数 = %d，期望 %d", len(batch), perCall)
		}
		if err := db.UpsertPingBuckets(ctx, batch); err != nil {
			t.Fatalf("写入第 %d 批: %v", i+1, err)
		}
	}
	elapsed := time.Since(start)
	after, wal := checkpointAndSizes(t, db, path)

	rows := pingRows(t, db, nodeID)
	if rows != total {
		t.Fatalf("落库行数 = %d，期望 %d", rows, total)
	}
	rate := float64(total) / elapsed.Seconds()
	grew := after - before

	t.Logf("② 放大路径写入速率：%d 行 / %s = **%.0f 行/秒**（放大路径需要 1600 行/秒）；"+
		"主库 +%d B（%.1f B/行）；-wal=%d B",
		total, elapsed.Round(time.Millisecond), rate, grew,
		float64(grew)/float64(total), wal)

	// "能持续多久"的判据：一个 60 秒窗口内必须写得完 96,000 行。
	if elapsed > 60*time.Second {
		t.Fatalf("写 96,000 行用了 %s > 60s ⇒ 这个速率撑不住（放大路径会积压）", elapsed)
	}
	// 反过来说也要成立：它确实写完了，而不是被静默丢掉。
	if rows != total {
		t.Fatalf("行数不对：%d", rows)
	}
	// 8 天稳态的算术（报告口径）：96000 行/分 × 60 × 24 × 8 天。
	perDay := float64(total) * 60 * 24
	t.Logf("   外推：%.0f 行/天/节点 × 8 天 = %.3g 行；按上面的字节/行 = %.1f GB/天 → 8 天 %.1f GB",
		perDay, perDay*8, perDay*float64(grew)/float64(total)/(1<<30),
		perDay*8*float64(grew)/float64(total)/(1<<30))
}
