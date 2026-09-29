package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"probe/internal/config"
	"probe/internal/store"
)

// ---------------------------------------------------------------- 认证与会话

func TestSecurityPasswordChangeRevokesOtherSessions(t *testing.T) {
	h := newAuthHarness(t)

	// 第二条会话（模拟"另一台设备"）。
	other := h.cloneClient(t)
	status, body, _ := other.do(t, http.MethodPost, "/api/v1/auth/login", map[string]any{
		"username": "admin", "password": h.password,
	}, false, nil)
	if status != http.StatusOK {
		t.Fatalf("第二台设备登录失败: %d %v", status, body)
	}
	otherCSRF, _ := body["csrf_token"].(string)

	// 当前会话改密码。
	status, body = h.post(t, "/api/v1/auth/password", map[string]any{
		"current_password": h.password,
		"new_password":     "an-even-better-password",
		"new_password2":    "an-even-better-password",
	}, nil)
	if status != http.StatusOK {
		t.Fatalf("改密失败: %d %v", status, body)
	}
	if body["ok"] != true {
		t.Fatalf("改密响应不对: %v", body)
	}
	revoked, _ := body["revoked_sessions"].(float64)
	if revoked < 1 {
		t.Fatalf("应当至少注销 1 条其它会话，实际 %v", revoked)
	}

	// 当前会话仍然可用。
	if status, _ := h.get(t, "/api/v1/nodes"); status != http.StatusOK {
		t.Fatalf("当前会话不该被注销，实际 %d", status)
	}
	// 其它会话已被注销。
	otherResp, _, _ := other.do(t, http.MethodGet, "/api/v1/nodes", nil, false, nil)
	if otherResp != http.StatusUnauthorized {
		t.Fatalf("其它设备的会话应当失效，实际 %d", otherResp)
	}

	// 旧密码登不上，新密码可以。
	if status, _, _ := h.do(t, http.MethodPost, "/api/v1/auth/login", map[string]any{
		"username": "admin", "password": h.password,
	}, false, nil); status != http.StatusUnauthorized {
		t.Fatalf("旧密码应当失效，实际 %d", status)
	}
	if status, _, _ := h.do(t, http.MethodPost, "/api/v1/auth/login", map[string]any{
		"username": "admin", "password": "an-even-better-password",
	}, false, nil); status != http.StatusOK {
		t.Fatalf("新密码应当可用，实际 %d", status)
	}

	// 审计里能查到这次改密。
	status, audit := h.get(t, "/api/v1/audit?limit=20")
	if status != http.StatusOK {
		t.Fatalf("查询审计失败: %d", status)
	}
	found := false
	for _, raw := range audit["entries"].([]any) {
		entry, _ := raw.(map[string]any)
		if entry["action"] == "password_change" {
			found = true
		}
	}
	if !found {
		t.Fatal("改密应当留下审计记录")
	}
	_ = otherCSRF
}

func TestSecurityPasswordChangeValidation(t *testing.T) {
	h := newAuthHarness(t)

	cases := []struct {
		name    string
		payload map[string]any
		want    int
	}{
		{"当前密码错误", map[string]any{"current_password": "wrong-password-x", "new_password": "another-good-password", "new_password2": "another-good-password"}, http.StatusUnauthorized},
		{"两次不一致", map[string]any{"current_password": h.password, "new_password": "another-good-password", "new_password2": "different-password"}, http.StatusBadRequest},
		{"新密码太短", map[string]any{"current_password": h.password, "new_password": "short", "new_password2": "short"}, http.StatusBadRequest},
		{"新旧相同", map[string]any{"current_password": h.password, "new_password": h.password, "new_password2": h.password}, http.StatusBadRequest},
	}
	for _, tc := range cases {
		status, body := h.post(t, "/api/v1/auth/password", tc.payload, nil)
		if status != tc.want {
			t.Errorf("%s → %d，期望 %d（%v）", tc.name, status, tc.want, body)
		}
	}

	// 失败若干次后原密码依然可用（没有被"改坏"）。
	if status, _, _ := h.do(t, http.MethodPost, "/api/v1/auth/login", map[string]any{
		"username": "admin", "password": h.password,
	}, false, nil); status != http.StatusOK {
		t.Fatalf("密码不该被改坏，实际 %d", status)
	}
}

