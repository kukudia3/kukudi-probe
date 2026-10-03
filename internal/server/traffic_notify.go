package server

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"probe/internal/alert"
	"probe/internal/store"
)

// trafficNotifyHour 是三种定时报告的触发小时：服务端时区（--timezone）的 09:00。
//
// 只写"小时"是因为分钟固定是 0，而"09:00"这个点本身要与 --timezone 相关
// （切天、判"今天是不是周一/1 号"都按它，见 store.DayStart / store.WeekStart）。
const trafficNotifyHour = 9

// trafficNotifyEvery 是"现在该不该发报告"的检查周期。
//
// 一分钟一次，与 pipelineLoop 里其它 ticker 同量级：绝大多数时候它只做一次
// settings 读取就返回。粒度取一分钟是因为判定精确到"小时 + 哪一天"，
// 为此单独设计一套"距离下一个 09:00 还有多久"的定时器没有收益，
// 而且那种定时器在休眠唤醒后本来就是不准的。
const trafficNotifyEvery = time.Minute

// trafficReportMaxUnits 是一条消息正文的长度上限（UTF-16 单元，与 Telegram 一致）。
//
// Telegram 的 text 上限是 4096 个字符，这里取 3000：分片是按整行切的，最后一片
// 可能比预算多出一整行的长度 —— 留出余量比掐着上限安全。
//
// 名字里的 Units 是这一轮改的：计账的地方是 alert.ChunkLines，它按 UTF-16 单元算
// （一个 emoji 占 2 个，与 Telegram 同口径）。
const trafficReportMaxUnits = 3000

// trafficReportCounterReserve 是分片序号（" 999/999"）在长度预算里占的位置。
const trafficReportCounterReserve = 8

// 报告名（trafficNotifyState 的键，也是日志里那个 report= 字段）。
const (
	trafficReportDaily   = "daily"
	trafficReportWeekly  = "weekly"
	trafficReportMonthly = "monthly"
)

// trafficReportSeparator 是明细与合计之间那条分隔线。
var trafficReportSeparator = strings.Repeat("─", 36)

// trafficReportSpec 描述一种定时报告：什么时候该发、报告哪一段、标题怎么写。
//
// 三个时间相关的字段都是纯函数（时间进、时间出），所以"触发时刻"与"区间边界"
// 这两件最容易出错的事可以在测试里注入固定时间逐条钉住 —— 不必真的等到
// 周一早上九点，也不必依赖 time.Now()。
type trafficReportSpec struct {
	// Name 是稳定标识（日志、测试与 trafficNotifyState 的键）。
	Name string
	// Switch / LastKey 是 settings 表里的两个键：开关、上次投递的触发日。
	Switch  string
	LastKey string
	// Label 是报告名（"流量日报"……）。
	Label string
	// Fires 判断"今天（服务端时区的本地日）算不算触发日"。
	Fires func(local time.Time) bool
	// Window 返回要统计的**上一个完整周期** [start, end)。
	Window func(local time.Time, loc *time.Location) (time.Time, time.Time)
	// Period 是标题里那句"报告的是哪一段"。
	Period func(start, end time.Time) string
}

