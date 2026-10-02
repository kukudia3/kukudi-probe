package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

var wantTables = []string{
	"settings", "nodes", "node_runtime", "traffic_daily",
	"samples_10s", "samples_1m", "ping_samples_1m", "alert_state", "sessions", "audit_log",
}

func openTemp(t *testing.T) *DB {
	t.Helper()
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "probe.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestOpenCreatesSchemaAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "probe.db")

	db, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("首次 Open: %v", err)
	}
	version, err := db.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if version != len(migrations) {
		t.Fatalf("schema 版本 = %d，期望 %d", version, len(migrations))
	}
	assertTables(t, db)

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// 再打开一次：迁移必须可重入，版本不变、不重复建表。
	again, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("再次 Open: %v", err)
	}
	defer func() { _ = again.Close() }()
	version2, err := again.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("再次 SchemaVersion: %v", err)
	}
	if version2 != version {
		t.Fatalf("重复打开后 schema 版本变成 %d，期望 %d", version2, version)
	}
	assertTables(t, again)
}

// TestOpenDoesNotChmodExistingDir 守住「只在本程序新建目录时才收紧权限」。
//
// dir 完全来自操作员的 --data-dir：指向一个已经存在的目录（部署脚本建好的
// /var/lib/probe-server、或自己挑的 ~/probe-data）时，本程序不该改写它的权限。
//
// 为什么权限值分平台：Windows 的 os.Chmod 只在"只读/可写"之间切（量出来就是
// 0555/0777 两个值），而 Linux 上 0555 的目录连 SQLite 的 -wal 都建不出来。
func TestOpenDoesNotChmodExistingDir(t *testing.T) {
	dir := t.TempDir()
	want := os.FileMode(0o755)
	if runtime.GOOS == "windows" {
		want = 0o555
	}
	if err := os.Chmod(dir, want); err != nil {
		t.Fatalf("设置目录权限: %v", err)
	}
	if got := dirPerm(t, dir); got != want {
		t.Fatalf("前置条件不成立：目录权限 = %#o，期望 %#o", got, want)
	}

	db, err := Open(context.Background(), filepath.Join(dir, "probe.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	if got := dirPerm(t, dir); got != want {
		t.Errorf("已存在的数据目录权限被改成了 %#o，期望原样保留 %#o（本程序不该动不是自己建的目录）",
			got, want)
	}
}

// TestOpenRejectsCurrentDirAndRoot 守住"明显危险的 --data-dir 直接拒启动"。
//
// `--data-dir .` 是常见写法，而 filepath.Join(".", "probe.db") 就是 "probe.db"、
// filepath.Dir("probe.db") 就是 "."：以前它会把操作员的当前目录 chmod 成 0700，
// 现在拒启动（根目录同理）。同时钉住"新建目录仍然收紧到 0700"这个原行为没丢。
func TestOpenRejectsCurrentDirAndRoot(t *testing.T) {
	ctx := context.Background()

	// 临时目录当"当前目录"：万一将来这条拒启动被改坏（Open 真的去建库了），
	// 建出来的文件也只会落在临时目录里，不会污染仓库。
	t.Chdir(t.TempDir())

	if _, err := Open(ctx, "probe.db"); err == nil {
		t.Error("--data-dir . 应当拒启动（会把当前目录当成数据目录）")
	} else {
		t.Logf("当前目录被拒: %v", err)
	}
	if _, err := Open(ctx, filepath.Join(string(filepath.Separator), "probe.db")); err == nil {
		t.Error("--data-dir / 应当拒启动（会把库文件摊在根目录下）")
	} else {
		t.Logf("根目录被拒: %v", err)
	}

	// 正常的相对目录照旧可用（默认配置就是 --data-dir data）。
	nested := filepath.Join(t.TempDir(), "data")
	db, err := Open(ctx, filepath.Join(nested, "probe.db"))
	if err != nil {
		t.Fatalf("普通目录应当可用: %v", err)
	}
	defer func() { _ = db.Close() }()
	// 本程序新建的目录仍然是 0700（Windows 下量不出权限位，跳过这一条）。
	if runtime.GOOS != "windows" {
		if got := dirPerm(t, nested); got != 0o700 {
			t.Errorf("新建的数据目录权限 = %#o，期望 0700", got)
		}
	}
}

// dirPerm 返回目录的权限位。
func dirPerm(t *testing.T, dir string) os.FileMode {
	t.Helper()
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat %s: %v", dir, err)
	}
	return info.Mode().Perm()
}

