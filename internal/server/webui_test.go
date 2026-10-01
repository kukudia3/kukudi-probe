package server

import (
	"io/fs"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"probe/web"
)

func readAsset(t *testing.T, name string) string {
	t.Helper()
	data, err := fs.ReadFile(web.FS, name)
	if err != nil {
		t.Fatalf("读取前端资源 %s: %v", name, err)
	}
	if len(data) == 0 {
		t.Fatalf("前端资源 %s 为空", name)
	}
	return string(data)
}

var (
	htmlIDPattern = regexp.MustCompile(`id="([^"]+)"`)
	jsIDPattern   = regexp.MustCompile(`\$\('([^']+)'\)`)
)

// 前端必须能在**子路径**下工作（例如 https://example.com/probe/）。
//
// 曾经的写法（HTML 用 /app.js、JS 用 '/api/v1/...'）只在域名根路径下正确：
// 子路径部署时 HTML 能打开，但资源与接口全部 404 —— 表现就是"一片空白，
// 连登录框都没有"。所以：资源一律相对路径，接口一律经 apiURL() 拼前缀。
func TestFrontendWorksUnderSubpath(t *testing.T) {
	html := readAsset(t, "index.html")
	js := readAsset(t, "app.js")

	for _, bad := range []string{`href="/`, `src="/`} {
		if strings.Contains(html, bad) {
			t.Errorf("index.html 里出现了以根路径开头的资源引用 %q；子路径部署会 404", bad)
		}
	}
	for _, need := range []string{`href="style.css"`, `src="app.js"`, `src="chart.js"`} {
		if !strings.Contains(html, need) {
			t.Errorf("index.html 缺少相对路径引用 %s", need)
		}
	}

	if !strings.Contains(js, "function apiURL(") || !strings.Contains(js, "var BASE = (function ()") {
		t.Fatal("app.js 应当有 apiURL()/BASE 来计算部署前缀")
	}
	if !strings.Contains(js, "fetch(apiURL(path)") {
		t.Error("api() 必须用 apiURL() 拼前缀")
	}
	// 所有网络调用的出口只有两个：api() 与 EventSource。
	// 只要这两处都经过 apiURL()，绝对路径就不会漏出去。
	if regexp.MustCompile(`fetch\(\s*['"]`).MatchString(js) {
		t.Error("app.js 里有绕过 api() 的裸 fetch（不会拼部署前缀）")
	}
	for _, match := range regexp.MustCompile(`EventSource\(([^)]*)\)`).FindAllStringSubmatch(js, -1) {
		if !strings.Contains(match[1], "apiURL(") {
			t.Errorf("EventSource 的参数必须经 apiURL()：%s", match[1])
		}
	}
}

// 前端靠 id 取元素；id 拼错在浏览器里就是一片空白，所以这里自动挡一道。
func TestFrontendReferencesExistingElementIDs(t *testing.T) {
	html := readAsset(t, "index.html")
	js := readAsset(t, "app.js")

	known := map[string]bool{}
	for _, match := range htmlIDPattern.FindAllStringSubmatch(html, -1) {
		known[match[1]] = true
	}
	if len(known) == 0 {
		t.Fatal("index.html 里没有解析到任何 id")
	}

	// main() 里批量注册的 id 列表 + 直接调用 $('...') 的地方，都要存在。
	referenced := 0
	for _, match := range jsIDPattern.FindAllStringSubmatch(js, -1) {
		referenced++
		if !known[match[1]] {
			t.Errorf("app.js 引用了不存在的元素 id %q", match[1])
		}
	}
	if referenced < 10 {
		t.Fatalf("只解析到 %d 个 id 引用，正则可能没匹配上", referenced)
	}
}

// el 的键必须能从某个 HTML id 推出来（原样，或短横线转驼峰）。
//
// 曾经的写法是 main() 里手写一份驼峰名单再去 getElementById，而 HTML 的 id 全是
// 短横线式：89 个键里 81 个查成 null，bind() 第一行就抛 TypeError，refreshSession()
// 永远执行不到，所有视图一直 hidden —— 页面全白，而且除了浏览器控制台毫无线索。
// 现在 el 由 DOM 自动登记（见 app.js 的 camelID），这个测试守住这个不变量。
func TestFrontendElementKeysResolveToHTMLIDs(t *testing.T) {
	html := readAsset(t, "index.html")
	js := readAsset(t, "app.js")

	ids := map[string]bool{}
	for _, match := range htmlIDPattern.FindAllStringSubmatch(html, -1) {
		ids[match[1]] = true
	}
	if len(ids) == 0 {
		t.Fatal("index.html 里没有解析到任何 id")
	}
	keys := map[string]bool{}
	for id := range ids {
		keys[id] = true
		keys[camelizeID(id)] = true
	}

	refs := map[string]bool{}
	for _, match := range regexp.MustCompile(`\bel\.([A-Za-z_$][\w$]*)`).FindAllStringSubmatch(js, -1) {
		refs[match[1]] = true
	}
	for _, match := range regexp.MustCompile(`\bel\['([^']+)'\]`).FindAllStringSubmatch(js, -1) {
		refs[match[1]] = true
	}
	if len(refs) < 50 {
		t.Fatalf("只解析到 %d 个 el 引用，正则可能没匹配上", len(refs))
	}
	for ref := range refs {
		if !keys[ref] {
			t.Errorf("el.%s 找不到对应的 HTML id（原样和短横线转驼峰都对不上）", ref)
		}
	}

	// 光有上面这条还不够：手写名单里的驼峰键同样"能被推导出来"，但拿它去
	// getElementById 是查不到的 —— 这正是原来那版翻车的方式。所以再钉一条：
	// el 必须从 DOM 自动登记，不能退回手写 id 名单。
	if !regexp.MustCompile(`querySelectorAll\(\s*['"]\[id\]['"]\s*\)`).MatchString(js) {
		t.Error("app.js 必须用 querySelectorAll('[id]') 从 DOM 自动登记 el；手写 id 名单会与 HTML 脱节")
	}
}

// camelizeID 与 app.js 里的 camelID 保持同一套规则。
func camelizeID(id string) string {
	return regexp.MustCompile(`-([a-z0-9])`).ReplaceAllStringFunc(id, func(m string) string {
		return strings.ToUpper(m[1:])
	})
}

// 设计约束：前端只用 DOM API 渲染数据，不用 innerHTML（防 XSS）。
func TestFrontendAvoidsInnerHTML(t *testing.T) {
	for _, name := range []string{"app.js", "chart.js"} {
		js := readAsset(t, name)
		for _, forbidden := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval("} {
			if strings.Contains(js, forbidden) {
				t.Errorf("%s 里出现了被禁止的 %s", name, forbidden)
			}
		}
	}
}

// 设计约束：页面不加载任何外部资源、不连接第三方服务。
func TestFrontendHasNoExternalResources(t *testing.T) {
	// app.js 唯一的例外：新建节点对话框要给出完整的安装命令，用户得把它复制到
	// VPS 上执行，所以那里必须是一个真实的 https 地址。它是**给用户看的文本**，
	// 不是页面加载的资源。先把这一段摘掉再查，其余任何 https:// 仍然算违规。
	const installerURL = `https://raw.githubusercontent.com/`

	files := []string{"index.html", "style.css", "app.js", "chart.js"}
	for _, name := range files {
		content := strings.ReplaceAll(readAsset(t, name), installerURL, "")
		for _, bad := range []string{"http://", "https://", "//cdn", "fonts.googleapis", "unpkg.com", "jsdelivr"} {
			if strings.Contains(content, bad) {
				t.Errorf("%s 里出现了外部引用 %q（设计要求零外链）", name, bad)
			}
		}
	}

	// 例外只许出现在 app.js 的安装命令里，别把地址抄到其它前端文件去。
	for _, name := range []string{"index.html", "style.css", "chart.js"} {
		if strings.Contains(readAsset(t, name), installerURL) {
			t.Errorf("%s 里不该出现安装脚本地址（那是 app.js 的对话框专用的）", name)
		}
	}
}

// api() 的第二个参数是 options 对象；写成字符串会静默退化成 GET，属于"看不出错"的 bug。
func TestFrontendUsesApiOptionsObject(t *testing.T) {
	js := readAsset(t, "app.js")
	bad := regexp.MustCompile(`api\([^)\n]*,\s*'[A-Z]+'`)
	for _, match := range bad.FindAllString(js, -1) {
		t.Errorf("api() 第二个参数应当是 { method: ... }，实际写成：%s", match)
	}
}

// 前端必须在切到后台时断开实时流：50 节点全变时每秒约 40 KB，
// 后台标签页一直收是纯粹的流量浪费（Phase 11 的结论）。
func TestFrontendPausesStreamWhenHidden(t *testing.T) {
	js := readAsset(t, "app.js")
	if !strings.Contains(js, "visibilitychange") {
		t.Error("app.js 应当监听 visibilitychange 并在后台断开实时流")
	}
	if !strings.Contains(js, "stopStream()") {
		t.Error("app.js 里没有找到 stopStream 的调用")
	}
}

// 图表引擎必须先于 app.js 加载，否则详情页会直接报错。
func TestFrontendLoadsChartBeforeApp(t *testing.T) {
	html := readAsset(t, "index.html")
	// 资源用相对路径（子路径部署也能加载），但顺序仍然是硬要求：
	// chart.js 提供 window.ProbeChart，app.js 启动时会用到它。
	chartAt := strings.Index(html, `src="chart.js"`)
	appAt := strings.Index(html, `src="app.js"`)
	if chartAt < 0 || appAt < 0 {
		t.Fatalf("index.html 必须同时引入 chart.js 与 app.js")
	}
	if chartAt > appAt {
		t.Fatal("chart.js 必须在 app.js 之前引入")
	}
}

// 四个视图必须是 <main> 下的平级兄弟节点，不能互相嵌套。
//
// 曾经的写法漏掉了 view-home 与 view-detail 的 </section>（到 </main> 才被
// 隐式收尾），于是 view-detail / view-audit 变成 view-home 的后代。setView()
// 切到详情页时会把 view-home 置为 hidden，而 hidden 的祖先会连后代一起隐藏
// ——表现是"点节点卡片后整页空白"，详情页与操作记录页永远打不开。
//
// 设置页也踩过同一个坑（它一度嵌在别的 <section> 里，整页打不开），
// 所以它同样必须在 depth 0。
func TestFrontendViewsAreSiblings(t *testing.T) {
	html := readAsset(t, "index.html")
	sectionTag := regexp.MustCompile(`<section([^>]*)>|</section>`)
	viewID := regexp.MustCompile(`id="(view-[a-z]+)"`)

	depth := 0
	seen := map[string]bool{}
	for _, match := range sectionTag.FindAllStringSubmatch(html, -1) {
		if match[0] == "</section>" {
			depth--
			continue
		}
		if id := viewID.FindStringSubmatch(match[1]); id != nil {
			seen[id[1]] = true
			if depth != 0 {
				t.Errorf("%s 嵌在别处（<section> 层级 %d）：父节点一旦 hidden，它会跟着消失", id[1], depth)
			}
		}
		depth++
	}
	if depth != 0 {
		t.Errorf("index.html 里有 %d 个 <section> 没有闭合", depth)
	}
	for _, name := range []string{"view-setup", "view-login", "view-home", "view-detail", "view-settings"} {
		if !seen[name] {
			t.Errorf("index.html 里没有解析到 %s", name)
		}
	}
}

// 首页必须能引导用户完成初始化与新增节点，详情页必须能画历史图。
func TestFrontendHasSetupAndNodeForms(t *testing.T) {
	html := readAsset(t, "index.html")
	for _, needle := range []string{
		`id="form-setup"`, `id="form-login"`, `id="form-node"`,
		`id="view-setup"`, `id="view-login"`, `id="view-home"`, `id="view-detail"`,
		`id="grid"`, `id="dlg-token"`, `id="token-value"`,
		// 首页总览（Phase 15）：所有机器加起来的合计，排在节点网格上方。
		`id="overview"`,
		`id="info-hardware"`, `id="info-system"`, `id="info-storage"`,
		`id="info-network"`, `id="info-traffic"`, `id="charts-resources"`,
		`id="stat-price"`, `id="stat-monthly"`, `id="stat-left"`, `id="stat-value"`,
		`id="detail-ranges"`, `id="chart-cpu"`, `id="chart-mem"`,
		`id="chart-disk"`, `id="chart-net"`, `id="chart-traffic"`, `id="detail-back"`,
		// 后台管理（Phase 9）
		`id="detail-edit"`, `id="detail-token"`, `id="detail-delete"`,
		`id="dlg-confirm"`, `id="confirm-ok"`, `id="node-title"`, `id="node-warn"`,
		`id="node-note"`, `id="node-enabled"`, `id="node-enabled-wrap"`,
		`id="audit-body"`, `id="audit-more"`, `id="audit-empty"`, `id="audit-table"`,
		`id="alert-cooldown"`, `id="alert-debounce"`, `id="server-info"`,
		`id="pw-current"`, `id="pw-new"`, `id="pw-new2"`, `id="pw-submit"`,
		// 价格与图表可见性（Phase 12）
		`id="node-price"`, `id="node-currency"`, `id="node-billing"`, `id="chart-toggles"`,
		// 设置改成整页视图（Phase 13）：左栏导航 + 右栏六栏内容。
		// 各栏的提示元素必须分开（每栏各自保存），所以 ok/error 是成对出现的。
		`id="view-settings"`, `id="settings-nav"`, `id="settings-panes"`, `id="settings-back"`,
		`id="notify-save"`, `id="notify-ok"`, `id="notify-error"`,
		`id="alert-save"`, `id="alert-ok"`, `id="alert-error"`,
		`id="dashboard-save"`, `id="dashboard-ok"`, `id="dashboard-error"`,
		`id="security-ok"`, `id="security-error"`,
		`id="settings-test"`, `id="tg-enabled"`, `id="tg-token"`, `id="tg-token-hint"`, `id="tg-chat"`,
		`id="alert-grace"`, `id="alert-recover"`,
		// 服务器列表 + 标签（Phase 16 → Phase 18 改版）：一行一台机器，行由 app.js 造，
		// HTML 里只有容器与按钮。标签没有自己的对话框了 —— 它在「新增/编辑节点」
		// 对话框里就是一个普通文本框（多个标签用 ; 分隔）。
		`id="nodes-list"`, `id="nodes-add"`, `id="nodes-empty"`, `id="nodes-error"`,
		`id="node-tags"`, `id="node-tags-hint"`,
	} {
		if !strings.Contains(html, needle) {
			t.Errorf("index.html 缺少 %s", needle)
		}
	}
}

