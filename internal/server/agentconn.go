package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"probe/internal/config"
	"probe/internal/protocol"
	"probe/internal/state"
	"probe/internal/store"
)

const (
	// agentSubprotocol 是 Agent 连接使用的 WebSocket 子协议。
	agentSubprotocol = "probe.v1"
	// agentMaxConnsPerIP / agentMaxConns 是连接数上限的默认值（可用命令行参数覆盖）。
	agentMaxConnsPerIP = 20
	agentMaxConns      = 500
	// agentWriteTimeout 是单次写入的上限：慢连接直接断开，由 Agent 重连自愈。
	agentWriteTimeout = 5 * time.Second
	// agentMsgPerSecond 是每个连接允许的消息速率（默认 1 秒一帧，留足余量）。
	agentMsgPerSecond = 5
	// agentMaxBadFrames 是连续不合法帧的上限，超过即断开。
	agentMaxBadFrames = 30
	// agentMinIdle 是连接静默上限的下界；实际取 max(30s, 3×上报间隔)。
	agentMinIdle = 30 * time.Second
	// maxTokenLen 是 Token 长度上限（正常 47 字节），挡住超长输入。
	maxTokenLen = 128
)

// errBadHello 表示首帧不是合法的 hello。
var errBadHello = errors.New("首帧必须是合法的 hello")

// Agents 负责 Agent 的 WebSocket 接入：鉴权、限流、校验、写入内存状态。
type Agents struct {
	cfg     config.Server
	store   *store.DB
	state   *state.Store
	agg     *accumulator
	traffic *trafficTracker
	log     *slog.Logger

	conns atomic.Int64

	mu    sync.Mutex
	perIP map[string]int
	// active 记录当前连接，供服务端退出时统一关闭。
	active map[uint64]*activeConn

	// 超时与限流参数在构造时取默认值；测试可覆盖，避免用例跑十几秒。
	helloTimeout time.Duration
	minIdle      time.Duration
	msgPerSecond int
	rateWindow   time.Duration
}

type activeConn struct {
	nodeID int64
	conn   *websocket.Conn
}

// NewAgents 构造 Agent 接入器。
func NewAgents(cfg config.Server, db *store.DB, st *state.Store, agg *accumulator, traffic *trafficTracker, log *slog.Logger) *Agents {
	return &Agents{
		cfg:          cfg,
		store:        db,
		state:        st,
		agg:          agg,
		traffic:      traffic,
		log:          log,
		perIP:        make(map[string]int),
		active:       make(map[uint64]*activeConn),
		helloTimeout: protocol.HelloTimeout * time.Second,
		minIdle:      agentMinIdle,
		msgPerSecond: agentMsgPerSecond,
		rateWindow:   time.Second,
	}
}

