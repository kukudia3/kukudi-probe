package server

// 第五轮「流量账」：回答"用户每月到底为这个探针花了多少流量、花在哪一段"。
//
// 两段必须**分开**量（用户真正在意的是前一段）：
//
//	① Agent→服务端：每台被监控机 **7×24 小时**都在发，与有没有人看面板无关；
//	   协议是 WebSocket 文本帧 JSON，默认**不协商** permessage-deflate
//	   （client.go 的 DialOptions 与 agentconn.go 的 AcceptOptions 都没开），
//	   也就是这一段在线上是**裸的**。
//	② 服务端→浏览器：只在有人开着面板时才有；真浏览器会声明 Accept-Encoding: gzip，
//	   SSE 因此会被 gzip（compress.go 的 streaming()）。这一段 ROUND4-GAPS-MEASURE
//	   已经量过（稳态 29,009–29,296 B/s 明文 / 373–375 B/s gzip），本轮复测。
//
// 本文件只**测量与打印**，不改任何生产行为；每个用例的判据写在注释里。
//
// 运行：
//
//	go test ./internal/server/ -run 'TestMeasureTraffic' -count=1 -v

import (
	"compress/flate"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"probe/internal/agent"
	"probe/internal/protocol"
	"probe/internal/store"
)

// r5AgentRoot 是 Agent 采集器的"假 /"快照（与 internal/agent 的基准测试同一份夹具）。
// 测试的工作目录是 internal/server，所以路径是 ../agent/testdata/root。
const r5AgentRoot = "../agent/testdata/root"

// r5MinAgentCompressionRatio 是 Agent→服务端 那一段**必须**达到的压缩比下限。
//
// 它是一条"有齿"的判据，不是"越接近实测越好"：
//   - 把 CompressionMode 改回 CompressionDisabled（改动前的样子）⇒ 实测 1.0× ⇒ 红；
//   - 改成 CompressionNoContextTakeover（弱一档：每帧重置窗口）⇒ 实测约 1.7× ⇒ 红；
//   - 本机实测（520 字节的帧、内容每拍都在变）3.6~3.8×。
//
// 取 2.5 是"比 NoContextTakeover 明显高、又给慢机器与内容差异留余量"的位置；
// 实测值会连同参照组的明文字节一起打进日志，所以放宽到这里不会把结论变模糊。
const r5MinAgentCompressionRatio = 2.5

// ---------- 通用小工具 ----------

// r5NewCollectorAt 造一个采集器，根指向给定的"假 /"（与真 Agent 走同一份采集实现）。
func r5NewCollectorAt(t *testing.T, root string) *agent.Collector {
	t.Helper()
	tr, warn, err := agent.LoadTraffic("")
	if err != nil {
		t.Fatalf("LoadTraffic: %v", err)
	}
	if warn != "" {
		t.Logf("LoadTraffic 提示: %q", warn)
	}
	return agent.New(root, "eth0", "/", tr)
}

// r5MutatingRoot 把 Agent 夹具复制到临时目录，并让一个后台 goroutine 每 200ms 改一改
// 那几个"真机上一直在动"的文件（/proc/stat 的 CPU 计数、/proc/meminfo 的
// MemFree/MemAvailable、/proc/net/dev 的 eth0 收发计数、/proc/loadavg、/proc/uptime）。
//
// 为什么要它：连续帧逐字节相同是 deflate 的**最好情况**（窗口一把全命中）。
// A/B 必须在"每一拍都不一样"的内容上做，量出来的倍数才代表真实部署 ——
// 真机上的 CPU、内存、累计流量、速率、延迟每一拍都在变。
func r5MutatingRoot(t *testing.T) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "root")
	if err := r5CopyTree(r5AgentRoot, dst); err != nil {
		t.Fatalf("复制夹具: %v", err)
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		step := 0
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
			}
			step++
			r5MutateFixture(dst, step)
		}
	}()
	t.Cleanup(func() {
		close(stop)
		<-done
	})
	return dst
}

// r5CopyTree 递归复制一棵小目录树（夹具只有二十来个文件）。
func r5CopyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}

// r5BumpLine 把文件里以 prefix 开头的那一行的第 idx 个字段改成 base（其余原样保留）。
func r5BumpLine(path, prefix string, idx int, value string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) <= idx {
			return
		}
		fields[idx] = value
		if prefix == "cpu " {
			lines[i] = "cpu  " + strings.Join(fields[1:], " ")
		} else {
			lines[i] = strings.Join(fields, " ")
		}
		_ = os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644)
		return
	}
}

// r5MutateFixture 按 step 改一次夹具里的动态度量。
func r5MutateFixture(root string, step int) {
	proc := filepath.Join(root, "proc")
	// 网卡累计计数：每次 +20 KB 级（真机上它一直在涨，速率随之非零）。
	devPath := filepath.Join(proc, "net", "dev")
	if data, err := os.ReadFile(devPath); err == nil {
		lines := strings.Split(string(data), "\n")
		for i, line := range lines {
			if !strings.HasPrefix(strings.TrimSpace(line), "eth0:") {
				continue
			}
			f := strings.Fields(line)
			if len(f) < 10 {
				break
			}
			rx, _ := strconv.ParseUint(f[1], 10, 64)
			tx, _ := strconv.ParseUint(f[9], 10, 64)
			f[1] = strconv.FormatUint(rx+uint64(20000+step*137), 10)
			f[9] = strconv.FormatUint(tx+uint64(9000+step*71), 10)
			lines[i] = "  " + strings.Join(f, " ")
			break
		}
		_ = os.WriteFile(devPath, []byte(strings.Join(lines, "\n")), 0o644)
	}
	// CPU：user 与 idle 都往前走（使用率随之在 10%~30% 之间晃）。
	statPath := filepath.Join(proc, "stat")
	if data, err := os.ReadFile(statPath); err == nil {
		lines := strings.Split(string(data), "\n")
		if len(lines) > 0 && strings.HasPrefix(lines[0], "cpu ") {
			f := strings.Fields(lines[0])
			if len(f) >= 5 {
				user, _ := strconv.ParseUint(f[1], 10, 64)
				idle, _ := strconv.ParseUint(f[4], 10, 64)
				f[1] = strconv.FormatUint(user+uint64(1000+step*13), 10)
				f[4] = strconv.FormatUint(idle+uint64(3000+step*7), 10)
				lines[0] = "cpu  " + strings.Join(f[1:], " ")
			}
		}
		_ = os.WriteFile(statPath, []byte(strings.Join(lines, "\n")), 0o644)
	}
	// 内存：MemAvailable 在小幅变化（used() 优先用它），MemFree 跟着动。
	memPath := filepath.Join(proc, "meminfo")
	r5BumpLine(memPath, "MemFree:", 1, strconv.Itoa(1234500+step*7919))
	r5BumpLine(memPath, "MemAvailable:", 1, strconv.Itoa(3456000+step*104729))
	// 负载与运行时长。
	loadPath := filepath.Join(proc, "loadavg")
	if _, err := os.Stat(loadPath); err == nil {
		s := float64(step)
		_ = os.WriteFile(loadPath, []byte(fmt.Sprintf("%.2f %.2f %.2f 1/234 5678\n",
			0.52+s*0.013, 0.41+s*0.011, 0.38+s*0.007)), 0o644)
	}
	upPath := filepath.Join(proc, "uptime")
	if data, err := os.ReadFile(upPath); err == nil {
		f := strings.Fields(string(data))
		if len(f) > 0 {
			up, _ := strconv.ParseFloat(f[0], 64)
			_ = os.WriteFile(upPath, []byte(fmt.Sprintf("%.2f %.2f\n", up+0.2, up+1.5)), 0o644)
		}
	}
}

