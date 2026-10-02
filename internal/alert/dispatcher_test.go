package alert

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubNotifier 记录被发送的通知，供断言使用。
type stubNotifier struct {
	name string

	mu        sync.Mutex
	sent      []Notification
	messages  []string
	times     []time.Time
	failTimes int   // 前 N 次故意失败
	failErr   error // 失败时返回的错误（默认普通错误）
}

func (s *stubNotifier) Name() string { return s.name }

func (s *stubNotifier) Send(_ context.Context, n Notification) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failTimes > 0 {
		s.failTimes--
		if s.failErr != nil {
			return s.failErr
		}
		return errors.New("故意失败")
	}
	s.sent = append(s.sent, n)
	s.messages = append(s.messages, n.Body)
	s.times = append(s.times, time.Now())
	return nil
}

func (s *stubNotifier) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sent)
}

// sentTimes 返回每次 Send 发生的时刻（用来验证"分片之间真的被拉开了"）。
func (s *stubNotifier) sentTimes() []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Time(nil), s.times...)
}

func (s *stubNotifier) lastBody() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.sent) == 0 {
		return ""
	}
	return s.sent[len(s.sent)-1].Body
}

func testNotification(node string) Notification {
	return Notification{
		NodeID: 1, NodeName: node, Rule: RuleOffline, Severity: SeverityCritical,
		Title: "节点离线", Body: node + " 掉线了", At: time.Now(),
	}
}

func TestDispatcherCoalescesBurstIntoOneMessage(t *testing.T) {
	stub := &stubNotifier{name: "stub"}
	opts := DefaultDispatcherOptions()
	opts.Coalesce = 30 * time.Millisecond
	opts.RateLimit = 0
	d := NewDispatcher(slog.New(slog.DiscardHandler), []Notifier{stub}, opts)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)

	for _, name := range []string{"hk-01", "hk-02", "hk-03"} {
		if !d.Enqueue(testNotification(name)) {
			t.Fatal("入队失败")
		}
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if stub.count() >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if stub.count() != 1 {
		t.Fatalf("3 条事件应当合并成 1 条消息，实际 %d 条", stub.count())
	}
	body := stub.lastBody()
	for _, name := range []string{"hk-01", "hk-02", "hk-03"} {
		if !strings.Contains(body, name) {
			t.Fatalf("合并消息里缺少 %s：%q", name, body)
		}
	}
	// 合并条数写在**头行**里（「🔴 合并 3 条通知」），不再是末尾那句
	// 「（本次合并 3 条事件）」：Telegram 的通知弹窗只看得到开头，
	// 数量放在末尾时用户得滚到底才知道这是一批。
	if !strings.Contains(body, "合并 3 条通知") {
		t.Fatalf("合并消息应当说明合并条数：%q", body)
	}
	if !strings.HasPrefix(body, "🔴 ") {
		t.Fatalf("头行的图标应当取批内最高级（critical）：%q", body)
	}
	if stub.sent[0].Severity != SeverityCritical {
		t.Fatalf("合并后级别应当是最高级别: %s", stub.sent[0].Severity)
	}
	// ① 的核心：交给通知器的那一条 Title 必须留空，否则 Telegram 会再渲染一次，
	// 消息开头出现两行标题（stub 看不到这个重复，wire 用例才看得到）。
	if stub.sent[0].Title != "" {
		t.Fatalf("合并后交给通知器的 Title 必须留空，实际 %q", stub.sent[0].Title)
	}
}

func TestDispatcherRetriesThenSucceeds(t *testing.T) {
	stub := &stubNotifier{name: "stub", failTimes: 2}
	opts := DefaultDispatcherOptions()
	opts.Coalesce = 0
	opts.RateLimit = 0
	opts.RetryBase = 10 * time.Millisecond
	d := NewDispatcher(slog.New(slog.DiscardHandler), []Notifier{stub}, opts)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)

	d.Enqueue(testNotification("hk-01"))
	// 等的是**统计计数**而不是 stub.count()：stub 被调用与"记进 sent"之间有一个窗口
	// （计数在通知器返回之后才加），负载一高窗口就变宽 —— 等错对象会让断言偶尔读到
	// 中间态（sent 还是 0）。这与下面那条"重试后仍失败"的用例等待的对象保持了一致。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if sent, failed, _ := d.Stats(); sent > 0 || failed > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if stub.count() != 1 {
		t.Fatal("重试之后应当发送成功")
	}
	if sent, failed, _ := d.Stats(); sent != 1 || failed != 0 {
		t.Fatalf("统计 = sent %d / failed %d", sent, failed)
	}
}

