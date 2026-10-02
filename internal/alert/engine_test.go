package alert

import (
	"strings"
	"testing"
	"time"
)

// testParams 返回一份"没有静默期、冷却很长"的参数，便于断言。
func testParams() Params {
	p := DefaultParams()
	p.StartupGrace = 0
	p.OfflineDebounce = 2 * time.Second
	p.RecoverStable = 30 * time.Second
	p.NotifyCooldown = 30 * time.Minute
	return p
}

func offlineNode(now time.Time) Node {
	return Node{ID: 1, Name: "hk-01", GroupName: "香港", Region: "HK",
		Status: "offline", LastSeen: now.Add(-time.Minute)}
}

func onlineNode(now time.Time) Node {
	n := offlineNode(now)
	n.Status = "online"
	n.LastSeen = now
	return n
}

func TestOfflineAlertNeedsDebounceAndRespectsCooldown(t *testing.T) {
	base := time.Now().Add(-time.Hour)
	e := NewEngine(testParams(), base)
	now := base.Add(time.Hour)

	// 第一拍：只记录条件起点。
	if d := e.Evaluate(now, []Node{offlineNode(now)}); len(d) != 0 {
		t.Fatalf("第一拍不该产生决策: %+v", d)
	}
	// 1 秒后：还没到去抖时长（2 秒）。
	if d := e.Evaluate(now.Add(time.Second), []Node{offlineNode(now)}); len(d) != 0 {
		t.Fatalf("去抖期内不该触发: %+v", d)
	}
	// 2 秒后：触发并通知。
	d := e.Evaluate(now.Add(2*time.Second), []Node{offlineNode(now)})
	if len(d) != 1 {
		t.Fatalf("应当触发 1 条决策，实际 %d", len(d))
	}
	if d[0].State.Rule != RuleOffline || d[0].State.State != StateFiring || !d[0].Notify {
		t.Fatalf("决策内容不对: %+v", d[0])
	}
	if !strings.Contains(d[0].Notification.Body, "hk-01") || !strings.Contains(d[0].Notification.Body, "香港") {
		t.Fatalf("通知正文应当带上节点名与分组: %q", d[0].Notification.Body)
	}
	if strings.Contains(d[0].Notification.Body, "*") || strings.Contains(d[0].Notification.Body, "_") {
		t.Fatalf("正文里不该有 Markdown 字符: %q", d[0].Notification.Body)
	}

	// 冷却期内不再重复（同一状态、且不产生决策）。
	if again := e.Evaluate(now.Add(3*time.Second), []Node{offlineNode(now)}); len(again) != 0 {
		t.Fatalf("冷却期内不该重复通知: %+v", again)
	}

	// 冷却过去后：状态没变，但再通知一次（避免"离线一整天只有一条"）。
	later := now.Add(2 * time.Second).Add(31 * time.Minute)
	repeated := e.Evaluate(later, []Node{offlineNode(later)})
	if len(repeated) != 1 || !repeated[0].Notify {
		t.Fatalf("冷却后应当重复通知: %+v", repeated)
	}
	if repeated[0].State.NotifyCnt != 2 {
		t.Fatalf("通知计数 = %d，期望 2", repeated[0].State.NotifyCnt)
	}
	if !repeated[0].State.Since.Equal(now.Add(2 * time.Second)) {
		t.Fatalf("重复通知不该改变 Since（否则离线时长会算错）: %v", repeated[0].State.Since)
	}
}

func TestOfflineAlertSilentDuringStartupGrace(t *testing.T) {
	now := time.Now()
	p := testParams()
	p.StartupGrace = 60 * time.Second
	p.OfflineDebounce = 0
	e := NewEngine(p, now) // 刚刚启动

	// 第一拍只是记录条件起点（去抖哪怕配成 0，也要有"连续成立"这个动作）。
	if d := e.Evaluate(now, []Node{offlineNode(now)}); len(d) != 0 {
		t.Fatalf("第一拍不该产生决策: %+v", d)
	}
	// 第二拍判定为离线：静默期内要更新状态，但不发通知。
	d := e.Evaluate(now.Add(time.Second), []Node{offlineNode(now)})
	if len(d) != 1 || d[0].State.State != StateFiring {
		t.Fatalf("静默期内仍要更新状态: %+v", d)
	}
	if d[0].Notify {
		t.Fatal("静默期内不该发通知")
	}

	// 静默期内也不该反复产生决策。
	if again := e.Evaluate(now.Add(30*time.Second), []Node{offlineNode(now)}); len(again) != 0 {
		t.Fatalf("静默期内不该重复评估出通知: %+v", again)
	}

	// 静默期结束后，仍然离线 → 补发一次。
	after := now.Add(61 * time.Second)
	later := e.Evaluate(after, []Node{offlineNode(after)})
	if len(later) != 1 || !later[0].Notify {
		t.Fatalf("静默期后应当补发: %+v", later)
	}
}