// r5Sample 采一帧；第一次调用只用来建立 CPU 基线（CPUPct 为 0）。
func r5Sample(t *testing.T, c *agent.Collector, at time.Time) protocol.Metrics {
	t.Helper()
	if _, _, err := c.Sample(at); err != nil {
		t.Fatalf("建立采样基线: %v", err)
	}
	m, _, err := c.Sample(at.Add(time.Second))
	if err != nil {
		t.Fatalf("采样: %v", err)
	}
	return m
}

// r5Seq 读节点当前 seq（没有状态时是 0）。
func r5Seq(s *Server, nodeID int64) uint64 {
	if st, ok := s.state.Get(nodeID); ok {
		return st.Seq
	}
	return 0
}

// r5WindowSample 是一次"帧边界取样"的结果（见 r5SampleAt）。
type r5WindowSample struct {
	seq     uint64 // 服务端已经收下的帧数（帧边界）
	wire    int64  // 这条连接在代理上量到的线上字节（Agent→服务端，含帧头与掩码）
	down    int64  // 服务端→Agent 的线上字节（welcome/config/pong）
	payload int64  // 这条连接自己发出去的**明文负载**字节（参照组才会填）
}

// r5SampleAt 在**同一个帧边界**上取一条连接的三个数：帧数（seq）、线上字节、明文负载。
//
// 为什么必须凑齐同批帧再算：
//
//   - "老 Agent 的线上每帧 = 明文负载 + 8 字节帧头/掩码"这条兼容性判据，只有在**同一批帧**
//     上量这两个数时才成立。原先负载用的是"开机以来"的累计平均 —— 头两帧还是采集器的
//     采样基线（cpu_pct / rx_rate 都是 0，比稳态帧小约 50 字节），它把平均值拉低了
//     3~4.5 字节，而判据的容差是 ±4 字节：机器一忙就越界 ⇒ 这条用例偶发红
//     （隔离跑 -count=10 红 2 次，两次都是这条断言）。
//   - 帧数（seq 差）也必须与两个字节窗口**同时刻**取：原先参照组的 seq 是在实测组窗口
//     结束后才读的，那几秒里参照组新发的帧会被算进分母、字节却不在分子里
//     （帧数虚高 ⇒ 压缩比腰斩）。窗口三要素同时刻取，这一条就不可能发生。
//
// 判据是"两次读到的 seq 相同"：seq 只在服务端收下一帧之后才前进，它一前进就说明又有
// 一帧的字节（可能）落进了计数器，于是立刻重取 —— 只重读计数器，不等任何时长。
// 每秒一帧，正常情况下第一次就成立。
func r5SampleAt(s *Server, nodeID int64, proxy *measureProxy, payload *atomic.Int64) r5WindowSample {
	sample := func() r5WindowSample {
		p := r5WindowSample{wire: proxy.toServer.Load(), down: proxy.toClient.Load()}
		if payload != nil {
			p.payload = payload.Load()
		}
		p.seq = r5Seq(s, nodeID)
		return p
	}
	for i := 0; i < 1000; i++ {
		before := r5Seq(s, nodeID)
		p := sample()
		if p.seq == before {
			return p
		}
	}
	return sample()
}

// r5WaitSeq 等到节点 seq 至少到 want。
//
// ⚠️ 帧边界对齐：代理上的字节计数**先于**服务端的状态更新，所以"看到 seq 到 N"
// 意味着这一帧的字节已经全部数进去了。按帧边界取窗口，才不会被"半帧"污染。
func r5WaitSeq(t *testing.T, s *Server, nodeID int64, want uint64, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if r5Seq(s, nodeID) >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待节点 %d 的 seq ≥ %d 超时（当前 %d）", nodeID, want, r5Seq(s, nodeID))
}

// r5DialPlain 连一条**默认配置**（= 不协商压缩）的 Agent WebSocket，
// 它就是"改动之前的线上形状"。
func r5DialPlain(t *testing.T, addr, token string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, "ws://"+addr+"/api/v1/agent/ws", &websocket.DialOptions{
		Subprotocols: []string{agentSubprotocol},
		HTTPHeader:   http.Header{"Authorization": []string{"Bearer " + token}},
	})
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

// ---------- ① Agent→服务端 ----------

// r5PlainReporter 每秒往一条**没有协商压缩**的连接发一帧 metrics，内容来自真采集器。
//
// 它扮演"改动之前的 Agent"：帧的构造方式与 agent/client.go 的 reportOnce 逐字一致
// （protocol.New(TypeMetrics, m) + seq + ts），只是不走 Client 的重连/探测逻辑。
type r5PlainReporter struct {
	frames  atomic.Uint64
	payload atomic.Int64
	stop    chan struct{}
	done    chan struct{}
}

