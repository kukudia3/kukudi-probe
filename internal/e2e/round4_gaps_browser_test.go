package e2e

// 第四轮「缺口补齐」的浏览器用例：第二轮安全审计 SECURITY-AUDIT-ROUND2.md §7 里
// 被判「本机可补、但当时没做」的浏览器侧缺口（逐条编号见 _audit/ROUND3-VERIFY-1.md
// §3.6 :327-339 与 §3.7 :340-349）。
//
// 本文件只加测试：产品代码一行未改（web/ 与 internal/** 的非 _test.go 文件逐字节未动）。
// 结论与反向验证记录在 D:\DEEPSEEK\_audit\ROUND4-GAPS.md。

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// seedNoJSProfile 造一个"浏览器里把 JavaScript 设为阻止"的画像目录。
//
// 这台机器上试过、**不生效**的做法（原文见 ROUND4-GAPS.md）：
//   - `--disable-javascript`：Chrome 154 直接忽略它，页面照样跑了 JS（实测打到了
//     /api/v1/session），dump 出来的 `#view-login` 已经没有 hidden；
//   - `--blink-settings=scriptEnabled=false`：`--dump-dom` 变成**零字节输出**，
//     拿不到 DOM，没法下任何断言。
//
// 生效的是**内容设置**：Chrome 把"阻止 JavaScript"记在画像的
// `Default/Preferences` 里（`profile.default_content_setting_values.javascript = 2`）。
// 预先把这个文件写好再启动，就是"用户自己把 JS 关掉"的那一档，而且 dump-dom 正常。
func seedNoJSProfile(t *testing.T, dir string) string {
	t.Helper()
	profile := filepath.Join(dir, "profile")
	if err := os.MkdirAll(filepath.Join(profile, "Default"), 0o755); err != nil {
		t.Fatalf("创建 Chrome 画像目录: %v", err)
	}
	const prefs = `{"profile":{"default_content_setting_values":{"javascript":2}}}`
	if err := os.WriteFile(filepath.Join(profile, "Default", "Preferences"), []byte(prefs), 0o644); err != nil {
		t.Fatalf("写入 Chrome 画像 Preferences: %v", err)
	}
	return profile
}

// dumpDOM 用无头 Chrome 打开页面并 dump 最终 DOM。profile 由调用方给（见 seedNoJSProfile）。
func dumpDOM(t *testing.T, chrome, profile string, extraArgs []string, pageURL string) string {
	t.Helper()
	args := []string{
		"--headless=new",
		"--no-proxy-server",
		"--disable-gpu",
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-extensions",
		"--window-size=1200,900",
		"--user-data-dir=" + profile,
		"--dump-dom",
	}
	args = append(args, extraArgs...)
	args = append(args, pageURL)

	cmd := exec.Command(chrome, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("Chrome 退出失败: %v\n%s", err, tail(stderr.String(), 800))
	}
	return stdout.String()
}

// openingTag 从 dump 出来的 DOM 里取某个 id 元素的**起始标签**（含属性）。
//
// 为什么要按 id 找而不是整串搜：`hidden` 在别处也会出现（设置页那十几个 pane 都带），
// 只有"这个元素自己的起始标签里有没有 hidden"才是这条用例要判的事。
func openingTag(t *testing.T, dom, id string) string {
	t.Helper()
	needle := `id="` + id + `"`
	i := strings.Index(dom, needle)
	if i < 0 {
		t.Fatalf("DOM 里没有 id=%q 的元素（页面没加载成功？）\n%s", id, tail(dom, 600))
	}
	start := strings.LastIndex(dom[:i], "<")
	end := strings.Index(dom[i:], ">")
	if start < 0 || end < 0 {
		t.Fatalf("id=%q 的起始标签取不出来", id)
	}
	return dom[start : i+end+1]
}

