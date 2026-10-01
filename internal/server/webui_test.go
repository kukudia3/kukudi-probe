package server

import (
	"io/fs"
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"probe/internal/store"
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
		`id="info-network"`, `id="info-traffic"`, `id="charts-resources"`, `id="charts-latency"`,
		`id="stat-price"`, `id="stat-monthly"`, `id="stat-left"`, `id="stat-value"`,
		`id="detail-ranges"`, `id="lat-ranges"`, `id="chart-cpu"`, `id="chart-mem"`,
		`id="chart-disk"`, `id="chart-net"`, `id="chart-lat"`, `id="chart-traffic"`, `id="detail-back"`,
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
		// 延迟探测（Phase 14）：详情页的「延迟」改成用户配置的探测目标。
		// 目标行是 app.js 动态生成的，HTML 里只有容器与按钮。
		`id="ping-hint"`, `id="ping-list"`, `id="ping-add"`, `id="ping-limit"`,
		`id="ping-interval"`, `id="ping-save"`, `id="ping-error"`, `id="ping-ok"`,
		`id="lat-targets"`, `id="lat-empty"`,
		// 服务器列表 + 标签（Phase 16 → Phase 18 改版）：一行一台机器，行由 app.js 造，
		// HTML 里只有容器与按钮。标签没有自己的对话框了 —— 它在「新增/编辑节点」
		// 对话框里就是一个普通文本框（多个标签用 ; 分隔）。
		`id="nodes-list"`, `id="nodes-add"`, `id="nodes-empty"`, `id="nodes-error"`,
		`id="node-tags"`, `id="node-tags-hint"`,
		// 首页分组筛选（改动 B）：一排 chip，选项由 app.js 从**实际存在的分组**
		// 生成，HTML 里只有一个空容器。
		`id="group-filter"`,
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

	// 八个栏名要在导航与内容里各出现一次且顺序一致：少一个就是"点进去一片空白"，
	// 多一个就是"有个按钮切不出内容"。
	want := []string{"notify", "alert", "dashboard", "ping", "nodes", "security", "server", "audit"}
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

// 详情页由三段组成：汇总排（4 格）→ 信息卡网格（5 张）→ 图表卡（2 张，共 6 张图）。
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

	// 图表：6 个 chart-block，每个带 data-chart，且有对应的 canvas。
	charts := []string{"cpu", "mem", "disk", "net", "lat", "traffic"}
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
	// 图表卡拆成两张之后，每张**各自**判断：五张资源图全关掉时不能连延迟图一起藏，
	// 反过来也一样；两边都关掉时页面上不该留下任何空块。
	for _, id := range []string{"chartsResources", "chartsLatency"} {
		if !strings.Contains(js, "el."+id) {
			t.Errorf("applyChartVisibility 应当分别收起两张图表卡（app.js 里没有 el.%s）", id)
		}
	}
	if !regexp.MustCompile(`card\.hidden\s*=\s*shown\.length\s*===\s*0`).MatchString(js) {
		t.Error("一张图都不显示的图表卡应当整体收起来（否则只剩一个空边框）")
	}
}

// 设置页多一栏「延迟探测」：左栏导航项 + 右栏内容栏，位置在「仪表盘」之后、
// 「安全」之前（它和仪表盘一样，讲的都是"详情页上画什么"）。
//
// 目标行是动态生成的（数量可变），所以这里只能按"生成方式"断言：控件必须用
// createElement 造、引用挂在行对象上。拼 id 字符串再 getElementById 既用不上
// main() 那份 el 自动登记，也容易和别处的 id 撞车 —— 撞了就是静默拿到 null。
func TestFrontendPingSettingsPane(t *testing.T) {
	html := readAsset(t, "index.html")
	js := readAsset(t, "app.js")
	css := readAsset(t, "style.css")

	nav := strings.Index(html, `class="nav-item" data-pane="ping"`)
	if nav < 0 {
		t.Fatal(`index.html 的左栏导航里缺少 data-pane="ping" 的按钮`)
	}
	dash := strings.Index(html, `class="nav-item" data-pane="dashboard"`)
	sec := strings.Index(html, `class="nav-item" data-pane="security"`)
	if dash < 0 || sec < 0 {
		t.Fatal("index.html 里找不到「仪表盘」或「安全」导航项")
	}
	if !(dash < nav && nav < sec) {
		t.Error("「延迟探测」应当排在「仪表盘」之后、「安全」之前")
	}
	if !strings.Contains(html, `<section class="pane" data-pane="ping" hidden>`) {
		t.Error(`index.html 里缺少 data-pane="ping" 的内容栏（或它没有 hidden）`)
	}
	// 每栏各自一对提示元素，不复用别栏的。
	for _, id := range []string{"ping-error", "ping-ok", "ping-list", "ping-add", "ping-interval", "ping-save"} {
		if !strings.Contains(html, `id="`+id+`"`) {
			t.Errorf("延迟探测栏缺少 id=%s", id)
		}
	}

	// 保存走冻结的契约：PUT /api/v1/settings/ping。
	body := funcBody(js, "function savePing()")
	if body == "" {
		t.Fatal("app.js 缺少 savePing()")
	}
	if !strings.Contains(body, "api('/api/v1/settings/ping', { method: 'PUT', body: payload })") {
		t.Error("savePing() 里没有 PUT /api/v1/settings/ping")
	}
	// 编辑已有目标必须回传它的 id：id 是曲线身份，丢了服务端会当成新目标，
	// 那条曲线的历史就断在这里。
	if !strings.Contains(js, "id: row.id") {
		t.Error("请求体里缺少目标 id（编辑已有目标必须回传 id）")
	}

	// 行内控件必须是 createElement 造出来的。
	for _, needle := range []string{"function newPingRow(", "document.createElement('select')", "row.refs = {"} {
		if !strings.Contains(js, needle) {
			t.Errorf("app.js 缺少 %q（目标行应当用 createElement 动态生成）", needle)
		}
	}
	// 提交前先本地校验一遍：不合法就只显示错误，不白发一个请求。
	for _, needle := range []string{"function validatePing(", "TCP 端口必须是 1-65535 之间的整数", "地址不能为空"} {
		if !strings.Contains(js, needle) {
			t.Errorf("app.js 缺少 %q（非法输入会被直接发到服务端）", needle)
		}
	}
	if !strings.Contains(js, "el.pingSave.disabled = true;") {
		t.Error("保存期间应当禁用「保存」按钮，避免连点发出两次 PUT")
	}
	// 达到 max_targets 时「＋ 添加目标」要禁用。
	if !strings.Contains(js, "el.pingAdd.disabled = full;") {
		t.Error("达到 max_targets 时应当禁用「＋ 添加目标」")
	}

	// 目标行用 grid 排列，窄屏堆叠：六列硬挤在窄屏里会把地址框压到没法编辑。
	if !regexp.MustCompile(`(?s)\.ping-row\s*\{[^}]*display:\s*grid`).MatchString(css) {
		t.Error("style.css 里 .ping-row 应当是 grid 布局")
	}
	if !regexp.MustCompile(`(?s)@media \(max-width: 900px\).*?\.ping-row\s*\{[^}]*grid-template-columns`).MatchString(css) {
		t.Error("窄屏 media query 里应当把 .ping-row 改成两列")
	}
}

// 详情页的「延迟」图改成画**探测目标**，不再用 /series 的 lat 指标。
//
// 旧 lat 是 Agent 到面板自身的 WebSocket ping/pong 往返（走 Cloudflare 隧道时
// 恒为 ~100ms），画成曲线没有参考价值；它现在只留在「网络信息」卡里，见
// TestFrontendPanelLatencyLabelIsUnambiguous。
func TestFrontendLatencyChartUsesPingTargets(t *testing.T) {
	js := readAsset(t, "app.js")

	if strings.Contains(js, "metric=lat") {
		t.Error("app.js 里还在请求 /series 的 lat 指标")
	}
	series := funcBody(js, "function loadSeries()")
	if series == "" {
		t.Fatal("app.js 缺少 loadSeries()")
	}
	if strings.Contains(series, "'lat'") {
		t.Error("loadSeries() 的 metrics 列表里还有 lat（延迟图不该再走 /series）")
	}

	// 数据改为按探测目标取。
	if !strings.Contains(js, "'/api/v1/nodes/' + detail.id + '/ping?range='") {
		t.Error("app.js 里没有请求 /api/v1/nodes/<id>/ping?range=...")
	}
	ping := funcBody(js, "function loadPingChart()")
	if ping == "" {
		t.Fatal("app.js 缺少 loadPingChart()")
	}
	// 与"隐藏的图不发请求"同一条约定。
	if !strings.Contains(ping, "if (!chartVisible('lat')) return Promise.resolve();") {
		t.Error("被图表可见性隐藏时不该请求 /ping")
	}
	if !strings.Contains(ping, "if (!t.has_data) return;") {
		t.Error("has_data:false 的目标应当跳过（不画线，但勾选框里仍要有它）")
	}

	// 一个目标都没配：显示空态提示，且**不发请求**。
	if !strings.Contains(js, "还没有配置探测目标") {
		t.Error("app.js 缺少「还没有配置探测目标」的空态提示")
	}
	if !strings.Contains(js, "function setLatEmpty(") {
		t.Error("空态提示应当由 setLatEmpty() 用 textContent 渲染")
	}
	// 有没有配目标只有设置接口知道（/ping 的返回不算：那时请求已经发出去了）。
	if !strings.Contains(js, "loadPingTargets().then(loadPingChart)") {
		t.Error("openDetail() 应当先取一次目标列表，再决定要不要请求 /ping")
	}

	// 勾选状态按 target id 存 localStorage。
	if !strings.Contains(js, "var PING_HIDDEN_KEY = 'probe-ping-hidden';") {
		t.Error("勾选状态应当存在 localStorage 的 probe-ping-hidden 里")
	}
	for _, needle := range []string{"function toggleLatTarget(", "function applyLatSeries(", "function renderLatToggles("} {
		if !strings.Contains(js, needle) {
			t.Errorf("app.js 缺少 %q（取消勾选要能只重画曲线、不销毁图表实例）", needle)
		}
	}
}

