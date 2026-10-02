package server

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"probe/internal/protocol"
)

// 这一批用例专门盯 SSE：**gzip + 流式响应**是加压缩时最经典的翻车方式。
//
// 翻车的形态很特别：连接活着、HTTP 200、Content-Type 也对，但一个字节都到不了
// 浏览器 —— 因为 flate 把不足一个块的输入攒在窗口里。页面上看起来就是整个面板
// 卡死，而服务端日志一切正常。所以这里的断言必须是**逐帧计时**：
// "最终收到了"这种断言对此完全无效（连接断开时数据会一次性到达，它照样成立）。

// countingConn 记下这条连接一共从网线上收到多少字节，以及开头与**末尾**的一段原文。
//
// 反向验证（把 gz.Flush 去掉）时要靠它说清"到底到没到、到了多少"：
// 那种状态下线上只会出现 gzip 的 10 字节文件头，而末尾那段 hexdump 就是证据
// （1f 8b 08 00 … 之后什么都没有）。
type countingConn struct {
	net.Conn
	n    int
	head []byte
	tail []byte
}

func (c *countingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.n += n
	if n > 0 {
		if len(c.head) < 64 {
			c.head = append(c.head, p[:n]...)
		}
		c.tail = append(c.tail, p[:n]...)
		if len(c.tail) > 64 {
			c.tail = append(c.tail[:0], c.tail[len(c.tail)-64:]...)
		}
	}
	return n, err
}

// sseWire 是一条裸 TCP 的 SSE 连接。
//
// 为什么不用 http.Client：要给**每一帧**单独定死到达期限（SetReadDeadline），
// 还要数网线上的原始字节 —— 这两件事在 http.Client 的封装里都做不到。
type sseWire struct {
	conn    net.Conn
	counter *countingConn
	body    io.Reader // 明文时是 resp.Body；gzip 时是 gzip.Reader
	header  http.Header
}

