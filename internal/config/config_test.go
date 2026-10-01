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

// 汇率的两个开关：默认**开着**（多数机器能出网，外币价格应当自动折算），
// 关闭走 --fx=false 或 PROBE_FX=0，数据源可以用 --fx-rate-url 换成内网镜像。
//
// 这里钉的是"能不能关掉"这一件事：没有外网的机器上，一个关不掉的出网请求
// 就是每天一条失败日志，而用户完全没有办法。
func TestParseFXOptions(t *testing.T) {
	cfg, err := Parse(nil, func(string) string { return "" }, io.Discard)
	if err != nil {
		t.Fatalf("默认参数应当解析成功: %v", err)
	}
	if !cfg.FX {
		t.Error("默认应当开着汇率自动获取")
	}
	if urls := cfg.FXRateURLs(); urls != nil {
		t.Errorf("默认不该有自定义数据源（用内置那两个），实际 %v", urls)
	}

	off, err := Parse([]string{"--fx=false"}, func(string) string { return "" }, io.Discard)
	if err != nil {
		t.Fatalf("--fx=false 应当解析成功: %v", err)
	}
	if off.FX {
		t.Error("--fx=false 没有关掉汇率自动获取")
	}

	envOff, err := Parse(nil, func(k string) string {
		if k == "PROBE_FX" {
			return "0"
		}
		return ""
	}, io.Discard)
	if err != nil {
		t.Fatalf("PROBE_FX=0 应当解析成功: %v", err)
	}
	if envOff.FX {
		t.Error("PROBE_FX=0 没有关掉汇率自动获取（systemd drop-in 依赖这条路）")
	}

	custom, err := Parse([]string{"--fx-rate-url", "http://mirror.internal/fx, https://a.example/b"}, func(string) string { return "" }, io.Discard)
	if err != nil {
		t.Fatalf("自定义数据源应当解析成功: %v", err)
	}
	urls := custom.FXRateURLs()
	if len(urls) != 2 || urls[0] != "http://mirror.internal/fx" || urls[1] != "https://a.example/b" {
		t.Errorf("数据源解析结果 = %v，期望按顺序的两个地址（去掉空白）", urls)
	}

	for _, bad := range []string{
		"mirror.internal/fx",       // 没有协议头
		"ftp://mirror.internal/fx", // 不支持的协议
		"https://",                 // 没有主机名
	} {
		if _, err := Parse([]string{"--fx-rate-url", bad}, func(string) string { return "" }, io.Discard); err == nil {
			t.Errorf("--fx-rate-url %q 应当报错（否则运行期只会静默失败）", bad)
		}
	}
	if _, err := Parse(nil, func(k string) string {
		if k == "PROBE_FX" {
			return "maybe"
		}
		return ""
	}, io.Discard); err == nil {
		t.Error("PROBE_FX 取值不认识时应当报错，不能悄悄当成开着")
	}
}