// trafficReportSpecs 是三种报告的定义，顺序就是消息的发送顺序。
//
// 区间口径刻意与"触发时刻的那 9 个小时"无关：报告说的是**上一个完整周期**
// （昨天 / 上周 / 上月），而不是"从 09:00 到 09:00"。理由有两层：
//   - 用户要的是"这一天/这一周/这一个月用了多少"，按 09:00 切出来的那段
//     跨两个自然日，与面板上「今日流量」按天切的口径对不上账；
//   - 按自然周期切可以直接复用 traffic_daily 的按天明细（store.DayStart /
//     store.WeekStart），不必再引入第二套"从几点开始算一天"。
var trafficReportSpecs = []trafficReportSpec{
	{
		Name:    trafficReportDaily,
		Switch:  store.KeyTrafficNotifyDaily,
		LastKey: store.KeyTrafficNotifyDailyLast,
		Label:   "流量日报",
		Fires:   func(time.Time) bool { return true },
		Window: func(local time.Time, loc *time.Location) (time.Time, time.Time) {
			today := store.DayStart(local, loc)
			// 昨天 00:00 → 今天 00:00。AddDate 而不是 Add(-24h)：夏令时那天
			// 一天不是 24 小时，往前退一天要按日历退。
			return today.AddDate(0, 0, -1), today
		},
		Period: func(start, _ time.Time) string {
			return "昨天 " + start.Format("01-02")
		},
	},
	{
		Name:    trafficReportWeekly,
		Switch:  store.KeyTrafficNotifyWeekly,
		LastKey: store.KeyTrafficNotifyWeeklyLast,
		Label:   "流量周报",
		// 只在周一发：用户要的是"礼拜一早上看上一周"。
		Fires: func(local time.Time) bool { return local.Weekday() == time.Monday },
		Window: func(local time.Time, loc *time.Location) (time.Time, time.Time) {
			// 周界复用 store.WeekStart（周一 00:00），不自己再算一套。
			thisWeek := store.WeekStart(local, loc)
			return thisWeek.AddDate(0, 0, -7), thisWeek
		},
		Period: func(start, end time.Time) string {
			return fmt.Sprintf("上周 %s ~ %s", start.Format("01-02"), end.AddDate(0, 0, -1).Format("01-02"))
		},
	},
	{
		Name:    trafficReportMonthly,
		Switch:  store.KeyTrafficNotifyMonthly,
		LastKey: store.KeyTrafficNotifyMonthlyLast,
		Label:   "流量月报",
		// 只在 1 号发。
		Fires: func(local time.Time) bool { return local.Day() == 1 },
		Window: func(local time.Time, loc *time.Location) (time.Time, time.Time) {
			year, month, _ := local.Date()
			// 本月 1 日 00:00 与上月 1 日 00:00。月份传 0 会被 time.Date 归一化成
			// 上一年的 12 月 —— 跨年不用自己判（与 WeekStart 里 day-offset 同一套写法）。
			return time.Date(year, month-1, 1, 0, 0, 0, 0, loc),
				time.Date(year, month, 1, 0, 0, 0, 0, loc)
		},
		Period: func(start, end time.Time) string {
			return fmt.Sprintf("上月 %s ~ %s", start.Format("01-02"), end.AddDate(0, 0, -1).Format("01-02"))
		},
	},
}

// trafficNotifySwitches 是三个开关的当前值。
//
// 单独一个结构体（而不是三个 bool 参数一路传）：GET/PUT /settings/telegram
// 的 JSON 形状与它一一对应，保存、比对"关→开"、读回设置都只用传它一个。
type trafficNotifySwitches struct {
	Daily   bool
	Weekly  bool
	Monthly bool
}

// asMap 把开关摊成 spec.Name -> bool，供只认名字的判定逻辑使用。
func (sw trafficNotifySwitches) asMap() map[string]bool {
	return map[string]bool{
		trafficReportDaily:   sw.Daily,
		trafficReportWeekly:  sw.Weekly,
		trafficReportMonthly: sw.Monthly,
	}
}

// trafficNotifyState 是"三个开关现在什么状态 + 各自上次投递的触发日"。
//
// 两者总是一起读（一次 GetSettings），所以放在同一个结构体里 —— 它们的键
// 是一对一的伴生关系（见 store.KeyTrafficNotifyDaily / ...DailyLast）。
type trafficNotifyState struct {
	On   map[string]bool   // spec.Name -> 开关
	Last map[string]string // spec.Name -> 上次投递的触发日（"" 表示从没发过）
}

// switches 取出三个开关的当前值。
func (st trafficNotifyState) switches() trafficNotifySwitches {
	return trafficNotifySwitches{
		Daily:   st.On[trafficReportDaily],
		Weekly:  st.On[trafficReportWeekly],
		Monthly: st.On[trafficReportMonthly],
	}
}

