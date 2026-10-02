package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"probe/internal/config"
	"probe/internal/store"
)

// authHarness 是一个"已初始化管理员"的真实服务端 + 带 Cookie 的 HTTP 客户端。
type authHarness struct {
	ts       *httptest.Server
	srv      *Server
	client   *http.Client
	csrf     string
	username string
	password string
	cancel   context.CancelFunc
}

func newAuthHarness(t *testing.T) *authHarness {
	t.Helper()
	return newAuthHarnessWithConfig(t, config.Default())
}

func newAuthHarnessWithConfig(t *testing.T, cfg config.Server) *authHarness {
	t.Helper()
	return newAuthHarnessFull(t, cfg, slog.New(slog.DiscardHandler), "correct-horse-battery-staple")
}

// newAuthHarnessWithConfigAndLogger 注入自定义 logger（用于断言日志内容）。
func newAuthHarnessWithConfigAndLogger(t *testing.T, cfg config.Server, logger *slog.Logger) *authHarness {
	t.Helper()
	return newAuthHarnessFull(t, cfg, logger, "a-very-good-password")
}

func newAuthHarnessFull(t *testing.T, cfg config.Server, logger *slog.Logger, password string) *authHarness {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "probe.db")
	db, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	s := New(cfg, db, logger, time.UTC)
	if err := s.auth.EnsureSetupCode(ctx); err != nil {
		t.Fatalf("生成初始化码: %v", err)
	}
	code := s.auth.setupCode
	if code == "" {
		t.Fatal("初始化码为空")
	}

	// 这个脚手架起了两个长期后台执行体：1 Hz 的实时循环与通知发送 worker。
	// 它们都要在用例返回前**停稳**（只 cancel 是发信号就走，见 newBackgroundGuard）。
	// 用例中途调 h.cancel() 停实时循环时，拿到的也是同一份"取消 + 等它退出"。
	guard := newBackgroundGuard(t)
	// 实时循环每秒都要读节点视图与流量汇总（写只在告警状态变化时发生），
	// 但"读"同样会碰数据目录（WAL 的 -shm），所以顺手盯一眼它静不静。
	guard.watchDir(filepath.Dir(dbPath))
	realtime := guard.start("realtimeLoop", s.realtimeLoop)
	dispatch := guard.start("dispatch", s.dispatch.Start)
	stopLoops := func() {
		dispatch.stop()
		realtime.stop()
	}

	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	// 后注册 ⇒ 先执行：先停稳后台循环，再关掉 httptest 服务端，最后才轮到
	// 守卫核账、关数据库、删临时目录（t.Cleanup 是后进先出）。
	t.Cleanup(stopLoops)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("创建 Cookie jar: %v", err)
	}
	h := &authHarness{
		ts:       ts,
		srv:      s,
		client:   &http.Client{Jar: jar, Timeout: 10 * time.Second},
		username: "admin",
		password: password,
		cancel:   stopLoops,
	}

	status, body := h.post(t, "/api/v1/setup", map[string]any{
		"code": code, "username": h.username, "password": h.password,
	}, nil)
	if status != http.StatusOK {
		t.Fatalf("初始化失败: HTTP %d %v", status, body)
	}
	h.csrf, _ = body["csrf_token"].(string)
	if h.csrf == "" {
		t.Fatal("初始化后没有拿到 CSRF Token")
	}
	return h
}

// do 发一个请求；csrf 为 true 时带上 CSRF 头，extra 用于附加头。
func (h *authHarness) do(t *testing.T, method, path string, payload any, csrf bool, extra map[string]string) (int, map[string]any, *http.Response) {
	t.Helper()

	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("序列化请求体: %v", err)
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, h.ts.URL+path, body)
	if err != nil {
		t.Fatalf("构造请求: %v", err)
	}
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if csrf {
		req.Header.Set("X-CSRF-Token", h.csrf)
	}
	for k, v := range extra {
		req.Header.Set(k, v)
	}

	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("请求 %s %s 失败: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	out := map[string]any{}
	raw, _ := io.ReadAll(resp.Body)
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return resp.StatusCode, out, resp
}

func (h *authHarness) post(t *testing.T, path string, payload any, extra map[string]string) (int, map[string]any) {
	t.Helper()
	status, body, _ := h.do(t, http.MethodPost, path, payload, true, extra)
	return status, body
}

