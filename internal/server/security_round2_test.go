package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"probe/internal/config"
	"probe/internal/protocol"
	"probe/internal/store"
)

// 这个文件是第二轮安全审计（SECURITY-AUDIT-ROUND2.md）里属于
// 认证 / 会话 / 限流 / 权限 / 路由 那几条 confirmed 发现的回归用例。
//
// 每条都写成"意图级"断言（"这张票据只能试有限次"、"这个来源会被锁"、
// "这条路由有闸门"），而不是钉住某个具体数字：数字改了不该让用例变红，
// 但把那条性质去掉就必须红。

// ---------------------------------------------------------------- 01-F1

// TestTwoFactorTicketDiesAfterRepeatedWrongCodes 钉住"一张票据只能错有限次"。
//
// 为什么必须有：票据按设计**不绑 IP**（手机从 WiFi 切到 4G 不该把人踢回登录页），
// 于是"这张票据能试几次"就成了第二因素扛爆破的唯一上限 —— 只靠按 IP 的限流，
// 拥有 IP 池的人换一个 IP 就是一份新预算。
//
// 断言分四段：① 不到上限时还能重试（正常人按错不该被踢出去）；
// ② 到上限那一次的错误码变成 no_2fa_ticket（前端据此回到密码那一步）；
// ③ 作废之后**连正确的码也换不到会话**（票据真的没了，不是换了个错误码）；
// ④ 重新走一次第一步就能拿到新票据（作废的是票据，不是账号）。
func TestTwoFactorTicketDiesAfterRepeatedWrongCodes(t *testing.T) {
	h := newAuthHarness(t)
	secret, _ := enableTwoFactor(t, h)
	h.anonymousClient(t)

	if status, _ := loginPasswordStep(t, h); status != http.StatusOK {
		t.Fatalf("登录第一步失败: %d", status)
	}

	// 每发一次都清掉按 IP 的登录限流（那个桶只有 5 次/分钟）：
	// 不清的话分不清这次被挡住的是"票据作废"还是"IP 限流"。
	for i := 1; i < twoFATicketMaxFailures; i++ {
		clearLoginLimit(t, h)
		status, body := finishTwoFactorLogin(t, h, "000000")
		if status != http.StatusUnauthorized {
			t.Fatalf("第 %d 次错误码应当 401，实际 %d %v", i, status, body)
		}
		if code, _ := body["error"].(map[string]any)["code"].(string); code != "bad_totp_code" {
			t.Fatalf("第 %d 次错误码 = %q，期望 bad_totp_code（还没到上限，票据不该作废）", i, code)
		}
	}

	clearLoginLimit(t, h)
	status, body := finishTwoFactorLogin(t, h, "000000")
	if status != http.StatusUnauthorized {
		t.Fatalf("到达上限的那一次应当 401，实际 %d %v", status, body)
	}
	if code, _ := body["error"].(map[string]any)["code"].(string); code != "no_2fa_ticket" {
		t.Fatalf("票据作废时的错误码 = %q，期望 no_2fa_ticket（前端据此回到密码那一步）", code)
	}

	clearLoginLimit(t, h)
	code, _ := totpCodeAt(secret, time.Now().Add(totpPeriod*time.Second))
	status, body = finishTwoFactorLogin(t, h, code)
	if status == http.StatusOK {
		t.Fatal("票据作废之后仍然用正确的码换到了会话 —— 票据没有被真正作废")
	}
	if code, _ := body["error"].(map[string]any)["code"].(string); code != "no_2fa_ticket" {
		t.Fatalf("作废后的错误码 = %q，期望 no_2fa_ticket", code)
	}

	clearLoginLimit(t, h)
	if status, body := loginPasswordStep(t, h); status != http.StatusOK {
		t.Fatalf("重新登录第一步应当成功，实际 %d %v", status, body)
	}
	clearLoginLimit(t, h)
	// 用**下一个窗口**的码（同一个窗口的码在启用时已经用掉，防重放会拒）。
	fresh, _ := totpCodeAt(secret, time.Now().Add(totpPeriod*time.Second))
	if status, body := finishTwoFactorLogin(t, h, fresh); status != http.StatusOK {
		t.Fatalf("重新拿票据之后正确的码应当能登录，实际 %d %v", status, body)
	}
}

