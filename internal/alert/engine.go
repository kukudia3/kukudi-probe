package alert

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// Params 是规则参数（默认值见 DefaultParams）。
type Params struct {
	// OfflineDebounce 是"连续离线多久"才算数，用于抵挡网络抖动。
	OfflineDebounce time.Duration
	// RecoverStable 是恢复后要稳定多久才发"已恢复"。
	RecoverStable time.Duration
	// NotifyCooldown 是同一规则重复通知的最短间隔（节点仍处于异常时）。
	NotifyCooldown time.Duration
	// TrafficRepeat 是流量超额后重复提醒的间隔（0 表示不重复）。
	TrafficRepeat time.Duration
	// ExpiryDays 是到期提醒的天数档位。
	ExpiryDays []int
	// StartupGrace 是服务端启动后不发通知的静默期。
	StartupGrace time.Duration
	// Loc 是告警文案里渲染时刻用的时区（--timezone）。
	//
	// 为什么必须显式给：告警里的每个时刻（最后通信、恢复时间、计费周期、到期日）
	// 都要与面板、定时报告同源。用进程本地时区时，进程跑在 UTC 而
	// --timezone=Asia/Shanghai 的话，同一件事在告警里写 06:07、在面板上写 14:07，
	// 用户没法把两条信息对上账（报告早就在标注时区了，见
	// internal/server 的 trafficReportNotifications）。
	//
	// nil 表示进程本地时区（time.Local），也就是这份配置没接上时的老行为。
	Loc *time.Location
}

// DefaultParams 返回默认参数（与 docs/DESIGN.md §12 一致）。
func DefaultParams() Params {
	return Params{
		OfflineDebounce: 2 * time.Second,
		RecoverStable:   30 * time.Second,
		NotifyCooldown:  30 * time.Minute,
		TrafficRepeat:   0,
		ExpiryDays:      []int{7, 3, 1},
		StartupGrace:    60 * time.Second,
	}
}

// State 是一条规则的持久化状态（对应 alert_state 表的一行）。
type State struct {
	NodeID     int64
	Rule       string
	State      string
	Since      time.Time
	LastNotify time.Time
	NotifyCnt  int
	Context    string
}

// Decision 是一次评估的结果。
//
// 只有状态真的变了（firing ↔ resolved）才会产生 Decision，
// 因此调用方可以直接把它写成一行 SQL，而不是每秒都写库。
type Decision struct {
	State State
	// Notify 为真时 Notification 才有意义。
	Notify       bool
	Notification Notification
}

type key struct {
	nodeID int64
	rule   string
}

// Engine 是规则引擎。
//
// 它只做判断与去重，不关心通知怎么发（那是 Dispatcher 的事），
// 也不直接碰数据库（状态由调用方在 Decision 里落盘）。
type Engine struct {
	params  Params
	started time.Time

	mu     sync.Mutex
	states map[key]*State
	// conditionSince 记录"当前规则条件"从什么时候开始连续成立（去抖用）。
	conditionSince map[key]time.Time
	// onlineSince 记录节点从什么时候开始持续在线（恢复确认用）。
	onlineSince map[int64]time.Time
}

// NewEngine 构造引擎；started 是服务端启动时刻（用于静默期）。
func NewEngine(params Params, started time.Time) *Engine {
	if len(params.ExpiryDays) == 0 {
		params.ExpiryDays = DefaultParams().ExpiryDays
	}
	// 升序排列：到期提醒"越接近越急"，先匹配最小档位（1 天 → 3 天 → 7 天）。
	days := make([]int, len(params.ExpiryDays))
	copy(days, params.ExpiryDays)
	sort.Ints(days)
	params.ExpiryDays = days

	return &Engine{
		params:         params,
		started:        started,
		states:         make(map[key]*State),
		conditionSince: make(map[key]time.Time),
		onlineSince:    make(map[int64]time.Time),
	}
}

