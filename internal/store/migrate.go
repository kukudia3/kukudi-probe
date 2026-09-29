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
