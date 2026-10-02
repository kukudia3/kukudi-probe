package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"probe/internal/config"
	"probe/internal/store"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "probe.db"))
	if err != nil {
		t.Fatalf("打开测试数据库: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return New(config.Default(), db, slog.New(slog.DiscardHandler), time.UTC)
}

func get(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestHealthzReportsOK(t *testing.T) {
	rec := get(t, newTestServer(t), "/healthz")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q", ct)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析 JSON 失败: %v（原文 %s）", err, rec.Body.String())
	}
	if body["ok"] != true || body["db"] != "ok" {
		t.Fatalf("健康检查结果异常: %v", body)
	}
	// **匿名探针只拿得到"活着没有"**：版本与 commit 组合起来可以直接拿去挑已知
	// 漏洞，而同一份信息在 /api/v1/settings 里是要登录的。这条断言是那个边界的
	// 唯一守卫（见 TestHealthzHidesVersionFromAnonymous 的正向用例）。
	for _, key := range []string{"version", "commit", "uptime_sec", "time"} {
		if _, ok := body[key]; ok {
			t.Errorf("匿名的 /healthz 不该带 %q：%v", key, body)
		}
	}
	if len(body) != 2 {
		t.Errorf("匿名的 /healthz 只该有 ok 与 db 两个键，实际 %v", body)
	}
}

// TestHealthzHidesVersionFromAnonymous 是必修 4 的正向用例：
// 同一份信息，**匿名拿不到、有会话拿得到**。
//
// 路由本身保持免鉴权（运维探针要在登录之前打得通），所以这里验的是"响应体按
// 身份裁剪"，而不是状态码 —— 匿名也必须是 200，否则探针全废。
func TestHealthzHidesVersionFromAnonymous(t *testing.T) {
	h := newAuthHarness(t)

	// 先用**未登录**的客户端：这是探针/陌生人的视角。
	h.anonymousClient(t)
	status, anon := h.get(t, "/healthz")
	if status != http.StatusOK {
		t.Fatalf("匿名的 /healthz 状态码 = %d，期望 200（探针要在未登录时可用）", status)
	}
	if anon["ok"] != true || anon["db"] != "ok" {
		t.Fatalf("匿名健康检查结果异常: %v", anon)
	}
	for _, key := range []string{"version", "commit", "uptime_sec", "time"} {
		if _, ok := anon[key]; ok {
			t.Errorf("匿名不该拿到 %q：%v", key, anon)
		}
	}
	if len(anon) != 2 {
		t.Errorf("匿名的 /healthz 只该有 ok 与 db 两个键，实际 %v", anon)
	}

	// 带会话（重新登录一个客户端）：照旧带版本、提交号、运行时长与时间。
	admin := loginSecondDevice(t, h)
	status, withSession := admin.get(t, "/healthz")
	if status != http.StatusOK {
		t.Fatalf("带会话的 /healthz 状态码 = %d", status)
	}
	for _, key := range []string{"version", "commit", "uptime_sec", "time"} {
		if _, ok := withSession[key]; !ok {
			t.Errorf("带会话的 /healthz 应当带 %q：%v", key, withSession)
		}
	}
	if v, _ := withSession["version"].(string); v == "" {
		t.Errorf("带会话的 /healthz 里 version 不该为空：%v", withSession)
	}

	// 时间必须按**服务端时区**渲染（这里是 UTC，所以带 Z 后缀）。
	// 匿名那份干脆不给 —— 连时区本身都不是访客该知道的东西。
	if got, _ := withSession["time"].(string); !strings.HasSuffix(got, "Z") {
		t.Errorf("健康检查的时间应当按服务端时区（这里是 UTC）渲染，实际 %q", got)
	}
}

func TestIndexPageIsServed(t *testing.T) {
	rec := get(t, newTestServer(t), "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("Content-Type = %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "极简 VPS 探针") {
		t.Fatal("首页内容里没有标题")
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("Cache-Control = %q，期望 no-cache", cc)
	}
}

