package config

import (
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"time"
)

// Agent 是 probe-agent 的全部启动参数。
//
// 与 Server 一样：只有命令行 + 环境变量，没有配置文件。
type Agent struct {
	Server             string
	Token              string
	TokenFile          string
	Name               string
	StateDir           string
	Root               string
	Iface              string
	Disk               string
	LogLevel           string
	LogFormat          string
	Interval           time.Duration
	Samples            int
	PrintJSON          bool
	Once               bool
	InsecureSkipVerify bool
	AllowPlaintext     bool
	Version            bool
}

// 默认状态目录（systemd 单元里也会指向同一位置）。
const defaultStateDir = "/var/lib/probe-agent"

// DefaultAgent 返回默认参数。
func DefaultAgent() Agent {
	return Agent{
		StateDir:  defaultStateDir,
		Root:      "/",
		Disk:      "/",
		LogLevel:  "info",
		LogFormat: "text",
		Interval:  time.Second,
		Samples:   1,
	}
}

// ParseAgent 解析 probe-agent 的参数。
func ParseAgent(args []string, lookupEnv func(string) string, usageOut io.Writer) (Agent, error) {
	if lookupEnv == nil {
		lookupEnv = func(string) string { return "" }
	}
	if usageOut == nil {
		usageOut = io.Discard
	}

	cfg := DefaultAgent()
	cfg.Server = envOr(lookupEnv, "PROBE_SERVER", cfg.Server)
	cfg.Token = envOr(lookupEnv, "PROBE_TOKEN", cfg.Token)
	cfg.TokenFile = envOr(lookupEnv, "PROBE_TOKEN_FILE", cfg.TokenFile)
	cfg.Name = envOr(lookupEnv, "PROBE_NAME", cfg.Name)
	cfg.StateDir = envOr(lookupEnv, "PROBE_STATE_DIR", cfg.StateDir)
	cfg.Root = envOr(lookupEnv, "PROBE_ROOT", cfg.Root)
	cfg.Iface = envOr(lookupEnv, "PROBE_IFACE", cfg.Iface)
	cfg.Disk = envOr(lookupEnv, "PROBE_DISK", cfg.Disk)
	cfg.LogLevel = envOr(lookupEnv, "PROBE_LOG_LEVEL", cfg.LogLevel)
	cfg.LogFormat = envOr(lookupEnv, "PROBE_LOG_FORMAT", cfg.LogFormat)

	fs := flag.NewFlagSet("probe-agent", flag.ContinueOnError)
	fs.SetOutput(usageOut)
	fs.StringVar(&cfg.Server, "server", cfg.Server, "服务端地址，如 https://monitor.example.com（必须是 https）")
	fs.StringVar(&cfg.TokenFile, "token-file", cfg.TokenFile, "Token 文件路径（推荐：0600 权限，避免出现在 ps 里）")
	fs.StringVar(&cfg.Token, "token", cfg.Token, "Token 明文（会出现在 ps 输出中，不推荐）")
	fs.StringVar(&cfg.Name, "name", cfg.Name, "节点显示名建议（仅首次注册时使用）")
	fs.StringVar(&cfg.StateDir, "state-dir", cfg.StateDir, "状态目录（流量 checkpoint 落在这里）")
	fs.StringVar(&cfg.Root, "root", cfg.Root, "根目录，其下应有 proc/ 与 etc/（诊断时可指向一份根文件系统快照）")
	fs.StringVar(&cfg.Iface, "iface", cfg.Iface, "要监控的网卡，留空=自动探测默认路由网卡")
	fs.StringVar(&cfg.Disk, "disk", cfg.Disk, "主文件系统路径，用于磁盘占用统计")
	fs.StringVar(&cfg.LogLevel, "log-level", cfg.LogLevel, "日志级别：debug|info|warn|error")
	fs.StringVar(&cfg.LogFormat, "log-format", cfg.LogFormat, "日志格式：text|json")
	fs.DurationVar(&cfg.Interval, "interval", cfg.Interval, "采集/上报间隔（1s-300s，实际以服务端下发为准）")
	fs.IntVar(&cfg.Samples, "samples", cfg.Samples, "配合 --print-json：连续打印几份采样后退出")
	fs.BoolVar(&cfg.PrintJSON, "print-json", false, "打印一次采集结果后退出（自检/排障，可在任何平台运行）")
	fs.BoolVar(&cfg.Once, "once", false, "连上服务端上报一次后退出（用于验证 Token 与网络）")
	fs.BoolVar(&cfg.InsecureSkipVerify, "insecure-skip-verify", false, "跳过 TLS 证书校验（不推荐，会被中间人攻击）")
	fs.BoolVar(&cfg.AllowPlaintext, "allow-plaintext", false, "允许用明文 ws:// 连接非本机地址（不推荐）")
	fs.BoolVar(&cfg.Version, "version", false, "打印版本后退出")

	if err := fs.Parse(args); err != nil {
		return Agent{}, err
	}
	if err := cfg.validateAgent(); err != nil {
		return Agent{}, err
	}
	return cfg, nil
}