func r5StartPlainReporter(t *testing.T, col *agent.Collector, conn *websocket.Conn) *r5PlainReporter {
	t.Helper()
	r := &r5PlainReporter{stop: make(chan struct{}), done: make(chan struct{})}
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
			m, _, err := col.Sample(time.Now())
			if err != nil {
				continue
			}
			env, err := protocol.New(protocol.TypeMetrics, m)
			if err != nil {
				continue
			}
			env.Seq = r.frames.Load() + 1
			env.TS = time.Now().Unix()
			data, err := env.Encode()
			if err != nil {
				continue
			}
			wctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			err = conn.Write(wctx, websocket.MessageText, data)
			cancel()
			if err != nil {
				return
			}
			r.frames.Add(1)
			r.payload.Add(int64(len(data)))
		}
	}()
	t.Cleanup(func() {
		close(r.stop)
		select {
		case <-r.done:
		case <-time.After(5 * time.Second):
		}
	})
	return r
}

// r5RealAgent 是一条"真 Agent（真 Client + 真 Collector）→ 真服务端"的连接，
// 连接经由一个数字节的 TCP 代理（measureProxy，见 round4_measure_stream_test.go）。
type r5RealAgent struct {
	proxy  *measureProxy
	nodeID int64
	done   chan struct{}
}

func r5StartRealAgent(t *testing.T, serverAddr, token, root string, nodeID int64) *r5RealAgent {
	t.Helper()
	proxy := newMeasureProxy(t, serverAddr)
	tr, warn, err := agent.LoadTraffic("")
	if err != nil {
		t.Fatalf("LoadTraffic: %v", err)
	}
	if warn != "" {
		t.Logf("LoadTraffic 提示: %q", warn)
	}
	col := agent.New(root, "eth0", "/", tr)
	client := agent.NewClient(agent.ClientConfig{
		ServerURL: "http://" + proxy.addr(),
		Token:     token,
	}, col, tr, slog.New(slog.DiscardHandler))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = client.Run(ctx)
	}()
	a := &r5RealAgent{proxy: proxy, nodeID: nodeID, done: done}
	t.Cleanup(func() {
		cancel()
		select {
		case <-a.done:
		case <-time.After(5 * time.Second):
		}
	})
	return a
}

