package alert

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
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

// ---------------------------------------------------------------- 05-A-2 / 05-A-3

// pathEscapedForm 按 RFC 3986 的路径口径转义：未保留字符（A-Za-z0-9-_.~）与
// 路径里允许的保留字符（$&+,/:;=@）原样留下，其余写成 %XX。
//
// 这是**独立于被测实现**的转义口径（不复用 tokenForms）：net/url 的 encodePath
// 对普通特殊字符就是这么转的，所以对"未保留字符 + 若干特殊字符"组成的 Token，
// 它给出的正是错误文本里那个形态。
func pathEscapedForm(s string) string {
	const hex = "0123456789ABCDEF"
	const keep = "$&+,/:;=@"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '-' || c == '_' || c == '.' || c == '~' ||
			('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') || ('0' <= c && c <= '9') ||
			strings.IndexByte(keep, c) >= 0 {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&0x0f])
	}
	return b.String()
}

// roundTripFunc 让用例能自己造一个"和 net/http 一模一样"的传输层错误。
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestTelegramErrorsNeverLeakEscapedToken 是 05-A-2 的用例。
//
// 病根：*url.Error 里放的是 req.URL.String()，路径是按 net/url 的 encodePath 转义
// 过的，而 scrubToken 只替换"原样"的 Token —— 含空格/引号/非 ASCII 的畸形 Token
// （ValidateTelegramConfig 只校验形状，不限字符集，所以真能存进库）就以转义形态
// 留在错误文本里，进日志、也进 POST /api/v1/settings/telegram/test 的响应体。
//
// 用例抓的是**真发给 api.telegram.org 的那个 URL**（httptest/自定义传输层接住），
// 而不是拍脑袋构造的字符串：先证明"URL 里确实是转义形态"，再断言错误文本干净。
func TestTelegramErrorsNeverLeakEscapedToken(t *testing.T) {
	secret := "AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw"
	cases := []struct {
		name  string
		token string
	}{
		{"空格", "123456789:" + secret[:3] + " " + secret[3:]},
		{"双引号", "123456789:" + secret[:3] + `"` + secret[3:]},
		{"非 ASCII", "123456789:" + secret[:3] + "路" + secret[3:]},
		{"反引号", "123456789:" + secret[:3] + "`" + secret[3:]},
		// 同时含 "/" 与空格：只按 url.PathEscape 替换会漏掉真正的那个形态
		// （PathEscape 把 "/" 转义成 %2F，而 URL 路径里的 encodePath 不转义它）。
		{"斜杠加空格", "123456789:" + secret[:3] + "/ " + secret[3:]},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// 前置条件：这种 Token 能过保存时的校验（否则线上存不下来）。
			if err := ValidateTelegramConfig(c.token, "1"); err != nil {
				t.Fatalf("前置条件不成立：畸形 Token 过不了校验: %v", err)
			}

			var requested string
			client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				requested = req.URL.String()
				// net/http 在传输层失败时包装出来的就是这个形状。
				return nil, &url.Error{
					Op: req.Method, URL: req.URL.String(),
					Err: errors.New("dial tcp 127.0.0.1:1: connect: connection refused"),
				}
			})}
			tg := NewTelegram(c.token, "1")
			tg.client = client

			err := tg.Send(context.Background(), Notification{Title: "x"})
			if err == nil {
				t.Fatal("传输层失败时应当报错")
			}
			// 前置条件：Token 在请求 URL 里**确实是转义形态**（这就是泄漏面）。
			if !strings.Contains(requested, pathEscapedForm(c.token)) {
				t.Fatalf("前置条件不成立：请求 URL 里没有转义形态的 Token\nURL: %s", requested)
			}

			message := err.Error()
			for _, leak := range []string{c.token, pathEscapedForm(c.token)} {
				if strings.Contains(message, leak) {
					t.Fatalf("错误信息泄露了 Token（形态 %q）：%s", leak, message)
				}
			}
			if !strings.Contains(message, "***") {
				t.Fatalf("Token 应当被替换成 ***：%s", message)
			}
			// 与 Token 无关的失败原因必须留着：它是管理员唯一能看到的线索。
			if !strings.Contains(message, "connection refused") {
				t.Fatalf("清洗把失败原因一起抹掉了：%s", message)
			}
		})
	}

	// 反向对照：正常形状的 Token（真实 Bot Token 的字符集 [0-9]+:[A-Za-z0-9_-]{35}）
	// 在路径里**一个字符都不用转义**：请求 URL 与错误文本的处理与修之前逐字节一致。
	t.Run("正常形状不变", func(t *testing.T) {
		const token = "123456789:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw"
		var requested string
		client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			requested = req.URL.String()
			return nil, &url.Error{Op: req.Method, URL: req.URL.String(), Err: errors.New("boom")}
		})}
		tg := NewTelegram(token, "1")
		tg.client = client
		err := tg.Send(context.Background(), Notification{Title: "x"})
		if err == nil {
			t.Fatal("传输层失败时应当报错")
		}
		if !strings.Contains(requested, "/bot"+token+"/sendMessage") {
			t.Fatalf("正常 Token 的请求 URL 变了：%s", requested)
		}
		message := err.Error()
		if strings.Contains(message, token) {
			t.Fatalf("错误信息泄露了 Token：%s", message)
		}
		if !strings.Contains(message, "***") {
			t.Fatalf("Token 应当被替换成 ***：%s", message)
		}
	})
}