// 设置必须是**整页视图**：左栏导航 + 右栏六栏内容，操作记录并进来当其中一栏。
//
// 这里钉死旧结构彻底不存在：留着 <dialog id="dlg-settings"> 或
// <section id="view-audit"> 的话，同一份内容会出现两套 id，而
// getElementById 只认第一个 —— 另一套永远填不上数据，且只有浏览器里能看出来。
func TestFrontendSettingsIsFullPageView(t *testing.T) {
	html := readAsset(t, "index.html")

	for _, gone := range []string{`<dialog id="dlg-settings"`, `<section id="view-audit"`} {
		if strings.Contains(html, gone) {
			t.Errorf("index.html 里还留着 %s（设置已经是整页视图，操作记录也并进了它的一栏）", gone)
		}
	}
	for _, need := range []string{
		`<section id="view-settings" hidden>`, `id="settings-nav"`, `id="settings-panes"`,
	} {
		if !strings.Contains(html, need) {
			t.Errorf("index.html 缺少 %s", need)
		}
	}

	// 七个栏名要在导航与内容里各出现一次且顺序一致：少一个就是"点进去一片空白"，
	// 多一个就是"有个按钮切不出内容"。
	want := []string{"notify", "alert", "dashboard", "nodes", "security", "server", "audit"}
	nav := regexp.MustCompile(`class="nav-item" data-pane="([a-z]+)"`).FindAllStringSubmatch(html, -1)
	panes := regexp.MustCompile(`<section class="pane" data-pane="([a-z]+)"([^>]*)>`).FindAllStringSubmatch(html, -1)
	if len(nav) != len(want) || len(panes) != len(want) {
		t.Fatalf("导航项 %d 个、内容栏 %d 个，期望各 %d 个", len(nav), len(panes), len(want))
	}
	for i, name := range want {
		if nav[i][1] != name {
			t.Errorf("第 %d 个导航项是 %q，期望 %q", i+1, nav[i][1], name)
		}
		if panes[i][1] != name {
			t.Errorf("第 %d 个内容栏是 %q，期望 %q", i+1, panes[i][1], name)
		}
		// 每一栏都要自带 hidden：漏一个就是六栏同时铺在页面上。
		if !strings.Contains(panes[i][2], "hidden") {
			t.Errorf("内容栏 %s 没有 hidden，进页面就会和别栏一起显示", name)
		}
	}

	// 导航必须是 <button>：<a href="#..."> 会同时触发浏览器跳转与 hashchange，
	// 两边各切一次视图（点一下闪两下，还可能切到别的栏）。
	if strings.Contains(html, `href="#/settings`) {
		t.Error("设置导航用了 <a href=\"#/settings...\">：会和 hashchange 打架，应当用 <button data-pane=...>")
	}
}

// 三个保存函数必须各自 PUT 自己的接口。
//
// 以前是一个「保存」按钮串行 PUT telegram → alert → charts：任何一段失败，
// 后面两段都不会发出去，而页面上只有一个提示，用户只能猜是哪段没存上。
func TestFrontendSettingsSavesPerPane(t *testing.T) {
	js := readAsset(t, "app.js")

	cases := []struct {
		fn    string
		path  string
		field string
	}{
		{"function saveNotify()", "/api/v1/settings/telegram", "bot_token: el['tg-token'].value.trim()"},
		{"function saveAlert()", "/api/v1/settings/alert", "recover_stable: el.alertRecover.value.trim()"},
		{"function saveDashboard()", "/api/v1/settings/charts", "visible: Array.prototype.filter.call("},
	}
	for _, c := range cases {
		body := funcBody(js, c.fn)
		if body == "" {
			t.Errorf("app.js 缺少 %s", c.fn)
			continue
		}
		if !strings.Contains(body, "api('"+c.path+"', { method: 'PUT', body:") {
			t.Errorf("%s 里没有 PUT %s", c.fn, c.path)
		}
		if !strings.Contains(body, c.field) {
			t.Errorf("%s 的请求体里缺少 %q", c.fn, c.field)
		}
	}

	if strings.Contains(js, "function saveSettings(") {
		t.Error("app.js 里还留着 saveSettings()：它一次 PUT 三个接口，正是要拆掉的东西")
	}

	// 各栏的提示元素必须是各自那一份：共用 #settings-error 会让一栏的报错
	// 显示在另一栏里，看起来像是那一栏出了问题。
	for _, gone := range []string{"el.settingsError", "el.settingsOk", "el.dlgSettings"} {
		if strings.Contains(js, gone) {
			t.Errorf("app.js 里还引用着已删除的 %s", gone)
		}
	}
}

// funcBody 粗略截取一个函数的函数体：从标记处到下一个"两空格缩进的右花括号"。
// 静态断言只需要判断"这几行在同一个函数里"，不必真去解析 JS。
func funcBody(js, marker string) string {
	start := strings.Index(js, marker)
	if start < 0 {
		return ""
	}
	rest := js[start:]
	if end := strings.Index(rest, "\n  }\n"); end >= 0 {
		return rest[:end]
	}
	return rest
}

// 详情页「网络信息」卡必须同时给出两个**不同含义**的地址。
//
// 曾经的标签是「出口地址」配 observed_ip，但在"Agent 与探针同一台机器 +
// Cloudflare Tunnel"的部署里，observed_ip 恒为 127.0.0.1 —— 用户看到的是一个
// 毫无意义的回环地址，而且「出口地址」这个名字会让人以为那是机器自己的公网出口。
// 现在拆成：本机地址（Agent 自报的 local_ip/local_ip6）+ 来源 IP（observed_ip）。
func TestFrontendShowsLocalIPAndSourceIP(t *testing.T) {
	js := readAsset(t, "app.js")

	for _, needle := range []string{"本机地址", "来源 IP", "node.local_ip", "node.local_ip6", "node.observed_ip"} {
		if !strings.Contains(js, needle) {
			t.Errorf("app.js 里缺少 %q（网络信息卡会少一行或读错字段）", needle)
		}
	}
	if strings.Contains(js, "出口地址") {
		t.Error("app.js 里还留着「出口地址」这个标签：它会被读成机器自己的公网出口，与「本机地址」撞车")
	}

	// 本机地址要拼成一行：v4 与 v6 各是一个字段，谁都不能丢。
	if !regexp.MustCompile(`function localIPText\(`).MatchString(js) {
		t.Fatal("app.js 应当有 localIPText() 来合并 local_ip 与 local_ip6")
	}
	if !regexp.MustCompile(`infoRow\(net, '本机地址',\s*localIPText\(node\)\)`).MatchString(js) {
		t.Error("网络信息卡应当用 infoRow(net, '本机地址', localIPText(node)) 渲染")
	}
	if !regexp.MustCompile(`infoRow\(net, '来源 IP',\s*node\.observed_ip \|\| '—'\)`).MatchString(js) {
		t.Error("网络信息卡应当用 infoRow(net, '来源 IP', node.observed_ip || '—') 渲染")
	}
	// 两个都没有时要显示 —（空白会被读成"界面没渲染出来"）。
	if !strings.Contains(js, "parts.length ? parts.join(' / ') : '—'") {
		t.Error("localIPText 应当在两个地址都为空时返回 —")
	}
}

// 详情页由三段组成：汇总排（4 格）→ 信息卡网格（5 张）→ 图表卡（1 张，共 5 张图）。
//
// 这些 id 与 data-chart 是 app.js 按名字找的：少一个 id 就是一块内容永远空白，
// 多一个或少一个 data-chart 就是"勾选框里有的图，详情页上找不到"。
// 只有浏览器控制台能看出这类问题，所以在这里按数量钉死。
func TestFrontendDetailLayoutGroupsStatsInfoAndCharts(t *testing.T) {
	html := readAsset(t, "index.html")
	css := readAsset(t, "style.css")

	// 汇总排：4 格
	wantStats := []string{"stat-price", "stat-monthly", "stat-left", "stat-value"}
	for _, id := range wantStats {
		if !strings.Contains(html, `id="`+id+`"`) {
			t.Errorf("详情页缺少汇总卡 %s", id)
		}
	}
	if n := len(regexp.MustCompile(`id="stat-[a-z]+"`).FindAllString(html, -1)); n != len(wantStats) {
		t.Errorf("汇总排应当有 %d 格，实际解析到 %d 个 stat-* id", len(wantStats), n)
	}

	// 信息卡：5 张，每张一个 <dl class="kv wide">
	wantInfo := []string{"info-hardware", "info-system", "info-storage", "info-network", "info-traffic"}
	for _, id := range wantInfo {
		if !strings.Contains(html, `id="`+id+`"`) {
			t.Errorf("详情页缺少信息卡 %s", id)
		}
	}
	if n := len(regexp.MustCompile(`id="info-[a-z]+"`).FindAllString(html, -1)); n != len(wantInfo) {
		t.Errorf("信息卡网格应当有 %d 张，实际解析到 %d 个 info-* id", len(wantInfo), n)
	}

	// 图表：5 个 chart-block，每个带 data-chart，且有对应的 canvas。
	charts := []string{"cpu", "mem", "disk", "net", "traffic"}
	for _, key := range charts {
		if !strings.Contains(html, `<div class="chart-block" data-chart="`+key+`">`) {
			t.Errorf("详情页缺少图表块 data-chart=%q", key)
		}
		if !strings.Contains(html, `id="chart-`+key+`"`) {
			t.Errorf("图表 %s 缺少对应的 canvas", key)
		}
	}
	if n := len(regexp.MustCompile(`class="chart-block" data-chart="[a-z]+"`).FindAllString(html, -1)); n != len(charts) {
		t.Errorf("详情页应当有 %d 个图表块，实际 %d 个", len(charts), n)
	}

	// 设置对话框里的勾选框：键表必须与图表块一一对应。
	for _, key := range charts {
		if !strings.Contains(html, `<input type="checkbox" data-chart="`+key+`"`) {
			t.Errorf("设置对话框缺少图表勾选框 data-chart=%q", key)
		}
	}
	if n := len(regexp.MustCompile(`type="checkbox" data-chart="[a-z]+"`).FindAllString(html, -1)); n != len(charts) {
		t.Errorf("图表勾选框应当有 %d 个，实际 %d 个", len(charts), n)
	}

	// 旧的左右分栏必须彻底删掉：两套布局同时在，栅格会互相打架。
	if strings.Contains(html, "detail-grid") {
		t.Error("index.html 里还留着旧的 detail-grid 布局")
	}
	if strings.Contains(css, "detail-grid") {
		t.Error("style.css 里还留着 .detail-grid 规则")
	}
	for _, rule := range []string{".detail-wrap", ".stat-row", ".stat", ".stat-label", ".stat-value", ".info-grid", ".chart-block", ".chart-grid"} {
		if !strings.Contains(css, rule) {
			t.Errorf("style.css 缺少 %s 规则", rule)
		}
	}
	// 窄屏下汇总排要变两列（四个金额挤一行会被折行，反而更难读）。
	narrow := regexp.MustCompile(`(?s)@media \(max-width: 640px\).*?\.stat-row\s*\{[^}]*repeat\(2,\s*1fr\)`).MatchString(css)
	if !narrow {
		t.Error("窄屏 media query 里应当把 .stat-row 改成两列")
	}
	if !regexp.MustCompile(`(?s)@media \(max-width: 900px\).*?\.info-grid\s*\{[^}]*1fr`).MatchString(css) {
		t.Error("窄屏 media query 里应当把 .info-grid 改成单列")
	}
}

// 取消勾选的图表必须连数据都不请求（否则服务端照旧按天/按范围查库、按秒推送）。
func TestFrontendSkipsHiddenChartRequests(t *testing.T) {
	js := readAsset(t, "app.js")
	for _, needle := range []string{
		"var visibleCharts = null;",
		"function chartVisible(",
		"function applyChartVisibility(",
		"function loadChartVisibility(",
		"if (!chartVisible('traffic')) return Promise.resolve();",
		"if (chartVisible('cpu')) metrics.push('cpu');",
		"if (chartVisible('net')) metrics.push('net_down', 'net_up');",
		"api('/api/v1/settings/charts', { method: 'PUT', body: charts })",
	} {
		if !strings.Contains(js, needle) {
			t.Errorf("app.js 缺少 %q（图表可见性会失灵）", needle)
		}
	}

	// 这两条按"意图"断言，不钉死原文：实现里怎么拆行、变量怎么命名都行，
	// 行为必须对（钉死原文会让一次无害的重构把测试弄红）。
	if !regexp.MustCompile(`chartVisible\(block\.dataset\.chart\)`).MatchString(js) {
		t.Error("applyChartVisibility 应当逐个 chart-block 按 data-chart 判断显隐")
	}
	// 图表卡**各自**判断显隐（现在只有一张，但 applyChartVisibility 是按数组遍历的）：
	// 五张资源图全关掉时整张卡收起，页面上不该留下一个空块。
	if !strings.Contains(js, "el.chartsResources") {
		t.Error("applyChartVisibility 应当收起图表卡（app.js 里没有 el.chartsResources）")
	}
	if !regexp.MustCompile(`card\.hidden\s*=\s*shown\.length\s*===\s*0`).MatchString(js) {
		t.Error("一张图都不显示的图表卡应当整体收起来（否则只剩一个空边框）")
	}
}

// sectionBody 截取 index.html 里某个 id 所在的 <section> 内容（到它的 </section> 为止）。
//
// 卡片里装的都是 <div>，不会嵌套 <section>，所以"截到第一个 </section>"是安全的；
// 这里只需要判断"哪张卡里有哪个图块"，不必真去解析 HTML。
func sectionBody(t *testing.T, html, id string) string {
	t.Helper()
	at := strings.Index(html, `id="`+id+`"`)
	if at < 0 {
		return ""
	}
	rest := html[at:]
	end := strings.Index(rest, "</section>")
	if end < 0 {
		return ""
	}
	return rest[:end]
}

