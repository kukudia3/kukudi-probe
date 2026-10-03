package agent

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"probe/internal/protocol"
)

const (
	clientSubprotocol = "probe.v1"
	// pingInterval 是测延迟与保活的应用层 ping 间隔。
	pingInterval = 5 * time.Second
	// handshakeTimeout 覆盖拨号与 hello/welcome 交换。
	handshakeTimeout = 15 * time.Second
	// 重连退避：正常抖动从 1s 起翻倍到 60s；认证/停用/协议不兼容用长间隔。
	initialBackoff     = 1 * time.Second
	maxBackoff         = 60 * time.Second
	authBackoff        = 5 * time.Minute
	disabledBackoff    = 10 * time.Minute
	versionBackoff     = 30 * time.Minute
	sessionStableAfter = 60 * time.Second
	// warnRepeat 是同类采集告警的最短重复间隔，避免日志被同一条刷屏。
	warnRepeat = 5 * time.Minute
	// clientWriteTimeout 是单帧写入上限。
	clientWriteTimeout = 5 * time.Second
	// maxWarnKeys 限制告警去重表的规模（告警文本里可能带路径等变量）。
	maxWarnKeys = 64
	// maxHandshakeRedirects 是握手重定向的跳数上限。
	//
	// 为什么自己写一遍：websocket.Dial 会在传给它的 http.Client 外面再包一层
	// CheckRedirect（coder/websocket dial.go 的 cloneWithDefaults），那层只把
	// Location 里的 ws/wss 改写成 http/https 然后放行 —— 一旦它接手，
	// net/http 的默认上限（defaultCheckRedirect 的 10 跳）就不再生效。
	// 也就是说"跟随重定向"这件事的**全部**策略（跳数上限 + 明文拦截）都落在这里。
	maxHandshakeRedirects = 10
)

// errOnceDone 表示 --once 模式已完成一次上报。
var errOnceDone = errors.New("已完成单次上报")

// permanentError 表示重试也没有意义的错误（配置写错了），Run 会直接返回。
type permanentError struct{ err error }

func (e *permanentError) Error() string { return e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }

// statusError 表示拨号被服务端以 HTTP 状态码拒绝。
type statusError struct{ code int }

func (e *statusError) Error() string { return fmt.Sprintf("服务端拒绝连接：HTTP %d", e.code) }

// serverError 表示服务端下发的 fatal 错误帧。
type serverError struct {
	code    string
	message string
}

func (e *serverError) Error() string {
	return fmt.Sprintf("服务端返回致命错误 %s：%s", e.code, e.message)
}

// ClientConfig 是上报链路的配置。
type ClientConfig struct {
	ServerURL          string
	Token              string
	InsecureSkipVerify bool
	AllowPlaintext     bool
	Once               bool
}

// Client 负责一条 Agent→Server 的连接：鉴权、上报、心跳、重连。
//
// 发送是同步的：没有队列、没有缓存。网络慢时宁可跳过这一拍（计入 dropped），
// 也不在内存里堆积数据——见 docs/DESIGN.md §8。
type Client struct {
	cfg       ClientConfig
	log       *slog.Logger
	collector *Collector
	traffic   *Traffic
	http      *http.Client

	// pings 是延迟探测器：目标与间隔由服务端下发的 config 帧决定。
	pings *Prober

	// writeMu 保护连接写入：上报循环与 pong/ack 都在写同一连接。
	writeMu sync.Mutex
	// mu 保护下面这些运行态。
	mu       sync.Mutex
	interval time.Duration
	latMS    float64
	dropped  uint64
	seq      uint64
	// cfgVer 是已应用的配置版本，用来丢弃过期的 config 帧（每个会话开头清零）。
	cfgVer   int64
	lastWarn map[string]time.Time

	// pingEvery 是应用层 ping 间隔（测试可缩短）。
	pingEvery time.Duration
}