// Handle 处理 GET /api/v1/agent/ws。
func (a *Agents) Handle(w http.ResponseWriter, r *http.Request) {
	if a.conns.Load() >= int64(a.maxConns()) {
		a.log.Warn("Agent 连接数达到上限，拒绝新连接", "limit", a.maxConns())
		http.Error(w, "too many agent connections", http.StatusServiceUnavailable)
		return
	}

	token, ok := bearerToken(r)
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer realm="probe-agent"`)
		http.Error(w, "missing or malformed bearer token", http.StatusUnauthorized)
		return
	}
	node, err := a.store.NodeByTokenHash(r.Context(), store.HashToken(token))
	switch {
	case errors.Is(err, store.ErrNodeNotFound):
		// 这里只记应用日志、不写审计表：失败的鉴权是高频事件，
		// 让它直接写库等于给攻击者一个廉价的写放大手段。
		a.log.Warn("Agent 鉴权失败：Token 无效", "ip", clientIP(r))
		w.Header().Set("WWW-Authenticate", `Bearer realm="probe-agent"`)
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	case err != nil:
		a.log.Error("查询节点失败", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	case !node.Enabled:
		a.log.Warn("已停用的节点尝试连接", "node_id", node.ID, "ip", clientIP(r))
		http.Error(w, "node disabled", http.StatusForbidden)
		return
	}

	ip := clientIP(r)
	if !a.acquireIP(ip) {
		a.log.Warn("同一来源的 Agent 连接过多", "ip", ip, "limit", a.maxConnsPerIP())
		http.Error(w, "too many connections from this address", http.StatusTooManyRequests)
		return
	}
	defer a.releaseIP(ip)

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols:    []string{agentSubprotocol},
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		a.log.Warn("WebSocket 握手失败", "ip", ip, "err", err)
		return
	}
	a.conns.Add(1)
	defer a.conns.Add(-1)
	conn.SetReadLimit(protocol.MaxFrame)

	a.serve(r.Context(), conn, node, ip)
}

func (a *Agents) serve(ctx context.Context, conn *websocket.Conn, node store.Node, ip string) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	connID := a.state.NextConnID()
	a.register(connID, node.ID, conn)
	defer func() {
		a.unregister(connID)
		a.state.Detach(node.ID, connID)
	}()

	hello, err := a.readHello(ctx, conn)
	if err != nil {
		code := protocol.CloseHelloTimeout
		switch {
		case errors.Is(err, protocol.ErrVersionMismatch):
			code = protocol.CloseUpgradeRequired
			_ = a.write(ctx, conn, protocol.ErrorEnvelope(protocol.CodeUpgradeRequired, err.Error(), true))
		case errors.Is(err, errBadHello):
			code = protocol.CloseBadRequest
		}
		a.log.Warn("Agent 握手失败", "node_id", node.ID, "name", node.Name, "ip", ip, "err", err)
		_ = conn.Close(websocket.StatusCode(code), "handshake failed")
		return
	}

	interval := node.IntervalSec
	if interval < protocol.MinIntervalSec || interval > protocol.MaxIntervalSec {
		interval = protocol.MinIntervalSec
	}
	frame, err := protocol.New(protocol.TypeWelcome, protocol.Welcome{
		NodeID:        node.ID,
		Name:          node.Name,
		IntervalSec:   interval,
		ServerTime:    time.Now().Unix(),
		ObservedIP:    ip,
		ConfigVersion: 1,
	})
	if err != nil {
		a.log.Error("构造 welcome 失败", "err", err)
		_ = conn.Close(websocket.StatusCode(protocol.CloseInternalError), "internal error")
		return
	}
	if err := a.write(ctx, conn, frame); err != nil {
		a.log.Warn("下发 welcome 失败", "node_id", node.ID, "err", err)
		return
	}

	a.state.Attach(node.ID, connID, protocol.Info{
		AgentVersion: hello.AgentVersion,
		Hostname:     hello.Hostname,
		OS:           hello.OS,
		CPU:          hello.CPU,
		BootID:       hello.BootID,
		UptimeSec:    hello.UptimeSec,
		Iface:        hello.Iface,
	}, ip, hello.LocalIP, hello.LocalIP6, time.Now())
	a.log.Info("Agent 已连接",
		"node_id", node.ID, "name", node.Name, "ip", ip,
		"local_ip", hello.LocalIP, "local_ip6", hello.LocalIP6,
		"agent_version", hello.AgentVersion, "iface", hello.Iface.Name, "interval_sec", interval)

	idle := a.minIdle
	if d := 3 * time.Duration(interval) * time.Second; d > idle {
		idle = d
	}
	limiter := newRateLimiter(a.msgPerSecond, a.rateWindow)
	badFrames := 0
	throttled := 0
	var lastFrameSeq, connGap uint64

	for {
		if ctx.Err() != nil {
			return
		}
		// coder/websocket 用 context 做超时：静默超过 idle 即认为这条连接已经没用。
		rctx, cancel := context.WithTimeout(ctx, idle)
		typ, data, err := conn.Read(rctx)
		cancel()
		if err != nil {
			if ctx.Err() == nil {
				a.log.Info("Agent 连接结束", "node_id", node.ID, "name", node.Name, "err", err)
			}
			return
		}
		if typ != websocket.MessageText {
			a.log.Warn("收到二进制帧，断开连接", "node_id", node.ID)
			_ = conn.Close(websocket.StatusCode(protocol.CloseBadRequest), "text frames only")
			return
		}

		env, err := protocol.Decode(data)
		if err != nil {
			if errors.Is(err, protocol.ErrVersionMismatch) {
				_ = a.write(ctx, conn, protocol.ErrorEnvelope(protocol.CodeUpgradeRequired, err.Error(), true))
				_ = conn.Close(websocket.StatusCode(protocol.CloseUpgradeRequired), "protocol version mismatch")
				return
			}
			badFrames++
			a.log.Warn("收到无法解析的帧，已丢弃", "node_id", node.ID, "err", err, "bad_frames", badFrames)
			if badFrames >= agentMaxBadFrames {
				_ = conn.Close(websocket.StatusCode(protocol.CloseBadRequest), "too many malformed frames")
				return
			}
			continue
		}

		switch env.T {
		case protocol.TypeMetrics:
			// 限流只针对上报帧：控制帧（pong/ack）不该把上报配额吃掉，
			// 否则心跳稍密一点就会开始丢真实数据。
			if !limiter.allow(time.Now()) {
				throttled++
				// 超速直接丢弃，不回错误帧（否则会变成放大的回包通道）。
				// 日志也必须节流：一个刷帧的客户端否则能刷爆日志与磁盘。
				if throttled == 1 || throttled%100 == 0 {
					a.log.Warn("Agent 上报超速，已丢弃",
						"node_id", node.ID, "dropped_total", throttled, "limit_per_sec", a.msgPerSecond)
				}
				continue
			}
			var m protocol.Metrics
			if err := env.Bind(&m); err != nil {
				badFrames++
				a.log.Warn("实时指标解析失败，已丢弃", "node_id", node.ID, "err", err)
				if badFrames >= agentMaxBadFrames {
					_ = conn.Close(websocket.StatusCode(protocol.CloseBadRequest), "too many malformed frames")
					return
				}
				continue
			}
			if err := protocol.ValidateMetrics(m); err != nil {
				badFrames++
				a.log.Warn("实时指标不合法，已丢弃", "node_id", node.ID, "err", err)
				if badFrames >= agentMaxBadFrames {
					_ = conn.Close(websocket.StatusCode(protocol.CloseBadRequest), "too many invalid frames")
					return
				}
				continue
			}
			badFrames = 0
			// 序号缺口 = 服务端观测到的丢帧数（Agent 自报的 dropped 是另一回事）。
			if env.Seq > 0 {
				if lastFrameSeq > 0 && env.Seq > lastFrameSeq+1 {
					connGap += env.Seq - lastFrameSeq - 1
				}
				lastFrameSeq = env.Seq
			}
			a.state.Update(node.ID, connID, m, connGap, time.Now())
			// 同一个样本同时进入"最新值"（实时）与"10 秒桶"（历史）。
			a.agg.add(node.ID, interval, m, time.Now())
			// 流量：用 Agent 的长期累计值做幂等增量（重复帧算 0，丢帧不丢流量）。
			if reason := a.traffic.observe(node.ID, m); reason != "" && reason != resetFirstSeen {
				a.log.Warn("流量基线已重设（不计入流量）",
					"node_id", node.ID, "reason", reason,
					"rx_total", m.Net.RxTotal, "tx_total", m.Net.TxTotal)
			}

		case protocol.TypePing:
			var p protocol.Ping
			if err := env.Bind(&p); err != nil {
				badFrames++
				continue
			}
			reply, err := protocol.New(protocol.TypePong, protocol.Pong{TsUS: p.TsUS})
			if err == nil {
				if err := a.write(ctx, conn, reply); err != nil {
					a.log.Debug("回复 pong 失败", "node_id", node.ID, "err", err)
					return
				}
			}

		case protocol.TypeAck:
			a.log.Debug("Agent 已确认配置", "node_id", node.ID)

		default:
			_ = a.write(ctx, conn, protocol.ErrorEnvelope(
				protocol.CodeUnknownType, fmt.Sprintf("未知消息类型 %q", env.T), false))
		}
	}
}

// readHello 读取并校验首帧。超时、类型错误与版本不匹配返回不同的错误。
func (a *Agents) readHello(ctx context.Context, conn *websocket.Conn) (protocol.Hello, error) {
	hctx, cancel := context.WithTimeout(ctx, a.helloTimeout)
	defer cancel()
	typ, data, err := conn.Read(hctx)
	if err != nil {
		return protocol.Hello{}, fmt.Errorf("等待 hello 失败: %w", err)
	}
	if typ != websocket.MessageText {
		return protocol.Hello{}, errBadHello
	}
	env, err := protocol.Decode(data)
	if err != nil {
		return protocol.Hello{}, err
	}
	if env.T != protocol.TypeHello {
		return protocol.Hello{}, fmt.Errorf("%w：收到 %q", errBadHello, env.T)
	}
	var hello protocol.Hello
	if err := env.Bind(&hello); err != nil {
		return protocol.Hello{}, err
	}
	if err := protocol.ValidateHello(hello); err != nil {
		return protocol.Hello{}, fmt.Errorf("%w：%v", errBadHello, err)
	}
	return hello, nil
}

func (a *Agents) write(ctx context.Context, conn *websocket.Conn, env protocol.Envelope) error {
	data, err := env.Encode()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, agentWriteTimeout)
	defer cancel()
	return conn.Write(ctx, websocket.MessageText, data)
}

// Shutdown 关闭所有在连的 Agent 连接：服务端退出时让 Agent 立刻重连，
// 而不是等 TCP 超时。
func (a *Agents) Shutdown(reason string) {
	a.mu.Lock()
	conns := make([]*websocket.Conn, 0, len(a.active))
	for _, c := range a.active {
		conns = append(conns, c.conn)
	}
	a.mu.Unlock()

	for _, conn := range conns {
		_ = conn.Close(websocket.StatusGoingAway, reason)
	}
	if len(conns) > 0 {
		a.log.Info("已关闭全部 Agent 连接", "count", len(conns))
	}
}

// maxConnsPerIP / maxConns 返回当前生效的上限：
// 配置优先，便于"多台机器在同一 NAT/反代后面"这种真实场景。
func (a *Agents) maxConnsPerIP() int {
	if a.cfg.AgentMaxPerIP > 0 {
		return a.cfg.AgentMaxPerIP
	}
	return agentMaxConnsPerIP
}

func (a *Agents) maxConns() int {
	if a.cfg.AgentMaxConns > 0 {
		return a.cfg.AgentMaxConns
	}
	return agentMaxConns
}

// DisconnectNode 断开某个节点当前的连接（重新生成 Token / 删除节点 / 停用节点时用）。
func (a *Agents) DisconnectNode(nodeID int64) int {
	a.mu.Lock()
	conns := make([]*websocket.Conn, 0, 1)
	for _, c := range a.active {
		if c.nodeID == nodeID {
			conns = append(conns, c.conn)
		}
	}
	a.mu.Unlock()

	for _, conn := range conns {
		_ = conn.Close(websocket.StatusPolicyViolation, "服务端已重置该节点的凭据")
	}
	return len(conns)
}

// activeCount 返回当前在连的 Agent 数量（诊断与测试用）。
func (a *Agents) activeCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.active)
}

func (a *Agents) register(connID uint64, nodeID int64, conn *websocket.Conn) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.active[connID] = &activeConn{nodeID: nodeID, conn: conn}
}

func (a *Agents) unregister(connID uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.active, connID)
}

func (a *Agents) acquireIP(ip string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.perIP[ip] >= a.maxConnsPerIP() {
		return false
	}
	a.perIP[ip]++
	return true
}

func (a *Agents) releaseIP(ip string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.perIP[ip] <= 1 {
		delete(a.perIP, ip)
		return
	}
	a.perIP[ip]--
}

// bearerToken 从 Authorization 头里取出 Token。
func bearerToken(r *http.Request) (string, bool) {
	const prefix = "bearer "
	header := r.Header.Get("Authorization")
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(header[len(prefix):])
	if token == "" || len(token) > maxTokenLen {
		return "", false
	}
	return token, true
}

// rateLimiter 是固定窗口的限流器：够用、无依赖、无后台 goroutine。
type rateLimiter struct {
	limit   int
	window  time.Duration
	started time.Time
	count   int
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{limit: limit, window: window}
}

func (l *rateLimiter) allow(now time.Time) bool {
	if now.Sub(l.started) >= l.window {
		l.started = now
		l.count = 0
	}
	if l.count >= l.limit {
		return false
	}
	l.count++
	return true
}
