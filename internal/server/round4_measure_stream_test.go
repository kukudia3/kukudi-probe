package server

// 第四轮「量级测量」补口之二：ROUND4-GAPS 的 02-5（L4 的字节率）。
//
// 要回答的是：稳态下 SSE 到底每秒往一个客户端发多少字节？"变更集其实是全量"
// （见 api_stream.go 的注释）是不是真的 —— 也就是每一帧里是不是 20 个节点都在。
//
// 现场（与第二轮同一形状）：真服务端 + 20 个节点各一条**真 WebSocket Agent 连接**、
// 每秒一帧新上报（等价默认 interval_sec=1 的稳态）+ **管理员会话的真 SSE 长连接**。
//
// 三份字节账同时记，各自的口径写在方法注释里：
//
//	负载字节："data: " 后面那段 JSON 本身
//	事件字节："event: nodes\ndata: <JSON>\n\n" 的完整字节（HTTP 正文里的那一帧）
//	线上字节：TCP 代理上数到的"服务端→客户端"全部字节（含响应头、chunked 分块）
//
// 压缩测量：SSE 是**会被 gzip 的**（compress.go 的 streaming()：text/event-stream
// 不受 gzipMinSize 约束，声明 Accept-Encoding: gzip 就压）。真浏览器一定声明，
// 所以"线上每秒多少字节"必须分"明文"与"gzip"两行看。
//
// 运行：
//
//	go test ./internal/server/ -run TestMeasureStreamOutboundBytes -count=1 -v

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"probe/internal/protocol"
	"probe/internal/store"
)

// countingReader 只数读出多少字节。
type countingReader struct {
	r io.Reader
	n atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

// countingForward 把字节**转发**给 w，同时累加计数。
//
// ⚠️ 必须真的转发：io.Copy(dst, src) 里 dst 就是数据的终点，一个"只数不写"的
// 写入端会把整条链路吞掉 —— 第一版就是这么写的（代理把客户端的请求读进计数器
// 就扔了，服务端一个字节都没收到 ⇒ 请求永远等不到响应，用例挂死 10 分钟）。
// 这个坑是靠"直连对照"抓出来的：同样的请求直连 httptest 200，过代理就超时。
type countingForward struct {
	w io.Writer
	n *atomic.Int64
}

func (c countingForward) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n.Add(int64(n))
	return n, err
}

// measureProxy 是一个 TCP 转发器：把一条连接原样转给目标，分别数两个方向的字节。
//
// 为什么要它：httptest 的 ResponseWriter 只能数到 handler 写进正文的字节，
// 数不到 HTTP 响应头与 chunked 分块；而"每秒外发多少字节"问的是网线上的字节。
// 代理是唯一能同时给出"正文"与"线上"两个口径的位置 —— 而且它只承载我们这一条
// SSE 连接（Agent 的 WebSocket 不经过它），不会把 Agent 的流量混进来。
type measureProxy struct {
	ln       net.Listener
	target   string
	toClient atomic.Int64
	toServer atomic.Int64
	conns    atomic.Int64
}

func newMeasureProxy(t *testing.T, target string) *measureProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("起代理: %v", err)
	}
	p := &measureProxy{ln: ln, target: target}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			p.conns.Add(1)
			go p.serve(c)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return p
}

func (p *measureProxy) addr() string { return p.ln.Addr().String() }

func (p *measureProxy) serve(down net.Conn) {
	defer func() { _ = down.Close() }()
	up, err := net.Dial("tcp", p.target)
	if err != nil {
		return
	}
	defer func() { _ = up.Close() }()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// 客户端 → 服务端；客户端收工时把 up 的写半边关掉（服务端据此看到请求结束）。
		_, _ = io.Copy(countingForward{w: up, n: &p.toServer}, down)
		if tc, ok := up.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}()
	// 服务端 → 客户端；服务端收工时把 down 的写半边关掉（客户端据此看到响应结束）。
	_, _ = io.Copy(countingForward{w: down, n: &p.toClient}, up)
	if tc, ok := down.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
	}
	wg.Wait()
}

// measureStreamFrame 是一帧 SSE 事件。
type measureStreamFrame struct {
	payload int // "data: " 后面的 JSON 字节
	event   int // 整帧（含 "event: nodes\n" / "data: " 前缀与结尾空行）
	nodes   int // 这一帧里的节点数
	wire    int64
	body    int64
	at      time.Time
}