// NewClient 构造客户端。
func NewClient(cfg ClientConfig, collector *Collector, traffic *Traffic, logger *slog.Logger) *Client {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout: 10 * time.Second,
		TLSClientConfig: &tls.Config{
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: cfg.InsecureSkipVerify, // 仅在用户显式要求时关闭校验
		},
	}
	c := &Client{
		cfg:       cfg,
		log:       logger,
		collector: collector,
		traffic:   traffic,
		pings:     NewProber(logger),
		// 注意：这里绝不能设置 Client.Timeout——WebSocket 是长连接。
		http:     &http.Client{Transport: transport},
		interval: time.Second,
		lastWarn: make(map[string]time.Time),

		pingEvery: pingInterval,
	}
	// 明文拦截必须覆盖重定向的每一跳，不能只拦 wsURL 构造出来的那个初始地址
	// （审计 03-A-5，见 checkRedirect）。
	c.http.CheckRedirect = c.checkRedirect
	return c
}

// checkRedirect 对握手重定向执行与 wsURL 完全相同的明文策略。
//
// 背景（审计 03-A-5）：wsURL 只在**构造初始 URL**时拦明文，而 websocket.Dial
// 默认跟随重定向，于是服务端回一个 `302 Location: http://<其它地址>/...`
// 就能让 Agent 连到一个非环回的明文地址 —— `--allow-plaintext` 声称的
// "只连本机明文"被一次重定向破掉（同域跳转还会把 Bearer Token 一起明文送出去）。
//
// 这里选"照旧跟随、但每一跳都复核策略"，而不是"一律拒绝跟随"：
//   - 拒绝跟随会打断正在用重定向做路径规范化或 http→https 跳转的部署
//     （那些部署今天是能连上的，属于功能回归）；
//   - 明文判断与 wsURL 共用一套（`--allow-plaintext` 或本机环回），所以合法部署的
//     行为一个字节都不变，被挡掉的只有"重定向到明文外网地址"这一类。
//
// 跳数上限也要自己补：库那层装了 CheckRedirect 之后，net/http 的
// defaultCheckRedirect（10 跳）就不再被调用，不补就是一个无上限的跟随链。
//
// 关于返回的错误类型：**不**标成 permanentError。这类拒绝最常见的来源是反代配置
// 被改错（例如把 wss 路径 302 到了 http 后端），让 Run 按常规退避继续重试
// （最长 60 秒一次）比让进程直接退出更符合"可自愈"：配置改回来就恢复，
// 不必人肉重启每一台被监控机。原因写在错误里 —— 它会随"连接中断，稍后重连"
// 那条 WARN 一起进日志（websocket.Dial 在握手请求失败时**不**返回响应，
// 所以这条错误会原样传上来，不会被换成一句 "HTTP 302"）。
func (c *Client) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxHandshakeRedirects {
		return fmt.Errorf("握手重定向超过 %d 跳，已放弃", maxHandshakeRedirects)
	}
	// 注意：库那层已经把 Location 里的 ws/wss 改写成了 http/https，所以到这里的
	// "明文"就是 http（与 wsURL 里的判断一一对应）。
	if req.URL.Scheme == "http" && !c.cfg.AllowPlaintext && !isLoopbackHost(req.URL.Hostname()) {
		return fmt.Errorf("拒绝跟随重定向到明文地址 %s：请改用 https/wss，或显式加 --allow-plaintext", req.URL.Host)
	}
	return nil
}

// Run 持续保持连接，直到 ctx 结束或遇到不可恢复的错误。
func (c *Client) Run(ctx context.Context) error {
	backoff := initialBackoff
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		start := time.Now()
		err := c.session(ctx)
		lifetime := time.Since(start)

		if err == nil {
			return nil // --once 完成，或 ctx 被取消
		}
		if ctx.Err() != nil {
			return nil
		}
		var perm *permanentError
		if errors.As(err, &perm) {
			return perm.err
		}

		wait := c.nextBackoff(err, lifetime, &backoff)
		c.log.Warn("连接中断，稍后重连", "err", err, "retry_in", wait.Round(time.Second).String())
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
	}
}

