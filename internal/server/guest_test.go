package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"probe/internal/protocol"
	"probe/internal/store"
)

// 这个文件是「允许访客查看」（访客只读模式）的验收。
//
// 为什么测试是这次的重点：这个功能一旦漏一处，**表现和做对了完全一样** ——
// 页面上看不出任何异常，只是别人能读到他不该读到的东西、或者能改你的机器。
// 所以断言分四层：
//
//  1. 路由枚举（TestEveryRouteIsProtected）：每一条路由的访问级别都被显式验证过。
//     "漏保护一个写接口"在这里当场变红。
//  2. 字段分类（TestNodeDTOFieldsAreClassified）：nodeDTO 的每个 JSON 字段都被
//     显式分过类 —— 以后加字段忘了分类，测试红，而不是"默认公开"。
//  3. 响应比对（TestGuestReadsAreRedacted）：访客响应 == 管理员响应**减去**
//     私有字段。既钉住"私有字段不存在"，也钉住"没有顺手少给别的东西"。
//  4. SSE 与 HTTP 同源（TestGuestStreamSnapshotIsRedacted）：实时流是最容易漏的
//     一条路径（首页每秒都靠它更新）。

// ---------------------------------------------------------------- 开关

// guestOn 打开「允许访客查看」（走真实的写入路径：库里 + 缓存一起更新）。
func guestOn(t *testing.T, h *authHarness) {
	t.Helper()
	if err := h.srv.setGuestAccess(context.Background(), true); err != nil {
		t.Fatalf("打开访客开关: %v", err)
	}
}

// guestOff 关闭开关。
func guestOff(t *testing.T, h *authHarness) {
	t.Helper()
	if err := h.srv.setGuestAccess(context.Background(), false); err != nil {
		t.Fatalf("关闭访客开关: %v", err)
	}
}

// TestGuestSwitchDefaultsToOff 钉住**默认必须是关**。
//
// 理由不是"保守一点更好"，而是很具体的一条：默认打开等于"升级即泄露" ——
// 老用户升级完，面板就自动挂到公网上了，而他们根本不知道这件事发生过。
// 所以这里验的是"一个新的数据库里，这个开关就是关的"（settings 表里没有那一行），
// 而不是"某个函数返回 false"。
func TestGuestSwitchDefaultsToOff(t *testing.T) {
	h := newAuthHarness(t)

	if h.srv.guestAccessEnabled(context.Background()) {
		t.Fatal("新装的面板不该默认允许访客查看（默认打开=升级即泄露）")
	}
	// 库里也确实没有那一行：默认值是"没有这个键"，不是"写了一个 0"。
	if _, ok, err := h.srv.db.GetSetting(context.Background(), store.KeyGuestAccess); err != nil {
		t.Fatalf("读取设置: %v", err)
	} else if ok {
		t.Fatal("默认状态不该往 settings 表里写 guest_access")
	}
	// 前台看到的是同一个答案（前端据此决定显示登录页还是只读面板）。
	status, body := h.get(t, "/api/v1/session")
	if status != http.StatusOK {
		t.Fatalf("会话接口状态码 = %d", status)
	}
	if body["guest_access"] != false {
		t.Fatalf("会话接口返回的 guest_access = %v，期望 false", body["guest_access"])
	}
	// 授权接口：GET /settings 里也能读到（设置页那个开关的回填来源）。
	status, body = h.get(t, "/api/v1/settings")
	if status != http.StatusOK {
		t.Fatalf("读取设置失败: %d", status)
	}
	guest, _ := body["guest"].(map[string]any)
	if guest == nil || guest["enabled"] != false {
		t.Fatalf("GET /settings 里的 guest = %v，期望 {enabled:false}", body["guest"])
	}

	// 打开 → 立刻生效（同一份缓存，不必重启）。
	status, body = h.put(t, "/api/v1/settings/guest", map[string]any{"enabled": true})
	if status != http.StatusOK {
		t.Fatalf("打开访客开关失败: %d %v", status, body)
	}
	if !h.srv.guestAccessEnabled(context.Background()) {
		t.Fatal("保存之后开关应当立刻生效")
	}
	if _, ok, _ := h.srv.db.GetSetting(context.Background(), store.KeyGuestAccess); !ok {
		t.Fatal("开关应当落进 settings 表（重启后仍然有效）")
	}
	status, body = h.get(t, "/api/v1/session")
	if status != http.StatusOK || body["guest_access"] != true {
		t.Fatalf("打开之后会话接口应当返回 guest_access=true，实际 %d %v", status, body["guest_access"])
	}

	// 缺 enabled 字段必须报 400，而不是被零值当成"关掉"。
	status, _ = h.put(t, "/api/v1/settings/guest", map[string]any{})
	if status != http.StatusBadRequest {
		t.Fatalf("缺 enabled 应当 400，实际 %d", status)
	}

	// 关掉 → 未登录的读接口回到 401。
	status, body = h.put(t, "/api/v1/settings/guest", map[string]any{"enabled": false})
	if status != http.StatusOK {
		t.Fatalf("关闭访客开关失败: %d %v", status, body)
	}
	h.anonymousClient(t)
	if status, _ := h.get(t, "/api/v1/nodes"); status != http.StatusUnauthorized {
		t.Fatalf("关掉开关之后未登录读列表应当 401，实际 %d", status)
	}
}

