// Package e2e 用真实 TCP 连接把 probe-server 与 probe-agent 拼在一起跑。
//
// 这是唯一能证明"两个二进制真的能协同工作"的测试：不 mock 传输、不 mock 数据库，
// 走的是和线上完全相同的 Server.Run / Client.Run 代码路径。
package e2e

import (
	"context"
	"log/slog"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"probe/internal/agent"
	"probe/internal/config"
	"probe/internal/server"
	"probe/internal/store"
)

const fixtureRoot = "../agent/testdata/root"

type harness struct {
	srv    *server.Server
	db     *store.DB
	cancel context.CancelFunc
	done   chan error
	addr   string
	once   sync.Once
}

// startServer 启动一个真实的 probe-server（走 Server.Run 完整路径）。
func startServer(t *testing.T, addr, dbPath string) *harness {
	t.Helper()
	return startServerFull(t, addr, dbPath, slog.New(slog.DiscardHandler), nil)
}

func startServerWithLogger(t *testing.T, addr, dbPath string, logger *slog.Logger) *harness {
	t.Helper()
	return startServerFull(t, addr, dbPath, logger, nil)
}

func startServerFull(t *testing.T, addr, dbPath string, logger *slog.Logger, mutate func(*config.Server)) *harness {
	t.Helper()
	return startServerAt(t, addr, dbPath, time.UTC, logger, mutate)
}

// startServerAt 与 startServerFull 完全一样，只是能指定服务端的 --timezone。
//
// 跨时区的浏览器用例（见 tz_browser_test.go）要的正是"服务端时区 != 浏览器时区"：
// 只有两边不一致时，才分得清页面上的时间到底是按哪一边渲染的。
func startServerAt(t *testing.T, addr, dbPath string, loc *time.Location, logger *slog.Logger, mutate func(*config.Server)) *harness {
	t.Helper()

	ln, err := listenWithRetry(addr, 3*time.Second)
	if err != nil {
		t.Fatalf("监听 %s 失败: %v", addr, err)
	}
	actual := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("释放预检端口失败: %v", err)
	}

	db, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}

	cfg := config.Default()
	cfg.Listen = actual
	cfg.ShutdownGrace = 5 * time.Second
	if mutate != nil {
		mutate(&cfg)
	}
	srv := server.New(cfg, db, logger, loc)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()

	h := &harness{srv: srv, db: db, cancel: cancel, done: done, addr: actual}
	t.Cleanup(func() { h.stop(t) })

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if srv.Addr() != nil {
			return h
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("服务端没有开始监听")
	return nil
}

