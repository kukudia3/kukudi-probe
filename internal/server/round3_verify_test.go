package server

// 第三轮验证（D:\DEEPSEEK\_audit\ROUND3-VERIFY-2.md）里 ④「反代形态」那一块。
//
// 背景：第二遍审计（SECURITY-AUDIT-ROUND2.md）把 01-F4（反代终止 TLS 时永不下发
// HSTS）与 01-F2（来源 IP 不可区分时"每 IP 限流"退化成全局单桶）的**未能验证**
// 都写成"没有真实反代环境（Caddy / nginx / cloudflared）"。
//
// 本机确实起不了那些反代（无 nginx / caddy / docker / podman / cloudflared，
// WSL 已装但**没有任何发行版**，见报告 ④a 的核查记录）。但这两条的判定核心
// **不是反代软件本身**，而是"到达进程的请求长什么样"：
//
//	remoteIP = 反代地址（不是真实客户端）
//	X-Forwarded-Proto = 反代声明的原始协议
//	X-Forwarded-For   = 反代追加的真实客户端
//
// 这三件事可以用 Go 标准库真做出来：httputil.ReverseProxy 是**真的**反向代理
// 实现（真 TCP 两跳、真头改写），httptest.NewTLSServer 是**真的** TLS 终止。
// 于是下面这两个用例跑的是"真反代 + 真 TLS + 真服务端"，只是反代不是 nginx。
//
// 为什么不用 httptest.NewRecorder 直接伪造头：那只能证明"函数认得这个头"，
// 证明不了"两跳链路上这些头真的会以那个形状到达"。本用例断言的是后者。

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"probe/internal/config"
	"probe/internal/store"
)

// ---------------------------------------------------------------------------
// 真反代：httputil.ReverseProxy
// ---------------------------------------------------------------------------

// frontProxyOptions 描述"一个反代是怎么配置的"。
type frontProxyOptions struct {
	// tls 为 true 时反代自己 terminate TLS（对应 Caddy/nginx 的推荐部署）。
	tls bool
	// xfp 非空时反代把 X-Forwarded-Proto 设成这个值（真实反代由它自己填）。
	xfp string
	// xff 非空时反代把 X-Forwarded-For 设成这个值（等价 nginx 的
	// proxy_set_header X-Forwarded-For $remote_addr）。
	// 为空时保留 Go 反代默认行为：把 TCP 对端地址追加进 XFF。
	xff string
}

// newFrontProxy 在 backend（形如 http://127.0.0.1:PORT）前面起一个真反代。
func newFrontProxy(t *testing.T, backend string, opt frontProxyOptions) *httptest.Server {
	t.Helper()
	target, err := url.Parse(backend)
	if err != nil {
		t.Fatalf("解析后端地址 %q: %v", backend, err)
	}
	rp := &httputil.ReverseProxy{
		// 用 Rewrite 而不是 Director：Go 的 Director 路径会在我们设完头**之后**
		// 再把 TCP 对端追加到 X-Forwarded-For 最右（等价 nginx 的
		// $proxy_add_x_forwarded_for），于是"反代报的客户端"与"反代自己的地址"
		// 会粘在一起，而本用例要的正是前者可控。
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.SetXForwarded() // 反代按它看到的连接填 X-Forwarded-Proto / -For（真实反代同样）
			if opt.xfp != "" {
				pr.Out.Header.Set("X-Forwarded-Proto", opt.xfp)
			}
			if opt.xff != "" {
				pr.Out.Header.Set("X-Forwarded-For", opt.xff)
			}
		},
	}

	var ts *httptest.Server
	if opt.tls {
		ts = httptest.NewTLSServer(rp)
	} else {
		ts = httptest.NewServer(rp)
	}
	t.Cleanup(ts.Close)
	return ts
}

// frontURL 是"浏览器看到的那个地址"（反代暴露的地址）。
func frontURL(ts *httptest.Server) string { return ts.URL }

// ---------------------------------------------------------------------------
// 04b · HSTS 真值表
// ---------------------------------------------------------------------------