// TestGuestReadsAllowedWhenSwitchOn 开关打开时，未登录也能读到数据（这是功能本体）。
func TestGuestReadsAllowedWhenSwitchOn(t *testing.T) {
	h := newAuthHarness(t)
	createNodeOverHTTP(t, h, "guest-read")
	guestOn(t, h)
	h.anonymousClient(t)

	for _, path := range []string{
		"/api/v1/nodes", "/api/v1/nodes/1", "/api/v1/nodes/1/series?metric=cpu&range=1h",
		"/api/v1/nodes/1/ping?range=1h", "/api/v1/nodes/1/traffic", "/api/v1/overview",
	} {
		status, body := h.get(t, path)
		if status != http.StatusOK {
			t.Errorf("开关打开时访客读 %s 应当 200，实际 %d（%v）", path, status, body)
		}
	}
}

// TestGuestCannotWriteAnything 开关打开也不会放开任何写操作。
//
// 与枚举测试重复是**故意**的：那条是"遍历所有路由"的通用规则，这条把
// "访客改不了机器"这句话单独钉一遍，读代码的人不必去推。
func TestGuestCannotWriteAnything(t *testing.T) {
	h := newAuthHarness(t)
	createNodeOverHTTP(t, h, "guest-write")
	guestOn(t, h)
	admin := h.client
	h.anonymousClient(t)

	writes := []struct {
		method string
		path   string
		body   map[string]any
	}{
		{http.MethodPost, "/api/v1/nodes", map[string]any{"name": "evil", "interval_sec": 1}},
		{http.MethodPatch, "/api/v1/nodes/1", map[string]any{"name": "evil", "interval_sec": 1}},
		{http.MethodPut, "/api/v1/nodes/1", map[string]any{"name": "evil", "interval_sec": 1}},
		{http.MethodPut, "/api/v1/nodes/order", map[string]any{"ids": []int64{1}}},
		{http.MethodDelete, "/api/v1/nodes/1", nil},
		{http.MethodPost, "/api/v1/nodes/1/token", nil},
		{http.MethodPut, "/api/v1/settings/alert", map[string]any{"cooldown": "5m"}},
		{http.MethodPut, "/api/v1/settings/charts", map[string]any{"visible": []string{}}},
		{http.MethodPut, "/api/v1/settings/ping", map[string]any{"targets": []any{}}},
		{http.MethodPut, "/api/v1/settings/guest", map[string]any{"enabled": false}},
		{http.MethodPut, "/api/v1/settings/telegram", map[string]any{"enabled": false, "chat_id": ""}},
		{http.MethodPost, "/api/v1/settings/telegram/test", nil},
		{http.MethodPost, "/api/v1/auth/password", map[string]any{
			"current_password": "correct-horse-battery-staple",
			"new_password":     "another-good-password",
			"new_password2":    "another-good-password"}},
	}
	for _, tc := range writes {
		status, _, _ := h.do(t, tc.method, tc.path, tc.body, false, nil)
		if status != http.StatusUnauthorized {
			t.Errorf("%s %s：访客必须 401，实际 %d", tc.method, tc.path, status)
		}
	}

	// 节点还在、名字没被改（不是"返回了 401 但事情已经做了"）。
	h.client = admin
	status, body := h.get(t, "/api/v1/nodes")
	if status != http.StatusOK {
		t.Fatalf("管理员读取节点失败: %d", status)
	}
	nodes, _ := body["nodes"].([]any)
	if len(nodes) != 1 {
		t.Fatalf("节点数 = %d，期望 1（访客的请求不该真的建/删节点）", len(nodes))
	}
	node, _ := nodes[0].(map[string]any)
	if node["name"] != "guest-write" {
		t.Fatalf("节点名被改成了 %v（访客的写请求被受理了）", node["name"])
	}
}

// ---------------------------------------------------------------- 路由枚举

// guestRouteKey 是路由在断言里的可读名字。
func guestRouteKey(rt routeSpec) string { return rt.Method + " " + rt.Pattern }

// guestRoutePath 把模式变成一个真实请求路径（{id} 用 1 代）。
func guestRoutePath(rt routeSpec) string {
	return strings.ReplaceAll(rt.Pattern, "{id}", "1")
}

