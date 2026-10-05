// Package alert 负责告警规则、去重冷却与通知发送。
//
// 设计约束（docs/DESIGN.md §12）：
//   - 只有一个扩展点 Notifier；v1 只实现 Telegram（外加永远开启的日志通知）；
//   - 规则状态持久化在 alert_state，重启不会重复轰炸；
//   - "不该发的绝不发"和"该发的必须发"同样重要，因此去抖、冷却、合并都在这里做。
package alert

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode"

	"probe/internal/store"
)

// 规则名（同时是 alert_state 里的 rule 字段）。
const (
	RuleOffline         = "offline"
	RuleRecovered       = "recovered"
	RuleTrafficWarn     = "traffic_warn"
	RuleTrafficExceeded = "traffic_exceeded"
	RuleExpiry          = "expiry"
	// RuleTrafficReport 不是"规则"，而是定时流量报告（日/周/月）走同一条通知
	// 流水线时用的名字：它没有 alert_state 行（不是状态机，到点发一次就完了），
	// 但通知里得有个能认出来的 rule，否则日志里分不清它和别的通知。
	RuleTrafficReport = "traffic_report"
)

// 规则状态。
const (
	StateFiring   = "firing"
	StateResolved = "resolved"
)

// Severity 是通知的严重级别，只用于展示与排序。
type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityWarn     Severity = "warn"
	SeverityCritical Severity = "critical"
)

// Notification 是一条要发出去的通知。
type Notification struct {
	NodeID   int64
	NodeName string
	Rule     string
	Severity Severity
	Title    string
	Body     string
	At       time.Time
	// NoCoalesce 表示这条通知要求**独占一条消息**，不参与合并窗口。
	//
	// 定时流量报告用它，因为报告正文本身可能就是好几千字符、而且已经被
	// 调用方切成多条分片：分片被合并窗口重新拼回一条，正好会顶破 Telegram
	// 的单条上限（4096 字符），表现是"整条消息发不出去"。告警事件不带它，
	// 抖动合并的行为一个字都不变。
	NoCoalesce bool
}

// Notifier 是唯一需要扩展的地方：以后加 Webhook / 邮件 / Bark 只需要实现这个接口。
type Notifier interface {
	Name() string
	Send(ctx context.Context, n Notification) error
}

// RetryAfterError 表示对端要求稍后重试（例如 Telegram 的 429）。
type RetryAfterError struct {
	After   time.Duration
	Message string
}

func (e *RetryAfterError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("对端要求 %s 后重试", e.After)
	}
	return fmt.Sprintf("对端要求 %s 后重试：%s", e.After, e.Message)
}

// Node 是评估规则需要的节点快照（全部来自内存状态 + 数据库配置）。
//
// 这里**只放规则真的会读的字段**：快照每秒钟重建一次，而"列表里有什么"看起来
// 随手就能多抄几个（曾经抄过 Connected / ObservedIP）—— 它们没有任何读取点，
// 只会让后来人以为告警侧也用那两个字段。
type Node struct {
	ID        int64
	Name      string
	GroupName string
	Region    string
	Status    string
	LastSeen  time.Time

	TrafficLimit   int64
	TrafficWarnPct int
	// TrafficBilling 是流量额度按哪个口径算（store.BillingBoth / BillingTxOnly /
	// BillingRxOnly）。它是配置项，由 internal/server 从节点配置抄进来。
	TrafficBilling int
	CycleRx        int64
	CycleTx        int64
	CycleStart     time.Time
	CycleEnd       time.Time

	ExpiresAt int64
}

// DisplayName 返回"名称（分组 · 地区）"这类便于一眼认出的写法。
//
// 为什么导出：定时流量报告在 internal/server 里渲染，而"这台机器叫什么"两处
// 必须是同一个写法 —— 告警写「hk-01（香港 · HK）」而报告只写「hk-01」时，
// 同一个名字前缀下有几台机器（hk-01 / hk-011）就分不清谁是谁，用户得回面板
// 一台台对。报告那一行因此会变长，但**不做截断**：截掉的后半截正是"哪台机器"
// 的信息（见 trafficReportRow 的注释）。
//
// 空分组/空地区不占位置；分组与名称相同时不重复写（前端把"分组"当分类用，
// 有用户会把它填成机器名）。
//
// 三个入参都先过一遍 sanitizeInline：告警正文是**多行**文本（displayName 之后
// 就是「最后通信：…」这些行），名字里的一个 \n 会让正文多出一行，把真话挤到
// 伪造行之下 —— 而节点名/分组/地区在接口与库层都没有字符集限制。报告侧早就在
// 这么做了（internal/server 的 singleLine），告警侧没有就是两套口径。
func DisplayName(name, group, region string) string {
	name = sanitizeInline(name)
	group = sanitizeInline(group)
	region = sanitizeInline(region)
	var extra []string
	if group != "" && group != name {
		extra = append(extra, group)
	}
	if region != "" {
		extra = append(extra, region)
	}
	if len(extra) == 0 {
		return name
	}
	return name + "（" + strings.Join(extra, " · ") + "）"
}

