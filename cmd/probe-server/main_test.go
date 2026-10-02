package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"probe/internal/store"
)

// 这个文件验的是 `probe-server --reset-2fa` 这条**救援命令**本身。
//
// 为什么值得单开一个包内用例：这是"忘了密码 + 丢了验证器"的唯一出路。
// 它坏掉的方式特别隐蔽 —— 参数名拼错、忘记建库、把库建在别处、
// 清了状态却没留日志 —— 每一种的表现都是"用户在服务器上敲了一条命令，
// 什么也没发生"，而这时他已经没有别的路可以走了。
//
// 所以这里不做任何 mock：真的解析命令行、真的开一个 SQLite 库、
// 真的把两步验证状态写进去再清掉，并检查日志与审计里留下了记录。

// TestResetTwoFactorFlagClearsStateAndLogs 走一遍完整的救援流程。
func TestResetTwoFactorFlagClearsStateAndLogs(t *testing.T) {
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "probe.db")

	// ① 造一个"开着两步验证"的库（直接用存储层写入，不启动服务端）。
	ctx := context.Background()
	db, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	const secret = "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP"
	if err := db.EnableTwoFactor(ctx, secret, []string{"deadbeef"}, 12345); err != nil {
		t.Fatalf("写入两步验证状态: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("关闭数据库: %v", err)
	}

	// ② 跑命令行（捕获 stdout，因为"写了日志"是这条命令的硬要求之一）。
	out := captureStdout(t, func() {
		if err := run([]string{"--reset-2fa", "--data-dir", dataDir}); err != nil {
			t.Errorf("--reset-2fa 应当成功返回，实际: %v", err)
		}
	})
	for _, needle := range []string{"reset-2fa", "os_user", "time", "host"} {
		if !strings.Contains(out, needle) {
			t.Errorf("日志里缺少 %q：\n%s", needle, out)
		}
	}

	// ③ 状态真的清掉了，而且审计里留了一条。
	db2, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("重新打开数据库: %v", err)
	}
	defer func() { _ = db2.Close() }()

	state, err := db2.TwoFactorState(ctx)
	if err != nil {
		t.Fatalf("读状态: %v", err)
	}
	if state.Enabled() {
		t.Fatalf("重置之后两步验证仍然是开着的：%+v", state)
	}
	if len(state.RecoveryHashes) != 0 || state.LastCounter != 0 {
		t.Fatalf("重置之后恢复码/计数器应当清空：%+v", state)
	}
	if _, ok, err := db2.GetSetting(ctx, store.KeyTwoFASecret); err != nil || ok {
		t.Fatalf("twofa_secret 应当从库里消失（ok=%v err=%v）", ok, err)
	}
	entries, err := db2.ListAudit(ctx, 50, 0)
	if err != nil {
		t.Fatalf("读审计: %v", err)
	}
	found := false
	for _, e := range entries {
		if e.Action == "twofa_reset" {
			found = true
			if !strings.Contains(e.Detail, "本机命令行") {
				t.Errorf("审计条目没说清是本机操作：%q", e.Detail)
			}
		}
	}
	if !found {
		t.Error("审计里没有 twofa_reset 这一条")
	}
}

// TestResetTwoFactorIsIdempotent 没开两步验证时也必须成功（否则运维会以为命令坏了）。
func TestResetTwoFactorIsIdempotent(t *testing.T) {
	dataDir := t.TempDir()
	db, err := store.Open(context.Background(), filepath.Join(dataDir, "probe.db"))
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("关闭数据库: %v", err)
	}
	out := captureStdout(t, func() {
		if err := run([]string{"--reset-2fa", "--data-dir", dataDir}); err != nil {
			t.Errorf("没有启用两步验证时 --reset-2fa 也应当成功: %v", err)
		}
	})
	if !strings.Contains(out, "changed=false") {
		t.Errorf("日志应当说明「这次没改到东西」：\n%s", out)
	}
}

// TestResetTwoFactorRejectsWrongDataDir 数据目录填错时必须直接报错。
//
// 为什么值得单独一条：store.Open 会"顺手建库"，所以少了这道检查时，
// 填错目录的表现为"重置成功、本来就没启用"，用户以为事情办好了 ——
// 而下次用正确的目录启动，两步验证还在那儿。
func TestResetTwoFactorRejectsWrongDataDir(t *testing.T) {
	dir := t.TempDir()
	err := run([]string{"--reset-2fa", "--data-dir", filepath.Join(dir, "typo-dir")})
	if err == nil {
		t.Fatal("--data-dir 指向一个没有数据库的目录时应当报错")
	}
	if !strings.Contains(err.Error(), "--data-dir") {
		t.Errorf("错误信息应当指出是 --data-dir 的问题：%v", err)
	}
	// 而且不许在错误的位置留下一个空库。
	if _, statErr := os.Stat(filepath.Join(dir, "typo-dir", "probe.db")); statErr == nil {
		t.Error("报错路径上不该创建出一个空数据库")
	}
}

// captureStdout 把标准输出接到管道上，返回这次调用写出来的内容。
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("创建管道: %v", err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		data, _ := io.ReadAll(r)
		done <- string(data)
	}()
	fn()
	_ = w.Close()
	os.Stdout = old
	return <-done
}
