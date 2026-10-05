// Package e2e 用真实 TCP 连接把 probe-server 与 probe-agent 拼在一起跑。
//
// 这是唯一能证明"两个二进制真的能协同工作"的测试：不 mock 传输、不 mock 数据库，
// 走的是和线上完全相同的 Server.Run / Client.Run 代码路径。
package protocol

import (
	"context"
	"net"
	"path/filepath"
	"probe/internal/e2e/harness"
	"testing"
	"time"
)

func TestAgentReportsToRealServer(t *testing.T) {
	h := harness.StartServer(t, "127.0.0.1:0", filepath.Join(t.TempDir(), "probe.db"))
	node, token := h.CreateNode(t, "e2e-01")

	client, _ := harness.NewClient(t, "http://"+h.Addr, token)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = client.Run(ctx) }()

	// 至少两拍：证明 1 秒节奏真的在跑。
	harness.WaitFor(t, 15*time.Second, "收到两拍上报", func() bool {
		n, ok := h.Srv.State().Get(node.ID)
		return ok && n.Seq >= 2
	})

	n, _ := h.Srv.State().Get(node.ID)
	if !n.Connected {
		t.Error("节点应当处于连接状态")
	}
	if n.ObservedIP != "127.0.0.1" {
		t.Errorf("观察到的来源 IP = %q", n.ObservedIP)
	}
	// Agent 自报的本机地址走的是"不发包的 UDP connect"（internal/agent/localip.go）：
	// 这是唯一一条能证明它真的填进 hello、又真的落进 state 的端到端断言。
	// 服务端地址就是 127.0.0.1，所以 v4 必然是回环；v6 允许为空（有些机器没开 IPv6）。
	if n.LocalIP == "" {
		t.Error("Agent 应当自报本机 IPv4（服务端地址是 127.0.0.1，UDP connect 必然拿得到）")
	} else if ip := net.ParseIP(n.LocalIP); ip == nil || ip.To4() == nil {
		t.Errorf("本机 IPv4 = %q，不是合法 IPv4", n.LocalIP)
	}
	if n.LocalIP6 != "" && net.ParseIP(n.LocalIP6) == nil {
		t.Errorf("本机 IPv6 = %q，既不是空也不是合法 IP", n.LocalIP6)
	}
	if n.Info.Iface.Name != "eth0" || n.Info.CPU.Cores != 2 {
		t.Errorf("静态信息不对: %+v", n.Info)
	}
	if n.Metrics.Net.Iface != "eth0" || n.Metrics.Net.RxRaw != 9876543210 {
		t.Errorf("网卡指标不对: %+v", n.Metrics.Net)
	}
	if n.Metrics.UptimeSec != 1234567 || n.Metrics.Load.L1 != 0.15 {
		t.Errorf("基础指标不对: %+v", n.Metrics)
	}
	if got := n.Metrics.Mem.Pct; got < 51.7 || got > 51.8 {
		t.Errorf("内存使用率 = %v，期望约 51.77", got)
	}
	if n.Gap != 0 {
		t.Errorf("连续上报不应出现缺口，实际 %d", n.Gap)
	}
}

func TestAgentReconnectsAfterServerRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "probe.db")
	h1 := harness.StartServer(t, "127.0.0.1:0", dbPath)
	addr := h1.Addr
	node, token := h1.CreateNode(t, "e2e-02")

	client, _ := harness.NewClient(t, "http://"+addr, token)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = client.Run(ctx) }()

	harness.WaitFor(t, 15*time.Second, "第一次连接成功", func() bool {
		_, ok := h1.Srv.State().Get(node.ID)
		return ok
	})

	// 完整重启服务端（进程级）：先优雅退出，再用同一个地址与同一个数据库起来。
	h1.Stop(t)

	h2 := harness.StartServer(t, addr, dbPath)
	// 重启后内存状态可能是空的，也可能从 node_runtime 恢复了上次的最后状态
	// （退出前会做一次收尾落盘）。无论哪种，**都不能是"已连接"**：
	// 恢复出来的状态必须由 last_seen 决定，也就是如实显示为抖动或离线。
	for _, n := range h2.Srv.State().Snapshot() {
		if n.Connected {
			t.Fatal("重启后恢复出来的节点不该被标记为已连接")
		}
		if n.LastSeen.IsZero() || n.LastSeen.After(time.Now()) {
			t.Fatalf("恢复出来的 last_seen 不合法: %v", n.LastSeen)
		}
	}

	harness.WaitFor(t, 30*time.Second, "Agent 自动重连", func() bool {
		n, ok := h2.Srv.State().Get(node.ID)
		return ok && n.Seq >= 1 && n.Connected
	})

	// 重连后节点身份不变（同一个 Token → 同一个节点）。
	n, _ := h2.Srv.State().Get(node.ID)
	if n.Info.Hostname == "" {
		t.Error("重连后应当收到完整的 hello 信息")
	}
	t.Logf("重连成功：node_id=%d seq=%d", n.NodeID, n.Seq)
}

func TestManyAgentsConcurrently(t *testing.T) {
	const agents = 10

	h := harness.StartServer(t, "127.0.0.1:0", filepath.Join(t.TempDir(), "probe.db"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ids := make([]int64, 0, agents)
	for i := 0; i < agents; i++ {
		node, token := h.CreateNode(t, "many-"+string(rune('a'+i)))
		ids = append(ids, node.ID)

		client, _ := harness.NewClient(t, "http://"+h.Addr, token)
		go func() { _ = client.Run(ctx) }()
	}

	harness.WaitFor(t, 20*time.Second, "全部 Agent 上报", func() bool {
		for _, id := range ids {
			n, ok := h.Srv.State().Get(id)
			if !ok || n.Seq < 2 {
				return false
			}
		}
		return true
	})

	if got := h.Srv.State().Len(); got != agents {
		t.Fatalf("内存状态里的节点数 = %d，期望 %d", got, agents)
	}
	for _, id := range ids {
		n, _ := h.Srv.State().Get(id)
		if !n.Connected || n.Metrics.Net.Iface != "eth0" {
			t.Fatalf("节点 %d 的状态不对: %+v", id, n)
		}
	}
}
