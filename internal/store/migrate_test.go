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

// 0008 只加一列，但这一条要验的**不是**"列有没有建出来"，而是那条安全性论证：
// DEFAULT 0 = 双向 = 这个字段出现之前的行为 ⇒ 老库升级后一个字节都不差。
//
// 为什么必须从真的 v7 库升一次：直接新建的库是 0001~0008 一次跑完的，那条
// ALTER TABLE 加出来的列会立刻被 INSERT 填上调用方给的值 —— 老行拿到的是什么，
// 只有"先建表、再插数据、再把版本号停在 7"这一条路能证明。
func TestMigration0008AddsTrafficBillingDefaultingToBoth(t *testing.T) {
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
	for _, m := range migrations[1:7] { // 0002 … 0007（都不含 traffic_billing）
		for _, stmt := range m.stmts {
			if _, err := raw.ExecContext(ctx, stmt); err != nil {
				t.Fatalf("执行 %s 语句: %v", m.name, err)
			}
		}
	}
	// 一台"老"机器：这个库是在还没有 traffic_billing 这一列时建的。
	if _, err := raw.ExecContext(ctx, `
		INSERT INTO nodes (name, token_hash, created_at, updated_at, traffic_limit, traffic_warn_pct, reset_day)
		VALUES ('legacy-both', ?, 100, 100, ?, 80, 19)`,
		[]byte{0x11, 0xFF}, int64(500)*1000*1000*1000); err != nil {
		t.Fatalf("插入老节点: %v", err)
	}
	if _, err := raw.ExecContext(ctx, `PRAGMA user_version = 7`); err != nil {
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
	if version != len(migrations) {
		t.Fatalf("升级后版本 = %d，期望 %d", version, len(migrations))
	}

	// 列本身的形状：NOT NULL + DEFAULT 0。这两条是"老行天然是双向"的全部依据 ——
	// 少了 DEFAULT 的话 ALTER TABLE 在非空表上就必须给个值（迁移会直接失败），
	// 而少了 NOT NULL 则会给读路径留下 NULL（scanNode 会把它扫进 int 里报错）。
	rows, err := db.Reader().QueryContext(ctx, `PRAGMA table_info(nodes)`)
	if err != nil {
		t.Fatalf("读表结构: %v", err)
	}
	defer func() { _ = rows.Close() }()
	found := false
	for rows.Next() {
		var (
			cid       int
			name, typ string
			notNull   int
			dflt      sql.NullString
			pk        int
		)
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			t.Fatalf("扫表结构: %v", err)
		}
		if name != "traffic_billing" {
			continue
		}
		found = true
		if notNull != 1 {
			t.Errorf("traffic_billing 的 notnull = %d，期望 1", notNull)
		}
		if !dflt.Valid || dflt.String != "0" {
			t.Errorf("traffic_billing 的默认值 = %v，期望 0（双向）", dflt)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历表结构: %v", err)
	}
	if !found {
		t.Fatal("升级后 nodes 表里没有 traffic_billing 列")
	}

	// 老行读出来必须是 0（双向），其余字段一个都不许变。
	node := legacyNode(t, ctx, db, "legacy-both")
	if node.TrafficBilling != BillingBoth {
		t.Errorf("老节点升级后 traffic_billing = %d，期望 %d（双向）", node.TrafficBilling, BillingBoth)
	}
	if node.TrafficLimit != int64(500)*1000*1000*1000 || node.TrafficWarnPct != 80 || node.ResetDay != 19 {
		t.Errorf("老节点的其它字段被迁移改动了: %+v", node)
	}

	// "行为一个字节都不差"的直接判据：升级后的老节点算出来的已用量 = 收 + 发。
	const rx, tx = 700, 300
	if got := BilledBytes(node.TrafficBilling, rx, tx); got != rx+tx {
		t.Errorf("升级后的老节点已用量 = %d，期望收 + 发 = %d", got, rx+tx)
	}
}

// legacyNode 按名字取一台节点（store 里只有按 id / token 取的两个入口，
// 而这条用例手上只有名字）。
func legacyNode(t *testing.T, ctx context.Context, db *DB, name string) Node {
	t.Helper()
	nodes, err := db.ListNodes(ctx)
	if err != nil {
		t.Fatalf("列出节点: %v", err)
	}
	for _, n := range nodes {
		if n.Name == name {
			return n
		}
	}
	t.Fatalf("列表里没有节点 %q", name)
	return Node{}
}