// TestTwoFactorGlobalGateIsIndependentOfSourceIP 钉住"与来源 IP 无关的那层闸门"。
//
// 这是 01-F1 的核心：按 IP 的限流对拥有 IP 池的人无效（换 IP = 换桶）。
// 用例从**多个不同来源 IP**（走可信代理 + X-Forwarded-For）各错一次，
// 最后一次用全新 IP + **正确的码**，仍然必须 429 —— 证明这道闸门不按 IP 分。
func TestTwoFactorGlobalGateIsIndependentOfSourceIP(t *testing.T) {
	cfg := config.Default()
	cfg.TrustedProxy = "127.0.0.1/32" // 直连对端是可信代理，于是 XFF 决定来源
	h := newAuthHarnessWithConfig(t, cfg)
	if h.srv.auth.twoFAGlobal == nil {
		t.Fatal("全局闸门没被构造出来（NewAuth 里漏了）")
	}
	secret, _ := enableTwoFactor(t, h)

	// 把全局额度换成小数额：要验的是"这道闸门与 IP 无关"，
	// 不是"50 这个数字对不对"（那样要发 50 次 Argon2，跑十几秒）。
	limit := 3
	h.srv.auth.twoFAGlobal = newAttemptLimiter(limit, time.Minute, 0, 0)

	h.anonymousClient(t)
	for i := 0; i < limit; i++ {
		xff := map[string]string{"X-Forwarded-For": fmt.Sprintf("10.9.0.%d", i+1)}
		if status, body, _ := h.do(t, http.MethodPost, "/api/v1/auth/login", map[string]any{
			"username": h.username, "password": h.password,
		}, false, xff); status != http.StatusOK || body["twofa_required"] != true {
			t.Fatalf("第 %d 个来源的第一步失败: %d %v", i+1, status, body)
		}
		status, body, _ := h.do(t, http.MethodPost, "/api/v1/auth/login/2fa",
			map[string]any{"code": "000000"}, false, xff)
		if status != http.StatusUnauthorized {
			t.Fatalf("第 %d 个来源的错误码应当 401，实际 %d %v", i+1, status, body)
		}
	}

	// 全新来源 + **正确的码**：额度是全局的，所以照样 429。
	xff := map[string]string{"X-Forwarded-For": "10.9.0.200"}
	if status, body, _ := h.do(t, http.MethodPost, "/api/v1/auth/login", map[string]any{
		"username": h.username, "password": h.password,
	}, false, xff); status != http.StatusOK {
		t.Fatalf("新来源的第一步应当成功（它有自己的按 IP 预算），实际 %d %v", status, body)
	}
	code, _ := totpCodeAt(secret, time.Now().Add(totpPeriod*time.Second))
	status, body, _ := h.do(t, http.MethodPost, "/api/v1/auth/login/2fa",
		map[string]any{"code": code}, false, xff)
	if status != http.StatusTooManyRequests {
		t.Fatalf("全局额度用尽后，换一个来源 + 正确的码也应当 429，实际 %d %v", status, body)
	}
	if code, _ := body["error"].(map[string]any)["code"].(string); code != "too_many_attempts" {
		t.Fatalf("错误码 = %q，期望 too_many_attempts", code)
	}
}

