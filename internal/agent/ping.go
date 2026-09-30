package agent

import (
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/netip"
	"os"
	"strconv"
	"sync"
	"time"

	"probe/internal/protocol"
)

// 探测的默认参数。
const (
	// probeTimeout 是单次探测的总超时（3 秒）。
	probeTimeout = 3 * time.Second
	// icmpPackets 是 ICMP 每次探测发出的包数：3 个包才能算出有意义的 min/avg/max，
	// 丢 1 个也还能看出"抖动"而不是"全丢"。
	icmpPackets = 3
	// icmpGap 是 ICMP 包之间的间隔（~200ms）。
	icmpGap = 200 * time.Millisecond
	// maxPingWarnKeys 限制"只记一次"告警表的规模（键里含主机名等变量）。
	maxPingWarnKeys = 64
	// icmpPayloadLen 是 echo 请求的载荷长度（32 字节，和系统 ping 的默认值一致）。
	icmpPayloadLen = 32
)

// ICMP 报文类型（RFC 792 / RFC 4443）。
const (
	icmpEchoRequest  = 8
	icmpEchoReply    = 0
	icmpv6EchoReply  = 129
	icmpv6NextHeader = 58
)

// probeOutcome 是一轮探测的结果。
type probeOutcome struct {
	// rtts 是成功的往返耗时（毫秒）。
	rtts []float64
	// sent 是这一轮实际尝试发出的探测包数（用来算丢包率）。
	sent int
	// skip 表示"这一轮根本没探成"（例如 ICMP 拿不到原始套接字权限）。
	//
	// 为什么要把这种情况单独标出来：它必须让目标**留空**，而不是记成 100% 丢包。
	// 把"探针自己没权限"画成"网络全丢"，用户会去查网络、换目标，怎么都查不出来。
	skip bool
}

// Prober 按配置的间隔探测各个目标，并把最近一次结果存在内存里，
// 供每秒的 metrics 帧读取。
//
// 生命周期跟着会话走（见 Client.session）：没有服务端下发的目标时，它什么都不做 ——
// 断线期间空转没有意义，重连后握手时的 config 帧会把目标重新给到。
type Prober struct {
	log *slog.Logger

	// wake 是"配置变了，重新读一遍"的信号（容量 1：连点保存只唤醒一次）。
	wake chan struct{}

	mu       sync.Mutex
	targets  []protocol.PingTarget
	interval time.Duration
	results  map[int64]protocol.PingResult

	// warnMu 单独一把锁：warnOnce 会在持锁路径外面调用，混用同一把锁迟早死锁。
	warnMu sync.Mutex
	warned map[string]bool

	// 下面三个是探测参数，测试里会调小。
	timeout time.Duration
	packets int
	gap     time.Duration
}

// proberConfig 是一次配置快照（Run 判断"要不要重启 worker"用）。
type proberConfig struct {
	targets  []protocol.PingTarget
	interval time.Duration
}

func (c proberConfig) same(other proberConfig) bool {
	if c.interval != other.interval || len(c.targets) != len(other.targets) {
		return false
	}
	for i := range c.targets {
		if c.targets[i] != other.targets[i] {
			return false
		}
	}
	return true
}

// NewProber 构造探测器。
func NewProber(logger *slog.Logger) *Prober {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Prober{
		log:      logger,
		wake:     make(chan struct{}, 1),
		interval: protocol.DefaultPingIntervalSec * time.Second,
		results:  make(map[int64]protocol.PingResult),
		warned:   make(map[string]bool),
		timeout:  probeTimeout,
		packets:  icmpPackets,
		gap:      icmpGap,
	}
}

// Update 应用服务端下发的探测目标与间隔。
//
// 这个函数**不做任何探测**，也不会阻塞：它只是换掉配置并唤醒调度 goroutine
// （见 Run）—— 调用它的是收帧的主循环，绝不能在这里等一次 3 秒的超时。
func (p *Prober) Update(targets []protocol.PingTarget, intervalSec int) {
	interval := protocol.DefaultPingIntervalSec * time.Second
	switch {
	case intervalSec >= protocol.MinPingIntervalSec && intervalSec <= protocol.MaxPingIntervalSec:
		interval = time.Duration(intervalSec) * time.Second
	case intervalSec != 0:
		p.warnOnce("interval", "服务端下发的探测间隔不合法，已按默认值处理",
			"interval_sec", intervalSec, "default_sec", protocol.DefaultPingIntervalSec)
	}
	p.update(targets, interval)
}

