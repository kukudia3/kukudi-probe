package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"probe/internal/alert"
	"probe/internal/config"
	"probe/internal/store"
)

// 这一组用例抓的是**服务端真正发到 Telegram 的那份文本**：把发往
// api.telegram.org 的请求劫持到本地 httptest，从请求体里取 text 字段。
//
// 为什么不改 telegramAPIBase：它是 internal/alert 的包内变量，服务端测试改不到。
// 而 alert.Telegram 的 http.Client 没有自带 Transport —— 请求时用的就是
// http.DefaultTransport。换掉它，每一次真的 Send 都会被接住（不是某个 stub 的
// 转述，也不是照着 Body 猜的）。
//
// 时区特意与跑测试的进程（这台机器是 UTC+8）不同：报告里的「统计区间（…）」
// 与告警里的时刻都必须按 --timezone 渲染。

const serverWireTestZone = "America/New_York"

// serverWireTestToken 形状合法即可（请求被 httptest 接住了，不会真的发出去）。
const serverWireTestToken = "123456789:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw"

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// telegramWire 收集"会发到 Telegram 的那份 text"。
type telegramWire struct {
	mu    sync.Mutex
	texts []string

	server *httptest.Server
}

func newTelegramWire(t *testing.T) *telegramWire {
	t.Helper()
	w := &telegramWire{}
	w.server = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var payload struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(raw, &payload)
		w.mu.Lock()
		w.texts = append(w.texts, payload.Text)
		w.mu.Unlock()
		rw.Header().Set("Content-Type", "application/json")
		_, _ = rw.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	}))
	target := strings.TrimPrefix(w.server.URL, "http://")

	saved := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		// 只有发往 Telegram 的请求被劫持，其余（本地的 httptest、Cookie 客户端）
		// 原样放行。
		if req.URL == nil || req.URL.Host != "api.telegram.org" {
			return saved.RoundTrip(req)
		}
		clone := req.Clone(req.Context())
		clone.URL.Scheme = "http"
		clone.URL.Host = target
		clone.Host = target
		return saved.RoundTrip(clone)
	})
	t.Cleanup(func() {
		http.DefaultTransport = saved
		w.server.Close()
	})
	return w
}

func (w *telegramWire) wait(t *testing.T, want int, timeout time.Duration) []string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		w.mu.Lock()
		ready := len(w.texts) >= want
		w.mu.Unlock()
		if ready {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.texts...)
}

func serverWireTestLoc(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(serverWireTestZone)
	if err != nil {
		t.Fatalf("加载时区 %s: %v", serverWireTestZone, err)
	}
	return loc
}

