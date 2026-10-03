package server

// 第四轮「量级测量」补口之一：ROUND4-GAPS 的 02-4（M3 的数字）与 01-3（压制曲线形状）。
//
// 这两个用例都是**测量**，不是"修复验证"：它们只打印原始数字并做几条"防止测量方法
// 整体跑偏"的宽区间断言。判据写在每段的注释里，报告见
// D:\DEEPSEEK\_audit\ROUND4-GAPS-MEASURE.md。
//
// 现场（与第二轮同一形状）：真 SQLite 文件 + 真 HTTP 端口（httptest，loopback）
// + 60 个节点 + 1 个探测目标 + 每节点 59 行探测样本（1 分钟一行，正好铺满一小时）。
//
// 运行：
//
//	go test ./internal/server/ -run 'TestMeasure' -count=1 -v

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"runtime/metrics"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"probe/internal/config"
	"probe/internal/protocol"
	"probe/internal/store"
)

// measureRowsPerNode 是每个节点在一小时窗口里播撒的探测行数。
//
// 59 而不是 60：行落在 [end-3540, end-60]（步长 60 秒），整段都在桶宽 1 秒的
// 3600 格里，而且**离窗口两边各有 60 秒余量** —— 请求时刻与播撒时刻差几秒也不会
// 让某一行掉出窗口，三次重复才量得到同一个字节数。ping_samples_1m 是"每分钟一行"，
// 所以 59 行也已经接近"1 个目标 1 小时"的真实上界（60 行）。
const measureRowsPerNode = 59

// measureFixture 是一个测量现场。
type measureFixture struct {
	s        *Server
	ts       *httptest.Server
	db       *store.DB
	path     string
	nodes    []store.Node
	targetID int64
	rows     int
}

// measureConfig 返回测量用的服务端参数：关掉汇率出网、放宽 Agent 连接上限。
func measureConfig(nodeCount int, gzip bool) config.Server {
	cfg := config.Default()
	cfg.Gzip = gzip
	// 汇率是唯一会主动出网的功能（每天一次）。测量期间不希望有任何外网抖动，
	// 而它只影响"剩余价值折合人民币"这一个显示字段，与本轮要量的字节数无关。
	cfg.FX = false
	// 所有 Agent 都是 127.0.0.1（同一来源 IP），默认每 IP 20 条会挡住第 21 条 ——
	// 那是另一条防线，不是本轮要量的东西。
	cfg.AgentMaxPerIP = 4*nodeCount + 16
	cfg.AgentMaxConns = 4*nodeCount + 16
	return cfg
}

