package server

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"probe/internal/config"
	"probe/internal/store"
	"probe/web"
)

// ---------------------------------------------------------------- 测试脚手架

// noGzipClient 是一个**不做透明解压**的客户端。
//
// 这一点是这批用例的前提：http.Client 默认会自动加 Accept-Encoding: gzip，
// 收到 gzip 响应后悄悄解开，还会把 Content-Encoding 与 Content-Length 两个头
// 一起删掉。用它测 gzip 等于什么都没测 —— 网线上到底发了多少字节、头长什么样，
// 全都看不见。
func noGzipClient() *http.Client {
	return &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{DisableCompression: true},
	}
}

type rawResp struct {
	status int
	header http.Header
	body   []byte
}

// fetchRaw 发一个 GET，把**网线上原样**的状态码、响应头与字节拿回来。
func fetchRaw(t *testing.T, c *http.Client, url string, headers map[string]string) rawResp {
	t.Helper()
	return fetchRawMethod(t, c, http.MethodGet, url, headers)
}

// fetchRawMethod 同上，可指定方法（HEAD 用）。
func fetchRawMethod(t *testing.T, c *http.Client, method, url string, headers map[string]string) rawResp {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatalf("构造请求 %s %s: %v", method, url, err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("请求 %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读响应体 %s: %v", url, err)
	}
	return rawResp{status: resp.StatusCode, header: resp.Header, body: body}
}

// mustGunzip 解开一个 gzip 响应体；解不开就说明服务端写出了坏流。
func mustGunzip(t *testing.T, what string, raw []byte) []byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("%s: gzip 头解析失败: %v（原始 %d 字节：%s）", what, err, len(raw), hex.Dump(headOf(raw, 64)))
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("%s: 解压失败: %v（原始 %d 字节）", what, err, len(raw))
	}
	if err := zr.Close(); err != nil {
		t.Fatalf("%s: gzip 收尾失败: %v", what, err)
	}
	return out
}

func headOf(b []byte, n int) []byte {
	if len(b) > n {
		return b[:n]
	}
	return b
}

// openTestDB 开一个临时库（用例之间互不影响）。
func openTestDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "probe.db"))
	if err != nil {
		t.Fatalf("打开测试数据库: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// hasVary 判断 Vary 头的逗号列表里有没有这一项。
func hasVary(h http.Header, want string) bool {
	for _, v := range h.Values("Vary") {
		for _, item := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(item), want) {
				return true
			}
		}
	}
	return false
}

// createTestNode 建一个节点并返回它的一次性 Token（SSE 用例要用真 Agent 喂数据）。
func createTestNode(t *testing.T, s *Server, name string) (store.Node, string) {
	t.Helper()
	node, token, err := s.db.CreateNode(context.Background(), store.NewNode{
		Name: name, GroupName: "测试", Region: "HK", Note: "gzip 用例",
		Iface: "eth0", IntervalSec: 1, TrafficLimit: 1 << 40, TrafficWarnPct: 80,
		ResetDay: 1, PriceCents: 1234, Currency: "USD", BillingMonths: 12,
	}, time.Now())
	if err != nil {
		t.Fatalf("创建节点 %s: %v", name, err)
	}
	return node, token
}

// sessionCookie 取出 harness 客户端手上的会话 Cookie（裸 TCP 请求要用它）。
func sessionCookie(t *testing.T, h *authHarness) string {
	t.Helper()
	u, err := url.Parse(h.ts.URL)
	if err != nil {
		t.Fatalf("解析服务端地址: %v", err)
	}
	for _, c := range h.client.Jar.Cookies(u) {
		if c.Name == sessionCookieName {
			return c.Name + "=" + c.Value
		}
	}
	t.Fatal("harness 客户端上没有会话 Cookie")
	return ""
}

// ---------------------------------------------------------------- 1. 压了，而且压得对

