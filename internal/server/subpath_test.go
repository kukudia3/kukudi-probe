package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestPanelWorksUnderSubpath 覆盖"面板挂在子路径下"的部署方式
// （例如把 nginx 的 location /probe/ 反代到本服务，并剥掉前缀）。
//
// 之前前端用的是 /app.js 与 '/api/v1/...' 这些绝对路径：子路径部署时
// HTML 能打开、但脚本与接口全 404，浏览器里就是**一片空白**（连登录框都没有）。
// 现在资源用相对路径、接口经 apiURL() 拼前缀，所以在子路径下也应当完全可用。
func TestPanelWorksUnderSubpath(t *testing.T) {
	h := newAuthHarness(t)

	// 模拟反代：/probe 前缀被剥掉后转给面板。
	//
	// 前缀写 "/probe" 而不是 "/probe/"：StripPrefix 会原样切掉前缀字符串，
	// 带尾斜杠会让内层看到 "healthz"（没有前导斜杠）。真实反代
	// （nginx `proxy_pass http://127.0.0.1:25774/;`）转发过去的是 "/healthz"。
	sub := httptest.NewServer(http.StripPrefix("/probe", h.srv.Handler()))
	defer sub.Close()

	paths := []string{
		"/probe/",               // 首页
		"/probe/app.js",         // 脚本（相对路径引用后浏览器会请求这个）
		"/probe/chart.js",       // 图表
		"/probe/style.css",      // 样式
		"/probe/api/v1/session", // 接口
		"/probe/healthz",        // 健康检查
	}
	for _, p := range paths {
		resp, err := http.Get(sub.URL + p)
		if err != nil {
			t.Fatalf("GET %s: %v", p, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s 状态码 = %d，期望 200", p, resp.StatusCode)
		}
	}

	// 首页里的资源引用必须是相对的：否则浏览器会去请求 /app.js（不带 /probe/）而 404。
	resp, err := http.Get(sub.URL + "/probe/")
	if err != nil {
		t.Fatalf("GET /probe/: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	buf := make([]byte, 8<<10)
	n, _ := resp.Body.Read(buf)
	html := string(buf[:n])
	for _, bad := range []string{`src="/`, `href="/`} {
		if strings.Contains(html, bad) {
			t.Errorf("首页在子路径下仍有根路径引用 %q（会导致子路径部署白屏）", bad)
		}
	}

	// 接口在子路径下也必须能登录（POST，顺便验证 POST 路由也被剥掉前缀）。
	payload, err := json.Marshal(map[string]any{"username": "admin", "password": h.password})
	if err != nil {
		t.Fatalf("序列化: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, sub.URL+"/probe/api/v1/auth/login", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("构造请求: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	loginResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("子路径登录请求失败: %v", err)
	}
	defer func() { _ = loginResp.Body.Close() }()
	if loginResp.StatusCode != http.StatusOK {
		t.Fatalf("子路径下的登录接口状态码 = %d，期望 200", loginResp.StatusCode)
	}
	var loginBody map[string]any
	if err := json.NewDecoder(loginResp.Body).Decode(&loginBody); err != nil {
		t.Fatalf("解析登录响应: %v", err)
	}
	if _, ok := loginBody["csrf_token"]; !ok {
		t.Fatalf("登录响应缺少 csrf_token: %v", loginBody)
	}
}