func (h *authHarness) get(t *testing.T, path string) (int, map[string]any) {
	t.Helper()
	status, body, _ := h.do(t, http.MethodGet, path, nil, false, nil)
	return status, body
}

// anonymousClient 换成一个没有会话 Cookie 的客户端（用于验证未登录行为）。
func (h *authHarness) anonymousClient(t *testing.T) {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("创建 Cookie jar: %v", err)
	}
	h.client = &http.Client{Jar: jar, Timeout: 10 * time.Second}
}

func (h *authHarness) put(t *testing.T, path string, payload any) (int, map[string]any) {
	t.Helper()
	status, body, _ := h.do(t, http.MethodPut, path, payload, true, nil)
	return status, body
}

// rawBody 发一个 GET 并返回未经解析的响应体（用于"响应里不能出现某字符串"这类断言）。
func (h *authHarness) rawBody(t *testing.T, path string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, h.ts.URL+path, nil)
	if err != nil {
		t.Fatalf("构造请求: %v", err)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("请求 %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取响应体: %v", err)
	}
	return string(raw)
}

func TestSetupCreatesAdminAndSession(t *testing.T) {
	h := newAuthHarness(t)

	status, body := h.get(t, "/api/v1/session")
	if status != http.StatusOK {
		t.Fatalf("会话查询失败: %d", status)
	}
	if body["needs_setup"] != false || body["authenticated"] != true {
		t.Fatalf("初始化后的会话状态不对: %v", body)
	}
	if body["username"] != h.username {
		t.Fatalf("用户名 = %v", body["username"])
	}

	// 重复初始化必须被拒绝。
	status, _ = h.post(t, "/api/v1/setup", map[string]any{
		"code": "whatever", "username": "x", "password": "0123456789",
	}, nil)
	if status != http.StatusConflict {
		t.Fatalf("重复初始化应当返回 409，实际 %d", status)
	}

	// 密码必须落库为 Argon2id PHC 字符串，且不是明文。
	hash, ok, err := h.srv.db.GetSetting(context.Background(), store.KeyAdminHash)
	if err != nil || !ok {
		t.Fatalf("读取密码哈希: %v ok=%v", err, ok)
	}
	if !strings.HasPrefix(hash, "$argon2id$") {
		t.Fatalf("密码哈希格式不对: %q", hash)
	}
	if strings.Contains(hash, h.password) {
		t.Fatal("哈希里出现了明文密码")
	}
}

func TestSetupRejectsWrongCodeAndWeakPassword(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "probe.db"))
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	s := New(config.Default(), db, slog.New(slog.DiscardHandler), time.UTC)
	if err := s.auth.EnsureSetupCode(ctx); err != nil {
		t.Fatalf("生成初始化码: %v", err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	jar, _ := cookiejar.New(nil)
	h := &authHarness{ts: ts, srv: s, client: &http.Client{Jar: jar, Timeout: 10 * time.Second}, username: "admin"}

	status, _ := h.post(t, "/api/v1/setup", map[string]any{
		"code": "ffffffffffff", "username": "admin", "password": "0123456789",
	}, nil)
	if status != http.StatusForbidden {
		t.Fatalf("错误初始化码应当返回 403，实际 %d", status)
	}

	status, _ = h.post(t, "/api/v1/setup", map[string]any{
		"code": s.auth.setupCode, "username": "admin", "password": "short",
	}, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("弱密码应当返回 400，实际 %d", status)
	}

	status, _ = h.post(t, "/api/v1/setup", map[string]any{
		"code": s.auth.setupCode, "username": "admin", "password": "admin",
	}, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("密码与用户名相同应当返回 400，实际 %d", status)
	}
}

func TestSetupCodeExpiry(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	cfg.SetupCodeTTL = time.Minute
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "probe.db"))
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	s := New(cfg, db, slog.New(slog.DiscardHandler), time.UTC)
	if err := s.auth.EnsureSetupCode(ctx); err != nil {
		t.Fatalf("生成初始化码: %v", err)
	}
	code := s.auth.setupCode

	// 手动把有效期拨到过去。
	s.auth.mu.Lock()
	s.auth.setupExpiry = time.Now().Add(-time.Second)
	s.auth.mu.Unlock()
	if s.auth.setupCodeMatches(code, time.Now()) {
		t.Fatal("过期后初始化码不应通过校验")
	}
}

