package server

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"probe/internal/protocol"
	"probe/internal/store"
)

// TestStaticAssetsUseETag 验证前端资源走协商缓存：
// go:embed 的文件没有修改时间，如果只依赖 Last-Modified，浏览器每次导航都会
// 重新下载全部资源（约 77 KB）。
func TestStaticAssetsUseETag(t *testing.T) {
	h := newAuthHarness(t)

	for _, path := range []string{"/", "/app.js", "/chart.js", "/style.css"} {
		_, _, resp := h.do(t, http.MethodGet, path, nil, false, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s 状态码 = %d", path, resp.StatusCode)
		}
		etag := resp.Header.Get("Etag")
		if etag == "" {
			t.Fatalf("%s 缺少 ETag（浏览器无法协商缓存）", path)
		}

		// 带上 If-None-Match 再请求：必须只回 304，不带正文。
		status, _, resp304 := h.do(t, http.MethodGet, path, nil, false,
			map[string]string{"If-None-Match": etag})
		if status != http.StatusNotModified {
			t.Fatalf("%s 带 If-None-Match 应当 304，实际 %d", path, status)
		}
		if resp304.ContentLength > 0 {
			t.Fatalf("%s 的 304 不该带正文（%d 字节）", path, resp304.ContentLength)
		}
	}

	// 内容变了 ETag 必须变（这里用两个不同文件模拟）。
	_, _, app := h.do(t, http.MethodGet, "/app.js", nil, false, nil)
	_, _, style := h.do(t, http.MethodGet, "/style.css", nil, false, nil)
	if app.Header.Get("Etag") == style.Header.Get("Etag") {
		t.Fatal("不同内容的 ETag 不该相同")
	}
}

// TestUnknownAPIPathsReturnJSON 验证 /api/ 下的所有响应都是统一的 JSON 错误信封，
// 包括"路径对、方法错"的情况（否则前端会拿到纯文本 405，提示变成未知错误）。
func TestUnknownAPIPathsReturnJSON(t *testing.T) {
	h := newAuthHarness(t)

	cases := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/v1/nope"},
		{http.MethodPost, "/api/v1/nope"},
		{http.MethodGet, "/api/v1/nodes/1/token"}, // 只支持 POST
		{http.MethodPost, "/api/v1/nodes"},        // 支持 POST，但缺 body/CSRF 时是 4xx JSON
		{http.MethodPut, "/api/v1/session"},       // 只支持 GET
		{http.MethodDelete, "/api/"},
	}
	for _, tc := range cases {
		status, body, resp := h.do(t, tc.method, tc.path, nil, false, nil)
		ct := resp.Header.Get("Content-Type")
		if !strings.HasPrefix(ct, "application/json") {
			t.Errorf("%s %s 的 Content-Type = %q，必须是 JSON", tc.method, tc.path, ct)
			continue
		}
		if status == http.StatusOK {
			t.Errorf("%s %s 不该返回 200", tc.method, tc.path)
		}
		if _, ok := body["error"]; !ok {
			t.Errorf("%s %s 的响应缺少 error 字段: %v", tc.method, tc.path, body)
		}
	}
}