// probeStatus 发一个请求**只取状态码**，不读 body。
//
// 为什么不能用 h.do：SSE 那条路由的 body 是一个永不结束的流，
// io.ReadAll 会一直挂到超时；而这里只关心"放行了没有"。
func probeStatus(t *testing.T, client *http.Client, method, rawURL string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
	if err != nil {
		t.Fatalf("构造请求 %s %s: %v", method, rawURL, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("请求 %s %s: %v", method, rawURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode
}

// TestEveryRouteIsProtected 遍历路由器里注册的**每一条**路由，逐条验证保护策略。
//
// 两条规则：
//
//	① 写方法（POST/PUT/PATCH/DELETE）+ 无会话 → 必须 401
//	   —— 唯一的例外是 accessOpen 那几条（登录/初始化/退出，它们必须在未登录时
//	   可用，否则没人能登录进来）。这几条的名单在下面被**冻结**了。
//	② 不在公开白名单里的路由 + 开关打开 + 无会话 → 必须 401
//
// 为什么必须"枚举"而不是抽查几条：漏保护一个写接口是这类功能最现实的翻车方式，
// 而且静默 —— 页面上看不出任何异常。路由是数据（server.go 的 routes()），
// 所以这里能一条不落地验；另有 TestAllRoutesGoThroughRouteTable 钉住
// "新路由不许绕过那张表"。
func TestEveryRouteIsProtected(t *testing.T) {
	h := newAuthHarness(t)
	createNodeOverHTTP(t, h, "route-audit")
	// 开关**打开**：这样"漏保护"的路由会露出真实面目（真的把数据发出来），
	// 而不是被"开关关着"这层假象挡住 —— 关着时所有访客读接口都 401，
	// 一个漏保护的写接口会跟着一起"看起来是对的"。
	guestOn(t, h)
	h.anonymousClient(t)

	// 免鉴权路由的**冻结清单**：改动这里必须是有意识的（想清楚"它凭什么公开"）。
	wantOpen := []string{
		"GET /healthz",
		"GET /api/v1/agent/ws",
		"GET /api/v1/session",
		"POST /api/v1/setup",
		"POST /api/v1/auth/login",
		"POST /api/v1/auth/logout",
		"GET /robots.txt",
		"GET /",
		"GET /api/", "POST /api/", "PUT /api/", "PATCH /api/", "DELETE /api/",
	}
	// 访客可读白名单的冻结清单：写接口**永远不会**出现在这里。
	wantGuestRead := []string{
		"GET /api/v1/nodes",
		"GET /api/v1/nodes/{id}",
		"GET /api/v1/nodes/{id}/series",
		"GET /api/v1/nodes/{id}/ping",
		"GET /api/v1/nodes/{id}/traffic",
		"GET /api/v1/overview",
		"GET /api/v1/stream",
	}
	// 写路由的冻结清单（不含 /api/ 的 404 兜底 —— 它不落地任何东西）。
	//
	// 为什么要单独冻这一份：新加一条写接口时，哪怕它默认就是"需要登录"，
	// 也必须有人在这里补一笔 —— 也就是**必须有人看一眼**它到底受不受保护。
	// 这正是这个功能最可能翻车的地方（漏保护一个写接口 = 别人能改你的机器，
	// 而页面上完全看不出来）。GET 的管理接口不冻：它们的默认值本来就是安全的，
	// 每加一条都改测试只会让人习惯性地"顺手加一行"。
	wantWrites := []string{
		"POST /api/v1/setup",
		"POST /api/v1/auth/login",
		"POST /api/v1/auth/logout",
		"POST /api/v1/auth/password",
		"POST /api/v1/nodes",
		"PATCH /api/v1/nodes/{id}",
		"PUT /api/v1/nodes/{id}",
		"PUT /api/v1/nodes/order",
		"DELETE /api/v1/nodes/{id}",
		"POST /api/v1/nodes/{id}/token",
		"PUT /api/v1/settings/alert",
		"PUT /api/v1/settings/charts",
		"PUT /api/v1/settings/guest",
		"PUT /api/v1/settings/ping",
		"PUT /api/v1/settings/telegram",
		"POST /api/v1/settings/telegram/test",
	}

	routes := h.srv.routeSpecs
	if len(routes) < 25 {
		t.Fatalf("只枚举到 %d 条路由，路由表多半没被填满（那这些断言会平凡通过）", len(routes))
	}

	var gotOpen, gotGuestRead, gotWrites []string
	for _, rt := range routes {
		key := guestRouteKey(rt)
		switch rt.Access {
		case accessOpen:
			gotOpen = append(gotOpen, key)
		case accessGuestRead:
			gotGuestRead = append(gotGuestRead, key)
			// 访客可读**只能是读**：把写接口标成"访客可读"是最危险的一种笔误，
			// 所以这里直接钉死它的形状。
			if isMutating(rt.Method) {
				t.Errorf("%s 被标成了访客可读，但它是写方法 —— 访客只读模式不接受任何写操作", key)
			}
		case accessAdmin:
			// 下面按规则②验。
		default:
			t.Errorf("%s 的访问级别是未知值 %d", key, rt.Access)
		}
		// 写路由的清单：/api/ 那几条是 404 兜底（哪个方法都收），不算接口。
		if isMutating(rt.Method) && rt.Pattern != apiPrefix {
			gotWrites = append(gotWrites, key)
		}

		path := guestRoutePath(rt)
		code := probeStatus(t, h.client, rt.Method, h.ts.URL+path)

		// ① 写方法 + 无会话 → 401（accessOpen 除外，理由见上）。
		if isMutating(rt.Method) && rt.Access != accessOpen && code != http.StatusUnauthorized {
			t.Errorf("%s：写方法 + 无会话必须 401，实际 %d", key, code)
		}
		// ② 不在公开白名单里的 → 401。
		if rt.Access == accessAdmin && code != http.StatusUnauthorized {
			t.Errorf("%s：不是公开接口，无会话（开关打开）必须 401，实际 %d", key, code)
		}
		// 反向：公开的读接口必须真的能用（否则"保护"是假的 —— 靠 500 挡住谁也读不到）。
		if rt.Access == accessGuestRead && code == http.StatusUnauthorized {
			t.Errorf("%s：标成了访客可读，开关打开时却不放行（%d）", key, code)
		}
	}

	sort.Strings(gotOpen)
	sort.Strings(gotGuestRead)
	sort.Strings(gotWrites)
	sort.Strings(wantOpen)
	sort.Strings(wantGuestRead)
	sort.Strings(wantWrites)
	if strings.Join(gotOpen, "|") != strings.Join(wantOpen, "|") {
		t.Errorf("免鉴权路由变了：\n实际 %v\n期望 %v\n"+
			"新增一条免鉴权路由必须同时改这份清单（并想清楚它凭什么公开）", gotOpen, wantOpen)
	}
	if strings.Join(gotGuestRead, "|") != strings.Join(wantGuestRead, "|") {
		t.Errorf("访客可读白名单变了：\n实际 %v\n期望 %v", gotGuestRead, wantGuestRead)
	}
	if strings.Join(gotWrites, "|") != strings.Join(wantWrites, "|") {
		t.Errorf("写路由清单变了：\n实际 %v\n期望 %v\n"+
			"新增/删除一条写接口必须同时改这份清单 —— 也就是必须有人确认过它受不受保护",
			gotWrites, wantWrites)
	}
}

// TestAllRoutesGoThroughRouteTable 钉住"所有路由都从 routes() 那张表注册"。
//
// 光有枚举测试不够：它只能看见**表里**的路由。一句手写的
// mux.HandleFunc("POST /api/v1/whatever", ...) 完全绕过那张表 ——
// "所有写接口都必须登录"的枚举断言对它一无所知，而这正是"漏保护一个写接口"
// 最现实的样子。所以这里按静态规则把那条路堵死。
func TestAllRoutesGoThroughRouteTable(t *testing.T) {
	const registerCall = `mux.HandleFunc(rt.Method+" "+rt.Pattern, rt.wrap(s))`

	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("扫描本包源码: %v", err)
	}
	if len(paths) < 10 {
		t.Fatalf("只扫到 %d 个 go 文件，多半是工作目录不对", len(paths))
	}

	handleCall := regexp.MustCompile(`mux\.Handle(Func)?\(`)
	total := 0
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("读取 %s: %v", path, err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			code := line
			if at := strings.Index(code, "//"); at >= 0 {
				code = code[:at]
			}
			if !handleCall.MatchString(code) {
				continue
			}
			total++
			if !strings.Contains(line, "rt.Method") {
				t.Errorf("%s:%d 直接往 mux 上挂了路由：%s\n"+
					"所有路由都必须写进 server.go 的 routes()（带访问级别），"+
					"否则枚举测试看不见它 —— 一个漏保护的写接口就是这么来的",
					path, i+1, strings.TrimSpace(line))
			}
		}
	}
	if total != 1 {
		t.Errorf("mux.Handle* 调用有 %d 处，期望恰好 1 处（routes() 的注册循环）", total)
	}

	// 注册循环本身也要在：上面的静态规则靠它把"表"和"保护"绑在一起。
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatalf("读取 server.go: %v", err)
	}
	if !strings.Contains(string(src), registerCall) {
		t.Errorf("server.go 里找不到注册循环 %q：路由表与保护包装的绑定被改掉了", registerCall)
	}
}

