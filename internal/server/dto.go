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

	Status    string `json:"status"`
	Connected bool   `json:"connected"`
	LastSeen  int64  `json:"last_seen"`

	CPUPct    float64 `json:"cpu_pct"`
	MemPct    float64 `json:"mem_pct"`
	SwapPct   float64 `json:"swap_pct"`
	DiskPct   float64 `json:"disk_pct"`
	Load1     float64 `json:"load1"`
	LatMS     float64 `json:"lat_ms"`
	UptimeSec uint64  `json:"uptime_sec"`

	RxRate  float64 `json:"rx_rate"`
	TxRate  float64 `json:"tx_rate"`
	RxTotal uint64  `json:"rx_total"`
	TxTotal uint64  `json:"tx_total"`

	OSName       string `json:"os_name"`
	Kernel       string `json:"kernel"`
	CPUModel     string `json:"cpu_model"`
	CPUCores     int    `json:"cpu_cores"`
	ObservedIP   string `json:"observed_ip"`
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

// buildNodeDTO 把数据库里的配置与内存里的最新状态合成前端视图。
func buildNodeDTO(node store.Node, st state.Node, hasState bool, now time.Time, staleAfter, offlineAfter time.Duration) nodeDTO {
	dto := nodeDTO{
		ID:             node.ID,
		Name:           node.Name,
		GroupName:      node.GroupName,
		Region:         node.Region,
		Note:           node.Note,
		IntervalSec:    node.IntervalSec,
		Iface:          node.Iface,
		Enabled:        node.Enabled,
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

	dto.Status = string(state.StatusFor(st.LastSeen, now, staleAfter, offlineAfter))
	dto.Connected = st.Connected
	dto.LastSeen = st.LastSeen.Unix()
	dto.CPUPct = st.Metrics.CPUPct
	dto.MemPct = st.Metrics.Mem.Pct
	dto.SwapPct = st.Metrics.Swap.Pct
	dto.Load1 = st.Metrics.Load.L1
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