// measureStreamResult 是一条 SSE 连接在一个窗口里的统计（不含第一帧全量快照）。
type measureStreamResult struct {
	encoding    string
	frames      int
	snapshot    measureStreamFrame
	elapsed     time.Duration
	payloadB    int
	eventB      int
	wireB       int64
	bodyB       int64
	heartbeats  int
	heartbeatB  int
	minNodes    int
	maxNodes    int
	lastNodes   int
	firstFrame  measureStreamFrame
	lastFrame   measureStreamFrame
	totalFrames int
	// includeSnapshotB/Sec 是"把建连时那一帧也当稳态"的口径（第二轮就是这么算的，
	// 见测试里的说明），用来把本轮的稳态数字与他们的 32165 B/s 对上。
	includeSnapshotB   int64
	includeSnapshotSec time.Duration
}

// readStreamWindow 连上 SSE、读掉第一帧（全量快照），再统计 window 时长的稳态窗口。
//
// 边界是**帧边界**而不是墙上时钟：每一帧读完的那一刻同时取代理计数与正文计数，
// 于是"窗口内的字节" = 末帧计数 − 快照帧的计数，正好是整数帧，不存在
// "窗口边缘切掉半帧"的误差。
func readStreamWindow(t *testing.T, h *authHarness, proxy *measureProxy, encoding string, window time.Duration) measureStreamResult {
	t.Helper()
	client := &http.Client{
		Jar: h.client.Jar,
		Transport: &http.Transport{
			DisableCompression:  true, // 自己控 Accept-Encoding，别让 Go 偷偷解压
			MaxIdleConnsPerHost: 4,
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+proxy.addr()+"/api/v1/stream", nil)
	if err != nil {
		t.Fatalf("构造 SSE 请求: %v", err)
	}
	if encoding != "" {
		req.Header.Set("Accept-Encoding", encoding)
	}
	requestedAt := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("连接 SSE 失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("SSE 状态码 = %d，期望 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("SSE Content-Type = %q", ct)
	}

	cr := &countingReader{r: resp.Body}
	var src io.Reader = cr
	if encoding == "gzip" {
		gz, err := gzip.NewReader(cr)
		if err != nil {
			t.Fatalf("gzip 解码器: %v", err)
		}
		src = gz
	}
	br := bufio.NewReaderSize(src, 1<<16)

	res := measureStreamResult{encoding: encoding, minNodes: 1 << 30}
	var (
		frames    []measureStreamFrame
		payload   []byte
		eventSize int
		deadline  time.Time
	)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("读 SSE 流失败（已收 %d 帧）: %v", len(frames), err)
		}
		eventSize += len(line)
		trimmed := strings.TrimRight(line, "\r\n")
		switch {
		case strings.HasPrefix(line, "data: "):
			payload = []byte(strings.TrimPrefix(trimmed, "data: "))
		case trimmed == "":
			// 事件结束。
			if payload != nil {
				var probe struct {
					Nodes []json.RawMessage `json:"nodes"`
					Full  bool              `json:"full"`
				}
				if err := json.Unmarshal(payload, &probe); err != nil {
					t.Fatalf("解析 SSE 负载失败: %v", err)
				}
				frame := measureStreamFrame{
					payload: len(payload), event: eventSize, nodes: len(probe.Nodes),
					wire: proxy.toClient.Load(), body: cr.n.Load(), at: time.Now(),
				}
				frames = append(frames, frame)
				if len(frames) == 1 {
					res.snapshot = frame
					deadline = time.Now().Add(window)
				} else {
					if frame.nodes < res.minNodes {
						res.minNodes = frame.nodes
					}
					if frame.nodes > res.maxNodes {
						res.maxNodes = frame.nodes
					}
					res.lastNodes = frame.nodes
				}
			} else if strings.HasPrefix(trimmed, ":") {
				res.heartbeats++
				res.heartbeatB += eventSize
			}
			payload, eventSize = nil, 0
		}
		if len(frames) >= 2 && time.Now().After(deadline) {
			break
		}
	}

	res.totalFrames = len(frames)
	res.frames = len(frames) - 1
	res.firstFrame = frames[1]
	res.lastFrame = frames[len(frames)-1]
	res.elapsed = res.lastFrame.at.Sub(res.snapshot.at)
	for _, fr := range frames[1:] {
		res.payloadB += fr.payload
		res.eventB += fr.event
	}
	res.wireB = res.lastFrame.wire - res.snapshot.wire
	res.bodyB = res.lastFrame.body - res.snapshot.body
	// 第二轮那种口径：**把建连时那一帧全量快照也算进速率的分母**（他们的 4 秒窗口
	// 里有 5 帧 = 1 快照 + 4 拍，于是"每秒字节"里含着一次性的快照）。
	res.includeSnapshotB = int64(res.snapshot.event+res.eventB) / 1
	res.includeSnapshotSec = res.lastFrame.at.Sub(requestedAt)
	return res
}

