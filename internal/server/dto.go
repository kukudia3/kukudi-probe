package server

import (
	"math"
	"time"

	"probe/internal/protocol"
	"probe/internal/state"
	"probe/internal/store"
)

// nodeDTO 是前端看到的节点视图：配置（来自数据库）+ 最新状态（来自内存）。
//
// 字段名与前端 JS 一一对应；不在这里做单位换算，前端负责显示格式。
type nodeDTO struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	GroupName   string `json:"group_name"`
	Region      string `json:"region"`
	Note        string `json:"note"`
	IntervalSec int    `json:"interval_sec"`
	Iface       string `json:"iface"`
	Enabled     bool   `json:"enabled"`
	// Tags 是节点标签（最多 8 个，每个最长 16 字）。
	// **空列表也必须序列化成 []**：JSON 里的 null 会让前端多一条"这里可能是空的"
	// 判断，而标签行的显隐本来只看长度 —— 少一条分支就少一处能在浏览器里才发现的坑。
	Tags []string `json:"tags"`

	Status    string `json:"status"`
	Connected bool   `json:"connected"`
	LastSeen  int64  `json:"last_seen"`

	CPUPct  float64 `json:"cpu_pct"`
	MemPct  float64 `json:"mem_pct"`
	SwapPct float64 `json:"swap_pct"`
	DiskPct float64 `json:"disk_pct"`
	Load1   float64 `json:"load1"`
	// Load5 / Load15 与 Load1 同源（/proc/loadavg 的三个数），卡片上要一起显示：
	// 只看 1 分钟负载分不清"刚刚抖了一下"与"已经压了半小时"。
	Load5  float64 `json:"load5"`
	Load15 float64 `json:"load15"`
	// MemUsed / MemTotal 是内存的绝对值（字节）。百分比 mem_pct 已经由 Agent 算好，
	// 但卡片要写「243.2 MB / 967.3 MB」这种"已用 / 总量"，缺了这两个数就只能拿
	// 百分比去反推总量 —— 那是前端在做算术，而且推出来的值与事实差一截。
	//
	// 硬盘不走这里：一个节点可能上报多个挂载点（详情页要用整个数组），
	// 由前端自己挑根挂载点（disks 已经在 DTO 里，见 buildNodeDTO 的 Disks）。
	MemUsed   uint64  `json:"mem_used"`
	MemTotal  uint64  `json:"mem_total"`
	LatMS     float64 `json:"lat_ms"`
	UptimeSec uint64  `json:"uptime_sec"`
	// OnlineSec 是**连续在线时长**（秒）：从最近一次进入 online 状态算起，
	// 一旦变成 stale/offline/unknown 就归零（见 online.go）。
	// 它不是开机时长（uptime_sec），两者经常差很多，前端也是分开显示的两行。
	// 不在线时恒为 0，前端据此显示 —（0 秒会被读成"刚上线"）。
	OnlineSec uint64 `json:"online_sec"`

	RxRate  float64 `json:"rx_rate"`
	TxRate  float64 `json:"tx_rate"`
	RxTotal uint64  `json:"rx_total"`
	TxTotal uint64  `json:"tx_total"`

	OSName     string `json:"os_name"`
	Kernel     string `json:"kernel"`
	CPUModel   string `json:"cpu_model"`
	CPUCores   int    `json:"cpu_cores"`
	ObservedIP string `json:"observed_ip"`
	// LocalIP / LocalIP6 是 Agent 自报的本机地址（可为空）。
	// 与 ObservedIP 并列返回而不是在服务端合并成一个字符串：前端才知道
	// 该怎么显示（两个都有要拼成 "v4 / v6"），服务端不做展示层的拼接。
	LocalIP      string `json:"local_ip"`
	LocalIP6     string `json:"local_ip6"`
	AgentVersion string `json:"agent_version"`
	BootID       string `json:"boot_id"`

	Disks   []protocol.Disk `json:"disks"`
	Gap     uint64          `json:"gap"`
	Dropped uint64          `json:"dropped"`

	TrafficLimit   int64 `json:"traffic_limit"`
	TrafficWarnPct int   `json:"traffic_warn_pct"`
	ResetDay       int   `json:"reset_day"`
	ExpiresAt      int64 `json:"expires_at"`

	// 价格：三个原始字段来自数据库，三个派生字段只在这里算（不入库）。
	// 派生值不落库是刻意的：它们随时间变化（剩余天数每天都在变），
	// 存下来就会和事实脱节，还要额外的定时任务去刷新。
	PriceCents          int64  `json:"price_cents"`
	Currency            string `json:"currency"`
	BillingMonths       int    `json:"billing_months"`
	MonthlyCents        int64  `json:"monthly_cents"`
	RemainingValueCents int64  `json:"remaining_value_cents"`
	RemainingDays       int64  `json:"remaining_days"`

	// 流量（由 traffic_daily 汇总，单位字节）。
	TrafficTodayRx int64   `json:"traffic_today_rx"`
	TrafficTodayTx int64   `json:"traffic_today_tx"`
	TrafficCycleRx int64   `json:"traffic_cycle_rx"`
	TrafficCycleTx int64   `json:"traffic_cycle_tx"`
	TrafficTotalRx int64   `json:"traffic_total_rx"`
	TrafficTotalTx int64   `json:"traffic_total_tx"`
	TrafficPct     float64 `json:"traffic_pct"`
	CycleStart     string  `json:"cycle_start"`
	CycleEnd       string  `json:"cycle_end"`
}