// trafficReportPlan 是一次"该发报告了"的判定结果。
type trafficReportPlan struct {
	Spec  trafficReportSpec
	Day   string // 触发日（服务端时区 YYYY-MM-DD）：也是要写回的哨兵值
	Start time.Time
	End   time.Time
	// Last 是判定时 settings 里那个哨兵值的**原文**：投递被队列满打断时会写上
	// "<触发日>#<已入队片数>" 的断点，下一分钟据此续传（见 reportProgress）。
	Last string
}

// Title 返回不带分片序号的标题，例如「流量日报（昨天 10-01）」。
//
// 分片序号由 trafficReportNotifications 追加：只有一片时不写序号，
// 免得每条正常的报告都挂一个 "1/1"。
func (p trafficReportPlan) Title() string {
	return p.Spec.Label + "（" + p.Spec.Period(p.Start, p.End) + "）"
}

// trafficReportRow 是一台机器在一个周期里的流量。
//
// Tx 是上行（上传）、Rx 是下行（下载）—— 与面板上的 ↑/↓ 同一套口径
// （见 web/app.js 的 trafficCell 与总览区），报告里不另立一套。
type trafficReportRow struct {
	Name string
	Tx   int64
	Rx   int64
}

// trafficNotifyTrigger 返回某一天的触发时刻（服务端时区的 hour:00）。
//
// 显式用 time.Date 而不是 day.Add(hour * time.Hour)：夏令时切换那天
// "零点 + 9 小时"不等于"本地 09:00"。
func trafficNotifyTrigger(day time.Time, loc *time.Location, hour int) time.Time {
	return time.Date(day.Year(), day.Month(), day.Day(), hour, 0, 0, 0, loc)
}

// planTrafficReports 返回"现在该发哪些报告"（0~3 份）。
//
// 判定规则三个报告共用一套：
//   - 触发点是**当天 09:00**（服务端时区）；Fires 决定今天算不算触发日，
//     于是周报只在周一、月报只在 1 号；
//   - 还没到 09:00 → 今天没到点，不发；
//   - 过了 09:00 且今天还没发过 → 发（这正是"重启 / 休眠错过了 09:00"的补发）；
//   - 今天已经发过 → 不发（一天之内检查多少次都只发一次）。
//
// "错过一整天之后不补发"是这套规则的自然结果：候选触发日**只有今天**
// （昨天那个触发点根本不在候选里），所以停机一周回来也只会发当天这一期，
// 不会攒出一串迟到的报告。
func planTrafficReports(now time.Time, loc *time.Location, hour int, state trafficNotifyState) []trafficReportPlan {
	if loc == nil {
		loc = time.UTC
	}
	local := now.In(loc)
	day := store.DayStart(local, loc)
	if local.Before(trafficNotifyTrigger(day, loc, hour)) {
		return nil
	}
	dayKey := store.FormatDay(day)

	var out []trafficReportPlan
	for _, spec := range trafficReportSpecs {
		if !state.On[spec.Name] || !spec.Fires(local) {
			continue
		}
		if state.Last[spec.Name] == dayKey {
			continue // 今天这一期已经投递过
		}
		start, end := spec.Window(local, loc)
		out = append(out, trafficReportPlan{
			Spec: spec, Day: dayKey, Start: start, End: end, Last: state.Last[spec.Name],
		})
	}
	return out
}

// reportName 把节点名压成**一行**，并补上分组与地区。
//
// 报告的不变量是"每行一台机器"：名字里混进换行（接口没有字符集限制，
// 直接写库也能塞进来）会把这一行拆成两行，后半截看起来就像另一台机器。
// 控制字符统一换成空格，与告警文案里"宁可难看也不能因为格式发不出去"
// 是同一个取舍（见 alert.RenderBatch 不用 Markdown 的理由）。
//
// 写法（名称（分组 · 地区））与告警**完全同源**（alert.DisplayName），
// 分组与地区同样要过一遍"压成一行"：它们也会出现在报告里。
// 名字被清空时退回 node-<id>：空名字在报告里是一行看不出是谁的记录。
//
// 刻意**不截断**：名字被截掉的后半截正是"哪台机器"的信息（hk-01 与 hk-011
// 在报告里必须分得开）。这一行因此可能变长，代价是值得的。
func reportName(n store.Node) string {
	name := singleLine(n.Name)
	if name == "" {
		name = fmt.Sprintf("node-%d", n.ID)
	}
	return alert.DisplayName(name, singleLine(n.GroupName), singleLine(n.Region))
}

