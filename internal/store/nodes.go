package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// TokenPrefix 是所有 Agent Token 的前缀，便于在日志/配置里一眼认出。
const TokenPrefix = "pba_"

// 节点字段上限。服务端 API 会先校验一次，这里再挡一次——
// 存储层是最后一道防线，任何调用方都不能绕过。
const (
	maxNodeNameLen  = 64
	maxGroupLen     = 64
	maxRegionLen    = 64
	maxNoteLen      = 512
	maxNodeIfaceLen = 32
)

var (
	// ErrNodeNotFound 表示节点不存在（或 token 无效）。
	ErrNodeNotFound = errors.New("节点不存在")
	// ErrNodeNameTaken 表示节点名称重复。
	ErrNodeNameTaken = errors.New("节点名称已存在")
	// ErrInvalidNode 表示节点参数不合法。
	ErrInvalidNode = errors.New("节点参数不合法")
)

// Node 是节点的配置信息（不含 token 明文——明文只在创建时返回一次）。
type Node struct {
	ID             int64
	Name           string
	GroupName      string
	Region         string
	Note           string
	TokenPrefix    string
	TokenCreatedAt int64
	Iface          string
	IntervalSec    int
	TrafficLimit   int64
	TrafficWarnPct int
	ResetDay       int
	ExpiresAt      int64
	SortOrder      int
	Enabled        bool
	CreatedAt      int64
	UpdatedAt      int64
}

// NewNode 是创建节点时的输入。
type NewNode struct {
	Name           string
	GroupName      string
	Region         string
	Note           string
	Iface          string
	IntervalSec    int
	TrafficLimit   int64
	TrafficWarnPct int
	ResetDay       int
	ExpiresAt      int64
	SortOrder      int
}

// GenerateToken 生成 32 字节随机 Token（前缀 pba_ + base64url）。
func GenerateToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成随机 Token 失败: %w", err)
	}
	return TokenPrefix + base64.RawURLEncoding.EncodeToString(buf), nil
}

// HashToken 返回 Token 的 SHA-256。数据库只存这个值，永远不存明文。
//
// Token 是高熵随机值（256 位），不需要 Argon2 之类的慢哈希。
func HashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// TokenPrefixOf 返回用于界面展示的前 8 个字符（不足以还原 Token）。
func TokenPrefixOf(token string) string {
	if len(token) <= 8 {
		return token
	}
	return token[:8]
}

func (n NewNode) normalized() NewNode {
	n.Name = strings.TrimSpace(n.Name)
	n.GroupName = strings.TrimSpace(n.GroupName)
	n.Region = strings.TrimSpace(n.Region)
	n.Note = strings.TrimSpace(n.Note)
	n.Iface = strings.TrimSpace(n.Iface)
	return n
}

// Validate 校验节点参数。返回的错误一定包装了 ErrInvalidNode。
//
// 长度一律按**字符**（rune）算：前端表单的 maxlength 也是按字符，
// 用字节数会出现"22 个汉字的名称被拒、提示却说超过 64"这种自相矛盾的提示，
// 也会让本该合法的中文备注（171–200 字）被拒。
func (n NewNode) Validate() error {
	fail := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalidNode, fmt.Sprintf(format, args...))
	}
	nameLen := utf8.RuneCountInString(n.Name)
	switch {
	case n.Name == "":
		return fail("名称不能为空")
	case nameLen > maxNodeNameLen:
		return fail("名称长度 %d 超过上限 %d", nameLen, maxNodeNameLen)
	case utf8.RuneCountInString(n.GroupName) > maxGroupLen:
		return fail("分组长度超过上限 %d", maxGroupLen)
	case utf8.RuneCountInString(n.Region) > maxRegionLen:
		return fail("地区长度超过上限 %d", maxRegionLen)
	case utf8.RuneCountInString(n.Note) > maxNoteLen:
		return fail("备注长度超过上限 %d", maxNoteLen)
	case utf8.RuneCountInString(n.Iface) > maxNodeIfaceLen:
		return fail("网卡名长度超过上限 %d", maxNodeIfaceLen)
	case n.IntervalSec < 1 || n.IntervalSec > 300:
		return fail("上报间隔 %d 必须在 1-300 秒之间", n.IntervalSec)
	case n.TrafficLimit < 0:
		return fail("流量额度不能为负")
	case n.TrafficWarnPct < 0 || n.TrafficWarnPct > 100:
		return fail("告警阈值 %d 必须在 0-100 之间", n.TrafficWarnPct)
	case n.ResetDay < 1 || n.ResetDay > 31:
		return fail("流量重置日 %d 必须在 1-31 之间", n.ResetDay)
	case n.ExpiresAt < 0:
		return fail("到期时间不能为负")
	}
	return nil
}