// Load 载入已持久化的状态（服务端启动时调用）。
func (e *Engine) Load(states []State) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, s := range states {
		copied := s
		e.states[key{s.NodeID, s.Rule}] = &copied
	}
}

// SetParams 在运行期替换参数（管理员在设置页改告警阈值时调用）。
//
// 只换参数，不动已经触发的状态：改了阈值不会让"已经报过的告警"重新报一遍。
func (e *Engine) SetParams(params Params) {
	if len(params.ExpiryDays) == 0 {
		params.ExpiryDays = DefaultParams().ExpiryDays
	}
	days := make([]int, len(params.ExpiryDays))
	copy(days, params.ExpiryDays)
	sort.Ints(days)
	params.ExpiryDays = days

	e.mu.Lock()
	defer e.mu.Unlock()
	e.params = params
}

// Evaluate 评估所有节点，返回需要落盘/通知的变化。
func (e *Engine) Evaluate(now time.Time, nodes []Node) []Decision {
	e.mu.Lock()
	defer e.mu.Unlock()

	var out []Decision
	seen := make(map[int64]bool, len(nodes))

	for _, n := range nodes {
		seen[n.ID] = true
		out = append(out, e.evaluateOffline(n, now)...)
		out = append(out, e.evaluateTraffic(n, now)...)
		out = append(out, e.evaluateExpiry(n, now)...)
	}
	// 节点被删掉后，它的内存状态也该清掉（否则重启前一直占着）。
	for k := range e.states {
		if !seen[k.nodeID] {
			delete(e.states, k)
			delete(e.conditionSince, k)
		}
	}
	return out
}

// Silence 报告是否处于启动静默期。
func (e *Engine) Silence(now time.Time) bool {
	return now.Sub(e.started) < e.params.StartupGrace
}

// loc 返回渲染告警时间用的时区（没配置时退回进程本地时区）。
//
// 只在 Evaluate 的锁内被调用（e.params 会被 SetParams 整体替换）。
func (e *Engine) loc() *time.Location {
	if e.params.Loc == nil {
		return time.Local
	}
	return e.params.Loc
}

// zoneNote 返回时区标注，形如 "（Asia/Shanghai）"。
//
// 与定时报告的「统计区间：…（Asia/Shanghai）」同一套写法：消息里的时刻是按这个
// 时区渲染的，不写出来，在别的时区看消息的人只会觉得"时间不对"，而没法知道
// 该按哪个时区去读它。
func (e *Engine) zoneNote() string {
	return "（" + e.loc().String() + "）"
}

// trafficBody 渲染流量告警的正文（预警与超额共用一套文案）。
//
// 口径与面板、报告完全一致：字节一律走 FormatBytes（十进制、同一套小数位），
// 百分比一位小数（与前端 fmtPct1 相同，见 web/app.js）。不拆上下行 —— 这条通知
// 要说的是"离额度还有多远"，拆成两行反而要多做一次加法；要拆开看的人会去面板。
func (e *Engine) trafficBody(n Node, pct float64) string {
	loc := e.loc()
	return fmt.Sprintf("%s\n本周期已用：%s / %s（%.1f%%）\n计费周期：%s → %s%s",
		displayName(n), FormatBytes(n.CycleRx+n.CycleTx), FormatBytes(n.TrafficLimit), pct,
		n.CycleStart.In(loc).Format("2006-01-02"), n.CycleEnd.In(loc).Format("2006-01-02"),
		e.zoneNote())
}