// TestMeasureTrafficAgentUploadBytes 是 Agent→服务端 那一段的**同场 A/B**。
//
// 两条连接在同一台服务端、同一份夹具、同一时刻各跑一条：
//
//	参照组：默认 DialOptions（不协商压缩）＝ 改动之前的线上形状；
//	实测组：真 Agent Client（它现在协商 permessage-deflate）。
//
// 两边的内容都来自真 Collector，所以帧大小可比（用例会把两个帧大小打出来）。
// 窗口按**帧边界**取（r5WaitSeq），且帧数 / 线上字节 / 明文负载三者取自**同一个**
// 帧边界（r5SampleAt）—— 三个数不同批，比值就是在比两个不同的帧集。
//
// 判据：实测组每帧线上字节 ≤ 参照组 / 3（见 r5MinAgentCompressionRatio）。
// 把压缩改回去（或换成 NoContextTakeover）这条必红。
func TestMeasureTrafficAgentUploadBytes(t *testing.T) {
	const (
		windowFrames = 8
		maxWait      = 40 * time.Second
	)
	h := newAuthHarnessWithConfig(t, measureConfig(2, true))
	ctx := context.Background()
	// 两边读**同一份会动的夹具**：真机上的数值每一拍都在变，恒定帧量出来的压缩比
	// 是最好情况（见 r5MutatingRoot）。
	root := r5MutatingRoot(t)

	// ---- 参照组：不协商压缩 ----
	refNode, refToken, err := h.srv.db.CreateNode(ctx, store.NewNode{
		Name: "r5-plain", IntervalSec: 1, TrafficWarnPct: 80, ResetDay: 1,
	}, time.Now())
	if err != nil {
		t.Fatalf("建节点（参照组）: %v", err)
	}
	refProxy := newMeasureProxy(t, h.ts.Listener.Addr().String())
	refConn := r5DialPlain(t, refProxy.addr(), refToken)
	sendFrame(t, refConn, helloFrame(t, testHello()))
	readHandshake(t, refConn)
	refRep := r5StartPlainReporter(t, r5NewCollectorAt(t, root), refConn)

	// ---- 实测组：真 Agent ----
	cmpNode, cmpToken, err := h.srv.db.CreateNode(ctx, store.NewNode{
		Name: "r5-agent", IntervalSec: 1, TrafficWarnPct: 80, ResetDay: 1,
	}, time.Now())
	if err != nil {
		t.Fatalf("建节点（实测组）: %v", err)
	}
	cmp := r5StartRealAgent(t, h.ts.Listener.Addr().String(), cmpToken, root, cmpNode.ID)

	r5WaitSeq(t, h.srv, refNode.ID, 2, maxWait)
	r5WaitSeq(t, h.srv, cmp.nodeID, 2, maxWait)

	// ★ 窗口三要素（帧数 / 线上字节 / 明文负载）必须在**同一个帧边界**上取，
	// 否则量出来的比值是在比两个不同的帧集 —— 见 r5SampleAt 的注释。
	ref0 := r5SampleAt(h.srv, refNode.ID, refProxy, &refRep.payload)
	cmp0 := r5SampleAt(h.srv, cmp.nodeID, cmp.proxy, nil)
	// 两组各等自己的第 windowFrames 帧到齐，并在**各自到点的时刻**取计数：
	// 两条连接不可能同一毫秒到点，共用一个窗口会让"B/s"低估（多算了等对方的那点时间）。
	start := time.Now()
	r5WaitSeq(t, h.srv, refNode.ID, ref0.seq+windowFrames, maxWait)
	ref1 := r5SampleAt(h.srv, refNode.ID, refProxy, &refRep.payload)
	refSeconds := time.Since(start).Seconds()
	r5WaitSeq(t, h.srv, cmp.nodeID, cmp0.seq+windowFrames, maxWait)
	cmp1 := r5SampleAt(h.srv, cmp.nodeID, cmp.proxy, nil)
	cmpSeconds := time.Since(start).Seconds()

	refFrames := float64(ref1.seq - ref0.seq)
	cmpFrames := float64(cmp1.seq - cmp0.seq)
	if refFrames <= 0 || cmpFrames <= 0 {
		t.Fatalf("窗口内没有量到帧（参照 %.0f 帧、实测 %.0f 帧）：取样点没有落在帧边界上，这次测量无效",
			refFrames, cmpFrames)
	}
	refUp, cmpUp := float64(ref1.wire-ref0.wire), float64(cmp1.wire-cmp0.wire)
	refDown, cmpDown := float64(ref1.down-ref0.down), float64(cmp1.down-cmp0.down)
	// 明文负载同样取**这个窗口**（而不是"开机以来"的累计平均）：它与 refUp 是同一批帧。
	refPayloadUp := ref1.payload - ref0.payload

	// 线上每帧字节：含 WebSocket 帧头（1.4 KB 级的帧是 2+2 字节长度）与客户端掩码 4 字节。
	refPerFrame, cmpPerFrame := refUp/refFrames, cmpUp/cmpFrames
	ratio := refPerFrame / cmpPerFrame
	// 参照组窗口内的**明文负载**每帧：与 refPerFrame 是同一批帧，两者只差帧头/掩码。
	refPayloadPerFrame := float64(refPayloadUp) / refFrames

	// ⚠️ 帧率口径：夹具在 Windows 上是几十个小文件，`Sample()` 每拍要读它们，
	// 于是实测帧率会比 1.00 帧/秒低（本机约 0.80）。**每帧字节**是实测的硬数字，
	// 速率与月流量按生产的 1 帧/秒 折出去（真实 Linux 上 /proc 是内存文件系统，读它不花时间）。
	refRate, cmpRate := refFrames/refSeconds, cmpFrames/cmpSeconds
	t.Logf("窗口 %.2fs / %.2fs（两组各 %d 帧；实测帧率 %.2f / %.2f 帧/秒 —— "+
		"夹具是几十个小文件、机器忙时会低于 1.00，生产 Linux 上是 1.00）",
		refSeconds, cmpSeconds, windowFrames, refRate, cmpRate)
	t.Logf("① 参照组（不协商压缩，= 改动前）: 每帧 %.1f 字节线上（%.0f B/s @本次帧率）；"+
		"窗口内负载每帧 %.1f 字节（窗口 %d 帧 / %d 字节；这条连接累计 %d 帧 / %d 字节）",
		refPerFrame, refUp/refSeconds, refPayloadPerFrame,
		int(refFrames), refPayloadUp, refRep.frames.Load(), refRep.payload.Load())
	t.Logf("② 实测组（真 Agent，协商 permessage-deflate）: 每帧 %.1f 字节线上（%.0f B/s @本次帧率）",
		cmpPerFrame, cmpUp/cmpSeconds)
	t.Logf("   下行（服务端→Agent，welcome/config/pong）: 参照 %.0f B/s、实测 %.0f B/s",
		refDown/refSeconds, cmpDown/cmpSeconds)
	t.Logf("★ Agent→服务端 压缩比 = %.1f×（线上每帧 %.1f → %.1f 字节）",
		ratio, refPerFrame, cmpPerFrame)
	// 每台机器的月流量：按生产的 1 帧/秒 + 7×24 折算（每帧字节 × 86400 秒 × 30 天）。
	for _, v := range []struct {
		name string
		perS float64
	}{{"改动前", refPerFrame}, {"改动后", cmpPerFrame}} {
		t.Logf("   %s：按 1 帧/秒 ⇒ %.1f B/s ⇒ %.1f MB/天 ⇒ %.2f GB/月（30 天）",
			v.name, v.perS, v.perS*86400/1e6, v.perS*86400*30/1e9)
	}

	if ratio < r5MinAgentCompressionRatio {
		t.Errorf("Agent→服务端 每帧线上字节 明文 %.1f → 实发 %.1f（只压到 %.2f×，要求 ≥ %.1f×）："+
			"permessage-deflate 没有生效", refPerFrame, cmpPerFrame, ratio, r5MinAgentCompressionRatio)
	}

	// A/B 可比性：两边的**明文负载**必须几乎一样大，否则"压了多少倍"是在比两个不同的东西。
	// 实测组的负载用服务端内存里的那一帧原样重编码（Metrics + Seq + TS 都是 Agent 发来的值）。
	st, ok := h.srv.state.Get(cmp.nodeID)
	if !ok {
		t.Fatalf("实测组没有状态")
	}
	env, err := protocol.New(protocol.TypeMetrics, st.Metrics)
	if err != nil {
		t.Fatalf("重建实测组帧: %v", err)
	}
	env.Seq = st.Seq
	env.TS = 1800000000
	cmpPayload, err := env.Encode()
	if err != nil {
		t.Fatalf("编码实测组帧: %v", err)
	}
	diff := (refPayloadPerFrame - float64(len(cmpPayload))) / float64(len(cmpPayload)) * 100
	t.Logf("    可比性：参照组负载每帧 %.1f 字节 / 实测组 %d 字节（差 %+.1f%%）",
		refPayloadPerFrame, len(cmpPayload), diff)
	if diff > 10 || diff < -10 {
		t.Errorf("两组的明文帧大小差 %+.1f%%（>10%%），这个 A/B 不可比", diff)
	}

	// 兼容性（老 Agent + 新服务端）：参照组就是"不声明 permessage-deflate 的老客户端"，
	// 服务端**不能**给它压 —— 线上每帧应当正好是"明文负载 + WebSocket 帧头/掩码 8 字节"。
	// 这条也是"改动没有波及老客户端"的硬证据。
	refPlainPerFrame := refPayloadPerFrame + 8
	if math.Abs(refPerFrame-refPlainPerFrame) > 4 {
		t.Errorf("老 Agent（不协商压缩）的线上每帧 %.1f 字节，明文负载 %.1f + 帧头 8 = %.1f："+
			"服务端不该给不声明扩展的客户端压缩", refPerFrame, refPayloadPerFrame, refPlainPerFrame)
	}

	// 数据完好：压缩路径上服务端**存下来的**指标必须与发出来的逐字段一致。
	// 夹具每次 +20 KB 级，所以速率与累计值都必须是非零的真值 —— 压坏一个字节都会在这里现形。
	if st.Metrics.Net.RxTotal == 0 || st.Metrics.Net.RxRate <= 0 {
		t.Errorf("压缩路径上收到的流量字段不对：rx_total=%d rx_rate=%v（应为非零）",
			st.Metrics.Net.RxTotal, st.Metrics.Net.RxRate)
	}
	if st.Metrics.Net.Iface != "eth0" || st.Metrics.CPUCores != 2 || st.Metrics.Mem.Total == 0 {
		t.Errorf("压缩路径上收到的字段不对：iface=%q cpu_cores=%d mem_total=%d",
			st.Metrics.Net.Iface, st.Metrics.CPUCores, st.Metrics.Mem.Total)
	}
	t.Logf("    数据完好：rx_total=%d rx_rate=%.1f iface=%q cpu_cores=%d mem_total=%d",
		st.Metrics.Net.RxTotal, st.Metrics.Net.RxRate, st.Metrics.Net.Iface,
		st.Metrics.CPUCores, st.Metrics.Mem.Total)
}

