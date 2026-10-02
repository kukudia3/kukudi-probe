package server

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"probe/internal/alert"
	"probe/internal/config"
	"probe/internal/protocol"
)

// recordingNotifier 记录服务端真实发出来的通知。
type recordingNotifier struct {
	mu       sync.Mutex
	received []alert.Notification
	ch       chan alert.Notification
}

func newRecordingNotifier() *recordingNotifier {
	return &recordingNotifier{ch: make(chan alert.Notification, 16)}
}

func (n *recordingNotifier) Name() string { return "recording" }

func (n *recordingNotifier) Send(_ context.Context, notification alert.Notification) error {
	n.mu.Lock()
	n.received = append(n.received, notification)
	n.mu.Unlock()
	select {
	case n.ch <- notification:
	default:
	}
	return nil
}

func (n *recordingNotifier) wait(t *testing.T, rule string, timeout time.Duration) alert.Notification {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case got := <-n.ch:
			if got.Rule == rule {
				return got
			}
		case <-deadline:
			n.mu.Lock()
			seen := make([]string, 0, len(n.received))
			for _, item := range n.received {
				seen = append(seen, item.Rule)
			}
			n.mu.Unlock()
			t.Fatalf("%s 内没有等到 %s 通知（已收到：%v）", timeout, rule, seen)
			return alert.Notification{}
		}
	}
}

func alertTestConfig() config.Server {
	cfg := config.Default()
	// StaleAfter / OfflineAfter 要大于"测试里发帧的间隔"，否则节点会在两帧之间被判离线。
	cfg.StaleAfter = time.Second
	cfg.OfflineAfter = 3 * time.Second
	cfg.AlertStartupGrace = 0
	cfg.AlertDebounce = 200 * time.Millisecond
	cfg.AlertRecoverStable = 200 * time.Millisecond
	cfg.AlertCooldown = time.Hour
	return cfg
}

// keepOnline 让节点持续上报，直到 stop 关闭（在后台 goroutine 里跑，不能用 t.Fatalf）。
func keepOnline(conn *websocket.Conn, interval time.Duration, stop <-chan struct{}) {
	go func() {
		seq := uint64(0)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				seq++
				frame, err := protocol.New(protocol.TypeMetrics, testMetrics())
				if err != nil {
					return
				}
				frame.Seq = seq
				data, err := frame.Encode()
				if err != nil {
					return
				}
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				err = conn.Write(ctx, websocket.MessageText, data)
				cancel()
				if err != nil {
					return
				}
			}
		}
	}()
}