// TestGzipCompressesPanelResponses 是这次改动的正题：面板上真正走流量的那几条
// 响应必须真的被压小，而且**解出来与不压时逐字节相同**。
//
// 为什么必须比字节：只断言"有 Content-Encoding: gzip"证明不了内容没坏
// （截断、错位、把明文混进 gzip 流里都照样有这个头）。这里两边都是网线上的
// 原始字节，一遍解压一遍不压，逐字节比对。
func TestGzipCompressesPanelResponses(t *testing.T) {
	h := newAuthHarness(t)
	node, _ := createTestNode(t, h.srv, "gz-01")
	c := noGzipClient()
	cookie := sessionCookie(t, h)

	paths := []string{
		"/", "/app.js", "/chart.js", "/style.css",
		"/api/v1/nodes", fmt.Sprintf("/api/v1/nodes/%d", node.ID),
	}

	type row struct {
		path          string
		plain, packed int
	}
	var rows []row
	for _, p := range paths {
		plain := fetchRaw(t, c, h.ts.URL+p, map[string]string{"Cookie": cookie})
		if plain.status != http.StatusOK {
			t.Fatalf("%s 不带 gzip 的状态码 = %d，期望 200", p, plain.status)
		}
		if enc := plain.header.Get("Content-Encoding"); enc != "" {
			t.Fatalf("%s 客户端没声明 gzip，却有 Content-Encoding: %q", p, enc)
		}
		// Vary 与"这次压没压"无关：只要媒体类型可压就必须有，否则中间缓存
		// 可能把 gzip 版本发给不支持 gzip 的客户端。
		if !hasVary(plain.header, "Accept-Encoding") {
			t.Errorf("%s 缺少 Vary: Accept-Encoding（Vary 只有 %q）", p, plain.header.Values("Vary"))
		}

		packed := fetchRaw(t, c, h.ts.URL+p, map[string]string{"Cookie": cookie, "Accept-Encoding": "gzip"})
		if packed.status != plain.status {
			t.Fatalf("%s 压缩后状态码 = %d，期望 %d", p, packed.status, plain.status)
		}
		if enc := packed.header.Get("Content-Encoding"); enc != "gzip" {
			t.Fatalf("%s 声明了 gzip 却没压（Content-Encoding = %q）", p, enc)
		}
		if !hasVary(packed.header, "Accept-Encoding") {
			t.Errorf("%s 压缩后缺少 Vary: Accept-Encoding", p)
		}
		// Content-Length 的两种合法形态：要么没有（chunked），要么等于**压缩后**
		// 的字节数。绝不能是明文长度 —— 那就是"头与体对不上"。
		if cl := packed.header.Get("Content-Length"); cl != "" {
			n, err := strconv.Atoi(cl)
			if err != nil || n != len(packed.body) {
				t.Errorf("%s 的 Content-Length = %q，压缩后实际 %d 字节", p, cl, len(packed.body))
			}
		}
		if got := mustGunzip(t, p, packed.body); !bytes.Equal(got, plain.body) {
			t.Fatalf("%s 解压后与明文不一致：解出 %d 字节、明文 %d 字节", p, len(got), len(plain.body))
		}
		if len(packed.body) >= len(plain.body) {
			t.Errorf("%s 压完反而更大：%d → %d 字节", p, len(plain.body), len(packed.body))
		}
		rows = append(rows, row{p, len(plain.body), len(packed.body)})
	}

	var plainTotal, packedTotal int
	for _, r := range rows {
		t.Logf("%-22s 明文 %7d → gzip %7d 字节（%.2f×）",
			r.path, r.plain, r.packed, float64(r.plain)/float64(r.packed))
		plainTotal += r.plain
		packedTotal += r.packed
	}
	// 首屏口径（index.html + app.js + chart.js + style.css）：实测 376 KB 那一段。
	firstScreenPlain := rows[0].plain + rows[1].plain + rows[2].plain + rows[3].plain
	firstScreenPacked := rows[0].packed + rows[1].packed + rows[2].packed + rows[3].packed
	t.Logf("首屏四件套合计：明文 %d 字节 → gzip %d 字节（%.2f×）；六条响应合计 %d → %d 字节（%.2f×）",
		firstScreenPlain, firstScreenPacked, float64(firstScreenPlain)/float64(firstScreenPacked),
		plainTotal, packedTotal, float64(plainTotal)/float64(packedTotal))
}

// ---------------------------------------------------------------- 2. 不该压的没被压