// ---------- 兼容性：新 Agent ↔ 老服务端 ----------

// r5OldServer 是"改动之前的服务端"：Accept 时**不**协商压缩（CompressionDisabled），
// 也就是 v1.5.0 的行为。协议、welcome/config 的内容与真服务端一致。
//
// 为什么要单独起一个：真服务端的 AcceptOptions 已经在 agentconn.go 里改成了
// CompressionContextTakeover，没法在同一条用例里切成"老"的行为 —— 而
// "新 Agent 连老服务端"恰恰是升级过程中**最可能先发生**的组合（先升 Agent 再升服务端，
// 或者一部分机器升了、一部分没升）。它必须照常工作，且字节与改动前逐字一致。
type r5OldServer struct {
	ts      *httptest.Server
	hello   atomic.Bool
	metrics atomic.Uint64
	payload atomic.Int64
	last    atomic.Value // protocol.Metrics
}

func r5StartOldServer(t *testing.T) *r5OldServer {
	t.Helper()
	o := &r5OldServer{}
	o.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			Subprotocols:    []string{agentSubprotocol},
			CompressionMode: websocket.CompressionDisabled, // ← v1.5.0 的样子
		})
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		conn.SetReadLimit(protocol.MaxFrame)

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for {
			typ, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			if typ != websocket.MessageText {
				continue
			}
			env, err := protocol.Decode(data)
			if err != nil {
				return
			}
			switch env.T {
			case protocol.TypeHello:
				o.hello.Store(true)
				welcome, _ := protocol.New(protocol.TypeWelcome, protocol.Welcome{
					NodeID: 1, Name: "old-server", IntervalSec: 1,
					ServerTime: time.Now().Unix(), ObservedIP: "127.0.0.1", ConfigVersion: 1,
				})
				cfg, _ := protocol.New(protocol.TypeConfig, protocol.Config{ConfigVersion: 1, IntervalSec: 1})
				for _, f := range []protocol.Envelope{welcome, cfg} {
					if raw, err := f.Encode(); err == nil {
						_ = conn.Write(ctx, websocket.MessageText, raw)
					}
				}
			case protocol.TypeMetrics:
				var m protocol.Metrics
				if err := env.Bind(&m); err == nil {
					o.last.Store(m)
				}
				o.metrics.Add(1)
				// 收到的**负载**字节：老服务端没有压缩能力，所以这里应该等于明文帧大小。
				o.payload.Add(int64(len(data)))
			case protocol.TypePing:
				var p protocol.Ping
				if err := env.Bind(&p); err == nil {
					if pong, err := protocol.New(protocol.TypePong, protocol.Pong{TsUS: p.TsUS}); err == nil {
						if raw, err := pong.Encode(); err == nil {
							_ = conn.Write(ctx, websocket.MessageText, raw)
						}
					}
				}
			}
		}
	}))
	t.Cleanup(o.ts.Close)
	return o
}

// TestMeasureTrafficNewAgentOldServerFallback 验证"新 Agent（会协商压缩）+ 老服务端
// （不协商）"这条路：握手必须成功、上报必须照常、**字节不能变小**（退回不解压/不压缩）。
//
// 判据有齿：连接断了或数据坏了 → metrics 计数上不去；客户端"没有对面确认就硬压" →
// 老服务端解不出来 → 帧数上不去或者 payload/帧明显小于明文（<400 字节）。
func TestMeasureTrafficNewAgentOldServerFallback(t *testing.T) {
	old := r5StartOldServer(t)
	cmp := r5StartRealAgent(t, old.ts.Listener.Addr().String(), "pba_oldserver_dummy", r5AgentRoot, 0)

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) && old.metrics.Load() < 4 {
		time.Sleep(20 * time.Millisecond)
	}
	frames := old.metrics.Load()
	if frames < 4 {
		t.Fatalf("老服务端只收到 %d 帧 metrics（期望 ≥ 4）：新 Agent 连老服务端这条路断了", frames)
	}
	if !old.hello.Load() {
		t.Fatalf("老服务端没有收到 hello")
	}
	avg := float64(old.payload.Load()) / float64(frames)
	m, _ := old.last.Load().(protocol.Metrics)
	t.Logf("新 Agent → 老服务端：%d 帧、负载每帧 %.1f 字节；"+
		"最后一帧 cpu_cores=%d iface=%q rx_total=%d（数据完好）",
		frames, avg, m.CPUCores, m.Net.Iface, m.Net.RxTotal)
	if avg < 400 {
		t.Errorf("老服务端收到的负载每帧只有 %.1f 字节（明文帧约 520）："+
			"客户端在没有对面确认的情况下压了，老服务端会解不出来", avg)
	}
	if m.CPUCores == 0 || m.Net.Iface == "" {
		t.Errorf("最后一帧内容不完整：cpu_cores=%d iface=%q", m.CPUCores, m.Net.Iface)
	}
	// 代理上量到的上行也应该还是"明文 + 帧头"的量级（这里含握手请求的几百字节，故除以帧数偏高）。
	up := float64(cmp.proxy.toServer.Load()) / float64(frames)
	t.Logf("    这条连接 4 帧上行共 %d 字节（含握手请求）：%.1f B/帧",
		cmp.proxy.toServer.Load(), up)
}

// ---------- ② 服务端→浏览器（字段级） ----------

// r5FieldByte 是一个 JSON 字段在帧里占的字节数（含键名、两个引号、冒号与分隔逗号）。
type r5FieldByte struct {
	Key   string
	Bytes int
}