// newMeasureFixture 建一个真 SQLite + 真 HTTP 端口的现场。
//
// rowsPerNode = 0 表示**不播撒探测数据**（用来量"整段全是 null"的响应基线）。
func newMeasureFixture(t *testing.T, nodeCount, targetCount, rowsPerNode int, gzip bool) *measureFixture {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "probe.db")
	db, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	s := New(measureConfig(nodeCount, gzip), db, slog.New(slog.DiscardHandler), time.UTC)
	// 第二轮量 M3 时用的是"未登录访客"：这里打开访客开关，走同一条路径。
	if err := s.setGuestAccess(ctx, true); err != nil {
		t.Fatalf("打开访客开关: %v", err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	f := &measureFixture{s: s, ts: ts, db: db, path: path}

	for i := 0; i < nodeCount; i++ {
		node, _, err := db.CreateNode(ctx, store.NewNode{
			Name: fmt.Sprintf("m-%03d", i+1), IntervalSec: 1, TrafficWarnPct: 80, ResetDay: 1,
		}, time.Now())
		if err != nil {
			t.Fatalf("建节点 %d: %v", i+1, err)
		}
		f.nodes = append(f.nodes, node)
	}

	targets := make([]store.PingTarget, 0, targetCount)
	for i := 0; i < targetCount; i++ {
		targets = append(targets, store.PingTarget{
			Label: fmt.Sprintf("t-%02d", i), Type: protocol.PingTypeICMP,
			Host: fmt.Sprintf("10.9.%d.%d", i/250, i%250+1), Enabled: true,
		})
	}
	saved, err := db.SetPingSettings(ctx, targets, 60)
	if err != nil {
		t.Fatalf("配置探测目标: %v", err)
	}
	if len(saved.Targets) != targetCount {
		t.Fatalf("探测目标数 = %d，期望 %d", len(saved.Targets), targetCount)
	}
	f.targetID = saved.Targets[0].ID

	if rowsPerNode > 0 {
		f.rows = seedMeasurePings(t, db, f.nodes, saved.Targets, rowsPerNode)
	}
	return f
}

// seedMeasurePings 播撒探测行并返回总行数。
//
// 时间锚点与 /overview 的窗口算法对齐（见 overview.go）：bucket_sec = 1 时
// end = 现在向下取整到秒、start = end − 3600，桶号 = ts − start。行落在
// end−60×k（k = 1..rows），于是桶号 = 3600−60k ∈ [60, 3540]，整段留在窗口里。
func seedMeasurePings(t *testing.T, db *store.DB, nodes []store.Node, targets []store.PingTarget, rows int) int {
	t.Helper()
	return seedMeasurePingsSpread(t, db, nodes, targets, rows, 60)
}

// seedMeasurePingsSpread 按 stepSec 的步长从"现在"往回铺 count 行。
func seedMeasurePingsSpread(t *testing.T, db *store.DB, nodes []store.Node, targets []store.PingTarget, count, stepSec int) int {
	t.Helper()
	ctx := context.Background()
	end := time.Now().Unix()
	buckets := make([]store.PingBucket, 0, 20000)
	total := 0
	flush := func() {
		if len(buckets) == 0 {
			return
		}
		if err := db.UpsertPingBuckets(ctx, buckets); err != nil {
			t.Fatalf("播撒探测行: %v", err)
		}
		total += len(buckets)
		buckets = buckets[:0]
	}
	for _, n := range nodes {
		for _, tg := range targets {
			for k := 1; k <= count; k++ {
				ts := end - int64(k)*int64(stepSec)
				avg := 12.5 + float64(k%7)*0.5
				buckets = append(buckets, store.NewPingBucket(n.ID, tg.ID, ts, avg, avg-2, avg+5, 0))
				if len(buckets) >= 20000 {
					flush()
				}
			}
		}
	}
	flush()
	return total
}

// measureHTTPClient 返回一个"不自动加 Accept-Encoding、不缓冲整段响应"的客户端。
//
// 两处都必须显式关掉，否则量出来的数不是被测对象：
//   - DisableCompression：Go 默认会给请求加 Accept-Encoding: gzip 并**自动解压**，
//     那样量到的是解压后的字节，压缩路径的开销被藏起来了；
//   - 读响应体一律用 io.Copy(io.Discard)，不用 io.ReadAll：ReadAll 会按增长重新
//     分配缓冲区（2.2 MB 的响应要分配约两倍的量），把**客户端**的分配算进服务端账。
func measureHTTPClient(maxConns int) *http.Client {
	tr := &http.Transport{
		DisableCompression:    true,
		MaxIdleConns:          4 * maxConns,
		MaxIdleConnsPerHost:   4 * maxConns,
		IdleConnTimeout:       30 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	return &http.Client{Transport: tr, Timeout: 2 * time.Minute}
}

// measureAdminClient 在 fixture 上初始化管理员并返回带 Cookie 的客户端。
func measureAdminClient(t *testing.T, f *measureFixture) *http.Client {
	t.Helper()
	ctx := context.Background()
	if err := f.s.auth.EnsureSetupCode(ctx); err != nil {
		t.Fatalf("生成初始化码: %v", err)
	}
	code := f.s.auth.setupCode
	if code == "" {
		t.Fatal("初始化码为空")
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("创建 Cookie jar: %v", err)
	}
	client := measureHTTPClient(64)
	client.Jar = jar
	body := fmt.Sprintf(`{"code":%q,"username":"admin","password":"a-very-good-password"}`, code)
	resp, err := client.Post(f.ts.URL+"/api/v1/setup", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("初始化管理员: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("初始化管理员失败: HTTP %d %s", resp.StatusCode, raw)
	}
	return client
}

// measureGet 发一次 GET，把响应体计数丢掉，返回状态码、字节数、耗时与响应头。
//
// acceptEncoding 为空表示不带这个头（明文）；"gzip" 表示声明可压缩
// （DisableCompression 之下客户端**不会**自动解压，量到的是压缩后的字节）。
func measureGet(client *http.Client, url, acceptEncoding string) (int, int64, time.Duration, http.Header, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return 0, 0, 0, nil, err
	}
	if acceptEncoding != "" {
		req.Header.Set("Accept-Encoding", acceptEncoding)
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, 0, time.Since(start), nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	var counter countingWriter
	if _, err := io.Copy(&counter, resp.Body); err != nil {
		return resp.StatusCode, counter.n, time.Since(start), resp.Header, err
	}
	return resp.StatusCode, counter.n, time.Since(start), resp.Header, nil
}

// measureGetBody 与 measureGet 走同一条路，但把响应正文**留在内存里**返回。
//
// 只有"同窗口对照"的最后一次重复用它：它要从服务端**真实写出**的正文里读出
// 窗口（bucket_ts / window_sec / bucket_sec / buckets）与 nodes 那一块的原始字节，
// 拿它们当基线。前面几次重复仍旧走 measureGet（边读边丢），中位数取自三次，
// 正文读取那点额外开销（2~3 MB 的分配）只可能落在最后一次上，
// 而且只会把那一格**拖慢**，不会把中位数拉低。
func measureGetBody(client *http.Client, url string) (int, int64, time.Duration, []byte, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return 0, 0, 0, nil, err
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, 0, time.Since(start), nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return resp.StatusCode, int64(len(raw)), time.Since(start), raw, readErr
	}
	return resp.StatusCode, int64(len(raw)), time.Since(start), raw, nil
}

// countingWriter 只数写进来多少字节。
type countingWriter struct{ n int64 }

func (w *countingWriter) Write(p []byte) (int, error) {
	w.n += int64(len(p))
	return len(p), nil
}

// discardResponseWriter 是一个"不缓冲"的 http.ResponseWriter：只数字节。
//
// 为什么不用 httptest.NewRecorder：NewRecorder 会把整个响应体收进一个 bytes.Buffer，
// 那块缓冲的增长（2.2 MB 的体要分配约两倍）会算进"服务端分配"里，把要量的东西
// 污染掉。这里 Write 直接丢掉，只剩 handler 自己的分配。
type discardResponseWriter struct {
	header http.Header
	status int
	n      int64
}

func (w *discardResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = http.Header{}
	}
	return w.header
}

func (w *discardResponseWriter) WriteHeader(code int) { w.status = code }

func (w *discardResponseWriter) Write(p []byte) (int, error) {
	w.n += int64(len(p))
	return len(p), nil
}

// memSnapshot 是一次 runtime.MemStats 的关键字段。
type memSnapshot struct {
	totalAlloc uint64
	heapAlloc  uint64
	heapInuse  uint64
	mallocs    uint64
	numGC      uint32
}

func readMem() memSnapshot {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return memSnapshot{
		totalAlloc: ms.TotalAlloc, heapAlloc: ms.HeapAlloc, heapInuse: ms.HeapInuse,
		mallocs: ms.Mallocs, numGC: ms.NumGC,
	}
}

// TestMeasureOverviewPingPayload60Nodes 是 02-4：60 节点下 /api/v1/overview 的
// buckets=3600 响应有多大、一次请求分配多少、20 并发时堆有多高。
//
// 三条与第二轮对照的说明：
//  1. 响应**字节数**：与 v1.4.0 的 0007 迁移（给 ping_samples_1m 加 ts 覆盖索引）
//     无关 —— 索引只改扫描路径，不改一行数据、不改 JSON 形状。所以这一项预期不变，
//     变的是耗时。
//  2. **分配量**：modernc.org/sqlite 是纯 Go 移植，SQLite 引擎自己的分配也算在
//     Go 堆上，所以索引确实可能改变分配与耗时（少建临时 B 树）。
//  3. 第二轮量的是"单请求累计分配 22.0 MB / 20 并发 HeapAlloc 峰值 234.3 MB"。
func TestMeasureOverviewPingPayload60Nodes(t *testing.T) {
	// Gzip 开关开着（默认），但下面的请求**不带** Accept-Encoding —— 中间件据此
	// 走明文，于是明文与 gzip 两行数字来自**同一个服务端、同一份数据**，是干净的对照。
	f := newMeasureFixture(t, 60, 1, measureRowsPerNode, true)
	t.Logf("现场：%d 节点 / %d 探测目标 / 播撒 %d 行（每节点 %d 行，1 分钟一格）",
		len(f.nodes), 1, f.rows, measureRowsPerNode)

	client := measureHTTPClient(64)
	// 预热：把 TCP 连接、SQLite 语句缓存、HTTP 服务端缓冲都建起来，
	// 否则第一次请求的分配里混着"冷启动"的那一份。
	if code, _, _, _, err := measureGet(client, f.ts.URL+"/api/v1/overview?window=1h&buckets=10", ""); err != nil || code != 200 {
		t.Fatalf("预热请求失败: code=%d err=%v", code, err)
	}

	// ---------- 1. 体积随 buckets 的增长（与第二轮同样三个点）----------
	t.Logf("---- 响应体积（明文，访客，Accept-Encoding 不带）----")
	for _, buckets := range []int{10, 600, 3600} {
		path := fmt.Sprintf("/api/v1/overview?window=1h&buckets=%d", buckets)
		code, n, dur, hdr, err := measureGet(client, f.ts.URL+path, "")
		if err != nil || code != 200 {
			t.Fatalf("%s 失败: code=%d err=%v", path, code, err)
		}
		t.Logf("buckets=%-4d 状态=%d 明文响应=%8d 字节 耗时=%9s Content-Length=%s",
			buckets, code, n, dur.Round(time.Microsecond), hdr.Get("Content-Length"))
	}

	// ---------- 2. gzip ----------
	path3600 := "/api/v1/overview?window=1h&buckets=3600"
	code, plain, durPlain, _, err := measureGet(client, f.ts.URL+path3600, "")
	if err != nil || code != 200 {
		t.Fatalf("明文请求失败: code=%d err=%v", code, err)
	}
	code, gz, durGz, hdrGz, err := measureGet(client, f.ts.URL+path3600, "gzip")
	if err != nil || code != 200 {
		t.Fatalf("gzip 请求失败: code=%d err=%v", code, err)
	}
	t.Logf("buckets=3600 明文 %d 字节 / %s；gzip 后 %d 字节 / %s（Content-Encoding=%q，压缩比 %.1fx）",
		plain, durPlain.Round(time.Microsecond), gz, durGz.Round(time.Microsecond),
		hdrGz.Get("Content-Encoding"), float64(plain)/float64(gz))

	// ---------- 3. 单请求的分配（真 HTTP，客户端只数字节）----------
	t.Logf("---- 单请求分配（真 HTTP over loopback，同一个进程里既有服务端也有客户端）----")
	for i := 1; i <= 3; i++ {
		runtime.GC()
		before := readMem()
		code, n, dur, _, err := measureGet(client, f.ts.URL+path3600, "")
		after := readMem()
		if err != nil || code != 200 {
			t.Fatalf("第 %d 次单请求失败: code=%d err=%v", i, code, err)
		}
		t.Logf("第 %d 次：响应 %d 字节 耗时 %9s 累计分配 %8.3f MiB（%d 次分配）HeapAlloc 增量 %+9.3f MiB",
			i, n, dur.Round(time.Microsecond),
			float64(after.totalAlloc-before.totalAlloc)/(1<<20), after.mallocs-before.mallocs,
			float64(int64(after.heapAlloc)-int64(before.heapAlloc))/(1<<20))
	}

	// ---------- 4. 服务端那一侧（进程内、不缓冲的 ResponseWriter）----------
	t.Logf("---- 同一个 handler 走进程内 discardResponseWriter（只有服务端那一半）----")
	handler := f.s.Handler()
	for i := 1; i <= 3; i++ {
		runtime.GC()
		before := readMem()
		req := httptest.NewRequest(http.MethodGet, path3600, nil)
		rec := &discardResponseWriter{}
		handler.ServeHTTP(rec, req)
		after := readMem()
		if rec.status != 200 {
			t.Fatalf("进程内请求状态码 = %d", rec.status)
		}
		t.Logf("第 %d 次：响应 %d 字节 累计分配 %8.3f MiB（%d 次分配）HeapAlloc 增量 %+9.3f MiB",
			i, rec.n, float64(after.totalAlloc-before.totalAlloc)/(1<<20),
			after.mallocs-before.mallocs,
			float64(int64(after.heapAlloc)-int64(before.heapAlloc))/(1<<20))
	}

	// ---------- 5. 20 并发：HeapAlloc 峰值 ----------
	//
	// 峰值采样用 runtime/metrics 的 /memory/classes/heap/objects:bytes（= HeapAlloc），
	// 不用 runtime.ReadMemStats：后者每次都 STW，1 kHz 采样会给被测路径凭空加上停顿，
	// 量出来的"峰值"里混着采样器自己的影响。
	//
	// ⚠️ 两个偏差必须一起量，否则数字没法与第二轮对照：
	//   - **GC 时机**：HeapAlloc 是"已分配但还没回收"，同样 20 个请求，从干净堆起步
	//     与从"上一轮刚留了一堆垃圾"的堆起步，峰值可以差一倍；
	//   - **客户端读法**：本进程里既有服务端也有客户端。客户端用 io.Copy(io.Discard)
	//     读（正常浏览器的形状：字节流过就走）与用 io.ReadAll 把 20 份响应体全留在
	//     内存里（第二轮那种探针的形状），堆峰值完全是两回事 —— 后者其实是把
	//     **客户端的缓冲**算进了"服务端并发成本"。
	//
	// 所以这里跑 4 轮"丢弃式读取"（3 轮干净堆 + 1 轮不 GC），再加 1 轮
	// "io.ReadAll 全留"的对照组，用来解释第二轮 234.3 MB 的来源。
	t.Logf("---- 20 并发（各一次 buckets=3600）----")
	for _, cr := range []struct {
		label   string
		preGC   bool
		readAll bool
	}{
		{"丢弃式读取，跑前先 GC（干净堆）", true, false},
		{"丢弃式读取，跑前先 GC（干净堆）", true, false},
		{"丢弃式读取，跑前先 GC（干净堆）", true, false},
		{"丢弃式读取，跑前**不** GC（堆里还留着上一轮的垃圾）", false, false},
		{"对照组：io.ReadAll 把 20 份响应全留在内存里（第二轮探针的形状）", false, true},
	} {
		burst := measureBurst(t, client, f.ts.URL+path3600, 20, cr.preGC, cr.readAll)
		t.Logf("（%s）20 个并发全部完成耗时 %9s；每个响应 %d 字节（全部相同=%v）；"+
			"起步 HeapAlloc %.3f MiB；期间 HeapAlloc 峰值 %.3f MiB；并发结束 HeapAlloc %.3f MiB；"+
			"GC 后 %.3f MiB；这一轮累计分配 %.3f MiB",
			cr.label, burst.elapsed.Round(time.Microsecond), burst.size, burst.allSame,
			float64(burst.before.heapAlloc)/(1<<20),
			float64(burst.peak)/(1<<20), float64(burst.afterBurst.heapAlloc)/(1<<20),
			float64(burst.afterGC.heapAlloc)/(1<<20),
			float64(burst.afterBurst.totalAlloc-burst.before.totalAlloc)/(1<<20))
	}

	// ---------- 断言（只防"测量方法整体跑偏"）----------
	code, n, _, _, err := measureGet(client, f.ts.URL+path3600, "")
	if err != nil || code != 200 {
		t.Fatalf("收尾请求失败: code=%d err=%v", code, err)
	}
	// 60 节点 × 3600 格 × 2 个数组，每格 4 个字符的 null 就已经 ≈1.7 MB。
	if n < 1_500_000 || n > 4_000_000 {
		t.Fatalf("响应体 %d 字节落在合理区间 [1.5 MB, 4 MB] 之外：夹具或窗口算法变了", n)
	}
	// 分配量必须大于响应体（编码过程会按段重新分配），但也别大到离谱。
	runtime.GC()
	before := readMem()
	_, n2, _, _, _ := measureGet(client, f.ts.URL+path3600, "")
	after := readMem()
	alloc := after.totalAlloc - before.totalAlloc
	if alloc < uint64(n2) || alloc > 20*uint64(n2) {
		t.Fatalf("单请求累计分配 %d 字节，与响应体 %d 字节的比例超出 [1x, 20x]", alloc, n2)
	}
	t.Logf("收尾对照：响应 %d 字节、单请求累计分配 %.3f MiB（%.1f 倍）",
		n2, float64(alloc)/(1<<20), float64(alloc)/float64(n2))
}

// burstResult 是一次并发冲击的观测结果。
type burstResult struct {
	elapsed    time.Duration
	size       int64
	allSame    bool
	before     memSnapshot
	peak       uint64
	afterBurst memSnapshot
	afterGC    memSnapshot
}

// measureBurst 同时打 concurrency 个请求，期间用 runtime/metrics 采 HeapAlloc 峰值。
//
// readAll 为真时客户端用 io.ReadAll 并把响应体**留到全部请求结束**（第二轮探针的
// 形状）；为假时用 io.Copy(io.Discard)（正常客户端的形状，字节流过就走）。
func measureBurst(t *testing.T, client *http.Client, url string, concurrency int, preGC, readAll bool) burstResult {
	t.Helper()
	if preGC {
		runtime.GC()
	}
	before := readMem()

	samples := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	var peak atomic.Uint64
	peak.Store(before.heapAlloc)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			metrics.Read(samples)
			if v := samples[0].Value.Uint64(); v > peak.Load() {
				peak.Store(v)
			}
			time.Sleep(time.Millisecond)
		}
	}()

	var (
		wg    sync.WaitGroup
		sizes = make([]int64, concurrency)
		codes = make([]int, concurrency)
		keep  = make([][]byte, concurrency)
	)
	start := time.Now()
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if readAll {
				req, err := http.NewRequest(http.MethodGet, url, nil)
				if err != nil {
					t.Errorf("并发请求 %d 构造失败: %v", i, err)
					return
				}
				resp, err := client.Do(req)
				if err != nil {
					t.Errorf("并发请求 %d 失败: %v", i, err)
					return
				}
				defer func() { _ = resp.Body.Close() }()
				raw, err := io.ReadAll(resp.Body)
				if err != nil {
					t.Errorf("并发请求 %d 读响应失败: %v", i, err)
					return
				}
				keep[i] = raw
				sizes[i], codes[i] = int64(len(raw)), resp.StatusCode
				return
			}
			code, n, _, _, err := measureGet(client, url, "")
			if err != nil {
				t.Errorf("并发请求 %d 失败: %v", i, err)
			}
			sizes[i], codes[i] = n, code
		}(i)
	}
	wg.Wait()
	elapsed := time.Since(start)
	close(stop)
	<-done

	afterBurst := readMem()
	runtime.GC()
	afterGC := readMem()
	// keep 在 GC 之后才释放引用：让"客户端把 20 份响应全留着"这件事真的体现在峰值里。
	_ = keep

	allSame := true
	for i := range sizes {
		if codes[i] != 200 || sizes[i] != sizes[0] {
			allSame = false
		}
	}
	return burstResult{
		elapsed: elapsed, size: sizes[0], allSame: allSame,
		before: before, peak: peak.Load(), afterBurst: afterBurst, afterGC: afterGC,
	}
}

