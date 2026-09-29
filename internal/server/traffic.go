package server

import (
	"sync"
	"time"

	"probe/internal/protocol"
	"probe/internal/store"
)

// 流量增量被判定为"可疑"的原因。
const (
	resetFirstSeen  = "first_seen"    // 第一次看到这个节点（只建基线）
	resetAgentReset = "agent_reset"   // Agent 累计值变小：重装或 checkpoint 丢失
	resetTooLarge   = "delta_too_big" // 增量超过上限：防御性重设基线
)

// nodeTraffic 是一个节点的流量状态。
type nodeTraffic struct {
	baseRx, baseTx uint64 // 已落盘的基线（重启后从这里接着算）
	lastRx, lastTx uint64 // 最近一次观测到的 Agent 累计值（增量相对它计算）
	pendRx, pendTx int64  // 尚未落盘的增量
	seen           bool   // 是否已经有基线
	dirty          bool   // 基线需要落盘（首次建基线 / 重设基线）
}

// trafficTracker 负责把 Agent 上报的**累计值**换算成增量。
//
// 关键设计（docs/DESIGN.md §9）：
//   - Agent 上报的是它自己的长期累计（单调），服务端保存"上次采纳的基线"；
//   - 增量 = 本次观测 - 上次观测，因此天然幂等：重复帧算 0，丢帧不丢流量；
//   - 观测值变小 → 说明 Agent 重装/checkpoint 丢失 → 只重设基线，本次差值不计（宁可少算）；
//   - 基线与增量在同一个事务里落盘，且只有写成功才推进内存状态 → 崩溃不丢也不重。
type trafficTracker struct {
	mu       sync.Mutex
	deltaMax int64
	nodes    map[int64]*nodeTraffic
	// filter 只保留"数据库里存在的节点"：删除节点后再收到它的帧时直接忽略，
	// 否则外键约束会让整个流量落盘事务失败，拖垮所有节点的记账。
	filter nodeFilter
}

func newTrafficTracker(deltaMax int64) *trafficTracker {
	if deltaMax <= 0 {
		deltaMax = 1 << 40 // 默认 1 TiB
	}
	return &trafficTracker{deltaMax: deltaMax, nodes: make(map[int64]*nodeTraffic)}
}

// setKnown 记录当前存在的节点集合（读出节点列表后调用），并清掉已删节点的残留状态。
func (t *trafficTracker) setKnown(ids []int64) {
	t.filter.set(ids)
	t.mu.Lock()
	t.filter.prune(t.nodes)
	t.mu.Unlock()
}

func (t *trafficTracker) allowNode(id int64) { t.filter.allow(id) }

// seed 载入已落盘的基线（服务端启动时调用一次）。
func (t *trafficTracker) seed(baselines map[int64][2]uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for id, b := range baselines {
		t.nodes[id] = &nodeTraffic{baseRx: b[0], baseTx: b[1], lastRx: b[0], lastTx: b[1], seen: b[0] > 0 || b[1] > 0}
	}
}

// observe 处理一次上报，返回需要记日志的重设原因（空字符串表示正常累加）。
func (t *trafficTracker) observe(nodeID int64, m protocol.Metrics) string {
	rx, tx := m.Net.RxTotal, m.Net.TxTotal

	t.mu.Lock()
	defer t.mu.Unlock()

	// 已删除节点（或还没进入节点列表的）直接忽略，避免脏数据进库。
	if !t.filter.allows(nodeID) {
		return ""
	}

	nt, ok := t.nodes[nodeID]
	if !ok {
		nt = &nodeTraffic{}
		t.nodes[nodeID] = nt
	}
	// 还没有基线（新节点，或老库里的 0 值）：第一次观测只用来建基线。
	// 绝不能把 Agent 的"安装以来的累计值"当成今天用的流量。
	if !nt.seen {
		nt.baseRx, nt.baseTx = rx, tx
		nt.lastRx, nt.lastTx = rx, tx
		nt.seen = true
		nt.dirty = true
		return resetFirstSeen
	}

	// 增量永远相对"上次观测值"计算：
	//   - 重复帧 → 差值为 0，不会重复计；
	//   - 丢帧/跳帧 → 差值自然补齐，不会漏计。
	reason := ""
	switch {
	case rx < nt.lastRx || tx < nt.lastTx:
		// Agent 重装 / checkpoint 丢失：重设基线，本次差值不计。
		reason = resetAgentReset
	case rx-nt.lastRx > uint64(t.deltaMax) || tx-nt.lastTx > uint64(t.deltaMax):
		// 防御性上限：单次增量超过上限一定不正常。
		reason = resetTooLarge
	}

	if reason != "" {
		nt.baseRx, nt.baseTx = rx, tx
		nt.lastRx, nt.lastTx = rx, tx
		nt.dirty = true
		return reason
	}

	nt.pendRx += int64(rx - nt.lastRx)
	nt.pendTx += int64(tx - nt.lastTx)
	nt.lastRx, nt.lastTx = rx, tx
	return ""
}

