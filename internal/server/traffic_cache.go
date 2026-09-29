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
type trafficCache struct {
	mu  sync.Mutex
	at  time.Time
	agg map[int64]trafficAgg
}

func (c *trafficCache) get(now time.Time) (map[int64]trafficAgg, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.agg == nil || now.Sub(c.at) > trafficCacheTTL {
		return nil, false
	}
	return c.agg, true
}

func (c *trafficCache) put(now time.Time, agg map[int64]trafficAgg) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.agg = agg
	c.at = now
}

func (c *trafficCache) invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.agg = nil
	c.at = time.Time{}
}