func TestStaticAssetsAreServed(t *testing.T) {
	s := newTestServer(t)
	cases := map[string]string{
		"/style.css": "text/css",
		"/app.js":    "javascript",
	}
	for path, wantType := range cases {
		rec := get(t, s, path)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s 状态码 = %d", path, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, wantType) {
			t.Fatalf("%s Content-Type = %q，期望包含 %q", path, ct, wantType)
		}
		if rec.Body.Len() == 0 {
			t.Fatalf("%s 内容为空", path)
		}
	}
}

func TestUnknownAPIPathReturnsJSONError(t *testing.T) {
	rec := get(t, newTestServer(t), "/api/nodes")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("状态码 = %d，期望 404", rec.Code)
	}
	var body errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析 JSON 失败: %v（原文 %s）", err, rec.Body.String())
	}
	if body.Error.Code != "not_found" || body.Error.Message == "" {
		t.Fatalf("错误结构不符合约定: %+v", body)
	}
}

func TestSecurityHeaders(t *testing.T) {
	rec := get(t, newTestServer(t), "/")
	header := rec.Header()
	if csp := header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") {
		t.Errorf("缺少 CSP: %q", csp)
	}
	if header.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("缺少 X-Content-Type-Options: nosniff")
	}
	if header.Get("Referrer-Policy") != "no-referrer" {
		t.Error("缺少 Referrer-Policy: no-referrer")
	}
	if header.Get("X-Frame-Options") != "DENY" {
		t.Error("缺少 X-Frame-Options: DENY")
	}
	// 未配置 TLS 时不应发 HSTS，否则会让浏览器把明文站点锁成 https。
	if hsts := header.Get("Strict-Transport-Security"); hsts != "" {
		t.Errorf("未启用 TLS 却设置了 HSTS: %q", hsts)
	}
}

func TestUnknownPathReturns404(t *testing.T) {
	rec := get(t, newTestServer(t), "/不存在的路径")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("状态码 = %d，期望 404", rec.Code)
	}
}

func TestPanicIsRecovered(t *testing.T) {
	s := newTestServer(t)
	handler := s.recoverPanic(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("测试 panic")
	}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/boom", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("状态码 = %d，期望 500", rec.Code)
	}
	if rec.Body.Len() == 0 {
		t.Fatal("应当返回错误内容")
	}
}

func TestHealthzWhenDatabaseIsClosed(t *testing.T) {
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "probe.db"))
	if err != nil {
		t.Fatalf("打开测试数据库: %v", err)
	}
	s := New(config.Default(), db, slog.New(slog.DiscardHandler), time.UTC)
	if err := db.Close(); err != nil {
		t.Fatalf("关闭数据库: %v", err)
	}
	rec := get(t, s, "/healthz")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("状态码 = %d，期望 503", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	if !strings.Contains(string(body), `"ok":false`) {
		t.Fatalf("响应应为 ok:false，实际 %s", body)
	}
}

// 真的监听端口、真的收到请求、真的优雅退出——Phase 2 的门禁就是这条链路。
func TestRunServesAndShutsDownGracefully(t *testing.T) {
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "probe.db"))
	if err != nil {
		t.Fatalf("打开测试数据库: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	cfg := config.Default()
	cfg.Listen = "127.0.0.1:0" // 让内核分配端口，避免测试之间抢占
	s := New(cfg, db, slog.New(slog.DiscardHandler), time.UTC)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	var addr string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if a := s.Addr(); a != nil {
			addr = a.String()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if addr == "" {
		t.Fatal("服务在 5 秒内没有开始监听")
	}

	res, err := http.Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatalf("请求 /healthz 失败: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", res.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("优雅退出返回错误: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("优雅退出超时")
	}

	if conn, err := net.DialTimeout("tcp", addr, 300*time.Millisecond); err == nil {
		_ = conn.Close()
		t.Fatal("退出后端口仍在监听")
	}
}
