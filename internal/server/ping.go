package server

import (
	"sync"
	"time"

	"probe/internal/protocol"
	"probe/internal/store"
)

// pingFlushEvery 是探测结果的落盘周期。
//
// 与探测间隔（默认 60 秒）对齐：Agent 每探一次，服务端就落一行 —— 再密没有意义
// （表是 1 分钟粒度），再疏会丢中间那些分钟。
const pingFlushEvery = time.Minute

// pingExtraTargets 是一个落盘周期内、每个节点允许接纳的**配置外**目标数上限。
//
// 为什么不能只按"当前配置"严格白名单过滤（审计 03-A-1）：服务端下发的配置与
// Agent 正在跑的配置之间**必然**存在错位窗口 —— 管理员刚删掉/换掉目标时推送
// 还没到达，推送写失败时 Agent 要等下一次重连才更新，老 Agent 也只认它收到过的
// 那一份。这些窗口里 Agent 上报的是**上一份合法配置**的 ID，严格过滤会把真实
// 数据丢掉（"删掉一个目标后最后一分钟的历史凭空少一段"），那是误伤。
//
// 取值 = protocol.MaxPingTargets（一条 config/metrics 帧的条目上限）：刚好装下
// "整套过期目标"，所以上面那些窗口里的数据一条不丢；而伪造 target_id 的那条路
// 每节点每个周期最多只能进 16 个 ID —— 与存储层的兜底
// maxTargetsPerNode = 2 × MaxPingTargets（16 个配置内 + 16 个配置外）对齐，
// 上游这道闸永远比下游先收紧，下游那 32 个不会被上游正常数据填满。
const pingExtraTargets = protocol.MaxPingTargets

// pingTracker 把各目标"最近一次探测结果"攒在内存里，由流水线每分钟落盘一次。
//
// 为什么不收到就写库：metrics 是每秒一帧，而探测结果的语义是"最近一次"
// （见 protocol.PingResult），同一行会被重复上报 60 次。直接写库等于把
// 每分钟一行放大成每秒一行，且每一行的内容完全一样。
//
// 取走（take）之后就清空：节点掉线后不应该继续被写入"新鲜"的探测结果，
// 图表上的空洞如实反映"这段时间没探到"。
//
// 这里同时是"哪些目标算数"的那道闸（审计 03-A-1）：target_id 完全由 Agent 自填，
// 没有这道闸时，一枚有效 Token 用每帧 16 个新 ID、每秒 5 帧、20 条连接就能往
// ping_samples_1m 灌约 9.6 万行/分钟，而这些行**永远不会被读到**（界面只遍历
// 配置里的目标）—— 纯磁盘放大。见 admit 的接纳规则。
type pingTracker struct {
	mu     sync.Mutex
	latest map[int64]map[int64]pingSample
	// allowed 是"服务端最近一次**真的**下发给 Agent 的目标 ID 集合"。
	// 探测设置是全局的，所以这份集合也是全局的（见 setAllowed）。
	allowed map[int64]struct{}
	// extra 记录本落盘周期内每个节点已经接纳过的配置外目标（按节点分别计数）。
	extra map[int64]map[int64]struct{}
}

// pingSample 是一次观测：结果 + 观测时刻（时刻决定它属于哪一分钟）。
type pingSample struct {
	result protocol.PingResult
	at     time.Time
}

func newPingTracker() *pingTracker {
	return &pingTracker{
		latest: make(map[int64]map[int64]pingSample),
		extra:  make(map[int64]map[int64]struct{}),
	}
}

// setAllowed 登记"服务端刚刚把这一批目标交给了 Agent"。
//
// 只在下发**写成功之后**调用（见 Agents.sendConfig）：写失败时 Agent 手里还是
// 上一份配置，把没送到的目标当成"已下发"只会让旧目标的合法数据变成配置外数据。
// 登记的是 ID 集合本身而不是某个连接的快照：探测设置是全局的，所有连接收到的
// 都是同一份；某条连接推送失败时，它上报的旧 ID 由配置外配额兜着（见 admit）。
func (p *pingTracker) setAllowed(ids []int64) {
	set := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		if id > 0 {
			set[id] = struct{}{}
		}
	}
	p.mu.Lock()
	p.allowed = set
	p.mu.Unlock()
}