func (e *Engine) evaluateOffline(n Node, now time.Time) []Decision {
	var out []Decision
	offlineKey := key{n.ID, RuleOffline}
	recKey := key{n.ID, RuleRecovered}

	if n.Status == "offline" {
		delete(e.onlineSince, n.ID)
		// 条件连续成立到达去抖时长后才算"真的离线"。
		since, ok := e.conditionSince[offlineKey]
		if !ok {
			e.conditionSince[offlineKey] = now
			return nil
		}
		if now.Sub(since) < e.params.OfflineDebounce {
			return nil
		}
		if d, changed := e.fire(offlineKey, now, "", func(prev *State) (Notification, bool) {
			if prev != nil && prev.State == StateFiring && now.Sub(prev.LastNotify) < e.params.NotifyCooldown {
				return Notification{}, false
			}
			if e.Silence(now) {
				return Notification{}, false
			}
			silent := now.Sub(n.LastSeen)
			loc := e.loc()
			return Notification{
				NodeID:   n.ID,
				NodeName: n.Name,
				Rule:     RuleOffline,
				Severity: SeverityCritical,
				Title:    "节点离线",
				// 「最后通信」只写时刻、由上一行的"多久之前"补足语义；
				// 两个时刻都按 --timezone 渲染，末尾标注时区名。
				Body: fmt.Sprintf("%s\n最后通信：%s（%s前）\n服务端时间：%s%s",
					displayName(n), n.LastSeen.In(loc).Format("15:04:05"), formatDuration(silent),
					now.In(loc).Format("2006-01-02 15:04:05"), e.zoneNote()),
				At: now,
			}, true
		}); changed {
			out = append(out, d)
		}
		return out
	}

	// 不再离线：清掉去抖状态。
	delete(e.conditionSince, offlineKey)

	// 恢复确认：必须持续在线一段时间，避免和老老实实掉线又反复横跳的机器互相刷屏。
	if n.Status == "online" {
		startedAt, ok := e.onlineSince[n.ID]
		if !ok {
			e.onlineSince[n.ID] = now
			startedAt = now
		}
		prev := e.states[offlineKey]
		if prev == nil || prev.State != StateFiring {
			return nil
		}
		if now.Sub(startedAt) < e.params.RecoverStable {
			return nil
		}
		if d, changed := e.fire(recKey, now, "", func(prevRec *State) (Notification, bool) {
			if prevRec != nil && prevRec.State == StateFiring && now.Sub(prevRec.LastNotify) < e.params.NotifyCooldown {
				return Notification{}, false
			}
			if e.Silence(now) {
				return Notification{}, false
			}
			offlineFor := time.Duration(0)
			if prev != nil && !prev.Since.IsZero() {
				offlineFor = now.Sub(prev.Since)
			}
			return Notification{
				NodeID:   n.ID,
				NodeName: n.Name,
				Rule:     RuleRecovered,
				Severity: SeverityInfo,
				Title:    "节点已恢复",
				Body: fmt.Sprintf("%s\n离线时长：约 %s\n恢复时间：%s%s",
					displayName(n), formatDuration(offlineFor),
					now.In(e.loc()).Format("2006-01-02 15:04:05"), e.zoneNote()),
				At: now,
			}, true
		}); changed {
			out = append(out, d)
			// 离线告警同时置为已恢复，避免下次离线被冷却挡住。
			if resolved, ok := e.resolve(offlineKey, now); ok {
				out = append(out, resolved)
			}
		}
	}
	return out
}