// singleLine 把一段文本压成一行并去掉首尾空白。
//
// 三类字符会被换成空格（顺序即"确定的换行"到"看不见的欺骗"）：
//
//   - Cc 控制字符：\n \r \t U+0085 等 —— 换行是唯一能真的**多出一行**的字符；
//   - Zl / Zp（U+2028 行分隔符、U+2029 段分隔符）：Unicode 意义上就是换行，
//     多数终端与聊天客户端也按换行渲染，但它们**不是** Cc，unicode.IsControl
//     返回 false，所以旧实现原样放行；
//   - 双向控制符（Unicode 的 Bidi_Control：LRM/RLM/ALM、LRE..RLO、LRI..PDI）：
//     不换行，但能把后面的字符**显示**成别的样子（U+202E RLO 之后整行反向），
//     用来把 hk-01 伪装成另一台机器、或让两台不同的机器看起来同名。
//
// 为什么没有把整个 Cf（格式字符）都换掉：Cf 里还有 U+200D 零宽连接符（emoji
// 序列靠它拼合）、U+00AD 软连字符这类**合法**字符，一律抹掉会把正常名字改样。
// U+200B 零宽空格 / U+FEFF BOM 属于"看不见但能造成同形名"的残留风险，见交付说明。
func singleLine(s string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' || isBidiControl(r) {
			return ' '
		}
		return r
	}, s))
}

// isBidiControl 报告 r 是不是 Unicode 的 Bidi_Control 字符。
//
// 逐个列出而不是用 unicode.In(r, unicode.Cf) 之类的近似：Cf 比 Bidi_Control 宽
// 得多（见 singleLine 的注释），而这里要的是"能改变显示顺序的那几个"。
func isBidiControl(r rune) bool {
	switch r {
	case '\u061C', // ARABIC LETTER MARK
		'\u200E', '\u200F', // LRM / RLM
		'\u202A', '\u202B', '\u202C', '\u202D', '\u202E', // LRE / RLE / PDF / LRO / RLO
		'\u2066', '\u2067', '\u2068', '\u2069': // LRI / RLI / FSI / PDI
		return true
	}
	return false
}

// trafficReportRows 把"每节点每天一行"的日流量按周期求和。
//
// 顺序跟着传进来的 nodes（也就是 ListNodes 的顺序 = 首页显示顺序），
// 每台机器都有一行：整个周期没有流量、或者从来没上报过（库里一行都没有）的
// 节点写 0，绝不跳过 —— 报告一半的价值在"谁用了多少"，另一半在"谁的用量是 0"
// （那正是"这台机器是不是没在跑"的线索）。
func trafficReportRows(nodes []store.Node, daily []store.DailyTraffic, start, end time.Time) []trafficReportRow {
	startDay, endDay := store.FormatDay(start), store.FormatDay(end)
	sums := make(map[int64][2]int64, len(nodes))
	for _, d := range daily {
		// 日期是 YYYY-MM-DD，字典序就是时间序，可以直接比大小
		// （与 buildTrafficAgg 同一套写法）。上界写 end 而不是只看下界：
		// 时钟回拨或改过时区时库里可能留下"未来"的行，它们不属于这一期。
		if d.Day < startDay || d.Day >= endDay {
			continue
		}
		cur := sums[d.NodeID]
		sums[d.NodeID] = [2]int64{cur[0] + d.Rx, cur[1] + d.Tx}
	}

	rows := make([]trafficReportRow, 0, len(nodes))
	for _, n := range nodes {
		sum := sums[n.ID]
		rows = append(rows, trafficReportRow{Name: reportName(n), Tx: sum[1], Rx: sum[0]})
	}
	return rows
}