// TestGzipLeavesIneligibleResponsesAlone 逐条钉住"不压"的每一种情况。
//
// 每一条都同时断言两件事：内容没被动过（逐字节），以及**该有的 Vary 有没有**
// —— Vary 的规则与"这次压没压"是两条独立的规则，混在一起判就会漏。
func TestGzipLeavesIneligibleResponsesAlone(t *testing.T) {
	big := bytes.Repeat([]byte("abcdefghij"), 400) // 4000 字节
	cases := []struct {
		name    string
		ctype   string
		status  int
		body    []byte
		headers map[string]string
		req     map[string]string
		wantVar bool
	}{
		{name: "png", ctype: "image/png", body: big},
		{name: "jpeg", ctype: "image/jpeg", body: big},
		{name: "ico", ctype: "image/x-icon", body: big},
		{name: "zip", ctype: "application/zip", body: big},
		{name: "gzip 自身（已压缩格式）", ctype: "application/gzip", body: big},
		{name: "字体 woff2", ctype: "font/woff2", body: big},
		{
			name: "handler 已设 Content-Encoding: br", ctype: "application/json",
			body: big, headers: map[string]string{"Content-Encoding": "br"}, wantVar: true,
		},
		{name: "204 无正文", ctype: "application/json", status: http.StatusNoContent, wantVar: true},
		{name: "304 协商缓存", ctype: "application/json", status: http.StatusNotModified, wantVar: true},
		{name: "206 部分内容", ctype: "application/json", status: http.StatusPartialContent, body: big, wantVar: true},
		{
			name: "小于阈值的 JSON（40 字节）", ctype: "application/json",
			body: []byte(`{"error":{"code":"not_found","message":""}}`), wantVar: true,
		},
		{
			name: "客户端说不（gzip;q=0）", ctype: "application/json", body: big, wantVar: true,
			req: map[string]string{"Accept-Encoding": "gzip;q=0"},
		},
		{
			name: "客户端只认 br", ctype: "application/json", body: big, wantVar: true,
			req: map[string]string{"Accept-Encoding": "br"},
		},
		{
			name: "客户端没声明", ctype: "application/json", body: big, wantVar: true,
			req: map[string]string{"Accept-Encoding": ""},
		},
		{name: "纯文本不在白名单", ctype: "text/plain; charset=utf-8", body: big},
	}

	if len(cases) < 10 {
		t.Fatalf("用例太少（%d 条）：这一组的意义就是覆盖得够宽", len(cases))
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t)
			handler := s.withMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for k, v := range tc.headers {
					w.Header().Set(k, v)
				}
				if tc.ctype != "" {
					w.Header().Set("Content-Type", tc.ctype)
				}
				w.WriteHeader(tc.status)
				if len(tc.body) > 0 {
					_, _ = w.Write(tc.body)
				}
			}))
			ts := httptest.NewServer(handler)
			defer ts.Close()

			req := map[string]string{"Accept-Encoding": "gzip"}
			for k, v := range tc.req {
				req[k] = v
			}
			got := fetchRaw(t, noGzipClient(), ts.URL+"/", req)

			if enc := got.header.Get("Content-Encoding"); enc != tc.headers["Content-Encoding"] {
				t.Errorf("Content-Encoding = %q，期望 %q（不该压，也不该改 handler 自己设的值）",
					enc, tc.headers["Content-Encoding"])
			}
			if !bytes.Equal(got.body, tc.body) {
				t.Errorf("响应体被改动了：%d → %d 字节", len(tc.body), len(got.body))
			}
			if hasVary(got.header, "Accept-Encoding") != tc.wantVar {
				t.Errorf("Vary: Accept-Encoding = %v，期望 %v（Vary 只看媒体类型，与这次压没压无关）",
					!tc.wantVar, tc.wantVar)
			}
		})
	}
}

// TestGzipSkipsRangeRequests 是一条真实路径上的 206：走 http.ServeContent
// 的 Range 请求。压了它，Content-Range 与实到的字节数就对不上了。
func TestGzipSkipsRangeRequests(t *testing.T) {
	h := newAuthHarness(t)
	c := noGzipClient()

	full := fetchRaw(t, c, h.ts.URL+"/app.js", map[string]string{"Accept-Encoding": "gzip"})
	if full.header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("/app.js 完整请求应当被压，实际 Content-Encoding = %q", full.header.Get("Content-Encoding"))
	}

	part := fetchRaw(t, c, h.ts.URL+"/app.js", map[string]string{
		"Accept-Encoding": "gzip", "Range": "bytes=0-99",
	})
	if part.status != http.StatusPartialContent {
		t.Fatalf("Range 请求状态码 = %d，期望 206", part.status)
	}
	if enc := part.header.Get("Content-Encoding"); enc != "" {
		t.Errorf("206 不该被压，Content-Encoding = %q", enc)
	}
	if len(part.body) != 100 {
		t.Errorf("206 的正文 = %d 字节，期望 100（压了就会与 Content-Range 对不上）", len(part.body))
	}
	if cr := part.header.Get("Content-Range"); cr == "" {
		t.Error("206 缺少 Content-Range")
	}
}

// ---------------------------------------------------------------- 3. 阈值两侧