// nextBackoff 计算下次重连等待时间。
func (c *Client) nextBackoff(err error, lifetime time.Duration, backoff *time.Duration) time.Duration {
	switch {
	case isAuthError(err):
		return authBackoff
	case isDisabledError(err):
		return disabledBackoff
	case isVersionError(err):
		return versionBackoff
	}
	// 稳定运行过一段时间的连接断开（例如服务端重启），立刻重试。
	if lifetime >= sessionStableAfter {
		*backoff = initialBackoff
	}
	wait := jitter(*backoff)
	if next := *backoff * 2; next <= maxBackoff {
		*backoff = next
	} else {
		*backoff = maxBackoff
	}
	return wait
}

func jitter(d time.Duration) time.Duration {
	factor := 0.8 + 0.4*rand.Float64() //nolint:gosec // 只用于错开重连时间，不涉及安全
	return time.Duration(float64(d) * factor)
}

func isAuthError(err error) bool {
	var se *statusError
	if errors.As(err, &se) {
		return se.code == http.StatusUnauthorized
	}
	return websocket.CloseStatus(err) == websocket.StatusCode(protocol.CloseUnauthorized)
}

func isDisabledError(err error) bool {
	var se *statusError
	if errors.As(err, &se) {
		return se.code == http.StatusForbidden
	}
	return websocket.CloseStatus(err) == websocket.StatusCode(protocol.CloseNodeDisabled)
}

func isVersionError(err error) bool {
	var se *serverError
	if errors.As(err, &se) {
		return se.code == protocol.CodeUpgradeRequired
	}
	return errors.Is(err, protocol.ErrVersionMismatch) ||
		websocket.CloseStatus(err) == websocket.StatusCode(protocol.CloseUpgradeRequired)
}