// ---------------------------------------------------------------- 字段分类

// jsonKeysOf 反射出一个结构体的全部 JSON 键（不含 "-" 的）。
func jsonKeysOf(t *testing.T, v any) []string {
	t.Helper()
	typ := reflect.TypeOf(v)
	if typ.Kind() == reflect.Ptr {
		typ = typ.Elem()
	}
	if typ.Kind() != reflect.Struct {
		t.Fatalf("%T 不是结构体", v)
	}
	out := make([]string, 0, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		tag := typ.Field(i).Tag.Get("json")
		name := strings.Split(tag, ",")[0]
		if name == "" || name == "-" {
			continue
		}
		out = append(out, name)
	}
	return out
}

// TestNodeDTOFieldsAreClassified 钉住"nodeDTO 的每个字段都被显式分过类"。
//
// 这是白名单能长期成立的关键：**新增字段默认私有**这件事，
// 只有在"忘了分类就变红"的前提下才真正成立。反过来说，如果这里不红，
// 那么下一个人往 nodeDTO 里加一个 address/secret 字段时，它会当场对访客公开，
// 而且没有任何地方会提醒他。
func TestNodeDTOFieldsAreClassified(t *testing.T) {
	public := map[string]bool{}
	for _, key := range guestPublicNodeFields {
		public[key] = true
	}
	private := map[string]bool{}
	for _, key := range guestPrivateNodeFields {
		private[key] = true
	}
	// 同一份清单里既有又有的键一定是抄错了：过滤只认公开那份，
	// 而分类测试会因为"公开∩私有≠∅"在这里直接报出来。
	for key := range public {
		if private[key] {
			t.Errorf("字段 %q 同时出现在公开与私有两份清单里", key)
		}
	}

	fields := jsonKeysOf(t, nodeDTO{})
	if len(fields) < 60 {
		t.Fatalf("只反射到 %d 个字段，nodeDTO 多半被换掉了（这些断言会平凡通过）", len(fields))
	}
	for _, key := range fields {
		switch {
		case public[key]:
		case private[key]:
		default:
			t.Errorf("nodeDTO 的字段 %q 没有被分类：\n"+
				"  公开 → 加进 guest.go 的 guestPublicNodeFields\n"+
				"  私有 → 加进 guestPrivateNodeFields（并在注释里写清它能用来做什么）\n"+
				"默认是**私有**：不加就是不给访客看", key)
		}
	}
	// 私有清单里的键必须真的存在（改名之后别留下一个永远匹配不上的死名字）。
	for key := range private {
		if !containsString(fields, key) {
			t.Errorf("私有清单里的 %q 在 nodeDTO 里不存在（改名字时漏改了清单？）", key)
		}
	}
	for key := range public {
		if !containsString(fields, key) {
			t.Errorf("公开清单里的 %q 在 nodeDTO 里不存在（改名字时漏改了清单？）", key)
		}
	}
}

