package server

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"probe/internal/alert"
	"probe/internal/protocol"
	"probe/internal/state"
	"probe/internal/store"
)

// 本机地址（Agent 自报）与服务端观测来源地址必须**各自**透传到 DTO：
// 前端靠 local_ip / local_ip6 显示「本机地址」，靠 observed_ip 显示「来源 IP」。
// 三者含义不同，谁也不能覆盖谁。
func TestBuildNodeDTOPassesThroughLocalIPs(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	node := store.Node{ID: 7, Name: "hk-01", IntervalSec: 1, Enabled: true}
	st := state.Node{
		NodeID:     7,
		Connected:  true,
		LastSeen:   now,
		ObservedIP: "127.0.0.1",
		LocalIP:    "203.0.113.5",
		LocalIP6:   "2001:db8::1",
		Info:       protocol.Info{AgentVersion: "0.1.0", Hostname: "hk-01"},
	}

	dto := buildNodeDTO(node, st, true, now, time.Minute, 2*time.Minute)
	if dto.ObservedIP != "127.0.0.1" {
		t.Errorf("observed_ip = %q，期望 127.0.0.1", dto.ObservedIP)
	}
	if dto.LocalIP != "203.0.113.5" {
		t.Errorf("local_ip = %q，期望 203.0.113.5", dto.LocalIP)
	}
	if dto.LocalIP6 != "2001:db8::1" {
		t.Errorf("local_ip6 = %q，期望 2001:db8::1", dto.LocalIP6)
	}

	// 没有内存状态时（节点从未连接）本机地址留空，而不是报错或填占位符：
	// 占位符是前端的事，服务端只传事实。
	empty := buildNodeDTO(node, state.Node{}, false, now, time.Minute, 2*time.Minute)
	if empty.LocalIP != "" || empty.LocalIP6 != "" || empty.ObservedIP != "" {
		t.Errorf("无状态时地址应当为空: %+v", empty)
	}
}

// 首页卡片四格资源要的字段必须逐个透传：少一个就是某一格没有副值
// （卡片照常渲染、控制台一声不吭）。
//
// 内存的绝对值与 5/15 分钟负载是"协议里早就有、DTO 一直没暴露"的三个字段：
// 缺了它们，卡片只能用 mem_pct 反推总量、CPU 那格只能显示一个 1 分钟负载。
func TestBuildNodeDTOPassesThroughResourceFields(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	node := store.Node{ID: 7, Name: "hk-01", IntervalSec: 1, Enabled: true}
	st := state.Node{
		NodeID:    7,
		Connected: true,
		LastSeen:  now,
		Metrics: protocol.Metrics{
			CPUPct: 2.5,
			Mem:    protocol.Mem{Total: 967 << 20, Used: 243 << 20, Pct: 25.1},
			Load:   protocol.Load{L1: 0.15, L5: 0.04, L15: 0.01},
			// 两个挂载点：服务端**不替前端挑**（详情页要用整个数组），
			// 卡片自己取 /（见 app.js 的 rootDiskOf）。
			Disk: []protocol.Disk{
				{Mount: "/", Total: 23 << 30, Used: 1800 << 20, Pct: 7.6},
				{Mount: "/data", Total: 100 << 30, Used: 40 << 30, Pct: 40},
			},
		},
	}

	dto := buildNodeDTO(node, st, true, now, time.Minute, 2*time.Minute)
	if dto.Load1 != 0.15 || dto.Load5 != 0.04 || dto.Load15 != 0.01 {
		t.Errorf("三个负载没有透传: %v / %v / %v", dto.Load1, dto.Load5, dto.Load15)
	}
	if dto.MemUsed != 243<<20 || dto.MemTotal != 967<<20 {
		t.Errorf("内存绝对值没有透传: used=%d total=%d", dto.MemUsed, dto.MemTotal)
	}
	if len(dto.Disks) != 2 || dto.Disks[0].Mount != "/" || dto.Disks[1].Mount != "/data" {
		t.Errorf("挂载点数组应当原样透传（顺序也保留）: %+v", dto.Disks)
	}

	// OnlineSec 是**连续在线时长**，由 Server.dtoFor 按状态填（见 online.go）：
	// buildNodeDTO 是纯函数，不碰服务端状态，所以这里恒为 0（= 不在线）。
	if dto.OnlineSec != 0 {
		t.Errorf("buildNodeDTO 不该自己算在线时长，实际 %d", dto.OnlineSec)
	}
}

