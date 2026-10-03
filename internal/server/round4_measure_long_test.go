package server

// 第四轮「量级测量」补口（ROUND4-GAPS 的 05-7）：把第二轮只跑了 105 秒 × 2 个点的
// 告警队列扫描拉长到十几分钟，回答"长时间运行下队列/丢弃/内存有没有漂移"。
//
// 装置与第二轮一致（可对照 _audit/round2/05-alert-verdicts.md §A-4）：
//
//	真 *alert.Dispatcher*（alert.DefaultDispatcherOptions()：3 秒合并窗口、
//	每条消息最多 10 条事件、20 条/分钟固定窗口限流、128 格队列、单 worker），
//	一个"立即成功"的假通知器（第二轮也是这个形状）。
//
// 与第二轮的差别只有**时长**与**观测量**：
//   - 第二轮：1 条/秒 × 105 秒、5 条/秒 × 105 秒，只记注入/入队/丢弃/已发/剩余格数；
//   - 本轮：同样两个速率各跑 15 分钟（默认），并且每秒采一次**队列深度**、
//     每 10 秒采一次 runtime.MemStats（HeapAlloc/HeapInuse/HeapObjects/NumGC）
//     与 goroutine 数 —— 这样才能看出"漂移"，而不是只看终点。
//
// ⚠️ 观测队列深度用的是 reflect 读 Dispatcher 的私有字段 queue（alert 包没有
// 暴露队列长度；本轮只允许碰 internal/server 与 internal/store 的测试文件，
// 所以不能给 alert 包加访问器）。reflect 只读 len/cap，不改任何东西；
// 若将来 alert 包加了正式访问器，这里应当换成它。
//
// ⚠️ 这个"立即成功"的通知器是**最有利**的排水形状：真实 Telegram 发送要走网络、
// 失败还要退避重试，排空只会更慢。所以本用例量到的"多久开始丢"是**乐观下界**
// （真实部署只会更早开始丢）。第三个扫描点（发送耗时 200ms）是给这一条做的
// 对照，不是第二轮的复现。
//
// 运行方式（默认跳过，避免拖慢 go test ./...）：
//
//	PROBE_LONG_SCAN=1 go test ./internal/server/ -run TestMeasureDispatcherLongScan -count=1 -v -timeout 60m
//
// 可调参数（环境变量）：
//
//	PROBE_LONG_SCAN_SEC      本轮扫描时长（秒），默认 900（15 分钟）
//	PROBE_LONG_SCAN_RATE     注入速率（条/秒），默认 1
//	PROBE_LONG_SCAN_SEND_MS  假通知器每次发送的耗时（毫秒），默认 0
//	PROBE_LONG_SCAN_LABEL    日志前缀标签（默认自动拼）

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"reflect"
	"runtime"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"probe/internal/alert"
)

// measureNotifier 是一个假通知器：可选地睡 delay（模拟真实发送耗时），永远成功。
type measureNotifier struct {
	delay time.Duration
	sent  atomic.Uint64
	bytes atomic.Uint64
}

func (n *measureNotifier) Name() string { return "measure" }