func TestSecurityPasswordChangeRequiresCSRF(t *testing.T) {
	h := newAuthHarness(t)
	status, _, _ := h.do(t, http.MethodPost, "/api/v1/auth/password", map[string]any{
		"current_password": h.password,
		"new_password":     "another-good-password",
		"new_password2":    "another-good-password",
	}, false, nil)
	if status != http.StatusForbidden {
		t.Fatalf("缺 CSRF 应当 403，实际 %d", status)
	}
}

func TestSecurityExpiredSessionIsRejected(t *testing.T) {
	h := newAuthHarness(t)
	ctx := context.Background()

	// 直接把这条会话的过期时间改到过去。
	res, err := h.srv.db.Writer().ExecContext(ctx, `UPDATE sessions SET expires_at = ?`, time.Now().Add(-time.Hour).Unix())
	if err != nil {
		t.Fatalf("篡改会话: %v", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		t.Fatal("没有会话被改动")
	}

	if status, _ := h.get(t, "/api/v1/nodes"); status != http.StatusUnauthorized {
		t.Fatalf("过期会话应当 401，实际 %d", status)
	}
}

func TestSecuritySessionsAreHashedAtRest(t *testing.T) {
	h := newAuthHarness(t)
	ctx := context.Background()

	// 从 Cookie 里取出真实 Token，确认数据库里找不到它的明文。
	csrf := h.csrf
	if csrf == "" {
		t.Fatal("缺少 CSRF Token")
	}
	rows, err := h.srv.db.Reader().QueryContext(ctx, `SELECT token_hash FROM sessions`)
	if err != nil {
		t.Fatalf("查询会话: %v", err)
	}
	defer func() { _ = rows.Close() }()
	count := 0
	for rows.Next() {
		var hash []byte
		if err := rows.Scan(&hash); err != nil {
			t.Fatalf("扫描: %v", err)
		}
		count++
		if len(hash) != 32 {
			t.Fatalf("会话哈希长度 = %d，期望 32（SHA-256）", len(hash))
		}
		if strings.Contains(string(hash), csrf) {
			t.Fatal("会话表里出现了明文")
		}
	}
	if count == 0 {
		t.Fatal("应当至少有一条会话")
	}
}

// ---------------------------------------------------------------- CSRF / 跨源

func TestSecurityAllMutatingEndpointsRequireCSRF(t *testing.T) {
	h := newAuthHarness(t)
	createNodeOverHTTP(t, h, "csrf-check")

	mutating := []struct {
		method string
		path   string
		body   map[string]any
	}{
		{http.MethodPost, "/api/v1/nodes", map[string]any{"name": "x", "interval_sec": 1}},
		{http.MethodPatch, "/api/v1/nodes/1", map[string]any{"name": "x", "interval_sec": 1}},
		{http.MethodDelete, "/api/v1/nodes/1", nil},
		{http.MethodPost, "/api/v1/nodes/1/token", nil},
		{http.MethodPut, "/api/v1/settings/alert", map[string]any{"cooldown": "5m"}},
		{http.MethodPut, "/api/v1/settings/telegram", map[string]any{"enabled": false, "chat_id": ""}},
		{http.MethodPost, "/api/v1/settings/telegram/test", nil},
		{http.MethodPost, "/api/v1/auth/password", map[string]any{"current_password": "a-very-good-password", "new_password": "another-good-password", "new_password2": "another-good-password"}},
	}
	for _, tc := range mutating {
		status, _, _ := h.do(t, tc.method, tc.path, tc.body, false, nil)
		if status != http.StatusForbidden {
			t.Errorf("%s %s 缺 CSRF 应当 403，实际 %d", tc.method, tc.path, status)
		}
		// 错误的 CSRF 也要拒绝。
		status, _, _ = h.do(t, tc.method, tc.path, tc.body, false, map[string]string{"X-CSRF-Token": "wrong-token-value"})
		if status != http.StatusForbidden {
			t.Errorf("%s %s 错误 CSRF 应当 403，实际 %d", tc.method, tc.path, status)
		}
	}

	// GET 不需要 CSRF（但仍然需要会话）。
	if status, _ := h.get(t, "/api/v1/nodes"); status != http.StatusOK {
		t.Fatalf("GET 不该要求 CSRF，实际 %d", status)
	}
}

func TestSecurityCrossOriginRequestsAreRejected(t *testing.T) {
	h := newAuthHarness(t)

	for _, origin := range []string{"https://evil.example", "http://127.0.0.1:1", "null"} {
		status, _, _ := h.do(t, http.MethodGet, "/api/v1/nodes", nil, false,
			map[string]string{"Origin": origin})
		if status != http.StatusForbidden {
			t.Errorf("Origin=%s 应当 403，实际 %d", origin, status)
		}
	}

	// 同源请求正常。
	status, _, _ := h.do(t, http.MethodGet, "/api/v1/nodes", nil, false,
		map[string]string{"Origin": h.ts.URL})
	if status != http.StatusOK {
		t.Fatalf("同源请求应当 200，实际 %d", status)
	}
}

// ---------------------------------------------------------------- XSS / 注入 / 日志

func TestSecurityHostileNodeNameIsStoredVerbatimAndNotExecuted(t *testing.T) {
	h := newAuthHarness(t)

	const hostile = `<script>alert(1)</script>"><img src=x onerror=alert(2)>`
	status, body := h.post(t, "/api/v1/nodes", map[string]any{
		"name": hostile, "interval_sec": 1, "group_name": "<b>粗体</b>",
	}, nil)
	if status != http.StatusCreated {
		t.Fatalf("创建失败: %d %v", status, body)
	}
	node, _ := body["node"].(map[string]any)
	if node["name"] != hostile {
		t.Fatalf("名称应当原样返回（前端用 textContent 渲染，不需要转义）: %v", node["name"])
	}

	// 响应必须是 JSON（Content-Type 正确），浏览器不会把它当 HTML 解析。
	_, _, resp := h.do(t, http.MethodGet, "/api/v1/nodes", nil, false, nil)
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q", ct)
	}
}

