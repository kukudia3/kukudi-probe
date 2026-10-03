package server

import (
	"sync"
	"time"
)

// trafficCacheTTL 是流量汇总的兜底有效期。
//
// 正常情况下不需要它：每次流量成功落盘（每分钟一次）都会主动失效缓存，
// 节点增删改也会失效。它只是防御性的上限，保证缓存不会"永远"陈旧。
const trafficCacheTTL = 30 * time.Second

// trafficCache 缓存"每节点今日/本周期/累计流量"。
//
// 为什么需要它：1 Hz 的实时循环（同时喂告警与 SSE）每秒都要这份汇总，
// 而它由两条聚合查询算出来，其中一条是对只增不减的 traffic_daily 做全表
// GROUP BY。流量本身每分钟才变一次，所以按分钟缓存既省掉每秒的重复扫描，
// 显示值也完全一样。
//
// 除了"新鲜的那一份"，这里还留着**最近一次成功**的那一份给降级路径用
// （见 lastGood）：invalidate 只把它标记成"不再新鲜"，不丢掉它 ——
// 丢掉的话，紧接着的一次聚合失败就会让流量重新显示成 0（审计 02·L3）。
type trafficCache struct {
	mu    sync.Mutex
	at    time.Time
	agg   map[int64]trafficAgg
	fresh bool
}

func (c *trafficCache) get(now time.Time) (map[int64]trafficAgg, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.agg == nil || !c.fresh || now.Sub(c.at) > trafficCacheTTL {
		return nil, false
	}
	return c.agg, true
}

func (c *trafficCache) put(now time.Time, agg map[int64]trafficAgg) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.agg = agg
	c.at = now
	c.fresh = true
}

// lastGood 返回**上一次成功**算出来的汇总，不看有效期（可能已经很陈旧）。
//
// 它只服务降级路径：流量聚合查询失败时，面板宁可显示一份略陈旧的数字，也不要
// 显示成 0 —— 0 与"这台机器这段周期真的没有流量"在界面上完全一样，既误导运维，
// 又会让告警引擎把"流量超额"误判成已恢复（审计 02·L3）。at 是那一份的取数时刻，
// 只用于日志。
func (c *trafficCache) lastGood() (map[int64]trafficAgg, time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.agg == nil {
		return nil, time.Time{}, false
	}
	return c.agg, c.at, true
}

// invalidate 让缓存立刻失效（流量落盘、节点增删改时调用）。
//
// 只清"新鲜"标记、**保留**上一次成功的结果：它是降级路径的兜底（见 lastGood）。
// 以前这里把 agg 置 nil，于是"缓存刚失效 + 聚合查询失败"这一瞬间会把流量
// 显示成 0 —— 那正是 02·L3 要消掉的东西。清不清 agg 对正常路径没有影响：
// get 还会检查 fresh 与有效期。
func (c *trafficCache) invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fresh = false
}