// snapshot 取出待落盘的数据。此时**不**修改内存状态。
func (t *trafficTracker) snapshot(day string) []store.TrafficUpdate {
	t.mu.Lock()
	defer t.mu.Unlock()

	out := make([]store.TrafficUpdate, 0, len(t.nodes))
	for id, nt := range t.nodes {
		// 节点已被删除：绝不让它毒化整个落盘事务（外键约束会让整批回滚）。
		if !t.filter.allows(id) {
			continue
		}
		if nt.pendRx == 0 && nt.pendTx == 0 && !nt.dirty {
			continue
		}
		out = append(out, store.TrafficUpdate{
			NodeID:  id,
			Day:     day,
			RxDelta: nt.pendRx,
			TxDelta: nt.pendTx,
			RxTotal: nt.lastRx,
			TxTotal: nt.lastTx,
		})
	}
	return out
}

// commit 在落盘成功后推进基线，并**只扣掉已经写进库的那部分增量**。
//
// 这里不能把 pending 清零：snapshot 与 commit 之间（一次写事务的时间）Agent
// 仍可能上报，那部分增量必须留在 pending 里等下一轮，否则就被永久少算了。
func (t *trafficTracker) commit(updates []store.TrafficUpdate) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, u := range updates {
		nt, ok := t.nodes[u.NodeID]
		if !ok {
			continue
		}
		nt.pendRx -= u.RxDelta
		nt.pendTx -= u.TxDelta
		if nt.pendRx < 0 {
			nt.pendRx = 0
		}
		if nt.pendTx < 0 {
			nt.pendTx = 0
		}
		// 基线 = 已经写进库的值（与 node_runtime 那一行一致）；
		// 期间新到的观测值会在下一轮作为增量继续累加。
		nt.baseRx, nt.baseTx = u.RxTotal, u.TxTotal
		if nt.lastRx == u.RxTotal && nt.lastTx == u.TxTotal {
			nt.dirty = false
		}
	}
}

// forget 丢弃某个节点的状态（删除节点时调用）。
func (t *trafficTracker) forget(nodeID int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.nodes, nodeID)
}

// pending 返回某个节点尚未落盘的增量（测试与诊断用）。
func (t *trafficTracker) pending(nodeID int64) (rx, tx int64, baseline [2]uint64, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	nt, ok := t.nodes[nodeID]
	if !ok {
		return 0, 0, [2]uint64{}, false
	}
	return nt.pendRx, nt.pendTx, [2]uint64{nt.baseRx, nt.baseTx}, true
}

// trafficAgg 是一个节点的流量汇总（今日 / 本周期 / 累计）。
type trafficAgg struct {
	TodayRx, TodayTx int64
	CycleRx, CycleTx int64
	TotalRx, TotalTx int64
	CycleStart       time.Time
	CycleEnd         time.Time
}

// buildTrafficAgg 把"每节点每天的流量"按各自的周期起点汇总。
//
// 这样 N 个节点只需要 2 次查询（日明细 + 累计），而不是每节点 3 次。
func buildTrafficAgg(now time.Time, loc *time.Location, nodes []store.Node,
	daily []store.DailyTraffic, totals map[int64][2]int64) map[int64]trafficAgg {

	type cycle struct {
		start, end string
		startT     time.Time
		endT       time.Time
	}
	cycles := make(map[int64]cycle, len(nodes))
	for _, n := range nodes {
		start := store.CycleStart(now, n.ResetDay, loc)
		end := store.NextCycleStart(now, n.ResetDay, loc)
		cycles[n.ID] = cycle{
			start:  store.FormatDay(start),
			end:    store.FormatDay(end),
			startT: start,
			endT:   end,
		}
	}

	today := store.FormatDay(now.In(loc))
	out := make(map[int64]trafficAgg, len(nodes))
	for id, c := range cycles {
		out[id] = trafficAgg{CycleStart: c.startT, CycleEnd: c.endT}
	}

	for _, d := range daily {
		c, ok := cycles[d.NodeID]
		if !ok {
			continue
		}
		agg := out[d.NodeID]
		if d.Day >= c.start && d.Day < c.end {
			agg.CycleRx += d.Rx
			agg.CycleTx += d.Tx
		}
		if d.Day == today {
			agg.TodayRx += d.Rx
			agg.TodayTx += d.Tx
		}
		out[d.NodeID] = agg
	}
	for id, t := range totals {
		if _, ok := out[id]; !ok {
			continue
		}
		agg := out[id]
		agg.TotalRx, agg.TotalTx = t[0], t[1]
		out[id] = agg
	}
	return out
}