// 详情页的图表分成**两张卡**：「资源与网络」（cpu/mem/disk/net/traffic）与
// 「延迟」（lat）。
//
// 为什么拆：延迟图原本夹在「网络」和「近 7 天流量」中间，把这两张同源的图
// （都来自 Agent 上报的网卡计数）拆散了；而延迟画的是"到外部探测目标"的往返，
// 与"这台机器自己的资源"也不是一回事。
//
// 卡片 id 是 app.js 按名字找的（applyChartVisibility 逐卡收起），
// 拼错就是"某一类图全被隐藏之后页面上留下一个空边框"，只有浏览器里能看出来。
func TestFrontendChartCardsAreSplit(t *testing.T) {
	html := readAsset(t, "index.html")
	js := readAsset(t, "app.js")
	css := readAsset(t, "style.css")

	res := sectionBody(t, html, "charts-resources")
	lat := sectionBody(t, html, "charts-latency")
	if res == "" || lat == "" {
		t.Fatal("index.html 里应当有两张图表卡：charts-resources 与 charts-latency")
	}
	// 旧的单卡容器必须彻底删掉：留着的话两套容器里会出现同一批 canvas id，
	// getElementById 只认第一个，另一处永远画不出来。
	if strings.Contains(html, `id="detail-charts"`) {
		t.Error("index.html 里还留着旧的单张图表卡 detail-charts")
	}

	for _, key := range []string{"cpu", "mem", "disk", "net", "traffic"} {
		if !strings.Contains(res, `data-chart="`+key+`"`) {
			t.Errorf("「资源与网络」卡里缺少 data-chart=%q", key)
		}
	}
	if !strings.Contains(lat, `data-chart="lat"`) {
		t.Errorf(`「延迟」卡里缺少 data-chart="lat"`)
	}
	// 延迟图的勾选框与空态跟着图一起搬进卡片 B，不能落在卡片 A 里。
	for _, id := range []string{"chart-lat", "lat-targets", "lat-empty"} {
		if !strings.Contains(lat, `id="`+id+`"`) {
			t.Errorf("「延迟」卡里缺少 id=%s", id)
		}
	}
	// 卡片 A 里不能再有第二张延迟图（两处图块 data-chart="lat" 会让勾选状态与
	// 实际画出来的图对不上）。注意只数 chart-block：设置页的勾选框也用 data-chart。
	if n := strings.Count(html, `class="chart-block" data-chart="lat"`); n != 1 {
		t.Errorf(`图块 class="chart-block" data-chart="lat" 出现了 %d 次，期望 1 次`, n)
	}
	// 五张图在宽屏排两列：可见个数是奇数时最后一个横跨整行，
	// 否则右下角会空出半格。
	if !regexp.MustCompile(`(?s)\.chart-grid\s*\{[^}]*display:\s*grid`).MatchString(css) ||
		!regexp.MustCompile(`(?s)\.chart-grid\s*\{[^}]*repeat\(2,`).MatchString(css) {
		t.Error("style.css 里 .chart-grid 应当是两列网格")
	}
	if !regexp.MustCompile(`\.chart-grid\s*>\s*\.chart-block\.span-full\s*\{[^}]*grid-column:\s*1\s*/\s*-1`).MatchString(css) {
		t.Error("style.css 里 .chart-block.span-full 应当跨满整行")
	}
	// 跨满整行的那一个由 app.js 按**当前可见个数**决定：被隐藏的图块仍然是
	// 子节点，纯 CSS 的 :last-child:nth-child(odd) 数不到"可见的兄弟"。
	if !regexp.MustCompile(`function spanFullRow\(`).MatchString(js) {
		t.Fatal("app.js 应当有 spanFullRow()：按可见个数决定哪张图跨满整行")
	}
	if !regexp.MustCompile(`spanFullRow\(shown\)`).MatchString(js) {
		t.Error("applyChartVisibility 应当在切完显隐之后调用 spanFullRow(shown)")
	}
	if !regexp.MustCompile(`blocks\.length\s*%\s*2\s*===\s*1`).MatchString(js) {
		t.Error("只有可见个数为奇数时才需要跨满整行")
	}
	if !regexp.MustCompile(`(?s)@media \(max-width: 900px\).*?\.chart-grid\s*\{[^}]*1fr`).MatchString(css) {
		t.Error("窄屏 media query 里应当把 .chart-grid 改成单列")
	}
	// .card 自己写了 display:flex，会盖掉 hidden 属性那条 display:none ——
	// 少这一条，被整张收起的图表卡会留下一条只有标题的空边框（浏览器里量到 50px）。
	if !regexp.MustCompile(`\.card\.charts\[hidden\]\s*\{[^}]*display:\s*none`).MatchString(css) {
		t.Error("style.css 缺少 .card.charts[hidden] { display: none }：收起的图表卡会留下空边框")
	}
	traffic := strings.Index(res, `data-chart="traffic"`)
	if traffic < 0 {
		t.Fatal("「资源与网络」卡里没有「近 7 天流量」")
	}
	if strings.Contains(res[traffic+1:], `data-chart="`) {
		t.Error("「近 7 天流量」应当是卡片 A 里最后一张图（CSS 靠 last-child 让它跨满整行）")
	}

	// 丢包竖条的接线：series 上带 bars 描述，值取点里的第 4 位（0-100 的丢包率）。
	if !strings.Contains(js, "bars: { valueIndex: 3, max: 100 }") {
		t.Error("app.js 应当给每个探测目标的 series 加 bars（丢包竖条）")
	}
	if !strings.Contains(js, "bars: s.bars") {
		t.Error("setChart() 必须原样透传 bars（漏掉的话竖条静默画不出来）")
	}
	// 区间聚合丢包率挂在图例（勾选框）上，0 时不写后缀。
	if !strings.Contains(js, "' · 丢包 '") {
		t.Error("app.js 里缺少「· 丢包 X%」这个图例后缀")
	}
	if !regexp.MustCompile(`if \(t\.loss_pct > 0\)`).MatchString(js) {
		t.Error("丢包率为 0 时不该显示丢包后缀（否则每个目标都挂一句「丢包 0%」）")
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

// 首页要有总览区（所有机器加起来的合计），节点卡片里要有「延迟 / 丢包」迷你条。
//
// 两块内容都由 app.js 用 createElement 动态填（数据驱动的格子数可变），
// 所以这里钉的是"容器 + 取数时机 + 结构位置"：容器 id 拼错、接口路径写错、
// 或者把迷你条建出来却忘了挂进卡片，浏览器里都只表现成"少了一块"，
// 而卡片与实时流照常工作 —— 除了盯着屏幕看，没有别的线索。
func TestFrontendHomeOverviewAndMiniBars(t *testing.T) {
	html := readAsset(t, "index.html")
	js := readAsset(t, "app.js")
	css := readAsset(t, "style.css")

	// 总览容器必须在节点网格**上方**：它是"集群整体"的结论，看完再往下看单台。
	ovAt := strings.Index(html, `id="overview"`)
	gridAt := strings.Index(html, `id="grid"`)
	if ovAt < 0 {
		t.Fatal(`index.html 里缺少总览区容器 id="overview"`)
	}
	if gridAt < 0 || ovAt > gridAt {
		t.Error("总览区应当排在节点网格（#grid）之前")
	}

	// 一次请求同时拿合计与分桶数据。路径写错的话总览与迷你条会一起消失。
	if !strings.Contains(js, "api('/api/v1/overview?window=1h&buckets=10')") {
		t.Error("app.js 里没有请求 /api/v1/overview")
	}
	// 每分钟一次，**不跟 SSE**：SSE 是每秒级的，总览是分钟级的数据。
	if !strings.Contains(js, "var OVERVIEW_POLL_MS = 60000;") {
		t.Error("总览应当每 60 秒取一次（OVERVIEW_POLL_MS = 60000）")
	}
	if !regexp.MustCompile(`function stopOverview\(`).MatchString(js) {
		t.Fatal("app.js 缺少 stopOverview()：离开首页时定时器必须停掉")
	}
	if !regexp.MustCompile(`window\.clearInterval\(overviewTimer\)`).MatchString(js) {
		t.Error("stopOverview() 应当 clearInterval 掉定时器")
	}
	// 定时器不能叠加：进首页的动作（hashchange、登录、会话刷新）会反复触发，
	// 每触发一次就 setInterval 一个的话，请求数会随时间翻倍。
	if !regexp.MustCompile(`if \(overviewTimer\) return;`).MatchString(js) {
		t.Error("startOverview() 应当先判断定时器是否已经在跑")
	}
	if !strings.Contains(js, "syncOverviewTimer();") {
		t.Error("总览定时器应当跟着视图切换（setView 里调 syncOverviewTimer）")
	}

	// 迷你条：容器由 createCard 建、默认隐藏（有没有数据要等接口回来）。
	card := funcBody(js, "function createCard(")
	if card == "" {
		t.Fatal("app.js 缺少 createCard()")
	}
	if !strings.Contains(card, "createMiniBar()") || !strings.Contains(card, "mini.root") {
		t.Error("createCard() 应当把迷你条建出来并挂进卡片（建了不 append 等于没做）")
	}
	// 卡片的骨架顺序：四格资源 → 点线引导行 → 延迟/丢包迷你条 → 标签行。
	// 迷你条排在引导行**之后**：引导行是逐项读数（速率/在线/最后通信/费用/探测），
	// 迷你条是"线路最近一小时怎么样"的结论，读完之后再看它。
	resAt := strings.Index(card, "root.appendChild(res);")
	linesAt := strings.Index(card, "root.appendChild(lines);")
	miniAt := strings.Index(card, "root.appendChild(mini.root);")
	tagsAt := strings.Index(card, "root.appendChild(tags);")
	if resAt < 0 || linesAt < 0 || miniAt < 0 || tagsAt < 0 ||
		!(resAt < linesAt && linesAt < miniAt && miniAt < tagsAt) {
		t.Error("卡片骨架应当依次是：四格资源 → 点线引导行 → 迷你条 → 标签行")
	}

	mini := funcBody(js, "function createMiniBar()")
	if mini == "" {
		t.Fatal("app.js 缺少 createMiniBar()")
	}
	if !strings.Contains(mini, ".hidden = true") {
		t.Error("迷你条默认应当隐藏：没配探测目标时不该留一个空框")
	}
	for _, needle := range []string{"function renderMiniBar(", "function renderMiniRow("} {
		if !strings.Contains(js, needle) {
			t.Errorf("app.js 缺少 %q", needle)
		}
	}
	if !regexp.MustCompile(`bar\.root\.hidden = true;`).MatchString(js) {
		t.Error("renderMiniBar() 在这个节点没有数据时应当把整块藏起来")
	}

	// 配色阈值分开写、都带注释（以后调阈值只改这几处）。
	// 延迟那两个口径（绝对阈值 + 本机倍数）**两行共用**：迷你条的延迟格子与
	// 「探测」那一行都走 latGrade()，常量只有一组（MINI_LAT_*_RATIO / LAT_ABS_*）——
	// 原先「探测」行那一对 PROBE_LAT_*_RATIO 已经在这次统一里合并掉了。
	for _, needle := range []string{
		"var MINI_LOSS_WARN_PCT = 5;",
		"var MINI_LAT_WARN_RATIO = 1.2;",
		"var MINI_LAT_BAD_RATIO = 2;",
		"var LAT_ABS_WARN_MS = 180;",
		"var LAT_ABS_BAD_MS = 240;",
	} {
		if !strings.Contains(js, needle) {
			t.Errorf("app.js 缺少配色阈值 %q", needle)
		}
	}
	// 合并之后不许留下没人用的常量：旧的两行各一套倍数，新代码只认一组。
	for _, gone := range []string{"PROBE_LAT_WARN_RATIO", "PROBE_LAT_BAD_RATIO"} {
		if strings.Contains(js, gone) {
			t.Errorf("app.js 里还留着 %s：两行已经共用同一组阈值，留着就是一份没人读的旧口径", gone)
		}
	}
	// 没有数据的那一段：不加颜色类（留浅灰底），绝不画成 0。
	// 现在有两个分级函数（丢包格子、延迟的 latGrade）各有一条同样的兜底。
	if n := strings.Count(js, "if (typeof value !== 'number') return '';"); n != 2 {
		t.Errorf("丢包格子与 latGrade 都应当把 null 判成「没有数据」，实际找到 %d 处", n)
	}

	// 样式：格子、颜色、以及 hidden 那条兜底规则。
	for _, rule := range []string{
		".overview", ".ov-item", ".ov-label", ".ov-value", ".ov-line",
		".card-mini", ".mini-head", ".mini-value", ".mini-cells", ".mini-cell",
		".mini-cell.ok", ".mini-cell.warn", ".mini-cell.bad",
	} {
		if !strings.Contains(css, rule) {
			t.Errorf("style.css 缺少 %s 规则", rule)
		}
	}
	// .overview / .card-mini 自己写了 display，会盖掉 hidden 属性那条 display:none。
	for _, sel := range []string{".overview", ".card-mini"} {
		if !regexp.MustCompile(regexp.QuoteMeta(sel) + `\[hidden\]\s*\{[^}]*display:\s*none`).MatchString(css) {
			t.Errorf("style.css 缺少 %s[hidden] { display: none }：收起时仍会占位置", sel)
		}
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

	// 点线引导行：五行的标签，以及"费用没填价格就整行不显示"。
	if !strings.Contains(js, `['probe', '探测']`) || !strings.Contains(js, `['seen', '最后通信']`) {
		t.Error("点线引导行里缺少「探测」或「最后通信」")
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

	// 「探测」那一行：每个目标一个当前延迟，按该目标这一小时的平均值着色 ——
	// 与迷你条的延迟格子**同一套判据**（latGrade：绝对阈值与本机倍数取严），
	// 只是基准不同（这里比该目标自己的窗口均值，格子比整节点的）。
	probe := funcBody(js, "function renderProbeLine(")
	if probe == "" {
		t.Fatal("app.js 缺少 renderProbeLine()：「探测」那一行没画")
	}
	for _, needle := range []string{"mini.targets", "latGrade(t.lat_ms, t.avg_ms)", "miniLatText(t.lat_ms)"} {
		if !strings.Contains(probe, needle) {
			t.Errorf("renderProbeLine() 里缺少 %q", needle)
		}
	}
	// 名称留空时回落到 host，这个回落由前端做（后端只给原始 label）。
	if !strings.Contains(probe, "pingTargetLabel(t)") {
		t.Error("探测行的悬停标题应当用 pingTargetLabel()（label 为空时回落 host）")
	}

	// 卡片上不许再出现「面板延迟」：它与探测结果不是一回事，摆在一起必然被读混。
	if strings.Contains(card, "面板延迟") || strings.Contains(card, "dto.lat_ms") {
		t.Error("首页卡片上还留着「面板延迟」（lat_ms）：它与「探测」是两回事")
	}
	// 但详情页「网络信息」卡里必须还在（那是有上下文的地方）。
	if !regexp.MustCompile(`infoRow\(net, '面板延迟',\s*node\.lat_ms`).MatchString(js) {
		t.Error("详情页「网络信息」卡里的「面板延迟」被误删了")
	}

	// 样式：2×2 网格、格子的三段、引导行、以及按倍数着色的三种颜色。
	for _, rule := range []string{
		".card-res", ".res-cell", ".res-head", ".res-pct", ".res-sub",
		".card-lines", ".line ", ".line-label", ".line-lead", ".line-value",
		".line-num.ok", ".line-num.warn", ".line-num.bad",
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
// updateCard / renderProbeLine）都排在这一段里，所以"卡片渲染里不许出现某个词"
// 可以整块断言，不必逐函数去数 —— 漏掉一个函数就等于漏掉一处误删。
func homeCardSource(js string) string {
	start := strings.Index(js, "function resCell(")
	end := strings.Index(js, "function renderSummary(")
	if start < 0 || end < 0 || end < start {
		return ""
	}
	return js[start:end]
}

// 鼠标停在迷你条的某一格上要弹出浮层：第一行时间段、第二行该段的值，同时高亮这一格。
//
// 这里按"意图"断言，不钉死源码字面量（实现怎么拆行、函数叫什么都可以），但几条
// 浏览器里才看得出来的坑必须守住：
//   - 时间段要来自后端下发的 bucket_ts（前端自己推桶边界会与真实桶错开一整格）；
//   - 没有数据的格子写「无数据」，不是 0 ms / 0%；
//   - 浮层整页只有一个（每格一个就是上千个元素），且不吃鼠标事件（否则疯狂闪烁）；
//   - 位置必须夹进视口（最右一列卡片、贴底的一行不能把浮层推出屏幕）。
func TestFrontendMiniBarHoverTooltip(t *testing.T) {
	js := readAsset(t, "app.js")
	css := readAsset(t, "style.css")

	// 时间段来自 /overview 的 bucket_ts，而不是前端用当前时间算出来的。
	if !strings.Contains(js, "bucket_ts") {
		t.Error("app.js 没有读取 /overview 的 bucket_ts：浮层上的时间段会是前端自己推的")
	}
	if !regexp.MustCompile(`overviewBucketTS\s*=\s*data\.bucket_ts`).MatchString(js) {
		t.Error("loadOverview() 应当把响应里的 bucket_ts 存下来供浮层使用")
	}
	// 「现在 − 窗口 + i×桶宽」这套推算不许出现：那是前端算术，而且与桶边界对不上。
	if regexp.MustCompile(`Date\.now\(\)[^\n]*3600`).MatchString(js) {
		t.Error("app.js 里在自己推算桶边界（应当直接用后端给的 bucket_ts）")
	}

	// 格子必须绑悬停事件：mouseover/mousemove 显示、mouseout 收起。
	for _, ev := range []string{"'mouseover'", "'mousemove'", "'mouseout'"} {
		if !strings.Contains(js, "addEventListener("+ev) {
			t.Errorf("迷你条的格子没有绑定 %s（悬停不会弹出/收起浮层）", ev)
		}
	}
	if !regexp.MustCompile(`function bindMiniCell\(`).MatchString(js) {
		t.Fatal("app.js 缺少 bindMiniCell()：事件应当在格子创建时绑一次，而不是每次刷新都叠一层")
	}
	for _, fn := range []string{"function showMiniTip(", "function hideMiniTip(", "function placeMiniTip("} {
		if !regexp.MustCompile(regexp.QuoteMeta(fn)).MatchString(js) {
			t.Errorf("app.js 缺少 %s", fn)
		}
	}

	// 浮层：createElement + textContent 造出来，且**共用一个**。
	if !regexp.MustCompile(`function ensureMiniTip\(`).MatchString(js) {
		t.Fatal("app.js 缺少 ensureMiniTip()：浮层应当只建一次并复用")
	}
	if !regexp.MustCompile(`if \(miniTip\) return miniTip;`).MatchString(js) {
		t.Error("ensureMiniTip() 应当先复用已有的浮层（每格建一个就是上千个元素）")
	}
	if !regexp.MustCompile(`document\.body\.appendChild\(tip\)`).MatchString(js) {
		t.Error("浮层应当挂在 <body> 上：塞进卡片会被祖先的圆角/overflow 裁掉")
	}

	// 第一行是 "HH:MM – HH:MM"（两个时刻各来自一格起点/下一格起点）。
	if !regexp.MustCompile(`clockOf\(start\)\s*\+\s*' – '\s*\+\s*clockOf\(end\)`).MatchString(js) {
		t.Error("浮层第一行应当是 `HH:MM – HH:MM`（两端都由后端给的桶边界格式化而来）")
	}
	// 第二行：没有数据的格子写「无数据」，不写 0 / —。
	if !strings.Contains(js, "'无数据'") || !regexp.MustCompile(`typeof value !== 'number'`).MatchString(js) {
		t.Error("没有数据的格子应当显示「无数据」，而不是 0 ms / 0% / —")
	}
	if !regexp.MustCompile(`return value\.toFixed\(1\) \+ ' ms';`).MatchString(js) {
		t.Error("延迟格的浮层应当是一位小数 + ' ms'（与卡片脚注、详情页写法一致）")
	}

	// 高亮：悬停那一格要加类，且这个类必须在每秒重画时被补回来（否则高亮每秒闪一下）。
	if !regexp.MustCompile(`function setMiniHighlight\(`).MatchString(js) {
		t.Fatal("app.js 缺少 setMiniHighlight()：被悬停的格子要能明显区分出来")
	}
	if !strings.Contains(js, "hoverIndex") {
		t.Error("高亮状态应当记在行对象上（卡片每秒重画一次，只加 DOM 类会被整体重写掉）")
	}
	if !regexp.MustCompile(`\.mini-cell\.hover\s*\{[^}]*outline`).MatchString(css) {
		t.Error("style.css 里 .mini-cell.hover 应当用 outline 描边：border 会改变宽度，整条迷你条都会动")
	}
	if !regexp.MustCompile(`(?s)\.mini-cell\.hover\s*\{[^}]*transform:`).MatchString(css) {
		t.Error("被悬停的那一格应当有 transform（轻微放大/上移），只改颜色分不出来")
	}

	// 浮层样式：不吃鼠标事件、有自己的 hidden 兜底、且不能溢出视口。
	if !regexp.MustCompile(`(?s)\.mini-tip\s*\{[^}]*position:\s*fixed`).MatchString(css) {
		t.Error("style.css 里 .mini-tip 应当是 position: fixed（坐标即视口坐标，夹取才算得准）")
	}
	if !regexp.MustCompile(`(?s)\.mini-tip\s*\{[^}]*pointer-events:\s*none`).MatchString(css) {
		t.Error("浮层必须 pointer-events: none：否则它会挡住鼠标，移上去就闪")
	}
	if !regexp.MustCompile(`\.mini-tip\[hidden\]\s*\{[^}]*display:\s*none`).MatchString(css) {
		t.Error("style.css 缺少 .mini-tip[hidden] { display: none }：收起的浮层会留在屏幕左上角")
	}
	// 夹取用的是可见区宽高（documentElement.clientWidth/Height）。
	for _, needle := range []string{"document.documentElement.clientWidth", "document.documentElement.clientHeight"} {
		if !strings.Contains(js, needle) {
			t.Errorf("placeMiniTip() 应当用 %s 夹取浮层位置（否则会溢出视口）", needle)
		}
	}
	// 先显示再量尺寸：hidden 的元素 offsetWidth 恒为 0。
	if !regexp.MustCompile(`tip\.hidden = false;`).MatchString(js) {
		t.Error("showMiniTip() 应当先让浮层可见再量尺寸（hidden 时 offsetWidth 是 0）")
	}
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

	// 左栏：服务器列表排在「延迟探测」之后、「安全」之前。
	nav := strings.Index(html, `class="nav-item" data-pane="nodes"`)
	ping := strings.Index(html, `class="nav-item" data-pane="ping"`)
	sec := strings.Index(html, `class="nav-item" data-pane="security"`)
	if nav < 0 || ping < 0 || sec < 0 {
		t.Fatal(`index.html 的左栏里缺少 data-pane="nodes" / "ping" / "security" 中的一个`)
	}
	if !(ping < nav && nav < sec) {
		t.Error("「服务器列表」应当排在「延迟探测」之后、「安全」之前")
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
	//
	// 金额这里钉的是"走公共的双币口径函数"（nodeMoney），不是某一次调用的字面量：
	// 首页卡片、详情页、服务器列表三处必须同源，否则同一台机器在三个地方会长得不一样。
	// 只钉 node.remaining_value_cents 这种单个字段名是钉不住的 —— 它正是这一轮
	// 换成"原币种 + 人民币口径"时被改掉的那一处。
	for _, needle := range []string{
		"node.local_ip", "node.observed_ip", "'分组：'", "node.price_cents > 0",
		"nodeMoney(node, 'remaining_value_cents', 'remaining_value_cny_cents')", "' 天后到期'",
	} {
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
// 它现在只管**这张卡**里的五张资源图：延迟卡自带一组档位（见
// TestFrontendLatencyCardOwnRangeButtons），两张卡的档位互相独立。
//
// 这类"两组控件"的改动最容易出的问题是**联动的线没剪断**：切资源档位时顺手
// 又调了一次 loadPingChart()，页面上照样正常（只是延迟图白重拉一次），
// 但"资源看 1 天、延迟看 1 小时"就永远做不到 —— 而且没有任何报错。
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

	// 语义**变了**：这一组档位只管这五张资源图，不再同时控制延迟图。
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
	if strings.Contains(pick, "loadPingChart(") {
		t.Error("切资源档位不该重新请求 /ping：延迟图有自己的档位（detail.pingRange）")
	}
	// 两组按钮由同一个渲染函数造（样式与高亮规则不许写两份），但状态与动作各传各的。
	for _, needle := range []string{
		"renderRangeGroup(el.detailRanges, detail.range, setResourceRange)",
		"renderRangeGroup(el.latRanges, detail.pingRange, setPingRange)",
	} {
		if !strings.Contains(js, needle) {
			t.Errorf("app.js 缺少 %q（两组档位的状态会串在一起）", needle)
		}
	}

	// 兜底逻辑（placeRangeButtons）必须**彻底删掉**：延迟卡自带档位之后，
	// 它只会把资源那一组按钮挪进延迟卡的标题行 —— 两组长得一样、控制的图却不同。
	// 按代码断言（注释里还留着"为什么删掉"的说明，那是给下一个改它的人看的）。
	if strings.Contains(codeLines(js), "placeRangeButtons") {
		t.Error("app.js 里还留着 placeRangeButtons()：延迟卡自带档位之后它已经没有要解决的问题，留着就是两套逻辑互相覆盖")
	}
	// 两张卡片都要有同构的标题行：档位各有各的落脚点。
	for _, id := range []string{"charts-resources", "charts-latency"} {
		if !strings.Contains(sectionBody(t, html, id), `class="chart-head"`) {
			t.Errorf("%s 卡片里缺少 .chart-head 标题行", id)
		}
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

// 「延迟」卡标题行最右有**自己**的时间档位（改动 5），与「资源与网络」互相独立。
//
// 独立 = 两件事，缺一条这个功能就不成立：
//  1. 状态是两份（detail.range / detail.pingRange），高亮各算各的 ——
//     否则点一边另一边跟着变，用户会以为两组按钮本来就是同一组；
//  2. 切延迟档位**只**请求 /ping，切资源档位完全不动延迟图 ——
//     "资源图看 1 天、延迟图看 1 小时同时成立"就是这么来的。
//
// 这里钉得细，是因为接错线的表现是"页面照常能看"：两组按钮都在、都能点，
// 只是其中一组点了会把另一张图也重拉一遍，除了看请求日志看不出来。
func TestFrontendLatencyCardOwnRangeButtons(t *testing.T) {
	html := readAsset(t, "index.html")
	js := readAsset(t, "app.js")
	css := readAsset(t, "style.css")

	lat := sectionBody(t, html, "charts-latency")
	if lat == "" {
		t.Fatal("index.html 里找不到「延迟」卡片")
	}
	if !strings.Contains(lat, `id="lat-ranges"`) {
		t.Error("「延迟」卡里缺少自己的档位容器 #lat-ranges")
	}
	// 与资源卡同构：标题在左、spacer、档位在右，且就在图块上方。
	head := regexp.MustCompile(`(?s)<div class="chart-head">.*?</div>\s*<div class="chart-block"`).FindString(lat)
	if head == "" {
		t.Fatal("「延迟」卡里找不到 .chart-head 标题行（或它后面不再紧跟图块）")
	}
	if !strings.Contains(head, "<h2>延迟</h2>") || !strings.Contains(head, `id="lat-ranges"`) {
		t.Error(".chart-head 里应当同时有标题与延迟卡自己的时间档位")
	}
	if strings.Index(head, "<h2>") > strings.Index(head, `id="lat-ranges"`) {
		t.Error("延迟档位应当排在标题**右边**（标题在左、控件在右）")
	}
	if !strings.Contains(head, `class="spacer"`) {
		t.Error("标题与档位之间要有 .spacer：没有它两者会挤在一起，档位也不在最右边")
	}
	// 资源卡那一组不能被这次改动顶掉（两个容器、两组按钮，各在各的卡里）。
	if !strings.Contains(sectionBody(t, html, "charts-resources"), `id="detail-ranges"`) {
		t.Error("「资源与网络」卡的档位容器 #detail-ranges 不见了")
	}
	// 两个独立状态，默认值相同（第一次打开时两张卡看起来一致，不会让人以为哪张坏了）。
	if !regexp.MustCompile(`range: '1h',\s*\n\s*pingRange: '1h',`).MatchString(js) {
		t.Error("detail 里应当有 range 与 pingRange 两份档位状态，且默认值相同")
	}

	// 切延迟档位：只请求 /ping。
	pick := funcBody(js, "function setPingRange(")
	if pick == "" {
		t.Fatal("app.js 缺少 setPingRange()：延迟档位的切换动作没地方写")
	}
	if !strings.Contains(pick, "detail.pingRange = key") {
		t.Error("setPingRange() 应当写 detail.pingRange（写成 detail.range 就等于两组又共用一个状态了）")
	}
	if !strings.Contains(pick, "loadPingChart()") {
		t.Error("切延迟档位要重新请求 /ping")
	}
	for _, gone := range []string{"loadSeries(", "loadTrafficChart("} {
		if strings.Contains(pick, gone) {
			t.Errorf("切延迟档位不该调用 %s：资源序列跟的是 detail.range", gone)
		}
	}
	// /ping 请求用的必须是延迟档位；延迟图的刻度与时间格式也必须是它
	// （拿错档位会按另一档的格式画刻度：1h 的曲线每隔几分钟标一个 "09-29"）。
	ping := funcBody(js, "function loadPingChart(")
	if ping == "" {
		t.Fatal("app.js 缺少 loadPingChart()")
	}
	if !regexp.MustCompile(`/ping\?range=' \+ encodeURIComponent\(detail\.pingRange\)`).MatchString(ping) {
		t.Error("loadPingChart() 应当用 detail.pingRange 请求 /ping")
	}
	opts := funcBody(js, "function latChartOptions(")
	if !strings.Contains(opts, "rangeMeta(detail.pingRange)") ||
		!strings.Contains(opts, "RANGE_X_FORMAT[detail.pingRange]") {
		t.Error("延迟图的刻度与时间格式都要取 detail.pingRange")
	}
	// 反过来：资源那条链路仍然只认 detail.range（改延迟档位不许影响它）。
	series := funcBody(js, "function loadSeries(")
	if series == "" {
		t.Fatal("app.js 缺少 loadSeries()")
	}
	if !regexp.MustCompile(`series\?range=' \+ encodeURIComponent\(detail\.range\)`).MatchString(series) {
		t.Error("loadSeries() 应当用 detail.range 请求 /series")
	}

	// 窄屏要能用：标题行与档位行都允许折行（六个档位在窄屏上放不下）。
	if !regexp.MustCompile(`(?s)\.chart-head\s*\{[^}]*flex-wrap:\s*wrap`).MatchString(css) ||
		!regexp.MustCompile(`(?s)\.ranges\s*\{[^}]*flex-wrap:\s*wrap`).MatchString(css) {
		t.Error("窄屏下标题行与档位行都要能折行（.chart-head / .ranges 的 flex-wrap: wrap）")
	}
}

// 延迟卡里的三块（目标卡片 / 开关行 / 图）要有读得出来的垂直间距（改动 4），
// 但又不能大到 1080p 的窗口要滚动才能把整张卡看全。
//
// 上下界都钉，是因为这个数只有"看着合适"这一个判据：太小三块糊成一坨，
// 太大就要滚 —— 而两种毛病在静态代码里都看不出来，只有截图能看出来。
func TestFrontendLatencyCardSpacing(t *testing.T) {
	css := readAsset(t, "style.css")

	cases := []struct{ sel, name string }{
		{".lat-cards", "目标卡片组"},
		{".lat-targets", "整块控制区（开关行 + 图之间）"},
	}
	for _, c := range cases {
		rule := cssRule(css, c.sel)
		if rule == "" {
			t.Fatalf("style.css 里找不到 %s 规则", c.sel)
		}
		m := regexp.MustCompile(`margin:\s*0\s+0\s+(\d+)px`).FindStringSubmatch(rule)
		if m == nil {
			t.Fatalf("%s 应当用 margin: 0 0 Npx 给出下边距（三块的间距全靠它）", c.sel)
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			t.Fatalf("%s 的下边距解析不出数字：%q", c.sel, m[1])
		}
		if n < 10 {
			t.Errorf("%s 的下边距只有 %dpx：卡片/开关/图三块会糊在一起，读不出分组", c.name, n)
		}
		if n > 24 {
			t.Errorf("%s 的下边距 %dpx 太大：延迟卡在 1080p 上要滚动才能看全", c.name, n)
		}
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

// 延迟图目标卡片上的那一行统计要显示**整段**的平均延迟与峰值，值由后端给
// （avg_ms / peak_ms），前端不做算术。
//
// 丢包为 0 时省略丢包后缀（探针绝大多数时间不丢包，全标一句"丢包 0%"会把
// 真正丢包的那个目标淹掉）；没有成功样本（avg_ms / peak_ms = 0，整段全丢）时
// 写 — 而不是 0 ms —— 0 ms 会被读成"快得没有延迟"，与"一个样本都没有"正好相反。
func TestFrontendLatencyLegendShowsAverage(t *testing.T) {
	js := readAsset(t, "app.js")

	body := funcBody(js, "function latTargetText(")
	if body == "" {
		t.Fatal("app.js 缺少 latTargetText()")
	}
	if !strings.Contains(body, "t.avg_ms") {
		t.Error("统计行应当显示后端给的 avg_ms（前端自己平均分桶点会把加权规则再实现一遍）")
	}
	if !regexp.MustCompile(`t\.avg_ms > 0`).MatchString(body) {
		t.Error("没有有效延迟样本（avg_ms = 0）时应当写 —，而不是 0 ms")
	}
	// 峰值同样由后端给（peak_ms）：它必须与悬浮读数里那一行是同一个数，前端自己
	// 遍历一遍就是把统计再做一次，而且"卡片上写的峰值"与"悬浮里那一行"迟早对不上。
	// （峰值线本身已经不画了：延迟图的 showMax 写死 false。）
	if !strings.Contains(body, "t.peak_ms") || !regexp.MustCompile(`' · 峰值 '`).MatchString(body) {
		t.Error("统计行应当显示后端给的 peak_ms（写成「· 峰值 X ms」）")
	}
	if !strings.Contains(body, "' · 丢包 '") || !regexp.MustCompile(`if \(t\.loss_pct > 0\)`).MatchString(body) {
		t.Error("丢包为 0 时应当省略丢包后缀")
	}
}

// 延迟图的目标控制区：每个目标一张**卡片**（不再是 <input type=checkbox>）。
// 卡片 = 左侧一条竖色条（该目标自己的线色）+ 名称 + 右上角 ⓘ + 一行统计，
// 整张是一个 <button>（可点、可 Tab、回车/空格都能切换），被隐藏时整张明显变灰。
//
// 为什么钉得这么细：这块东西"坏掉"的方式全都是静默的 —— 卡片照渲染、曲线照画，
// 只是色条与线不同色（认不出谁是谁）、统计行少一个数字、点击不生效，
// 或者隐藏之后只是"勾没了"而看不出被关掉。除了盯着屏幕看没有别的线索。
func TestFrontendLatencyTargetCards(t *testing.T) {
	js := readAsset(t, "app.js")
	css := readAsset(t, "style.css")

	toggles := funcBody(js, "function renderLatToggles(")
	if toggles == "" {
		t.Fatal("app.js 缺少 renderLatToggles()")
	}
	card := funcBody(js, "function latCard(")
	if card == "" {
		t.Fatal("app.js 缺少 latCard()：目标卡片没造出来")
	}

	// 1) 不再是勾选框：控制区里一个 input 都不该有 —— 复选框的"勾没了"也表达不了
	//    "这条曲线被关掉了"（与"这个目标没数据"分不出来）。
	for _, gone := range []string{"createElement('input')", "input.type = 'checkbox'", "input.checked"} {
		if strings.Contains(toggles, gone) || strings.Contains(card, gone) {
			t.Errorf("延迟图的控制区里还留着勾选框（%s）：目标是卡片", gone)
		}
	}
	for _, gone := range []string{".lat-targets label.check", ".lat-targets .swatch"} {
		if strings.Contains(css, gone) {
			t.Errorf("style.css 里还留着 %s（勾选框那一套已经删掉）", gone)
		}
	}

	// 2) 竖色条：颜色必须来自 pingColor(index)，也就是与图上那条线**同一个**取色
	//    函数（写死一个颜色、或者交给 CSS 类，色条与曲线就会不同色）。
	if !strings.Contains(card, "'lat-bar'") ||
		!regexp.MustCompile(`bar\.style\.background = pingColor\(index\)`).MatchString(card) {
		t.Error("卡片左侧的竖色条应当用 pingColor(index) 上色（与那条曲线同色）")
	}
	if !regexp.MustCompile(`(?s)\.lat-bar\s*\{[^}]*width:`).MatchString(css) {
		t.Error("style.css 里 .lat-bar 要有宽度：竖色条看不见就等于没有")
	}

	// 3) ⓘ：说明挂在 title 上（这是两个 ⓘ 的要求：不做浮层组件）。
	if !strings.Contains(card, "'lat-card-info'") || !strings.Contains(card, "info.title = LAT_CARD_HINT") {
		t.Error("卡片右上角的 ⓘ 应当把 LAT_CARD_HINT 写进 title")
	}
	// 说明必须真的解释那一行数字（少解释一个，用户就只能猜）。
	for _, word := range []string{"平均延迟", "峰值", "丢包率"} {
		if !strings.Contains(js, word) {
			t.Errorf("卡片 ⓘ 的说明里缺少 %q（那一行几个数字各自是什么）", word)
		}
	}

	// 4) 可点 + 可键盘操作：整张卡片是 <button>（Tab 到、回车/空格触发都是浏览器
	//    自带的，不需要自己接 keydown 去模拟）。
	if !strings.Contains(card, "document.createElement('button')") ||
		!strings.Contains(card, "btn.type = 'button'") {
		t.Error("目标卡片应当是 <button>（可 Tab、可回车/空格切换），而不是 <div> + 手写 keydown")
	}
	if !strings.Contains(card, "toggleLatTarget(t.id, willHide)") {
		t.Error("点卡片应当切换这个目标的显示/隐藏（toggleLatTarget）")
	}

	// 5) 隐藏时**整张卡片明显变灰**，而不是只把勾去掉。
	if !strings.Contains(card, "setLatCardOff(btn, !!hidden)") {
		t.Error("渲染卡片时要按存下来的隐藏状态把它画成灰的")
	}
	if !regexp.MustCompile(`(?s)\.lat-card\.off\s*\{[^}]*opacity:`).MatchString(css) {
		t.Error("style.css 里 .lat-card.off 应当明显变灰（opacity）")
	}

	// 6) 统计行由 latTargetText（平均 · 峰值 · 丢包）拼出来，整段一个 textContent。
	//    这里曾经还有一截单独成元素的「· 慢 X%」（红色的），随"慢"一起删掉了。
	if !strings.Contains(card, "latTargetText(t)") {
		t.Error("卡片的统计行应当由 latTargetText 拼出来")
	}
	if strings.Contains(card, "stats.appendChild") {
		t.Error("统计行不该再挂额外的子元素：慢那一截已经删掉，统计行只剩 平均 · 峰值 · 丢包")
	}

	// 7) 布局：卡片是 auto-fit 网格（宽屏并排并填满整行、窄屏自动折行），
	//    手机上（≤640px）再退回单列 —— minmax(200px, 1fr) 在 320px 宽的窗口里
	//    会把卡片顶出容器、出现横向滚动条。
	if !regexp.MustCompile(`(?s)\.lat-cards\s*\{[^}]*grid-template-columns:\s*repeat\(auto-fit`).MatchString(css) {
		t.Error("style.css 里 .lat-cards 应当是 auto-fit 网格（auto-fill 会留下空轨道，目标少时右边空一大片）")
	}
	if !regexp.MustCompile(`(?s)@media \(max-width: 640px\).*?\.lat-cards\s*\{[^}]*grid-template-columns:\s*1fr`).MatchString(css) {
		t.Error("窄屏 media query 里应当把 .lat-cards 改成单列（不然卡片会横向溢出）")
	}
}

// 卡片下面那一行全局开关：**三个**（延迟 / 丢包 / 平滑曲线）、chip 样式、
// 选中态高亮、状态存 localStorage（键名带版本前缀）。
//
// 这里以前钉的是四个开关，第四个是「峰值线」。用户要求去掉它 —— 峰值线永远不画、
// Y 轴也不再为峰值留空间（"图表上面的留白因为峰值延迟的缘故变得太多了"），
// 一个没有开关的功能不该留一个 chip。峰值本身没丢：它由悬浮读数里的那一行保留
// （见 TestFrontendLatencyKeepsPeakForHoverOnly）。
//
// 版本前缀是**必须**的：以后改这三个开关的结构（加一个、把布尔改成三态）而
// 不换键名的话，老浏览器里存着的旧结构会被读成一个字段对不上的对象 ——
// 表现是"开关点了没反应"，而控制台一声不吭。
// 但**这一轮不换**版本号：老结构里多出来的 peak 键必须被安全忽略（下面第 3 条），
// 换版本会把用户另外三个开关的选择一起清掉。
func TestFrontendLatencyViewChips(t *testing.T) {
	js := readAsset(t, "app.js")
	css := readAsset(t, "style.css")

	// sliceTo 从 marker 截到 end（含）：数组/对象字面量没有"两空格缩进的右花括号"
	// 可截，funcBody 那种粗截法会一路吃进后面一大段代码，顺序断言就失去意义了。
	sliceTo := func(marker, end string) string {
		at := strings.Index(js, marker)
		if at < 0 {
			return ""
		}
		rest := js[at:]
		if stop := strings.Index(rest, end); stop >= 0 {
			return rest[:stop+len(end)]
		}
		return rest
	}

	for _, needle := range []string{
		"var LAT_VIEW_ITEMS = [",
		"['mean', '延迟']",
		"['loss', '丢包']",
		"['smooth', '平滑曲线']",
		"var LAT_VIEW_DEFAULT = ",
		"var PING_VIEW_KEY = 'probe-ping-view-v1';",
		"function latView(",
		"function setLatView(",
		"function toggleLatView(",
		"function latChips(",
	} {
		if !strings.Contains(js, needle) {
			t.Errorf("app.js 缺少 %q（三个开关会少一个或状态存不下来）", needle)
		}
	}

	// 1) 「峰值线」这个 chip 必须**彻底消失**：文案、存储键、状态项，一个都不许留。
	//    留一半（比如 LAT_VIEW_ITEMS 里删了、LAT_VIEW_DEFAULT 里还留着 peak）就是
	//    一份谁也不读的死状态；留全了则是"按钮没了但功能还在"，用户没法关掉它。
	if strings.Contains(js, "峰值线']") || regexp.MustCompile(`\['peak'`).MatchString(js) {
		t.Error("app.js 里还留着「峰值线」这个 chip（LAT_VIEW_ITEMS 的 'peak' 项）：" +
			"用户要求把按钮去掉，峰值线现在永远不画（见 latChartOptions 的 showMax: false）")
	}
	if !regexp.MustCompile(`(?s)LAT_VIEW_DEFAULT = \{[^}]*\};`).MatchString(js) ||
		regexp.MustCompile(`LAT_VIEW_DEFAULT = \{[^}]*\bpeak\b[^}]*\};`).MatchString(js) {
		t.Error("LAT_VIEW_DEFAULT 里不该再有 peak 项：按钮没了，就不再需要这个状态")
	}
	if regexp.MustCompile(`view\.peak`).MatchString(js) {
		t.Error("app.js 里还在读 view.peak：峰值线已经不是一个开关了（showMax 写死 false）")
	}

	// 2) 文案与顺序：数组顺序就是页面顺序，三个都得在。
	want := []string{"'延迟'", "'丢包'", "'平滑曲线'"}
	items := sliceTo("var LAT_VIEW_ITEMS = [", "];")
	if items == "" {
		t.Fatal("app.js 里找不到 LAT_VIEW_ITEMS 的定义")
	}
	at := -1
	for _, label := range want {
		i := strings.Index(items, label)
		if i < 0 {
			t.Errorf("三个开关里缺少 %s", label)
			continue
		}
		if i < at {
			t.Errorf("开关 %s 的顺序不对（应当依次是 延迟 / 丢包 / 平滑曲线）", label)
		}
		at = i
	}
	if n := strings.Count(items, "['"); n != 3 {
		t.Errorf("LAT_VIEW_ITEMS 里有 %d 个开关，期望 3 个（延迟 / 丢包 / 平滑曲线）：%s", n, items)
	}

	// 3) 默认值只剩三项；老浏览器里存着 {..., peak: true} 时**必须安全忽略**。
	//
	//    读法就是保证：latView() 只遍历 LAT_VIEW_DEFAULT 的键去取值，存着的那一坨
	//    里多出来的 peak 根本不会被读到（既不会进 view，也不会传进图表选项）。
	//    这一条不成立时的表现是"老用户一进详情页就白屏"或者开关变哑巴 ——
	//    只有拿一份旧结构试才知道，所以这里用静态断言把读法钉死。
	def := sliceTo("var LAT_VIEW_DEFAULT = ", "};")
	if def == "" {
		t.Fatal("app.js 里找不到 LAT_VIEW_DEFAULT 的定义")
	}
	for _, on := range []string{"mean: true", "loss: true", "smooth: false"} {
		if !strings.Contains(def, on) {
			t.Errorf("LAT_VIEW_DEFAULT 里缺少 %q：另外三个开关的默认值不该跟着这次改动变", on)
		}
	}
	view := funcBody(js, "function latView(")
	if view == "" {
		t.Fatal("app.js 缺少 latView()")
	}
	if !strings.Contains(view, "Object.keys(LAT_VIEW_DEFAULT).forEach(") {
		t.Error("latView() 必须**只**遍历 LAT_VIEW_DEFAULT 的键：存着的旧对象里多出来的 peak 键" +
			"（删按钮之前存下的）要能被安全忽略，否则老浏览器会读到一个没人认识的状态")
	}
	if strings.Contains(view, "parsed.peak") {
		t.Error("latView() 里还在直接读 parsed.peak：那个键已经不存在了")
	}
	if !strings.Contains(view, "typeof parsed[key] === 'boolean'") ||
		!strings.Contains(view, "view[key] = parsed[key]") {
		t.Error("latView() 必须优先采用 localStorage 里用户选过的值：改默认值不许覆盖用户已经做过的选择")
	}
	if !strings.Contains(js, "peak") {
		t.Error("注释里要写明「老浏览器存着的 peak 键怎么办」：下一个清理峰值相关代码的人得知道" +
			"为什么读到不认识的键不会出错（多出来的键被忽略，写回时顺手消失）")
	}

	chips := funcBody(js, "function latChips(")
	if chips == "" {
		t.Fatal("app.js 缺少 latChips()：开关行没造出来")
	}
	// 与设置页的导航项同一套交互：<button> + active 类 + aria-pressed。
	if !strings.Contains(chips, "document.createElement('button')") {
		t.Error("开关应当是 <button>（可 Tab、可回车/空格切换）")
	}
	if !strings.Contains(chips, "classList.toggle('active', !!view[key])") ||
		!strings.Contains(chips, "setAttribute('aria-pressed'") {
		t.Error("开关要同时给出选中态（active 类）与无障碍状态（aria-pressed）")
	}
	if !strings.Contains(chips, "'lat-chips-info'") || !strings.Contains(chips, "info.title = LAT_CHIPS_HINT") {
		t.Error("三个开关后面那个 ⓘ 应当把 LAT_CHIPS_HINT 写进 title")
	}
	// 说明要跟着这次改动更新：不能再写"峰值线"是一个开关，但必须写明峰值去哪儿了
	// （悬浮读数里还在）—— 用户点进 ⓘ 就是想知道"峰值还能不能看"。
	if !strings.Contains(js, "var LAT_CHIPS_HINT") {
		t.Fatal("app.js 缺少 LAT_CHIPS_HINT")
	}
	hint := sliceTo("var LAT_CHIPS_HINT = ", "';")
	if hint == "" {
		t.Fatal("LAT_CHIPS_HINT 的定义截取不到")
	}
	if regexp.MustCompile(`峰值线：`).MatchString(hint) {
		t.Error("开关说明里还在讲「峰值线」这个开关：它已经不存在了（用户要的就是别再有这个按钮）")
	}
	for _, word := range []string{"峰值", "悬浮"} {
		if !strings.Contains(hint, word) {
			t.Errorf("开关说明里缺少 %q：必须写明峰值仍然能在悬浮读数里看到（否则用户会以为峰值这块功能被删了）", word)
		}
	}
	// 切换后只同步高亮 + 重画曲线（重建整行会把键盘焦点丢掉）。
	toggle := funcBody(js, "function toggleLatView(")
	if toggle == "" {
		t.Fatal("app.js 缺少 toggleLatView()")
	}
	if !strings.Contains(toggle, "setLatView(view)") || !strings.Contains(toggle, "syncLatChips(view)") ||
		!strings.Contains(toggle, "applyLatSeries()") {
		t.Error("切开关要：存下来 + 同步高亮 + 重画曲线（缺一个就是「点了没反应」）")
	}

	// 样式：圆角 chip + 选中态高亮（只改文字颜色的"高亮"在一排里看不出来）。
	if !regexp.MustCompile(`(?s)\.lat-chips\s*\{[^}]*flex-wrap:\s*wrap`).MatchString(css) {
		t.Error("style.css 里 .lat-chips 应当 flex-wrap: wrap（窄屏要能折行）")
	}
	// 改动 3：这一行**水平居中**（原来左对齐 —— 左边缘与上面的目标卡片对齐，
	// 看上去像"还有一张卡片"）。
	if !regexp.MustCompile(`(?s)\.lat-chips\s*\{[^}]*justify-content:\s*center`).MatchString(css) {
		t.Error("style.css 里 .lat-chips 应当 justify-content: center（三个开关要居中，不是左对齐）")
	}
	// 行尾那个 ⓘ 会把居中的按钮整体推左半个它的宽度，所以左边要配一个等宽的隐形占位，
	// 否则"居中"的按钮看起来仍然偏左（几何中心差约 9px）。
	if !regexp.MustCompile(`\.lat-chips::before\s*\{[^}]*visibility:\s*hidden`).MatchString(css) {
		t.Error("style.css 缺少 .lat-chips::before 的对称配重：按钮的几何中心会偏左")
	}
	if !regexp.MustCompile(`(?s)\.chip\.active\s*\{[^}]*border-color:\s*var\(--accent\)`).MatchString(css) {
		t.Error("style.css 里 .chip.active 应当用强调色描边（选中态要高亮）")
	}
}

// 平滑必须用**单调三次插值**（Fritsch–Carlson），不能用普通的 Catmull-Rom /
// 自然三次样条：后者会**过冲** —— 数据在 0 附近时曲线会画到负数去，尖峰两侧
// 会鼓出比真实峰值还高的包。延迟有物理下限（> 0），凭空造一个不存在的读数
// 比"曲线不好看"严重得多，而且看图的人看不出这是插值算法的错。
//
// 这里既钉"用的是哪一种"，也钉"没有另一种"：只钉前者的话，下一次改动完全可能
// 在旁边再补一条普通样条的分支（比如给别的图用），而那个分支照样会过冲。
func TestFrontendSmoothingIsMonotone(t *testing.T) {
	chart := readAsset(t, "chart.js")

	// 1) 单调插值本体：切线函数 + Fritsch–Carlson 的那道夹子（|m| ≤ 3|Δ|）。
	if !strings.Contains(chart, "Fritsch") {
		t.Error("chart.js 里要写明用的是 Fritsch–Carlson 单调三次插值（下一个改它的人得知道为什么不能换）")
	}
	if !strings.Contains(chart, "过冲") {
		t.Error("注释里要写清「为什么不能用普通样条」：会过冲，会画出负数/比真实峰值还高的鼓包")
	}
	if !regexp.MustCompile(`function monotoneTangents\(`).MatchString(chart) {
		t.Fatal("chart.js 缺少 monotoneTangents()：平滑没有用单调插值")
	}
	tan := chartFuncBody(chart, "function monotoneTangents(")
	if tan == "" {
		t.Fatal("monotoneTangents() 的函数体没截取到")
	}
	// 极值点切线压平（不压平的话峰/谷处必然鼓出去）。
	if !regexp.MustCompile(`m\[j\] = delta\[j - 1\] \* delta\[j\] <= 0 \? 0 :`).MatchString(tan) {
		t.Error("极值点（左右斜率反号）的切线必须是 0：不压平的话峰谷两侧一定会鼓包")
	}
	// 半径 3 的圆夹子 —— 这一句就是"不会过冲"的全部依据。
	if !regexp.MustCompile(`if \(s > 9\)`).MatchString(tan) ||
		!regexp.MustCompile(`3 / Math\.sqrt\(s\)`).MatchString(tan) {
		t.Error("缺少 Fritsch–Carlson 的切线夹子（|m| ≤ 3|Δ|）：切线一旦超出这个圆就会过冲")
	}

	// 2) 逐段平滑：每个连续段各自算切线，绝不跨过缺口（跨过去就等于把缺口填上了）。
	if !strings.Contains(chart, "function drawRun(") {
		t.Fatal("chart.js 缺少 drawRun()：平滑没有逐段处理")
	}
	if !strings.Contains(chart, "ctx.bezierCurveTo(") {
		t.Error("平滑曲线应当用三次贝塞尔（bezierCurveTo）画")
	}
	// 3) 没有第二套普通样条的实现：二次曲线/基数样条（Catmull-Rom 是基数样条的一种）
	//    在本文件里一个都不许有 —— 它们都会过冲。
	for _, gone := range []string{"quadraticCurveTo", "cardinal", "CatmullRom", "catmullRom", "spline("} {
		if strings.Contains(chart, gone) {
			t.Errorf("chart.js 里出现了 %q：普通样条会过冲（画出负延迟/比峰值还高的鼓包）", gone)
		}
	}
}

// 断线：缺口两端不许再用一条直线"桥"过去。
//
// 两条判据缺一不可：
//  1. 值是 null（这一桶整段丢包，一个成功的探测都没有）；
//  2. 相邻两点的 ts 间隔 > 1.5 × 桶宽 —— 这一条更重要：Agent 离线时服务端
//     **根本不会往 ping_samples_1m 里写行**，那些桶连点都不存在，
//     只按 null 判断是查不出来的，而它恰恰是最该断开的一种。
//
// 桶宽取 /ping 响应的 meta.bucket_sec（不是 /nodes/{id} 里 ranges 那个：
// 同一个档位下两者桶宽不同，拿错了会把一条正常的曲线切碎）。
func TestFrontendLatencyGapsBreakTheLine(t *testing.T) {
	js := readAsset(t, "app.js")
	chart := readAsset(t, "chart.js")

	// 1) 判据本体：值不是数字（null）→ 断；间隔超过 1.5 个桶宽 → 断。
	if !regexp.MustCompile(`function linkedWithPrev\(`).MatchString(chart) {
		t.Fatal("chart.js 缺少 linkedWithPrev()：断线没有统一判据，两处（曲线/平滑）会各断各的")
	}
	link := chartFuncBody(chart, "function linkedWithPrev(")
	if link == "" {
		t.Fatal("linkedWithPrev() 的函数体没截取到")
	}
	if !strings.Contains(link, "typeof v !== 'number'") || !strings.Contains(link, "typeof prev !== 'number'") {
		t.Error("断线的第一条判据是「值是 null」（这一桶一个成功的探测都没有）")
	}
	if !regexp.MustCompile(`bucketSec \* GAP_BUCKET_RATIO`).MatchString(link) {
		t.Error("断线的第二条判据是「相邻两点的 ts 间隔超过桶宽的一定倍数」：Agent 离线时那些桶连点都不存在，只按 null 判断查不出来")
	}
	if !regexp.MustCompile(`var GAP_BUCKET_RATIO = 1\.5;`).MatchString(chart) {
		t.Error("桶宽的倍数应当是 1.5（留半个桶的余量容忍对齐偏差；掐着 1 倍会把正常曲线切碎）")
	}
	if !regexp.MustCompile(`runsOf\(pts, valueIndex, opts\.bucketSec\)`).MatchString(chart) {
		t.Error("drawLine 应当按 runsOf(..., opts.bucketSec) 分段：桶宽必须传到判据里")
	}

	// 2) 旧的"跳过这个点但路径不断"的写法必须彻底消失 —— 那正是把缺口两端
	//    用一条直线连过去的原因（看图的人会以为那段时间延迟正常）。
	line := chartFuncBody(chart, "function drawLine(")
	if line == "" {
		t.Fatal("drawLine() 的函数体没截取到")
	}
	if strings.Contains(line, "continue;") {
		t.Error("drawLine 里还在用 continue 跳过缺失值：缺口会被一条直线桥接过去")
	}
	if !strings.Contains(line, "drawRun(") {
		t.Error("drawLine 应当逐段调用 drawRun（每段自成一条子路径）")
	}

	// 3) 判据只有一处实现：曲线（drawLine → runsOf）与悬浮读数都用同一套分段/距离
	//    尺度。这里曾经还检查过 drawSlow 的断开（红线不跨缺口）—— 那段绘制已经随
	//    "慢"一起删掉了，但"尺度必须一致"这条要求还在：悬浮读数找最近点用的
	//    HOVER_SLACK_RATIO 必须与断线的 GAP_BUCKET_RATIO 是同一个数，
	//    否则会出现"线是断的、浮层却从缺口另一头拿了读数"。
	hover := chartFuncBody(chart, "function drawHover(")
	if hover == "" {
		t.Fatal("drawHover() 的函数体没截取到")
	}
	if !regexp.MustCompile(`var HOVER_SLACK_RATIO = 1\.5;`).MatchString(chart) ||
		!strings.Contains(hover, "HOVER_SLACK_RATIO") {
		t.Error("悬浮读数找最近点必须有 HOVER_SLACK_RATIO=1.5（与断线判据同一个尺度）")
	}

	// 4) 桶宽来自 /ping 的 meta.bucket_sec（**不是** /nodes/{id} 里 ranges 的
	//    bucket_sec：同一个档位下两者桶宽不同，拿错了会把正常曲线切成一段段）。
	if !regexp.MustCompile(`detail\.pingBucketSec = meta\.bucket_sec > 0 \? meta\.bucket_sec : 0;`).MatchString(js) {
		t.Error("loadPingChart() 应当从 /ping 响应的 meta.bucket_sec 取桶宽")
	}
	//    交给图表引擎的**不是**服务端桶宽本身，而是"相邻两点实际间距"这个尺度：
	//    桶宽 / 手机端聚合目标 / **探测间隔** / **实测点距中位数** 里最大的那个
	//    （见 latBucketSec；实测那一项单独由
	//    TestFrontendLatencyBucketMergesMeasuredSpacing 钉住）。
	//    拿一个比实际点距小的尺度去比，每一对相邻点都会被判成缺口 ——
	//    整条曲线退化成一串孤立圆点。
	if !regexp.MustCompile(`bucketSec: latBucketSec\(\)`).MatchString(js) {
		t.Error("latChartOptions() 交给图表引擎的应当是 latBucketSec()（桶宽/聚合目标/探测间隔/实测点距里最大的那个）")
	}
	scale := funcBody(js, "function latBucketSec(")
	if scale == "" {
		t.Fatal("app.js 缺少 latBucketSec()：断线判据的时间尺度没有统一出处")
	}
	for _, needle := range []string{"mobileAggSec(detail.pingRange)", "detail.pingBucketSec", "detail.pingIntervalSec"} {
		if !strings.Contains(scale, needle) {
			t.Errorf("latBucketSec() 少了 %s：它会让断线判据比实际点距还小，整条曲线被画成一串孤立点", needle)
		}
	}
	if regexp.MustCompile(`bucketSec: meta\.bucket_sec`).MatchString(js) {
		t.Error("桶宽不能取 rangeMeta() 的 bucket_sec：那是 /series 的桶宽，与 /ping 不同")
	}
	if !regexp.MustCompile(`pingBucketSec: 0`).MatchString(js) {
		t.Error("detail 里应当有 pingBucketSec 这个状态（换节点/关详情页时跟着清空）")
	}

	// 5) 探测间隔也是断线判据的一部分。
	//
	//    探测间隔是**可配的**（10~3600 秒，默认 60）。间隔 > 桶宽时（例如间隔 300 秒
	//    而 6h 档桶宽 60 秒），每 5 个桶里只有 1 个有行，相邻两点的实际间距是 300 秒 ——
	//    只按桶宽判就会把整条曲线画成一串孤立点。间隔来自 /settings 的
	//    ping.interval_sec（同一个响应里就有，不必再加接口）。
	if !regexp.MustCompile(`detail\.pingIntervalSec = ping\.interval_sec > 0 \? ping\.interval_sec : 0;`).MatchString(js) {
		t.Error("探测间隔应当从 /settings 的 ping.interval_sec 取下来（断线判据要用它）")
	}
	if !regexp.MustCompile(`pingIntervalSec: 0`).MatchString(js) {
		t.Error("detail 里应当有 pingIntervalSec 这个状态（换节点/关详情页时跟着清空）")
	}
	if !regexp.MustCompile(`detail\.pingIntervalSec = 0;`).MatchString(js) {
		t.Error("换节点/关详情页时要把 pingIntervalSec 一起清掉：留着上一个节点的探测间隔会算错断线")
	}

	// 6) 手机端二次聚合必须用**这张图自己的档位**。
	//
	//    这里曾经写死 detail.range（资源卡的档位）：延迟图跟的是 detail.pingRange，
	//    两张卡停在不同档位时，1h 的延迟曲线会被按 7d 的聚合目标（1800 秒）合并 ——
	//    一小时里只剩两个点。seriesFor 因此必须收一个档位参数，而且两个调用点
	//    各自传自己的那一份。
	if !regexp.MustCompile(`function seriesFor\(points, rangeKey\)`).MatchString(js) {
		t.Fatal("seriesFor() 必须按调用方给的档位取 meta（写死 detail.range 会让延迟图用错聚合目标）")
	}
	if !strings.Contains(funcBody(js, "function seriesFor("), "mobileAggSec(rangeKey)") {
		t.Error("seriesFor() 应当用传进来的 rangeKey 去取手机端聚合目标")
	}
	if !strings.Contains(funcBody(js, "function latencySeriesFor("), "seriesFor(points, detail.pingRange)") {
		t.Error("延迟图的取点必须按 detail.pingRange 聚合（延迟卡有自己那组档位，与资源卡无关）")
	}
	if n := strings.Count(js, "seriesFor(data.points, detail.range)") +
		strings.Count(js, "seriesFor(s.data.points, detail.range)"); n != 2 {
		t.Errorf("资源图的取点应当按 detail.range 聚合（setChart 的两条路径各一处），实际匹配到 %d 处", n)
	}
	if regexp.MustCompile(`seriesFor\((data|s\.data)\.points\)`).MatchString(js) {
		t.Error("还有调用点在用不传档位的 seriesFor()：它会退回资源档位，延迟图就错了")
	}
	if !strings.Contains(funcBody(js, "function mobileAggSec("), "matchMedia(MOBILE_QUERY)") {
		t.Error("mobileAggSec() 应当只在窄屏（MOBILE_QUERY）下给聚合目标：桌面端不聚合")
	}
}

// 断线判据的时间尺度必须再并进一个**实测值**：相邻点距的中位数。
//
// 为什么需要它（真事故的形状）：桶宽 / 聚合目标 / 探测间隔这三项全是"别人自报的
// 数"，只能覆盖**已知**的三种原因。真出现过"后端自报桶宽 60 秒、实际点距 300 秒"
// 的数据（Agent 上报节奏不齐）：三个自报项算出来是 60 秒，而 300 > 1.5 × 60 ——
// 每一对相邻点都被判成缺口，整条曲线退化成一串孤立圆点（e2e 的
// TestMobileLatencyChartStaysConnected 场景三在真浏览器里量这一条）。
// 实测中位数不看任何自报值，直接量画出来的那串点，于是能兜住"没想到的第四种"。
//
// 这里钉住四件事（画出来的行为由 e2e 量）：
//  1. 实测项确实并进了 latBucketSec()，而且仍然与其它三项**取最大值**
//     —— 保守优先：实测偏小时由自报的三项托底，实测偏大时按实测；
//  2. 量的是**手机端二次聚合之后**的点（detail.pingSeries，与 applyLatSeries
//     交给图表的是同一批点）：在聚合之前量会得到偏小的间距；
//  3. 取的是**中位数**（排序后取中间），不是平均值（平均值会被一个两小时的洞带偏）；
//  4. 差值少于 3 个时**不猜**：实测项返回 0，退回原来的三项（不崩、也不是 0）；
//     结果有缓存（悬浮重绘那种每帧路径不能每帧扫一遍整串点再排序），缓存键覆盖
//     "目标显示/隐藏、切档位、换节点、数据刷新"。
func TestFrontendLatencyBucketMergesMeasuredSpacing(t *testing.T) {
	js := readAsset(t, "app.js")

	// 1) 实测那一项并进来了，并与其它三项取最大值。
	scale := funcBody(js, "function latBucketSec(")
	if scale == "" {
		t.Fatal("app.js 缺少 latBucketSec()：断线判据的时间尺度没有统一出处")
	}
	// 用带赋值的正则而不是 strings.Contains：后者能被一句注释里的
	// "latMeasuredSpacingSec()" 蒙混过去（反向验证时就是这么发现的：把实测项换成
	// `var measured = 0; // ...latMeasuredSpacingSec()...` 之后静态断言竟然还是绿的）。
	if !regexp.MustCompile(`var measured = latMeasuredSpacingSec\(\);`).MatchString(scale) {
		t.Error("latBucketSec() 必须并进实测点距（var measured = latMeasuredSpacingSec()）：只信自报的三项会漏掉" +
			"「后端自报桶宽与实际点距不符」这一类，整条曲线被判成一串孤立圆点")
	}
	if !regexp.MustCompile(`if \(measured > scale\) scale = measured;`).MatchString(scale) {
		t.Error("实测值必须与其它三项**取最大值**（保守优先）：直接赋值会让实测偏小时把正常曲线切碎")
	}
	for _, needle := range []string{"mobileAggSec(detail.pingRange)", "detail.pingBucketSec", "detail.pingIntervalSec"} {
		if !strings.Contains(scale, needle) {
			t.Errorf("latBucketSec() 少了自报项 %s：实测再准也不能取代它们（点太少时实测根本算不出来）", needle)
		}
	}
	if !regexp.MustCompile(`return scale > 0 \? scale : 0;`).MatchString(scale) {
		t.Error("latBucketSec() 的最后一步应当是「取不到任何尺度时返回 0=未知」：不能返回一个猜出来的数")
	}

	// 2) 实测项量的必须是**聚合之后**的那串点（detail.pingSeries）。
	//    detail.pingSeries 里的 points 就是 latencySeriesFor 聚合完的（见 loadPingChart），
	//    而 applyLatSeries 把它交给图表 —— 判据量的点与画出来的点是同一批。
	shown := funcBody(js, "function latShownSeries(")
	if shown == "" {
		t.Fatal("app.js 缺少 latShownSeries()：判据与绘制必须用同一个「要画哪几条曲线」的筛选")
	}
	if !strings.Contains(shown, "detail.pingSeries") || !strings.Contains(shown, "latHiddenSet()") {
		t.Error("latShownSeries() 应当在 detail.pingSeries 上按隐藏状态筛选（隐藏的目标不算「要画的那串点」）")
	}
	if !strings.Contains(funcBody(js, "function applyLatSeries("), "latShownSeries()") {
		t.Error("applyLatSeries() 必须走 latShownSeries()：判据量的点与真正画出来的点不能是两批")
	}
	deltas := funcBody(js, "function latSpacingDeltas(")
	if deltas == "" {
		t.Fatal("app.js 缺少 latSpacingDeltas()：实测点距没有实现")
	}
	if !strings.Contains(deltas, "latShownSeries()") {
		t.Error("latSpacingDeltas() 必须量 latShownSeries() 的点（聚合之后、且要画的那些）")
	}
	if !regexp.MustCompile(`pts\[i\]\[0\] - pts\[i - 1\]\[0\]`).MatchString(deltas) {
		t.Error("实测点距应当拿相邻两点的 ts 相减（pts[i][0] - pts[i-1][0]）")
	}
	if !regexp.MustCompile(`d > 0`).MatchString(deltas) {
		t.Error("实测点距要丢掉 <= 0 的差值：重复 ts 会把中位数往小里拽，而那正是「曲线碎成点」的方向")
	}
	if !strings.Contains(deltas, "deltas.push(d)") {
		t.Error("latSpacingDeltas() 应当把所有目标的差值都收进**同一个**数组（多目标取并集再求中位数）")
	}

	// 3) 中位数（不是平均值）。
	measured := funcBody(js, "function latMeasuredSpacingSec(")
	if measured == "" {
		t.Fatal("app.js 缺少 latMeasuredSpacingSec()：实测值没有实现")
	}
	if !strings.Contains(measured, "latSpacingDeltas()") {
		t.Error("latMeasuredSpacingSec() 应当基于 latSpacingDeltas() 的差值算")
	}
	if !strings.Contains(measured, "deltas.sort(") || !strings.Contains(measured, "deltas.length >> 1") {
		t.Error("实测值必须取**中位数**（排序后取中间那个）：平均值会被一个两小时的洞带偏，" +
			"于是真正的掉线反倒小于「平均点距」被连了起来")
	}
	if strings.Contains(measured, "/ deltas.length") || strings.Contains(measured, "/deltas.length") {
		t.Error("latMeasuredSpacingSec() 里出现了除以差值个数的写法（那是平均值）")
	}

	// 4) 退回路径：差值不足时实测项返回 0（"不知道"），由上面三项托底。
	if !regexp.MustCompile(`var LAT_SCALE_MIN_DELTAS = 3;`).MatchString(js) {
		t.Error("至少要 3 个差值才算得出中位数（1 个时中位数就是那个差值本身，正好落在缺口上就全判错）")
	}
	if !strings.Contains(measured, "LAT_SCALE_MIN_DELTAS") {
		t.Error("latMeasuredSpacingSec() 必须按 LAT_SCALE_MIN_DELTAS 判断差值够不够")
	}
	if !strings.Contains(measured, "var value = 0;") || !strings.Contains(measured, "return value;") {
		t.Error("差值不足时实测项应当返回 0（=没有实测值），绝不能返回一个猜出来的数：猜小了切碎曲线、猜大了连上掉线")
	}
	// detail 里"还没数据"的形态：pingSeries 是空数组、bucket 与 interval 都是 0 时，
	// 三项自报值也都是 0 —— 那时整体返回 0（未知），只按 null 断线，不崩。
	if !regexp.MustCompile(`pingSeries: \[\]`).MatchString(js) {
		t.Error("detail 里应当有 pingSeries: [] 这个空状态（换节点/关详情页时跟着清空，实测项因此退回自报的三项）")
	}

	// 5) 缓存：每帧路径不能每帧重算整串点；键必须覆盖四个失效来源。
	memo := funcBody(js, "function latHiddenSignature(")
	if memo == "" {
		t.Fatal("app.js 缺少 latHiddenSignature()：隐藏状态的签名没有实现")
	}
	if !strings.Contains(memo, "Object.keys(latHiddenSet())") || !strings.Contains(memo, ".sort()") {
		t.Error("latHiddenSignature() 应当把隐藏的 id 排序后拼成签名（与顺序无关）")
	}
	if !strings.Contains(measured, "latScaleMemo") {
		t.Error("实测值必须有缓存（latScaleMemo）：latBucketSec() 在悬浮重绘那一帧也会走到，" +
			"每帧扫一遍全部曲线再排序是白烧 CPU")
	}
	if !regexp.MustCompile(`latScaleMemo\.series === series && latScaleMemo\.hidden === hidden && latScaleMemo\.agg === agg`).MatchString(measured) {
		t.Error("缓存键必须同时覆盖「数据/节点/档位」（pingSeries 的数组引用）、「目标显示/隐藏」（hidden 签名）" +
			"与「手机端聚合目标」（agg）：少一项就会拿着一份过期尺度去判线，而页面上一点征兆都没有")
	}
	if !strings.Contains(measured, "mobileAggSec(detail.pingRange)") {
		t.Error("缓存键里要带上手机端聚合目标：窗口跨过 640px 时点距会变，缓存必须跟着失效")
	}
	if !regexp.MustCompile(`latScaleMemo\.value = value;`).MatchString(measured) {
		t.Error("算出实测值之后要写回缓存（latScaleMemo.value）")
	}
	// 数据刷新的失效依据：detail.pingSeries 每次都是**整份新数组**（换节点时是 []，
	// /ping 回来时是 series），引用一变缓存就作废，不需要谁记得通知这里。
	if !regexp.MustCompile(`detail\.pingSeries = series;`).MatchString(js) {
		t.Error("loadPingChart() 应当把新数组赋给 detail.pingSeries：缓存靠这个引用失效（数据刷新必须作废缓存）")
	}
}

// 悬浮读数按 **ts 逐条曲线各自匹配**，不是一个下标索引所有曲线。
//
// 为什么这是真 bug：后端只返回**存在**的桶（GROUP BY bucket），某个目标中途没数据
// 时它的 points 数组就更短 —— 用第 0 条曲线的下标去读第 1 条曲线，读到的是**另一个
// 时间**的读数。界面上只写一个时间，两条线的值都"有数"，用户完全看不出来。
//
// 三条要求一起钉住：
//   - 每条曲线各自找离鼠标最近的点（nearestPoint），匹配到哪一刻就画在哪一刻；
//   - 该曲线在这个时间附近没有点（超过 1.5 个桶宽）时写 —，不拿别的点充数；
//   - 桶宽 ≥ 1 小时时，时间那行是**区间** [起点, 起点+桶宽)，跨天时两端带日期。
func TestFrontendHoverMatchesEachSeriesByTimestamp(t *testing.T) {
	chart := readAsset(t, "chart.js")

	nearest := chartFuncBody(chart, "function nearestPoint(")
	if nearest == "" {
		t.Fatal("chart.js 缺少 nearestPoint()：逐条曲线找最近点没有实现")
	}
	if !strings.Contains(nearest, "points[i][0]") {
		t.Error("nearestPoint() 必须按点自己的时间戳（points[i][0]）比距离，而不是按下标")
	}

	hover := chartFuncBody(chart, "function drawHover(")
	if hover == "" {
		t.Fatal("chart.js 的 drawHover() 函数体没截取到")
	}
	if !strings.Contains(hover, "opts.series.forEach(") || !strings.Contains(hover, "nearestPoint(pts, hoverTS)") {
		t.Error("drawHover() 必须在遍历 series 时对**每条曲线各自**调用 nearestPoint(pts, hoverTS)")
	}
	// 旧的写法就是这个：共用一个下标。它必须彻底消失。
	if regexp.MustCompile(`s\.points\[index\]`).MatchString(chart) ||
		regexp.MustCompile(`function nearestIndex\(`).MatchString(chart) {
		t.Error("chart.js 里还留着「一个下标索引所有曲线」的写法：某条曲线缺桶时读数会指向另一个时间点")
	}
	// 每条曲线的圆点画在**它自己**匹配到的时间上（x(p[0])），不是大家共用一个 px。
	if !regexp.MustCompile(`ctx\.arc\(x\(p\[0\]\), y\(p\[1\]\)`).MatchString(hover) {
		t.Error("每条曲线的圆点应当画在它自己匹配到的时间上（x(p[0])）")
	}
	// 找不到就写 —：这条分支必须存在（而且不能顺手把 bars 那一行也吞掉）。
	if !regexp.MustCompile(`if \(!p\) \{`).MatchString(hover) {
		t.Error("drawHover() 缺少「这条曲线在这个时间附近没有点」的分支")
	}
	if !regexp.MustCompile(`rows\.push\(s\.label \+ ' —'\)`).MatchString(hover) {
		t.Error("找不到点时应当写「<名称> —」，不要拿别的时间的点充数")
	}
	// 丢包竖条那一行留着：整桶全丢时它才是最该看到的那一行。
	if !regexp.MustCompile(`if \(s\.bars\) \{`).MatchString(hover) {
		t.Error("悬浮读数里的「丢包 X%」那一行被误删了（丢包与延迟是两件事）")
	}

	// 悬浮时间：桶宽超过最细的历史桶就显示 [起点, 起点+桶宽)，否则单个时刻。
	stamp := chartFuncBody(chart, "function hoverStampText(")
	if stamp == "" {
		t.Fatal("chart.js 缺少 hoverStampText()：悬浮读数的时间还是只写一个起点")
	}
	if !strings.Contains(stamp, "opts.bucketSec") {
		t.Error("区间要用**桶宽**（opts.bucketSec，来自 /ping 的 meta.bucket_sec）判断，不能写死")
	}
	if !regexp.MustCompile(`width <= HOVER_INTERVAL_MIN_SEC`).MatchString(stamp) ||
		!regexp.MustCompile(`return opts\.xFormat\(ts\);`).MatchString(stamp) {
		t.Error("桶宽不超过最细的历史桶（HOVER_INTERVAL_MIN_SEC）时只写一个时刻（各档位自己的格式），否则太啰嗦")
	}
	// 右端是**开**区间：直接写起点 + 桶宽，不是 + 桶宽 - 1。
	if !regexp.MustCompile(`var end = ts \+ width;`).MatchString(stamp) {
		t.Error("区间的右端应当是「起点 + 桶宽」（开区间），不要减一（08:30–09:30 而不是 08:30–09:29）")
	}
	if !strings.Contains(stamp, "'–'") {
		t.Error("区间要用「–」连接两端（08:30–09:30）")
	}
	// 跨天：两端都要带日期，否则 "23:30–00:30" 看上去像倒着走。
	if !strings.Contains(stamp, "dayKey(ts) === dayKey(end)") || !strings.Contains(stamp, "dayHM(") {
		t.Error("跨天时两端都要带日期（09-29 23:30–09-30 00:30）")
	}
}

// 悬浮读数的"区间"阈值必须盖住**每一个比最细历史桶更粗的档位**。
//
// 这条是被一次真实的回归逼出来的：阈值原来是写死的 3600 秒，而那个数就是
// **当时**延迟图桶宽表里最大的一档（7d）。桶宽改细之后 7d 变成 900 秒，
// 它掉到线外面去了 —— 7d 档的悬浮从"08:30–09:30"退回成"08:30"，
// 用户上一轮明确要的"看得到这一段有多长"被悄悄抵消一半。
// 页面上没有任何异常：读数照样有，只是少了一半信息。
//
// 所以这里把前端阈值与**后端那张桶宽表**对起来（store.PingRanges 是唯一事实来源）：
// 任何比 1 分钟（本项目最细的历史桶，ping_samples_1m 一行就是一分钟）更粗的档位，
// 都必须落在阈值之上。以后再改桶宽，只要改出一个"粗桶"，这条就会响。
func TestFrontendHoverIntervalThresholdCoversCoarseBuckets(t *testing.T) {
	chart := readAsset(t, "chart.js")
	m := regexp.MustCompile(`var HOVER_INTERVAL_MIN_SEC = (\d+);`).FindStringSubmatch(chart)
	if m == nil {
		t.Fatal("chart.js 缺少 HOVER_INTERVAL_MIN_SEC：悬浮区间的阈值又变成写死的魔法数字了")
	}
	sec, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("解析 HOVER_INTERVAL_MIN_SEC=%q: %v", m[1], err)
	}
	// 60 秒 = 最细的历史桶。阈值高于它，就意味着某些"一格不止一分钟"的档位
	// 又被退回成单个时刻了。
	const finestBucketSec = 60
	if sec != finestBucketSec {
		t.Errorf("HOVER_INTERVAL_MIN_SEC = %d，期望 %d（最细的历史桶）：阈值一旦抬高，"+
			"桶宽落在它下面的档位就又只剩一个时刻了", sec, finestBucketSec)
	}
	// 逐个档位对一遍：比最细桶更粗的（1d/3d/7d…）必须显示区间。
	coarse := 0
	for _, r := range store.PingRanges() {
		if r.Bucket <= finestBucketSec {
			continue // 一格正好一分钟：写单个时刻不含糊
		}
		coarse++
		if r.Bucket <= int64(sec) {
			t.Errorf("%s 档桶宽 %d 秒 > 最细的历史桶 %d 秒，却 ≤ HOVER_INTERVAL_MIN_SEC(%d)：这一档的悬浮会退回单个时刻（正是这次要修的回归）",
				r.Key, r.Bucket, finestBucketSec, sec)
		}
	}
	if coarse == 0 {
		t.Fatal("延迟图桶宽表里一个粗桶都没有：这条断言失去了意义，请连同 store.pingRangeSpecs 一起复核")
	}
	t.Logf("阈值 %d 秒：%d 个粗桶档位（> %d 秒）都会显示区间", sec, coarse, finestBucketSec)
}

// 延迟图**永远不画峰值线**、**Y 轴也不为峰值留空间**，但**悬浮读数里仍然要有峰值**。
//
// 用户原话："默认不显示峰值线，把按钮也去掉吧，图表上面的留白因为峰值延迟的缘故
// 变得太多了，但是我需要保留移到上面的时候也能显示峰值"。
//
// 于是原来被一个 showMax 捆在一起的三件事必须拆成两半：
//
//	画不画淡线 + Y 轴算不算峰值   → showMax（延迟图写死 false；资源图照旧 true）
//	悬浮浮层里列不列峰值那一行     → hoverPeak（默认 true，延迟图不覆盖）
//
// 为什么必须静态钉住"悬浮那一行还在"：这一轮的改动主题就是"删掉峰值相关的东西"，
// 下一个清理的人很自然会把 drawHover 里那三行一起删掉 —— 而画面上**看不出少了一行**
// （悬浮读数本来就有三四行），只有把鼠标移上去逐行读才发现。真浏览器里那条断言
// 见 e2e 的 TestMobileLatencyChartStaysConnected（场景 0）。
func TestFrontendLatencyKeepsPeakForHoverOnly(t *testing.T) {
	js := readAsset(t, "app.js")
	chart := readAsset(t, "chart.js")

	// 1) 延迟图的选项：showMax 写死 false（不是 view.peak），hoverPeak 明确开着。
	opts := funcBody(js, "function latChartOptions(")
	if opts == "" {
		t.Fatal("app.js 缺少 latChartOptions()")
	}
	if !regexp.MustCompile(`showMax: false`).MatchString(opts) {
		t.Error("延迟图的 showMax 必须写死 false：峰值淡线不画 + Y 轴不为峰值留空间" +
			"（用户抱怨的「上面留白太多」就是这一半；别把它改回一个可配置项）")
	}
	if regexp.MustCompile(`showMax: view\.`).MatchString(opts) {
		t.Error("延迟图的 showMax 不该再接到任何开关上：「峰值线」chip 已经删掉，没有人能改它了")
	}
	if !regexp.MustCompile(`hoverPeak: true`).MatchString(opts) {
		t.Error("延迟图必须显式打开 hoverPeak：峰值线不画了，但悬浮读数里的「峰值 N ms」那一行要留着")
	}
	// 其余三个开关照旧接在自己的选项上。
	for _, wire := range []string{"showMean: view.mean", "smooth: view.smooth"} {
		if !regexp.MustCompile(strings.ReplaceAll(wire, ".", `\.`)).MatchString(opts) {
			t.Errorf("latChartOptions() 少了 %s：这次只动峰值，另外两个开关不许受影响", wire)
		}
	}

	// 2) 引擎这边：轴的范围里那句条件还在（showMax=false 才真的能让轴不为峰值留空间），
	//    淡线仍然受 per-series 的 showMax 控制，平均线与它是各自独立的判断。
	b := chartFuncBody(chart, "function bounds(")
	if b == "" {
		t.Fatal("bounds() 的函数体没截取到")
	}
	if !regexp.MustCompile(`if \(opts\.showMax && p\[2\] > vMax\) vMax = p\[2\];`).MatchString(b) {
		t.Error("bounds() 里应当是 `opts.showMax && p[2] > vMax`：showMax=false 时轴不许把峰值算进去" +
			"（这就是「曲线撑满绘图区」的实现）")
	}
	draw := chartFuncBody(chart, "function draw()")
	if draw == "" {
		t.Fatal("draw() 的函数体没截取到")
	}
	if !regexp.MustCompile(`if \(opts\.showMax && s\.showMax !== false\) \{`).MatchString(draw) {
		t.Error("draw() 里峰值淡线要受 showMax 控制（关掉后不画它）")
	}
	if !regexp.MustCompile(`if \(opts\.showMean !== false\) \{`).MatchString(draw) {
		t.Error("draw() 里平均线要受 showMean 控制（关掉「延迟」后不画它）")
	}
	meanAt := strings.Index(draw, "if (opts.showMean !== false) {")
	peakAt := strings.Index(draw, "if (opts.showMax && s.showMax !== false) {")
	if meanAt < 0 || peakAt < 0 || meanAt == peakAt {
		t.Error("平均线与峰值线必须是两处独立判断（关掉一个不影响另一个）")
	}

	// 3) **悬浮那一行必须留着**（这一条就是这次改动的重点）：
	//    它由 hoverPeak 控制，且与 showMax 不再有任何关系。
	hover := chartFuncBody(chart, "function drawHover(")
	if hover == "" {
		t.Fatal("drawHover() 的函数体没截取到")
	}
	if !regexp.MustCompile(`opts\.hoverPeak &&`).MatchString(hover) {
		t.Error("drawHover() 里的峰值行必须由 hoverPeak 控制：它已经和 showMax（画线/轴）拆开了")
	}
	if !strings.Contains(hover, "'  峰值 '") {
		t.Error("悬浮浮层里的「峰值 N ms」那一行被删掉了：用户明确要求保留" +
			"（「我需要保留移到上面的时候也能显示峰值」），它现在是峰值唯一露出的地方")
	}
	if regexp.MustCompile(`opts\.showMax && typeof p\[2\]`).MatchString(hover) {
		t.Error("悬浮里的峰值行又跟 showMax 绑在一起了：延迟图 showMax=false，绑上去这一行就永远不显示")
	}
	if !regexp.MustCompile(`hoverPeak: true,`).MatchString(chart) {
		t.Error("chart.js 的默认选项里 hoverPeak 应当默认为 true（资源图不受影响，延迟图不用手动开）")
	}

	// 4) 资源图一个字都不许动：两张百分比的图与速率图照旧把 showMax 传成 true。
	//    它们的 points 里没有第 3 位，这条开着对画面没有任何影响 —— 但"资源图的
	//    峰值线行为不变"这件事必须能从代码上读出来，否则下一次改动会顺手把它们也关掉。
	if n := strings.Count(js, "showMax: true"); n != 2 {
		t.Errorf("资源图（百分比 / 速率）应当仍然传 showMax: true，实际匹配到 %d 处："+
			"用户只说了延迟图，其余五张图的峰值线行为不变", n)
	}

	// 5) 「丢包」开关：关掉时逐 series 摘掉 bars（引擎那边的约定保持"有 bars 就画"）。
	apply := funcBody(js, "function applyLatSeries(")
	if apply == "" {
		t.Fatal("app.js 缺少 applyLatSeries()")
	}
	if !regexp.MustCompile(`view\.loss \? s : \{`).MatchString(apply) {
		t.Error("「丢包」关掉时应当把 bars 从 series 上摘掉（否则竖条照画）")
	}
}

// 迷你条的延迟格子按**绝对阈值与本机倍数取严**分级（用户定的"两者取严"）。
//
// 两个口径都要，因为它们各挡住一类误判：
//   - 只有绝对阈值：一条常年 20ms 的线路抖到 60ms（3 倍）在绝对分档里还是绿；
//   - 只有相对倍数：一台常年 205ms 的机器上 197.9ms 显示绿色，而绝对分档
//     （180 < 197.9 ≤ 240）应当是琥珀色 —— 这正是用户报的那个例子。
//
// 为什么钉得这么细：分级坏掉的方式全是静默的 —— 格子照画、颜色照有，只是
// "哪一段该被注意到"变了；而基准一旦与行首那个数不是同一个，同一张卡片上
// 就会自相矛盾（数字写着 40ms，格子却按另一个基准判红）。
// 画出来的颜色由 e2e 的 TestMiniLatencyColorsInRealBrowser 在真浏览器里量。
func TestFrontendMiniLatencyGradesByOwnAverage(t *testing.T) {
	js := readAsset(t, "app.js")
	css := readAsset(t, "style.css")

	render := funcBody(js, "function renderMiniBar(")
	if render == "" {
		t.Fatal("app.js 缺少 renderMiniBar()")
	}
	// 基准必须是行首那个数（mini.lat_ms：整窗口按成功探测次数加权）——
	// 不是别的字段、也不是前端自己再算一遍。
	if !regexp.MustCompile(`latGrade\(value,\s*mini\.lat_ms\)`).MatchString(render) {
		t.Error("迷你条延迟格子必须用 mini.lat_ms（行首那个数）当基准")
	}
	lat := funcBody(js, "function latGrade(")
	if lat == "" {
		t.Fatal("app.js 缺少 latGrade()：延迟着色没有统一口径")
	}
	if !regexp.MustCompile(`if \(!\(avg > 0\)\) return '';`).MatchString(lat) {
		t.Error("均值算不出来（lat_ms = 0，没配目标/整段全丢）时应当保持浅灰：拿 0 当基准会把所有格子判成红的")
	}
	// 取严：两条判据各自命中更坏的那一档。顺序必须是先 bad 后 warn
	// （写反了 warn 会把 bad 盖掉，全图只剩黄绿两色）。
	if !regexp.MustCompile(`if \(value > LAT_ABS_BAD_MS \|\| value > avg \* MINI_LAT_BAD_RATIO\) return 'bad';`).MatchString(lat) {
		t.Error("红档应当是「value > 240（绝对）**或** value > 2× 均值（相对）」：两个口径取严")
	}
	if !regexp.MustCompile(`if \(value > LAT_ABS_WARN_MS \|\| value > avg \* MINI_LAT_WARN_RATIO\) return 'warn';`).MatchString(lat) {
		t.Error("黄档应当是「value > 180（绝对）**或** value > 1.2× 均值（相对）」：两个口径取严")
	}
	if strings.Index(lat, "'bad'") > strings.Index(lat, "'warn'") {
		t.Error("bad 必须先判（否则 warn 会把红档盖成黄档）")
	}
	// 绝对边界出自 Komari Emerald 的分档，且必须是具名常量 —— 散落成字面量之后
	// 下一个改阈值的人只会找到其中一处。
	if strings.Contains(lat, "180") || strings.Contains(lat, "240") {
		t.Error("latGrade() 里不该出现 180 / 240 这两个字面量：绝对边界要写成 LAT_ABS_WARN_MS / LAT_ABS_BAD_MS")
	}
	for _, needle := range []string{"var LAT_ABS_WARN_MS = 180;", "var LAT_ABS_BAD_MS = 240;"} {
		if !strings.Contains(js, needle) {
			t.Errorf("app.js 缺少绝对边界常量 %q（出处见注释：Komari Emerald 主题的分档）", needle)
		}
	}
	if !strings.Contains(js, "Komari") {
		t.Error("绝对边界的注释里要写明出处（Komari Emerald 主题的分档）：不写清来源，下一个人只会当它是随手取的数")
	}

	// **防回归**：相对的 warn 倍数必须 > 1。
	//
	// 写成"裸均值"（value > avg）看着更简单，但按定义大约一半的值都在均值之上 ——
	// 那样每一行会有一半格子变黄，图就没有信息量了（1.2 这个数就是为此存在的）。
	// 这里解析实际写在代码里的那个数，而不是钉字面量：以后把 1.2 调成 1.3 也应当过。
	m := regexp.MustCompile(`var MINI_LAT_WARN_RATIO = ([0-9.]+);`).FindStringSubmatch(js)
	if m == nil {
		t.Fatal("app.js 缺少 MINI_LAT_WARN_RATIO：相对 warn 倍数没有名字，改起来只能靠搜数字")
	}
	ratio, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		t.Fatalf("解析 MINI_LAT_WARN_RATIO = %q: %v", m[1], err)
	}
	if ratio <= 1 {
		t.Errorf("相对 warn 倍数 = %v，必须 > 1：写成裸均值（1）会让每行大约一半的格子变黄，"+
			"图就没有信息量了（这正是 1.2 存在的理由）", ratio)
	}
	if ratio >= 2 {
		t.Errorf("相对 warn 倍数 = %v，已经不小于红档的 %v 倍：黄档永远不会出现", ratio, 2)
	}

	// 「探测」那一行必须**共用同一个函数**（不是各写一份同样的判断）：
	// 同一台机器上"格子绿着、探测行黄着"这种自相矛盾比"哪一档更准"严重得多。
	probe := funcBody(js, "function renderProbeLine(")
	if probe == "" {
		t.Fatal("app.js 缺少 renderProbeLine()")
	}
	if !strings.Contains(probe, "latGrade(t.lat_ms, t.avg_ms)") {
		t.Error("「探测」那一行必须走同一个 latGrade（两套口径会让两行颜色互相矛盾）")
	}
	if n := strings.Count(js, "function latGrade("); n != 1 {
		t.Errorf("延迟着色只能有一处实现（找到 %d 个 latGrade 定义）", n)
	}

	// 规则要给用户讲清楚（挂在迷你条「延迟」那一行的标题上，悬停可见）：
	// 两个口径取严这件事不是一眼能猜出来的。数字必须由常量拼出来 —— 写死
	// "180/240/1.2" 的话，以后调阈值就会留下一句骗人的说明。
	if !strings.Contains(js, "var MINI_LAT_HINT = ") ||
		!strings.Contains(js, "lat.label.title = MINI_LAT_HINT") {
		t.Error("迷你条「延迟」那一行的标题上应当挂 MINI_LAT_HINT：说明颜色是" +
			"「绝对阈值与本机倍数取严」——用户看不出黄色是按哪条判据来的")
	}
	// 这段是拼接出来的字符串，funcBody 那种粗截法会一路吃进后面的函数：
	// 直接截到语句结尾的 "';"。
	hint := ""
	if at := strings.Index(js, "var MINI_LAT_HINT = "); at >= 0 {
		if stop := strings.Index(js[at:], "';"); stop >= 0 {
			hint = js[at : at+stop]
		}
	}
	if hint == "" {
		t.Fatal("MINI_LAT_HINT 的定义截取不到")
	}
	for _, need := range []string{"LAT_ABS_WARN_MS", "LAT_ABS_BAD_MS", "MINI_LAT_WARN_RATIO", "MINI_LAT_BAD_RATIO"} {
		if !strings.Contains(hint, need) {
			t.Errorf("MINI_LAT_HINT 里的数字要由 %s 拼出来：写死字面量会在调阈值之后变成一句骗人的说明", need)
		}
	}

	// 三个颜色类都得有对应的 CSS：少一个就是"某些格子莫名不变色"，页面上不报错。
	for _, rule := range []string{".mini-cell.ok", ".mini-cell.warn", ".mini-cell.bad"} {
		if !strings.Contains(css, rule) {
			t.Errorf("style.css 缺少 %s 规则", rule)
		}
	}
	// 丢包格子不动：它用的是绝对阈值（0% / 5%），与延迟那一套取严规则无关。
	if !strings.Contains(render, "miniLossClass") || !regexp.MustCompile(`MINI_LOSS_WARN_PCT`).MatchString(js) {
		t.Error("丢包格子的绝对阈值口径不该被这次改动牵连（它一直是 0%/5%）")
	}
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
		// 详情页：实时网络（速率）、累计流量、入库以来累计流量。
		"infoRow(net, '实时网络', '↑ ' + fmtRate(node.tx_rate) + '  ↓ ' + fmtRate(node.rx_rate))",
		"infoRow(net, '累计流量', '↑ ' + fmtBytesDec(node.tx_total) + '  ↓ ' + fmtBytesDec(node.rx_total))",
		// 标签是「入库以来累计流量」（原来叫「历史累计流量」）：这个数来自
		// SUM(rx), SUM(tx) FROM traffic_daily，是**这个库开始记录以来**的累计
		// （删掉节点会一起级联清掉），不是"这台机器开机以来"—— 原名会被读成后者。
		"infoRow(tra, '入库以来累计流量', '↓ ' + fmtBytesDec(node.traffic_total_rx) + '  ↑ ' + fmtBytesDec(node.traffic_total_tx))",
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
// 拖不动、忘了本地重排只是迟半拍、忘了回滚只是"下次刷新顺序又变回去"。
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

// 延迟数据的约定：avg == 0 表示"这一桶**没有任何成功的探测**"（没有延迟样本），
// **不是** 0 毫秒。后端就是这么约定的（ping_samples_1m 里整分钟全丢的行
// avg_ms = 0、up_cnt = 0；服务端已改成按 up_cnt 加权，见 store.QueryPingSeries），
// 前端有两处必须守住同一条约定：
//
//  1. 手机端二次聚合 aggregate()：只累加正样本，分母也只数正样本。
//     把 0 平均进去的错误方向是反的 —— 线路越丢包、图上的延迟越低
//     （一个 200ms 的桶旁边挂三个全丢的桶，均值被拽到 50ms）；
//     整个合并桶一个正样本都没有时，结果必须是 0（"没有样本"的哨兵），
//     而不是 0/0 得到的 NaN。
//
//  2. 画线前的 latencySeriesFor()：把"没有样本"的 0 规范化成 null。
//     chart.js 的 drawLine 只跳过**非数字**，0 会被画成 0ms（画布底边），
//     而算 Y 轴范围的 bounds() 又不拿 0 撑轴 —— 于是画面上是"轴的范围里
//     根本没有 0、线却扎到了底"，看起来就像"丢包时延迟反而最低"。
//
// 为什么用正则钉住这几行的**写法**，而不是只断言函数存在：这两个错误都属于
// "函数都在、页面照样能看"的那一类 —— 曲线照画、图例照显示、竖条照画，
// 只有数值是错的，除了盯着屏幕看没有别的线索。
func TestFrontendLatencyZeroMeansNoSample(t *testing.T) {
	js := readAsset(t, "app.js")
	chart := readAsset(t, "chart.js")

	// 1) 聚合：分子（累加）与分母（计数）必须在同一个"只算正样本"的分支里。
	agg := funcBody(js, "function aggregate(")
	if agg == "" {
		t.Fatal("app.js 缺少 aggregate()")
	}
	guard := regexp.MustCompile(`(?s)if \(p\[1\] > 0\) \{(.*?)\}`).FindStringSubmatch(agg)
	if guard == nil {
		t.Fatal("aggregate() 里没有 `if (p[1] > 0)` 分支：avg == 0（这一桶全丢）会被当成 0ms 累加进去")
	}
	if !strings.Contains(guard[1], "cur[1] += p[1];") {
		t.Error("avg 的累加必须在 `p[1] > 0` 分支里：0 是「没有样本」的哨兵，累加它会让越丢包的段平均延迟越低")
	}
	if !strings.Contains(guard[1], "cur[3] += 1;") {
		t.Error("分母（正样本个数）必须与分子在同一个分支里：把全丢的桶也数进分母，等于在前端再犯一次 AVG(avg_ms)")
	}
	// 两行各自只能出现一次：还留在分支外面（旧写法）就说明会被无条件累加。
	if n := strings.Count(agg, "cur[1] += p[1];"); n != 1 {
		t.Errorf("`cur[1] += p[1];` 出现了 %d 次，期望 1 次（只许在正样本分支里）", n)
	}
	if n := strings.Count(agg, "cur[3] += 1;"); n != 1 {
		t.Errorf("`cur[3] += 1;` 出现了 %d 次，期望 1 次（只许在正样本分支里）", n)
	}
	// max 的口径这次不动：它是"这一桶最高多少"，仍然对全部点取最大值。
	if strings.Contains(guard[1], "cur[2]") {
		t.Error("max（cur[2]）不该被挪进正样本分支：它的取最大值口径是另一件事，这次不改")
	}
	if !regexp.MustCompile(`cur\[2\] = Math\.max\(cur\[2\], p\[2\]\);`).MatchString(agg) {
		t.Error("aggregate() 少了 max 的取最大值（峰值曲线会画成一条平的）")
	}
	// 一个正样本都没有的合并桶：avg 是 0（"没有样本"），不是 NaN。
	if !regexp.MustCompile(`c\[3\] > 0 \? c\[1\] / c\[3\] : 0`).MatchString(agg) {
		t.Error("合并桶里一个正样本都没有时 avg 必须是 0：c[1] / c[3] 会得到 NaN，Y 轴范围与悬浮读数会一起坏掉")
	}
	// 丢包率（第 4 位）的取平均口径不变。
	if !strings.Contains(agg, "cur[4] += p[3];") ||
		!regexp.MustCompile(`c\[5\] > 0\) merged\.push\(c\[4\] / c\[5\]\)`).MatchString(agg) {
		t.Error("丢包率（第 4 位）应当照旧取这几个桶的平均")
	}

	// 2) 画线前把"没有样本"的点规范化成 null —— 而且**只对延迟序列**做。
	lat := funcBody(js, "function latencySeriesFor(")
	if lat == "" {
		t.Fatal("app.js 缺少 latencySeriesFor()：avg == 0 的点会被 drawLine 画成 0ms")
	}
	if !regexp.MustCompile(`has \? p\[1\] : null`).MatchString(lat) {
		t.Error("latencySeriesFor() 应当把 avg 不是正数的点写成 null（chart.js 的 drawLine 会跳过非数字）")
	}
	if !regexp.MustCompile(`has && p\[2\] > 0 \? p\[2\] : null`).MatchString(lat) {
		t.Error("max（p[2]）同样要规范化：整桶全丢时 max 也是 0，峰值淡线照样会扎到底")
	}
	if !strings.Contains(lat, ", p[3]];") {
		t.Error("规范化后的点必须原样带上第 4 位（丢包率）：丢包竖条读的就是它，漏掉的话真丢了包也画不出条")
	}
	ping := funcBody(js, "function loadPingChart()")
	if ping == "" {
		t.Fatal("app.js 缺少 loadPingChart()")
	}
	if !strings.Contains(ping, "points: latencySeriesFor(points),") {
		t.Error("延迟图必须走 latencySeriesFor()：直接把 seriesFor() 的结果交出去，丢包处就会扎到 0ms")
	}
	// 非延迟序列（CPU / 内存 / 磁盘 / 网络 / 流量）绝不能经过规范化：
	// 0% CPU、0 B/s 都是**合法读数**，被当成缺失就等于把真实数据抹掉。
	if strings.Contains(js, "latencySeriesFor(data.points)") ||
		strings.Contains(js, "latencySeriesFor(s.data.points)") {
		t.Error("CPU/内存/磁盘/网络的点不能过 latencySeriesFor()：它们的 0 是真实读数，不是缺失")
	}
	if n := strings.Count(js, "latencySeriesFor("); n != 2 {
		t.Errorf("latencySeriesFor() 只该有「定义 + 延迟图一个调用点」共 2 处，实际 %d 处 —— 多出来的调用点会把别的序列的 0 当成缺失", n)
	}
	// 通用取点 seriesFor() 保持原样：只做聚合，不碰 0 的语义（它给 CPU 等图用）。
	generic := funcBody(js, "function seriesFor(")
	if generic == "" {
		t.Fatal("app.js 缺少 seriesFor()")
	}
	if strings.Contains(generic, "null") {
		t.Error("seriesFor() 是通用取点（CPU 等也走它），里面不能出现 null 规范化 —— 那会把 0% CPU 当成缺失")
	}

	// 3) chart.js：悬浮读数遇到 null 既不能崩、也不能显示 0 ms。
	hover := chartFuncBody(chart, "function drawHover(")
	if hover == "" {
		t.Fatal("chart.js 的 drawHover() 函数体没截取到")
	}
	if !regexp.MustCompile(`var hasValue = typeof p\[1\] === 'number' && isFinite\(p\[1\]\);`).MatchString(hover) {
		t.Error("悬浮读数要先判 p[1] 是不是数字：y(null) 会画出假的 0ms 点，yFormat(null) 里的 toFixed 还会抛 TypeError")
	}
	if !regexp.MustCompile(`if \(hasValue\) \{`).MatchString(hover) {
		t.Error("画点与格式化都必须收在 hasValue 分支里")
	}
	if !regexp.MustCompile(`rows\.push\(s\.label \+ ' —'\);`).MatchString(hover) {
		t.Error("没有有效读数时应当显示 —：0 ms 是合法读数（这一桶很快），写 0 与「一个样本都没有」正好相反")
	}
	if !regexp.MustCompile(`if \(s\.bars\) \{`).MatchString(hover) {
		t.Error("丢包那一行必须留在 hasValue 分支**外面**：整桶全丢时最该看到的就是丢包 100%")
	}
	// 丢包竖条读的是第 4 位（bars.valueIndex = 3，见上面 TestFrontendChartCardsAreSplit），
	// 规范化没有动它；这里再钉一次"drawBars 不碰第 2 位"，免得以后有人顺手改坏。
	bars := chartFuncBody(chart, "function drawBars(")
	if bars == "" {
		t.Fatal("chart.js 的 drawBars() 函数体没截取到")
	}
	if strings.Contains(bars, "[1]") {
		t.Error("drawBars() 只该读 bars.valueIndex（丢包率在位 3）：去读 p[1] 的话丢包条会被延迟的缺失值带走")
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
	// 反过来：与留白无关的上限一个都不许动（对话框、标签徽章、图表块里的空态…）。
	for _, keep := range []string{"max-width: 380px", "max-width: 100%", "max-width: none", "max-width: calc(100vw - 32px)"} {
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

// 改动 2② / 改动 A：X 轴标签从"基准间隔"出发，按标签的**实际文本宽度**自动稀疏到
// 读起来不累（间距见 X_LABEL_MIN_GAP），稀疏用的是钟表/日历上有的**整齐刻度**。
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

	// 2) 整齐刻度阶梯：每一步都必须是钟表/日历上有的分档（整分钟 / 整小时 / 整天）。
	//
	//    这里曾经钉的是"基准间隔 × 整齐倍数"（X_STEP_MULTIPLIERS）：倍数化在六个
	//    档位上会算出「1 天 = 900 秒 × 96」这种谁也读不出来的刻度，于是换成绝对秒数
	//    （见 chart.js 的 X_STEP_LADDER）。要守的不变量没变：**每一步都得是能一眼
	//    换算的分档**，不能冒出"每 7 分钟""每 3.2 小时"。
	if !regexp.MustCompile(`var X_STEP_LADDER = \[\s*60, 120, 300,`).MatchString(codeLines(chart)) {
		t.Error("稀疏阶梯应当从 1/2/5 分钟这种整齐档开始（按整数倍递增会冒出『每 7 分钟』）")
	}
	ladder := regexp.MustCompile(`var X_STEP_LADDER = \[([^\]]*)\]`).FindStringSubmatch(codeLines(chart))
	if ladder == nil {
		t.Fatal("chart.js 里找不到 X_STEP_LADDER 的定义")
	}
	// 允许的刻度只有这些：1/2/5/10/15/30 分钟、1/2/3/6/12 小时、1/2/7 天。
	allowed := map[int]bool{
		60: true, 120: true, 300: true, 600: true, 900: true, 1800: true,
		3600: true, 7200: true, 10800: true, 21600: true, 43200: true,
		86400: true, 172800: true, 604800: true,
	}
	seen := 0
	prev := 0
	for _, raw := range strings.Split(ladder[1], ",") {
		n, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil {
			t.Fatalf("X_STEP_LADDER 里解析不出数字：%q", raw)
		}
		seen++
		if !allowed[n] {
			t.Errorf("X_STEP_LADDER 里的 %d 秒不是整齐刻度：画出来就是『每 7 分钟』那种没人用的间隔（允许的只有整分钟/整小时/整天）", n)
		}
		if n <= prev {
			t.Errorf("X_STEP_LADDER 必须严格递增（%d 排在 %d 后面）：逐级放大靠的就是这个顺序", n, prev)
		}
		prev = n
	}
	if seen < 6 {
		t.Errorf("X_STEP_LADDER 只有 %d 级：档位跨度很大时稀疏不到位，标签仍会重叠", seen)
	}
	// 一整天的刻度必须在：7d 档要按"一天一个标签"稀疏（否则标签会变成一串
	// 重复的日期，见 app.js 的 RANGE_X_FORMAT 与 TestFrontendXAxisFormatsPerRange）。
	if !allowed[86400] || !strings.Contains(ladder[1], "86400") {
		t.Error("阶梯里缺少 86400（一天）：7d 档只能稀疏到一天以上，标签会退成一串一模一样的 MM-DD")
	}

	// 3) 间隔逐级试到放得下为止：候选**只能来自阶梯**，而且不许比基准间隔更细。
	step := chartFuncBody(chart, "function xLabelStep(")
	if step == "" {
		t.Fatal("chart.js 缺少 xLabelStep()：标签间隔没有从基准间隔逐级放大")
	}
	if !strings.Contains(step, "opts.tickBaseSec") {
		t.Error("xLabelStep() 应当从后端给的基准间隔（opts.tickBaseSec）出发")
	}
	if !regexp.MustCompile(`var step = X_STEP_LADDER\[i\];`).MatchString(step) {
		t.Error("候选间隔必须逐个取自 X_STEP_LADDER（写成『基准间隔 × 整数倍』就会算出一小时三十六分这种刻度）")
	}
	if !regexp.MustCompile(`if \(step < base\) continue;`).MatchString(step) {
		t.Error("比基准间隔还细的刻度不许用：那不是稀疏，是把这一档的刻度语义改掉")
	}
	if !strings.Contains(step, "labelFits(") {
		t.Error("xLabelStep() 必须靠 labelFits() 逐个刻度试到放得下为止")
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
	// 为什么不能直接按基准间隔画、为什么刻度必须整齐：两段理由都要留在注释里
	// （前者是"会糊成一片/互相重叠"，后者是"不能冒出每 7 分钟这种刻度"）。
	if !regexp.MustCompile(`糊在一起|重叠`).MatchString(chart) {
		t.Error("注释里要写明为什么不能直接按基准间隔画（1h 档 60 个标签会糊成一片）")
	}
	for _, note := range []string{"钟表", "每 7 分钟"} {
		if !strings.Contains(chart, note) {
			t.Errorf("注释里缺少 %q：下一个改 X 轴的人得知道为什么刻度必须是钟表/日历上有的分档", note)
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

// jsObject 截出 "var NAME = {" 到它那一层的 "};" 之间的对象字面量。
//
// 只用于"这张表里有哪几档、每档给的是什么"这类断言：真去解析 JS 不值得，
// 而按行 grep 又会跨过表尾跑到别的常量里去。
func jsObject(t *testing.T, js, name string) string {
	t.Helper()
	marker := "var " + name + " = {"
	at := strings.Index(js, marker)
	if at < 0 {
		return ""
	}
	rest := js[at:]
	end := strings.Index(rest, "\n  };")
	if end < 0 {
		return rest
	}
	return rest[:end]
}

// 改动 A①：相邻标签之间的最小间距要从"正好一个标签宽"放宽到"空白比字宽"。
//
// 这条**不钉具体数字**（28 → 56 是取值，不是不变量）：钉的是"比原来那档明显宽松、
// 又没宽到一屏只剩两三个标签"，以及"注释里必须留下取舍说明" —— 间距越大标签越少，
// 下一个人调这个数时得知道代价。
func TestFrontendXAxisLabelGapWidened(t *testing.T) {
	chart := readAsset(t, "chart.js")

	m := regexp.MustCompile(`var X_LABEL_MIN_GAP = (\d+);`).FindStringSubmatch(chart)
	if m == nil {
		t.Fatal("chart.js 缺少 X_LABEL_MIN_GAP：判定标签放不放得下靠它")
	}
	gap, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("解析 X_LABEL_MIN_GAP=%q: %v", m[1], err)
	}
	// v1.0.32 的值：一个 "HH:MM" 标签差不多就这么宽，两个标签之间只剩这点空白。
	const oldGap = 28
	if gap <= oldGap {
		t.Errorf("X_LABEL_MIN_GAP = %d，没有比 %d 放宽：紧挨着的两个时刻读起来仍然是一串数字", gap, oldGap)
	}
	// 下界：至少一个半标签宽，才谈得上"空白比字宽"。
	if gap < oldGap*3/2 {
		t.Errorf("X_LABEL_MIN_GAP = %d：还不到一个半标签宽（旧值 %d），读起来仍然费劲", gap, oldGap)
	}
	// 上界：再宽下去，窄一点的画布上会只剩两三个标签，时间轴就没意义了。
	if gap > 96 {
		t.Errorf("X_LABEL_MIN_GAP = %d 太宽了：窄画布上会只剩两三个标签，时间轴失去意义", gap)
	}

	// 取舍必须写在常量旁边（间距越大 → 标签越少），否则下一个人只看到"这个数好大"。
	note := chartFuncBody(chart, "// X_LABEL_MIN_GAP")
	if note == "" {
		note = chart
	}
	for _, word := range []string{"标签越少", "取舍"} {
		if !strings.Contains(note, word) {
			t.Errorf("X_LABEL_MIN_GAP 的注释里缺少 %q：间距越大标签越少这条代价必须写出来", word)
		}
	}

	// 它比的必须是两个标签**边缘之间**的空白（不是中心距、也不是锚点距）：
	// 标签是居中的，拿锚点去比会宽松半个标签 —— 那正是"看起来够宽、画出来却挤"。
	fit := chartFuncBody(chart, "function labelFits(")
	if fit == "" {
		t.Fatal("chart.js 的 labelFits() 函数体没截取到")
	}
	if !regexp.MustCompile(`left - prevRight < X_LABEL_MIN_GAP`).MatchString(fit) {
		t.Error("判定必须比两个标签的**边缘**之间的空白（left - prevRight）：拿锚点比会宽松半个标签")
	}
}

// 改动 A②③：每一档的标签内容按用户定稿的那张表给，**跨天的档位必须标出"哪天"**。
//
// 为什么钉在 app.js 的 RANGE_X_FORMAT 上：格式坏掉的方式全是静默的 —— 轴照画、
// 标签照有，只是 1d 档的 24 小时里一个日期都没有（用户的原话是"分不清哪段是今天、
// 哪段是昨天"），或者 3d/7d 档画出一串一模一样的 "09-29"。
//
// 日期与"跨天"判定必须走**服务端时区**渲染层（clockOf/dateOf/dateTimeOf → tzFields）：
// 图上每一个时刻的口径都要与后端切天一致；用浏览器本地时区判断"是不是新的一天"，
// 在两端时区不一致时会把日期标在错的位置上（v1.0.32 刚统一了这一口径）。
func TestFrontendXAxisFormatsPerRange(t *testing.T) {
	js := readAsset(t, "app.js")

	formats := jsObject(t, js, "RANGE_X_FORMAT")
	if formats == "" {
		t.Fatal("app.js 里找不到 RANGE_X_FORMAT 的定义")
	}
	// 六个档位一个都不能少（少一个就是这一档退回默认的 HH:MM）。
	for _, key := range []string{"'1h'", "'6h'", "'12h'", "'1d'", "'3d'", "'7d'"} {
		if !strings.Contains(formats, key+":") {
			t.Errorf("RANGE_X_FORMAT 里缺少 %s 档的格式", key)
		}
	}

	// 短档（1h/6h/12h）：纯时分。窗口 ≤ 12 小时，标签上再挂日期只是噪声
	// （唯一例外是 12h 档可能跨一次零点 —— 那条取舍写在 app.js 的注释里）。
	for _, key := range []string{"1h", "6h", "12h"} {
		line := regexp.MustCompile(`'` + key + `': function \(ts[^)]*\) \{ ([^}]*)\}`).FindStringSubmatch(formats)
		if line == nil {
			t.Errorf("%s 档的格式没解析出来", key)
			continue
		}
		if !strings.Contains(line[1], "clockOf(ts)") {
			t.Errorf("%s 档应当写时分（clockOf）：%s", key, line[1])
		}
		if strings.Contains(line[1], "dateOf(") {
			t.Errorf("%s 档不写日期（窗口 ≤ 12 小时）：%s", key, line[1])
		}
	}

	// 1d 档：**每天的第一个标签写日期**，其余写时分。
	day := regexp.MustCompile(`'1d': function \(ts[^)]*\) \{ ([^}]*)\}`).FindStringSubmatch(formats)
	if day == nil {
		t.Fatal("RANGE_X_FORMAT 里 1d 档的格式没解析出来")
	}
	if !strings.Contains(day[1], "startsNewDay(") {
		t.Error("1d 档必须按「跨天」挑日期标签（startsNewDay）：24 小时的窗口里全是纯时分就分不清哪段是哪天")
	}
	if !strings.Contains(day[1], "dateOf(ts)") || !strings.Contains(day[1], "clockOf(ts)") {
		t.Errorf("1d 档应当是「跨天处写 10-01、其余写 13:10」：%s", day[1])
	}

	// 3d 档：每天的**第一个**标签写日期，其余写时分；间隔 ≥ 一天时整排都是日期
	// （每一格本来就是新的一天）。这样一天一个日期标记，中间的刻度只写时分就够 ——
	// 半宽的资源图上因此放得下 6 个标签（每一格都写 MM-DD HH:MM 的话只放得下 3 个）。
	threeD := regexp.MustCompile(`'3d': function \(ts,\s*step,\s*prev[^)]*\) \{ ([^}]*)\}`).FindStringSubmatch(formats)
	if threeD == nil {
		t.Error("3d 档的格式没解析出来（它必须同时收 ts、实际间隔与自己前面那个标签）")
	} else {
		if !strings.Contains(threeD[1], "startsNewDay(ts, prev)") {
			t.Errorf("3d 档必须按「跨天」挑日期标签：%s", threeD[1])
		}
		if !strings.Contains(threeD[1], "step >= 86400") {
			t.Errorf("3d 档仍然要用实际间隔判断「整排都是日期」：%s", threeD[1])
		}
		if !strings.Contains(threeD[1], "dateOf(ts)") || !strings.Contains(threeD[1], "clockOf(ts)") {
			t.Errorf("3d 档应当是「跨天（或整排按天）写 09-29、其余写 13:10」：%s", threeD[1])
		}
	}

	// 7d 档：以日期为主，间隔细于一天时才带上时分（否则会画出一串一模一样的
	// "09-29"）——这正是上一轮加进来的"格式函数收实际间隔"的能力，
	// 这次改动不许把它弄丢（3d 档用的是同一个参数 + prev）。
	for _, key := range []string{"7d"} {
		line := regexp.MustCompile(`'` + key + `': function \(ts,\s*step[^)]*\) \{ ([^}]*)\}`).FindStringSubmatch(formats)
		if line == nil {
			t.Errorf("%s 档的格式没解析出来（它必须同时收 ts 与实际间隔 step）", key)
			continue
		}
		if !strings.Contains(line[1], "step >= 86400") {
			t.Errorf("%s 档应当按实际间隔判断要不要带时分（step >= 86400）：%s", key, line[1])
		}
		if !strings.Contains(line[1], "dateOf(ts)") || !strings.Contains(line[1], "dateTimeOf(ts)") {
			t.Errorf("%s 档应当是「间隔 ≥ 一天用 MM-DD，否则 MM-DD HH:MM」：%s", key, line[1])
		}
	}
	// 7d 档的间隔必须真的能稀疏到"一天"（阶梯里有 86400，见上一条用例）：
	// 稀疏不到一天就会退回 dateTimeOf —— 一行里挤着 7 个 "09-25 06:00"。

	// 跨天判定与日期格式都走时区渲染层：tzFields（Intl + 服务端时区名）。
	dayFn := funcBody(js, "function startsNewDay(")
	if dayFn == "" {
		t.Fatal("app.js 缺少 startsNewDay()：1d 档的日期判定没地方写")
	}
	if !strings.Contains(dayFn, "dayKeyOf(") {
		t.Error("startsNewDay() 应当比「哪一天」（dayKeyOf），而不是比小时数")
	}
	keyFn := funcBody(js, "function dayKeyOf(")
	if keyFn == "" {
		t.Fatal("app.js 缺少 dayKeyOf()：跨天判定没有统一出处")
	}
	if !strings.Contains(keyFn, "dayOf(ts)") {
		t.Error("dayKeyOf() 必须用 dayOf()（服务端时区下的 YYYY-MM-DD），不能自己拼浏览器日期")
	}
	for _, fn := range []string{"function clockOf(", "function dateOf(", "function dateTimeOf(", "function dayOf("} {
		body := funcBody(js, fn)
		if body == "" {
			t.Fatalf("app.js 缺少 %s", fn)
		}
		if !strings.Contains(body, "tzFields(") {
			t.Errorf("%s 必须走时区渲染层（tzFields）：图上每一个时刻都要按服务端时区渲染", fn)
		}
		// 浏览器本地时区的取值口只有一处退路（localFields）：
		// 出现在这里就说明有个别格式绕过了服务端时区。
		for _, bad := range []string{"getHours()", "getMinutes()", "getDate()", "getMonth()", "getFullYear()"} {
			if strings.Contains(body, bad) {
				t.Errorf("%s 里出现了浏览器本地时区的 %s：X 轴标签会与后端切天对不上", fn, bad)
			}
		}
	}
}

// 改动 A③：每一档的标签个数要有"适合观察"的上限（用户定稿的目标区间）。
//
// 为什么必须有：间距放宽之后，整行宽的大图（延迟卡约 1400px 绘图区）放得下十几个
// 标签 —— 不挤，但也没人会逐个读；而"几个人算合适"是**按档位**定的（1h 与 7d 的
// 密度感完全不同）。这里守住两件事：表里的上限落在目标区间内、并且真的接到了
// 每一个图表实例上（漏一处，那一张图的密度就不受控）。
func TestFrontendXAxisLabelCountTargets(t *testing.T) {
	js := readAsset(t, "app.js")
	chart := readAsset(t, "chart.js")

	// 用户定稿的目标区间：1h/6h/12h/1d 8~12 个，3d 6~10 个，7d 7~10 个。
	bands := map[string][2]int{
		"1h": {8, 12}, "6h": {8, 12}, "12h": {8, 12},
		"1d": {8, 12}, "3d": {6, 10}, "7d": {7, 10},
	}
	table := jsObject(t, js, "RANGE_X_LABEL_MAX")
	if table == "" {
		t.Fatal("app.js 里找不到 RANGE_X_LABEL_MAX：每一档的标签个数上限没地方给")
	}
	for key, band := range bands {
		m := regexp.MustCompile(`'` + key + `':\s*(\d+)`).FindStringSubmatch(table)
		if m == nil {
			t.Errorf("RANGE_X_LABEL_MAX 里缺少 %s 档", key)
			continue
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			t.Fatalf("解析 %s 档的上限 %q: %v", key, m[1], err)
		}
		if n < band[0] || n > band[1] {
			t.Errorf("%s 档的标签个数上限 = %d，落在目标区间 %d~%d 之外", key, n, band[0], band[1])
		}
	}

	// 上限必须接到**每一条**图表选项路径上：
	//   - chartFor（建实例时的默认值）
	//   - loadSeries 的 pctOpts / rateOpts（CPU/内存/磁盘/网络）
	//   - latChartOptions（延迟图，取的是延迟卡自己的档位）
	for _, needle := range []string{
		"xLabelMax: RANGE_X_LABEL_MAX[detail.range] || 0,",
		"xLabelMax: xLabelMax,",
		"xLabelMax: RANGE_X_LABEL_MAX[detail.pingRange] || 0,",
	} {
		if !strings.Contains(js, needle) {
			t.Errorf("app.js 缺少 %q：对应的图表拿不到这一档的标签上限", needle)
		}
	}
	if n := strings.Count(js, "RANGE_X_LABEL_MAX["); n < 3 {
		t.Errorf("RANGE_X_LABEL_MAX 只在 %d 处被读到，期望至少 3 处（建实例、资源图选项、延迟图选项）", n)
	}

	// 引擎侧：放得下但比上限还密时，继续往上一级稀疏；默认不设上限（0）——
	// 图表引擎是通用的，别的调用方（流量图）不该被迫接受某个档位的密度。
	step := chartFuncBody(chart, "function xLabelStep(")
	if step == "" {
		t.Fatal("chart.js 的 xLabelStep() 函数体没截取到")
	}
	if !strings.Contains(step, "opts.xLabelMax") {
		t.Error("xLabelStep() 没有读 opts.xLabelMax：标签个数上限形同虚设")
	}
	if !regexp.MustCompile(`fit\.count > maxCount`).MatchString(step) {
		t.Error("放得下但个数超过上限时应当继续往上一级（fit.count > maxCount → continue）")
	}
	if !strings.Contains(chart, "xLabelMax: 0,") {
		t.Error("chart.js 的默认选项里应当有 xLabelMax: 0（不设上限）：引擎是通用的，密度由调用方按档位给")
	}
}

// 改动 B：首页顶部的分组筛选 —— 一排 chip（[全部 5] [香港 3] [未分组 1]）。
//
// 数据是现成的（节点早就有 group_name，在「编辑节点」里填），这里守的是几条
// "坏掉了也照样能看"的接线：
//   - 选项必须从**实际存在的分组**生成（写死一份名单 = 与节点脱节）；
//   - 没填分组的机器要有「未分组」入口（否则一筛选它们就"消失"了）；
//   - 顺序按卡片在首页的显示顺序（拖完排序之后 chip 的顺序也要符合直觉）；
//   - 选择存 localStorage（键名带版本前缀，与延迟图那几个开关同一套做法）；
//   - 只影响卡片区（上面的「在线 3/5」与总览区是全量数字）；
//   - 筛选时禁用拖动排序（子集里的落点写回去会打乱没显示的机器）。
func TestFrontendHomeGroupFilter(t *testing.T) {
	html := readAsset(t, "index.html")
	js := readAsset(t, "app.js")
	css := readAsset(t, "style.css")

	// 1) 容器在总览区之后、节点网格之前：它是"下面这一堆卡片"的控件，
	//    而总览区讲的是整个面板（不受筛选影响），挨在一起会被读成"总览也被筛了"。
	ovAt := strings.Index(html, `id="overview"`)
	filterAt := strings.Index(html, `id="group-filter"`)
	gridAt := strings.Index(html, `id="grid"`)
	if filterAt < 0 {
		t.Fatal(`index.html 里缺少分组筛选的容器 id="group-filter"`)
	}
	if !(ovAt < filterAt && filterAt < gridAt) {
		t.Error("分组筛选应当排在总览区之后、节点网格之前")
	}
	// 容器默认 hidden（还没有节点时不占位置），而且里面**一个 chip 都没有**：
	// 选项全部由 app.js 按实际分组生成。
	if !regexp.MustCompile(`id="group-filter"[^>]*hidden`).MatchString(html) {
		t.Error(`分组筛选的容器应当默认 hidden：还没有节点时首页不该多出一行空控件`)
	}
	if !regexp.MustCompile(`id="group-filter"[^>]*>\s*</div>`).MatchString(html) {
		t.Error("分组筛选的容器里应当是空的：选项由 app.js 从实际存在的分组生成，写死在 HTML 里必然与节点脱节")
	}
	// 可键盘操作：整组有名字，读屏能说清这一排是干什么的。
	if !strings.Contains(html, `aria-label="按分组筛选"`) {
		t.Error("分组筛选那一排应当有 aria-label（读屏用户看不到「这一排是筛什么」）")
	}

	// 2) 选项从实际存在的分组生成：按**卡片在首页的显示顺序**（#grid 的子节点顺序，
	//    也就是拖动排序后的顺序）列出分组，一个分组排在哪里取决于它第一台机器。
	entries := funcBody(js, "function groupEntries(")
	if entries == "" {
		t.Fatal("app.js 缺少 groupEntries()：分组列表没地方算")
	}
	for _, needle := range []string{"el.grid.children", "dataset.nodeId", "group_name", "groupKey(dto"} {
		if !strings.Contains(entries, needle) {
			t.Errorf("groupEntries() 里缺少 %q（分组要么没按卡片顺序、要么没读节点的分组字段）", needle)
		}
	}
	if !regexp.MustCompile(`if \(!counts\.has\(key\)\) \{ counts\.set\(key, 0\); order\.push\(key\); \}`).MatchString(entries) {
		t.Error("分组的顺序应当是「首次出现的顺序」：按名字排序会与卡片顺序对不上（拖完排序之后尤其明显）")
	}
	// 卡片上必须记着自己的节点 id：DOM 里没有 id 的话只能回去翻 nodes，
	// 而那个 Map 的顺序与列表当前顺序未必一致（Map 对已存在的键不改变位置）。
	if !strings.Contains(js, "root.dataset.nodeId = String(dto.id);") {
		t.Error("createCard() 应当在卡片上记下 nodeId（分组顺序是按卡片顺序算的）")
	}

	// 3) 「未分组」入口：分组名是用户随便填的，"未分组"四个字可能真的被当成组名，
	//    所以键用正常输入框打不出来的 NUL；台数写在文案里。
	if !strings.Contains(js, `var GROUP_UNGROUPED = '\u0000';`) {
		t.Error(`app.js 应当有 GROUP_UNGROUPED 哨兵（'\u0000'）：空分组与「全部」不能共用同一个键`)
	}
	key := funcBody(js, "function groupKey(")
	if key == "" || !strings.Contains(key, "GROUP_UNGROUPED") || !strings.Contains(key, ".trim()") {
		t.Error("groupKey() 应当把空/空白的分组名归一成 GROUP_UNGROUPED（服务端也是 trim 后存的）")
	}
	label := funcBody(js, "function groupLabel(")
	if label == "" || !strings.Contains(label, "'未分组'") {
		t.Error("groupLabel() 应当把哨兵显示成「未分组」：没填分组的机器必须有个入口，否则一筛选它们就「消失」了")
	}
	// 台数：文案里直接带台数（"香港 3"），不点进去就知道这一组有几台。
	if !strings.Contains(js, "btn.textContent = item.label + ' ' + item.count;") {
		t.Error("chip 的文案应当是「分组名 + 台数」（香港 3）")
	}
	if !strings.Contains(js, "var items = [{ key: '', label: '全部', count: total }].concat(entries);") {
		t.Error("第一枚 chip 必须是「全部」，并带上总台数（筛选前的基准）")
	}

	// 4) 选择存 localStorage：键名带版本前缀（结构以后变了不会读到对不上的旧值），
	//    只在"点了别的分组"时写（renderGroupFilter 每次重画都会跑）。
	if !strings.Contains(js, "var GROUP_FILTER_KEY = 'probe-group-filter-v1';") {
		t.Error("分组选择应当存在 localStorage 的 probe-group-filter-v1 里（键名带版本前缀）")
	}
	save := funcBody(js, "function saveGroupFilter(")
	if save == "" || !strings.Contains(save, "localStorage.setItem(GROUP_FILTER_KEY") {
		t.Error("saveGroupFilter() 应当把选择写进 localStorage")
	}
	load := funcBody(js, "function loadGroupFilter(")
	if load == "" || !strings.Contains(load, "localStorage.getItem(GROUP_FILTER_KEY)") {
		t.Error("loadGroupFilter() 应当把选择读回来（刷新后停在原来那一组）")
	}
	if !strings.Contains(load, "typeof parsed === 'string'") {
		t.Error("localStorage 里读到的值不是字符串时应当退回「全部」：拿一个坏值去筛会变成「一台机器都没有」")
	}
	// 读回来的时机：在第一帧渲染之前（main 里），否则刷新后会先闪一下「全部」。
	if !strings.Contains(js, "groupFilter = loadGroupFilter();") {
		t.Error("main() 应当在第一帧渲染前把分组选择读回来")
	}

	// 5) 只影响卡片区：筛选落到卡片上是 hidden（不删 DOM），而**汇总条/总览区
	//    一个数字都不许动** —— 它们是整个面板的健康度。
	apply := funcBody(js, "function applyGroupFilter(")
	if apply == "" {
		t.Fatal("app.js 缺少 applyGroupFilter()：筛选结果没落到卡片上")
	}
	if !strings.Contains(apply, "card.root.hidden") {
		t.Error("筛选应当把不匹配的卡片 hidden（删 DOM 的话，每秒一次的 SSE 重画还得把它们建回来）")
	}
	if !strings.Contains(js, "card.root.hidden = !groupMatches(dto);") {
		t.Error("renderNode() 里新画出来的卡片也要立刻守规矩（否则新上线的机器会无视筛选冒出来）")
	}
	filterBlock := js[strings.Index(js, "// ---------------------------------------------------------------- 首页分组筛选"):]
	if end := strings.Index(filterBlock, "function reorderLocked("); end > 0 {
		filterBlock = filterBlock[:end]
	}
	for _, bad := range []string{"renderSummary(", "sumOnline", "el.updated", "renderOverview(", "setOverviewText("} {
		if strings.Contains(filterBlock, bad) {
			t.Errorf("分组筛选里动到了 %q：上面的「在线 X/Y」与总览区必须保持全量（那是整个面板的健康度）", bad)
		}
	}

	// 6) 筛选时禁用拖动排序：把手变灰 + title 说明原因 + 代码一侧也拦一道。
	lock := funcBody(js, "function reorderLocked(")
	if lock == "" {
		t.Fatal("app.js 缺少 reorderLocked()：拖动排序的闸门没有统一出处")
	}
	if !regexp.MustCompile(`return !!groupFilter;`).MatchString(lock) {
		t.Error("reorderLocked() 应当在「按分组筛选」时返回 true")
	}
	row := funcBody(js, "function settingsNodeRow(")
	if row == "" {
		t.Fatal("settingsNodeRow() 的函数体没截取到")
	}
	if !strings.Contains(row, "'node-drag off'") {
		t.Error("被停用的把手应当加 .off 类变灰（一个还能按、按了没反应的把手，用户只会以为拖动坏了）")
	}
	if !regexp.MustCompile(`handle\.title = locked[\s\S]{0,200}全部`).MatchString(row) {
		t.Error("停用时的 title 必须写明原因与出路（切回「全部」再拖）")
	}
	start := funcBody(js, "function startNodeDrag(")
	if start == "" {
		t.Fatal("startNodeDrag() 的函数体没截取到")
	}
	if !strings.Contains(start, "if (reorderLocked()) return;") {
		t.Error("startNodeDrag() 里要有同一道闸门：合成事件、键盘或以后新加的入口都绕不过去")
	}
	if !regexp.MustCompile(`(?s)\.node-drag\.off[^{]*\{[^}]*opacity`).MatchString(css) {
		t.Error("style.css 里 .node-drag.off 应当明显变灰（opacity）")
	}
	if !regexp.MustCompile(`(?s)\.node-drag\.off[^{]*\{[^}]*cursor:\s*not-allowed`).MatchString(css) {
		t.Error("style.css 里 .node-drag.off 应当给出 not-allowed 光标")
	}

	// 7) chip 本身：<button>（可 Tab、回车/空格切换）+ aria-pressed；
	//    容器的 hidden 兜底、窄屏折行、以及被筛掉的卡片真的不占位置。
	if !strings.Contains(js, "btn.setAttribute('aria-pressed'") {
		t.Error("chip 要给出选中状态（aria-pressed）：读屏用户看不到高亮")
	}
	if !strings.Contains(js, "document.createElement('button')") {
		t.Error("chip 应当是 <button>（可键盘操作），而不是 <span> + 手写 keydown")
	}
	if !regexp.MustCompile(`(?s)\.group-filter\s*\{[^}]*display:\s*flex`).MatchString(css) ||
		!regexp.MustCompile(`(?s)\.group-filter\s*\{[^}]*flex-wrap:\s*wrap`).MatchString(css) {
		t.Error("style.css 里 .group-filter 应当是允许折行的 flex（分组多了/窄屏下不能横向溢出）")
	}
	if !regexp.MustCompile(`\.group-filter\[hidden\]\s*\{[^}]*display:\s*none`).MatchString(css) {
		t.Error("style.css 缺少 .group-filter[hidden] { display: none }：没有分组时首页会白占一行")
	}
	// .card 自己写了 display:flex，会盖掉 hidden 那条 display:none ——
	// 少这一条，被筛掉的卡片照样铺在页面上（"点了没反应"）。
	if !regexp.MustCompile(`\.card\[hidden\]\s*\{[^}]*display:\s*none`).MatchString(css) {
		t.Error("style.css 缺少 .card[hidden] { display: none }：被筛掉的卡片不会真的消失")
	}
	// 与内容列同宽：它夹在总览区与网格之间，宽度不一致就会左边缘错开。
	if !regexp.MustCompile(`(?s)\.group-filter\s*\{[^}]*max-width:\s*1600px`).MatchString(css) {
		t.Error("style.css 里 .group-filter 应当与全站内容列同宽（1600px）")
	}
}

// ---------------------------------------------------------------- Y 轴刻度

// Y 轴取整：**对网格线步长取整**，不是对轴顶取整。
//
// 用户报的现象：延迟图上三条曲线都在 200~550ms，Y 轴却是 0/250/500/750/1000
// —— 上面将近一半是空的。根因不在峰值线（v1.0.34 已经彻底不画了），
// 而在旧的 niceCeil(vMax × 1.1)：它只把**轴顶**取整，档位又只有 1/2/2.5/5/10
// （5 与 10 之间整整空着一倍），于是 550×1.1 = 605 只能顶到 1000。
//
// 现在改成"选一个整齐的**步长**，轴顶 = 步长 × 网格线条数（4）"：
// 轴顶与每一格因此同时是整齐的 —— 605 → 步长 200 → 轴顶 800 → 0/200/400/600/800。
//
// 三条硬指标（下面把 vMax 从 0 到 10000 逐点扫一遍钉住）：
//  1. 轴顶 ≥ 峰值（数据永远不会被画出界）；
//  2. 轴顶 ≤ 峰值 × 1.6（"上面别留太多白"的量化上界）；
//  3. 轴顶 ÷ 网格线条数 是整齐数（刻度读得出来，不会出现 173/866）。
//
// 为什么可以在 Go 里镜像一份算法：档位表、网格线条数、余量这三个数都是从
// chart.js **源码里读出来**的（改了代码不改测试是不可能的），而"逐点扫一万个值"
// 放进浏览器要几十秒。镜像与实际实现的一致性由真浏览器用例负责
// （internal/e2e 的 TestYAxisTopMatchesStepRuleInRealBrowser：那边喂的是真的
// ProbeChart，十几个取值逐个与这里的 top() 对比）。
func TestChartYAxisRoundsTheStepNotTheTop(t *testing.T) {
	rule := readYAxisRule(t, readAsset(t, "chart.js"))

	// 档位表本身的两条要求：
	//   - 严格递增（逐级放大靠的就是这个顺序）；
	//   - 相邻两级最多差 1.6/余量 倍 —— 这正是"轴顶 ≤ 峰值×1.6"对档位表的要求：
	//     差得更大，就会有一整段峰值被顶到下一级的轴顶上去（旧实现 5→10 差一倍，
	//     550ms 因此配了 1000 的轴）。
	if len(rule.ladder) < 8 {
		t.Errorf("Y_STEP_LADDER 只有 %d 级：档位太稀，5 与 10 之间那一整段留白会回来", len(rule.ladder))
	}
	if rule.ladder[0] != 1 || rule.ladder[len(rule.ladder)-1] != 10 {
		t.Errorf("Y_STEP_LADDER 必须覆盖一个完整的十进数量级（首级 1、末级 10）：实际 %v…%v，"+
			"少了任何一头都会在某个数量级上直接落回 10×base，轴顶凭空翻十倍",
			rule.ladder[0], rule.ladder[len(rule.ladder)-1])
	}
	prev := 0.0
	for _, rung := range rule.ladder {
		if rung <= prev {
			t.Errorf("Y_STEP_LADDER 必须严格递增：%v 排在 %v 后面", rung, prev)
		}
		if prev > 0 && rung/prev > 1.6/rule.head+1e-9 {
			t.Errorf("Y_STEP_LADDER 里 %v → %v 差了 %.2f 倍（上限 %.2f）：这一段峰值会被顶到下一级，"+
				"轴顶必然超过峰值的 1.6 倍", prev, rung, rung/prev, 1.6/rule.head)
		}
		prev = rung
	}

	// 逐点穷举。maxRatio 顺便记下"最松的那一档"（写进日志，方便以后调档位时对比）。
	const limit = 10000
	maxRatio, maxRatioAt := 0.0, 0
	outOfBounds, tooLoose, notTidy, notInteger, badFallback := 0, 0, 0, 0, 0
	for v := 0; v <= limit; v++ {
		vMax := float64(v)
		top := rule.top(vMax, 0)
		if v == 0 {
			// 兜底：一个数据点都没有（或全是 null/0）时必须返回 1。
			// 返回 0 会让绘图里的 v/top 变成 NaN，整张图一个像素都画不出来。
			if top != 1 {
				badFallback++
			}
			continue
		}
		if math.IsNaN(top) || math.IsInf(top, 0) || top < vMax {
			outOfBounds++
			continue
		}
		if r := top / vMax; r > 1.6 {
			tooLoose++
		} else if r > maxRatio {
			maxRatio, maxRatioAt = r, v
		}
		step := top / rule.lines
		if !rule.isLadderStep(step) {
			notTidy++
		}
		// vMax ≥ 10 时每一格都必须是整数：延迟图的 yFormat 是 toFixed(0)，
		// 步长 12.5 会被印成 "13"（旧实现里 39ms 的轴就是 0/13/25/38/50）。
		if v >= 10 {
			for i := 1; float64(i) <= rule.lines; i++ {
				if math.Abs(step*float64(i)-math.Round(step*float64(i))) > 1e-6 {
					notInteger++
					break
				}
			}
		}
	}
	if badFallback != 0 {
		t.Errorf("vMax = 0 时有 %d 次没有返回 1（兜底坏了会让整张图变成 NaN）", badFallback)
	}
	if outOfBounds != 0 {
		t.Errorf("有 %d 个 vMax 的轴顶 < 峰值或不是有限数：数据会被画出界", outOfBounds)
	}
	if tooLoose != 0 {
		t.Errorf("有 %d 个 vMax 的轴顶超过峰值的 1.6 倍：上面留白太多，正是用户抱怨的那件事", tooLoose)
	}
	if notTidy != 0 {
		t.Errorf("有 %d 个 vMax 的『轴顶 ÷ %.0f』不是整齐步长：刻度会变成 173/866 那种读不出来的数", notTidy, rule.lines)
	}
	if notInteger != 0 {
		t.Errorf("有 %d 个 vMax（≥10）的刻度不是整数：toFixed(0) 会把 12.5 印成 13", notInteger)
	}
	t.Logf("vMax 0…%d 逐点扫过：轴顶全部 ≥ 峰值、全部 ≤ 峰值×1.6（最松的一档 %.3f× @ vMax=%d），"+
		"步长全部落在 Y_STEP_LADDER × 10^k 上，vMax ≥ 10 的刻度全是整数；最松的那一档离 1.6 还有 %.2f 的余量",
		limit, maxRatio, maxRatioAt, 1.6-maxRatio)

	// 用户报的那一例，以及几个容易踩的点：钉死具体数字（"用户看到的轴顶"就是验收标准）。
	cases := []struct {
		name  string
		vMax  float64
		top   float64
		ticks []float64
	}{
		// 550 × 1.1 = 605 → 步长 200 → 轴顶 800。旧规则给的是 1000（见下面那条对比）。
		{"用户截图那一例（峰值 550ms，含余量 605）", 605, 800, []float64{0, 200, 400, 600, 800}},
		{"同一个峰值的另一种量法", 550, 800, []float64{0, 200, 400, 600, 800}},
		{"旧规则在 500~1000 之间没有档位（峰值 550 会被顶到 1000）", 400, 560, []float64{0, 140, 280, 420, 560}},
		{"延迟 fixture 的均值档（39ms）", 39, 56, []float64{0, 14, 28, 42, 56}},
		{"峰值 200ms（旧规则给 250，刻度是 63/125/188）", 200, 240, []float64{0, 60, 120, 180, 240}},
		{"峰值 1000ms（旧规则给 2000）", 1000, 1200, []float64{0, 300, 600, 900, 1200}},
		{"速率 1MB/s：步长 300KB/s", 1000000, 1200000, []float64{0, 300000, 600000, 900000, 1200000}},
		{"日流量 465GB（旧规则给 1000GB）", 465e9, 560e9, []float64{0, 140e9, 280e9, 420e9, 560e9}},
	}
	for _, c := range cases {
		checkYTop(t, rule, c.name, c.vMax, 0, c.top, c.ticks)
	}
	// 旧规则在 605 上给出的就是用户截图里的 1000 —— 这条对比是"这次到底修了什么"的凭据。
	if got := oldNiceCeil(605); got != 1000 {
		t.Errorf("旧规则对 605 给出的轴顶 = %v，期望 1000（用户截图里那个轴）；这条对比用例本身失效了", got)
	}

	// 极端小数据（e2e 里那一组）：不许出现 NaN/0，轴顶仍然要盖住峰值。
	small := rule.top(0.5, 0)
	if math.IsNaN(small) || small <= 0 || small < 0.5 {
		t.Errorf("vMax = 0.5 时轴顶 = %v：要么盖不住峰值，要么是 NaN/0（整张图会画不出来）", small)
	}
	t.Logf("vMax = 0.5 → 轴顶 %.4f、刻度 %v（这样的小量级只出现在「刚好探到一次、还是 0.5ms」这种极端数据上）",
		small, rule.ticks(small))
}

// TestChartYAxisFixedMaxIsUntouched 守住两条**不许被取整规则牵连**的路径：
// 百分比图的硬上限（yMax = 100）与"一个数据点都没有"的兜底（返回 1）。
//
// 为什么单独一条用例：这两条都是"改坏了页面照样能看"的那一类 —— 百分比图被
// 步长取整成 120% 之后，刻度读起来还是挺整齐的，只有把 CPU 图与设置页对着看
// 才发现"利用率怎么永远到不了满格"。
func TestChartYAxisFixedMaxIsUntouched(t *testing.T) {
	chart := readAsset(t, "chart.js")
	rule := readYAxisRule(t, chart)

	// 传了 yMax（百分比图）时原样返回：100 就是 100，不许被步长改成 120。
	checkYTop(t, rule, "CPU/内存/磁盘的硬上限 100%", 96, 100, 100,
		[]float64{0, 25, 50, 75, 100})
	// 数据超过硬上限时轴顶也不动（超出的部分由 draw() 里的 Math.min 夹住）。
	checkYTop(t, rule, "数据超过硬上限", 130, 100, 100, []float64{0, 25, 50, 75, 100})
	// 没有数据：返回 1（不是 0、不是 NaN）。
	for _, v := range []float64{0, -1, -1e9} {
		if got := rule.top(v, 0); got != 1 {
			t.Errorf("vMax = %v 时轴顶 = %v，期望 1：返回 0 会让 v/top 变成 NaN", v, got)
		}
	}

	// 代码形状：yMax 那条分支必须排在 vMax <= 0 那条**前面**（硬上限优先），
	// 而且两条都在自动取整那条前面。
	yTop := chartFuncBody(chart, "function yTop(")
	if yTop == "" {
		t.Fatal("chart.js 的 yTop() 函数体没截取到")
	}
	fixedAt := strings.Index(yTop, "if (opts.yMax > 0) return opts.yMax;")
	emptyAt := strings.Index(yTop, "if (vMax <= 0) return 1;")
	autoAt := strings.Index(yTop, "niceStep(")
	if fixedAt < 0 || emptyAt < 0 || autoAt < 0 {
		t.Fatalf("yTop() 的三个分支不齐（yMax=%d、vMax<=0=%d、自动取整=%d）：\n%s", fixedAt, emptyAt, autoAt, yTop)
	}
	if !(fixedAt < emptyAt && emptyAt < autoAt) {
		t.Error("yTop() 的分支顺序必须是「yMax 硬上限 → vMax<=0 兜底 → 自动取整」：" +
			"把硬上限排到后面，百分比图的 100 就会被步长改掉")
	}
	if !regexp.MustCompile(`return niceStep\(vMax \* Y_HEADROOM / Y_GRID_LINES\) \* Y_GRID_LINES;`).MatchString(yTop) {
		t.Error("自动那条必须是『步长 × 网格线条数』：先乘余量、按步长取整、再乘线条数（顺序错了轴顶就盖不住峰值）")
	}

	// 旧的"按轴顶取整"必须彻底消失：留着它就是两套规则，改一处漏一处。
	if regexp.MustCompile(`function niceCeil\(`).MatchString(codeLines(chart)) {
		t.Error("chart.js 里还留着 niceCeil()（按轴顶取整的旧规则）：它正是 550ms 配 1000 轴顶的来源")
	}

	// 取整规则本身：第一个不小于 norm 的档位（取最小 = 别多留白；不小于 = 别画不下）。
	step := chartFuncBody(chart, "function niceStep(")
	if step == "" {
		t.Fatal("chart.js 的 niceStep() 函数体没截取到")
	}
	if !regexp.MustCompile(`if \(norm <= Y_STEP_LADDER\[i\]\) return Y_STEP_LADDER\[i\] \* base;`).MatchString(step) {
		t.Error("niceStep() 必须取『第一个不小于 target 的档位』：取最近的一级有时会小于峰值，曲线就被画出界了")
	}
	if !regexp.MustCompile(`if \(!\(target > 0\)\) return 1;`).MatchString(step) {
		t.Error("niceStep() 自己也要挡住 0/负数/NaN：Math.log10(0) 是 -Infinity，后面全是 NaN")
	}

	// draw() 里的刻度必须与 yTop 出自**同一个步长**：step = 轴顶 / 线条数。
	draw := codeLines(chartFuncBody(chart, "function draw()"))
	if draw == "" {
		t.Fatal("chart.js 的 draw() 函数体没截取到")
	}
	if !regexp.MustCompile(`var step = top / Y_GRID_LINES;`).MatchString(draw) {
		t.Error("draw() 应当用『轴顶 ÷ 网格线条数』得到步长（自己再调一次 niceStep 就是把取整规则写第二遍）")
	}
	if !regexp.MustCompile(`var value = step \* i;`).MatchString(draw) {
		t.Error("刻度应当是步长的整数倍（step × i），不是 top × i / lines：后者在浮点下会多出 4.199999999999999 这种尾巴")
	}
	if regexp.MustCompile(`var lines = \d`).MatchString(draw) {
		t.Error("draw() 里又出现了一份写死的网格线条数：它必须只有 Y_GRID_LINES 一处（两份迟早对不上）")
	}
	if !strings.Contains(draw, "Y_GRID_LINES") {
		t.Error("draw() 应当用 Y_GRID_LINES 画网格线（轴顶与刻度的分母是同一个数）")
	}

	// 注释：为什么按步长取整而不是按轴顶 —— 这是下一个改这里的人最先会问的问题。
	for _, word := range []string{"步长", "轴顶", "1.4", "1.6"} {
		if !strings.Contains(chart, word) {
			t.Errorf("注释里缺少 %q：按步长取整的理由（以及 1.4 那一级与 1.6 倍上界）必须写在常量旁边", word)
		}
	}
}

// TestChartYAxisNeverWorseThanBefore 是这次改动的**回归闸门**：
// 新的取整规则不许在任何一段把轴顶搞得比旧规则更高（"别把一个本来正好的档位
// 搞成留白更多"）。旧规则（对轴顶取整）只在这条用例里作为对比基准存在。
func TestChartYAxisNeverWorseThanBefore(t *testing.T) {
	rule := readYAxisRule(t, readAsset(t, "chart.js"))

	const limit = 10000
	tighter, same, taller, worstAt := 0, 0, 0, 0
	worst := 1.0
	var bands [][3]int // 变高的连续区间：[起, 止, 旧轴顶]
	open := false
	start, oldTopAt := 0, 0.0
	oldSum, newSum := 0.0, 0.0
	for v := 1; v <= limit; v++ {
		vMax := float64(v)
		oldTop := oldNiceCeil(vMax * rule.head)
		newTop := rule.top(vMax, 0)
		oldSum += oldTop / vMax
		newSum += newTop / vMax
		switch {
		case newTop < oldTop:
			tighter++
		case newTop == oldTop:
			same++
		default:
			taller++
			if r := newTop / oldTop; r > worst {
				worst, worstAt = r, v
			}
			if !open {
				open, start, oldTopAt = true, v, oldTop
			}
			continue
		}
		if open {
			bands = append(bands, [3]int{start, v - 1, int(oldTopAt)})
			open = false
		}
	}
	if open {
		bands = append(bands, [3]int{start, limit, int(oldTopAt)})
	}

	// 最坏的一例只许高 25%（实测 1.12×：旧的 5×10^k 那一档被 5.6×10^k 取代）。
	// 这条上界就是"没有哪一档变得更差"的量化说法；真要再放宽，得先说清为什么。
	if worst > 1.25 {
		t.Errorf("新规则的轴顶比旧规则最高高出 %.3f×（vMax=%d：旧 %v → 新 %v）：这已经是"+
			"「把本来正好的档位搞成留白更多」了，不能接受（上限 1.25×）",
			worst, worstAt, oldNiceCeil(float64(worstAt)*rule.head), rule.top(float64(worstAt), 0))
	}
	// 整体只许更紧，不许更空。
	if newSum >= oldSum {
		t.Errorf("逐点平均留白 = %.4f×（旧规则 %.4f×）：这次改动应当整体更紧", newSum/limit, oldSum/limit)
	}
	t.Logf("与旧规则（对轴顶取整）逐点对比 vMax 1…%d：更紧 %d 个、不变 %d 个、更高 %d 个（最高 %.3f× @ vMax=%d）；"+
		"平均留白 %.4f× → %.4f×", limit, tighter, same, taller, worst, worstAt, oldSum/limit, newSum/limit)
	for _, b := range bands {
		t.Logf("  更高的那几段：vMax %d…%d（旧轴顶 %.0f → 新轴顶 %.0f，%.2f×）",
			b[0], b[1], oldNiceCeil(float64(b[0])*rule.head), rule.top(float64(b[0]), 0),
			rule.top(float64(b[0]), 0)/oldNiceCeil(float64(b[0])*rule.head))
	}
	// 变高的必须是少数：多数取值要么更紧、要么持平（实测 6465 紧 / 2424 平 / 1111 高）。
	if taller > limit/5 {
		t.Errorf("有 %d/%d 个取值的轴顶比旧规则更高（超过五分之一）：这不是「个别窄带」，是规则整体变松了", taller, limit)
	}
}

// yAxisRule 是从 chart.js 源码里读出来的那套 Y 轴取整规则。
type yAxisRule struct {
	ladder []float64 // 步长档位（乘 10 的整数次幂之后使用）
	lines  float64   // 网格线间隔数：轴顶 = 步长 × 它
	head   float64   // 轴顶相对峰值要留的余量
}

// readYAxisRule 读三个常量。不抄一份到测试里：抄了的话"改了代码没改测试"
// 就变成自己跟自己比（X_LABEL_MIN_GAP / X_STEP_LADDER 那两条也是从源码读的）。
func readYAxisRule(t *testing.T, chart string) yAxisRule {
	t.Helper()
	var rule yAxisRule

	m := regexp.MustCompile(`var Y_STEP_LADDER = \[([^\]]*)\];`).FindStringSubmatch(codeLines(chart))
	if m == nil {
		t.Fatal("chart.js 里找不到 Y_STEP_LADDER：按步长取整的档位表是这次改动的核心")
	}
	for _, raw := range strings.Split(m[1], ",") {
		v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
		if err != nil {
			t.Fatalf("Y_STEP_LADDER 里解析不出数字：%q", raw)
		}
		rule.ladder = append(rule.ladder, v)
	}

	lines := regexp.MustCompile(`var Y_GRID_LINES = (\d+);`).FindStringSubmatch(codeLines(chart))
	if lines == nil {
		t.Fatal("chart.js 里找不到 Y_GRID_LINES：轴顶要除以它才得到刻度")
	}
	n, err := strconv.Atoi(lines[1])
	if err != nil || n < 2 {
		t.Fatalf("Y_GRID_LINES = %q 不合法（至少 2 段才谈得上刻度）", lines[1])
	}
	rule.lines = float64(n)

	head := regexp.MustCompile(`var Y_HEADROOM = ([\d.]+);`).FindStringSubmatch(codeLines(chart))
	if head == nil {
		t.Fatal("chart.js 里找不到 Y_HEADROOM：轴顶相对峰值的余量")
	}
	h, err := strconv.ParseFloat(head[1], 64)
	if err != nil || !(h > 1) {
		t.Fatalf("Y_HEADROOM = %q：它必须大于 1（等于 1 时最尖的那一点会贴着顶网格线画）", head[1])
	}
	rule.head = h
	return rule
}

// niceStep 与 chart.js 的 niceStep 同一套：取"不小于 target 的最小整齐步长"。
func (r yAxisRule) niceStep(target float64) float64 {
	if !(target > 0) {
		return 1
	}
	base := math.Pow(10, math.Floor(math.Log10(target)))
	norm := target / base
	for _, rung := range r.ladder {
		if norm <= rung {
			return rung * base
		}
	}
	return 10 * base
}

// top 与 chart.js 的 yTop 同一套（fixedMax 就是 opts.yMax，0 表示没传）。
func (r yAxisRule) top(vMax, fixedMax float64) float64 {
	if fixedMax > 0 {
		return fixedMax
	}
	if vMax <= 0 {
		return 1
	}
	return r.niceStep(vMax*r.head/r.lines) * r.lines
}

// ticks 是轴顶对应的那几个刻度（0、step、2step…轴顶）。
func (r yAxisRule) ticks(top float64) []float64 {
	step := top / r.lines
	out := make([]float64, 0, int(r.lines)+1)
	for i := 0; float64(i) <= r.lines; i++ {
		out = append(out, step*float64(i))
	}
	return out
}

// isLadderStep 判断一个步长是不是"档位 × 10 的整数次幂"（整齐数的定义）。
func (r yAxisRule) isLadderStep(step float64) bool {
	if !(step > 0) {
		return false
	}
	for k := -6; k <= 9; k++ {
		base := math.Pow(10, float64(k))
		for _, rung := range r.ladder {
			if math.Abs(rung*base-step) <= 1e-9*step {
				return true
			}
		}
	}
	return false
}

// oldNiceCeil 是 v1.0.34 的规则：对**轴顶**取整（档位只有 1/2/2.5/5/10）。
//
// 它只作为"改前"的对比基准留在测试里，产品代码里已经没有这套规则了
// （见 TestChartYAxisFixedMaxIsUntouched 里"不许再有 niceCeil"那条断言）。
func oldNiceCeil(value float64) float64 {
	if !(value > 0) {
		return 1
	}
	base := math.Pow(10, math.Floor(math.Log10(value)))
	norm := value / base
	step := 10.0
	switch {
	case norm <= 1:
		step = 1
	case norm <= 2:
		step = 2
	case norm <= 2.5:
		step = 2.5
	case norm <= 5:
		step = 5
	}
	return step * base
}

// checkYTop 核对一个取值：轴顶与每一个刻度都要对得上（浮点比较留 1e-9 的相对余量）。
func checkYTop(t *testing.T, rule yAxisRule, name string, vMax, fixedMax, wantTop float64, wantTicks []float64) {
	t.Helper()
	gotTop := rule.top(vMax, fixedMax)
	if math.Abs(gotTop-wantTop) > 1e-9*wantTop {
		t.Errorf("%s：vMax=%v（yMax=%v）轴顶 = %v，期望 %v", name, vMax, fixedMax, gotTop, wantTop)
		return
	}
	gotTicks := rule.ticks(gotTop)
	if len(gotTicks) != len(wantTicks) {
		t.Errorf("%s：刻度 %v 的个数与期望 %v 不一致", name, gotTicks, wantTicks)
		return
	}
	for i := range wantTicks {
		if math.Abs(gotTicks[i]-wantTicks[i]) > 1e-9*wantTicks[i] {
			t.Errorf("%s：刻度 = %v，期望 %v", name, gotTicks, wantTicks)
			return
		}
	}
	t.Logf("%s：vMax=%v → 轴顶 %v，刻度 %v", name, vMax, gotTop, gotTicks)
}

// ---------------------------------------------------------------- 汇率与人民币口径

// currencyOptionsFromHTML 抓出货币下拉里的选项值（含空值选项）。
func currencyOptionsFromHTML(t *testing.T, html string) []string {
	t.Helper()
	node := dialogBody(t, html, "dlg-node")
	if node == "" {
		t.Fatal(`index.html 里找不到 <dialog id="dlg-node">`)
	}
	start := strings.Index(node, `id="node-currency"`)
	if start < 0 {
		t.Fatal(`「新增/编辑节点」对话框里缺少 #node-currency`)
	}
	rest := node[start:]
	// 只看到这个字段的 </select> 为止：后面的控件不该被算进来。
	if end := strings.Index(rest, "</select>"); end >= 0 {
		rest = rest[:end]
	}
	var out []string
	for _, match := range regexp.MustCompile(`<option value="([^"]*)"`).FindAllStringSubmatch(rest, -1) {
		out = append(out, match[1])
	}
	return out
}

// 货币字段必须是**下拉**，选项覆盖常用币种，并且保留"不填"。
//
// 为什么必须是下拉而不是文本框：文本框里任何字符串都能提交（"usd "、"美元"、"US"），
// 而汇率表是按 ISO 代码查的 —— 拼错一个字母，换算就静默地不生效（退回原值），
// 页面上完全看不出哪里不对。下拉把"我们认不认识这个币种"提前到输入的那一刻。
func TestFrontendCurrencyIsSelectWithCommonCurrencies(t *testing.T) {
	html := readAsset(t, "index.html")

	if strings.Contains(html, `<input id="node-currency"`) {
		t.Error("货币字段还是文本框：拼错的币种会静默地换算不了")
	}
	options := currencyOptionsFromHTML(t, html)
	if len(options) < 10 {
		t.Fatalf("货币下拉只有 %d 个选项：%v", len(options), options)
	}

	// 币种是**可选**的（没填就是人民币口径），所以必须有一个空值选项。
	if options[0] != "" {
		t.Errorf("货币下拉的第一个选项应当是空值（不填），实际 %q", options[0])
	}
	have := map[string]bool{}
	for _, opt := range options {
		have[opt] = true
	}
	for _, code := range []string{"CNY", "USD", "EUR", "GBP", "JPY", "HKD", "SGD", "AUD", "CAD", "KRW", "TWD", "MYR", "THB", "CHF"} {
		if !have[code] {
			t.Errorf("货币下拉里缺少常用币种 %s", code)
		}
	}
	// 选项值必须是**纯代码**：带空格/小写/中文的值会被原样发到服务端，
	// 而 store 层的校验只认大写字母。
	for _, opt := range options {
		if opt == "" {
			continue
		}
		if opt != strings.ToUpper(opt) || strings.TrimSpace(opt) != opt {
			t.Errorf("币种选项值 %q 不是规范的大写代码（服务端会拒收）", opt)
		}
	}
}

// 老数据里"下拉里没有的币种"必须能显示、能原样保存 —— **绝不**静默改成 CNY。
//
// 这是这一改动里最容易造成数据损坏的一处：用户只是打开编辑框看一眼再点保存，
// 库里的 USD/XYZ 要是被界面改成了 CNY，他不会收到任何提示，账单口径却已经变了。
func TestFrontendKeepsUnknownCurrencyInsteadOfRewritingToCNY(t *testing.T) {
	js := readAsset(t, "app.js")

	if !regexp.MustCompile(`function setCurrencyValue\(`).MatchString(js) {
		t.Fatal("app.js 缺少 setCurrencyValue()：老币种没有地方安放，只能被静默改写")
	}
	body := funcBody(js, "function setCurrencyValue(")
	if body == "" {
		t.Fatal("setCurrencyValue() 的函数体没截取到")
	}
	// 回填时先清掉上一次临时插进来的选项：不清的话每打开一次编辑框就多一条。
	if !strings.Contains(body, "option[data-legacy]") {
		t.Error("setCurrencyValue() 应当先清掉上一次为老币种临时补的选项（否则会一条条攒起来）")
	}
	// 列表里没有的币种要**临时补一个选项**，而不是换成别的值。
	if !strings.Contains(body, "createElement('option')") {
		t.Error("setCurrencyValue() 应当为下拉里没有的币种临时补一个选项")
	}
	if !strings.Contains(body, "select.value = want") {
		t.Error("setCurrencyValue() 最后必须把下拉选中到那个币种本身")
	}
	// 这一条是硬规则：回填路径里不许出现任何"兜底成某个币种"的写法。
	for _, bad := range []string{"'CNY'", `"CNY"`, "USD"} {
		if strings.Contains(body, bad) {
			t.Errorf("setCurrencyValue() 里出现了字面币种 %s：老数据可能被静默改写", bad)
		}
	}

	// 打开的路径：把 dto.currency 原样交给 setCurrencyValue。
	if !strings.Contains(js, "setCurrencyValue(el.nodeCurrency, d.currency || '')") {
		t.Error("openNodeDialog() 应当把节点当前的币种（原样）交给 setCurrencyValue()")
	}
	// 保存的路径：下拉的 value 原样发回去（含临时补出来的老币种）。
	if !strings.Contains(js, "currency: el.nodeCurrency.value.trim().toUpperCase()") {
		t.Error("请求体里的 currency 应当来自下拉当前值（老币种原样发回去）")
	}
	// 编辑节点的对话框里不许出现"货币为空就填 CNY"这类兜底。
	open := funcBody(js, "function openNodeDialog(")
	if open == "" {
		t.Fatal("openNodeDialog() 的函数体没截取到")
	}
	for _, bad := range []string{"'CNY'", `"CNY"`} {
		if strings.Contains(open, bad) {
			t.Errorf("openNodeDialog() 里出现了字面币种 %s：打开编辑框就可能改坏用户的币种", bad)
		}
	}
}

// 前端**不许**出现任何汇率数据源的地址（硬规则）。
//
// 汇率只能在服务端取：浏览器直连第三方会被 CORS/隐私策略/内网环境各种情况打穿，
// 而且"这一份汇率是哪来的"必须由服务端统一落库、统一下发（见 fx.go）。
// 这里连"只有主机名、没有协议头"的写法也一起挡：那同样是外部依赖。
func TestFrontendHasNoFXProviderURLs(t *testing.T) {
	for _, name := range []string{"index.html", "style.css", "app.js", "chart.js"} {
		content := readAsset(t, name)
		for _, bad := range []string{"frankfurter", "er-api", "exchangerate"} {
			if strings.Contains(strings.ToLower(content), bad) {
				t.Errorf("%s 里出现了汇率数据源 %q：前端永远不碰外部 URL（只有服务端能取汇率）", name, bad)
			}
		}
	}
	// 顺带把"人民币口径是服务端算好的"钉住：前端不许自己乘除汇率。
	js := readAsset(t, "app.js")
	if regexp.MustCompile(`cny[^\n]*(\*|/)\s*[a-zA-Z]`).MatchString(js) {
		t.Error("app.js 里在自己算汇率：人民币口径必须由服务端下发（前端不做算术）")
	}
}

// 三个显示位置（首页卡片的「费用」行、详情页顶部三格、设置页服务器列表的剩余价值）
// 必须走**同一个**双币口径函数，口径不一致的话同一台机器在三处长的不一样。
func TestFrontendMoneyShowsCNYEverywhere(t *testing.T) {
	js := readAsset(t, "app.js")

	if !regexp.MustCompile(`function moneyBothText\(`).MatchString(js) {
		t.Fatal("app.js 缺少 moneyBothText()：三处金额会各写一套拼接逻辑")
	}
	helper := funcBody(js, "function moneyBothText(")
	// 什么时候不接人民币那一段：靠服务端的 cny_converted（真的换算过没有），
	// 而不是"数字不一样就显示"——币种是 CNY 时会把同一个金额写两遍。
	if !strings.Contains(helper, "if (!converted) return text;") {
		t.Error("moneyBothText() 必须在没有真换算时只显示原币种（币种是 CNY、或没有可用汇率）")
	}
	// 人民币那一段写成「· ¥500.00」：符号已经说明是人民币，不再重复一个 CNY
	// （这一行在卡片上只有一格宽）。
	if !strings.Contains(helper, "' · ¥'") || !strings.Contains(helper, "fmtAmount(cnyCents)") {
		t.Error("moneyBothText() 应当把人民币口径写成「 · ¥金额」（不重复币种代码）")
	}
	if !regexp.MustCompile(`function nodeMoney\(`).MatchString(js) {
		t.Fatal("app.js 缺少 nodeMoney()：每个显示位置都要自己写一遍字段名配对")
	}

	// 三处调用点。字段名成对出现，缺一个就是某一处没有人民币口径。
	places := []struct {
		name   string
		marker string
		want   []string
	}{
		{"首页卡片", "function updateCard(", []string{
			"nodeMoney(dto, 'price_cents', 'price_cny_cents')",
		}},
		{"详情页汇总", "function renderDetailStats(", []string{
			"nodeMoney(node, 'price_cents', 'price_cny_cents')",
			"nodeMoney(node, 'monthly_cents', 'monthly_cny_cents')",
			"nodeMoney(node, 'remaining_value_cents', 'remaining_value_cny_cents')",
		}},
		{"服务器列表", "function settingsNodeRow(", []string{
			"nodeMoney(node, 'remaining_value_cents', 'remaining_value_cny_cents')",
		}},
	}
	for _, place := range places {
		body := funcBody(js, place.marker)
		if body == "" {
			t.Fatalf("%s：截不到 %s 的函数体", place.name, place.marker)
		}
		for _, want := range place.want {
			if !strings.Contains(body, want) {
				t.Errorf("%s里没有 %s（这一处的金额会只有原币种，与其它两处口径不一致）", place.name, want)
			}
		}
	}
}

// 汇率元信息必须显示在设置页的「服务器信息」里：**哪天的、从哪来的、是不是兜底**。
//
// 价格上凭空多出来的那个 ¥ 数字如果不写清出处，没人敢拿它对账；而"兜底"两个字
// 更重要 —— 内置兜底表是写死在程序里的量级估计，不是实时值。
func TestFrontendSettingsShowsFXMeta(t *testing.T) {
	js := readAsset(t, "app.js")

	if !regexp.MustCompile(`function fxRows\(`).MatchString(js) {
		t.Fatal("app.js 缺少 fxRows()：设置页看不到汇率的元信息")
	}
	// 这几行必须拼进「服务器信息」那张卡（serverInfo），不是别的什么地方。
	if !strings.Contains(js, "].concat(fxRows(all.fx))") {
		t.Error("「服务器信息」的 rows 里没有并入 fxRows(all.fx)")
	}
	body := funcBody(js, "function fxRows(")
	if body == "" {
		t.Fatal("fxRows() 的函数体没截取到")
	}
	for _, needle := range []string{"汇率数据", "汇率日期", "汇率来源"} {
		if !strings.Contains(body, needle) {
			t.Errorf("汇率那几行里缺少「%s」", needle)
		}
	}
	// 三个状态各说各的话：关了 / 兜底 / 正常。兜底必须写明"不是实时值"，
	// 否则用户会拿一个静态估计去比价。
	for _, needle := range []string{"fx.enabled === false", "fx.is_default", "兜底"} {
		if !strings.Contains(body, needle) {
			t.Errorf("fxRows() 里缺少 %q（关掉/兜底/正常三种状态必须区分得开）", needle)
		}
	}
	if !strings.Contains(body, "fx.date") || !strings.Contains(body, "fx.source") {
		t.Error("fxRows() 没有用服务端下发的 date/source：用户看不到「哪天的、从哪来的」")
	}
	// 取得时间只在真的取到过时才显示（兜底表的 fetched_at 是 0）。
	if !strings.Contains(body, "fx.fetched_at > 0") {
		t.Error("fxRows() 应当只在真取到过时显示取得时间（兜底表没有这个时间）")
	}
}
