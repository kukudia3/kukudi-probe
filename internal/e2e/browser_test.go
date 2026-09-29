package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// captureHandler 把服务端日志留在内存里。
//
// 浏览器路径的第一步是"从服务端日志里读一次性初始化码"，
// 所以这些用例顺带验证了这个码真的会出现在日志里。
type captureHandler struct {
	mu      sync.Mutex
	records []slog.Record
	attrs   [][]slog.Attr
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
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

func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

func (h *captureHandler) attrValue(key string) string {
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

// browser 是一个带 Cookie 的最小 HTTP 客户端，模拟前端的行为。
type browser struct {
	t      *testing.T
	base   string
	client *http.Client
	csrf   string
}

func newBrowser(t *testing.T, base string) *browser {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("创建 Cookie jar: %v", err)
	}
	return &browser{t: t, base: base, client: &http.Client{Jar: jar, Timeout: 10 * time.Second}}
}

func (b *browser) do(method, path string, payload any, withCSRF bool) (int, map[string]any) {
	b.t.Helper()
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			b.t.Fatalf("序列化请求体: %v", err)
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, b.base+path, body)
	if err != nil {
		b.t.Fatalf("构造请求: %v", err)
	}
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if withCSRF {
		req.Header.Set("X-CSRF-Token", b.csrf)
	}
	resp, err := b.client.Do(req)
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

func (b *browser) sse(path string) (chan map[string]any, func()) {
	b.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.base+path, nil)
	if err != nil {
		cancel()
		b.t.Fatalf("构造 SSE 请求: %v", err)
	}
	resp, err := b.client.Do(req)
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

// waitSetupCode 等服务端把一次性初始化码打进日志。
func waitSetupCode(t *testing.T, logs *captureHandler) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if code := logs.attrValue("setup_code"); code != "" {
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

// setupAdmin 走一遍"输入初始化码 → 创建管理员 → 拿到 CSRF Token"。
func setupAdmin(t *testing.T, br *browser, code string) {
	t.Helper()
	status, body := br.do(http.MethodPost, "/api/v1/setup", map[string]any{
		"code": code, "username": "admin", "password": "a-very-good-password",
	}, false)
	if status != http.StatusOK {
		t.Fatalf("初始化失败: %d %v", status, body)
	}
	br.csrf, _ = body["csrf_token"].(string)
	if br.csrf == "" {
		t.Fatal("初始化后没有 CSRF Token")
	}
}

// createNodeViaAPI 建节点并返回 ID 与一次性 Token。
func createNodeViaAPI(t *testing.T, br *browser, name string) (int64, string) {
	t.Helper()
	status, body := br.do(http.MethodPost, "/api/v1/nodes", map[string]any{
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

// TestBrowserSeesLiveData 走完整条"用户在浏览器里能看到什么"的链路：
// 日志里读初始化码 → 初始化管理员 → 登录态 → 建节点拿 Token →
// 起真 Agent → SSE 收到这个节点的实时数据。
func TestBrowserSeesLiveData(t *testing.T) {
	logs := &captureHandler{}
	h := startServerWithLogger(t, "127.0.0.1:0", filepath.Join(t.TempDir(), "probe.db"), slog.New(logs))

	br := newBrowser(t, "http://"+h.addr)

	// 初始化前，前端会看到 needs_setup=true。
	status, body := br.do(http.MethodGet, "/api/v1/session", nil, false)
	if status != http.StatusOK || body["needs_setup"] != true {
		t.Fatalf("初始化前会话状态不对: %d %v", status, body)
	}

	setupAdmin(t, br, waitSetupCode(t, logs))
	nodeID, token := createNodeViaAPI(t, br, "e2e-browser")

	client, _ := newClient(t, "http://"+h.addr, token)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = client.Run(ctx) }()

	events, stop := br.sse("/api/v1/stream")
	defer stop()

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case payload, ok := <-events:
			if !ok {
				t.Fatal("SSE 流已关闭")
			}
			nodes, _ := payload["nodes"].([]any)
			for _, raw := range nodes {
				n, _ := raw.(map[string]any)
				if id, _ := n["id"].(float64); int64(id) != nodeID {
					continue
				}
				if n["status"] != "online" {
					continue
				}
				memPct, _ := n["mem_pct"].(float64)
				if memPct < 51 || memPct > 52 {
					t.Fatalf("内存使用率 = %v，与 /proc 快照不符", memPct)
				}
				if n["iface"] != "eth0" || n["uptime_sec"] != float64(1234567) {
					t.Fatalf("节点视图内容不对: %v", n)
				}
				summary, _ := payload["summary"].(map[string]any)
				if summary["online"] != float64(1) || summary["total"] != float64(1) {
					t.Fatalf("汇总不对: %v", summary)
				}
				return
			}
		case <-time.After(500 * time.Millisecond):
		}
	}
	t.Fatal("20 秒内没有等到节点的实时数据")
}

// TestNodeListHidesTokenFromBrowser 验证列表接口不会把 Token 之类的敏感字段漏给前端。
func TestNodeListHidesTokenFromBrowser(t *testing.T) {
	logs := &captureHandler{}
	h := startServerWithLogger(t, "127.0.0.1:0", filepath.Join(t.TempDir(), "probe.db"), slog.New(logs))

	br := newBrowser(t, "http://"+h.addr)
	setupAdmin(t, br, waitSetupCode(t, logs))
	createNodeViaAPI(t, br, "hide-01")

	status, body := br.do(http.MethodGet, "/api/v1/nodes", nil, false)
	if status != http.StatusOK {
		t.Fatalf("查询节点失败: %d", status)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("序列化响应: %v", err)
	}
	for _, needle := range []string{"pba_", "token_hash", "token"} {
		if bytes.Contains(raw, []byte(needle)) {
			t.Fatalf("列表响应里出现了敏感内容 %q: %s", needle, raw)
		}
	}
}