// stateSummary 是首页顶部的集群汇总。
type stateSummary struct {
	Total   int `json:"total"`
	Online  int `json:"online"`
	Stale   int `json:"stale"`
	Offline int `json:"offline"`
	Unknown int `json:"unknown"`
}

// statusOf 是节点状态判定的唯一入口。
//
// 抽出来是为了让"算状态"与"按状态记连续在线时长"用的是同一套判定（后者见
// online.go 的 observe）：两处各写一遍迟早分叉，表现就是卡片上写着「在线」
// 而「在线」那一行显示 —。
func statusOf(lastSeen time.Time, hasState bool, now time.Time, staleAfter, offlineAfter time.Duration) state.Status {
	if !hasState {
		// 内存里没有这个节点的状态（从未连接、或服务端刚重启还没恢复）：
		// 是"未知"而不是"离线" —— 离线意味着"连接过、然后掉了"。
		return state.StatusUnknown
	}
	return state.StatusFor(lastSeen, now, staleAfter, offlineAfter)
}

// dtoFor 组装一个节点的前端视图，并顺带推进「连续在线时长」的记账。
//
// 为什么要包一层而不是直接调 buildNodeDTO：在线时长要跟着**状态**走，而状态是
// buildNodeDTO 现算的（见 statusOf）。这一层把"算状态 → 记起点 → 填字段"串成一步，
// 所有对外路径（/nodes、SSE 快照、详情、总览、创建/修改的响应）就都不会漏记。
func (s *Server) dtoFor(node store.Node, st state.Node, hasState bool, now time.Time) nodeDTO {
	dto := buildNodeDTO(node, st, hasState, now, s.cfg.StaleAfter, s.cfg.OfflineAfter)
	dto.OnlineSec = s.online.observe(node.ID, state.Status(dto.Status), now)
	return dto
}

// buildNodeDTO 把数据库里的配置与内存里的最新状态合成前端视图。
func buildNodeDTO(node store.Node, st state.Node, hasState bool, now time.Time, staleAfter, offlineAfter time.Duration) nodeDTO {
	// 标签为 nil（老数据、或直接构造的 store.Node）时补成空切片：
	// 序列化出来必须是 []，不能是 null（见 nodeDTO.Tags）。
	tags := node.Tags
	if tags == nil {
		tags = []string{}
	}
	dto := nodeDTO{
		ID:             node.ID,
		Name:           node.Name,
		GroupName:      node.GroupName,
		Region:         node.Region,
		Note:           node.Note,
		IntervalSec:    node.IntervalSec,
		Iface:          node.Iface,
		Enabled:        node.Enabled,
		Tags:           tags,
		Status:         string(state.StatusUnknown),
		TrafficLimit:   node.TrafficLimit,
		TrafficWarnPct: node.TrafficWarnPct,
		ResetDay:       node.ResetDay,
		ExpiresAt:      node.ExpiresAt,
		PriceCents:     node.PriceCents,
		Currency:       node.Currency,
		BillingMonths:  node.BillingMonths,
	}
	applyPricing(&dto, now)
	if !hasState {
		return dto
	}

	dto.Status = string(statusOf(st.LastSeen, hasState, now, staleAfter, offlineAfter))
	dto.Connected = st.Connected
	dto.LastSeen = st.LastSeen.Unix()
	dto.CPUPct = st.Metrics.CPUPct
	dto.MemPct = st.Metrics.Mem.Pct
	dto.MemUsed = st.Metrics.Mem.Used
	dto.MemTotal = st.Metrics.Mem.Total
	dto.SwapPct = st.Metrics.Swap.Pct
	dto.Load1 = st.Metrics.Load.L1
	dto.Load5 = st.Metrics.Load.L5
	dto.Load15 = st.Metrics.Load.L15
	dto.LatMS = st.Metrics.LatMS
	dto.UptimeSec = st.Metrics.UptimeSec
	dto.RxRate = st.Metrics.Net.RxRate
	dto.TxRate = st.Metrics.Net.TxRate
	dto.RxTotal = st.Metrics.Net.RxTotal
	dto.TxTotal = st.Metrics.Net.TxTotal
	dto.Disks = st.Metrics.Disk
	dto.Gap = st.Gap
	dto.Dropped = st.Metrics.Dropped
	dto.BootID = st.Metrics.Net.BootID
	if len(st.Metrics.Disk) > 0 {
		dto.DiskPct = st.Metrics.Disk[0].Pct
	}

	dto.OSName = st.Info.OS.Name
	dto.Kernel = st.Info.OS.Kernel
	dto.CPUModel = st.Info.CPU.Model
	dto.CPUCores = st.Info.CPU.Cores
	dto.ObservedIP = st.ObservedIP
	dto.LocalIP = st.LocalIP
	dto.LocalIP6 = st.LocalIP6
	dto.AgentVersion = st.Info.AgentVersion
	if dto.Iface == "" {
		dto.Iface = st.Metrics.Net.Iface
	}
	return dto
}

