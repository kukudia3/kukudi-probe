package agent

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"probe/internal/protocol"
)

// stubServer 是一个"最小服务端"：只实现握手与收帧，用来单独验证客户端行为。
type stubServer struct {
	t     *testing.T
	ts    *httptest.Server
	token string

	statusCode int // 非 0 时直接以该 HTTP 状态码拒绝连接

	intervalSec      int
	closeAfterMetric int
	pushConfig       *protocol.Config
	extraNote        string
	pongDelay        time.Duration

	mu       sync.Mutex
	attempts int
	conns    int
	hellos   []protocol.Hello
	metrics  []protocol.Metrics
	acks     int
	pings    int
}

func newStubServer(t *testing.T, token string) *stubServer {
	t.Helper()
	s := &stubServer{t: t, token: token}
	s.ts = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.ts.Close)
	return s
}

func (s *stubServer) url() string { return s.ts.URL }

func (s *stubServer) counts() (attempts, conns, metrics, acks, pings, hellos int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts, s.conns, len(s.metrics), s.acks, s.pings, len(s.hellos)
}

func (s *stubServer) firstHello() (protocol.Hello, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.hellos) == 0 {
		return protocol.Hello{}, false
	}
	return s.hellos[0], true
}

func (s *stubServer) lastMetrics() (protocol.Metrics, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.metrics) == 0 {
		return protocol.Metrics{}, false
	}
	return s.metrics[len(s.metrics)-1], true
}

func (s *stubServer) handle(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.attempts++
	s.mu.Unlock()

	if s.statusCode != 0 {
		http.Error(w, "rejected by stub", s.statusCode)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+s.token {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{clientSubprotocol}})
	if err != nil {
		return
	}
	defer func() { _ = conn.CloseNow() }()
	ctx := r.Context()

	s.mu.Lock()
	s.conns++
	s.mu.Unlock()

	// hello
	_, data, err := conn.Read(ctx)
	if err != nil {
		return
	}
	frame, err := protocol.Decode(data)
	if err != nil || frame.T != protocol.TypeHello {
		return
	}
	var hello protocol.Hello
	if err := frame.Bind(&hello); err != nil {
		return
	}
	s.mu.Lock()
	s.hellos = append(s.hellos, hello)
	s.mu.Unlock()

	// welcome
	interval := s.intervalSec
	if interval == 0 {
		interval = 1
	}
	welcome, err := protocol.New(protocol.TypeWelcome, protocol.Welcome{
		NodeID: 1, Name: "stub", IntervalSec: interval,
		ServerTime: time.Now().Unix(), ObservedIP: "203.0.113.9", Notes: s.extraNote,
	})
	if err != nil {
		return
	}
	if err := writeStub(ctx, conn, welcome); err != nil {
		return
	}
	if s.pushConfig != nil {
		cfg, err := protocol.New(protocol.TypeConfig, *s.pushConfig)
		if err == nil {
			if err := writeStub(ctx, conn, cfg); err != nil {
				return
			}
		}
	}

	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		frame, err := protocol.Decode(data)
		if err != nil {
			continue
		}
		switch frame.T {
		case protocol.TypeMetrics:
			var m protocol.Metrics
			if frame.Bind(&m) != nil {
				continue
			}
			s.mu.Lock()
			s.metrics = append(s.metrics, m)
			count := len(s.metrics)
			closeAfter := s.closeAfterMetric
			s.mu.Unlock()
			if closeAfter > 0 && count >= closeAfter {
				_ = conn.Close(websocket.StatusNormalClosure, "stub closing")
				return
			}
		case protocol.TypePing:
			var p protocol.Ping
			if frame.Bind(&p) != nil {
				continue
			}
			s.mu.Lock()
			s.pings++
			delay := s.pongDelay
			s.mu.Unlock()
			if delay > 0 {
				// 本机回环的往返时间小于微秒级，必须人为延迟才能验证"确实测到了 RTT"。
				time.Sleep(delay)
			}
			pong, err := protocol.New(protocol.TypePong, protocol.Pong{TsUS: p.TsUS})
			if err == nil {
				if err := writeStub(ctx, conn, pong); err != nil {
					return
				}
			}
		case protocol.TypeAck:
			s.mu.Lock()
			s.acks++
			s.mu.Unlock()
		}
	}
}

func writeStub(ctx context.Context, conn *websocket.Conn, frame protocol.Envelope) error {
	data, err := frame.Encode()
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return conn.Write(wctx, websocket.MessageText, data)
}

func newTestClient(t *testing.T, url, token string, once bool) *Client {
	t.Helper()
	traffic, warn, err := LoadTraffic("")
	if err != nil || warn != "" {
		t.Fatalf("LoadTraffic: %v %q", err, warn)
	}
	collector := New(filepath.Join("testdata", "root"), "", "/", traffic)
	c := NewClient(ClientConfig{
		ServerURL:      url,
		Token:          token,
		AllowPlaintext: true,
		Once:           once,
	}, collector, traffic, slog.New(slog.DiscardHandler))
	c.pingEvery = 100 * time.Millisecond
	return c
}