// measureWindow 是一个**钉死的**总览窗口。
//
// ★ 为什么必须钉死（这是本文件里唯一一处"测量方法"的关键）：
// /api/v1/overview 的窗口是**请求时刻现算**的 —— internal/server/overview.go 里
// `end := time.Now().Unix()`、按 bucket_sec 对齐、`start := end - windowSec`。
// 于是"有索引量一遍、DROP INDEX 后再量一遍"这两次**天然落在不同的窗口里**；
// 而夹具的探测行又是按 time.Now() 铺的（seedMeasurePingsSpread 的 end 锚点），
// 所以窗口每往前滑 60 秒，每节点就有 1 行掉出 1 小时窗口，整窗口加权平均
// （lat_ms / avg_ms / loss_pct）的小数位长度跟着变 ⇒ **响应字节会随墙钟漂移**。
// 本机两次测量隔 ~1.7 s 时漂不到边界，CI runner 慢 17~55 倍、隔 ~95 s 时必然漂过去 ——
// 那是"两次测量没落在同一个窗口"这个前提不成立，与索引无关。
//
// 钉法：窗口不从墙钟现取，而是从**有索引那一次真实响应**的正文里读回来
// （bucket_ts[0] + window_sec + bucket_sec + buckets）。两次测量都用同一个
// (start, end, bucketSec, buckets)，索引就成了唯一的变量。
type measureWindow struct {
	start, end int64
	bucketSec  int64
	buckets    int
}