// TestGzipSizeThreshold 钉住阈值两侧：511 不压，512/513 压。
//
// 为什么阈值要卡得这么死：它决定的是"小响应会不会反而变大"。gzip 有定长开销
// （10 字节头 + 8 字节尾）、每次 flush 一个 5 字节空块，去掉 Content-Length 之后
// chunked 还要几字节 —— 所以阈值必须大于这些开销之和。
func TestGzipSizeThreshold(t *testing.T) {
	unit := []byte("hello compressible world ")
	for _, n := range []int{511, 512, 513} {
		t.Run(strconv.Itoa(n), func(t *testing.T) {
			body := bytes.Repeat(unit, n/len(unit)+1)[:n]
			s := newTestServer(t)
			ts := httptest.NewServer(s.withMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				_, _ = w.Write(body)
			})))
			defer ts.Close()

			got := fetchRaw(t, noGzipClient(), ts.URL+"/", map[string]string{"Accept-Encoding": "gzip"})
			enc := got.header.Get("Content-Encoding")
			switch {
			case n < gzipMinSize:
				if enc != "" {
					t.Fatalf("%d 字节 < 阈值 %d，不该压（Content-Encoding = %q）", n, gzipMinSize, enc)
				}
				if !bytes.Equal(got.body, body) {
					t.Fatalf("不压时正文必须原样，实际 %d 字节", len(got.body))
				}
			default:
				if enc != "gzip" {
					t.Fatalf("%d 字节 ≥ 阈值 %d，应当压（Content-Encoding = %q）", n, gzipMinSize, enc)
				}
				if got := mustGunzip(t, "阈值用例", got.body); !bytes.Equal(got, body) {
					t.Fatalf("解压后与明文不一致：%d → %d", len(body), len(got))
				}
			}
		})
	}
}

// TestGzipSizeThresholdAcrossWrites 验证阈值是按"累计写了多少"算的，不是按单次 Write。
//
// 真实 handler 常常分几次写（json.Encoder + 手写的几段、模板渲染等）。
// 只看第一次 Write 就决定"不压"，会让一个 600 字节的响应白白发成明文。
func TestGzipSizeThresholdAcrossWrites(t *testing.T) {
	t.Run("累计超过阈值要压", func(t *testing.T) {
		chunks := [][]byte{bytes.Repeat([]byte("a"), 200), bytes.Repeat([]byte("b"), 200), bytes.Repeat([]byte("c"), 200)}
		ts := newTestServer(t)
		server := httptest.NewServer(ts.withMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			for _, c := range chunks {
				_, _ = w.Write(c)
			}
		})))
		defer server.Close()
		got := fetchRaw(t, noGzipClient(), server.URL+"/", map[string]string{"Accept-Encoding": "gzip"})
		if got.header.Get("Content-Encoding") != "gzip" {
			t.Fatalf("三次共写 600 字节，应当压，实际 Content-Encoding = %q", got.header.Get("Content-Encoding"))
		}
		want := bytes.Join(chunks, nil)
		if out := mustGunzip(t, "分次写", got.body); !bytes.Equal(out, want) {
			t.Fatalf("解压后与明文不一致：%d → %d", len(want), len(out))
		}
	})

	t.Run("累计不到阈值不压", func(t *testing.T) {
		chunks := [][]byte{bytes.Repeat([]byte("a"), 100), bytes.Repeat([]byte("b"), 100), bytes.Repeat([]byte("c"), 100)}
		ts := newTestServer(t)
		server := httptest.NewServer(ts.withMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			for _, c := range chunks {
				_, _ = w.Write(c)
			}
		})))
		defer server.Close()
		got := fetchRaw(t, noGzipClient(), server.URL+"/", map[string]string{"Accept-Encoding": "gzip"})
		if enc := got.header.Get("Content-Encoding"); enc != "" {
			t.Fatalf("三次共写 300 字节（< %d），不该压，实际 Content-Encoding = %q", gzipMinSize, enc)
		}
		if !bytes.Equal(got.body, bytes.Join(chunks, nil)) {
			t.Fatalf("不压时正文必须原样，实际 %d 字节", len(got.body))
		}
	})

	t.Run("handler 提前 Flush 之后就不再压", func(t *testing.T) {
		// 阈值是靠"攒够 512 字节"实现的，而 handler 主动 Flush 等于要求"现在就把
		// 已有的东西发出去" —— 那一刻 header 就定死了，后面写多少都改不了。
		// 这是刻意的取舍：宁可少压一次，也不能把 handler 要求立刻发出的字节扣住。
		// 要紧的是**一个字节都不能丢**。
		want := append(bytes.Repeat([]byte("head"), 50), bytes.Repeat([]byte("tail"), 500)...)
		ts := newTestServer(t)
		server := httptest.NewServer(ts.withMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(want[:200])
			_ = http.NewResponseController(w).Flush()
			_, _ = w.Write(want[200:])
		})))
		defer server.Close()
		got := fetchRaw(t, noGzipClient(), server.URL+"/", map[string]string{"Accept-Encoding": "gzip"})
		if enc := got.header.Get("Content-Encoding"); enc != "" {
			t.Fatalf("提前 Flush 之后应当按不压处理（header 已经发出去了），实际 %q", enc)
		}
		if !bytes.Equal(got.body, want) {
			t.Fatalf("字节数不对：%d，期望 %d", len(got.body), len(want))
		}
	})
}