// 到期文案：面板上显示的这句话（expires_text）必须与告警消息里那一行**逐字一致**。
//
// 它以前是两端各写一份的：告警那边早就会写「剩余不足 1 天（约 5 小时）」，
// 而面板（详情页「到期」那一格、首页卡片的费用行、设置页的服务器列表）
// 直接拿 remaining_days 拼 "N 天" —— 于是同一台机器在 Telegram 里说
// "不足 1 天"、在详情页上写「0 天」。那不是排版问题：差 5 小时与"今天到期"
// 对应完全不同的行动，而"刚过期"与"已经过期一个月"更是两回事。
func TestExpiresTextMatchesAlertWording(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	const day = 24 * time.Hour

	cases := []struct {
		name      string
		remaining time.Duration
		wantText  string
		wantDays  int64
	}{
		{"还剩 28 天", 28 * day, "剩余 28 天", 28},
		{"还剩 23 小时", 23 * time.Hour, "剩余不足 1 天（约 23 小时）", 0},
		{"还剩 5 小时", 5 * time.Hour, "剩余不足 1 天（约 5 小时）", 0},
		{"已过期 6 小时", -6 * time.Hour, "已过期不足 1 天（约 6 小时）", 0},
		{"已过期 3 天", -3 * day, "已过期 3 天", 0},
	}
	for _, c := range cases {
		node := store.Node{ID: 1, Name: "x", ExpiresAt: now.Add(c.remaining).Unix()}
		dto := buildNodeDTO(node, state.Node{}, false, now, time.Minute, 2*time.Minute)
		if dto.ExpiresText != c.wantText {
			t.Errorf("%s：expires_text = %q，期望 %q", c.name, dto.ExpiresText, c.wantText)
		}
		if dto.RemainingDays != c.wantDays {
			t.Errorf("%s：remaining_days = %d，期望 %d", c.name, dto.RemainingDays, c.wantDays)
		}
		// 「0 天」正是这次要修的 bug 的字面形态：它把"还差几小时"与
		// "已经过期很久"都压成同一个读起来像"今天到期"的说法。
		if strings.Contains(dto.ExpiresText, "0 天") {
			t.Errorf("%s：文案里出现了「0 天」：%q", c.name, dto.ExpiresText)
		}
	}

	// 没填到期日：整段不显示（空串），而不是写「0 天」。
	none := buildNodeDTO(store.Node{ID: 1, Name: "x"}, state.Node{}, false, now, time.Minute, 2*time.Minute)
	if none.ExpiresText != "" {
		t.Errorf("没填到期日时 expires_text 应当是空串，实际 %q", none.ExpiresText)
	}

	// 与告警引擎逐字对一次：同一台机器、同一个时刻，两边说的必须是同一句话。
	//
	// 引擎的时区是**显式注入**的：alert.DefaultParams() 的 Loc 是 nil，含义是
	// "退回进程本地时区"，而 Windows 上是 "Local"、Linux（CI）上是 "UTC" ——
	// 让断言依赖它，用例的结论就与机器有关了（多时区那条用例专门守这件事）。
	expiresAt := now.Add(5 * time.Hour)
	engine := newExpiryEngine(time.UTC, now.Add(-time.Hour))
	decisions := engine.Evaluate(now, []alert.Node{{
		ID: 1, Name: "x", Status: "online", LastSeen: now, ExpiresAt: expiresAt.Unix(),
	}})
	if len(decisions) != 1 || !decisions[0].Notify {
		t.Fatalf("应当触发一条「即将到期」: %+v", decisions)
	}
	body := decisions[0].Notification.Body
	dto := buildNodeDTO(store.Node{ID: 1, Name: "x", ExpiresAt: expiresAt.Unix()},
		state.Node{}, false, now, time.Minute, 2*time.Minute)
	if dto.ExpiresText == "" || !strings.Contains(body, dto.ExpiresText) {
		t.Errorf("面板文案 %q 没有出现在告警正文里：\n%s", dto.ExpiresText, body)
	}
}

