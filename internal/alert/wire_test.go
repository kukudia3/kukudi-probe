package alert

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	// 内嵌时区数据库：Windows 上没有系统 tzdata，而这一组用例必须在一个**具体**
	// 时区（与进程本地时区不同）下验证告警文案。
	_ "time/tzdata"
)

// 这一组用例抓的是**线上真正发出去的文本**：httptest 顶掉 telegramAPIBase，
// 从请求体里取 text 字段。
//
// 为什么不拿 stubNotifier 断言 Body：仓库自带的 stub 与日志通知器**不会二次
// 渲染**，而线上那条路径会 —— Telegram.Send 拿到 Title 非空的通知时，会先把它
// 过一遍 RenderBatch，在正文前面补一行「图标 + 标题」。于是"标题重复两行"这种
// 问题在 stub 上完全看不见（Body 里只有一行标题），只有抓 wire 才看得见。

// wireTestZone 是这一组用例的服务端时区（--timezone）。
//
// 特意取一个与跑测试的进程时区（这台机器是 UTC+8）差 12 小时、连日期都不同的
// 时区：任何一处忘了按 --timezone 渲染，断言立刻会红。
const wireTestZone = "America/New_York"

// wireTestBotToken 形状合法即可（真的请求被 httptest 接住了，不发出去）。
const wireTestBotToken = "123456789:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw"

func wireTestLoc(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(wireTestZone)
	if err != nil {
		t.Fatalf("加载时区 %s: %v", wireTestZone, err)
	}
	return loc
}

// wireRecorder 收集 Telegram 收到的每一次 sendMessage 的 text。
type wireRecorder struct {
	mu    sync.Mutex
	texts []string
	times []time.Time
}

func (w *wireRecorder) handler() http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var payload struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(raw, &payload)
		w.mu.Lock()
		w.texts = append(w.texts, payload.Text)
		w.times = append(w.times, time.Now())
		w.mu.Unlock()
		rw.Header().Set("Content-Type", "application/json")
		_, _ = rw.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	}
}

func (w *wireRecorder) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.texts)
}

// wait 等到至少 want 条文本再返回快照（超时直接失败）。
func (w *wireRecorder) wait(t *testing.T, want int, timeout time.Duration) []string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) && w.count() < want {
		time.Sleep(5 * time.Millisecond)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.texts...)
}

func (w *wireRecorder) sentTimes() []time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]time.Time(nil), w.times...)
}

// wireHarness 把「规则引擎 → 通知分发器 → 真 Telegram 通知器 → httptest」装起来。
type wireHarness struct {
	engine     *Engine
	dispatcher *Dispatcher
	wire       *wireRecorder
	loc        *time.Location
}

func newWireHarness(t *testing.T, loc *time.Location, tune func(*DispatcherOptions)) *wireHarness {
	t.Helper()
	recorder := &wireRecorder{}
	withTelegramServer(t, recorder.handler())

	opts := DefaultDispatcherOptions()
	// 合并窗口短一点：用例要连着发几条，等 3 秒没有意义（合并本身有专门的用例）。
	opts.Coalesce = 60 * time.Millisecond
	opts.RateLimit = 0
	opts.ExclusiveGap = 0
	opts.RetryBase = 5 * time.Millisecond
	if tune != nil {
		tune(&opts)
	}
	d := NewDispatcher(slog.New(slog.DiscardHandler),
		[]Notifier{NewTelegram(wireTestBotToken, "-1001234567890")}, opts)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	d.Start(ctx)

	params := DefaultParams()
	params.StartupGrace = 0
	params.OfflineDebounce = 0
	params.RecoverStable = 0
	params.Loc = loc
	return &wireHarness{
		engine:     NewEngine(params, time.Now().Add(-time.Hour)),
		dispatcher: d,
		wire:       recorder,
		loc:        loc,
	}
}

// fire 评估一次并投递所有"该发"的通知（等价于服务端每秒那一跳）。
func (h *wireHarness) fire(t *testing.T, now time.Time, nodes []Node) int {
	t.Helper()
	sent := 0
	for _, d := range h.engine.Evaluate(now, nodes) {
		if !d.Notify {
			continue
		}
		if !h.dispatcher.Enqueue(d.Notification) {
			t.Fatal("入队失败")
		}
		sent++
	}
	return sent
}

// wireNode 造一台带分组与地区的在线机器（displayName 的两个后缀都有值）。
func wireNode(now time.Time, name string) Node {
	return Node{ID: 1, Name: name, GroupName: "香港", Region: "HK", Status: "online", LastSeen: now}
}

