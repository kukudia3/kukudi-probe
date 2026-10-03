package agent

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"probe/internal/protocol"
)

// 第二轮安全审计（SECURITY-AUDIT-ROUND2.md）03-A-5 的回归用例。
//
// 报告的问题：明文拦截（"拒绝以明文连接非本机地址"）只作用在 wsURL() 构造初始
// URL 那一步，而 websocket.Dial 会跟随重定向 —— 服务端回一个
// `302 Location: http://<其它地址>/...` 就能让 Agent 连到一个非环回的明文地址，
// `--allow-plaintext` 声称的"只连本机明文"被一次重定向破掉（同域跳转还会把
// Bearer Token 一起明文送出去）。
//
// 修法：给 http.Client 装 CheckRedirect（见 client.go 的 checkRedirect），按跳
// 复核与 wsURL 相同的策略，并把 net/http 因为"库自己装了 CheckRedirect"而失效的
// 10 跳上限补回来。

// TestHandshakeRedirectEnforcesPlaintextPolicy 直接驱动策略函数：合法目标放行、
// 明文外网/内网目标拒绝、跳数封顶。
//
// 反向验证：把 NewClient 里的 `c.http.CheckRedirect = c.checkRedirect` 删掉，
// 本用例仍然会过（它测的是策略函数本身）—— 所以"装配"由下面那条真实拨号的用例钉住。
func TestHandshakeRedirectEnforcesPlaintextPolicy(t *testing.T) {
	traffic, _, err := LoadTraffic("")
	if err != nil {
		t.Fatalf("LoadTraffic: %v", err)
	}
	collector := New(filepath.Join("testdata", "root"), "", "/", traffic)

	cases := []struct {
		name     string
		allow    bool
		location string
		wantErr  string // 空 = 应当放行
	}{
		{"明文重定向到外网域名", false, "http://agent.example.com:9999/api/v1/agent/ws", "拒绝跟随重定向到明文地址"},
		{"明文重定向到内网地址", false, "http://10.0.0.5:25774/api/v1/agent/ws", "拒绝跟随重定向到明文地址"},
		{"明文重定向到公网 IP", false, "http://198.51.100.7:8080/api/v1/agent/ws", "拒绝跟随重定向到明文地址"},
		{"明文重定向到本机环回", false, "http://127.0.0.1:25774/api/v1/agent/ws", ""},
		{"明文重定向到 localhost", false, "http://localhost:25774/api/v1/agent/ws", ""},
		{"明文重定向到 IPv6 环回", false, "http://[::1]:25774/api/v1/agent/ws", ""},
		{"显式允许明文时放行", true, "http://10.0.0.5:25774/api/v1/agent/ws", ""},
		{"重定向到 TLS 放行", false, "https://monitor.example.com/api/v1/agent/ws", ""},
		{"重定向到 wss 放行", false, "wss://monitor.example.com/api/v1/agent/ws", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := NewClient(ClientConfig{
				ServerURL: "wss://monitor.example.com", Token: "x", AllowPlaintext: tc.allow,
			}, collector, traffic, nil)
			req, err := http.NewRequest(http.MethodGet, tc.location, nil)
			if err != nil {
				t.Fatalf("构造请求: %v", err)
			}
			// 库那层已经把 ws/wss 的 Location 改写成 http/https 再调用本函数，
			// 所以这里模拟的就是它交过来的形状。
			if u, _ := url.Parse(tc.location); u.Scheme == "wss" {
				req.URL.Scheme = "https"
			}
			err = c.checkRedirect(req, []*http.Request{req})
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("应当放行，实际 %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("应当拒绝")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("错误信息 = %q，期望含 %q", err.Error(), tc.wantErr)
			}
		})
	}

	// 跳数上限：库装了 CheckRedirect 之后 net/http 的默认 10 跳不再生效，必须自己补。
	t.Run("跳数上限", func(t *testing.T) {
		c := NewClient(ClientConfig{ServerURL: "wss://monitor.example.com", Token: "x"}, collector, traffic, nil)
		req, _ := http.NewRequest(http.MethodGet, "https://monitor.example.com/api/v1/agent/ws", nil)
		for _, n := range []int{1, maxHandshakeRedirects - 1} {
			if err := c.checkRedirect(req, make([]*http.Request, n)); err != nil {
				t.Fatalf("%d 跳应当放行，实际 %v", n, err)
			}
		}
		err := c.checkRedirect(req, make([]*http.Request, maxHandshakeRedirects))
		if err == nil || !strings.Contains(err.Error(), "重定向超过") {
			t.Fatalf("第 %d 跳应当被拒，实际 %v", maxHandshakeRedirects, err)
		}
	})
}

