package server

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"probe/internal/config"
	"probe/internal/store"
)

// TestCookieSecureBehindProxy 覆盖推荐部署（反代终止 TLS）下 Cookie 丢 Secure 的问题。
//
// 只看 r.TLS 的话，Caddy/nginx 后面 r.TLS 恒为 nil，会话 Cookie 永远不带 Secure，
// 浏览器会把它附到同主机的 http:// 请求上，中间人可直接读走会话。
func TestCookieSecureBehindProxy(t *testing.T) {
	cfg := config.Default()
	cfg.TrustedProxy = "127.0.0.0/8" // 直连对端是可信代理
	h := newAuthHarnessWithConfig(t, cfg)

	// 反代声明了 https：Cookie 必须带 Secure。
	status, _, resp := h.do(t, http.MethodPost, "/api/v1/auth/login", map[string]any{
		"username": "admin", "password": h.password,
	}, false, map[string]string{"X-Forwarded-Proto": "https"})
	if status != http.StatusOK {
		t.Fatalf("登录失败: %d", status)
	}
	if cookie := sessionSetCookie(resp); cookie == "" {
		t.Fatal("没有下发会话 Cookie")
	} else if !strings.Contains(cookie, "Secure") {
		t.Fatalf("反代走 https 时 Cookie 应当带 Secure: %s", cookie)
	}

	// 没有 X-Forwarded-Proto（或非 https）时保持保守：不带 Secure。
	status, _, resp = h.do(t, http.MethodPost, "/api/v1/auth/login", map[string]any{
		"username": "admin", "password": h.password,
	}, false, nil)
	if status != http.StatusOK {
		t.Fatalf("登录失败: %d", status)
	}
	if cookie := sessionSetCookie(resp); strings.Contains(cookie, "Secure") {
		t.Fatalf("没有证据表明是 https 时不该带 Secure: %s", cookie)
	}

	// 不可信来源伪造 X-Forwarded-Proto 无效。
	untrusted := newAuthHarness(t) // 默认没有 trusted-proxy
	status, _, resp = untrusted.do(t, http.MethodPost, "/api/v1/auth/login", map[string]any{
		"username": "admin", "password": untrusted.password,
	}, false, map[string]string{"X-Forwarded-Proto": "https"})
	if status != http.StatusOK {
		t.Fatalf("登录失败: %d", status)
	}
	if cookie := sessionSetCookie(resp); strings.Contains(cookie, "Secure") {
		t.Fatalf("非可信来源的 X-Forwarded-Proto 不该被采信: %s", cookie)
	}
}

// TestForwardedIPOnlyTrustsTheLastHop 覆盖"只采信最近一跳"的策略：
// 客户端自己写在左边的地址一律不采信，避免限流键被伪造。
func TestForwardedIPOnlyTrustsTheLastHop(t *testing.T) {
	_, trustedNet, err := net.ParseCIDR("10.0.0.0/8")
	if err != nil {
		t.Fatalf("解析 CIDR: %v", err)
	}
	trusted := []*net.IPNet{trustedNet}

	cases := []struct {
		name string
		xff  string
		want string
	}{
		{"单条（代理追加的真实客户端）", "1.2.3.4", "1.2.3.4"},
		{"客户端伪造在前、真实地址在后", "9.9.9.9, 1.2.3.4", "1.2.3.4"},
		{"最右一条仍在可信网段 → 无法判断，退回直连地址", "1.2.3.4, 10.0.0.5", ""},
		{"全部可信", "10.1.1.1, 10.0.0.5", ""},
		{"空值", "", ""},
		{"全是垃圾", "not-an-ip", ""},
		{"末尾有空项", "1.2.3.4, ", "1.2.3.4"},
	}
	for _, tc := range cases {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if tc.xff != "" {
			r.Header.Set("X-Forwarded-For", tc.xff)
		}
		if got := forwardedIP(r, trusted); got != tc.want {
			t.Errorf("%s：forwardedIP(%q) = %q，期望 %q", tc.name, tc.xff, got, tc.want)
		}
	}

	// X-Real-IP 同样只在"不在可信网段"时才采信。
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("X-Real-IP", "10.0.0.7")
	if got := forwardedIP(r, trusted); got != "" {
		t.Errorf("可信网段内的 X-Real-IP 不该被采信，实际 %q", got)
	}
}

