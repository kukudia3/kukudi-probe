package server

import (
	"context"
	"net/http"
	"time"

	"probe/internal/alert"
	"probe/internal/store"
)

// alertConfig 是 Telegram 通知的配置（保存在 settings 表里）。
type alertConfig struct {
	Enabled bool
	Token   string
	ChatID  string
}

// loadAlertConfig 从数据库读取 Telegram 配置。
func (s *Server) loadAlertConfig(ctx context.Context) (alertConfig, error) {
	values, err := s.db.GetSettings(ctx, store.KeyTelegramEnabled, store.KeyTelegramToken, store.KeyTelegramChatID)
	if err != nil {
		return alertConfig{}, err
	}
	return alertConfig{
		Enabled: values[store.KeyTelegramEnabled] == "1",
		Token:   values[store.KeyTelegramToken],
		ChatID:  values[store.KeyTelegramChatID],
	}, nil
}

// saveAlertConfig 写入 Telegram 配置。
func (s *Server) saveAlertConfig(ctx context.Context, cfg alertConfig) error {
	enabled := "0"
	if cfg.Enabled {
		enabled = "1"
	}
	return s.db.SetSettings(ctx, map[string]string{
		store.KeyTelegramEnabled: enabled,
		store.KeyTelegramToken:   cfg.Token,
		store.KeyTelegramChatID:  cfg.ChatID,
	})
}

// applyNotifiers 按配置重建通知器列表。
//
// 日志通知永远保留：既方便排查"为什么没收到"，也让告警在没配 Telegram 时不会彻底消失。
func (s *Server) applyNotifiers(cfg alertConfig) {
	notifiers := []alert.Notifier{alert.LogNotifier{Log: s.log}}
	if cfg.Enabled && cfg.Token != "" && cfg.ChatID != "" {
		notifiers = append(notifiers, alert.NewTelegram(cfg.Token, cfg.ChatID))
	}
	s.dispatch.SetNotifiers(notifiers)
}

// evaluateAlerts 每秒评估一次规则（纯内存判断，只有状态变化才写库）。
func (s *Server) evaluateAlerts(ctx context.Context, nodes []nodeDTO) {
	snapshots := make([]alert.Node, 0, len(nodes))
	for _, n := range nodes {
		snapshots = append(snapshots, alert.Node{
			ID:             n.ID,
			Name:           n.Name,
			GroupName:      n.GroupName,
			Region:         n.Region,
			Status:         n.Status,
			LastSeen:       time.Unix(n.LastSeen, 0),
			Connected:      n.Connected,
			TrafficLimit:   n.TrafficLimit,
			TrafficWarnPct: n.TrafficWarnPct,
			CycleRx:        n.TrafficCycleRx,
			CycleTx:        n.TrafficCycleTx,
			CycleStart:     s.parseDay(n.CycleStart),
			CycleEnd:       s.parseDay(n.CycleEnd),
			ExpiresAt:      n.ExpiresAt,
		})
	}

	decisions := s.engine.Evaluate(time.Now(), snapshots)
	if len(decisions) == 0 {
		return
	}

	rows := make([]store.AlertStateRow, 0, len(decisions))
	for _, d := range decisions {
		rows = append(rows, store.AlertStateRow{
			NodeID:     d.State.NodeID,
			Rule:       d.State.Rule,
			State:      d.State.State,
			Since:      d.State.Since.Unix(),
			LastNotify: d.State.LastNotify.Unix(),
			NotifyCnt:  d.State.NotifyCnt,
			Context:    d.State.Context,
		})
		if !d.Notify {
			continue
		}
		s.log.Warn("触发告警",
			"rule", d.Notification.Rule,
			"severity", string(d.Notification.Severity),
			"node", d.Notification.NodeName,
			"title", d.Notification.Title)
		s.dispatch.Enqueue(d.Notification)
	}
	if err := s.db.UpsertAlertStates(ctx, rows); err != nil {
		s.log.Error("写入告警状态失败", "err", err, "rows", len(rows))
	}
}

// loadAlertStates 把持久化的告警状态载入引擎（启动时调用）。
func (s *Server) loadAlertStates(ctx context.Context) error {
	rows, err := s.db.LoadAlertStates(ctx)
	if err != nil {
		return err
	}
	states := make([]alert.State, 0, len(rows))
	for _, r := range rows {
		states = append(states, alert.State{
			NodeID:     r.NodeID,
			Rule:       r.Rule,
			State:      r.State,
			Since:      time.Unix(r.Since, 0),
			LastNotify: time.Unix(r.LastNotify, 0),
			NotifyCnt:  r.NotifyCnt,
			Context:    r.Context,
		})
	}
	s.engine.Load(states)
	return nil
}

