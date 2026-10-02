package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	// 内嵌时区数据库：Windows 上没有系统 tzdata，而这一组用例的区间边界
	// （切天/切周/切月）必须在一个**具体**时区下验证，不能靠机器的本地时区。
	_ "time/tzdata"

	"probe/internal/alert"
	"probe/internal/config"
	"probe/internal/store"
)

// trafficNotifyTestZone 是这组用例用的服务端时区（--timezone）。
//
// 特意不用 UTC：跨月/跨年/周边界在 UTC 下也能过，但那样测不出"边界是按
// 配置时区切的"这件事 —— 换成东八区之后，UTC 下属于同一天的 16:00 之后
// 就属于第二天了。
const trafficNotifyTestZone = "Asia/Shanghai"

func trafficNotifyTestLoc(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(trafficNotifyTestZone)
	if err != nil {
		t.Fatalf("加载时区 %s: %v", trafficNotifyTestZone, err)
	}
	return loc
}

// atLocal 把"服务端时区下的某个时刻"写成 time.Time。
func atLocal(t *testing.T, loc *time.Location, value string) time.Time {
	t.Helper()
	parsed, err := time.ParseInLocation("2006-01-02 15:04", value, loc)
	if err != nil {
		t.Fatalf("解析时间 %q: %v", value, err)
	}
	return parsed
}

// allTrafficReports 是"三个开关全开"的状态。
func allTrafficReports() trafficNotifyState {
	return trafficNotifyState{
		On: trafficNotifySwitches{Daily: true, Weekly: true, Monthly: true}.asMap(),
		Last: map[string]string{
			trafficReportDaily:   "",
			trafficReportWeekly:  "",
			trafficReportMonthly: "",
		},
	}
}

// planNames 把判定结果压成"报告名"列表，供顺序敏感的比较使用。
func planNames(plans []trafficReportPlan) []string {
	out := make([]string, 0, len(plans))
	for _, p := range plans {
		out = append(out, p.Spec.Name)
	}
	return out
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestTrafficReportTriggerTimes 钉住三个触发时刻：日报每天、周报只在周一、
// 月报只在 1 号，而且都要过了当天 09:00。
//
// 星期几全部按**服务端时区**算 —— 这也是为什么这些用例用一个具体时区
// （Asia/Shanghai）而不是 UTC。
func TestTrafficReportTriggerTimes(t *testing.T) {
	loc := trafficNotifyTestLoc(t)

	cases := []struct {
		name string
		now  string // 服务端时区的本地时刻
		want []string
	}{
		// 2026-10-07 是周三。
		{"周三 08:59 还没到点", "2026-10-07 08:59", nil},
		{"周三 09:00 正点发日报", "2026-10-07 09:00", []string{trafficReportDaily}},
		{"周三 23:59 仍然只发日报", "2026-10-07 23:59", []string{trafficReportDaily}},
		// 2026-10-12 是周一。
		{"周一 08:59 不发", "2026-10-12 08:59", nil},
		{"周一 09:00 日报 + 周报", "2026-10-12 09:00", []string{trafficReportDaily, trafficReportWeekly}},
		{"周二 09:00 只有日报（周报只在周一）", "2026-10-13 09:00", []string{trafficReportDaily}},
		// 2026-11-01 是周日，但它是 1 号。
		{"1 号 09:00 日报 + 月报", "2026-11-01 09:00", []string{trafficReportDaily, trafficReportMonthly}},
		{"3 号 09:00 只有日报（月报只在 1 号）", "2026-11-03 09:00", []string{trafficReportDaily}},
		// 2026-06-01 既是周一又是 1 号：三个都该发。
		{"周一且 1 号 08:59 一份都不发", "2026-06-01 08:59", nil},
		{"周一且 1 号 09:00 三份都发", "2026-06-01 09:00",
			[]string{trafficReportDaily, trafficReportWeekly, trafficReportMonthly}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := planNames(planTrafficReports(atLocal(t, loc, c.now), loc, trafficNotifyHour, allTrafficReports()))
			if !sameStrings(got, c.want) {
				t.Fatalf("%s 应当发 %v，实际 %v", c.now, c.want, got)
			}
		})
	}

	// 触发时刻本身：不是"零点 + 9 小时"这种近似，而是本地 09:00 整。
	day := store.DayStart(atLocal(t, loc, "2026-10-07 15:00"), loc)
	trigger := trafficNotifyTrigger(day, loc, trafficNotifyHour)
	if got := trigger.Format("2006-01-02 15:04:05 -0700"); got != "2026-10-07 09:00:00 +0800" {
		t.Fatalf("触发时刻 = %q，期望 2026-10-07 09:00:00 +0800", got)
	}
}

// TestTrafficReportWindows 钉住三种报告统计的区间：上一个**完整**周期。
//
// 覆盖跨月、跨年、周一边界与月末最后一天 —— 这些正是"自己算日期"最容易差
// 一天的地方（差一天的表现是报告里的数字与面板对不上，而没人看得出来）。
func TestTrafficReportWindows(t *testing.T) {
	loc := trafficNotifyTestLoc(t)
	specs := map[string]trafficReportSpec{}
	for _, spec := range trafficReportSpecs {
		specs[spec.Name] = spec
	}

	cases := []struct {
		name       string
		report     string
		now        string
		wantStart  string
		wantEnd    string
		wantPeriod string
	}{
		{"日报：普通一天", trafficReportDaily, "2026-10-07 09:00", "2026-10-06 00:00", "2026-10-07 00:00", "昨天 10-06"},
		{"日报：跨月", trafficReportDaily, "2026-03-01 09:00", "2026-02-28 00:00", "2026-03-01 00:00", "昨天 02-28"},
		{"日报：跨年", trafficReportDaily, "2027-01-01 09:00", "2026-12-31 00:00", "2027-01-01 00:00", "昨天 12-31"},
		{"周报：周一当天的上一周", trafficReportWeekly, "2026-10-12 09:00", "2026-10-05 00:00", "2026-10-12 00:00", "上周 10-05 ~ 10-11"},
		{"周报：跨月的一周", trafficReportWeekly, "2026-11-02 09:00", "2026-10-26 00:00", "2026-11-02 00:00", "上周 10-26 ~ 11-01"},
		{"周报：跨年的一周", trafficReportWeekly, "2027-01-04 09:00", "2026-12-28 00:00", "2027-01-04 00:00", "上周 12-28 ~ 01-03"},
		{"月报：31 天的上月", trafficReportMonthly, "2026-11-01 09:00", "2026-10-01 00:00", "2026-11-01 00:00", "上月 10-01 ~ 10-31"},
		{"月报：30 天的上月（月末最后一天）", trafficReportMonthly, "2026-05-01 09:00", "2026-04-01 00:00", "2026-05-01 00:00", "上月 04-01 ~ 04-30"},
		{"月报：平年 2 月", trafficReportMonthly, "2026-03-01 09:00", "2026-02-01 00:00", "2026-03-01 00:00", "上月 02-01 ~ 02-28"},
		{"月报：跨年", trafficReportMonthly, "2026-01-01 09:00", "2025-12-01 00:00", "2026-01-01 00:00", "上月 12-01 ~ 12-31"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			spec := specs[c.report]
			local := atLocal(t, loc, c.now)
			start, end := spec.Window(local, loc)
			if got := start.Format("2006-01-02 15:04"); got != c.wantStart {
				t.Errorf("%s 的区间起点 = %q，期望 %q", c.report, got, c.wantStart)
			}
			if got := end.Format("2006-01-02 15:04"); got != c.wantEnd {
				t.Errorf("%s 的区间终点 = %q，期望 %q", c.report, got, c.wantEnd)
			}
			if got := spec.Period(start, end); got != c.wantPeriod {
				t.Errorf("%s 的区间写法 = %q，期望 %q", c.report, got, c.wantPeriod)
			}
			// 区间一定是 [start, end) 且 start < end：报告算的是"已经结束的那一段"。
			if !start.Before(end) {
				t.Errorf("%s 的区间是空的：%s → %s", c.report, start, end)
			}
		})
	}

	// 区间恰好覆盖上一个自然周/自然月：把区间里的每一天枚举出来，天数必须对。
	t.Run("周报区间恰好 7 天", func(t *testing.T) {
		local := atLocal(t, loc, "2027-01-04 09:00")
		start, end := specs[trafficReportWeekly].Window(local, loc)
		if days := int(end.Sub(start).Hours() / 24); days != 7 {
			t.Fatalf("周报区间跨了 %d 天，期望 7 天（%s → %s）", days, start, end)
		}
	})
	t.Run("月报区间恰好覆盖上月每一天", func(t *testing.T) {
		local := atLocal(t, loc, "2026-11-01 09:00")
		start, end := specs[trafficReportMonthly].Window(local, loc)
		days := 0
		for d := start; d.Before(end); d = d.AddDate(0, 0, 1) {
			days++
		}
		if days != 31 {
			t.Fatalf("月报区间跨了 %d 天，期望 31 天（%s → %s）", days, start, end)
		}
	})
}

