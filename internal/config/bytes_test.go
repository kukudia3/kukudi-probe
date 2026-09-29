package config

import (
	"testing"
	"time"
)

func TestParseBytes(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"1TiB", 1 << 40},
		{"512GiB", 512 << 30},
		{"10MiB", 10 << 20},
		{"1KiB", 1 << 10},
		{"100B", 100},
		{"1048576", 1 << 20},
		{"1.5GiB", 1610612736},
		{"  2GiB  ", 2 << 30},
		{"0", 0},
	}
	for _, tc := range cases {
		got, err := ParseBytes(tc.in)
		if err != nil {
			t.Errorf("ParseBytes(%q) 报错: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseBytes(%q) = %d，期望 %d", tc.in, got, tc.want)
		}
	}

	for _, bad := range []string{"", "abc", "-1GiB", "1XB", "GiB"} {
		if _, err := ParseBytes(bad); err == nil {
			t.Errorf("ParseBytes(%q) 应当报错", bad)
		}
	}
}

func TestFormatBytes(t *testing.T) {
	cases := map[int64]string{
		1 << 40:    "1TiB",
		512 << 30:  "512GiB",
		1536 << 20: "1536MiB", // 不是整 GiB 就退到 MiB
		100:        "100B",
	}
	for in, want := range cases {
		if got := FormatBytes(in); got != want {
			t.Errorf("FormatBytes(%d) = %q，期望 %q", in, got, want)
		}
	}
}

func TestParseTrafficDeltaMaxFlag(t *testing.T) {
	cfg, err := Parse([]string{"--traffic-delta-max", "8GiB"}, func(string) string { return "" }, nil)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if cfg.TrafficDeltaMax != 8<<30 {
		t.Fatalf("TrafficDeltaMax = %d，期望 %d", cfg.TrafficDeltaMax, int64(8)<<30)
	}

	if _, err := Parse([]string{"--traffic-delta-max", "很大"}, func(string) string { return "" }, nil); err == nil {
		t.Fatal("非法值应当报错")
	}
}

func TestAgentConnectionLimits(t *testing.T) {
	// 默认值：单来源 20、总数 500。Phase 11 把这两个从硬编码常量改成可配置，
	// 因为多台机器在同一 NAT/反代后面时会看起来来自同一个 IP。
	cfg := Default()
	if cfg.AgentMaxPerIP != 20 || cfg.AgentMaxConns != 500 {
		t.Fatalf("默认上限 = %d/%d，期望 20/500", cfg.AgentMaxPerIP, cfg.AgentMaxConns)
	}

	parsed, err := Parse([]string{"--agent-max-per-ip", "60", "--agent-max-conns", "200"},
		func(string) string { return "" }, nil)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if parsed.AgentMaxPerIP != 60 || parsed.AgentMaxConns != 200 {
		t.Fatalf("参数没有生效: %d/%d", parsed.AgentMaxPerIP, parsed.AgentMaxConns)
	}

	// 非法值必须在启动时就被挡住，而不是等到 Agent 连不上才发现。
	for _, args := range [][]string{
		{"--agent-max-per-ip", "0"},
		{"--agent-max-conns", "0"},
		{"--agent-max-per-ip", "-5"},
		{"--agent-max-per-ip", "100", "--agent-max-conns", "50"}, // 每 IP 大于总数
	} {
		if _, err := Parse(args, func(string) string { return "" }, nil); err == nil {
			t.Errorf("%v 应当报错", args)
		}
	}
}

func TestAlertTimingParams(t *testing.T) {
	cfg := Default()
	if cfg.AlertCooldown != 30*time.Minute || cfg.AlertStartupGrace != time.Minute ||
		cfg.AlertDebounce != 2*time.Second || cfg.AlertRecoverStable != 30*time.Second {
		t.Fatalf("告警默认参数不对: %+v", cfg)
	}

	parsed, err := Parse([]string{
		"--alert-cooldown", "5m", "--alert-startup-grace", "0s",
		"--alert-debounce", "500ms", "--alert-recover-stable", "1m",
	}, func(string) string { return "" }, nil)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if parsed.AlertCooldown != 5*time.Minute || parsed.AlertStartupGrace != 0 ||
		parsed.AlertDebounce != 500*time.Millisecond || parsed.AlertRecoverStable != time.Minute {
		t.Fatalf("参数没有生效: %+v", parsed)
	}

	if _, err := Parse([]string{"--alert-startup-grace", "2h"}, func(string) string { return "" }, nil); err == nil {
		t.Error("静默期超过 1 小时应当报错")
	}
	if _, err := Parse([]string{"--alert-cooldown", "-1m"}, func(string) string { return "" }, nil); err == nil {
		t.Error("负的冷却时间应当报错")
	}
}

func TestFlushIntervalValidation(t *testing.T) {
	if _, err := Parse([]string{"--flush-interval", "500ms"}, func(string) string { return "" }, nil); err == nil {
		t.Fatal("小于 1s 应当报错")
	}
	if _, err := Parse([]string{"--flush-interval", "2m"}, func(string) string { return "" }, nil); err == nil {
		t.Fatal("大于 1m 应当报错")
	}
	cfg, err := Parse([]string{"--flush-interval", "30s"}, func(string) string { return "" }, nil)
	if err != nil || cfg.FlushInterval != 30*time.Second {
		t.Fatalf("合法值解析失败: %v %s", err, cfg.FlushInterval)
	}
}
