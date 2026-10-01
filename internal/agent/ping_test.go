package agent

import (
	"context"
	"encoding/binary"
	"errors"
	"log/slog"
	"net"
	"strconv"
	"testing"
	"time"

	"probe/internal/protocol"
)

// listenLocal 起一个只监听回环的 TCP 服务，返回端口与关闭函数。
func listenLocal(t *testing.T) (int, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听本地端口: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	// 有人连上来就立刻接受并关掉：探测只看 connect 的耗时。
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	return port, func() { _ = ln.Close() }
}

func newTestProber() *Prober {
	p := NewProber(slog.New(slog.DiscardHandler))
	p.timeout = time.Second
	p.gap = 10 * time.Millisecond
	return p
}

func TestProbeTCPMeasuresLocalListener(t *testing.T) {
	port, closer := listenLocal(t)
	defer closer()

	p := newTestProber()
	target := protocol.PingTarget{ID: 1, Type: protocol.PingTypeTCP, Host: "127.0.0.1", Port: port}
	p.update([]protocol.PingTarget{target}, time.Minute)

	out := p.probe(context.Background(), target)
	if out.skip {
		t.Fatal("TCP 探测不该被跳过")
	}
	if out.sent != 1 || len(out.rtts) != 1 {
		t.Fatalf("TCP 一轮应当只有 1 个包且成功: %+v", out)
	}
	// 回环连接的耗时应当很小，但绝不能是负数或超过超时。
	// 允许恰好 0：Windows 上的单调时钟分辨率可能粗到量不出一次回环 connect。
	if rtt := out.rtts[0]; rtt < 0 || rtt > 1000 {
		t.Fatalf("回环 RTT = %v ms，不合理", rtt)
	}

	// 经 probeAndStore 之后应当有一条 loss=0 的结果。
	p.probeAndStore(context.Background(), target)
	results := p.Results()
	if len(results) != 1 || results[0].TargetID != 1 || results[0].LossPct != 0 {
		t.Fatalf("结果不对: %+v", results)
	}
	if results[0].MinMS > results[0].AvgMS || results[0].AvgMS > results[0].MaxMS {
		t.Fatalf("min/avg/max 关系不对: %+v", results[0])
	}
}

// 连不通的端口＝丢包 100%，而不是 panic、也不是"跳过"。
func TestProbeTCPUnreachableCountsAsFullLoss(t *testing.T) {
	port, closer := listenLocal(t)
	closer() // 关掉监听：这个端口上已经没有人了
	// 关掉之后端口可能被别的进程抢走，但刚释放的端口立刻被占用的概率极低；
	// 真被占用了这条用例只会"更宽松地通过"（探测成功），不会误报失败。
	unreachable := protocol.PingTarget{ID: 7, Type: protocol.PingTypeTCP, Host: "127.0.0.1", Port: port}
	// 解析不了的主机名（RFC 6761 保留的 .invalid 顶级域永远不会有解析结果）：
	// 用它来验证"连不通 → loss=100"这条路径，而不是拿一个"应该不通"的测试网段
	// —— 本机可能挂着代理，什么 IP 都连得上。
	blackhole := protocol.PingTarget{ID: 8, Type: protocol.PingTypeTCP, Host: "no-such-host.invalid", Port: 9}

	p := newTestProber()
	p.timeout = 300 * time.Millisecond // 不可达地址要等超时，别让用例跑太久
	p.update([]protocol.PingTarget{unreachable, blackhole}, time.Minute)

	p.probeAndStore(context.Background(), unreachable)
	results := p.Results()
	if len(results) != 1 {
		t.Fatalf("连不通也应当留下结果（loss=100），实际 %+v", results)
	}
	got := results[0]
	if got.LossPct != 0 && got.LossPct != 100 {
		t.Fatalf("丢包率 = %v，期望 0（端口被占用）或 100（连不通）", got.LossPct)
	}
	if got.LossPct == 100 && (got.AvgMS != 0 || got.MinMS != 0 || got.MaxMS != 0) {
		t.Fatalf("全丢时不该有耗时: %+v", got)
	}

	// 落不到 IP 的地址也必须只是丢包。
	p.probeAndStore(context.Background(), blackhole)
	all := p.Results()
	if len(all) != 2 || all[1].TargetID != 8 || all[1].LossPct != 100 {
		t.Fatalf("不可达地址应当记 100%% 丢包: %+v", all)
	}
}

