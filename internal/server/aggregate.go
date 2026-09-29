package server

import (
	"math"
	"sync"
	"time"

	"probe/internal/protocol"
	"probe/internal/store"
)

// uptimeScale 是 up_cnt / all_cnt 的公共缩放因子（两者必须同单位，比值才有意义）。
//
// 为什么需要它：桶宽是 10 秒，而上报间隔可以是 1–300 秒。间隔大于桶宽时
// "每个桶应有的帧数"是个分数（30 秒间隔 → 1/3 帧），直接用整数会变成 0，
// 可用率就永远算不出来（只能显示"—"）。乘 600 之后，1–300 秒的间隔都能得到
// 整数表示，最大误差 0.02%（6000/7 = 857 而不是 857.14）。
const uptimeScale int64 = 600

// bucketWidth 是内存聚合的桶宽（秒）。与 samples_10s 的桶宽一致。
const bucketWidth int64 = 10

// aggBucket 是正在累积的桶（保存的是"和"，落盘时才除以样本数）。
type aggBucket struct {
	ts  int64
	n   int64
	all int64

	cpuSum, cpuMax   float64
	memSum, memMax   float64
	swapSum          float64
	diskSum, diskMax float64
	loadSum          float64
	rxSum, rxMax     float64
	txSum, txMax     float64
	latSum           float64
	latN             int64
	latMin, latMax   float64
	latSeen          bool
}

func newAggBucket(ts int64) *aggBucket {
	return &aggBucket{ts: ts, latMin: math.MaxFloat64}
}

func (b *aggBucket) add(m protocol.Metrics, intervalSec int) {
	b.n++
	// 本桶"应有"的帧数 = 桶宽 / 间隔，再乘缩放因子（见 uptimeScale）。
	b.all = uptimeScale * bucketWidth / int64(clampInterval(intervalSec))

	b.cpuSum += m.CPUPct
	b.cpuMax = math.Max(b.cpuMax, m.CPUPct)
	b.memSum += m.Mem.Pct
	b.memMax = math.Max(b.memMax, m.Mem.Pct)
	b.swapSum += m.Swap.Pct
	diskPct := 0.0
	if len(m.Disk) > 0 {
		diskPct = m.Disk[0].Pct
	}
	b.diskSum += diskPct
	b.diskMax = math.Max(b.diskMax, diskPct)
	b.loadSum += m.Load.L1
	b.rxSum += m.Net.RxRate
	b.rxMax = math.Max(b.rxMax, m.Net.RxRate)
	b.txSum += m.Net.TxRate
	b.txMax = math.Max(b.txMax, m.Net.TxRate)
	if m.LatMS > 0 {
		// 只把"真的测到了延迟"的样本算进平均值：把 0 当样本会把均值拉低。
		b.latSum += m.LatMS
		b.latN++
		b.latSeen = true
		b.latMin = math.Min(b.latMin, m.LatMS)
		b.latMax = math.Max(b.latMax, m.LatMS)
	}
}

func (b *aggBucket) toSample(nodeID int64) store.SampleBucket {
	n := float64(b.n)
	if n <= 0 {
		n = 1
	}
	out := store.SampleBucket{
		NodeID:  nodeID,
		TS:      b.ts,
		CPUAvg:  b.cpuSum / n,
		CPUMax:  b.cpuMax,
		MemAvg:  b.memSum / n,
		MemMax:  b.memMax,
		SwapAvg: b.swapSum / n,
		DiskAvg: b.diskSum / n,
		DiskMax: b.diskMax,
		LoadAvg: b.loadSum / n,
		RxRate:  b.rxSum / n,
		RxMax:   b.rxMax,
		TxRate:  b.txSum / n,
		TxMax:   b.txMax,
		Up:      b.n * uptimeScale,
		All:     b.all,
	}
	if b.latSeen && b.latN > 0 {
		latN := float64(b.latN)
		out.LatAvg = b.latSum / latN
		out.LatMin = b.latMin
		out.LatMax = b.latMax
	}
	return out
}

