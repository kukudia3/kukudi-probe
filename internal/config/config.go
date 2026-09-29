// Package config 解析 probe-server 的启动参数。
//
// 设计约定（docs/DESIGN.md §5）：只有命令行参数 + 环境变量，没有配置文件；
// 运行期可变的设置放在数据库里，由后台页面修改。
package config

import (
	"flag"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

// Server 是 probe-server 的全部启动参数。
type Server struct {
	Listen             string
	DataDir            string
	TLSCert            string
	TLSKey             string
	LogLevel           string
	LogFormat          string
	Timezone           string
	TrustedProxy       string
	Retention10s       time.Duration
	Retention1m        time.Duration
	FlushInterval      time.Duration
	StaleAfter         time.Duration
	OfflineAfter       time.Duration
	SetupCodeTTL       time.Duration
	TrafficDeltaMax    int64
	AlertCooldown      time.Duration
	AlertStartupGrace  time.Duration
	AlertDebounce      time.Duration
	AlertRecoverStable time.Duration
	AgentMaxPerIP      int
	AgentMaxConns      int
	ShutdownGrace      time.Duration
	ShowVersion        bool
}

// Default 返回默认参数。
//
// 默认只监听本机：公网暴露必须由 Caddy/nginx 提供 TLS，或显式给出 --tls-cert/--tls-key。
func Default() Server {
	return Server{
		Listen:             "127.0.0.1:25774",
		DataDir:            "data",
		LogLevel:           "info",
		LogFormat:          "text",
		Timezone:           "Local",
		Retention10s:       12 * time.Hour,
		Retention1m:        8 * 24 * time.Hour,
		FlushInterval:      10 * time.Second,
		StaleAfter:         10 * time.Second,
		OfflineAfter:       30 * time.Second,
		SetupCodeTTL:       30 * time.Minute,
		TrafficDeltaMax:    1 << 40, // 1 TiB
		AgentMaxPerIP:      20,
		AgentMaxConns:      500,
		AlertCooldown:      30 * time.Minute,
		AlertStartupGrace:  60 * time.Second,
		AlertDebounce:      2 * time.Second,
		AlertRecoverStable: 30 * time.Second,
		ShutdownGrace:      10 * time.Second,
	}
}

// Parse 解析参数。usageOut 接收错误与用法（测试时可传 io.Discard），
// lookupEnv 读取环境变量（测试时可注入假实现）。
func Parse(args []string, lookupEnv func(string) string, usageOut io.Writer) (Server, error) {
	if lookupEnv == nil {
		lookupEnv = func(string) string { return "" }
	}
	if usageOut == nil {
		usageOut = io.Discard
	}

	cfg := Default()
	cfg.Listen = envOr(lookupEnv, "PROBE_LISTEN", cfg.Listen)
	cfg.DataDir = envOr(lookupEnv, "PROBE_DATA_DIR", cfg.DataDir)
	cfg.TLSCert = envOr(lookupEnv, "PROBE_TLS_CERT", cfg.TLSCert)
	cfg.TLSKey = envOr(lookupEnv, "PROBE_TLS_KEY", cfg.TLSKey)
	cfg.LogLevel = envOr(lookupEnv, "PROBE_LOG_LEVEL", cfg.LogLevel)
	cfg.LogFormat = envOr(lookupEnv, "PROBE_LOG_FORMAT", cfg.LogFormat)
	cfg.Timezone = envOr(lookupEnv, "PROBE_TIMEZONE", cfg.Timezone)
	cfg.TrustedProxy = envOr(lookupEnv, "PROBE_TRUSTED_PROXY", cfg.TrustedProxy)

	fs := flag.NewFlagSet("probe-server", flag.ContinueOnError)
	fs.SetOutput(usageOut)
	fs.StringVar(&cfg.Listen, "listen", cfg.Listen, "监听地址，默认只监听本机")
	fs.StringVar(&cfg.DataDir, "data-dir", cfg.DataDir, "数据目录（SQLite 放在这里）")
	fs.StringVar(&cfg.TLSCert, "tls-cert", cfg.TLSCert, "TLS 证书路径（与 --tls-key 同时给出才生效）")
	fs.StringVar(&cfg.TLSKey, "tls-key", cfg.TLSKey, "TLS 私钥路径")
	fs.StringVar(&cfg.LogLevel, "log-level", cfg.LogLevel, "日志级别：debug|info|warn|error")
	fs.StringVar(&cfg.LogFormat, "log-format", cfg.LogFormat, "日志格式：text|json")
	fs.StringVar(&cfg.Timezone, "timezone", cfg.Timezone, "日流量与日期归属的时区，如 Local、Asia/Shanghai")
	fs.StringVar(&cfg.TrustedProxy, "trusted-proxy", cfg.TrustedProxy, "可信反向代理的 CIDR（逗号分隔，如 127.0.0.1/32）；只有来自这些地址的请求才采信 X-Forwarded-For")
	fs.DurationVar(&cfg.Retention10s, "retention-10s", cfg.Retention10s, "10 秒桶保留时长（0=不清理）")
	fs.DurationVar(&cfg.Retention1m, "retention-1m", cfg.Retention1m, "1 分钟桶保留时长（0=不清理）")
	fs.DurationVar(&cfg.FlushInterval, "flush-interval", cfg.FlushInterval, "内存聚合落盘周期（1s-60s）")
	fs.DurationVar(&cfg.StaleAfter, "stale-after", cfg.StaleAfter, "超过该时长无通信显示为抖动")
	fs.DurationVar(&cfg.OfflineAfter, "offline-after", cfg.OfflineAfter, "超过该时长无通信判定离线并告警")
	fs.DurationVar(&cfg.SetupCodeTTL, "setup-code-ttl", cfg.SetupCodeTTL, "首次初始化码的有效期")
	trafficDeltaMax := FormatBytes(cfg.TrafficDeltaMax)
	fs.StringVar(&trafficDeltaMax, "traffic-delta-max", trafficDeltaMax, "单次接受的流量增量上限（超过即重设基线），如 1TiB、512GiB")
	fs.DurationVar(&cfg.AlertCooldown, "alert-cooldown", cfg.AlertCooldown, "同一告警重复通知的最短间隔（节点仍异常时）")
	fs.DurationVar(&cfg.AlertStartupGrace, "alert-startup-grace", cfg.AlertStartupGrace, "服务端启动后不发通知的静默期")
	fs.DurationVar(&cfg.AlertDebounce, "alert-debounce", cfg.AlertDebounce, "连续离线多久才判定为真的离线（抗抖动）")
	fs.DurationVar(&cfg.AlertRecoverStable, "alert-recover-stable", cfg.AlertRecoverStable, "恢复后需要稳定在线多久才发「已恢复」")
	fs.IntVar(&cfg.AgentMaxPerIP, "agent-max-per-ip", cfg.AgentMaxPerIP, "同一来源最多允许的 Agent 连接数（多台机器在同一 NAT 后面时需要调大）")
	fs.IntVar(&cfg.AgentMaxConns, "agent-max-conns", cfg.AgentMaxConns, "Agent 连接总数上限")
	fs.DurationVar(&cfg.ShutdownGrace, "shutdown-grace", cfg.ShutdownGrace, "收到退出信号后的最长等待时间")
	fs.BoolVar(&cfg.ShowVersion, "version", false, "打印版本后退出")

	if err := fs.Parse(args); err != nil {
		return Server{}, err
	}
	parsed, err := ParseBytes(trafficDeltaMax)
	if err != nil {
		return Server{}, fmt.Errorf("无效的 --traffic-delta-max %q: %w", trafficDeltaMax, err)
	}
	cfg.TrafficDeltaMax = parsed
	if err := cfg.validate(); err != nil {
		return Server{}, err
	}
	return cfg, nil
}

// LoopbackListen 报告监听地址是否只对本机开放。
func (c Server) LoopbackListen() bool {
	host, _, err := net.SplitHostPort(c.Listen)
	if err != nil || host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (c Server) validate() error {
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("无效的 --log-level %q（可选 debug|info|warn|error）", c.LogLevel)
	}
	switch c.LogFormat {
	case "text", "json":
	default:
		return fmt.Errorf("无效的 --log-format %q（可选 text|json）", c.LogFormat)
	}
	if _, err := time.LoadLocation(c.Timezone); err != nil {
		return fmt.Errorf("无效的 --timezone %q: %w", c.Timezone, err)
	}
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		return fmt.Errorf("无效的 --listen %q: %w", c.Listen, err)
	}
	if (c.TLSCert == "") != (c.TLSKey == "") {
		return fmt.Errorf("--tls-cert 与 --tls-key 必须同时提供")
	}
	if c.DataDir == "" {
		return fmt.Errorf("--data-dir 不能为空")
	}
	if c.StaleAfter <= 0 || c.OfflineAfter <= c.StaleAfter {
		return fmt.Errorf("要求 0 < --stale-after < --offline-after（当前 %s / %s）", c.StaleAfter, c.OfflineAfter)
	}
	if c.Retention10s < 0 || c.Retention1m < 0 {
		return fmt.Errorf("保留时长不能为负")
	}
	if c.FlushInterval < time.Second || c.FlushInterval > time.Minute {
		return fmt.Errorf("--flush-interval 必须在 1s 到 60s 之间（当前 %s）", c.FlushInterval)
	}
	if c.AlertCooldown < 0 {
		return fmt.Errorf("--alert-cooldown 不能为负")
	}
	if c.AlertStartupGrace < 0 || c.AlertStartupGrace > time.Hour {
		return fmt.Errorf("--alert-startup-grace 必须在 0 到 1 小时之间（当前 %s）", c.AlertStartupGrace)
	}
	if c.AlertDebounce < 0 || c.AlertRecoverStable < 0 {
		return fmt.Errorf("--alert-debounce 与 --alert-recover-stable 不能为负")
	}
	if c.AgentMaxPerIP < 1 || c.AgentMaxConns < 1 {
		return fmt.Errorf("--agent-max-per-ip 与 --agent-max-conns 至少为 1")
	}
	if c.AgentMaxPerIP > c.AgentMaxConns {
		return fmt.Errorf("--agent-max-per-ip（%d）不能大于 --agent-max-conns（%d）", c.AgentMaxPerIP, c.AgentMaxConns)
	}
	if c.ShutdownGrace <= 0 {
		return fmt.Errorf("--shutdown-grace 必须为正")
	}
	if c.SetupCodeTTL < time.Minute || c.SetupCodeTTL > 24*time.Hour {
		return fmt.Errorf("--setup-code-ttl 必须在 1 分钟到 24 小时之间（当前 %s）", c.SetupCodeTTL)
	}
	if _, err := ParseTrustedProxies(c.TrustedProxy); err != nil {
		return err
	}
	return nil
}

// ParseTrustedProxies 解析逗号分隔的 CIDR 列表；单个 IP 会按 /32 或 /128 处理。
func ParseTrustedProxies(spec string) ([]*net.IPNet, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, nil
	}
	var nets []*net.IPNet
	for _, item := range strings.Split(spec, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if !strings.Contains(item, "/") {
			ip := net.ParseIP(item)
			if ip == nil {
				return nil, fmt.Errorf("无效的 --trusted-proxy %q", item)
			}
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			item = fmt.Sprintf("%s/%d", ip.String(), bits)
		}
		_, ipnet, err := net.ParseCIDR(item)
		if err != nil {
			return nil, fmt.Errorf("无效的 --trusted-proxy %q: %w", item, err)
		}
		nets = append(nets, ipnet)
	}
	return nets, nil
}

func envOr(lookupEnv func(string) string, key, fallback string) string {
	if v := lookupEnv(key); v != "" {
		return v
	}
	return fallback
}