// r5AccountObject 算一个 JSON 对象里每个字段占多少字节。
//
// 口径：`"key":value,` = 键名 + 值的原文 + 4（两个引号 + 冒号 + 逗号）。
// 最后一个字段会多算一个逗号（一帧差一字节，对账够用）。
func r5AccountObject(t *testing.T, obj map[string]json.RawMessage) []r5FieldByte {
	t.Helper()
	out := make([]r5FieldByte, 0, len(obj))
	for k, v := range obj {
		out = append(out, r5FieldByte{Key: k, Bytes: len(k) + len(v) + 4})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Bytes != out[j].Bytes {
			return out[i].Bytes > out[j].Bytes
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// r5LogFields 打印字段账（按字节数从大到小）。
func r5LogFields(t *testing.T, title string, fields []r5FieldByte, total int) {
	t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, "%s：帧合计 %d 字节，%d 个键", title, total, len(fields))
	for _, f := range fields {
		fmt.Fprintf(&b, "\n    %-34s %7d 字节  %5.1f%%", f.Key, f.Bytes, float64(f.Bytes)*100/float64(total))
	}
	t.Log(b.String())
}

// r5CountWriter 只数写入的字节。
type r5CountWriter struct{ n int64 }

func (w *r5CountWriter) Write(p []byte) (int, error) {
	w.n += int64(len(p))
	return len(p), nil
}

// r5DeflateStream 模拟 permessage-deflate 的**发送侧**：
// 每帧 Write + Flush，并把末尾 4 字节（00 00 FF FF）从线上剪掉
// —— 这正是 coder/websocket 的 trimLastFourBytesWriter 干的事。
//
// contextTakeover 为真时所有帧共用同一个 flate.Writer（= 32 KB 滑动窗口跨帧保留），
// 就是 CompressionContextTakeover 模式；为假时每帧新建一个（= 每帧独立，
// 对应 CompressionNoContextTakeover 的 Reset）。等级 BestSpeed 与库里的
// getFlateWriter 一致 —— 用默认档会把这个场景压得更好，那是乐观估计。
func r5DeflateStream(frames [][]byte, contextTakeover bool) []int {
	var cw r5CountWriter
	var fw *flate.Writer
	sizes := make([]int, 0, len(frames))
	for _, f := range frames {
		var before int64
		if !contextTakeover || fw == nil {
			cw.n = 0
			if fw != nil {
				_ = fw.Close()
			}
			fw, _ = flate.NewWriter(&cw, flate.BestSpeed)
		}
		before = cw.n
		if _, err := fw.Write(f); err != nil {
			panic(err)
		}
		if err := fw.Flush(); err != nil {
			panic(err)
		}
		n := int(cw.n - before - 4)
		if n < 0 {
			n = 0
		}
		sizes = append(sizes, n)
	}
	if fw != nil {
		_ = fw.Close()
	}
	return sizes
}

// r5TypicalMetrics 造一帧"真机上长这样"的指标。
//
// 为什么不用夹具直接采：夹具的 /proc 是**静态快照**（网卡计数不动 ⇒ 速率为 0），
// 而且 root != "/" 时采集器会跳过磁盘统计。字段集合与类型都跟真机一致，
// 缺的只是"数值的量级"，而 JSON 的字节数恰恰由量级决定（13 位计数比 0 多 12 字节）。
// 这里把缺的量级补上，字段一个都不增删。
func r5TypicalMetrics(disks int, pings int) protocol.Metrics {
	m := protocol.Metrics{
		CPUPct:   12.47,
		CPUCores: 4,
		Mem:      protocol.Mem{Total: 17179869184, Used: 1730150400, Pct: 10.0712},
		Swap:     protocol.Mem{Total: 2147483648, Used: 0, Pct: 0},
		Load:     protocol.Load{L1: 0.52, L5: 0.41, L15: 0.38},
		Net: protocol.Net{
			Iface:    "eth0",
			RxTotal:  1234567890123,
			TxTotal:  987654321098,
			RxRaw:    4821345678901,
			TxRaw:    1234567890123,
			RxRate:   28014.5,
			TxRate:   17223.25,
			BootID:   "9f2c1a4e-0b3d-4c5a-8e7f-1234567890ab",
			CkptAgeS: 3,
		},
		LatMS:     0.42,
		UptimeSec: 3802345,
	}
	mounts := []string{"/", "/var/lib/docker", "/www", "/data", "/home", "/opt", "/srv", "/mnt/backup"}
	for i := 0; i < disks && i < len(mounts); i++ {
		m.Disk = append(m.Disk, protocol.Disk{
			Mount: mounts[i],
			FS:    []string{"ext4", "xfs", "btrfs"}[i%3],
			Total: 107374182400 + uint64(i)*10737418240,
			Used:  41875931136 + uint64(i)*2147483648,
			Pct:   39.0 + float64(i)*1.7,
		})
	}
	for i := 0; i < pings; i++ {
		m.Pings = append(m.Pings, protocol.PingResult{
			TargetID: int64(i + 1),
			AvgMS:    23.4 + float64(i)*7.25,
			MinMS:    18.1 + float64(i)*7.1,
			MaxMS:    44.9 + float64(i)*7.4,
			LossPct:  float64(i) * 0.5,
		})
	}
	return m
}

// r5FrameSeq 按 seq 让每个数值都动起来，模拟稳态上报（真实部署里 1 拍/秒，
// CPU/内存/流量/延迟每一拍都不一样）——gzip 的压缩比极度依赖"这一帧与上一帧像不像"，
// 用恒定帧量出来的压缩比是最好情况，不是常态。
func r5FrameSeq(seq uint64, disks, pings int) ([][]byte, error) {
	out := make([][]byte, 0, seq)
	for i := uint64(1); i <= seq; i++ {
		m := r5TypicalMetrics(disks, pings)
		s := float64(i)
		m.CPUPct = 12.47 + float64(i%17)*1.73
		m.Mem.Used += uint64(i%997) * 7919
		m.Mem.Pct = 10.07 + float64(i%23)*0.37
		m.Load = protocol.Load{L1: 0.52 + s*0.013, L5: 0.41 + s*0.011, L15: 0.38 + s*0.007}
		m.Net.RxTotal += i * 1048573
		m.Net.TxTotal += i * 524287
		m.Net.RxRate = 28014.5 + s*7.3
		m.Net.TxRate = 17223.25 + s*3.1
		m.LatMS = 0.42 + float64(i%41)*0.37
		m.UptimeSec += i
		m.Disk[0].Used += i * 104729
		env, err := protocol.New(protocol.TypeMetrics, m)
		if err != nil {
			return nil, err
		}
		env.Seq = i
		env.TS = 1800000000 + int64(i)
		data, err := env.Encode()
		if err != nil {
			return nil, err
		}
		out = append(out, data)
	}
	return out, nil
}

// TestMeasureTrafficAgentFrameFieldAccounting 是 Agent→服务端 那一帧的**字段级字节账**
// 与压缩潜力预估（不需要连服务端，纯编解码）。
//
// 两种形态都量：
//
//	典型：1 个挂载点、没有探测目标（多数部署就是这样 —— 探测目标是可选配置）；
//	重载：8 个挂载点、3 个探测目标（一帧的上界方向）。
func TestMeasureTrafficAgentFrameFieldAccounting(t *testing.T) {
	for _, tc := range []struct {
		name  string
		disks int
		pings int
	}{
		{"典型（1 磁盘 / 无探测目标）", 1, 0},
		{"重载（8 磁盘 / 3 探测目标）", 8, 3},
	} {
		m := r5TypicalMetrics(tc.disks, tc.pings)
		env, err := protocol.New(protocol.TypeMetrics, m)
		if err != nil {
			t.Fatalf("构造帧: %v", err)
		}
		env.Seq = 12345
		env.TS = 1800000000
		data, err := env.Encode()
		if err != nil {
			t.Fatalf("编码帧: %v", err)
		}

		var obj map[string]json.RawMessage
		if err := json.Unmarshal(data, &obj); err != nil {
			t.Fatalf("解析帧: %v", err)
		}
		fields := r5AccountObject(t, obj)
		r5LogFields(t, "[Agent→服务端] "+tc.name, fields, len(data))

		// 信封里的三个字段（v/t/seq/ts）不在 d 里，单列出来。
		t.Logf("    信封（v/t/seq/ts/d 的包装）: %d 字节", len(data)-len(obj["d"]))

		// d 里面才是"每拍都在发的真东西"，继续往下拆两层（mem/swap/load/net/disk/pings）。
		var inner map[string]json.RawMessage
		if err := json.Unmarshal(obj["d"], &inner); err != nil {
			t.Fatalf("解析 d: %v", err)
		}
		r5LogFields(t, "    └ d 里的字段（= 每拍真正发的内容）", r5AccountObject(t, inner), len(obj["d"]))
		for _, sub := range []string{"mem", "swap", "load", "net"} {
			raw, ok := inner[sub]
			if !ok {
				continue
			}
			var m map[string]json.RawMessage
			if err := json.Unmarshal(raw, &m); err != nil {
				continue
			}
			r5LogFields(t, "        └ "+sub, r5AccountObject(t, m), len(raw))
		}
		if raw, ok := inner["disk"]; ok {
			var arr []map[string]json.RawMessage
			if err := json.Unmarshal(raw, &arr); err == nil && len(arr) > 0 {
				r5LogFields(t, fmt.Sprintf("        └ disk[0]（共 %d 项，每项同形）", len(arr)),
					r5AccountObject(t, arr[0]), len(raw)/len(arr))
			}
		}
		if raw, ok := inner["pings"]; ok {
			var arr []map[string]json.RawMessage
			if err := json.Unmarshal(raw, &arr); err == nil && len(arr) > 0 {
				r5LogFields(t, fmt.Sprintf("        └ pings[0]（共 %d 项，每项同形）", len(arr)),
					r5AccountObject(t, arr[0]), len(raw)/len(arr))
			}
		}

		// 压缩潜力：6 帧、每帧数值都在变。
		frames, err := r5FrameSeq(6, tc.disks, tc.pings)
		if err != nil {
			t.Fatalf("造连续帧: %v", err)
		}
		plain := 0
		for _, f := range frames {
			plain += len(f)
		}
		perMsg := r5DeflateStream(frames, false)
		takeover := r5DeflateStream(frames, true)
		var sumPer, sumTake int
		for i := range frames {
			sumPer += perMsg[i]
			sumTake += takeover[i]
		}
		t.Logf("    6 帧明文共 %d 字节（每帧 %.1f）", plain, float64(plain)/6)
		t.Logf("    deflate 每帧独立（NoContextTakeover）: 稳态每帧 %.1f 字节 ⇒ %.1f×（总体 %d → %d）",
			float64(sumPer)/6, float64(plain)/float64(sumPer), plain, sumPer)
		t.Logf("    deflate 跨帧保留窗口（ContextTakeover）: 第 1 帧 %d、稳态每帧 %.1f 字节 ⇒ %.1f×（总体 %d → %d）",
			takeover[0], float64(sumTake)/6, float64(plain)/float64(sumTake), plain, sumTake)
	}
}

// TestMeasureTrafficStreamFrameFieldAccounting 是服务端→浏览器 那一帧的字段级字节账。
//
// 用 currentNodes + encodePayload 组帧：这正是 handleStream 的**两条**路径用的同一组函数
// （建连快照见 snapshotPayload，每秒推送见 realtimeLoop），所以字节数与线上一致。
// 20 个节点、真 SQLite、真状态、真流量汇总（落一笔真机量级的 traffic_daily），
// 只差 HTTP/SSE 那层壳（壳的字节数由 round4 的 TestMeasureStreamOutboundBytes 量）。
func TestMeasureTrafficStreamFrameFieldAccounting(t *testing.T) {
	const nodeCount = 20
	h := newAuthHarnessWithConfig(t, measureConfig(nodeCount, true))
	ctx := context.Background()
	now := time.Now()

	// 真机量级的流量：这台机器本周期已经用了 137 GB↓ / 42 GB↑。
	updates := make([]store.TrafficUpdate, 0, nodeCount)
	ids := make([]int64, 0, nodeCount)
	for i := 0; i < nodeCount; i++ {
		node, _, err := h.srv.db.CreateNode(ctx, store.NewNode{
			Name:           fmt.Sprintf("hk-%02d.example.com", i+1),
			GroupName:      "香港",
			Region:         "HK",
			Note:           "月付 3.99 美元 / 500 GB 流量",
			IntervalSec:    1,
			TrafficLimit:   500 << 30,
			TrafficWarnPct: 80,
			ResetDay:       1,
			ExpiresAt:      now.AddDate(0, 0, 28).Unix(),
			PriceCents:     399,
			Currency:       "USD",
			BillingMonths:  12,
			Tags:           []string{"vps", "hk", "prod"},
		}, now)
		if err != nil {
			t.Fatalf("建节点 %d: %v", i+1, err)
		}
		ids = append(ids, node.ID)

		m := r5TypicalMetrics(1, 0)
		m.Net.RxTotal += uint64(i) * 12345678901
		m.Net.TxTotal += uint64(i) * 1234567890
		m.Net.RxRate = 28014.5 + float64(i)*1000
		m.Net.TxRate = 17223.25 + float64(i)*500
		m.Net.Iface = "eth0"
		m.CPUPct = 12.47 + float64(i)
		m.Mem.Used += uint64(i) * 1048576
		m.UptimeSec += uint64(i) * 3600
		h.srv.state.Attach(node.ID, uint64(i+1), protocol.Info{
			AgentVersion: "1.5.0",
			Hostname:     fmt.Sprintf("hk-%02d", i+1),
			OS:           protocol.OSInfo{Name: "Debian GNU/Linux 12 (bookworm)", Kernel: "6.1.0-13-amd64", Arch: "amd64"},
			CPU:          protocol.CPUInfo{Model: "Intel(R) Xeon(R) Platinum 8255C CPU @ 2.50GHz", Cores: 4},
			BootID:       "9f2c1a4e-0b3d-4c5a-8e7f-1234567890ab",
			Iface:        protocol.IfaceInfo{Name: "eth0", IfIndex: 2, MAC: "52:54:00:aa:bb:cc"},
		}, "203.0.113.7", fmt.Sprintf("10.0.0.%d", i+2), fmt.Sprintf("2001:db8::%x", i+2), now)
		h.srv.state.Update(node.ID, uint64(i+1), m, 0, now)

		updates = append(updates, store.TrafficUpdate{
			NodeID:  node.ID,
			Day:     store.FormatDay(now),
			RxDelta: int64(137e9) + int64(i)*int64(1e9),
			TxDelta: int64(42e9) + int64(i)*int64(1e8),
			RxTotal: 1234567890123 + uint64(i)*1234567,
			TxTotal: 987654321098 + uint64(i)*7654321,
		})
	}
	if err := h.srv.db.FlushTraffic(ctx, updates, now); err != nil {
		t.Fatalf("落盘流量: %v", err)
	}

	nodes, err := h.srv.currentNodes(ctx)
	if err != nil {
		t.Fatalf("读节点视图: %v", err)
	}
	if len(nodes) != nodeCount {
		t.Fatalf("节点数 = %d，期望 %d", len(nodes), nodeCount)
	}
	// 自己包一层与线上同形的信封，**不调** encodePayload：那个函数的签名正在被别人改
	// （SSE 那一帧要多带一个 totals），而这里要量的只是"每个节点多少字节、花在哪些键上"。
	// 信封本身只有一百来字节，且与 totals 无关。
	payload, err := json.Marshal(struct {
		Type    string       `json:"type"`
		TS      int64        `json:"ts"`
		Summary stateSummary `json:"summary"`
		Nodes   []nodeDTO    `json:"nodes"`
	}{Type: "nodes", TS: now.Unix(), Summary: summarize(nodes), Nodes: nodes})
	if err != nil {
		t.Fatalf("编码负载: %v", err)
	}

	// 字段账：把所有节点的同名字段累加，得到"这一帧的字节花在哪些键上"。
	sum := map[string]int{}
	var raw struct {
		Nodes []map[string]json.RawMessage `json:"nodes"`
	}
	if err := json.Unmarshal(payload, &raw); err != nil {
		t.Fatalf("解析负载: %v", err)
	}
	for _, n := range raw.Nodes {
		for k, v := range n {
			sum[k] += len(k) + len(v) + 4
		}
	}
	fields := make([]r5FieldByte, 0, len(sum))
	body := 0
	for k, v := range sum {
		fields = append(fields, r5FieldByte{Key: k, Bytes: v})
		body += v
	}
	// 每个节点对象最后一个字段的那个逗号是多算的（没有后继字段），减掉。
	body -= len(raw.Nodes)
	sort.Slice(fields, func(i, j int) bool {
		if fields[i].Bytes != fields[j].Bytes {
			return fields[i].Bytes > fields[j].Bytes
		}
		return fields[i].Key < fields[j].Key
	})

	shell := len(payload) - body
	t.Logf("20 个节点、全量快照负载 %d 字节（每个节点 %.1f 字节）；"+
		"信封（type/ts/summary + nodes 的括号与逗号）= %d 字节", len(payload),
		float64(body)/nodeCount, shell)
	r5LogFields(t, "[服务端→浏览器] 20 个节点的字段合计（每帧）", fields, body)

	// 按"这一帧里会不会变"分三档：每拍都变的、慢变的、基本不变的。
	perTick := map[string]bool{
		"status": true, "connected": true, "last_seen": true, "online_sec": true,
		"cpu_pct": true, "mem_pct": true, "swap_pct": true, "disk_pct": true,
		"load1": true, "load5": true, "load15": true, "mem_used": true, "mem_total": true,
		"lat_ms": true, "uptime_sec": true, "rx_rate": true, "tx_rate": true,
		"rx_total": true, "tx_total": true, "gap": true, "dropped": true,
		"disks": true, "traffic_today_rx": true, "traffic_today_tx": true,
		"traffic_today_total": true, "traffic_today_pct": true,
		"traffic_week_rx": true, "traffic_week_tx": true, "traffic_week_total": true,
		"traffic_week_pct": true, "traffic_cycle_rx": true, "traffic_cycle_tx": true,
		"traffic_cycle_total": true, "traffic_cycle_used": true,
		"traffic_total_rx": true, "traffic_total_tx": true,
		"traffic_pct": true,
	}
	var tick, cfgBytes int
	for _, f := range fields {
		if perTick[f.Key] {
			tick += f.Bytes
		} else {
			cfgBytes += f.Bytes
		}
	}
	t.Logf("    每拍都可能变的字段 %d 字节（%.1f%%）；配置/身份字段 %d 字节（%.1f%%）",
		tick, float64(tick)*100/float64(body), cfgBytes, float64(cfgBytes)*100/float64(body))
}
