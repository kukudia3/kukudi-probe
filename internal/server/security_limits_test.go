package server

import (
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// TestSetupIsRateLimited 验证暴力猜测初始化码会被限流（而不是无限次尝试）。
func TestSetupIsRateLimited(t *testing.T) {
	h := newAuthHarnessWithConfig(t, alertTestConfig())

	// 把初始化码换回"还没初始化"的状态是不现实的（harness 已经建好管理员），
	// 所以这里直接测限流器本身：同一 IP 连续试错会被锁。
	now := time.Now()
	for i := 0; i < 5; i++ {
		if ok, _ := h.srv.auth.setup.allowed("203.0.113.9", now); !ok {
			t.Fatalf("第 %d 次不该被拦", i+1)
		}
		h.srv.auth.setup.fail("203.0.113.9", now)
	}
	if ok, retry := h.srv.auth.setup.allowed("203.0.113.9", now); ok {
		t.Fatal("超过窗口上限后应当被拦")
	} else if retry <= 0 {
		t.Fatalf("应当给出重试等待时间，实际 %s", retry)
	}
	// 其它 IP 不受影响。
	if ok, _ := h.srv.auth.setup.allowed("198.51.100.7", now); !ok {
		t.Fatal("别的来源不该被牵连")
	}
}

// TestAgentPerIPConnectionLimit 验证单一来源的连接数上限（防止一个人占满）。
func TestAgentPerIPConnectionLimit(t *testing.T) {
	ts, _, _, token := newAgentTestServer(t)

	conns := make([]*websocket.Conn, 0, agentMaxConnsPerIP)
	defer func() {
		for _, c := range conns {
			_ = c.CloseNow()
		}
	}()
	for i := 0; i < agentMaxConnsPerIP; i++ {
		conn := mustDialAgent(t, ts, token)
		sendFrame(t, conn, helloFrame(t, testHello()))
		if _, err := readFrame(t, conn, 3*time.Second); err != nil {
			t.Fatalf("第 %d 条连接握手失败: %v", i+1, err)
		}
		conns = append(conns, conn)
	}

	// 第 N+1 条：服务端应当在升级之前就拒绝（返回 HTTP 状态码 429）。
	_, resp, err := dialAgent(t, ts, token)
	if err == nil {
		t.Fatalf("超过每 IP 上限后第 %d 条连接不该成功", agentMaxConnsPerIP+1)
	}
	if resp == nil || resp.StatusCode != http.StatusTooManyRequests {
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		t.Fatalf("应当以 429 拒绝，实际 %d", code)
	}

	// 释放一条之后应当又能连上（配额会归还）。
	_ = conns[len(conns)-1].CloseNow()
	conns = conns[:len(conns)-1]
	var reconnected bool
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		conn, _, err := dialAgent(t, ts, token)
		if err == nil {
			_ = conn.CloseNow()
			reconnected = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !reconnected {
		t.Fatal("释放连接后应当可以重新连上")
	}
}