// update 是 Update 的内部实现（测试可直接传缩短后的间隔）。
func (p *Prober) update(targets []protocol.PingTarget, interval time.Duration) {
	clean := make([]protocol.PingTarget, 0, len(targets))
	for _, t := range targets {
		if err := checkWireTarget(t); err != nil {
			p.warnOnce(fmt.Sprintf("target/%d", t.ID), "忽略不合法的探测目标", "err", err)
			continue
		}
		clean = append(clean, t)
		if len(clean) >= protocol.MaxPingTargets {
			break
		}
	}

	p.mu.Lock()
	p.targets = clean
	if interval > 0 {
		p.interval = interval
	}
	// 已经不在配置里的目标，连结果一起丢掉：留着它只会上报一份没人认领的旧数据。
	keep := make(map[int64]bool, len(clean))
	for _, t := range clean {
		keep[t.ID] = true
	}
	for id := range p.results {
		if !keep[id] {
			delete(p.results, id)
		}
	}
	p.mu.Unlock()

	select {
	case p.wake <- struct{}{}:
	default: // 已经有一次待处理的唤醒，合并掉
	}
}

// checkWireTarget 校验一个下发的目标（配置来自网络，必须自己再挡一次）。
func checkWireTarget(t protocol.PingTarget) error {
	if t.ID <= 0 {
		return fmt.Errorf("目标 ID %d 不合法", t.ID)
	}
	if !protocol.IsPingType(t.Type) {
		return fmt.Errorf("探测方式 %q 不支持", t.Type)
	}
	if t.Host == "" {
		return fmt.Errorf("目标 %d 的主机为空", t.ID)
	}
	if len(t.Host) > protocol.MaxPingHostLen {
		return fmt.Errorf("目标 %d 的主机名过长", t.ID)
	}
	if t.Type == protocol.PingTypeTCP && (t.Port < 1 || t.Port > protocol.MaxPingPort) {
		return fmt.Errorf("目标 %d 的 TCP 端口 %d 不合法", t.ID, t.Port)
	}
	return nil
}