// TestTwoFactorCrossIPUseIsRecorded 钉住"票据不绑 IP，但跨来源使用会留痕"。
//
// 不绑 IP 是**有意**的（手机切网不该被踢出去），所以这里断言的不是"拒绝"，
// 而是"这件事在日志里看得出来"。
func TestTwoFactorCrossIPUseIsRecorded(t *testing.T) {
	cfg := config.Default()
	cfg.TrustedProxy = "127.0.0.1/32"
	logs := &twoFALogCapture{} // 通用日志捕获（定义在 twofa_test.go）
	h := newAuthHarnessWithConfigAndLogger(t, cfg, slog.New(logs))
	secret, _ := enableTwoFactor(t, h)

	h.anonymousClient(t)
	if status, _, _ := h.do(t, http.MethodPost, "/api/v1/auth/login", map[string]any{
		"username": h.username, "password": h.password,
	}, false, map[string]string{"X-Forwarded-For": "10.1.1.1"}); status != http.StatusOK {
		t.Fatalf("登录第一步失败: %d", status)
	}
	code, _ := totpCodeAt(secret, time.Now().Add(totpPeriod*time.Second))
	status, body, _ := h.do(t, http.MethodPost, "/api/v1/auth/login/2fa",
		map[string]any{"code": code}, false, map[string]string{"X-Forwarded-For": "10.2.2.2"})
	if status != http.StatusOK {
		t.Fatalf("切网之后第二步应当照常成功（票据不绑 IP），实际 %d %v", status, body)
	}
	if line := logs.all(); !strings.Contains(line, "另一个来源 IP") || !strings.Contains(line, "issued_ip=10.1.1.1") {
		t.Fatalf("跨来源使用票据应当留下 Warn 记录，实际日志：\n%s", line)
	}
}

// ---------------------------------------------------------------- 01-F2

// TestDegradedRateLimitIsWarned 钉住"限流退化成全局单桶时必须在启动日志里说出来"。
//
// 01-F2 的修法是**只告警不改语义**：把整桶硬锁 15 分钟换成指数退避会让爆破成本
// 从"每 IP 约 10 次/17 分钟"变成"每 30 秒一次"，那是拿第二因素的抗爆破能力去换
// 可用性（见交付说明）。所以这里钉住的是"运维能被明确告知"这件事。
func TestDegradedRateLimitIsWarned(t *testing.T) {
	// 监听对本机之外开放 + 没配可信代理 ⇒ 所有请求的来源 IP 都是同一个地址。
	logs := &twoFALogCapture{}
	cfg := config.Default()
	cfg.Listen = "0.0.0.0:25774"
	s := New(cfg, nil, slog.New(logs), time.UTC)
	s.warnRateLimitDegradation()
	if line := logs.all(); !strings.Contains(line, "共用一个桶") {
		t.Fatalf("限流退化时应当有一条启动告警，实际日志：\n%s", line)
	}

	// 配好了可信代理（推荐部署的默认值）：不该报警。
	logs = &twoFALogCapture{}
	cfg.TrustedProxy = "127.0.0.1/32"
	s = New(cfg, nil, slog.New(logs), time.UTC)
	s.warnRateLimitDegradation()
	if line := logs.all(); strings.Contains(line, "共用一个桶") {
		t.Fatalf("配好 --trusted-proxy 之后不该再报限流退化：\n%s", line)
	}

	// 默认配置（只听本机、没有反代）：不该报警 —— 否则默认启动就有一条噪音。
	logs = &twoFALogCapture{}
	s = New(config.Default(), nil, slog.New(logs), time.UTC)
	s.warnRateLimitDegradation()
	if line := logs.all(); strings.Contains(line, "共用一个桶") {
		t.Fatalf("默认配置（只监听 127.0.0.1）不该报限流退化：\n%s", line)
	}
}

// ---------------------------------------------------------------- 01-F3