// 「已过期」必须是一个**独立的事实**（dto.Expired），界面才能据此把「剩余价值」
// 那一段整段省略。它推不出来：
//   - remaining_days <= 0 不行：还剩 5 小时的机器整天数同样是 0，而它没过期
//     （"今天到期、还剩几小时"与"已经过期"对应完全相反的行动）；
//   - remaining_value_cents == 0 不行：没填价格、没填计费周期的机器同样是 0，
//     那是"没填"，把它和"过期"混成一种，就再也分不出"这台机器没记价格"了。
//
// 所以这一条把三种机器（没过期 / 已过期 / 没填价格）的三个字段逐一对齐，
// 并显式钉住"两张不同的牌面在 remaining_days 上完全一样"。
func TestExpiredFlagSeparatesExpiredFromUnpriced(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	const day = 24 * time.Hour
	priced := func(expiresAt int64) store.Node {
		return store.Node{ID: 1, Name: "x", PriceCents: 3100, BillingMonths: 1, ExpiresAt: expiresAt}
	}

	cases := []struct {
		name string
		node store.Node
		// wantExpired 是这一轮新加的判据：到期时刻已经过去（剩余时间 ≤ 0）。
		wantExpired bool
		// wantSegment 是"这一台机器该不该出现「剩余价值 …」这一段"：
		// 前端的判据是"填了价格 && 没过期"（见 app.js）。
		wantSegment bool
		// wantValue 是"这一段里的金额是不是一个有意义的正数"。
		//
		// 它与 wantSegment **不是一回事**：还剩 5 小时的机器没过期（照旧显示），
		// 但整天数已经归零，所以金额就是 ¥0.00；没填到期日的机器同理。
		// 这两种"显示 ¥0.00"是改动前就有的行为，这一轮只掐掉"已过期"那一种。
		wantValue bool
	}{
		{"没过期（还剩 28 天）", priced(now.Add(28*day + 6*time.Hour).Unix()), false, true, true},
		{"还剩 5 小时（没过期，但整天数是 0）", priced(now.Add(5 * time.Hour).Unix()), false, true, false},
		{"已过期 3 天", priced(now.Add(-3 * day).Unix()), true, false, false},
		// 边界：到期时刻**正好**是现在这一秒 —— 它已经到期了（判据是 secs <= 0，
		// 与告警引擎的 expired := remaining <= 0 同一口径）。
		{"正好到点", priced(now.Unix()), true, false, false},
		{"没填价格（有到期日、也没过期）", store.Node{ID: 1, Name: "x", ExpiresAt: now.Add(28 * day).Unix()}, false, false, false},
		{"没填到期日", store.Node{ID: 1, Name: "x", PriceCents: 3100, BillingMonths: 1}, false, true, false},
	}
	for _, c := range cases {
		dto := buildNodeDTO(c.node, state.Node{}, false, now, time.Minute, 2*time.Minute)
		if dto.Expired != c.wantExpired {
			t.Errorf("%s：expired = %v，期望 %v", c.name, dto.Expired, c.wantExpired)
		}
		if got := dto.PriceCents > 0 && !dto.Expired; got != c.wantSegment {
			t.Errorf("%s：前端的判据（填了价格 && !expired）= %v，期望 %v", c.name, got, c.wantSegment)
		}
		if got := dto.RemainingValueCents > 0; got != c.wantValue {
			t.Errorf("%s：剩余价值 %d 是不是正数 = %v，期望 %v", c.name, dto.RemainingValueCents, got, c.wantValue)
		}
	}

	// 为什么非要有 expired 这个字段：这两台机器在 remaining_days 上**一模一样**，
	// 一个还剩 5 小时（要续费）、一个已经过期 3 天（可能已经被停机）。
	soon := buildNodeDTO(priced(now.Add(5*time.Hour).Unix()), state.Node{}, false, now, time.Minute, 2*time.Minute)
	gone := buildNodeDTO(priced(now.Add(-3*day).Unix()), state.Node{}, false, now, time.Minute, 2*time.Minute)
	if soon.RemainingDays != 0 || gone.RemainingDays != 0 {
		t.Fatalf("前置条件不成立：这两台机器的 remaining_days 应当都是 0，实际 %d / %d",
			soon.RemainingDays, gone.RemainingDays)
	}
	if soon.Expired || !gone.Expired {
		t.Errorf("两台机器的 remaining_days 相同，expired 却必须不同（还剩 5 小时 = %v，已过期 3 天 = %v）",
			soon.Expired, gone.Expired)
	}
}