// Results 返回各目标最近一次的结果（没有结果的目标不出现在数组里）。
//
// 顺序按配置顺序固定：metrics 帧的内容因此是可预期的，测试与排查都省事。
func (p *Prober) Results() []protocol.PingResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.results) == 0 {
		return nil
	}
	out := make([]protocol.PingResult, 0, len(p.targets))
	for _, t := range p.targets {
		if r, ok := p.results[t.ID]; ok {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Run 是探测调度器：配置变一次，就重建一轮 worker。
//
// 每个目标一个 goroutine 独立计时，所以某个目标超时（最长 3 秒）不会拖住别的目标，
// 也不会碰到收帧/上报的主循环。目标数量有 16 个的上限，goroutine 数因此是常数级。
func (p *Prober) Run(ctx context.Context) {
	var (
		applied proberConfig
		started bool
		cancel  context.CancelFunc
		wg      sync.WaitGroup
	)
	stop := func() {
		if cancel != nil {
			cancel()
			wg.Wait() // 等在飞的探测结束，避免配置切换时两批探测叠在一起
			cancel = nil
		}
	}
	defer stop()

	for {
		cfg := p.snapshot()
		if !started || !applied.same(cfg) {
			stop()
			applied, started = cfg, true
			if len(cfg.targets) > 0 {
				cctx, c := context.WithCancel(ctx)
				cancel = c
				for _, target := range cfg.targets {
					wg.Add(1)
					go func(t protocol.PingTarget) {
						defer wg.Done()
						p.worker(cctx, t, cfg.interval)
					}(target)
				}
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-p.wake:
		}
	}
}

func (p *Prober) snapshot() proberConfig {
	p.mu.Lock()
	defer p.mu.Unlock()
	targets := make([]protocol.PingTarget, len(p.targets))
	copy(targets, p.targets)
	return proberConfig{targets: targets, interval: p.interval}
}

// worker 持续探测一个目标。
func (p *Prober) worker(ctx context.Context, target protocol.PingTarget, interval time.Duration) {
	// 立刻探一次：用户刚配好目标就想看到数据，等一整个间隔（默认 60 秒）太傻。
	p.probeAndStore(ctx, target)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.probeAndStore(ctx, target)
		}
	}
}

// probeAndStore 探一轮并记下结果。
//
// 探测失败只影响这一个目标的数据：它绝不能让 Agent 掉线，也不该影响别的目标。
func (p *Prober) probeAndStore(ctx context.Context, target protocol.PingTarget) {
	out := p.probe(ctx, target)
	if out.skip {
		return
	}
	sent := out.sent
	if sent <= 0 {
		sent = 1
	}
	res := protocol.PingResult{TargetID: target.ID, LossPct: 100}
	if len(out.rtts) > 0 {
		sum := 0.0
		minRTT, maxRTT := out.rtts[0], out.rtts[0]
		for _, v := range out.rtts {
			sum += v
			minRTT = math.Min(minRTT, v)
			maxRTT = math.Max(maxRTT, v)
		}
		res.AvgMS = sum / float64(len(out.rtts))
		res.MinMS = minRTT
		res.MaxMS = maxRTT
		res.LossPct = float64(sent-len(out.rtts)) / float64(sent) * 100
	}
	// 全丢时 avg/min/max 保持 0、loss=100：不能拿上一次的成功耗时充数，
	// 否则目标断网后图上还是一条漂亮的直线，只有丢包率在说话。
	p.mu.Lock()
	p.results[target.ID] = res
	p.mu.Unlock()
}

// probe 按类型分派。
func (p *Prober) probe(ctx context.Context, target protocol.PingTarget) probeOutcome {
	switch target.Type {
	case protocol.PingTypeTCP:
		return p.probeTCP(ctx, target)
	case protocol.PingTypeICMP:
		return p.probeICMP(ctx, target)
	default:
		return probeOutcome{skip: true}
	}
}

// probeTCP 用一次 TCP 连接握手量耗时。
//
// 用 Dialer.DialContext 而不是 net.DialTimeout：语义一样（Timeout 就是总超时），
// 但 ctx 一取消就能立刻放弃 —— 配置变更/退出时不必等满 3 秒。
func (p *Prober) probeTCP(ctx context.Context, target protocol.PingTarget) probeOutcome {
	// 连不上就是这一轮丢包（loss=100），不是"没探成"：端口不通/被防火墙拒绝
	// 正是用户配这个目标想看到的信息。
	out := probeOutcome{sent: 1}
	dialer := net.Dialer{Timeout: p.timeout}
	start := time.Now()
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(target.Host, strconv.Itoa(target.Port)))
	if err != nil {
		// 被取消（配置变更、连接断开、进程退出）不是"探测失败"：
		// 记成 100% 丢包会在图上留下一条假的"网络全丢"。
		if ctx.Err() != nil {
			return probeOutcome{skip: true}
		}
		return out
	}
	rtt := float64(time.Since(start).Nanoseconds()) / 1e6
	_ = conn.Close()
	out.rtts = []float64{rtt}
	return out
}

