package server

import (
	"compress/gzip"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// gzipMinSize 是"小响应不压"的阈值（字节）。
//
// 为什么不"能压就压"：gzip 自己有定长开销（10 字节头 + 8 字节尾），每 flush 一次
// 还要多一个 5 字节的空块，去掉 Content-Length 之后 chunked 每块还要几字节头尾。
// 面板上大量响应（/healthz、错误信封、字段少的单节点 JSON）只有一两百字节，
// 压完反而更大。512 是"压了稳赚"的下界：真到 512 字节的文本，gzip 通常能压掉
// 一半以上，远大于上面那几十字节固定开销。
//
// 这条阈值**不适用于 SSE**：流的总长度事先不可知，而且它天然是"每帧几百字节、
// 一天几十万帧"的形态（见 gzipResponseWriter.streaming）。
const gzipMinSize = 512

// gzipContentTypes 是允许压缩的媒体类型白名单。
//
// 为什么用白名单而不是"除了二进制都压"：漏掉一个该压的类型，代价只是流量
// （看得见、随时能补）；误压一个不该压的类型，代价可能是**坏掉**（已经在
// Content-Encoding 里声明过的内容被二次压缩、Range 语义错乱、客户端按
// Content-Length 校验收到的字节数）。白名单是唯一不会静默翻车的那一边。
//
// 面板实际会产生的内容都在这 7 种里：前端三件套（html/css/js）、接口的 JSON、
// SSE 的 event-stream、以及内嵌的 svg 图标。
var gzipContentTypes = map[string]bool{
	"text/event-stream":      true,
	"application/json":       true,
	"text/html":              true,
	"text/css":               true,
	"application/javascript": true,
	"text/javascript":        true,
	"image/svg+xml":          true,
}

// gzipPool 复用 *gzip.Writer。
//
// 每个响应都可能走到这里，而 gzip.Writer 里带着一个几十 KB 的 deflate 窗口：
// 每个请求新建一个，等于给每个响应都造一个短命大对象。等级用默认（-1 → 6）：
// 面板的流量是"每秒几十万字节的 JSON 与文本"，默认档是 CPU 与字节数的标准折中，
// 换 BestCompression 多花的 CPU 换不来成比例的字节，换 BestSpeed 又省不下多少。
var gzipPool = sync.Pool{
	New: func() any { return gzip.NewWriter(io.Discard) },
}

func acquireGzip(w io.Writer) *gzip.Writer {
	gz := gzipPool.Get().(*gzip.Writer)
	gz.Reset(w)
	return gz
}

// releaseGzip 归还前必须先 Reset(io.Discard)：池子里绝不能留下 ResponseWriter
// 的引用 —— 那会把一条已经结束的请求（连同它的连接与缓冲）一直吊在进程里。
func releaseGzip(gz *gzip.Writer) {
	gz.Reset(io.Discard)
	gzipPool.Put(gz)
}

// compress 给响应体做 gzip 压缩。
//
// 只在"客户端声明支持 + 媒体类型在白名单里 + 状态码合适 + 长度够阈值"时才压；
// 压与不压都按媒体类型补 Vary: Accept-Encoding。
//
// 挂在 logRequests 里、recoverPanic 外（见 withMiddleware 的说明）：
//   - 在 recoverPanic 外：panic 之后那个 500 也经过同一个 writer；
//   - 在 logRequests 里：访问日志里的 bytes 数的是真正上网线的压缩后字节。
func (s *Server) compress(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 协议升级（WebSocket）：整条放行，把**原始** writer 交给 handler。
		// 101 之后是双向裸字节流，不是"响应体"：包一层既没有意义，又可能把
		// Hijack 拿走的那条连接上的字节也当成响应去处理。
		if isUpgradeRequest(r) {
			next.ServeHTTP(w, r)
			return
		}
		gw := &gzipResponseWriter{ResponseWriter: w, acceptsGzip: acceptsGzip(r)}
		// finish 必须跑在 handler 返回之后：小响应要等"到底写了多少"才能定，
		// 而 gzip 的尾（CRC32 + 原始长度）也只能在这里补。
		defer gw.finish()
		next.ServeHTTP(gw, r)
	})
}