func TestDispatcherCountsFailureAfterRetries(t *testing.T) {
	stub := &stubNotifier{name: "stub", failTimes: 10}
	opts := DefaultDispatcherOptions()
	opts.Coalesce = 0
	opts.RateLimit = 0
	opts.Retries = 2
	opts.RetryBase = 5 * time.Millisecond
	d := NewDispatcher(slog.New(slog.DiscardHandler), []Notifier{stub}, opts)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)

	d.Enqueue(testNotification("hk-01"))
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, failed, _ := d.Stats(); failed > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, failed, _ := d.Stats(); failed != 1 {
		t.Fatalf("重试用尽后应当计入失败，实际 failed = %d", failed)
	}
}

func TestDispatcherRespectsRetryAfter(t *testing.T) {
	stub := &stubNotifier{
		name:      "stub",
		failTimes: 1,
		failErr:   &RetryAfterError{After: 30 * time.Millisecond, Message: "太频繁"},
	}
	opts := DefaultDispatcherOptions()
	opts.Coalesce = 0
	opts.RateLimit = 0
	opts.RetryBase = 1 * time.Millisecond
	d := NewDispatcher(slog.New(slog.DiscardHandler), []Notifier{stub}, opts)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)

	started := time.Now()
	d.Enqueue(testNotification("hk-01"))
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && stub.count() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if stub.count() != 1 {
		t.Fatal("尊重 retry_after 之后应当发送成功")
	}
	if elapsed := time.Since(started); elapsed < 30*time.Millisecond {
		t.Fatalf("应当在 retry_after 之后才重试，实际只等了 %s", elapsed)
	}
}

func TestDispatcherDropsWhenQueueFull(t *testing.T) {
	stub := &stubNotifier{name: "stub"}
	opts := DefaultDispatcherOptions()
	opts.QueueSize = 2
	opts.Coalesce = 0
	opts.RateLimit = 0
	d := NewDispatcher(slog.New(slog.DiscardHandler), []Notifier{stub}, opts)
	// 刻意不 Start：队列不会有人消费。
	if !d.Enqueue(testNotification("a")) || !d.Enqueue(testNotification("b")) {
		t.Fatal("前两条应当入队成功")
	}
	if d.Enqueue(testNotification("c")) {
		t.Fatal("队列满时应当丢弃而不是阻塞")
	}
	if _, _, dropped := d.Stats(); dropped != 1 {
		t.Fatalf("丢弃计数 = %d，期望 1", dropped)
	}
}

