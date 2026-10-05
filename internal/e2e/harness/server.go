package harness

import (
	"context"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"probe/internal/agent"
	"probe/internal/config"
	"probe/internal/server"
	"probe/internal/store"
)

// Harness 是一个真的跑起来的 probe-server：内存状态、数据库句柄与监听地址都在它身上。
//
// 字段是导出的（Srv / DB / Addr），因为用例要直接读它们：`h.Srv.State()` 断言"上报真的
// 落进了内存"、`h.DB` 用来播撒样本、`h.Addr` 拼出浏览器要访问的地址。
type Harness struct {
	Srv    *server.Server
	DB     *store.DB
	cancel context.CancelFunc
	done   chan error
	Addr   string
	once   sync.Once
}

// FixtureRoot 是 Agent 采集用的假 /proc 现场（internal/agent/testdata）。
//
// 注意：测试二进制的 CWD 是【调用方那个包的目录】（internal/e2e/<pkg>/），
// 所以这里是两级 ".." —— 比原来（用例就在 internal/e2e/ 下时）多一层。
const FixtureRoot = "../../agent/testdata/root"

// StartServer 启动一个真实的 probe-server（走 Server.Run 完整路径）。
func StartServer(t *testing.T, addr, dbPath string) *Harness {
	t.Helper()
	return StartServerFull(t, addr, dbPath, slog.New(slog.DiscardHandler), nil)
}

func StartServerWithLogger(t *testing.T, addr, dbPath string, logger *slog.Logger) *Harness {
	t.Helper()
	return StartServerFull(t, addr, dbPath, logger, nil)
}

func StartServerFull(t *testing.T, addr, dbPath string, logger *slog.Logger, mutate func(*config.Server)) *Harness {
	t.Helper()
	return StartServerAt(t, addr, dbPath, time.UTC, logger, mutate)
}

// StartServerAt 与 startServerFull 完全一样，只是能指定服务端的 --timezone。
//
// 跨时区的浏览器用例（见 tz_browser_test.go）要的正是"服务端时区 != 浏览器时区"：
// 只有两边不一致时，才分得清页面上的时间到底是按哪一边渲染的。
func StartServerAt(t *testing.T, addr, dbPath string, loc *time.Location, logger *slog.Logger, mutate func(*config.Server)) *Harness {
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

	h := &Harness{Srv: srv, DB: db, cancel: cancel, done: done, Addr: actual}
	t.Cleanup(func() { h.Stop(t) })

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

// Stop 优雅停掉服务端，并**等它真的停稳**再关数据库。
//
// 这里的等待之所以够用，靠的是 Server.Run 的退出契约：Run 返回时，它起的
// 后台循环（realtimeLoop / pipelineLoop / 通知分发器 worker）已经全部退出
// （见 server.go 里的 stopBackground）。少了那一层，"Run 已返回"就只代表
// "信号发出去了"，而这些循环可能还在写库 —— 紧接着的 db.Close() 与
// t.TempDir() 的 RemoveAll 就会和它们抢同一个目录（Linux 上表现为
// RemoveAll: directory not empty，Windows 上看不出来）。
func (h *Harness) Stop(t *testing.T) {
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
		if err := h.DB.Close(); err != nil {
			t.Errorf("关闭数据库: %v", err)
		}
	})
}

func (h *Harness) CreateNode(t *testing.T, name string) (store.Node, string) {
	t.Helper()
	node, token, err := h.DB.CreateNode(context.Background(), store.NewNode{
		Name: name, IntervalSec: 1, TrafficWarnPct: 80, ResetDay: 1,
	}, time.Now())
	if err != nil {
		t.Fatalf("创建节点 %s: %v", name, err)
	}
	return node, token
}

func NewClient(t *testing.T, serverURL, token string) (*agent.Client, *agent.Traffic) {
	t.Helper()
	traffic, warn, err := agent.LoadTraffic("")
	if err != nil || warn != "" {
		t.Fatalf("加载流量状态: %v %q", err, warn)
	}
	collector := agent.New(FixtureRoot, "", "/", traffic)
	client := agent.NewClient(agent.ClientConfig{
		ServerURL:      serverURL,
		Token:          token,
		AllowPlaintext: true,
	}, collector, traffic, slog.New(slog.DiscardHandler))
	return client, traffic
}

func WaitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
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
