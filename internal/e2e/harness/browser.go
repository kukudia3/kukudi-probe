package harness

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"sync"
	"testing"
	"time"
)

// CaptureHandler 把服务端日志留在内存里。
//
// 浏览器路径的第一步是"从服务端日志里读一次性初始化码"，
// 所以这些用例顺带验证了这个码真的会出现在日志里。
type CaptureHandler struct {
	mu      sync.Mutex
	records []slog.Record
	attrs   [][]slog.Attr
}

func (h *CaptureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *CaptureHandler) Handle(_ context.Context, r slog.Record) error {
	var attrs []slog.Attr
	r.Attrs(func(a slog.Attr) bool {
		attrs = append(attrs, a)
		return true
	})
	h.mu.Lock()
	h.records = append(h.records, r)
	h.attrs = append(h.attrs, attrs)
	h.mu.Unlock()
	return nil
}

func (h *CaptureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h *CaptureHandler) WithGroup(string) slog.Handler { return h }

func (h *CaptureHandler) AttrValue(key string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, attrs := range h.attrs {
		for _, a := range attrs {
			if a.Key == key {
				return a.Value.String()
			}
		}
	}
	return ""
}

// Browser 是一个带 Cookie 的最小 HTTP 客户端，模拟前端的行为。
type Browser struct {
	t      *testing.T
	Base   string
	Client *http.Client
	CSRF   string
}

func NewBrowser(t *testing.T, base string) *Browser {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("创建 Cookie jar: %v", err)
	}
	return &Browser{t: t, Base: base, Client: &http.Client{Jar: jar, Timeout: 10 * time.Second}}
}

func (b *Browser) Do(method, path string, payload any, withCSRF bool) (int, map[string]any) {
	b.t.Helper()
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			b.t.Fatalf("序列化请求体: %v", err)
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, b.Base+path, body)
	if err != nil {
		b.t.Fatalf("构造请求: %v", err)
	}
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if withCSRF {
		req.Header.Set("X-CSRF-Token", b.CSRF)
	}
	resp, err := b.Client.Do(req)
	if err != nil {
		b.t.Fatalf("请求 %s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	out := map[string]any{}
	raw, _ := io.ReadAll(resp.Body)
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return resp.StatusCode, out
}

func (b *Browser) SSE(path string) (chan map[string]any, func()) {
	b.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.Base+path, nil)
	if err != nil {
		cancel()
		b.t.Fatalf("构造 SSE 请求: %v", err)
	}
	resp, err := b.Client.Do(req)
	if err != nil {
		cancel()
		b.t.Fatalf("连接 SSE: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		cancel()
		b.t.Fatalf("SSE 状态码 = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		_ = resp.Body.Close()
		cancel()
		b.t.Fatalf("SSE Content-Type = %q", ct)
	}

	events := make(chan map[string]any, 32)
	go func() {
		defer close(events)
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var payload map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &payload); err != nil {
				continue
			}
			events <- payload
		}
	}()
	return events, func() {
		cancel()
		_ = resp.Body.Close()
	}
}

// WaitSetupCode 等服务端把一次性初始化码打进日志。
func WaitSetupCode(t *testing.T, logs *CaptureHandler) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if code := logs.AttrValue("setup_code"); code != "" {
			if len(code) != 12 {
				t.Fatalf("初始化码 = %q，期望 12 个字符", code)
			}
			return code
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("日志里没有出现初始化码")
	return ""
}

// AdminPass 是 SetupAdmin 创建管理员时用的密码。
//
// 为什么把它导出来：有几条用例要在"别处"（另一个 HTTP 会话 / 反代里的标记请求）
// 用管理员身份登录，用的必须是同一个字符串 —— 否则那几条用例测的就成了
// "另一个账号"，而不是"同一个管理员的会话在别处失效"。以前它叫 frontStatePass
// 且写死在 frontstate 那个文件里，只是靠注释与这里对齐；现在两边共用这一个常量。
const AdminPass = "a-very-good-password"

// SetupAdmin 走一遍"输入初始化码 → 创建管理员 → 拿到 CSRF Token"。
func SetupAdmin(t *testing.T, br *Browser, code string) {
	t.Helper()
	status, body := br.Do(http.MethodPost, "/api/v1/setup", map[string]any{
		"code": code, "username": "admin", "password": AdminPass,
	}, false)
	if status != http.StatusOK {
		t.Fatalf("初始化失败: %d %v", status, body)
	}
	br.CSRF, _ = body["csrf_token"].(string)
	if br.CSRF == "" {
		t.Fatal("初始化后没有 CSRF Token")
	}
}

// CreateNodeViaAPI 建节点并返回 ID 与一次性 Token。
func CreateNodeViaAPI(t *testing.T, br *Browser, name string) (int64, string) {
	t.Helper()
	status, body := br.Do(http.MethodPost, "/api/v1/nodes", map[string]any{
		"name": name, "group_name": "测试", "region": "HK", "interval_sec": 1,
	}, true)
	if status != http.StatusCreated {
		t.Fatalf("创建节点失败: %d %v", status, body)
	}
	token, _ := body["token"].(string)
	if token == "" {
		t.Fatal("创建节点没有返回 Token")
	}
	node, _ := body["node"].(map[string]any)
	id, _ := node["id"].(float64)
	return int64(id), token
}