func TestLoginFlowAndLogout(t *testing.T) {
	h := newAuthHarness(t)

	// 先登出，再重新登录。
	status, _, _ := h.do(t, http.MethodPost, "/api/v1/auth/logout", nil, true, nil)
	if status != http.StatusNoContent {
		t.Fatalf("登出应当返回 204，实际 %d", status)
	}
	if status, _ := h.get(t, "/api/v1/nodes"); status != http.StatusUnauthorized {
		t.Fatalf("登出后访问业务接口应当 401，实际 %d", status)
	}

	status, body := h.post(t, "/api/v1/auth/login", map[string]any{
		"username": h.username, "password": "wrong-password-here",
	}, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("错误密码应当返回 401，实际 %d", status)
	}

	status, body = h.post(t, "/api/v1/auth/login", map[string]any{
		"username": h.username, "password": h.password,
	}, nil)
	if status != http.StatusOK {
		t.Fatalf("登录失败: %d %v", status, body)
	}
	csrf, _ := body["csrf_token"].(string)
	if csrf == "" {
		t.Fatal("登录后没有 CSRF Token")
	}
	h.csrf = csrf

	if status, _ := h.get(t, "/api/v1/nodes"); status != http.StatusOK {
		t.Fatalf("登录后应当可以访问业务接口，实际 %d", status)
	}
}

func TestLoginRateLimitAndLockout(t *testing.T) {
	h := newAuthHarness(t)

	// 窗口内最多 5 次尝试。
	for i := 0; i < 5; i++ {
		status, _ := h.post(t, "/api/v1/auth/login", map[string]any{
			"username": h.username, "password": "nope-nope-nope",
		}, nil)
		if status != http.StatusUnauthorized {
			t.Fatalf("第 %d 次失败登录状态码 = %d，期望 401", i+1, status)
		}
	}
	status, body, resp := h.do(t, http.MethodPost, "/api/v1/auth/login",
		map[string]any{"username": h.username, "password": "nope-nope-nope"}, true, nil)
	if status != http.StatusTooManyRequests {
		t.Fatalf("超过窗口上限应当返回 429，实际 %d", status)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("429 应当带 Retry-After 头")
	}
	if body["error"] == nil {
		t.Errorf("429 响应体应当包含错误信息: %v", body)
	}

	// 即使密码正确，被限流期间也不能登录。
	status, _ = h.post(t, "/api/v1/auth/login", map[string]any{
		"username": h.username, "password": h.password,
	}, nil)
	if status != http.StatusTooManyRequests {
		t.Fatalf("限流期间正确密码也应当被拒绝，实际 %d", status)
	}
}

func TestCSRFAndOriginChecks(t *testing.T) {
	h := newAuthHarness(t)

	// 缺少 CSRF 头。
	status, body, _ := h.do(t, http.MethodPost, "/api/v1/nodes",
		map[string]any{"name": "n1", "interval_sec": 1}, false, nil)
	if status != http.StatusForbidden || body["error"].(map[string]any)["code"] != "bad_csrf" {
		t.Fatalf("缺少 CSRF 应当 403 bad_csrf，实际 %d %v", status, body)
	}

	// 错误的 CSRF 头。
	status, _, _ = h.do(t, http.MethodPost, "/api/v1/nodes",
		map[string]any{"name": "n1", "interval_sec": 1}, false,
		map[string]string{"X-CSRF-Token": "deadbeef"})
	if status != http.StatusForbidden {
		t.Fatalf("错误 CSRF 应当 403，实际 %d", status)
	}

	// 跨站 Origin。
	status, body, _ = h.do(t, http.MethodPost, "/api/v1/nodes",
		map[string]any{"name": "n1", "interval_sec": 1}, true,
		map[string]string{"Origin": "http://evil.example"})
	if status != http.StatusForbidden || body["error"].(map[string]any)["code"] != "bad_origin" {
		t.Fatalf("跨站 Origin 应当 403 bad_origin，实际 %d %v", status, body)
	}

	// 同源 Origin 正常放行。
	status, _, _ = h.do(t, http.MethodPost, "/api/v1/nodes",
		map[string]any{"name": "n1", "interval_sec": 1}, true,
		map[string]string{"Origin": h.ts.URL})
	if status != http.StatusCreated {
		t.Fatalf("同源请求应当成功，实际 %d", status)
	}

	// GET 请求不需要 CSRF。
	if status, _ := h.get(t, "/api/v1/nodes"); status != http.StatusOK {
		t.Fatalf("GET 不需要 CSRF，实际 %d", status)
	}
}

