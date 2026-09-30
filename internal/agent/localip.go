package agent

import (
	"net"
	"net/url"
	"strings"
)

// localIPProbePort 是 UDP connect 用的目标端口。
//
// 端口号本身**没有意义**：我们不发包，只借 connect 让内核做一次路由查找，
// 查出来的源地址就是"这台机器去 serverHost 时会用哪个地址"。用 9（discard）
// 只是图个语义清楚，换成任何端口结果都一样。
const localIPProbePort = "9"

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
func LocalIPs(serverHost string) (v4, v6 string) {
	host := strings.TrimSpace(serverHost)
	if host == "" {
		return "", ""
	}
	return probeLocalIP(host, "udp4"), probeLocalIP(host, "udp6")
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
func probeLocalIP(host, network string) string {
	conn, err := net.Dial(network, net.JoinHostPort(host, localIPProbePort))
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
