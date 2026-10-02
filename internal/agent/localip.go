package agent

import (
	"context"
	"net"
	"net/url"
	"strings"
	"time"
)

// localIPProbePort 是 UDP connect 用的目标端口。
//
// 端口号本身**没有意义**：我们不发包，只借 connect 让内核做一次路由查找，
// 查出来的源地址就是"这台机器去 serverHost 时会用哪个地址"。用 9（discard）
// 只是图个语义清楚，换成任何端口结果都一样。
const localIPProbePort = "9"

// localIPProbeTimeout 是两次地址族探测**共享**的等待上限。
//
// 为什么必须有这个上限：LocalIPs 夹在「WebSocket 升级完成」与「写 hello」之间，
// 而服务端从 Accept 成功就开始计 protocol.HelloTimeout（10 秒）。net.Dial 不带
// deadline 时用的是 Go 解析器的默认值（每个 nameserver 5s × 2 次尝试），坏 DNS 下
// 光解析就可能超过 10 秒 —— 那样 hello 永远发不出去，Agent 被反复踢下线、永久重连。
//
// 超时的代价只是 local_ip/local_ip6 为空串，而协议明确允许（见 protocol.Hello 的
// LocalIP 注释：两个字段都是可选的、取不到就是空串）。
const localIPProbeTimeout = 2 * time.Second

// localIPDial 是探测实际使用的拨号函数。
//
// 抽成变量只为一件事：让测试能替换它来模拟"DNS 解析卡住"，从而钉住"探测必须有
// 上界"这条不变量（见 localip_test.go 的 TestLocalIPsProbeIsBounded）。
// 生产路径永远是 DialContext + localIPProbeTimeout。
var localIPDial = (&net.Dialer{Timeout: localIPProbeTimeout}).DialContext

// LocalIPs 返回本机用于访问 serverHost 的源地址（IPv4 与 IPv6 各试一次）。
//
// 实现是"不发包的 UDP connect"：只建套接字 + connect，内核做一次路由查找后
// LocalAddr 就是我们要的源地址，全程没有报文发出。
//
// 为什么不用 net.Interfaces()：Linux 上它走 AF_NETLINK，而 Agent 的 systemd 单元
// 把 RestrictAddressFamilies 限制成 AF_INET AF_INET6 AF_UNIX —— 调用会直接失败
// （socket: address family not supported），表现为"代码没错、沙箱里跑不通"。
// udp4/udp6 只用到 AF_INET/AF_INET6，都在白名单里。
//
// 入参可以是主机名或 IP 字面量；出错一律静默返回空串 —— 这只是附加信息，
// 绝不能因为它让 Agent 连不上（v6 失败尤其常见：服务端没有 AAAA 记录时必然失败）。
//
// 两次探测共享 localIPProbeTimeout 这一个 deadline：最坏情况下总共等这么久，
// 而不是每个地址族各等一次；ctx 被取消（退出/重连）时也立刻返回空串。
func LocalIPs(ctx context.Context, serverHost string) (v4, v6 string) {
	host := strings.TrimSpace(serverHost)
	if host == "" {
		return "", ""
	}
	pctx, cancel := context.WithTimeout(ctx, localIPProbeTimeout)
	defer cancel()
	return probeLocalIP(pctx, host, "udp4"), probeLocalIP(pctx, host, "udp6")
}

// serverHostFromURL 从服务端地址里取出主机名，供 LocalIPs 使用。
//
// 只做解析不做校验：地址的合法性由 client.wsURL() 负责（它才决定能不能连），
// 这里解析失败就返回空串，让本机地址留空，不干扰连接流程。
func serverHostFromURL(serverURL string) string {
	u, err := url.Parse(strings.TrimSpace(serverURL))
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// probeLocalIP 对单个地址族做一次 UDP connect 并读回 LocalAddr。
//
// 走 DialContext 而不是 net.Dial：ctx 负责总上限（DNS 卡住时立刻放弃），
// Dialer.Timeout 是第二道保险。
func probeLocalIP(ctx context.Context, host, network string) string {
	conn, err := localIPDial(ctx, network, net.JoinHostPort(host, localIPProbePort))
	if err != nil {
		return ""
	}
	// UDP 的 Close 不会发任何东西，纯本地资源释放。
	defer conn.Close()

	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok || addr.IP == nil {
		return ""
	}
	// 规范化：上游拿到的是 "1.2.3.4"/"2001:db8::1"，而不是带 zone 或
	// 4 字节/16 字节混用的原始形态，服务端校验与前端展示都省事。
	return addr.IP.String()
}
