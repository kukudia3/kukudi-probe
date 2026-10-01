package server

import (
	"io/fs"
	"regexp"
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
		`id="info-network"`, `id="info-traffic"`, `id="charts-resources"`, `id="charts-latency"`,
		`id="stat-price"`, `id="stat-monthly"`, `id="stat-left"`, `id="stat-value"`,
		`id="detail-ranges"`, `id="chart-cpu"`, `id="chart-mem"`,
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
		// 服务器列表 + 编辑标签（Phase 16）：一行一台机器，行与徽章都由 app.js 造，
		// HTML 里只有容器、按钮与对话框骨架。
		`id="nodes-list"`, `id="nodes-add"`, `id="nodes-empty"`, `id="nodes-error"`,
		`id="dlg-tags"`, `id="tags-node"`, `id="tags-editor"`, `id="tags-input"`,
		`id="tags-hint"`, `id="tags-error"`, `id="tags-existing-wrap"`, `id="tags-existing"`,
		`id="tags-cancel"`, `id="tags-save"`,
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
	// 延迟格子按**后端给的慢阈值**分级，倍数只有一个（2×）；"≤1.2× 该目标均值"
	// 那一套是「探测」那一行的口径，两者故意不合并（见下一条用例）。
	for _, needle := range []string{
		"var MINI_LOSS_WARN_PCT = 5;",
		"var MINI_LAT_BAD_RATIO = 2;",
		"var PROBE_LAT_WARN_RATIO = 1.2;",
		"var PROBE_LAT_BAD_RATIO = 2;",
	} {
		if !strings.Contains(js, needle) {
			t.Errorf("app.js 缺少配色阈值 %q", needle)
		}
	}
	// 没有数据的那一段：不加颜色类（留浅灰底），绝不画成 0。
	// 三个分级函数（丢包格子、延迟格子、探测行）各有一条同样的兜底。
	if n := strings.Count(js, "if (typeof value !== 'number') return '';"); n != 3 {
		t.Errorf("三个分级函数都应当把 null 判成「没有数据」，实际找到 %d 处", n)
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
	// 与迷你条**故意不共用**（迷你条比的是后端的慢阈值，见
	// TestFrontendSlowMarkingIsBackendDriven），这里比的是"它自己平时多快"。
	probe := funcBody(js, "function renderProbeLine(")
	if probe == "" {
		t.Fatal("app.js 缺少 renderProbeLine()：「探测」那一行没画")
	}
	for _, needle := range []string{"mini.targets", "probeLatClass(t.lat_ms, t.avg_ms)", "miniLatText(t.lat_ms)"} {
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

// 节点标签：设置页的「服务器列表」一栏 + 编辑标签对话框 + 首页卡片上的标签行。
//
// 三处**必须共用同一个取色函数**：标签颜色唯一的用处就是"一眼认出这是哪个标签"，
// 首页与设置页各取各的颜色等于没有颜色。同理，颜色必须由标签文字哈希决定 ——
// 用随机数或"第几个标签"的话，两次加载之间同一个标签就会换色。
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

	// 一行一台机器：整行一个圆角浅边框，行里三段（头部 / 信息 / 标签）都由 app.js 造。
	if !regexp.MustCompile(`function settingsNodeRow\(`).MatchString(js) {
		t.Fatal("app.js 缺少 settingsNodeRow()：一行一台机器")
	}
	row := funcBody(js, "function settingsNodeRow(")
	if row == "" {
		t.Fatal("settingsNodeRow() 的函数体没截取到")
	}
	for _, needle := range []string{"node-item-head", "node-item-meta", "node-item-tags", "rowButton('编辑标签'", "rowButton('编辑节点'"} {
		if !strings.Contains(row, needle) {
			t.Errorf("服务器列表的一行里缺少 %q", needle)
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

	// 编辑标签对话框：骨架、回车添加、× 删除、上限禁用输入框、候选徽章。
	for _, id := range []string{
		"dlg-tags", "tags-node", "tags-editor", "tags-input", "tags-hint",
		"tags-error", "tags-existing-wrap", "tags-existing", "tags-cancel", "tags-save",
	} {
		if !strings.Contains(html, `id="`+id+`"`) {
			t.Errorf("编辑标签对话框缺少 id=%s", id)
		}
	}
	for _, needle := range []string{
		"function openTagDialog(", "function addTag(", "function removeTag(",
		"function renderTagEditor(", "function renderTagSuggestions(", "function syncTagEditor(",
		"function tagFormPayload(", "function saveTags(",
		"el.tagsInput.disabled = full;",
		"var TAG_MAX_COUNT = 8;", "var TAG_MAX_LEN = 16;",
		"el.tagsEditor.insertBefore(chip, el.tagsInput);",
	} {
		if !strings.Contains(js, needle) {
			t.Errorf("app.js 缺少 %q", needle)
		}
	}
	if !regexp.MustCompile(`addEventListener\('keydown'`).MatchString(js) {
		t.Error("标签输入框没有绑定 keydown：回车加不进去")
	}
	if !strings.Contains(js, "el.tagsExistingWrap.hidden = list.length === 0;") {
		t.Error("「已有的标签」一个候选都没有时应当整块隐藏（留着标题会让人以为徽章没渲染出来）")
	}

	// 保存：PUT 到节点接口，且**带上完整字段**（整体替换语义，只发 tags 会被冲掉别的字段）。
	save := funcBody(js, "function saveTags(")
	if save == "" {
		t.Fatal("app.js 缺少 saveTags()")
	}
	if !strings.Contains(save, "api('/api/v1/nodes/' + tagNode.id, { method: 'PUT', body: tagFormPayload() })") {
		t.Error("saveTags() 里没有 PUT /api/v1/nodes/<id>")
	}
	payload := funcBody(js, "function tagFormPayload(")
	if payload == "" {
		t.Fatal("app.js 缺少 tagFormPayload()")
	}
	for _, field := range []string{
		"name:", "group_name:", "region:", "note:", "interval_sec:", "traffic_limit:",
		"traffic_warn_pct:", "reset_day:", "expires_at:", "price_cents:", "currency:",
		"billing_months:", "enabled:", "tags:",
	} {
		if !strings.Contains(payload, field) {
			t.Errorf("保存标签的请求体缺少字段 %q：会把那个字段冲成默认值", field)
		}
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

	// 颜色：由标签文字哈希决定，三处共用同一个 tagChip()。
	if !regexp.MustCompile(`function tagHash\(`).MatchString(js) {
		t.Fatal("app.js 缺少 tagHash()：标签颜色必须由文字哈希决定")
	}
	if !regexp.MustCompile(`h \* 31 \+ text\.charCodeAt\(i\)`).MatchString(js) {
		t.Error("tagHash() 应当是稳定的字符哈希（乘 31 累加）")
	}
	if !regexp.MustCompile(`function tagClass\(`).MatchString(js) {
		t.Fatal("app.js 缺少 tagClass()")
	}
	if strings.Contains(js, "Math.random") {
		t.Error("标签颜色不能用随机数：同一个标签两次加载会换色")
	}
	// 定义处 1 次 + 首页卡片 1 次 + 服务器列表 1 次 + 编辑器徽章 1 次。
	if n := strings.Count(js, "tagChip("); n < 4 {
		t.Errorf("tagChip() 只被用了 %d 次：首页、服务器列表、编辑标签三处必须共用同一个取色", n)
	}

	// CSS：色板、徽章、折行、hidden 兜底。
	for _, rule := range []string{
		".tag {", ".tag-c0", ".tag-c9", ".card-tags", ".node-item", ".node-item-head",
		".node-item-meta", ".node-item-region", ".tag-editor", ".tag-x", ".tag-suggest",
	} {
		if !strings.Contains(css, rule) {
			t.Errorf("style.css 缺少 %s 规则", rule)
		}
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
	// 深色主题：色板只**定义**一次（明暗共用同一组"浅底 + 深字"），切主题时颜色不跳。
	// 数的是定义（带冒号），不是 .tag-cN 里的 var(...) 引用。
	if n := strings.Count(css, "--tag-0-bg:"); n != 1 {
		t.Errorf("标签色板应当只定义一次（两套主题共用同一组值），实际定义 %d 次", n)
	}
}

// 延迟图的图例要显示**整段平均延迟**，值由后端给（avg_ms），前端不做算术。
// 丢包为 0 时省略丢包后缀（探针绝大多数时间不丢包，全标一句"丢包 0%"会把
// 真正丢包的那个目标淹掉）；延迟没有有效样本（avg_ms = 0，整段全丢）时同理。
func TestFrontendLatencyLegendShowsAverage(t *testing.T) {
	js := readAsset(t, "app.js")

	body := funcBody(js, "function latTargetText(")
	if body == "" {
		t.Fatal("app.js 缺少 latTargetText()")
	}
	if !strings.Contains(body, "t.avg_ms") {
		t.Error("图例应当显示后端给的 avg_ms（前端自己平均分桶点会把加权规则再实现一遍）")
	}
	if !regexp.MustCompile(`if \(t\.avg_ms > 0\)`).MatchString(body) {
		t.Error("没有有效延迟样本（avg_ms = 0）时应当省略延迟部分")
	}
	if !strings.Contains(body, "' · 丢包 '") || !regexp.MustCompile(`if \(t\.loss_pct > 0\)`).MatchString(body) {
		t.Error("丢包为 0 时应当省略丢包后缀")
	}
}

// 「慢」（超过阈值的那一段）由**后端**判定，前端只做两件事：
// 按 threshold_ms 把超出的那一段画红、把 slow_pct 写进图例。
//
// 为什么钉得这么细：这三样东西任何一处接错线，页面**照样能看**（曲线照画、
// 图例照显示、竖条照画），只是红线不见了或者图例少一段 —— 除了盯着屏幕看，
// 没有别的线索。这里逐条守住：
//   - 阈值/占比来自 /ping 的 threshold_ms / slow_pct（前端一个都不自己算）；
//   - 迷你条的延迟格子用**同一个** threshold_ms 分级，不再按均值倍数；
//   - 「探测」那一行仍是均值口径（那是另一个问题，故意不合并）；
//   - 图例里「· 慢 X%」是红的且与图上红线同色；slow_pct 为 0 时整段不显示；
//   - 图表引擎逐段着色：孤立的单点画成红点，且**不跨过正常区间**连线。
func TestFrontendSlowMarkingIsBackendDriven(t *testing.T) {
	js := readAsset(t, "app.js")
	chart := readAsset(t, "chart.js")
	css := readAsset(t, "style.css")

	// 1) 阈值来自后端字段，且只拿它比大小。
	ping := funcBody(js, "function loadPingChart()")
	if ping == "" {
		t.Fatal("app.js 缺少 loadPingChart()")
	}
	if !strings.Contains(ping, "slow: { valueIndex: 1, threshold: t.threshold_ms, color: SLOW_COLOR }") {
		t.Error("延迟图的 series 应当带 slow（valueIndex=1 是画曲线用的 avg，阈值取后端的 threshold_ms）")
	}
	// 与 bars 同样的坑：setChart 少透传一个字段，红线就静默画不出来。
	if !strings.Contains(js, "slow: s.slow") {
		t.Error("setChart() 必须原样透传 slow（漏掉的话红线静默消失）")
	}

	// 2) 图例：slow_pct > 0 才显示「· 慢 X%」，而且那一段必须是红的。
	slow := funcBody(js, "function latSlowText(")
	if slow == "" {
		t.Fatal("app.js 缺少 latSlowText()：图例里的「· 慢 X%」没写")
	}
	if !strings.Contains(slow, "' · 慢 '") {
		t.Error("图例里缺少「· 慢 X%」这个后缀")
	}
	if !regexp.MustCompile(`if \(!\(t\.slow_pct > 0\)\) return '';`).MatchString(slow) {
		t.Error("slow_pct 为 0 时不该显示「· 慢 X%」（否则每个目标都挂一句「慢 0%」）")
	}
	if !strings.Contains(slow, "t.slow_pct") {
		t.Error("慢占比必须用后端给的 slow_pct（前端自己数一遍就是把判定规则再实现一次）")
	}
	// 主段（名称 · 平均 · 丢包）里**不含**慢那一段：它单独成元素才可能是红的。
	if main := funcBody(js, "function latTargetText("); strings.Contains(main, "' · 慢 '") {
		t.Error("「· 慢 X%」应当由 latSlowText 单独给：混在 textContent 里就没法只让那一段变红")
	}
	toggles := funcBody(js, "function renderLatToggles(")
	if toggles == "" {
		t.Fatal("app.js 缺少 renderLatToggles()")
	}
	if !strings.Contains(toggles, "latSlowText(t)") {
		t.Error("图例没有把「· 慢 X%」渲染出来")
	}
	if !strings.Contains(toggles, "slow.style.color = SLOW_COLOR;") {
		t.Error("「· 慢 X%」必须用 SLOW_COLOR 上色：图上红线与图例红字得是同一种红")
	}
	if !regexp.MustCompile(`var SLOW_COLOR = '#ef4444';`).MatchString(js) {
		t.Fatal("app.js 缺少 SLOW_COLOR（慢的红色，图上与图例共用）")
	}
	// 与曲线调色板撞色的话，"这条线本来就是红的"与"这一段慢"就分不出来了。
	if regexp.MustCompile(`PING_COLORS = \[[^\]]*SLOW_COLOR`).MatchString(js) {
		t.Error("慢的红色不该出现在 PING_COLORS 里（撞色就分不清是线色还是慢段）")
	}
	if !regexp.MustCompile(`\.lat-targets \.legend-slow`).MatchString(css) {
		t.Error("style.css 缺少 .lat-targets .legend-slow 规则")
	}

	// 3) 迷你条：延迟格子按同一个 threshold_ms 分级（≤阈值 绿 / ≤2× 黄 / >2× 红）。
	render := funcBody(js, "function renderMiniBar(")
	if render == "" {
		t.Fatal("app.js 缺少 renderMiniBar()")
	}
	if !regexp.MustCompile(`miniLatClass\(value,\s*mini\.threshold_ms\)`).MatchString(render) {
		t.Error("迷你条延迟格子必须用后端给的 threshold_ms 分级，而不是该节点的延迟均值")
	}
	lat := funcBody(js, "function miniLatClass(")
	if lat == "" {
		t.Fatal("app.js 缺少 miniLatClass()")
	}
	if !regexp.MustCompile(`if \(!\(threshold > 0\)\) return '';`).MatchString(lat) {
		t.Error("阈值算不出来（threshold_ms = 0）时应当保持浅灰：拿 0 当基准会把所有格子判成红的")
	}
	if !regexp.MustCompile(`value > threshold \* MINI_LAT_BAD_RATIO`).MatchString(lat) ||
		!regexp.MustCompile(`return value > threshold \? 'warn' : 'ok';`).MatchString(lat) {
		t.Error("延迟格子应当是：≤ 阈值 绿、≤ 2× 阈值 黄、> 2× 阈值 红")
	}
	if strings.Contains(lat, "avg") {
		t.Error("迷你条的延迟分级里还留着均值口径（那是「探测」行的，两处故意不同）")
	}

	// 4) 图表引擎：逐段着色 + 孤立点画成红点 + 不跨过正常区间连线。
	if !regexp.MustCompile(`function drawSlow\(`).MatchString(chart) {
		t.Fatal("chart.js 缺少 drawSlow()：超阈值的段画不红")
	}
	draw := chartFuncBody(chart, "function drawSlow(")
	if draw == "" {
		t.Fatal("chart.js 的 drawSlow() 函数体没截取到")
	}
	if !regexp.MustCompile(`if \(!\(spec\.threshold > 0\)\) return;`).MatchString(draw) {
		t.Error("阈值 <= 0（服务端算不出来）时不该标红")
	}
	// 孤立的单个超阈值点：moveTo 之后没有 lineTo，stroke 什么也画不出来 ——
	// 必须单独画成一个点，否则"偶发一根尖峰"就消失了。
	if !regexp.MustCompile(`if \(run\.length === 1\)`).MatchString(draw) ||
		!regexp.MustCompile(`ctx\.arc\(`).MatchString(draw) {
		t.Error("孤立的单个超阈值点必须画成红点（只 moveTo 不 lineTo 的话 stroke 画不出任何东西）")
	}
	// 连续段：相邻两个及以上才连线。
	if !regexp.MustCompile(`else if \(run\.length > 1\)`).MatchString(draw) {
		t.Error("连续多个超阈值点应当连成一段红线")
	}
	// 遇到正常点就断开 —— 这是"红线不跨越正常区间"的关键。
	if !regexp.MustCompile(`else flush\(\);`).MatchString(draw) {
		t.Error("遇到正常点必须把当前段 flush 掉：否则两个不相邻的尖峰之间会被拉一条跨过正常区间的红线")
	}
	// 段是遍历 pts 攒出来的（下标全部来自 pts），所以首尾的尖峰天然不越界。
	if !regexp.MustCompile(`for \(var i = 0; i < pts\.length; i\+\+\)`).MatchString(draw) {
		t.Error("drawSlow 应当遍历 pts 攒连续段（这样首尾的超阈值点不会越界）")
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
		// 详情页：实时网络（速率）、累计流量、今日流量、历史累计流量。
		"infoRow(net, '实时网络', '↑ ' + fmtRate(node.tx_rate) + '  ↓ ' + fmtRate(node.rx_rate))",
		"infoRow(net, '累计流量', '↑ ' + fmtBytesDec(node.tx_total) + '  ↓ ' + fmtBytesDec(node.rx_total))",
		"infoRow(tra, '今日流量', '↓ ' + fmtBytesDec(node.traffic_today_rx) + '  ↑ ' + fmtBytesDec(node.traffic_today_tx))",
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
	// 本周期流量：四个数（收/发/共/额度）都得是 Dec。
	cycle := funcBody(js, "function trafficCycleText(")
	if cycle == "" {
		t.Fatal("app.js 缺少 trafficCycleText()")
	}
	if n := strings.Count(cycle, "fmtBytesDec("); n != 4 {
		t.Errorf("trafficCycleText() 里应当有 4 处 fmtBytesDec（↓收 ↑发 共 额度），实际 %d 处", n)
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
	// 行是三个格子（把手 + 内容 + 按钮），窄屏也要能拖：把手仍在，且内容列能收缩。
	if !regexp.MustCompile(`(?s)\.node-item\s*\{[^}]*grid-template-columns:\s*auto\s+minmax\(0,\s*1fr\)\s+auto`).MatchString(css) {
		t.Error("style.css 里 .node-item 应当是「把手 + 内容 + 按钮」三列")
	}
	if !regexp.MustCompile(`(?s)@media \(max-width: 900px\).*?\.node-item\s*\{[^}]*grid-template-columns:\s*auto`).MatchString(css) {
		t.Error("窄屏 media query 里 .node-item 仍要给把手留一列（否则手机上没地方拖）")
	}
	if !regexp.MustCompile(`(?s)@media \(max-width: 900px\).*?\.node-drag\s*\{`).MatchString(css) {
		t.Error("窄屏 media query 里应当写明 .node-drag 的跨行方式（把手要垂直居中）")
	}

	// 这里原本断言"index.html 里要有一句说明可以拖动排序的提示"。**用户明确要求把它去掉**
	// （嫌占地方），所以断言删了 —— 不是疏漏，别再加回去。
	// 可发现性由行左侧那个六点把手承担；真有人反馈"不知道能拖"，再去谈要不要加提示。
	// 反过来钉一条：提示没了，列表容器本身必须在，否则拖拽的事件挂载点就丢了。
	if !strings.Contains(html, `id="nodes-list"`) {
		t.Error("服务器列表的容器 #nodes-list 不见了 —— 拖拽的事件就挂不上去了")
	}
}