// TestTrafficReportCatchUpProtection 钉住补发保护：一天只发一次、
// 当天迟到会补、跨天不补。
//
// 这三条是同一套规则的三个面，缺一条都会出问题：
//   - 少了"一天只发一次" → 每分钟的检查会把它变成刷屏；
//   - 少了"当天迟到要补" → 09:00 那一刻重启/休眠就永远收不到那一天的报告；
//   - 少了"跨天不补" → 停机一周回来会一口气发七份日报。
func TestTrafficReportCatchUpProtection(t *testing.T) {
	loc := trafficNotifyTestLoc(t)

	t.Run("同一天重复检查只发一次", func(t *testing.T) {
		state := allTrafficReports()
		first := planTrafficReports(atLocal(t, loc, "2026-10-07 09:00"), loc, trafficNotifyHour, state)
		if len(first) != 1 {
			t.Fatalf("09:00 应当发 1 份，实际 %d 份", len(first))
		}
		// 模拟投递完成：写回哨兵（这就是 checkTrafficReportsAt 干的事）。
		state.Last[trafficReportDaily] = first[0].Day
		for _, at := range []string{"2026-10-07 09:00", "2026-10-07 09:01", "2026-10-07 14:00", "2026-10-07 23:59"} {
			if got := planTrafficReports(atLocal(t, loc, at), loc, trafficNotifyHour, state); len(got) != 0 {
				t.Fatalf("%s 又判定要发 %v：同一天重复发送", at, planNames(got))
			}
		}
		// 第二天 09:00 恢复（哨兵是"哪一天发过"，不是"发过一次就再也不发"）。
		if got := planTrafficReports(atLocal(t, loc, "2026-10-08 09:00"), loc, trafficNotifyHour, state); len(got) != 1 {
			t.Fatalf("第二天应当恢复发送，实际 %v", planNames(got))
		}
	})

	t.Run("当天错过 09:00 会在当天补一次", func(t *testing.T) {
		// 服务端 09:00 那会儿不在线（重启 / 机器休眠），10:23 才醒过来。
		state := allTrafficReports()
		state.Last[trafficReportDaily] = "2026-10-06" // 前一天发过
		got := planTrafficReports(atLocal(t, loc, "2026-10-07 10:23"), loc, trafficNotifyHour, state)
		if len(got) != 1 || got[0].Day != "2026-10-07" {
			t.Fatalf("当天迟到应当补发（哨兵写今天），实际 %v", planNames(got))
		}
		if start := got[0].Start.Format("01-02"); start != "10-06" {
			t.Fatalf("补发的日报统计区间 = %s，期望 10-06（仍然是「昨天」）", start)
		}
	})

	t.Run("错过一整天之后不补发", func(t *testing.T) {
		// 10-07 一整天没开机，10-08 才起来：只发 10-08 这一期，
		// 10-07 那一期就跳过了 —— 停机一周也不会攒出一串报告。
		state := allTrafficReports()
		state.Last[trafficReportDaily] = "2026-10-06"
		got := planTrafficReports(atLocal(t, loc, "2026-10-08 10:00"), loc, trafficNotifyHour, state)
		if len(got) != 1 || got[0].Day != "2026-10-08" {
			t.Fatalf("只应当发 10-08 这一期，实际 %v", planNames(got))
		}
	})

	t.Run("周报和月报同样只在当天补", func(t *testing.T) {
		state := allTrafficReports()
		state.Last[trafficReportWeekly] = "2026-10-05"
		state.Last[trafficReportMonthly] = "2026-10-01"
		// 10-07 是周三：上周一那一期没发出去，周三不补。
		got := planTrafficReports(atLocal(t, loc, "2026-10-07 09:30"), loc, trafficNotifyHour, state)
		if !sameStrings(planNames(got), []string{trafficReportDaily}) {
			t.Fatalf("周三只应当发日报，实际 %v", planNames(got))
		}
		// 10-01 那一期月报同理：11-03（周二，既不是 1 号也不是周一）不补，
		// 下一次要等下个月 1 号。
		if got := planTrafficReports(atLocal(t, loc, "2026-11-03 09:30"), loc, trafficNotifyHour, state); len(got) != 1 || got[0].Spec.Name != trafficReportDaily {
			t.Fatalf("11-03 既不是 1 号也不是周一，只应当发日报，实际 %v", planNames(got))
		}
	})

	t.Run("一天之内检查多少次都只判定一次", func(t *testing.T) {
		state := allTrafficReports()
		day := atLocal(t, loc, "2026-10-07 09:00")
		triggered := 0
		for i := 0; i < 1440; i++ {
			at := day.Add(time.Duration(i) * time.Minute)
			plans := planTrafficReports(at, loc, trafficNotifyHour, state)
			if len(plans) == 1 {
				triggered++
				state.Last[trafficReportDaily] = plans[0].Day // 投递成功
			}
		}
		if triggered != 1 {
			t.Fatalf("一天 1440 次检查里触发了 %d 次，期望 1 次", triggered)
		}
	})
}

