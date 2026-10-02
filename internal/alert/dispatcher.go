package alert

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Dispatcher 是通知发送流水线。
//
// 它解决三件事：合并（短时间内多台机器一起抖动只发一条）、
// 重试（对端偶发失败不丢消息）、限流（不因为刷屏被 Telegram 封）。
type Dispatcher struct {
	log       *slog.Logger
	notifiers []Notifier
	notifyMu  sync.RWMutex

	coalesce  time.Duration
	maxPerMsg int
	rateLimit int
	retries   int
	retryBase time.Duration
	// exclusiveGap 是两条独占消息（NoCoalesce）之间的最小间隔。
	exclusiveGap time.Duration

	queue   chan Notification
	dropped atomic.Uint64
	sent    atomic.Uint64
	failed  atomic.Uint64

	closeOnce sync.Once
	done      chan struct{}
}

// SetNotifiers 替换通知器（管理员改设置后调用，无需重启）。
func (d *Dispatcher) SetNotifiers(notifiers []Notifier) {
	d.notifyMu.Lock()
	defer d.notifyMu.Unlock()
	d.notifiers = notifiers
}

// NotifierNames 返回当前生效的通知器名字（诊断与测试用）。
func (d *Dispatcher) NotifierNames() []string {
	d.notifyMu.RLock()
	defer d.notifyMu.RUnlock()
	out := make([]string, 0, len(d.notifiers))
	for _, n := range d.notifiers {
		out = append(out, n.Name())
	}
	return out
}

func (d *Dispatcher) currentNotifiers() []Notifier {
	d.notifyMu.RLock()
	defer d.notifyMu.RUnlock()
	out := make([]Notifier, len(d.notifiers))
	copy(out, d.notifiers)
	return out
}

// DispatcherOptions 是流水线参数。
type DispatcherOptions struct {
	// Coalesce 是合并窗口：窗口内产生的事件合成一条消息。
	Coalesce time.Duration
	// MaxPerMessage 是一条消息最多带几个节点事件。
	MaxPerMessage int
	// RateLimit 是每分钟最多发送多少条消息（0 表示不限）。
	RateLimit int
	// Retries 是每条消息的重试次数。
	Retries int
	// RetryBase 是重试的基础退避（实际为 base、2×base、4×base…）。
	RetryBase time.Duration
	// QueueSize 是队列容量；满了之后新事件直接丢弃并计数（绝不阻塞采集链路）。
	QueueSize int
	// ExclusiveGap 是两条独占消息（Notification.NoCoalesce）之间的最小间隔。
	//
	// 为什么需要它：独占消息眼下只有定时流量报告的分片，而分片是**紧挨着**
	// 入队的（一次报告要把好几片发给同一个 chat）。Telegram 对同一 chat 的
	// 连续消息有限流，几百毫秒内连发几条大概率换回 429 —— 而 429 只能靠
	// 重试硬扛。宁可每片之间多等一秒多，也不去撞那个限流。
	//
	// 作用范围：只把**独占消息之间**拉开，普通告警不会被套上间隔（它们的节流
	// 是合并窗口与限流器的事）。唯一的副作用是"排在分片后面的告警会跟着顺延"，
	// 最坏就是 ExclusiveGap × 分片数（几秒）—— 对一条离线告警来说无关紧要。
	// 等待发生在本流水线自己的 worker 里，不阻塞任何投递方（Enqueue 始终不阻塞）。
	ExclusiveGap time.Duration
}

// DefaultDispatcherOptions 返回默认参数（docs/DESIGN.md §12）。
func DefaultDispatcherOptions() DispatcherOptions {
	return DispatcherOptions{
		Coalesce:      3 * time.Second,
		MaxPerMessage: 10,
		RateLimit:     20,
		Retries:       3,
		RetryBase:     2 * time.Second,
		QueueSize:     128,
		// 1.5 秒：Telegram 对同一 chat 的连续消息大约是按"每秒一条"限的，
		// 留 50% 余量；而一份 3~4 片的报告因此最多多花 4.5 秒发完 ——
		// 报告迟到几秒没有任何代价，被 429 挡回来才是真代价。
		ExclusiveGap: 1500 * time.Millisecond,
	}
}

// NewDispatcher 构造流水线。
func NewDispatcher(log *slog.Logger, notifiers []Notifier, opts DispatcherOptions) *Dispatcher {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if opts.QueueSize <= 0 {
		opts.QueueSize = DefaultDispatcherOptions().QueueSize
	}
	return &Dispatcher{
		log:          log,
		notifiers:    notifiers,
		coalesce:     opts.Coalesce,
		maxPerMsg:    opts.MaxPerMessage,
		rateLimit:    opts.RateLimit,
		retries:      opts.Retries,
		retryBase:    opts.RetryBase,
		exclusiveGap: opts.ExclusiveGap,
		queue:        make(chan Notification, opts.QueueSize),
		done:         make(chan struct{}),
	}
}

