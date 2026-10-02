package store

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// 这个文件是两步验证（TOTP）在存储层的全部状态，键的定义见 settings.go。
//
// 为什么单独一个文件而不是塞进 settings.go：这里除了读写还有一个**原子消费**
// （用掉一个恢复码要在同一个事务里"读—删—写回"），而那个函数是这套东西里
// 唯一有并发语义的地方，值得单独摆出来被人看见。

// TwoFactorState 是两步验证的完整状态（一次读全，避免半新半旧）。
type TwoFactorState struct {
	// Secret 是 TOTP 共享种子（base32，已归一化：大写、无空格）。空串表示没开。
	Secret string
	// RecoveryHashes 是还没用过的恢复码的 SHA-256（十六进制）。
	RecoveryHashes []string
	// LastCounter 是最后一次被接受的 TOTP 时间计数器（防重放）。
	LastCounter int64
}

// Enabled 报告两步验证是否开着。
//
// 判据只有"密钥在不在"一条：密钥是**确认过当前码之后**才写进去的
// （见 server 的 handleTwoFAEnable），所以它存在就等于这套两步验证已经生效。
func (s TwoFactorState) Enabled() bool { return s.Secret != "" }

// TwoFactorState 读出两步验证的全部状态。
func (d *DB) TwoFactorState(ctx context.Context) (TwoFactorState, error) {
	var out TwoFactorState

	secret, ok, err := d.GetSetting(ctx, KeyTwoFASecret)
	if err != nil {
		return out, err
	}
	if ok {
		out.Secret = secret
	}

	raw, ok, err := d.GetSetting(ctx, KeyTwoFARecovery)
	if err != nil {
		return out, err
	}
	if ok && raw != "" {
		if err := json.Unmarshal([]byte(raw), &out.RecoveryHashes); err != nil {
			// 这里**不能**当成"没有恢复码"含糊过去：那会让"恢复码还有几个"
			// 显示成 0，用户以为已经用光了。所以原样报错，由调用方决定怎么办。
			return out, fmt.Errorf("解析恢复码列表失败: %w", err)
		}
	}

	rawCounter, ok, err := d.GetSetting(ctx, KeyTwoFALastCounter)
	if err != nil {
		return out, err
	}
	if ok && rawCounter != "" {
		n, err := strconv.ParseInt(rawCounter, 10, 64)
		if err != nil {
			return out, fmt.Errorf("解析两步验证计数器失败: %w", err)
		}
		out.LastCounter = n
	}
	return out, nil
}

// EnableTwoFactor 写入密钥、恢复码哈希与初始计数器（一个事务，要么全生效要么全不生效）。
func (d *DB) EnableTwoFactor(ctx context.Context, secret string, recoveryHashes []string, lastCounter int64) error {
	if secret == "" {
		return errors.New("两步验证密钥为空")
	}
	raw, err := json.Marshal(recoveryHashes)
	if err != nil {
		return fmt.Errorf("序列化恢复码失败: %w", err)
	}

	tx, err := d.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().Unix()
	for key, value := range map[string]string{
		KeyTwoFASecret:      secret,
		KeyTwoFARecovery:    string(raw),
		KeyTwoFALastCounter: strconv.FormatInt(lastCounter, 10),
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

// ClearTwoFactor 删除两步验证的三块状态，返回是否真的删掉了东西。
//
// 这是"忘了密码 + 丢了验证器"的唯一出路（见 cmd/probe-server 的 --reset-2fa）：
// 它**不校验任何凭据**（密码可能正是忘掉的那个），所以调用方必须保证
// "能在服务器本机执行"这一前提，并把这次操作写进日志与审计。
func (d *DB) ClearTwoFactor(ctx context.Context) (bool, error) {
	tx, err := d.w.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()

	cleared := false
	for _, key := range []string{KeyTwoFASecret, KeyTwoFARecovery, KeyTwoFALastCounter} {
		res, err := tx.ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, key)
		if err != nil {
			return false, fmt.Errorf("删除设置 %s 失败: %w", key, err)
		}
		if n, err := res.RowsAffected(); err == nil && n > 0 {
			cleared = true
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return cleared, nil
}

// SetTwoFactorLastCounter 记录最后用过的计数器（防重放）。
func (d *DB) SetTwoFactorLastCounter(ctx context.Context, counter int64) error {
	return d.SetSetting(ctx, KeyTwoFALastCounter, strconv.FormatInt(counter, 10))
}

// SetTwoFactorRecovery 覆盖恢复码列表（重新生成时用；旧的一次性全部作废）。
func (d *DB) SetTwoFactorRecovery(ctx context.Context, hashes []string) error {
	raw, err := json.Marshal(hashes)
	if err != nil {
		return fmt.Errorf("序列化恢复码失败: %w", err)
	}
	return d.SetSetting(ctx, KeyTwoFARecovery, string(raw))
}

// ConsumeRecoveryCode 原子地"用掉"一个恢复码：命中就把它从列表里删掉。
//
// 为什么必须在**一个事务**里做"读—改—写回"：两步登录是并发的（用户可能同时
// 在两个标签页里用同一个恢复码，攻击者也可能并行重放）。分开读再写回的话，
// 两个请求都能读到"它还在"，于是同一个一次性凭据被用两次 —— 而"一次性"
// 正是恢复码的全部意义。写连接池只有一个连接，事务串行执行，天然互斥。
func (d *DB) ConsumeRecoveryCode(ctx context.Context, hash string) (bool, error) {
	if hash == "" {
		return false, nil
	}
	tx, err := d.w.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()

	var raw string
	err = tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, KeyTwoFARecovery).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("读取恢复码失败: %w", err)
	}
	var hashes []string
	if err := json.Unmarshal([]byte(raw), &hashes); err != nil {
		return false, fmt.Errorf("解析恢复码列表失败: %w", err)
	}

	kept := make([]string, 0, len(hashes))
	found := false
	for _, h := range hashes {
		// 哈希之间用恒定时间比较：这里比较的不是秘密本身，但"哪一个下标命中"
		// 同样是时序信息，而这段代码的代价可以忽略。
		if !found && subtle.ConstantTimeCompare([]byte(h), []byte(hash)) == 1 {
			found = true
			continue
		}
		kept = append(kept, h)
	}
	if !found {
		return false, nil
	}

	encoded, err := json.Marshal(kept)
	if err != nil {
		return false, fmt.Errorf("序列化恢复码失败: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		KeyTwoFARecovery, string(encoded), time.Now().Unix()); err != nil {
		return false, fmt.Errorf("写回恢复码失败: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}