// 「网络信息」卡里那一行不是到探测目标的延迟，而是 Agent 到**面板自身**的
// WebSocket 往返。它以前就叫「延迟」，和延迟图里的探测结果撞名 ——
// 用户会把 100ms 的隧道往返读成"到 1.1.1.1 的延迟"。
// 「Uptime」这个标签会让人以为是"在线了多久"。它其实来自 /proc/uptime，
// 是**机器自上次重启**以来的时长：重启会让它归零（但节点一秒没掉线），
// 反过来机器一年没重启、中途断网三天，它照样显示一年。
// 在线率要看「可用率」（流量卡里的 24h/7d）。所以三处标签都得写明是"开机时长"。
func TestFrontendUptimeLabelSaysBootTime(t *testing.T) {
	js := readAsset(t, "app.js")

	// 节点卡片脚注与详情页「系统信息」卡。
	if !strings.Contains(js, "'开机时长'") {
		t.Error("app.js 里缺少「开机时长」这个标签")
	}
	// 设置页「服务器信息」那一栏说的是**面板进程自己**的启动时长，
	// 不是任何一台被监控节点的，所以标签必须点明"面板"。
	if !strings.Contains(js, "'面板已运行'") {
		t.Error("app.js 里缺少「面板已运行」这个标签")
	}
	// 光秃秃的 'Uptime' 不能再当显示标签用。
	if regexp.MustCompile(`'Uptime'`).MatchString(js) {
		t.Error("app.js 里还留着含糊的「Uptime」显示标签")
	}
	if strings.Contains(js, "运行时间（Uptime）") && !strings.Contains(js, "「开机时长」而不是「运行时间（Uptime）」") {
		t.Error("app.js 里还留着含糊的「运行时间（Uptime）」显示标签")
	}
	// 只改名，不改数据来源：两个值仍然取 uptime_sec。
	if !regexp.MustCompile(`infoRow\(sys, '开机时长',\s*fmtUptime\(node\.uptime_sec\)`).MatchString(js) {
		t.Error("「开机时长」这一行应当仍然渲染 node.uptime_sec")
	}
	if !regexp.MustCompile(`\['面板已运行',\s*fmtUptime\(info\.uptime_sec\)\]`).MatchString(js) {
		t.Error("「面板已运行」这一行应当仍然渲染面板自己的 info.uptime_sec")
	}
}

func TestFrontendPanelLatencyLabelIsUnambiguous(t *testing.T) {
	js := readAsset(t, "app.js")

	if !strings.Contains(js, "面板延迟") {
		t.Error("app.js 里缺少「面板延迟」这个标签")
	}
	if regexp.MustCompile(`infoRow\(net, '延迟'`).MatchString(js) {
		t.Error("「网络信息」卡里还留着含糊的「延迟」标签")
	}
	// 值仍然取 node.lat_ms —— 只改名，不改数据来源。
	if !regexp.MustCompile(`infoRow\(net, '面板延迟',\s*node\.lat_ms`).MatchString(js) {
		t.Error("「面板延迟」这一行应当仍然渲染 node.lat_ms")
	}
}

// 首页卡片（Phase 17 重做）：四格资源（2×2）+ 点线引导行。
//
// 这些字段与标签都是"少一个就少一格/少一行"的东西：卡片照常渲染、控制台一声不吭，
// 只有盯着屏幕看才发现「内存」那格没有副值、「探测」整行不见了。所以这里按
// 数据来源逐条钉住，并守住两条口径：
//   - 硬盘只取**根挂载点**（一个节点上报多个挂载点时全加起来会重复计算同一块盘）；
//   - 「面板延迟」**不许出现在卡片上**（它是 Agent 到面板自身的往返，与「探测」
//     那一行的探测结果不是一回事），但它必须仍在详情页的「网络信息」卡里
//     （见 TestFrontendPanelLatencyLabelIsUnambiguous）。
func TestFrontendNodeCardResourceCellsAndLeaderLines(t *testing.T) {
	js := readAsset(t, "app.js")
	css := readAsset(t, "style.css")

	card := homeCardSource(js)
	if card == "" {
		t.Fatal("app.js 里找不到首页卡片那一段（resCell … renderProbeLine）")
	}

	// 四格：CPU / 内存 / 硬盘 / 流量，顺序就是 2×2 网格的填充顺序。
	for _, needle := range []string{
		"var CARD_RES = [['cpu', 'CPU'], ['mem', '内存'], ['disk', '硬盘'], ['quota', '流量']];",
		"function resCell(",
	} {
		if !strings.Contains(js, needle) {
			t.Errorf("app.js 缺少 %q（四格资源会少一格或顺序不对）", needle)
		}
	}

	// 副值的数据来源：load1/5/15、内存已用/总量、硬盘根挂载点、流量已用/额度。
	// 单位口径见 TestFrontendByteUnitsSplitByResource：内存 Bin、硬盘/流量 Dec。
	for _, needle := range []string{
		"fmtLoad(dto.load1)", "fmtLoad(dto.load5)", "fmtLoad(dto.load15)",
		"pairTextBin(dto.mem_used, dto.mem_total)",
		"rootDiskOf(dto.disks)",
		"pairTextDec(disk.used, disk.total)",
		"fmtBytesDec(cycleUsed) + ' / ' + fmtBytesDec(dto.traffic_limit)",
	} {
		if !strings.Contains(card, needle) {
			t.Errorf("卡片渲染里缺少 %q", needle)
		}
	}
	// 负载两位小数：取整会把 0.04 与 0.004 显示成同一个数。
	if !regexp.MustCompile(`toFixed\(2\)`).MatchString(funcBody(js, "function fmtLoad(")) {
		t.Error("负载应当保留两位小数")
	}
	// 硬盘挑根挂载点，找不到才退回第一项（与后端 overview.go 的 rootDisk 同一口径）。
	root := funcBody(js, "function rootDiskOf(")
	if root == "" {
		t.Fatal("app.js 缺少 rootDiskOf()：硬盘格会算成多个挂载点之和")
	}
	if !strings.Contains(root, "list[i].mount === '/'") || !strings.Contains(root, "return list[0];") {
		t.Error("rootDiskOf 应当优先取 / 挂载点，找不到才退回第一项")
	}
	// 没填额度时：百分比那格传 null（画成 —），副值只写已用量 ——
	// 写 "/ 0" 会被读成"额度已经用光"。
	if !regexp.MustCompile(`setRes\(r\.quota,\s*null,\s*fmtBytesDec\(cycleUsed\)\)`).MatchString(card) {
		t.Error("没填流量额度时，流量格应当只显示已用量（百分比写 —，而不是 / 0）")
	}

	// 点线引导行：四行的标签，以及"费用没填价格就整行不显示"。
	if !strings.Contains(js, `['cost', '费用']`) || !strings.Contains(js, `['seen', '最后通信']`) {
		t.Error("点线引导行里缺少「费用」或「最后通信」")
	}
	if !strings.Contains(card, "r.cost.root.hidden = !hasPrice;") {
		t.Error("没填价格时「费用」整行应当隐藏（留一行 0.00 会被读成免费）")
	}
	// 引导线是 CSS 画的，不是一串句点字符。
	if !regexp.MustCompile(`\.line-lead\s*\{[^}]*border-bottom:\s*1px\s+dotted`).MatchString(css) {
		t.Error("style.css 里 .line-lead 应当用 1px dotted 的下边框画引导线")
	}
	if regexp.MustCompile(`textContent\s*=\s*'[·.\s]{4,}'`).MatchString(js) {
		t.Error("app.js 在用小圆点/句点字符堆引导线：应当交给 CSS（.line-lead）")
	}
	// .line 自己写了 display:flex，会盖掉 hidden 那条 display:none。
	if !regexp.MustCompile(`\.line\[hidden\]\s*\{[^}]*display:\s*none`).MatchString(css) {
		t.Error("style.css 缺少 .line[hidden] { display: none }：没填价格的卡片会留下一条空行")
	}

	// 「在线」= 连续在线时长：不在线时显示 —（0 秒会被读成"刚上线"）。
	if !strings.Contains(card, "onlineText(dto.online_sec)") {
		t.Error("「在线」那一行应当渲染 dto.online_sec（连续在线时长）")
	}
	if !regexp.MustCompile(`sec > 0 \? fmtUptime\(sec\) : '—'`).MatchString(js) {
		t.Error("不在线（online_sec = 0）时「在线」应当显示 —")
	}

	// 卡片上不许出现「面板延迟」：它测的是 Agent 到面板自身的往返，
	// 与卡片上任何一行读数都不是一回事，摆在一起必然被读混。
	if strings.Contains(card, "面板延迟") || strings.Contains(card, "dto.lat_ms") {
		t.Error("首页卡片上还留着「面板延迟」（lat_ms）")
	}
	// 但详情页「网络信息」卡里必须还在（那是有上下文的地方）。
	if !regexp.MustCompile(`infoRow\(net, '面板延迟',\s*node\.lat_ms`).MatchString(js) {
		t.Error("详情页「网络信息」卡里的「面板延迟」被误删了")
	}

	// 样式：2×2 网格、格子的三段、引导行。
	for _, rule := range []string{
		".card-res", ".res-cell", ".res-head", ".res-pct", ".res-sub",
		".card-lines", ".line ", ".line-label", ".line-lead", ".line-value",
	} {
		if !strings.Contains(css, rule) {
			t.Errorf("style.css 缺少 %s 规则", rule)
		}
	}
	if !regexp.MustCompile(`(?s)\.card-res\s*\{[^}]*grid-template-columns:\s*repeat\(2,`).MatchString(css) {
		t.Error("style.css 里 .card-res 应当是两列网格（四格排成 2×2）")
	}
	// 窄屏仍然两列：四格已经是"2×2"了，再折成一列只会让卡片凭空高一倍。
	if regexp.MustCompile(`(?s)@media \(max-width: 640px\).*?\.card-res\s*\{`).MatchString(css) {
		t.Error("窄屏不该把 .card-res 改成单列：四格本来就只占两列")
	}
	// 长副值不许把卡片撑宽。
	if !regexp.MustCompile(`(?s)\.card-res\s*\{[^}]*minmax\(0,\s*1fr\)`).MatchString(css) {
		t.Error("style.css 里 .card-res 的列宽应当是 minmax(0, 1fr)（否则长副值会撑破卡片）")
	}
}

// homeCardSource 截出"首页卡片"那一整块源码：从 resCell 到 renderSummary 之前。
//
// 卡片相关的函数（resCell / lineRow / setRes / createCard / renderCardTags /
// updateCard）都排在这一段里，所以"卡片渲染里不许出现某个词"
// 可以整块断言，不必逐函数去数 —— 漏掉一个函数就等于漏掉一处误删。
func homeCardSource(js string) string {
	start := strings.Index(js, "function resCell(")
	end := strings.Index(js, "function renderSummary(")
	if start < 0 || end < 0 || end < start {
		return ""
	}
	return js[start:end]
}