// wireLog 把线上文本原样贴进测试输出：这一组用例的核心证据就是这些文本。
func wireLog(t *testing.T, kind, text string) {
	t.Helper()
	t.Logf("【%s】线上文本（%d 字符）：\n%s", kind, utf8.RuneCountInString(text), text)
}

// assertTitleOnce 是 ① 的核心断言：标题在这条消息里**只能出现一次**。
//
// 出现两次的样子就是线上那个 bug：一次来自 Body 里渲染好的标题行，
// 一次来自 Telegram.Send 对非空 Title 的二次渲染。
func assertTitleOnce(t *testing.T, text, title string) {
	t.Helper()
	if got := strings.Count(text, title); got != 1 {
		t.Errorf("标题 %q 在消息里出现了 %d 次，期望 1 次：\n%s", title, got, text)
	}
}

// TestTelegramWireTextForEveryAlertKind 逐类抓线上文本。
func TestTelegramWireTextForEveryAlertKind(t *testing.T) {
	loc := wireTestLoc(t)
	// 截到整秒：到期时间在库里就是整秒，对齐之后"剩余 5 小时"这类断言才是确定的。
	base := time.Now().Truncate(time.Second)

	t.Run("离线", func(t *testing.T) {
		h := newWireHarness(t, loc, nil)
		node := wireNode(base, "hk-01")
		node.Status = "offline"
		node.LastSeen = base.Add(-2 * time.Minute)

		h.fire(t, base, []Node{node})                  // 第一拍：只记录条件起点
		h.fire(t, base.Add(time.Second), []Node{node}) // 第二拍：触发
		text := h.wire.wait(t, 1, 5*time.Second)[0]

		wireLog(t, "节点离线", text)
		assertTitleOnce(t, text, "节点离线")
		if !strings.Contains(text, "hk-01（香港 · HK）") {
			t.Errorf("正文里应当有 displayName（名称（分组 · 地区））：\n%s", text)
		}
	})

	t.Run("恢复", func(t *testing.T) {
		h := newWireHarness(t, loc, nil)
		offline := wireNode(base, "hk-01")
		offline.Status = "offline"
		offline.LastSeen = base.Add(-2 * time.Minute)
		h.fire(t, base, []Node{offline})
		h.fire(t, base.Add(time.Second), []Node{offline})
		if got := h.wire.wait(t, 1, 5*time.Second); len(got) != 1 {
			t.Fatalf("先要发出一条离线通知，实际 %d 条", len(got))
		}

		online := wireNode(base, "hk-01")
		h.fire(t, base.Add(2*time.Second), []Node{online}) // 记录在线起点
		h.fire(t, base.Add(3*time.Second), []Node{online}) // 稳定满 0 秒即算稳定
		texts := h.wire.wait(t, 2, 5*time.Second)
		if len(texts) != 2 {
			t.Fatalf("离线 + 恢复应当各发一条，实际 %d 条", len(texts))
		}

		text := texts[1]
		wireLog(t, "节点已恢复", text)
		assertTitleOnce(t, text, "节点已恢复")
	})

	t.Run("流量接近额度", func(t *testing.T) {
		h := newWireHarness(t, loc, nil)
		node := wireNode(base, "hk-02")
		node.TrafficLimit = 1000 * 1000 * 1000 * 1000 // 1.00 TB
		node.TrafficWarnPct = 80
		node.CycleStart = base.AddDate(0, 0, -1)
		node.CycleEnd = base.AddDate(0, 1, -1)
		node.CycleRx = 850 * 1000 * 1000 * 1000 // 850.0 GB

		h.fire(t, base, []Node{node})
		text := h.wire.wait(t, 1, 5*time.Second)[0]

		wireLog(t, "流量接近额度", text)
		assertTitleOnce(t, text, "流量接近额度")
		// ⑧ 口径：字节走 FormatBytes（十进制、同一套小数位），百分比一位小数。
		for _, want := range []string{"850.0 GB", "1.00 TB", "（85.0%）"} {
			if !strings.Contains(text, want) {
				t.Errorf("流量文案里缺少 %q：\n%s", want, text)
			}
		}
		// 二进制单位一个都不许出现：报告、面板、告警必须是同一套账。
		for _, bad := range []string{"GiB", "TiB", "MiB", "KiB"} {
			if strings.Contains(text, bad) {
				t.Errorf("告警里出现了二进制单位 %s（面板与报告的额度是十进制）：\n%s", bad, text)
			}
		}
	})

	t.Run("流量已超额", func(t *testing.T) {
		h := newWireHarness(t, loc, nil)
		node := wireNode(base, "hk-02")
		node.TrafficLimit = 1000 * 1000 * 1000 * 1000
		node.TrafficWarnPct = 80
		node.CycleStart = base.AddDate(0, 0, -1)
		node.CycleEnd = base.AddDate(0, 1, -1)
		node.CycleRx = 1_100 * 1000 * 1000 * 1000 // 110%

		h.fire(t, base, []Node{node})
		// 超额时预警也成立，两条事件落在同一个合并窗口里 → 一条消息两条事件。
		texts := h.wire.wait(t, 1, 5*time.Second)
		var text string
		for _, candidate := range texts {
			if strings.Contains(candidate, "流量已超额") {
				text = candidate
			}
		}
		if text == "" {
			t.Fatalf("没有收到「流量已超额」，实际收到：%v", texts)
		}
		wireLog(t, "流量已超额", text)
		assertTitleOnce(t, text, "流量已超额")
		if !strings.HasPrefix(text, "🔴 ") {
			t.Errorf("超额是 critical，头行图标应当是 🔴：\n%s", text)
		}
	})

	t.Run("即将到期（不足 1 天）", func(t *testing.T) {
		h := newWireHarness(t, loc, nil)
		node := wireNode(base, "hk-03")
		node.ExpiresAt = base.Add(5 * time.Hour).Unix()

		h.fire(t, base, []Node{node})
		text := h.wire.wait(t, 1, 5*time.Second)[0]

		wireLog(t, "即将到期", text)
		assertTitleOnce(t, text, "VPS 即将到期")
		if strings.Contains(text, "剩余 0 天") {
			t.Errorf("不足 24 小时不许写「剩余 0 天」（读起来像今天到期）：\n%s", text)
		}
		if !strings.Contains(text, "剩余不足 1 天（约 5 小时）") {
			t.Errorf("不足 24 小时应当写清小时数：\n%s", text)
		}
		if !strings.HasPrefix(text, "🟡 ") {
			t.Errorf("即将到期仍是 warn，图标应当是 🟡：\n%s", text)
		}
	})

	t.Run("已到期（30 天前）", func(t *testing.T) {
		h := newWireHarness(t, loc, nil)
		node := wireNode(base, "hk-03")
		node.ExpiresAt = base.Add(-30 * 24 * time.Hour).Unix()

		h.fire(t, base, []Node{node})
		text := h.wire.wait(t, 1, 5*time.Second)[0]

		wireLog(t, "已到期", text)
		assertTitleOnce(t, text, "VPS 已到期")
		if !strings.Contains(text, "已过期 30 天") {
			t.Errorf("过期天数必须按真实值算（老 bug 是不管多久都写「已过期 1 天」）：\n%s", text)
		}
		// ⑥ 到期了比流量超了更要紧：🔴。
		if !strings.HasPrefix(text, "🔴 ") {
			t.Errorf("已到期是 critical，图标应当是 🔴：\n%s", text)
		}
	})

	t.Run("合并多条", func(t *testing.T) {
		h := newWireHarness(t, loc, nil)
		// ⑤ 的原始场景：一条 critical（离线）+ 一条 warn（流量接近额度）落在同一个
		// 合并窗口里。老代码的头行是「🔴 流量接近额度 等 2 条」—— 图标说严重、
		// 文字说预警，而且「流量接近额度」在消息里出现两遍。
		offline := wireNode(base, "hk-01")
		offline.Status = "offline"
		offline.LastSeen = base.Add(-2 * time.Minute)

		traffic := Node{ID: 2, Name: "hk-02", GroupName: "香港", Region: "HK", Status: "online", LastSeen: base}
		traffic.TrafficLimit = 1000 * 1000 * 1000 * 1000
		traffic.TrafficWarnPct = 80
		traffic.CycleStart = base.AddDate(0, 0, -1)
		traffic.CycleEnd = base.AddDate(0, 1, -1)
		traffic.CycleRx = 850 * 1000 * 1000 * 1000

		h.fire(t, base, []Node{offline, traffic})                  // 记录离线条起点 + 触发流量预警
		h.fire(t, base.Add(time.Second), []Node{offline, traffic}) // 触发离线
		texts := h.wire.wait(t, 1, 5*time.Second)
		text := texts[0]
		if len(texts) != 1 {
			t.Fatalf("同一个合并窗口里的两条事件应当合成一条消息，实际 %d 条：%v", len(texts), texts)
		}

		wireLog(t, "合并 2 条", text)
		if !strings.HasPrefix(text, "🔴 合并 2 条通知") {
			t.Errorf("头行应当是「🔴 合并 2 条通知」（图标与文字同源，都取批内最高级）：\n%s", text)
		}
		assertTitleOnce(t, text, "节点离线")
		assertTitleOnce(t, text, "流量接近额度")
		if strings.Contains(text, "等 2 条") {
			t.Errorf("头行里不该再出现「… 等 N 条」（那是"+"取第一条标题"+"的写法，级别与图标会对不上）：\n%s", text)
		}
	})
}