func listenWithRetry(addr string, timeout time.Duration) (net.Listener, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			return ln, nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			return nil, lastErr
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// stop 优雅停掉服务端，并**等它真的停稳**再关数据库。
//
// 这里的等待之所以够用，靠的是 Server.Run 的退出契约：Run 返回时，它起的
// 后台循环（realtimeLoop / pipelineLoop / 通知分发器 worker）已经全部退出
// （见 server.go 里的 stopBackground）。少了那一层，"Run 已返回"就只代表
// "信号发出去了"，而这些循环可能还在写库 —— 紧接着的 db.Close() 与
// t.TempDir() 的 RemoveAll 就会和它们抢同一个目录（Linux 上表现为
// RemoveAll: directory not empty，Windows 上看不出来）。
func (h *harness) stop(t *testing.T) {
	t.Helper()
	h.once.Do(func() {
		h.cancel()
		select {
		case err := <-h.done:
			if err != nil {
				t.Errorf("服务端退出返回错误: %v", err)
			}
		case <-time.After(20 * time.Second):
			t.Error("服务端退出超时")
		}
		if err := h.db.Close(); err != nil {
			t.Errorf("关闭数据库: %v", err)
		}
	})
}

func (h *harness) createNode(t *testing.T, name string) (store.Node, string) {
	t.Helper()
	node, token, err := h.db.CreateNode(context.Background(), store.NewNode{
		Name: name, IntervalSec: 1, TrafficWarnPct: 80, ResetDay: 1,
	}, time.Now())
	if err != nil {
		t.Fatalf("创建节点 %s: %v", name, err)
	}
	return node, token
}

func newClient(t *testing.T, serverURL, token string) (*agent.Client, *agent.Traffic) {
	t.Helper()
	traffic, warn, err := agent.LoadTraffic("")
	if err != nil || warn != "" {
		t.Fatalf("加载流量状态: %v %q", err, warn)
	}
	collector := agent.New(fixtureRoot, "", "/", traffic)
	client := agent.NewClient(agent.ClientConfig{
		ServerURL:      serverURL,
		Token:          token,
		AllowPlaintext: true,
	}, collector, traffic, slog.New(slog.DiscardHandler))
	return client, traffic
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待超时: %s", what)
}

func TestAgentReportsToRealServer(t *testing.T) {
	h := startServer(t, "127.0.0.1:0", filepath.Join(t.TempDir(), "probe.db"))
	node, token := h.createNode(t, "e2e-01")

	client, _ := newClient(t, "http://"+h.addr, token)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = client.Run(ctx) }()

	// 至少两拍：证明 1 秒节奏真的在跑。
	waitFor(t, 15*time.Second, "收到两拍上报", func() bool {
		n, ok := h.srv.State().Get(node.ID)
		return ok && n.Seq >= 2
	})

	n, _ := h.srv.State().Get(node.ID)
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
	h1 := startServer(t, "127.0.0.1:0", dbPath)
	addr := h1.addr
	node, token := h1.createNode(t, "e2e-02")

	client, _ := newClient(t, "http://"+addr, token)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = client.Run(ctx) }()

	waitFor(t, 15*time.Second, "第一次连接成功", func() bool {
		_, ok := h1.srv.State().Get(node.ID)
		return ok
	})

	// 完整重启服务端（进程级）：先优雅退出，再用同一个地址与同一个数据库起来。
	h1.stop(t)

	h2 := startServer(t, addr, dbPath)
	// 重启后内存状态可能是空的，也可能从 node_runtime 恢复了上次的最后状态
	// （退出前会做一次收尾落盘）。无论哪种，**都不能是"已连接"**：
	// 恢复出来的状态必须由 last_seen 决定，也就是如实显示为抖动或离线。
	for _, n := range h2.srv.State().Snapshot() {
		if n.Connected {
			t.Fatal("重启后恢复出来的节点不该被标记为已连接")
		}
		if n.LastSeen.IsZero() || n.LastSeen.After(time.Now()) {
			t.Fatalf("恢复出来的 last_seen 不合法: %v", n.LastSeen)
		}
	}

	waitFor(t, 30*time.Second, "Agent 自动重连", func() bool {
		n, ok := h2.srv.State().Get(node.ID)
		return ok && n.Seq >= 1 && n.Connected
	})

	// 重连后节点身份不变（同一个 Token → 同一个节点）。
	n, _ := h2.srv.State().Get(node.ID)
	if n.Info.Hostname == "" {
		t.Error("重连后应当收到完整的 hello 信息")
	}
	t.Logf("重连成功：node_id=%d seq=%d", n.NodeID, n.Seq)
}

func TestManyAgentsConcurrently(t *testing.T) {
	const agents = 10

	h := startServer(t, "127.0.0.1:0", filepath.Join(t.TempDir(), "probe.db"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ids := make([]int64, 0, agents)
	for i := 0; i < agents; i++ {
		node, token := h.createNode(t, "many-"+string(rune('a'+i)))
		ids = append(ids, node.ID)

		client, _ := newClient(t, "http://"+h.addr, token)
		go func() { _ = client.Run(ctx) }()
	}

	waitFor(t, 20*time.Second, "全部 Agent 上报", func() bool {
		for _, id := range ids {
			n, ok := h.srv.State().Get(id)
			if !ok || n.Seq < 2 {
				return false
			}
		}
		return true
	})

	if got := h.srv.State().Len(); got != agents {
		t.Fatalf("内存状态里的节点数 = %d，期望 %d", got, agents)
	}
	for _, id := range ids {
		n, _ := h.srv.State().Get(id)
		if !n.Connected || n.Metrics.Net.Iface != "eth0" {
			t.Fatalf("节点 %d 的状态不对: %+v", id, n)
		}
	}
}