// gzipResponseWriter 是一个"边写边决定"的 gzip 包装器。
//
// 为什么不能一开始就决定压不压：Content-Type 与状态码都是 handler 写的时候才定，
// 长度更是要等写完才知道；而一旦把 header 交给底层，Content-Encoding 与
// Content-Length 就再也改不了了。所以决定发生在**第一次 WriteHeader/Write**
// 以及 handler 返回时（finish），在那之前的 header 一律压着不发，body 攒在 buf 里。
//
// 状态机（decided 之前 header 不往下发）：
//
//	未决定 + 客户端不支持 / 类型不在白名单 / 状态码排除 / 已有 Content-Encoding
//	                                        → 立刻放弃压缩，原样透传
//	未决定 + text/event-stream              → 立刻压（流不受阈值约束）
//	未决定 + 其它可压类型                    → 攒到 gzipMinSize 字节再决定压
//	未决定 + 攒不到阈值就写完了              → 原样写出（小响应不压）
//	已决定压                                → 全部走 gz
//
// 它还实现了 FlushError / Flush / Unwrap，好让 http.ResponseController
// （SSE 用的那套）能穿过这一层找到底层真正的 flush 与 SetWriteDeadline。
type gzipResponseWriter struct {
	http.ResponseWriter

	// acceptsGzip 是客户端有没有声明 Accept-Encoding: gzip。
	//
	// 不支持时也照样包这一层：Vary 要看响应的媒体类型，而那个只有 handler
	// 写了 header 之后才知道（见 decide）。
	acceptsGzip bool

	decided  bool // 已经定下压不压
	compress bool // decided 之后有效

	status      int  // 记下的状态码（决定时要用；0 = 还没写过）
	wroteHeader bool // 底层已经收到过 WriteHeader

	// buf 暂存"还没决定"的响应体，最多 gzipMinSize 字节。
	buf []byte
	// gz 只在 compress 为真时非 nil。
	gz *gzip.Writer
}

func (w *gzipResponseWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	if w.decided {
		w.writeHeader()
		return
	}
	switch {
	case !w.mayCompress(code):
		// 明确不能压：立刻把 header 放下去 —— 二进制大文件不该被我们多留一手。
		w.decide(false)
	case w.streaming():
		// SSE：总长度事先不可知，不受阈值约束。
		w.decide(true)
	default:
		// 可压，但还不知道要写多少：header 先压住（这是唯一还能改
		// Content-Encoding 的时刻），等长度够阈值或 handler 结束再定。
	}
}

func (w *gzipResponseWriter) Write(b []byte) (int, error) {
	// 没有显式状态码时，第一次写就是隐式 200：决定必须在这里做，不能更晚。
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if !w.decided {
		if len(w.buf)+len(b) < gzipMinSize {
			// 还没到阈值：先攒着（header 也还压着，攒够了还能改成 gzip）。
			w.buf = append(w.buf, b...)
			return len(b), nil
		}
		// 够阈值了：连攒下的前缀一起压。
		w.decide(true)
		if len(w.buf) > 0 {
			if _, err := w.gz.Write(w.buf); err != nil {
				w.buf = nil
				return 0, err
			}
			w.buf = nil
		}
	}
	if !w.compress {
		return w.ResponseWriter.Write(b)
	}
	n, err := w.gz.Write(b)
	if err != nil {
		return n, err
	}
	// 每写完一段就把 gzip 的缓冲推平。这一步**不能省**：flate 会把不足一个块的
	// 输入攒在窗口里，而 SSE 一帧只有几百字节 —— 不 Flush 的表现是"连接活着、
	// 一个字节都不到"，页面上就是整个面板卡死，且连接看上去完全正常。
	if err := w.gz.Flush(); err != nil {
		return n, err
	}
	// 再把底层的缓冲推出去：只 Flush gzip 还不够，数据会停在 net/http 那 2 KB
	// 缓冲里，而 SSE 的每一条事件都必须立刻到达（心跳也是这么发的）。
	if err := w.flush(); err != nil {
		return n, err
	}
	return n, nil
}