// 节点标签：设置页的「服务器列表」一栏 + 节点对话框里的「标签」框 + 首页卡片上的标签行。
//
// 标签**不按文字取色**（这里曾经钉过"颜色必须由文字哈希决定"）：现在只有一种
// 低调的元数据样式（淡边框 + 弱化色 + 11px，见 .tag），所以既不该有哈希函数，
// 也不该有色板类名。
//
// 两个改版在这里同时钉住：
//   - 改动 4a：首页卡片与设置页列表**共用同一套**弱化样式（.tag + 同一条容器规则），
//     看上去像"附注"而不是"按钮"；
//   - 改动 4b：独立的「编辑标签」对话框与它的整套 JS 已经删掉，标签改到
//     节点对话框里编辑（一个普通文本框、; 分隔、64 个 / 每个 32 字），
//     超限必须在**节点对话框的错误位**上拦住且不发请求。
func TestFrontendNodeTags(t *testing.T) {
	html := readAsset(t, "index.html")
	js := readAsset(t, "app.js")
	css := readAsset(t, "style.css")

	// 左栏：服务器列表排在「仪表盘」之后、「安全」之前。
	dash := strings.Index(html, `class="nav-item" data-pane="dashboard"`)
	nav := strings.Index(html, `class="nav-item" data-pane="nodes"`)
	sec := strings.Index(html, `class="nav-item" data-pane="security"`)
	if dash < 0 || nav < 0 || sec < 0 {
		t.Fatal(`index.html 的左栏里缺少 data-pane="dashboard" / "nodes" / "security" 中的一个`)
	}
	if !(dash < nav && nav < sec) {
		t.Error("「服务器列表」应当排在「仪表盘」之后、「安全」之前")
	}
	if !strings.Contains(html, `<section class="pane" data-pane="nodes" hidden>`) {
		t.Error(`index.html 里缺少 data-pane="nodes" 的内容栏（或它没有 hidden）`)
	}

	// 一栏的骨架：行容器 + 顶部「＋ 添加节点」+ 空态 + 本栏自己的错误位。
	for _, id := range []string{"nodes-list", "nodes-add", "nodes-empty", "nodes-error"} {
		if !strings.Contains(html, `id="`+id+`"`) {
			t.Errorf("服务器列表栏缺少 id=%s", id)
		}
	}
	// 「编辑节点」必须复用现有的 dlg-node，而不是另写一套表单：一套表单两处维护，
	// 迟早出现"这里能填、那里不能填"。
	if !strings.Contains(js, "openNodeDialog('edit', node)") {
		t.Error("服务器列表的「编辑节点」应当复用 openNodeDialog('edit', ...)")
	}
	if !strings.Contains(js, "openNodeDialog('create', null)") {
		t.Error("服务器列表的「＋ 添加节点」应当复用 openNodeDialog('create', null)")
	}
	// 编辑目标 id 必须跟着对话框走：以前保存时用的是 detail.id，而从设置页打开
	// 这个对话框时详情页是关着的（detail.id = 0），保存会打到 /api/v1/nodes/0 上。
	if !strings.Contains(js, "var nodeDialogID = 0;") ||
		!strings.Contains(js, "'/api/v1/nodes/' + nodeDialogID") {
		t.Error("节点对话框必须记住自己这次编辑的是哪个 id（否则从设置页保存会打到 /nodes/0）")
	}

	// 一行一台机器：整行一个圆角浅边框，行里的几段（头部 / 信息 / 标签）都由 app.js 造。
	if !regexp.MustCompile(`function settingsNodeRow\(`).MatchString(js) {
		t.Fatal("app.js 缺少 settingsNodeRow()：一行一台机器")
	}
	row := funcBody(js, "function settingsNodeRow(")
	if row == "" {
		t.Fatal("settingsNodeRow() 的函数体没截取到")
	}
	for _, needle := range []string{"node-item-head", "node-item-meta", "node-item-tags", "rowButton('编辑节点'"} {
		if !strings.Contains(row, needle) {
			t.Errorf("服务器列表的一行里缺少 %q", needle)
		}
	}
	// 「编辑标签」按钮必须彻底消失：留着它就有两个改标签的入口，
	// 而两处保存的都是同一份数据（整体替换），用户会以为改的是两样东西。
	for _, gone := range []string{"编辑标签", "openTagDialog"} {
		if strings.Contains(row, gone) || strings.Contains(js, gone) {
			t.Errorf("app.js 里还留着 %q（标签已经并进「编辑节点」对话框）", gone)
		}
	}
	// 信息行是**从已有字段拼的**：IP 用 local_ip（没有才退回 observed_ip），
	// 分组/剩余价值/到期天数各自"有才显示"。
	for _, needle := range []string{"node.local_ip", "node.observed_ip", "'分组：'", "node.price_cents > 0", "node.remaining_value_cents", "' 天后到期'"} {
		if !strings.Contains(row, needle) {
			t.Errorf("服务器列表的信息行缺少 %q", needle)
		}
	}
	if strings.Contains(row, "'—'") {
		t.Error("信息行不该出现占位符 —：字段缺失时整段省略，而不是显示「分组：—」")
	}

	// 标签的编辑入口：**节点对话框里的一个普通文本框** + 一行"用 ; 分隔"的提示。
	//
	// 这里钉三件事：
	//   1. 独立的 <dialog id="dlg-tags"> 与它的整套 JS 已经整块删掉（不是藏在别的
	//      地方继续用）：留着就是"两个入口改同一份数据"；
	//   2. 标签框在 dlg-node **里面**（跑到别的对话框里等于没并进来）；
	//   3. 没有第二套输入方式：徽章编辑器（一组动态徽章 + 每个的 × 删除）与
	//      「已有的标签」候选区都删掉了 —— 文本框加候选区等于两处都能改标签。
	for _, gone := range []string{
		`<dialog id="dlg-tags"`, `id="tags-input"`, `id="tags-hint"`,
		`id="tags-error"`, `id="tags-cancel"`, `id="tags-save"`, `id="tags-node"`,
	} {
		if strings.Contains(html, gone) {
			t.Errorf("index.html 里还留着 %q（标签已经并进「编辑节点」对话框）", gone)
		}
	}
	for _, gone := range []string{
		"dlgTags", "tagsInput", "tagsHint", "tagsSave", "tagsCancel", "tagsNode",
		"tagsError", "openTagDialog", "saveTags", "tagFormPayload", "tagNode",
	} {
		if strings.Contains(js, gone) {
			t.Errorf("app.js 里还引用着已删除的 %s", gone)
		}
	}

	node := dialogBody(t, html, "dlg-node")
	if node == "" {
		t.Fatal(`index.html 里找不到 <dialog id="dlg-node">`)
	}
	if !strings.Contains(node, `id="node-tags"`) {
		t.Error("「新增/编辑节点」对话框里缺少标签输入框 #node-tags")
	}
	// 提示必须在**这个对话框里**、且写明分隔符（不写的话用户只能猜是逗号还是空格）。
	hint := ""
	for _, line := range strings.Split(node, "\n") {
		if strings.Contains(line, `id="node-tags-hint"`) {
			hint = line
		}
	}
	if hint == "" {
		t.Fatal("「新增/编辑节点」对话框里缺少 #node-tags-hint（提示不能跑到对话框外）")
	}
	if !strings.Contains(hint, ";") {
		t.Error("标签提示里必须出现 ;（不写分隔符，用户只能猜是逗号还是空格）")
	}
	for _, gone := range []string{"tags-editor", "tags-existing", "tag-suggest", "tag-x"} {
		if strings.Contains(html, gone) {
			t.Errorf("index.html 里还留着 %q：标签是一个纯文本框", gone)
		}
	}
	for _, gone := range []string{"tagsEditor", "tagsExisting", "tagsExistingWrap", "tagDraft"} {
		if strings.Contains(js, gone) {
			t.Errorf("app.js 里还引用着已删除的 %s", gone)
		}
	}

	// 切分：半角 ; 与全角 ；都要认（中文输入法下打出来的是全角，只认半角会被当成 bug）。
	if !regexp.MustCompile(`var TAG_SEP = /\[;；\]/;`).MatchString(js) {
		t.Fatal("app.js 应当有 TAG_SEP = /[;；]/：半角与全角分号都要能切")
	}
	split := funcBody(js, "function splitTags(")
	if split == "" {
		t.Fatal("app.js 缺少 splitTags()：文本框里的内容没法变成标签列表")
	}
	for _, needle := range []string{
		"split(TAG_SEP)",   // 按分隔符切
		"piece.trim()",     // 去首尾空白
		"if (!tag) return", // 末尾多一个分隔符 / 连打两个 ;; 切出来的空串要丢掉
		"seen[tag]",        // 去重（保持首次出现的顺序，与服务端一致）
	} {
		if !strings.Contains(split, needle) {
			t.Errorf("splitTags() 里缺少 %q", needle)
		}
	}
	// 回填：用「; 」连接（分号 + 空格）。打开编辑节点时既有的标签要出现在文本框里。
	if !regexp.MustCompile(`function tagsToInputValue\(`).MatchString(js) {
		t.Fatal("app.js 缺少 tagsToInputValue()：打开对话框时要把已有标签回填进文本框")
	}
	if !strings.Contains(js, "el.nodeTags.value = tagsToInputValue(d.tags);") {
		t.Error("openNodeDialog() 应当把 d.tags 回填进标签文本框")
	}
	if !strings.Contains(js, "join('; ')") {
		t.Error("回填应当用「; 」连接（分号 + 空格），与提示里写的分隔符一致")
	}

	// 上限与服务端对齐（64 个 / 每个 32 字），且**保存前先本地校验**：
	// 超限时用户看到的应当是"哪个标签、超了多少"，而不是等一个来回之后才被告知。
	for _, needle := range []string{"var TAG_MAX_COUNT = 64;", "var TAG_MAX_LEN = 32;"} {
		if !strings.Contains(js, needle) {
			t.Errorf("app.js 缺少 %q（前端的上限必须跟着服务端一起改）", needle)
		}
	}
	problem := funcBody(js, "function tagProblem(")
	if problem == "" {
		t.Fatal("app.js 缺少 tagProblem()：超限的标签会被直接发到服务端")
	}
	if !strings.Contains(problem, "TAG_MAX_LEN") || !strings.Contains(problem, "TAG_MAX_COUNT") {
		t.Error("tagProblem() 应当同时校验单个长度与总数量")
	}
	if !strings.Contains(problem, "list[i]") {
		t.Error("超长提示里必须带上出问题的那个标签（一次可能有几十个，只说「某个太长」没法排查）")
	}
	// 提交前先在本地校验一遍：不合法就只显示错误、**一个字节都不发**。
	// 为什么要有这一道（服务端已经会 400）：超限时用户看到的应该是"哪一个标签、
	// 超了多少"这种能直接改的提示，而不是等一个来回之后才被告知。
	validate := funcBody(js, "function validateNodeTags(")
	if validate == "" {
		t.Fatal("app.js 缺少 validateNodeTags()：超限的标签会被直接发到服务端")
	}
	if !strings.Contains(validate, "tagProblem(payload.tags") {
		t.Error("validateNodeTags() 应当拿请求体里那份切好的标签去校验 tagProblem()")
	}
	submit := funcBody(js, "function submitNodeForm(")
	if submit == "" {
		t.Fatal("app.js 缺少 submitNodeForm()")
	}
	if !strings.Contains(submit, "validateNodeTags(payload)") {
		t.Error("submitNodeForm() 提交前必须先过一遍 validateNodeTags()（拦不住就白发请求）")
	}
	// 拦下来那一支必须在发请求之前 return，并把消息写在**节点对话框的错误位**上。
	if !regexp.MustCompile(`if \(problem\) \{\s*el\.nodeError\.textContent = problem;\s*return;`).MatchString(submit) {
		t.Error("标签不合法时应当把消息写进 #node-error 并直接 return（不发请求）")
	}
	// 保存失败不许关对话框：关掉的话用户填了十几个字段的表单就没了。
	if !regexp.MustCompile(`(?s)\)\.catch\(function \(err\) \{.*?el\.nodeError\.textContent = err\.message;`).MatchString(submit) {
		t.Error("保存失败时应当把错误留在对话框里（并保持对话框打开）")
	}

	// 保存：标签跟着节点一起提交 —— 用的就是现有的节点接口与现有的整体替换 payload，
	// 所以别的字段一个都不能少（少了就会被冲成默认值）。
	payload := funcBody(js, "function nodeFormPayload(")
	if payload == "" {
		t.Fatal("app.js 缺少 nodeFormPayload()")
	}
	for _, field := range []string{
		"name:", "group_name:", "region:", "note:", "interval_sec:", "traffic_limit:",
		"traffic_warn_pct:", "reset_day:", "expires_at:", "price_cents:", "currency:",
		"billing_months:", "enabled:", "tags:",
	} {
		if !strings.Contains(payload, field) {
			t.Errorf("节点请求体缺少字段 %q：会把那个字段冲成默认值", field)
		}
	}
	// 请求体里的 tags 必须是**现切**的：文本框是唯一的事实来源。
	if !strings.Contains(payload, "tags: splitTags(el.nodeTags.value)") {
		t.Error("节点请求体的 tags 应当来自 splitTags(el.nodeTags.value)")
	}

	// 首页卡片：标签行排在**所有读数**（资源格 / 引导行 / 迷你条）下面，
	// 没有标签就整行隐藏，多了自动折行。
	card := funcBody(js, "function createCard(")
	if card == "" {
		t.Fatal("app.js 缺少 createCard()")
	}
	linesAt := strings.Index(card, "root.appendChild(lines);")
	tagsAt := strings.Index(card, "root.appendChild(tags);")
	if linesAt < 0 || tagsAt < 0 || linesAt > tagsAt {
		t.Error("卡片上的标签行应当排在读数（资源格/引导行/迷你条）之后")
	}
	if !strings.Contains(js, "function renderCardTags(") {
		t.Fatal("app.js 缺少 renderCardTags()")
	}
	if !regexp.MustCompile(`card\.refs\.tags\.hidden = list\.length === 0;`).MatchString(js) {
		t.Error("没有标签的卡片应当把标签行整行隐藏")
	}
	// 每秒都会被 SSE 重画一次：标签没变就不该重建 DOM。
	if !regexp.MustCompile(`if \(key === card\.tagKey\) return;`).MatchString(js) {
		t.Error("renderCardTags() 应当先比对再重建（卡片每秒重画一次）")
	}

	// 三处（首页卡片、服务器列表、对话框之外的一切）共用同一个 tagChip()，
	// 而且它**不带颜色类**：样式全部由 CSS 的 .tag 决定。
	if !regexp.MustCompile(`function tagChip\(`).MatchString(js) {
		t.Fatal("app.js 缺少 tagChip()")
	}
	if !regexp.MustCompile(`span\.className = 'tag';`).MatchString(js) {
		t.Error("tagChip() 应当只给 'tag' 这一个类名（标签不再有颜色）")
	}
	if n := strings.Count(js, "tagChip("); n < 3 {
		t.Errorf("tagChip() 只被用了 %d 次：首页卡片与服务器列表必须共用同一个", n)
	}
	// 取色那套（哈希 / 色板 / 彩色类名）必须彻底消失：留着它，下次改动又会有人
	// 顺手把颜色加回来，而"同一个标签在两处不同色"是浏览器里才看得出来的问题。
	for _, gone := range []string{"tagHash", "tagClass", "TAG_COLOR_COUNT", "tag-c"} {
		if strings.Contains(js, gone) {
			t.Errorf("app.js 里还留着 %q（标签改成低调样式，不再按文字哈希取色）", gone)
		}
	}
	if strings.Contains(css, "tag-c") {
		t.Error("style.css 里还留着彩色标签类名（应当只剩一个 .tag）")
	}
	if regexp.MustCompile(`Math\.random`).MatchString(js) {
		t.Error("标签样式不该与随机数有任何关系")
	}

	// CSS：低调的徽章、折行、hidden 兜底。
	for _, rule := range []string{
		".tag {", ".card-tags", ".node-item", ".node-item-head",
		".node-item-meta", ".node-item-region",
	} {
		if !strings.Contains(css, rule) {
			t.Errorf("style.css 缺少 %s 规则", rule)
		}
	}
	for _, gone := range []string{".tag-node", ".tag-editor", ".tag-x", ".tag-suggest"} {
		if strings.Contains(css, gone) {
			t.Errorf("style.css 里还留着 %s 规则（标签对话框已经删掉）", gone)
		}
	}
	// 低调 = 淡边框 + **弱化色** + 更小的字号，且没有色块
	// （.tag 里出现 background 就是又加回了颜色）。
	tagRule := cssRule(css, ".tag")
	if tagRule == "" {
		t.Fatal("style.css 里找不到 .tag 规则")
	}
	if !strings.Contains(tagRule, "border: 1px solid var(--tag-border)") {
		t.Error(".tag 应当是 1px 实线淡边框（用 --tag-border，深浅主题各一档）")
	}
	if !strings.Contains(tagRule, "color: var(--fg-muted)") {
		t.Error(".tag 的文字应当用弱化色（--fg-muted）：标签是附注，不该跟正文抢注意力")
	}
	if strings.Contains(tagRule, "color: var(--fg)") {
		t.Error(".tag 用的是正文色（--fg）：那正是「很突兀」的原因，应当用 --fg-muted")
	}
	if !strings.Contains(tagRule, "font-size: 11px") {
		t.Error(".tag 的字号应当是 11px（比正文小一号，看上去像附注）")
	}
	if strings.Contains(tagRule, "background") {
		t.Error(".tag 里出现了 background：标签不该有色块")
	}
	// 两处（首页卡片与设置页列表）的标签容器由**同一条规则**给布局：
	// 一处改了另一处没改，两个页面上的标签就会长得不一样。
	if !regexp.MustCompile(`(?s)\.node-item-tags,\s*\n\.card-tags\s*\{`).MatchString(css) {
		t.Error("style.css 里 .node-item-tags 与 .card-tags 应当共用同一条规则（两处标签必须一致）")
	}
	// 首页那一处只多一条上边框当分隔线，不许覆盖标签本身的颜色/字号 ——
	// 覆盖了就等于两处不一样了。
	if !regexp.MustCompile(`(?s)\.card-tags\s*\{[^}]*border-top:`).MatchString(css) {
		t.Error("style.css 里 .card-tags 应当用一条上边框把它与上面的读数分开")
	}
	for _, bad := range []string{"color:", "font-size:", "padding:"} {
		if strings.Contains(cssRule(css, ".card-tags"), bad) {
			t.Errorf("style.css 的 .card-tags 里出现了 %s：首页与设置页的标签会不一样", bad)
		}
	}
	// 深浅两套主题各定义一次边框色（浅色 :root + prefers-color-scheme + data-theme="dark"）：
	// 深色下 --border 太暗，直接拿它当边框等于没有边框。
	if n := strings.Count(css, "--tag-border:"); n != 3 {
		t.Errorf("--tag-border 应当在浅色、系统深色、手动深色三处各定义一次，实际 %d 次", n)
	}
	// 标签多了必须折行：不折行会把卡片撑破或顶出横向滚动条。
	if !regexp.MustCompile(`(?s)\.card-tags[^{]*\{[^}]*flex-wrap:\s*wrap`).MatchString(css) {
		t.Error(".card-tags 应当 flex-wrap: wrap（标签多了要换行）")
	}
	// 这两个容器自己写了 display:flex，会盖掉 hidden 那条 display:none。
	for _, sel := range []string{".card-tags", ".node-item-tags"} {
		if !regexp.MustCompile(regexp.QuoteMeta(sel) + `\[hidden\]\s*\{[^}]*display:\s*none`).MatchString(css) {
			t.Errorf("style.css 缺少 %s[hidden] { display: none }：没有标签的行会留一条空白", sel)
		}
	}
}

