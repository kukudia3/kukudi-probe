// Package store 负责 SQLite 的打开、迁移与查询。
//
// 写连接池只有 1 条连接（MaxOpenConns(1)），读连接池 4 条：
// 所有写入都排在同一条连接上，天然不存在写-写竞争；WAL 让读不阻塞写。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // 纯 Go 驱动：无 CGO，交叉编译静态二进制无额外要求
)

const (
	writerConns = 1
	readerConns = 4
)

// DB 持有同一个 SQLite 文件上的写连接池与读连接池。
type DB struct {
	path string
	w    *sql.DB
	r    *sql.DB
}

// Open 打开（必要时创建）数据库、执行迁移，并返回可用的 DB。
func Open(ctx context.Context, path string) (*DB, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("数据库路径为空")
	}
	dir := filepath.Dir(path)
	// 数据目录用 0700：目录里除了主库还有 SQLite 自己创建的 -wal / -shm，
	// 而 WAL 会包含最近的提交页（设置里的密码哈希、会话、审计日志）。
	// 只 chmod 主库文件挡不住同组用户读 WAL，所以把保护落在目录上。
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("创建数据目录 %s 失败: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("收紧数据目录 %s 权限失败: %w", dir, err)
	}

	w, err := openPool(path, true, writerConns)
	if err != nil {
		return nil, err
	}
	if err := w.PingContext(ctx); err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("打开数据库 %s 失败: %w", path, err)
	}
	if err := migrate(ctx, w); err != nil {
		_ = w.Close()
		return nil, err
	}

	r, err := openPool(path, false, readerConns)
	if err != nil {
		_ = w.Close()
		return nil, err
	}
	if err := r.PingContext(ctx); err != nil {
		_ = r.Close()
		_ = w.Close()
		return nil, fmt.Errorf("打开数据库读连接失败: %w", err)
	}

	// 主库文件也收紧到 0600；-wal/-shm 的权限由数据目录的 0700 兜住。
	_ = os.Chmod(path, 0o600)

	return &DB{path: path, w: w, r: r}, nil
}

// Writer 返回写连接池（调用方需自行开启事务）。
func (d *DB) Writer() *sql.DB { return d.w }

// Reader 返回读连接池。
func (d *DB) Reader() *sql.DB { return d.r }

// Ping 检查数据库是否可用。
func (d *DB) Ping(ctx context.Context) error { return d.r.PingContext(ctx) }

// SchemaVersion 返回当前迁移版本。
func (d *DB) SchemaVersion(ctx context.Context) (int, error) { return userVersion(ctx, d.r) }

// Close 关闭两个连接池。
func (d *DB) Close() error {
	var firstErr error
	if d.r != nil {
		if err := d.r.Close(); err != nil {
			firstErr = err
		}
	}
	if d.w != nil {
		if err := d.w.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func openPool(path string, writer bool, maxConns int) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dsn(path, writer))
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}
	db.SetMaxOpenConns(maxConns)
	db.SetMaxIdleConns(maxConns)
	db.SetConnMaxIdleTime(5 * time.Minute)
	return db, nil
}

// dsn 组装 modernc.org/sqlite 的 URI。_pragma 会在每条新连接建立时生效。
func dsn(path string, writer bool) string {
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "temp_store(MEMORY)")
	if writer {
		// 写事务一开始就取写锁，避免"读事务升级为写事务"时的 SQLITE_BUSY。
		q.Add("_txlock", "immediate")
	}
	return "file:" + escapePath(path) + "?" + q.Encode()
}

func escapePath(p string) string {
	p = filepath.ToSlash(p)
	return strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23").Replace(p)
}
