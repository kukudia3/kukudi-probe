package alert

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
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

// maxRetryAfter 是我们愿意为对端 retry_after 等待的上限。
//
// 为什么必须封顶：retry_after 是 JSON int（最大 2^31-1 秒 ≈ 68 年），而等待发生在
// **唯一的**发送 worker 里（见 Dispatcher.sendWithRetry）—— 一个荒诞的取值会把整条
// 通知流水线按住，队列随后填满、新告警全部被丢弃，而运维看不到任何"告警系统已停"
// 的信号。现实中的 429 flood wait 可能是整天（86400），那时"提前重试一次换来又一次
// 429"远比"静默一整天"划算。
//
// 超限时把对端要求的值与封顶值一并写进错误信息：日志里必须看得出"消息被压住了"。
//
// var 而不是 const：用例要把它调小，好在秒级内验证"worker 不会被按住到天亮"，
// 而不是真的等 5 分钟（见 TestTelegramHugeRetryAfterDoesNotStallThePipeline）。
var maxRetryAfter = 5 * time.Minute

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
		after := time.Duration(parsed.Parameters.RetryAfter) * time.Second
		// 对端要求得比我们能接受的上限还久：只等上限，并把这件事写清楚 ——
		// 这条错误会被分发器打进日志（"通知发送失败"），是"告警被压住"的唯一信号。
		capped := ""
		if after > maxRetryAfter {
			capped = fmt.Sprintf("（对端要求 %s，已封顶为 %s）", after, maxRetryAfter)
			after = maxRetryAfter
		}
		return &RetryAfterError{
			After:   after,
			Message: scrubToken(errors.New(parsed.Description), t.token).Error() + capped,
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
//
// 为什么不能只替换"原样"那一份：*url.Error 里放的是 req.URL.String()，路径是按
// net/url 的 encodePath 规则**转义**过的 —— Token 含空格、引号、反引号、非 ASCII
// 时，错误文本里是 "%20" / "%22" / "%E8%B7%AF" 这种**可逆**的转义形态，原样替换
// 匹配不上，畸形 Token 于是原封不动地进了服务端日志（dispatcher.go 的"通知发送
// 失败"）以及 POST /api/v1/settings/telegram/test 的响应体（internal/server/alert.go）。
//
// 替换的形态（真实 Bot Token 的字符集是 [0-9]+:[A-Za-z0-9_-]{35}，这几种转义对它
// 全是恒等变换，所以线上正常配置逐字节不变）：
//   - 原样：出现在响应体/描述文本里；
//   - 请求 URL 里的转义形态：用与 net/url 同一个编码器算出来（tokenForms）；
//   - PathEscape / QueryEscape 形态：同一件事的另两种常见转义。
func scrubToken(err error, token string) error {
	if err == nil || token == "" {
		return err
	}
	message := err.Error()
	for _, form := range tokenForms(token) {
		message = strings.ReplaceAll(message, form, "***")
	}
	return errors.New(message)
}

// tokenForms 返回 token 在错误文本里可能出现的形态（去重；原样一定在第一个）。
//
// 为什么不用 url.PathEscape 代替第一个转义形态：PathEscape 转义 "/"、";"、","，
// 而出现在 URL 路径里的那一种（net/url 的 encodePath）**不转义**它们 —— 两者对
// 同一个 Token 会给出不同的串（token 同时含 "/" 与空格时，只按 PathEscape 替换
// 就会漏掉真正的那个形态）。所以这里按实际的构造方式算：Token 在路径里的位置是
// /bot<token>/sendMessage，把它交给同一个编码器，再去掉固定的前后缀。
func tokenForms(token string) []string {
	escaped := (&url.URL{Path: "/bot" + token + "/sendMessage"}).EscapedPath()
	escaped = strings.TrimSuffix(strings.TrimPrefix(escaped, "/bot"), "/sendMessage")

	forms := []string{token, escaped, url.PathEscape(token), url.QueryEscape(token)}
	out := forms[:0]
	seen := make(map[string]bool, len(forms))
	for _, form := range forms {
		if form == "" || seen[form] {
			continue
		}
		seen[form] = true
		out = append(out, form)
	}
	return out
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
