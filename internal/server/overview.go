package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"probe/internal/protocol"
	"probe/internal/state"
	"probe/internal/store"
)

// 总览接口的默认窗口与段数（前端不做算术：窗口、段数、桶宽全部由后端下发，
// 首页只要把 buckets 个格子原样铺出来 —— 以后想改成 24 段也不用动前端）。
const (
	overviewWindowDefault  = time.Hour
	overviewBucketsDefault = 10
	// overviewBucketsMax 是段数上限。它挡的不是画布（格子再多也画得下），
	// 而是"每段不足一秒"这种没有意义的请求：桶宽会被压成 0，
	// 一组 ts 挤进同一格，图上看起来跟 1 段没区别。
	overviewBucketsMax = 3600
	// overviewWindowMax 是窗口上限（7 天 = ping_samples_1m 的保留期上限）。
	overviewWindowMax = 7 * 24 * time.Hour
)

// overviewTotals 是"所有机器加起来"的合计。
//
// 口径（每一项都不一样，改之前先看清）：
//   - 内存 / 硬盘 / 速率：只统计**在线**节点。离线节点的最后一份快照可能是几小时前的，
//     把它算进"实时用量"等于把一台已经关机的机器算成还在跑。
//   - 累计流量：**所有**节点，而且取数据库里的历史累计（traffic_daily 求和），
//     不是 Agent 的实时快照 —— 后者在 Agent 重装后会归零，用它当"历史累计"
//     会让总数凭空缩水。
//   - 剩余价值：按币种分组，绝不跨币种相加（见 overviewCurrency）。
type overviewTotals struct {
	NodesTotal  int `json:"nodes_total"`
	NodesOnline int `json:"nodes_online"`

	MemUsed  uint64 `json:"mem_used"`
	MemTotal uint64 `json:"mem_total"`
	// MemPct / DiskPct 是派生值，由服务端算好：百分比属于"聚合值"，
	// 前端不做算术是本项目的既有原则（见 api_series.go 的注释）。
	MemPct float64 `json:"mem_pct"`

	DiskUsed  uint64  `json:"disk_used"`
	DiskTotal uint64  `json:"disk_total"`
	DiskPct   float64 `json:"disk_pct"`

	// RxRate 是下行、TxRate 是上行 —— 与节点卡片上的 ↑/↓ 完全同一套口径
	// （见 app.js 的 updateCard：'↑ ' + tx_rate + ' ↓ ' + rx_rate）。
	// 方向搞反了页面一样"能显示"，只是所有数字都反着，所以这里写死一遍。
	RxRate float64 `json:"rx_rate"`
	TxRate float64 `json:"tx_rate"`

	TrafficRxTotal int64 `json:"traffic_rx_total"`
	TrafficTxTotal int64 `json:"traffic_tx_total"`

	// RemainingValue 按币种分组，人民币与美元各占一项。
	// 只有一种币种时数组里就一项，前端照样一行一行渲染（多币种时天然分行）。
	RemainingValue []overviewCurrency `json:"remaining_value"`
}

// overviewCurrency 是一种币种的剩余价值合计。
//
// 为什么不加一个"折合人民币"的总额：换算需要汇率，而汇率既不是本项目的输入、
// 也不该由探针去猜。￥1000 与 $1000 相加得到的 2000 没有任何意义。
type overviewCurrency struct {
	Currency string `json:"currency"`
	Cents    int64  `json:"cents"`
}

// handleOverview 返回首页总览区与节点卡片迷你条要的全部数据。
//
// 一次请求覆盖两块 UI：合计（totals）与"每节点最近一小时的探测分桶"（nodes）。
// 它们的数据源与刷新节奏完全一样（都是分钟级），拆成两个接口只会让首页多发一次请求、
// 还多一处"两边对不上"的可能。
func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	window, buckets, ok := s.overviewParams(w, r)
	if !ok {
		return
	}

	windowSec := int64(window.Seconds())
	bucketSec := windowSec / int64(buckets)
	if bucketSec <= 0 {
		s.badRequest(w, fmt.Errorf("窗口 %s 分成 %d 段后每段不足一秒", window, buckets))
		return
	}
	// 窗口长度对齐成 bucketSec × buckets（而不是用请求里的秒数）：
	// 桶号是 (ts-start)/bucketSec，只有长度正好是整数倍，桶号才保证落在 [0, buckets)。
	// 代价是窗口最多缩短 buckets-1 秒，对分钟级的数据没有影响。
	windowSec = bucketSec * int64(buckets)
	end := time.Now().Unix()
	end -= end % bucketSec // 与 PingRange.window 同样的理由：对齐了每段才是完整的一段
	start := end - windowSec

	ctx := r.Context()
	nodes, err := s.db.ListNodes(ctx)
	if err != nil {
		s.log.Error("查询节点列表失败", "err", err)
		s.writeJSON(w, http.StatusInternalServerError, errorEnvelope{Error: apiError{
			Code: "internal", Message: "服务端内部错误"}})
		return
	}

	totals := s.overviewTotals(ctx, nodes)
	pings := s.overviewPings(ctx, start, end, bucketSec, buckets)

	s.writeJSON(w, http.StatusOK, map[string]any{
		"window_sec": windowSec,
		"buckets":    buckets,
		"bucket_sec": bucketSec,
		// bucket_ts 是每一格的**起始** Unix 秒（升序，最后一格是最近的一段）。
		//
		// 为什么由服务端给：桶号是 (ts-start)/bucketSec，而 end 已经向下对齐到
		// bucketSec（见上面）。前端若自己用"现在 − 窗口 + i×桶宽"去推边界，会与
		// 真实桶错开最多一整格 —— 悬停浮层上写的时间段就不再是那一格数据的实际
		// 区间；何况在当前时间上做窗口运算也属于前端不该做的算术。
		"bucket_ts": overviewBucketTS(start, bucketSec, buckets),
		"totals":    totals,
		// nodes 一定是对象（哪怕是空的），不能是 null：前端拿到 null 会去读它的属性。
		"nodes": pings,
	})
}