// TestClientIPFallsBackToRemoteWhenAllForwardedTrusted 端到端验证：
// 伪造的 XFF 不能改变服务端认定的客户端 IP（限流键与审计里的 IP）。
func TestClientIPFallsBackToRemoteWhenAllForwardedTrusted(t *testing.T) {
	cfg := config.Default()
	cfg.TrustedProxy = "127.0.0.0/8"
	h := newAuthHarnessWithConfig(t, cfg)

	status, _, _ := h.do(t, http.MethodPost, "/api/v1/auth/login", map[string]any{
		"username": "admin", "password": h.password,
	}, false, map[string]string{"X-Forwarded-For": "203.0.113.9, 127.0.0.1"})
	if status != http.StatusOK {
		t.Fatalf("登录失败: %d", status)
	}

	// 会话里记录的 IP 必须是直连地址，而不是伪造的 203.0.113.9。
	var ip string
	if err := h.srv.db.Reader().QueryRowContext(context.Background(),
		`SELECT ip FROM sessions ORDER BY created_at DESC LIMIT 1`).Scan(&ip); err != nil {
		t.Fatalf("读取会话 IP: %v", err)
	}
	if ip != "127.0.0.1" {
		t.Fatalf("会话记录的 IP = %q，期望直连地址 127.0.0.1（伪造的 XFF 不该被采信）", ip)
	}
}

// TestLimiterKeepsLockoutsWhenTableIsFull 覆盖"表满时整体清空"的问题：
// 清空等于把所有人的锁定期一起抹掉，攻击者用 IP 池就能随时解锁自己。
func TestLimiterKeepsLockoutsWhenTableIsFull(t *testing.T) {
	l := newAttemptLimiter(5, time.Minute, 3, 15*time.Minute)
	now := time.Now()

	// 先制造一个锁定。
	for i := 0; i < 3; i++ {
		l.fail("victim", now)
	}
	if ok, _ := l.allowed("victim", now); ok {
		t.Fatal("连续失败 3 次后应当被锁定")
	}

	// 把表灌满（模拟 IP 池）。
	for i := 0; i < maxLimiterEntries+50; i++ {
		key := "ip-" + strings.Repeat("0", i%5) + string(rune('a'+i%26)) + itoaSmall(i)
		l.allowed(key, now)
	}

	if ok, _ := l.allowed("victim", now); ok {
		t.Fatal("表被灌满后，原有的锁定状态不该被抹掉")
	}
	if len(l.entries) > maxLimiterEntries {
		t.Fatalf("限流表大小 = %d，必须封顶在 %d", len(l.entries), maxLimiterEntries)
	}
}