// sanitizeInline 把一段文本压成"能安全放进一行里"的形态：会断行的字符、以及能
// 改变显示顺序的双向控制符换成空格，并去掉首尾空白。
//
// 字符集与报告侧 internal/server 的 singleLine **逐字符一致**（两处不一致时，
// 同一台机器在报告里是一行、在告警里却能被拆成两行，那正是这条要修的毛病）：
//   - Cc：\n \r \t 与 U+0085 这类控制字符 —— 换行是唯一能真的多出一行的字符；
//   - Zl / Zp：U+2028 / U+2029，Unicode 意义上的行/段分隔符，它们**不是** Cc；
//   - Bidi_Control：U+202E 这类双向控制符，不换行但能把后面的字符显示成别的样子。
//
// 刻意**不**把整个 Cf（格式字符）都换掉：Cf 里有 U+200D 零宽连接符（emoji 序列
// 靠它拼合）、U+00AD 软连字符这类合法字符，一律抹掉会把正常名字改样（U+200B
// 零宽空格、U+FEFF 这类"看不见但能造成同形名"的残留风险见交付说明）。
//
// 正常名字（中文、emoji、组合字符）逐字节不变。
func sanitizeInline(s string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.In(r, unicode.Cc, unicode.Zl, unicode.Zp, unicode.Bidi_Control) {
			return ' '
		}
		return r
	}, s))
}

// displayName 是 DisplayName 在 Node 上的便捷写法（规则引擎内部用）。
func displayName(n Node) string {
	return DisplayName(n.Name, n.GroupName, n.Region)
}

// cycleUsagePct 返回本周期额度使用率（没有额度时返回 0）。
//
// "已用"按节点的**计费口径**算（双向 / 只算出站 / 只算入站）：预警说的是
// "离额度还有多远"，而额度是按那个口径结算的 —— 拿收 + 发去比一个只算流出的
// 额度，会在真实用量还不到一半时就报警。口径本身只有一处实现（store.BilledBytes），
// 面板与这里读的是同一个函数。
func cycleUsagePct(n Node) float64 {
	if n.TrafficLimit <= 0 {
		return 0
	}
	return float64(cycleUsedBytes(n)) / float64(n.TrafficLimit) * 100
}

// cycleUsedBytes 是本周期"按计费口径记进额度"的字节数（预警文案里的那个已用量）。
func cycleUsedBytes(n Node) int64 {
	return store.BilledBytes(n.TrafficBilling, n.CycleRx, n.CycleTx)
}

// FormatBytes 把字节数写成**十进制**单位（B/KB/MB/GB/TB/PB，1000 进制）。
//
// 它只服务流量：这些数字会直接进 Telegram 告警文案（「本周期已用：X / Y」），
// 而流量与硬盘是商家按 10 的幂卖的 —— 1 TB 额度 = 10¹² 字节，前端输入框
// 「月流量额度（GB）」也是 1 GB = 10⁹ 字节。写成 GiB/TiB 会与用户买的额度对不上账
// （"我买的是 2 TB，怎么显示 1.8 TiB？"），也会让"用了百分之多少"看着不对。
//
// 内存才是 1024 进制的特例（物理上是 2 的幂），但内存不进告警文案，见
// web/app.js 的 fmtBytesBin。注意 internal/config 的 FormatBytes 保持 1024 进制
// **不要改**：它回显的是用户自己在 --traffic-delta-max 里带单位写的配置（1TiB）。
//
// 小数位与前端**完全一致**（见 web/app.js 的 fmtScaledBytes）：B 不带小数；
// 10 以下两位（1.00 TB、3.00 GB），10 及以上一位（21.5 GB、908.0 GB）。
//
// 为什么要对齐到这种程度：告警说「已用 1.5 GB」而面板说「1.50 GB」时，
// 用户没法一眼确认说的是不是同一个数 —— 而这条告警的全部意义就是让他去面板上看。
// （早先这里用的是"100 以下一位、100 以上取整"，会出现 1.5 GB vs 1.50 GB 的差异。）
//
// 导出（而不是留在包内）是因为定时流量报告在 internal/server 里渲染，而
// "流量用哪套单位"必须只有一处实现：报告说 1.50 GB、面板说 1.5 GB 的时候，
// 用户同样没法把两份数对起来。它不依赖包内任何状态，导出没有任何副作用。
func FormatBytes(bytes int64) string {
	if bytes < 0 {
		bytes = 0
	}
	units := []string{"B", "KB", "MB", "GB", "TB", "PB"}
	value := float64(bytes)
	i := 0
	for value >= 1000 && i < len(units)-1 {
		value /= 1000
		i++
	}
	// 999999 字节按上面那条循环停在 999.999 KB；取整后会写成 "1000 KB"，
	// 所以先再升一级，宁可写 "1.00 MB"。
	if i > 0 && i < len(units)-1 && value >= 999.5 {
		value /= 1000
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%.0f %s", value, units[i])
	}
	if value < 10 {
		return fmt.Sprintf("%.2f %s", value, units[i])
	}
	return fmt.Sprintf("%.1f %s", value, units[i])
}