// TestSessionRenewalIsThrottled 验证会话滑动续期不会"每个请求写一次库"：
// 那会把"每分钟个位数事务"变成"每请求一次写"，并与采样落盘抢唯一的写连接。
func TestSessionRenewalIsThrottled(t *testing.T) {
	h := newAuthHarness(t)
	ctx := context.Background()

	// 找出当前会话的哈希与 last_seen_at。
	var hash []byte
	var lastSeen int64
	if err := h.srv.db.Reader().QueryRowContext(ctx,
		`SELECT token_hash, last_seen_at FROM sessions LIMIT 1`).Scan(&hash, &lastSeen); err != nil {
		t.Fatalf("读取会话: %v", err)
	}

	// 第一次续期（距上次写库超过节流窗口）应当写库。
	now := time.Unix(lastSeen, 0).Add(store.SessionRenewInterval + time.Second)
	if _, err := h.srv.db.SessionByHash(ctx, hash, now, 7*24*time.Hour); err != nil {
		t.Fatalf("续期: %v", err)
	}
	var afterFirst int64
	if err := h.srv.db.Reader().QueryRowContext(ctx,
		`SELECT last_seen_at FROM sessions WHERE token_hash = ?`, hash).Scan(&afterFirst); err != nil {
		t.Fatalf("读取: %v", err)
	}
	if afterFirst != now.Unix() {
		t.Fatalf("第一次续期应当写入 last_seen_at=%d，实际 %d", now.Unix(), afterFirst)
	}

	// 紧接着的请求（窗口内）不该再写库。
	soon := now.Add(time.Second)
	session, err := h.srv.db.SessionByHash(ctx, hash, soon, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("续期: %v", err)
	}
	var afterSecond int64
	if err := h.srv.db.Reader().QueryRowContext(ctx,
		`SELECT last_seen_at FROM sessions WHERE token_hash = ?`, hash).Scan(&afterSecond); err != nil {
		t.Fatalf("读取: %v", err)
	}
	if afterSecond != afterFirst {
		t.Fatalf("节流窗口内不该写库：%d → %d", afterFirst, afterSecond)
	}
	// 但返回值里的有效期仍然要往后推（调用方语义不变）。
	if session.ExpiresAt != soon.Add(7*24*time.Hour).Unix() {
		t.Fatalf("返回值里的 ExpiresAt = %d，应当按本次请求续期", session.ExpiresAt)
	}
}

// TestAgentRateLimitOnlyCountsMetrics 验证控制帧不消耗上报配额：
// 否则心跳 / ack 稍密一点就会开始丢真实数据。
func TestAgentRateLimitOnlyCountsMetrics(t *testing.T) {
	ts, s, node, token := newAgentTestServer(t)
	s.agents.msgPerSecond = 3
	s.agents.rateWindow = time.Hour // 窗口拉长，避免测试与时间赛跑

	conn := mustDialAgent(t, ts, token)
	sendFrame(t, conn, helloFrame(t, testHello()))
	readHandshake(t, conn)

	// 先用一堆 ping 把"配额"占满（如果限流把控制帧也算进去的话）。
	for i := 0; i < 10; i++ {
		ping, err := protocol.New(protocol.TypePing, protocol.Ping{TsUS: time.Now().UnixMicro()})
		if err != nil {
			t.Fatalf("构造 ping: %v", err)
		}
		sendFrame(t, conn, ping)
		// 每条 ping 都会收到 pong：顺便确认控制帧确实被处理了。
		if _, err := readFrame(t, conn, 3*time.Second); err != nil {
			t.Fatalf("第 %d 条 ping 没有回 pong: %v", i+1, err)
		}
	}

	// 之后仍然要能正常接收 3 帧指标（配额没被 ping 吃掉）。
	for i := 1; i <= 3; i++ {
		frame, err := protocol.New(protocol.TypeMetrics, testMetrics())
		if err != nil {
			t.Fatalf("构造指标帧: %v", err)
		}
		frame.Seq = uint64(i)
		sendFrame(t, conn, frame)
	}
	waitFor(t, 3*time.Second, "3 帧指标全部入库", func() bool {
		n, ok := s.State().Get(node.ID)
		return ok && n.Seq >= 3
	})

	// 第 4 帧要被限流丢弃（每秒 3 帧），但连接不断开。
	frame, err := protocol.New(protocol.TypeMetrics, testMetrics())
	if err != nil {
		t.Fatalf("构造指标帧: %v", err)
	}
	frame.Seq = 4
	sendFrame(t, conn, frame)
	time.Sleep(300 * time.Millisecond)
	if n, _ := s.State().Get(node.ID); n.Seq > 3 {
		t.Fatalf("超速的第 4 帧不该入库（seq=%d）", n.Seq)
	}
}
