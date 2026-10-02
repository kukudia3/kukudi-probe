package alert

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// telegramAPIBase 是 Telegram Bot API 的固定地址。
//
// 整个项目只有这一个外部目标，且**硬编码**：绝不接受用户传入的 URL，
// 因此不存在 SSRF 面（docs/DESIGN.md §13）。只有测试会临时替换它。
var telegramAPIBase = "https://api.telegram.org"

// Telegram 是 v1 唯一实现的通知渠道。
type Telegram struct {
	token  string
	chatID string
	client *http.Client
}

// NewTelegram 构造 Telegram 通知器。token/chatID 为空时 Send 会直接返回错误。
func NewTelegram(token, chatID string) *Telegram {
	return &Telegram{
		token:  strings.TrimSpace(token),
		chatID: strings.TrimSpace(chatID),
		client: &http.Client{Timeout: 15 * time.Second},
	}
}

// Name 实现 Notifier。
func (t *Telegram) Name() string { return "telegram" }

// Configured 报告是否已经填好 Bot Token 与 Chat ID。
func (t *Telegram) Configured() bool { return t.token != "" && t.chatID != "" }

// ValidateTelegramConfig 在保存设置时做一次基本校验，避免把明显写错的配置存起来。
func ValidateTelegramConfig(token, chatID string) error {
	token = strings.TrimSpace(token)
	chatID = strings.TrimSpace(chatID)
	if token == "" {
		return errors.New("Bot Token 不能为空")
	}
	parts := strings.SplitN(token, ":", 2)
	if len(parts) != 2 || parts[0] == "" || len(parts[1]) < 10 {
		return errors.New("Bot Token 形如 123456789:AA...，请检查是否填错")
	}
	if _, err := strconv.ParseInt(parts[0], 10, 64); err != nil {
		return errors.New("Bot Token 前半段应当是数字（Bot ID）")
	}
	if chatID == "" {
		return errors.New("Chat ID 不能为空")
	}
	if strings.HasPrefix(chatID, "@") {
		// Telegram 频道用户名至少 5 个字符（@ 后面 5 位起）。
		if len(chatID) < 6 {
			return errors.New("频道用户名过短（形如 @my_channel）")
		}
		return nil
	}
	if _, err := strconv.ParseInt(chatID, 10, 64); err != nil {
		return errors.New("Chat ID 应当是数字（群/私聊）或以 @ 开头的频道名")
	}
	return nil
}

type telegramResponse struct {
	OK          bool   `json:"ok"`
	Description string `json:"description"`
	Parameters  *struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
}

// Send 发送一条通知。
//
// 注意：Token 出现在 URL 路径里，因此**任何日志与错误信息都不得包含 URL**。
func (t *Telegram) Send(ctx context.Context, n Notification) error {
	if !t.Configured() {
		return errors.New("Telegram 未配置（缺少 Bot Token 或 Chat ID）")
	}

	payload := map[string]any{
		"chat_id":                  t.chatID,
		"text":                     RenderBatch([]Notification{n}),
		"disable_web_page_preview": true,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("序列化 Telegram 请求失败: %w", err)
	}

	endpoint := telegramAPIBase + "/bot" + t.token + "/sendMessage"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		// 不要把 err 原样返回：它可能包含完整 URL（含 Token）。
		return errors.New("构造 Telegram 请求失败")
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := t.client.Do(req)
	if err != nil {
		return fmt.Errorf("请求 Telegram 失败: %w", scrubToken(err, t.token))
	}
	defer func() { _ = resp.Body.Close() }()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	var parsed telegramResponse
	_ = json.Unmarshal(raw, &parsed)

	if parsed.Parameters != nil && parsed.Parameters.RetryAfter > 0 {
		return &RetryAfterError{
			After:   time.Duration(parsed.Parameters.RetryAfter) * time.Second,
			Message: scrubToken(errors.New(parsed.Description), t.token).Error(),
		}
	}
	if resp.StatusCode/100 != 2 || !parsed.OK {
		message := parsed.Description
		if message == "" {
			message = resp.Status
		}
		return fmt.Errorf("Telegram 返回失败: %s", message)
	}
	return nil
}

// scrubToken 把错误信息里可能出现的 Token 替换掉（URL 会出现在 http 错误里）。
func scrubToken(err error, token string) error {
	if err == nil || token == "" {
		return err
	}
	message := strings.ReplaceAll(err.Error(), token, "***")
	return errors.New(message)
}

// LogNotifier 把通知写进服务端日志。
//
// 它永远开启：既方便排查"为什么没收到"，也让告警在没配 Telegram 时也不至于彻底消失。
type LogNotifier struct {
	Log interface {
		Info(msg string, args ...any)
	}
}

// Name 实现 Notifier。
func (n LogNotifier) Name() string { return "log" }

// Send 实现 Notifier。
//
// Title 为空时不写 title 字段：分发器交给通知器的那一条通知，Body 已经是渲染好的
// 整段文本，标题就在 Body 第一行里（见 batchNotification）—— 再单列一次就是同一行
// 日志里出现两遍标题，而那正是要修的毛病（Telegram 那条路上它是**两行**标题）。
func (n LogNotifier) Send(_ context.Context, notification Notification) error {
	if n.Log == nil {
		return nil
	}
	attrs := []any{
		"rule", notification.Rule,
		"severity", string(notification.Severity),
		"node", notification.NodeName,
	}
	if notification.Title != "" {
		attrs = append(attrs, "title", notification.Title)
	}
	attrs = append(attrs, "body", notification.Body)
	n.Log.Info("告警通知", attrs...)
	return nil
}
