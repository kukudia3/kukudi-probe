package server

import (
	"net/http"
	"strconv"
	"testing"
)

// TestHeadRequestsNeverMismatch 钉住 HEAD 的行为。
//
// HEAD 没有正文，而"有没有正文"正是压缩中间件判断长度的依据，所以这条路的
// 结果必须写清楚（它也确实分两种情况）：
//
//   - 静态资源：http.ServeContent 对 HEAD 干脆不写 body（它自己跳过了拷贝），
//     中间件永远等不到"够阈值"的那一刻，于是按"不压"收尾 —— 响应里是明文的
//     Content-Length，**不能**再声明 Content-Encoding: gzip（那就是错配）。
//   - 接口：handler 照常写 body，net/http 把字节吃掉；中间件看到的是完整的
//     响应，于是与 GET 一样声明 gzip（这正是 RFC 9110 要的："HEAD 的头应当与
//     GET 相同"）。
//
// 两种情况都必须满足的两条：正文长度为 0，而且 **Vary 必须在** —— 少了它，
// 共享缓存会把 gzip 版本发给不支持 gzip 的客户端。
func TestHeadRequestsNeverMismatch(t *testing.T) {
	h := newAuthHarness(t)
	c := noGzipClient()
	node, _ := createTestNode(t, h.srv, "head-01")
	cookie := sessionCookie(t, h)

	plainAsset := fetchRaw(t, c, h.ts.URL+"/app.js", nil)
	cases := []struct {
		name     string
		path     string
		headers  map[string]string
		wantGzip bool // 期望的 Content-Encoding: gzip
		// wantCL 非空时断言 Content-Length 等于它。
		wantCL string
	}{
		{
			name: "静态资源（ServeContent 对 HEAD 不写 body）", path: "/app.js",
			headers: map[string]string{"Cookie": cookie}, wantGzip: false,
			wantCL: strconv.Itoa(len(plainAsset.body)),
		},
		{
			name: "接口（handler 照常写 body，net/http 吃掉）",
			path: "/api/v1/nodes/" + strconv.FormatInt(node.ID, 10),
			// 期望与 GET+gzip 一致：声明 gzip（那时不能有明文 Content-Length）。
			headers: map[string]string{"Cookie": cookie}, wantGzip: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			headers := map[string]string{"Accept-Encoding": "gzip"}
			for k, v := range tc.headers {
				headers[k] = v
			}
			got := fetchRawMethod(t, c, http.MethodHead, h.ts.URL+tc.path, headers)
			if got.status != http.StatusOK {
				t.Fatalf("状态码 = %d", got.status)
			}
			if len(got.body) != 0 {
				t.Errorf("HEAD 不该有正文，实际 %d 字节", len(got.body))
			}
			if !hasVary(got.header, "Accept-Encoding") {
				t.Error("HEAD 的响应同样必须带 Vary: Accept-Encoding")
			}
			enc := got.header.Get("Content-Encoding")
			if tc.wantGzip {
				if enc != "gzip" {
					t.Errorf("Content-Encoding = %q，期望 gzip（与 GET 一致）", enc)
				}
				if cl := got.header.Get("Content-Length"); cl != "" {
					t.Errorf("声明了 gzip 就不该再声明明文的 Content-Length，实际 %q", cl)
				}
				return
			}
			if enc != "" {
				t.Errorf("Content-Encoding = %q，期望空（这条路上没有任何东西被压过）", enc)
			}
			if cl := got.header.Get("Content-Length"); cl != tc.wantCL {
				t.Errorf("Content-Length = %q，期望 %q（与不带 gzip 的 GET 一致）", cl, tc.wantCL)
			}
		})
	}
}
