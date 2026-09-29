package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"probe/internal/state"
	"probe/internal/store"
)

// currentNodes 返回全部节点的前端视图与集群汇总。
func (s *Server) currentNodes(ctx context.Context) ([]nodeDTO, error) {
	nodes, err := s.db.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	aggs, err := s.trafficAggregates(ctx, nodes, now)
	if err != nil {
		// 流量汇总失败不该让首页整体挂掉：指标照常显示，流量显示为 0。
		s.log.Warn("流量汇总失败", "err", err)
		aggs = map[int64]trafficAgg{}
	}

	out := make([]nodeDTO, 0, len(nodes))
	for _, n := range nodes {
		st, ok := s.state.Get(n.ID)
		dto := buildNodeDTO(n, st, ok, now, s.cfg.StaleAfter, s.cfg.OfflineAfter)
		applyTraffic(&dto, aggs[n.ID], s.loc)
		out = append(out, dto)
	}
	return out, nil
}

// trafficAggregates 汇总一批节点的今日/本周期/累计流量。
//
// 无论多少节点都只有 2 次查询：日明细（最多往前 62 天，覆盖任何重置日）+ 累计分组。
// 结果会缓存到下一次流量落盘（每分钟）或节点增删改时失效——因为 1 Hz 的实时循环
// 每秒都要这份数据，而它其实每分钟才变一次。
func (s *Server) trafficAggregates(ctx context.Context, nodes []store.Node, now time.Time) (map[int64]trafficAgg, error) {
	if len(nodes) == 0 {
		return map[int64]trafficAgg{}, nil
	}
	if cached, ok := s.trafficCache.get(now); ok {
		return cached, nil
	}
	since := store.FormatDay(now.In(s.loc).AddDate(0, 0, -62))
	daily, err := s.db.TrafficDailySince(ctx, since)
	if err != nil {
		return nil, err
	}
	totals, err := s.db.TrafficTotals(ctx)
	if err != nil {
		return nil, err
	}
	aggs := buildTrafficAgg(now, s.loc, nodes, daily, totals)
	s.trafficCache.put(now, aggs)
	return aggs, nil
}

func (s *Server) handleListNodes(w http.ResponseWriter, r *http.Request) {
	nodes, err := s.currentNodes(r.Context())
	if err != nil {
		s.log.Error("查询节点列表失败", "err", err)
		s.writeJSON(w, http.StatusInternalServerError, errorEnvelope{Error: apiError{
			Code: "internal", Message: "服务端内部错误"}})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"nodes":   nodes,
		"summary": summarize(nodes),
		"server": map[string]any{
			"stale_after_sec":   int(s.cfg.StaleAfter.Seconds()),
			"offline_after_sec": int(s.cfg.OfflineAfter.Seconds()),
			"time":              time.Now().In(s.loc).Format(time.RFC3339),
			"timezone":          s.loc.String(),
		},
	})
}

type createNodeRequest struct {
	Name           string `json:"name"`
	GroupName      string `json:"group_name"`
	Region         string `json:"region"`
	Note           string `json:"note"`
	Iface          string `json:"iface"`
	IntervalSec    int    `json:"interval_sec"`
	TrafficLimit   int64  `json:"traffic_limit"`
	TrafficWarnPct int    `json:"traffic_warn_pct"`
	ResetDay       int    `json:"reset_day"`
	ExpiresAt      int64  `json:"expires_at"`
	// 指针类型：不传表示"保持原值"（编辑时前端可能不带这些字段）。
	Enabled   *bool `json:"enabled"`
	SortOrder *int  `json:"sort_order"`
}

func (s *Server) handleCreateNode(w http.ResponseWriter, r *http.Request) {
	var req createNodeRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.writeJSON(w, http.StatusBadRequest, errorEnvelope{Error: apiError{Code: "bad_request", Message: err.Error()}})
		return
	}
	// 未填的字段用默认值补齐（前端的表单也允许留空）。
	if req.IntervalSec == 0 {
		req.IntervalSec = 1
	}
	if req.TrafficWarnPct == 0 {
		req.TrafficWarnPct = 80
	}
	if req.ResetDay == 0 {
		req.ResetDay = 1
	}

	ctx := r.Context()
	node, token, err := s.db.CreateNode(ctx, store.NewNode{
		Name:           req.Name,
		GroupName:      req.GroupName,
		Region:         req.Region,
		Note:           req.Note,
		Iface:          req.Iface,
		IntervalSec:    req.IntervalSec,
		TrafficLimit:   req.TrafficLimit,
		TrafficWarnPct: req.TrafficWarnPct,
		ResetDay:       req.ResetDay,
		ExpiresAt:      req.ExpiresAt,
	}, time.Now())
	switch {
	case errors.Is(err, store.ErrNodeNameTaken):
		s.writeJSON(w, http.StatusConflict, errorEnvelope{Error: apiError{
			Code: "name_taken", Message: "节点名称已存在"}})
		return
	case errors.Is(err, store.ErrInvalidNode):
		s.writeJSON(w, http.StatusBadRequest, errorEnvelope{Error: apiError{
			Code: "bad_request", Message: err.Error()}})
		return
	case err != nil:
		s.log.Error("创建节点失败", "err", err)
		s.writeJSON(w, http.StatusInternalServerError, errorEnvelope{Error: apiError{
			Code: "internal", Message: "服务端内部错误"}})
		return
	}

	if user, ok := userFrom(ctx); ok {
		if err := s.db.AppendAudit(ctx, "node_create", node.ID, clientIP(r), "创建节点 "+node.Name+"（by "+user.Username+"）"); err != nil {
			s.log.Warn("写入审计日志失败", "err", err)
		}
	}
	s.trafficCache.invalidate() // 新节点要立刻出现在流量汇总里
	s.agg.allowNode(node.ID)    // 立刻接受它的上报，不必等下一次节点列表刷新
	s.traffic.allowNode(node.ID)
	s.log.Info("已创建节点", "node_id", node.ID, "name", node.Name)
	s.writeJSON(w, http.StatusCreated, map[string]any{
		"node": buildNodeDTO(node, state.Node{}, false, time.Now(), s.cfg.StaleAfter, s.cfg.OfflineAfter),
		// Token 只在这里返回一次，之后数据库里只有它的哈希。
		"token": token,
	})
}