// 定时流量报告的分片必须**独占**消息：被合并窗口拼回一条，就会顶破 Telegram
// 的单条上限（4096 字符），表现是整条报告一条都发不出去。
//
// 这条同时钉住两件事：
//   - 批首是分片时不等窗口，立刻发；
//   - 分片出现在**别人的**合并窗口里时不被吞并，而是留给下一轮单独发。
func TestDispatcherKeepsNoCoalesceNotificationsSeparate(t *testing.T) {
	stub := &stubNotifier{name: "stub"}
	opts := DefaultDispatcherOptions()
	opts.Coalesce = 150 * time.Millisecond
	opts.RateLimit = 0
	// 这条用例只关心"合不合并"，间隔由下一条用例单独验，这里关掉免得白等。
	opts.ExclusiveGap = 0
	d := NewDispatcher(slog.New(slog.DiscardHandler), []Notifier{stub}, opts)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)

	chunk := func(title, body string) Notification {
		n := testNotification("")
		n.Rule = RuleTrafficReport
		n.Title = title
		n.Body = body
		n.NoCoalesce = true
		return n
	}
	// 一条普通告警 → 两条报告分片 → 又一条普通告警：分片夹在中间，
	// 正是"被合并窗口吞掉"最容易发生的排布。
	for _, n := range []Notification{
		testNotification("hk-01"),
		chunk("流量日报 1/2", "分片一"),
		chunk("流量日报 2/2", "分片二"),
		testNotification("hk-02"),
	} {
		if !d.Enqueue(n) {
			t.Fatal("入队失败")
		}
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && stub.count() < 4 {
		time.Sleep(10 * time.Millisecond)
	}
	if stub.count() != 4 {
		t.Fatalf("4 条通知应当各发一条消息，实际 %d 条", stub.count())
	}
	// 两条分片各自成条：每一片的正文里只能有自己的内容。
	bodies := func() []string {
		stub.mu.Lock()
		defer stub.mu.Unlock()
		return append([]string(nil), stub.messages...)
	}()
	var reports []string
	for _, body := range bodies {
		if strings.Contains(body, "分片") {
			reports = append(reports, body)
		}
		if strings.Contains(body, " 合并 ") {
			t.Fatalf("出现了被合并的消息（分片被吞并了）：%q", body)
		}
	}
	if len(reports) != 2 {
		t.Fatalf("应当有 2 条报告消息，实际 %d 条：%v", len(reports), bodies)
	}
	if strings.Contains(reports[0], "分片二") || strings.Contains(reports[1], "分片一") {
		t.Fatalf("两片被拼进了同一条消息：%v", reports)
	}
}

// 独占消息之间必须**拉开间隔**：分片是紧挨着入队的，几百毫秒内连发几条，
// Telegram 会当成刷屏（429），而 429 只能靠重试硬扛。
//
// 两个方向都钉：
//   - 分片之间确实隔了至少 ExclusiveGap；
//   - **第一片不等**（等的是"上一条独占消息"，此前没有）；
//   - 普通告警完全不受影响（它们的节流是合并窗口与限流器的事）。
func TestDispatcherSpacesOutExclusiveNotifications(t *testing.T) {
	reportChunk := func() Notification {
		return Notification{
			Rule: RuleTrafficReport, Severity: SeverityInfo,
			Body: "📊 流量日报 分片", At: time.Now(), NoCoalesce: true,
		}
	}

	t.Run("分片之间至少隔一个 ExclusiveGap", func(t *testing.T) {
		const gap = 300 * time.Millisecond
		stub := &stubNotifier{name: "stub"}
		opts := DefaultDispatcherOptions()
		opts.Coalesce = 0
		opts.RateLimit = 0
		opts.ExclusiveGap = gap
		d := NewDispatcher(slog.New(slog.DiscardHandler), []Notifier{stub}, opts)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		started := time.Now()
		d.Start(ctx)

		for i := 0; i < 3; i++ {
			if !d.Enqueue(reportChunk()) {
				t.Fatal("入队失败")
			}
		}
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) && stub.count() < 3 {
			time.Sleep(10 * time.Millisecond)
		}
		times := stub.sentTimes()
		if len(times) != 3 {
			t.Fatalf("3 个分片应当各发一条消息，实际 %d 条", len(times))
		}
		if first := times[0].Sub(started); first >= gap {
			t.Errorf("第一片等了 %s 才发出去（≥ %s）：需要等的只有「上一条独占消息」，"+
				"第一片不该被推迟", first, gap)
		}
		for i := 1; i < len(times); i++ {
			if got := times[i].Sub(times[i-1]); got < gap {
				t.Errorf("第 %d 片与上一片只隔了 %s（< %s）：连发会被 Telegram 限流",
					i+1, got, gap)
			}
		}
	})

	t.Run("普通告警不受间隔影响", func(t *testing.T) {
		const gap = 2 * time.Second
		stub := &stubNotifier{name: "stub"}
		opts := DefaultDispatcherOptions()
		opts.Coalesce = 0
		opts.RateLimit = 0
		opts.ExclusiveGap = gap
		d := NewDispatcher(slog.New(slog.DiscardHandler), []Notifier{stub}, opts)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		started := time.Now()
		d.Start(ctx)

		for _, name := range []string{"hk-01", "hk-02", "hk-03"} {
			if !d.Enqueue(testNotification(name)) {
				t.Fatal("入队失败")
			}
		}
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) && stub.count() < 3 {
			time.Sleep(5 * time.Millisecond)
		}
		times := stub.sentTimes()
		if len(times) != 3 {
			t.Fatalf("3 条告警都应当发出去，实际 %d 条", len(times))
		}
		if elapsed := times[2].Sub(started); elapsed >= gap {
			t.Errorf("3 条普通告警花了 %s（≥ ExclusiveGap %s）：间隔被错误地套到了告警上",
				elapsed, gap)
		}
	})
}