// 时间档位（1h…7d）在**「资源与网络」卡片的标题行**里，而不是详情页头部。
//
// 它只控制这张卡里的五张资源图。这里曾经还有第二组档位（「延迟」卡自带的那一组，
// 两张卡互相独立），随「延迟探测」功能一起删除 —— 现在全页只有这一组，
// 所以更要钉住"它长在卡片标题行里、而不是页面级的位置上"（页面级会被读成
// "它管的是整页的数据"）。
func TestFrontendRangeButtonsInResourcesCard(t *testing.T) {
	html := readAsset(t, "index.html")
	js := readAsset(t, "app.js")
	css := readAsset(t, "style.css")

	res := sectionBody(t, html, "charts-resources")
	if res == "" {
		t.Fatal("index.html 里找不到「资源与网络」卡片")
	}
	if !strings.Contains(res, `id="detail-ranges"`) {
		t.Error("时间档位按钮不在「资源与网络」卡片里")
	}
	// 必须在标题行（.chart-head）里、在最右边：前面要有标题与一个 spacer。
	head := regexp.MustCompile(`(?s)<div class="chart-head">.*?</div>\s*<div class="chart-grid">`).FindString(res)
	if head == "" {
		t.Fatal("「资源与网络」卡片里找不到 .chart-head 标题行（或它后面不再紧跟图表网格）")
	}
	if !strings.Contains(head, "<h2>资源与网络</h2>") || !strings.Contains(head, `id="detail-ranges"`) {
		t.Error(".chart-head 里应当同时有标题与时间档位")
	}
	if strings.Index(head, "<h2>") > strings.Index(head, `id="detail-ranges"`) {
		t.Error("时间档位应当排在标题**右边**（标题在左、控件在右）")
	}
	if !strings.Contains(head, `class="spacer"`) {
		t.Error("标题与档位之间要有 .spacer：没有它两者会挤在一起，档位也不在最右边")
	}
	// 详情页头部不许再留着它（两处同时存在时 getElementById 只认第一个，
	// 另一处永远是空的 —— 而且看起来就像"档位没生效"）。
	detailHead := html[:strings.Index(html, `id="charts-resources"`)]
	if strings.Contains(detailHead, `id="detail-ranges"`) {
		t.Error("详情页头部里还留着时间档位按钮")
	}

	// 语义：这一组档位只管这五张资源图。
	pick := funcBody(js, "function setResourceRange(")
	if pick == "" {
		t.Fatal("app.js 缺少 setResourceRange()：资源档位的切换动作没地方写")
	}
	if !strings.Contains(pick, "detail.range = key") {
		t.Error("setResourceRange() 应当写 detail.range")
	}
	if !strings.Contains(pick, "loadSeries()") {
		t.Error("切资源档位要重取资源序列（loadSeries）")
	}
	// 渲染与状态分开：renderRangeGroup 只画按钮，选中的档位在 detail.range 里。
	if !strings.Contains(js, "renderRangeGroup(el.detailRanges, detail.range, setResourceRange)") {
		t.Error("app.js 缺少 renderRangeGroup(el.detailRanges, detail.range, setResourceRange)：档位按钮没画或状态没接上")
	}

	// 兜底逻辑（placeRangeButtons）必须**彻底删掉**：它只会把按钮挪进另一张卡的
	// 标题行，两组长得一样、控制的图却不同。
	// 按代码断言（注释里还留着"为什么删掉"的说明，那是给下一个改它的人看的）。
	if strings.Contains(codeLines(js), "placeRangeButtons") {
		t.Error("app.js 里还留着 placeRangeButtons()：它已经没有要解决的问题")
	}
	// 卡片要有同构的标题行：档位的落脚点。
	if !strings.Contains(sectionBody(t, html, "charts-resources"), `class="chart-head"`) {
		t.Error("charts-resources 卡片里缺少 .chart-head 标题行")
	}
	// 全页只有一张图表卡：延迟卡整张已经删掉。
	if strings.Contains(html, `id="charts-latency"`) {
		t.Error("index.html 里还留着「延迟」卡片（#charts-latency）")
	}

	// 窄屏不许把标题挤爆：标题行允许折行，档位自己也允许折行。
	if !regexp.MustCompile(`(?s)\.chart-head\s*\{[^}]*flex-wrap:\s*wrap`).MatchString(css) {
		t.Error("style.css 里 .chart-head 应当 flex-wrap: wrap（窄屏把档位整排折到标题下面）")
	}
	if !regexp.MustCompile(`(?s)\.ranges\s*\{[^}]*flex-wrap:\s*wrap`).MatchString(css) {
		t.Error("style.css 里 .ranges 应当 flex-wrap: wrap（六个档位在极窄屏上要能折行）")
	}
	if !regexp.MustCompile(`(?s)\.chart-head\s*\{[^}]*display:\s*flex`).MatchString(css) {
		t.Error("style.css 里 .chart-head 应当是 flex 布局")
	}
}

// 设置页用满宽屏（改动 3）：容器上限从 1200px 放宽到 1600px，
// 「服务器列表」的每一行改成允许折行的单行 flex，让一台机器的标签能跟名称并排。
//
// 为什么选 1600px 而不是"不设上限"：在 3440px 的带鱼屏上，一行里的名称与行尾按钮
// 会隔开两米远，扫一行要来回转头。1600px 足够装下"名称 + 信息 + 标签 + 按钮"，
// 又不至于让人读丢行。主页/详情页/顶栏后来也统一到这个宽度，
// 见 TestFrontendHomeAndDetailUseWideLayout。
func TestFrontendSettingsUsesWideLayout(t *testing.T) {
	html := readAsset(t, "index.html")
	js := readAsset(t, "app.js")
	css := readAsset(t, "style.css")

	layout := cssRule(css, ".settings-layout")
	if layout == "" {
		t.Fatal("style.css 里找不到 .settings-layout 规则")
	}
	if !strings.Contains(layout, "max-width: 1600px") {
		t.Error("设置页容器应当放宽到 1600px（宽屏下两侧留白太大，标签永远排不进名称那一行）")
	}
	if strings.Contains(layout, "max-width: 1200px") {
		t.Error(".settings-layout 还是 1200px：宽屏下标签会被挤到下一行")
	}
	if !regexp.MustCompile(`(?s)\.settings-layout\s*\{[^}]*max-width:\s*1600px`).MatchString(css) {
		t.Error("设置页容器应当与全站同宽（1600px）")
	}
	// 设置页头部跟着放宽：它用的就是全站共用的 .detail-head —— 那一页专用的
	// `#view-settings .detail-head` 覆盖已经删掉（两处写同一个数，改一处漏一处
	// 就会让头部与内容左右边界不齐，而那正是当初加这条覆盖的原因）。
	if !regexp.MustCompile(`(?s)\.detail-head\s*\{[^}]*max-width:\s*1600px`).MatchString(css) {
		t.Error("设置页头部（.detail-head）应当与内容同宽，否则返回按钮与内容左右边界不齐")
	}
	if regexp.MustCompile(`#view-settings \.detail-head\s*\{`).MatchString(css) {
		t.Error("style.css 里还留着 #view-settings .detail-head 的单独覆盖：.detail-head 已经是全站 1600px")
	}

	// 行内的三段（头部 / 信息 / 标签）在 body 里自己折行：宽度够时全在一行，
	// 不够时按"头部 → 信息 → 标签"的顺序折 —— 这就是"标签与服务器同一行"的机制。
	body := cssRule(css, ".node-item-body")
	if !strings.Contains(body, "display: flex") || !strings.Contains(body, "flex-wrap: wrap") {
		t.Error(".node-item-body 应当是允许折行的 flex（宽度够时名称/信息/标签同在一行）")
	}
	if !strings.Contains(body, "align-items: center") {
		t.Error(".node-item-body 里的三段应当垂直居中对齐")
	}
	// 标签行自己也要能收缩：它是 body 的 flex 项，min-width:auto 会把整行顶宽。
	if !regexp.MustCompile(`(?s)\.node-item-tags,[^}]*min-width:\s*0`).MatchString(css) {
		t.Error(".node-item-tags 应当 min-width: 0（否则标签多的一行会把整行顶宽、横向溢出）")
	}
	// 名称要能被省略号截断，而不是把整行撑开。
	if !regexp.MustCompile(`(?s)\.node-item-name\s*\{[^}]*min-width:\s*0`).MatchString(css) {
		t.Error(".node-item-name 应当 min-width: 0（flex 项的 min-width:auto 让它永远不会被截断）")
	}

	// 右侧操作列现在只剩一个按钮：标签的编辑入口并进了节点对话框。
	row := funcBody(js, "function settingsNodeRow(")
	if n := strings.Count(row, "rowButton("); n != 1 {
		t.Errorf("服务器列表的一行里应当只有 1 个按钮（编辑节点），实际 %d 个", n)
	}
	// 设置页仍然是自己那一栏，没有被这次改动牵连。
	if !strings.Contains(html, `<section class="pane" data-pane="nodes" hidden>`) {
		t.Error(`index.html 里缺少 data-pane="nodes" 的内容栏`)
	}
}

// dialogBody 截取 index.html 里某个 <dialog id="..."> 的内容（到它的 </dialog> 为止）。
//
// 对话框里不会嵌套 <dialog>，所以"截到第一个 </dialog>"是安全的；这里只需要判断
// "这个对话框里用的是哪种输入控件"，不必真去解析 HTML。
func dialogBody(t *testing.T, html, id string) string {
	t.Helper()
	at := strings.Index(html, `<dialog id="`+id+`"`)
	if at < 0 {
		return ""
	}
	rest := html[at:]
	end := strings.Index(rest, "</dialog>")
	if end < 0 {
		return ""
	}
	return rest[:end]
}

// cssRule 截取 style.css 里某条规则的声明块。选择器要精确到"选择器 + 空格 + 左花括号"，
// 否则 .tag 会先匹配到 .card-tags 之类的名字里带 tag 的规则。
func cssRule(css, selector string) string {
	at := strings.Index(css, selector+" {")
	if at < 0 {
		return ""
	}
	rest := css[at:]
	end := strings.Index(rest, "}")
	if end < 0 {
		return ""
	}
	return rest[:end]
}

// chartFuncBody 截取 chart.js 里一个函数体：chart.js 的函数都嵌在 create() 内部
// （缩进 4 空格），app.js 那个 funcBody 认的是 2 空格，在这里截不准。
func chartFuncBody(js, marker string) string {
	start := strings.Index(js, marker)
	if start < 0 {
		return ""
	}
	rest := js[start:]
	if end := strings.Index(rest, "\n    }\n"); end >= 0 {
		return rest[:end]
	}
	return rest
}

