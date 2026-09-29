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
	"strings"
	"time"
)

// 规则名（同时是 alert_state 里的 rule 字段）。
const (
	RuleOffline         = "offline"
	RuleRecovered       = "recovered"
	RuleTrafficWarn     = "traffic_warn"
	RuleTrafficExceeded = "traffic_exceeded"
	RuleExpiry          = "expiry"
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
type Node struct {
	ID         int64
	Name       string
	GroupName  string
	Region     string
	Status     string
	LastSeen   time.Time
	Connected  bool
	ObservedIP string

	TrafficLimit   int64
	TrafficWarnPct int
	CycleRx        int64
	CycleTx        int64
	CycleStart     time.Time
	CycleEnd       time.Time

	ExpiresAt int64
}

// displayName 返回"名称（分组 · 地区）"这类便于一眼认出的写法。
func displayName(n Node) string {
	var extra []string
	if n.GroupName != "" && n.GroupName != n.Name {
		extra = append(extra, n.GroupName)
	}
	if n.Region != "" {
		extra = append(extra, n.Region)
	}
	if len(extra) == 0 {
		return n.Name
	}
	return n.Name + "（" + strings.Join(extra, " · ") + "）"
}

// cycleUsagePct 返回本周期额度使用率（没有额度时返回 0）。
func cycleUsagePct(n Node) float64 {
	if n.TrafficLimit <= 0 {
		return 0
	}
	return float64(n.CycleRx+n.CycleTx) / float64(n.TrafficLimit) * 100
}

func formatBytes(bytes int64) string {
	if bytes < 0 {
		bytes = 0
	}
	units := []string{"B", "KiB", "MiB", "GiB", "TiB", "PiB"}
	value := float64(bytes)
	i := 0
	for value >= 1024 && i < len(units)-1 {
		value /= 1024
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
