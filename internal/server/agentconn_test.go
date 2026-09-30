package server

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"probe/internal/config"
	"probe/internal/protocol"
	"probe/internal/state"
	"probe/internal/store"
)

// newAgentTestServer 起一个真实监听端口的服务端，并预先创建一个节点。
func newAgentTestServer(t *testing.T) (*httptest.Server, *Server, store.Node, string) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "probe.db"))
	if err != nil {
		t.Fatalf("打开测试数据库: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	s := New(config.Default(), db, slog.New(slog.DiscardHandler), time.UTC)
	// 缩短超时，让用例秒级完成。
	s.agents.helloTimeout = 500 * time.Millisecond
	s.agents.minIdle = 2 * time.Second

	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	node, token, err := db.CreateNode(ctx, store.NewNode{
		Name: "t-01", IntervalSec: 1, TrafficWarnPct: 80, ResetDay: 1,
	}, time.Now())
	if err != nil {
		t.Fatalf("创建节点: %v", err)
	}
	return ts, s, node, token
}

func agentWSURL(ts *httptest.Server) string {
	return "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/v1/agent/ws"
}

// dialAgent 用给定 Token 连接；失败时返回 HTTP 响应，便于断言状态码。
func dialAgent(t *testing.T, ts *httptest.Server, token string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	opts := &websocket.DialOptions{Subprotocols: []string{agentSubprotocol}}
	if token != "" {
		opts.HTTPHeader = http.Header{"Authorization": []string{"Bearer " + token}}
	}
	return websocket.Dial(ctx, agentWSURL(ts), opts)
}

func mustDialAgent(t *testing.T, ts *httptest.Server, token string) *websocket.Conn {
	t.Helper()
	conn, resp, err := dialAgent(t, ts, token)
	if err != nil {
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		t.Fatalf("连接失败: %v（HTTP %d）", err, code)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	return conn
}

func sendFrame(t *testing.T, conn *websocket.Conn, env protocol.Envelope) {
	t.Helper()
	data, err := env.Encode()
	if err != nil {
		t.Fatalf("编码帧: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatalf("发送帧: %v", err)
	}
}

func sendRaw(t *testing.T, conn *websocket.Conn, data []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatalf("发送原始帧: %v", err)
	}
}

func readFrame(t *testing.T, conn *websocket.Conn, timeout time.Duration) (protocol.Envelope, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	typ, data, err := conn.Read(ctx)
	if err != nil {
		return protocol.Envelope{}, err
	}
	if typ != websocket.MessageText {
		t.Fatalf("收到非文本帧: %v", typ)
	}
	return protocol.Decode(data)
}

// readHandshake 读掉握手的两帧：welcome 与紧随其后的 config。
//
// 服务端在 welcome 之后**紧接着**下发 config（把上报间隔、探测目标与探测间隔
// 交给 Agent，见 agentconn.go）。因此"读完 welcome 就发下一帧、再断言收到的
// 第一个东西"的用例，必须先把这个 config 帧消费掉 —— 否则断言会撞上它。
func readHandshake(t *testing.T, conn *websocket.Conn) protocol.Welcome {
	t.Helper()
	env, err := readFrame(t, conn, 3*time.Second)
	if err != nil {
		t.Fatalf("读取 welcome: %v", err)
	}
	if env.T != protocol.TypeWelcome {
		t.Fatalf("首帧应当是 welcome，实际 %q", env.T)
	}
	var welcome protocol.Welcome
	if err := env.Bind(&welcome); err != nil {
		t.Fatalf("解析 welcome: %v", err)
	}
	cfg, err := readFrame(t, conn, 3*time.Second)
	if err != nil {
		t.Fatalf("welcome 之后应当收到 config: %v", err)
	}
	if cfg.T != protocol.TypeConfig {
		t.Fatalf("第二帧应当是 config，实际 %q", cfg.T)
	}
	return welcome
}

func testHello() protocol.Hello {
	return protocol.Hello{
		AgentVersion: "test-1.0",
		Hostname:     "hk-01",
		OS:           protocol.OSInfo{Name: "Debian GNU/Linux 12", Kernel: "6.1.0", Arch: "amd64"},
		CPU:          protocol.CPUInfo{Model: "Xeon", Cores: 4},
		BootID:       "boot-1",
		UptimeSec:    1000,
		Iface:        protocol.IfaceInfo{Name: "eth0", IfIndex: 2, MAC: "52:54:00:aa:bb:cc"},
		IntervalSec:  1,
		LocalIP:      "203.0.113.5",
		LocalIP6:     "2001:db8::1",
	}
}

func testMetrics() protocol.Metrics {
	return protocol.Metrics{
		CPUPct:    12.5,
		CPUCores:  4,
		Mem:       protocol.Mem{Total: 1 << 30, Used: 1 << 29, Pct: 50},
		Swap:      protocol.Mem{Total: 1 << 28, Used: 0, Pct: 0},
		Disk:      []protocol.Disk{{Mount: "/", FS: "ext4", Total: 1 << 40, Used: 1 << 39, Pct: 50}},
		Load:      protocol.Load{L1: 0.5, L5: 0.4, L15: 0.3},
		Net:       protocol.Net{Iface: "eth0", RxTotal: 1 << 30, TxTotal: 1 << 29, RxRaw: 1 << 31, TxRaw: 1 << 30, RxRate: 1024, TxRate: 512, BootID: "boot-1", CkptAgeS: 2},
		LatMS:     23.4,
		UptimeSec: 1000,
	}
}

func helloFrame(t *testing.T, h protocol.Hello) protocol.Envelope {
	t.Helper()
	env, err := protocol.New(protocol.TypeHello, h)
	if err != nil {
		t.Fatalf("构造 hello: %v", err)
	}
	return env
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待超时: %s", what)
}

// stableNodeState 等到节点状态连续 3 次完全一致再返回。
//
// 为什么需要它：Agent 常常是"一口气发 12 帧"，而 waitFor(Seq > 0) 只能证明
// **第一帧**到了。机器一忙，断言时剩下的帧还在 TCP/WebSocket 缓冲里没读完，
// 于是读到 3、4 这种中间值 —— 这就是限流用例偶发失败的真正原因（限流器本身
// 是纯计数的，300 次压测都是恰好放行 5 帧）。等状态稳定下来再断言，
// 与机器负载无关，且不改动它要验证的语义。
func stableNodeState(t *testing.T, s *Server, nodeID int64) state.Node {
	t.Helper()
	const window = 3
	var last state.Node
	same := 0
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		cur, ok := s.State().Get(nodeID)
		if ok && cur.Seq == last.Seq && cur.ConnID == last.ConnID {
			same++
			if same >= window {
				return cur
			}
		} else {
			same = 0
		}
		last = cur
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("等待节点状态稳定超时")
	return state.Node{}
}

func TestAgentRejectsBadToken(t *testing.T) {
	ts, _, _, _ := newAgentTestServer(t)

	_, resp, err := dialAgent(t, ts, "pba_not_a_real_token")
	if err == nil {
		t.Fatal("错误 Token 不应握手成功")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("HTTP 状态码 = %v，期望 401", resp)
	}

	_, resp, err = dialAgent(t, ts, "")
	if err == nil {
		t.Fatal("缺少 Token 不应握手成功")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("缺少 Token 时 HTTP 状态码 = %v，期望 401", resp)
	}
}

func TestAgentRejectsDisabledNode(t *testing.T) {
	ts, s, node, token := newAgentTestServer(t)
	node.Enabled = false
	if err := s.db.UpdateNode(context.Background(), node, time.Now()); err != nil {
		t.Fatalf("停用节点: %v", err)
	}

	_, resp, err := dialAgent(t, ts, token)
	if err == nil {
		t.Fatal("已停用节点不应握手成功")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("HTTP 状态码 = %v，期望 403", resp)
	}
}

func TestAgentHandshakeAndMetrics(t *testing.T) {
	ts, s, node, token := newAgentTestServer(t)
	conn := mustDialAgent(t, ts, token)
	sendFrame(t, conn, helloFrame(t, testHello()))

	env, err := readFrame(t, conn, 3*time.Second)
	if err != nil {
		t.Fatalf("读取 welcome: %v", err)
	}
	if env.T != protocol.TypeWelcome {
		t.Fatalf("首帧应当是 welcome，实际 %q", env.T)
	}
	var welcome protocol.Welcome
	if err := env.Bind(&welcome); err != nil {
		t.Fatalf("解析 welcome: %v", err)
	}
	if welcome.NodeID != node.ID || welcome.Name != node.Name || welcome.IntervalSec != 1 {
		t.Fatalf("welcome 内容不对: %+v", welcome)
	}
	if welcome.ObservedIP != "127.0.0.1" {
		t.Fatalf("welcome.ObservedIP = %q，期望 127.0.0.1", welcome.ObservedIP)
	}
	// 握手是两帧：welcome 之后紧跟着 config（内容由 TestAgentReceivesConfigAfterHandshake 覆盖），
	// 这里只把它读掉，免得后面把 config 当成指标回包。
	if cfgEnv, err := readFrame(t, conn, 3*time.Second); err != nil || cfgEnv.T != protocol.TypeConfig {
		t.Fatalf("welcome 之后应当收到 config: %v（帧类型 %q）", err, cfgEnv.T)
	}

	// 静态信息应当已经进入内存状态。
	got, ok := s.State().Get(node.ID)
	if !ok || !got.Connected || got.Info.Hostname != "hk-01" || got.Info.CPU.Cores != 4 {
		t.Fatalf("连接后状态不对: %+v", got)
	}
	// Agent 自报的本机地址（local_ip / local_ip6）必须跟着 AgentVersion 那批
	// 静态信息一起落进内存，而 ObservedIP 仍然是服务端看到的 TCP 来源 ——
	// 两者是不同的东西，前者绝不能被后者覆盖（Cloudflare Tunnel 场景下
	// ObservedIP 恒为 127.0.0.1，页面显示它毫无意义）。
	if got.Info.AgentVersion != "test-1.0" {
		t.Fatalf("AgentVersion = %q，期望 test-1.0", got.Info.AgentVersion)
	}
	if got.LocalIP != "203.0.113.5" || got.LocalIP6 != "2001:db8::1" {
		t.Fatalf("握手未写入本机地址: local_ip=%q local_ip6=%q", got.LocalIP, got.LocalIP6)
	}
	if got.ObservedIP != "127.0.0.1" {
		t.Fatalf("ObservedIP = %q，期望 127.0.0.1", got.ObservedIP)
	}

	// 上报一次指标。
	metrics := testMetrics()
	frame, err := protocol.New(protocol.TypeMetrics, metrics)
	if err != nil {
		t.Fatalf("构造 metrics: %v", err)
	}
	frame.Seq = 1
	sendFrame(t, conn, frame)

	waitFor(t, 3*time.Second, "内存状态更新", func() bool {
		n, ok := s.State().Get(node.ID)
		return ok && n.Seq >= 1
	})
	n, _ := s.State().Get(node.ID)
	if n.Metrics.CPUPct != 12.5 || n.Metrics.Net.RxTotal != 1<<30 || n.Metrics.Disk[0].Mount != "/" {
		t.Fatalf("指标内容不对: %+v", n.Metrics)
	}
	if n.Gap != 0 {
		t.Fatalf("连续帧不应产生缺口，实际 %d", n.Gap)
	}
}

func TestAgentFirstFrameMustBeHello(t *testing.T) {
	ts, s, node, token := newAgentTestServer(t)
	conn := mustDialAgent(t, ts, token)

	frame, err := protocol.New(protocol.TypeMetrics, testMetrics())
	if err != nil {
		t.Fatalf("构造帧: %v", err)
	}
	sendFrame(t, conn, frame)

	if _, err := readFrame(t, conn, 3*time.Second); err == nil {
		t.Fatal("服务端应当在收到非 hello 首帧后断开")
	}
	if _, ok := s.State().Get(node.ID); ok {
		t.Fatal("握手失败不应写入内存状态")
	}
}

func TestAgentVersionMismatch(t *testing.T) {
	ts, _, _, token := newAgentTestServer(t)
	conn := mustDialAgent(t, ts, token)

	// 手工构造一个未来版本的 hello。
	sendRaw(t, conn, []byte(`{"v":99,"t":"hello","d":{}}`))

	env, err := readFrame(t, conn, 3*time.Second)
	if err == nil {
		if env.T != protocol.TypeError {
			t.Fatalf("应当先收到 error 帧，实际 %q", env.T)
		}
		var e protocol.ErrorPayload
		if err := env.Bind(&e); err != nil || e.Code != protocol.CodeUpgradeRequired {
			t.Fatalf("错误码不对: %+v (err=%v)", e, err)
		}
		if _, err := readFrame(t, conn, 3*time.Second); err == nil {
			t.Fatal("随后应当断开连接")
		}
	} else if websocket.CloseStatus(err) != websocket.StatusCode(protocol.CloseUpgradeRequired) {
		t.Fatalf("关闭码不对: %v", err)
	}
}

func TestAgentHelloTimeout(t *testing.T) {
	ts, s, node, token := newAgentTestServer(t)
	conn := mustDialAgent(t, ts, token)

	// 不发 hello，等服务端超时关闭。
	if _, err := readFrame(t, conn, 3*time.Second); err == nil {
		t.Fatal("服务端应当在 hello 超时后断开")
	}
	if _, ok := s.State().Get(node.ID); ok {
		t.Fatal("超时握手不应写入内存状态")
	}
}

func TestAgentInvalidMetricsAreDropped(t *testing.T) {
	ts, s, node, token := newAgentTestServer(t)
	conn := mustDialAgent(t, ts, token)
	sendFrame(t, conn, helloFrame(t, testHello()))
	readHandshake(t, conn)

	// 非法指标（CPU 超过 100）：丢弃但不影响连接。
	bad := testMetrics()
	bad.CPUPct = 150
	for i := 0; i < 3; i++ {
		frame, err := protocol.New(protocol.TypeMetrics, bad)
		if err != nil {
			t.Fatalf("构造帧: %v", err)
		}
		frame.Seq = uint64(i + 1)
		sendFrame(t, conn, frame)
	}

	// 随后一帧合法数据必须照常入库（注意总帧数不能超过每秒限流窗口）。
	frame, err := protocol.New(protocol.TypeMetrics, testMetrics())
	if err != nil {
		t.Fatalf("构造帧: %v", err)
	}
	frame.Seq = 4
	sendFrame(t, conn, frame)

	waitFor(t, 3*time.Second, "合法指标入库", func() bool {
		n, ok := s.State().Get(node.ID)
		return ok && n.Seq >= 1
	})
}

func TestAgentTooManyBadFramesClosesConnection(t *testing.T) {
	ts, s, _, token := newAgentTestServer(t)
	// 这条用例要专门验证"连续非法帧"的计数，因此把限流放大，
	// 否则帧会先被限流丢掉（限流本身由另一条用例覆盖）。
	s.agents.msgPerSecond = 1000

	conn := mustDialAgent(t, ts, token)
	sendFrame(t, conn, helloFrame(t, testHello()))
	readHandshake(t, conn)

	bad := testMetrics()
	bad.CPUPct = -1
	for i := 0; i < agentMaxBadFrames+2; i++ {
		frame, err := protocol.New(protocol.TypeMetrics, bad)
		if err != nil {
			t.Fatalf("构造帧: %v", err)
		}
		sendFrame(t, conn, frame)
	}

	_, err := readFrame(t, conn, 5*time.Second)
	if err == nil {
		t.Fatal("连续非法帧达到上限后应当断开")
	}
	if status := websocket.CloseStatus(err); status != websocket.StatusCode(protocol.CloseBadRequest) {
		t.Fatalf("关闭码 = %v，期望 %d（err=%v）", status, protocol.CloseBadRequest, err)
	}
}

func TestAgentOversizeFrameIsRejected(t *testing.T) {
	ts, _, _, token := newAgentTestServer(t)
	conn := mustDialAgent(t, ts, token)
	sendFrame(t, conn, helloFrame(t, testHello()))
	readHandshake(t, conn)

	big := make([]byte, protocol.MaxFrame+1024)
	for i := range big {
		big[i] = 'a'
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// 客户端可能本地就失败（帧太大），也可能发出去后被服务端断开，两种都算通过。
	_ = conn.Write(ctx, websocket.MessageText, big)

	_, err := readFrame(t, conn, 5*time.Second)
	if err == nil {
		t.Fatal("超大帧之后连接应当被断开")
	}
	if status := websocket.CloseStatus(err); status != websocket.StatusMessageTooBig {
		t.Fatalf("关闭码 = %v，期望 %d（err=%v）", status, websocket.StatusMessageTooBig, err)
	}
}

func TestAgentBinaryFrameIsRejected(t *testing.T) {
	ts, _, _, token := newAgentTestServer(t)
	conn := mustDialAgent(t, ts, token)
	sendFrame(t, conn, helloFrame(t, testHello()))
	readHandshake(t, conn)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = conn.Write(ctx, websocket.MessageBinary, []byte{0x01, 0x02})

	_, err := readFrame(t, conn, 3*time.Second)
	if err == nil {
		t.Fatal("二进制帧之后连接应当被断开")
	}
	if status := websocket.CloseStatus(err); status != websocket.StatusCode(protocol.CloseBadRequest) {
		t.Fatalf("关闭码 = %v，期望 %d（err=%v）", status, protocol.CloseBadRequest, err)
	}
}

func TestAgentPingPongAndUnknownType(t *testing.T) {
	ts, _, _, token := newAgentTestServer(t)
	conn := mustDialAgent(t, ts, token)
	sendFrame(t, conn, helloFrame(t, testHello()))
	readHandshake(t, conn)

	ping, err := protocol.New(protocol.TypePing, protocol.Ping{TsUS: 1700000000123456})
	if err != nil {
		t.Fatalf("构造 ping: %v", err)
	}
	sendFrame(t, conn, ping)
	env, err := readFrame(t, conn, 3*time.Second)
	if err != nil {
		t.Fatalf("读取 pong: %v", err)
	}
	if env.T != protocol.TypePong {
		t.Fatalf("期望 pong，实际 %q", env.T)
	}
	var pong protocol.Pong
	if err := env.Bind(&pong); err != nil || pong.TsUS != 1700000000123456 {
		t.Fatalf("pong 未原样回传: %+v (err=%v)", pong, err)
	}

	// 未知类型：回 error 但不关连接。
	sendRaw(t, conn, []byte(`{"v":1,"t":"future_type","d":{}}`))
	env, err = readFrame(t, conn, 3*time.Second)
	if err != nil {
		t.Fatalf("未知类型不应断开连接: %v", err)
	}
	if env.T != protocol.TypeError {
		t.Fatalf("期望 error 帧，实际 %q", env.T)
	}
	var e protocol.ErrorPayload
	if err := env.Bind(&e); err != nil || e.Code != protocol.CodeUnknownType || e.Fatal {
		t.Fatalf("错误内容不对: %+v (err=%v)", e, err)
	}

	// 连接仍然可用。
	frame, err := protocol.New(protocol.TypeMetrics, testMetrics())
	if err != nil {
		t.Fatalf("构造帧: %v", err)
	}
	sendFrame(t, conn, frame)
	time.Sleep(100 * time.Millisecond)
}

func TestAgentRateLimitDropsExcess(t *testing.T) {
	ts, s, node, token := newAgentTestServer(t)
	// 把窗口拉长到 30 秒：这样"一口气发 12 帧"必然落在同一个窗口里，
	// 断言就与机器负载无关（限流窗口本身由 TestRateLimiterWindow 覆盖）。
	s.agents.rateWindow = 30 * time.Second

	conn := mustDialAgent(t, ts, token)
	sendFrame(t, conn, helloFrame(t, testHello()))
	readHandshake(t, conn)

	for i := 0; i < 12; i++ {
		frame, err := protocol.New(protocol.TypeMetrics, testMetrics())
		if err != nil {
			t.Fatalf("构造帧: %v", err)
		}
		frame.Seq = uint64(i + 1)
		sendFrame(t, conn, frame)
	}
	waitFor(t, 3*time.Second, "限流窗口内的帧被处理", func() bool {
		n, ok := s.State().Get(node.ID)
		return ok && n.Seq > 0
	})

	// waitFor 只看 Seq > 0，而 12 帧是连着发的：机器一忙，断言可能在服务端
	// 还没读完剩下的帧时就执行了，读到 3、4 这种中间值（flaky 的来源）。
	// 这里等到 Seq 连续几次不再变化，拿到的才是"这一窗口最终放行了几帧"。
	n := stableNodeState(t, s, node.ID)
	if n.Seq != agentMsgPerSecond {
		t.Fatalf("限流后入库 %d 帧，期望恰好 %d 帧", n.Seq, agentMsgPerSecond)
	}

	// 超速只丢帧，不断连接：接下来的读应当"超时"而不是"关闭"。
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, _, err := conn.Read(ctx)
	if err == nil {
		t.Fatal("限流后不该有额外回包")
	}
	if websocket.CloseStatus(err) != -1 {
		t.Fatalf("限流不应该断开连接，实际收到关闭: %v", err)
	}
}

func TestAgentsShutdownClosesConnections(t *testing.T) {
	ts, s, _, token := newAgentTestServer(t)
	conn := mustDialAgent(t, ts, token)
	sendFrame(t, conn, helloFrame(t, testHello()))
	readHandshake(t, conn)

	s.agents.Shutdown("服务端退出")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _, err := conn.Read(ctx)
	if err == nil {
		t.Fatal("服务端退出时应当关闭 Agent 连接")
	}
	if status := websocket.CloseStatus(err); status != websocket.StatusGoingAway {
		t.Fatalf("关闭码 = %v，期望 %v（err=%v）", status, websocket.StatusGoingAway, err)
	}
}

func TestRateLimiterWindow(t *testing.T) {
	l := newRateLimiter(2, time.Second)
	now := time.Unix(1_700_000_000, 0)
	if !l.allow(now) || !l.allow(now) {
		t.Fatal("前两次应当放行")
	}
	if l.allow(now) {
		t.Fatal("超过上限应当拒绝")
	}
	if !l.allow(now.Add(time.Second)) {
		t.Fatal("新窗口应当重新计数")
	}
}

func TestBearerTokenParsing(t *testing.T) {
	cases := []struct {
		header string
		want   string
		ok     bool
	}{
		{"Bearer pba_abc", "pba_abc", true},
		{"bearer pba_abc", "pba_abc", true},
		{"Bearer   pba_abc  ", "pba_abc", true},
		{"", "", false},
		{"Bearer", "", false},
		{"Bearer ", "", false},
		{"Basic pba_abc", "", false},
		{"Bearer " + strings.Repeat("x", maxTokenLen+1), "", false},
	}
	for _, tc := range cases {
		req, err := http.NewRequest(http.MethodGet, "/api/v1/agent/ws", nil)
		if err != nil {
			t.Fatalf("构造请求: %v", err)
		}
		if tc.header != "" {
			req.Header.Set("Authorization", tc.header)
		}
		got, ok := bearerToken(req)
		if ok != tc.ok || got != tc.want {
			t.Errorf("bearerToken(%q) = (%q, %v)，期望 (%q, %v)", tc.header, got, ok, tc.want, tc.ok)
		}
	}
}