// overviewBucketTS 给出每一格的起始 Unix 秒（升序）。
//
// 第 i 格覆盖 [start+i×bucketSec, start+(i+1)×bucketSec)，与 store.QueryOverviewPing
// 里算桶号的 (ts-start)/bucketSec 是同一套边界 —— 两处一旦分叉，前端悬停时显示的
// 时间段就会和格子里那个数字对不上，而这种错位在页面上完全看不出来。
func overviewBucketTS(start, bucketSec int64, buckets int) []int64 {
	out := make([]int64, buckets)
	for i := range out {
		out[i] = start + int64(i)*bucketSec
	}
	return out
}

// overviewParams 解析 window / buckets 两个查询参数。
func (s *Server) overviewParams(w http.ResponseWriter, r *http.Request) (time.Duration, int, bool) {
	query := r.URL.Query()

	window := overviewWindowDefault
	if raw := query.Get("window"); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil || parsed < time.Minute || parsed > overviewWindowMax {
			s.badRequest(w, errors.New("window 必须是 1m-168h 之间的时长（如 1h）"))
			return 0, 0, false
		}
		window = parsed
	}

	buckets := overviewBucketsDefault
	if raw := query.Get("buckets"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > overviewBucketsMax {
			s.badRequest(w, fmt.Errorf("buckets 必须是 1-%d 之间的整数", overviewBucketsMax))
			return 0, 0, false
		}
		buckets = parsed
	}
	return window, buckets, true
}

// overviewTotals 汇总所有节点。
//
// 查询次数：节点列表 1 次 + 流量汇总 2 次（按分钟缓存，见 trafficAggregates），
// 与节点数无关 —— 不会随节点变多退化成 N+1。
func (s *Server) overviewTotals(ctx context.Context, nodes []store.Node) overviewTotals {
	now := time.Now()
	totals := overviewTotals{
		NodesTotal:     len(nodes),
		RemainingValue: []overviewCurrency{},
	}

	// 流量汇总失败不该让整个总览消失：其它数字照常显示，流量显示为 0。
	aggs, err := s.trafficAggregates(ctx, nodes, now)
	if err != nil {
		s.log.Warn("总览的流量汇总失败", "err", err)
		aggs = map[int64]trafficAgg{}
	}

	// 剩余价值按币种累加（币种 -> 分）：同一个币种的节点相加，跨币种绝不相加。
	byCurrency := make(map[string]int64)

	for _, n := range nodes {
		st, hasState := s.state.Get(n.ID)
		dto := s.dtoFor(n, st, hasState, now)

		// 累计流量对所有节点求和（在线的、离线的都算）：它是历史账，
		// 机器关了不代表用掉的流量不存在。
		totals.TrafficRxTotal += aggs[n.ID].TotalRx
		totals.TrafficTxTotal += aggs[n.ID].TotalTx

		// 没有价格的节点不参与剩余价值（否则会多出一行"空币种"的 0 元）。
		// 有价格但已过期的节点仍然参与：它的剩余价值就是 0，这是事实。
		if dto.PriceCents > 0 {
			byCurrency[dto.Currency] += dto.RemainingValueCents
		}

		// 下面三项只算在线节点（口径见 overviewTotals 的注释）。
		if dto.Status != string(state.StatusOnline) {
			continue
		}
		totals.NodesOnline++
		totals.MemUsed += st.Metrics.Mem.Used
		totals.MemTotal += st.Metrics.Mem.Total
		// 硬盘只取根挂载点，见 rootDisk 的注释。
		if disk, ok := rootDisk(st.Metrics.Disk); ok {
			totals.DiskUsed += disk.Used
			totals.DiskTotal += disk.Total
		}
		totals.RxRate += st.Metrics.Net.RxRate
		totals.TxRate += st.Metrics.Net.TxRate
	}

	totals.MemPct = percentOf(totals.MemUsed, totals.MemTotal)
	totals.DiskPct = percentOf(totals.DiskUsed, totals.DiskTotal)

	currencies := make([]string, 0, len(byCurrency))
	for code := range byCurrency {
		currencies = append(currencies, code)
	}
	// 排序让响应稳定（前端每次刷新看到的是同一个顺序，测试也不必靠 map 的随机序）。
	sort.Strings(currencies)
	for _, code := range currencies {
		totals.RemainingValue = append(totals.RemainingValue, overviewCurrency{
			Currency: code,
			Cents:    byCurrency[code],
		})
	}
	return totals
}

