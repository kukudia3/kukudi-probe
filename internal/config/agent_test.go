package config

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestParseAgentDefaults(t *testing.T) {
	// 自检模式不需要服务端参数。
	cfg, err := ParseAgent([]string{"--print-json"}, func(string) string { return "" }, io.Discard)
	if err != nil {
		t.Fatalf("自检模式应当解析成功: %v", err)
	}
	if cfg.Root != "/" || cfg.Disk != "/" {
		t.Errorf("默认根目录/磁盘 = %q / %q", cfg.Root, cfg.Disk)
	}
	if cfg.StateDir != "/var/lib/probe-agent" {
		t.Errorf("默认状态目录 = %q", cfg.StateDir)
	}
	if cfg.Iface != "" {
		t.Errorf("默认应当自动探测网卡，实际 %q", cfg.Iface)
	}
	if cfg.Interval != time.Second {
		t.Errorf("默认间隔 = %s，期望 1s", cfg.Interval)
	}
	if cfg.Samples != 1 || !cfg.PrintJSON || cfg.Once {
		t.Errorf("默认采样数/print-json/once = %d / %v / %v", cfg.Samples, cfg.PrintJSON, cfg.Once)
	}
}

func TestParseAgentRequiresServerAndToken(t *testing.T) {
	// 正式运行必须有服务端与鉴权信息。
	if _, err := ParseAgent(nil, func(string) string { return "" }, io.Discard); err == nil {
		t.Fatal("缺少 --server 应当报错")
	}
	if _, err := ParseAgent([]string{"--server", "https://x.example"}, func(string) string { return "" }, io.Discard); err == nil {
		t.Fatal("缺少 Token 应当报错")
	}
	if _, err := ParseAgent([]string{
		"--server", "https://x.example", "--token", "a", "--token-file", "/tmp/t",
	}, func(string) string { return "" }, io.Discard); err == nil {
		t.Fatal("同时给出 --token 与 --token-file 应当报错")
	}
	// --once 也需要服务端参数（它要真的连一次）。
	if _, err := ParseAgent([]string{"--once"}, func(string) string { return "" }, io.Discard); err == nil {
		t.Fatal("--once 缺少 --server 应当报错")
	}
}

func TestParseAgentEnvThenFlagOverridesEnv(t *testing.T) {
	env := map[string]string{
		"PROBE_ROOT":      "/srv/snapshot",
		"PROBE_IFACE":     "eth0",
		"PROBE_LOG_LEVEL": "debug",
	}
	cfg, err := ParseAgent([]string{"--iface", "ens3", "--print-json"}, func(k string) string { return env[k] }, io.Discard)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if cfg.Iface != "ens3" {
		t.Errorf("命令行应当覆盖环境变量，实际 %q", cfg.Iface)
	}
	if cfg.Root != "/srv/snapshot" {
		t.Errorf("环境变量未生效，Root = %q", cfg.Root)
	}
	if cfg.LogLevel != "debug" || !cfg.PrintJSON {
		t.Errorf("解析结果 = %+v", cfg)
	}
}

func TestResolveTokenFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte("  pba_testtoken\n"), 0o600); err != nil {
		t.Fatalf("写入 Token 文件: %v", err)
	}

	cfg, err := ParseAgent([]string{
		"--server", "https://x.example", "--token-file", path, "--print-json",
	}, func(string) string { return "" }, io.Discard)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	token, warning, err := cfg.ResolveToken()
	if err != nil {
		t.Fatalf("读取 Token: %v", err)
	}
	if token != "pba_testtoken" {
		t.Fatalf("Token = %q（应当去掉首尾空白）", token)
	}
	if warning != "" {
		t.Errorf("0600 权限不应有警告，实际 %q", warning)
	}
}

func TestResolveTokenWarnsOnLoosePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上没有 POSIX 权限位")
	}
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("pba_x\n"), 0o644); err != nil {
		t.Fatalf("写入 Token 文件: %v", err)
	}
	cfg := DefaultAgent()
	cfg.TokenFile = path
	_, warning, err := cfg.ResolveToken()
	if err != nil {
		t.Fatalf("读取 Token: %v", err)
	}
	if warning == "" {
		t.Fatal("权限 0644 应当给出提示")
	}
}

func TestResolveTokenErrors(t *testing.T) {
	cfg := DefaultAgent()
	if _, _, err := cfg.ResolveToken(); err == nil {
		t.Fatal("没有 Token 时应当报错")
	}

	cfg.TokenFile = filepath.Join(t.TempDir(), "missing")
	if _, _, err := cfg.ResolveToken(); err == nil {
		t.Fatal("文件不存在时应当报错")
	}

	empty := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(empty, []byte("   \n"), 0o600); err != nil {
		t.Fatalf("写入文件: %v", err)
	}
	cfg.TokenFile = empty
	if _, _, err := cfg.ResolveToken(); err == nil {
		t.Fatal("空文件应当报错")
	}
}

func TestParseAgentRejectsBadInput(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"间隔过小", []string{"--interval", "500ms"}},
		{"间隔过大", []string{"--interval", "301s"}},
		{"采样数为零", []string{"--samples", "0"}},
		{"采样数过大", []string{"--samples", "99999"}},
		{"日志级别非法", []string{"--log-level", "loud"}},
		{"日志格式非法", []string{"--log-format", "xml"}},
		{"根目录为空", []string{"--root", ""}},
		{"磁盘路径为空", []string{"--disk", ""}},
		{"网卡名含斜杠", []string{"--iface", "../etc/passwd"}},
		{"网卡名为点点", []string{"--iface", ".."}},
		{"网卡名过长", []string{"--iface", "aaaaaaaaaaaaaaaaaaaa"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseAgent(tc.args, func(string) string { return "" }, io.Discard); err == nil {
				t.Fatal("期望报错，实际通过")
			}
		})
	}
}

func TestAgentPlatformSupported(t *testing.T) {
	cfg := DefaultAgent()
	if got, want := cfg.PlatformSupported(), runtime.GOOS == "linux"; got != want {
		t.Fatalf("PlatformSupported = %v，期望 %v（GOOS=%s）", got, want, runtime.GOOS)
	}
}