func TestRecoveredRequiresStableOnline(t *testing.T) {
	base := time.Now().Add(-time.Hour)
	p := testParams()
	p.OfflineDebounce = 0
	e := NewEngine(p, base)
	now := base.Add(time.Hour)

	// 第一拍记录条件、第二拍判定离线并通知。
	e.Evaluate(now, []Node{offlineNode(now)})
	if d := e.Evaluate(now.Add(time.Second), []Node{offlineNode(now)}); len(d) != 1 || !d[0].Notify {
		t.Fatalf("先要触发离线: %+v", d)
	}

	// 刚上线：不发"已恢复"。
	if d := e.Evaluate(now.Add(2*time.Second), []Node{onlineNode(now)}); len(d) != 0 {
		t.Fatalf("刚上线不该立刻发恢复: %+v", d)
	}
	// 稳定 29 秒：还不够。
	if d := e.Evaluate(now.Add(29*time.Second), []Node{onlineNode(now)}); len(d) != 0 {
		t.Fatalf("稳定时长不足时不该发恢复: %+v", d)
	}
	// 稳定足够久（从 +2s 起算满 30 秒）：发恢复，并把离线规则置为 resolved。
	decisions := e.Evaluate(now.Add(33*time.Second), []Node{onlineNode(now)})
	var recovered, resolved bool
	for _, d := range decisions {
		if d.State.Rule == RuleRecovered && d.State.State == StateFiring && d.Notify {
			recovered = true
			if !strings.Contains(d.Notification.Body, "离线时长") {
				t.Fatalf("恢复通知应当说明离线时长: %q", d.Notification.Body)
			}
		}
		if d.State.Rule == RuleOffline && d.State.State == StateResolved {
			resolved = true
		}
	}
	if !recovered {
		t.Fatalf("应当发出恢复通知: %+v", decisions)
	}
	if !resolved {
		t.Fatalf("离线规则应当被置为 resolved: %+v", decisions)
	}

	// 再离线：因为离线规则已 resolved，冷却不应挡住新一轮告警。
	next := now.Add(2 * time.Minute)
	e.Evaluate(next, []Node{offlineNode(next)}) // 第一拍记录条件起点
	if d := e.Evaluate(next.Add(time.Second), []Node{offlineNode(next)}); len(d) != 1 || !d[0].Notify {
		t.Fatalf("下一轮离线应当重新通知: %+v", d)
	}
}

func TestRecoveredNotSentWithoutPriorOffline(t *testing.T) {
	base := time.Now().Add(-time.Hour)
	p := testParams()
	e := NewEngine(p, base)
	now := base.Add(time.Hour)

	// 节点一直是好的：不该发任何"已恢复"。
	for i := 0; i < 5; i++ {
		at := now.Add(time.Duration(i) * 20 * time.Second)
		if d := e.Evaluate(at, []Node{onlineNode(at)}); len(d) != 0 {
			t.Fatalf("一直在线时不该有决策: %+v", d)
		}
	}
}

func TestTrafficWarnOncePerCycle(t *testing.T) {
	base := time.Now().Add(-time.Hour)
	p := testParams()
	e := NewEngine(p, base)
	now := base.Add(time.Hour)
	cycleStart := time.Date(now.Year(), now.Month(), 19, 0, 0, 0, 0, time.UTC)

	node := onlineNode(now)
	node.TrafficLimit = 100 << 30
	node.TrafficWarnPct = 80
	node.CycleStart = cycleStart
	node.CycleEnd = cycleStart.AddDate(0, 1, 0)
	node.CycleRx = 85 << 30

	d := e.Evaluate(now, []Node{node})
	if len(d) != 1 || d[0].State.Rule != RuleTrafficWarn || !d[0].Notify {
		t.Fatalf("应当触发流量预警: %+v", d)
	}
	if d[0].Notification.Severity != SeverityWarn {
		t.Fatalf("预警级别 = %s", d[0].Notification.Severity)
	}
	// 同一周期内不重复。
	if again := e.Evaluate(now.Add(time.Minute), []Node{node}); len(again) != 0 {
		t.Fatalf("同一周期不该重复预警: %+v", again)
	}
	// 进入新周期：重新预警。
	node.CycleStart = cycleStart.AddDate(0, 1, 0)
	node.CycleEnd = cycleStart.AddDate(0, 2, 0)
	if next := e.Evaluate(now.Add(2*time.Minute), []Node{node}); len(next) != 1 || !next[0].Notify {
		t.Fatalf("新周期应当重新预警: %+v", next)
	}
}