func (c Agent) validateAgent() error {
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
	if c.Interval < time.Second || c.Interval > 300*time.Second {
		return fmt.Errorf("--interval 必须在 1s 到 300s 之间（当前 %s）", c.Interval)
	}
	if c.Samples < 1 || c.Samples > 3600 {
		return fmt.Errorf("--samples 必须在 1 到 3600 之间（当前 %d）", c.Samples)
	}
	if c.Root == "" {
		return fmt.Errorf("--root 不能为空")
	}
	if c.Disk == "" {
		return fmt.Errorf("--disk 不能为空")
	}
	if c.StateDir == "" {
		return fmt.Errorf("--state-dir 不能为空")
	}
	if len(c.Name) > 64 {
		return fmt.Errorf("--name 长度超过 64")
	}
	if c.Iface != "" {
		// 网卡名会被用来拼接 /sys/class/net/<name> 路径，必须挡掉路径穿越。
		if c.Iface == "." || c.Iface == ".." || strings.ContainsAny(c.Iface, `/\`) {
			return fmt.Errorf("无效的 --iface %q", c.Iface)
		}
		if len(c.Iface) > 15 { // Linux IFNAMSIZ
			return fmt.Errorf("--iface 过长（Linux 网卡名最多 15 字节）")
		}
	}
	// 自检模式不需要服务端参数。
	if c.PrintJSON && !c.Once {
		return nil
	}
	if strings.TrimSpace(c.Server) == "" {
		return fmt.Errorf("缺少 --server（服务端地址，如 https://monitor.example.com）")
	}
	if c.Token != "" && c.TokenFile != "" {
		return fmt.Errorf("--token 与 --token-file 只能给一个（推荐 --token-file）")
	}
	if c.Token == "" && c.TokenFile == "" {
		return fmt.Errorf("缺少 Token：请提供 --token-file 或 --token")
	}
	return nil
}

// ResolveToken 返回实际使用的 Token。
//
// 第二个返回值是给调用方打印的提示（例如文件权限过于宽松），不是错误。
func (c Agent) ResolveToken() (string, string, error) {
	if c.Token != "" {
		return c.Token, "", nil
	}
	if c.TokenFile == "" {
		return "", "", fmt.Errorf("未提供 Token")
	}
	info, err := os.Stat(c.TokenFile)
	if err != nil {
		return "", "", fmt.Errorf("读取 Token 文件失败: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", "", fmt.Errorf("Token 文件 %s 不是普通文件", c.TokenFile)
	}
	data, err := os.ReadFile(c.TokenFile)
	if err != nil {
		return "", "", fmt.Errorf("读取 Token 文件失败: %w", err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", "", fmt.Errorf("Token 文件 %s 内容为空", c.TokenFile)
	}
	warning := ""
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		warning = fmt.Sprintf("Token 文件 %s 权限为 %o，建议改为 0600", c.TokenFile, info.Mode().Perm())
	}
	return token, warning, nil
}

// PlatformSupported 报告当前平台是否支持正式运行（只支持 Linux）。
func (c Agent) PlatformSupported() bool {
	return runtime.GOOS == "linux"
}
