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
	for _, name := range []string{"view-setup", "view-login", "view-home", "view-detail", "view-audit"} {
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
		`id="info-hardware"`, `id="info-system"`, `id="info-storage"`,
		`id="info-network"`, `id="info-traffic"`, `id="detail-charts"`,
		`id="stat-price"`, `id="stat-monthly"`, `id="stat-left"`, `id="stat-value"`,
		`id="detail-ranges"`, `id="chart-cpu"`, `id="chart-mem"`,
		`id="chart-disk"`, `id="chart-net"`, `id="chart-lat"`, `id="chart-traffic"`, `id="detail-back"`,
		// 后台管理（Phase 9）
		`id="detail-edit"`, `id="detail-token"`, `id="detail-delete"`,
		`id="dlg-confirm"`, `id="confirm-ok"`, `id="node-title"`, `id="node-warn"`,
		`id="node-note"`, `id="node-enabled"`, `id="node-enabled-wrap"`,
		`id="view-audit"`, `id="audit-body"`, `id="audit-more"`, `id="audit-back"`,
		`id="alert-cooldown"`, `id="alert-debounce"`, `id="server-info"`, `id="settings-audit"`,
		`id="pw-current"`, `id="pw-new"`, `id="pw-new2"`, `id="pw-submit"`,
		// 价格与图表可见性（Phase 12）
		`id="node-price"`, `id="node-currency"`, `id="node-billing"`, `id="chart-toggles"`,
	} {
		if !strings.Contains(html, needle) {
			t.Errorf("index.html 缺少 %s", needle)
		}
	}
}

// 详情页由三段组成：汇总排（4 格）→ 信息卡网格（5 张）→ 全宽图表（6 张）。
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
	for _, rule := range []string{".detail-wrap", ".stat-row", ".stat", ".stat-label", ".stat-value", ".info-grid", ".chart-block"} {
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
	if !regexp.MustCompile(`el\.detailCharts\.hidden\s*=`).MatchString(js) {
		t.Error("六张图全被取消勾选时，应当连图表卡片一起收起来（否则只剩一个空边框）")
	}
}