// TestHSTSAndCookieSecureTruthTable 把 HSTS 与 Cookie Secure 的**完整真值表**
// 钉在一处：谁决定下发、谁决定不下发。
//
// 判定函数只有一个：requestIsHTTPS（middleware.go:107-120）。HSTS 头
// （middleware.go:160）与登录 Cookie 的 Secure（auth.go:743-745）都走它
// —— 这是 01-F4 的修法核心（"同一个判断不能在两处给出不同结果"）。
// 但**路由条件**不同：HSTS 还要求进程内 TLS 或显式 --hsts 开关。
//
// 表格（每一格都是端到端跑出来的，不是读代码推的）：
//
//	| # | 形态                          | TLSCert | --hsts | 可信代理 | 反代声明 | HSTS | Cookie Secure |
//	|---|-------------------------------|---------|--------|----------|----------|------|---------------|
//	| 1 | 直连明文（默认）              | 空      | 关     | 无       | —        | 否   | 否            |
//	| 2 | 直连 TLS（进程内证书）        | 有      | 关     | 无       | —        | 是   | 是            |
//	| 3 | 直连 TLS + 开关开             | 有      | 开     | 无       | —        | 是   | 是            |
//	| 4 | 反代终止 TLS，开关关          | 空      | 关     | 有       | https    | 否   | 是（有意不对称）|
//	| 5 | 反代终止 TLS，开关开          | 空      | 开     | 有       | https    | 是   | 是            |
//	| 6 | 反代终止 TLS，开关开，无可信代理 | 空    | 开     | 无       | https    | 否   | 否            |
//	| 7 | 明文反代，开关开，反代说 http | 空      | 开     | 有       | http     | 否   | 否            |
//	| 8 | 明文反代，开关开，反代说 HTTPS | 空     | 开     | 有       | HTTPS    | 是   | 是            |
//	| 9 | 明文反代，开关开，反代说 https,http | 空 | 开    | 有       | https,http | 是 | 是            |
//	| 10| 明文反代，开关开，反代说 " https " | 空 | 开     | 有       | " https " | 是  | 是            |
//	| 11| 明文反代，开关开，反代什么都不说 | 空   | 开     | 有       | —        | 否   | 否            |
//	| 12| 配了进程内证书但这一跳是明文（反代转发）| 有 | 关 | 无     | https    | 是   | 否（不可达的既存不对称）|
//
// 第 12 行是**代码形状**而不是可达部署：HSTS 的条件写的是 `s.cfg.TLSCert != ""`，
// 判的是"配置"，而 Cookie 的 Secure 判的是"这次连接"（requestIsHTTPS）。两者在
// 正常部署里同向（进程内 TLS 时 r.TLS 非空），只有"证书配了却收到明文"这种
// shipped Run() 下不可达的形状才会分叉 —— 钉住它是为了让改这一行的人看见差异。
//
// 与既有用例的关系（不重复劳动）：security_round2_test.go 的
// TestHSTSBehindTLSProxyIsExplicitOptIn 已经覆盖第 1、4、5、6、11 行，
// security_fixes_test.go 的 TestCookieSecureBehindProxy 覆盖 Cookie 那半。
// 本用例补的是：②直连 TLS、③开关对 TLS 分支的影响、⑦~⑩头解析变体、⑫判据形状，
// 并且**每一格同时**断言 HSTS 与 Cookie Secure（"同源"这件事只有在同一张表里
// 才看得出来）。
//
// 反向验证（实测，见报告 ④b）：把 middleware.go:160 改回
// `if s.cfg.TLSCert != "" {` → 第 5/8/9/10 行红在"HSTS 为空"；
// 改成 `if true {` → 第 1/6/7/11 行红在"HSTS 不该出现"。
func TestHSTSAndCookieSecureTruthTable(t *testing.T) {
	const (
		tlsCertPath = "/etc/probe/tls/server.crt" // 只为让 cfg.TLSCert != ""，不真读文件
	)

	cases := []struct {
		name string
		// cfg 由每一行自己构造（互不共享，避免限流/会话串味）。
		cfg func() config.Server
		// directTLS 为 true 时**进程自己**terminate TLS（真 TLS 监听）。
		directTLS bool
		// front 非 nil 时在其前面再套一个真反代。
		front *frontProxyOptions
		// wantHSTS / wantSecure 是这一格期望的取值。
		wantHSTS   bool
		wantSecure bool
		// asymmetryOK 只给第 ⑫ 格：那里 HSTS 与 Cookie Secure 的判据**故意**
		// 分叉（配置层 vs 连接层），是记录既存形状，不是可接受的部署形态。
		asymmetryOK bool
	}{
		{
			name: "①直连明文（默认配置）",
			cfg:  config.Default,
		},
		{
			name: "②直连 TLS（进程内证书，开关关）",
			cfg: func() config.Server {
				c := config.Default()
				c.TLSCert = tlsCertPath
				c.TLSKey = tlsCertPath + ".key"
				return c
			},
			directTLS:  true,
			wantHSTS:   true,
			wantSecure: true,
		},
		{
			name: "③直连 TLS + 开关开（开关不该破坏 TLS 分支）",
			cfg: func() config.Server {
				c := config.Default()
				c.TLSCert = tlsCertPath
				c.TLSKey = tlsCertPath + ".key"
				c.HSTS = true
				return c
			},
			directTLS:  true,
			wantHSTS:   true,
			wantSecure: true,
		},
		{
			name:       "④反代终止 TLS，开关关：有意的不对称",
			cfg:        func() config.Server { c := config.Default(); c.TrustedProxy = "127.0.0.0/8"; return c },
			front:      &frontProxyOptions{tls: true, xfp: "https"},
			wantHSTS:   false,
			wantSecure: true,
		},
		{
			name:       "⑤反代终止 TLS，开关开：HSTS 与 Cookie 同源",
			cfg:        func() config.Server { c := config.Default(); c.TrustedProxy = "127.0.0.0/8"; c.HSTS = true; return c },
			front:      &frontProxyOptions{tls: true, xfp: "https"},
			wantHSTS:   true,
			wantSecure: true,
		},
		{
			name:       "⑥反代终止 TLS，开关开，但没配可信代理：伪造/不可信来源无效",
			cfg:        func() config.Server { c := config.Default(); c.HSTS = true; return c },
			front:      &frontProxyOptions{tls: true, xfp: "https"},
			wantHSTS:   false,
			wantSecure: false,
		},
		{
			name:       "⑦明文反代，开关开，反代说 http",
			cfg:        func() config.Server { c := config.Default(); c.TrustedProxy = "127.0.0.0/8"; c.HSTS = true; return c },
			front:      &frontProxyOptions{xfp: "http"},
			wantHSTS:   false,
			wantSecure: false,
		},
		{
			name:       "⑧明文反代，开关开，反代说 HTTPS（大小写不敏感）",
			cfg:        func() config.Server { c := config.Default(); c.TrustedProxy = "127.0.0.0/8"; c.HSTS = true; return c },
			front:      &frontProxyOptions{xfp: "HTTPS"},
			wantHSTS:   true,
			wantSecure: true,
		},
		{
			name:       "⑨明文反代，开关开，反代说 https,http（取第一个）",
			cfg:        func() config.Server { c := config.Default(); c.TrustedProxy = "127.0.0.0/8"; c.HSTS = true; return c },
			front:      &frontProxyOptions{xfp: "https,http"},
			wantHSTS:   true,
			wantSecure: true,
		},
		{
			name:       "⑩明文反代，开关开，反代说 \" https \"（去空白）",
			cfg:        func() config.Server { c := config.Default(); c.TrustedProxy = "127.0.0.0/8"; c.HSTS = true; return c },
			front:      &frontProxyOptions{xfp: " https "},
			wantHSTS:   true,
			wantSecure: true,
		},
		{
			name:       "⑪明文反代，开关开，反代什么都不说",
			cfg:        func() config.Server { c := config.Default(); c.TrustedProxy = "127.0.0.0/8"; c.HSTS = true; return c },
			front:      &frontProxyOptions{},
			wantHSTS:   false,
			wantSecure: false,
		},
		{
			// 这一格实测出来的是**不对称**：HSTS 有、Cookie 的 Secure 没有。
			// 原因：HSTS 的判据是 `s.cfg.TLSCert != ""`（配置层），
			// 而 cookieSecure → requestIsHTTPS（请求层）看到的是"明文到达的请求
			// + 不可信来源"，于是 false。
			//
			// 它在 shipped 部署里**不可达**：配了 --tls-cert 时 Run() 只开 TLS
			// 监听（server.go:666-667），不会出现"证书配了却收到明文"这一跳。
			// 保留这一格是为了钉住"两个判据不是同一个东西"—— 想把这行改成
			// `requestIsHTTPS(...)` 的人必须先看见这个差异。
			name: "⑫配了进程内证书但这一跳是明文（判据是配置不是连接）",
			cfg: func() config.Server {
				c := config.Default()
				c.TLSCert = tlsCertPath
				c.TLSKey = tlsCertPath + ".key"
				return c
			},
			front:       &frontProxyOptions{tls: true, xfp: "https"},
			wantHSTS:    true,
			wantSecure:  false,
			asymmetryOK: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHSTSProbeServer(t, tc.cfg(), tc.directTLS)

			base := h.ts.URL
			client := h.client
			if tc.front != nil {
				front := newFrontProxy(t, base, *tc.front)
				base = frontURL(front)
				client = h.clientFor(front)
			}

			status, _, resp := doJSON(t, client, base, http.MethodPost, "/api/v1/auth/login",
				map[string]any{"username": h.username, "password": h.password}, nil)
			if status != http.StatusOK {
				t.Fatalf("登录失败: HTTP %d", status)
			}

			hsts := resp.Header.Get("Strict-Transport-Security")
			if tc.wantHSTS {
				if hsts == "" {
					t.Fatalf("这一格应当下发 HSTS，实际没有（响应头 %v）", resp.Header)
				}
				if hsts != hstsValue {
					t.Fatalf("HSTS 取值 = %q，期望 %q", hsts, hstsValue)
				}
			} else if hsts != "" {
				t.Fatalf("这一格不该下发 HSTS，实际 %q", hsts)
			}

			cookie := sessionSetCookie(resp)
			if cookie == "" {
				t.Fatal("登录成功却没有下发会话 Cookie")
			}
			hasSecure := strings.Contains(cookie, "Secure")
			if hasSecure != tc.wantSecure {
				t.Fatalf("Cookie 的 Secure = %v，期望 %v（Cookie=%q，HSTS=%q）",
					hasSecure, tc.wantSecure, cookie, hsts)
			}
			// "同源"这件事只有在一格里同时看两个头才有意义：
			// HSTS 开着却不认这次连接、或反过来，都是 01-F4 那种自相矛盾。
			// 第 ⑫ 格是唯一的例外（asymmetryOK），它记录的正是那处既存分叉。
			if tc.wantHSTS && !hasSecure && !tc.asymmetryOK {
				t.Fatalf("下发了 HSTS 的同一响应里 Cookie 却没有 Secure（Cookie=%q）", cookie)
			}
		})
	}
}