func TestTrafficExceededFireAndResolve(t *testing.T) {
	base := time.Now().Add(-time.Hour)
	p := testParams()
	e := NewEngine(p, base)
	now := base.Add(time.Hour)
	cycleStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)

	node := onlineNode(now)
	node.TrafficLimit = 100 << 30
	node.TrafficWarnPct = 80
	node.CycleStart = cycleStart
	node.CycleEnd = cycleStart.AddDate(0, 1, 0)
	node.CycleRx = 120 << 30 // 已超额

	d := e.Evaluate(now, []Node{node})
	var exceeded, warned bool
	for _, item := range d {
		if item.State.Rule == RuleTrafficExceeded && item.Notify {
			exceeded = true
			if item.Notification.Severity != SeverityCritical {
				t.Fatalf("超额级别 = %s", item.Notification.Severity)
			}
		}
		if item.State.Rule == RuleTrafficWarn {
			warned = true
		}
	}
	if !exceeded {
		t.Fatalf("应当触发超额告警: %+v", d)
	}
	if !warned {
		t.Fatalf("超额时通常也已经超过预警线: %+v", d)
	}

	// 新周期清零（用量掉回阈值下）→ 超额规则被 resolve。
	node.CycleStart = cycleStart.AddDate(0, 1, 0)
	node.CycleEnd = cycleStart.AddDate(0, 2, 0)
	node.CycleRx = 1 << 30
	resolved := e.Evaluate(now.Add(time.Hour), []Node{node})
	found := false
	for _, item := range resolved {
		if item.State.Rule == RuleTrafficExceeded && item.State.State == StateResolved {
			found = true
		}
	}
	if !found {
		t.Fatalf("新周期应当把超额规则置为 resolved: %+v", resolved)
	}
}

func TestExpiryBucketsAndRenewal(t *testing.T) {
	base := time.Now().Add(-time.Hour)
	p := testParams()
	e := NewEngine(p, base)
	now := base.Add(time.Hour)

	node := onlineNode(now)
	node.ExpiresAt = now.Add(6 * 24 * time.Hour).Unix() // 剩 6 天 → 7 天档

	d := e.Evaluate(now, []Node{node})
	if len(d) != 1 || d[0].State.Rule != RuleExpiry || !d[0].Notify {
		t.Fatalf("剩 6 天应当提醒: %+v", d)
	}
	if d[0].State.Context != "7d" {
		t.Fatalf("档位 = %q，期望 7d", d[0].State.Context)
	}

	// 还在 7 天档内：不重复。
	node.ExpiresAt = now.Add(5 * 24 * time.Hour).Unix()
	if again := e.Evaluate(now.Add(time.Hour), []Node{node}); len(again) != 0 {
		t.Fatalf("同一档位不该重复提醒: %+v", again)
	}

	// 剩 2 天（此刻过了 2 小时，还剩 70 小时）→ 3 天档（更急的档位要再说一次）。
	node.ExpiresAt = now.Add(3 * 24 * time.Hour).Unix()
	d = e.Evaluate(now.Add(2*time.Hour), []Node{node})
	if len(d) != 1 || d[0].State.Context != "3d" {
		t.Fatalf("应当切到 3d 档: %+v", d)
	}

	// 剩 17 小时 → 1 天档。
	node.ExpiresAt = now.Add(20 * time.Hour).Unix()
	d = e.Evaluate(now.Add(3*time.Hour), []Node{node})
	if len(d) != 1 || d[0].State.Context != "1d" {
		t.Fatalf("应当切到 1d 档: %+v", d)
	}

	// 已过期。
	node.ExpiresAt = now.Add(-2 * time.Hour).Unix()
	d = e.Evaluate(now.Add(4*time.Hour), []Node{node})
	if len(d) != 1 || d[0].State.Context != "expired" {
		t.Fatalf("应当提醒已过期: %+v", d)
	}
	if !strings.Contains(d[0].Notification.Title, "已到期") {
		t.Fatalf("标题 = %q", d[0].Notification.Title)
	}

	// 续期后 → 规则 resolve。
	node.ExpiresAt = now.Add(300 * 24 * time.Hour).Unix()
	d = e.Evaluate(now.Add(5*time.Hour), []Node{node})
	if len(d) != 1 || d[0].State.State != StateResolved {
		t.Fatalf("续期后应当 resolve: %+v", d)
	}
}

