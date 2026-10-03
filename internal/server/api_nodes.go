package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
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
	aggs := s.trafficAggsOrFallback(ctx, nodes, now)

	out := make([]nodeDTO, 0, len(nodes))
	for _, n := range nodes {
		st, ok := s.state.Get(n.ID)
		dto := s.dtoFor(n, st, ok, now)
		applyTraffic(&dto, aggs[n.ID], s.loc)
		out = append(out, dto)
	}
	return out, nil
}

// trafficAggsOrFallback 取流量汇总，失败时降级成"上一次成功的那一份"。
//
// 降级口径（审计 02·L3）：查询失败时**不能**把流量当成 0。
//   - 0 与"这台机器这个周期真的没跑流量"在界面上长得一模一样，运维会照它判断，
//     而真正的数据库故障被一个 0 掩盖掉；
//   - 更实际的一条：这份数据同时喂给 1 Hz 的告警评估，静默填 0 会让"本周期流量
//     已超额"看起来恢复了，于是 1 Hz 循环照常走"恢复确认"分支；
//   - 反过来，一次聚合失败就让整个 /nodes 与 /overview 500 也不划算：内存里的
//     实时指标（CPU/内存/在线状态）与探测数据都不依赖这次查询，它们照常是对的。
//
// 所以：有上一次成功的结果就沿用它（数字可能陈旧，日志里说清），一次都没有
// （进程刚起、或从来没成功算过一次）才退回空表 —— 与修复前的行为一致。
// 缓存"失效"（流量落盘、节点增删改）不再等于"把上一次成功的结果丢掉"，
// 见 traffic_cache.go 的 invalidate。
func (s *Server) trafficAggsOrFallback(ctx context.Context, nodes []store.Node, now time.Time) map[int64]trafficAgg {
	aggs, err := s.trafficAggregates(ctx, nodes, now)
	if err == nil {
		return aggs
	}
	if last, at, ok := s.trafficCache.lastGood(); ok {
		s.log.Warn("流量汇总失败，沿用上一次成功的汇总（数字可能已陈旧）",
			"err", err, "since", time.Since(at).Round(time.Second).String())
		return last
	}
	s.log.Warn("流量汇总失败，且没有可沿用的上次结果，本次流量按 0 处理", "err", err)
	return map[int64]trafficAgg{}
}

// trafficAggregates 汇总一批节点的今日/本周期/累计流量（带缓存）。
//
// 无论多少节点都只有 2 次查询：日明细（最多往前 62 天，覆盖任何重置日）+ 累计分组。
// 结果会缓存到下一次流量落盘（每分钟）或节点增删改时失效——因为 1 Hz 的实时循环
// 每秒都要这份数据，而它其实每分钟才变一次。
//
// **它只服务"全量节点"这条路径**（1 Hz 实时循环、/nodes、/overview）。这份缓存没有
// 「覆盖了哪些节点」这个维度，所以传**单元素**列表的调用方（详情页、流量接口）必须走
// trafficAggregatesFresh：否则那一个节点的结果会落进缓存，之后最多 30 秒内其它节点
// 全部命中残缺缓存、流量显示成 0，并让告警引擎误以为"流量已超额"已经恢复。
func (s *Server) trafficAggregates(ctx context.Context, nodes []store.Node, now time.Time) (map[int64]trafficAgg, error) {
	if len(nodes) == 0 {
		return map[int64]trafficAgg{}, nil
	}
	if cached, ok := s.trafficCache.get(now); ok {
		return cached, nil
	}
	aggs, err := s.trafficAggregatesFresh(ctx, nodes, now)
	if err != nil {
		return nil, err
	}
	s.trafficCache.put(now, aggs)
	return aggs, nil
}

