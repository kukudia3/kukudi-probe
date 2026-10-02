package server

import (
	"context"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"time"
)

// withMiddleware 的层次（由外到内）：
// 解析来源 IP → 日志 → gzip → panic 恢复 → 安全响应头 → 请求体上限。
//
// gzip 为什么正好夹在这两层之间（两个方向都有具体理由，不是随手放的）：
//
//   - 在 logRequests **里面**：这样访问日志里的 bytes 数的是真正上网线的
//     **压缩后**字节 —— 不然这条日志会一直报明文大小，而 gzip 的全部意义就是
//     让上网线的字节变少，两者一差就是 2~8 倍。
//   - 在 recoverPanic **外面**：panic 之后那个 500 也走同一个 writer。
//     反过来的话，wrapper 已经把 Content-Encoding: gzip 写进了两边共享的
//     header map，而 recoverPanic 手里是原始 writer —— 它会写出
//     "头声明 gzip、体是明文"的错配，客户端拿到的是坏掉的流（见
//     compress_test.go 的 TestPanicAfterPartialWriteStaysValidGzip）。
func (s *Server) withMiddleware(next http.Handler) http.Handler {
	inner := s.recoverPanic(s.securityHeaders(s.limitBody(next)))
	if s.cfg.Gzip {
		inner = s.compress(inner)
	}
	return s.resolveClientIP(s.logRequests(inner))
}

type clientIPKey struct{}

// clientIP 返回请求来源 IP。
//
// 默认只认 TCP 连接的对端地址；只有来自 --trusted-proxy 网段的请求，
// 才会采信 X-Forwarded-For（否则任何人加一个头就能伪装来源、绕过限流）。
func clientIP(r *http.Request) string {
	if ip, ok := r.Context().Value(clientIPKey{}).(string); ok {
		return ip
	}
	return remoteIP(r)
}

func (s *Server) resolveClientIP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := remoteIP(r)
		if len(s.trustedProxies) > 0 && ipInNets(ip, s.trustedProxies) {
			if forwarded := forwardedIP(r, s.trustedProxies); forwarded != "" {
				ip = forwarded
			}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), clientIPKey{}, ip)))
	})
}

// forwardedIP 从 X-Forwarded-For / X-Real-IP 里取出真实客户端地址。
//
// 策略：**只采信最近一跳代理追加的那一条**（列表最右边），而且它本身不能落在
// 可信网段里。
//
// 为什么不"继续往左找"：左边每一条都是客户端可以随便写的。一个真实攻击是——
// 攻击者自己就是可信网段里的主机（或伪造了最右一条），一路向左找就会把客户端
// 伪造的地址当成真实地址，于是按 IP 计的登录限流被"换个假 IP 就是一个新桶"绕过。
// 只认最右一条时：公网客户端伪造的前缀一定被忽略（代理追加的真实地址在最右），
// 而"最右一条也在可信网段"意味着链路里还有别的可信代理/或对端本身可疑，
// 此时无法判断，返回空串让调用方退回直连地址。
//
// 单反代（本项目推荐的 Caddy/nginx 部署）下这正好是需要的语义。
func forwardedIP(r *http.Request, trusted []*net.IPNet) string {
	xff := r.Header.Get("X-Forwarded-For")
	if xff == "" {
		ip := net.ParseIP(strings.TrimSpace(r.Header.Get("X-Real-IP")))
		if ip == nil || ipInNets(ip.String(), trusted) {
			return ""
		}
		return ip.String()
	}

	parts := strings.Split(xff, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		ip := net.ParseIP(strings.TrimSpace(parts[i]))
		if ip == nil {
			continue // 跳过空项/垃圾项，继续看更靠右的合法值
		}
		if ipInNets(ip.String(), trusted) {
			return ""
		}
		return ip.String()
	}
	return ""
}

func ipInNets(ipStr string, nets []*net.IPNet) bool {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// securityHeaders 设置安全响应头。CSP 不含 unsafe-inline：前端没有内联脚本/样式。
//
// X-Robots-Tag 与 index.html 的 <meta name="robots">、/robots.txt 是**三道一起**的：
// 面板一旦被搜索引擎收录，任何人搜一下就能看到节点名称、用量与价格。
// meta 标签只对"渲染 HTML"的爬虫有效，robots.txt 只对守规矩的爬虫有效，
// 而响应头对**每一个**响应都成立（包括 /api/ 的 JSON 与静态资源）。
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	csp := "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
		"connect-src 'self'; font-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("X-Robots-Tag", "noindex, nofollow, noarchive")
		if s.cfg.TLSCert != "" {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

// limitBody 限制请求体大小，避免超大请求占用内存。
func (s *Server) limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		}
		next.ServeHTTP(w, r)
	})
}

// recoverPanic 捕获处理器里的 panic：记录堆栈，返回 500，绝不让进程退出。
func (s *Server) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			s.log.Error("处理请求时发生 panic",
				"err", rec,
				"method", r.Method,
				"path", r.URL.Path,
				"stack", string(debug.Stack()),
			)
			if strings.HasPrefix(r.URL.Path, apiPrefix) {
				s.writeJSON(w, http.StatusInternalServerError, errorEnvelope{Error: apiError{
					Code:    "internal",
					Message: "服务端内部错误",
				}})
				return
			}
			http.Error(w, "服务端内部错误", http.StatusInternalServerError)
		}()
		next.ServeHTTP(w, r)
	})
}

// logRequests 记录访问日志：不含查询串（避免把 Token 之类写进日志），/healthz 降到 debug。
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)

		if sw.status == 0 {
			sw.status = http.StatusOK
		}
		attrs := []any{
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.status,
			"bytes", sw.bytes,
			"ms", float64(time.Since(start).Microseconds()) / 1000,
			"ip", clientIP(r),
		}
		switch {
		case r.URL.Path == healthzPath:
			s.log.Debug("请求", attrs...)
		case sw.status >= 500:
			s.log.Warn("请求", attrs...)
		default:
			s.log.Info("请求", attrs...)
		}
	})
}

// remoteIP 只取连接来源地址；未配置 --trusted-proxy 时不信任任何转发头。
func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// statusWriter 记录状态码与响应字节数。
//
// 它实现 Unwrap，因此 http.ResponseController 仍能找到底层的 Flusher
// ——Phase 5 的 SSE 需要这个能力。
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