// newHSTSProbeServer 起一个"已初始化管理员"的真服务端；
// directTLS 为 true 时**进程自己**terminate TLS（真 TLS 监听）。
//
// 与 auth_test.go 的 newAuthHarnessFull 是同一套起法，只是把 httptest.NewServer
// 换成可选的 NewUnstartedServer + StartTLS —— 直连 TLS 这一格必须真走 TLS，
// 否则 r.TLS 恒为 nil，测的就不是那一格了。
func newHSTSProbeServer(t *testing.T, cfg config.Server, directTLS bool) *authHarness {
	t.Helper()
	ctx := context.Background()
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
	if code == "" {
		t.Fatal("初始化码为空")
	}

	guard := newBackgroundGuard(t)
	realtime := guard.start("realtimeLoop", s.realtimeLoop)
	dispatch := guard.start("dispatch", s.dispatch.Start)
	stopLoops := func() {
		dispatch.stop()
		realtime.stop()
	}

	var ts *httptest.Server
	if directTLS {
		ts = httptest.NewUnstartedServer(s.Handler())
		ts.StartTLS()
	} else {
		ts = httptest.NewServer(s.Handler())
	}
	t.Cleanup(ts.Close)
	t.Cleanup(stopLoops)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("创建 Cookie jar: %v", err)
	}
	client := ts.Client()
	client.Jar = jar
	client.Timeout = 10 * time.Second
	h := &authHarness{
		ts:       ts,
		srv:      s,
		client:   client,
		username: "admin",
		password: "correct-horse-battery-staple",
		cancel:   stopLoops,
	}

	status, body, _ := doJSON(t, h.client, h.ts.URL, http.MethodPost, "/api/v1/setup",
		map[string]any{"code": code, "username": h.username, "password": h.password}, nil)
	if status != http.StatusOK {
		t.Fatalf("初始化失败: HTTP %d %v", status, body)
	}
	return h
}