// dialSSEWire 连到真服务端的 /api/v1/stream 上，返回这条流。
func dialSSEWire(t *testing.T, base, cookie string, withGzip bool) *sseWire {
	t.Helper()
	addr := strings.TrimPrefix(base, "http://")
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("连接 %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	counter := &countingConn{Conn: conn}
	accept := ""
	if withGzip {
		accept = "Accept-Encoding: gzip\r\n"
	}
	req := "GET /api/v1/stream HTTP/1.1\r\nHost: " + addr + "\r\n" + accept +
		"Cookie: " + cookie + "\r\n\r\n"
	if _, err := io.WriteString(counter, req); err != nil {
		t.Fatalf("发送 SSE 请求: %v", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("设置读超时: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(counter), &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatalf("读 SSE 响应头: %v（线上已收到 %d 字节）", err, counter.n)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("SSE 状态码 = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("SSE Content-Type = %q", ct)
	}

	w := &sseWire{conn: conn, counter: counter, body: resp.Body, header: resp.Header}
	if withGzip {
		if enc := resp.Header.Get("Content-Encoding"); enc != "gzip" {
			t.Fatalf("SSE 声明了 gzip 却没压：Content-Encoding = %q", enc)
		}
		zr, err := gzip.NewReader(resp.Body)
		if err != nil {
			t.Fatalf("SSE 的 gzip 头解析失败: %v（线上已收到 %d 字节：%s）",
				err, counter.n, hex.Dump(counter.head))
		}
		w.body = zr
	}
	return w
}

// readFrames 逐帧读 n 条 data 事件，每条都必须在上一条到达后的 perFrame 内
// **完整**到达（读到分隔的空行为止）。超时/断流时报出线上实际收到了多少字节。
func (w *sseWire) readFrames(t *testing.T, n int, perFrame time.Duration) []time.Duration {
	t.Helper()
	sc := bufio.NewScanner(w.body)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)

	var gaps []time.Duration
	last := time.Now()
	for len(gaps) < n {
		if err := w.conn.SetReadDeadline(time.Now().Add(perFrame)); err != nil {
			t.Fatalf("设置读超时: %v", err)
		}
		var data string
		for {
			if !sc.Scan() {
				t.Fatalf("SSE 第 %d 帧没有在 %s 内完整到达（%v）。\n"+
					"已完整收到 %d 帧；这条连接一共只从网线上读到 %d 字节。\n"+
					"开头收到的：\n%s末尾收到的（最后 64 字节）：\n%s",
					len(gaps)+1, perFrame, sc.Err(), len(gaps), w.counter.n,
					hex.Dump(w.counter.head), hex.Dump(w.counter.tail))
			}
			line := sc.Text()
			if line == "" {
				break
			}
			if strings.HasPrefix(line, "data: ") {
				data = line
			}
		}
		if data == "" {
			continue // ": ping" 心跳注释帧
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(data, "data: ")), &payload); err != nil {
			t.Fatalf("SSE 数据不是合法 JSON: %v", err)
		}
		if payload["type"] != "nodes" {
			t.Fatalf("SSE 帧类型 = %v，期望 nodes", payload["type"])
		}
		now := time.Now()
		gaps = append(gaps, now.Sub(last))
		last = now
	}
	return gaps
}

// startAgentFeed 用**真 Agent 通道**给节点喂数据，让它每秒都有变化。
//
// 为什么必须喂：realtimeLoop 只推"变了的"节点（见 api_stream.go），没有新上报时
// SSE 上只剩 15 秒一次的心跳注释 —— 根本凑不出"连续 8 帧"。
func startAgentFeed(t *testing.T, ts *httptest.Server, s *Server, token string, nodeID int64) {
	t.Helper()
	conn := mustDialAgent(t, ts, token)
	sendFrame(t, conn, helloFrame(t, testHello()))
	readHandshake(t, conn)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(900 * time.Millisecond)
		defer ticker.Stop()
		for i := 1; ; i++ {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			env, err := protocol.New(protocol.TypeMetrics, testMetrics())
			if err != nil {
				return
			}
			env.Seq = uint64(i)
			data, err := env.Encode()
			if err != nil {
				return
			}
			wctx, wcancel := context.WithTimeout(context.Background(), 3*time.Second)
			err = conn.Write(wctx, websocket.MessageText, data)
			wcancel()
			if err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	waitFor(t, 5*time.Second, "节点收到第一帧上报", func() bool {
		n, ok := s.State().Get(nodeID)
		return ok && n.Seq >= 1
	})
}

// TestGzipSSEDeliversEveryFramePromptly 是这次改动最核心的用例。
//
// 连续 8 帧，**逐帧**断言：每一条事件都必须在上一条之后的 3 秒内完整到达。
// 只断言"最终收到了"是发现不了"被压死"的：连接断开时攒住的数据会一次性到达，
// 那种断言照样成立，而用户看到的是面板整天不动。
func TestGzipSSEDeliversEveryFramePromptly(t *testing.T) {
	h := newAuthHarness(t)
	node, token := createTestNode(t, h.srv, "sse-gz")
	cookie := sessionCookie(t, h)
	startAgentFeed(t, h.ts, h.srv, token, node.ID)

	packed := dialSSEWire(t, h.ts.URL, cookie, true)
	gaps := packed.readFrames(t, 8, 3*time.Second)
	for i, d := range gaps {
		t.Logf("第 %d 帧：距上一帧 %s", i+1, d.Round(time.Millisecond))
		if d > 3*time.Second {
			t.Errorf("第 %d 帧来晚了：%s", i+1, d)
		}
	}
	packedBytes := packed.counter.n

	// 同样读 8 帧、不带 gzip 的那条流，用来量"gzip 前后 SSE 的字节数"。
	plain := dialSSEWire(t, h.ts.URL, cookie, false)
	plain.readFrames(t, 8, 3*time.Second)
	plainBytes := plain.counter.n

	t.Logf("8 帧 SSE 的网线字节：明文 %d → gzip %d（%.2f×，含各自约 200 字节响应头）",
		plainBytes, packedBytes, float64(plainBytes)/float64(packedBytes))
	if packedBytes >= plainBytes {
		t.Errorf("SSE 压完没有变小：明文 %d 字节，gzip %d 字节", plainBytes, packedBytes)
	}
}

// TestSSESmallFirstEventIsCompressedImmediately 钉住"SSE 不受阈值约束"。
//
// SSE 的第一条数据只有十几字节（retry 提示），比 gzipMinSize 小得多。
// 如果阈值也套在它身上，要么这条流永远不压（一天几十万帧全发明文），
// 要么第一条被攒在缓冲里 —— 两者都是这次改动要避免的。
//
// dialSSEWire 会断言 Content-Encoding: gzip，而它是在**任何一帧到达之前**
// 就检查响应头的 —— 也就是说：一个 13 字节的开头照样被压了。
func TestSSESmallFirstEventIsCompressedImmediately(t *testing.T) {
	h := newAuthHarness(t)
	node, _ := createTestNode(t, h.srv, "sse-small")
	cookie := sessionCookie(t, h)

	wire := dialSSEWire(t, h.ts.URL, cookie, true)
	if wire.counter.n > 0 {
		t.Logf("收到响应头时线上已有 %d 字节", wire.counter.n)
	}
	gaps := wire.readFrames(t, 1, 3*time.Second)
	t.Logf("gzip 下第一条 SSE 事件在 %s 内到达（网线共 %d 字节，节点 %d 还没上报过）",
		gaps[0].Round(time.Millisecond), wire.counter.n, node.ID)
}

// TestStreamingWriteWithoutExplicitFlushArrivesPromptly 单独钉住"Write 之后那次
// gz.Flush"。
//
// 为什么不能只靠上面那条 SSE 用例：真实的 SSE handler 每条事件之后还会自己调一次
// rc.Flush()（见 api_stream.go 的 writeSSE），而那条路会走到包装器的 FlushError
// —— 两处 flush 互相兜底，只删一处是看不出来的。这条用例用一个**不显式 Flush**
// 的流式 handler，让 Write 里那次 flush 成为唯一的数据出口：
// 少了它，13 字节的事件会一直攒在 flate 的窗口里（直到写满 64 KB 或 handler
// 返回才出去），而这条流在测试里会一直开着。
func TestStreamingWriteWithoutExplicitFlushArrivesPromptly(t *testing.T) {
	s := newTestServer(t)
	release := make(chan struct{})
	ts := httptest.NewServer(s.withMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// 故意**不**调 Flush：数据能不能立刻出去，全靠包装器 Write 里的两次 flush。
		_, _ = io.WriteString(w, "event: tick\ndata: hello\n\n")
		<-release // 流还开着：handler 没有返回，finish 里的收尾还没发生
	})))
	defer ts.Close()
	defer close(release)

	addr := ts.Listener.Addr().String()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("连接 %s: %v", addr, err)
	}
	defer func() { _ = conn.Close() }()
	counter := &countingConn{Conn: conn}
	start := time.Now()

	req := "GET / HTTP/1.1\r\nHost: " + addr + "\r\nAccept-Encoding: gzip\r\n\r\n"
	if _, err := io.WriteString(counter, req); err != nil {
		t.Fatalf("发送请求: %v", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("设置读超时: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(counter), &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatalf("3 秒内没读到响应头: %v（线上只收到 %d 字节：%s）", err, counter.n, hex.Dump(counter.tail))
	}
	if enc := resp.Header.Get("Content-Encoding"); enc != "gzip" {
		t.Fatalf("Content-Encoding = %q", enc)
	}
	zr, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatalf("gzip 头解析失败: %v", err)
	}
	buf := make([]byte, 64)
	n, err := zr.Read(buf)
	if err != nil {
		t.Fatalf("3 秒内没读到事件: %v（线上只收到 %d 字节，末尾是：%s）",
			err, counter.n, hex.Dump(counter.tail))
	}
	if !strings.Contains(string(buf[:n]), "event: tick") {
		t.Fatalf("读到的事件不对：%q", buf[:n])
	}
	t.Logf("handler 没有显式 Flush，事件仍在 %s 内到达（线上共 %d 字节）",
		time.Since(start).Round(time.Millisecond), counter.n)
}