// TestOfflineAndRecoveredAlertsEndToEnd 走完整链路：
// Agent 断开 → 状态机判离线 → 规则引擎 → 通知流水线 → 通知器。
func TestOfflineAndRecoveredAlertsEndToEnd(t *testing.T) {
	h := newAuthHarnessWithConfig(t, alertTestConfig())
	h.srv.agents.msgPerSecond = 100

	recorder := newRecordingNotifier()
	h.srv.dispatch.SetNotifiers([]alert.Notifier{recorder})

	nodeID, token := createNodeOverHTTP(t, h, "alert-01")

	// 先让节点在线。
	conn := mustDialAgent(t, h.ts, token)
	sendFrame(t, conn, helloFrame(t, testHello()))
	readHandshake(t, conn)
	stopReport := make(chan struct{})
	keepOnline(conn, 500*time.Millisecond, stopReport)
	waitFor(t, 5*time.Second, "节点上线", func() bool {
		nodes, err := h.srv.currentNodes(context.Background())
		return err == nil && len(nodes) == 1 && nodes[0].Status == "online"
	})

	// 断开：3 秒后判离线 + 200ms 去抖。
	close(stopReport)
	if err := conn.Close(websocket.StatusNormalClosure, "测试断开"); err != nil {
		t.Fatalf("关闭连接: %v", err)
	}

	offline := recorder.wait(t, alert.RuleOffline, 10*time.Second)
	if offline.Severity != alert.SeverityCritical {
		t.Fatalf("离线通知级别 = %s", offline.Severity)
	}
	// 断言**线上那条文本**而不是通知里的 Title：分发器交给通知器的 Title 是空的
	// （非空时 Telegram 会在正文前再补一行标题，消息开头就是两行标题），
	// 标题只存在于 Body 的首行里。见 alert.RenderBatch。
	if offline.Title != "" {
		t.Fatalf("交给通知器的 Title 必须留空，实际 %q", offline.Title)
	}
	if wire := alert.RenderBatch([]alert.Notification{offline}); !strings.Contains(wire, "🔴 节点离线") {
		t.Fatalf("离线通知的文本 = %q", wire)
	}

	// 重新连上并持续上报：稳定之后应当发"已恢复"。
	conn2 := mustDialAgent(t, h.ts, token)
	sendFrame(t, conn2, helloFrame(t, testHello()))
	readHandshake(t, conn2)
	stopReport2 := make(chan struct{})
	defer close(stopReport2)
	keepOnline(conn2, 500*time.Millisecond, stopReport2)

	recovered := recorder.wait(t, alert.RuleRecovered, 10*time.Second)
	if wire := alert.RenderBatch([]alert.Notification{recovered}); !strings.Contains(wire, "🟢 节点已恢复") {
		t.Fatalf("恢复通知的文本 = %q", wire)
	}
	if !strings.Contains(recovered.Body, "离线时长") {
		t.Fatalf("恢复通知应当说明离线时长: %q", recovered.Body)
	}

	// 状态落盘：offline 已 resolved，recovered 为 firing。
	rows, err := h.srv.db.LoadAlertStates(context.Background())
	if err != nil {
		t.Fatalf("读取告警状态: %v", err)
	}
	states := map[string]string{}
	for _, row := range rows {
		if row.NodeID == nodeID {
			states[row.Rule] = row.State
		}
	}
	if states[alert.RuleOffline] != alert.StateResolved {
		t.Fatalf("离线规则应当是 resolved: %v", states)
	}
	if states[alert.RuleRecovered] != alert.StateFiring {
		t.Fatalf("恢复规则应当是 firing: %v", states)
	}
}

// TestAlertStatePersistedAcrossRestart 验证状态落盘：重启后不会因为"又看到离线"而重复轰炸。
func TestAlertStatePersistedAcrossRestart(t *testing.T) {
	cfg := alertTestConfig()
	h := newAuthHarnessWithConfig(t, cfg)
	recorder := newRecordingNotifier()
	h.srv.dispatch.SetNotifiers([]alert.Notifier{recorder})

	nodeID, token := createNodeOverHTTP(t, h, "alert-02")

	conn := mustDialAgent(t, h.ts, token)
	sendFrame(t, conn, helloFrame(t, testHello()))
	readHandshake(t, conn)
	if err := conn.Close(websocket.StatusNormalClosure, "断开"); err != nil {
		t.Fatalf("关闭连接: %v", err)
	}
	recorder.wait(t, alert.RuleOffline, 8*time.Second)

	// 状态应当已经落库。
	rows, err := h.srv.db.LoadAlertStates(context.Background())
	if err != nil {
		t.Fatalf("读取告警状态: %v", err)
	}
	found := false
	for _, row := range rows {
		if row.NodeID == nodeID && row.Rule == alert.RuleOffline && row.State == alert.StateFiring {
			found = true
			if row.LastNotify == 0 {
				t.Fatal("应当记录通知时间（用于冷却）")
			}
		}
	}
	if !found {
		t.Fatalf("离线状态没有落盘: %+v", rows)
	}

	// 模拟重启后的引擎：载入状态 → 仍然离线也不重复通知。
	restarted := alert.NewEngine(alert.DefaultParams(), time.Now().Add(-time.Hour))
	loaded := make([]alert.State, 0, len(rows))
	for _, row := range rows {
		loaded = append(loaded, alert.State{
			NodeID: row.NodeID, Rule: row.Rule, State: row.State,
			Since: time.Unix(row.Since, 0), LastNotify: time.Unix(row.LastNotify, 0),
			NotifyCnt: row.NotifyCnt, Context: row.Context,
		})
	}
	restarted.Load(loaded)

	now := time.Now()
	node := alert.Node{ID: nodeID, Name: "alert-02", Status: "offline", LastSeen: now.Add(-time.Minute)}
	restarted.Evaluate(now, []alert.Node{node})
	for _, d := range restarted.Evaluate(now.Add(time.Second), []alert.Node{node}) {
		if d.Notify {
			t.Fatalf("重启后处于冷却期内不该重复通知: %+v", d)
		}
	}
}