// clientFor 复制一个只指向 front 的独立客户端（自己的 Cookie jar，
// 相当于反代后面另一个浏览器；反代换了端口，但 Cookie jar 不区分端口，
// 所以要**新建**一个 jar，别把后端的会话带过来）。
func (h *authHarness) clientFor(front *httptest.Server) *http.Client {
	jar, _ := cookiejar.New(nil)
	c := front.Client()
	c.Jar = jar
	c.Timeout = 10 * time.Second
	return c
}

// doJSON 向任意 baseURL 发一个 JSON 请求（authHarness.do 只会打 h.ts.URL）。
func doJSON(t *testing.T, client *http.Client, base, method, path string, payload any, extra map[string]string) (int, map[string]any, *http.Response) {
	t.Helper()
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("序列化请求体: %v", err)
		}
		body = strings.NewReader(string(data))
	}
	req, err := http.NewRequest(method, base+path, body)
	if err != nil {
		t.Fatalf("构造请求: %v", err)
	}
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
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

// TestRequestIsHTTPSTruthTable 把判定函数本身的输入空间钉住（单元层）。
//
// HSTS 与 Cookie Secure 共用这一个函数，所以它的每一条边界都同时影响两个头：
// 大小写、前后空白、逗号列表（"https,http" 取第一个）、无意义值、
// 以及"对端不在可信网段时一律 false"。
//
// 这里直接构造 *http.Request：端到端那一半在上面那张表里（真 TLS、真反代）。
func TestRequestIsHTTPSTruthTable(t *testing.T) {
	_, lan, err := net.ParseCIDR("10.0.0.0/8")
	if err != nil {
		t.Fatalf("解析 CIDR: %v", err)
	}
	trusted := []*net.IPNet{lan}

	cases := []struct {
		name   string
		remote string
		tls    bool
		xfp    string
		want   bool
	}{
		{"可信代理 + https", "10.0.0.5:1234", false, "https", true},
		{"可信代理 + HTTPS（大小写）", "10.0.0.5:1234", false, "HTTPS", true},
		{"可信代理 + 前后空白", "10.0.0.5:1234", false, " https\t", true},
		{"可信代理 + 列表取第一个", "10.0.0.5:1234", false, "https,http", true},
		{"可信代理 + 列表第一个是 http", "10.0.0.5:1234", false, "http,https", false},
		{"可信代理 + http", "10.0.0.5:1234", false, "http", false},
		{"可信代理 + 空", "10.0.0.5:1234", false, "", false},
		{"可信代理 + 垃圾值", "10.0.0.5:1234", false, "gopher", false},
		{"不可信来源 + https（伪造）", "203.0.113.9:1234", false, "https", false},
		{"直连 TLS（r.TLS 非空，无可信代理）", "203.0.113.9:1234", true, "", true},
		{"直连 TLS + 伪造 http（TLS 就是 TLS）", "203.0.113.9:1234", true, "http", true},
		{"对端地址畸形", "not-an-ip", false, "https", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "http://panel.example/", nil)
			r.RemoteAddr = tc.remote
			if tc.xfp != "" {
				r.Header.Set("X-Forwarded-Proto", tc.xfp)
			}
			if tc.tls {
				r.TLS = &tls.ConnectionState{HandshakeComplete: true}
			}
			if got := requestIsHTTPS(r, trusted); got != tc.want {
				t.Fatalf("requestIsHTTPS = %v，期望 %v", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 04c · 限流退化成全局单桶
// ---------------------------------------------------------------------------

// TestReverseProxySingleBucketLocksAdminOut 用**真反代**复现 01-F2 的退化路径。
//
// 退化条件（report 01-F2）：所有请求的 remoteIP 都等于反代地址，于是
// `clientIP` 对所有人返回同一个键，`newAttemptLimiter(5, 1m, 10, 15m)` 从
// "每 IP 的桶"变成"全网的桶"。任何未认证的人在两个窗口里各发 5 次失败请求
// （共 10 次连败）就能把**管理员本人**锁在门外 15 分钟。
//
// 本用例走真反代（httputil.ReverseProxy）而不是直接打后端：退化的成因正是
// "中间多了一跳"，绕过反代就复现不出来了。两个"人"用两个独立 Cookie jar
// 与两条独立 TCP 连接，唯一相同的是反代看到的对端地址。
//
// 只把限流的**窗口**换成 200ms，锁定阈值与锁定时长**读生产对象**（shortWindow）：
// 要验的是"连败到阈值就锁死**别人**、而且锁得够久"，不是"窗口是不是 60 秒"。
// 参数抄一份到自己这边就等于自己跟自己玩 —— 把生产参数改掉用例也不会红。
//
// 反向验证（实测，见报告 ④c）：
//   - 把 auth.go 的 `newAttemptLimiter(5, time.Minute, 10, 15*time.Minute)` 的
//     锁定参数改成 `0, 0`（去掉连败锁定）→ 红在 shortWindow 的
//     "生产登录限流的锁定参数异常"。
//   - 把 lockFor 从 15*time.Minute 改成 30*time.Second → 红在
//     "锁定剩余时间 = 29s，期望 >= 600s"。
//   - 把 middleware.go 的 `ipInNets(...)`+`forwardedIP(...)` 分支删掉
//     （不再采信 XFF）→ 第二段红在"配好可信代理后管理员应当 200"。
func TestReverseProxySingleBucketLocksAdminOut(t *testing.T) {
	t.Run("没配可信代理：反代后面所有人共用一个桶，未认证者锁死管理员", func(t *testing.T) {
		// 推荐部署的经典误配：反代在另一个容器/主机上，运维没写 --trusted-proxy
		// （config.go:26 默认空串）。
		cfg := config.Default()
		h := newAuthHarnessWithConfig(t, cfg)
		lim := shortWindow(t, h)

		front := newFrontProxy(t, h.ts.URL, frontProxyOptions{})
		base := frontURL(front)

		attacker := h.clientFor(front)
		admin := h.clientFor(front)

		// 攻击者：按生产参数连败到阈值（每窗口 max 次，用完等窗口过去）。
		failures := 0
		for failures < lim.lockAfter {
			for i := 0; i < lim.max && failures < lim.lockAfter; i++ {
				status, _, _ := doJSON(t, attacker, base, http.MethodPost, "/api/v1/auth/login",
					map[string]any{"username": h.username, "password": "not-the-password"}, nil)
				if status != http.StatusUnauthorized {
					t.Fatalf("第 %d 次失败登录 = %d，期望 401（窗口额度被提前吃掉了？）", failures+1, status)
				}
				failures++
			}
			if failures < lim.lockAfter {
				time.Sleep(220 * time.Millisecond) // 等窗口过去，换一份新预算
			}
		}

		// 管理员（另一条连接、另一个 Cookie jar、**正确**的密码）。
		status, body, resp := doJSON(t, admin, base, http.MethodPost, "/api/v1/auth/login",
			map[string]any{"username": h.username, "password": h.password}, nil)
		if status != http.StatusTooManyRequests {
			t.Fatalf("攻击者连败 %d 次之后管理员应当也被挡住（这就是退化），实际 %d %v",
				failures, status, body)
		}
		retry := resp.Header.Get("Retry-After")
		if retry == "" {
			t.Fatal("429 应当带 Retry-After")
		}
		secs, err := strconv.Atoi(strings.TrimSpace(retry))
		if err != nil {
			t.Fatalf("Retry-After = %q 不是秒数: %v", retry, err)
		}
		// 锁定必须够久才有意义：接管这个桶的成本要高到不值得（报告口径 15 分钟）。
		// 这里不钉死 15 分钟这个数字，而是钉住"至少 10 分钟"这条性质。
		if secs < 600 {
			t.Fatalf("锁定剩余时间 = %ds，期望 >= 600s（否则退化的代价太小）", secs)
		}
		if code, _ := body["error"].(map[string]any)["code"].(string); code != "too_many_attempts" {
			t.Fatalf("错误码 = %q，期望 too_many_attempts", code)
		}
	})

	t.Run("配了可信代理：反代按客户端给 XFF，攻击者锁不到管理员", func(t *testing.T) {
		cfg := config.Default()
		cfg.TrustedProxy = "127.0.0.1/32" // 直连对端是反代，采信它追加的 XFF
		h := newAuthHarnessWithConfig(t, cfg)
		lim := shortWindow(t, h)

		attackerIP, adminIP := "198.51.100.7", "203.0.113.9"
		attackerFront := newFrontProxy(t, h.ts.URL, frontProxyOptions{xff: attackerIP})
		adminFront := newFrontProxy(t, h.ts.URL, frontProxyOptions{xff: adminIP})

		attacker := h.clientFor(attackerFront)
		admin := h.clientFor(adminFront)

		failures := 0
		for failures < lim.lockAfter {
			for i := 0; i < lim.max && failures < lim.lockAfter; i++ {
				status, _, _ := doJSON(t, attacker, frontURL(attackerFront), http.MethodPost, "/api/v1/auth/login",
					map[string]any{"username": h.username, "password": "not-the-password"}, nil)
				if status != http.StatusUnauthorized {
					t.Fatalf("攻击者第 %d 次 = %d，期望 401", failures+1, status)
				}
				failures++
			}
			if failures < lim.lockAfter {
				time.Sleep(220 * time.Millisecond)
			}
		}

		// 攻击者自己确实被锁住了（证明上面那些失败真的生效，不是空断言）。
		if status, _, _ := doJSON(t, attacker, frontURL(attackerFront), http.MethodPost, "/api/v1/auth/login",
			map[string]any{"username": h.username, "password": h.password}, nil); status != http.StatusTooManyRequests {
			t.Fatalf("攻击者自己应当被锁住，实际 %d", status)
		}
		// 管理员来自另一个来源：不受影响。
		status, body, _ := doJSON(t, admin, frontURL(adminFront), http.MethodPost, "/api/v1/auth/login",
			map[string]any{"username": h.username, "password": h.password}, nil)
		if status != http.StatusOK {
			t.Fatalf("配好 --trusted-proxy 之后管理员不该被攻击者锁住，实际 %d %v", status, body)
		}
	})
}

// shortWindow 只把登录限流的**窗口**换成 200ms，并返回生产那份参数。
//
// 锁定阈值（lockAfter）与锁定时长（lockFor）**从生产对象上读**：
//
//   - 抄一份常数到测试里 = 自己跟自己玩，生产参数被改掉用例还是绿的；
//   - 读出来用 = 改小/去掉锁定参数，用例必红（见 TestReverseProxy... 的注释）。
//
// 先把窗口缩短还有第二个作用：窗口额度（max 次）在 200ms 内用完就换新的，
// 用例不必真的等一分钟。
func shortWindow(t *testing.T, h *authHarness) *attemptLimiter {
	t.Helper()
	prod := h.srv.auth.login
	if prod.lockAfter <= 0 {
		t.Fatalf("生产登录限流的连败锁定被关掉了（lockAfter=%d）—— 退化时就没有任何代价了", prod.lockAfter)
	}
	if prod.lockFor < 5*time.Minute {
		t.Fatalf("生产登录限流的锁定时长 = %s，太短（退化的代价太小）", prod.lockFor)
	}
	short := newAttemptLimiter(prod.max, 200*time.Millisecond, prod.lockAfter, prod.lockFor)
	h.srv.auth.login = short
	return short
}
