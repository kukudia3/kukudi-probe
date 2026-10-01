package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"probe/internal/config"
	"probe/internal/state"
	"probe/internal/store"
	"probe/internal/version"
)

// handleUpdateNode 修改节点配置（字段与新增一致；Token 不在这里改）。
func (s *Server) handleUpdateNode(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDFromPath(w, r)
	if !ok {
		return
	}
	var req createNodeRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.badRequest(w, err)
		return
	}

	ctx := r.Context()
	current, ok := s.loadNode(w, r, id)
	if !ok {
		return
	}

	updated := current
	updated.Name = req.Name
	updated.GroupName = req.GroupName
	updated.Region = req.Region
	updated.Note = req.Note
	updated.Iface = req.Iface
	updated.IntervalSec = req.IntervalSec
	updated.TrafficLimit = req.TrafficLimit
	updated.TrafficWarnPct = req.TrafficWarnPct
	updated.ResetDay = req.ResetDay
	updated.ExpiresAt = req.ExpiresAt
	updated.PriceCents = req.PriceCents
	updated.Currency = normalizedCurrency(req.Currency)
	updated.BillingMonths = req.BillingMonths
	// 标签在这里先归一化一次：一是拿到"去空白/去重之后"的值才能在审计里写清
	// 到底改成了什么，二是非法标签（超长/超量）能立刻返回 400，错误消息与
	// 存储层完全一致（同一个函数产生的）。
	tags, err := store.NormalizeTags(req.Tags)
	if err != nil {
		s.badRequest(w, err)
		return
	}
	updated.Tags = tags
	if req.Enabled != nil {
		updated.Enabled = *req.Enabled
	}
	if req.SortOrder != nil {
		updated.SortOrder = *req.SortOrder
	}

	if err := s.db.UpdateNode(ctx, updated, time.Now()); err != nil {
		switch {
		case errors.Is(err, store.ErrNodeNameTaken):
			s.writeJSON(w, http.StatusConflict, errorEnvelope{Error: apiError{
				Code: "name_taken", Message: "节点名称已存在"}})
		case errors.Is(err, store.ErrInvalidNode):
			s.badRequest(w, err)
		default:
			s.log.Error("更新节点失败", "err", err, "node_id", id)
			s.writeJSON(w, http.StatusInternalServerError, errorEnvelope{Error: apiError{
				Code: "internal", Message: "服务端内部错误"}})
		}
		return
	}

	// 配置变了（额度/到期日/重置日都可能变），告警规则重新判断一次。
	// 已经发过的通知不受影响：状态被清掉后，下一轮评估只是"从当前事实重新开始"。
	if err := s.db.DeleteAlertStates(ctx, id); err != nil {
		s.log.Warn("重置告警状态失败", "err", err, "node_id", id)
	}
	s.trafficCache.invalidate()

	// 停用的节点必须断开现有连接：否则它会继续上报、继续显示"在线"，
	// 与界面上已经关掉的开关自相矛盾（新连接本来就会被拒）。
	if !updated.Enabled {
		if n := s.agents.DisconnectNode(id); n > 0 {
			s.log.Info("已断开被停用节点的连接", "node_id", id, "connections", n)
		}
	}

	// 标签变了就在审计里写清变成了什么：设置页的「编辑标签」发的也是这个接口，
	// 而"修改节点 X"这一句看不出到底改了什么 —— 事后翻操作记录时等于没记。
	detail := "修改节点 " + updated.Name
	if !slices.Equal(current.Tags, tags) {
		detail += fmt.Sprintf("（标签：%s）", strings.Join(tags, "、"))
	}
	s.audit(ctx, r, "node_update", id, detail)
	s.log.Info("已更新节点", "node_id", id, "name", updated.Name, "enabled", updated.Enabled)

	// 上报间隔、网卡名都在 config 帧里：改完主动推一帧，Agent 立刻按新配置工作，
	// 不必等它下次重连（以前 config 帧从来没发过，这些字段只能在重连时才生效）。
	s.agents.PushConfig()

	st, hasState := s.state.Get(id)
	s.writeJSON(w, http.StatusOK, map[string]any{
		"node": s.dtoFor(updated, st, hasState, time.Now()),
	})
}

// handleDeleteNode 删除节点及其全部历史数据。
func (s *Server) handleDeleteNode(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDFromPath(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	node, ok := s.loadNode(w, r, id)
	if !ok {
		return
	}

	if err := s.db.DeleteNode(ctx, id); err != nil {
		s.log.Error("删除节点失败", "err", err, "node_id", id)
		s.writeJSON(w, http.StatusInternalServerError, errorEnvelope{Error: apiError{
			Code: "internal", Message: "服务端内部错误"}})
		return
	}
	// 先断开连接，再清内存状态与流量记账：否则残留的帧会让已删节点"复活"，
	// 而它的外键目标已经不存在，整批流量落盘都会失败（拖垮其它节点的记账）。
	if n := s.agents.DisconnectNode(id); n > 0 {
		s.log.Info("已断开被删除节点的连接", "node_id", id, "connections", n)
	}
	s.state.Delete(id)
	s.traffic.forget(id)
	s.agg.forget(id)
	s.online.forget(id)
	s.trafficCache.invalidate()

	s.audit(ctx, r, "node_delete", id, "删除节点 "+node.Name)
	s.log.Info("已删除节点", "node_id", id, "name", node.Name)
	s.writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "id": id})
}

