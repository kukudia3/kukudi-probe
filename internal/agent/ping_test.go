package agent

import (
	"context"
	"encoding/binary"
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