// TestOpenRoutesAreRateLimited 钉住 accessOpen 那两条"每次请求都要打库"的路由有闸门。
//
// /api/v1/session 与 /auth/logout 不需要任何凭据就能触发，而它们每次请求至少
// 一次库操作（会话查询 / 设置读 / 删会话），/auth/logout 更会在**唯一那条
// 写连接**上删一次会话。断言的是"超过额度就 429"，不是某个具体数字。
func TestOpenRoutesAreRateLimited(t *testing.T) {
	h := newAuthHarness(t)
	if h.srv.auth.openReads == nil {
		t.Fatal("openReads 限流器没被构造出来（NewAuth 里漏了）")
	}
	// 换成很小的额度：要验的是"有没有闸门"，不是"300 这个数字"。
	h.srv.auth.openReads = newAttemptLimiter(3, time.Minute, 0, 0)

	for _, tc := range []struct {
		name   string
		method string
		path   string
	}{
		{"session", http.MethodGet, "/api/v1/session"},
		{"logout", http.MethodPost, "/api/v1/auth/logout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h.srv.auth.openReads.succeed("127.0.0.1")
			var got429 bool
			for i := 0; i < 5; i++ {
				status, _, _ := h.do(t, tc.method, tc.path, nil, false, nil)
				if status == http.StatusTooManyRequests {
					got429 = true
					break
				}
			}
			if !got429 {
				t.Fatalf("%s %s 连打 5 次都没有被限流挡住 —— 这条路由没有闸门", tc.method, tc.path)
			}
		})
	}
}

// TestHealthzAnonymousProbeIsCached 钉住"匿名探活不会每个请求都打一次库"。
//
// /healthz **故意不限流**（探针收到 429 会当成服务不健康），所以只能靠缓存：
// 库关掉之后的一小段时间里匿名探活仍然回上一次的结果，过了缓存时长才如实报 503。
func TestHealthzAnonymousProbeIsCached(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "probe.db"))
	if err != nil {
		t.Fatalf("打开测试数据库: %v", err)
	}
	s := New(config.Default(), db, slog.New(slog.DiscardHandler), time.UTC)

	if rec := get(t, s, "/healthz"); rec.Code != http.StatusOK {
		t.Fatalf("库正常时匿名探活应当 200，实际 %d", rec.Code)
	}
	// 关掉库：紧接着的那次探活必须仍然拿到缓存里的结果（没有再去打库）。
	if err := db.Close(); err != nil {
		t.Fatalf("关闭数据库: %v", err)
	}
	if rec := get(t, s, "/healthz"); rec.Code != http.StatusOK {
		t.Fatalf("缓存期内的匿名探活不该重新打库（应当仍是 200），实际 %d", rec.Code)
	}
	// 缓存过期之后必须如实报告。
	time.Sleep(healthzDBCacheTTL + 200*time.Millisecond)
	if rec := get(t, s, "/healthz"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("缓存过期后库不可用应当 503，实际 %d", rec.Code)
	}
}

// ---------------------------------------------------------------- 01-F4

