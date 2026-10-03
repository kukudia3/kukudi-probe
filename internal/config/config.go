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
	"net/url"
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

	// FX 控制"每天取一次汇率"这个功能；FXRateURL 覆盖数据源。
	//
	// 为什么要有一个开关：探针服务端本身**不需要**出网就能跑完所有核心功能
	// （收数据、落库、推 SSE、发 Telegram 由用户自己配）。汇率只影响"外币价格
	// 折成人民币"这一个显示口径，在没有外网的机器上（内网、白名单防火墙）
	// 反复失败只会让日志变脏。关掉之后价格照常显示：一直用上次取到的值，
	// 从没取到过就用内置兜底表（见 internal/fx 的 Default）。
	FX bool
	// FXRateURL 是自定义数据源（逗号分隔，按顺序试）。留空 = 用内置的两个。
	// 给内网镜像 / 自建代理用，也便于在测试里指向本地 httptest 服务。
	FXRateURL string

	// Gzip 控制是否给 HTTP 响应做 gzip 压缩（默认开）。
	//
	// 为什么要这个开关：实测（真服务端 + 真 Agent + 计数 TCP 代理）里，
	// **浏览器→面板**的流量比 Agent 上报大一个数量级 —— 面板开着 SSE 常连时，
	// 1 个节点 139 MB/天、20 个节点 2.54 GB/天，而这些都是 JSON 与 JS 文本
	// （压缩比通常 2~8 倍）；首屏那 376 KB 静态资源同理。
	//
	// 但仍然留一个"能关掉"的口子：万一撞上某个中间设备/客户端对 gzip 处理有问题
	// （老式反代、某些抓包工具、嵌入式浏览器），运维必须能一条 drop-in 关掉它，
	// 而不是被迫降级版本。关掉写 --gzip=false 或 PROBE_GZIP=0。
	Gzip bool

	// HSTS 控制"反代终止 TLS 时要不要由应用下发 HSTS"（默认**关**）。
	//
	// 进程内 TLS（--tls-cert/--tls-key）时一直会下发，与这个开关无关；
	// 这里是推荐部署（Caddy/nginx 终止 TLS、到达进程的是明文 HTTP）的补充：
	// 打开后，请求来自可信代理且 X-Forwarded-Proto: https 时，
	// 应用会补上 Strict-Transport-Security。
	//
	// 为什么默认关：一年的 includeSubDomains 是**不可撤销**的 ——
	// 一旦下发，浏览器在有效期内只走 https。如果反代同时提供 http 且没有 301，
	// 或者同一个域下还有只支持 http 的兄弟子域，用户会被直接挡在门外，
	// 而应用看不到反代那边的配置。这个决定必须由运维显式来下（写
	// --hsts 或 PROBE_HSTS=1）。更稳妥的做法是在反代那一层加响应头，
	// 见 docs/DEPLOY.md §4.5。
	HSTS bool

	ShowVersion bool

	// Reset2FA 是"在本机把两步验证关掉"的救援开关（--reset-2fa）。
	//
	// 为什么必须有它：用户可能同时忘掉面板密码与验证器，这时面板是一个
	// 打不开的盒子。这条路刻意**不校验任何凭据**（要校验密码的话，
	// 而密码正是忘掉的那个，就等于没有出路），代价是它只应该由
	// "能在服务器上执行命令的人"使用 —— 而那本来就是这台机器的所有人。
	// 用法见 docs/DEPLOY.md，执行时会往日志与审计里各写一条。
	Reset2FA bool
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
		FX:                 true,
		Gzip:               true,
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
	cfg.FXRateURL = envOr(lookupEnv, "PROBE_FX_RATE_URL", cfg.FXRateURL)
	fxOn, err := envBool(lookupEnv, "PROBE_FX", cfg.FX)
	if err != nil {
		return Server{}, err
	}
	cfg.FX = fxOn
	gzipOn, err := envBool(lookupEnv, "PROBE_GZIP", cfg.Gzip)
	if err != nil {
		return Server{}, err
	}
	cfg.Gzip = gzipOn
	// 与 --gzip 同一套写法：正向开关、默认关；PROBE_HSTS=1 与 --hsts 等价。
	hstsOn, err := envBool(lookupEnv, "PROBE_HSTS", cfg.HSTS)
	if err != nil {
		return Server{}, err
	}
	cfg.HSTS = hstsOn

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
	// 汇率的两个开关。为什么用"正向的 --fx（默认开）"而不是 --no-fx：
	// 环境变量 PROBE_FX 与命令行 --fx 两边语义一致（都是"要不要取汇率"），
	// 不必在脑子里做一次取反；关掉写 --fx=false 或 PROBE_FX=0。
	fs.BoolVar(&cfg.FX, "fx", cfg.FX, "是否每天自动获取汇率（用于把外币价格折算成人民币）；关掉后一直用上次取到的值或内置兜底表")
	fs.StringVar(&cfg.FXRateURL, "fx-rate-url", cfg.FXRateURL, "自定义汇率数据源（逗号分隔，按顺序试；留空用内置的两个公开源）")
	// 与 --fx 同一套写法：正向开关、默认开，命令行与环境变量语义一致
	//（PROBE_GZIP=0 就是 --gzip=false），关掉不需要在脑子里做一次取反。
	fs.BoolVar(&cfg.Gzip, "gzip", cfg.Gzip, "是否对 HTTP 响应做 gzip 压缩（浏览器→面板的流量主要是 JSON 与前端静态资源，压缩比通常 2~8 倍）；关掉写 --gzip=false 或 PROBE_GZIP=0")
	fs.BoolVar(&cfg.HSTS, "hsts", cfg.HSTS, "反代终止 TLS 时也由应用下发 Strict-Transport-Security（需要 --trusted-proxy 覆盖反代地址，且反代要发 X-Forwarded-Proto）；默认关：一年的 includeSubDomains 是不可撤销的，反代那边 http 没有 301 时会让面板打不开")
	fs.BoolVar(&cfg.ShowVersion, "version", false, "打印版本后退出")
	fs.BoolVar(&cfg.Reset2FA, "reset-2fa", false, "在本机关闭两步验证（忘了密码又丢了验证器时的救援命令；只改数据库，不启动服务）")

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
	for _, u := range c.FXRateURLs() {
		parsed, err := url.Parse(u)
		if err != nil {
			return fmt.Errorf("无效的 --fx-rate-url %q: %w", u, err)
		}
		if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return fmt.Errorf("无效的 --fx-rate-url %q：必须是 http:// 或 https:// 开头的完整地址", u)
		}
	}
	return nil
}

// FXRateURLs 把 --fx-rate-url 拆成按顺序尝试的数据源列表；留空返回 nil
// （由 internal/fx 用它内置的那两个公开源）。
func (c Server) FXRateURLs() []string {
	spec := strings.TrimSpace(c.FXRateURL)
	if spec == "" {
		return nil
	}
	var urls []string
	for _, item := range strings.Split(spec, ",") {
		if item = strings.TrimSpace(item); item != "" {
			urls = append(urls, item)
		}
	}
	return urls
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

// envBool 读一个布尔环境变量（系统单元里用 Environment=PROBE_FX=0 这种写法）。
//
// 取值不认识时**报错**而不是悄悄用默认值：把 PROBE_FX=nope 当成"开着"，
// 在没有外网的机器上就是每天一条失败日志，而用户以为自己已经关掉了。
func envBool(lookupEnv func(string) string, key string, fallback bool) (bool, error) {
	raw := strings.TrimSpace(lookupEnv(key))
	if raw == "" {
		return fallback, nil
	}
	switch strings.ToLower(raw) {
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	default:
		return false, fmt.Errorf("无效的 %s=%q（可选 1/0、true/false、yes/no、on/off）", key, raw)
	}
}