// TestTelegramWireTextForReports 抓日/周/月三份报告的线上文本。
func TestTelegramWireTextForReports(t *testing.T) {
	loc := serverWireTestLoc(t)
	wire := newTelegramWire(t)
	srv := newWireServer(t, loc)

	ctx := context.Background()
	// ⑨ 节点命名口径：报告与告警都用「名称（分组 · 地区）」。
	for _, n := range []store.NewNode{
		{Name: "hk-01", GroupName: "香港", Region: "HK", IntervalSec: 1, TrafficWarnPct: 80, ResetDay: 1},
		{Name: "us-01", GroupName: "美西", Region: "US", IntervalSec: 1, TrafficWarnPct: 80, ResetDay: 1},
	} {
		if _, _, err := srv.db.CreateNode(ctx, n, time.Now()); err != nil {
			t.Fatalf("创建节点 %s: %v", n.Name, err)
		}
	}
	nodes, err := srv.db.ListNodes(ctx)
	if err != nil {
		t.Fatalf("读取节点: %v", err)
	}
	seedDailyTraffic(t, srv, nodes[0].ID, "2026-10-06", 8_120_000_000, 1_230_000_000)
	seedDailyTraffic(t, srv, nodes[1].ID, "2026-10-06", 3_010_000_000, 512_000_000)

	turnOnTrafficReports(t, srv, trafficNotifySwitches{Daily: true, Weekly: true, Monthly: true})
	// 2026-10-07 是周三 → 只有日报；2026-10-12 是周一 → 日报 + 周报；
	// 2026-11-01 是 1 号 → 日报 + 月报。三次触发一共 5 条。
	srv.checkTrafficReportsAt(ctx, atLocal(t, loc, "2026-10-07 09:00"))
	srv.checkTrafficReportsAt(ctx, atLocal(t, loc, "2026-10-12 09:00"))
	srv.checkTrafficReportsAt(ctx, atLocal(t, loc, "2026-11-01 09:00"))
	texts := wire.wait(t, 5, 15*time.Second)
	if len(texts) != 5 {
		t.Fatalf("应当收到 5 份报告（日报 3 + 周报 1 + 月报 1），实际 %d 份：%v",
			len(texts), firstLinesOf(texts))
	}
	var daily, weekly, monthly string
	for _, text := range texts {
		switch {
		case strings.HasPrefix(firstLine(text), "📊 流量日报（昨天 10-06）"):
			daily = text
		case strings.HasPrefix(firstLine(text), "📊 流量周报"):
			weekly = text
		case strings.HasPrefix(firstLine(text), "📊 流量月报"):
			monthly = text
		}
	}
	if daily == "" || weekly == "" || monthly == "" {
		t.Fatalf("三份报告没有凑齐（daily=%v weekly=%v monthly=%v）：%v",
			daily != "", weekly != "", monthly != "", firstLinesOf(texts))
	}
	t.Logf("【流量日报】线上文本（%d 字符）：\n%s", len([]rune(daily)), daily)
	t.Logf("【流量周报】线上文本（%d 字符）：\n%s", len([]rune(weekly)), weekly)
	t.Logf("【流量月报】线上文本（%d 字符）：\n%s", len([]rune(monthly)), monthly)

	if n := strings.Count(daily, "流量日报"); n != 1 {
		t.Errorf("报告名出现了 %d 次，期望 1 次（消息开头两行标题就是这里没对齐）：\n%s", n, daily)
	}
	if !strings.Contains(daily, "统计区间：") || !strings.Contains(daily, "（"+serverWireTestZone+"）") {
		t.Errorf("统计区间那行必须标注 --timezone：\n%s", daily)
	}
	for _, want := range []string{"hk-01（香港 · HK）", "us-01（美西 · US）"} {
		if !strings.Contains(daily, want) {
			t.Errorf("报告里应当有 %q（与告警同一套 displayName）：\n%s", want, daily)
		}
	}
	// ⑧ 单位口径：报告里的字节同样走 alert.FormatBytes（十进制），
	// 二进制单位一个都不许出现。
	for _, bad := range []string{"GiB", "TiB", "MiB", "KiB"} {
		if strings.Contains(daily, bad) {
			t.Errorf("报告里出现了二进制单位 %s：\n%s", bad, daily)
		}
	}
	for _, c := range []struct{ kind, text string }{{"流量周报", weekly}, {"流量月报", monthly}} {
		if n := strings.Count(c.text, c.kind); n != 1 {
			t.Errorf("%s 的报告名出现了 %d 次，期望 1 次：\n%s", c.kind, n, c.text)
		}
		if !strings.Contains(c.text, "（"+serverWireTestZone+"）") {
			t.Errorf("%s 缺少时区标注：\n%s", c.kind, c.text)
		}
	}
}

// firstLinesOf 把一批线上文本的首行收起来（失败信息里一眼看出收到了哪几份）。
func firstLinesOf(texts []string) []string {
	out := make([]string, 0, len(texts))
	for _, text := range texts {
		out = append(out, firstLine(text))
	}
	return out
}