// 字节单位有**两套口径**，必须按资源分开用：
//
//   - 内存 → 1024 进制（KiB/MiB/GiB/TiB）。内存条物理上就是 2 的幂，商家说的
//     "1 GB 内存"给的其实是 1024³ 字节；
//   - 硬盘 / 流量 / 速率 → 1000 进制（KB/MB/GB/TB）。商家卖硬盘与流量就是按 10 的
//     幂（1 TB 额度 = 10¹² 字节），也是「月流量额度（GB）」输入框的口径。
//
// 这里**按调用点**断言，而不是只断言两个函数存在：函数都在、但内存那格错用了十进制
// （或反过来），页面照样渲染得出来，只有对着数字看才发现。混用的代价是具体的 ——
// 输入框曾按 1024³ 存、标签却写 GB：用户填 2000，库里变成 2147 GB，80% 预警要等
// 真实用量到 86% 才响，用户可能在收到预警前就超了商家的额度。
func TestFrontendByteUnitsSplitByResource(t *testing.T) {
	js := readAsset(t, "app.js")
	html := readAsset(t, "index.html")
	css := readAsset(t, "style.css")

	// 两个格式化函数与两个 pairText 都必须存在（内存与其它分开）。
	for _, fn := range []string{
		"function fmtBytesBin(", "function fmtBytesDec(",
		"function pairTextBin(", "function pairTextDec(",
	} {
		if !strings.Contains(js, fn) {
			t.Errorf("app.js 缺少 %s：内存与硬盘/流量的单位口径必须各有一个函数", fn)
		}
	}
	// 老的单一口径函数必须彻底消失：留着它，下次改动又会挑一个"看起来能用"的用上。
	for _, gone := range []string{"fmtBytes(", "pairText("} {
		if strings.Contains(js, gone) {
			t.Errorf("app.js 里还有 %s：字节单位必须写明是 Bin（内存）还是 Dec（其它）", gone)
		}
	}

	// 进制写死在函数里：Bin 用 1024、Dec 用 1000，且后缀表不同（带 i 与不带 i）。
	binBody := funcBody(js, "function fmtBytesBin(")
	if !strings.Contains(binBody, "1024") || !strings.Contains(js, "['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB']") {
		t.Error("fmtBytesBin 应当是 1024 进制、后缀带 i（KiB/MiB/GiB/TiB）")
	}
	decBody := funcBody(js, "function fmtBytesDec(")
	if !strings.Contains(decBody, "1000") || !strings.Contains(js, "['B', 'KB', 'MB', 'GB', 'TB', 'PB']") {
		t.Error("fmtBytesDec 应当是 1000 进制、后缀不带 i（KB/MB/GB/TB）")
	}
	// 两套口径各自只认自己的进制：Bin 里出现 1000（或 Dec 里出现 1024）就是把两档
	// 换算搞反了 —— 页面上照样出数，只是数字差 2.4%~7.4%。
	if strings.Contains(binBody, "1000") {
		t.Error("fmtBytesBin 里出现了 1000：内存的 1024 进制被写成了十进制")
	}
	if strings.Contains(decBody, "1024") {
		t.Error("fmtBytesDec 里出现了 1024：硬盘/流量又被算成二进制单位了")
	}
	// 速率走 Dec（网络惯例 KB/s、MB/s），不是二进制。
	if !regexp.MustCompile(`function fmtRate\(n\)[\s\S]{0,160}fmtBytesDec\(n\) \+ '/s'`).MatchString(js) {
		t.Error("fmtRate 应当用 fmtBytesDec（1000 进制）+ '/s'")
	}
	// 图表轴：曾经自带一段 1024 逻辑，现在跟着 Dec 走。
	axisBody := funcBody(js, "function fmtAxisBytes(")
	if axisBody == "" {
		t.Fatal("app.js 缺少 fmtAxisBytes()")
	}
	if strings.Contains(axisBody, "1024") {
		t.Error("fmtAxisBytes 还是 1024 进制：流量图的刻度会与 GB 额度对不上")
	}
	if !strings.Contains(axisBody, "1000") {
		t.Error("fmtAxisBytes 应当按 1000 进制换算")
	}

	// 内存：只有这两处，且必须用 Bin —— 内存条是 2 的幂，写 MiB/GiB 才是实话。
	card := homeCardSource(js)
	if card == "" {
		t.Fatal("app.js 里找不到首页卡片那一段")
	}
	for _, needle := range []string{
		"pairTextBin(dto.mem_used, dto.mem_total)",
	} {
		if !strings.Contains(card, needle) {
			t.Errorf("卡片内存格应当用 %q（1024 进制）", needle)
		}
	}
	for _, needle := range []string{
		"fmtBytesBin(t.mem_used) + ' / ' + fmtBytesBin(t.mem_total)",
	} {
		if !strings.Contains(js, needle) {
			t.Errorf("总览「总内存用量」应当用 %q（1024 进制）", needle)
		}
	}

	// 硬盘 / 流量 / 速率：逐处必须是 Dec，一处漏了就会在同一张卡片上冒出 MiB。
	for _, needle := range []string{
		// 首页卡片：硬盘副值、流量副值（有额度 / 没额度两条路径）。
		"pairTextDec(disk.used, disk.total)",
		"fmtBytesDec(cycleUsed) + ' / ' + fmtBytesDec(dto.traffic_limit)",
		"setRes(r.quota, null, fmtBytesDec(cycleUsed))",
		// 总览：硬盘、累计流量、上下行速率。
		"fmtBytesDec(t.disk_used) + ' / ' + fmtBytesDec(t.disk_total)",
		"fmtBytesDec(t.traffic_tx_total) + '  ↓ ' + fmtBytesDec(t.traffic_rx_total)",
		"setOverviewText('up', '↑ ' + fmtRate(t.tx_rate))",
		"setOverviewText('down', '↓ ' + fmtRate(t.rx_rate))",
		// 详情页：实时网络（速率）、累计流量、历史累计流量。
		"infoRow(net, '实时网络', '↑ ' + fmtRate(node.tx_rate) + '  ↓ ' + fmtRate(node.rx_rate))",
		"infoRow(net, '累计流量', '↑ ' + fmtBytesDec(node.tx_total) + '  ↓ ' + fmtBytesDec(node.rx_total))",
		"infoRow(tra, '历史累计流量', '↓ ' + fmtBytesDec(node.traffic_total_rx) + '  ↑ ' + fmtBytesDec(node.traffic_total_tx))",
	} {
		if !strings.Contains(js, needle) {
			t.Errorf("app.js 缺少 %q（这一处应当用 1000 进制）", needle)
		}
	}
	// 卡片速率那一行（拼上下行）同理。
	if !strings.Contains(card, "'⬆ ' + fmtRate(dto.tx_rate) + '  ⬇ ' + fmtRate(dto.rx_rate)") {
		t.Error("卡片速率应当用 fmtRate（1000 进制 + /s）")
	}
	// 流量信息卡（改动 2）：每行都是"↓收 ↑发" + 右边一段"总和（占比）"。
	// 四处数字全部来自后端，前端一个都不加：本周期那条以前是前端自己 rx+tx 并写成
	// "（共 X）"，现在连总和一起由服务端给（traffic_cycle_total），"共"字也去掉了。
	//
	// 逐处钉住，是因为这类改动"坏掉"时页面照样能看：少一个字段就少一段文字，
	// 而控制台一声不吭。
	traffic := funcBody(js, "function trafficCell(")
	if traffic == "" {
		t.Fatal("app.js 缺少 trafficCell()：流量卡每一行的两段没有落脚点")
	}
	if !strings.Contains(traffic, "fmtBytesDec(rx)") || !strings.Contains(traffic, "fmtBytesDec(tx)") {
		t.Error("trafficCell() 里的收/发必须是 Dec（1000 进制，GB/TB）")
	}
	if !strings.Contains(traffic, "traffic-dir") || !strings.Contains(traffic, "traffic-sum") {
		t.Error("trafficCell() 应当是两段（方向读数 + 总和），间隔交给 CSS 的 gap")
	}
	sum := funcBody(js, "function trafficSumText(")
	if sum == "" {
		t.Fatal("app.js 缺少 trafficSumText()：总和与占比没地方拼")
	}
	if !strings.Contains(sum, "if (!(limit > 0)) return text;") {
		t.Error("没填额度时不显示占比（没有分母的百分比没有意义），见 trafficSumText()")
	}
	if !strings.Contains(sum, "' / '") {
		t.Error("本周期那一行要写出分母（总和 / 额度）")
	}
	for _, needle := range []string{
		"infoRow(tra, '今日流量', trafficCell(node.traffic_today_rx, node.traffic_today_tx,",
		"trafficSumText(node.traffic_today_total, node.traffic_today_pct, 0)",
		"infoRow(tra, '本周流量', trafficCell(node.traffic_week_rx, node.traffic_week_tx,",
		"trafficSumText(node.traffic_week_total, node.traffic_week_pct, 0)",
		"infoRow(tra, '本周期流量', trafficCell(node.traffic_cycle_rx, node.traffic_cycle_tx,",
		"trafficSumText(node.traffic_cycle_total, node.traffic_pct, node.traffic_limit)",
	} {
		if !strings.Contains(js, needle) {
			t.Errorf("流量信息卡缺少 %q", needle)
		}
	}
	// 「本周」的口径要写进注释（周一 00:00 起、与今日同一套切天、周一时等于今日）：
	// 这是页面上唯一看不出对错的地方，注释是下一个改它的人唯一的线索。
	for _, note := range []string{"本周一 00:00", "store.WeekStart"} {
		if !strings.Contains(js, note) {
			t.Errorf("app.js 的注释里要写明「本周」的口径（缺少 %q）", note)
		}
	}
	// 「共」字去掉：本周期那条现在是"总和 / 额度（占比）"。
	if strings.Contains(funcBody(js, "function renderDetailInfo("), "共 ") {
		t.Error("流量卡里还留着「共」字（本周期那条已经改成直接写总和）")
	}
	// 两行可用率删掉（后端 uptime 字段仍然返回，只是前端不再显示）。
	for _, gone := range []string{"可用率 24h", "可用率 7d", "u24", "u7"} {
		if strings.Contains(js, gone) {
			t.Errorf("app.js 里还留着 %q（可用率那两行已经删掉）", gone)
		}
	}
	// 两段之间的间隔必须由 CSS 给（不能靠空格字符堆）：空格在比例字体里宽度不可控，
	// 折行时还会被留在行首/行尾。
	if !regexp.MustCompile(`(?s)\.traffic-cell\s*\{[^}]*display:\s*inline-flex`).MatchString(css) ||
		!regexp.MustCompile(`(?s)\.traffic-cell\s*\{[^}]*gap:`).MatchString(css) {
		t.Error("style.css 里 .traffic-cell 应当是 inline-flex + gap（两段的间隔交给 CSS）")
	}
	for _, needle := range []string{"'↓ ' + fmtBytesDec", "'  ↑ ' + fmtBytesDec"} {
		if !strings.Contains(traffic, needle) {
			t.Errorf("trafficCell() 里缺少 %q（方向符号与单位口径要和其它地方一致）", needle)
		}
	}

	// 速率图的 yFormat 必须是 fmtRate：只有它会把 "/s" 写进刻度（KB/s、MB/s）；
	// 此时 unit 必须留空，否则读数会变成 "MB/s/s"。
	if !regexp.MustCompile(`yFormat:\s*fmtRate`).MatchString(js) {
		t.Error("速率图的 yFormat 应当是 fmtRate（刻度要写出 KB/s）")
	}
	if !regexp.MustCompile(`rateOpts = \{[^}]*unit:\s*''`).MatchString(js) {
		t.Error("速率图既然用 fmtRate 当 yFormat，unit 就必须是空串（否则读数是 MB/s/s）")
	}
	// 流量图的轴走 fmtAxisBytes（它带单位后缀 GB/TB，所以 unit 也是空串）。
	if !regexp.MustCompile(`yFormat:\s*fmtAxisBytes`).MatchString(js) {
		t.Error("流量图的 yFormat 应当是 fmtAxisBytes")
	}

	// 额度输入框：GB（10⁹ 字节）↔ 字节，与标签「月流量额度（GB，0 表示不限）」一致。
	if !strings.Contains(html, `月流量额度（GB，0 表示不限）`) {
		t.Error("index.html 的额度标签应当写明是 GB（它是 10⁹ 字节口径的唯一说明）")
	}
	if !regexp.MustCompile(`d\.traffic_limit\s*/\s*(1e9|1000\s*\*\s*1000\s*\*\s*1000)`).MatchString(js) {
		t.Error("回填额度时应当用 / 1e9（GB = 10⁹ 字节），否则用户填的 2000 会变成 2147")
	}
	if !regexp.MustCompile(`nodeTraffic\.value[^\n]*\*\s*(1e9|1000\s*\*\s*1000\s*\*\s*1000)`).MatchString(js) {
		t.Error("保存额度时应当用 × 1e9（GB = 10⁹ 字节）")
	}
	// 旧口径（GiB）一个字符都不许留：它正是"80% 预警要等到 86% 才响"的原因。
	if regexp.MustCompile(`1024\s*\*\s*1024\s*\*\s*1024`).MatchString(js) {
		t.Error("app.js 里还留着 1024 * 1024 * 1024（GiB）的额度换算：标签写的是 GB")
	}
}