// ---------------------------------------------------------------- 4. WebSocket 没被弄坏

// hijackableRecorder 是一个能 Hijack 的假 writer（直接调链路时用）。
//
// 为什么不拿真连接测：这条链上一直有 statusWriter（它只实现 Unwrap，不实现
// Hijacker），所以"能不能升级"的真正判据是 http.ResponseController 能不能
// 顺着 Unwrap 找到底层的 Hijack —— 拿假 writer 反而能把这件事看得更清楚。
type hijackableRecorder struct {
	*httptest.ResponseRecorder
	hijacked bool
}

// errFakeHijack 是假 writer 的哨兵错误：它被返回就说明请求**真的走到了最底层**。
var errFakeHijack = errors.New("测试不真的接管连接")

func (h *hijackableRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h.hijacked = true
	return nil, nil, errFakeHijack
}

// TestUpgradeRequestsGetRawWriter 钉住"升级请求整条放行"。
//
// 为什么这条必须有：101 之后走的是 Hijack 拿到的裸连接，任何包装层都可能
// 把之后的字节当成响应体去处理。这里断言两件事：
//   - 升级请求拿到的是**原始** writer（没有 gzip 包装）；
//   - 无论包没包，ResponseController 都要能一路找到底层的 Hijack
//     （包装器实现了 Unwrap）—— 这条链上本来就夹着 statusWriter，
//     "升级能力"从来不是靠类型断言拿到的。
func TestUpgradeRequestsGetRawWriter(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		wantRaw bool
	}{
		{"标准握手（Upgrade + Connection: Upgrade）", map[string]string{"Upgrade": "websocket", "Connection": "Upgrade"}, true},
		{"Connection 是逗号列表", map[string]string{"Upgrade": "websocket", "Connection": "keep-alive, Upgrade"}, true},
		{"只有 Connection: upgrade", map[string]string{"Connection": "upgrade"}, true},
		{"只有 Upgrade 头", map[string]string{"Upgrade": "websocket"}, true},
		{"Accept-Encoding: gzip 的普通请求", map[string]string{"Accept-Encoding": "gzip"}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t)
			rec := &hijackableRecorder{ResponseRecorder: httptest.NewRecorder()}
			var wrapped, hijacked bool
			var hijackErr error

			handler := s.withMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, wrapped = w.(*gzipResponseWriter)
				_, _, hijackErr = http.NewResponseController(w).Hijack()
				hijacked = rec.hijacked
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"ok":true}`)
			}))

			req := httptest.NewRequest(http.MethodGet, apiPrefix+"v1/agent/ws", nil)
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			handler.ServeHTTP(rec, req)

			if wrapped {
				if tc.wantRaw {
					t.Error("升级请求被 gzip 包装了：handler 拿到的不是原始 writer")
				}
			} else if !tc.wantRaw {
				t.Error("普通请求没有走 gzip 包装器")
			}
			if !errors.Is(hijackErr, errFakeHijack) {
				t.Errorf("ResponseController.Hijack 没有走到最底层 writer（err=%v）：WebSocket 升级会失败", hijackErr)
			}
			if !hijacked {
				t.Error("Hijack 没有落到最底层 writer 上")
			}
		})
	}
}

// TestAgentWebSocketStillWorksUnderGzipMiddleware 走一遍真的 WebSocket 握手。
//
// 浏览器与 Go 的 WS 客户端都会在握手请求上带 Accept-Encoding: gzip，
// 所以"压缩中间件装上之后 Agent 还能不能连上"必须有一条真握手的用例 ——
// 上面那条只断言了 writer 的类型。
func TestAgentWebSocketStillWorksUnderGzipMiddleware(t *testing.T) {
	ts, s, _, token := newAgentTestServer(t)
	s.cfg.Gzip = true // 默认就是 true；写出来是为了让这条用例的意图一眼可见

	conn := mustDialAgent(t, ts, token)
	sendFrame(t, conn, helloFrame(t, testHello()))
	readHandshake(t, conn)
}

// ---------------------------------------------------------------- 5. ETag / 304 没被弄坏

// TestGzipKeepsETagAndNotModified 钉住协商缓存：同一条 URL 带不带 gzip
// 必须给出**同一个 ETag**，而且带 If-None-Match 时仍然只回 304。
//
// 为什么 ETag 必须与压不压无关：ETag 是"这份资源的内容指纹"。如果按编码后的
// 字节算，中间缓存就会把"gzip 版本"与"明文版本"当成两个不同的资源，
// 客户端换来换去、每次都重新下载。
func TestGzipKeepsETagAndNotModified(t *testing.T) {
	h := newAuthHarness(t)
	c := noGzipClient()

	for _, path := range []string{"/", "/app.js", "/chart.js", "/style.css"} {
		plain := fetchRaw(t, c, h.ts.URL+path, nil)
		packed := fetchRaw(t, c, h.ts.URL+path, map[string]string{"Accept-Encoding": "gzip"})
		if plain.header.Get("Etag") == "" {
			t.Fatalf("%s 没有 ETag", path)
		}
		if plain.header.Get("Etag") != packed.header.Get("Etag") {
			t.Errorf("%s 的 ETag 随 gzip 变了：%q vs %q", path, plain.header.Get("Etag"), packed.header.Get("Etag"))
		}

		notModified := fetchRaw(t, c, h.ts.URL+path, map[string]string{
			"Accept-Encoding": "gzip", "If-None-Match": packed.header.Get("Etag"),
		})
		if notModified.status != http.StatusNotModified {
			t.Fatalf("%s 带 If-None-Match 应当 304，实际 %d", path, notModified.status)
		}
		if enc := notModified.header.Get("Content-Encoding"); enc != "" {
			t.Errorf("%s 的 304 带了 Content-Encoding: %q（304 没有正文）", path, enc)
		}
		if len(notModified.body) != 0 {
			t.Errorf("%s 的 304 带了 %d 字节正文", path, len(notModified.body))
		}
	}
}

// ---------------------------------------------------------------- 6. panic 之后的 500

// TestPanicAfterPartialWriteStaysValidGzip 是"挂链位置"的守卫用例。
//
// 场景：handler 已经写出去 600 字节（于是响应头已经带着 Content-Encoding: gzip
// 发出去了），这时它 panic。recoverPanic 要再写一个 500 的错误信封。
//
// 正确的挂法（gzip 在 recoverPanic 外面）：recoverPanic 手里也是 gzip writer，
// 于是那 500 的正文**接在同一个 gzip 流里**，客户端解出来是"600 字节 + 错误信封"，
// 整条流合法。
//
// 挂反了（gzip 在 recoverPanic 里面）：wrapper 已经把 Content-Encoding: gzip
// 写进了两边共享的 header map，而 recoverPanic 手里是原始 writer —— 它会往
// gzip 数据**后面**追一段明文，客户端拿到的是坏流（gzip: invalid header）。
func TestPanicAfterPartialWriteStaysValidGzip(t *testing.T) {
	prefix := bytes.Repeat([]byte("payload-"), 75) // 600 字节
	s := newTestServer(t)
	ts := httptest.NewServer(s.withMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(prefix)
		panic("boom")
	})))
	defer ts.Close()

	got := fetchRaw(t, noGzipClient(), ts.URL+"/api/v1/anything", map[string]string{"Accept-Encoding": "gzip"})
	if enc := got.header.Get("Content-Encoding"); enc != "gzip" {
		t.Fatalf("头声明与实际不符：Content-Encoding = %q（响应已经按 gzip 开始了）", enc)
	}
	body := mustGunzip(t, "panic 之后的响应", got.body)
	if !bytes.HasPrefix(body, prefix) {
		t.Fatalf("解压出来的开头不是 panic 前写的那 %d 字节", len(prefix))
	}
	rest := body[len(prefix):]
	if !bytes.Contains(rest, []byte(`"internal"`)) {
		t.Fatalf("panic 之后的 500 没有进同一条 gzip 流：解压出来只剩 %q", rest)
	}
	var envelope map[string]any
	if err := json.Unmarshal(rest, &envelope); err != nil {
		t.Fatalf("500 的正文不是合法 JSON: %v（%q）", err, rest)
	}
	if _, ok := envelope["error"]; !ok {
		t.Fatalf("500 的正文不是错误信封: %v", envelope)
	}
}

// TestPanicBeforeWriteKeepsHeaderAndBodyConsistent 覆盖另一种 panic：还没写任何
// 东西就崩了。这时响应才刚决定（错误信封只有几十字节，按阈值本来就不压），
// 但**头与体必须一致**：要么声明 gzip 且能解开，要么不声明且能直接当 JSON 读。
func TestPanicBeforeWriteKeepsHeaderAndBodyConsistent(t *testing.T) {
	s := newTestServer(t)
	ts := httptest.NewServer(s.withMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})))
	defer ts.Close()

	got := fetchRaw(t, noGzipClient(), ts.URL+"/api/v1/anything", map[string]string{"Accept-Encoding": "gzip"})
	body := got.body
	if enc := got.header.Get("Content-Encoding"); enc != "" {
		if enc != "gzip" {
			t.Fatalf("Content-Encoding = %q", enc)
		}
		body = mustGunzip(t, "panic 响应", got.body)
	}
	var envelope map[string]any
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("头与体不一致（声明 %q，正文 %q）：%v", got.header.Get("Content-Encoding"), body, err)
	}
	if _, ok := envelope["error"]; !ok {
		t.Fatalf("不是错误信封: %v", envelope)
	}
}

// ---------------------------------------------------------------- 7. ResponseController 能穿过去

// TestResponseControllerPassesThroughGzipWriter 是 SSE 那条路的守卫。
//
// SSE 用的是 http.NewResponseController(w)，它按 FlushError() → http.Flusher →
// Unwrap() 的顺序找能力。包装器少实现一个，rc.Flush() 就会返回
// "feature not supported"，而 SSE 那边的写法是"flush 失败就断开这条连接" ——
// 表现是连上就断，不是页面卡住，但同样是坏的。
func TestResponseControllerPassesThroughGzipWriter(t *testing.T) {
	rec := httptest.NewRecorder()
	gw := &gzipResponseWriter{ResponseWriter: rec, acceptsGzip: true}

	// 编译期就钉住：ResponseController 优先找的就是这个方法名。
	var _ interface{ FlushError() error } = gw

	rc := http.NewResponseController(gw)
	if err := rc.Flush(); err != nil {
		t.Fatalf("ResponseController.Flush() = %v，期望 nil", err)
	}
	if !rec.Flushed {
		t.Fatal("ResponseController.Flush() 没有真的落到最底层 writer 上")
	}

	// 老路（直接断言 http.Flusher）也必须通。
	if _, ok := http.ResponseWriter(gw).(http.Flusher); !ok {
		t.Fatal("包装器没有提供 Flush()：w.(http.Flusher) 这条老路会断")
	}
	// 其它能力（Hijack / SetWriteDeadline）靠 Unwrap 穿过去。
	if gw.Unwrap() != http.ResponseWriter(rec) {
		t.Fatal("Unwrap() 没有返回底层 writer")
	}
}

// TestStatusWriterChainKeepsControllerAbilities 在真实链路（含 statusWriter）
// 上再走一遍：SSE 的 SetWriteDeadline 要能穿过 gzip → statusWriter 两层。
func TestStatusWriterChainKeepsControllerAbilities(t *testing.T) {
	s := newTestServer(t)
	// 结果用 channel 收：handler 在服务端 goroutine 里跑，直接读变量会撞上竞态检测。
	results := make(chan [2]error, 1)
	ts := httptest.NewServer(s.withMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc := http.NewResponseController(w)
		// 先按 SSE 的写法写一段（不被阈值挡住），再验能力穿透。
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "event: nodes\ndata: "+strings.Repeat("x", 600)+"\n\n")
		deadlineErr := rc.SetWriteDeadline(time.Now().Add(5 * time.Second))
		flushErr := rc.Flush()
		results <- [2]error{deadlineErr, flushErr}
	})))
	defer ts.Close()

	got := fetchRaw(t, noGzipClient(), ts.URL+"/", map[string]string{"Accept-Encoding": "gzip"})
	if got.header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("SSE 类型的响应应当被压，Content-Encoding = %q", got.header.Get("Content-Encoding"))
	}
	if got := mustGunzip(t, "SSE 流", got.body); !bytes.Contains(got, []byte("data: ")) {
		t.Fatalf("解出来的内容不像 SSE：%q", headOf(got, 80))
	}
	res := <-results
	if res[0] != nil {
		t.Errorf("rc.SetWriteDeadline 穿过 gzip+statusWriter 失败: %v", res[0])
	}
	if res[1] != nil {
		t.Errorf("rc.Flush 穿过 gzip+statusWriter 失败: %v", res[1])
	}
}

// ---------------------------------------------------------------- 8. 开关与池子

// TestGzipDisabledWritesPlainBytes 验证 --gzip=false 之后一个字节都不压
// （连 Vary 也不加：那时服务端对所有客户端都是一样的行为）。
func TestGzipDisabledWritesPlainBytes(t *testing.T) {
	want, err := web.FS.ReadFile("app.js")
	if err != nil {
		t.Fatalf("读取内嵌的 app.js: %v", err)
	}
	cfg := config.Default()
	cfg.Gzip = false
	h := newAuthHarnessWithConfig(t, cfg)

	got := fetchRaw(t, noGzipClient(), h.ts.URL+"/app.js", map[string]string{"Accept-Encoding": "gzip"})
	if enc := got.header.Get("Content-Encoding"); enc != "" {
		t.Fatalf("--gzip=false 之后仍然压了：Content-Encoding = %q", enc)
	}
	if hasVary(got.header, "Accept-Encoding") {
		t.Error("--gzip=false 之后不该再发 Vary: Accept-Encoding")
	}
	if len(got.body) != len(want) {
		t.Errorf("/app.js 明文 = %d 字节，期望 %d（内嵌资源应当原样发出）", len(got.body), len(want))
	}
}

// TestGzipWriterPoolIsReusable 验证池子确实在复用，而且**没有把 writer 留在里面**。
//
// 归还前不 Reset(io.Discard) 的话，池子里会一直攥着一条已经结束的请求的
// ResponseWriter（连接、缓冲都在），既是内存泄漏，也可能被下一个请求误用。
func TestGzipWriterPoolIsReusable(t *testing.T) {
	var sink bytes.Buffer
	gz := acquireGzip(&sink)
	if _, err := io.WriteString(gz, "hello"); err != nil {
		t.Fatalf("写: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("关闭: %v", err)
	}
	releaseGzip(gz)

	// 再借一次：应当是一个可用的 writer，而且不再指向上面那个 buffer。
	var other bytes.Buffer
	gz2 := acquireGzip(&other)
	if _, err := io.WriteString(gz2, "world"); err != nil {
		t.Fatalf("写: %v", err)
	}
	if err := gz2.Close(); err != nil {
		t.Fatalf("关闭: %v", err)
	}
	releaseGzip(gz2)

	zr, err := gzip.NewReader(bytes.NewReader(other.Bytes()))
	if err != nil {
		t.Fatalf("复用后的 writer 输出不是合法 gzip: %v", err)
	}
	out, err := io.ReadAll(zr)
	if err != nil || string(out) != "world" {
		t.Fatalf("复用后的 writer 输出 = %q（err=%v）", out, err)
	}
}

// ---------------------------------------------------------------- 9. 访问日志的字节数

// logCapture 记下服务端写出的访问日志属性。
type logCapture struct {
	mu      sync.Mutex
	records []map[string]any
}

func (c *logCapture) Enabled(context.Context, slog.Level) bool { return true }

func (c *logCapture) Handle(_ context.Context, r slog.Record) error {
	attrs := map[string]any{}
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.Any()
		return true
	})
	c.mu.Lock()
	c.records = append(c.records, attrs)
	c.mu.Unlock()
	return nil
}

func (c *logCapture) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *logCapture) WithGroup(string) slog.Handler      { return c }

// bytesFor 取出某条路径的访问日志里记录的 bytes。
func (c *logCapture) bytesFor(path string) (int64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, attrs := range c.records {
		if attrs["path"] == path {
			n, ok := attrs["bytes"].(int64)
			return n, ok
		}
	}
	return 0, false
}

// TestAccessLogCountsCompressedBytes 钉住挂链位置的另一半：日志里的 bytes
// 必须是**压缩后**、真正上网线的字节数。
//
// 这条只有在 gzip 位于 logRequests 里面时才成立。挂反了（gzip 在外）时，
// 日志会一直报明文大小 —— 而 gzip 的全部意义就是让这个数变小，
// 两者一差就是几倍，运维看着日志根本判断不出压缩有没有生效。
func TestAccessLogCountsCompressedBytes(t *testing.T) {
	plainSize := func() int {
		data, err := web.FS.ReadFile("app.js")
		if err != nil {
			t.Fatalf("读取内嵌的 app.js: %v", err)
		}
		return len(data)
	}()

	logs := &logCapture{}
	s := New(config.Default(), openTestDB(t), slog.New(logs), time.UTC)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	c := noGzipClient()
	packed := fetchRaw(t, c, ts.URL+"/app.js", map[string]string{"Accept-Encoding": "gzip"})
	if packed.header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("/app.js 应当被压，Content-Encoding = %q", packed.header.Get("Content-Encoding"))
	}

	// 等日志落下来（日志是在 handler 返回之后写的）。
	var logged int64
	var ok bool
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if logged, ok = logs.bytesFor("/app.js"); ok {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !ok {
		t.Fatal("没有等到 /app.js 的访问日志")
	}
	if logged != int64(len(packed.body)) {
		t.Fatalf("访问日志里的 bytes = %d，网线上实际是 %d 字节（%d 是明文大小）",
			logged, len(packed.body), plainSize)
	}
	t.Logf("/app.js：明文 %d 字节 → 网线 %d 字节，访问日志记录的也是 %d", plainSize, len(packed.body), logged)
}