// TestSSEHeartbeatPassesThroughGzip 是心跳那条路的回归：15 秒一次的心跳注释
// 也必须能立刻推出去（它走的是同一个 writer，只是不经过 writeSSE）。
//
// 心跳间隔被调小到 300ms：15 秒在单测里等不起。
func TestSSEHeartbeatPassesThroughGzip(t *testing.T) {
	h := newAuthHarness(t)
	createTestNode(t, h.srv, "sse-ping")
	cookie := sessionCookie(t, h)
	h.srv.streamPingEvery = 300 * time.Millisecond

	wire := dialSSEWire(t, h.ts.URL, cookie, true)

	// 直接读原始文本行：心跳是 ": ping" 注释，readFrames 会把它跳过。
	sc := bufio.NewScanner(wire.body)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := wire.conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
			t.Fatalf("设置读超时: %v", err)
		}
		if !sc.Scan() {
			t.Fatalf("没读到心跳：%v（网线 %d 字节）", sc.Err(), wire.counter.n)
		}
		if strings.HasPrefix(sc.Text(), ": ping") {
			t.Logf("gzip 下心跳按时到达（网线共 %d 字节）", wire.counter.n)
			return
		}
	}
	t.Fatal("5 秒内没等到心跳")
}

// TestGzipSSEBodyDecompressesToPlainStream 反向钉住"内容没坏"：
// 同一个 SSE 流，gzip 版本解出来的文本必须与明文版本**逐字节相同**
// （服务端 1 Hz 推的是同一种信封，两条流在同一秒里内容一致）。
func TestGzipSSEBodyDecompressesToPlainStream(t *testing.T) {
	h := newAuthHarness(t)
	_, _ = createTestNode(t, h.srv, "sse-eq")
	cookie := sessionCookie(t, h)

	// 只取第一条完整的 data 事件（快照帧：同一时刻连的两条流内容相同）。
	readOne := func(withGzip bool) string {
		wire := dialSSEWire(t, h.ts.URL, cookie, withGzip)
		sc := bufio.NewScanner(wire.body)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for {
			if err := wire.conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatalf("设置读超时: %v", err)
			}
			if !sc.Scan() {
				t.Fatalf("没读到 SSE 数据帧：%v（网线 %d 字节：%s）",
					sc.Err(), wire.counter.n, hex.Dump(wire.counter.head))
			}
			if line := sc.Text(); strings.HasPrefix(line, "data: ") {
				return line
			}
		}
	}

	plainEvent := readOne(false)
	packedEvent := readOne(true)
	if plainEvent != packedEvent {
		t.Fatalf("gzip 版与明文版的 SSE 事件不一致：\n明文 %q\ngzip %q", plainEvent, packedEvent)
	}
	t.Logf("gzip 版 SSE 事件解压后与明文逐字节相同（%d 字节）", len(packedEvent))
}