// TestFormsStayUnreachableWithJavaScriptDisabled 覆盖 06-1（`X1` 的"禁用 JS 实测"）。
//
// 审计的原文：「`X1` 的可达路径：对抗者已证明**不可达并关闭**（refuted），
// 但"未在真实浏览器里禁用 JS 实测"这一步始终没做。」
// 对抗者的证明是纯代码推理：三个 `<form>` 都没有 `method`/`action` ⇒ HTML 默认 GET；
// 但"表单可见"与"submit 处理器已绑上"互斥（`<section>` 出厂全带 `hidden`，可见只可能
// 发生在 `bind()` 之后）⇒ 拿不到"处理器没绑上还能提交"的场景。
//
// 本用例把那个推理**在真 Chrome 里跑一遍**，并带两个对照，免得它变成永远绿的用例：
//
//	① JS 被阻止：三个容器（`#view-setup` / `#view-login` / `#dlg-node`）必须仍然带
//	   `hidden`（节点表单那个是 `<dialog>`，未 `showModal()` ⇒ 也没有 `open`），
//	   并且这一趟里**没有任何带查询串的请求**打到服务端 —— 表单若真被提交，URL 上会带
//	   `?username=…&password=…`，这是唯一能证明"提交没有发生"的地方（DOM 上看不出来）；
//	② 第二个形态：**JS 开着、但两个脚本都取不到**（反代对 /app.js、/chart.js 返 404）
//	   —— 这是"脚本没加载成功"的现实形态，同样必须看不见也提交不了；
//	③ **对照跑**（同一个页面、脚本正常）：必须至少有一个视图**摘掉** `hidden`
//	   —— 证明①②不是"页面本来就长这样/根本没加载"；
//	④ 全程不许出现 `username=` / `password=` / `code=` 这类凭据进 URL 的请求。
//
// 反向验证（已实测，见报告）：在副本的 web/index.html 里把 `#view-login` 的 `hidden`
// 去掉（模拟"JS 缺席也能看见并提交表单"）→ ①红在"JS 被阻止之后 #view-login 仍须带 hidden"。
func TestFormsStayUnreachableWithJavaScriptDisabled(t *testing.T) {
	chrome := findChrome()
	if chrome == "" {
		t.Skip("找不到 Chrome：设置 PROBE_CHROME 或把它装到默认位置后这条用例会自动跑起来")
	}

	// 真服务端 + 全新数据库：前端会落到"初始化"分支（needs_setup=true），
	// 也就是 X1 最关心的那个形态（页面上确实有一张要填密码的表单）。
	h := startServer(t, "127.0.0.1:0", filepath.Join(t.TempDir(), "probe.db"))

	// 本地反代：记录每一个打进来的请求（含查询串），并且能按开关让两个脚本 404。
	base, err := url.Parse("http://" + h.addr)
	if err != nil {
		t.Fatalf("解析服务端地址: %v", err)
	}
	rp := httputil.NewSingleHostReverseProxy(base)
	var blockScripts atomic.Bool
	var mu sync.Mutex
	var seen []string
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.RequestURI())
		mu.Unlock()
		if blockScripts.Load() && (r.URL.Path == "/app.js" || r.URL.Path == "/chart.js") {
			http.NotFound(w, r)
			return
		}
		rp.ServeHTTP(w, r)
	}))
	defer front.Close()

	takeSeen := func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := append([]string(nil), seen...)
		seen = nil
		return out
	}
	assertHidden := func(dom, phase string) {
		t.Helper()
		for _, id := range []string{"view-setup", "view-login"} {
			if tag := openingTag(t, dom, id); !strings.Contains(tag, "hidden") {
				t.Errorf("%s：#%s 仍然必须带 hidden（表单不可见才谈得上不可提交），实际起始标签：%s",
					phase, id, tag)
			}
		}
		dialog := openingTag(t, dom, "dlg-node")
		if strings.Contains(dialog, " open") || strings.HasSuffix(dialog, " open>") {
			t.Errorf("%s：节点对话框不该是打开状态，实际起始标签：%s", phase, dialog)
		}
		// 表单本身还在文档里（不是"页面没加载"）——这样断言才有判别力。
		if !strings.Contains(dom, `id="form-login"`) || !strings.Contains(dom, `id="form-node"`) {
			t.Fatalf("%s：DOM 里连三个表单都没有，页面没加载成功，断言失去判别力\n%s", phase, tail(dom, 600))
		}
		for _, line := range takeSeen() {
			if strings.Contains(line, "?") {
				t.Errorf("%s：出现了带查询串的请求（表单默认 GET 提交的形态）：%s", phase, line)
			}
		}
	}

	// ---- ① JS 被阻止（画像内容设置） ----
	domNoJS := dumpDOM(t, chrome, seedNoJSProfile(t, t.TempDir()),
		[]string{"--virtual-time-budget=4000"}, front.URL+"/")
	assertHidden(domNoJS, "JS 被阻止")

	// ---- ② JS 开着，但两个脚本 404 ----
	blockScripts.Store(true)
	domNoScripts := dumpDOM(t, chrome, filepath.Join(t.TempDir(), "profile"),
		[]string{"--virtual-time-budget=6000"}, front.URL+"/")
	assertHidden(domNoScripts, "脚本取不到")
	blockScripts.Store(false)

	// ---- ③ 对照：同一个页面、脚本正常 ----
	domJS := dumpDOM(t, chrome, filepath.Join(t.TempDir(), "profile"),
		[]string{"--virtual-time-budget=8000"}, front.URL+"/")
	unhidden := false
	for _, id := range []string{"view-setup", "view-login", "view-home"} {
		if tag := openingTag(t, domJS, id); !strings.Contains(tag, "hidden") {
			unhidden = true
		}
	}
	if !unhidden {
		t.Fatalf("对照组（脚本正常）里三个视图全都还带着 hidden：说明这个页面根本没跑起来，" +
			"①②「仍然 hidden」也就没有判别力")
	}

	// ---- ④ 凭据不许进 URL ----
	all := append(takeSeen(), seen...)
	for _, line := range all {
		for _, needle := range []string{"username=", "password=", "code="} {
			if strings.Contains(line, needle) {
				t.Errorf("凭据进了 URL（X1 描述的那条泄漏路径）：%s", line)
			}
		}
	}
	t.Logf("JS 被阻止 / 脚本 404 / 脚本正常 三个形态都跑完；最后一段的全部请求：%v", all)
}