// TestTelegramWireTextForServerAlert 抓服务端那一跳发出来的告警文本。
//
// 这条走的是 evaluateAlerts（每秒真实那一跳）：nodeDTO → 引擎快照 → 规则
// → 分发器 → Telegram。它同时是 ②③⑥⑦ 的服务端证据 —— 时区由
// alertParams(cfg, s.loc) 注入，报文的时刻与标注都必须是 America/New_York。
func TestTelegramWireTextForServerAlert(t *testing.T) {
	loc := serverWireTestLoc(t)
	wire := newTelegramWire(t)
	srv := newWireServer(t, loc)

	ctx := context.Background()
	created, _, err := srv.db.CreateNode(ctx, store.NewNode{
		Name: "hk-09", GroupName: "香港", Region: "HK",
		IntervalSec: 1, TrafficWarnPct: 80, ResetDay: 1,
	}, time.Now())
	if err != nil {
		t.Fatalf("创建节点: %v", err)
	}

	now := time.Now().Truncate(time.Second)
	srv.evaluateAlerts(ctx, []nodeDTO{{
		ID: created.ID, Name: "hk-09", GroupName: "香港", Region: "HK",
		Status: "online", LastSeen: now.Unix(),
		ExpiresAt: now.Add(-30 * 24 * time.Hour).Unix(),
		// 手动构造的快照也要写 Enabled：停用节点不参与告警评估（见 alert.go 的
		// evaluateAlerts），零值 false 在这条链路上等于"这台机器被停用了"。
		Enabled: true,
	}})

	texts := wire.wait(t, 1, 10*time.Second)
	if len(texts) != 1 {
		t.Fatalf("应当收到 1 条告警，实际 %d 条", len(texts))
	}
	text := texts[0]
	t.Logf("【服务端告警：已到期 30 天】线上文本（%d 字符）：\n%s", len([]rune(text)), text)

	if n := strings.Count(text, "VPS 已到期"); n != 1 {
		t.Errorf("标题出现了 %d 次，期望 1 次：\n%s", n, text)
	}
	if !strings.HasPrefix(text, "🔴 VPS 已到期") {
		t.Errorf("已到期必须是 🔴（⑥）：\n%s", text)
	}
	if !strings.Contains(text, "已过期 30 天") {
		t.Errorf("过期天数必须按真实值算（③）：\n%s", text)
	}
	if !strings.Contains(text, "hk-09（香港 · HK）") {
		t.Errorf("节点名要走 displayName（⑨）：\n%s", text)
	}
	// ⑦ 到期日按 --timezone 切天，并标注时区名。
	wantDay := time.Unix(now.Add(-30*24*time.Hour).Unix(), 0).In(loc).Format("2006-01-02")
	if !strings.Contains(text, "到期时间："+wantDay+"（"+serverWireTestZone+"）") {
		t.Errorf("到期时间应当是 %s（%s）：\n%s", wantDay, serverWireTestZone, text)
	}
}

// TestTelegramWireTextForTestSend 抓「发送测试」按钮那条消息。
func TestTelegramWireTextForTestSend(t *testing.T) {
	wire := newTelegramWire(t)
	h := newAuthHarness(t)

	// 先存一份能通过校验的 Telegram 配置（测试发送读的就是它）。
	if status, body := h.put(t, "/api/v1/settings/telegram", map[string]any{
		"enabled": true, "bot_token": serverWireTestToken, "chat_id": "-1001234567890",
	}); status != http.StatusOK {
		t.Fatalf("保存通知设置失败: %d %v", status, body)
	}

	status, body := h.post(t, "/api/v1/settings/telegram/test", map[string]any{}, nil)
	if status != http.StatusOK {
		t.Fatalf("测试发送失败: %d %v", status, body)
	}

	texts := wire.wait(t, 1, 10*time.Second)
	if len(texts) != 1 {
		t.Fatalf("测试发送应当只发一条消息，实际 %d 条", len(texts))
	}
	text := texts[0]
	t.Logf("【发送测试】线上文本（%d 字符）：\n%s", len([]rune(text)), text)

	if n := strings.Count(text, "测试通知"); n != 1 {
		t.Errorf("标题出现了 %d 次，期望 1 次：\n%s", n, text)
	}
	if !strings.Contains(text, "极简 VPS 探针") {
		t.Errorf("测试消息正文不对：\n%s", text)
	}
}

// newWireServer 起一个"真库 + 真 Server"的服务端，并把通知器换成真 Telegram
// （发往 api.telegram.org 的请求会被 telegramWire 劫持到本地）。静默期与去抖
// 都归零：这一组用例要验的是文案，不是"该不该发"（那两件事在 internal/alert
// 里有专门的用例）。
func newWireServer(t *testing.T, loc *time.Location) *Server {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "probe.db"))
	if err != nil {
		t.Fatalf("打开测试数据库: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	cfg := config.Default()
	// 静默期与去抖都归零：这一组用例要验的是文案，不是"该不该发"
	// （那两件事在 internal/alert 里有专门的用例）。
	cfg.AlertStartupGrace = 0
	cfg.AlertDebounce = 0
	cfg.AlertRecoverStable = 0

	srv := New(cfg, db, slog.New(slog.DiscardHandler), loc)
	opts := alert.DefaultDispatcherOptions()
	opts.Coalesce = 20 * time.Millisecond
	opts.RateLimit = 0
	opts.ExclusiveGap = 0
	srv.dispatch = alert.NewDispatcher(slog.New(slog.DiscardHandler),
		[]alert.Notifier{alert.NewTelegram(serverWireTestToken, "-1001234567890")}, opts)

	// 分发器的 worker 是后台起的：用例返回前要等它真的退出（只 cancel 不算等，
	// 它可能正卡在合并窗口或一次重试里 —— 见 bgloop_test.go）。
	guard := newBackgroundGuard(t)
	dispatch := guard.start("dispatch", srv.dispatch.Start)
	t.Cleanup(dispatch.stop)
	return srv
}