// parseDay 把 "2006-01-02" 解析成本地时区的零点；空串返回零值。
func (s *Server) parseDay(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	parsed, err := time.ParseInLocation("2006-01-02", value, s.loc)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

type telegramSettingsRequest struct {
	Enabled  bool   `json:"enabled"`
	BotToken string `json:"bot_token"`
	ChatID   string `json:"chat_id"`
}

// handleGetTelegramSettings 返回当前配置（**永不返回 Token 明文**）。
func (s *Server) handleGetTelegramSettings(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.loadAlertConfig(r.Context())
	if err != nil {
		s.log.Error("读取通知配置失败", "err", err)
		s.writeJSON(w, http.StatusInternalServerError, errorEnvelope{Error: apiError{
			Code: "internal", Message: "服务端内部错误"}})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"enabled":   cfg.Enabled,
		"chat_id":   cfg.ChatID,
		"has_token": cfg.Token != "",
		"ready":     cfg.Enabled && cfg.Token != "" && cfg.ChatID != "",
	})
}

// handlePutTelegramSettings 保存配置；Bot Token 留空表示"不修改"。
func (s *Server) handlePutTelegramSettings(w http.ResponseWriter, r *http.Request) {
	var req telegramSettingsRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.writeJSON(w, http.StatusBadRequest, errorEnvelope{Error: apiError{Code: "bad_request", Message: err.Error()}})
		return
	}

	ctx := r.Context()
	current, err := s.loadAlertConfig(ctx)
	if err != nil {
		s.log.Error("读取通知配置失败", "err", err)
		s.writeJSON(w, http.StatusInternalServerError, errorEnvelope{Error: apiError{Code: "internal", Message: "服务端内部错误"}})
		return
	}
	token := current.Token
	if req.BotToken != "" {
		token = req.BotToken
	}
	if req.Enabled || token != "" || req.ChatID != "" {
		if err := alert.ValidateTelegramConfig(token, req.ChatID); err != nil {
			s.writeJSON(w, http.StatusBadRequest, errorEnvelope{Error: apiError{Code: "bad_request", Message: err.Error()}})
			return
		}
	}

	next := alertConfig{Enabled: req.Enabled, Token: token, ChatID: req.ChatID}
	if err := s.saveAlertConfig(ctx, next); err != nil {
		s.log.Error("保存通知配置失败", "err", err)
		s.writeJSON(w, http.StatusInternalServerError, errorEnvelope{Error: apiError{Code: "internal", Message: "服务端内部错误"}})
		return
	}
	s.applyNotifiers(next)

	if user, ok := userFrom(ctx); ok {
		if err := s.db.AppendAudit(ctx, "telegram_settings", 0, clientIP(r), "更新 Telegram 通知设置（by "+user.Username+"）"); err != nil {
			s.log.Warn("写入审计日志失败", "err", err)
		}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"enabled": next.Enabled, "chat_id": next.ChatID, "has_token": next.Token != "",
	})
}

// handleTestTelegram 立刻发一条测试通知。
func (s *Server) handleTestTelegram(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.loadAlertConfig(r.Context())
	if err != nil {
		s.log.Error("读取通知配置失败", "err", err)
		s.writeJSON(w, http.StatusInternalServerError, errorEnvelope{Error: apiError{Code: "internal", Message: "服务端内部错误"}})
		return
	}
	if cfg.Token == "" || cfg.ChatID == "" {
		s.writeJSON(w, http.StatusBadRequest, errorEnvelope{Error: apiError{
			Code: "not_configured", Message: "请先填写 Bot Token 与 Chat ID"}})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	notifier := alert.NewTelegram(cfg.Token, cfg.ChatID)
	err = notifier.Send(ctx, alert.Notification{
		Rule:     "test",
		Severity: alert.SeverityInfo,
		Title:    "测试通知",
		Body:     "极简 VPS 探针：如果你看到这条消息，说明 Telegram 通知已经配置成功。",
		At:       time.Now(),
	})
	if err != nil {
		s.log.Warn("测试通知发送失败", "err", err)
		s.writeJSON(w, http.StatusBadGateway, errorEnvelope{Error: apiError{
			Code: "send_failed", Message: err.Error()}})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