// observe 记录一帧里各目标的最新结果，返回**被丢弃**的条目数。
//
// 丢弃只有一种原因：配置外的目标把本周期配额用完了（见 admit）。返回值让调用方
// 能把这件事件记进日志 —— 静默丢数据是运维最难查的那类故障，而它同时是"配置刚
// 改过、Agent 还没更新"的可见信号。
func (p *pingTracker) observe(nodeID int64, results []protocol.PingResult, now time.Time) int {
	if len(results) == 0 {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	var node map[int64]pingSample
	dropped := 0
	for _, r := range results {
		// 同一帧里的重复目标、以及 target_id 非法的数据在上面已经由
		// protocol.ValidateMetrics 挡掉了，这里只做防御性过滤。
		if r.TargetID <= 0 {
			continue
		}
		if !p.admit(nodeID, r.TargetID) {
			dropped++
			continue
		}
		if node == nil {
			if node = p.latest[nodeID]; node == nil {
				node = make(map[int64]pingSample, len(results))
				p.latest[nodeID] = node
			}
		}
		node[r.TargetID] = pingSample{result: r, at: now}
	}
	return dropped
}

// admit 判断一个目标 ID 是否接纳（调用方必须持有 p.mu）。
//
// 规则（两级，都不看"这个 ID 存不存在于数据库"——那要按帧查库，且删目标与
// Agent 更新之间的窗口同样会把合法数据判成不存在）：
//
//   - 配置内的目标（allowed）**一律**接纳：伪造者刷再多随机 ID 也挤不掉真实目标；
//   - 配置外的目标按节点、按落盘周期给一份配额（pingExtraTargets）：配额内接纳
//     并记账，配额用完就丢弃。记账是必要的 —— 不记账的话同一个旧目标每帧都重新
//     吃一次配额，一秒就能把配额吃光，随后真正的旧目标的最后一帧反而进不来。
//
// 配额按周期（take）重置：这样"配置整体换了一轮"的旧目标下一个周期还能继续
// 上报，直到 Agent 更新（推送失败时可能要好几个周期）。
func (p *pingTracker) admit(nodeID, targetID int64) bool {
	if _, ok := p.allowed[targetID]; ok {
		return true
	}
	extra := p.extra[nodeID]
	if extra == nil {
		extra = make(map[int64]struct{}, pingExtraTargets)
		p.extra[nodeID] = extra
	}
	if _, ok := extra[targetID]; ok {
		return true
	}
	if len(extra) >= pingExtraTargets {
		return false
	}
	extra[targetID] = struct{}{}
	return true
}

// take 取出并清空所有待落盘的观测，按 (节点, 目标) 生成 1 分钟桶。
//
// 桶的时间戳取**观测时刻**所在的分钟（而不是 flush 时刻）：服务端启动、
// ticker 与墙钟分钟错位时，样本仍然落在它真正发生的那一分钟里。
// 代价是极少数情况下（两次 tick 之间跨过整分钟）较早那一分钟只能拿到部分结果，
// 最多少一行 —— 比为此引入一套补算逻辑划算得多。
func (p *pingTracker) take() []store.PingBucket {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.latest) == 0 {
		return nil
	}

	out := make([]store.PingBucket, 0, len(p.latest))
	for nodeID, targets := range p.latest {
		for targetID, s := range targets {
			ts := s.at.Unix() - s.at.Unix()%60
			out = append(out, store.NewPingBucket(nodeID, targetID, ts,
				s.result.AvgMS, s.result.MinMS, s.result.MaxMS, s.result.LossPct))
		}
	}
	p.latest = make(map[int64]map[int64]pingSample)
	// 配置外目标的配额按落盘周期重置：下一个周期又能装下一整套"刚被换掉"的目标。
	// allowed 不在这里清 —— 它是"服务端当前告诉过 Agent 的目标"，跨周期有效。
	p.extra = make(map[int64]map[int64]struct{})
	return out
}

// forget 丢弃某个节点的观测（节点被删除时调用）。
func (p *pingTracker) forget(nodeID int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.latest, nodeID)
	delete(p.extra, nodeID)
}
