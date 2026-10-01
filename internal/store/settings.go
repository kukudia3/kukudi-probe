package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// settings 表里用到的键。
const (
	// KeyAdminUsername / KeyAdminHash 是单管理员的账号与密码哈希（Argon2id PHC 字符串）。
	KeyAdminUsername = "admin_username"
	KeyAdminHash     = "admin_password_hash"

	// KeyFXRates 是最近一次取到的汇率快照（JSON，见 internal/fx.Snapshot）。
	//
	// 放在已有的 settings KV 表里而不是新开一张表 / 加一次迁移：它是"一份会整体
	// 覆盖的值"，没有查询需求，KV 表就是为这种东西准备的。键名带 _rates 后缀，
	// 与将来可能出现的其它汇率相关键（比如手动覆盖表）区分得开。
	KeyFXRates = "fx_rates"
)

// GetSetting 读取一个设置项；不存在时返回 ok=false。
func (d *DB) GetSetting(ctx context.Context, key string) (string, bool, error) {
	var value string
	err := d.r.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("读取设置 %s 失败: %w", key, err)
	}
	return value, true, nil
}

// SetSetting 写入一个设置项（已存在则覆盖）。
//
// 与 AdminAccount 那几个不同，这里是通用的单键写入：汇率快照每天覆盖一次，
// 不需要事务（一次写入要么整份生效、要么整份没写，不存在写一半的中间态）。
func (d *DB) SetSetting(ctx context.Context, key, value string) error {
	_, err := d.w.ExecContext(ctx, `
		INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("写入设置 %s 失败: %w", key, err)
	}
	return nil
}

// AdminAccount 返回管理员账号与密码哈希。两者都缺失时 ok=false（说明还没初始化）。
func (d *DB) AdminAccount(ctx context.Context) (username, hash string, ok bool, err error) {
	username, hasUser, err := d.GetSetting(ctx, KeyAdminUsername)
	if err != nil {
		return "", "", false, err
	}
	hash, hasHash, err := d.GetSetting(ctx, KeyAdminHash)
	if err != nil {
		return "", "", false, err
	}
	return username, hash, hasUser && hasHash, nil
}

// SetAdminAccount 在一个事务里写入管理员账号与密码哈希（已存在则覆盖，用于改密）。
func (d *DB) SetAdminAccount(ctx context.Context, username, passwordHash string) error {
	tx, err := d.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().Unix()
	for key, value := range map[string]string{
		KeyAdminUsername: username,
		KeyAdminHash:     passwordHash,
	} {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
			key, value, now); err != nil {
			return fmt.Errorf("写入设置 %s 失败: %w", key, err)
		}
	}
	return tx.Commit()
}

// CreateAdminIfAbsent 只在"还没有管理员"时创建账号，返回是否抢到了创建权。
//
// 初始化接口在检查（还没管理员）与写入之间要做一次约 100ms 的 Argon2 计算，
// 双击/并发提交时两个请求都能通过检查，后写的会静默覆盖先建好的密码。
// 用数据库的唯一键做哨兵（admin_username 只能被插入一次）就没有这个竞态了。
func (d *DB) CreateAdminIfAbsent(ctx context.Context, username, passwordHash string) (bool, error) {
	tx, err := d.w.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().Unix()
	res, err := tx.ExecContext(ctx, `
		INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO NOTHING`, KeyAdminUsername, username, now)
	if err != nil {
		return false, fmt.Errorf("写入管理员用户名失败: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if affected == 0 {
		return false, nil // 已经有人抢先创建了
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		KeyAdminHash, passwordHash, now); err != nil {
		return false, fmt.Errorf("写入管理员密码失败: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// ErrSessionNotFound 表示会话不存在或已过期。
var ErrSessionNotFound = errors.New("会话不存在")

// Session 是一条登录会话（只保存 Token 的哈希）。
type Session struct {
	Hash       []byte
	CreatedAt  int64
	ExpiresAt  int64
	LastSeenAt int64
	IP         string
	UA         string
}

// CreateSession 新建会话。
func (d *DB) CreateSession(ctx context.Context, hash []byte, ttl time.Duration, now time.Time, ip, ua string) error {
	_, err := d.w.ExecContext(ctx, `
		INSERT INTO sessions (token_hash, created_at, expires_at, last_seen_at, ip, ua)
		VALUES (?, ?, ?, ?, ?, ?)`,
		hash, now.Unix(), now.Add(ttl).Unix(), now.Unix(), truncate(ip, 64), truncate(ua, 200))
	if err != nil {
		return fmt.Errorf("创建会话失败: %w", err)
	}
	return nil
}

// SessionRenewInterval 是滑动续期的最小写库间隔。
//
// 为什么需要节流：SessionByHash 在每个已鉴权请求上都会被调用，如果每次都 UPDATE，
// 就把"每分钟个位数事务"的设计目标变成"每个请求一次写"，而且这些写都落在唯一的
// 写连接上，会和采样落盘、rollup 抢锁。7 天有效期的会话按分钟续期完全够用。
const SessionRenewInterval = time.Minute

// SessionByHash 按 Token 哈希读取未过期会话（同时刷新 last_seen 与到期时间，实现滑动续期）。
//
// 续期不是每次都写库：距上次写库超过 SessionRenewInterval 才落一次
// （返回值里的 ExpiresAt 始终是"本次请求之后的有效期"，调用方语义不变）。
func (d *DB) SessionByHash(ctx context.Context, hash []byte, now time.Time, ttl time.Duration) (Session, error) {
	var s Session
	err := d.r.QueryRowContext(ctx,
		`SELECT token_hash, created_at, expires_at, last_seen_at, ip, ua FROM sessions WHERE token_hash = ?`,
		hash).Scan(&s.Hash, &s.CreatedAt, &s.ExpiresAt, &s.LastSeenAt, &s.IP, &s.UA)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrSessionNotFound
	}
	if err != nil {
		return Session{}, fmt.Errorf("读取会话失败: %w", err)
	}
	if s.ExpiresAt <= now.Unix() {
		return Session{}, ErrSessionNotFound
	}
	s.ExpiresAt = now.Add(ttl).Unix()
	if now.Unix()-s.LastSeenAt < int64(SessionRenewInterval.Seconds()) {
		// 刚续过：不写库。落库的 expires_at 最多滞后一个节流窗口，
		// 远小于每小时一次的过期会话清理判据，不会误删活跃会话。
		return s, nil
	}
	if _, err := d.w.ExecContext(ctx, `UPDATE sessions SET last_seen_at = ?, expires_at = ? WHERE token_hash = ?`,
		now.Unix(), s.ExpiresAt, hash); err != nil {
		return Session{}, fmt.Errorf("续期会话失败: %w", err)
	}
	s.LastSeenAt = now.Unix()
	return s, nil
}

// DeleteSession 删除会话（登出）。
func (d *DB) DeleteSession(ctx context.Context, hash []byte) error {
	if _, err := d.w.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, hash); err != nil {
		return fmt.Errorf("删除会话失败: %w", err)
	}
	return nil
}

// DeleteSessionsExcept 删除除保留项以外的所有会话（改密码后调用，踢掉其它登录）。
func (d *DB) DeleteSessionsExcept(ctx context.Context, keep []byte) (int64, error) {
	query := `DELETE FROM sessions`
	var args []any
	if len(keep) > 0 {
		query = `DELETE FROM sessions WHERE token_hash <> ?`
		args = append(args, keep)
	}
	res, err := d.w.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("清理会话失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("读取清理条数失败: %w", err)
	}
	return n, nil
}

// DeleteExpiredSessions 清理已过期会话，返回删除条数。
func (d *DB) DeleteExpiredSessions(ctx context.Context, now time.Time) (int64, error) {
	res, err := d.w.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, now.Unix())
	if err != nil {
		return 0, fmt.Errorf("清理过期会话失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("读取清理条数失败: %w", err)
	}
	return n, nil
}

// truncate 按字符（而不是字节）截断，避免截出半个 UTF-8 字符。
func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit])
}