func TestClientOnceModeReportsAndExits(t *testing.T) {
	stub := newStubServer(t, "pba_stub")
	client := newTestClient(t, stub.url(), "pba_stub", true)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.Run(ctx); err != nil {
		t.Fatalf("--once 模式应当干净退出，实际 %v", err)
	}

	// --once 写完最后一帧就直接退出（writeLoop 退出后立刻 cancel），不等服务端确认。
	// stub 的读循环跑在另一个 goroutine 里，Run 返回时它未必已经处理完那一帧 ——
	// 帧不会丢（它排在关闭帧前面），但断言前得等它落地，否则 CPU 一紧张就偶发
	// metrics=0（`go test -cpu=1 -count=40` 可稳定复现）。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, n, _, _, _ := stub.counts(); n >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	_, conns, metrics, _, _, hellos := stub.counts()
	if conns != 1 || hellos != 1 || metrics != 1 {
		t.Fatalf("连接=%d hello=%d metrics=%d，期望各 1", conns, hellos, metrics)
	}
	hello, _ := stub.firstHello()
	if hello.Hostname == "" || hello.AgentVersion == "" || hello.Iface.Name != "eth0" {
		t.Fatalf("hello 内容不完整: %+v", hello)
	}
	if hello.State == nil || hello.State.TotalRx != 0 || hello.State.CkptAgeS != -1 {
		t.Fatalf("hello.state 不对（仅内存模式应为 ckpt_age_s=-1）: %+v", hello.State)
	}

	m, _ := stub.lastMetrics()
	if m.Net.Iface != "eth0" || m.Net.RxRaw != 9876543210 {
		t.Fatalf("上报的网卡数据不对: %+v", m.Net)
	}
	if m.CPUCores != 2 || m.Load.L1 != 0.15 || m.UptimeSec != 1234567 {
		t.Fatalf("上报的基础指标不对: %+v", m)
	}
	if m.Mem.Total != 2048000*1024 {
		t.Fatalf("上报的内存不对: %+v", m.Mem)
	}
}

func TestClientSendsMetricsAndPing(t *testing.T) {
	stub := newStubServer(t, "pba_stub")
	stub.pongDelay = 20 * time.Millisecond
	client := newTestClient(t, stub.url(), "pba_stub", false)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := client.Run(ctx); err != nil {
		t.Fatalf("Run 应当因超时正常返回，实际 %v", err)
	}

	_, conns, metrics, _, pings, _ := stub.counts()
	if conns != 1 {
		t.Fatalf("连接数 = %d，期望 1", conns)
	}
	if metrics < 2 {
		t.Fatalf("3 秒内应当上报至少 2 次，实际 %d", metrics)
	}
	if pings == 0 {
		t.Fatal("应当发送 ping")
	}

	client.mu.Lock()
	latency := client.latMS
	client.mu.Unlock()
	if latency < 15 {
		t.Fatalf("往返延迟应当接近 20ms，实际 %v", latency)
	}
}

func TestClientReconnectsAfterDisconnect(t *testing.T) {
	stub := newStubServer(t, "pba_stub")
	stub.closeAfterMetric = 1
	client := newTestClient(t, stub.url(), "pba_stub", false)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	go func() { _ = client.Run(ctx) }()

	deadline := time.Now().Add(7 * time.Second)
	for time.Now().Before(deadline) {
		if _, conns, _, _, _, _ := stub.counts(); conns >= 2 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	_, conns, _, _, _, _ := stub.counts()
	t.Fatalf("断开后应当自动重连，实际连接数 = %d", conns)
}

func TestClientStopsOnUnauthorized(t *testing.T) {
	stub := newStubServer(t, "pba_stub")
	stub.statusCode = http.StatusUnauthorized
	client := newTestClient(t, stub.url(), "pba_wrong", false)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() { _ = client.Run(ctx) }()

	// 401 属于"重试也没用"的一类：退避 5 分钟，短时间内不应重新连接。
	time.Sleep(800 * time.Millisecond)
	if attempts, _, _, _, _, _ := stub.counts(); attempts != 1 {
		t.Fatalf("认证失败后不应快速重连，实际尝试次数 = %d", attempts)
	}
}

func TestClientAppliesConfigPushAndAcks(t *testing.T) {
	stub := newStubServer(t, "pba_stub")
	stub.pushConfig = &protocol.Config{ConfigVersion: 9, IntervalSec: 2}
	client := newTestClient(t, stub.url(), "pba_stub", false)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() { _ = client.Run(ctx) }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, _, acks, _, _ := stub.counts(); acks >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, _, _, acks, _, _ := stub.counts(); acks < 1 {
		t.Fatal("客户端应当回 ack")
	}
	if got := client.currentInterval(); got != 2*time.Second {
		t.Fatalf("应用配置后的间隔 = %s，期望 2s", got)
	}
}

func TestClientRejectsPlaintextForNonLoopback(t *testing.T) {
	traffic, _, err := LoadTraffic("")
	if err != nil {
		t.Fatalf("LoadTraffic: %v", err)
	}
	collector := New(filepath.Join("testdata", "root"), "", "/", traffic)

	cases := []struct {
		name string
		url  string
	}{
		{"明文连接远程地址", "http://monitor.example.com"},
		{"明文连接内网地址", "ws://10.0.0.5:25774"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := NewClient(ClientConfig{ServerURL: tc.url, Token: "x"}, collector, traffic, nil)
			_, err := c.wsURL()
			if err == nil {
				t.Fatal("应当拒绝明文连接非本机地址")
			}
			var perm *permanentError
			if !asPermanent(err, &perm) {
				t.Fatalf("应当是永久错误（不重试），实际 %v", err)
			}
		})
	}
}