// handleRotateNodeToken 重新生成 Token（旧的立即失效），新 Token 只返回一次。
func (s *Server) handleRotateNodeToken(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDFromPath(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	node, ok := s.loadNode(w, r, id)
	if !ok {
		return
	}

	updated, token, err := s.db.RotateNodeToken(ctx, id, time.Now())
	if err != nil {
		s.log.Error("重新生成 Token 失败", "err", err, "node_id", id)
		s.writeJSON(w, http.StatusInternalServerError, errorEnvelope{Error: apiError{
			Code: "internal", Message: "服务端内部错误"}})
		return
	}
	// 旧 Token 立刻作废：把该节点现有的连接断开。
	s.agents.DisconnectNode(id)

	s.audit(ctx, r, "node_token_rotate", id, "重新生成节点 "+node.Name+" 的 Token")
	s.log.Info("已重新生成 Token", "node_id", id, "name", node.Name)
	s.writeJSON(w, http.StatusOK, map[string]any{
		"node":  s.dtoFor(updated, state.Node{}, false, time.Now()),
		"token": token,
	})
}

// handleListAudit 返回最近的审计日志。
func (s *Server) handleListAudit(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	limit := 100
	if raw := query.Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 500 {
			s.badRequest(w, errors.New("limit 必须在 1 到 500 之间"))
			return
		}
		limit = parsed
	}
	beforeID := int64(0)
	if raw := query.Get("before_id"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 0 {
			s.badRequest(w, errors.New("before_id 不合法"))
			return
		}
		beforeID = parsed
	}

	rows, err := s.db.ListAudit(r.Context(), limit, beforeID)
	if err != nil {
		s.log.Error("查询审计日志失败", "err", err)
		s.writeJSON(w, http.StatusInternalServerError, errorEnvelope{Error: apiError{
			Code: "internal", Message: "服务端内部错误"}})
		return
	}

	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, map[string]any{
			"id":      row.ID,
			"ts":      row.TS,
			"action":  row.Action,
			"node_id": row.NodeID,
			"ip":      row.IP,
			"detail":  row.Detail,
		})
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"entries": out})
}

// serverSettings 是设置页展示的服务器信息（**只读**：改这些要改命令行参数并重启）。
type serverSettings struct {
	Version         string `json:"version"`
	Commit          string `json:"commit"`
	Listen          string `json:"listen"`
	Timezone        string `json:"timezone"`
	Retention10s    string `json:"retention_10s"`
	Retention1m     string `json:"retention_1m"`
	FlushInterval   string `json:"flush_interval"`
	StaleAfter      string `json:"stale_after"`
	OfflineAfter    string `json:"offline_after"`
	TLS             bool   `json:"tls"`
	UptimeSec       int64  `json:"uptime_sec"`
	NodeCount       int    `json:"node_count"`
	TrafficDeltaMax string `json:"traffic_delta_max"`
}

// alertSettings 是可以随时修改的告警参数。
type alertSettings struct {
	Cooldown      string `json:"cooldown"`
	StartupGrace  string `json:"startup_grace"`
	Debounce      string `json:"debounce"`
	RecoverStable string `json:"recover_stable"`
}

func (s *Server) currentAlertSettings() alertSettings {
	return alertSettings{
		Cooldown:      s.cfg.AlertCooldown.String(),
		StartupGrace:  s.cfg.AlertStartupGrace.String(),
		Debounce:      s.cfg.AlertDebounce.String(),
		RecoverStable: s.cfg.AlertRecoverStable.String(),
	}
}

// handleGetSettings 返回服务器信息（只读）+ 告警参数。
func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	nodes, err := s.db.ListNodes(r.Context())
	if err != nil {
		s.log.Error("查询节点数量失败", "err", err)
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"server": serverSettings{
			Version:         version.Version,
			Commit:          version.Commit,
			Listen:          s.cfg.Listen,
			Timezone:        s.loc.String(),
			Retention10s:    s.cfg.Retention10s.String(),
			Retention1m:     s.cfg.Retention1m.String(),
			FlushInterval:   s.cfg.FlushInterval.String(),
			StaleAfter:      s.cfg.StaleAfter.String(),
			OfflineAfter:    s.cfg.OfflineAfter.String(),
			TLS:             s.cfg.TLSCert != "",
			UptimeSec:       int64(time.Since(s.started).Seconds()),
			NodeCount:       len(nodes),
			TrafficDeltaMax: config.FormatBytes(s.cfg.TrafficDeltaMax),
		},
		"alert":  s.currentAlertSettings(),
		"charts": s.currentChartSettings(r.Context()),
	})
}