// rootDisk 从节点上报的挂载点里挑出**根挂载点**。
//
// 为什么集群硬盘总量只算 /：Agent 会把 /、/var/lib/probe-agent、/var/tmp 之类的挂载点
// 全都上报，而它们中有很多其实是**同一个文件系统**（同一分区的多个挂载点、bind mount、
// overlay 的上层）。把每个挂载点的 Used/Total 都加一遍，同一块盘就被算了好几遍 ——
// 数字变大但并不荒谬，只有拿 df 对一遍才会发现。根挂载点每个节点有且只有一个，
// 是唯一不会重复的口径。
//
// 找不到 / 时退回列表第一项：protocol.Disk 约定第一项是 --disk 指定的主文件系统，
// 容器或老 Agent 上报的可能是别的路径。列表为空（节点刚建、还没上报）时返回 false，
// 这个节点不参与合计。
func rootDisk(disks []protocol.Disk) (protocol.Disk, bool) {
	if len(disks) == 0 {
		return protocol.Disk{}, false
	}
	for _, d := range disks {
		if d.Mount == "/" {
			return d, true
		}
	}
	return disks[0], true
}

// percentOf 算百分比；分母为 0（一个在线节点都没有）时返回 0。
func percentOf(used, total uint64) float64 {
	if total == 0 {
		return 0
	}
	return float64(used) / float64(total) * 100
}

// overviewPings 取回所有节点的探测分桶与分目标聚合。
//
// 一个探测目标都没配时**直接不查库**并返回空 map：此时任何节点都不会有新的探测结果，
// 库里残留的历史也不该再画 —— 用户刚把目标全删掉，首页却还挂着 10 个格子，
// 会让人以为删除没生效（前端据此把整块迷你条藏起来，见 app.js 的 renderMiniBar）。
//
// 目标的名字（label/host）来自配置，探测结果里只有 target_id；这里把两者合上，
// 并**按配置顺序**排好 targets：前端按顺序取色与显示，与详情页延迟图的图例一致。
// 库里某个目标没有数据（或目标已被删掉、只剩历史）时它照样出现在数组里，
// 只是 has_data=false，前端画成 —。
func (s *Server) overviewPings(ctx context.Context, start, end, bucketSec int64, buckets int) map[int64]store.OverviewPing {
	targets, err := s.db.PingTargets(ctx)
	if err != nil {
		s.log.Warn("读取探测目标失败，总览的延迟数据按「未配置」处理", "err", err)
		return map[int64]store.OverviewPing{}
	}
	if len(targets) == 0 {
		return map[int64]store.OverviewPing{}
	}

	pings, err := s.db.QueryOverviewPing(ctx, start, end, bucketSec, buckets)
	if err != nil {
		// 只降级迷你条，不让整个总览 500：合计（内存/硬盘/流量/金额）与探测无关，
		// 为了一个附加区块把它们一起丢掉不划算。
		s.log.Error("查询总览探测数据失败", "err", err)
		return map[int64]store.OverviewPing{}
	}
	for id, p := range pings {
		pings[id] = orderOverviewTargets(p, targets)
	}
	return pings
}

// orderOverviewTargets 把"按目标聚合"的结果整理成前端要的形状。
//
//   - 顺序按**配置顺序**（不是 id、也不是库里的返回顺序）：详情页的延迟图就是按
//     这个顺序取色的，两处顺序不一致时同一个目标在首页与详情页会是两种颜色；
//   - 配置里没有的目标（已被删除，但窗口内还有历史数据）直接丢掉：用户删掉的东西
//     不该在首页上继续出现；
//   - label 原样给出（可能为空），留空时回落到 host 由**前端**做 —— 服务端不做
//     展示层拼接，这与 nodeDTO 里 local_ip/local_ip6 分开返回是同一条约定。
func orderOverviewTargets(p store.OverviewPing, configured []store.PingTarget) store.OverviewPing {
	byID := make(map[int64]store.OverviewPingTarget, len(p.Targets))
	for _, t := range p.Targets {
		byID[t.ID] = t
	}
	ordered := make([]store.OverviewPingTarget, 0, len(configured))
	for _, cfg := range configured {
		item, ok := byID[cfg.ID]
		if !ok {
			// 这个目标这一小时一个点都没有：照样列出来（has_data=false）。
			item = store.OverviewPingTarget{ID: cfg.ID}
		}
		item.Label = cfg.Label
		item.Host = cfg.Host
		ordered = append(ordered, item)
	}
	p.Targets = ordered
	return p
}