// TestMeasureStreamOutboundBytes 是 02-5：稳态 SSE 每秒外发多少字节。
func TestMeasureStreamOutboundBytes(t *testing.T) {
	const nodeCount = 20
	cfg := measureConfig(nodeCount, true)
	h := newAuthHarnessWithConfig(t, cfg)
	ctx := context.Background()

	// 20 个真 Agent：各一条 WebSocket、每秒一帧 metrics。
	conns := make([]*websocket.Conn, 0, nodeCount)
	nodeIDs := make([]int64, 0, nodeCount)
	for i := 0; i < nodeCount; i++ {
		node, token, err := h.srv.db.CreateNode(ctx, store.NewNode{
			Name: fmt.Sprintf("s-%02d", i+1), IntervalSec: 1, TrafficWarnPct: 80, ResetDay: 1,
		}, time.Now())
		if err != nil {
			t.Fatalf("建节点 %d: %v", i+1, err)
		}
		nodeIDs = append(nodeIDs, node.ID)
		conn := mustDialAgent(t, h.ts, token)
		sendFrame(t, conn, helloFrame(t, testHello()))
		readHandshake(t, conn)
		conns = append(conns, conn)
	}

	reporters := make([]*measureAgentReporter, 0, nodeCount)
	for _, conn := range conns {
		reporters = append(reporters, startMeasureReporter(t, conn))
	}
	// 等每个节点都至少进过一次上报（否则第一帧变更集里会少节点）。
	waitFor(t, 10*time.Second, "20 个节点都有过上报", func() bool {
		for _, id := range nodeIDs {
			st, ok := h.srv.state.Get(id)
			if !ok || st.Seq == 0 {
				return false
			}
		}
		return true
	})
	defer func() {
		for _, r := range reporters {
			r.stopAndWait()
		}
	}()

	// 三次"明文"（与第二轮同一口径）+ 两次"gzip"（真浏览器的口径）。
	reps := []struct {
		encoding string
		count    int
	}{{"", 3}, {"gzip", 2}}
	for _, rep := range reps {
		for i := 1; i <= rep.count; i++ {
			proxy := newMeasureProxy(t, h.ts.Listener.Addr().String())
			res := readStreamWindow(t, h, proxy, rep.encoding, 8*time.Second)
			encoding := rep.encoding
			if encoding == "" {
				encoding = "明文"
			}
			perSec := func(n float64) float64 { return n / res.elapsed.Seconds() }
			t.Logf("[%s 第 %d 次] 窗口 %.3fs，稳态帧 %d 帧（含快照共 %d 帧，%.2f 帧/秒）；"+
				"帧内节点数 min/max/末=%d/%d/%d（共 %d 个节点）",
				encoding, i, res.elapsed.Seconds(), res.frames, res.totalFrames,
				perSec(float64(res.frames)), res.minNodes, res.maxNodes, res.lastNodes, nodeCount)
			t.Logf("    快照帧：负载 %d 字节 / 整帧 %d 字节 / **线上 %d 字节**（含响应头与 chunked 分块，"+
				"建连时一次性）；稳态：负载 %.0f B/s、整帧 %.0f B/s、"+
				"正文 %.0f B/s、**线上 %.0f B/s**（每分钟 %.1f KB）；心跳 %d 个（%d 字节，已含在正文里）",
				res.snapshot.payload, res.snapshot.event, res.snapshot.wire,
				perSec(float64(res.payloadB)), perSec(float64(res.eventB)),
				perSec(float64(res.bodyB)), perSec(float64(res.wireB)),
				perSec(float64(res.wireB))*60/1024,
				res.heartbeats, res.heartbeatB)
			t.Logf("    每帧平均：负载 %.1f 字节、整帧 %.1f 字节；每节点每秒（整帧口径）%.1f B/s",
				float64(res.payloadB)/float64(res.frames), float64(res.eventB)/float64(res.frames),
				perSec(float64(res.eventB))/float64(nodeCount))
			t.Logf("    「含建连快照」口径（第二轮 32165 B/s 的算法）：%d 字节 / %.3fs = **%.0f B/s**"+
				"（多算了建连时那一帧的一次性 1.0 帧，所以比稳态高 %.1f%%）",
				res.includeSnapshotB, res.includeSnapshotSec.Seconds(),
				float64(res.includeSnapshotB)/res.includeSnapshotSec.Seconds(),
				(float64(res.includeSnapshotB)/res.includeSnapshotSec.Seconds())/perSec(float64(res.eventB))*100-100)
			if res.frames < 4 {
				t.Fatalf("[%s] 8 秒窗口只收到 %d 帧稳态帧：节点没有在每秒上报，数字不能用",
					encoding, res.frames)
			}
			if res.lastNodes != nodeCount || res.maxNodes != nodeCount {
				t.Errorf("[%s] 帧内节点数 = %d（末帧）/ %d（最大），期望 %d：稳态变更集不是全量，"+
					"L4 的结论要重写", encoding, res.lastNodes, res.maxNodes, nodeCount)
			}
			if res.minNodes == 0 {
				t.Errorf("[%s] 有帧一个节点都不带", encoding)
			}
		}
	}
}