// TestTrafficReportMessageContent 用造好的数据验证消息内容：
// 每台机器都有一行（含整个周期零流量的那台）、合计正确、节点顺序按首页顺序。
func TestTrafficReportMessageContent(t *testing.T) {
	loc := trafficNotifyTestLoc(t)
	srv, recorder := newTrafficNotifyHarness(t, loc)

	// 三台机器：两台有量、一台整个周期一行都没有（从未上报）。
	for _, name := range []string{"hk-01", "us-01", "zero-01"} {
		if _, _, err := srv.db.CreateNode(context.Background(), store.NewNode{
			Name: name, IntervalSec: 1, TrafficWarnPct: 80, ResetDay: 1,
		}, time.Now()); err != nil {
			t.Fatalf("创建节点 %s: %v", name, err)
		}
	}
	nodes, err := srv.db.ListNodes(context.Background())
	if err != nil {
		t.Fatalf("读取节点列表: %v", err)
	}
	if len(nodes) != 3 {
		t.Fatalf("节点数 = %d，期望 3", len(nodes))
	}

	// 10-06 这一天的量（= 10-07 早上报告的"昨天"）。
	seedDailyTraffic(t, srv, nodes[0].ID, "2026-10-06", 8_120_000_000, 1_230_000_000)
	seedDailyTraffic(t, srv, nodes[1].ID, "2026-10-06", 3_010_000_000, 512_000_000)
	// 区间外的量：今天与三天前都不该进这一期的合计。
	seedDailyTraffic(t, srv, nodes[0].ID, "2026-10-07", 99_000_000_000, 99_000_000_000)
	seedDailyTraffic(t, srv, nodes[0].ID, "2026-10-03", 77_000_000_000, 77_000_000_000)

	turnOnTrafficReports(t, srv, trafficNotifySwitches{Daily: true})
	srv.checkTrafficReportsAt(context.Background(), atLocal(t, loc, "2026-10-07 09:00"))

	note := recorder.wait(t, alert.RuleTrafficReport, 5*time.Second)
	body := note.Body
	t.Logf("日报正文：\n%s", body)
	// 通知器（Telegram）真正发出去的文本 = 它拿到的那条通知再渲染一次。
	if wire := alert.RenderBatch([]alert.Notification{note}); wire != body {
		t.Fatalf("实际发出去的文本与正文不一致：\n%s", wire)
	}

	// 每台机器都有一行 —— 零流量那台也必须在，而且写 0 而不是 —：
	// 这一期的数据来源是已经定稿的 traffic_daily，"0" 是**确定的事实**
	// （这台机器这段时间一点流量都没有），写 —（未知）反而是错的。
	for _, want := range []struct {
		name string
		line []string
	}{
		{"hk-01", []string{"↑ 1.23 GB", "↓ 8.12 GB", "计 9.35 GB"}},
		{"us-01", []string{"↑ 512.0 MB", "↓ 3.01 GB", "计 3.52 GB"}},
		{"zero-01", []string{"↑ 0 B", "↓ 0 B", "计 0 B"}},
	} {
		line := reportLineFor(t, body, want.name)
		for _, needle := range want.line {
			if !strings.Contains(line, needle) {
				t.Errorf("%s 那一行缺少 %q：%q", want.name, needle, line)
			}
		}
	}

	// 合计 = 三台机器之和（区间外的量不许混进来）。
	sum := reportLineFor(t, body, "合计")
	for _, needle := range []string{"↑ 1.74 GB", "↓ 11.1 GB", "计 12.9 GB"} {
		if !strings.Contains(sum, needle) {
			t.Errorf("合计行缺少 %q：%q", needle, sum)
		}
	}
	if strings.Contains(body, "99.0 GB") || strings.Contains(body, "77.0 GB") {
		t.Error("区间外的流量混进了报告：窗口过滤没生效")
	}

	// 顺序 = 首页顺序（ListNodes 的顺序）。
	order := make([]int, 0, 3)
	for _, n := range nodes {
		order = append(order, strings.Index(body, n.Name))
	}
	if !(order[0] < order[1] && order[1] < order[2]) {
		t.Errorf("报告里的顺序与首页顺序不一致：%v（节点列表 %v）", order, order)
	}
	// 首行 = 报告名 + 统计区间，而且**只有这一行**标题：
	// Title 必须留空（见 trafficReportNotifications）—— 非空时通知器会在前面
	// 再补一行按 severity 上色的「🟢 标题」，消息开头就是两行标题。
	first := firstLine(body)
	if !strings.HasPrefix(first, "📊 ") || !strings.Contains(first, "流量日报") || !strings.Contains(first, "昨天 10-06") {
		t.Errorf("首行 = %q，期望「📊 流量日报（昨天 10-06）」", first)
	}
	if note.Title != "" {
		t.Errorf("Title = %q，必须留空：非空时通知器会在前面补一行告警配色的标题", note.Title)
	}
	if n := strings.Count(body, "流量日报"); n != 1 {
		t.Errorf("报告名在正文里出现了 %d 次，期望 1 次（消息开头出现两行标题就是这里没对齐）", n)
	}
	// 通知走的是告警分发器，级别是 info（它不是告警，是定期汇报）。
	if note.Severity != alert.SeverityInfo {
		t.Errorf("报告的级别 = %s，期望 info", note.Severity)
	}
}

// TestTrafficReportSkipsEmptyNodeList 验证"没有节点时不发空报告"。
func TestTrafficReportSkipsEmptyNodeList(t *testing.T) {
	loc := trafficNotifyTestLoc(t)
	srv, recorder := newTrafficNotifyHarness(t, loc)
	turnOnTrafficReports(t, srv, trafficNotifySwitches{Daily: true, Weekly: true, Monthly: true})

	// 2026-06-01 是周一又是 1 号：正常情况下三个都会发。
	srv.checkTrafficReportsAt(context.Background(), atLocal(t, loc, "2026-06-01 09:00"))
	time.Sleep(200 * time.Millisecond)
	if n := recorder.count(); n != 0 {
		t.Fatalf("没有节点时不应当发报告，实际收到 %d 条", n)
	}
	// 哨兵也不该被写：当天稍后加了机器还能补上这一期。
	for _, spec := range trafficReportSpecs {
		if value, ok, err := srv.db.GetSetting(context.Background(), spec.LastKey); err != nil || (ok && value != "") {
			t.Fatalf("没有节点时不该记投递日期：%s = %q (ok=%v err=%v)", spec.LastKey, value, ok, err)
		}
	}
}