// TestHSTSBehindTLSProxyIsExplicitOptIn 钉住反代场景下的 HSTS 语义。
//
// 事实：推荐部署（Caddy/nginx 终止 TLS）下进程内 TLSCert 恒为空，所以以前
// HSTS 永远不会出现在响应里，而同一个响应里的会话 Cookie 却带 Secure
// （cookieSecure 认 X-Forwarded-Proto）—— 同一个判断在两处给出不同结果。
//
// 修法是**显式开关**：默认关（一年期 includeSubDomains 是不可撤销的，
// 反代那边 http 没有 301 时会让面板打不开），打开后与 Cookie 的 Secure 同源。
func TestHSTSBehindTLSProxyIsExplicitOptIn(t *testing.T) {
	// ① 默认关：反代声明 https 也不下发（不偷偷改变浏览器行为）。
	off := newAuthHarnessWithConfig(t, proxyHTTPSConfig(false))
	_, _, resp := off.do(t, http.MethodGet, "/api/v1/session", nil, false,
		map[string]string{"X-Forwarded-Proto": "https"})
	if hsts := resp.Header.Get("Strict-Transport-Security"); hsts != "" {
		t.Fatalf("默认配置下不该下发 HSTS（会被浏览器锁成一年 https），实际 %q", hsts)
	}

	// ② 打开且反代说 https：必须下发，而且与 Cookie 的 Secure 口径一致。
	on := newAuthHarnessWithConfig(t, proxyHTTPSConfig(true))
	status, _, resp := on.do(t, http.MethodPost, "/api/v1/auth/login", map[string]any{
		"username": "admin", "password": on.password,
	}, false, map[string]string{"X-Forwarded-Proto": "https"})
	if status != http.StatusOK {
		t.Fatalf("登录失败: %d", status)
	}
	if hsts := resp.Header.Get("Strict-Transport-Security"); hsts == "" {
		t.Fatal("--hsts 打开且反代声明 https 时应当下发 HSTS")
	} else if hsts != hstsValue {
		t.Fatalf("HSTS 取值 = %q，期望 %q", hsts, hstsValue)
	}
	if cookie := sessionSetCookie(resp); !strings.Contains(cookie, "Secure") {
		t.Fatalf("同一个响应里 Cookie 的 Secure 与 HSTS 必须同源，实际 Cookie=%q", cookie)
	}

	// ③ 没有证据表明是 https（反代没发 X-Forwarded-Proto）时保守：不下发。
	_, _, resp = on.do(t, http.MethodGet, "/api/v1/session", nil, false, nil)
	if hsts := resp.Header.Get("Strict-Transport-Security"); hsts != "" {
		t.Fatalf("没有证据表明走的是 https 时不该下发 HSTS，实际 %q", hsts)
	}

	// ④ 非可信来源伪造 X-Forwarded-Proto 无效（开关开着也不认）。
	untrustedCfg := config.Default()
	untrustedCfg.HSTS = true
	untrusted := newAuthHarnessWithConfig(t, untrustedCfg)
	_, _, resp = untrusted.do(t, http.MethodGet, "/api/v1/session", nil, false,
		map[string]string{"X-Forwarded-Proto": "https"})
	if hsts := resp.Header.Get("Strict-Transport-Security"); hsts != "" {
		t.Fatalf("非可信来源的 X-Forwarded-Proto 不该换来 HSTS，实际 %q", hsts)
	}
}

// proxyHTTPSConfig 是"反代在 127.0.0.1 上终止 TLS"的配置。
func proxyHTTPSConfig(hsts bool) config.Server {
	cfg := config.Default()
	cfg.TrustedProxy = "127.0.0.0/8"
	cfg.HSTS = hsts
	return cfg
}

// ---------------------------------------------------------------- 03-A-2

// TestAgentControlFramesAreRateLimited 钉住"控制帧有自己的配额，而且有上限"。
//
// 两条性质一起验：
//   - 控制帧**不能**吃掉上报配额（否则心跳稍密就开始丢真实数据）；
//   - 控制帧**也不能**完全没有上限（以前一个 ping 回一个 pong、未知类型同样
//     即时回帧，取到 Token 的人可以用 20 条连接把 CPU 与出网带宽吃满）。
func TestAgentControlFramesAreRateLimited(t *testing.T) {
	ts, s, node, token := newAgentTestServer(t)
	s.agents.msgPerSecond = 2
	s.agents.rateWindow = time.Hour // 窗口拉长，断言与机器负载无关

	conn := mustDialAgent(t, ts, token)
	sendFrame(t, conn, helloFrame(t, testHello()))
	readHandshake(t, conn)

	// 用一个后台读协程收帧，而不是"读超时 = 没有回包"：coder/websocket 的 Read
	// 一旦出错就会把整条连接关掉（read.go 的 finishRead），用读超时判断
	// "这条 ping 被丢了"会把后面的断言一起弄坏。
	type frameOrErr struct {
		env protocol.Envelope
		err error
	}
	frames := make(chan frameOrErr, 128)
	go func() {
		for {
			_, data, err := conn.Read(context.Background())
			if err != nil {
				frames <- frameOrErr{err: err}
				return
			}
			env, err := protocol.Decode(data)
			frames <- frameOrErr{env: env, err: err}
		}
	}()

	limit := s.agents.msgPerSecond * agentCtlPerSecondFactor
	for i := 0; i < limit+3; i++ {
		ping, err := protocol.New(protocol.TypePing, protocol.Ping{TsUS: int64(i)})
		if err != nil {
			t.Fatalf("构造 ping: %v", err)
		}
		sendFrame(t, conn, ping)
	}

	// 收 1 秒：配额内的每一条 ping 都有一个 pong，超出的必须**没有**回包
	// （超速是静默丢弃，不回错误帧 —— 否则回包通道本身就成了放大器）。
	pongs := 0
	deadline := time.After(time.Second)
collect:
	for {
		select {
		case f := <-frames:
			if f.err != nil {
				t.Fatalf("读帧失败: %v", f.err)
			}
			if f.env.T == protocol.TypePong {
				pongs++
			}
		case <-deadline:
			break collect
		}
	}
	if pongs != limit {
		t.Fatalf("回包数 = %d，期望 %d（配额内的要给，超出的要静默丢弃）", pongs, limit)
	}

	// 控制帧刷屏之后，上报配额必须完好无损。
	frame, err := protocol.New(protocol.TypeMetrics, testMetrics())
	if err != nil {
		t.Fatalf("构造指标帧: %v", err)
	}
	frame.Seq = 1
	sendFrame(t, conn, frame)
	waitFor(t, 3*time.Second, "控制帧刷屏之后指标仍然入库", func() bool {
		n, ok := s.State().Get(node.ID)
		return ok && n.Seq > 0
	})
}

