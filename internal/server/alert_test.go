package server

import (
	"context"
	"fmt"
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

// rules 返回已经收到的通知的规则名（失败详情里要能一眼看出"发的是什么"）。
func (n *recordingNotifier) rules() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]string, 0, len(n.received))
	for _, item := range n.received {
		out = append(out, item.Rule)
	}
	return out
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

// keepOnline 让节点持续上报，直到调用返回的 stop。
//
// 返回的 stop 做两件事：关掉信号，然后**等这个写手真的退出**。用例必须调它
// （defer stop()）—— 只关信号是不等的：写手可能正卡在一次 conn.Write 里，
// 而用例返回之后连接与临时目录都会开始被清理。
//
// （在后台 goroutine 里跑，所以里面不能用 t.Fatalf。）
func keepOnline(conn *websocket.Conn, interval time.Duration) func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	var once sync.Once
	go func() {
		defer close(done)
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
	return func() {
		once.Do(func() {
			close(stop)
			<-done
		})
	}
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
	stopReport := keepOnline(conn, 500*time.Millisecond)
	waitFor(t, 5*time.Second, "节点上线", func() bool {
		nodes, err := h.srv.currentNodes(context.Background())
		return err == nil && len(nodes) == 1 && nodes[0].Status == "online"
	})

	// 断开：3 秒后判离线 + 200ms 去抖。
	stopReport()
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
	stopReport2 := keepOnline(conn2, 500*time.Millisecond)
	defer stopReport2()

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

// TestDisabledNodeIsSkippedByAlertEvaluation 覆盖 C1（round5）：
// 停用的节点不参与告警评估，因此不再产生"必然的假阳性"通知。
//
// 现象（round4 第四轮确认）：在面板上停用一台机器之后，它断了连、也连不回来，
// 但服务端照常按它评估，于是「节点离线」这类通知**反复发**。停用 = 断连 + 拒新连
// （api_admin.go 的 DisconnectNode / agentconn.go 的 `case !node.Enabled`），
// 所以停用节点物理上不可能在线 —— 那些通知 100% 是假阳性，而运维学会忽略它们
// 之后，真告警也一起被淹掉。
//
// 本用例走**生产那一跳**：PATCH 停用（面板上的同一个接口）→ currentNodes
// （realtimeLoop 每秒喂给 evaluateAlerts 的正是这一份视图）→ evaluateAlerts。
// 手工造 nodeDTO 也能测，但那样测不出 Enabled 是不是真的从数据库一路接了过来。
//
// 反向验证（已实测，见 _audit/ROUND5-BACKEND.md）：撤掉 alert.go 里的
// `if !n.Enabled { continue }` → 红在下面"停用节点不该产生任何通知"那一句。
func TestDisabledNodeIsSkippedByAlertEvaluation(t *testing.T) {
	h := newAuthHarnessWithConfig(t, alertTestConfig())
	recorder := newRecordingNotifier()
	h.srv.dispatch.SetNotifiers([]alert.Notifier{recorder})

	nodeID, _ := createNodeOverHTTP(t, h, "disabled-01")
	ctx := context.Background()
	path := fmt.Sprintf("/api/v1/nodes/%d", nodeID)

	// 面板上的"停用"就是这一次 PATCH。
	if status, body, _ := h.do(t, http.MethodPatch, path, map[string]any{
		"name": "disabled-01", "interval_sec": 1, "reset_day": 19, "enabled": false,
	}, true, nil); status != http.StatusOK {
		t.Fatalf("停用节点失败: %d %v", status, body)
	}

	nodes, err := h.srv.currentNodes(ctx)
	if err != nil {
		t.Fatalf("读取节点视图: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("节点视图里应当只有 1 台机器，实际 %d", len(nodes))
	}
	if nodes[0].Enabled {
		t.Fatalf("停用之后 nodeDTO.Enabled 仍是 true：Enabled 没有从 store 接过来" +
			"（store.ListNodes 的 nodeSelect 取 enabled → store.Node.Enabled → buildNodeDTO）")
	}

	// 把三条规则的条件一次全踩上：离线、流量超额、已到期 30 天。停用节点"不可能在线"，
	// 所以这些条件在真实部署里天天成立 —— 这正是它必须被跳过的原因。
	alarming := func(n nodeDTO) nodeDTO {
		n.Status = "offline"
		n.LastSeen = time.Now().Add(-time.Hour).Unix()
		n.ExpiresAt = time.Now().Add(-30 * 24 * time.Hour).Unix()
		n.TrafficLimit = int64(1) << 40
		n.TrafficWarnPct = 80
		n.TrafficCycleRx = int64(1) << 41
		n.CycleStart = time.Now().Add(-24 * time.Hour).Format("2006-01-02")
		n.CycleEnd = time.Now().Add(24 * time.Hour).Format("2006-01-02")
		return n
	}

	// evaluateFor 在 dur 这段时间里反复评估同一份快照（每 250ms 一次）。
	//
	// 为什么要反复而不是评估一两次：服务端的 1 Hz realtimeLoop 也在跑，它每秒用
	// **真实**视图评估一次，而真实视图里这台机器还没连过（状态不是 offline）——
	// 离线规则在"状态不再是 offline"时会清掉去抖起点（engine.evaluateOffline 里的
	// delete(conditionSince)）。单次评估可能正好被那一拍夹住（实测踩过一次），
	// 所以按"这个条件持续成立"应有的样子去驱动它。
	evaluateFor := func(d nodeDTO, dur time.Duration) {
		deadline := time.Now().Add(dur)
		for time.Now().Before(deadline) {
			h.srv.evaluateAlerts(ctx, []nodeDTO{d})
			time.Sleep(250 * time.Millisecond)
		}
	}

	// 停用期间：连续评估 1 秒（离线去抖是 200ms，这个时长足够让一台"还在评估范围内"
	// 的机器发出离线 + 流量 + 到期三条通知）。
	evaluateFor(alarming(nodes[0]), time.Second)
	// 再等过分发器的合并窗口（3s）：真产生了通知的话，这时早该到了。
	time.Sleep(3500 * time.Millisecond)
	if got := recorder.count(); got != 0 {
		t.Fatalf("停用节点不该产生任何通知，实际收到 %d 条（%v）—— 停用 = 断连 + 拒新连，"+
			"这些告警是必然的假阳性", got, recorder.rules())
	}
	// 也没有为它写告警状态：引擎压根没看到这个节点（决策为空 ⇒ 一行都不落库）。
	rows, err := h.srv.db.LoadAlertStates(ctx)
	if err != nil {
		t.Fatalf("读取告警状态: %v", err)
	}
	for _, row := range rows {
		if row.NodeID == nodeID {
			t.Errorf("停用节点的告警状态被写库了: %+v", row)
		}
	}

	// 反过来：重新启用之后，同样的条件必须照常发通知 —— 缺了这一半，上面那条
	// "一条都没收到"可能只是"引擎根本没在工作"（永远绿的用例）。
	if status, body, _ := h.do(t, http.MethodPatch, path, map[string]any{
		"name": "disabled-01", "interval_sec": 1, "reset_day": 19, "enabled": true,
	}, true, nil); status != http.StatusOK {
		t.Fatalf("重新启用失败: %d %v", status, body)
	}
	nodes, err = h.srv.currentNodes(ctx)
	if err != nil || len(nodes) != 1 || !nodes[0].Enabled {
		t.Fatalf("重新启用之后节点视图 = %+v (err=%v)，期望 Enabled=true", nodes, err)
	}
	evaluateFor(alarming(nodes[0]), 3*time.Second)

	// 断言落在**正文**而不是通知的 Rule 上：分发器会把 3 秒窗口内的多条事件合并成
	// 一条消息（batchNotification 的 Rule 取批首那条），所以"离线"不一定是第一条
	// —— 这条链路上"离线告警有没有发出去"只能从渲染后的文本里看。
	deadline := time.Now().Add(10 * time.Second)
	var wireText string
	for time.Now().Before(deadline) {
		var b strings.Builder
		for _, n := range recorder.snapshot() {
			b.WriteString(n.Body)
			b.WriteString("\n")
		}
		if wireText = b.String(); strings.Contains(wireText, "节点离线") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(wireText, "节点离线") {
		t.Errorf("重新启用之后应当照常发离线告警，实际收到的通知正文是：\n%s", wireText)
	}
}