// TestTrafficReportRespectsSwitches 验证"开关关掉时不发"。
func TestTrafficReportRespectsSwitches(t *testing.T) {
	loc := trafficNotifyTestLoc(t)
	ctx := context.Background()

	cases := []struct {
		name     string
		switches trafficNotifySwitches
		want     int
	}{
		{"三个都关：什么都不发", trafficNotifySwitches{}, 0},
		{"只开周报而今天是周三：不发", trafficNotifySwitches{Weekly: true}, 0},
		{"只开月报而今天是周三：不发", trafficNotifySwitches{Monthly: true}, 0},
		{"只开日报：发一份", trafficNotifySwitches{Daily: true}, 1},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, recorder := newTrafficNotifyHarness(t, loc)
			node, _, err := srv.db.CreateNode(ctx, store.NewNode{
				Name: "sw-01", IntervalSec: 1, TrafficWarnPct: 80, ResetDay: 1,
			}, time.Now())
			if err != nil {
				t.Fatalf("创建节点: %v", err)
			}
			seedDailyTraffic(t, srv, node.ID, "2026-10-06", 1_000_000_000, 1_000_000_000)
			turnOnTrafficReports(t, srv, c.switches)

			srv.checkTrafficReportsAt(ctx, atLocal(t, loc, "2026-10-07 09:00"))
			time.Sleep(200 * time.Millisecond)
			if n := recorder.count(); n != c.want {
				t.Fatalf("收到 %d 条通知，期望 %d 条", n, c.want)
			}
		})
	}
}

// TestTrafficReportSentOnceAndSurvivesRestart 走的是数据库这一层：
// 同一天重复检查、乃至**换一个 Server 实例**（等价于重启）都只发一次。
//
// 为什么必须落库而不是只放在内存里：这台机器半夜重启是常态，
// 而"重启后把当天的报告再发一遍"正是用户明确不要的重复发送。
func TestTrafficReportSentOnceAndSurvivesRestart(t *testing.T) {
	loc := trafficNotifyTestLoc(t)
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "probe.db")

	srv, recorder := newTrafficNotifyHarnessAt(t, loc, dbPath, nil)
	node, _, err := srv.db.CreateNode(ctx, store.NewNode{
		Name: "once-01", IntervalSec: 1, TrafficWarnPct: 80, ResetDay: 1,
	}, time.Now())
	if err != nil {
		t.Fatalf("创建节点: %v", err)
	}
	seedDailyTraffic(t, srv, node.ID, "2026-10-06", 2_000_000_000, 1_000_000_000)
	turnOnTrafficReports(t, srv, trafficNotifySwitches{Daily: true})

	at := atLocal(t, loc, "2026-10-07 09:00")
	srv.checkTrafficReportsAt(ctx, at)
	recorder.wait(t, alert.RuleTrafficReport, 5*time.Second)

	// 同一个进程里再检查三次（模拟每分钟的 ticker）。
	for i := 0; i < 3; i++ {
		srv.checkTrafficReportsAt(ctx, at.Add(time.Duration(i+1)*time.Minute))
	}
	time.Sleep(150 * time.Millisecond)
	if n := recorder.count(); n != 1 {
		t.Fatalf("同一天重复检查发出了 %d 条，期望 1 条", n)
	}

	// 换一个 Server 实例（同一个库）＝ 服务端重启。
	srv2, recorder2 := newTrafficNotifyHarnessAt(t, loc, dbPath, srv.db)
	srv2.checkTrafficReportsAt(ctx, at.Add(30*time.Minute))
	time.Sleep(150 * time.Millisecond)
	if n := recorder2.count(); n != 0 {
		t.Fatalf("重启后重发了 %d 条报告：投递日期没有落库", n)
	}
	// 第二天照常发（哨兵只挡住"同一天"）。
	srv2.checkTrafficReportsAt(ctx, atLocal(t, loc, "2026-10-08 09:00"))
	if note := recorder2.wait(t, alert.RuleTrafficReport, 5*time.Second); !strings.Contains(firstLine(note.Body), "昨天 10-07") {
		t.Fatalf("第二天的报告首行 = %q，期望说的是 10-07", firstLine(note.Body))
	}
}