// TestClientRefusesPlaintextRedirectOnRealDial 钉住"装配"这件事：
// 真正拨号时（走 websocket.Dial + 客户端的 http.Client）那次 302 **不会**被跟随到
// 明文地址，而且原因会随错误一路传上来（进日志、可诊断）。
//
// 反向验证：删掉 NewClient 里的 `c.http.CheckRedirect = c.checkRedirect`，
// 本用例红在"错误里应当有明文策略的说明"那一条：真实拨号会去连
// http://198.51.100.7:9（TEST-NET-2，不可路由），拿到的是一个网络错误
// （连接超时/"network is unreachable"），而不是我们这条策略错误。
func TestClientRefusesPlaintextRedirectOnRealDial(t *testing.T) {
	// 一个只会 302 到**明文外网地址**的"服务端"。
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://198.51.100.7:9/api/v1/agent/ws", http.StatusFound)
	}))
	t.Cleanup(ts.Close)

	traffic, _, err := LoadTraffic("")
	if err != nil {
		t.Fatalf("LoadTraffic: %v", err)
	}
	collector := New(filepath.Join("testdata", "root"), "", "/", traffic)

	var logs bytes.Buffer
	c := NewClient(ClientConfig{ServerURL: ts.URL, Token: "pba_x"},
		collector, traffic, slog.New(slog.NewTextHandler(&logs, nil)))
	wsURL, err := c.wsURL()
	if err != nil {
		t.Fatalf("wsURL: %v（httptest 是 127.0.0.1，本机明文应当允许）", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// 直接用客户端自己的 http.Client 拨号：这正是 session() 走的那条路
	// （websocket.Dial 会在这上面包一层 CheckRedirect，再回调我们的策略）。
	_, _, err = websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPClient:   c.http,
		HTTPHeader:   http.Header{"Authorization": []string{"Bearer pba_x"}},
		Subprotocols: []string{clientSubprotocol},
	})
	if err == nil {
		t.Fatal("握手不应当成功")
	}
	if !strings.Contains(err.Error(), "拒绝跟随重定向到明文地址") {
		t.Fatalf("错误里应当有明文策略的说明，实际 %v", err)
	}

	// 行为契约（三条一起钉住）：
	//   ① 拒绝的原因是**可诊断**的 —— websocket.Dial 在握手请求失败时**不**返回
	//      响应，所以这条错误会原样传上来，不会被换成一句 "HTTP 302"；
	//   ② 它**不是**永久错误 —— Agent 按常规退避继续重试（最长 60 秒一次），
	//      反代配置改回来就自愈，不需要人肉重启每一台被监控机；
	//   ③ 于是原因会随 Run 那条"连接中断，稍后重连"的 WARN 一起进日志。
	sessionErr := c.session(ctx)
	if sessionErr == nil {
		t.Fatal("session 应当以失败结束")
	}
	if !strings.Contains(sessionErr.Error(), "拒绝跟随重定向到明文地址") {
		t.Fatalf("session 的错误里应当保留原因，实际 %v", sessionErr)
	}
	var perm *permanentError
	if asPermanent(sessionErr, &perm) {
		t.Fatalf("跳转被拒不该是永久错误（会让 Agent 直接退出），实际 %v", sessionErr)
	}

	runCtx, cancelRun := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelRun()
	if err := c.Run(runCtx); err != nil {
		t.Fatalf("「跳转被拒」不该让 Run 退出（退避重试即可）：%v", err)
	}
	if !strings.Contains(logs.String(), "拒绝跟随重定向到明文地址") {
		t.Fatalf("拒绝的原因应当进日志，实际 %q", logs.String())
	}
}