func TestTelegramSettingsAPI(t *testing.T) {
	h := newAuthHarness(t)

	// 初始状态：未配置。
	status, body := h.get(t, "/api/v1/settings/telegram")
	if status != http.StatusOK {
		t.Fatalf("读取设置失败: %d", status)
	}
	if body["enabled"] != false || body["has_token"] != false || body["ready"] != false {
		t.Fatalf("初始设置不对: %v", body)
	}
	if _, leaked := body["bot_token"]; leaked {
		t.Fatal("响应里不该出现 bot_token 字段")
	}

	// 非法 token 要被拒绝。
	status, body = h.put(t, "/api/v1/settings/telegram", map[string]any{
		"enabled": true, "bot_token": "not-a-token", "chat_id": "123",
	})
	if status != http.StatusBadRequest {
		t.Fatalf("非法 token 应当 400，实际 %d %v", status, body)
	}

	// 合法保存。
	const token = "123456789:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw"
	status, body = h.put(t, "/api/v1/settings/telegram", map[string]any{
		"enabled": true, "bot_token": token, "chat_id": "-1001234567890",
	})
	if status != http.StatusOK {
		t.Fatalf("保存失败: %d %v", status, body)
	}
	if body["has_token"] != true || body["chat_id"] != "-1001234567890" || body["enabled"] != true {
		t.Fatalf("保存结果不对: %v", body)
	}

	// 再读取：仍然不返回 token 明文，但知道已经配置。
	status, body = h.get(t, "/api/v1/settings/telegram")
	if status != http.StatusOK {
		t.Fatalf("读取设置失败: %d", status)
	}
	if body["has_token"] != true || body["ready"] != true {
		t.Fatalf("配置状态不对: %v", body)
	}
	if raw := h.rawBody(t, "/api/v1/settings/telegram"); strings.Contains(raw, token) {
		t.Fatalf("接口响应里泄露了 Bot Token: %s", raw)
	}

	// Token 留空表示不修改。
	status, body = h.put(t, "/api/v1/settings/telegram", map[string]any{
		"enabled": true, "bot_token": "", "chat_id": "-1001234567890",
	})
	if status != http.StatusOK || body["has_token"] != true {
		t.Fatalf("留空 token 应当保留原值: %d %v", status, body)
	}

	// 通知器已经被换成"日志 + Telegram"。
	if names := h.srv.dispatch.NotifierNames(); len(names) != 2 || names[0] != "log" || names[1] != "telegram" {
		t.Fatalf("通知器 = %v，期望 [log telegram]", names)
	}

	// 测试发送会真的去连 Telegram（这里没有网络），因此只要求不返回 5xx 之外的意外状态。
	status, _ = h.post(t, "/api/v1/settings/telegram/test", map[string]any{}, nil)
	if status != http.StatusBadGateway && status != http.StatusOK {
		t.Fatalf("测试发送的状态码 = %d（连不上时应当是 502）", status)
	}
}

func TestTelegramTestEndpointWithoutConfig(t *testing.T) {
	h := newAuthHarness(t)
	status, body := h.post(t, "/api/v1/settings/telegram/test", map[string]any{}, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("未配置时应当 400，实际 %d %v", status, body)
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj == nil || errObj["code"] != "not_configured" {
		t.Fatalf("错误码不对: %v", errObj)
	}
}

func TestTelegramSettingsRequireLogin(t *testing.T) {
	h := newAuthHarnessWithConfig(t, config.Default())
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("创建 Cookie jar: %v", err)
	}
	h.client = &http.Client{Jar: jar, Timeout: 5 * time.Second} // 换成没有会话 Cookie 的客户端

	if status, _ := h.get(t, "/api/v1/settings/telegram"); status != http.StatusUnauthorized {
		t.Fatalf("读取设置未登录应当 401，实际 %d", status)
	}
	// 测试发送是 POST 接口，未登录时同样要被挡下。
	status, _, _ := h.do(t, http.MethodPost, "/api/v1/settings/telegram/test", nil, false, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("测试发送未登录应当 401，实际 %d", status)
	}
}
