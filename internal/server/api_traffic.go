package server

import (
	"net/http"
	"strconv"
	"time"

	"probe/internal/store"
)

// defaultTrafficDays / maxTrafficDays 是"按天流量"图的窗口。
const (
	defaultTrafficDays = 7
	maxTrafficDays     = 90
)

// handleTraffic 返回按天的流量曲线 + 今日/本周期/累计汇总。
//
// 流量天生是"按天"的量，所以它不跟随六档时间范围，而是固定按天（默认 7 天）。
func (s *Server) handleTraffic(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDFromPath(w, r)
	if !ok {
		return
	}
	node, ok := s.loadNode(w, r, id)
	if !ok {
		return
	}

	days := defaultTrafficDays
	if raw := r.URL.Query().Get("days"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxTrafficDays {
			s.writeJSON(w, http.StatusBadRequest, errorEnvelope{Error: apiError{
				Code: "bad_request", Message: "days 必须在 1 到 90 之间"}})
			return
		}
		days = n
	}

	now := time.Now()
	loc := s.loc
	today := now.In(loc)
	// 从 days-1 天前的本地零点开始。
	startDay := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, -(days - 1))

	daily, err := s.db.TrafficDailySince(r.Context(), store.FormatDay(startDay))
	if err != nil {
		s.log.Error("查询日流量失败", "err", err, "node_id", id)
		s.writeJSON(w, http.StatusInternalServerError, errorEnvelope{Error: apiError{
			Code: "internal", Message: "服务端内部错误"}})
		return
	}

	byDay := make(map[string][2]int64, days)
	for _, d := range daily {
		if d.NodeID != id {
			continue
		}
		byDay[d.Day] = [2]int64{d.Rx, d.Tx}
	}

	// 补齐没有数据的日期（补 0），保证图表的时间轴是连续的。
	points := make([][3]int64, 0, days)
	for i := 0; i < days; i++ {
		day := startDay.AddDate(0, 0, i)
		v := byDay[store.FormatDay(day)]
		points = append(points, [3]int64{day.Unix(), v[0], v[1]})
	}

	aggs, err := s.trafficAggregates(r.Context(), []store.Node{node}, now)
	if err != nil {
		s.log.Warn("流量汇总失败", "err", err, "node_id", id)
	}
	agg := aggs[id]

	s.writeJSON(w, http.StatusOK, map[string]any{
		"days":   days,
		"points": points,
		"today": map[string]int64{
			"rx": agg.TodayRx,
			"tx": agg.TodayTx,
		},
		"cycle": map[string]any{
			"rx":    agg.CycleRx,
			"tx":    agg.CycleTx,
			"start": store.FormatDay(agg.CycleStart),
			"end":   store.FormatDay(agg.CycleEnd),
		},
		"total": map[string]int64{
			"rx": agg.TotalRx,
			"tx": agg.TotalTx,
		},
		"limit": node.TrafficLimit,
	})
}