// probeICMP 用原始套接字自己组 echo 请求，解析回包算 RTT。
//
// 需要 CAP_NET_RAW（Linux，见 deploy/install-agent.sh）或管理员权限（Windows）：
// 拿不到权限时 Dial 就会失败，这里返回 skip，由调用方把该目标留空。
// IPv4 与 IPv6 都走同一段代码，只是网络类型不同（ip4:icmp / ip6:ipv6-icmp）。
func (p *Prober) probeICMP(ctx context.Context, target protocol.PingTarget) probeOutcome {
	// 先解析：host 既可以是 IPv4/IPv6 字面量，也可以是域名；由解析结果决定走
	// ip4 还是 ip6 —— 纯 IPv6 的机器只有后者可用。
	sent := p.packets
	if sent < 1 {
		sent = 1
	}
	// 解析也受探测超时约束：DNS 服务器不响应时，系统解析器可能等十几秒，
	// 那会让这个目标的 worker（以及配置切换时的等待）一起卡住。
	rctx, cancel := context.WithTimeout(ctx, p.timeout)
	addr, err := resolveOne(rctx, target.Host)
	cancel()
	if err != nil {
		// 解析失败是"目标不可达"，与 TCP 侧的 dial 失败一致：记 100% 丢包。
		// （它和下面的"没权限"是两回事：权限问题不该污染目标的曲线。）
		p.warnOnce(fmt.Sprintf("icmp-resolve/%s", target.Host),
			"ICMP 探测目标无法解析", "host", target.Host, "err", err)
		return probeOutcome{sent: sent}
	}

	conn, isV6, err := dialICMP(ctx, addr, p.timeout)
	if err != nil {
		if ctx.Err() != nil {
			return probeOutcome{skip: true} // 被取消（配置变更/退出），不是探测失败
		}
		p.warnOnce(fmt.Sprintf("icmp/%s", target.Host),
			"ICMP 探测不可用（需要 CAP_NET_RAW 或管理员权限），该目标将留空；"+
				"不想给权限就把它改成 TCP 探测",
			"host", target.Host, "err", err)
		return probeOutcome{skip: true}
	}
	defer func() { _ = conn.Close() }()

	perPacket := p.timeout / time.Duration(sent)
	if perPacket <= 0 {
		perPacket = p.timeout
	}

	out := probeOutcome{sent: sent}
	// ID 字段：Linux 的原始套接字会用我们写的值；Windows 会改写成内核自己的值。
	// 因此回包匹配只看 type 与 seq（见 readEchoReply 的注释）。
	id := uint16(os.Getpid())
	payload := make([]byte, icmpPayloadLen)
	copy(payload, "probe-agent-ping")

	for i := 0; i < sent; i++ {
		if ctx.Err() != nil {
			if i == 0 {
				// 一个包都没发出去（配置刚变、连接刚断）：这不是"全丢"，
				// 而是"这轮没探"，结果必须留空。
				return probeOutcome{skip: true}
			}
			out.sent = i
			break
		}
		seq := uint16(i + 1)
		msg := buildEcho(id, seq, isV6, payload, conn)
		start := time.Now()
		if _, err := conn.Write(msg); err != nil {
			// 写失败按丢包计（不中止整轮）：内核在收到 ICMP 错误后短时间内
			// 可能让写失败，剩下的包还有机会成功。
			continue
		}
		if rtt, ok := readEchoReply(conn, seq, start.Add(perPacket), isV6, start); ok {
			out.rtts = append(out.rtts, rtt)
		}
		if i < sent-1 {
			select {
			case <-ctx.Done():
				return out
			case <-time.After(p.gap):
			}
		}
	}
	return out
}

// dialICMP 在已解析出的地址上建立原始套接字连接，返回连接与"是不是 IPv6"。
func dialICMP(ctx context.Context, addr netip.Addr, timeout time.Duration) (net.Conn, bool, error) {
	network := "ip4:icmp"
	isV6 := !addr.Is4()
	if isV6 {
		network = "ip6:ipv6-icmp"
	}
	dialer := net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, network, addr.String())
	if err != nil {
		return nil, isV6, fmt.Errorf("建立 %s 套接字失败: %w", network, err)
	}
	return conn, isV6, nil
}

// resolveOne 把主机解析成一个地址；IPv4 优先（与绝大多数机器上的默认路由一致）。
func resolveOne(ctx context.Context, host string) (netip.Addr, error) {
	if addr, err := netip.ParseAddr(host); err == nil {
		return addr.Unmap(), nil
	}
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("解析 %q 失败: %w", host, err)
	}
	var fallback netip.Addr
	for _, a := range addrs {
		a = a.Unmap()
		if a.Is4() {
			return a, nil
		}
		if !fallback.IsValid() {
			fallback = a
		}
	}
	if !fallback.IsValid() {
		return netip.Addr{}, fmt.Errorf("域名 %q 没有解析结果", host)
	}
	return fallback, nil
}

// buildEcho 组装一个 echo 请求（8 字节 ICMP 头 + 载荷）。
//
// 校验和：IPv4 必须由我们算（Linux 不会替原始套接字填）；IPv6 的校验和要带伪头，
// 而伪头需要源地址 —— 连接建立后内核已经选好了本地地址，从 conn.LocalAddr() 拿，
// 这样算出来的值与内核一致（内核若自己重算，结果相同）。
func buildEcho(id, seq uint16, isV6 bool, payload []byte, conn net.Conn) []byte {
	msg := make([]byte, 8+len(payload))
	if isV6 {
		msg[0] = 128 // ICMPv6 echo request
	} else {
		msg[0] = icmpEchoRequest
	}
	binary.BigEndian.PutUint16(msg[4:6], id)
	binary.BigEndian.PutUint16(msg[6:8], seq)
	copy(msg[8:], payload)

	if isV6 {
		binary.BigEndian.PutUint16(msg[2:4], checksum6(msg, localIP(conn), remoteIP(conn)))
		return msg
	}
	binary.BigEndian.PutUint16(msg[2:4], checksum(msg))
	return msg
}