// TestTargetDTONestedFieldsAreClassified 同样的分类断言，覆盖探测目标那几个结构。
//
// 它们各有一个 host 字段是私有的（探测目标是**地址**，属于"能定位到具体主机/网络"
// 那一类）；label 与全部数值照旧公开。
func TestTargetDTONestedFieldsAreClassified(t *testing.T) {
	cases := []struct {
		name  string
		value any
		allow []string
	}{
		{"pingTargetSeries", pingTargetSeries{}, guestPublicPingTargetFields},
		{"store.OverviewPingTarget", store.OverviewPingTarget{}, guestPublicOverviewTargetFields},
	}
	for _, tc := range cases {
		allow := map[string]bool{}
		for _, key := range tc.allow {
			allow[key] = true
		}
		for _, key := range jsonKeysOf(t, tc.value) {
			if allow[key] || key == "host" {
				continue
			}
			t.Errorf("%s 的字段 %q 没有被分类：公开的加进白名单，私有的加进 guestPrivateTargetFields",
				tc.name, key)
		}
	}
	// 私有那一份只有 host 一个：它是"地址"，不给访客。
	if len(guestPrivateTargetFields) != 1 || guestPrivateTargetFields[0] != "host" {
		t.Errorf("探测目标的私有字段清单 = %v，期望只有 host（改动它请一并想清楚理由）",
			guestPrivateTargetFields)
	}
}