// 服务器列表的拖动排序：把手 + Pointer Events + 本地先重排 + 失败回滚。
//
// 为什么钉得这么细：拖动这类交互"坏掉"的方式全都不会报错 —— 事件挂错元素只是
// 拖不动、忘了本地重排只是慢半拍、忘了回滚只是"下次刷新顺序又变回去"。
// 只有真的用手指拖一遍才发现，所以这里把几条关键接线固定下来。
func TestFrontendNodeDragReordering(t *testing.T) {
	html := readAsset(t, "index.html")
	js := readAsset(t, "app.js")
	css := readAsset(t, "style.css")

	// 一行的结构：把手在 app.js 里 createElement 造出来并挂在行上。
	row := funcBody(js, "function settingsNodeRow(")
	if row == "" {
		t.Fatal("settingsNodeRow() 的函数体没截取到")
	}
	for _, needle := range []string{"'node-drag'", "handle.addEventListener('pointerdown'", "row.appendChild(handle)"} {
		if !strings.Contains(row, needle) {
			t.Errorf("settingsNodeRow() 里缺少 %q（拖拽把手没造出来或没挂上行）", needle)
		}
	}
	// 只有把手能开始拖：整行绑 pointerdown 的话，按住名称想选字、按住按钮想点击
	// 都会变成"把这行拖走了"。
	if regexp.MustCompile(`row\.addEventListener\(\s*'pointerdown'`).MatchString(js) {
		t.Error("pointerdown 绑在了整行上：只有把手才该开始拖动")
	}

	// 必须用 Pointer Events（触摸屏上唯一能用的一套），且用指针捕获保证
	// "拖出这一行/拖出窗口"之后事件还回得来。
	for _, needle := range []string{
		"function startNodeDrag(", "function onNodeDragMove(", "function onNodeDragEnd(",
		"function onNodeDragCancel(", "function endNodeDrag(",
		"document.addEventListener('pointermove', onNodeDragMove)",
		"document.addEventListener('pointerup', onNodeDragEnd)",
		"document.addEventListener('pointercancel', onNodeDragCancel)",
		"handle.setPointerCapture(event.pointerId)",
	} {
		if !strings.Contains(js, needle) {
			t.Errorf("app.js 缺少 %q", needle)
		}
	}
	// 明确禁止 HTML5 那套原生拖放属性：它在触摸屏上根本不触发 dragstart。
	for _, src := range []struct{ name, body string }{{"app.js", js}, {"index.html", html}} {
		if strings.Contains(src.body, "draggable") {
			t.Errorf("%s 里出现了 draggable：触摸屏上不工作，必须用 Pointer Events", src.name)
		}
	}
	// 合成事件里 setPointerCapture 会抛（没有活动指针）：必须包在 try 里，
	// 否则拖动一按下就抛异常，整行都不动了。
	start := funcBody(js, "function startNodeDrag(")
	if !regexp.MustCompile(`(?s)try\s*\{[^}]*setPointerCapture`).MatchString(start) {
		t.Error("setPointerCapture 必须包在 try 里：没有活动指针时会抛 NotFoundError")
	}

	// 松手：先**本地**重排（立刻看到结果），再发请求；原地放下不发请求。
	end := funcBody(js, "function onNodeDragEnd(")
	if end == "" {
		t.Fatal("app.js 缺少 onNodeDragEnd()")
	}
	localAt := strings.Index(end, "applyNodeOrder(order);")
	saveAt := strings.Index(end, "saveNodeOrder(order, state.orderBefore);")
	if localAt < 0 || saveAt < 0 || localAt > saveAt {
		t.Error("松手后应当先本地重排（applyNodeOrder）再发请求（saveNodeOrder）")
	}
	if !regexp.MustCompile(`sameOrder\(order,\s*state\.orderBefore\)`).MatchString(end) {
		t.Error("原地放下（顺序没变）时应当直接返回、不发请求")
	}

	// 请求：PUT /api/v1/nodes/order，请求体是完整顺序。
	save := funcBody(js, "function saveNodeOrder(")
	if save == "" {
		t.Fatal("app.js 缺少 saveNodeOrder()")
	}
	if !strings.Contains(save, "api('/api/v1/nodes/order', { method: 'PUT', body: { ids: ids } })") {
		t.Error("saveNodeOrder() 里没有 PUT /api/v1/nodes/order（请求体必须是 { ids: [...] }）")
	}
	// 成功 → 重新拉一次（首页网格的顺序只有全量接口里有，SSE 推的是变更集）。
	if !strings.Contains(save, "refreshNodeViews()") {
		t.Error("重排成功后必须重新拉一次节点（refreshNodeViews），否则首页卡片顺序不会跟着变")
	}
	if !strings.Contains(js, "SSE 推的是") || !strings.Contains(js, "没有顺序这个概念") {
		t.Error("注释里要写明为什么成功之后必须显式重拉：SSE 推的是变更集、payload 里没有顺序")
	}
	// 失败 → 回滚到拖动前的顺序 + 把错误留在这一栏的错误位上（不是转瞬即逝的 toast）。
	if !strings.Contains(save, "applyNodeOrder(before)") {
		t.Error("重排失败时必须回滚到拖动前的顺序（applyNodeOrder(before)）")
	}
	if !strings.Contains(save, "el.nodesError.textContent = '调整顺序失败：'") {
		t.Error("重排失败时必须把错误显示在这一栏的错误位（#nodes-error）上")
	}
	if regexp.MustCompile(`(?s)function saveNodeOrder\(.*?toast\(`).MatchString(save) {
		t.Error("重排失败只用 toast 提示是不够的：它转眼就没了，页面上仍停在旧顺序")
	}

	// 拖动中不许误触发行里的按钮：拖完那一次 click 要被吞掉。
	if !strings.Contains(js, "suppressRowClick") || !strings.Contains(js, "function saveNodeOrder(") {
		t.Fatal("app.js 缺少 suppressRowClick：拖动结束时补发的那次 click 会点到「编辑节点」")
	}
	if !regexp.MustCompile(`el\.nodesList\.addEventListener\('click',[\s\S]{0,200}\},\s*true\)`).MatchString(js) {
		t.Error("必须在 #nodes-list 的**捕获阶段**拦掉拖完那次 click（冒泡阶段拦已经晚了）")
	}

	// 首页网格的顺序：loadNodes 要按接口顺序重新 append 一遍卡片，
	// 否则已存在的卡片不会移动位置（renderNode 只更新内容）。
	if !strings.Contains(js, "el.grid.appendChild(card.root)") {
		t.Error("loadNodes() 必须按接口顺序重排 #grid 的子节点（否则首页顺序不跟着变）")
	}

	// 样式：把手、拖动中、插入提示、拖动期间禁止选中。
	for _, rule := range []string{
		".node-drag {", ".node-item.dragging", ".node-item.drop-before", ".node-item.drop-after",
		"body.drag-active",
	} {
		if !strings.Contains(css, rule) {
			t.Errorf("style.css 缺少 %s 规则", rule)
		}
	}
	// touch-action:none 少了它，手机上按住把手会被当成滚动页面、指针事件收到
	// pointercancel，拖动断在半路。
	if !regexp.MustCompile(`(?s)\.node-drag\s*\{[^}]*touch-action:\s*none`).MatchString(css) {
		t.Error("style.css 里 .node-drag 必须有 touch-action: none（否则手机上拖动会被当成滚动）")
	}
	if !regexp.MustCompile(`(?s)\.node-drag\s*\{[^}]*user-select:\s*none`).MatchString(css) {
		t.Error("style.css 里 .node-drag 必须有 user-select: none（否则拖动时会选中文字）")
	}
	if !regexp.MustCompile(`(?s)body\.drag-active\s*\{[^}]*user-select:\s*none`).MatchString(css) {
		t.Error("style.css 里 body.drag-active 必须有 user-select: none（指针扫过的文字会被选成一片蓝）")
	}
	// 拖动中的那一行要明显"浮"起来：半透明 + 阴影。
	if !regexp.MustCompile(`(?s)\.node-item\.dragging\s*\{[^}]*opacity:`).MatchString(css) ||
		!regexp.MustCompile(`(?s)\.node-item\.dragging\s*\{[^}]*box-shadow:`).MatchString(css) {
		t.Error("拖动中的那一行应当半透明 + 有抬起的阴影（只靠位置变化看不出它在跟着手）")
	}
	// 插入提示是伪元素画的：真插一个占位元素会把其它行挤动，插入位置就会跟着跳。
	if !regexp.MustCompile(`(?s)\.node-item\.drop-(before|after)::(before|after)\s*\{[^}]*background:\s*var\(--accent\)`).MatchString(css) {
		t.Error("插入提示应当是一条强调色横线，用伪元素画（不能占位）")
	}
	// 行是**单行 flex + 允许折行**（改动 3）：把手 / 内容 / 按钮依次排开，
	// 宽度够时全在一行（标签因此能跟名称并排），不够时按顺序折行。
	// 三个位置关系缺一不可：
	//   - 容器 flex-wrap：不折行就会横向溢出（右栏出现滚动条）；
	//   - 内容列 flex-basis 为 0：写成 auto 时它一宽就把按钮顶到下一行，
	//     而把手会被单独留在第一行（flex 的换行是顺序收集的）；
	//   - 按钮 flex:none：它是行尾的操作，不能被内容挤扁。
	if !regexp.MustCompile(`(?s)\.node-item\s*\{[^}]*display:\s*flex`).MatchString(css) ||
		!regexp.MustCompile(`(?s)\.node-item\s*\{[^}]*flex-wrap:\s*wrap`).MatchString(css) {
		t.Error("style.css 里 .node-item 应当是「允许折行的单行 flex」（内容宽了要能折行，不能横向溢出）")
	}
	if !regexp.MustCompile(`(?s)\.node-item-body\s*\{[^}]*flex:\s*1\s+1\s+0`).MatchString(css) {
		t.Error("style.css 里 .node-item-body 的 flex-basis 应当是 0（basis:auto 会把按钮顶到下一行、把手留在第一行）")
	}
	if !regexp.MustCompile(`(?s)\.node-item-acts\s*\{[^}]*flex:\s*none`).MatchString(css) {
		t.Error("style.css 里 .node-item-acts 应当 flex:none（行尾按钮不参与伸缩）")
	}
	// 窄屏仍然要能拖、能看：按钮整段落到第二行，内容拿到整行宽度；
	// 把手留在第一段最左边（窄屏下它是唯一的拖动入口，不能被挤没）。
	if !regexp.MustCompile(`(?s)@media \(max-width: 900px\).*?\.node-item-acts\s*\{[^}]*flex:\s*1\s+1\s+100%`).MatchString(css) {
		t.Error("窄屏 media query 里应当让 .node-item-acts 整段换行（flex: 1 1 100%）")
	}
	if !regexp.MustCompile(`(?s)\.node-drag\s*\{[^}]*flex:\s*none`).MatchString(css) {
		t.Error("style.css 里 .node-drag 应当 flex:none（把手被挤没就没地方拖了）")
	}

	// 这里原本断言"index.html 里要有一句说明可以拖动排序的提示"。**用户明确要求把它去掉**
	// （嫌占地方），所以断言删了 —— 不是疏漏，别再加回去。
	// 可发现性由行左侧那个六点把手承担；真有人反馈"不知道能拖"，再去谈要不要加提示。
	// 反过来钉一条：提示没了，列表容器本身必须在，否则拖拽的事件挂载点就丢了。
	if !strings.Contains(html, `id="nodes-list"`) {
		t.Error("服务器列表的容器 #nodes-list 不见了 —— 拖拽的事件就挂不上去了")
	}
}

// 主页与详情页也用满宽屏：容器上限从 1200px 放宽到 1600px，与设置页一致。
//
// 为什么是 1600 而不是"不设上限"：在 3440px 的带鱼屏上，内容与行尾元素会隔开
// 两米远，扫一行要来回转头 —— 1600px 是"够宽、又不至于让人读丢行"的那一档。
//
// 顶部栏（.top）与页脚必须跟着一起放宽：它们是所有页面共用的那两行。只放宽内容的话，
// 标题/返回按钮会停在 1200px 的边界上，而下面的内容伸到 1600px，左边缘差 400px，
// 一眼就看出来没对齐（设置页那轮踩过这个坑）。
func TestFrontendHomeAndDetailUseWideLayout(t *testing.T) {
	css := readAsset(t, "style.css")

	// 全站一条 1600px 的内容列：主页（顶栏/汇总条/总览/节点网格）、详情页（头部/正文）、
	// 页脚都在这条列上，左右边缘才会互相对齐。
	for _, sel := range []string{".top", ".summary", ".overview", ".grid", ".detail-head", ".detail-wrap", ".empty", "footer"} {
		rule := cssRule(css, sel)
		if rule == "" {
			t.Errorf("style.css 里找不到 %s 规则", sel)
			continue
		}
		if !strings.Contains(rule, "max-width: 1600px") {
			t.Errorf("%s 应当是 1600px 的内容列（宽屏下两侧留白太大）", sel)
		}
		if strings.Contains(rule, "max-width: 1200px") {
			t.Errorf("%s 还是 1200px：它会与别的列边缘对不齐", sel)
		}
	}
	// 顶栏与内容必须同宽：这是"左边缘对齐"的全部依据。
	if !regexp.MustCompile(`(?s)\.top\s*\{[^}]*max-width:\s*1600px`).MatchString(css) {
		t.Error(".top 必须与内容同宽（否则标题/返回按钮与内容左边缘错开 400px）")
	}
	// 一处 1200px 都不该再留：留着就说明某一块内容仍停在旧的列宽上。
	if n := strings.Count(css, "max-width: 1200px"); n != 0 {
		t.Errorf("style.css 里还有 %d 处 max-width: 1200px（内容列应当统一到 1600px）", n)
	}
	// 反过来：与留白无关的上限一个都不许动（对话框、标签徽章、提示条…）。
	//
	// 这里曾经还钉着一条 "max-width: none" —— 它出自 `.chart-block .empty`
	// （延迟卡的空态提示要占满 canvas 的位置），那条规则随「延迟探测」功能一起
	// 删掉了，所以这条哨兵也跟着去掉：留着它只会永远红。
	for _, keep := range []string{"max-width: 380px", "max-width: 100%", "max-width: calc(100vw - 32px)"} {
		if !strings.Contains(css, keep) {
			t.Errorf("style.css 里少了与留白无关的 %q：它不该被这次改动牵连", keep)
		}
	}
	// 只许改 max-width：写成固定宽度会让窄屏出现横向滚动条。
	if regexp.MustCompile(`[^-\w]width:\s*1600px`).MatchString(css) {
		t.Error("内容列应当是 max-width 而不是固定 width：写死宽度会让窄屏横向溢出")
	}
	// 窄屏的两条兜底规则不许被这次改动删掉（1600px 只是上限，窄屏按可用宽度排）。
	for _, narrow := range []string{"@media (max-width: 900px)", "@media (max-width: 640px)"} {
		if !strings.Contains(css, narrow) {
			t.Errorf("style.css 里缺少 %s：窄屏会退回桌面版布局（可能横向溢出）", narrow)
		}
	}
}

