package server

import (
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
	expiresAt := now.Add(5 * time.Hour)
	engine := alert.NewEngine(alert.DefaultParams(), now.Add(-time.Hour))
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