// ICMP 没有权限时（Linux 没有 CAP_NET_RAW、Windows 非管理员）只能返回错误，
// 绝不能 panic；有权限时则应当真的探到回环。
//
// 两种结果都接受：这条用例要守住的是"降级路径不能炸"，而不是"这台机器一定有权限"。
func TestProbeICMPNeverPanics(t *testing.T) {
	p := newTestProber()
	p.timeout = 500 * time.Millisecond
	target := protocol.PingTarget{ID: 3, Type: protocol.PingTypeICMP, Host: "127.0.0.1"}

	out := p.probe(context.Background(), target)
	if out.skip {
		// 降级路径：目标必须留空（不能记成 100% 丢包，否则图上是一条假曲线）。
		p.probeAndStore(context.Background(), target)
		if got := p.Results(); len(got) != 0 {
			t.Fatalf("没有权限时该目标必须留空，实际 %+v", got)
		}
		t.Logf("本机没有原始套接字权限，走降级路径（预期内）")
		return
	}
	if out.sent != p.packets {
		t.Fatalf("ICMP 一轮应当发 %d 个包，实际 %d", p.packets, out.sent)
	}
	for _, rtt := range out.rtts {
		if rtt < 0 || rtt > 5000 {
			t.Fatalf("回环 ICMP RTT = %v ms，不合理", rtt)
		}
	}
}

// 解析不了的主机名：与 TCP 一侧的 dial 失败一致，记 100% 丢包（而不是留空），
// 并且解析必须受探测超时约束（不能让 DNS 拖住这个目标的 worker）。
func TestProbeICMPUnresolvableHostCountsAsLoss(t *testing.T) {
	p := newTestProber()
	p.timeout = 500 * time.Millisecond
	target := protocol.PingTarget{ID: 4, Type: protocol.PingTypeICMP, Host: "no-such-host.invalid"}
	p.update([]protocol.PingTarget{target}, time.Minute)

	start := time.Now()
	out := p.probe(context.Background(), target)
	elapsed := time.Since(start)
	if out.skip {
		t.Fatalf("解析失败属于「目标不可达」，应当记丢包而不是留空: %+v", out)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("解析耗时 %s，没有受探测超时约束", elapsed)
	}
	p.probeAndStore(context.Background(), target)
	results := p.Results()
	if len(results) != 1 || results[0].LossPct != 100 {
		t.Fatalf("不可解析的目标应当记 100%% 丢包: %+v", results)
	}
}