func asPermanent(err error, target **permanentError) bool {
	for err != nil {
		if p, ok := err.(*permanentError); ok {
			*target = p
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapper.Unwrap()
	}
	return false
}

func TestClientURLNormalization(t *testing.T) {
	traffic, _, _ := LoadTraffic("")
	collector := New(filepath.Join("testdata", "root"), "", "/", traffic)

	cases := []struct {
		name    string
		url     string
		allow   bool
		want    string
		wantErr bool
	}{
		{"https 转 wss", "https://monitor.example.com", false, "wss://monitor.example.com/api/v1/agent/ws", false},
		{"带路径前缀", "https://example.com/probe/", false, "wss://example.com/probe/api/v1/agent/ws", false},
		{"本机 http 允许", "http://127.0.0.1:25774", false, "ws://127.0.0.1:25774/api/v1/agent/ws", false},
		{"localhost 允许", "http://localhost:25774", false, "ws://localhost:25774/api/v1/agent/ws", false},
		{"显式允许明文", "http://10.0.0.5:25774", true, "ws://10.0.0.5:25774/api/v1/agent/ws", false},
		{"协议不支持", "ftp://example.com", false, "", true},
		{"缺少主机名", "https://", false, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := NewClient(ClientConfig{ServerURL: tc.url, Token: "x", AllowPlaintext: tc.allow}, collector, traffic, nil)
			got, err := c.wsURL()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("应当报错，实际得到 %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			if got != tc.want {
				t.Fatalf("URL = %q，期望 %q", got, tc.want)
			}
		})
	}
}

func TestNextBackoffClassification(t *testing.T) {
	traffic, _, _ := LoadTraffic("")
	collector := New(filepath.Join("testdata", "root"), "", "/", traffic)

	cases := []struct {
		name     string
		err      error
		lifetime time.Duration
		want     time.Duration
	}{
		{"认证失败长退避", &statusError{code: http.StatusUnauthorized}, time.Second, authBackoff},
		{"节点停用长退避", &statusError{code: http.StatusForbidden}, time.Second, disabledBackoff},
		{"协议不兼容长退避", &serverError{code: protocol.CodeUpgradeRequired}, time.Second, versionBackoff},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := NewClient(ClientConfig{}, collector, traffic, nil)
			backoff := initialBackoff
			if got := c.nextBackoff(tc.err, tc.lifetime, &backoff); got != tc.want {
				t.Fatalf("退避 = %s，期望 %s", got, tc.want)
			}
		})
	}

	// 普通断开：从 1s 起翻倍，稳定运行后重置。
	c := NewClient(ClientConfig{}, collector, traffic, nil)
	backoff := initialBackoff
	first := c.nextBackoff(wsClosed{}, 5*time.Second, &backoff)
	if first < initialBackoff*8/10 || first > initialBackoff*12/10 {
		t.Fatalf("首次退避 = %s，应当约为 1s（含 ±20%% 抖动）", first)
	}
	if backoff != 2*initialBackoff {
		t.Fatalf("退避未翻倍: %s", backoff)
	}
	// 长时间稳定后断开：重置为 1s。
	backoff = 30 * time.Second
	got := c.nextBackoff(wsClosed{}, 5*time.Minute, &backoff)
	if got > 2*initialBackoff {
		t.Fatalf("稳定连接断开后应当立即重试，实际 %s", got)
	}

	// 抖动范围。
	for i := 0; i < 100; i++ {
		d := jitter(10 * time.Second)
		if d < 8*time.Second || d > 12*time.Second {
			t.Fatalf("抖动超出 ±20%%: %s", d)
		}
	}
}

type wsClosed struct{}

func (wsClosed) Error() string { return "连接已关闭" }

func TestClientWarnsOnTokenFilePermissionsOnlyViaConfig(t *testing.T) {
	// 这条用例只是确保 client 不关心 Token 来源（明文/文件都在 config 层处理）。
	client := newTestClient(t, "https://example.com", strings.Repeat("x", 40), true)
	if client.cfg.Token == "" {
		t.Fatal("Token 应当原样保留")
	}
}