func itoaSmall(v int) string {
	if v == 0 {
		return "0"
	}
	var buf [12]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

// TestSetupIsNotRaceable 覆盖并发初始化的竞态：
// 检查"还没有管理员"与写入之间隔着约 100ms 的 Argon2，双击会让后写的覆盖先建的。
func TestSetupIsNotRaceable(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir()+"/probe.db")
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	s := New(config.Default(), db, slog.New(slog.DiscardHandler), time.UTC)
	if err := s.auth.EnsureSetupCode(ctx); err != nil {
		t.Fatalf("生成初始化码: %v", err)
	}
	code := s.auth.setupCode
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	type result struct {
		status int
		body   string
	}
	results := make([]result, 2)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			payload := `{"code":"` + code + `","username":"admin` + itoaSmall(idx) +
				`","password":"a-very-good-password"}`
			resp, err := http.Post(ts.URL+"/api/v1/setup", "application/json", strings.NewReader(payload))
			if err != nil {
				results[idx] = result{status: -1, body: err.Error()}
				return
			}
			defer func() { _ = resp.Body.Close() }()
			buf := make([]byte, 512)
			n, _ := resp.Body.Read(buf)
			results[idx] = result{status: resp.StatusCode, body: string(buf[:n])}
		}(i)
	}
	wg.Wait()

	okCount, conflictCount := 0, 0
	for _, r := range results {
		switch r.status {
		case http.StatusOK:
			okCount++
		case http.StatusConflict:
			conflictCount++
		default:
			t.Fatalf("意外的响应: %d %s", r.status, r.body)
		}
	}
	if okCount != 1 || conflictCount != 1 {
		t.Fatalf("并发初始化应当恰好一成功一冲突，实际成功 %d 冲突 %d", okCount, conflictCount)
	}

	// 数据库里只应存在一个管理员（后写者不能覆盖）。
	username, _, ok, err := db.AdminAccount(ctx)
	if err != nil || !ok {
		t.Fatalf("读取管理员: %v ok=%v", err, ok)
	}
	var count int
	if err := db.Reader().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM settings WHERE key = ?`, store.KeyAdminUsername).Scan(&count); err != nil {
		t.Fatalf("统计管理员设置: %v", err)
	}
	if count != 1 {
		t.Fatalf("管理员用户名键应当只有一行，实际 %d", count)
	}
	if username == "" {
		t.Fatal("管理员用户名为空")
	}
	// 库里的用户名必须能配合它自己的密码登录：确保没有留下"半截状态"。
	other := &authHarness{ts: ts}
	other.anonymousClient(t)
	status, body, _ := other.do(t, http.MethodPost, "/api/v1/auth/login", map[string]any{
		"username": username, "password": "a-very-good-password",
	}, false, nil)
	if status != http.StatusOK {
		t.Fatalf("数据库里的管理员应当能登录（%s）: %d %v", username, status, body)
	}
}

// TestUnicodeLengthLimits 验证长度按字符算（否则"22 个汉字的用户名"会被误判超长，
// 而"4 个汉字的密码"又能骗过"至少 10 位"）。
func TestUnicodeLengthLimits(t *testing.T) {
	// 服务端：用户名与密码
	if err := validateCredentials(strings.Repeat("测", 22), "a-very-good-password"); err != nil {
		t.Fatalf("22 个汉字的用户名应当合法: %v", err)
	}
	if err := validateCredentials("admin", strings.Repeat("密", 10)); err != nil {
		t.Fatalf("10 个汉字的密码应当合法: %v", err)
	}
	if err := validateCredentials("admin", strings.Repeat("密", 4)); err == nil {
		t.Fatal("4 个汉字的密码不该通过（只有 4 位）")
	}
	if err := validateCredentials(strings.Repeat("测", 65), "a-very-good-password"); err == nil {
		t.Fatal("65 个汉字的用户名应当被拒")
	}

	// store：节点字段
	db, err := store.Open(context.Background(), t.TempDir()+"/probe.db")
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	in := store.NewNode{
		Name: strings.Repeat("测", 64), IntervalSec: 1, TrafficWarnPct: 80, ResetDay: 1,
		TrafficLimit: 0,
	}
	if _, _, err := db.CreateNode(ctx, in, time.Now()); err != nil {
		t.Fatalf("64 个汉字的名称应当合法: %v", err)
	}
	in.Name = strings.Repeat("测", 65)
	if _, _, err := db.CreateNode(ctx, in, time.Now()); err == nil {
		t.Fatal("65 个汉字的名称应当被拒")
	}

	in.Name = "中文备注"
	in.Note = strings.Repeat("备", 200)
	if _, _, err := db.CreateNode(ctx, in, time.Now()); err != nil {
		t.Fatalf("200 个汉字的备注应当合法: %v", err)
	}
}

// TestVerifySemaphoreRespectsContext 验证排队等 Argon2 时能被取消，
// 否则客户端断开后请求仍在排队，很容易堆积 goroutine。
func TestVerifySemaphoreRespectsContext(t *testing.T) {
	a := NewAuth(nil, config.Default(), slog.New(slog.DiscardHandler), nil)

	// 占满信号量。
	a.verify <- struct{}{}
	a.verify <- struct{}{}
	defer func() { <-a.verify; <-a.verify }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	if _, err := a.hashWithLimit(ctx, "whatever-password"); err == nil {
		t.Fatal("已取消的请求不该继续排队执行哈希")
	}
	if _, err := a.verifyWithLimit(ctx, "hash", "password"); err == nil {
		t.Fatal("已取消的请求不该继续排队校验密码")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("取消应当立刻返回，实际等了 %s", elapsed)
	}
}

// TestChangePasswordRejectsUsernameAsPassword 验证改密与初始化共用同一套策略。
func TestChangePasswordRejectsUsernameAsPassword(t *testing.T) {
	h := newAuthHarness(t)
	status, body := h.post(t, "/api/v1/auth/password", map[string]any{
		"current_password": h.password,
		"new_password":     "admin", // 与用户名相同
		"new_password2":    "admin",
	}, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("把密码改成用户名应当被拒，实际 %d %v", status, body)
	}
}

func sessionSetCookie(resp *http.Response) string {
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookieName {
			found := ""
			for _, raw := range resp.Header["Set-Cookie"] {
				if strings.HasPrefix(raw, sessionCookieName+"=") {
					found = raw
					break
				}
			}
			return found
		}
	}
	return ""
}