// padRight 按**字符**（不是字节）右补空格：中文名字按字节算会多出一倍的宽度，
// 整张表就会歪。
func padRight(s string, width int) string {
	if gap := width - utf8.RuneCountInString(s); gap > 0 {
		return s + strings.Repeat(" ", gap)
	}
	return s
}

// renderTrafficReport 把报告渲染成消息正文的分片。
//
// 返回的每一片都是一条**独立的消息**（见 trafficReportNotifications）。节点很多时
// 一条消息装不下，这里按行切成几片，而不是截断：截断会把排在后面的机器整台整台
// 地漏掉，而"每台机器都有自己那一行"正是这个报告的全部意义。
func renderTrafficReport(header string, rows []trafficReportRow, maxRunes int) [][]string {
	type cells struct{ name, up, down, total string }
	lines := make([]cells, 0, len(rows))
	nameW, upW, downW := 0, 0, 0
	var sumTx, sumRx int64
	for _, r := range rows {
		c := cells{
			name: r.Name,
			// ↑ 是上行（上传，tx）、↓ 是下行（下载，rx）—— 与面板同一套口径。
			up:    "↑ " + alert.FormatBytes(r.Tx),
			down:  "↓ " + alert.FormatBytes(r.Rx),
			total: "计 " + alert.FormatBytes(r.Tx+r.Rx),
		}
		nameW = max(nameW, utf8.RuneCountInString(c.name))
		upW = max(upW, utf8.RuneCountInString(c.up))
		downW = max(downW, utf8.RuneCountInString(c.down))
		sumTx += r.Tx
		sumRx += r.Rx
		lines = append(lines, c)
	}

	// 名称列不截断：名字被截掉一半比"某一行不齐"严重得多（"hk-01" 与 "hk-0…"
	// 分不出是哪台机器）。数字列只在这份报告内部对齐。
	body := make([]string, 0, len(lines)+3)
	for _, c := range lines {
		body = append(body, padRight(c.name, nameW)+"  "+
			padRight(c.up, upW)+"  "+padRight(c.down, downW)+"  "+c.total)
	}
	// 合计：一台机器都没有时不会走到这里（调用方先挡掉空报告），
	// 所以这一行永远配着至少一行明细。
	body = append(body,
		trafficReportSeparator,
		padRight("合计", nameW)+"  "+
			padRight("↑ "+alert.FormatBytes(sumTx), upW)+"  "+
			padRight("↓ "+alert.FormatBytes(sumRx), downW)+"  "+
			"计 "+alert.FormatBytes(sumTx+sumRx))

	items := make([]string, 0, len(body)+1)
	if header != "" {
		items = append(items, header)
	}
	items = append(items, body...)
	// 末两行（分隔线 + 合计）永远留在同一片里，而且是最后一片：
	// 只看到合计却不知道它属于哪几片，等于没有合计。
	//
	// 切分本身与告警消息共用 alert.ChunkLines（只有一处切分实现，
	// 长度账目才不会在两处慢慢跑偏）。
	return alert.ChunkLines(items, 2, maxRunes)
}