// daysPerMonth 是"一个月按多少天"的约定值：30.4375 = 365.25 / 12。
//
// 用平均值而不是自然月天数，是为了让剩余价值按天线性摊销：按当月天数算的话，
// 2 月（28 天）的每份价值会比 1 月贵 10%，同一台机器换个日子看就换了价钱。
const daysPerMonth = 30.4375

// applyPricing 计算价格相关的派生字段（只在展示层用，不落库）。
//
// 月均一律**向下取整**：金额以"分"为单位存整数，向下取整能保证
// "月均 × 月数"永远不超过实际付掉的钱 —— 报多了在对账时最扎眼，
// 少算几分钱则没人会计较。
func applyPricing(dto *nodeDTO, now time.Time) {
	if dto.BillingMonths > 0 {
		dto.MonthlyCents = dto.PriceCents / int64(dto.BillingMonths)
	}
	if dto.ExpiresAt > 0 {
		// 已过期的节点剩余天数取 0（而不是负数）：负数传到前端会显示成"-3 天"，
		// 而且剩余价值也得跟着变成负的，看起来像倒欠钱。
		dto.RemainingDays = (dto.ExpiresAt - now.Unix()) / 86400
		if dto.RemainingDays < 0 {
			dto.RemainingDays = 0
		}
	}
	if dto.MonthlyCents > 0 && dto.RemainingDays > 0 {
		dto.RemainingValueCents = int64(math.Round(
			float64(dto.MonthlyCents) * float64(dto.RemainingDays) / daysPerMonth))
	}
}

// applyTraffic 把流量汇总写进 DTO，并算出额度使用率。
//
// cycle_start / cycle_end 用服务器本地时区格式化（与计费周期的定义一致），
// 所以必须传 loc，不能图省事用 UTC。
func applyTraffic(dto *nodeDTO, agg trafficAgg, loc *time.Location) {
	dto.TrafficTodayRx = agg.TodayRx
	dto.TrafficTodayTx = agg.TodayTx
	dto.TrafficCycleRx = agg.CycleRx
	dto.TrafficCycleTx = agg.CycleTx
	dto.TrafficTotalRx = agg.TotalRx
	dto.TrafficTotalTx = agg.TotalTx
	if !agg.CycleStart.IsZero() {
		dto.CycleStart = agg.CycleStart.In(loc).Format("2006-01-02")
		dto.CycleEnd = agg.CycleEnd.In(loc).Format("2006-01-02")
	}
	if dto.TrafficLimit > 0 {
		used := float64(agg.CycleRx + agg.CycleTx)
		dto.TrafficPct = used / float64(dto.TrafficLimit) * 100
		if dto.TrafficPct > 999 {
			dto.TrafficPct = 999
		}
	}
}

func summarize(nodes []nodeDTO) stateSummary {
	var s stateSummary
	s.Total = len(nodes)
	for _, n := range nodes {
		switch state.Status(n.Status) {
		case state.StatusOnline:
			s.Online++
		case state.StatusStale:
			s.Stale++
		case state.StatusOffline:
			s.Offline++
		default:
			s.Unknown++
		}
	}
	return s
}
