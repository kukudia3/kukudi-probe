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
	return nil
}

func (s *stubNotifier) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sent)
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
	if !strings.Contains(body, "本次合并 3 条事件") {
		t.Fatalf("合并消息应当说明合并条数：%q", body)
	}
	if stub.sent[0].Severity != SeverityCritical {
		t.Fatalf("合并后级别应当是最高级别: %s", stub.sent[0].Severity)
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