// Enqueue 投递一条通知。队列满时返回 false（不阻塞、不丢弃已有告警）。
func (d *Dispatcher) Enqueue(n Notification) bool {
	select {
	case d.queue <- n:
		return true
	default:
		d.dropped.Add(1)
		d.log.Error("通知队列已满，丢弃本次通知",
			"node", n.NodeName, "rule", n.Rule, "dropped_total", d.dropped.Load())
		return false
	}
}

// Stats 返回发送统计（诊断用）。
func (d *Dispatcher) Stats() (sent, failed, dropped uint64) {
	return d.sent.Load(), d.failed.Load(), d.dropped.Load()
}

// Start 启动发送 worker。ctx 结束时 worker 退出。
func (d *Dispatcher) Start(ctx context.Context) {
	go func() {
		defer close(d.done)
		limiter := newRateLimiter(d.rateLimit, time.Minute)
		// held 是"从队列里取出来、但不属于上一批"的那一条：合并窗口里撞上
		// 一条不可合并的通知（定时流量报告的分片）时，本批到此为止，
		// 它留到下一轮单独发 —— 并进来会把它和别的通知拼成一条超长消息。
		var held *Notification
		// lastExclusive 是上一条"要独占一条消息"的通知发出去的时刻，用来把同一次
		// 报告的分片拉开（见 DispatcherOptions.ExclusiveGap）。它的更新在 sendBatch
		// 里：一个批次可能被切成好几片（见 renderMessages），片与片之间同样要拉开。
		// 等待发生在本 worker 里，不阻塞任何投递方。
		var lastExclusive time.Time
		for {
			var first Notification
			if held != nil {
				first, held = *held, nil
			} else {
				select {
				case <-ctx.Done():
					return
				case first = <-d.queue:
				}
			}
			if first.NoCoalesce {
				d.sendBatch(ctx, []Notification{first}, limiter, &lastExclusive)
				continue
			}
			batch, spill := d.collectBatch(first)
			d.sendBatch(ctx, batch, limiter, &lastExclusive)
			held = spill
		}
	}()
}

// waitExclusiveGap 把两条独占消息之间拉开至少 exclusiveGap。
//
// 第一条（lastExclusive 为零值）不等：报告的第一片该立刻发出去。
func (d *Dispatcher) waitExclusiveGap(ctx context.Context, lastExclusive *time.Time) {
	if d.exclusiveGap <= 0 || lastExclusive.IsZero() {
		return
	}
	wait := d.exclusiveGap - time.Since(*lastExclusive)
	if wait <= 0 {
		return
	}
	select {
	case <-ctx.Done():
	case <-time.After(wait):
	}
}

// collectBatch 在合并窗口内继续收事件，最多 maxPerMsg 条。
//
// 返回的第二个值是从队列里取出来、但**不属于**本批的那一条（不可合并的通知）：
// 调用方下一轮把它当批首单独发出去，而不是塞回队列（塞回去它会排到所有
// 事件后面，报告与告警的先后顺序就乱了）。
func (d *Dispatcher) collectBatch(first Notification) ([]Notification, *Notification) {
	batch := []Notification{first}
	// 批首自己就要求独占：窗口都不用等，立刻发。
	if first.NoCoalesce || d.coalesce <= 0 || d.maxPerMsg <= 1 {
		return batch, nil
	}
	timer := time.NewTimer(d.coalesce)
	defer timer.Stop()
	for len(batch) < d.maxPerMsg {
		select {
		case next := <-d.queue:
			if next.NoCoalesce {
				return batch, &next
			}
			batch = append(batch, next)
		case <-timer.C:
			return batch, nil
		case <-d.done:
			return batch, nil
		}
	}
	return batch, nil
}