// trafficAggregatesFresh 与 trafficAggregates 是同一份查询，但既不读也不写缓存。
//
// 给"只关心一个节点"的调用方用（详情页 /api/v1/nodes/{id}、流量接口
// /api/v1/nodes/{id}/traffic）：它们的结果不能进那份全量缓存（理由见
// trafficAggregates 的注释），而这两个接口每 30 秒才来一次，这 2 条只读聚合
// 查询的成本可以忽略。
func (s *Server) trafficAggregatesFresh(ctx context.Context, nodes []store.Node, now time.Time) (map[int64]trafficAgg, error) {
	if len(nodes) == 0 {
		return map[int64]trafficAgg{}, nil
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
	return buildTrafficAgg(now, s.loc, nodes, daily, totals), nil
}

func (s *Server) handleListNodes(w http.ResponseWriter, r *http.Request) {
	nodes, err := s.currentNodes(r.Context())
	if err != nil {
		s.log.Error("查询节点列表失败", "err", err)
		s.writeJSON(w, http.StatusInternalServerError, errorEnvelope{Error: apiError{
			Code: "internal", Message: "服务端内部错误"}})
		return
	}
	// 访客走白名单脱敏（与 SSE 共用同一个函数，见 guest.go）。脱敏发生在**这一步**
	// 而不是 currentNodes() 里：那份完整视图还要用来评估告警、写审计日志。
	guest := isGuestView(r.Context())
	var payload any = nodes
	if guest {
		payload = guestNodesJSON(nodes)
	}
	// server 这几项是"面板自己的时钟与判定阈值"，不含任何节点信息：
	// 访客页面上的每个时间都要按 server.timezone 渲染，不给就没法显示。
	//
	// stale_after_sec / offline_after_sec 只发给**非访客**。它们不是节点数据，
	// 但是面板**内部**的状态判定阈值，而且全仓 JS 一处都不读（设置页「状态判定」
	// 那两行来自 admin-only 的 /api/v1/settings）—— 对访客就是两个没人用的内部
	// 数字，没有理由下发。访客的 server 块因此只剩 time + timezone，
	// 与 /series 的 meta 口径一致：只给渲染页面**必需**的那几项。
	server := map[string]any{
		"time":     time.Now().In(s.loc).Format(time.RFC3339),
		"timezone": s.loc.String(),
	}
	if !guest {
		server["stale_after_sec"] = int(s.cfg.StaleAfter.Seconds())
		server["offline_after_sec"] = int(s.cfg.OfflineAfter.Seconds())
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"nodes":   payload,
		"summary": summarize(nodes),
		"server":  server,
	})
}

// createNodeRequest 是新增/修改节点的请求体。
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
	// 价格三件套：与其它配置字段一样是"整体替换"语义，缺省即 0/空（没填价格）。
	PriceCents    int64  `json:"price_cents"`
	Currency      string `json:"currency"`
	BillingMonths int    `json:"billing_months"`
	// 标签：与其它配置字段一样是"整体替换"语义 —— 请求里不带就等于"没有标签"。
	// 所以前端改标签时也必须带上完整字段（见 app.js 的 tagFormPayload），
	// 只发 tags 会把名称等字段冲成空值、直接被存储层拒掉。
	Tags []string `json:"tags"`
	// 指针类型：不传表示"保持原值"（编辑时前端可能不带这些字段）。
	Enabled   *bool `json:"enabled"`
	SortOrder *int  `json:"sort_order"`
}

// normalizedCurrency 把货币代码统一成大写去空格的形式。
//
// 放在服务端入口做，而不是靠存储层或前端：前端可以被绕过（curl 直接打接口），
// 而存储层拒绝小写输入会让"cny"这种顺手写的值直接报错，体验不必要地差。
func normalizedCurrency(raw string) string {
	return strings.ToUpper(strings.TrimSpace(raw))
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
		// SortOrder 可能为 nil（前端表单从来不传它）：nil 交给存储层排到最后，
		// 而不是让列默认值 0 把这台新机器顶到列表最前面。
		SortOrder:     req.SortOrder,
		PriceCents:    req.PriceCents,
		Currency:      normalizedCurrency(req.Currency),
		BillingMonths: req.BillingMonths,
		Tags:          req.Tags,
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
		"node": s.dtoFor(node, state.Node{}, false, time.Now()),
		// Token 只在这里返回一次，之后数据库里只有它的哈希。
		"token": token,
	})
}

// reorderNodesRequest 是批量重排的请求体。
//
// ids 是**全部节点**的完整顺序，按下标依次写入 sort_order = 1..N
// （为什么不做"给一部分就只排一部分"的宽松语义，见 store.ReorderNodes）。
type reorderNodesRequest struct {
	IDs []int64 `json:"ids"`
}

// handleReorderNodes 按请求里的顺序重排全部节点。
//
// 只返回一个计数：新顺序对调用方没有信息量（它就是它刚发上来的那个），
// 而前端要重画首页卡片时本来就要重新取一次完整列表（SSE 推的是变更集、不含顺序）。
func (s *Server) handleReorderNodes(w http.ResponseWriter, r *http.Request) {
	var req reorderNodesRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.badRequest(w, err)
		return
	}
	ctx := r.Context()
	if err := s.db.ReorderNodes(ctx, req.IDs); err != nil {
		if errors.Is(err, store.ErrNodeOrderInvalid) {
			// 缺/多/重复/不存在都在这里：消息由存储层给出（那里才看得到真实集合）。
			s.badRequest(w, err)
			return
		}
		s.log.Error("调整节点顺序失败", "err", err)
		s.writeJSON(w, http.StatusInternalServerError, errorEnvelope{Error: apiError{
			Code: "internal", Message: "服务端内部错误"}})
		return
	}
	// node_id 写 0：这是一次作用于整个列表的操作，挂到某一台机器上会误导。
	s.audit(ctx, r, "node_order", 0, fmt.Sprintf("调整节点顺序（%d 台）", len(req.IDs)))
	s.log.Info("已调整节点顺序", "nodes", len(req.IDs))
	s.writeJSON(w, http.StatusOK, map[string]any{"count": len(req.IDs)})
}