// utf16Units 数一条消息在 Telegram 眼里的长度：UTF-16 单元数（星平面字符占 2）。
//
// 断言必须与 alert.ChunkLines 的计账单位一致：按 rune 数的话，"emoji 密集时整条
// 超限被拒收"这件事根本测不出来（那正是这条上限要防的故障）。
func utf16Units(s string) int {
	n := 0
	for _, r := range s {
		if r > 0xFFFF {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// TestTrafficReportChunksLongNodeLists 验证节点很多时按行分片而不是截断。
//
// 截断会把排在后面的机器整台整台地漏掉 —— 而"每台机器都有一行"正是这个报告的
// 全部意义；分片则要求每一片都能单独发出去（独占一条消息，见 NoCoalesce）。
func TestTrafficReportChunksLongNodeLists(t *testing.T) {
	loc := trafficNotifyTestLoc(t)
	const nodeCount = 120

	rows := make([]trafficReportRow, 0, nodeCount)
	for i := 0; i < nodeCount; i++ {
		rows = append(rows, trafficReportRow{
			Name: fmt.Sprintf("node-%03d", i),
			Tx:   int64(i+1) * 1_000_000,
			Rx:   int64(i+1) * 2_000_000,
		})
	}
	plan := trafficReportPlan{
		Spec:  trafficReportSpecs[0],
		Day:   "2026-10-07",
		Start: atLocal(t, loc, "2026-10-06 00:00"),
		End:   atLocal(t, loc, "2026-10-07 00:00"),
	}
	notes := trafficReportNotifications(plan, rows, atLocal(t, loc, "2026-10-07 09:00"), loc)
	if len(notes) < 2 {
		t.Fatalf("%d 台机器只生成了 %d 条消息：一条装不下时应当分片", nodeCount, len(notes))
	}

	joined := make([]string, 0, len(notes))
	for i, note := range notes {
		joined = append(joined, note.Body)
		if got := utf16Units(note.Body); got > trafficReportMaxUnits {
			t.Errorf("第 %d 片有 %d 个 UTF-16 单元，超过上限 %d", i+1, got, trafficReportMaxUnits)
		}
		if !note.NoCoalesce {
			t.Errorf("第 %d 片没有标记为独占消息：合并窗口会把它和别的通知拼成一条，正好超长", i+1)
		}
		// 序号在**首行**上（Title 留空，见 trafficReportNotifications）。
		want := fmt.Sprintf("%d/%d", i+1, len(notes))
		if first := firstLine(note.Body); !strings.Contains(first, want) {
			t.Errorf("第 %d 片的标题行 = %q，缺少序号 %s（收件人看不出还有没有下文）", i+1, first, want)
		}
	}
	all := strings.Join(joined, "\n")
	for i := 0; i < nodeCount; i++ {
		name := fmt.Sprintf("node-%03d", i)
		if n := strings.Count(all, name); n != 1 {
			t.Fatalf("%s 在报告里出现了 %d 次，期望 1 次（分片漏掉或重复了机器）", name, n)
		}
	}
	// 合计只出现一次，而且落在最后一片。
	if n := strings.Count(all, "合计"); n != 1 {
		t.Errorf("「合计」出现了 %d 次，期望 1 次", n)
	}
	if !strings.Contains(notes[len(notes)-1].Body, "合计") {
		t.Error("合计应当在最后一片里")
	}
	// 统计区间只在第一片：每一片都重复一遍头部是浪费，而它又只对整份报告有意义。
	if !strings.Contains(notes[0].Body, "统计区间") {
		t.Error("第一片里应当写明统计区间")
	}
	for i := 1; i < len(notes); i++ {
		if strings.Contains(notes[i].Body, "统计区间") {
			t.Errorf("第 %d 片里重复了统计区间", i+1)
		}
	}
}

// TestTrafficNotifySeedSkipsPassedTrigger 验证"关→开"那一刻的取舍：
// 已经过了 09:00 就跳过今天这一期（否则用户一勾开关就会立刻收到一条旧报告），
// 还没到 09:00 则今天照常发。
func TestTrafficNotifySeedSkipsPassedTrigger(t *testing.T) {
	loc := trafficNotifyTestLoc(t)
	ctx := context.Background()

	t.Run("下午打开开关：跳过今天", func(t *testing.T) {
		srv, _ := newTrafficNotifyHarness(t, loc)
		on := trafficNotifySwitches{Daily: true, Weekly: true, Monthly: true}
		// 与真实路径一致：先把开关存下来，再写"今天这一期已经处理过"的哨兵。
		turnOnTrafficReports(t, srv, on)
		if err := srv.seedTrafficNotifyLatches(ctx, atLocal(t, loc, "2026-10-07 14:00"), trafficNotifySwitches{}, on); err != nil {
			t.Fatalf("写哨兵: %v", err)
		}
		state, err := srv.loadTrafficNotifyState(ctx)
		if err != nil {
			t.Fatalf("读取状态: %v", err)
		}
		for _, spec := range trafficReportSpecs {
			if state.Last[spec.Name] != "2026-10-07" {
				t.Errorf("%s 的投递日期 = %q，期望 2026-10-07（今天已经过去的那一期算跳过）",
					spec.Name, state.Last[spec.Name])
			}
		}
		// 于是当天 15:00 不再触发，第二天 09:00 恢复。
		if got := planTrafficReports(atLocal(t, loc, "2026-10-07 15:00"), loc, trafficNotifyHour, state); len(got) != 0 {
			t.Errorf("下午点开开关后仍然立刻发了 %v", planNames(got))
		}
		if got := planTrafficReports(atLocal(t, loc, "2026-10-08 09:00"), loc, trafficNotifyHour, state); len(got) != 1 {
			t.Errorf("第二天应当恢复发送，实际 %v", planNames(got))
		}
	})

	t.Run("早上八点打开开关：当天九点照发", func(t *testing.T) {
		srv, _ := newTrafficNotifyHarness(t, loc)
		on := trafficNotifySwitches{Daily: true}
		turnOnTrafficReports(t, srv, on)
		if err := srv.seedTrafficNotifyLatches(ctx, atLocal(t, loc, "2026-10-07 08:00"), trafficNotifySwitches{}, on); err != nil {
			t.Fatalf("写哨兵: %v", err)
		}
		state, err := srv.loadTrafficNotifyState(ctx)
		if err != nil {
			t.Fatalf("读取状态: %v", err)
		}
		if state.Last[trafficReportDaily] != "" {
			t.Fatalf("还没到 09:00 就不该记投递日期，实际 %q", state.Last[trafficReportDaily])
		}
		if got := planTrafficReports(atLocal(t, loc, "2026-10-07 09:00"), loc, trafficNotifyHour, state); len(got) != 1 {
			t.Fatalf("当天 09:00 应当照发，实际 %v", planNames(got))
		}
	})

	t.Run("本来就是开着的开关不受影响", func(t *testing.T) {
		srv, _ := newTrafficNotifyHarness(t, loc)
		// 每天都不该被"保存设置"顺手重置成"今天发过了"。
		if err := srv.saveTrafficNotifySwitches(ctx, trafficNotifySwitches{Daily: true}); err != nil {
			t.Fatalf("保存开关: %v", err)
		}
		if err := srv.seedTrafficNotifyLatches(ctx, atLocal(t, loc, "2026-10-07 14:00"),
			trafficNotifySwitches{Daily: true}, trafficNotifySwitches{Daily: true}); err != nil {
			t.Fatalf("写哨兵: %v", err)
		}
		state, err := srv.loadTrafficNotifyState(ctx)
		if err != nil {
			t.Fatalf("读取状态: %v", err)
		}
		if state.Last[trafficReportDaily] != "" {
			t.Fatalf("一直开着的开关不该被写哨兵，实际 %q", state.Last[trafficReportDaily])
		}
	})
}

// TestTrafficReportKeepsOneLinePerNode 钉住"每行一台机器"这条不变量。
//
// 节点名没有字符集限制（接口与库里都能塞进换行），而换行会把一行拆成两行 ——
// 后半截看上去就像另一台机器，报告的对账价值当场作废。
func TestTrafficReportKeepsOneLinePerNode(t *testing.T) {
	loc := trafficNotifyTestLoc(t)
	nodes := []store.Node{
		{ID: 1, Name: "hk-01"},
		{ID: 2, Name: "bad\nname"},
		{ID: 3, Name: "tab\tname"},
		{ID: 4, Name: "\n"},
	}
	start := atLocal(t, loc, "2026-10-06 00:00")
	end := atLocal(t, loc, "2026-10-07 00:00")
	rows := trafficReportRows(nodes, nil, start, end)
	if len(rows) != len(nodes) {
		t.Fatalf("行数 = %d，期望 %d（每台机器都必须有自己的一行）", len(rows), len(nodes))
	}
	for _, r := range rows {
		if strings.ContainsAny(r.Name, "\n\r\t") {
			t.Errorf("名字 %q 里还留着控制字符：它会把一行拆成两行", r.Name)
		}
		if strings.TrimSpace(r.Name) == "" {
			t.Errorf("第 %d 行的名字是空的：报告里会出现一行看不出是谁的记录", len(rows))
		}
	}
	plan := trafficReportPlan{Spec: trafficReportSpecs[0], Day: "2026-10-07", Start: start, End: end}
	notes := trafficReportNotifications(plan, rows, atLocal(t, loc, "2026-10-07 09:00"), loc)
	if len(notes) != 1 {
		t.Fatalf("消息条数 = %d，期望 1", len(notes))
	}
	// 行数 = 标题行 + 统计区间 + 每台机器一行 + 分隔线 + 合计。
	lines := strings.Split(notes[0].Body, "\n")
	if want := len(nodes) + 4; len(lines) != want {
		t.Fatalf("正文有 %d 行，期望 %d 行（每台机器一行）：\n%s", len(lines), want, notes[0].Body)
	}
	if !strings.HasPrefix(lines[0], "📊 ") {
		t.Errorf("首行 = %q，期望是「📊 …」的标题行", lines[0])
	}
}

// TestTelegramSettingsKeepsReportsWhenFieldsMissing 钉住"字段缺失 = 不改"。
//
// 三个开关和 Telegram 配置挤在同一个 PUT 里，所以"没传"与"传了 false"必须分得开：
// 拿 curl 改一下 chat_id、body 里不带这三个字段时，开关**不许**被静默关掉 ——
// 那种故障要等到第二天早上没收到报告才会被发现。
//
// 项目里已有这个先例：store.NewNode.SortOrder 用 *int 区分"没传"与"传了 0"。
func TestTelegramSettingsKeepsReportsWhenFieldsMissing(t *testing.T) {
	h := newAuthHarness(t)
	const token = "123456789:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw"

	// 先把三个开关全打开（带上完整字段），并配一个能通过校验的 Bot。
	if status, body := h.put(t, "/api/v1/settings/telegram", map[string]any{
		"enabled": true, "bot_token": token, "chat_id": "12345",
		"daily_report": true, "weekly_report": true, "monthly_report": true,
	}); status != http.StatusOK {
		t.Fatalf("打开三个开关失败: %d %v", status, body)
	}

	// 只改 chat_id，**不带**那三个字段 —— 这是"用 curl 顺手改一下"的样子。
	status, body := h.put(t, "/api/v1/settings/telegram", map[string]any{
		"enabled": true, "chat_id": "99999",
	})
	if status != http.StatusOK {
		t.Fatalf("只改 chat_id 失败: %d %v", status, body)
	}
	if body["chat_id"] != "99999" {
		t.Fatalf("chat_id 没改成功: %v", body["chat_id"])
	}
	for _, key := range []string{"daily_report", "weekly_report", "monthly_report"} {
		if body[key] != true {
			t.Errorf("请求里没带 %s，它却被改成了 %v：字段缺失应当表示「不改」", key, body[key])
		}
	}
	// 服务端侧的真相也要核一遍（响应是回显，库里的才是事实）。
	status, got := h.get(t, "/api/v1/settings/telegram")
	if status != http.StatusOK {
		t.Fatalf("读取通知设置失败: %d", status)
	}
	for _, key := range []string{"daily_report", "weekly_report", "monthly_report"} {
		if got[key] != true {
			t.Errorf("库里 %s = %v，期望 true", key, got[key])
		}
	}

	// 反过来：**显式传 false** 必须真的关掉它（指针的意义就在这里）。
	status, body = h.put(t, "/api/v1/settings/telegram", map[string]any{
		"enabled": true, "chat_id": "99999", "daily_report": false,
	})
	if status != http.StatusOK {
		t.Fatalf("显式关掉日报失败: %d %v", status, body)
	}
	if body["daily_report"] != false {
		t.Errorf("显式传 daily_report=false 之后它还是 %v", body["daily_report"])
	}
	if body["weekly_report"] != true || body["monthly_report"] != true {
		t.Errorf("只传了 daily_report，另外两个却被改了：weekly=%v monthly=%v",
			body["weekly_report"], body["monthly_report"])
	}
}

// TestPipelineLoopSendsDueReportOnStartup 走的是**真正的接线**：
// pipelineLoop 启动时那一跳就应当把"当天已经过了 09:00 的这一期"发出去。
//
// 为什么必须单独测这一条：判定、统计、渲染、入队各有各的用例，但"它到底有没有
// 被挂到流水线的 goroutine 上"没人管 —— 少一行调用的话上面所有用例照样全绿，
// 而线上永远收不到报告（这正是不许为它新开 goroutine 的代价：接线只有一处）。
//
// pipelineLoop 用的是 time.Now，没法注入，所以这里反过来构造时区：
// 让"服务端本地时间"落在 10:00（离 09:00 还有一小时余量，离次日 09:00 还有 23 小时）。
func TestPipelineLoopSendsDueReportOnStartup(t *testing.T) {
	utc := time.Now().UTC()
	offset := 10*time.Hour -
		time.Duration(utc.Hour())*time.Hour -
		time.Duration(utc.Minute())*time.Minute
	loc := time.FixedZone("TEST+10", int(offset.Seconds()))
	if hour := time.Now().In(loc).Hour(); hour < trafficNotifyHour {
		t.Fatalf("构造的时区没把当地时间摆到 %02d:00 之后：%s", trafficNotifyHour, time.Now().In(loc))
	}

	cfg := config.Default()
	// 汇率每天一次的出网请求与这条用例无关，关掉它让用例不碰网络。
	cfg.FX = false
	// 守卫先建：它的核账挂在清理链的最后（= t.TempDir() 的删除之后），
	// 而这个数据目录正是"用例返回后还有没有人在写"要盯的地方。
	guard := newBackgroundGuard(t)
	dbPath := filepath.Join(t.TempDir(), "probe.db")
	guard.watchDir(filepath.Dir(dbPath))
	srv, recorder := newTrafficNotifyHarnessAt(t, loc, dbPath, nil)
	srv.cfg = cfg

	ctx := context.Background()
	node, _, err := srv.db.CreateNode(ctx, store.NewNode{
		Name: "pipe-01", IntervalSec: 1, TrafficWarnPct: 80, ResetDay: 1,
	}, time.Now())
	if err != nil {
		t.Fatalf("创建节点: %v", err)
	}
	// 昨天（按服务端时区的今天往前一天）的量。
	today := store.DayStart(time.Now().In(loc), loc)
	seedDailyTraffic(t, srv, node.ID, store.FormatDay(today.AddDate(0, 0, -1)), 3_000_000_000, 1_000_000_000)
	turnOnTrafficReports(t, srv, trafficNotifySwitches{Daily: true})

	// 循环用可取消的 context 启动，并且**必须在用例返回前等它真的退出**。
	//
	// 为什么"光 cancel 不够"：cancel 只是发一个信号，循环可能还在跑最后一段 ——
	// 这条用例等到哨兵落库时，它往往正走在启动那几跳的后半段（读汇率、
	// 收尾落盘），手里还捏着 SQLite 连接。用例一返回，t.TempDir() 的清理就会
	// 去删那个目录，而 SQLite 会在删除的空隙里重建 -wal/-shm：Linux 的 RemoveAll
	// 严格，于是报 "directory not empty"；Windows 宽容，同样的漏等本机全绿。
	// loop.stop() = cancel + 等 done 关上（defer 在 t.Cleanup 之前执行，
	// 所以它一定跑在临时目录被删之前）。
	loop := guard.start("pipelineLoop", srv.pipelineLoop)
	defer loop.stop()

	note := recorder.wait(t, alert.RuleTrafficReport, 20*time.Second)
	if !strings.Contains(note.Body, "pipe-01") {
		t.Errorf("报告里没有那台机器：%q", note.Body)
	}
	if !strings.Contains(note.Body, "↑ 1.00 GB") || !strings.Contains(note.Body, "↓ 3.00 GB") {
		t.Errorf("报告里的数字与灌进去的日流量对不上：%q", note.Body)
	}
	// 哨兵也要真的落库：否则每分钟的 ticker 会把同一条报告发 1440 次。
	//
	// 等一小会儿再读：通知是分发器在另一个 goroutine 里发出去的，
	// 而"收到通知"与"哨兵写进库"之间没有先后保证（写哨兵排在入队之后）。
	wantDay := store.FormatDay(today)
	var gotDay string
	waitFor(t, 5*time.Second, "投递日期落库", func() bool {
		value, ok, err := srv.db.GetSetting(ctx, store.KeyTrafficNotifyDailyLast)
		if err != nil || !ok {
			return false
		}
		gotDay = value
		return value == wantDay
	})
	if gotDay != wantDay {
		t.Fatalf("投递日期没有落库：%q，期望 %q", gotDay, wantDay)
	}
}

// TestTrafficReportSamplesForReview 把三份消息的**原文**打进测试输出，
// 并且让这三份样本本身就能证明"窗口真的生效了"。
//
// 为什么数据要**跨多个日期离散分布**：如果三份样本用的是同一组数字，那么
// "窗口写错了、三份报告都汇总了全部历史"与"窗口正确"打印出来会**一模一样** ——
// 那样这三份样本一个字符都证明不了。所以这里刻意把流量撒在
// 10-06（只在"昨天"里）、10-05/10-08/10-11（只在"上周"里）、
// 10-20/10-25（只在"10 月"里），另外再撒两笔 09-15 与 11-05（三份都不该有，
// 一旦窗口失效它们立刻会把数字顶上去）。三份合计因此**必然互不相同**，
// 末尾还会断言这一点。
//
// 这不是给人看的日志，而是把"真正会被发出去的那段文本"固定下来：列对齐、
// ↑↓ 的方向、单位口径（1000 进制）这几件事没法用断言完全表达，
// 出问题时对照它最快。跑法：
//
//	go test ./internal/server/ -run TestTrafficReportSamplesForReview -v
func TestTrafficReportSamplesForReview(t *testing.T) {
	loc := trafficNotifyTestLoc(t)
	ctx := context.Background()
	srv, recorder := newTrafficNotifyHarness(t, loc)

	// 三台机器：两台有量、一台**整个历史都没有任何流量**（报告里必须是 0 B）。
	ids := map[string]int64{}
	for _, name := range []string{"hk-01", "us-01", "sg-01"} {
		node, _, err := srv.db.CreateNode(ctx, store.NewNode{
			Name: name, IntervalSec: 1, TrafficWarnPct: 80, ResetDay: 1,
		}, time.Now())
		if err != nil {
			t.Fatalf("创建节点 %s: %v", name, err)
		}
		ids[name] = node.ID
	}

	// 灌进 traffic_daily 的原始数据（rx = 下行/下载，tx = 上行/上传）。
	seed := []struct {
		node    string
		day     string
		rx, tx  int64
		comment string
	}{
		{"hk-01", "2026-09-15", 9_000_000_000, 9_000_000_000, "三份都不该有（窗口失效才会露出来）"},
		{"hk-01", "2026-10-05", 2_000_000_000, 500_000_000, "只在上周里（不在昨天）"},
		{"hk-01", "2026-10-06", 8_120_000_000, 1_230_000_000, "昨天：三份都有"},
		{"hk-01", "2026-10-11", 1_000_000_000, 250_000_000, "上周日：只在上周里"},
		{"hk-01", "2026-10-20", 4_000_000_000, 1_000_000_000, "只在 10 月里"},
		{"hk-01", "2026-11-05", 7_000_000_000, 7_000_000_000, "未来：三份都不该有"},
		{"us-01", "2026-10-06", 3_010_000_000, 512_000_000, "昨天：三份都有"},
		{"us-01", "2026-10-08", 1_500_000_000, 300_000_000, "只在上周里"},
		{"us-01", "2026-10-25", 2_500_000_000, 700_000_000, "只在 10 月里"},
	}
	for _, s := range seed {
		seedDailyTraffic(t, srv, ids[s.node], s.day, s.rx, s.tx)
		t.Logf("种子 %s %s：↓ %s / ↑ %s —— %s", s.node, s.day,
			alert.FormatBytes(s.rx), alert.FormatBytes(s.tx), s.comment)
	}

	turnOnTrafficReports(t, srv, trafficNotifySwitches{Daily: true, Weekly: true, Monthly: true})

	// 依次把时钟推到三个触发点，全部走**真实**链路：
	// 判定 → 查 traffic_daily → 按窗口求和 → 渲染 → 分发器 → 通知器。
	//
	// 日报每天都发，所以这一路会额外带上两份"别的日期"的日报：
	//	10-07（周三）→ 日报（10-06）
	//	10-12（周一）→ 日报（10-11） + 周报（10-05 ~ 10-11）
	//	11-01（周日、1 号）→ 日报（10-31） + 月报（10-01 ~ 10-31）
	steps := []struct {
		at   string
		want int
	}{
		{"2026-10-07 09:00", 1},
		{"2026-10-12 09:00", 3},
		{"2026-11-01 09:00", 5},
	}
	for _, st := range steps {
		srv.checkTrafficReportsAt(ctx, atLocal(t, loc, st.at))
		notes := waitNotifications(t, recorder, st.want)
		if len(notes) != st.want {
			t.Fatalf("%s 之后应当收到 %d 条通知，实际 %d 条：%v",
				st.at, st.want, len(notes), firstLines(notes))
		}
	}
	notes := recorder.snapshot()

	// 按首行里的区间标签挑出要展示的那三份。
	pick := func(label string) alert.Notification {
		t.Helper()
		for _, n := range notes {
			if strings.Contains(firstLine(n.Body), label) {
				return n
			}
		}
		t.Fatalf("没有收到「%s」那一份，已收到：%v", label, firstLines(notes))
		return alert.Notification{}
	}

	cases := []struct {
		report string
		label  string
		// wantTotal 是这一期合计行里**必须**出现的三个数（手算自上面那张表）。
		wantTotal []string
	}{
		// 只算 10-06：hk-01(8.12+1.23) + us-01(3.01+0.512)
		{trafficReportDaily, "（昨天 10-06）", []string{"↑ 1.74 GB", "↓ 11.1 GB", "计 12.9 GB"}},
		// 只算 10-05 ~ 10-11：多出 hk-01 的 10-05、10-11 与 us-01 的 10-08
		{trafficReportWeekly, "（上周 10-05 ~ 10-11）", []string{"↑ 2.79 GB", "↓ 15.6 GB", "计 18.4 GB"}},
		// 只算 10-01 ~ 10-31：再多出 hk-01 的 10-20 与 us-01 的 10-25
		{trafficReportMonthly, "（上月 10-01 ~ 10-31）", []string{"↑ 4.49 GB", "↓ 22.1 GB", "计 26.6 GB"}},
	}

	sums := map[string]string{}
	samples := make([]string, 0, len(cases))
	for _, c := range cases {
		note := pick(c.label)
		// 通知器（Telegram）真正发出去的文本 = 它拿到的那条通知再渲染一次。
		wire := alert.RenderBatch([]alert.Notification{note})
		if wire != note.Body {
			t.Fatalf("%s：发出去的文本与正文不一致（标题没留空？）:\n%s", c.report, wire)
		}
		first := firstLine(wire)
		if !strings.HasPrefix(first, "📊 ") {
			t.Errorf("%s 的首行 = %q，期望以 📊 开头（它是汇报，不是告警）", c.report, first)
		}
		if n := strings.Count(wire, "流量"); n == 0 {
			t.Errorf("%s 的正文里连报告名都没有", c.report)
		}

		sum := reportLineFor(t, wire, "合计")
		for _, want := range c.wantTotal {
			if !strings.Contains(sum, want) {
				t.Errorf("%s 的合计行缺少 %q：%q", c.report, want, sum)
			}
		}
		sums[c.report] = sum

		// 窗口外的两笔（09-15 与 11-05）一笔都不许进来：这正是"窗口生效"的自证。
		for _, leak := range []string{"9.00 GB", "7.00 GB"} {
			if strings.Contains(wire, leak) {
				t.Errorf("%s 里出现了窗口外的流量 %s：统计区间没生效\n%s", c.report, leak, wire)
			}
		}
		samples = append(samples, fmt.Sprintf("【%s】\n%s", c.report, wire))
	}

	// 三份合计必须两两不同。三份一模一样正是"三份都汇总了全部历史"的样子 ——
	// 那种情况下这三份样本一个字符都证明不了。
	if sums[trafficReportDaily] == sums[trafficReportWeekly] ||
		sums[trafficReportWeekly] == sums[trafficReportMonthly] ||
		sums[trafficReportDaily] == sums[trafficReportMonthly] {
		t.Fatalf("三份报告的合计有两份完全相同，样本没有区分度：%v", sums)
	}

	for _, s := range samples {
		t.Logf("\n%s\n", s)
	}
}

// ---------------------------------------------------------------- 测试脚手架

// count 返回已经收到的通知条数。
//
// recordingNotifier（见 alert_test.go）自带的是"等某一条"，这里补一个"数一数"：
// "不该发"的用例要断言的正是**条数**（0 条 / 1 条）。
func (n *recordingNotifier) count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.received)
}

// snapshot 返回已经收到的通知（副本）：用例要连着看几条，而不是只等其中一条。
func (n *recordingNotifier) snapshot() []alert.Notification {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]alert.Notification(nil), n.received...)
}