// CreateNode 创建节点并生成 Token，返回节点与**仅此一次**的明文 Token。
func (d *DB) CreateNode(ctx context.Context, in NewNode, now time.Time) (Node, string, error) {
	in = in.normalized()
	if err := in.Validate(); err != nil {
		return Node{}, "", err
	}
	token, err := GenerateToken()
	if err != nil {
		return Node{}, "", err
	}

	tx, err := d.w.BeginTx(ctx, nil)
	if err != nil {
		return Node{}, "", err
	}
	defer func() { _ = tx.Rollback() }()

	ts := now.Unix()
	res, err := tx.ExecContext(ctx, `
		INSERT INTO nodes (name, group_name, region, note, token_hash, token_prefix, token_created_at,
			iface, interval_sec, traffic_limit, traffic_warn_pct, reset_day, expires_at, sort_order,
			enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)`,
		in.Name, in.GroupName, in.Region, in.Note, HashToken(token), TokenPrefixOf(token), ts,
		in.Iface, in.IntervalSec, in.TrafficLimit, in.TrafficWarnPct, in.ResetDay, in.ExpiresAt, in.SortOrder,
		ts, ts)
	if err != nil {
		if isUniqueViolation(err, "nodes.name") {
			return Node{}, "", ErrNodeNameTaken
		}
		return Node{}, "", fmt.Errorf("创建节点失败: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Node{}, "", fmt.Errorf("读取新节点 ID 失败: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO node_runtime (node_id, updated_at) VALUES (?, ?)`, id, ts); err != nil {
		return Node{}, "", fmt.Errorf("初始化节点运行态失败: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Node{}, "", fmt.Errorf("提交创建节点事务失败: %w", err)
	}

	node, err := d.NodeByID(ctx, id)
	if err != nil {
		return Node{}, "", err
	}
	return node, token, nil
}

// NodeByID 按 ID 读取节点。
func (d *DB) NodeByID(ctx context.Context, id int64) (Node, error) {
	row := d.r.QueryRowContext(ctx, nodeSelect+` WHERE id = ?`, id)
	return scanNode(row)
}

// NodeByTokenHash 按 Token 哈希读取节点（Agent 鉴权路径）。
func (d *DB) NodeByTokenHash(ctx context.Context, hash []byte) (Node, error) {
	row := d.r.QueryRowContext(ctx, nodeSelect+` WHERE token_hash = ?`, hash)
	return scanNode(row)
}

// ListNodes 返回全部节点，按排序值、ID 排列。
func (d *DB) ListNodes(ctx context.Context) ([]Node, error) {
	rows, err := d.r.QueryContext(ctx, nodeSelect+` ORDER BY sort_order, id`)
	if err != nil {
		return nil, fmt.Errorf("查询节点列表失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var nodes []Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历节点列表失败: %w", err)
	}
	return nodes, nil
}

// RotateNodeToken 重新生成 Token，旧 Token 立即失效。
func (d *DB) RotateNodeToken(ctx context.Context, id int64, now time.Time) (Node, string, error) {
	token, err := GenerateToken()
	if err != nil {
		return Node{}, "", err
	}
	res, err := d.w.ExecContext(ctx,
		`UPDATE nodes SET token_hash = ?, token_prefix = ?, token_created_at = ?, updated_at = ? WHERE id = ?`,
		HashToken(token), TokenPrefixOf(token), now.Unix(), now.Unix(), id)
	if err != nil {
		return Node{}, "", fmt.Errorf("更新 Token 失败: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return Node{}, "", err
	}
	if affected == 0 {
		return Node{}, "", ErrNodeNotFound
	}
	node, err := d.NodeByID(ctx, id)
	if err != nil {
		return Node{}, "", err
	}
	return node, token, nil
}

// UpdateNode 更新节点配置（Token 不在其中，走 RotateNodeToken）。
func (d *DB) UpdateNode(ctx context.Context, n Node, now time.Time) error {
	in := NewNode{
		Name: n.Name, GroupName: n.GroupName, Region: n.Region, Note: n.Note, Iface: n.Iface,
		IntervalSec: n.IntervalSec, TrafficLimit: n.TrafficLimit, TrafficWarnPct: n.TrafficWarnPct,
		ResetDay: n.ResetDay, ExpiresAt: n.ExpiresAt, SortOrder: n.SortOrder,
	}
	in = in.normalized()
	if err := in.Validate(); err != nil {
		return err
	}
	enabled := 0
	if n.Enabled {
		enabled = 1
	}
	res, err := d.w.ExecContext(ctx, `
		UPDATE nodes SET name = ?, group_name = ?, region = ?, note = ?, iface = ?, interval_sec = ?,
			traffic_limit = ?, traffic_warn_pct = ?, reset_day = ?, expires_at = ?, sort_order = ?,
			enabled = ?, updated_at = ?
		WHERE id = ?`,
		in.Name, in.GroupName, in.Region, in.Note, in.Iface, in.IntervalSec,
		in.TrafficLimit, in.TrafficWarnPct, in.ResetDay, in.ExpiresAt, in.SortOrder,
		enabled, now.Unix(), n.ID)
	if err != nil {
		if isUniqueViolation(err, "nodes.name") {
			return ErrNodeNameTaken
		}
		return fmt.Errorf("更新节点失败: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		// 注意：字段值没有变化时 RowsAffected 也可能为 0，因此再用一次存在性检查。
		if _, err := d.NodeByID(ctx, n.ID); err != nil {
			return err
		}
	}
	return nil
}

// DeleteNode 删除节点及其全部历史数据。
//
// node_runtime / traffic_daily / alert_state 靠外键级联清理；
// samples_10s / samples_1m 没有外键（每秒级写入不想付外键检查成本），必须在这里显式删除。
func (d *DB) DeleteNode(ctx context.Context, id int64) error {
	tx, err := d.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	for _, table := range []string{"samples_10s", "samples_1m"} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE node_id = ?`, id); err != nil {
			return fmt.Errorf("清理 %s 失败: %w", table, err)
		}
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM nodes WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("删除节点失败: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrNodeNotFound
	}
	return tx.Commit()
}

// AuditRow 是审计日志的一行。
type AuditRow struct {
	ID     int64
	TS     int64
	Action string
	NodeID int64
	IP     string
	Detail string
}

// ListAudit 按时间倒序返回审计日志（beforeID > 0 时只返回更早的记录，用于翻页）。
func (d *DB) ListAudit(ctx context.Context, limit int, beforeID int64) ([]AuditRow, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var (
		rows *sql.Rows
		err  error
	)
	if beforeID > 0 {
		rows, err = d.r.QueryContext(ctx, `
			SELECT id, ts, action, node_id, ip, detail FROM audit_log
			WHERE id < ? ORDER BY id DESC LIMIT ?`, beforeID, limit)
	} else {
		rows, err = d.r.QueryContext(ctx, `
			SELECT id, ts, action, node_id, ip, detail FROM audit_log
			ORDER BY id DESC LIMIT ?`, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("查询审计日志失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]AuditRow, 0, limit)
	for rows.Next() {
		var r AuditRow
		if err := rows.Scan(&r.ID, &r.TS, &r.Action, &r.NodeID, &r.IP, &r.Detail); err != nil {
			return nil, fmt.Errorf("读取审计日志失败: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历审计日志失败: %w", err)
	}
	return out, nil
}

// AppendAudit 写入一条审计记录，并把表保持在 2000 行以内。
func (d *DB) AppendAudit(ctx context.Context, action string, nodeID int64, ip, detail string) error {
	tx, err := d.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO audit_log (ts, action, node_id, ip, detail) VALUES (?, ?, ?, ?, ?)`,
		time.Now().Unix(), action, nodeID, ip, detail); err != nil {
		return fmt.Errorf("写入审计日志失败: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM audit_log WHERE id <= (SELECT max(id) FROM audit_log) - 2000`); err != nil {
		return fmt.Errorf("清理审计日志失败: %w", err)
	}
	return tx.Commit()
}

const nodeSelect = `SELECT id, name, group_name, region, note, token_prefix, token_created_at,
	iface, interval_sec, traffic_limit, traffic_warn_pct, reset_day, expires_at, sort_order,
	enabled, created_at, updated_at FROM nodes`

// rowScanner 让 Node 的扫描逻辑同时适用于 QueryRow 与 Rows。
type rowScanner interface {
	Scan(dest ...any) error
}

func scanNode(row rowScanner) (Node, error) {
	var (
		n       Node
		enabled int
	)
	err := row.Scan(&n.ID, &n.Name, &n.GroupName, &n.Region, &n.Note, &n.TokenPrefix, &n.TokenCreatedAt,
		&n.Iface, &n.IntervalSec, &n.TrafficLimit, &n.TrafficWarnPct, &n.ResetDay, &n.ExpiresAt, &n.SortOrder,
		&enabled, &n.CreatedAt, &n.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Node{}, ErrNodeNotFound
	}
	if err != nil {
		return Node{}, fmt.Errorf("读取节点失败: %w", err)
	}
	n.Enabled = enabled != 0
	return n, nil
}

func isUniqueViolation(err error, index string) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed") &&
		strings.Contains(err.Error(), index)
}