// TestAgentUnknownTypesEventuallyClose 钉住"未知类型不再是无限回帧通道"。
//
// 以前未知类型来一帧回一帧、且不计入 badFrames，于是它是一条 1:1 的回包通道，
// 永远不会断。现在它计入连续不合法帧（与"畸形帧"同一条规则），
// 连续 agentMaxBadFrames 帧即断开。
func TestAgentUnknownTypesEventuallyClose(t *testing.T) {
	ts, _, _, token := newAgentTestServer(t)
	conn := mustDialAgent(t, ts, token)
	sendFrame(t, conn, helloFrame(t, testHello()))
	readHandshake(t, conn)

	for i := 0; i < agentMaxBadFrames+2; i++ {
		sendRaw(t, conn, []byte(`{"v":1,"t":"future_type","d":{}}`))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var closed error
	for {
		if _, _, err := conn.Read(ctx); err != nil {
			closed = err
			break
		}
	}
	if status := websocket.CloseStatus(closed); status != websocket.StatusCode(protocol.CloseBadRequest) {
		t.Fatalf("连续未知类型帧之后的关闭码 = %v（err=%v），期望 %d",
			status, closed, protocol.CloseBadRequest)
	}
}

// ---------------------------------------------------------------- 03-A-3 / 02-1

// TestAgentAuthFailuresLockTheSource 钉住"Agent 鉴权失败路径有闸门"。
//
// 这条路径匿名可达（accessOpen + 空 Origin 放行），没有闸门时一个循环 GET 就能
// 让服务端每个请求打一次库、写两条日志（访问日志 + Warn）。
// 断言：连续失败会被按来源锁住（429 + Retry-After），而不是永远 401。
func TestAgentAuthFailuresLockTheSource(t *testing.T) {
	ts, s, _, token := newAgentTestServer(t)
	if s.agents.authFails == nil {
		t.Fatal("authFails 限流器没被构造出来（NewAgents 里漏了）")
	}

	for i := 0; i < agentAuthFailLockAfter; i++ {
		_, resp, err := dialAgent(t, ts, "pba_not_a_real_token")
		if err == nil {
			t.Fatal("错误 Token 不该握手成功")
		}
		if resp == nil || resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("第 %d 次失败应当 401，实际 %v", i+1, resp)
		}
	}

	_, resp, err := dialAgent(t, ts, "pba_not_a_real_token")
	if err == nil {
		t.Fatal("被锁的来源不该握手成功")
	}
	code := 0
	if resp != nil {
		code = resp.StatusCode
	}
	if code != http.StatusTooManyRequests {
		t.Fatalf("连续鉴权失败之后应当 429，实际 %d", code)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("429 应当带 Retry-After，客户端据此决定重试时间")
	}

	// 反向确认：锁的是"这个来源的失败计数"，不是"Token 坏了" ——
	// 清零之后立刻恢复（正确 Token 能连上，不再 429）。
	s.agents.authFails.succeed("127.0.0.1")
	conn, resp, err := dialAgent(t, ts, token)
	if err != nil {
		t.Fatalf("清零之后正确的 Token 应当能握手: %v", err)
	}
	_ = conn.CloseNow()
	if resp != nil && resp.StatusCode == http.StatusTooManyRequests {
		t.Fatal("清零之后不该还被锁着")
	}
}

