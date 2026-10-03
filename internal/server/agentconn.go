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
	// agentCtlPerSecondFactor 是控制帧（ping/ack/未知类型）相对上报配额的倍数。
	//
	// 控制帧必须有**独立**配额：把它算进上报配额里，心跳稍密一点就开始丢真实
	// 数据（这条有专门的用例钉住：efficiency_test.go 的
	// TestAgentRateLimitOnlyCountsMetrics）。但它也不能完全没有闸门 ——
	// 以前一个 ping 来一帧回一帧、未知类型同样即时回一帧，取 Token 的人
	// 用 20 条连接就能把 CPU 与出网带宽吃满（写还各带 5 秒超时）。
	// 10 倍是"远远高于真实心跳（5 秒一次），但把刷帧的收益压到可忽略"的量级。
	agentCtlPerSecondFactor = 10
	// agentAuthFailLockAfter / agentAuthFailLockFor：同一来源连续这么多次
	// Agent 鉴权失败之后，暂时拒绝该来源的握手。只看**失败**（成功会清零），
	// 所以同一 NAT 后面十几台 Agent 正常重连不会被自己的成功挤掉额度。
	agentAuthFailLockAfter = 10
	agentAuthFailLockFor   = 5 * time.Minute
	// agentMaxBadFrames 是连续不合法帧的上限，超过即断开。
	agentMaxBadFrames = 30
	// maxSeqGapPerFrame 是单帧允许计入"服务端观测缺口"的上限。
	//
	// seq 完全由 Agent 自填，两帧（1、2^62）就能把 gap 推到天文数字（还会超过
	// JSON 安全整数范围）。真实路径不可能触到 1000：静默超时最多 30 秒、
	// 上报间隔最小 1 秒，也就是最多 30 帧连续丢失。
	maxSeqGapPerFrame = 1000
	// maxConnGap 是单条连接累计缺口的上限（2^53）：与 protocol 对 Agent **自报**
	// dropped/gap 的夹取口径一致，保证它进 JSON 时不会退化成浮点。
	maxConnGap = uint64(1) << 53
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
	ping    *pingTracker
	log     *slog.Logger

	conns atomic.Int64

	// authFails 是"同一来源的鉴权失败"闸门（按 IP 锁定，见 agentAuthFailLockAfter）。
	//
	// 为什么必须有：GET /api/v1/agent/ws 是 accessOpen 且对空 Origin 放行，
	// 匿名可达。没有闸门时，一个循环 GET 就能让服务端每个请求打一次库、
	// 写两条日志（访问日志 + Warn），这是最廉价的磁盘/日志放大器。
	// 用的是 newLockoutLimiter：只数失败，正常 Agent 的重连不受影响。
	authFails *attemptLimiter
	// authFailLogs 统计鉴权失败总数：Warn 必须节流（第 1 次与每 100 次一条），
	// 否则"限流"只挡住了库查询，日志照样被刷。
	authFailLogs atomic.Int64

	mu    sync.Mutex
	perIP map[string]int
	// active 记录当前连接，供服务端退出时统一关闭。
	active map[uint64]*activeConn

	// configVer 是 config 帧的版本号，每次下发 +1（Agent 靠它丢弃过期的配置）。
	configVer atomic.Int64

	// pushMu 保护下面两个字段，实现"推送合并且不阻塞调用方"（见 PushConfig）。
	pushMu    sync.Mutex
	pushing   bool
	pushAgain bool

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
func NewAgents(cfg config.Server, db *store.DB, st *state.Store, agg *accumulator, traffic *trafficTracker, ping *pingTracker, log *slog.Logger) *Agents {
	return &Agents{
		cfg:          cfg,
		store:        db,
		state:        st,
		agg:          agg,
		traffic:      traffic,
		ping:         ping,
		log:          log,
		perIP:        make(map[string]int),
		active:       make(map[uint64]*activeConn),
		authFails:    newLockoutLimiter(agentAuthFailLockAfter, agentAuthFailLockFor),
		helloTimeout: protocol.HelloTimeout * time.Second,
		minIdle:      agentMinIdle,
		msgPerSecond: agentMsgPerSecond,
		rateWindow:   time.Second,
	}
}