func TestSessionExpiryIsEnforced(t *testing.T) {
	h := newAuthHarness(t)

	// 把会话到期时间改到过去。
	if _, err := h.srv.db.Writer().ExecContext(context.Background(),
		`UPDATE sessions SET expires_at = ?`, time.Now().Add(-time.Hour).Unix()); err != nil {
		t.Fatalf("修改会话: %v", err)
	}
	status, body := h.get(t, "/api/v1/nodes")
	if status != http.StatusUnauthorized {
		t.Fatalf("过期会话应当 401，实际 %d %v", status, body)
	}
}

func TestUnauthenticatedRequestsAreRejected(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "probe.db"))
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := New(config.Default(), db, slog.New(slog.DiscardHandler), time.UTC)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	client := &http.Client{Timeout: 5 * time.Second}
	for _, path := range []string{"/api/v1/nodes", "/api/v1/stream"} {
		resp, err := client.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("请求 %s: %v", path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s 未登录应当 401，实际 %d", path, resp.StatusCode)
		}
	}
}

func TestSessionCookieAttributes(t *testing.T) {
	h := newAuthHarness(t)
	status, body, resp := h.do(t, http.MethodPost, "/api/v1/auth/login", map[string]any{
		"username": h.username, "password": h.password,
	}, true, nil)
	if status != http.StatusOK {
		t.Fatalf("登录失败: %d %v", status, body)
	}

	var setCookie string
	for _, c := range resp.Header.Values("Set-Cookie") {
		if strings.HasPrefix(c, sessionCookieName+"=") {
			setCookie = c
		}
	}
	if setCookie == "" {
		t.Fatal("登录后没有下发会话 Cookie")
	}
	for _, want := range []string{"HttpOnly", "SameSite=Lax", "Path=/"} {
		if !strings.Contains(setCookie, want) {
			t.Errorf("Cookie 缺少 %s：%s", want, setCookie)
		}
	}
	// httptest 走的是纯 HTTP，因此不应设置 Secure（否则浏览器根本不会回传）。
	if strings.Contains(setCookie, "Secure") {
		t.Errorf("非 TLS 连接不应设置 Secure：%s", setCookie)
	}

	// Cookie 值必须是高熵 Token（32 字节 base64url ≈ 43 字符）。
	value := strings.TrimSuffix(strings.TrimPrefix(setCookie, sessionCookieName+"="), ";")
	value, _, _ = strings.Cut(value, ";")
	if len(value) < 40 {
		t.Errorf("会话 Token 长度 = %d，过短", len(value))
	}
}

func TestCSRFDerivationIsStableAndUnpredictable(t *testing.T) {
	a := csrfFor("token-a")
	if a != csrfFor("token-a") {
		t.Fatal("同一会话的 CSRF Token 必须稳定（前端会缓存它）")
	}
	if a == csrfFor("token-b") {
		t.Fatal("不同会话必须得到不同的 CSRF Token")
	}
	raw, err := base64.RawURLEncoding.DecodeString(a)
	if err != nil || len(raw) != 32 {
		t.Fatalf("CSRF Token 解码异常: %v len=%d", err, len(raw))
	}
}

func TestPasswordHashRoundTrip(t *testing.T) {
	hash, err := hashPassword("correct-horse-battery-staple")
	if err != nil {
		t.Fatalf("生成哈希: %v", err)
	}
	if !strings.HasPrefix(hash, "$argon2id$v=19$m=65536,t=3,p=1$") {
		t.Fatalf("PHC 参数与设计不符: %q", hash)
	}
	ok, err := verifyPassword(hash, "correct-horse-battery-staple")
	if err != nil || !ok {
		t.Fatalf("正确密码应当通过: ok=%v err=%v", ok, err)
	}
	ok, err = verifyPassword(hash, "wrong")
	if err != nil || ok {
		t.Fatalf("错误密码不应通过: ok=%v err=%v", ok, err)
	}

	// 每次生成的盐都不同。
	other, err := hashPassword("correct-horse-battery-staple")
	if err != nil {
		t.Fatalf("生成哈希: %v", err)
	}
	if other == hash {
		t.Fatal("两次哈希结果相同，说明没有加盐")
	}

	for _, bad := range []string{"", "not-a-hash", "$argon2id$v=19$m=65536,t=3,p=1$onlyfour$parts$extra"} {
		if _, err := verifyPassword(bad, "x"); err == nil {
			t.Errorf("非法哈希 %q 应当报错", bad)
		}
	}
}