// trafficReportNotifications 把一份报告渲染成要投递的通知。
//
// 每片一条通知，多于一片时标题带 "1/3" 这样的序号：分片之后收件人必须能看出
// "还有没有下文"，否则最后一片丢了他也不知道。
//
// NoCoalesce 是必须的：这些分片是紧挨着入队的，被合并窗口重新拼回一条正好会
// 顶破 Telegram 的单条上限（4096 字符），表现是整份报告一条都发不出去。
// 分片之间的最小间隔由分发器的 ExclusiveGap 负责（同一 chat 连发会被限流）。
func trafficReportNotifications(plan trafficReportPlan, rows []trafficReportRow, now time.Time, loc *time.Location) []alert.Notification {
	// 统计区间那行带上时区：周期边界是按 --timezone 切的，不写出来，
	// 在别的时区里看这两个时刻会对不上账。
	header := fmt.Sprintf("统计区间：%s → %s（%s）",
		plan.Start.Format("01-02 15:04"), plan.End.Format("01-02 15:04"), loc)

	// 标题**写在正文第一行**，而 Title 留空。
	//
	// 为什么不能用 Title：分发器交给通知器的那条通知，Body 已经是渲染好的
	// 整段文本，而通知器（Telegram）拿到 Title 非空的通知时会自己再在前面
	// 补一行「图标 + 标题」—— 两下一叠加，消息开头会出现两行标题，第二行的
	// 图标还会被按 severity 猜成告警的绿点。Title 留空 = "Body 就是完整消息"
	// （见 alert.RenderBatch），报告因此既能独占一条消息，也不会顶着告警配色。
	//
	// 标题行每一片都要有（分片之后收件人得知道这一片属于哪份报告的哪一段），
	// 所以切分时要先把"标题 + 序号"占的位置扣掉，否则第一片会超出一行标题的长度。
	title := "📊 " + plan.Title()
	// 标题按 rune 数扣：它比单元数少 1 个（"📊" 在 UTF-16 里是代理对，占 2 个单元），
	// 而 3000 距 Telegram 的 4096 还有一千多，这点差不会让消息发不出去。
	budget := trafficReportMaxUnits - utf8.RuneCountInString(title) - trafficReportCounterReserve - 1
	chunks := renderTrafficReport(header, rows, budget)

	out := make([]alert.Notification, 0, len(chunks))
	for i, chunk := range chunks {
		head := title
		if len(chunks) > 1 {
			head = fmt.Sprintf("%s %d/%d", title, i+1, len(chunks))
		}
		out = append(out, alert.Notification{
			Rule:     alert.RuleTrafficReport,
			Severity: alert.SeverityInfo,
			Body:     strings.Join(append([]string{head}, chunk...), "\n"),
			At:       now,
			// 分片必须独占消息：见函数注释。
			NoCoalesce: true,
		})
	}
	return out
}

// loadTrafficNotifyState 读取三个开关与三个"上次投递日期"。
//
// 每次检查都重新读库（一分钟一次的小查询），不在内存里缓存：
// 用户改完设置必须立刻生效，而缓存又要处理"别处改了设置"的失效问题 ——
// 为一天三次的判断引入一份需要维护的缓存不划算。
func (s *Server) loadTrafficNotifyState(ctx context.Context) (trafficNotifyState, error) {
	keys := make([]string, 0, 2*len(trafficReportSpecs))
	for _, spec := range trafficReportSpecs {
		keys = append(keys, spec.Switch, spec.LastKey)
	}
	values, err := s.db.GetSettings(ctx, keys...)
	if err != nil {
		return trafficNotifyState{}, err
	}
	state := trafficNotifyState{
		On:   make(map[string]bool, len(trafficReportSpecs)),
		Last: make(map[string]string, len(trafficReportSpecs)),
	}
	for _, spec := range trafficReportSpecs {
		state.On[spec.Name] = values[spec.Switch] == "1"
		state.Last[spec.Name] = values[spec.LastKey]
	}
	return state, nil
}

// saveTrafficNotifySwitches 写入三个开关（不动"上次投递日期"）。
func (s *Server) saveTrafficNotifySwitches(ctx context.Context, sw trafficNotifySwitches) error {
	on := sw.asMap()
	values := make(map[string]string, len(trafficReportSpecs))
	for _, spec := range trafficReportSpecs {
		values[spec.Switch] = "0"
		if on[spec.Name] {
			values[spec.Switch] = "1"
		}
	}
	return s.db.SetSettings(ctx, values)
}