func (e *Engine) evaluateTraffic(n Node, now time.Time) []Decision {
	if n.TrafficLimit <= 0 {
		return nil
	}
	var out []Decision
	pct := cycleUsagePct(n)
	cycleTag := n.CycleStart.Format("2006-01-02")

	warnKey := key{n.ID, RuleTrafficWarn}
	if pct >= float64(n.TrafficWarnPct) {
		if d, changed := e.fire(warnKey, now, cycleTag, func(prev *State) (Notification, bool) {
			// 每个计费周期只提醒一次：周期变了就重新提醒。
			if prev != nil && prev.State == StateFiring && prev.Context == cycleTag {
				return Notification{}, false
			}
			if e.Silence(now) {
				return Notification{}, false
			}
			return Notification{
				NodeID:   n.ID,
				NodeName: n.Name,
				Rule:     RuleTrafficWarn,
				Severity: SeverityWarn,
				Title:    "流量接近额度",
				Body:     e.trafficBody(n, pct),
				At:       now,
			}, true
		}); changed {
			out = append(out, d)
		}
	}

	exceedKey := key{n.ID, RuleTrafficExceeded}
	if pct >= 100 {
		if d, changed := e.fire(exceedKey, now, cycleTag, func(prev *State) (Notification, bool) {
			if prev != nil && prev.State == StateFiring {
				if prev.Context == cycleTag && (e.params.TrafficRepeat <= 0 || now.Sub(prev.LastNotify) < e.params.TrafficRepeat) {
					return Notification{}, false
				}
			}
			if e.Silence(now) {
				return Notification{}, false
			}
			return Notification{
				NodeID:   n.ID,
				NodeName: n.Name,
				Rule:     RuleTrafficExceeded,
				Severity: SeverityCritical,
				Title:    "流量已超额",
				Body:     e.trafficBody(n, pct),
				At:       now,
			}, true
		}); changed {
			out = append(out, d)
		}
	} else if resolved, ok := e.resolve(exceedKey, now); ok {
		out = append(out, resolved)
	}
	return out
}

func (e *Engine) evaluateExpiry(n Node, now time.Time) []Decision {
	if n.ExpiresAt <= 0 {
		return nil
	}
	loc := e.loc()
	expires := time.Unix(n.ExpiresAt, 0).In(loc)
	// remaining 是两个绝对时刻之差，与 loc 无关；loc 只决定"哪个日历日"被写进文案。
	remaining := expires.Sub(now)
	// 天数按**绝对值**向下取整：没到期时是"还有几个整天"，过期后是"已经过去几个整天"。
	//
	// Go 的整数除法向零取整，对负数正好等于"按绝对值取整"，所以一行就够，
	// 不需要为负号单独写分支 —— 而这个负号正是老 bug 的根源：以前这里被
	// 强制写成 -1，于是过期 30 天也只显示「已过期 1 天」，读起来像"刚过期"。
	days := int(remaining / (24 * time.Hour))
	// 用 <= 0 而不是 < 0：现在这一秒正好等于到期时刻时，它已经到期了。
	expired := remaining <= 0

	bucket := ""
	if expired {
		// 过期只留一个档位：它已经是最急的状态，再按天数分档只会在用户不处理时
		// 每天提醒同一件事（续期后规则自动 resolve，见 evaluateExpiry 的调用方）。
		// 档位是**去重键**，不是显示值 —— 显示的天数每次都按真实值算（见 expiryWhen）。
		bucket = "expired"
	} else {
		for _, d := range e.params.ExpiryDays {
			if days <= d {
				bucket = fmt.Sprintf("%dd", d)
				break
			}
		}
	}

	expKey := key{n.ID, RuleExpiry}
	if bucket == "" {
		if resolved, ok := e.resolve(expKey, now); ok {
			return []Decision{resolved}
		}
		return nil
	}

	d, changed := e.fire(expKey, now, bucket, func(prev *State) (Notification, bool) {
		if prev != nil && prev.State == StateFiring && prev.Context == bucket {
			return Notification{}, false
		}
		if e.Silence(now) {
			return Notification{}, false
		}
		// 过期比流量超额更要紧：机器随时会被商家停掉（连带数据），而流量超额
		// 只是账单问题。所以已到期是 🔴，即将到期仍是 🟡。
		title, severity := "VPS 即将到期", SeverityWarn
		if expired {
			title, severity = "VPS 已到期", SeverityCritical
		}
		return Notification{
			NodeID:   n.ID,
			NodeName: n.Name,
			Rule:     RuleExpiry,
			Severity: severity,
			Title:    title,
			Body: fmt.Sprintf("%s\n到期时间：%s%s\n%s",
				displayName(n), expires.Format("2006-01-02"), e.zoneNote(),
				expiryWhen(remaining, days)),
			At: now,
		}, true
	})
	if !changed && !d.Notify {
		return nil
	}
	return []Decision{d}
}