// Handle 处理 GET /api/v1/agent/ws。
func (a *Agents) Handle(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	now := time.Now()

	// 鉴权失败的闸门放在**最前面**：被锁的来源连 Token 都不必解析，
	// 更不会打库或写日志。只数失败（成功会 succeed 清零），
	// 所以正常 Agent 的握手/重连永远不会被自己挤掉额度。
	if ok, retry := a.authFails.allowed(ip, now); !ok {
		w.Header().Set("Retry-After", fmt.Sprintf("%d", int(retry.Seconds())+1))
		http.Error(w, "too many failed authentication attempts", http.StatusTooManyRequests)
		return
	}

	if a.conns.Load() >= int64(a.maxConns()) {
		a.log.Warn("Agent 连接数达到上限，拒绝新连接", "limit", a.maxConns())
		http.Error(w, "too many agent connections", http.StatusServiceUnavailable)
		return
	}

	token, ok := bearerToken(r)
	if !ok {
		a.failAuth(ip, now, "Token 缺失或格式不对")
		w.Header().Set("WWW-Authenticate", `Bearer realm="probe-agent"`)
		http.Error(w, "missing or malformed bearer token", http.StatusUnauthorized)
		return
	}
	node, err := a.store.NodeByTokenHash(r.Context(), store.HashToken(token))
	switch {
	case errors.Is(err, store.ErrNodeNotFound):
		// 这里只记应用日志、不写审计表：失败的鉴权是高频事件，
		// 让它直接写库等于给攻击者一个廉价的写放大手段。
		a.failAuth(ip, now, "Token 无效")
		w.Header().Set("WWW-Authenticate", `Bearer realm="probe-agent"`)
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	case err != nil:
		a.log.Error("查询节点失败", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	case !node.Enabled:
		// 停用**不算**鉴权失败：那是管理员的显式动作，合法节点会按
		// disabledBackoff 一直重试，把它记进失败计数等于用一次配置变更
		// 把同一个 NAT 后面的其它 Agent 一起锁住。
		a.log.Warn("已停用的节点尝试连接", "node_id", node.ID, "ip", ip)
		http.Error(w, "node disabled", http.StatusForbidden)
		return
	}
	// 鉴权成功：把这个来源的连续失败清零（同一 NAT 后面的正常 Agent 会
	// 立刻把攻击者留下的计数洗掉，不会被连坐）。
	a.authFails.succeed(ip)

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
	// 先把静态信息写进内存状态，再下发 welcome/config。
	//
	// 顺序很重要：客户端一读到 welcome 就会认为"我已经连上了"，此时界面/测试
	// 去查内存状态必须能查到 —— 否则会出现"welcome 到了、节点却还是空的"这种
	// 只在握手瞬间存在的中间态。
	a.state.Attach(node.ID, connID, protocol.Info{
		AgentVersion: hello.AgentVersion,
		Hostname:     hello.Hostname,
		OS:           hello.OS,
		CPU:          hello.CPU,
		BootID:       hello.BootID,
		UptimeSec:    hello.UptimeSec,
		Iface:        hello.Iface,
	}, ip, hello.LocalIP, hello.LocalIP6, time.Now())

	// welcome 与紧随其后的 config 用**同一个**版本号：Agent 只需要记住
	// "服务端现在到哪一版了"，不必区分这两帧。
	version := a.nextConfigVersion()
	frame, err := protocol.New(protocol.TypeWelcome, protocol.Welcome{
		NodeID:        node.ID,
		Name:          node.Name,
		IntervalSec:   interval,
		ServerTime:    time.Now().Unix(),
		ObservedIP:    ip,
		ConfigVersion: version,
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
	// 紧接着下发配置：上报间隔、探测目标与探测间隔。
	//
	// 以前这里只有 welcome 里的 interval_sec，config 帧从来没有真正发过 ——
	// 于是 Agent 侧的目标与间隔只能靠内置默认值，服务端改了也没人知道。
	if err := a.sendConfig(ctx, conn, node, version, a.loadPingSettings(ctx, node.ID)); err != nil {
		a.log.Warn("下发 config 失败", "node_id", node.ID, "err", err)
		return
	}

	a.log.Info("Agent 已连接",
		"node_id", node.ID, "name", node.Name, "ip", ip,
		"local_ip", hello.LocalIP, "local_ip6", hello.LocalIP6,
		"agent_version", hello.AgentVersion, "iface", hello.Iface.Name, "interval_sec", interval)

	idle := a.minIdle
	if d := 3 * time.Duration(interval) * time.Second; d > idle {
		idle = d
	}
	limiter := newRateLimiter(a.msgPerSecond, a.rateWindow)
	// 控制帧的独立配额（见 agentCtlPerSecondFactor）：与上报配额分开，
	// 谁都不会把谁的额度吃掉。
	ctlLimiter := newRateLimiter(a.msgPerSecond*agentCtlPerSecondFactor, a.rateWindow)
	badFrames := 0
	throttled := 0
	ctlThrottled := 0
	// offConfig 统计"配置外目标"被丢弃的条目数（只用于日志节流，见下面的丢弃分支）。
	offConfig := 0
	// dropControl 记一次"控制帧被限速丢弃"，日志与 metrics 一样按次数节流。
	dropControl := func(what string) {
		ctlThrottled++
		if ctlThrottled == 1 || ctlThrottled%100 == 0 {
			a.log.Warn("Agent "+what+"超速，已丢弃",
				"node_id", node.ID, "dropped_total", ctlThrottled,
				"limit_per_sec", a.msgPerSecond*agentCtlPerSecondFactor)
		}
	}
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
			//
			// 两处封顶都必要：seq 完全由 Agent 自填，"1 然后 2^62"两帧就能把这个
			// 服务端观测值推到天文数字（还会超出 JSON 的安全整数范围）。
			// 这里选**钳制**而不是整帧拒绝：帧里的指标本身是合法数据，丢掉它
			// 会让曲线真的出现一个洞 —— 而这一段要限制的只是那个由 seq 推出来的数。
			if env.Seq > 0 {
				if lastFrameSeq > 0 && env.Seq > lastFrameSeq+1 {
					delta := env.Seq - lastFrameSeq - 1
					if delta > maxSeqGapPerFrame {
						delta = maxSeqGapPerFrame
					}
					connGap += delta
					if connGap > maxConnGap {
						connGap = maxConnGap
					}
				}
				lastFrameSeq = env.Seq
			}
			a.state.Update(node.ID, connID, m, connGap, time.Now())
			// 同一个样本同时进入"最新值"（实时）与"10 秒桶"（历史）。
			a.agg.add(node.ID, interval, m, time.Now())
			// 探测结果另走一条路：它是"最近一次"的语义（每秒都会重复上报），
			// 由 pingTracker 攒着、每分钟落一行（见 server/ping.go）。
			//
			// target_id 完全由 Agent 自填，所以这里必须过滤：不在"下发给它的
			// 目标"里的 ID 只在配额内被接纳（审计 03-A-1）。丢弃要留下痕迹 ——
			// 与 metrics 超速同一套节流写法（第 1 次与每 100 条各一条）。
			if a.ping != nil {
				if dropped := a.ping.observe(node.ID, m.Pings, time.Now()); dropped > 0 {
					prev := offConfig
					offConfig += dropped
					if prev == 0 || prev/100 != offConfig/100 {
						a.log.Warn("Agent 上报了配置外的探测目标，已丢弃",
							"node_id", node.ID, "dropped", dropped, "dropped_total", offConfig,
							"limit_per_min", pingExtraTargets)
					}
				}
			}
			// 流量：用 Agent 的长期累计值做幂等增量（重复帧算 0，丢帧不丢流量）。
			if reason := a.traffic.observe(node.ID, m); reason != "" && reason != resetFirstSeen {
				a.log.Warn("流量基线已重设（不计入流量）",
					"node_id", node.ID, "reason", reason,
					"rx_total", m.Net.RxTotal, "tx_total", m.Net.TxTotal)
			}

		case protocol.TypePing:
			// 控制帧走自己的配额：回一条 pong 要一次 JSON 解析 + 序列化
			// + 一次带 5 秒超时的写，不封顶就是一条不限速的回包通道。
			if !ctlLimiter.allow(time.Now()) {
				dropControl("控制帧")
				continue
			}
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
			if !ctlLimiter.allow(time.Now()) {
				dropControl("控制帧")
				continue
			}
			a.log.Debug("Agent 已确认配置", "node_id", node.ID)

		default:
			// 未知类型：计入 badFrames（连续 30 帧即断开），回帧同样限速 ——
			// 以前是"来一帧回一帧"，等于给刷帧的人一条 1:1 的回包通道。
			badFrames++
			if badFrames >= agentMaxBadFrames {
				_ = conn.Close(websocket.StatusCode(protocol.CloseBadRequest), "too many unknown frames")
				return
			}
			if !ctlLimiter.allow(time.Now()) {
				dropControl("未知类型帧")
				continue
			}
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

// nextConfigVersion 分配一个配置版本号（每次下发 +1）。
func (a *Agents) nextConfigVersion() int64 { return a.configVer.Add(1) }

// nodeInterval 取该节点生效的上报间隔（越界值退回最小值，与 welcome 保持一致）。
func nodeInterval(node store.Node) int {
	if node.IntervalSec < protocol.MinIntervalSec || node.IntervalSec > protocol.MaxIntervalSec {
		return protocol.MinIntervalSec
	}
	return node.IntervalSec
}

// loadPingSettings 读取全局探测设置。
//
// 读失败时退回"不下发任何目标 + 默认间隔"：宁可暂时不探测，也不要拿一份
// 半真半假的配置去指挥 Agent。探测设置是全局的，一帧 config 读一次就够。
func (a *Agents) loadPingSettings(ctx context.Context, nodeID int64) store.PingSettings {
	settings, err := a.store.PingSettings(ctx)
	if err != nil {
		a.log.Warn("读取延迟探测设置失败，本次不下发目标", "err", err, "node_id", nodeID)
		return store.PingSettings{IntervalSec: protocol.DefaultPingIntervalSec}
	}
	if settings.IntervalSec == 0 {
		settings.IntervalSec = protocol.DefaultPingIntervalSec
	}
	return settings
}

// sendConfig 下发一帧 config，并在**写成功之后**把这一帧里的目标登记成
// pingTracker 的白名单。
//
// 为什么登记点在这里而不是"读设置的地方"：这一帧就是 Agent 手里那份配置的
// 来源（审计 03-A-1 的修法①）—— 只有它真的写出去，才能说"这个 Agent 被允许
// 上报这些 target_id"。写失败时不登记：Agent 手里还是上一份配置，它上报的旧 ID
// 由 pingTracker 的配置外配额兜着（见 ping.go 的 admit），不会丢数据。
func (a *Agents) sendConfig(ctx context.Context, conn *websocket.Conn, node store.Node, version int64, settings store.PingSettings) error {
	frame, targets := a.configFrame(node, version, settings)
	if err := a.write(ctx, conn, frame); err != nil {
		return err
	}
	if a.ping != nil {
		a.ping.setAllowed(targets)
	}
	return nil
}

// configFrame 构造一帧 config（上报间隔来自节点，探测目标来自全局设置）。
//
// 第二个返回值是这一帧里**实际带上**的目标 ID（序列化失败退化成空配置时为空）：
// 它同时是"这个 Agent 被允许上报哪些 target_id"的那份白名单，见 sendConfig。
func (a *Agents) configFrame(node store.Node, version int64, settings store.PingSettings) (protocol.Envelope, []int64) {
	interval := nodeInterval(node)
	targets := wirePingTargets(settings.Targets)
	cfg := protocol.Config{
		ConfigVersion:   version,
		IntervalSec:     interval,
		PingTargets:     targets,
		PingIntervalSec: settings.IntervalSec,
	}
	frame, err := protocol.New(protocol.TypeConfig, cfg)
	if err != nil {
		// 负载是固定结构，序列化失败只可能是目标异常大；此时退化成空配置，
		// 至少让 Agent 的上报间隔是对的。
		a.log.Error("构造 config 帧失败，改为只下发上报间隔", "err", err, "node_id", node.ID)
		frame, _ = protocol.New(protocol.TypeConfig, protocol.Config{
			ConfigVersion: version, IntervalSec: interval,
			PingIntervalSec: settings.IntervalSec,
		})
		return frame, nil
	}
	return frame, pingTargetIDs(targets)
}

// pingTargetIDs 取出一批目标里的 ID。
func pingTargetIDs(targets []protocol.PingTarget) []int64 {
	ids := make([]int64, 0, len(targets))
	for _, t := range targets {
		ids = append(ids, t.ID)
	}
	return ids
}

// wirePingTargets 把设置里的目标转成下发给 Agent 的形状。
//
// 只下发 enabled 的目标：停用的目标留在设置页里，但 Agent 不该再去探它 ——
// "停用"必须真的省掉那份流量，否则用户关掉它就没有意义。
func wirePingTargets(targets []store.PingTarget) []protocol.PingTarget {
	out := make([]protocol.PingTarget, 0, len(targets))
	for _, t := range targets {
		if !t.Enabled {
			continue
		}
		out = append(out, protocol.PingTarget{ID: t.ID, Type: t.Type, Host: t.Host, Port: t.Port})
		if len(out) >= protocol.MaxPingTargets {
			break
		}
	}
	return out
}

// PushConfig 给所有在线 Agent 下发一帧新的 config（设置变更后调用）。
//
// 两个刻意的设计：
//   - 后台推送：设置接口的响应不能被一个卡住的 Agent 拖到写超时（每个连接单独超时）；
//   - 合并重复请求：连点保存只推最后那一次（pushing/pushAgain）。
//
// 返回值没有意义（推送是异步的），需要断言"Agent 收到了"的测试应当从
// 连接那一侧读帧，而不是看这里的返回。
func (a *Agents) PushConfig() {
	a.pushMu.Lock()
	if a.pushing {
		a.pushAgain = true
		a.pushMu.Unlock()
		return
	}
	a.pushing = true
	a.pushMu.Unlock()

	go func() {
		for {
			a.pushConfigOnce()
			a.pushMu.Lock()
			again := a.pushAgain
			a.pushAgain = false
			if !again {
				a.pushing = false
				a.pushMu.Unlock()
				return
			}
			a.pushMu.Unlock()
		}
	}()
}

// pushConfigOnce 是 PushConfig 的一次实际下发。
func (a *Agents) pushConfigOnce() {
	// 配置是全局的，但每个连接的 IntervalSec 不同，所以按连接查一次节点。
	snapshot := make(map[uint64]*activeConn)
	a.mu.Lock()
	for id, c := range a.active {
		snapshot[id] = c
	}
	a.mu.Unlock()
	if len(snapshot) == 0 {
		return
	}

	settings := store.PingSettings{IntervalSec: protocol.DefaultPingIntervalSec}
	{
		ctx, cancel := context.WithTimeout(context.Background(), agentWriteTimeout)
		settings = a.loadPingSettings(ctx, 0)
		cancel()
	}
	version := a.nextConfigVersion()

	for _, ac := range snapshot {
		// 每个连接一个独立的超时：一个卡住的 Agent 不该把后面所有节点的推送
		// 一起耗光（写超时是 5 秒，连接多了就会把共享的 ctx 用完）。
		ctx, cancel := context.WithTimeout(context.Background(), agentWriteTimeout)
		node, err := a.store.NodeByID(ctx, ac.nodeID)
		if err != nil {
			// 节点刚被删掉时连接还没断干净，属于正常竞态。
			cancel()
			a.log.Debug("跳过已不存在节点的配置推送", "node_id", ac.nodeID, "err", err)
			continue
		}
		err = a.sendConfig(ctx, ac.conn, node, version, settings)
		cancel()
		if err != nil {
			// 推失败不重试：Agent 下一次重连会在握手里拿到最新配置，
			// 在这里重试只会与它自己的重连互相踩。
			a.log.Warn("下发 config 失败", "node_id", ac.nodeID, "err", err)
		}
	}
	a.log.Debug("已推送配置", "connections", len(snapshot), "config_version", version)
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

// activeCount 返回当前在连的 Agent 数量（只被测试用）。
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

// failAuth 记一次"这个来源的 Agent 鉴权失败"，并节流地留一条日志。
//
// 为什么要节流：这条路径匿名可达，一次 GET = 一条访问日志 + 一条 Warn。
// 不节流的话，限流只省下了库查询，日志照样能被刷爆小磁盘/journald 配额 ——
// 那正是这条路径最初被报出来的形态。与 metrics 超速的处理同一套写法：
// 记第 1 次与每 100 次（这样"攻击开始了"和"还在继续、规模多大"都看得到）。
func (a *Agents) failAuth(ip string, now time.Time, reason string) {
	a.authFails.fail(ip, now)
	total := a.authFailLogs.Add(1)
	if total == 1 || total%100 == 0 {
		a.log.Warn("Agent 鉴权失败", "ip", ip, "reason", reason, "failed_total", total)
	}
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