func clampInterval(sec int) int {
	if sec < 1 {
		return 1
	}
	if sec > 300 {
		return 300
	}
	return sec
}

// maxReadyBuckets 是"待落盘队列"的上限：库短暂不可写时先攒着重试，
// 但不能无限攒（按 50 节点算，1 万个桶约等于 33 分钟的数据）。
const maxReadyBuckets = 10000

// accumulator 把 1 秒级的上报聚合成 10 秒桶。
//
// 它只保留"当前桶"和"已结束但还没落盘的桶"，内存占用与运行时长无关。
type accumulator struct {
	mu      sync.Mutex
	width   int64
	current map[int64]*aggBucket
	ready   []store.SampleBucket
	// filter 只保留"数据库里存在的节点"：已删除节点的残留帧进库会触发外键错误，
	// 让整批样本落盘失败（见 nodefilter.go）。
	filter nodeFilter
}

func newAccumulator(width int64) *accumulator {
	return &accumulator{width: width, current: make(map[int64]*aggBucket)}
}

// setKnown / allow / drop 把节点过滤器的能力透出来（由服务端在合适时机调用）。
func (a *accumulator) setKnown(ids []int64) {
	a.filter.set(ids)
	// 顺手清掉已删除节点的残留桶，避免它们占着内存又被写出。
	a.mu.Lock()
	a.filter.prune(a.current)
	a.mu.Unlock()
}

func (a *accumulator) allowNode(id int64) { a.filter.allow(id) }

func (a *accumulator) dropNodeFilter(id int64) { a.filter.drop(id) }

// add 把一个上报样本并入当前桶；跨桶时把旧桶移入待落盘队列。
func (a *accumulator) add(nodeID int64, intervalSec int, m protocol.Metrics, now time.Time) {
	if !a.filter.allows(nodeID) {
		return
	}
	ts := now.Unix() - (now.Unix() % a.width)

	a.mu.Lock()
	defer a.mu.Unlock()

	b, ok := a.current[nodeID]
	if !ok || b.ts != ts {
		if ok {
			a.ready = append(a.ready, b.toSample(nodeID))
		}
		b = newAggBucket(ts)
		a.current[nodeID] = b
	}
	b.add(m, intervalSec)
}

// flushClosed 取出所有"已经结束"的桶（ts + width <= alignedNow）。
//
// 刻意不取出仍在累积的桶：服务端退出时丢掉最后不到 10 秒的数据，
// 好过把它当作完整桶写进库、之后被重启后的同一桶覆盖。
func (a *accumulator) flushClosed(now time.Time) []store.SampleBucket {
	cutoff := now.Unix() - (now.Unix() % a.width)

	a.mu.Lock()
	defer a.mu.Unlock()

	for nodeID, b := range a.current {
		if b.ts+a.width <= cutoff {
			a.ready = append(a.ready, b.toSample(nodeID))
			delete(a.current, nodeID)
		}
	}
	if len(a.ready) == 0 {
		return nil
	}
	out := a.ready
	a.ready = nil
	return out
}

// requeue 把落盘失败的一批桶放回待落盘队列（写库是幂等 upsert，重试安全）。
//
// 必须封顶：库长期不可写时不能让内存无限增长。超过上限就丢掉最旧的并返回丢弃数量。
func (a *accumulator) requeue(buckets []store.SampleBucket) int {
	if len(buckets) == 0 {
		return 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	a.ready = append(a.ready, buckets...)
	if len(a.ready) <= maxReadyBuckets {
		return 0
	}
	drop := len(a.ready) - maxReadyBuckets
	a.ready = append([]store.SampleBucket(nil), a.ready[drop:]...)
	return drop
}

// forget 丢弃某个节点的聚合状态（节点被删除时调用）。
func (a *accumulator) forget(nodeID int64) {
	a.filter.drop(nodeID)
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.current, nodeID)
}
