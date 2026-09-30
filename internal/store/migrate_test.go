package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// 0006 是**改数据**的迁移（0002~0005 都只加列/加表），只有真的从 v5 库升级上来
// 才算测到：直接新建的库是 0001~0006 一次跑完的，nodes 表里根本没有存量数据，
// 那条 UPDATE 跑了个寂寞也照样"通过"。
//
// 口径：旧代码按「用户填的 GB × 1024³」存，新代码按「× 10⁹」存。迁移要把字节数
// 换算回去，**保住用户当初填的那个数字** —— 这是用户在界面上唯一能核对的东西，
// 也是 80% 预警算得准的前提。
func TestMigration0006ConvertsTrafficLimitToDecimalGB(t *testing.T) {
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
	for _, m := range migrations[1:5] { // 0002 / 0003 / 0004 / 0005
		for _, stmt := range m.stmts {
			if _, err := raw.ExecContext(ctx, stmt); err != nil {
				t.Fatalf("执行 %s 语句: %v", m.name, err)
			}
		}
	}

	const gib = 1024 * 1024 * 1024
	// 三行老数据：
	//   1) 用户当初填 2000（GB），旧口径存成 2000 GiB；
	//   2) 0 = 不限，迁移必须原样留着（不能变成 NULL，也不能变成别的数）；
	//   3) 用 API 直接写进去的非整 GB（旧口径 1.5 GiB 对应前端能显示的 1.5 GB）。
	for _, row := range []struct {
		name    string
		limit   int64
		tokenHi byte
	}{
		{"legacy-2000gb", 2000 * gib, 0x01},
		{"legacy-unlimited", 0, 0x02},
		{"legacy-1500gb", 3 * gib / 2, 0x03},
	} {
		if _, err := raw.ExecContext(ctx,
			`INSERT INTO nodes (name, token_hash, created_at, updated_at, traffic_limit) VALUES (?, ?, 100, 100, ?)`,
			row.name, []byte{row.tokenHi, 0xFF}, row.limit); err != nil {
			t.Fatalf("插入老节点 %s: %v", row.name, err)
		}
	}
	if _, err := raw.ExecContext(ctx, `PRAGMA user_version = 5`); err != nil {
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
	if version != len(migrations) || version < 6 {
		t.Fatalf("升级后版本 = %d，期望 %d（0006 及以后的全部迁移）", version, len(migrations))
	}

	// 用户当初填的 2000（GB）必须原样读得回来：2000 × 10⁹，而不是 2000 × 1024³。
	// 输入框（app.js）按 1e9 反算，所以这一格显示的还是 2000。
	cases := []struct {
		name  string
		want  int64
		label string
	}{
		{"legacy-2000gb", 2000 * 1000 * 1000 * 1000, "2000 GB"},
		{"legacy-unlimited", 0, "不限"},
		{"legacy-1500gb", 1500 * 1000 * 1000, "1.5 GB（非整 GB）"},
	}
	for _, c := range cases {
		var got int64
		if err := db.Reader().QueryRowContext(ctx,
			`SELECT traffic_limit FROM nodes WHERE name = ?`, c.name).Scan(&got); err != nil {
			t.Fatalf("读取 %s 的 traffic_limit: %v", c.name, err)
		}
		if got != c.want {
			t.Errorf("%s（%s）迁移后 traffic_limit = %d，期望 %d", c.name, c.label, got, c.want)
		}
	}

	// 换算后的额度要能撑得住"80% 预警落在用户真实额度的 80%"：
	// 2000 GB 的 80% 是 1600 GB。
	var limit int64
	if err := db.Reader().QueryRowContext(ctx,
		`SELECT traffic_limit FROM nodes WHERE name = 'legacy-2000gb'`).Scan(&limit); err != nil {
		t.Fatalf("再次读取额度: %v", err)
	}
	if want := int64(1600 * 1000 * 1000 * 1000); limit*80/100 != want {
		t.Errorf("2000 GB 额度的 80%% = %d，期望 %d", limit*80/100, want)
	}
}
