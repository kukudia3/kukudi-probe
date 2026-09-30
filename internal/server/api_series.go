package server

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"probe/internal/store"
)

// nodeIDFromPath 取出并校验路径里的节点 ID。
func (s *Server) nodeIDFromPath(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := r.PathValue("id")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		s.writeJSON(w, http.StatusBadRequest, errorEnvelope{Error: apiError{
			Code: "bad_request", Message: "节点 ID 不合法"}})
		return 0, false
	}
	return id, true
}

func (s *Server) loadNode(w http.ResponseWriter, r *http.Request, id int64) (store.Node, bool) {
	node, err := s.db.NodeByID(r.Context(), id)
	if errors.Is(err, store.ErrNodeNotFound) {
		s.writeJSON(w, http.StatusNotFound, errorEnvelope{Error: apiError{
			Code: "not_found", Message: "节点不存在"}})
		return store.Node{}, false
	}
	if err != nil {
		s.log.Error("读取节点失败", "err", err)
		s.writeJSON(w, http.StatusInternalServerError, errorEnvelope{Error: apiError{
			Code: "internal", Message: "服务端内部错误"}})
		return store.Node{}, false
	}
	return node, true
}

// rangeMeta 是前端画图需要的全部参数（前端不做算术）。
type rangeMeta struct {
	Key          string `json:"key"`
	Seconds      int64  `json:"seconds"`
	BucketSec    int64  `json:"bucket_sec"`
	Points       int    `json:"points"`
	TickBaseSec  int64  `json:"tick_base_sec"`
	TickLabelSec int64  `json:"tick_label_sec"`
	MobileAggSec int64  `json:"mobile_agg_sec"`
	Source       string `json:"source"`
}

func allRangeMeta() []rangeMeta {
	ranges := store.Ranges()
	out := make([]rangeMeta, 0, len(ranges))
	for _, r := range ranges {
		out = append(out, rangeMetaOf(r))
	}
	return out
}

func rangeMetaOf(r store.Range) rangeMeta {
	return rangeMeta{
		Key:          r.Key,
		Seconds:      int64(r.Window.Seconds()),
		BucketSec:    r.Bucket,
		Points:       r.Points(),
		TickBaseSec:  r.TickBaseSec,
		TickLabelSec: r.TickLabelSec(),
		MobileAggSec: r.MobileAggSec,
		Source:       r.Source,
	}
}

// handleNodeDetail 返回详情页需要的一切：配置、实时状态、可用率、六档参数。
func (s *Server) handleNodeDetail(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDFromPath(w, r)
	if !ok {
		return
	}
	node, ok := s.loadNode(w, r, id)
	if !ok {
		return
	}

	now := time.Now()
	st, hasState := s.state.Get(id)
	dto := s.dtoFor(node, st, hasState, now)
	if aggs, err := s.trafficAggregates(r.Context(), []store.Node{node}, now); err != nil {
		s.log.Warn("流量汇总失败", "err", err, "node_id", id)
	} else {
		applyTraffic(&dto, aggs[id], s.loc)
	}

	uptime := map[string]any{}
	for _, key := range []string{"1d", "7d"} {
		rg, ok := store.RangeByKey(key)
		if !ok {
			continue
		}
		pct, has, err := s.db.QueryUptime(r.Context(), id, rg, now)
		if err != nil {
			s.log.Error("查询可用率失败", "err", err, "node_id", id, "range", key)
			continue
		}
		uptime[key] = map[string]any{"pct": pct, "has_data": has}
	}

	s.writeJSON(w, http.StatusOK, map[string]any{
		"node":   dto,
		"uptime": uptime,
		"ranges": allRangeMeta(),
		"server": map[string]any{
			"time":     now.In(s.loc).Format(time.RFC3339),
			"timezone": s.loc.String(),
		},
	})
}

// handleSeries 返回一条历史曲线。
//
// 点数、桶宽、刻度全部由后端决定（前端只画），因此 PC 与手机拿到的是同一份数据。
func (s *Server) handleSeries(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDFromPath(w, r)
	if !ok {
		return
	}
	if _, ok := s.loadNode(w, r, id); !ok {
		return
	}

	query := r.URL.Query()
	metric := query.Get("metric")
	if metric == "" {
		metric = "cpu"
	}
	rangeKey := query.Get("range")
	if rangeKey == "" {
		rangeKey = "1h"
	}
	rg, ok := store.RangeByKey(rangeKey)
	if !ok {
		s.writeJSON(w, http.StatusBadRequest, errorEnvelope{Error: apiError{
			Code: "bad_range", Message: "不支持的时间范围，可选：" + rangeKeysHint()}})
		return
	}
	if !isKnownMetric(metric) {
		s.writeJSON(w, http.StatusBadRequest, errorEnvelope{Error: apiError{
			Code: "bad_metric", Message: "不支持的指标，可选：" + metricKeysHint()}})
		return
	}

	now := time.Now()
	points, err := s.db.QuerySeries(r.Context(), id, metric, rg, now)
	if err != nil {
		s.log.Error("查询曲线失败", "err", err, "node_id", id, "metric", metric, "range", rangeKey)
		s.writeJSON(w, http.StatusInternalServerError, errorEnvelope{Error: apiError{
			Code: "internal", Message: "服务端内部错误"}})
		return
	}

	flat := make([][3]float64, 0, len(points))
	for _, p := range points {
		flat = append(flat, [3]float64{float64(p.TS), p.Avg, p.Max})
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"meta":   rangeMetaOf(rg),
		"metric": metric,
		"points": flat,
	})
}

func isKnownMetric(metric string) bool {
	for _, m := range store.MetricNames() {
		if m == metric {
			return true
		}
	}
	return false
}

func rangeKeysHint() string {
	keys := ""
	for i, r := range store.Ranges() {
		if i > 0 {
			keys += ", "
		}
		keys += r.Key
	}
	return keys
}

func metricKeysHint() string {
	keys := ""
	for i, m := range store.MetricNames() {
		if i > 0 {
			keys += ", "
		}
		keys += m
	}
	return keys
}
