package config

import (
	"io"
	"testing"
	"time"
)

func TestParseDefaults(t *testing.T) {
	cfg, err := Parse(nil, func(string) string { return "" }, io.Discard)
	if err != nil {
		t.Fatalf("默认参数应当解析成功，实际报错: %v", err)
	}
	if cfg.Listen != "127.0.0.1:25774" {
		t.Errorf("默认监听 = %q，期望 127.0.0.1:25774", cfg.Listen)
	}
	if !cfg.LoopbackListen() {
		t.Error("默认监听必须只对本机开放")
	}
	if cfg.Retention10s != 12*time.Hour || cfg.Retention1m != 8*24*time.Hour {
		t.Errorf("默认保留时长 = %s / %s", cfg.Retention10s, cfg.Retention1m)
	}
	if cfg.StaleAfter != 10*time.Second || cfg.OfflineAfter != 30*time.Second {
		t.Errorf("默认在线判定阈值 = %s / %s", cfg.StaleAfter, cfg.OfflineAfter)
	}
	if cfg.DataDir != "data" {
		t.Errorf("默认数据目录 = %q", cfg.DataDir)
	}
}

func TestParseEnvThenFlagOverridesEnv(t *testing.T) {
	env := map[string]string{
		"PROBE_LISTEN":    "0.0.0.0:9999",
		"PROBE_LOG_LEVEL": "debug",
	}
	lookup := func(k string) string { return env[k] }

	cfg, err := Parse([]string{"--listen", "127.0.0.1:1234"}, lookup, io.Discard)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if cfg.Listen != "127.0.0.1:1234" {
		t.Errorf("命令行应当覆盖环境变量，实际 = %q", cfg.Listen)
	}
	if !cfg.LoopbackListen() {
		t.Error("127.0.0.1:1234 应当判定为仅本机")
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("环境变量未生效，LogLevel = %q", cfg.LogLevel)
	}
}

func TestParseRejectsBadInput(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"日志级别非法", []string{"--log-level", "loud"}},
		{"日志格式非法", []string{"--log-format", "xml"}},
		{"时区非法", []string{"--timezone", "Mars/Olympus"}},
		{"监听地址缺端口", []string{"--listen", "127.0.0.1"}},
		{"只给证书不给私钥", []string{"--tls-cert", "cert.pem"}},
		{"数据目录为空", []string{"--data-dir", ""}},
		{"离线阈值不大于抖动阈值", []string{"--stale-after", "60s", "--offline-after", "30s"}},
		{"保留时长为负", []string{"--retention-10s", "-1h"}},
		{"退出等待为负", []string{"--shutdown-grace", "-1s"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse(tc.args, func(string) string { return "" }, io.Discard); err == nil {
				t.Fatal("期望报错，实际通过")
			}
		})
	}
}

func TestLoopbackListen(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:25774": true,
		"localhost:25774": true,
		"[::1]:25774":     true,
		"0.0.0.0:25774":   false,
		":25774":          false,
		"10.0.0.5:25774":  false,
	}
	for listen, want := range cases {
		cfg := Default()
		cfg.Listen = listen
		if got := cfg.LoopbackListen(); got != want {
			t.Errorf("LoopbackListen(%q) = %v，期望 %v", listen, got, want)
		}
	}
}