// TestTelegramWireTextSplitsOversizeBatch 是 ④ 的证据：合并窗口里最多 10 条事件，
// 每条带长节点名时整条消息会超过 Telegram 的 4096 —— 不许出现"整条被拒收"。
func TestTelegramWireTextSplitsOversizeBatch(t *testing.T) {
	loc := wireTestLoc(t)
	base := time.Now().Truncate(time.Second)

	t.Run("10 条长名字的事件被切成多片", func(t *testing.T) {
		const gap = 250 * time.Millisecond
		h := newWireHarness(t, loc, func(opts *DispatcherOptions) {
			opts.Coalesce = 2 * time.Second // 保证 10 条都落在同一批里
			opts.ExclusiveGap = gap
		})

		long := strings.Repeat("超长节点名", 100) // 500 个字符
		for i := 0; i < 10; i++ {
			n := Notification{
				NodeID: int64(i + 1), NodeName: long,
				Rule: RuleOffline, Severity: SeverityCritical,
				Title: "节点离线",
				Body:  fmt.Sprintf("%s-%02d（香港 · HK）\n最后通信：12:00:00（2 分钟前）\n服务端时间：2026-10-01 12:00:00（%s）", long, i, wireTestZone),
				At:    base,
			}
			if !h.dispatcher.Enqueue(n) {
				t.Fatal("入队失败")
			}
		}

		texts := h.wire.wait(t, 2, 15*time.Second)
		if len(texts) < 2 {
			t.Fatalf("这条消息必然超过 4096 字符，应当被切成多片，实际只有 %d 片", len(texts))
		}
		for i, text := range texts {
			runes := utf8.RuneCountInString(text)
			t.Logf("第 %d/%d 片：%d 字符", i+1, len(texts), runes)
			if runes > 4096 {
				t.Errorf("第 %d 片有 %d 个字符，超过 Telegram 的 4096", i+1, runes)
			}
			if !strings.Contains(text, fmt.Sprintf("（%d/%d）", i+1, len(texts))) {
				t.Errorf("第 %d 片缺少分片序号「（%d/%d）」：\n%s", i+1, i+1, len(texts), firstN(text, 120))
			}
		}
		wireLog(t, "第 1 片开头", firstN(texts[0], 300))

		// 片与片之间用既有的 ExclusiveGap 拉开（同一 chat 连发会被 Telegram 限流）。
		times := h.wire.sentTimes()
		for i := 1; i < len(times); i++ {
			if got := times[i].Sub(times[i-1]); got < gap {
				t.Errorf("第 %d 片与上一片只隔了 %s（< ExclusiveGap %s）", i+1, got, gap)
			}
		}
		// 每台机器的名字都还在（分片不许丢事件）。
		all := strings.Join(texts, "\n")
		for i := 0; i < 10; i++ {
			if !strings.Contains(all, fmt.Sprintf("%s-%02d", long, i)) {
				t.Errorf("第 %d 条事件在分片后不见了", i+1)
			}
		}
	})

	t.Run("单条事件本身就超长时截断并注明", func(t *testing.T) {
		h := newWireHarness(t, loc, nil)
		huge := strings.Repeat("名", 9000)
		if !h.dispatcher.Enqueue(Notification{
			NodeID: 1, NodeName: huge, Rule: RuleOffline, Severity: SeverityCritical,
			Title: "节点离线", Body: huge + "（香港 · HK）", At: base,
		}) {
			t.Fatal("入队失败")
		}
		text := h.wire.wait(t, 1, 10*time.Second)[0]
		runes := utf8.RuneCountInString(text)
		t.Logf("截断后的文本长度 = %d 字符（末尾 %q）", runes, lastN(text, 20))
		if runes > 4096 {
			t.Errorf("截断后仍有 %d 个字符，超过 Telegram 的 4096", runes)
		}
		if !strings.HasSuffix(text, "…（已截断）") {
			t.Errorf("截断必须在末尾留下明确标记：\n%s", lastN(text, 80))
		}
		if !strings.Contains(text, "节点离线") {
			t.Errorf("截断把标题也切掉了：\n%s", firstN(text, 80))
		}
	})
}

