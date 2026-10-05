package alert

import (
	"strings"
	"testing"
	"time"

	"probe/internal/store"
)

// testParams 返回一份"没有静默期、冷却很长"的参数，便于断言。
//
// Loc 显式给 UTC，不留 nil：DefaultParams 的 Loc 是 nil，含义是"退回进程本地时区"
// （那边是给 --timezone 没接上的老行为兜底的），而进程本地时区随机器而变
// ——Windows 上 time.Local.String() 是 "Local"，Linux 上（CI）是 "UTC"。
// 用例要断言的内容必须与跑在哪台机器上无关，所以这里把它钉死。
func testParams() Params {
	p := DefaultParams()
	p.Loc = time.UTC
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

// 保存启动静默期时，静默期的起点要重置为**保存那一刻**（方案 B）。
//
// 病根：started 原本只是"进程启动时刻"，而设置页把启动静默期从 60s 改成 1h 时，
// 它会接着进程启动那一刻算 —— 面板已经跑了 10 分钟，于是"从现在起再静默 50 分钟"。
// 这 50 分钟里的真告警一条都发不出去，而用户改这个值的本意只是"启动那阵子的抖动
// 别报"。新语义：这次改动之后重新开始静默。
//
// 断言为什么挑"保存后 59 分钟"这个点：新旧行为在保存当刻都是"静默中"（新的算 0 分钟、
// 旧的算 10 分钟），只有往后走才分得开 —— 新行为要静默满 1 小时（到第 60 分钟），
// 旧行为在第 50 分钟就放行了。反向验证时（把 SetParamsRestartingGrace 换回
// SetParams）红的正是这一条。
func TestSetParamsRestartingGraceRestartsSilenceWindow(t *testing.T) {
	now := time.Now()
	p := testParams()
	p.StartupGrace = time.Minute
	// 进程已经跑了 10 分钟：1 分钟的静默期早就过去了。
	e := NewEngine(p, now.Add(-10*time.Minute))
	if e.Silence(now) {
		t.Fatal("前置条件不成立：10 分钟前起的 1 分钟静默期不该还在静默")
	}

	// 保存成 1h：静默期从**现在**重新开始计时。
	changed := p
	changed.StartupGrace = time.Hour
	e.SetParamsRestartingGrace(changed, now)

	if !e.Silence(now) {
		t.Error("保存后应当重新进入静默期")
	}
	if !e.Silence(now.Add(59 * time.Minute)) {
		t.Error("保存后 59 分钟应当在 1h 的静默期内（旧行为按进程启动时刻算，第 50 分钟就放行了）")
	}
	if e.Silence(now.Add(61 * time.Minute)) {
		t.Error("静默期不该超过「保存后 1 小时」：起点就是保存那一刻")
	}

	// 对照组：只改**别的**告警参数（走 SetParams）不该重置起点 —— 用户改的常常只是
	// "重复提醒间隔"，那时把静默期重新开始计时是个意外副作用（他好不容易等到静默期
	// 过去，改一下别的参数又静默了一轮）。这一条同时钉住"不要每次换参数都重置"。
	other := NewEngine(p, now.Add(-10*time.Minute))
	tweaked := p
	tweaked.NotifyCooldown = 5 * time.Minute
	other.SetParams(tweaked)
	if other.Silence(now) {
		t.Error("只换别的参数不该重新开始静默：SetParams 不动静默期的起点")
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

// traffic_warn_pct = 0 是"没配阈值"，不是"阈值 0%"。
//
// store 的校验允许 0（只挡 <0 与 >100），PUT /api/v1/nodes/{id} 也不补默认值
// （只有创建接口补 80），所以 0 真的能存进库 —— 而 pct >= 0 恒成立，会让每个计费
// 周期一开始就发一条「流量接近额度 已用 0.00%」，同一个数字在面板上
// （web/app.js 的 traffic_warn_pct > 0）却什么都不显示。
func TestTrafficWarnZeroMeansDisabled(t *testing.T) {
	base := time.Now().Add(-time.Hour)
	e := NewEngine(testParams(), base)
	now := base.Add(time.Hour)
	cycleStart := time.Date(now.Year(), now.Month(), 19, 0, 0, 0, 0, time.UTC)

	node := onlineNode(now)
	node.TrafficLimit = 100 << 30
	node.TrafficWarnPct = 0
	node.CycleStart = cycleStart
	node.CycleEnd = cycleStart.AddDate(0, 1, 0)
	node.CycleRx = 0 // 一点都没用：这正是会误报「已用 0.00%」的时刻

	if d := e.Evaluate(now, []Node{node}); len(d) != 0 {
		t.Fatalf("阈值为 0（没配）时不该有流量预警: %+v", d)
	}

	// 阈值照旧生效：同一台机器配上 1% 就会预警（证明上面不是因为别的原因空着）。
	node.TrafficWarnPct = 1
	node.CycleRx = 2 << 30
	d := e.Evaluate(now.Add(time.Minute), []Node{node})
	if len(d) != 1 || d[0].State.Rule != RuleTrafficWarn || !d[0].Notify {
		t.Fatalf("配上阈值后应当照旧预警: %+v", d)
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

// 节点被删除后，去抖/恢复确认那两张表也不能留下孤儿键。
//
// 它们只对"当前存在的节点"起作用，所以漏清不影响任何判定 —— 但一台"刚离线、
// 还没到去抖时间就被删掉"的节点从来没产生过 states 行（清理挂在 states 上，
// 扫不到它），而 onlineSince 只在节点变成 offline 时才删，直接删除的节点没人清。
func TestDeletedNodeClearsOrphanTimers(t *testing.T) {
	base := time.Now().Add(-time.Hour)
	p := testParams()
	p.OfflineDebounce = time.Hour // 去抖远未到点：这台节点不会产生 states 行
	p.RecoverStable = time.Hour   // 恢复确认也没到点
	e := NewEngine(p, base)
	now := base.Add(time.Hour)

	online := onlineNode(now) // 写 onlineSince
	online.ID = 1
	offline := offlineNode(now) // 写 conditionSince，但到不了点
	offline.ID = 2
	e.Evaluate(now, []Node{online, offline})
	if len(e.onlineSince) != 1 || len(e.conditionSince) != 1 {
		t.Fatalf("前置条件不成立: onlineSince=%d conditionSince=%d",
			len(e.onlineSince), len(e.conditionSince))
	}

	// 两台一起被删掉。
	e.Evaluate(now.Add(time.Second), nil)
	if n := len(e.onlineSince); n != 0 {
		t.Errorf("节点删除后 onlineSince 残留 %d 个键", n)
	}
	if n := len(e.conditionSince); n != 0 {
		t.Errorf("节点删除后 conditionSince 残留 %d 个键", n)
	}
}

// ---------------------------------------------------------------- 05-A-1 / 05-A-4 / 05-A-11

// findDecision 取出某条规则的决策（同一拍里可能同时有流量预警与超额两条）。
//
// 找不到时返回零值：它的 State.Rule 是空串，断言写成"规则名不对"就能一眼看出。
func findDecision(decisions []Decision, rule string) Decision {
	for _, d := range decisions {
		if d.State.Rule == rule {
			return d
		}
	}
	return Decision{}
}

// TestStartupGraceDoesNotSwallowTrafficWarn 是 05-A-1 的主用例（四类里的一类）。
//
// 病根：fire 无条件把周期标记（Context）写进状态，而"每个计费周期只提醒一次"
// 正是按 Context 去重的 —— 于是静默期里跨过阈值的机器，**整个计费周期**一条都
// 收不到「流量接近额度」。修法：静默期按住的这一拍不写周期标记。
//
// 每一拍都传**完整节点列表**（Evaluate 会把本拍没出现的节点的状态删掉；逐个节点
// 单独 Evaluate 的探针会得出相反结论，见审计报告 A-1 的证据链提示）。
func TestStartupGraceDoesNotSwallowTrafficWarn(t *testing.T) {
	now := time.Now()
	p := testParams()
	p.StartupGrace = 60 * time.Second
	e := NewEngine(p, now) // 静默期的起点 = 进程刚启动
	cycleStart := time.Date(now.Year(), now.Month(), 19, 0, 0, 0, 0, time.UTC)
	cycleTag := cycleStart.Format("2006-01-02")

	node := onlineNode(now)
	node.TrafficLimit = 100 << 30
	node.TrafficWarnPct = 80
	node.CycleStart = cycleStart
	node.CycleEnd = cycleStart.AddDate(0, 1, 0)
	node.CycleRx = 85 << 30

	// 静默期第一拍：状态照样进 firing（要落盘），但不发通知，而且**不许**把
	// "这一期已经提醒过"写进 Context。
	d := e.Evaluate(now.Add(time.Second), []Node{node})
	if len(d) != 1 || d[0].State.Rule != RuleTrafficWarn || d[0].Notify {
		t.Fatalf("静默期内应当只更新状态、不发通知: %+v", d)
	}
	if d[0].State.Context == cycleTag {
		t.Fatalf("静默期里写下了周期标记 %q：这一期会被永久吞掉", d[0].State.Context)
	}
	// 静默期内也不反复产生决策。
	if again := e.Evaluate(now.Add(30*time.Second), []Node{node}); len(again) != 0 {
		t.Fatalf("静默期内不该重复产生决策: %+v", again)
	}

	// 静默期结束：补发这一期的唯一一条提醒。
	after := now.Add(61 * time.Second)
	d = e.Evaluate(after, []Node{node})
	if len(d) != 1 || !d[0].Notify || d[0].State.Rule != RuleTrafficWarn {
		t.Fatalf("静默期结束后应当补发流量预警: %+v", d)
	}
	if d[0].State.Context != cycleTag {
		t.Fatalf("补发之后周期标记 = %q，期望 %q", d[0].State.Context, cycleTag)
	}
	// 补发之后仍然"每周期一次"：补发不会变成刷屏。
	if again := e.Evaluate(after.Add(time.Second), []Node{node}); len(again) != 0 {
		t.Fatalf("补发之后同一周期不该再提醒: %+v", again)
	}
}

// TestStartupGraceDoesNotSwallowTrafficExceeded 是同一件事在"已超额"上的形态。
//
// 它的去重条件是 (Context == cycleTag && TrafficRepeat <= 0)：默认 TrafficRepeat=0，
// 所以静默期写下的 Context 一样会让它这一期永远不再发。
func TestStartupGraceDoesNotSwallowTrafficExceeded(t *testing.T) {
	now := time.Now()
	p := testParams()
	p.StartupGrace = 60 * time.Second
	e := NewEngine(p, now)
	cycleStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	cycleTag := cycleStart.Format("2006-01-02")

	node := onlineNode(now)
	node.TrafficLimit = 100 << 30
	node.TrafficWarnPct = 80 // 超额时预警也成立：两条都该在静默期后被补上
	node.CycleStart = cycleStart
	node.CycleEnd = cycleStart.AddDate(0, 1, 0)
	node.CycleRx = 120 << 30

	d := e.Evaluate(now.Add(time.Second), []Node{node})
	if got := findDecision(d, RuleTrafficExceeded); got.State.Rule == "" || got.Notify {
		t.Fatalf("静默期内应当只更新超额状态: %+v", d)
	}
	if got := findDecision(d, RuleTrafficExceeded); got.State.Context == cycleTag {
		t.Fatalf("静默期里写下了周期标记 %q：整个周期收不到「流量已超额」", got.State.Context)
	}
	if again := e.Evaluate(now.Add(30*time.Second), []Node{node}); len(again) != 0 {
		t.Fatalf("静默期内不该重复产生决策: %+v", again)
	}

	after := now.Add(61 * time.Second)
	d = e.Evaluate(after, []Node{node})
	got := findDecision(d, RuleTrafficExceeded)
	if !got.Notify {
		t.Fatalf("静默期结束后应当补发「流量已超额」: %+v", d)
	}
	if got.State.Context != cycleTag {
		t.Fatalf("补发之后周期标记 = %q，期望 %q", got.State.Context, cycleTag)
	}
	if again := e.Evaluate(after.Add(time.Second), []Node{node}); len(again) != 0 {
		t.Fatalf("补发之后同一周期不该再提醒: %+v", again)
	}
}

// TestStartupGraceDoesNotSwallowExpiry 是同一件事在"到期档位"上的形态。
//
// 档位（7d/3d/1d/expired）是去重键：静默期里写下的档位会把那一档永久吞掉 ——
// 只在"1 天档"那一档静默的机器，可能直到过期才再次出声。
func TestStartupGraceDoesNotSwallowExpiry(t *testing.T) {
	now := time.Now()
	p := testParams()
	p.StartupGrace = 60 * time.Second
	e := NewEngine(p, now)

	node := onlineNode(now)
	node.ExpiresAt = now.Add(6 * 24 * time.Hour).Unix() // 剩 6 天 → 7 天档

	d := e.Evaluate(now.Add(time.Second), []Node{node})
	if len(d) != 1 || d[0].State.Rule != RuleExpiry || d[0].Notify {
		t.Fatalf("静默期内应当只更新到期状态: %+v", d)
	}
	if d[0].State.Context == "7d" {
		t.Fatalf("静默期里写下了档位 %q：这一档会被永久吞掉", d[0].State.Context)
	}
	if again := e.Evaluate(now.Add(30*time.Second), []Node{node}); len(again) != 0 {
		t.Fatalf("静默期内不该重复产生决策: %+v", again)
	}

	d = e.Evaluate(now.Add(61*time.Second), []Node{node})
	if len(d) != 1 || !d[0].Notify || d[0].State.Rule != RuleExpiry {
		t.Fatalf("静默期结束后应当补发到期提醒: %+v", d)
	}
	if d[0].State.Context != "7d" {
		t.Fatalf("补发之后档位 = %q，期望 7d", d[0].State.Context)
	}
}

// TestStartupGraceDoesNotSwallowRecovered 钉住四类里的最后一类（也是最隐蔽的一类）。
//
// 恢复通知没发出去时，旧代码照样把离线规则 resolve 掉 —— 而 evaluateOffline
// 一进门就靠"离线规则还是 firing"判断要不要发恢复，于是「节点已恢复」**永远**
// 补不上（审计报告实测：recovered state=firing lastNotify=0001-01-01）。
func TestStartupGraceDoesNotSwallowRecovered(t *testing.T) {
	now := time.Now()
	p := testParams()
	p.StartupGrace = 60 * time.Second
	p.OfflineDebounce = 0
	e := NewEngine(p, now)

	off := offlineNode(now)
	on := onlineNode(now)

	// 静默期内：离线规则进 firing，但不发通知（去抖哪怕配成 0，第一拍也只是
	// 记录条件起点，所以这里要两拍）。
	if d := e.Evaluate(now.Add(time.Second), []Node{off}); len(d) != 0 {
		t.Fatalf("第一拍只记录条件起点: %+v", d)
	}
	if d := e.Evaluate(now.Add(2*time.Second), []Node{off}); len(d) != 1 || d[0].Notify {
		t.Fatalf("静默期内离线应当只更新状态: %+v", d)
	}
	// 静默期内又在线并稳定满 RecoverStable（30s）：恢复通知也发不出去。
	if d := e.Evaluate(now.Add(10*time.Second), []Node{on}); len(d) != 0 {
		t.Fatalf("刚上线不该有决策: %+v", d)
	}
	d := e.Evaluate(now.Add(45*time.Second), []Node{on})
	if len(d) != 1 || d[0].State.Rule != RuleRecovered || d[0].Notify {
		t.Fatalf("静默期内应当只更新恢复状态: %+v", d)
	}
	// 关键：静默期里**不许**把离线规则收尾成 resolved —— 收了的话下面那条
	// 「节点已恢复」就永远补不上。
	if off := e.states[key{on.ID, RuleOffline}]; off == nil || off.State != StateFiring {
		t.Fatalf("静默期内不该 resolve 离线规则: %+v", off)
	}

	// 静默期结束：补发「节点已恢复」，并在**真的发出去之后**收尾离线规则。
	decisions := e.Evaluate(now.Add(61*time.Second), []Node{on})
	rec := findDecision(decisions, RuleRecovered)
	if rec.State.Rule == "" || !rec.Notify {
		t.Fatalf("静默期结束后应当补发「节点已恢复」: %+v", decisions)
	}
	if res := findDecision(decisions, RuleOffline); res.State.State != StateResolved {
		t.Fatalf("恢复通知真发出去之后，离线规则应当置为 resolved: %+v", decisions)
	}
}

// TestNotifyCooldownZeroFallsBackToFloor 钉住 05-A-4 的修法。
//
// 冷却时间的语义是"同一条规则重复通知的**最短间隔**"，0 等于取消节流：引擎每秒
// 评估一次，持续 firing 的规则于是每秒产生一条通知（过了合并窗口与限流之后用户
// 实际收到 ~15~28 条/分钟），并长期占用与真告警共享的发送额度。
// 修法：0（与负数）抬到 minNotifyCooldown；非 0 取值一律不变。
func TestNotifyCooldownZeroFallsBackToFloor(t *testing.T) {
	base := time.Now().Add(-time.Hour)
	p := testParams()
	p.NotifyCooldown = 0 // 设置页 {"cooldown":"0"} 与 --alert-cooldown 0 都能到这儿
	p.OfflineDebounce = 0
	e := NewEngine(p, base)
	now := base.Add(time.Hour)

	sent := 0
	for i := 0; i < 60; i++ {
		at := now.Add(time.Duration(i) * time.Second)
		for _, d := range e.Evaluate(at, []Node{offlineNode(at)}) {
			if d.Notify {
				sent++
			}
		}
	}
	if sent != 1 {
		t.Fatalf("冷却为 0 时 60 拍发了 %d 条重复通知，期望 1 条（下界 %s）", sent, minNotifyCooldown)
	}

	// 下界是 1 分钟，不是"再也不重复"：跨过下界之后照常重复提醒。
	at := now.Add(2 * time.Minute)
	repeated := 0
	for _, d := range e.Evaluate(at, []Node{offlineNode(at)}) {
		if d.Notify {
			repeated++
		}
	}
	if repeated != 1 {
		t.Fatalf("跨过下界之后应当重复提醒一条，实际 %d 条", repeated)
	}

	// 非 0 取值（哪怕小于下界）不受影响：30s 仍然是 30s。
	if got := normalizeParams(Params{NotifyCooldown: 30 * time.Second}).NotifyCooldown; got != 30*time.Second {
		t.Fatalf("非 0 的冷却时间被改成了 %s，期望 30s", got)
	}
}

// TestSilenceIsSafeOutsideEvaluateLock 钉住 05-A-11 的修法（结构陷阱）。
//
// Silence 是**导出**方法，谁都能在 Evaluate 之外调它，而 e.params 是被 SetParams
// 整体替换的 —— 所以它必须自己加锁。这条用例钉两件事：
//   - 锁外可调用（Evaluate 内部已改走 silenceLocked，不会自锁死）；
//   - 与并发的 SetParams / Evaluate 一起跑不会死锁。
//
// 本机 go test -race 不可用（只有 32 位 gcc），所以这里**不是**竞争检测，
// 只是"锁外可调用"这一条结构断言。
func TestSilenceIsSafeOutsideEvaluateLock(t *testing.T) {
	base := time.Now().Add(-time.Hour)
	p := testParams()
	p.StartupGrace = time.Minute
	e := NewEngine(p, base)
	now := base.Add(30 * time.Second)

	done := make(chan struct{})
	go func() {
		defer close(done)
		next := p
		next.StartupGrace = time.Hour
		for i := 0; i < 200; i++ {
			e.SetParams(next)
			e.Evaluate(now, []Node{offlineNode(now)})
			e.SetParams(p)
			if !e.Silence(now) {
				t.Errorf("静默期内 Silence 应当为真")
				return
			}
		}
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Silence 与 Evaluate 并发时卡住了：导出方法必须自己加锁，内部走 silenceLocked")
	}
}

// 流量预警必须按节点的**计费口径**判：额度是按那个口径结算的，"离额度还有多远"
// 就不能拿收 + 发去比一个只算流出的额度（那会在真实用量还不到一半时就报警）。
//
// 表里每一行都是"同一批字节、同一个额度、只有口径不同"的对照，所以它同时钉住了
// 三件事：算哪个数、正文里写哪个数、以及不跟口径走的那部分一个字都没变。
func TestTrafficAlertsFollowBillingMode(t *testing.T) {
	const (
		gib   = int64(1) << 30
		limit = 100 * gib
	)
	base := time.Now().Add(-time.Hour)
	now := base.Add(time.Hour)
	cycleStart := time.Date(now.Year(), now.Month(), 19, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name         string
		billing      int
		rx, tx       int64
		wantUsed     int64
		wantWarn     bool
		wantExceeded bool
	}{
		{"双向：40 + 45 = 85% ⇒ 预警", store.BillingBoth, 40 * gib, 45 * gib, 85 * gib, true, false},
		{"只算出站：发 45 = 45%（收的 40 不算）", store.BillingTxOnly, 40 * gib, 45 * gib, 45 * gib, false, false},
		{"只算入站：收 40 = 40%（发的 45 不算）", store.BillingRxOnly, 40 * gib, 45 * gib, 40 * gib, false, false},
		{"只算出站：发 90 = 90% ⇒ 预警", store.BillingTxOnly, 5 * gib, 90 * gib, 90 * gib, true, false},
		{"只算入站：收 120 = 120% ⇒ 超额", store.BillingRxOnly, 120 * gib, 1 * gib, 120 * gib, true, true},
		{"双向：收 200 = 200% ⇒ 超额", store.BillingBoth, 200 * gib, 0, 200 * gib, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := NewEngine(testParams(), base)
			node := onlineNode(now)
			node.TrafficLimit = limit
			node.TrafficWarnPct = 80
			node.TrafficBilling = c.billing
			node.CycleStart = cycleStart
			node.CycleEnd = cycleStart.AddDate(0, 1, 0)
			node.CycleRx = c.rx
			node.CycleTx = c.tx

			var warnBody string
			warned, exceeded := false, false
			for _, d := range e.Evaluate(now, []Node{node}) {
				switch d.State.Rule {
				case RuleTrafficWarn:
					if d.Notify {
						warned = true
						warnBody = d.Notification.Body
					}
				case RuleTrafficExceeded:
					if d.Notify {
						exceeded = true
					}
				}
			}
			if warned != c.wantWarn {
				t.Errorf("预警 = %v，期望 %v", warned, c.wantWarn)
			}
			if exceeded != c.wantExceeded {
				t.Errorf("超额 = %v，期望 %v", exceeded, c.wantExceeded)
			}
			// 正文里的"本周期已用"必须是**同一个**口径：pct 按口径算、正文写收 + 发
			// 的话，会发出一条「已用 150 GB / 100 GB（45.0%）」这种自相矛盾的消息。
			if c.wantWarn {
				if !strings.Contains(warnBody, FormatBytes(c.wantUsed)) {
					t.Errorf("预警正文里的已用量应当是 %s，实际：%q",
						FormatBytes(c.wantUsed), warnBody)
				}
			}
		})
	}
}

// 告警正文里的百分比必须与**面板**逐位相同（用户 2026-10-05 拍板 ②）。
//
// 面板上的百分比是浏览器用 Number.prototype.toFixed(1) 渲染的，告警正文是 Go
// 渲染的：Go 的 %.1f 对"恰好落在 .x5 上"的二进制精确值取偶（85.25 → 85.2%），
// 浏览器给 85.3% —— 同一台机器在手机上一条消息、在面板上是另一个数，正是这一轮
// 要消灭的那类不一致。表里的期望值是 node 里**实测**出来的浏览器输出（同一张表
// 也钉在 internal/server 的 TestPctText1MatchesBrowserToFixed 上，两处用的是
// 同一个 alert.PctText1）。
func TestTrafficAlertBodyPctMatchesBrowserToFixed(t *testing.T) {
	base := time.Now().Add(-time.Hour)
	e := NewEngine(testParams(), base)
	now := base.Add(time.Hour)
	cycleStart := time.Date(now.Year(), now.Month(), 19, 0, 0, 0, 0, time.UTC)

	node := onlineNode(now)
	node.TrafficLimit = 400 * 1000 * 1000 * 1000
	node.CycleStart = cycleStart
	node.CycleEnd = cycleStart.AddDate(0, 1, 0)

	cases := []struct {
		pct  float64
		want string
		// goFmt 是 Go 的 %.1f 对同一个值会写出来的那串字：只有"恰好 .x5"的
		// 那几个与 want 不同，而那些正是这条用例的区分度所在。
		goFmt string
	}{
		{85.25, "85.3%", "85.2%"},
		{0.25, "0.3%", "0.2%"},
		{1.25, "1.3%", "1.2%"},
		{2.25, "2.3%", "2.2%"},
		{0.15, "0.1%", "0.1%"},
		{120, "120.0%", "120.0%"},
	}
	for _, c := range cases {
		body := e.trafficBody(node, c.pct)
		if !strings.Contains(body, "（"+c.want+"）") {
			t.Errorf("pct=%v 的告警正文 = %q，期望含（%s）（浏览器 toFixed(1) 的实测值）",
				c.pct, body, c.want)
		}
		if c.goFmt != c.want && strings.Contains(body, "（"+c.goFmt+"）") {
			t.Errorf("pct=%v 的告警正文写着 Go %%.1f 的取整结果（%s）：%q",
				c.pct, c.goFmt, body)
		}
	}
}

// 没填额度（limit = 0）时，三种口径都不该有任何流量预警：百分比没有分母，
// 也就无从判断"离额度还有多远"。这一条与前端 trafficSumText 的 limit <= 0 分支
// 是同一条口径（那边整段不写占比）。
//
// 三个口径各跑一遍是有意的：口径只改"用哪个数当分子"，不该把"没有分母就不报"
// 这条边界绕过去 —— 只测双向的话，一个"按口径分支提前 return"的写法会漏过它。
func TestTrafficAlertsSilentWithoutLimit(t *testing.T) {
	base := time.Now().Add(-time.Hour)
	now := base.Add(time.Hour)
	cycleStart := time.Date(now.Year(), now.Month(), 19, 0, 0, 0, 0, time.UTC)

	for _, billing := range []int{store.BillingBoth, store.BillingTxOnly, store.BillingRxOnly} {
		e := NewEngine(testParams(), base)
		node := onlineNode(now)
		node.TrafficLimit = 0 // 没填额度
		node.TrafficWarnPct = 80
		node.TrafficBilling = billing
		node.CycleStart = cycleStart
		node.CycleEnd = cycleStart.AddDate(0, 1, 0)
		node.CycleRx = 500 << 30
		node.CycleTx = 500 << 30

		if d := e.Evaluate(now, []Node{node}); len(d) != 0 {
			t.Errorf("口径 %d：没填额度时不该有任何决策，实际 %+v", billing, d)
		}
	}
}

// 换口径 ⇒ 流量预警记录被清 ⇒ 之后能重新触发（用户逐条确认的第六条）。
//
// 这条用例是**成对**的：同一个场景跑两遍，唯一的差别是有没有调 ForgetTrafficRules。
// 只断言"调了之后能重新提醒"是不够的 —— 那种断言在"状态根本没挡住"的实现下
// 也会绿（比如去重条件被谁改坏了），于是这条用例就永远证明不了它想证明的事。
// 对照那一半才是真正的靶子：**不调**的时候必须一声不响。
func TestForgetTrafficRulesAllowsRefireAfterBillingChange(t *testing.T) {
	const (
		gib   = int64(1) << 30
		limit = 100 * gib
	)
	base := time.Now().Add(-time.Hour)
	now := base.Add(time.Hour)
	cycleStart := time.Date(now.Year(), now.Month(), 19, 0, 0, 0, 0, time.UTC)

	// scenario 跑一遍完整的"改口径"流程，返回换口径之后那几拍里发出的规则名。
	scenario := func(t *testing.T, forget bool) (firedAfterSwitch []string) {
		t.Helper()
		e := NewEngine(testParams(), base)
		node := onlineNode(now)
		node.TrafficLimit = limit
		node.TrafficWarnPct = 80
		node.TrafficBilling = store.BillingBoth
		node.CycleStart = cycleStart
		node.CycleEnd = cycleStart.AddDate(0, 1, 0)
		node.CycleRx = 150 * gib // 双向 150% ⇒ 预警 + 超额各发一条

		first := e.Evaluate(now, []Node{node})
		var fired []string
		for _, d := range first {
			if d.Notify {
				fired = append(fired, d.State.Rule)
			}
		}
		if len(fired) != 2 {
			t.Fatalf("第一拍应当发出预警 + 超额两条，实际 %v", fired)
		}
		// 同一周期内不再重复（这是"记录"存在时的正常行为）。
		for _, d := range e.Evaluate(now.Add(time.Minute), []Node{node}) {
			if d.Notify {
				t.Fatalf("同一周期内不该重复提醒: %s", d.State.Rule)
			}
		}

		if forget {
			e.ForgetTrafficRules(node.ID)
		}

		// 用户把口径改成"只算出站"：同一批字节算出来是 0%（发是 0）⇒ 一条都不该发。
		switched := node
		switched.TrafficBilling = store.BillingTxOnly
		for _, d := range e.Evaluate(now.Add(2*time.Minute), []Node{switched}) {
			if d.Notify {
				t.Fatalf("改成只算出站后用量是 0%%，不该提醒: %s", d.State.Rule)
			}
		}

		// 之后真的把流量用超（发 90 GiB = 90%）⇒ 必须能**重新触发**。
		switched.CycleTx = 90 * gib
		for _, d := range e.Evaluate(now.Add(3*time.Minute), []Node{switched}) {
			if d.Notify {
				firedAfterSwitch = append(firedAfterSwitch, d.State.Rule)
			}
		}
		return firedAfterSwitch
	}

	t.Run("换口径时清了记录 ⇒ 重新触发", func(t *testing.T) {
		got := scenario(t, true)
		if len(got) != 1 || got[0] != RuleTrafficWarn {
			t.Fatalf("换口径后越过 80%% 应当重新发出预警，实际 %v", got)
		}
	})

	t.Run("对照：不清记录 ⇒ 这一期永远不会再提醒（这正是要修的毛病）", func(t *testing.T) {
		if got := scenario(t, false); len(got) != 0 {
			t.Fatalf("没有清记录时不该重新提醒，实际 %v —— 那说明去重键没起作用，"+
				"上面那一半「清了就能提醒」也就证明不了任何事", got)
		}
	})
}