// redirectStub 是一个"先 302、再正常握手"的最小服务端：
// 只用来证明**合法的重定向照旧可用**（修复没有把跟随重定向这条路一起关掉）。
type redirectStub struct {
	t        *testing.T
	redirect string

	mu     sync.Mutex
	hops   int
	hellos int
}

func newRedirectStub(t *testing.T, redirect string) (*redirectStub, *httptest.Server) {
	t.Helper()
	s := &redirectStub{t: t, redirect: redirect}
	ts := httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(ts.Close)
	return s, ts
}

func (s *redirectStub) handle(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.hops++
	s.mu.Unlock()

	// 只要还不是"跳转后的那个地址"就 302 过去 —— 必须**按路径**判断，
	// 不能只跳第一次：Agent 的 Run 会退避重连，那次重连也会打到同一个 handler，
	// 只跳第一次的话第二次就被直接握手成功了，用例会变成空断言
	// （重定向根本没被跟随，却看起来通过了）。
	if s.redirect != "" && r.URL.Path != s.redirect {
		http.Redirect(w, r, s.redirect, http.StatusFound)
		return
	}
	if r.Header.Get("Authorization") != "Bearer pba_stub" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{clientSubprotocol}})
	if err != nil {
		return
	}
	defer func() { _ = conn.CloseNow() }()
	ctx := r.Context()

	_, data, err := conn.Read(ctx)
	if err != nil {
		return
	}
	frame, err := protocol.Decode(data)
	if err != nil || frame.T != protocol.TypeHello {
		return
	}
	s.mu.Lock()
	s.hellos++
	s.mu.Unlock()

	welcome, err := protocol.New(protocol.TypeWelcome, protocol.Welcome{
		NodeID: 7, Name: "redirected", IntervalSec: 1,
		ServerTime: time.Now().Unix(), ConfigVersion: 1,
	})
	if err != nil {
		return
	}
	if err := writeEnvelope(ctx, conn, welcome); err != nil {
		return
	}
	cfg, err := protocol.New(protocol.TypeConfig, protocol.Config{
		ConfigVersion: 1, IntervalSec: 1, PingIntervalSec: 60,
	})
	if err != nil {
		return
	}
	_ = writeEnvelope(ctx, conn, cfg)
	// 之后只读不回：--once 模式的客户端发完一帧就自己收尾。
	for {
		if _, _, err := conn.Read(ctx); err != nil {
			return
		}
	}
}

func writeEnvelope(ctx context.Context, conn *websocket.Conn, env protocol.Envelope) error {
	data, err := env.Encode()
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return conn.Write(wctx, websocket.MessageText, data)
}

func (s *redirectStub) helloCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hellos
}

// TestClientFollowsLoopbackRedirect 钉住"没有把跟随重定向一起关掉"：
// 302 到本机明文地址（与 wsURL 允许的初始地址同一种）时，握手照旧完成，
// 也就是说修复只收紧了**跨到非本机明文**这一种跳转。
//
// 反向验证：把 checkRedirect 改成"一律拒绝跟随"（返回 http.ErrUseLastResponse
// 或任意错误），本用例红在"重定向后的握手应当完成"那一条 t.Fatalf 上。
func TestClientFollowsLoopbackRedirect(t *testing.T) {
	stub, ts := newRedirectStub(t, "/hop/api/v1/agent/ws")
	client := newTestClient(t, ts.URL, "pba_stub", true)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Run(ctx); err != nil {
		t.Fatalf("跟随本机明文重定向应当干净退出，实际 %v", err)
	}

	waitUntil(t, 2*time.Second, "重定向后的握手完成", func() bool { return stub.helloCount() >= 1 })
}

// waitUntil 轮询等待一个条件（Agent 侧的用例此前没有这个helper）。
func waitUntil(t *testing.T, timeout time.Duration, what string, cond func() bool) {
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
