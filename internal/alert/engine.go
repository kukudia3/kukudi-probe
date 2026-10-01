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
			return Notification{
				NodeID:   n.ID,
				NodeName: n.Name,
				Rule:     RuleOffline,
				Severity: SeverityCritical,
				Title:    "节点离线",
				Body: fmt.Sprintf("%s\n最后通信：%s（%s前）\n服务端时间：%s",
					displayName(n), n.LastSeen.Format("15:04:05"), formatDuration(silent),
					now.Format("2006-01-02 15:04:05")),
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
				Body: fmt.Sprintf("%s\n离线时长：约 %s\n恢复时间：%s",
					displayName(n), formatDuration(offlineFor), now.Format("2006-01-02 15:04:05")),
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
				Body: fmt.Sprintf("%s\n本周期已用：%s / %s（%.1f%%）\n计费周期：%s → %s",
					displayName(n), FormatBytes(n.CycleRx+n.CycleTx), FormatBytes(n.TrafficLimit), pct,
					n.CycleStart.Format("2006-01-02"), n.CycleEnd.Format("2006-01-02")),
				At: now,
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
				Body: fmt.Sprintf("%s\n本周期已用：%s / %s（%.1f%%）\n计费周期：%s → %s",
					displayName(n), FormatBytes(n.CycleRx+n.CycleTx), FormatBytes(n.TrafficLimit), pct,
					n.CycleStart.Format("2006-01-02"), n.CycleEnd.Format("2006-01-02")),
				At: now,
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
	expires := time.Unix(n.ExpiresAt, 0)
	remaining := expires.Sub(now)
	days := int(remaining.Hours() / 24)
	if remaining < 0 {
		days = -1
	}

	bucket := ""
	for _, d := range e.params.ExpiryDays {
		if days >= 0 && days <= d {
			bucket = fmt.Sprintf("%dd", d)
			break
		}
	}
	if bucket == "" && days < 0 {
		bucket = "expired"
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
		title := "VPS 即将到期"
		when := fmt.Sprintf("剩余 %d 天", days)
		if days < 0 {
			title = "VPS 已到期"
			when = fmt.Sprintf("已过期 %d 天", -days)
		}
		return Notification{
			NodeID:   n.ID,
			NodeName: n.Name,
			Rule:     RuleExpiry,
			Severity: SeverityWarn,
			Title:    title,
			Body: fmt.Sprintf("%s\n到期时间：%s\n%s",
				displayName(n), expires.Format("2006-01-02"), when),
			At: now,
		}, true
	})
	if !changed && !d.Notify {
		return nil
	}
	return []Decision{d}
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