// codeLines 去掉整行注释（缩进后以 // 开头的行）后剩下的 JS 源码。
//
// 断言"某段代码不许存在"时必须先摘掉注释：注释里常常**引用**被删掉的写法
// （"原来是 ctx.moveTo(px, g.top); ctx.lineTo(px, g.top + plotH);"），
// 照原文匹配就会把注释当成代码，测试自己把自己弄红。
//
// 只处理整行注释：本仓库的注释都是整行写的（见 chart.js / app.js），
// 行尾注释会原样留下 —— 保守方向是"宁可多报"，不会漏掉真的代码。
func codeLines(js string) string {
	kept := make([]string, 0, 64)
	for _, line := range strings.Split(js, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// 改动 2①：X 轴的**竖网格线**整条删掉；横网格线（读数值用的）与悬浮十字线
// （交互反馈）都留着。
//
// 为什么钉得这么细：多删少删在页面上都"能看" —— 竖线没删干净只是画面脏；
// 顺手把横线或悬浮线一起删掉，则是读数和交互悄悄失灵，控制台一声不吭。
func TestFrontendChartDropsVerticalGridLines(t *testing.T) {
	chart := readAsset(t, "chart.js")

	// 这条正则认的就是"从绘图区顶部到底部的竖线"那段写法
	// （moveTo(x, g.top) 之后紧跟 lineTo(x, g.top + plotH)）——也就是当初画竖网格线
	// 的那两行。它同时用在 draw()（必须没有）与 drawHover()（必须还有）上，
	// 所以"把竖网格线加回来"一定会让它变红，而不是只断言某个函数名。
	vertical := regexp.MustCompile(`moveTo\([^;]*g\.top\)\s*;\s*(?:[A-Za-z_$][\w$]*\.)*lineTo\([^;]*g\.top \+ plotH\)`)

	draw := codeLines(chartFuncBody(chart, "function draw()"))
	if draw == "" {
		t.Fatal("chart.js 的 draw() 函数体没截取到")
	}
	if vertical.MatchString(draw) {
		t.Error("draw() 里还在画竖网格线：每个刻度位置上那条从绘图区顶部到底部的竖线已经删掉（读数值靠横网格线）")
	}
	// 只删竖线，别把 X 轴整段删掉：标签仍然要按基准间隔 + 自动稀疏画出来。
	if !strings.Contains(draw, "xLabelStep(") {
		t.Error("draw() 应当用 xLabelStep() 定标签间隔（按标签实际宽度自动稀疏）")
	}
	// 匹配到 `opts.xFormat(` 为止、不锁死后面的参数：xFormat 现在会**多收一个
	// 实际间隔**（3d/7d 的格式取决于抽稀后的间隔，见 labelFits 里的说明），
	// 断言写成 `opts.xFormat(ts)` 会把这次改动误判成"标签被删了"。
	if !strings.Contains(draw, "ctx.fillText(opts.xFormat(") {
		t.Error("draw() 里的 X 轴标签不见了：删竖网格线不等于把整段 X 轴删掉")
	}
	// 横网格线（配 Y 轴刻度那几条）必须还在：读数值全靠它。
	if !regexp.MustCompile(`moveTo\(g\.left, py\)`).MatchString(draw) ||
		!regexp.MustCompile(`lineTo\(g\.w - g\.right, py\)`).MatchString(draw) {
		t.Error("横网格线被误删了：Y 轴刻度的读数全靠它")
	}
	if !strings.Contains(draw, "ctx.strokeStyle = i === 0 ? COLORS.axis : COLORS.grid;") {
		t.Error("横网格线的取色（0 是轴线、其余是网格）不该被这次改动牵连")
	}

	// 悬浮时那条竖线是"鼠标停在哪一点"的反馈，不在删除范围内。
	hover := codeLines(chartFuncBody(chart, "function drawHover("))
	if hover == "" {
		t.Fatal("chart.js 的 drawHover() 函数体没截取到")
	}
	if !vertical.MatchString(hover) {
		t.Error("悬浮时的竖线不见了：鼠标停在图上要有反馈（它不是刻度线，别跟着竖网格线一起删）")
	}
}

// 改动 2②：X 轴标签从"基准间隔"出发，按标签的**实际文本宽度**自动稀疏到不重叠，
// 稀疏倍数是 1/2/5/10/15/30/60… 这种整齐档。
//
// 为什么钉得这么细：稀疏坏掉的方式全是静默的 —— 直接按基准间隔画（1h 档 60 个标签）
// 页面照样渲染得出来，只是糊成一片；而"最多画 N 个"这种硬编码在换个字号、格式或
// 窄画布之后要么挤要么空，同样不报错。
func TestFrontendXAxisLabelsDecimateByMeasuredWidth(t *testing.T) {
	chart := readAsset(t, "chart.js")

	// 1) 判定用实际文本宽度 + 一个最小间距常量，而不是"最多 N 个"。
	if !regexp.MustCompile(`var X_LABEL_MIN_GAP = \d+;`).MatchString(chart) {
		t.Error("chart.js 应当有最小间距常量 X_LABEL_MIN_GAP：判定靠它，不是硬编码的『最多 N 个』")
	}
	fit := chartFuncBody(chart, "function labelFits(")
	if fit == "" {
		t.Fatal("chart.js 缺少 labelFits()：标签放不放得下没有判据")
	}
	if !strings.Contains(fit, "ctx.measureText(") {
		t.Error("判定必须用标签的实际文本宽度（ctx.measureText）：写死『最多 N 个』在换字号/格式/画布宽度之后必然失手")
	}
	if !strings.Contains(fit, "X_LABEL_MIN_GAP") {
		t.Error("labelFits() 要用最小间距常量（紧挨着的两个时刻读不出是两个数）")
	}

	// 2) 整齐倍数梯级：1/2/5/10/15/30/60…（钟表上有的分档）。
	if !regexp.MustCompile(`var X_STEP_MULTIPLIERS = \[1, 2, 5, 10, 15, 30, 60,`).MatchString(chart) {
		t.Error("稀疏倍数应当是 1/2/5/10/15/30/60… 这种整齐档（按整数倍递增会冒出『每 7 分钟』）")
	}
	ladder := regexp.MustCompile(`var X_STEP_MULTIPLIERS = \[([^\]]*)\]`).FindStringSubmatch(chart)
	if ladder == nil {
		t.Fatal("chart.js 里找不到 X_STEP_MULTIPLIERS 的定义")
	}
	// 允许的倍数只有这几个：钟表上读得出来的分档（1 分钟…1 小时、2/5 小时…）。
	allowed := map[int]bool{1: true, 2: true, 5: true, 10: true, 15: true, 30: true, 60: true, 120: true, 300: true, 600: true, 1200: true, 3000: true}
	seen := 0
	for _, raw := range strings.Split(ladder[1], ",") {
		n, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil {
			t.Fatalf("X_STEP_MULTIPLIERS 里解析不出数字：%q", raw)
		}
		seen++
		if !allowed[n] {
			t.Errorf("X_STEP_MULTIPLIERS 里的 %d 不是整齐倍数：画出来就是『每 7 分钟』那种没人用的刻度", n)
		}
	}
	if seen < 6 {
		t.Errorf("X_STEP_MULTIPLIERS 只有 %d 级：档位跨度很大时稀疏不到位，标签仍会重叠", seen)
	}

	// 3) 实际间隔 = 基准间隔 × 整齐倍数，逐级试到放得下为止。
	step := chartFuncBody(chart, "function xLabelStep(")
	if step == "" {
		t.Fatal("chart.js 缺少 xLabelStep()：标签间隔没有从基准间隔逐级放大")
	}
	if !strings.Contains(step, "opts.tickBaseSec") {
		t.Error("xLabelStep() 应当从后端给的基准间隔（opts.tickBaseSec）出发")
	}
	if !regexp.MustCompile(`base \* X_STEP_MULTIPLIERS\[i\]`).MatchString(step) {
		t.Error("实际间隔必须是『基准间隔 × 整齐倍数』：少了这一步就退回『直接按基准间隔画』（1h 档 60 个标签挤成一团）")
	}
	if !strings.Contains(step, "labelFits(") {
		t.Error("xLabelStep() 必须靠 labelFits() 逐个倍数试到放得下为止")
	}

	// 4) 锚点仍然钉在绝对时间网格上（切档位时位置稳定），格式逻辑不变。
	//
	//    这里**故意不锁死 xFormat 的参数个数**：格式函数现在会多收一个"实际间隔"，
	//    因为 3d/7d 抽稀后的间隔可能小于一天，只用 MM-DD 会画出一串一模一样的
	//    「09-29 09-29 09-29」。要钉的是"标签文本仍由 opts.xFormat 给"，
	//    不是"它只接受一个参数"。
	//
	//    锚点那句 ceil 从 draw() 挪进了 labelFits()：判定与绘制现在共用同一份
	//    标签计划（见 TestFrontendXAxisLabelsStayInsideCanvas），所以这里按**意图**
	//    断言"网格对齐那一句还在"，而不是钉它在哪个函数里。
	if !regexp.MustCompile(`Math\.ceil\(t0 / step\) \* step`).MatchString(chart) {
		t.Error("标签锚点必须钉在绝对时间网格上（ceil 到间隔的整数倍），不能改成从绘图区左边缘等分")
	}
	draw := codeLines(chartFuncBody(chart, "function draw()"))
	if !strings.Contains(draw, "xLabelStep(") {
		t.Error("draw() 应当用 xLabelStep() 拿这一帧的标签计划（间隔 + 真要画的锚点）")
	}
	if !strings.Contains(draw, "opts.xFormat(") {
		t.Error("标签文本仍然应当由 opts.xFormat 给（短档 HH:MM、长档日期）")
	}

	// 5) 概念只剩一个：tickLabelSec（"实际标签 + 竖网格线"的间隔）已经不存在，
	//    注释里也要写明"它不再影响画面"，免得下一个改它的人以为它还在用。
	if regexp.MustCompile(`tickLabelSec\s*:`).MatchString(chart) {
		t.Error("chart.js 的选项里还留着 tickLabelSec：竖网格线删掉后它不再影响画面，概念已合并进 tickBaseSec")
	}
	if !strings.Contains(chart, "不再影响画面") {
		t.Error("注释里要写明『tickLabelSec 已经不再影响画面上的任何一条线』")
	}
	// 为什么不能直接按基准间隔画、为什么倍数必须整齐：两段理由都要留在注释里。
	for _, note := range []string{"重叠", "整齐倍数"} {
		if !strings.Contains(chart, note) {
			t.Errorf("注释里缺少 %q：下一个改 X 轴的人得知道为什么不能直接按基准间隔画、为什么倍数必须整齐", note)
		}
	}
}

// 改动 1 / 2：X 轴标签不许压到 Y 轴刻度上，也不许被画布右缘裁掉半个字。
//
// 两条边界规则必须写在**抽稀判定**（labelFits）里，而不是"判定归判定、画的
// 时候顺手裁一下"：两边各写一套时，判定说放得下、画出来却是挤的或者半个字，
// 页面照样能看 —— 除了盯着屏幕看，没有别的线索。
//
// 具体到写法上还有两个必须钉住的点：
//   - X 标签是**居中**锚定的（textAlign: 'center'），所以要比的是标签的**边缘**
//     （left = px - half、right = px + half），不是锚点本身 —— 拿锚点去比，
//     判定会宽松半个标签，最左边那个先压上去；
//   - 右边界比的是**画布**宽度 g.w，不是绘图区右缘（g.w - g.right）：标签本来就
//     画在绘图区之外，拿绘图区右缘去比会把本来画得下的整条标签丢掉。
func TestFrontendXAxisLabelsStayInsideCanvas(t *testing.T) {
	chart := readAsset(t, "chart.js")

	fit := chartFuncBody(chart, "function labelFits(")
	if fit == "" {
		t.Fatal("chart.js 的 labelFits() 函数体没截取到")
	}

	// 1) 用标签的半个宽度算出左右边缘（居中锚定 → 要先 / 2 再减/加）。
	if !strings.Contains(fit, "/ 2") {
		t.Error("labelFits() 应当用 measureText 的宽度除以 2 得到半个标签宽（居中锚定）")
	}
	if !regexp.MustCompile(`-\s*half`).MatchString(fit) || !regexp.MustCompile(`\+\s*half`).MatchString(fit) {
		t.Error("labelFits() 应当分别算出标签的左边缘（px - half）与右边缘（px + half）")
	}

	// 2) 左边界：左边缘越过绘图区左缘（g.left）的标签不画 —— 那里是 Y 轴刻度文字。
	if !regexp.MustCompile(`left\s*<\s*g\.left`).MatchString(fit) {
		t.Error("labelFits() 缺少左边界检查：最左边那个标签会压到 Y 轴刻度文字上")
	}
	// 3) 右边界：右边缘越过画布右缘（g.w）的标签不画。
	if !regexp.MustCompile(`right\s*>\s*g\.w`).MatchString(fit) {
		t.Error("labelFits() 缺少右边界检查：贴着右缘的那个标签会被画布裁掉半个字")
	}
	if regexp.MustCompile(`right\s*>\s*g\.w\s*-\s*g\.right`).MatchString(fit) {
		t.Error("右边界应当比画布右缘 g.w，不是绘图区右缘 g.w - g.right：那会把本来画得下的标签也丢掉")
	}

	// 4) 越界了怎么处理：左边**跳过**（整排标签往后挪一个间隔），右边**整条不画**
	//    （锚点单调递增，后面的只会更靠右，直接结束）。
	//    两条都不许"夹取"到边界上：夹了标签就不在它代表的时刻上了，读出来的时间是错的。
	if !regexp.MustCompile(`if \(left < g\.left\) continue;`).MatchString(fit) {
		t.Error("左边越界的标签应当跳过（整体后移一个间隔），而不是把它夹到边界上")
	}
	if !regexp.MustCompile(`if \(right > g\.w\) break;`).MatchString(fit) {
		t.Error("右边越界的标签应当整条不画（它后面的只会更靠右，循环直接结束）")
	}

	// 5) 判定与绘制共用**同一份**结果：draw() 遍历计划里的锚点，不自己重算一遍。
	//    自己重算就等于把上面两条边界规则写第二遍 —— 那正是"判定通过、画出来被裁"的来源。
	draw := codeLines(chartFuncBody(chart, "function draw()"))
	if draw == "" {
		t.Fatal("chart.js 的 draw() 函数体没截取到")
	}
	if regexp.MustCompile(`Math\.ceil\(t0 / step\)`).MatchString(draw) {
		t.Error("draw() 不该自己重算标签锚点：它必须画 xLabelStep() 给出的那一份计划")
	}
	if !strings.Contains(draw, "plan.ticks") {
		t.Error("draw() 应当遍历标签计划里的 ticks（判定过的那几条），而不是从网格重新铺一遍")
	}
	// xLabelStep 返回的是**整份计划**（含间隔与要画的锚点），不再是光秃秃一个秒数：
	// 只回一个 step 的话，draw() 就得自己从时间网格重新铺一遍锚点 —— 那正是两条
	// 边界规则被写第二遍的地方。
	step := chartFuncBody(chart, "function xLabelStep(")
	if step == "" {
		t.Fatal("chart.js 的 xLabelStep() 函数体没截取到")
	}
	if !strings.Contains(step, "labelFits(") {
		t.Error("xLabelStep() 必须靠 labelFits() 挑间隔（含两条边界规则）")
	}
	if !regexp.MustCompile(`return labelFits\(`).MatchString(step) || !strings.Contains(step, "return fit;") {
		t.Error("xLabelStep() 应当把 labelFits() 的结果（含 ticks）整份返回给 draw()")
	}
	if regexp.MustCompile(`return step;`).MatchString(step) {
		t.Error("xLabelStep() 只回一个间隔秒数的话，draw() 就得自己重算锚点，边界规则会被写第二遍")
	}
}