// TestTelegramRetryAfterIsCapped 是 05-A-3 的一半：封顶值本身。
func TestTelegramRetryAfterIsCapped(t *testing.T) {
	// 对端要求 2^31-1 秒 ≈ 68 年。
	withTelegramServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"ok":false,"description":"Too Many Requests","parameters":{"retry_after":2147483647}}`))
	})

	tg := NewTelegram("123456789:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw", "1")
	err := tg.Send(context.Background(), Notification{Title: "x"})
	var retry *RetryAfterError
	if !asRetryAfter(err, &retry) {
		t.Fatalf("应当返回 RetryAfterError: %v", err)
	}
	if retry.After != maxRetryAfter {
		t.Fatalf("封顶后的等待 = %s，期望 %s", retry.After, maxRetryAfter)
	}
	// 日志里必须看得出"消息被压住了"：对端要求了多少、我们封到多少。
	for _, want := range []string{"596523h14m7s", maxRetryAfter.String(), "已封顶"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息里缺少 %q（运维只能靠它看出告警被压住了）：%v", want, err)
		}
	}
}

// TestTelegramHugeRetryAfterDoesNotStallThePipeline 是 05-A-3 的另一半：端到端。
//
// 等待发生在**唯一的**发送 worker 里（内部重试循环），所以"封顶"必须真的体现在
// 流水线上，而不只是错误对象里的一个数。这里把封顶值调小（否则要等 5 分钟），让
// 对端连回两次：第一次 429 + 68 年，第二次成功。worker 若真按对端要求睡下去，
// 第二次请求永远不会发生 —— 反向验证时红的正是下面那条 deadline 断言。
func TestTelegramHugeRetryAfterDoesNotStallThePipeline(t *testing.T) {
	previous := maxRetryAfter
	maxRetryAfter = 50 * time.Millisecond
	t.Cleanup(func() { maxRetryAfter = previous })

	var calls atomic.Int32
	withTelegramServer(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"ok":false,"description":"Too Many Requests","parameters":{"retry_after":2147483647}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	})

	opts := DefaultDispatcherOptions()
	opts.Coalesce = 0
	opts.RateLimit = 0
	opts.Retries = 2
	opts.RetryBase = time.Millisecond
	d := NewDispatcher(slog.New(slog.DiscardHandler),
		[]Notifier{NewTelegram("123456789:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw", "1")}, opts)
	startDispatcher(t, d)

	started := time.Now()
	d.Enqueue(testNotification("hk-01"))

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && calls.Load() < 2 {
		time.Sleep(5 * time.Millisecond)
	}
	if got := calls.Load(); got < 2 {
		t.Fatalf("%s 内只发了 %d 次请求：唯一发送 worker 被 retry_after 按住了", time.Since(started), got)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("重试等了 %s，远超封顶值 %s", elapsed, maxRetryAfter)
	}
}
