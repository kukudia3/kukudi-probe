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

// pingTracker 把各目标"最近一次探测结果"攒在内存里，由流水线每分钟落盘一次。
//
// 为什么不收到就写库：metrics 是每秒一帧，而探测结果的语义是"最近一次"
// （见 protocol.PingResult），同一行会被重复上报 60 次。直接写库等于把
// 每分钟一行放大成每秒一行，且每一行的内容完全一样。
//
// 取走（take）之后就清空：节点掉线后不应该继续被写入"新鲜"的探测结果，
// 图表上的空洞如实反映"这段时间没探到"。
type pingTracker struct {
	mu     sync.Mutex
	latest map[int64]map[int64]pingSample
}

// pingSample 是一次观测：结果 + 观测时刻（时刻决定它属于哪一分钟）。
type pingSample struct {
	result protocol.PingResult
	at     time.Time
}

func newPingTracker() *pingTracker {
	return &pingTracker{latest: make(map[int64]map[int64]pingSample)}
}

// observe 记录一帧里各目标的最新结果。
func (p *pingTracker) observe(nodeID int64, results []protocol.PingResult, now time.Time) {
	if len(results) == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	node, ok := p.latest[nodeID]
	if !ok {
		node = make(map[int64]pingSample, len(results))
		p.latest[nodeID] = node
	}
	for _, r := range results {
		// 同一帧里的重复目标、以及 target_id 非法的数据在上面已经由
		// protocol.ValidateMetrics 挡掉了，这里只做防御性过滤。
		if r.TargetID <= 0 {
			continue
		}
		node[r.TargetID] = pingSample{result: r, at: now}
	}
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
	return out
}

// forget 丢弃某个节点的观测（节点被删除时调用）。
func (p *pingTracker) forget(nodeID int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.latest, nodeID)
}