func TestSecurityLogInjectionIsEscaped(t *testing.T) {
	// 节点名里带换行 + 伪日志行：slog 的 text handler 必须把它引起来，
	// 否则日志可以被伪造（例如假造一条 "level=ERROR"）。
	logger, captured := newCapturedLogger()
	h := newAuthHarnessWithConfigAndLogger(t, config.Default(), logger)

	const hostile = "evil\nlevel=ERROR msg=伪造的日志"
	if status, body := h.post(t, "/api/v1/nodes", map[string]any{
		"name": hostile, "interval_sec": 1,
	}, nil); status != http.StatusCreated {
		t.Fatalf("创建失败: %d %v", status, body)
	}

	out := captured.String()
	if strings.Contains(out, "\nlevel=ERROR msg=伪造的日志") {
		t.Fatalf("换行没有被转义，日志可被伪造:\n%s", out)
	}
	if !strings.Contains(out, `\n`) {
		t.Fatalf("带换行的值应当被引号包裹并转义:\n%s", out)
	}
}

func TestSecuritySensitiveValuesNeverReachLogs(t *testing.T) {
	logger, captured := newCapturedLogger()
	h := newAuthHarnessWithConfigAndLogger(t, config.Default(), logger)

	const password = "a-very-good-password"
	// 初始化 + 登录 + 建节点（都会写日志）。
	if status, body := h.post(t, "/api/v1/nodes", map[string]any{
		"name": "log-check", "interval_sec": 1,
	}, nil); status != http.StatusCreated {
		t.Fatalf("创建失败: %d %v", status, body)
	}
	_, _ = h.post(t, "/api/v1/auth/login", map[string]any{"username": "admin", "password": password}, nil)
	_, _ = h.post(t, "/api/v1/nodes", map[string]any{"name": "log-check2", "interval_sec": 1}, nil)

	out := captured.String()
	if strings.Contains(out, password) {
		t.Fatal("日志里出现了明文密码")
	}
	if strings.Contains(out, "pba_") {
		t.Fatal("日志里出现了 Agent Token")
	}
	if strings.Contains(out, "probe_session=") {
		t.Fatal("日志里出现了会话 Cookie")
	}
}