func containsString(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- 响应比对

// jsonKeySet 递归收集一份 JSON 里的**全部键名**（数组用同一个前缀继续往下走）。
//
// 用键名集合而不是文本匹配：文本里出现 "local_ip" 可能是某个节点的名字，
// 而"键存不存在"才是脱敏这件事的定义（见 guest.go 的白名单）。
func jsonKeySet(value any, out map[string]bool) {
	switch v := value.(type) {
	case map[string]any:
		for key, child := range v {
			out[key] = true
			jsonKeySet(child, out)
		}
	case []any:
		for _, child := range v {
			jsonKeySet(child, out)
		}
	}
}

// guestPrivateKeyUniverse 是"访客响应里一个都不许出现"的键名全集。
func guestPrivateKeyUniverse() map[string]bool {
	out := map[string]bool{}
	for _, key := range guestPrivateNodeFields {
		out[key] = true
	}
	for _, key := range guestPrivateTargetFields {
		out[key] = true
	}
	return out
}

// guestSample 造一个"该有的都有"的现场：节点带备注/价格/到期日，
// 内存状态里有三个地址字段，还配了两个探测目标。
//
// 为什么要把现场铺满：只断言"访客响应里没有 local_ip"是不够的 ——
// 如果管理员响应里本来就没有它，这条断言在任何实现下都成立（平凡通过）。
// 所以下面还要反过来断言"管理员那份**确实有**这些字段"。
func guestSample(t *testing.T, h *authHarness) int64 {
	t.Helper()
	ctx := context.Background()

	status, body := h.post(t, "/api/v1/nodes", map[string]any{
		"name": "guest-01", "group_name": "香港", "region": "HK", "interval_sec": 1,
		"note":        "备注里可以写任何东西：root@203.0.113.7:22",
		"price_cents": 7121, "currency": "CNY", "billing_months": 12,
		"expires_at":    time.Now().Add(30 * 24 * time.Hour).Unix(),
		"traffic_limit": int64(1) << 40, "traffic_warn_pct": 80, "reset_day": 1,
	}, nil)
	if status != http.StatusCreated {
		t.Fatalf("创建节点失败: %d %v", status, body)
	}
	node, _ := body["node"].(map[string]any)
	id := int64(node["id"].(float64))

	// 内存状态：三个地址字段 + 一份指标（boot_id 在 Net 里）。
	h.srv.State().Attach(id, 1, protocol.Info{
		OS:           protocol.OSInfo{Name: "Debian", Kernel: "6.1.0"},
		CPU:          protocol.CPUInfo{Model: "EPYC", Cores: 2},
		AgentVersion: "1.0.42",
	}, "203.0.113.9", "10.0.0.5", "fd00::5", time.Now())
	h.srv.State().Update(id, 1, protocol.Metrics{
		CPUPct: 12.5, Mem: protocol.Mem{Total: 1 << 30, Used: 1 << 29, Pct: 50},
		Net:       protocol.Net{Iface: "eth0", BootID: "boot-abc", RxRate: 1024, TxRate: 2048},
		UptimeSec: 3600,
	}, 0, time.Now())

	// 两个探测目标：ping 与总览的 targets 都要有 host 可以漏。
	status, body = h.put(t, "/api/v1/settings/ping", map[string]any{
		"interval_sec": 60,
		"targets": []map[string]any{
			{"label": "Cloudflare", "type": "tcp", "host": "1.1.1.1", "port": 443, "enabled": true},
			{"label": "内网 NAS", "type": "icmp", "host": "nas.home.lan", "port": 0, "enabled": true},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("配置探测目标失败: %d %v", status, body)
	}
	// 落一行探测样本：总览的 targets[].host 只有在真有数据时才会出现。
	h.srv.ping.observe(id, []protocol.PingResult{
		{TargetID: 1, AvgMS: 23.4, MinMS: 20, MaxMS: 31, LossPct: 0},
		{TargetID: 2, AvgMS: 5.1, MinMS: 4, MaxMS: 9, LossPct: 0},
	}, time.Now())
	h.srv.flushPings(ctx)
	return id
}

// TestGuestReadsAreRedacted 逐接口比对"管理员看到的"与"访客看到的"。
//
// 三条断言，缺一条就会漏掉一类错：
//  1. 访客响应里**一个私有键都没有**（这是安全性质本身）；
//  2. 访客响应 == 管理员响应 − 私有键（不多不少）：
//     多出来的是"漏了脱敏"，少掉的是"顺手把访客该看的东西也砍了"；
//  3. 管理员响应里**确实有**这些私有键（否则第 1 条是空断言）。
func TestGuestReadsAreRedacted(t *testing.T) {
	h := newAuthHarness(t)
	id := guestSample(t, h)
	privateUniverse := guestPrivateKeyUniverse()

	paths := []string{
		"/api/v1/nodes",
		fmt.Sprintf("/api/v1/nodes/%d", id),
		fmt.Sprintf("/api/v1/nodes/%d/series?metric=cpu&range=1h", id),
		fmt.Sprintf("/api/v1/nodes/%d/ping?range=1h", id),
		fmt.Sprintf("/api/v1/nodes/%d/traffic", id),
		"/api/v1/overview?window=1h&buckets=10",
	}

	// 先把管理员那一份读出来（用带会话的客户端）。
	adminKeys := map[string]map[string]bool{}
	for _, path := range paths {
		status, body := h.get(t, path)
		if status != http.StatusOK {
			t.Fatalf("管理员读取 %s 失败: %d %v", path, status, body)
		}
		keys := map[string]bool{}
		jsonKeySet(body, keys)
		adminKeys[path] = keys
	}

	// 现场自检：管理员响应里必须**真的**有这些私有字段，否则下面全是空断言。
	// 三个地址字段与备注出现在 /nodes 里，boot_id 也是；host 出现在 ping 里。
	needNodeKeys := []string{"observed_ip", "local_ip", "local_ip6", "note", "boot_id"}
	for _, key := range needNodeKeys {
		if !adminKeys["/api/v1/nodes"][key] {
			t.Fatalf("管理员响应里没有 %q：这条用例会退化成空断言（现场没铺满）", key)
		}
	}
	if !adminKeys[fmt.Sprintf("/api/v1/nodes/%d/ping?range=1h", id)]["host"] {
		t.Fatalf("管理员响应里没有 host：这条用例会退化成空断言（探测目标没配上）")
	}
	// 价格是**公开**的（用户明确要求访客能看到），所以它必须在两边都在。
	for _, key := range []string{"price_cents", "monthly_cents", "remaining_value_cents", "remaining_days", "expires_text"} {
		if !adminKeys["/api/v1/nodes"][key] {
			t.Fatalf("价格字段 %q 不在管理员响应里（现场没铺满）", key)
		}
	}

	// 换成访客（没有会话），并把开关打开。
	guestOn(t, h)
	h.anonymousClient(t)

	for _, path := range paths {
		status, body := h.get(t, path)
		if status != http.StatusOK {
			t.Fatalf("访客读取 %s 失败: %d %v", path, status, body)
		}
		guestKeys := map[string]bool{}
		jsonKeySet(body, guestKeys)

		for key := range guestKeys {
			if privateUniverse[key] {
				t.Errorf("%s：访客响应里出现了私有字段 %q（整份响应：%v）", path, key, body)
			}
		}

		for key := range adminKeys[path] {
			if privateUniverse[key] {
				continue
			}
			if !guestKeys[key] {
				t.Errorf("%s：字段 %q 不是私有的，却被访客那份漏掉了（脱敏不该顺手砍别的）", path, key)
			}
		}
	}

	// 价格三件套在访客那份里必须还在（用户明确要求：访客能看价格）。
	status, body := h.get(t, "/api/v1/nodes")
	if status != http.StatusOK {
		t.Fatalf("访客读取节点列表失败: %d", status)
	}
	nodes, _ := body["nodes"].([]any)
	if len(nodes) == 0 {
		t.Fatal("访客拿到的节点列表是空的")
	}
	first, _ := nodes[0].(map[string]any)
	for _, key := range []string{
		"price_cents", "monthly_cents", "remaining_value_cents", "remaining_days",
		"price_cny_cents", "monthly_cny_cents", "remaining_value_cny_cents", "cny_converted",
		"expires_text", "expires_at",
	} {
		if _, ok := first[key]; !ok {
			t.Errorf("访客的节点里缺少价格/到期字段 %q —— 这些是**公开**的（用户明确要求）", key)
		}
	}
}

// TestGuestNodeFieldsAreWhitelisted 直接对白名单函数本身钉一遍：
// 私有字段不是"值被清空"，而是**键不存在**。
func TestGuestNodeFieldsAreWhitelisted(t *testing.T) {
	dto := nodeDTO{
		ID: 7, Name: "whitelist-01", Note: "秘密备注",
		ObservedIP: "203.0.113.9", LocalIP: "10.0.0.5", LocalIP6: "fd00::5", BootID: "boot-abc",
		PriceCents: 7121, MonthlyCents: 593, RemainingValueCents: 1200,
	}
	out := guestNodeJSON(dto)

	for _, key := range guestPrivateNodeFields {
		if _, ok := out[key]; ok {
			t.Errorf("guestNodeJSON 里出现了私有键 %q（脱敏必须是「键不存在」，不是「值为空」）", key)
		}
	}
	for _, key := range []string{"id", "name", "price_cents", "monthly_cents", "remaining_value_cents"} {
		if _, ok := out[key]; !ok {
			t.Errorf("guestNodeJSON 里缺少公开键 %q", key)
		}
	}
	// 值没有被改写：访客看到的数字必须与管理员看到的一模一样。
	if out["price_cents"] != float64(7121) {
		t.Errorf("guestNodeJSON 改动了 price_cents：%v", out["price_cents"])
	}
}

// ---------------------------------------------------------------- SSE

// TestGuestStreamSnapshotIsRedacted 连上实时流，断言推过来的快照里也没有私有字段。
//
// 为什么单独测：首页每秒靠 SSE 更新，快照与增量是**另一条编码路径**
// （hub 的按角色广播）。只给 HTTP 响应脱敏时，IP 会从这条流里漏出去 ——
// 这是整个功能里最容易漏的地方。
func TestGuestStreamSnapshotIsRedacted(t *testing.T) {
	h := newAuthHarness(t)
	guestSample(t, h)
	guestOn(t, h)
	h.anonymousClient(t)

	reader, stop := startSSE(t, h)
	defer stop()

	first := reader.next(t, 5*time.Second)
	if first["type"] != "nodes" {
		t.Fatalf("首个事件类型 = %v", first["type"])
	}
	nodes, _ := first["nodes"].([]any)
	if len(nodes) == 0 {
		t.Fatal("访客的快照里没有任何节点（那样下面的断言会平凡通过）")
	}
	node, _ := nodes[0].(map[string]any)

	private := guestPrivateKeyUniverse()
	keys := map[string]bool{}
	jsonKeySet(first, keys)
	for key := range keys {
		if private[key] {
			t.Errorf("SSE 快照里出现了私有字段 %q（整份：%v）", key, first)
		}
	}
	// 该有的还在：快照不是被整块砍空了。
	for _, key := range []string{"id", "name", "status", "price_cents", "cpu_pct", "traffic_cycle_total"} {
		if _, ok := node[key]; !ok {
			t.Errorf("SSE 快照里缺少公开字段 %q", key)
		}
	}
	// 汇总也是访客该看的（在线数/总数）。
	summary, _ := first["summary"].(map[string]any)
	if summary == nil || summary["total"] == nil {
		t.Fatalf("SSE 快照缺少 summary：%v", first["summary"])
	}
}

// TestAdminStreamStillHasPrivateFields 反向钉住"管理员那一份没有被误伤"。
//
// 与上一条成对：只测访客那份的话，一个"给所有人都脱敏"的实现也能通过 ——
// 而那会把管理员自己的 IP 也藏掉（他不是访客，没理由看不到）。
func TestAdminStreamStillHasPrivateFields(t *testing.T) {
	h := newAuthHarness(t)
	guestSample(t, h)
	guestOn(t, h)

	reader, stop := startSSE(t, h)
	defer stop()

	first := reader.next(t, 5*time.Second)
	nodes, _ := first["nodes"].([]any)
	if len(nodes) == 0 {
		t.Fatal("快照里没有任何节点")
	}
	node, _ := nodes[0].(map[string]any)
	for _, key := range []string{"observed_ip", "local_ip", "local_ip6", "note", "boot_id"} {
		if _, ok := node[key]; !ok {
			t.Errorf("管理员的 SSE 快照里缺少 %q（脱敏只该对访客生效）", key)
		}
	}
}

// TestMixedAudienceStreamsEachGetTheirOwnView 管理员与访客**同时**连着实时流时，
// 各自拿到属于自己的那一份。
//
// 为什么必须单独测这一条：hub 本来是"一份负载发给所有人"的结构，为了这个功能
// 才改成按角色分发（见 hub.broadcastTo 与 realtimeLoop 里那两次编码）。
// 上面两条用例各连一种身份、**分开**跑，是发现不了"两份负载发反了"的 ——
// 而发反正是最危险的形态：管理员正在看面板，访客一进来就把完整视图推给了他
// （IP 从流里出去，页面上什么都看不出来）。
func TestMixedAudienceStreamsEachGetTheirOwnView(t *testing.T) {
	h := newAuthHarness(t)
	id := guestSample(t, h)
	guestOn(t, h)

	// 先连管理员（带会话）。
	adminReader, stopAdmin := startSSE(t, h)
	defer stopAdmin()
	if n := firstNode(t, adminReader.next(t, 5*time.Second)); n == nil {
		t.Fatal("管理员的快照里没有节点")
	}

	// 再连访客（同一个服务端，去掉会话）。
	h.anonymousClient(t)
	guestReader, stopGuest := startSSE(t, h)
	defer stopGuest()
	if n := firstNode(t, guestReader.next(t, 5*time.Second)); n == nil {
		t.Fatal("访客的快照里没有节点")
	}
	if !h.srv.hub.hasGuest() {
		t.Fatal("hub 应当知道此刻有访客连着（否则它不会编码访客那一份）")
	}

	// 触发一次真实的变更：两条流都该收到这个节点的推送，但内容不同。
	h.srv.State().Update(id, 1, protocol.Metrics{
		CPUPct: 77, Mem: protocol.Mem{Total: 1 << 30, Used: 1 << 29, Pct: 50},
		Net:       protocol.Net{Iface: "eth0", BootID: "boot-xyz", RxRate: 4096, TxRate: 8192},
		UptimeSec: 7200,
	}, 0, time.Now())

	touched := func(p map[string]any) bool {
		n := firstNode(t, p)
		return n != nil && n["cpu_pct"] == 77.0
	}
	adminNode := firstNode(t, adminReader.nextMatching(t, 8*time.Second, "管理员收到 cpu_pct=77", touched))
	guestNode := firstNode(t, guestReader.nextMatching(t, 8*time.Second, "访客收到 cpu_pct=77", touched))
	if adminNode == nil || guestNode == nil {
		t.Fatal("两条流都应当收到这次变更")
	}

	// 管理员那份带着地址；访客那份一个私有键都没有。
	if _, ok := adminNode["observed_ip"]; !ok {
		t.Error("管理员那一份应当带着 observed_ip（他本来就看得见）")
	}
	for _, key := range guestPrivateNodeFields {
		if _, ok := guestNode[key]; ok {
			t.Errorf("访客那一份里出现了 %q —— 管理员与访客的负载发反了？", key)
		}
	}
}

// ---------------------------------------------------------------- 其它加固

// TestGuestReadsAreRateLimited 访客读接口有粗限流（公开之后会有机器人扫）。
func TestGuestReadsAreRateLimited(t *testing.T) {
	h := newAuthHarness(t)
	createNodeOverHTTP(t, h, "rate-limit")
	guestOn(t, h)
	h.anonymousClient(t)

	// 直接把限流计满（不是真发 300 个请求：那既慢又只是在测 HTTP 栈）。
	ip := "203.0.113.77"
	now := time.Now()
	for i := 0; i < guestReadLimit; i++ {
		if ok, _ := h.srv.guestReads.allowed(ip, now); !ok {
			t.Fatalf("第 %d 次就被拦了，配额比预期小", i+1)
		}
	}
	if ok, retry := h.srv.guestReads.allowed(ip, now); ok {
		t.Fatalf("超过 %d 次/分钟之后应当被拦", guestReadLimit)
	} else if retry <= 0 {
		t.Fatalf("应当给出重试等待时间，实际 %s", retry)
	}
	// 别的来源不受牵连。
	if ok, _ := h.srv.guestReads.allowed("198.51.100.7", now); !ok {
		t.Fatal("限流不该跨来源生效")
	}
	// 管理员不受这个限流影响：把匿名客户端换回带会话的，连发几十个请求都应当 200。
	// （限流是在 guestOrAdmin 的"访客"分支里做的，管理员根本走不到那一步。）
	for i := 0; i < 20; i++ {
		if status, _ := h.get(t, "/api/v1/nodes"); status != http.StatusOK {
			t.Fatalf("管理员第 %d 次读取被限流了（%d）", i+1, status)
		}
	}
}

// TestGuestRateLimitedResponseIsJSONEnvelope 限流时的响应仍然是 JSON 错误信封
// （前端的 api() 只认这一种形状，纯文本会让提示变成"未知错误"）。
func TestGuestRateLimitedResponseIsJSONEnvelope(t *testing.T) {
	h := newAuthHarness(t)
	createNodeOverHTTP(t, h, "rate-limit-json")
	guestOn(t, h)

	// 把这条 IP 的配额耗光：之后一个真实的 HTTP 请求就会撞上限流。
	ip := "127.0.0.1"
	now := time.Now()
	for i := 0; i < guestReadLimit; i++ {
		h.srv.guestReads.allowed(ip, now)
	}
	h.anonymousClient(t)

	status, body := h.get(t, "/api/v1/nodes")
	if status != http.StatusTooManyRequests {
		t.Fatalf("配额用尽应当 429，实际 %d（%v）", status, body)
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj == nil || errObj["code"] != "too_many_attempts" {
		t.Fatalf("429 的响应体不是标准错误信封: %v", body)
	}
}

// rawResponse 发一个 GET 并完整读回响应。
//
// 为什么不用 h.do：它会在返回前关掉 body（并且把内容解析成 JSON），
// 而这里要看的是原始字节与响应头。
func rawResponse(t *testing.T, h *authHarness, path string) (int, http.Header, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, h.ts.URL+path, nil)
	if err != nil {
		t.Fatalf("构造请求 %s: %v", path, err)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("请求 %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取 %s: %v", path, err)
	}
	return resp.StatusCode, resp.Header, raw
}

// TestRobotsAndNoIndexHeader 面板不该被搜索引擎收录：三道一起。
func TestRobotsAndNoIndexHeader(t *testing.T) {
	h := newAuthHarness(t)
	h.anonymousClient(t)

	status, header, raw := rawResponse(t, h, "/robots.txt")
	if status != http.StatusOK {
		t.Fatalf("robots.txt 状态码 = %d", status)
	}
	if !strings.Contains(string(raw), "Disallow: /") {
		t.Errorf("robots.txt 应当禁止抓取整站，实际：%q", raw)
	}
	if ct := header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("robots.txt 的 Content-Type = %q", ct)
	}
	// 它必须能被爬虫在**没有会话**时读到。
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if !strings.HasPrefix(line, "User-agent:") && !strings.HasPrefix(line, "Disallow:") {
			t.Errorf("robots.txt 里有无法识别的行：%q", line)
		}
	}

	// 每个响应都要带 X-Robots-Tag（meta 标签只对渲染 HTML 的爬虫有效）。
	for _, path := range []string{"/", "/robots.txt", "/api/v1/session", "/app.js"} {
		_, header, _ := rawResponse(t, h, path)
		if got := header.Get("X-Robots-Tag"); !strings.Contains(got, "noindex") {
			t.Errorf("%s 的 X-Robots-Tag = %q，应当包含 noindex", path, got)
		}
	}

	// index.html 里那份 meta 也还在（第三道）。
	html := readAsset(t, "index.html")
	if !strings.Contains(html, `name="robots"`) {
		t.Error("index.html 里缺少 <meta name=\"robots\">")
	}
}