// measureMetrics 造一帧"每秒都在变"的指标：真实部署里 CPU/内存/流量/延迟每一拍
// 都不一样，而 gzip 的压缩比**极度依赖"这一帧与上一帧像不像"**。若用恒定的
// testMetrics()，连续帧几乎逐字节相同、deflate 的窗口能把它压到几百字节 ——
// 那是压缩比的最好情况，不是常态。所以这里按 seq 让每个数值都动起来。
func measureMetrics(seq uint64) protocol.Metrics {
	m := testMetrics()
	s := float64(seq)
	m.CPUPct = 12.5 + float64(seq%17)*1.73
	m.Mem = protocol.Mem{Total: 1 << 30, Used: (1 << 29) + uint64(seq%997)*7919, Pct: 50 + float64(seq%23)*0.37}
	m.Swap = protocol.Mem{Total: 1 << 28, Used: uint64(seq%53) * 131071, Pct: float64(seq%53) * 0.31}
	m.Disk = []protocol.Disk{{Mount: "/", FS: "ext4", Total: 1 << 40, Used: (1 << 39) + uint64(seq%811)*104729, Pct: 50 + float64(seq%11)*0.13}}
	m.Load = protocol.Load{L1: 0.5 + float64(seq%29)*0.13, L5: 0.4 + float64(seq%31)*0.11, L15: 0.3 + float64(seq%37)*0.07}
	m.Net = protocol.Net{
		Iface: "eth0", RxTotal: (1 << 30) + seq*1048573, TxTotal: (1 << 29) + seq*524287,
		RxRaw: (1 << 31) + seq*2097143, TxRaw: (1 << 30) + seq*1048571,
		RxRate: 1024 + s*7.3, TxRate: 512 + s*3.1, BootID: "boot-1", CkptAgeS: 2,
	}
	m.LatMS = 23.4 + float64(seq%41)*0.37
	m.UptimeSec = 1000 + seq
	return m
}

// measureAgentReporter 每秒给一条 Agent 连接发一帧 metrics（seq 递增）。
type measureAgentReporter struct {
	conn *websocket.Conn
	seq  atomic.Uint64
	sent atomic.Uint64
	errs atomic.Int64
	stop chan struct{}
	done chan struct{}
}

func startMeasureReporter(t *testing.T, conn *websocket.Conn) *measureAgentReporter {
	t.Helper()
	r := &measureAgentReporter{conn: conn, stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(r.done)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-r.stop:
				return
			case <-ticker.C:
			}
			seq := r.seq.Add(1)
			env, err := protocol.New(protocol.TypeMetrics, measureMetrics(seq))
			if err != nil {
				r.errs.Add(1)
				continue
			}
			env.Seq = seq
			data, err := env.Encode()
			if err != nil {
				r.errs.Add(1)
				continue
			}
			writeCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			err = conn.Write(writeCtx, websocket.MessageText, data)
			cancel()
			if err != nil {
				// 连接被关掉（用例收尾）是正常的：不当错误刷屏。
				select {
				case <-r.stop:
				default:
					r.errs.Add(1)
				}
				return
			}
			r.sent.Add(1)
		}
	}()
	return r
}

func (r *measureAgentReporter) stopAndWait() {
	close(r.stop)
	select {
	case <-r.done:
	case <-time.After(5 * time.Second):
	}
}