func TestTrustedProxyOnlyForwardsFromTrustedNetwork(t *testing.T) {
	captured := ""
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = clientIP(r)
		w.WriteHeader(http.StatusNoContent)
	})

	// 未配置可信代理：忽略 X-Forwarded-For。
	s := New(config.Default(), nil, slog.New(slog.DiscardHandler), time.UTC)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.9:1234"
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	s.resolveClientIP(handler).ServeHTTP(httptest.NewRecorder(), req)
	if captured != "203.0.113.9" {
		t.Fatalf("未配置可信代理时应当使用对端地址，实际 %q", captured)
	}

	// 配置了可信代理且来源在其中：只采信最右一条（代理追加的那条）。
	cfg := config.Default()
	cfg.TrustedProxy = "127.0.0.1/32"
	s2 := New(cfg, nil, slog.New(slog.DiscardHandler), time.UTC)
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2.RemoteAddr = "127.0.0.1:1234"
	req2.Header.Set("X-Forwarded-For", "198.51.100.7")
	s2.resolveClientIP(handler).ServeHTTP(httptest.NewRecorder(), req2)
	if captured != "198.51.100.7" {
		t.Fatalf("可信代理转发时应当取真实客户端 IP，实际 %q", captured)
	}

	// 伪造的 XFF：最右边的地址不可信，取它（而不是左边伪造的那个）。
	req3 := httptest.NewRequest(http.MethodGet, "/", nil)
	req3.RemoteAddr = "127.0.0.1:1234"
	req3.Header.Set("X-Forwarded-For", "1.1.1.1, 198.51.100.7")
	s2.resolveClientIP(handler).ServeHTTP(httptest.NewRecorder(), req3)
	if captured != "198.51.100.7" {
		t.Fatalf("应当取最右侧不可信地址，实际 %q", captured)
	}

	// 最右一条本身就在可信网段：说明"转发链"里最近一跳不是真正的客户端，
	// 左边的值是客户端可以随便写的——此时必须退回直连地址，绝不能采信伪造值。
	req4 := httptest.NewRequest(http.MethodGet, "/", nil)
	req4.RemoteAddr = "127.0.0.1:1234"
	req4.Header.Set("X-Forwarded-For", "198.51.100.7, 127.0.0.1")
	s2.resolveClientIP(handler).ServeHTTP(httptest.NewRecorder(), req4)
	if captured != "127.0.0.1" {
		t.Fatalf("最右一条可信时应当退回直连地址，实际 %q", captured)
	}
}

func TestAttemptLimiterWindowAndLock(t *testing.T) {
	l := newAttemptLimiter(3, time.Minute, 5, 10*time.Minute)
	now := time.Unix(1_700_000_000, 0)

	for i := 0; i < 3; i++ {
		if ok, _ := l.allowed("ip", now); !ok {
			t.Fatalf("第 %d 次应当放行", i+1)
		}
	}
	if ok, retry := l.allowed("ip", now); ok || retry <= 0 {
		t.Fatalf("超过窗口上限应当拒绝并给出重试时间，ok=%v retry=%s", ok, retry)
	}
	// 新窗口恢复。
	if ok, _ := l.allowed("ip", now.Add(time.Minute)); !ok {
		t.Fatal("新窗口应当放行")
	}
	// 连续失败到达阈值后锁定。
	for i := 0; i < 5; i++ {
		l.fail("ip2", now)
	}
	if ok, retry := l.allowed("ip2", now); ok || retry < 9*time.Minute {
		t.Fatalf("锁定后应当拒绝，ok=%v retry=%s", ok, retry)
	}
	// 成功一次即解除。
	l.succeed("ip2")
	if ok, _ := l.allowed("ip2", now.Add(11*time.Minute)); !ok {
		t.Fatal("锁定到期后应当放行")
	}
}

func TestAttemptLimiterBoundsEntries(t *testing.T) {
	l := newAttemptLimiter(1, time.Minute, 0, 0)
	now := time.Unix(1_700_000_000, 0)
	for i := 0; i < 3000; i++ {
		l.allowed(string(rune('a'+i%26))+string(rune('a'+i/26)), now)
	}
	l.mu.Lock()
	size := len(l.entries)
	l.mu.Unlock()
	if size > 1024 {
		t.Fatalf("限流表规模 = %d，必须封顶", size)
	}
}
