package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// 与告警相关的设置项键名。
const (
	KeyTelegramEnabled = "telegram_enabled"
	KeyTelegramToken   = "telegram_bot_token"
	KeyTelegramChatID  = "telegram_chat_id"
)

// AlertStateRow 是 alert_state 表的一行（一条规则的持久化状态）。
type AlertStateRow struct {
	NodeID     int64
	Rule       string
	State      string
	Since      int64
	LastNotify int64
	NotifyCnt  int
	Context    string
}

// LoadAlertStates 读回全部告警状态（服务端启动时载入，避免重启后重复轰炸）。
func (d *DB) LoadAlertStates(ctx context.Context) ([]AlertStateRow, error) {
	rows, err := d.r.QueryContext(ctx,
		`SELECT node_id, rule, state, since, last_notify, notify_cnt, context FROM alert_state`)
	if err != nil {
		return nil, fmt.Errorf("读取告警状态失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []AlertStateRow
	for rows.Next() {
		var r AlertStateRow
		if err := rows.Scan(&r.NodeID, &r.Rule, &r.State, &r.Since, &r.LastNotify, &r.NotifyCnt, &r.Context); err != nil {
			return nil, fmt.Errorf("读取告警状态失败: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历告警状态失败: %w", err)
	}
	return out, nil
}

// UpsertAlertStates 写入/更新告警状态。
func (d *DB) UpsertAlertStates(ctx context.Context, states []AlertStateRow) error {
	if len(states) == 0 {
		return nil
	}
	tx, err := d.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO alert_state (node_id, rule, state, since, last_notify, notify_cnt, context)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(node_id, rule) DO UPDATE SET
			state = excluded.state, since = excluded.since,
			last_notify = excluded.last_notify, notify_cnt = excluded.notify_cnt,
			context = excluded.context`)
	if err != nil {
		return fmt.Errorf("准备写入告警状态失败: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for _, s := range states {
		if _, err := stmt.ExecContext(ctx,
			s.NodeID, s.Rule, s.State, s.Since, s.LastNotify, s.NotifyCnt, s.Context); err != nil {
			return fmt.Errorf("写入告警状态失败: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交告警状态失败: %w", err)
	}
	return nil
}

// DeleteAlertStates 删除某节点的告警状态（编辑节点配置时重置规则）。
func (d *DB) DeleteAlertStates(ctx context.Context, nodeID int64) error {
	if _, err := d.w.ExecContext(ctx, `DELETE FROM alert_state WHERE node_id = ?`, nodeID); err != nil {
		return fmt.Errorf("删除告警状态失败: %w", err)
	}
	return nil
}

// GetSettings 批量读取若干设置项（缺失的键不会出现在结果里）。
//
// 一条 SQL 取回全部：调用方（读取通知配置）在每次设置变更与启动时都会调用，
// 循环单查是 N 次往返；设置项本来就只有几行。
func (d *DB) GetSettings(ctx context.Context, keys ...string) (map[string]string, error) {
	out := make(map[string]string, len(keys))
	if len(keys) == 0 {
		return out, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",")
	args := make([]any, 0, len(keys))
	for _, key := range keys {
		args = append(args, key)
	}
	rows, err := d.r.QueryContext(ctx,
		`SELECT key, value FROM settings WHERE key IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("读取设置失败: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, fmt.Errorf("读取设置失败: %w", err)
		}
		out[key] = value
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历设置失败: %w", err)
	}
	return out, nil
}

// SetSettings 在一个事务里写入多个设置项。
func (d *DB) SetSettings(ctx context.Context, values map[string]string) error {
	if len(values) == 0 {
		return nil
	}
	tx, err := d.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().Unix()
	for key, value := range values {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
			key, value, now); err != nil {
			return fmt.Errorf("写入设置 %s 失败: %w", key, err)
		}
	}
	return tx.Commit()
}