// expiryWhen 把"离到期还有多久 / 已经过期多久"写成一句人话。
//
// 为什么不足一天不能写「剩余 0 天」：那读起来像"今天到期"，可能只是还有
// 23 小时，也可能只剩 5 分钟 —— 而这两个结论对应完全不同的行动。写成
// 「不足 1 天（约 5 小时）」之后，用户可以立刻判断要不要现在去续费。
//
// 为什么过期天数必须按真实值算：固定写「已过期 1 天」时，过期一个月的机器
// 看起来像"昨天刚过期，还来得及"，而它可能早就被商家停掉了。
func expiryWhen(remaining time.Duration, days int) string {
	if remaining <= 0 {
		if days == 0 {
			return "已过期不足 1 天" + shortDurationNote(-remaining)
		}
		return fmt.Sprintf("已过期 %d 天", -days)
	}
	if days == 0 {
		return "剩余不足 1 天" + shortDurationNote(remaining)
	}
	return fmt.Sprintf("剩余 %d 天", days)
}

// shortDurationNote 把不足一天的时长写成"（约 5 小时）"这样的补充说明。
//
// 说"约"是因为 formatDuration 是**向下取整**的：4 小时 59 分会被写成 4 小时。
// 向下取整是有意的（与"剩余 N 天"同一套口径：宁可说少，不让人以为还很久）。
//
// 不足一分钟时 formatDuration 会写"30 秒"，而刚到点的那一瞬间会写"0 秒"
// （读起来像没算出结果），统一说成"不到 1 分钟"。
func shortDurationNote(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	if d < time.Minute {
		return "（不到 1 分钟）"
	}
	return "（约 " + formatDuration(d) + "）"
}

// fire 把一条规则置为 firing；notify 回调决定是否真的要发通知（冷却/静默期在这里生效）。
//
// contextTag 是规则自己的"上下文"（例如流量周期起点、到期档位），用于判断
// "同一周期/同一档位内不要重复提醒"。
func (e *Engine) fire(k key, now time.Time, contextTag string, notify func(prev *State) (Notification, bool)) (Decision, bool) {
	prev := e.states[k]
	changed := prev == nil || prev.State != StateFiring

	notification, shouldNotify := notify(prev)

	state := &State{NodeID: k.nodeID, Rule: k.rule, State: StateFiring, Since: now, Context: contextTag}
	if prev != nil {
		state.Since = prev.Since
		if changed {
			state.Since = now
		}
		state.NotifyCnt = prev.NotifyCnt
		state.LastNotify = prev.LastNotify
	}
	if shouldNotify {
		state.LastNotify = now
		state.NotifyCnt++
	}
	e.states[k] = state

	if !changed && !shouldNotify {
		return Decision{}, false
	}
	return Decision{
		State:        *state,
		Notify:       shouldNotify,
		Notification: notification,
	}, changed || shouldNotify
}

// resolve 把一条规则置为 resolved；只有原本是 firing 才算变化。
func (e *Engine) resolve(k key, now time.Time) (Decision, bool) {
	prev := e.states[k]
	if prev == nil || prev.State != StateFiring {
		return Decision{}, false
	}
	state := &State{
		NodeID: k.nodeID, Rule: k.rule, State: StateResolved, Since: now,
		LastNotify: prev.LastNotify, NotifyCnt: prev.NotifyCnt,
	}
	e.states[k] = state
	delete(e.conditionSince, k)
	return Decision{State: *state}, true
}

// States 返回当前所有规则的持久化视图（用于落盘或诊断）。
func (e *Engine) States() []State {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]State, 0, len(e.states))
	for _, s := range e.states {
		out = append(out, *s)
	}
	return out
}