func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%d 秒", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%d 分钟", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%d 小时", int(d.Hours()))
	}
	return fmt.Sprintf("%d 天", int(d.Hours()/24))
}

// PctText1 把百分比写成"一位小数 + %"的字符串，规则与**前端**的
// Number.prototype.toFixed(1) 逐位一致：0.25 → "0.3%"、1.25 → "1.3%"、
// 85.25 → "85.3%"。
//
// 为什么不能用 Go 的 fmt / strconv（%.1f）：它们对"恰好落在 .x5 上"的二进制
// 精确值**取偶**（0.25 → "0.2%"、85.25 → "85.2%"），而浏览器的 toFixed 逢半
// 进一。差别只在那些值上出现，但它们是真实数据：月额度填 400 GB、用掉 341 GB
// 就是 85.25%。同一台机器在面板上写 85.3%、在告警里写 85.2%，正是要消灭的
// 那类不一致 —— 用户判据是"面板 / 日报 / 告警正文三处一模一样"。
//
// 它只服务百分比，而三个读百分比的地方（面板的 toFixed、定时流量日报、流量告警
// 正文）必须是同一个规则：日报与告警正文都调这里，**不许各自写一套取整**
// （各写一遍时总有一处会漏掉，与"已用量只有 store.BilledBytes 一处实现"同理）。
// 放在 internal/alert 而不是 internal/server，是因为告警引擎反过来不能引用
// internal/server（导入环）。
//
// 实现要点：**不许**先乘 10 再比较。0.15 的双精度值其实是
// 0.14999999999999999445（比 0.15 小），而 0.15×10 在浮点里会**四舍五入到
// 1.5**，于是"看小数第二位"变成"看一个假的半值"，给出 0.2%（浏览器给 0.1%）。
// 所以这里取 20 位**精确十进制展开**，只看它的第二位小数：>= 5 就进一 ——
// "恰好 .x5"与"后面还有非零数字"两种情况下浏览器都向上（toFixed 的
// ties-go-larger），所以一条判据就够。20 位足够：双精度值要么恰好等于 .x5，
// 要么离它至少半个 ulp（≈1e-16），远在 1e-20 之外。
//
// 定义域是 pct ≥ 0 的有限值 —— 调用方的输出恒在 [0, 999]。NaN / Inf /
// 负数（代码被改坏才可能出现）一律兜成 0.0%：把 "NaN%" 发到用户手机上更糟。
func PctText1(pct float64) string {
	if !(pct >= 0) || math.IsInf(pct, 0) {
		return "0.0%"
	}
	digits := strconv.FormatFloat(pct, 'f', 20, 64)
	dot := strings.IndexByte(digits, '.')
	if dot < 0 || len(digits) < dot+3 {
		return "0.0%" // 不可达：上面那一步永远给出 "整数.20 位小数"
	}
	// 整数部分 + 第一位小数，拼成一个"已放大 10 倍"的整数（"85" + "2" → 852）。
	n, err := strconv.ParseInt(digits[:dot]+digits[dot+1:dot+2], 10, 64)
	if err != nil {
		return "0.0%"
	}
	if digits[dot+2] >= '5' {
		n++
	}
	return strconv.FormatFloat(float64(n)/10, 'f', 1, 64) + "%"
}
