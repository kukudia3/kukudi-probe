package store

import (
	"context"
	"path/filepath"
	"testing"
)

var wantTables = []string{
	"settings", "nodes", "node_runtime", "traffic_daily",
	"samples_10s", "samples_1m", "alert_state", "sessions", "audit_log",
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