// TestTelegramWireTextUsesServerTimezone 是 ⑦ 的证据。
//
// 进程本地时区是 UTC+8，服务端时区（--timezone）是 America/New_York —— 差 12 小时、
// 连日期都不同，任何一处忘了换时区都会立刻露馅。
func TestTelegramWireTextUsesServerTimezone(t *testing.T) {
	loc := wireTestLoc(t)
	if time.Local.String() == loc.String() {
		t.Skipf("进程本地时区就是 %s，这条用例失去区分度", loc)
	}
	base := time.Now().Truncate(time.Second)
	h := newWireHarness(t, loc, nil)

	node := wireNode(base, "hk-01")
	node.Status = "offline"
	node.LastSeen = base.Add(-2 * time.Minute)
	h.fire(t, base, []Node{node})
	firedAt := base.Add(time.Second)
	h.fire(t, firedAt, []Node{node})
	text := h.wire.wait(t, 1, 5*time.Second)[0]

	wireLog(t, "离线（服务端时区 America/New_York）", text)

	// 服务端时间行必须是 --timezone 下的时刻，并带时区标注。
	wantTime := firedAt.In(loc).Format("2006-01-02 15:04:05")
	if !strings.Contains(text, "服务端时间："+wantTime+"（"+wireTestZone+"）") {
		t.Errorf("服务端时间行应当是 %s（%s）：\n%s", wantTime, wireTestZone, text)
	}
	// 最后通信也是同一个时区。
	if want := node.LastSeen.In(loc).Format("15:04:05"); !strings.Contains(text, "最后通信："+want) {
		t.Errorf("最后通信应当是 %s（%s）：\n%s", want, wireTestZone, text)
	}
	// 反向：进程本地时区的那个时刻一个字符都不许出现。
	if local := firedAt.In(time.Local).Format("2006-01-02 15:04:05"); local != wantTime && strings.Contains(text, local) {
		t.Errorf("消息里出现了进程本地时区的时刻 %s：\n%s", local, text)
	}
}