// waitNotifications 等到至少 want 条通知之后再取快照（超时直接失败）。
func waitNotifications(t *testing.T, n *recordingNotifier, want int) []alert.Notification {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && n.count() < want {
		time.Sleep(5 * time.Millisecond)
	}
	return n.snapshot()
}

// firstLine 取文本的第一行（报告的"标题"就在这一行上，见 trafficReportNotifications）。
func firstLine(text string) string {
	if at := strings.IndexByte(text, '\n'); at >= 0 {
		return text[:at]
	}
	return text
}

// firstLines 把一批通知的首行收起来，失败信息里一眼能看出收到了哪几份。
func firstLines(notes []alert.Notification) []string {
	out := make([]string, 0, len(notes))
	for _, n := range notes {
		out = append(out, firstLine(n.Body))
	}
	return out
}

// newTrafficNotifyHarness 起一个"真库 + 真 Server"的脚手架，并返回记录型通知器。
func newTrafficNotifyHarness(t *testing.T, loc *time.Location) (*Server, *recordingNotifier) {
	t.Helper()
	return newTrafficNotifyHarnessAt(t, loc, filepath.Join(t.TempDir(), "probe.db"), nil)
}

// newTrafficNotifyHarnessAt 与上面一样，但可以指定库路径、也可以复用已有的库
// （reuse 非空 = "同一个库、换一个进程"，用来验重启后的行为）。
func newTrafficNotifyHarnessAt(t *testing.T, loc *time.Location, dbPath string, reuse *store.DB) (*Server, *recordingNotifier) {
	t.Helper()
	// 这个脚手架自己起了一个后台执行体（通知分发器的 worker），
	// 它同样要在用例返回前停稳 —— 见 newBackgroundGuard。
	guard := newBackgroundGuard(t)
	db := reuse
	if db == nil {
		opened, err := store.Open(context.Background(), dbPath)
		if err != nil {
			t.Fatalf("打开测试数据库: %v", err)
		}
		t.Cleanup(func() { _ = opened.Close() })
		db = opened
	}
	guard.watchDir(filepath.Dir(dbPath))

	srv := New(config.Default(), db, slog.New(slog.DiscardHandler), loc)
	// 分发器换成"不等合并窗口、不等独占间隔"的：默认 3 秒窗口 + 1.5 秒分片间隔
	// 会让每条用例白等好几秒，而这两件事各自有专门的用例
	// （internal/alert 的 coalesce / ExclusiveGap），这里要验的是报告的判定与内容。
	opts := alert.DefaultDispatcherOptions()
	opts.Coalesce = 20 * time.Millisecond
	opts.RateLimit = 0
	opts.ExclusiveGap = 0
	recorder := newRecordingNotifier()
	srv.dispatch = alert.NewDispatcher(slog.New(slog.DiscardHandler), []alert.Notifier{recorder}, opts)

	dispatch := guard.start("dispatch", srv.dispatch.Start)
	// t.Cleanup 后进先出：这一条跑在数据库关闭与临时目录删除之前。
	t.Cleanup(dispatch.stop)
	return srv, recorder
}