// wsURL 把配置里的地址规范化为 WebSocket 地址，并在这里做明文连接的拦截。
func (c *Client) wsURL() (string, error) {
	u, err := url.Parse(strings.TrimSpace(c.cfg.ServerURL))
	if err != nil {
		return "", &permanentError{fmt.Errorf("服务端地址不合法: %w", err)}
	}
	switch strings.ToLower(u.Scheme) {
	case "https", "wss":
		u.Scheme = "wss"
	case "http", "ws":
		if !c.cfg.AllowPlaintext && !isLoopbackHost(u.Hostname()) {
			return "", &permanentError{fmt.Errorf(
				"拒绝以明文连接非本机地址 %s：请改用 https/wss，或显式加 --allow-plaintext", u.Host)}
		}
		u.Scheme = "ws"
	default:
		return "", &permanentError{fmt.Errorf("不支持的地址协议 %q（需要 https/wss）", u.Scheme)}
	}
	if u.Host == "" {
		return "", &permanentError{errors.New("服务端地址缺少主机名")}
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/api/v1/agent/ws"
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// session 是一次完整的连接生命周期：拨号 → hello/welcome → 上报循环。
func (c *Client) session(ctx context.Context) error {
	wsURL, err := c.wsURL()
	if err != nil {
		return err
	}

	dialCtx, cancelDial := context.WithTimeout(ctx, handshakeTimeout)
	conn, resp, err := websocket.Dial(dialCtx, wsURL, &websocket.DialOptions{
		HTTPClient:   c.http,
		HTTPHeader:   http.Header{"Authorization": []string{"Bearer " + c.cfg.Token}},
		Subprotocols: []string{clientSubprotocol},
	})
	cancelDial()
	if err != nil {
		if resp != nil {
			return &statusError{code: resp.StatusCode}
		}
		return fmt.Errorf("连接 %s 失败: %w", wsURL, err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(protocol.MaxFrame)

	if err := c.sendHello(ctx, conn); err != nil {
		return err
	}
	welcome, err := c.readWelcome(ctx, conn)
	if err != nil {
		return err
	}
	c.setInterval(welcome.IntervalSec)
	c.log.Info("已连接到服务端",
		"node_id", welcome.NodeID, "name", welcome.Name,
		"interval_sec", welcome.IntervalSec, "observed_ip", welcome.ObservedIP)

	sctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// 新连接＝服务端那边重新开始的配置序列，版本号从 0 记起。
	c.mu.Lock()
	c.cfgVer = 0
	c.mu.Unlock()

	// 探测器跟着会话走：断线期间没有目标可探，重连后握手时的 config 帧会重新给到。
	go c.pings.Run(sctx)

	writeErrCh := make(chan error, 1)
	go func() {
		err := c.writeLoop(sctx, conn)
		writeErrCh <- err
		cancel() // 写循环退出即结束整个会话
	}()

	readErr := c.readLoop(sctx, conn)
	cancel()

	var writeErr error
	select {
	case writeErr = <-writeErrCh:
	case <-time.After(2 * time.Second):
		writeErr = errors.New("上报循环没有及时退出")
	}

	if errors.Is(writeErr, errOnceDone) {
		_ = conn.Close(websocket.StatusNormalClosure, "completed one report")
		return nil
	}
	if ctx.Err() != nil {
		return nil
	}
	if readErr != nil {
		return readErr
	}
	return writeErr
}

func (c *Client) sendHello(ctx context.Context, conn *websocket.Conn) error {
	info, err := c.collector.Info()
	if err != nil {
		return fmt.Errorf("读取本机信息失败: %w", err)
	}
	cp := c.traffic.Checkpoint()
	// 本机地址只在握手时算一次（见 localip.go）：它随网络环境变化很慢，
	// 放进每秒的上报循环纯属浪费——UDP connect 虽快，也没必要每秒做两遍。
	// 取不到就是空串，不影响握手。
	//
	// 必须带上 ctx 与超时：这一步夹在「升级完成」和「写 hello」之间，而服务端
	// 从 Accept 成功就开始计 10 秒的握手超时（protocol.HelloTimeout），
	// DNS 卡住时一个纯展示字段就能把 hello 拖过时限（见 localIPProbeTimeout）。
	localV4, localV6 := LocalIPs(ctx, serverHostFromURL(c.cfg.ServerURL))
	hello := protocol.Hello{
		AgentVersion: info.AgentVersion,
		Hostname:     info.Hostname,
		OS:           info.OS,
		CPU:          info.CPU,
		BootID:       info.BootID,
		UptimeSec:    info.UptimeSec,
		Iface:        info.Iface,
		IntervalSec:  int(c.currentInterval().Seconds()),
		LocalIP:      localV4,
		LocalIP6:     localV6,
		State: &protocol.AgentStat{
			CkptAgeS: c.traffic.CkptAge(time.Now()),
			TotalRx:  cp.TotalRx,
			TotalTx:  cp.TotalTx,
		},
	}
	frame, err := protocol.New(protocol.TypeHello, hello)
	if err != nil {
		return err
	}
	return c.write(ctx, conn, frame)
}

func (c *Client) readWelcome(ctx context.Context, conn *websocket.Conn) (protocol.Welcome, error) {
	wctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	typ, data, err := conn.Read(wctx)
	if err != nil {
		return protocol.Welcome{}, fmt.Errorf("等待 welcome 失败: %w", err)
	}
	if typ != websocket.MessageText {
		return protocol.Welcome{}, errors.New("welcome 必须是文本帧")
	}
	frame, err := protocol.Decode(data)
	if err != nil {
		return protocol.Welcome{}, err
	}
	if frame.T == protocol.TypeError {
		var e protocol.ErrorPayload
		_ = frame.Bind(&e)
		return protocol.Welcome{}, &serverError{code: e.Code, message: e.Message}
	}
	if frame.T != protocol.TypeWelcome {
		return protocol.Welcome{}, fmt.Errorf("握手失败：服务端先发来 %q", frame.T)
	}
	var welcome protocol.Welcome
	if err := frame.Bind(&welcome); err != nil {
		return protocol.Welcome{}, err
	}
	if welcome.IntervalSec < protocol.MinIntervalSec || welcome.IntervalSec > protocol.MaxIntervalSec {
		return protocol.Welcome{}, fmt.Errorf(
			"服务端下发的上报间隔 %d 超出 %d-%d，等待管理员修正",
			welcome.IntervalSec, protocol.MinIntervalSec, protocol.MaxIntervalSec)
	}
	return welcome, nil
}

// writeLoop 按间隔采样并上报，同时每 5s 发一次 ping。
func (c *Client) writeLoop(ctx context.Context, conn *websocket.Conn) error {
	if err := c.reportOnce(ctx, conn); err != nil {
		return err
	}
	if c.cfg.Once {
		return errOnceDone
	}

	interval := c.currentInterval()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	lastPing := time.Now()

	for {
		tickAt := time.Now()
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}

		if err := c.reportOnce(ctx, conn); err != nil {
			return err
		}
		// 发送阻塞导致跳拍的，如实计入 dropped；ticker 不会补发错过的拍子。
		if spent := time.Since(tickAt); spent > interval {
			c.addDropped(uint64(spent / interval))
		}
		if time.Since(lastPing) >= c.pingEvery {
			lastPing = time.Now()
			if err := c.sendPing(ctx, conn); err != nil {
				return err
			}
		}
		if next := c.currentInterval(); next != interval {
			interval = next
			ticker.Reset(interval)
			c.log.Info("上报间隔已更新", "interval_sec", int(interval.Seconds()))
		}
	}
}

func (c *Client) reportOnce(ctx context.Context, conn *websocket.Conn) error {
	now := time.Now()
	metrics, warnings, err := c.collector.Sample(now)
	if err != nil {
		// 采集失败不算连接失败：本次不上报，下一拍继续。
		c.log.Error("采集失败，本次不上报", "err", err)
		return nil
	}
	c.logWarnings(warnings)

	c.mu.Lock()
	c.seq++
	seq := c.seq
	metrics.Dropped = c.dropped
	metrics.LatMS = c.latMS
	c.mu.Unlock()

	// 各目标最近一次的探测结果。没有结果（还没探到、或 ICMP 没权限被留空）时
	// 数组为空，omitempty 会让它整个不出现在帧里。
	metrics.Pings = c.pings.Results()

	// 发送前用**服务端同一个**校验函数自查：不合法就不上网。
	//
	// 为什么值得做：服务端对非法帧只记 badFrames 并静默丢弃，连拒 30 帧才关连接，
	// 于是采集侧的 bug 在本地表现为"莫名断连"、毫无线索。这里自查之后，问题第一次
	// 出现就在 Agent 自己的日志里，而且与 ValidateMetrics 天然不会漂移。
	// 不因此断连：与上面 Sample 失败的处理保持一致（跳过这一帧，下一拍继续）。
	if err := protocol.ValidateMetrics(metrics); err != nil {
		c.warnThrottled("metrics-invalid", "本机采集结果不合法，本次不上报", err)
		return nil
	}

	frame, err := protocol.New(protocol.TypeMetrics, metrics)
	if err != nil {
		return err
	}
	frame.Seq = seq
	frame.TS = now.Unix()
	return c.write(ctx, conn, frame)
}

func (c *Client) sendPing(ctx context.Context, conn *websocket.Conn) error {
	frame, err := protocol.New(protocol.TypePing, protocol.Ping{TsUS: time.Now().UnixMicro()})
	if err != nil {
		return err
	}
	return c.write(ctx, conn, frame)
}

// readLoop 处理服务端下发的一切消息。
func (c *Client) readLoop(ctx context.Context, conn *websocket.Conn) error {
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		if typ != websocket.MessageText {
			continue
		}
		frame, err := protocol.Decode(data)
		if err != nil {
			c.log.Warn("收到无法解析的帧，已忽略", "err", err)
			continue
		}
		switch frame.T {
		case protocol.TypePong:
			var p protocol.Pong
			if err := frame.Bind(&p); err != nil || p.TsUS <= 0 {
				continue
			}
			latency := float64(time.Now().UnixMicro()-p.TsUS) / 1000
			if latency >= 0 {
				c.mu.Lock()
				c.latMS = latency
				c.mu.Unlock()
			}

		case protocol.TypeConfig:
			var cfg protocol.Config
			if err := frame.Bind(&cfg); err != nil {
				c.log.Warn("配置帧解析失败", "err", err)
				continue
			}
			// 不合法整帧丢弃：配置决定"探谁、多久探一次"，
			// 半真半假的配置比不收更危险（例如 interval_sec 被写成 0）。
			if err := protocol.ValidateConfig(cfg); err != nil {
				c.log.Warn("服务端下发的配置不合法，已忽略", "err", err)
				continue
			}
			if cfg.ConfigVersion <= c.appliedConfigVersion() {
				// 过期配置（乱序、或重连后服务端重发了旧版本）：
				// 按版本号丢弃，绝不覆盖已经生效的新配置。
				c.log.Debug("忽略过期的配置", "config_version", cfg.ConfigVersion)
				continue
			}
			if cfg.IntervalSec != 0 {
				c.setInterval(cfg.IntervalSec)
			}
			// 注意：cfg.Iface 是有意不处理的（服务端目前也不下发）。
			// 真去切网卡会改变被监控的网卡并重设流量基线 —— 那是功能变更而不是修 bug，
			// 见 protocol.Config.Iface 的注释。
			// 更新探测目标与间隔。这里**不会阻塞**：真正干活的是 Prober 自己的
			// 调度 goroutine，这个调用只换配置并唤醒它。
			c.pings.Update(cfg.PingTargets, cfg.PingIntervalSec)
			c.setAppliedConfigVersion(cfg.ConfigVersion)

			ack, err := protocol.New(protocol.TypeAck, protocol.Ack{ConfigVersion: cfg.ConfigVersion})
			if err == nil {
				if err := c.write(ctx, conn, ack); err != nil {
					return err
				}
			}

		case protocol.TypeError:
			var e protocol.ErrorPayload
			_ = frame.Bind(&e)
			c.log.Warn("服务端返回错误", "code", e.Code, "message", e.Message, "fatal", e.Fatal)
			if e.Fatal {
				return &serverError{code: e.Code, message: e.Message}
			}

		default:
			c.log.Debug("忽略未知消息", "type", frame.T)
		}
	}
}

func (c *Client) write(ctx context.Context, conn *websocket.Conn, frame protocol.Envelope) error {
	data, err := frame.Encode()
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, clientWriteTimeout)
	defer cancel()
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return conn.Write(wctx, websocket.MessageText, data)
}

// logWarnings 对同类告警做限频，避免磁盘/网卡异常时日志刷屏。
func (c *Client) logWarnings(warnings []string) {
	if len(warnings) == 0 {
		return
	}
	now := time.Now()
	var pending []string

	c.mu.Lock()
	if len(c.lastWarn) > maxWarnKeys {
		// 告警文本可能带路径等变量，规模必须封顶。
		c.lastWarn = make(map[string]time.Time)
	}
	for _, w := range warnings {
		if last, ok := c.lastWarn[w]; ok && now.Sub(last) < warnRepeat {
			continue
		}
		c.lastWarn[w] = now
		pending = append(pending, w)
	}
	c.mu.Unlock()

	for _, w := range pending {
		c.log.Warn("采集提示", "warn", w)
	}
}

// warnThrottled 按固定 key 限频地记一条 WARN。
//
// 与 logWarnings 的区别：这里的告警文本带着每拍都可能变化的数值（例如非法的
// lat_ms），拿文本本身当去重键会失效，所以由调用方给一个稳定的 key。
func (c *Client) warnThrottled(key, msg string, err error) {
	now := time.Now()
	c.mu.Lock()
	if last, ok := c.lastWarn[key]; ok && now.Sub(last) < warnRepeat {
		c.mu.Unlock()
		return
	}
	if len(c.lastWarn) > maxWarnKeys {
		c.lastWarn = make(map[string]time.Time)
	}
	c.lastWarn[key] = now
	c.mu.Unlock()

	c.log.Warn(msg, "err", err)
}

func (c *Client) setInterval(seconds int) {
	if seconds < protocol.MinIntervalSec || seconds > protocol.MaxIntervalSec {
		return
	}
	c.mu.Lock()
	c.interval = time.Duration(seconds) * time.Second
	c.mu.Unlock()
}

func (c *Client) currentInterval() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.interval
}

// appliedConfigVersion 返回已应用的配置版本（见 readLoop 的过期判断）。
func (c *Client) appliedConfigVersion() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cfgVer
}

func (c *Client) setAppliedConfigVersion(v int64) {
	c.mu.Lock()
	c.cfgVer = v
	c.mu.Unlock()
}

func (c *Client) addDropped(n uint64) {
	c.mu.Lock()
	c.dropped += n
	c.mu.Unlock()
}