// FlushError 是 http.ResponseController 优先找的那个方法（它先找 FlushError()，
// 再找 http.Flusher，最后才顺着 Unwrap 往下找）。
//
// SSE 那边走的就是 ResponseController：只实现 Flush() 也能被找到，但这里仍然
// 显式提供 FlushError —— 它能把"gzip 的缓冲"和"底层的缓冲"两件事一起做掉，
// 而底层的 Flush() 是没法报告错误的。
func (w *gzipResponseWriter) FlushError() error {
	if !w.decided {
		// handler 明确要求"现在就发"：这时候不能等阈值了。SSE 照压（它不受阈值
		// 约束），其余类型按不压处理 —— 已经攒下的字节原样写出去，后面也走明文。
		w.decide(w.streaming())
	}
	if err := w.drainBuf(); err != nil {
		return err
	}
	if w.gz != nil {
		if err := w.gz.Flush(); err != nil {
			return err
		}
	}
	return w.flush()
}

// Flush 让 w.(http.Flusher) 这条老路也通（不是每个调用方都给 ResponseController
// 面子）。与 net/http 自己的 response 一样，错误交给 FlushError 那条路报告。
func (w *gzipResponseWriter) Flush() { _ = w.FlushError() }

// Unwrap 让 http.ResponseController 的其它能力（Hijack / SetWriteDeadline）
// 也能穿过这一层。
func (w *gzipResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// decide 定下"这条响应压不压"，并把该改的 header 改好之后立刻把 header 发出去。
//
// 只能在底层 WriteHeader 之前调用 —— 调完之后 Content-Encoding 就定死了。
func (w *gzipResponseWriter) decide(compress bool) {
	if w.decided {
		return
	}
	w.decided = true
	h := w.Header()
	// Vary 只看媒体类型：只要它是可压的就必须加 —— 哪怕这一次没压、或者客户端
	// 压根没声明 gzip。否则中间缓存可能把 gzip 版本发给不支持 gzip 的客户端
	// （页面直接白屏），而那时服务端已经无从补救了。
	if compressibleContentType(h.Get("Content-Type")) {
		addVaryAcceptEncoding(h)
	}
	w.compress = compress
	if compress {
		// 压了就不能再声明明文的长度：Content-Length 的语义是"编码之后的字节数"。
		h.Del("Content-Length")
		h.Set("Content-Encoding", "gzip")
		w.gz = acquireGzip(w.ResponseWriter)
	}
	w.writeHeader()
}

// writeHeader 把 header 真正交给底层（幂等）。
func (w *gzipResponseWriter) writeHeader() {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	code := w.status
	if code == 0 {
		code = http.StatusOK
	}
	w.ResponseWriter.WriteHeader(code)
}

// finish 在 handler 返回之后收尾：补上还没做的决定、把攒下的小响应原样写出去、
// 给 gzip 流补上尾（CRC32 + 原始长度）并把 writer 还给池子。
func (w *gzipResponseWriter) finish() {
	if !w.decided {
		// 到 handler 返回都没够阈值（或者根本没有 body）：小响应不压，原样发。
		w.decide(false)
	}
	// 无论写成功没有，writer 都必须还回池子（否则池子会随着断开连接慢慢瘪掉）。
	_ = w.drainBuf()
	w.closeGzip()
}

// drainBuf 把"还没决定时攒下的字节"交给正确的去处：压了就往 gz 里写，
// 没压就原样写到底层 writer。调用前 header 必须已经定下来了。
func (w *gzipResponseWriter) drainBuf() error {
	if len(w.buf) == 0 {
		return nil
	}
	body := w.buf
	w.buf = nil
	if w.compress {
		_, err := w.gz.Write(body)
		return err
	}
	_, err := w.ResponseWriter.Write(body)
	return err
}

// closeGzip 补上 gzip 的尾（少了它客户端会报 unexpected EOF）并把 writer 还回池子。
func (w *gzipResponseWriter) closeGzip() {
	if w.gz == nil {
		return
	}
	_ = w.gz.Close()
	releaseGzip(w.gz)
	w.gz = nil
}

// flush 把底层 writer 的缓冲也推出去。
//
// 用 ResponseController 而不是 w.ResponseWriter.(http.Flusher)：中间还夹着
// statusWriter 这样的包装器，ResponseController 会顺着 Unwrap 一路找下去。
// 底层不支持 flush 时它返回 ErrNotSupported，这里原样往上抛给调用方决定
// （SSE 那边就是"flush 不了就断开这条连接"）。
func (w *gzipResponseWriter) flush() error {
	return http.NewResponseController(w.ResponseWriter).Flush()
}

// mayCompress 报告"其它条件都满足时，这条响应现在就可以压"。
func (w *gzipResponseWriter) mayCompress(code int) bool {
	if !w.acceptsGzip {
		return false
	}
	// handler 自己已经设了 Content-Encoding（比如它自己压过了）：绝不覆盖成 gzip。
	if w.Header().Get("Content-Encoding") != "" {
		return false
	}
	if !compressibleStatus(code) {
		return false
	}
	return compressibleContentType(w.Header().Get("Content-Type"))
}

// streaming 报告这是不是一条"边写边发"的流（只有 SSE）。
//
// 它与别的响应有两处不同：不受 gzipMinSize 约束（总长度事先不可知），并且每次写
// 都要真的推到网线上。
func (w *gzipResponseWriter) streaming() bool {
	return mediaType(w.Header().Get("Content-Type")) == "text/event-stream"
}

// compressibleContentType 报告这个 Content-Type 是不是白名单里可压的类型。
func compressibleContentType(ct string) bool {
	return gzipContentTypes[mediaType(ct)]
}

// compressibleStatus 报告这个状态码的响应能不能压。
//
// 三种明确排除：204（没有正文）、206（部分内容 —— 压了与 Content-Range 就对不上）、
// 304（协商缓存，本来就没有正文，而且 net/http 会主动删掉 Content-Encoding）。
// 1xx 是过渡响应，同样不压。
func compressibleStatus(code int) bool {
	switch code {
	case http.StatusNoContent, http.StatusPartialContent, http.StatusNotModified:
		return false
	}
	return code >= 200
}

// mediaType 取出 Content-Type 的媒体类型部分（去掉 charset 之类的参数、转小写）。
func mediaType(ct string) string {
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return strings.ToLower(strings.TrimSpace(ct))
}

// addVaryAcceptEncoding 往 Vary 里补一个 Accept-Encoding（已经有了就不重复加）。
func addVaryAcceptEncoding(h http.Header) {
	for _, v := range h.Values("Vary") {
		for _, item := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(item), "Accept-Encoding") {
				return
			}
		}
	}
	h.Add("Vary", "Accept-Encoding")
}