// 0002 是"给已发布的库加列"的迁移，只有真的从 v1 库升级上来才算测到。
//
// 手工造一个 v1 库（只跑 0001、user_version=1），再让 Open 去补迁移：
// 直接新建的库是 0001+0002 一次跑完的，盖不住"老库缺列"这条路径。
func TestMigration0002UpgradesExistingV1Database(t *testing.T) {
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
	// 老版本里已经存在的节点：升级后必须拿到默认值，而不是 NULL 或报错。
	if _, err := raw.ExecContext(ctx,
		`INSERT INTO nodes (name, token_hash, created_at, updated_at) VALUES (?, ?, ?, ?)`,
		"legacy-01", []byte{0x01, 0x02}, 100, 100); err != nil {
		t.Fatalf("插入老节点: %v", err)
	}
	if _, err := raw.ExecContext(ctx, `PRAGMA user_version = 1`); err != nil {
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
	// 这里只断言"补到了最新版本"，因为后面每加一条迁移都会让这个数字变大；
	// 具体每一版做了什么由各自的用例（0002/0003）负责。
	if version != len(migrations) || version < 2 {
		t.Fatalf("升级后版本 = %d，期望 %d（应当补完 0002 及以后的全部迁移）", version, len(migrations))
	}

	legacy, err := db.NodeByID(ctx, 1)
	if err != nil {
		t.Fatalf("读取老节点: %v", err)
	}
	if legacy.PriceCents != 0 || legacy.Currency != "" || legacy.BillingMonths != 0 {
		t.Fatalf("老节点应当默认是「没填价格」的状态: %+v", legacy)
	}

	// 新列能写能读（ALTER TABLE 真的生效了）。
	legacy.PriceCents = 7121
	legacy.Currency = "CNY"
	legacy.BillingMonths = 12
	if err := db.UpdateNode(ctx, legacy, time.Now()); err != nil {
		t.Fatalf("更新老节点的价格: %v", err)
	}
	got, err := db.NodeByID(ctx, 1)
	if err != nil {
		t.Fatalf("读回老节点: %v", err)
	}
	if got.PriceCents != 7121 || got.Currency != "CNY" || got.BillingMonths != 12 {
		t.Fatalf("升级后写价格失败: %+v", got)
	}
}

func assertTables(t *testing.T, db *DB) {
	t.Helper()
	for _, name := range wantTables {
		var n int
		err := db.Reader().QueryRowContext(context.Background(),
			`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&n)
		if err != nil {
			t.Fatalf("查询表 %s 失败: %v", name, err)
		}
		if n != 1 {
			t.Errorf("表 %s 不存在", name)
		}
	}
}

func TestWALEnabled(t *testing.T) {
	db := openTemp(t)
	var mode string
	if err := db.Reader().QueryRowContext(context.Background(), "PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("读取 journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode = %q，期望 wal", mode)
	}
}

// STRICT 表应当拒绝类型错误的值，而不是悄悄转换。
func TestStrictTableRejectsWrongType(t *testing.T) {
	db := openTemp(t)
	_, err := db.Writer().ExecContext(context.Background(),
		`INSERT INTO nodes (name, token_hash, created_at, updated_at) VALUES (?, ?, ?, ?)`,
		"test", []byte{0x01}, "不是整数", 1)
	if err == nil {
		t.Fatal("向 INTEGER 列写字符串应当报错")
	}
}

func TestNodeRuntimeCascadeDelete(t *testing.T) {
	ctx := context.Background()
	db := openTemp(t)

	res, err := db.Writer().ExecContext(ctx,
		`INSERT INTO nodes (name, token_hash, created_at, updated_at) VALUES (?, ?, ?, ?)`,
		"hk-01", []byte{0xAA, 0xBB}, 100, 100)
	if err != nil {
		t.Fatalf("插入节点: %v", err)
	}
	nodeID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("LastInsertId: %v", err)
	}

	if _, err := db.Writer().ExecContext(ctx,
		`INSERT INTO node_runtime (node_id, status, rx_total, tx_total) VALUES (?, ?, ?, ?)`,
		nodeID, "online", 1234, 5678); err != nil {
		t.Fatalf("插入运行态: %v", err)
	}

	if _, err := db.Writer().ExecContext(ctx, `DELETE FROM nodes WHERE id = ?`, nodeID); err != nil {
		t.Fatalf("删除节点: %v", err)
	}

	var n int
	if err := db.Reader().QueryRowContext(ctx,
		`SELECT count(*) FROM node_runtime WHERE node_id = ?`, nodeID).Scan(&n); err != nil {
		t.Fatalf("查询运行态: %v", err)
	}
	if n != 0 {
		t.Fatalf("删除节点后 node_runtime 仍有 %d 行：外键级联未生效", n)
	}
}

func TestSampleRoundTrip(t *testing.T) {
	ctx := context.Background()
	db := openTemp(t)

	if _, err := db.Writer().ExecContext(ctx,
		`INSERT INTO samples_10s (node_id, ts, cpu_avg, cpu_max, mem_avg, mem_max, up_cnt, all_cnt)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		1, 1_700_000_000, 12.5, 40.0, 33.0, 35.5, 10, 10); err != nil {
		t.Fatalf("插入历史桶: %v", err)
	}

	var cpuAvg, cpuMax float64
	err := db.Reader().QueryRowContext(ctx,
		`SELECT cpu_avg, cpu_max FROM samples_10s WHERE node_id = ? AND ts = ?`, 1, 1_700_000_000).
		Scan(&cpuAvg, &cpuMax)
	if err != nil {
		t.Fatalf("读取历史桶: %v", err)
	}
	if cpuAvg != 12.5 || cpuMax != 40.0 {
		t.Fatalf("读回的值 = %v / %v", cpuAvg, cpuMax)
	}
}

func TestOpenRejectsEmptyPath(t *testing.T) {
	if _, err := Open(context.Background(), "   "); err == nil {
		t.Fatal("空路径应当报错")
	}
}
