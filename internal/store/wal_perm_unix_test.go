//go:build !windows

package store

// 04-S-1 的权限断言：这条用例**只在非 Windows 上生效**。
//
// 为什么必须带 build tag / 只能跳过：Windows 的 os.Chmod 只在"只读 / 可写"之间切，
// 量出来永远是 0555/0777，量不出 0600 与 0644 的区别 —— 而这一条要钉的正是
// "WAL 不能被同机其它本地账号读走"。
//
// 为什么要把 umask 显式设成 022：不设的话这条用例可能**平凡通过**。
// systemd 单元（deploy/install-server.sh）里是 UMask=0077，那种环境下即使修复前
// 建出来的 -wal 也是 0600；而"操作员手工 ./probe-server 起进程"时的 umask 通常是
// 022 —— 那才是 WAL 停在 0644 的真实场景。修复（主库在第一次写之前 chmod 0600 +
// 顺手收紧 -wal/-shm）正是为它做的。

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestWALAndSHMAreTightenedBeforeFirstWrite(t *testing.T) {
	old := syscall.Umask(0o022)
	defer syscall.Umask(old)

	// 目录刻意不用 0700：t.TempDir 建出来是 0700，而缺陷的触发条件恰恰是
	// "数据目录已存在且 group/other 可进入"（mkdir data 默认 0755）。
	// Open 不会去改已存在目录的权限（那是既定取舍），所以目录不是防线，
	// 文件权限必须是。
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("设置目录权限: %v", err)
	}

	path := filepath.Join(dir, "probe.db")
	db, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	// -wal 必须存在：Open 里的迁移一定写过库（要么建表、要么写 user_version）。
	// 它还在磁盘上，正是"暴露窗口"本身。
	for _, suffix := range []string{"", "-wal", "-shm"} {
		info, err := os.Stat(path + suffix)
		if err != nil {
			if suffix == "-shm" {
				// -shm 由 SQLite 按需创建，某些时机下可能刚被删掉：不强制要求。
				continue
			}
			t.Fatalf("stat %s%s: %v", filepath.Base(path), suffix, err)
		}
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			t.Errorf("%s%s 权限 = %#o，组/其他账号可读：WAL 里是最近提交页"+
				"（settings 表的密码哈希、TOTP 明文种子、通知 Token，见 04-S-1）",
				filepath.Base(path), suffix, perm)
		}
	}
}