// seedTrafficNotifyLatches 处理"开关从关到开"的那一刻。
//
// 点开开关的**瞬间**不该收到报告 —— 那是"下一个触发点"才该发生的事，
// 否则用户勾一下就会立刻收到一条上个月的月报。但也不能无脑把今天记成
// "已投递"：早上八点点开开关的人，当天九点还是要收到的。
//
// 所以只在"今天的触发点已经过去"时才落这个哨兵：今天这一期算跳过，
// 下一次触发（明天 09:00 / 下周一 09:00 / 下月 1 日 09:00）照常。
//
// now 从外面传进来是为了可测（与 checkTrafficReportsAt 同一个理由）：
// 这条规则本身就是"以 09:00 为界的两半"，只有注入固定时刻才测得准。
func (s *Server) seedTrafficNotifyLatches(ctx context.Context, now time.Time, before, after trafficNotifySwitches) error {
	local := now.In(s.loc)
	day := store.DayStart(local, s.loc)
	if local.Before(trafficNotifyTrigger(day, s.loc, trafficNotifyHour)) {
		return nil
	}

	wasOn, nowOn := before.asMap(), after.asMap()
	values := map[string]string{}
	for _, spec := range trafficReportSpecs {
		if nowOn[spec.Name] && !wasOn[spec.Name] {
			values[spec.LastKey] = store.FormatDay(day)
		}
	}
	if len(values) == 0 {
		return nil
	}
	return s.db.SetSettings(ctx, values)
}

// checkTrafficReports 是挂在 pipelineLoop 上的那一跳：判断该不该发，该发就投递。
//
// 它**不新开 goroutine**，也不自己做发送：真正的发送、重试、限流都在告警
// 分发器里（s.dispatch），这里只负责"统计 + 渲染 + 入队 + 记哨兵"。
func (s *Server) checkTrafficReports(ctx context.Context) {
	s.checkTrafficReportsAt(ctx, time.Now())
}

// checkTrafficReportsAt 是 checkTrafficReports 去掉"现在几点"这一步的版本。
//
// 时间从外面传进来是为了可测：触发判定完全依赖"现在"（是不是周一、到没到 09:00、
// 今天是几号），而这正是整条链路里最不能出错的一段 —— 注入固定的时刻之后，
// 测试不必依赖跑测试的时间，也不必真的等到周一早上九点。
func (s *Server) checkTrafficReportsAt(ctx context.Context, now time.Time) {
	state, err := s.loadTrafficNotifyState(ctx)
	if err != nil {
		s.log.Error("读取定时流量报告设置失败", "err", err)
		return
	}
	plans := planTrafficReports(now, s.loc, trafficNotifyHour, state)
	if len(plans) == 0 {
		return
	}
	nodes, err := s.db.ListNodes(ctx)
	if err != nil {
		s.log.Error("读取节点列表失败（本次不发流量报告）", "err", err)
		return
	}
	if len(nodes) == 0 {
		// 没有节点时不发：一条只剩「合计 0 B」的消息没有任何信息量，
		// 而它每天准时出现只会训练用户忽略这些消息。
		//
		// 刻意**不**记哨兵：当天稍后加了机器还能补上这一期，
		// 而且"没有节点"这件事本身不该被一次写入永久封存。
		s.log.Debug("没有节点，跳过定时流量报告")
		return
	}

	for _, plan := range plans {
		sent, err := s.sendTrafficReport(ctx, plan, nodes, now)
		if err != nil {
			s.log.Error("发送定时流量报告失败", "report", plan.Spec.Name, "err", err)
		}
		// 队列满：**绝不写哨兵**。写了这一期就永久缺片 —— 用户收到的是 1..128 片
		// （每片都印着 i/N，他看得到缺），而哨兵让"今天已经发过"成立，尾部分片
		// 再也不会补投。改写断点，下一分钟从这一片继续。
		if errors.Is(err, errReportQueueFull) {
			if perr := s.db.SetSetting(ctx, plan.Spec.LastKey, reportProgressValue(plan.Day, sent)); perr != nil {
				s.log.Warn("记录定时流量报告的投递断点失败（下一分钟会重发队首分片）",
					"report", plan.Spec.Name, "err", perr)
			}
			continue
		}
		// 其它失败照旧写哨兵：真正的重试在分发器里，这里再叠一层每分钟重投
		// 只会让用户收到重复报告。哨兵写失败只记警告：最坏是重启/下一分钟再发
		// 一次，不值得让整条流水线报错。
		if err := s.db.SetSetting(ctx, plan.Spec.LastKey, plan.Day); err != nil {
			s.log.Warn("记录定时流量报告的投递日期失败（可能重发一次）",
				"report", plan.Spec.Name, "err", err)
		}
	}
}