// measureWindowOf 从 /api/v1/overview 的响应正文里读回这一次请求实际用的窗口，
// 以及 nodes 字段的原始字节。
//
// 从正文里读、而不是自己按同一个公式算，是为了不复制产品代码的窗口规则：
// 这里读到的就是服务端**真的**用了的那个窗口（顺带也就证明了这一点）。
func measureWindowOf(t *testing.T, url string, raw []byte) (measureWindow, json.RawMessage) {
	t.Helper()
	var env struct {
		WindowSec int64           `json:"window_sec"`
		Buckets   int             `json:"buckets"`
		BucketSec int64           `json:"bucket_sec"`
		BucketTS  []int64         `json:"bucket_ts"`
		Nodes     json.RawMessage `json:"nodes"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("%s：解析响应正文失败: %v", url, err)
	}
	if len(env.BucketTS) == 0 || env.WindowSec <= 0 || env.BucketSec <= 0 || env.Buckets <= 0 {
		t.Fatalf("%s：响应里的窗口参数不完整（bucket_ts=%d window_sec=%d bucket_sec=%d buckets=%d）",
			url, len(env.BucketTS), env.WindowSec, env.BucketSec, env.Buckets)
	}
	if len(env.Nodes) == 0 {
		t.Fatalf("%s：响应里没有 nodes", url)
	}
	w := measureWindow{
		start:     env.BucketTS[0],
		end:       env.BucketTS[0] + env.WindowSec,
		bucketSec: env.BucketSec,
		buckets:   env.Buckets,
	}
	return w, env.Nodes
}

// measureOverviewPayload 用**显式窗口**把响应里 "每节点探测分桶" 那一块（nodes 字段）
// 编码成字节，作为同窗口对照的基线。
//
// 全程走服务端同一条路：
//   - s.overviewPings —— handleOverview 用的就是它（同一份查询 + 同一个目标排序）；
//   - guestOverviewNodesJSON —— 访客出口的那层投影（本夹具是访客请求）；
//   - json.Marshal —— writeJSON 用的也是标准库的 encoder。
//
// 这三条不是"看起来一样"，而是有断言钉住的：run() 里那条"标定"会在索引还在时
// 用同一个窗口重算一次，要求它与响应正文里的 nodes **逐字节相同**。
func measureOverviewPayload(t *testing.T, f *measureFixture, w measureWindow) []byte {
	t.Helper()
	pings := f.s.overviewPings(context.Background(), w.start, w.end, w.bucketSec, w.buckets)
	raw, err := json.Marshal(guestOverviewNodesJSON(pings))
	if err != nil {
		t.Fatalf("按窗口 [%d, %d) 重算总览载荷失败: %v", w.start, w.end, err)
	}
	return raw
}

// measureSlowGap 是"慢环境模拟"：设了 PROBE_MEASURE_SLOW_GAP（如 90s）就在
// "有索引/无索引"两组测量之间多等这么久，用来复现 CI runner 上那种
// "两次测量隔了几十秒到几分钟"的现场。
//
// 默认不设 = 完全不等待（不影响正常跑法）。钉死窗口之后，睡多久都不该改变判定：
//
//	PROBE_MEASURE_SLOW_GAP=90s go test ./internal/server/ -run TestMeasureOverviewPingIndexEffect -count=1 -v
func measureSlowGap(t *testing.T) {
	t.Helper()
	raw := os.Getenv("PROBE_MEASURE_SLOW_GAP")
	if raw == "" {
		return
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		t.Fatalf("PROBE_MEASURE_SLOW_GAP=%q 不是合法的正时长（例如 90s）", raw)
	}
	t.Logf("慢环境模拟：两组测量之间额外等待 %s（窗口已钉死，判定不应受影响）", d)
	time.Sleep(d)
}

// measureMedian 把同一请求打 reps 次，返回耗时的中位数、全部样本、响应字节数，
// 以及**最后一次**的响应正文（正文交给 measureWindowOf 读窗口与 nodes）。
func measureMedian(t *testing.T, client *http.Client, url string, reps int) (time.Duration, []time.Duration, int64, []byte) {
	t.Helper()
	samples := make([]time.Duration, 0, reps)
	var size int64
	var lastBody []byte
	for i := 0; i < reps; i++ {
		if i == reps-1 {
			code, n, dur, raw, err := measureGetBody(client, url)
			if err != nil || code != 200 {
				t.Fatalf("%s 第 %d 次失败: code=%d err=%v", url, i+1, code, err)
			}
			samples = append(samples, dur)
			size, lastBody = n, raw
			continue
		}
		code, n, dur, _, err := measureGet(client, url, "")
		if err != nil || code != 200 {
			t.Fatalf("%s 第 %d 次失败: code=%d err=%v", url, i+1, code, err)
		}
		samples = append(samples, dur)
		size = n
	}
	sorted := append([]time.Duration(nil), samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted[len(sorted)/2], samples, size, lastBody
}

// TestMeasureOverviewPingIndexEffect 是 02-4 的"为什么"那一半：
// 0007（给 ping_samples_1m 加 (ts,node_id,target_id,avg_ms,up_cnt,all_cnt) 覆盖索引）
// 到底改变了什么。
//
// 做法是在**测试库**里做 A/B：同一份数据，先按线上（有索引）量一遍，
// 再 DROP INDEX 量一遍。产品代码一行没动，改的只是临时库的 schema。
//
// 两种表形各量一次：
//   - 1 小时的小表（3,540 行；第二轮的现场就是这个量级）；
//   - 8 天保留期的表（691,200 行；migrate.go 注释里说的形状）。
//
// 期望（也是本用例要证伪/证实的假设）：
//   - **响应里探测载荷的字节数逐字节不变**（索引不改数据、不改 JSON 形状）；
//   - 小表上耗时差不多（整表扫也就 3,540 行，索引没有用武之地）；
//   - 大表窄窗口上索引明显更快；宽窗口（168h）收益消失甚至更慢（migrate.go 注释
//     里说 468 ms → 635 ms，本轮顺带复一次）。
//
// ★ 字节比较为什么必须钉死窗口（v1.5.0 的 CI 红过这条）：
// /api/v1/overview 的窗口是**请求时刻现算**的（overview.go：`end := time.Now().Unix()`、
// 按 bucket_sec 对齐、`start = end - windowSec`），而夹具的探测行锚在 time.Now() 上；
// 两次测量隔 60 秒，就有 1 行/节点掉出 1 小时窗口 ⇒ 整窗口加权平均的小数位长度变了
// ⇒ 响应字节自己就变了。本机两次隔 ~1.7 s 撞不上边界，CI runner 慢 17~55 倍、
// 两次隔 ~95 s（实测）必然撞上 —— 于是那条"有/无索引字节必须相同"的断言
// 用自己的现场把自己判红（−1440 字节），跟索引一点关系都没有。
//
// 现在的判定方式：每一步的 HTTP 测量照旧（耗时与响应字节都打出来），但
// **字节判定只比"同一个窗口"**：窗口从"有索引那一次真实响应"的正文里读回来
// （bucket_ts[0] / window_sec / bucket_sec / buckets，见 measureWindowOf），
// 然后两组都用它去算 nodes 那一块的载荷字节（measureOverviewPayload）。
// 这样索引是唯一变量：窗口滑动再也不会污染判定，而"索引真的改变了结果"照样判红 ——
// 载荷与响应正文里 nodes 逐字节相同这件事由 run() 里那条标定断言钉住。
//
// 慢环境模拟（默认关闭）：
//
//	PROBE_MEASURE_SLOW_GAP=90s go test ./internal/server/ -run TestMeasureOverviewPingIndexEffect -count=1 -v
func TestMeasureOverviewPingIndexEffect(t *testing.T) {
	const indexName = "idx_ping_samples_1m_ts"
	client := measureHTTPClient(8)

	// pinned 是一次测量里"同一个窗口"的两份载荷。
	type pinned struct {
		win        measureWindow
		withIndex  []byte
		httpMed    time.Duration
		httpSize   int64
		withoutIdx []byte
	}

	// run 对一个现场做 A/B。calibrate 为真时额外做一次"标定"：
	// 用同一个窗口重算载荷，要求它与响应正文里的 nodes 逐字节相同 ——
	// 这条钉住的是"同窗口对照比的确实是响应载荷"，只需要做一次（编码路径与表形无关）。
	run := func(title string, f *measureFixture, calibrate bool) {
		t.Helper()
		queries := []string{
			"/api/v1/overview?window=1h&buckets=10",
			"/api/v1/overview?window=1h&buckets=3600",
			"/api/v1/overview?window=168h&buckets=3600",
		}
		items := make([]pinned, len(queries))
		for i, q := range queries {
			med, all, size, body := measureMedian(t, client, f.ts.URL+q, 3)
			win, nodes := measureWindowOf(t, q, body)
			items[i] = pinned{win: win, withIndex: nodes, httpMed: med, httpSize: size}
			t.Logf("[%s 有索引] %-42s 中位 %9s（3 次：%v）响应 %d 字节（现场测量）",
				title, q, med.Round(time.Microsecond), roundAll(all), size)
			if calibrate && i == 0 {
				recomputed := measureOverviewPayload(t, f, win)
				if !bytes.Equal(recomputed, nodes) {
					t.Errorf("[%s] %s：同一窗口重算的载荷与响应正文里的 nodes 不一致（%d vs %d 字节）——"+
						"同窗口对照的基线不成立，下面的字节比较没有意义",
						title, q, len(nodes), len(recomputed))
				}
				t.Logf("[%s] 标定：窗口 [%d, %d) 重算载荷 %d 字节，与响应正文里的 nodes 逐字节相同",
					title, win.start, win.end, len(recomputed))
			}
		}

		if _, err := f.db.Writer().ExecContext(context.Background(),
			"DROP INDEX "+indexName); err != nil {
			t.Fatalf("DROP INDEX: %v", err)
		}
		// 慢环境模拟：默认不睡。窗口钉死之后，这两组之间隔多久都不该改变判定。
		measureSlowGap(t)

		for i, q := range queries {
			med, all, size, _ := measureMedian(t, client, f.ts.URL+q, 3)
			items[i].withoutIdx = measureOverviewPayload(t, f, items[i].win)
			t.Logf("[%s 无索引] %-42s 中位 %9s（3 次：%v）响应 %d 字节　⇒ 中位变化 %+.1f%%，"+
				"现场字节变化 %+d（现场字节含窗口滑动，只作记录；判定看下面的同窗口对照）",
				title, q, med.Round(time.Microsecond), roundAll(all), size,
				float64(med-items[i].httpMed)/float64(items[i].httpMed)*100, size-items[i].httpSize)
			t.Logf("[%s] 同窗口对照 %-42s 窗口 [%d, %d) bucket_sec=%d："+
				"有索引载荷 %d 字节 / 无索引载荷 %d 字节　⇒ 字节变化 %+d",
				title, q, items[i].win.start, items[i].win.end, items[i].win.bucketSec,
				len(items[i].withIndex), len(items[i].withoutIdx),
				len(items[i].withoutIdx)-len(items[i].withIndex))
			if len(items[i].withoutIdx) != len(items[i].withIndex) {
				t.Errorf("[%s] %s：**同一窗口**下、有/无索引的响应载荷字节不同（%d vs %d）——"+
					"索引改变了结果，这比「慢一点」严重得多", title, q,
					len(items[i].withIndex), len(items[i].withoutIdx))
			}
		}
	}

	small := newMeasureFixture(t, 60, 1, measureRowsPerNode, true)
	t.Logf("小表现场：60 节点 × 1 目标 × %d 行（1 小时，1 分钟一格）", small.rows)
	run("1 小时小表", small, true)

	big := newMeasureFixture(t, 60, 1, 0, true)
	start := time.Now()
	rows := seedMeasurePingsSpread(t, big.db, big.nodes, []store.PingTarget{{ID: big.targetID}}, 8*24*60, 60)
	t.Logf("大表现场：60 节点 × 1 目标 × %d 行（8 天，1 分钟一格），播撒耗时 %s",
		rows, time.Since(start).Round(time.Millisecond))
	var count int64
	if err := big.db.Reader().QueryRowContext(context.Background(),
		`SELECT count(*) FROM ping_samples_1m`).Scan(&count); err != nil {
		t.Fatalf("数行数: %v", err)
	}
	t.Logf("    实际落库 %d 行", count)
	run("8 天大表", big, false)

	// 这两条断言只防"夹具没真的建起来"。
	if small.rows != 3540 {
		t.Errorf("小表行数 = %d，期望 3540", small.rows)
	}
	if count != 691200 {
		t.Errorf("大表行数 = %d，期望 691200", count)
	}
}

func roundAll(xs []time.Duration) []string {
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		out = append(out, x.Round(time.Microsecond).String())
	}
	return out
}

// TestMeasureOverviewPingPayloadSizeModel 是 02-4 的"尺寸模型"：
// 把响应体大小拆开，用来回答"第二轮的 2208318 与本轮实测的差值到底是代码变了
// 还是夹具不同"。
//
// ⚠️ 这条用例第一版写错了模型、被自己的断言抓红（39957 / 2207927 / 2196407 不单调），
// 修出来的两个事实都值得写进报告：
//
//  1. **窗口里一行数据都没有的节点，根本不会出现在 nodes 里**：三个现场里
//     "0 行/节点"的响应只有 39,957 字节（bucket_ts 那 3600 个 Unix 秒 + 合计 + 包装），
//     不是 60 × 两个 3600 长的数组 —— store.QueryOverviewPing 的累加器是"扫到行才建"的。
//  2. **响应体大小对"非空格个数"不是单调的**：一格没有数据时写 `null`（4 字节），
//     有数据时写那个数字 —— 本夹具里 loss=0 写成 `0`（1 字节）、lat=12.5 写成 `12.5`
//     （4 字节），所以"多 58 个非空的 loss 格"反而让响应**变小** 11,520 字节。
//     换句话说：拿字节数当"数据量"的代理会错，它只说明 JSON 形状。
//
// 三个现场、同一份代码、同一个请求：
//
//	0 行/节点 → 结构基线 A（没有任何节点条目）
//	1 行/节点 → A + 60 个节点条目（每节点每数组 1 个非空格）
//	59 行/节点 → A + 60 个节点条目（每节点每数组 59 个非空格）
func TestMeasureOverviewPingPayloadSizeModel(t *testing.T) {
	path := "/api/v1/overview?window=1h&buckets=3600"
	client := measureHTTPClient(8)

	measure := func(rows int) int64 {
		t.Helper()
		f := newMeasureFixture(t, 60, 1, rows, true)
		if code, _, _, _, err := measureGet(client, f.ts.URL+path, ""); err != nil || code != 200 {
			t.Fatalf("预热失败（rows=%d）: code=%d err=%v", rows, code, err)
		}
		_, n, _, _, err := measureGet(client, f.ts.URL+path, "")
		if err != nil {
			t.Fatalf("请求失败（rows=%d）: %v", rows, err)
		}
		return n
	}

	base := measure(0)
	one := measure(1)
	full := measure(measureRowsPerNode)

	perNode := (float64(one) - float64(base)) / 60
	// 从 1 行到 59 行：节点条目数不变，只有非空格在变（这里变成**变短**）。
	extraNonNull := 2 * (measureRowsPerNode - 1) * 60
	perNonNull := float64(full-one) / float64(extraNonNull)

	const round2Size = 2208318
	t.Logf("A 结构基线（0 行数据/节点，nodes 里一个条目都没有）：%d 字节", base)
	t.Logf("B 每节点 1 行：%d 字节 ⇒ 每个**窗口内有数据的节点**贡献 %.1f 字节（两个 3600 长的数组）",
		one, perNode)
	t.Logf("C 每节点 %d 行：%d 字节（比 B **小** %d）⇒ 每个非空格 %.3f 字节"+
		"（loss 那一格写 0 只要 1 字节，比 4 字节的 null 短 3 字节 ⇒ 负斜率）",
		measureRowsPerNode, full, one-full, perNonNull)
	t.Logf("第二轮报的 2208318 字节：与 B（每节点 1 行）差 %+d 字节、与 C（每节点 %d 行）差 %+d 字节 ⇒ "+
		"**同一量级、同一结构**。三个数字之间的差全部来自夹具（非空格的个数与值的位数），"+
		"与 0007 索引无关：索引不改变一行数据，也不改变 JSON 形状",
		round2Size-one, measureRowsPerNode, round2Size-full)

	// 结构基线远小于"有数据"的版本（bucket_ts 就 ~40 KB），
	// 而 B 与 C 都在"60 × 36 KB"的量级上（±1%）。
	if base > 100_000 {
		t.Fatalf("结构基线 %d 字节，比预期（只有 bucket_ts + 合计）大得多", base)
	}
	if perNode < 34_000 || perNode > 40_000 {
		t.Fatalf("每个节点条目 %.0f 字节落在合理区间 [34000, 40000] 之外", perNode)
	}
	if diff := float64(full-one) / float64(one); diff > 0.02 || diff < -0.02 {
		t.Fatalf("B 与 C 相差 %.1f%%，超出「只有非空格在变」应有的 2%% 以内", diff*100)
	}
}

// ---------------------------------------------------------------------------
// 01-3：压制曲线的形状
// ---------------------------------------------------------------------------

// sweepPoint 是一个并发档位的结果。
type sweepPoint struct {
	concurrency int
	requests    int
	qps         float64
	p50, p90    time.Duration
	p99, max    time.Duration
	statuses    map[int]int
}

func (p sweepPoint) String() string {
	return fmt.Sprintf("并发=%-4d 请求=%-6d QPS=%8.1f p50=%9s p90=%9s p99=%9s max=%9s 状态=%v",
		p.concurrency, p.requests, p.qps, p.p50.Round(time.Microsecond),
		p.p90.Round(time.Microsecond), p.p99.Round(time.Microsecond),
		p.max.Round(time.Microsecond), p.statuses)
}

// measureSweep 在一个端点上按给定并发档位扫一遍延迟。
//
// 每一档：预热 1 次（建连接），然后 C 个 worker 在 window 时间里持续打，
// 各自记录延迟；这一档的 QPS = 完成请求数 / 实测墙钟。
func measureSweep(t *testing.T, client *http.Client, url string, levels []int, window time.Duration) []sweepPoint {
	t.Helper()
	out := make([]sweepPoint, 0, len(levels))
	for _, c := range levels {
		if _, _, _, _, err := measureGet(client, url, ""); err != nil {
			t.Fatalf("并发=%d 预热失败: %v", c, err)
		}
		var (
			mu       sync.Mutex
			latency  []time.Duration
			statuses = map[int]int{}
		)
		deadline := time.Now().Add(window)
		var wg sync.WaitGroup
		for w := 0; w < c; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				local := make([]time.Duration, 0, 256)
				localStatus := map[int]int{}
				for time.Now().Before(deadline) {
					code, _, dur, _, err := measureGet(client, url, "")
					if err != nil {
						localStatus[-1]++
						continue
					}
					local = append(local, dur)
					localStatus[code]++
				}
				mu.Lock()
				latency = append(latency, local...)
				for k, v := range localStatus {
					statuses[k] += v
				}
				mu.Unlock()
			}()
		}
		start := time.Now()
		wg.Wait()
		elapsed := time.Since(start)

		sort.Slice(latency, func(i, j int) bool { return latency[i] < latency[j] })
		p := sweepPoint{
			concurrency: c, requests: len(latency), statuses: statuses,
			qps: float64(len(latency)) / elapsed.Seconds(),
			p50: percentile(latency, 0.50), p90: percentile(latency, 0.90),
			p99: percentile(latency, 0.99), max: percentile(latency, 1.0),
		}
		out = append(out, p)
		t.Logf("    %s", p)
	}
	return out
}

func percentile(sorted []time.Duration, q float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(q * float64(len(sorted)-1))
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// TestMeasureLatencyKneeOnLoopback 是 01-3：在 loopback 上找"延迟开始拐弯"的并发与 QPS。
//
// ⚠️ 这不是目标部署的阈值：本机是 20 核 / 28 线程的桌面 CPU、loopback 无网卡与
// 真实网络、客户端与服务端在**同一个进程**里抢同一批核。它给出的是"这台机器上
// 曲线的形状"（在哪个并发量级开始弯、弯得多陡），不能外推。
//
// 三条路径：
//   - /api/v1/nodes                     轻（列表）
//   - /api/v1/overview?...buckets=10     前端真实轮询（约 15 KB）
//   - /api/v1/overview?...buckets=3600   重（约 2.2 MB / 单请求 20+ MB 分配）
//
// 用**管理员会话**而不是访客：访客读接口有 300 次/分钟的固定窗口限流
// （guestReadLimit），扫到高并发时量到的会是 429 的延迟而不是服务端的拐点。
//
// 默认跳过：这一条会把机器压到 128 并发（与另一个组正在跑的 e2e 浏览器用例抢机器，
// 也是唯一一条会显著干扰别人的测量）。完整跑法：
//
//	PROBE_LOADTEST=1 go test ./internal/server/ -run TestMeasureLatencyKneeOnLoopback -count=1 -v -timeout 30m
func TestMeasureLatencyKneeOnLoopback(t *testing.T) {
	if len(os.Getenv("PROBE_LOADTEST")) == 0 {
		t.Skip("负载扫描默认跳过（会把机器压到 128 并发）。完整跑法：" +
			"PROBE_LOADTEST=1 go test ./internal/server/ -run TestMeasureLatencyKneeOnLoopback -count=1 -v -timeout 30m")
	}
	f := newMeasureFixture(t, 60, 1, measureRowsPerNode, false)
	client := measureAdminClient(t, f)

	t.Logf("本机：NumCPU=%d GOMAXPROCS=%d；现场：60 节点 / 1 探测目标 / %d 行探测数据",
		runtime.NumCPU(), runtime.GOMAXPROCS(0), f.rows)
	t.Logf("⚠️ 这是 loopback（客户端与服务端同进程）的曲线，**不是目标部署的阈值**")

	cases := []struct {
		name   string
		path   string
		levels []int
		window time.Duration
	}{
		{"轻：GET /api/v1/nodes", "/api/v1/nodes", []int{1, 2, 4, 8, 16, 32, 64, 128}, 4 * time.Second},
		{"前端默认：overview buckets=10", "/api/v1/overview?window=1h&buckets=10", []int{1, 2, 4, 8, 16, 32, 64, 128}, 4 * time.Second},
		{"重：overview buckets=3600", "/api/v1/overview?window=1h&buckets=3600", []int{1, 2, 4, 8, 16, 32}, 4 * time.Second},
	}

	for _, tc := range cases {
		t.Logf("---- %s ----", tc.name)
		points := measureSweep(t, client, f.ts.URL+tc.path, tc.levels, tc.window)
		if len(points) < 2 {
			t.Fatalf("%s：档位太少，量不出形状", tc.name)
		}
		for _, p := range points {
			for code, count := range p.statuses {
				if code != 200 {
					t.Fatalf("%s 并发=%d 出现非 200 状态 %d（%d 次）：这次扫描量的是限流/错误，不是拐点",
						tc.name, p.concurrency, code, count)
				}
			}
		}
		base := points[0]
		// 拐点判据一（主）：p50 首次超过"单并发 p50 的 2 倍"。
		// 用 p50 而不是 p99：p99 在单并发档位自己就可能超过 p50 的 2 倍（噪声尾部），
		// 拿它当判据会立刻误报；而 p50 的抬升就是"排队开始"的直接证据。
		kneeLatency, kneeQPS := -1, -1
		for i, p := range points {
			if kneeLatency < 0 && p.p50 > 2*base.p50 {
				kneeLatency = i
			}
			if kneeQPS < 0 && i > 0 && p.qps < 1.1*points[i-1].qps {
				kneeQPS = i
			}
		}
		describe := func(i int) string {
			if i < 0 {
				return "本次扫描没出现"
			}
			p := points[i]
			return fmt.Sprintf("并发=%d（QPS=%.0f，p50=%s，p99=%s）",
				p.concurrency, p.qps, p.p50.Round(time.Microsecond), p.p99.Round(time.Microsecond))
		}
		t.Logf("    ⇒ 拐点（p50 首次 > 2×单并发 p50=%s）：%s", base.p50.Round(time.Microsecond), describe(kneeLatency))
		t.Logf("    ⇒ QPS 首次不再增长（<上一档的 1.1 倍）的档位：%s", describe(kneeQPS))
		t.Logf("    ⇒ 本档位最大 QPS：%.0f（并发=%d）", maxQPS(points), maxQPSAt(points))
		if kneeLatency < 0 && kneeQPS < 0 {
			t.Errorf("%s：8 个并发档位里既没有延迟拐点也没有吞吐饱和点 —— "+
				"要么这台机器远远没被压到，要么测量方法有问题（不能这么报）", tc.name)
		}
	}
}

func maxQPS(points []sweepPoint) float64 {
	best := 0.0
	for _, p := range points {
		if p.qps > best {
			best = p.qps
		}
	}
	return best
}

func maxQPSAt(points []sweepPoint) int {
	best, at := 0.0, 0
	for _, p := range points {
		if p.qps > best {
			best, at = p.qps, p.concurrency
		}
	}
	return at
}
