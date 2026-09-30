package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// 0005 是"给已发布的库加一列"的迁移，只有真的从 v4 库升级上来才算测到。
//
// 与 0002/0003/0004 的用例同样的思路：手工造一个 v1+0002+0003+0004 的库
// （user_version=4），再让 Open 去补 0005 —— 直接新建的库是 0001~0005 一次跑完的，
// 盖不住"老库缺列"这条路径。
func TestMigration0005UpgradesExistingV4Database(t *testing.T) {
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
	for _, m := range migrations[1:4] { // 0002 / 0003 / 0004
		for _, stmt := range m.stmts {
			if _, err := raw.ExecContext(ctx, stmt); err != nil {
				t.Fatalf("执行 %s 语句: %v", m.name, err)
			}
		}
	}
	// 老库里已经有节点与它的运行态行：升级后这一行必须原样还在，
	// 并且 online_since 是"不在线"（0）而不是 NULL。
	if _, err := raw.ExecContext(ctx,
		`INSERT INTO nodes (id, name, token_hash, created_at, updated_at) VALUES (1, ?, ?, 100, 100)`,
		"legacy-01", []byte{0x01, 0x02}); err != nil {
		t.Fatalf("插入老节点: %v", err)
	}
	if _, err := raw.ExecContext(ctx,
		`INSERT INTO node_runtime (node_id, last_seen, status, updated_at) VALUES (1, 1000, 'online', 1000)`); err != nil {
		t.Fatalf("插入老运行态: %v", err)
	}
	if _, err := raw.ExecContext(ctx, `PRAGMA user_version = 4`); err != nil {
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
	if version != len(migrations) || version < 5 {
		t.Fatalf("升级后版本 = %d，期望 %d（0005_node_online_since 及以后的全部迁移）", version, len(migrations))
	}

	// 老行的默认值：0 = 不在线。用 0 而不是 NULL，读路径就不必再判一次空。
	var since int64
	if err := db.Reader().QueryRowContext(ctx,
		`SELECT online_since FROM node_runtime WHERE node_id = 1`).Scan(&since); err != nil {
		t.Fatalf("读取 online_since 列: %v", err)
	}
	if since != 0 {
		t.Fatalf("老行的 online_since = %d，期望 0（不在线）", since)
	}

	// 新列能写能读（ALTER TABLE 真的生效了，而不是只改了版本号）。
	now := time.Now()
	if err := db.UpsertRuntime(ctx, []RuntimeRow{{
		NodeID: 1, LastSeen: now.Unix(), Status: "online", OnlineSince: now.Add(-3 * time.Hour).Unix(),
	}}, now); err != nil {
		t.Fatalf("写入运行态: %v", err)
	}
	loaded, err := db.LoadRuntime(ctx)
	if err != nil {
		t.Fatalf("读取运行态: %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("运行态行数 = %d，期望 1", len(loaded))
	}
	if loaded[0].OnlineSince != now.Add(-3*time.Hour).Unix() {
		t.Fatalf("online_since 写读不一致: %d", loaded[0].OnlineSince)
	}
}

// online_since 要能真的落库、读回，并且"清零"（回到不在线）同样写得进去 ——
// 只写得进不清得掉的话，重启后会把一段早就结束的在线时长继续算下去。
func TestRuntimeOnlineSinceRoundTrip(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()
	now := time.Now()

	if _, _, err := db.CreateNode(ctx, validNewNode(), now); err != nil {
		t.Fatalf("创建节点: %v", err)
	}

	since := now.Add(-90 * time.Minute).Unix()
	row := RuntimeRow{NodeID: 1, LastSeen: now.Unix(), Status: "online", OnlineSince: since}
	if err := db.UpsertRuntime(ctx, []RuntimeRow{row}, now); err != nil {
		t.Fatalf("写入运行态: %v", err)
	}
	loaded, err := db.LoadRuntime(ctx)
	if err != nil {
		t.Fatalf("读取运行态: %v", err)
	}
	if len(loaded) != 1 || loaded[0].OnlineSince != since {
		t.Fatalf("online_since 没有落库: %+v", loaded)
	}

	// 节点掉线：起点清零，下一次启动不该接着算。
	row.OnlineSince = 0
	if err := db.UpsertRuntime(ctx, []RuntimeRow{row}, now.Add(time.Minute)); err != nil {
		t.Fatalf("清零运行态: %v", err)
	}
	loaded, err = db.LoadRuntime(ctx)
	if err != nil {
		t.Fatalf("再次读取运行态: %v", err)
	}
	if len(loaded) != 1 || loaded[0].OnlineSince != 0 {
		t.Fatalf("清零没有生效: %+v", loaded)
	}

	// 运行态落盘不该碰流量基线（Phase 7 的约定，见 UpsertRuntime 的注释）。
	var rxTotal, txTotal int64
	if err := db.Reader().QueryRowContext(ctx,
		`SELECT rx_total, tx_total FROM node_runtime WHERE node_id = 1`).Scan(&rxTotal, &txTotal); err != nil {
		t.Fatalf("读取基线: %v", err)
	}
	if rxTotal != 0 || txTotal != 0 {
		t.Fatalf("流量基线不该被运行态写入碰过: rx=%d tx=%d", rxTotal, txTotal)
	}
}
