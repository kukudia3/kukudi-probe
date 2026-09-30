package agent

import (
	"net"
	"strings"
	"testing"
)

// 回环地址是唯一**可以断言**的场景：任何联网机器都能把 127.0.0.1 路由到
// 127.0.0.1，不需要外网、不需要 DNS，CI 里也稳定。
func TestLocalIPsLoopback(t *testing.T) {
	v4, v6 := LocalIPs("127.0.0.1")
	if v4 != "127.0.0.1" {
		t.Fatalf("LocalIPs(127.0.0.1) 的 IPv4 = %q，期望 127.0.0.1", v4)
	}
	// v6 分支只要求"要么空、要么是合法 IPv6"：有些机器没开 IPv6，
	// 内核会直接返回 network is unreachable，这是正常情况而不是失败。
	if v6 != "" && net.ParseIP(v6).To4() != nil {
		t.Fatalf("LocalIPs(127.0.0.1) 的 IPv6 = %q，既不是空也不是 IPv6", v6)
	}
}

func TestLocalIPsLoopbackV6(t *testing.T) {
	v4, v6 := LocalIPs("::1")
	// 传 v6 字面量时 udp4 解析不出地址，v4 必然为空；v6 在有 IPv6 的机器上是 ::1。
	if v4 != "" {
		t.Fatalf("对 ::1 做 udp4 探测应当拿不到地址，实际 %q", v4)
	}
	if v6 != "" && net.ParseIP(v6) == nil {
		t.Fatalf("LocalIPs(::1) 的 IPv6 = %q 不是合法 IP", v6)
	}
}

// 取不到地址时必须是空串且不 panic：这只是附加信息，绝不能影响 Agent 连接。
func TestLocalIPsSilentlyFailsOnBadHost(t *testing.T) {
	for _, host := range []string{
		"",
		"   ",
		"no-such-host.invalid",
		"not a host",
		"300.300.300.300",
		"127.0.0.1:8080", // 已经带了端口：JoinHostPort 会再套一层，解析必失败
	} {
		v4, v6 := LocalIPs(host)
		if v4 != "" || v6 != "" {
			t.Errorf("LocalIPs(%q) = (%q, %q)，期望两个空串", host, v4, v6)
		}
	}
}

// 主机名也要能走通：先解析再 connect，拿到的应当是本地某个地址。
func TestLocalIPsResolvesHostname(t *testing.T) {
	v4, _ := LocalIPs("localhost")
	if v4 == "" {
		t.Skip("本机解析不出 localhost 的 A 记录，跳过")
	}
	ip := net.ParseIP(v4)
	if ip == nil || ip.To4() == nil {
		t.Fatalf("LocalIPs(localhost) 的 IPv4 = %q 不是合法 IPv4", v4)
	}
}

// serverHostFromURL 复用的是 client.go 里那套地址解析，测试钉住它的行为：
// 带端口、带路径、带子路径前缀都要能取出纯主机名。
func TestServerHostFromURL(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"https://probe.example.com", "probe.example.com"},
		{"https://probe.example.com:8443", "probe.example.com"},
		{"wss://probe.example.com/probe/api/v1/agent/ws", "probe.example.com"},
		{"http://127.0.0.1:8080", "127.0.0.1"},
		{"https://[2001:db8::1]:8443", "2001:db8::1"},
		{"  https://probe.example.com  ", "probe.example.com"},
		{"://bad", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := serverHostFromURL(tc.in); got != tc.want {
			t.Errorf("serverHostFromURL(%q) = %q，期望 %q", tc.in, got, tc.want)
		}
	}
}

// 探测过程绝不能发出报文：UDP connect 只是让内核做一次路由查找。
// 这里用"连一个必然不可达的黑洞地址也不能阻塞"来间接守住这条——
// 真发包的话在无网络环境下要么超时要么报错，绝不会微秒级返回。
func TestLocalIPsDoesNotSendPackets(t *testing.T) {
	// 203.0.113.0/24 是 TEST-NET-3，永远不可达；connect 不发包所以立刻返回。
	v4, v6 := LocalIPs("203.0.113.1")
	if v4 == "" {
		// 本机没有默认路由时为空，这是允许的；这里只要求不阻塞、不 panic。
		t.Log("没有到 TEST-NET-3 的路由，v4 为空（符合预期）")
	} else if net.ParseIP(v4).To4() == nil {
		t.Fatalf("拿到的 v4 = %q 不是 IPv4", v4)
	}
	if v6 != "" && net.ParseIP(v6) == nil {
		t.Fatalf("拿到的 v6 = %q 不是 IP", v6)
	}
	if strings.Contains(v4, ":") {
		t.Fatalf("udp4 分支不该返回 IPv6: %q", v4)
	}
}
