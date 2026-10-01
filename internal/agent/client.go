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
	return &Client{
		cfg:       cfg,
		log:       logger,
		collector: collector,
		traffic:   traffic,
		// 注意：这里绝不能设置 Client.Timeout——WebSocket 是长连接。
		http:     &http.Client{Transport: transport},
		interval: time.Second,
		lastWarn: make(map[string]time.Time),

		pingEvery: pingInterval,
	}
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
	localV4, localV6 := LocalIPs(serverHostFromURL(c.cfg.ServerURL))
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
			// 不合法整帧丢弃：配置决定上报节奏，半真半假的配置比不收更危险
			// （例如 interval_sec 被写成 0）。
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