func localIP(conn net.Conn) net.IP {
	if addr, ok := conn.LocalAddr().(*net.IPAddr); ok {
		return addr.IP
	}
	return nil
}

func remoteIP(conn net.Conn) net.IP {
	if addr, ok := conn.RemoteAddr().(*net.IPAddr); ok {
		return addr.IP
	}
	return nil
}

// readEchoReply 等到本轮 seq 的回包，返回距发送时刻的毫秒数。
//
// 匹配只看 type 与 seq，不看 ID：Windows 的原始套接字会把 ID 换成内核自己的值，
// 而 Linux 的原始套接字在 connect 之后只收对端的包，seq 足以区分本轮探测。
// 收到的其它 ICMP（目的不可达、超时…）直接跳过。
func readEchoReply(conn net.Conn, seq uint16, deadline time.Time, isV6 bool, start time.Time) (float64, bool) {
	buf := make([]byte, 1500)
	for {
		if err := conn.SetReadDeadline(deadline); err != nil {
			return 0, false
		}
		n, err := conn.Read(buf)
		if err != nil {
			return 0, false
		}
		got, ok := parseEchoReply(buf[:n], isV6)
		if !ok || got != seq {
			continue
		}
		return float64(time.Since(start).Nanoseconds()) / 1e6, true
	}
}

// parseEchoReply 从收到的字节里定位 echo 回包并返回它的 seq。
//
// 原始套接字交上来的内容与平台有关：IPv4 通常带 IP 头（Linux/Windows），
// 而 ICMPv6 在 Linux 上不带 IPv6 头。所以这里按首字节的 IP 版本号判断
// "有没有 IP 头"，而不是按平台猜。
func parseEchoReply(data []byte, isV6 bool) (uint16, bool) {
	if len(data) < 8 {
		return 0, false
	}
	switch data[0] >> 4 {
	case 4:
		ihl := int(data[0]&0x0f) * 4
		if ihl < 20 || len(data) < ihl+8 {
			return 0, false
		}
		data = data[ihl:]
	case 6:
		if len(data) < 40+8 {
			return 0, false
		}
		data = data[40:]
	}
	want := uint8(icmpEchoReply)
	if isV6 {
		want = icmpv6EchoReply
	}
	if data[0] != want {
		return 0, false
	}
	return binary.BigEndian.Uint16(data[6:8]), true
}

// checksum 是 RFC 1071 的 16 位反码和（ICMPv4 的校验和就是它）。
func checksum(b []byte) uint16 {
	sum := sum16(nil, b)
	return ^uint16(sum)
}

// checksum6 计算带 IPv6 伪头的校验和（RFC 4443 要求）。
func checksum6(msg []byte, src, dst net.IP) uint16 {
	var pseudo []byte
	if s := src.To16(); s != nil {
		pseudo = append(pseudo, s...)
	}
	if d := dst.To16(); d != nil {
		pseudo = append(pseudo, d...)
	}
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(msg)))
	pseudo = append(pseudo, length[:]...)
	pseudo = append(pseudo, 0, 0, 0, icmpv6NextHeader)

	sum := sum16(pseudo, msg)
	return ^uint16(sum)
}

// sum16 累加两段字节的 16 位和（末尾奇数字节按高位补齐）。
func sum16(parts ...[]byte) uint32 {
	var sum uint32
	for _, b := range parts {
		for i := 0; i+1 < len(b); i += 2 {
			sum += uint32(b[i])<<8 | uint32(b[i+1])
		}
		if len(b)%2 == 1 {
			sum += uint32(b[len(b)-1]) << 8
		}
	}
	for sum>>16 != 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return sum
}

// warnOnce 对同一类问题只记一条警告。
//
// ICMP 没权限是"每次探测都会发生"的事：按默认 60 秒一次、16 个目标算，
// 不做去重的话日志会被同一条消息刷满，真正有用的那几条反而看不见。
func (p *Prober) warnOnce(key, msg string, args ...any) {
	p.warnMu.Lock()
	if p.warned == nil {
		p.warned = make(map[string]bool)
	}
	if p.warned[key] {
		p.warnMu.Unlock()
		return
	}
	if len(p.warned) >= maxPingWarnKeys {
		// 键里带主机名（用户可配），规模必须封顶；满了就从头再来一轮。
		p.warned = make(map[string]bool)
	}
	p.warned[key] = true
	p.warnMu.Unlock()

	p.log.Warn(msg, args...)
}