// Title 为空 = **Body 就是完整消息**，不再补「图标 + 标题」那一行。
//
// 定时流量报告靠这条活命：分发器交给通知器的通知，Body 已经是渲染好的整段文本，
// 而 Telegram.Send 会拿它再渲染一次。Title 不留空的话，消息开头会出现两行标题
// —— 而且第二行的图标是按 severity 猜的告警配色（详见 server 的
// trafficReportNotifications）。
//
// 两个方向都钉：空 Title 逐字节等于 Body；非空 Title 的行为**一个字符都不变**
// （告警文案不允许被这次改动碰到）。
func TestRenderBatchEmptyTitleIsVerbatimBody(t *testing.T) {
	body := "📊 流量日报（昨天 10-06）\n统计区间：10-06 00:00 → 10-07 00:00\n合计  ↑ 1.74 GB"
	got := RenderBatch([]Notification{{Rule: RuleTrafficReport, Severity: SeverityInfo, Body: body}})
	if got != body {
		t.Fatalf("Title 为空时应当原样输出 Body：\n得到：%q\n期望：%q", got, body)
	}
	// 尤其是：不许出现按 severity 猜出来的图标。
	if strings.Contains(got, "🟢") {
		t.Errorf("Title 为空时还补了告警图标：%q", got)
	}

	// 非空 Title 一律还是「图标 + 标题 + 换行 + 正文」，三种级别各一次。
	for _, c := range []struct {
		severity Severity
		want     string
	}{
		{SeverityCritical, "🔴"},
		{SeverityWarn, "🟡"},
		{SeverityInfo, "🟢"},
	} {
		out := RenderBatch([]Notification{{Title: "节点离线", Body: "hk-01 掉线了", Severity: c.severity}})
		want := c.want + " 节点离线\nhk-01 掉线了"
		if out != want {
			t.Errorf("severity=%s 的告警文案变了：\n得到：%q\n期望：%q", c.severity, out, want)
		}
	}
	// 没有正文时只输出那一行标题（原来就是这个行为）。
	if out := RenderBatch([]Notification{{Title: "节点离线", Severity: SeverityWarn}}); out != "🟡 节点离线" {
		t.Errorf("只有标题时应当只输出标题行，实际 %q", out)
	}
}

func TestRateLimiterWaitSharesWindow(t *testing.T) {
	l := newRateLimiter(2, 40*time.Millisecond)
	ctx := context.Background()
	start := time.Now()
	l.wait(ctx)
	l.wait(ctx)
	l.wait(ctx) // 第三次应当等到下一个窗口
	if elapsed := time.Since(start); elapsed < 40*time.Millisecond {
		t.Fatalf("第三次应当等待一个窗口，实际 %s", elapsed)
	}
}

func TestRenderBatchHasNoMarkdownCharacters(t *testing.T) {
	body := RenderBatch([]Notification{
		{NodeID: 1, NodeName: "a_b*c[d]", Title: "节点离线", Body: "a_b*c[d] 掉线了", Severity: SeverityCritical},
	})
	if !strings.Contains(body, "🔴") {
		t.Fatalf("应当带严重级别图标: %q", body)
	}
	if strings.Contains(body, "```") || strings.Contains(body, "* ") {
		t.Fatalf("不该输出 Markdown: %q", body)
	}
}