func (n *measureNotifier) Send(ctx context.Context, note alert.Notification) error {
	if n.delay > 0 {
		timer := time.NewTimer(n.delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	n.sent.Add(1)
	n.bytes.Add(uint64(len(note.Body)))
	return nil
}

// measureMemSample 是一次内存采样。
type measureMemSample struct {
	at         time.Duration
	heapAlloc  uint64
	heapInuse  uint64
	heapObjs   uint64
	numGC      uint32
	goroutines int
}

// dispatcherQueueDepth 用 reflect 只读 Dispatcher.queue 的长度与容量。
func dispatcherQueueDepth(d *alert.Dispatcher) (int, int, error) {
	v := reflect.ValueOf(d).Elem().FieldByName("queue")
	if !v.IsValid() || v.Kind() != reflect.Chan {
		return 0, 0, fmt.Errorf("Dispatcher 里没有可读的 queue channel（字段被改名了？）")
	}
	return v.Len(), v.Cap(), nil
}

// longScanEnvInt 读一个非负整数环境变量（缺省时用 def）。
func longScanEnvInt(t *testing.T, key string, def int) int {
	t.Helper()
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		t.Fatalf("%s=%q 不是非负整数", key, raw)
	}
	return n
}

// longScanNotification 造一条形状接近真实的告警事件（引擎给的就是 Title + Body）。
func longScanNotification(i int) alert.Notification {
	node := fmt.Sprintf("hk-%02d", i%200+1)
	return alert.Notification{
		NodeID:   int64(i%200 + 1),
		NodeName: node,
		Rule:     "offline",
		Severity: alert.SeverityCritical,
		Title:    "节点离线",
		Body: fmt.Sprintf("最后通信：%s（3 分钟前）\n状态：离线\n分组：香港 · HK",
			time.Now().Format("2006-01-02 15:04:05")),
		At: time.Now(),
	}
}

// TestMeasureDispatcherLongScan 是 05-7：长时间扫描告警队列的排水/丢弃/内存。
func TestMeasureDispatcherLongScan(t *testing.T) {
	if os.Getenv("PROBE_LONG_SCAN") == "" {
		t.Skip("长时间扫描测量默认跳过（约 15 分钟）。完整跑法：" +
			"PROBE_LONG_SCAN=1 go test ./internal/server/ -run TestMeasureDispatcherLongScan -count=1 -v -timeout 60m")
	}

	seconds := longScanEnvInt(t, "PROBE_LONG_SCAN_SEC", 900)
	rate := longScanEnvInt(t, "PROBE_LONG_SCAN_RATE", 1)
	sendMS := longScanEnvInt(t, "PROBE_LONG_SCAN_SEND_MS", 0)
	label := os.Getenv("PROBE_LONG_SCAN_LABEL")
	if label == "" {
		label = fmt.Sprintf("%d条每秒_发送%dms", rate, sendMS)
	}

	opts := alert.DefaultDispatcherOptions()
	notifier := &measureNotifier{delay: time.Duration(sendMS) * time.Millisecond}
	d := alert.NewDispatcher(slog.New(slog.DiscardHandler), []alert.Notifier{notifier}, opts)

	cap0, capN, err := dispatcherQueueDepth(d)
	if err != nil {
		t.Fatalf("读取队列容量失败: %v", err)
	}
	if cap0 != 0 || capN != opts.QueueSize {
		t.Fatalf("初始队列深度/容量 = %d/%d，期望 0/%d", cap0, capN, opts.QueueSize)
	}

	// 与第二轮同一份参数，逐项打印出来，免得"默认值悄悄变了"没人发现。
	t.Logf("[%s] 参数：队列 %d、合并窗口 %s、每条最多 %d 条事件、限流 %d 条/%s、重试 %d（退避 %s）、独占间隔 %s",
		label, opts.QueueSize, opts.Coalesce, opts.MaxPerMessage,
		opts.RateLimit, time.Minute, opts.Retries, opts.RetryBase, opts.ExclusiveGap)
	t.Logf("[%s] 扫描时长 %d 秒、注入速率 %d 条/秒（期望共 %d 条）、假通知器每次发送耗时 %d ms",
		label, seconds, rate, seconds*rate, sendMS)

	ctx, cancel := context.WithCancel(context.Background())
	baseGoroutines := runtime.NumGoroutine()
	d.Start(ctx)
	stopWorker := func() {
		cancel()
		select {
		case <-d.Done():
		case <-time.After(30 * time.Second):
			t.Errorf("Dispatcher 的 worker 在 30 秒内没有退出")
		}
	}
	t.Cleanup(stopWorker)

	var injected, enqueued atomic.Uint64
	period := time.Second / time.Duration(rate)
	injectTicker := time.NewTicker(period)
	defer injectTicker.Stop()
	sampleTicker := time.NewTicker(250 * time.Millisecond)
	defer sampleTicker.Stop()
	deadline := time.NewTimer(time.Duration(seconds) * time.Second)
	defer deadline.Stop()

	var (
		memSamples []measureMemSample
		depths     []int
		maxDepth   int
		firstDepth time.Duration // 队列第一次 >0
		firstHalf  time.Duration // 队列第一次 ≥ cap/2
		firstFull  time.Duration // 队列第一次 == cap
		firstDrop  time.Duration // 第一次丢弃（Enqueue 返回 false）
	)

	start := time.Now()
	lastMem := start
	lastLog := start

	inject := func() {
		note := longScanNotification(int(injected.Add(1)))
		if d.Enqueue(note) {
			enqueued.Add(1)
		}
	}

scan:
	for {
		select {
		case <-injectTicker.C:
			inject()
			continue
		case <-sampleTicker.C:
		case <-deadline.C:
			break scan
		}

		now := time.Now()
		elapsed := now.Sub(start)

		depth, _, err := dispatcherQueueDepth(d)
		if err != nil {
			t.Fatalf("读取队列深度失败: %v", err)
		}
		depths = append(depths, depth)
		if depth > maxDepth {
			maxDepth = depth
		}
		if depth > 0 && firstDepth == 0 {
			firstDepth = elapsed
		}
		if depth >= capN/2 && firstHalf == 0 {
			firstHalf = elapsed
		}
		if depth == capN && firstFull == 0 {
			firstFull = elapsed
		}
		if _, _, dropped := d.Stats(); dropped > 0 && firstDrop == 0 {
			firstDrop = elapsed
		}

		if now.Sub(lastMem) >= 10*time.Second {
			var ms runtime.MemStats
			runtime.ReadMemStats(&ms)
			memSamples = append(memSamples, measureMemSample{
				at: elapsed, heapAlloc: ms.HeapAlloc, heapInuse: ms.HeapInuse,
				heapObjs: ms.HeapObjects, numGC: ms.NumGC, goroutines: runtime.NumGoroutine(),
			})
			lastMem = now
		}

		if now.Sub(lastLog) >= time.Minute {
			sent, failed, dropped := d.Stats()
			t.Logf("[%s] t=%4.0fs 注入=%d 入队=%d 丢弃=%d 已发消息=%d 失败=%d 队列=%d/%d 通知器发出=%d",
				label, elapsed.Seconds(), injected.Load(), enqueued.Load(), dropped, sent, failed,
				depth, capN, notifier.sent.Load())
			lastLog = now
		}
	}
	elapsed := time.Since(start)

	// 收尾：让 worker 把队列排空（最多等 60 秒），以便区分"积压"与"丢失"。
	drainStart := time.Now()
	var lastDepth int
	for time.Since(drainStart) < 60*time.Second {
		lastDepth, _, err = dispatcherQueueDepth(d)
		if err != nil {
			t.Fatalf("读取队列深度失败: %v", err)
		}
		if lastDepth == 0 {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	drainWaited := time.Since(drainStart)

	// 先停 worker，再统计：否则 Stats 可能在"正在发最后一条"的时刻被读到。
	stopWorker()
	sent, failed, dropped := d.Stats()
	injectedN, enqueuedN := injected.Load(), enqueued.Load()
	depthAtEnd := depths[len(depths)-1]

	// ---------- 原始数字（照抄进报告）----------
	t.Logf("[%s] ==== 原始结果 ====", label)
	t.Logf("[%s] 墙钟 %.1f 秒（扫描 %.1f 秒 + 排空等待 %.1f 秒）；注入速率 %.4f 条/秒（期望 %d）",
		label, elapsed.Seconds()+drainWaited.Seconds(), elapsed.Seconds(), drainWaited.Seconds(),
		float64(injectedN)/elapsed.Seconds(), rate)
	t.Logf("[%s] 注入=%d 入队成功=%d 入队被丢=%d 已发消息=%d 发送失败=%d；通知器实际收到 %d 条消息、共 %d 字节正文",
		label, injectedN, enqueuedN, dropped, sent, failed, notifier.sent.Load(), notifier.bytes.Load())
	t.Logf("[%s] 队列深度：最大 %d/%d；第一次 >0 在 %.0fs、第一次 ≥%d 在 %.0fs、第一次打满在 %s、**第一次丢弃在 %s**",
		label, maxDepth, capN, firstDepth.Seconds(), capN/2, firstHalf.Seconds(),
		durationOrNever(firstFull), durationOrNever(firstDrop))
	t.Logf("[%s] 扫描结束时队列 %d 格；再等 %.1f 秒后 %d 格（0 表示能排空 ⇒ 没有丢不掉的积压）",
		label, depthAtEnd, drainWaited.Seconds(), lastDepth)

	// 深度曲线：抽稀成 10 份（每份取峰值），方便看形状而不是只看终点。
	t.Logf("[%s] 队列深度曲线（%d 秒的采样抽稀成 10 份，每份取峰值）：%v",
		label, len(depths)/maxInt(1, seconds), decimate(depths, 10))

	// 内存漂移：把样本按时间分四段，看首段与末段的 HeapAlloc 中位数与最大值。
	t.Logf("[%s] 内存样本 %d 个（每 10 秒一个）：", label, len(memSamples))
	for i, s := range memSamples {
		if int(s.at.Seconds())%60 == 0 || i == len(memSamples)-1 {
			t.Logf("[%s]   t=%4.0fs HeapAlloc=%8.2f MiB HeapInuse=%8.2f MiB 对象=%7d NumGC=%d goroutine=%d",
				label, s.at.Seconds(), float64(s.heapAlloc)/(1<<20), float64(s.heapInuse)/(1<<20),
				s.heapObjs, s.numGC, s.goroutines)
		}
	}
	if len(memSamples) >= 4 {
		q := len(memSamples) / 4
		firstQuart, lastQuart := memSamples[:q], memSamples[len(memSamples)-q:]
		t.Logf("[%s] HeapAlloc：首 %d 个样本 中位数 %.2f MiB / 峰值 %.2f MiB；末 %d 个样本 中位数 %.2f MiB / 峰值 %.2f MiB ⇒ 漂移 %+.2f MiB",
			label, len(firstQuart), medianHeap(firstQuart), maxHeap(firstQuart),
			len(lastQuart), medianHeap(lastQuart), maxHeap(lastQuart),
			medianHeap(lastQuart)-medianHeap(firstQuart))
		t.Logf("[%s] goroutine：首段中位 %.1f、末段中位 %.1f（用例开始时 %d）",
			label, medianGoroutines(firstQuart), medianGoroutines(lastQuart), baseGoroutines)
	}

	// ---------- 断言（能真的红的那几条）----------
	if dropped != injectedN-enqueuedN {
		t.Errorf("丢弃计数对不上：Stats 说 %d，注入-入队 = %d", dropped, injectedN-enqueuedN)
	}
	if injectedN < uint64(seconds*rate)*9/10 {
		t.Errorf("实际注入 %d 条，低于期望 %d 条的 90%%：定时器被拖慢了，这次扫描的数字不能用",
			injectedN, seconds*rate)
	}
	if enqueuedN != injectedN-dropped {
		t.Errorf("入队成功数 %d + 丢弃 %d ≠ 注入 %d", enqueuedN, dropped, injectedN)
	}
	if notifier.sent.Load() != sent {
		t.Errorf("通知器实际收到 %d 条消息，Dispatcher 记的已发 %d 条：账目不一致",
			notifier.sent.Load(), sent)
	}
	if maxDepth > capN {
		t.Errorf("队列深度 %d 超过容量 %d", maxDepth, capN)
	}
	runtime.GC()
	time.Sleep(200 * time.Millisecond)
	if got := runtime.NumGoroutine(); got > baseGoroutines+3 {
		t.Errorf("收尾后 goroutine 数 = %d（用例开始时 %d）：worker 可能没退干净", got, baseGoroutines)
	}
}

func durationOrNever(d time.Duration) string {
	if d == 0 {
		return "从未（本次扫描没打满）"
	}
	return fmt.Sprintf("%.0fs", d.Seconds())
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// decimate 把一秒钟若干次的采样按份数抽稀（每份取该份的最大值：积压的峰值比均值有信息量）。
func decimate(xs []int, parts int) []int {
	if len(xs) == 0 || parts <= 0 {
		return nil
	}
	out := make([]int, 0, parts)
	step := (len(xs) + parts - 1) / parts
	for i := 0; i < len(xs); i += step {
		end := i + step
		if end > len(xs) {
			end = len(xs)
		}
		peak := xs[i]
		for _, v := range xs[i:end] {
			if v > peak {
				peak = v
			}
		}
		out = append(out, peak)
	}
	return out
}

func medianHeap(samples []measureMemSample) float64 {
	vals := make([]float64, 0, len(samples))
	for _, s := range samples {
		vals = append(vals, float64(s.heapAlloc)/(1<<20))
	}
	return medianFloat(vals)
}

func maxHeap(samples []measureMemSample) float64 {
	best := 0.0
	for _, s := range samples {
		if v := float64(s.heapAlloc) / (1 << 20); v > best {
			best = v
		}
	}
	return best
}

func medianGoroutines(samples []measureMemSample) float64 {
	vals := make([]float64, 0, len(samples))
	for _, s := range samples {
		vals = append(vals, float64(s.goroutines))
	}
	return medianFloat(vals)
}

func medianFloat(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	sorted := append([]float64(nil), vals...)
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j] < sorted[j-1]; j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}
	return sorted[len(sorted)/2]
}