// newExpiryEngine 构造一个**显式指定时区**的告警引擎。
//
// 为什么要包一层而不是直接 alert.NewEngine(alert.DefaultParams(), …)：默认参数里
// Loc 是 nil，引擎会退回 time.Local —— Windows 的 time.Local.String() 是 "Local"、
// Linux（CI）上是 "UTC"，于是"文案里带时区的那些部分"随机器而变。这里把时区当成
// 一个必须给出来的参数，调用方就没有机会"忘了它"。
func newExpiryEngine(loc *time.Location, started time.Time) *alert.Engine {
	params := alert.DefaultParams()
	params.Loc = loc
	return alert.NewEngine(params, started)
}

// 到期文案必须与"服务端/进程时区"无关：它是按**剩余时长**算出来的，不是按日历日相减。
//
// 为什么值得单钉一条：按日历日算天数是最容易写出来的实现（"10-01 到期、今天 09-30
// → 还有 1 天"），而它给出的答案取决于**看的是哪个时区的日历** —— 同一个到期时刻，
// UTC+8 的机器上可能显示「剩余 1 天」、UTC 的机器上显示「剩余不足 1 天」。
// 本机（Windows，UTC+8）与 CI（Linux，UTC）正好落在日历的两侧，所以这种 bug
// 只在 CI 上露头，本机连跑多少次都是绿的。
//
// 这条用例分两段，因为两段的"对照物"不一样：
//
//	A. 面板那句文案（buildNodeDTO 不收时区参数）必须与**进程时区**无关：
//	   把 time.Local 依次切到四个时区，同一组断言必须逐字成立；
//	B. 告警引擎必须按**注入的 loc** 渲染，而不是进程时区：让二者故意不一致
//	   （进程 = Asia/Tokyo，注入 = 待测时区），正文里的「到期时间：…（时区）」
//	   必须跟着注入的那个走。任务里问的"告警侧与 DTO 侧用的是不是同一个 location"
//	   就是靠这一段钉住的 —— 只看文案抓不到它（那句话由时长算，与 loc 无关）。
func TestExpiresWordingIsTimezoneIndependent(t *testing.T) {
	now := time.Unix(1_700_000_000, 0) // 2023-11-14 22:13:20 UTC
	const day = 24 * time.Hour

	cases := []struct {
		name      string
		remaining time.Duration
		wantText  string
	}{
		{"还剩 28 天", 28 * day, "剩余 28 天"},
		// 这一条最容易分叉：UTC 下"22:13 + 5h30m"已经翻到次日的日历（03:43），
		// 而 UTC+8 / 纽约 / 加尔各答下都还在同一天 —— 按日历日算的实现会在
		// 其中一部分时区给出「剩余 1 天」（反向验证时实测就是这个现象）。
		{"还剩 5 小时 30 分", 5*time.Hour + 30*time.Minute, "剩余不足 1 天（约 5 小时）"},
		{"已过期 3 天多 1 分钟", -(3*day + time.Minute), "已过期 3 天"},
		{"已过期 6 小时", -6 * time.Hour, "已过期不足 1 天（约 6 小时）"},
	}
	// 四个时区各有各的用处：UTC 就是 CI 的环境；Asia/Shanghai 是本机（UTC+8）；
	// America/New_York 是负偏移（日历往另一边翻）；Asia/Kolkata 是半小时偏移
	// （+05:30），专门抓"按整小时算日子 / 拿 24h 硬套本地时间"的实现。
	zones := []string{"UTC", "Asia/Shanghai", "America/New_York", "Asia/Kolkata"}
	locs := make([]*time.Location, 0, len(zones))
	for _, zone := range zones {
		loc, err := time.LoadLocation(zone)
		if err != nil {
			t.Fatalf("加载时区 %s: %v", zone, err)
		}
		locs = append(locs, loc)
	}

	old := time.Local
	defer func() { time.Local = old }()

	// ---- A. 面板文案与进程时区无关 -----------------------------------------
	for i, loc := range locs {
		time.Local = loc
		for _, c := range cases {
			dto := expiryDTO(now, c.remaining)
			if dto.ExpiresText != c.wantText {
				t.Errorf("%s @ 进程时区 %s：expires_text = %q，期望 %q（换个时区也必须是同一句话）",
					c.name, zones[i], dto.ExpiresText, c.wantText)
			}
			if strings.Contains(dto.ExpiresText, "0 天") {
				t.Errorf("%s @ 进程时区 %s：文案里出现了「0 天」：%q", c.name, zones[i], dto.ExpiresText)
			}
		}
	}

	// ---- B. 告警引擎按注入的 loc 渲染，而不是进程时区 ------------------------
	//
	// 进程时区故意取一个**不在待测列表里**的时区：两边一样时，"引擎用的是注入的
	// loc"与"引擎用的是 time.Local"会给出同样的结果，这一段就白测了
	// （alert/wire_test.go 用的是同一招）。
	processLoc, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatalf("加载进程时区: %v", err)
	}
	time.Local = processLoc

	for i, loc := range locs {
		if zones[i] == processLoc.String() {
			t.Fatalf("进程时区与待测时区 %s 相同：这一段失去区分度", zones[i])
		}
		for _, c := range cases {
			expiresAt := now.Add(c.remaining).Unix()
			dto := expiryDTO(now, c.remaining)
			engine := newExpiryEngine(loc, now.Add(-time.Hour))
			decisions := engine.Evaluate(now, []alert.Node{{
				ID: 1, Name: "x", Status: "online", LastSeen: now, ExpiresAt: expiresAt,
			}})
			// 还剩 7 天以上不落进任何提醒档位，本来就不该有决策。
			if c.remaining > 7*day {
				if len(decisions) != 0 {
					t.Errorf("%s @ %s：还剩 7 天以上不该发到期提醒：%+v", c.name, zones[i], decisions)
				}
				continue
			}
			if len(decisions) != 1 || !decisions[0].Notify {
				t.Fatalf("%s @ %s：应当触发一条到期提醒：%+v", c.name, zones[i], decisions)
			}
			body := decisions[0].Notification.Body
			// 与面板逐字一致：Telegram 里那一行必须就是面板上那一句。
			if !strings.Contains(body, dto.ExpiresText) {
				t.Errorf("%s @ 注入 %s（进程 %s）：告警正文里没有面板那句话 %q：\n%s",
					c.name, zones[i], processLoc, dto.ExpiresText, body)
			}
			// 时刻按**注入的**时区渲染，并标注那个时区的名字（而不是进程时区的）。
			wantWhen := "到期时间：" + time.Unix(expiresAt, 0).In(loc).Format("2006-01-02") + "（" + zones[i] + "）"
			if !strings.Contains(body, wantWhen) {
				t.Errorf("%s @ 注入 %s（进程 %s）：告警正文里应当是 %q：\n%s",
					c.name, zones[i], processLoc, wantWhen, body)
			}
		}
	}
}