// logRecorder 记录日志通知器的输出（LogNotifier.Log 只要求一个 Info 方法）。
type logRecorder struct {
	mu    sync.Mutex
	lines []string
}

func (r *logRecorder) Info(msg string, args ...any) {
	var b strings.Builder
	b.WriteString(msg)
	for i := 0; i+1 < len(args); i += 2 {
		fmt.Fprintf(&b, " %v=%v", args[i], args[i+1])
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, b.String())
}

func (r *logRecorder) text() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.lines, "\n")
}

// TestLogNotifierTextHasNoDuplicateTitle 是回归项：日志走的是**另一条路径**
// （它不做二次渲染），修 Telegram 那两行标题时不许把日志弄重复。
func TestLogNotifierTextHasNoDuplicateTitle(t *testing.T) {
	recorder := &logRecorder{}
	opts := DefaultDispatcherOptions()
	opts.Coalesce = 60 * time.Millisecond
	opts.RateLimit = 0
	opts.ExclusiveGap = 0
	d := NewDispatcher(slog.New(slog.DiscardHandler), []Notifier{LogNotifier{Log: recorder}}, opts)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)

	d.Enqueue(testNotification("hk-01"))
	d.Enqueue(testNotification("hk-02"))

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && strings.Count(recorder.text(), "节点离线") < 2 {
		time.Sleep(5 * time.Millisecond)
	}
	text := recorder.text()
	t.Logf("日志通知器输出：\n%s", text)

	// 两条事件（合并成一条通知）→ 标题在日志里正好出现两次，一条一次。
	// 出现三次就说明"头行 + 每条标题"又叠加了。
	if got := strings.Count(text, "节点离线"); got != 2 {
		t.Errorf("日志里标题出现 %d 次，期望 2 次（两条事件各一次）：\n%s", got, text)
	}
	// 交给通知器的 Title 是空的：标题就在 Body 第一行里，日志不再单列一遍 ——
	// 那正是"同一行日志里两份标题"。
	if strings.Contains(text, "title=") {
		t.Errorf("日志里不该再单列 title 字段（标题已在 body 第一行）：\n%s", text)
	}
}

// firstN / lastN 只用于把证据截短后贴进测试输出与失败信息。
func firstN(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func lastN(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return "…" + string(r[len(r)-n:])
}