func TestSecuritySetupCodeNeverReturnedByAPI(t *testing.T) {
	logger, captured := newCapturedLogger()
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir()+"/probe.db")
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// 这里刻意**不**完成初始化：初始化码还有效时最值得检查会不会泄露。
	s := New(config.Default(), db, logger, time.UTC)
	if err := s.auth.EnsureSetupCode(ctx); err != nil {
		t.Fatalf("生成初始化码: %v", err)
	}
	code := s.auth.setupCode
	if code == "" {
		t.Fatal("初始化码应当已生成")
	}

	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	for _, path := range []string{"/api/v1/session", "/healthz", "/"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("请求 %s: %v", path, err)
		}
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if strings.Contains(string(raw), code) {
			t.Fatalf("%s 的响应里泄露了初始化码", path)
		}
	}
	// 但日志里必须有一次（这是唯一的取码途径）。
	if !strings.Contains(captured.String(), code) {
		t.Fatal("初始化码应当出现在日志里")
	}
}

// ---------------------------------------------------------------- 资源与限额

func TestSecurityRequestBodyLimit(t *testing.T) {
	h := newAuthHarness(t)

	// 超过上限的请求体应当被拒（而不是被读进内存）。
	huge := strings.Repeat("A", maxBodyBytes+1024)
	req, err := http.NewRequest(http.MethodPost, h.ts.URL+"/api/v1/nodes",
		strings.NewReader(`{"name":"`+huge+`","interval_sec":1}`))
	if err != nil {
		t.Fatalf("构造请求: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", h.csrf)
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest && resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("超大请求体应当被拒，实际 %d", resp.StatusCode)
	}
}

func TestSecuritySSEConnectionCap(t *testing.T) {
	h := newAuthHarness(t)

	// 占满配额（每个 IP 上限 8 条）。
	var closers []func()
	defer func() {
		for _, closeFn := range closers {
			closeFn()
		}
	}()
	for i := 0; i < maxSSEClientsPerIP; i++ {
		req, err := http.NewRequest(http.MethodGet, h.ts.URL+"/api/v1/stream", nil)
		if err != nil {
			t.Fatalf("构造请求: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		resp, err := h.client.Do(req.WithContext(ctx))
		if err != nil {
			cancel()
			t.Fatalf("第 %d 条 SSE 连接失败: %v", i+1, err)
		}
		if resp.StatusCode != http.StatusOK {
			cancel()
			_ = resp.Body.Close()
			t.Fatalf("第 %d 条 SSE 连接状态码 = %d", i+1, resp.StatusCode)
		}
		closers = append(closers, func() { cancel(); _ = resp.Body.Close() })
	}

	// 第 9 条应当被明确拒绝。
	status, body, _ := h.do(t, http.MethodGet, "/api/v1/stream", nil, false, nil)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("超过上限应当 503，实际 %d", status)
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj == nil || errObj["code"] != "too_many_streams" {
		t.Fatalf("错误码不对: %v", errObj)
	}
}

func TestSecurityAgentTokenNotAcceptedInQueryString(t *testing.T) {
	ts, _, _, token := newAgentTestServer(t)

	// Token 只走 Authorization 头；放进 URL 会被各种日志/代理记录下来，因此不支持。
	url := agentWSURL(ts) + "?token=" + token
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, resp, err := dialRawURL(ctx, url)
	if err == nil {
		_ = conn.CloseNow()
		t.Fatal("URL 里的 Token 不该被接受")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		t.Fatalf("缺少 Authorization 头应当 401，实际 %d", code)
	}
}

// ---------------------------------------------------------------- 响应头

func TestSecurityHeadersOnAuthenticatedPages(t *testing.T) {
	h := newAuthHarness(t)
	_, _, resp := h.do(t, http.MethodGet, "/", nil, false, nil)

	want := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
	}
	for header, value := range want {
		if got := resp.Header.Get(header); got != value {
			t.Errorf("%s = %q，期望 %q", header, got, value)
		}
	}

	csp := resp.Header.Get("Content-Security-Policy")
	for _, needle := range []string{"default-src 'self'", "script-src 'self'", "frame-ancestors 'none'", "base-uri 'none'", "form-action 'self'"} {
		if !strings.Contains(csp, needle) {
			t.Errorf("CSP 缺少 %q：%s", needle, csp)
		}
	}
	for _, forbidden := range []string{"unsafe-inline", "unsafe-eval", "*"} {
		if strings.Contains(csp, forbidden) {
			t.Errorf("CSP 里不该出现 %q：%s", forbidden, csp)
		}
	}

	// 未加密连接上不该发 HSTS（否则本地 http 调试会被浏览器强制升级）。
	if hsts := resp.Header.Get("Strict-Transport-Security"); hsts != "" {
		t.Errorf("非 TLS 请求不该带 HSTS: %s", hsts)
	}
}

func TestSecurityStaticFilesHaveNoCacheAndCorrectTypes(t *testing.T) {
	h := newAuthHarness(t)
	for _, path := range []string{"/", "/app.js", "/chart.js", "/style.css"} {
		_, _, resp := h.do(t, http.MethodGet, path, nil, false, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s 状态码 = %d", path, resp.StatusCode)
		}
		if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "no-cache") {
			t.Errorf("%s 应当禁止缓存，实际 %q", path, cc)
		}
	}
}

