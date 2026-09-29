package alert

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestValidateTelegramConfig(t *testing.T) {
	valid := []struct{ token, chat string }{
		{"123456789:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw", "123456789"},
		{"123456789:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw", "-1001234567890"},
		{"123456789:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw", "@my_channel"},
	}
	for _, c := range valid {
		if err := ValidateTelegramConfig(c.token, c.chat); err != nil {
			t.Errorf("合法配置被判为非法: %v（chat=%s）", err, c.chat)
		}
	}

	invalid := []struct{ token, chat string }{
		{"", "123"},
		{"abc", "123"},
		{"123456789", "123"},
		{"123456789:short", "123"},
		{"123456789:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw", ""},
		{"123456789:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw", "not-a-chat"},
		{"123456789:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw", "@ab"},
	}
	for _, c := range invalid {
		if err := ValidateTelegramConfig(c.token, c.chat); err == nil {
			t.Errorf("非法配置未被拒绝: token=%q chat=%q", c.token, c.chat)
		}
	}
}

// withTelegramServer 把硬编码的 API 地址临时指向测试服务器。
func withTelegramServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(handler)
	previous := telegramAPIBase
	telegramAPIBase = ts.URL
	t.Cleanup(func() {
		telegramAPIBase = previous
		ts.Close()
	})
	return ts
}

func TestTelegramSendSuccess(t *testing.T) {
	var gotPath, gotBody string
	withTelegramServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	})

	const token = "123456789:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw"
	tg := NewTelegram(token, "-1001234567890")
	if !tg.Configured() {
		t.Fatal("配置完整时 Configured 应当为真")
	}
	err := tg.Send(context.Background(), Notification{
		Rule: RuleOffline, Severity: SeverityCritical,
		Title: "节点离线", Body: "hk-01 掉线了", At: time.Now(),
	})
	if err != nil {
		t.Fatalf("发送失败: %v", err)
	}
	if gotPath != "/bot"+token+"/sendMessage" {
		t.Fatalf("请求路径不对: %s", gotPath)
	}

	var payload map[string]any
	if err := json.Unmarshal([]byte(gotBody), &payload); err != nil {
		t.Fatalf("请求体不是 JSON: %v", err)
	}
	if payload["chat_id"] != "-1001234567890" {
		t.Fatalf("chat_id = %v", payload["chat_id"])
	}
	text, _ := payload["text"].(string)
	if !strings.Contains(text, "节点离线") || !strings.Contains(text, "hk-01") {
		t.Fatalf("消息内容不对: %q", text)
	}
	if _, hasParseMode := payload["parse_mode"]; hasParseMode {
		t.Fatal("不该设置 parse_mode：节点名里的特殊字符会解析失败")
	}
}

func TestTelegramSendFailureIncludesDescription(t *testing.T) {
	withTelegramServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"ok":false,"description":"chat not found"}`))
	})

	tg := NewTelegram("123456789:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw", "1")
	err := tg.Send(context.Background(), Notification{Title: "x"})
	if err == nil {
		t.Fatal("应当返回错误")
	}
	if !strings.Contains(err.Error(), "chat not found") {
		t.Fatalf("错误信息应当带上 Telegram 的说明: %v", err)
	}
}

func TestTelegramSendHonoursRetryAfter(t *testing.T) {
	withTelegramServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"ok":false,"description":"Too Many Requests","parameters":{"retry_after":7}}`))
	})

	tg := NewTelegram("123456789:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw", "1")
	err := tg.Send(context.Background(), Notification{Title: "x"})
	var retry *RetryAfterError
	if !asRetryAfter(err, &retry) {
		t.Fatalf("应当返回 RetryAfterError: %v", err)
	}
	if retry.After != 7*time.Second {
		t.Fatalf("retry_after = %s，期望 7s", retry.After)
	}
}

// 安全约束：Token 出现在 URL 里，任何错误信息都不能把它带出去。
func TestTelegramErrorsNeverLeakToken(t *testing.T) {
	const token = "123456789:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw"

	// 关掉的端口 → 传输层错误。
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := ts.URL
	ts.Close()
	previous := telegramAPIBase
	telegramAPIBase = url
	defer func() { telegramAPIBase = previous }()

	tg := NewTelegram(token, "1")
	err := tg.Send(context.Background(), Notification{Title: "x"})
	if err == nil {
		t.Fatal("连不上时应当报错")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("错误信息泄露了 Token: %v", err)
	}
}

func TestTelegramNotConfigured(t *testing.T) {
	tg := NewTelegram("", "")
	if tg.Configured() {
		t.Fatal("缺配置时 Configured 应当为假")
	}
	if err := tg.Send(context.Background(), Notification{}); err == nil {
		t.Fatal("未配置时发送应当报错")
	}
}