// TestAgentAuthFailureLogsAreThrottled 钉住"失败日志不会一条请求一条"。
//
// 闸门挡住了库查询与响应，日志如果还是每请求一条，磁盘照样能被刷爆 ——
// 那正是这条发现被报出来的形态。
func TestAgentAuthFailureLogsAreThrottled(t *testing.T) {
	ts, s, _, _ := newAgentTestServer(t)
	logs := &twoFALogCapture{} // 通用日志捕获（定义在 twofa_test.go）
	s.agents.log = slog.New(logs)

	for i := 0; i < agentAuthFailLockAfter; i++ {
		if _, _, err := dialAgent(t, ts, "pba_not_a_real_token"); err == nil {
			t.Fatal("错误 Token 不该握手成功")
		}
	}
	lines := strings.Count(logs.all(), "Agent 鉴权失败")
	if lines == 0 {
		t.Fatal("一次都没有记录鉴权失败：攻击就完全不可见了")
	}
	if lines >= agentAuthFailLockAfter {
		t.Fatalf("%d 次失败写了 %d 条日志 —— 日志没有节流", agentAuthFailLockAfter, lines)
	}
}

// ---------------------------------------------------------------- 03-A-4

// TestAgentSeqGapIsClamped 钉住"服务端自己算的 gap 有上限"。
//
// seq 完全由 Agent 自填：两帧（1、2^62）就能把"服务端观测到的丢帧数"推到
// 天文数字（还会超出 JSON 的安全整数范围），而界面上（含访客）会照原样显示。
// 这里选的是**钳制**（不是整帧拒绝）：帧里的指标本身合法，丢掉它会让曲线
// 真的出现一个洞，而这一段只该限制那个由 seq 推出来的数。
func TestAgentSeqGapIsClamped(t *testing.T) {
	ts, s, node, token := newAgentTestServer(t)
	conn := mustDialAgent(t, ts, token)
	sendFrame(t, conn, helloFrame(t, testHello()))
	readHandshake(t, conn)

	// 第一帧 seq=1：lastFrameSeq 从 0 变成 1，不产生缺口。
	first, err := protocol.New(protocol.TypeMetrics, testMetrics())
	if err != nil {
		t.Fatalf("构造帧: %v", err)
	}
	first.Seq = 1
	sendFrame(t, conn, first)
	waitFor(t, 3*time.Second, "第一帧入库", func() bool {
		n, ok := s.State().Get(node.ID)
		return ok && n.Seq > 0
	})

	// 第二帧把 seq 推到 2^62：缺口必须被夹到上限。
	huge, err := protocol.New(protocol.TypeMetrics, testMetrics())
	if err != nil {
		t.Fatalf("构造帧: %v", err)
	}
	huge.Seq = uint64(1) << 62
	sendFrame(t, conn, huge)

	deadline := time.Now().Add(3 * time.Second)
	for {
		n, ok := s.State().Get(node.ID)
		if ok && n.Gap > 0 {
			if n.Gap != maxSeqGapPerFrame {
				t.Fatalf("单帧缺口 = %d，期望被钳制到 %d（seq 由 Agent 自填，不能直接信）",
					n.Gap, maxSeqGapPerFrame)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("第二帧没有被处理（缺口始终为 0）")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