// turnOnTrafficReports 把开关写进 settings（等价于用户在设置页勾上再保存）。
func turnOnTrafficReports(t *testing.T, srv *Server, switches trafficNotifySwitches) {
	t.Helper()
	if err := srv.saveTrafficNotifySwitches(context.Background(), switches); err != nil {
		t.Fatalf("保存定时报告开关: %v", err)
	}
}

// seedDailyTraffic 直接往 traffic_daily 写一行（日流量是 Agent 累加出来的，
// 没有"灌一天的量"这种接口；这里要验的是统计与渲染，不是采集链路）。
func seedDailyTraffic(t *testing.T, srv *Server, nodeID int64, day string, rx, tx int64) {
	t.Helper()
	if _, err := srv.db.Writer().ExecContext(context.Background(),
		`INSERT INTO traffic_daily (node_id, day, rx, tx) VALUES (?, ?, ?, ?)`,
		nodeID, day, rx, tx); err != nil {
		t.Fatalf("写入日流量 %s: %v", day, err)
	}
}

// reportLineFor 取出报告里以 name 开头的那一行（找不到就报错）。
//
// 按"行首是这个名字"匹配而不是 Contains：名字短的节点（hk-01）会命中
// 名字更长的节点（hk-011）那一行，断言就串了。
func reportLineFor(t *testing.T, body, name string) string {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, name) {
			return line
		}
	}
	t.Fatalf("报告里没有 %s 那一行：\n%s", name, body)
	return ""
}