func TestProberAppliesConfigAndDropsRemovedTargets(t *testing.T) {
	port, closer := listenLocal(t)
	defer closer()

	p := newTestProber()
	// 缩短到 50ms：正常间隔最小是 10 秒，测试等不起。
	p.update([]protocol.PingTarget{
		{ID: 1, Type: protocol.PingTypeTCP, Host: "127.0.0.1", Port: port},
		{ID: 2, Type: "udp", Host: "1.1.1.1"},                // 非法类型：忽略
		{ID: 3, Type: protocol.PingTypeTCP, Host: "1.1.1.1"}, // tcp 缺端口：忽略
	}, 50*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	waitForResults(t, p, 2*time.Second, func(results []protocol.PingResult) bool {
		return len(results) == 1 && results[0].TargetID == 1
	})

	// 目标被移除后，它的旧结果必须一起消失（不能继续上报没人认领的数据）。
	p.update(nil, 50*time.Millisecond)
	waitForResults(t, p, 2*time.Second, func(results []protocol.PingResult) bool {
		return len(results) == 0
	})

	// 配置没变时不该重启 worker（否则每次重连都会重新探一轮）；
	// 这里只验证重复调用不会把结果弄丢。
	p.update(nil, 50*time.Millisecond)
	p.update(nil, 50*time.Millisecond)
	if got := p.Results(); len(got) != 0 {
		t.Fatalf("没有目标时不该有结果: %+v", got)
	}
}

func waitForResults(t *testing.T, p *Prober, timeout time.Duration, cond func([]protocol.PingResult) bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond(p.Results()) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待探测结果超时，当前 %+v", p.Results())
}

// 校验和必须算对：把算好的校验和填回去之后，整段的 16 位反码和应当为 0。
// 这条在 Windows 上也能跑，正好守住"不能只在 Linux 上验证"的那部分逻辑。
func TestEchoChecksumIsSelfConsistent(t *testing.T) {
	msg := make([]byte, 8+icmpPayloadLen)
	msg[0] = icmpEchoRequest
	binary.BigEndian.PutUint16(msg[4:6], 0x1234)
	binary.BigEndian.PutUint16(msg[6:8], 1)
	copy(msg[8:], "probe-agent-ping")
	binary.BigEndian.PutUint16(msg[2:4], checksum(msg))
	if got := checksum(msg); got != 0 {
		t.Fatalf("填入校验和后整段的校验和 = %#x，期望 0", got)
	}

	// IPv6：带伪头的校验和同样要自洽（这里伪头只有长度与 next header，
	// 填入校验和后整段的校验和应当为 0）。
	msg6 := make([]byte, 8+icmpPayloadLen)
	msg6[0] = 128
	binary.BigEndian.PutUint16(msg6[6:8], 2)
	binary.BigEndian.PutUint16(msg6[2:4], checksum6(msg6, nil, nil))
	if got := checksum6(msg6, nil, nil); got != 0 {
		t.Fatalf("IPv6 校验和自洽性检查失败: %#x", got)
	}
}

// 回包解析要同时认"带 IP 头"与"不带 IP 头"两种形态：原始套接字在不同平台上
// 交上来的内容不一样，猜平台一定会错。
func TestParseEchoReplyHandlesBothShapes(t *testing.T) {
	echo := func(typ uint8, seq uint16) []byte {
		msg := make([]byte, 8+icmpPayloadLen)
		msg[0] = typ
		binary.BigEndian.PutUint16(msg[4:6], 0x4321)
		binary.BigEndian.PutUint16(msg[6:8], seq)
		return msg
	}

	// 1) 不带 IP 头（ICMPv6 在 Linux 上就是这种）。
	if seq, ok := parseEchoReply(echo(icmpv6EchoReply, 5), true); !ok || seq != 5 {
		t.Fatalf("不带 IP 头的 v6 回包没解析出来: seq=%d ok=%v", seq, ok)
	}
	// 2) 带 IPv4 头（Linux/Windows 的 ip4:icmp 是这种）。
	v4 := make([]byte, 20)
	v4[0] = 0x45
	v4[9] = 1 // protocol = ICMP
	v4 = append(v4, echo(icmpEchoReply, 7)...)
	if seq, ok := parseEchoReply(v4, false); !ok || seq != 7 {
		t.Fatalf("带 IPv4 头的回包没解析出来: seq=%d ok=%v", seq, ok)
	}
	// 3) 带 IPv6 头（40 字节）。
	v6 := make([]byte, 40)
	v6[0] = 0x60
	v6 = append(v6, echo(icmpv6EchoReply, 9)...)
	if seq, ok := parseEchoReply(v6, true); !ok || seq != 9 {
		t.Fatalf("带 IPv6 头的回包没解析出来: seq=%d ok=%v", seq, ok)
	}
	// 4) 请求包、其它类型、截断的数据都要被拒。
	if _, ok := parseEchoReply(echo(icmpEchoRequest, 1), false); ok {
		t.Fatal("echo 请求不该被当成回包")
	}
	if _, ok := parseEchoReply([]byte{0x45, 0x00}, false); ok {
		t.Fatal("截断的数据不该被解析出 seq")
	}
	if _, ok := parseEchoReply(nil, false); ok {
		t.Fatal("空数据不该被解析出 seq")
	}
	// 5) IPv4 头与 IPv6 头的回包对不上时也不能误判（v4 头 + v6 期望）。
	if _, ok := parseEchoReply(v4, true); ok {
		t.Fatal("IPv4 的 echo 回包不该被当成 ICMPv6 的回包")
	}
}

// 探测目标从 config 帧来到 metrics 帧里去：整条链路在 Agent 侧跑通。
func TestClientReportsPingsFromConfigPush(t *testing.T) {
	port, closer := listenLocal(t)
	defer closer()

	stub := newStubServer(t, "pba_stub")
	stub.pushConfig = &protocol.Config{
		ConfigVersion: 1,
		IntervalSec:   1,
		PingTargets: []protocol.PingTarget{
			{ID: 42, Type: protocol.PingTypeTCP, Host: "127.0.0.1", Port: port},
		},
		PingIntervalSec: protocol.MinPingIntervalSec, // 10 秒：这条用例只等第一次立即探测
	}
	client := newTestClient(t, stub.url(), "pba_stub", false)
	client.pings.timeout = time.Second

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	go func() { _ = client.Run(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if m, ok := stub.lastMetrics(); ok && len(m.Pings) > 0 {
			got := m.Pings[0]
			if got.TargetID != 42 {
				t.Fatalf("上报的目标 ID = %d，期望 42", got.TargetID)
			}
			if got.LossPct != 0 || got.AvgMS < 0 || got.MaxMS < got.MinMS {
				t.Fatalf("上报的探测结果不对: %+v", got)
			}
			// 探测结果必须通过协议校验（真实 Agent 发的帧也要过服务端那一关）。
			if err := protocol.ValidateMetrics(m); err != nil {
				t.Fatalf("上报的 metrics 帧不合法: %v", err)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	m, _ := stub.lastMetrics()
	t.Fatalf("等待 metrics 里出现 ping 结果超时，最后一帧: %+v", m.Pings)
}

// 端口用字符串拼出来的路径也要对（IPv6 字面量需要 JoinHostPort 加方括号）。
func TestProbeTCPIPv6Literal(t *testing.T) {
	ln, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skipf("本机不支持 IPv6 回环: %v", err)
	}
	defer func() { _ = ln.Close() }()
	port := ln.Addr().(*net.TCPAddr).Port

	// 有些环境（容器、被加固的 Windows 沙箱）能监听 ::1 却禁止连接它，
	// 那种情况下这条用例没有意义，直接跳过而不是误报失败。
	probe, err := net.DialTimeout("tcp", net.JoinHostPort("::1", strconv.Itoa(port)), time.Second)
	if err != nil {
		t.Skipf("本机不允许连接 IPv6 回环: %v", err)
	}
	_ = probe.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	p := newTestProber()
	out := p.probe(context.Background(), protocol.PingTarget{
		ID: 1, Type: protocol.PingTypeTCP, Host: "::1", Port: port,
	})
	if len(out.rtts) != 1 {
		t.Fatalf("IPv6 字面量探测失败: %+v（端口 %d）", out, port)
	}
}

// fakeStep 是脚本里的一步 TCP 测量：返回 ms / err。
type fakeStep struct {
	ms     float64
	err    error
	cancel bool // 在这一步"测量"期间取消 ctx（模拟配置变更/进程退出打断测量）
}

// fakeTCPMeasure 是按脚本返回测量结果的假测量函数。
//
// 为什么用脚本而不是"起个慢握手的监听"：TCP 握手是内核在 accept 之前就完成的，
// 测试里 sleep 一下再 Accept 根本拖不慢客户端的 connect；而这条规则的每条分支
// 都只取决于"第几次测量返回多少毫秒"，脚本能精确打到每一条上。
type fakeTCPMeasure struct {
	steps  []fakeStep
	cancel context.CancelFunc
	calls  int
}

func (f *fakeTCPMeasure) measure(_ context.Context, _ protocol.PingTarget) (float64, error) {
	if len(f.steps) == 0 {
		return 0, errors.New("测试脚本是空的")
	}
	// 脚本用完之后从头再来一遍：同一条用例里 probeAndStore 会再探一轮，
	// 循环播放才能让那一轮复现同一个场景（否则它就变成"只测了一次最后一步"）。
	i := f.calls % len(f.steps)
	f.calls++
	s := f.steps[i]
	if s.cancel && f.cancel != nil {
		f.cancel()
	}
	return s.ms, s.err
}

// newScriptedProber 造一个把 TCP 握手测量换成脚本的 Prober。
func newScriptedProber(steps ...fakeStep) (*Prober, *fakeTCPMeasure) {
	f := &fakeTCPMeasure{steps: steps}
	p := newTestProber()
	// 必须在启动 worker 之前替换：worker 会并发读这个字段。
	p.measureTCP = f.measure
	return p, f
}

// tcpTarget 造一个"只走判定逻辑"的 TCP 目标（测量已经被换成脚本，不会被真的连）。
func tcpTarget() protocol.PingTarget {
	return protocol.PingTarget{ID: 1, Type: protocol.PingTypeTCP, Host: "127.0.0.1", Port: 9}
}

// 「高延迟重试判丢包」（语义照抄 Komari）：每条分支都用脚本化的测量序列精确构造。
//
// 覆盖：不重试 / 边界（恰好 1000ms、落差恰好 800ms）/ 重试回落成功 /
// 重传判据丢包 / 重试报错丢包 / 三次重试仍高延迟丢包 / 首次就报错丢包，
// 以及每一条分支的落库语义（loss 与 avg/min/max）。
func TestProbeTCPHighLatencyRetry(t *testing.T) {
	refused := errors.New("connect: connection refused")

	cases := []struct {
		name      string
		steps     []fakeStep
		wantCalls int
		wantLoss  bool
		wantRTT   float64
	}{
		{
			name:      "首次就很快：成功，不重试",
			steps:     []fakeStep{{ms: 12}},
			wantCalls: 1,
			wantRTT:   12,
		},
		{
			name:      "恰好 1000ms：不触发重试（严格大于）",
			steps:     []fakeStep{{ms: 1000}},
			wantCalls: 1,
			wantRTT:   1000,
		},
		{
			name:      "1000.5ms：刚过分界，重试回落到 900ms（落差小）→ 成功",
			steps:     []fakeStep{{ms: 1000.5}, {ms: 900}},
			wantCalls: 2,
			wantRTT:   900,
		},
		{
			name:      "首次 1500ms、重试 900ms（落差 600）→ 成功，用重试的值",
			steps:     []fakeStep{{ms: 1500}, {ms: 900}},
			wantCalls: 2,
			wantRTT:   900,
		},
		{
			name:      "落差恰好 800ms：不算重传（严格大于）",
			steps:     []fakeStep{{ms: 1800}, {ms: 1000}},
			wantCalls: 2,
			wantRTT:   1000,
		},
		{
			name:      "2203ms 回落到 900ms（落差 1303）→ 判 SYN 重传，丢包",
			steps:     []fakeStep{{ms: 2203}, {ms: 900}},
			wantCalls: 2,
			wantLoss:  true,
		},
		{
			name:      "落差 900ms：同样判重传，丢包",
			steps:     []fakeStep{{ms: 1900}, {ms: 1000}},
			wantCalls: 2,
			wantLoss:  true,
		},
		{
			name:      "三次重试都仍高延迟：丢包",
			steps:     []fakeStep{{ms: 1200}, {ms: 1500}},
			wantCalls: 4,
			wantLoss:  true,
		},
		{
			name:      "某次重试报错：丢包",
			steps:     []fakeStep{{ms: 1200}, {err: refused}},
			wantCalls: 2,
			wantLoss:  true,
		},
		{
			name:      "首次就报错：丢包（既有行为）",
			steps:     []fakeStep{{err: refused}},
			wantCalls: 1,
			wantLoss:  true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, fake := newScriptedProber(tc.steps...)
			target := tcpTarget()
			p.update([]protocol.PingTarget{target}, time.Minute)

			out := p.probe(context.Background(), target)
			if out.skip {
				t.Fatalf("不该是 skip（skip 只留给取消）: %+v", out)
			}
			if fake.calls != tc.wantCalls {
				t.Fatalf("测量被调用 %d 次，期望 %d 次", fake.calls, tc.wantCalls)
			}
			if tc.wantLoss {
				if len(out.rtts) != 0 || out.sent != 1 {
					t.Fatalf("判为丢包时不该有 rtt: %+v", out)
				}
			} else if len(out.rtts) != 1 || out.rtts[0] != tc.wantRTT {
				t.Fatalf("采用的延迟 = %v，期望 [%v]（只放最终采用的那一次）", out.rtts, tc.wantRTT)
			}

			// 落库语义：成功 → loss=0 且就是那一次的值；丢包 → loss=100 且
			// avg/min/max 保持 0。
			p.probeAndStore(context.Background(), target)
			got := p.Results()
			if len(got) != 1 {
				t.Fatalf("应当有一条结果: %+v", got)
			}
			if tc.wantLoss {
				if got[0].LossPct != 100 || got[0].AvgMS != 0 || got[0].MinMS != 0 || got[0].MaxMS != 0 {
					t.Fatalf("判为丢包的结果必须是 loss=100 且 avg/min/max=0: %+v", got[0])
				}
			} else if got[0].LossPct != 0 || got[0].AvgMS != tc.wantRTT ||
				got[0].MinMS != tc.wantRTT || got[0].MaxMS != tc.wantRTT {
				t.Fatalf("成功的结果应当只有那一次的值 %v: %+v", tc.wantRTT, got[0])
			}
		})
	}
}

// 判为丢包之后绝不能留着上一次成功的耗时：否则目标断网后图上还是一条漂亮的直线，
// 只有丢包率在说话。这是既有约定，高延迟重试新增的两条丢包分支同样要遵守。
func TestProbeTCPRetryDropDoesNotReusePreviousRTT(t *testing.T) {
	p, fake := newScriptedProber(fakeStep{ms: 42}, fakeStep{ms: 2203}, fakeStep{ms: 900})
	target := tcpTarget()
	p.update([]protocol.PingTarget{target}, time.Minute)

	p.probeAndStore(context.Background(), target)
	first := p.Results()
	if len(first) != 1 || first[0].LossPct != 0 || first[0].AvgMS != 42 {
		t.Fatalf("第一次（42ms，成功）的结果不对: %+v", first)
	}

	// 第二次：2203ms → 重试 900ms，落差 1303 > 800 → 判 SYN 重传 → 丢包。
	p.probeAndStore(context.Background(), target)
	got := p.Results()
	if len(got) != 1 {
		t.Fatalf("应当有一条结果: %+v", got)
	}
	if got[0].LossPct != 100 || got[0].AvgMS != 0 || got[0].MinMS != 0 || got[0].MaxMS != 0 {
		t.Fatalf("判为丢包后不该留着上一次的耗时: %+v", got[0])
	}
	if fake.calls != 3 {
		t.Fatalf("两次探测共测量 %d 次，期望 3 次（1 次直接成功 + 1 次重试）", fake.calls)
	}
}

// ctx 被取消（配置变更、连接断开、进程退出）必须返回 skip，不能记成丢包 ——
// 否则会在图上留下一条假的"网络全丢"。高延迟重试把这条路径拉长到最长 12 秒，
// 所以"重试途中被取消"的每一条子路径都要单独守住。
func TestProbeTCPRetryCancelledReturnsSkip(t *testing.T) {
	cases := []struct {
		name      string
		preCancel bool
		steps     []fakeStep
		wantCalls int
	}{
		{
			name:      "开始前就取消：第一次测量返回的错误就是取消",
			preCancel: true,
			steps:     []fakeStep{{err: context.Canceled}},
			wantCalls: 1,
		},
		{
			name:      "重试途中取消：这次测量报错",
			steps:     []fakeStep{{ms: 2203}, {cancel: true, err: context.Canceled}},
			wantCalls: 2,
		},
		{
			name:      "重试途中取消：这次测量成功了、且低得像是重传",
			steps:     []fakeStep{{ms: 2203}, {cancel: true, ms: 900}},
			wantCalls: 2,
		},
		{
			name:      "重试途中取消：这次测量仍然高延迟（不该把剩下的重试跑完）",
			steps:     []fakeStep{{ms: 2210}, {cancel: true, ms: 1500}},
			wantCalls: 2,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, fake := newScriptedProber(tc.steps...)
			target := tcpTarget()
			p.update([]protocol.PingTarget{target}, time.Minute)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			fake.cancel = cancel
			if tc.preCancel {
				cancel()
			}

			out := p.probe(ctx, target)
			if !out.skip {
				t.Fatalf("被取消的一轮必须是 skip，而不是丢包: %+v", out)
			}
			if fake.calls != tc.wantCalls {
				t.Fatalf("测量被调用 %d 次，期望 %d 次", fake.calls, tc.wantCalls)
			}

			// 落库路径：被取消的目标必须留空，不能记成 100% 丢包。
			p.probeAndStore(ctx, target)
			if got := p.Results(); len(got) != 0 {
				t.Fatalf("被取消的目标必须留空，实际 %+v", got)
			}
		})
	}
}

// 真实套接字：确认 probeTCP 走的确实是可注入的那个测量函数，
// 而且正常线路（回环，远低于 1000ms）只测一次、不触发重试。
func TestProbeTCPRealDialMeasuredOnce(t *testing.T) {
	port, closer := listenLocal(t)
	defer closer()

	p := newTestProber()
	real := p.measureTCP
	calls := 0
	p.measureTCP = func(ctx context.Context, target protocol.PingTarget) (float64, error) {
		calls++
		return real(ctx, target)
	}
	target := protocol.PingTarget{ID: 1, Type: protocol.PingTypeTCP, Host: "127.0.0.1", Port: port}
	p.update([]protocol.PingTarget{target}, time.Minute)

	out := p.probe(context.Background(), target)
	if out.skip || len(out.rtts) != 1 {
		t.Fatalf("回环探测应当成功: %+v", out)
	}
	if calls != 1 {
		t.Fatalf("回环（远低于 %dms）只该测一次，实际 %d 次", slowProbeThresholdMS, calls)
	}
}

// ICMP 不参与高延迟重试：判定逻辑只落在 TCP 一侧（见 probeICMP 上面的注释）。
//
// 同时把那段注释的前提钉住：默认参数下 ICMP 的单包超时恰好是 1 秒 ——
// 谁要是把 icmpPackets 调小或 probeTimeout 调大，"单个包超过 1000ms 就已经算丢"
// 这个说法就不再成立，注释也就骗人了。
func TestProbeICMPIgnoresHighLatencyRetry(t *testing.T) {
	if got := probeTimeout / icmpPackets; got != time.Second {
		t.Fatalf("ICMP 单包超时 = %s，注释里「超过 1000ms 就算丢」的前提不再成立", got)
	}

	p, fake := newScriptedProber(fakeStep{ms: 5000})
	p.timeout = 300 * time.Millisecond
	target := protocol.PingTarget{ID: 6, Type: protocol.PingTypeICMP, Host: "127.0.0.1"}

	// 有没有原始套接字权限都行（见 TestProbeICMPNeverPanics）：
	// 这条用例守的是"ICMP 不去碰 TCP 的测量函数"。
	_ = p.probe(context.Background(), target)
	if fake.calls != 0 {
		t.Fatalf("ICMP 探测不该调用 TCP 的测量函数（被调用 %d 次）", fake.calls)
	}
}