// acceptsGzip 解析 Accept-Encoding：认 gzip（x-gzip 是它的历史别名）与 *，
// 并尊重 q=0（RFC 9110：q=0 表示"明确不要"，而不是"随便"）。
func acceptsGzip(r *http.Request) bool {
	var wildcard bool
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		enc, params, _ := strings.Cut(part, ";")
		switch strings.ToLower(strings.TrimSpace(enc)) {
		case "gzip", "x-gzip":
			// 具体写法优先于通配：gzip;q=0, * 的意思是"就是不要 gzip"。
			return !zeroQuality(params)
		case "*":
			wildcard = !zeroQuality(params)
		}
	}
	return wildcard
}

// zeroQuality 判断参数串里有没有 q=0（q=0.000 也算）。
func zeroQuality(params string) bool {
	for _, p := range strings.Split(params, ";") {
		k, v, ok := strings.Cut(p, "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(k), "q") {
			continue
		}
		q, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return err == nil && q == 0
	}
	return false
}

// isUpgradeRequest 报告这是不是一条协议升级请求（WebSocket）。
//
// 两个头都要看：RFC 9110 要求 Connection 里出现 upgrade 这个 token（可能是逗号
// 列表里的一项，例如 "keep-alive, Upgrade"），Upgrade 头本身则是另一条独立线索，
// 两者任一命中就整条放行。宁可漏判成"没压"（代价是流量），也不要把一条升级请求
// 包进 wrapper（代价是连不上）。
func isUpgradeRequest(r *http.Request) bool {
	if r.Header.Get("Upgrade") != "" {
		return true
	}
	for _, part := range strings.Split(r.Header.Get("Connection"), ",") {
		if strings.EqualFold(strings.TrimSpace(part), "upgrade") {
			return true
		}
	}
	return false
}
