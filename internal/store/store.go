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
	//
	// 但**只在目录是本程序新建的时候**收紧权限：dir 完全来自操作员的
	// --data-dir / PROBE_DATA_DIR，`--data-dir .` 时 filepath.Dir 会把它变成 "."
	// （见 filepath.Join(".", "probe.db") == "probe.db"），无条件 chmod 就等于
	// 改写一个本程序没创建、也可能不属于本程序的目录。已经存在的目录保持现状，
	// 它的权限由操作员与 deploy/install-server.sh 负责（安装脚本本来就 chmod 0700）。
	if clean := filepath.Clean(dir); clean == "." || clean == string(filepath.Separator) ||
		clean == filepath.VolumeName(clean)+string(filepath.Separator) {
		// 当前目录与根目录都是误配置：前者会把操作员的 cwd 当成数据目录，
		// 后者会把库文件摊在根目录下。直接拒启动，比"悄悄改权限"清楚得多。
		return nil, fmt.Errorf("数据目录不能是当前目录或根目录: %s（数据库路径 %s）", dir, path)
	}
	_, statErr := os.Stat(dir)
	created := errors.Is(statErr, os.ErrNotExist)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("创建数据目录 %s 失败: %w", dir, err)
	}
	if created {
		if err := os.Chmod(dir, 0o700); err != nil {
			return nil, fmt.Errorf("收紧数据目录 %s 权限失败: %w", dir, err)
		}
	}

	w, err := openPool(path, true, writerConns)
	if err != nil {
		return nil, err
	}
	// 主库必须在**第一次写之前**就收紧到 0600。
	//
	// 为什么顺序是这个：-wal / -shm 的权限位是 SQLite 在 Open 内部**创建那一刻**
	// 从主库复制来的（驱动 _robust_open 对非零 mode 忽略 umask），而 migrate 的
	// 第一次写事务就会建出 -wal。把 chmod 留到 migrate 之后，主库是 0600 了，
	// WAL 却停在 0644 —— 同机任意本地用户 grep -a 就能读走最近提交页里的
	// settings（Argon2id 管理员哈希、TOTP **明文**种子）。文件还不存在时
	// os.Chmod 返回错误，忽略即可（sql.Open 是惰性的，主库要到 Ping 才建出来）。
	tightenPerms(path)
	if err := w.PingContext(ctx); err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("打开数据库 %s 失败: %w", path, err)
	}
	// Ping 之后主库文件才真正存在，再补一次；顺带收紧崩溃残留的旧 -wal / -shm
	// （它们不会在下一次启动时被重建，只能显式改）。
	tightenPerms(path)
	if err := migrate(ctx, w); err != nil {
		_ = w.Close()
		return nil, err
	}
	// migrate 的写事务会建出 -wal（或在老库上重新打开它），这里再补一次：
	// 权限位记在 inode 上，文件被打开着也能改。
	tightenPerms(path)

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

	// 主库与 -wal / -shm 都在上面收紧过了（见 tightenPerms）；这里再补一次是因为
	// 读连接建立时可能刚建出 -shm。数据目录的 0700 是第二层保护，不是唯一一层。
	tightenPerms(path)

	return &DB{path: path, w: w, r: r}, nil
}

// tightenPerms 把主库与 SQLite 的两个旁路文件收紧到 0600（忽略错误）。
//
// 为什么 -wal / -shm 要单独 chmod：它们的权限位是 SQLite 在**创建那一刻**从主库
// 复制来的，主库晚一步收紧就再也追不回来；而崩溃退出留下的旧 -wal 不会被重建，
// 只能显式改。WAL 里是最近提交页（settings 表：密码哈希、TOTP 明文种子、通知
// Token），-shm 只是索引、危害小得多，但一起收紧没有代价。
//
// 为什么忽略错误：文件可能还不存在（主库首次创建之前、非 WAL 模式下没有 -wal），
// 而 Windows 上的 os.Chmod 只切只读位。收紧失败不该让服务起不来 —— 它是一层
// 纵深防御，真正的边界是数据目录本身。
func tightenPerms(path string) {
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		_ = os.Chmod(p, 0o600)
	}
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