// errReportQueueFull 表示分片没能全部入队（见 Dispatcher.Enqueue 的"队列满直接丢弃"）。
//
// 它必须与别的失败分开：别的失败写哨兵（"这一期就到这儿"），队列满**不能**写 ——
// 写了哨兵这一期就永久缺片。
var errReportQueueFull = errors.New("通知队列已满，报告未能全部投递")

// reportProgress 返回"这一期已经入队了几片"，取自哨兵值里的 "#N" 后缀。
//
// 为什么要记断点：分片是**瞬间**全部入队的（sendTrafficReport 是同步循环），
// 而发送 worker 每片之间要等 ExclusiveGap（1.5 秒）还要过 20 条/分钟的限流 ——
// 队列只有 128 格，所以"分片数 > 128"的部署里尾部分片必然当场失败。
//
// 下一分钟**从头再来一遍**是错的：队列满时能挤进去的永远是队首那几片，于是
// 每一分钟都只重复投递队首、队尾永远轮不到（livelock），用户收到的是没完没了的
// 重复分片，而这一期始终不完整。
func reportProgress(last, day string) int {
	prefix := day + "#"
	if !strings.HasPrefix(last, prefix) {
		return 0 // 空值，或上一个周期的普通哨兵值
	}
	n, err := strconv.Atoi(strings.TrimPrefix(last, prefix))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// reportProgressValue 是断点的落库写法："<触发日>#<已入队片数>"。
//
// 它一个值承担两件事：既是断点，又**不是**"这一期已经投递完"的哨兵
// （planTrafficReports 判的是 `== dayKey`，带后缀就不相等）—— 于是下一分钟会
// 接着投，而只要全部分片入队成功就写回普通哨兵值。
func reportProgressValue(day string, sent int) string {
	return fmt.Sprintf("%s#%d", day, sent)
}

// sendTrafficReport 统计上一个周期的流量并交给通知流水线。
//
// 返回"这一轮成功入队了几片"（调用方在队列满时据此记断点）与错误。
func (s *Server) sendTrafficReport(ctx context.Context, plan trafficReportPlan, nodes []store.Node, now time.Time) (int, error) {
	daily, err := s.db.TrafficDailySince(ctx, store.FormatDay(plan.Start))
	if err != nil {
		return 0, fmt.Errorf("读取日流量失败: %w", err)
	}
	rows := trafficReportRows(nodes, daily, plan.Start, plan.End)
	notes := trafficReportNotifications(plan, rows, now, s.loc)
	// 续传：上一轮被队列满打断时，从断点接着入队（队首那几片不重复投递）。
	start := reportProgress(plan.Last, plan.Day)
	if start > len(notes) {
		start = len(notes)
	}
	for i := start; i < len(notes); i++ {
		n := notes[i]
		// 长度兜底：渲染侧按预算切了行，但预算只算"一行有多长"—— 单行本身超长时
		// （名字列刻意不截断）那一片仍会超过 Telegram 的单条上限，后果是**整条被
		// 拒收**（不是少显示一点，是一条都收不到）。合法数据（名字 ≤64 rune）
		// 永远进不到截断分支；真截断时留一条 Warn（消息末尾也会注明已截断）。
		body, truncated := alert.Truncate(n.Body, trafficReportMaxUnits)
		if truncated {
			s.log.Warn("定时流量报告的单片超过长度预算，已截断（后面几行不会出现在消息里）",
				"report", plan.Spec.Name, "units", alert.MsgUnits(n.Body), "limit", trafficReportMaxUnits)
		}
		n.Body = body
		if !s.dispatch.Enqueue(n) {
			// 已经入队 i 片（下标从 0 起）：把断点交给调用方，下一分钟从这里继续。
			return i, fmt.Errorf("%w（已入队 %d/%d 片）", errReportQueueFull, i, len(notes))
		}
	}
	s.log.Info("已投递流量报告",
		"report", plan.Spec.Name,
		"period", plan.Spec.Period(plan.Start, plan.End),
		"nodes", len(rows),
		"messages", len(notes),
		"resumed_from", start)
	return len(notes), nil
}
