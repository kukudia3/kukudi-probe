package store

import (
	"context"
	"database/sql"
	"fmt"
)

// migration 是一次不可回退的结构变更，对应 PRAGMA user_version 的一次递增。
type migration struct {
	name  string
	stmts []string
}

// migrations 只前进不回退。
//
// 已发布的迁移绝不能修改（否则老部署的库会与新代码不一致）；
// 任何结构变化都追加新元素。
var migrations = []migration{
	{name: "0001_init", stmts: schemaV1},
	// 0002 只做加法：给已有部署补上价格字段。DEFAULT 0/'' 让老行天然是"没填价格"，
	// 不需要在迁移里回填，也不会让老的 PATCH 请求突然校验失败。
	{name: "0002_node_price", stmts: []string{
		`ALTER TABLE nodes ADD COLUMN price_cents INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE nodes ADD COLUMN currency TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE nodes ADD COLUMN billing_months INTEGER NOT NULL DEFAULT 0`,
	}},
	// 0003 只加一张新表：延迟探测（ping_samples_1m）。
	//
	// 不动 0001/0002 的任何语句 —— 老库升级时只会多出这张空表，
	// 已有数据、已有设置（包括 visible_charts）一律不受影响。
	{name: "0003_ping_samples", stmts: []string{pingSamplesDDL}},
	// 0004 给节点加「标签」列：存 JSON 字符串数组（如 ["探针","搜索"]）。
	//
	// 为什么不建关联表：标签永远跟着节点**整体**读写（一次最多 8 个，界面上就是
	// 一行徽章），既不做跨节点的标签查询，也不需要在标签改名时批量更新。一张
	// node_tags 表要多一次 join、多一套增删改逻辑，还要处理"删节点时级联"，
	// 换不来任何东西 —— 与"分组/地区是普通列"同一条取舍（见 schema.go 的说明）。
	//
	// DEFAULT '[]' 让老行天然是"没有标签"，不需要在迁移里回填，也不会让老的
	// PATCH 请求突然校验失败；NOT NULL 保证读路径永远拿到字符串而不是 NULL。
	{name: "0004_node_tags", stmts: []string{
		`ALTER TABLE nodes ADD COLUMN tags TEXT NOT NULL DEFAULT '[]'`,
	}},
}

func migrate(ctx context.Context, db *sql.DB) error {
	current, err := userVersion(ctx, db)
	if err != nil {
		return err
	}
	if current > len(migrations) {
		return fmt.Errorf("数据库 schema 版本为 %d，高于本程序支持的 %d：请升级 probe-server，不要用旧版本打开新库", current, len(migrations))
	}
	for i := current; i < len(migrations); i++ {
		m := migrations[i]
		if err := applyMigration(ctx, db, m, i+1); err != nil {
			return fmt.Errorf("迁移 %s 失败: %w", m.name, err)
		}
	}
	return nil
}

func applyMigration(ctx context.Context, db *sql.DB, m migration, version int) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // 已提交时返回 ErrTxDone，忽略

	for _, stmt := range m.stmts {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("%w（出错语句：%.80s）", err, stmt)
		}
	}
	// version 是本包内的常量，不存在注入风险。
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
		return err
	}
	return tx.Commit()
}

func userVersion(ctx context.Context, db *sql.DB) (int, error) {
	var v int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return 0, fmt.Errorf("读取 schema 版本失败: %w", err)
	}
	return v, nil
}