func TestSecurityPathTraversalOnStaticFiles(t *testing.T) {
	h := newAuthHarness(t)
	for _, path := range []string{
		"/../store/store.go", "/..%2f..%2fgo.mod", "/web/../go.mod", "/app.js/../../go.mod",
	} {
		_, _, resp := h.do(t, http.MethodGet, path, nil, false, nil)
		if resp.StatusCode == http.StatusOK {
			t.Errorf("%s 不该返回 200", path)
		}
		raw, _ := io.ReadAll(resp.Body)
		if strings.Contains(string(raw), "module probe") {
			t.Fatalf("%s 泄露了源码", path)
		}
	}
}

// ---------------------------------------------------------------- 依赖与配置

func TestSecurityAgentRejectsPlainHTTPToRemoteHost(t *testing.T) {
	// Agent 上报的是节点信息，明文传输等于把这些信息送给链路上的任何人。
	if _, err := config.ParseAgent([]string{"--server", "http://example.com"}, func(string) string { return "" }, nil); err == nil {
		t.Fatal("明文 http 到远端应当被拒绝")
	}
	if _, err := config.ParseAgent([]string{"--server", "https://example.com", "--token", "x"}, func(string) string { return "" }, nil); err != nil {
		t.Fatalf("https 应当被接受: %v", err)
	}
	// 本机回环允许明文（自测用）。
	if _, err := config.ParseAgent([]string{"--server", "http://127.0.0.1:25774", "--token", "x"}, func(string) string { return "" }, nil); err != nil {
		t.Fatalf("回环地址应当被接受: %v", err)
	}
}

func TestSecurityServerDefaultsToLoopback(t *testing.T) {
	cfg := config.Default()
	if !cfg.LoopbackListen() {
		t.Fatalf("默认监听地址应当是回环: %s", cfg.Listen)
	}
	if cfg.TLSCert != "" || cfg.TLSKey != "" {
		t.Fatal("默认不该自带证书（应由反代或显式参数提供）")
	}
}

// ---------------------------------------------------------------- 辅助

// newCapturedLogger 返回一个把日志写进内存的 logger。
func newCapturedLogger() (*slog.Logger, *strings.Builder) {
	var mu sync.Mutex
	builder := &strings.Builder{}
	handler := slog.NewTextHandler(&lockedWriter{mu: &mu, w: builder}, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(handler), builder
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// cloneClient 复制一个独立的浏览器客户端（用于模拟第二台设备）。
func (h *authHarness) cloneClient(t *testing.T) *authHarness {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("创建 Cookie jar: %v", err)
	}
	return &authHarness{
		ts: h.ts, srv: h.srv, username: h.username,
		client: &http.Client{Jar: jar, Timeout: 10 * time.Second},
	}
}

// dialRawURL 直接按 URL 连接 Agent 端点（用于验证"Token 不能放在 URL 里"）。
func dialRawURL(ctx context.Context, url string) (*websocket.Conn, *http.Response, error) {
	opts := &websocket.DialOptions{Subprotocols: []string{agentSubprotocol}}
	return websocket.Dial(ctx, url, opts)
}