// expiryDTO 按"还剩多久到期"造一台机器并取它的 DTO（到期文案与剩余天数都在里面）。
func expiryDTO(now time.Time, remaining time.Duration) nodeDTO {
	return buildNodeDTO(
		store.Node{ID: 1, Name: "x", ExpiresAt: now.Add(remaining).Unix()},
		state.Node{}, false, now, time.Minute, 2*time.Minute)
}

// TestAbsurdExpiresAtDoesNotWrap 守住「int64 秒 → time.Duration 回绕」与
// 「剩余价值越界」两条：expires_at 是能直接写进库的 int64 秒，store 层没有上限，
// 而 time.Duration(secs) * time.Second 在 |secs| > 292 年时会回绕 —— 于是同一条
// DTO 里 expired=false、remaining_days 是个大正数，expires_text 却写着「已过期 N 天」，
// 同一张卡片上两句话互相打架；剩余价值那条乘法还会越界成 minInt64，
// 一路污染总览里的「剩余价值合计」。
func TestAbsurdExpiresAtDoesNotWrap(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	const day = 24 * time.Hour
	// 价格取存储层的上限（一千亿元）：剩余价值那条路只有在乘数离谱时才会溢出。
	const maxPriceCents = 100_000_000_000
	priced := func(expiresAt int64) store.Node {
		return store.Node{ID: 1, Name: "x", PriceCents: maxPriceCents, BillingMonths: 1, ExpiresAt: expiresAt}
	}

	// ① 正常范围（200 年，仍在 Duration 能表达的 292 年内）：夹取必须逐字节无影响。
	inRange := now.Add(200 * 365 * day)
	dto := buildNodeDTO(priced(inRange.Unix()), state.Node{}, false, now, time.Minute, 2*time.Minute)
	secs := inRange.Unix() - now.Unix()
	if dto.RemainingDays != secs/86400 {
		t.Errorf("正常范围：remaining_days = %d，期望 %d", dto.RemainingDays, secs/86400)
	}
	if want := alert.ExpiryPhrase(time.Duration(secs) * time.Second); dto.ExpiresText != want {
		t.Errorf("正常范围：expires_text = %q，期望 %q", dto.ExpiresText, want)
	}

	// ② 超出 292 年的到期值：三个字段必须自洽，且金额不能变成负数。
	for _, c := range []struct {
		name      string
		expiresAt int64
	}{
		{"MaxInt64", math.MaxInt64},
		{"300 年后", now.AddDate(300, 0, 0).Unix()},
	} {
		dto := buildNodeDTO(priced(c.expiresAt), state.Node{}, false, now, time.Minute, 2*time.Minute)
		if dto.Expired {
			t.Errorf("%s：远期到期不该是「已过期」", c.name)
		}
		if strings.Contains(dto.ExpiresText, "已过期") {
			t.Errorf("%s：expired=false，但 expires_text 说已过期：%q", c.name, dto.ExpiresText)
		}
		// remaining_days 与 expires_text 仍然来自同一个整数秒（面板上两处不会打架）。
		if want := fmt.Sprintf("剩余 %d 天", dto.RemainingDays); dto.ExpiresText != want {
			t.Errorf("%s：expires_text = %q，期望 %q", c.name, dto.ExpiresText, want)
		}
		if dto.RemainingValueCents <= 0 {
			t.Errorf("%s：剩余价值越界成 %d（minInt64 的形态）", c.name, dto.RemainingValueCents)
		}
	}

	// ③ 负的 expires_at 仍然整段不生效（这是 `ExpiresAt > 0` 的既有语义）。
	none := buildNodeDTO(priced(math.MinInt64), state.Node{}, false, now, time.Minute, 2*time.Minute)
	if none.Expired || none.RemainingDays != 0 || none.ExpiresText != "" || none.RemainingValueCents != 0 {
		t.Errorf("负到期值应当整段不生效：%+v", none)
	}
}