// sendBatch 把一批事件渲染成 1..N 条消息，逐条发给每一个通知器。
//
// 长度、限流、独占间隔都按**每条消息**算，而不是按每个批次算：一个批次可能
// 被切成好几片（见 renderMessages），片与片是紧挨着发出的，像"一条消息"那样
// 只计一次限流、只等一次间隔都不够。
func (d *Dispatcher) sendBatch(ctx context.Context, batch []Notification, limiter *rateLimiter, lastExclusive *time.Time) {
	if len(batch) == 0 {
		return
	}
	messages := renderMessages(batch, alertMessageMaxRunes)
	// 要拉开间隔的两种消息：报告分片（NoCoalesce，调用方自己切的），
	// 以及被这里切成多片的告警。普通告警一条一批，完全不受影响。
	spaceOut := batch[0].NoCoalesce || len(messages) > 1
	for _, message := range messages {
		if spaceOut {
			d.waitExclusiveGap(ctx, lastExclusive)
		}
		limiter.wait(ctx)
		for _, notifier := range d.currentNotifiers() {
			if err := d.sendWithRetry(ctx, notifier, message); err != nil {
				d.failed.Add(1)
				d.log.Error("通知发送失败",
					"notifier", notifier.Name(), "rule", message.Rule, "err", err)
				continue
			}
			d.sent.Add(1)
		}
		if spaceOut {
			*lastExclusive = time.Now()
		}
	}
}

func (d *Dispatcher) sendWithRetry(ctx context.Context, notifier Notifier, notification Notification) error {
	attempts := d.retries
	if attempts < 1 {
		attempts = 1
	}
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			wait := d.retryBase * time.Duration(1<<uint(attempt-1))
			if wait > 0 {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(wait):
				}
			}
		}
		err := notifier.Send(ctx, notification)
		if err == nil {
			return nil
		}
		lastErr = err
		var retryAfter *RetryAfterError
		if asRetryAfter(err, &retryAfter) {
			// 对端明确要求等待：尊重它，并把它算作一次尝试。
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(retryAfter.After):
			}
		}
	}
	return lastErr
}

// batchNotification 把一批事件合成一条要交给通知器的通知。
//
// Title 一律留空。Body 已经是渲染好的整段文本（含每条的「图标 + 标题」行），
// 而通知器（Telegram）拿到 Title 非空的通知时会**再渲染一次**：在正文前面补一行
// 「图标 + 标题」。两下一叠加，线上消息的开头就是两行标题（第二行还会按 severity
// 被猜成告警配色）。Title 留空 = "Body 就是完整消息"，这条语义定时流量报告
// 已经在用（见 RenderBatch）。
//
// 合并条数写在正文的头行里（「🔴 合并 3 条通知」，见 batchHeadLine），不放 Title：
// 它是给人看的正文，而 Title 这条路只服务于"逐字输出 Body"。
func batchNotification(batch []Notification, message string) Notification {
	first := batch[0]
	return Notification{
		NodeID:   first.NodeID,
		NodeName: first.NodeName,
		Rule:     first.Rule,
		Severity: batchSeverity(batch),
		Title:    "",
		Body:     message,
		At:       first.At,
	}
}

// RenderBatch 把一批事件渲染成最终要发送的纯文本。
//
// 刻意不用 Markdown：节点名里出现 _ * [ ] 之类的字符会把 Telegram 的解析搞崩，
// 而告警最不能接受的就是"因为格式问题没发出去"。
//
// 多条事件时开头有一行中性的头行（「🔴 合并 3 条通知」）：合并批次必须让人一眼
// 看出"这是一批"，而头行里**不写任何一条的标题** —— 写了那条标题就会出现两遍。
func RenderBatch(batch []Notification) string {
	if len(batch) == 0 {
		return ""
	}
	var b strings.Builder
	if head := batchHeadLine(batch); head != "" {
		b.WriteString(head)
		b.WriteString("\n\n")
	}
	for i, n := range batch {
		if i > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(renderOne(n))
	}
	return b.String()
}

func icon(n Notification) string {
	switch {
	case n.Severity == SeverityCritical:
		return "🔴"
	case n.Severity == SeverityWarn:
		return "🟡"
	default:
		return "🟢"
	}
}

// rateLimiter 是固定窗口限流（与 Agent 接入用的那个同构，这里独立实现以保持包内自洽）。
type rateLimiter struct {
	limit   int
	window  time.Duration
	started time.Time
	count   int
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{limit: limit, window: window}
}

// wait 在超出限流时等待到下一个窗口。
func (l *rateLimiter) wait(ctx context.Context) {
	if l.limit <= 0 {
		return
	}
	now := time.Now()
	if now.Sub(l.started) >= l.window {
		l.started = now
		l.count = 0
	}
	if l.count < l.limit {
		l.count++
		return
	}
	wait := l.window - now.Sub(l.started)
	l.started = now.Add(wait)
	l.count = 1
	select {
	case <-ctx.Done():
	case <-time.After(wait):
	}
}

func asRetryAfter(err error, target **RetryAfterError) bool {
	for err != nil {
		if ra, ok := err.(*RetryAfterError); ok {
			*target = ra
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapper.Unwrap()
	}
	return false
}