// TestExpiryWhenWording 逐字钉住到期文案（②③）。
//
// 三件事各自都是线上踩过的：
//   - 不足 24 小时写成「剩余 0 天」，读起来像"今天到期"，而它可能还剩 23 小时；
//   - 过期天数被固定写成 -1，于是过期 30 天也显示「已过期 1 天」，
//     看起来像"昨天刚过期，还来得及"；
//   - 溢出的两个方向都要有下界说明（不到 1 分钟 / 刚过期）。
func TestExpiryWhenWording(t *testing.T) {
	const day = 24 * time.Hour
	cases := []struct {
		name      string
		remaining time.Duration
		days      int
		want      string
	}{
		{"还剩 3 整天", 3*day + 5*time.Hour, 3, "剩余 3 天"},
		{"还剩 23 小时", 23 * time.Hour, 0, "剩余不足 1 天（约 23 小时）"},
		{"还剩 5 小时", 5 * time.Hour, 0, "剩余不足 1 天（约 5 小时）"},
		{"还剩 45 分钟", 45 * time.Minute, 0, "剩余不足 1 天（约 45 分钟）"},
		{"还剩 30 秒", 30 * time.Second, 0, "剩余不足 1 天（不到 1 分钟）"},
		{"过期 1 天", -25 * time.Hour, -1, "已过期 1 天"},
		{"过期 30 天", -30 * day, -30, "已过期 30 天"},
		{"过期 6 小时", -6 * time.Hour, 0, "已过期不足 1 天（约 6 小时）"},
	}
	for _, c := range cases {
		if got := expiryWhen(c.remaining, c.days); got != c.want {
			t.Errorf("%s：expiryWhen(%s, %d) = %q，期望 %q",
				c.name, c.remaining, c.days, got, c.want)
		}
	}
}

// TestExpirySeverityAndRealDays 走完整规则：过期 30 天必须写 30 天，而且是 🔴；
// 即将到期仍是 🟡（⑥）。
func TestExpirySeverityAndRealDays(t *testing.T) {
	// 截到整秒：到期时间在库里就是整秒，对齐之后"约 5 小时"才是确定的。
	now := time.Now().Truncate(time.Second)
	base := now.Add(-time.Hour)
	e := NewEngine(testParams(), base)

	// 已过期 30 天（服务端停机一个月后重启的样子：状态还在 1d 档，重算时已经是过期）。
	node := onlineNode(now)
	node.ExpiresAt = now.Add(-30 * 24 * time.Hour).Unix()
	d := e.Evaluate(now, []Node{node})
	if len(d) != 1 || !d[0].Notify {
		t.Fatalf("应当提醒已过期: %+v", d)
	}
	if got := d[0].Notification.Severity; got != SeverityCritical {
		t.Errorf("已到期的级别 = %s，期望 critical（🔴）", got)
	}
	if body := d[0].Notification.Body; !strings.Contains(body, "已过期 30 天") {
		t.Errorf("过期 30 天必须写 30 天，实际 %q", body)
	}

	// 即将到期：只差 5 小时，写清小时数，级别仍是 warn。
	e2 := NewEngine(testParams(), base)
	node2 := onlineNode(now)
	node2.ExpiresAt = now.Add(5 * time.Hour).Unix()
	d2 := e2.Evaluate(now, []Node{node2})
	if len(d2) != 1 || !d2[0].Notify {
		t.Fatalf("应当提醒即将到期: %+v", d2)
	}
	if got := d2[0].Notification.Severity; got != SeverityWarn {
		t.Errorf("即将到期的级别 = %s，期望 warn（🟡）", got)
	}
	if body := d2[0].Notification.Body; !strings.Contains(body, "剩余不足 1 天（约 5 小时）") {
		t.Errorf("不足 1 天的文案不对: %q", body)
	}
}

func TestNoExpiryAlertWithoutConfig(t *testing.T) {
	base := time.Now().Add(-time.Hour)
	e := NewEngine(testParams(), base)
	now := base.Add(time.Hour)
	node := onlineNode(now) // ExpiresAt = 0
	if d := e.Evaluate(now, []Node{node}); len(d) != 0 {
		t.Fatalf("没填到期时间就不该提醒: %+v", d)
	}
}

func TestDeletedNodeStateIsCleared(t *testing.T) {
	base := time.Now().Add(-time.Hour)
	p := testParams()
	p.OfflineDebounce = 0
	e := NewEngine(p, base)
	now := base.Add(time.Hour)

	e.Evaluate(now, []Node{offlineNode(now)})
	e.Evaluate(now.Add(time.Second), []Node{offlineNode(now)})
	if len(e.States()) == 0 {
		t.Fatal("应当有离线状态")
	}
	// 节点被删掉：状态清理掉。
	e.Evaluate(now.Add(2*time.Second), nil)
	if states := e.States(); len(states) != 0 {
		t.Fatalf("节点删除后不该残留状态: %+v", states)
	}
}