// chartSettings 是图表可见性设置。GET 与 PUT 返回同一形状，
// 前端一套解析逻辑就够；"可见"之外还带上"全部可选"，前端不用再抄一份键表。
type chartSettings struct {
	Visible []string `json:"visible"`
	All     []string `json:"all"`
}

// currentChartSettings 读可见性；读失败时退回"全部显示"。
//
// 设置页里还挤着密码、通知、只读服务器信息，不能因为一行图表设置读不出来
// 就让整个设置对话框打不开——少几张图远好过打不开设置。
func (s *Server) currentChartSettings(ctx context.Context) chartSettings {
	visible, err := s.db.VisibleCharts(ctx)
	if err != nil {
		s.log.Warn("读取图表显示设置失败，本次按全部显示处理", "err", err)
		visible = store.AllChartsCopy()
	}
	return chartSettings{Visible: visible, All: store.AllChartsCopy()}
}

type chartSettingsRequest struct {
	Visible []string `json:"visible"`
}

// handlePutChartSettings 保存详情页要显示哪些图表。
func (s *Server) handlePutChartSettings(w http.ResponseWriter, r *http.Request) {
	var req chartSettingsRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.badRequest(w, err)
		return
	}

	// 未知键必须报错而不是静默丢弃：前端勾选框与服务端键表一旦漂移，
	// 静默丢弃的表现是"勾了保存后自己弹回去"，用户完全看不出哪里错了。
	clean := make([]string, 0, len(req.Visible))
	seen := make(map[string]bool, len(req.Visible))
	for _, key := range req.Visible {
		if !store.IsKnownChart(key) {
			s.badRequest(w, fmt.Errorf("未知的图表 %q，可选：%s", key, strings.Join(store.AllCharts, ", ")))
			return
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		clean = append(clean, key)
	}

	if err := s.db.SetVisibleCharts(r.Context(), clean); err != nil {
		s.log.Error("保存图表显示设置失败", "err", err)
		s.writeJSON(w, http.StatusInternalServerError, errorEnvelope{Error: apiError{
			Code: "internal", Message: "服务端内部错误"}})
		return
	}
	s.audit(r.Context(), r, "settings_update", 0,
		fmt.Sprintf("修改图表显示（保留 %d/%d）", len(clean), len(store.AllCharts)))
	s.writeJSON(w, http.StatusOK, chartSettings{Visible: clean, All: store.AllChartsCopy()})
}

type alertSettingsRequest struct {
	Cooldown      string `json:"cooldown"`
	StartupGrace  string `json:"startup_grace"`
	Debounce      string `json:"debounce"`
	RecoverStable string `json:"recover_stable"`
}

// handlePutAlertSettings 修改告警参数并立刻生效（不需要重启）。
func (s *Server) handlePutAlertSettings(w http.ResponseWriter, r *http.Request) {
	var req alertSettingsRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.badRequest(w, err)
		return
	}

	parse := func(name, raw string, min, max time.Duration) (time.Duration, bool, error) {
		if raw == "" {
			return 0, false, nil
		}
		d, err := time.ParseDuration(raw)
		if err != nil {
			return 0, false, errors.New(name + " 不是合法的时长（如 2s、30m、1h）")
		}
		if d < min || d > max {
			return 0, false, errors.New(name + " 超出允许范围")
		}
		return d, true, nil
	}

	if value, set, err := parse("冷却时间", req.Cooldown, 0, 24*time.Hour); err != nil {
		s.badRequest(w, err)
		return
	} else if set {
		s.cfg.AlertCooldown = value
	}
	if value, set, err := parse("启动静默期", req.StartupGrace, 0, time.Hour); err != nil {
		s.badRequest(w, err)
		return
	} else if set {
		s.cfg.AlertStartupGrace = value
	}
	if value, set, err := parse("离线去抖", req.Debounce, 0, time.Minute); err != nil {
		s.badRequest(w, err)
		return
	} else if set {
		s.cfg.AlertDebounce = value
	}
	if value, set, err := parse("恢复确认", req.RecoverStable, 0, time.Hour); err != nil {
		s.badRequest(w, err)
		return
	} else if set {
		s.cfg.AlertRecoverStable = value
	}

	// 立刻生效；已触发的状态保留，不会因为改参数而重复通知。
	s.engine.SetParams(alertParams(s.cfg))

	s.audit(r.Context(), r, "settings_update", 0, "修改告警参数")
	s.writeJSON(w, http.StatusOK, map[string]any{"alert": s.currentAlertSettings()})
}

func (s *Server) badRequest(w http.ResponseWriter, err error) {
	s.writeJSON(w, http.StatusBadRequest, errorEnvelope{Error: apiError{Code: "bad_request", Message: err.Error()}})
}

// audit 写审计日志（失败只记警告，不影响主流程）。
func (s *Server) audit(ctx context.Context, r *http.Request, action string, nodeID int64, detail string) {
	username := "unknown"
	if user, ok := userFrom(ctx); ok {
		username = user.Username
	}
	if err := s.db.AppendAudit(ctx, action, nodeID, clientIP(r), detail+"（by "+username+"）"); err != nil {
		s.log.Warn("写入审计日志失败", "err", err)
	}
}
