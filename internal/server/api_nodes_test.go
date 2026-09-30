package server

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"probe/internal/config"
	"probe/internal/protocol"
	"probe/internal/state"
)

func TestCreateNodeReturnsTokenOnce(t *testing.T) {
	h := newAuthHarness(t)

	status, body := h.post(t, "/api/v1/nodes", map[string]any{
		"name": "hk-01", "group_name": "香港", "region": "HK", "interval_sec": 1,
		"traffic_limit": 1 << 40, "reset_day": 19,
	}, nil)
	if status != http.StatusCreated {
		t.Fatalf("创建节点失败: %d %v", status, body)
	}
	token, _ := body["token"].(string)
	if len(token) < 40 {
		t.Fatalf("Token 形态不对: %q", token)
	}
	node, _ := body["node"].(map[string]any)
	if node == nil || node["name"] != "hk-01" {
		t.Fatalf("返回的节点不对: %v", body)
	}
	if node["status"] != string(state.StatusUnknown) {
		t.Fatalf("新节点状态应当是 unknown，实际 %v", node["status"])
	}

	// 列表里不能再出现 Token。
	status, list := h.get(t, "/api/v1/nodes")
	if status != http.StatusOK {
		t.Fatalf("查询节点失败: %d", status)
	}
	nodes, _ := list["nodes"].([]any)
	if len(nodes) != 1 {
		t.Fatalf("节点数 = %d", len(nodes))
	}
	first, _ := nodes[0].(map[string]any)
	for key := range first {
		if key == "token" || key == "token_hash" {
			t.Fatalf("列表接口泄漏了字段 %q", key)
		}
	}
	summary, _ := list["summary"].(map[string]any)
	if summary["total"] != float64(1) || summary["unknown"] != float64(1) {
		t.Fatalf("汇总不对: %v", summary)
	}

	// 重名 → 409。
	status, _ = h.post(t, "/api/v1/nodes", map[string]any{"name": "hk-01", "interval_sec": 1}, nil)
	if status != http.StatusConflict {
		t.Fatalf("重名应当返回 409，实际 %d", status)
	}

	// 非法参数 → 400。
	status, _ = h.post(t, "/api/v1/nodes", map[string]any{"name": "  ", "interval_sec": 1}, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("空名称应当返回 400，实际 %d", status)
	}
	status, _ = h.post(t, "/api/v1/nodes", map[string]any{"name": "bad-interval", "interval_sec": 999}, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("非法间隔应当返回 400，实际 %d", status)
	}

	// 未知字段（拼错的字段名）应当被拒绝，避免静默失败。
	status, _ = h.post(t, "/api/v1/nodes", map[string]any{"name": "typo", "interval_secx": 1}, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("未知字段应当返回 400，实际 %d", status)
	}
}

func TestNodeListReflectsLiveState(t *testing.T) {
	// 用很短的阈值：这样可以真的"等时间流逝"来观察掉线，而不是伪造时间。
	cfg := config.Default()
	cfg.StaleAfter = 60 * time.Millisecond
	cfg.OfflineAfter = 120 * time.Millisecond
	h := newAuthHarnessWithConfig(t, cfg)

	status, body := h.post(t, "/api/v1/nodes", map[string]any{"name": "live-01", "interval_sec": 1}, nil)
	if status != http.StatusCreated {
		t.Fatalf("创建节点失败: %d %v", status, body)
	}
	node, _ := body["node"].(map[string]any)
	id := int64(node["id"].(float64))

	// 模拟 Agent 上报（直接写内存状态，等价于 Phase 4 链路的结果）。
	m := protocol.Metrics{
		CPUPct: 12.5,
		Mem:    protocol.Mem{Total: 1 << 30, Used: 1 << 29, Pct: 50},
		Disk:   []protocol.Disk{{Mount: "/", FS: "ext4", Total: 1 << 40, Used: 1 << 39, Pct: 50}},
		Load:   protocol.Load{L1: 0.5},
		Net:    protocol.Net{Iface: "eth0", RxTotal: 1 << 20, TxTotal: 1 << 19, RxRate: 1024, TxRate: 512},
		LatMS:  23.4, UptimeSec: 3600,
	}
	h.srv.State().Update(id, 1, m, 0, time.Now())

	status, list := h.get(t, "/api/v1/nodes")
	if status != http.StatusOK {
		t.Fatalf("查询失败: %d", status)
	}
	got := firstNodeFromList(t, list)
	if got["status"] != string(state.StatusOnline) || got["connected"] != true {
		t.Fatalf("刚上报完应当是 online/connected: %v", got)
	}
	if got["cpu_pct"] != 12.5 || got["mem_pct"] != 50.0 || got["disk_pct"] != 50.0 {
		t.Fatalf("指标没有进入 DTO: %v", got)
	}
	if got["rx_rate"] != 1024.0 || got["lat_ms"] != 23.4 {
		t.Fatalf("速率/延迟不对: %v", got)
	}

	// 没有新数据、时间流逝 → 自动变成离线（这就是 Agent 掉线的真实机制）。
	time.Sleep(200 * time.Millisecond)
	status, list = h.get(t, "/api/v1/nodes")
	if status != http.StatusOK {
		t.Fatalf("查询失败: %d", status)
	}
	got = firstNodeFromList(t, list)
	if got["status"] != string(state.StatusOffline) {
		t.Fatalf("长时间无通信应当是 offline，实际 %v", got["status"])
	}
	if got["last_seen"] == float64(0) {
		t.Fatal("掉线后仍应显示最后一次通信时间")
	}

	// 连接关闭后 connected 变 false（状态由 LastSeen 决定，两者互不影响）。
	h.srv.State().Detach(id, 1)
	status, list = h.get(t, "/api/v1/nodes")
	if status != http.StatusOK {
		t.Fatalf("查询失败: %d", status)
	}
	got = firstNodeFromList(t, list)
	if got["connected"] != false {
		t.Fatalf("Detach 之后 connected 应为 false: %v", got["connected"])
	}
}

func firstNodeFromList(t *testing.T, list map[string]any) map[string]any {
	t.Helper()
	nodes, _ := list["nodes"].([]any)
	if len(nodes) == 0 {
		t.Fatal("节点列表为空")
	}
	node, _ := nodes[0].(map[string]any)
	return node
}

// 价格字段：创建 → 派生值 → 列表/详情 → PATCH 修改 → 落库 → 半残状态 400。
//
// 派生值（月均/剩余天数/剩余价值）必须由服务端算：PC 与手机、详情页与首页
// 都读同一份数字，前端各算一遍早晚会出现两处对不上。
func TestNodePriceFieldsAndDerivedValues(t *testing.T) {
	h := newAuthHarness(t)

	// 多给两小时余量：剩余天数按 (expires_at - now)/86400 向下取整，
	// 卡在整秒边界上会随机少一天（测试变成偶发失败）。
	expiresAt := time.Now().Add(161*24*time.Hour + 2*time.Hour).Unix()
	status, body := h.post(t, "/api/v1/nodes", map[string]any{
		"name": "price-01", "interval_sec": 1,
		"price_cents": 7121, "currency": "cny", "billing_months": 12,
		"expires_at": expiresAt,
	}, nil)
	if status != http.StatusCreated {
		t.Fatalf("创建节点失败: %d %v", status, body)
	}
	node, _ := body["node"].(map[string]any)
	if node["price_cents"] != float64(7121) || node["billing_months"] != float64(12) {
		t.Fatalf("价格字段没有回显: %v", node)
	}
	if node["currency"] != "CNY" {
		t.Fatalf("小写货币应当被统一成大写，实际 %v", node["currency"])
	}
	if node["monthly_cents"] != float64(593) { // 7121 / 12 整除
		t.Fatalf("月均 = %v，期望 593", node["monthly_cents"])
	}
	if node["remaining_days"] != float64(161) {
		t.Fatalf("剩余天数 = %v，期望 161", node["remaining_days"])
	}
	if node["remaining_value_cents"] != float64(3137) { // 593 × 161 / 30.4375 ≈ 3136.7
		t.Fatalf("剩余价值 = %v，期望 3137", node["remaining_value_cents"])
	}

	// 首页列表与详情接口读的是同一个 DTO，也都要带上。
	status, list := h.get(t, "/api/v1/nodes")
	if status != http.StatusOK {
		t.Fatalf("查询节点失败: %d", status)
	}
	if got := firstNodeFromList(t, list); got["price_cents"] != float64(7121) || got["currency"] != "CNY" {
		t.Fatalf("列表里的价格字段不对: %v", got)
	}
	status, detail := h.get(t, "/api/v1/nodes/1")
	if status != http.StatusOK {
		t.Fatalf("查询详情失败: %d", status)
	}
	if node, _ := detail["node"].(map[string]any); node["monthly_cents"] != float64(593) {
		t.Fatalf("详情里的月均不对: %v", node)
	}

	// PATCH 改价格（PATCH 是整体替换语义，必须带全字段）。
	status, body, _ = h.do(t, http.MethodPatch, "/api/v1/nodes/1", map[string]any{
		"name": "price-01", "interval_sec": 1, "traffic_warn_pct": 80, "reset_day": 1,
		"price_cents": 3600, "currency": "usd", "billing_months": 1, "expires_at": 0, "enabled": true,
	}, true, nil)
	if status != http.StatusOK {
		t.Fatalf("修改价格失败: %d %v", status, body)
	}
	node, _ = body["node"].(map[string]any)
	if node["currency"] != "USD" || node["monthly_cents"] != float64(3600) {
		t.Fatalf("月付的价格/月均不对: %v", node)
	}
	if node["remaining_days"] != float64(0) || node["remaining_value_cents"] != float64(0) {
		t.Fatalf("没填到期日时剩余天数/价值应当是 0: %v", node)
	}

	loaded, err := h.srv.db.NodeByID(context.Background(), 1)
	if err != nil {
		t.Fatalf("读取节点: %v", err)
	}
	if loaded.PriceCents != 3600 || loaded.Currency != "USD" || loaded.BillingMonths != 1 {
		t.Fatalf("数据库里的价格不对: %+v", loaded)
	}

	// 半残状态：填了价格没填周期，接口与存储层都要拒（前端也挡一次，见 app.js）。
	status, body = h.post(t, "/api/v1/nodes", map[string]any{
		"name": "price-bad", "interval_sec": 1, "price_cents": 5000,
	}, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("有价格没周期应当 400，实际 %d %v", status, body)
	}
	status, body, _ = h.do(t, http.MethodPatch, "/api/v1/nodes/1", map[string]any{
		"name": "price-01", "interval_sec": 1, "traffic_warn_pct": 80, "reset_day": 1,
		"price_cents": 5000, "billing_months": 0, "enabled": true,
	}, true, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("PATCH 有价格没周期应当 400，实际 %d %v", status, body)
	}
	// 超限价格也要挡住（防前端传进"一亿元以上"的离谱值把派生金额撑爆）。
	status, body = h.post(t, "/api/v1/nodes", map[string]any{
		"name": "price-huge", "interval_sec": 1,
		"price_cents": 1_000_000_000_01, "currency": "CNY", "billing_months": 12,
	}, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("超限价格应当 400，实际 %d %v", status, body)
	}
}

func TestCreateNodeWritesAudit(t *testing.T) {
	h := newAuthHarness(t)
	if status, body := h.post(t, "/api/v1/nodes", map[string]any{"name": "audit-01", "interval_sec": 1}, nil); status != http.StatusCreated {
		t.Fatalf("创建节点失败: %d %v", status, body)
	}

	var action, detail string
	err := h.srv.db.Reader().QueryRowContext(context.Background(),
		`SELECT action, detail FROM audit_log ORDER BY id DESC LIMIT 1`).Scan(&action, &detail)
	if err != nil {
		t.Fatalf("读取审计: %v", err)
	}
	if action != "node_create" {
		t.Fatalf("审计动作 = %q", action)
	}
	if detail == "" {
		t.Fatal("审计详情为空")
	}
}

// listNodeIDs 取一次 /api/v1/nodes，按接口给的顺序返回 id 列表。
//
// 顺序断言必须走这个接口、而不是直接读库：首页卡片的顺序就是它决定的，
// 直接读库只能证明"库里排对了"，证明不了"接口按这个顺序给"。
func listNodeIDs(t *testing.T, h *authHarness) []int64 {
	t.Helper()
	status, list := h.get(t, "/api/v1/nodes")
	if status != http.StatusOK {
		t.Fatalf("查询节点失败: %d", status)
	}
	raw, _ := list["nodes"].([]any)
	ids := make([]int64, 0, len(raw))
	for _, item := range raw {
		node, _ := item.(map[string]any)
		id, _ := node["id"].(float64)
		ids = append(ids, int64(id))
	}
	return ids
}

func sameIDs(got, want []int64) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// 正常重排：PUT /api/v1/nodes/order 之后，列表接口的顺序与请求里的 ids 一致。
func TestReorderNodesAPI(t *testing.T) {
	h := newAuthHarness(t)
	ids := make([]int64, 0, 3)
	for _, name := range []string{"order-a", "order-b", "order-c"} {
		id, _ := createNodeOverHTTP(t, h, name)
		ids = append(ids, id)
	}
	if got := listNodeIDs(t, h); !sameIDs(got, ids) {
		t.Fatalf("创建后的顺序 = %v，期望 %v", got, ids)
	}

	want := []int64{ids[2], ids[0], ids[1]}
	status, body := h.put(t, "/api/v1/nodes/order", map[string]any{"ids": want})
	if status != http.StatusOK {
		t.Fatalf("重排失败: %d %v", status, body)
	}
	if body["count"] != float64(3) {
		t.Fatalf("返回的 count = %v，期望 3", body["count"])
	}
	if got := listNodeIDs(t, h); !sameIDs(got, want) {
		t.Fatalf("重排后的顺序 = %v，期望 %v", got, want)
	}

	// 落库确认：sort_order 就是 1..N（不是只有接口层看着对）。
	nodes, err := h.srv.db.ListNodes(context.Background())
	if err != nil {
		t.Fatalf("读库: %v", err)
	}
	for i, id := range want {
		if nodes[i].ID != id || nodes[i].SortOrder != i+1 {
			t.Fatalf("第 %d 台：id=%d sort_order=%d，期望 id=%d sort_order=%d",
				i+1, nodes[i].ID, nodes[i].SortOrder, id, i+1)
		}
	}

	// 审计：动作与详情（详情里要有台数）。
	var action, detail string
	if err := h.srv.db.Reader().QueryRowContext(context.Background(),
		`SELECT action, detail FROM audit_log ORDER BY id DESC LIMIT 1`).Scan(&action, &detail); err != nil {
		t.Fatalf("读取审计: %v", err)
	}
	if action != "node_order" {
		t.Fatalf("审计动作 = %q，期望 node_order", action)
	}
	if !strings.Contains(detail, "调整节点顺序（3 台）") {
		t.Fatalf("审计详情 = %q，期望包含「调整节点顺序（3 台）」", detail)
	}

	// 再来一次：从任意顺序都能重排（不是只能排一次）。
	want2 := []int64{ids[1], ids[2], ids[0]}
	if status, body := h.put(t, "/api/v1/nodes/order", map[string]any{"ids": want2}); status != http.StatusOK {
		t.Fatalf("第二次重排失败: %d %v", status, body)
	}
	if got := listNodeIDs(t, h); !sameIDs(got, want2) {
		t.Fatalf("第二次重排后的顺序 = %v，期望 %v", got, want2)
	}
}

// 校验：ids 必须恰好是全部节点 —— 缺/多/重复/不存在都是 400 bad_request，
// 且消息里要点名是哪个 id 不对（只报"顺序不合法"等于让调用方自己猜）。
func TestReorderNodesValidation(t *testing.T) {
	h := newAuthHarness(t)
	var ids []int64
	for _, name := range []string{"v-a", "v-b", "v-c"} {
		id, _ := createNodeOverHTTP(t, h, name)
		ids = append(ids, id)
	}

	cases := []struct {
		name    string
		payload map[string]any
		words   []string
	}{
		{"缺一个", map[string]any{"ids": []int64{ids[0], ids[1]}}, []string{"缺少", strconv.FormatInt(ids[2], 10)}},
		{"多一个", map[string]any{"ids": []int64{ids[0], ids[1], ids[2], 999}}, []string{"999"}},
		{"重复", map[string]any{"ids": []int64{ids[0], ids[1], ids[1]}}, []string{"重复"}},
		{"不存在的 id", map[string]any{"ids": []int64{ids[0], ids[1], 999}}, []string{"999"}},
		{"空列表", map[string]any{"ids": []int64{}}, []string{"缺少"}},
		{"根本没带 ids 字段", map[string]any{}, []string{"缺少"}},
	}
	for _, tc := range cases {
		status, body, _ := h.do(t, http.MethodPut, "/api/v1/nodes/order", tc.payload, true, nil)
		if status != http.StatusBadRequest {
			t.Errorf("%s → %d，期望 400（%v）", tc.name, status, body)
			continue
		}
		apiErr, _ := body["error"].(map[string]any)
		if apiErr["code"] != "bad_request" {
			t.Errorf("%s 的错误码 = %v，期望 bad_request", tc.name, apiErr["code"])
		}
		message, _ := apiErr["message"].(string)
		if message == "" {
			t.Errorf("%s 的错误消息为空", tc.name)
		}
		for _, word := range tc.words {
			if !strings.Contains(message, word) {
				t.Errorf("%s 的错误消息 %q 里没有 %q", tc.name, message, word)
			}
		}
	}

	// 校验失败不能改动顺序。
	if got := listNodeIDs(t, h); !sameIDs(got, ids) {
		t.Fatalf("校验失败后顺序被改动了：%v，期望 %v", got, ids)
	}
}

// 空库传空列表是允许的：前端拖一个空列表本来就不该报错。
func TestReorderNodesEmptyLibrary(t *testing.T) {
	h := newAuthHarness(t)
	status, body := h.put(t, "/api/v1/nodes/order", map[string]any{"ids": []int64{}})
	if status != http.StatusOK {
		t.Fatalf("空库重排空列表应当 200，实际 %d %v", status, body)
	}
	if body["count"] != float64(0) {
		t.Fatalf("count = %v，期望 0", body["count"])
	}
}

// 未登录 401、缺 CSRF 403 —— 与其它写接口同一套中间件。
func TestReorderNodesRequiresLoginAndCSRF(t *testing.T) {
	h := newAuthHarness(t)
	id, _ := createNodeOverHTTP(t, h, "guard-a")
	payload := map[string]any{"ids": []int64{id}}

	// 缺 CSRF。
	status, body, _ := h.do(t, http.MethodPut, "/api/v1/nodes/order", payload, false, nil)
	if status != http.StatusForbidden {
		t.Fatalf("缺 CSRF 应当 403，实际 %d %v", status, body)
	}
	if apiErr, _ := body["error"].(map[string]any); apiErr["code"] != "bad_csrf" {
		t.Fatalf("错误码 = %v，期望 bad_csrf", apiErr["code"])
	}

	// 未登录（换一个没有会话 Cookie 的客户端）。
	h.anonymousClient(t)
	status, body, _ = h.do(t, http.MethodPut, "/api/v1/nodes/order", payload, true, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("未登录应当 401，实际 %d %v", status, body)
	}
}

// 路由共存："PUT /api/v1/nodes/order" 与 "PUT /api/v1/nodes/{id}"。
//
// Go 1.22 的 ServeMux 里字面量路径比通配更具体、优先匹配，两条能同时注册
// 且各走各的。这条规则不显眼，而且坏掉的方式很隐蔽 —— /nodes/order 会被
// {id} 那条吃掉，表现成"重排接口返回 400 节点 ID 非法"。所以这里用**两种
// 处理函数的不同响应**把它钉住：
//   - 重排接口回 {"count": N}（没有 node 字段）；
//   - 更新接口回 {"node": {...}}。
func TestReorderNodesRouteCoexistsWithNodeRoute(t *testing.T) {
	h := newAuthHarness(t)
	var ids []int64
	for _, name := range []string{"route-a", "route-b"} {
		id, _ := createNodeOverHTTP(t, h, name)
		ids = append(ids, id)
	}

	status, body := h.put(t, "/api/v1/nodes/order", map[string]any{"ids": []int64{ids[1], ids[0]}})
	if status != http.StatusOK {
		t.Fatalf("PUT /nodes/order 应当命中重排接口，实际 %d %v", status, body)
	}
	if _, ok := body["count"]; !ok {
		t.Fatalf("PUT /nodes/order 的响应里没有 count（%v）：多半被 {id} 那条路由吃掉了", body)
	}
	if _, ok := body["node"]; ok {
		t.Fatalf("PUT /nodes/order 返回了 node 字段：说明它走的是更新节点那条路（%v）", body)
	}
	if got := listNodeIDs(t, h); !sameIDs(got, []int64{ids[1], ids[0]}) {
		t.Fatalf("顺序 = %v，期望 %v", got, []int64{ids[1], ids[0]})
	}

	// 同一台机器的更新接口仍然照常（用 ID 访问不受重排接口影响）。
	status, body = h.put(t, "/api/v1/nodes/"+strconv.FormatInt(ids[0], 10), map[string]any{
		"name": "route-a-renamed", "interval_sec": 1, "traffic_warn_pct": 80, "reset_day": 1,
		"enabled": true,
	})
	if status != http.StatusOK {
		t.Fatalf("PUT /nodes/{id} 失败: %d %v", status, body)
	}
	node, _ := body["node"].(map[string]any)
	if node["name"] != "route-a-renamed" {
		t.Fatalf("PUT /nodes/{id} 没有命中更新接口: %v", body)
	}
}

// 新建节点排到最后：这是"前端不传 sort_order → 0 → 插到最前面"那个 bug 的回归测试。
func TestCreateNodeWithoutSortOrderGoesLastAPI(t *testing.T) {
	h := newAuthHarness(t)
	var ids []int64
	for _, name := range []string{"new-a", "new-b", "new-c"} {
		id, _ := createNodeOverHTTP(t, h, name)
		ids = append(ids, id)
	}
	// 第四台（请求体里没有 sort_order）。
	id, _ := createNodeOverHTTP(t, h, "new-d")

	got := listNodeIDs(t, h)
	want := append(append([]int64{}, ids...), id)
	if !sameIDs(got, want) {
		t.Fatalf("新建的节点应当排在最后：实际 %v，期望 %v", got, want)
	}

	// 显式传 sort_order 时仍然照用（POST 上这个字段以前被直接丢掉了）。
	status, body := h.post(t, "/api/v1/nodes", map[string]any{
		"name": "new-zeroth", "interval_sec": 1, "sort_order": 0,
	}, nil)
	if status != http.StatusCreated {
		t.Fatalf("显式 sort_order 创建失败: %d %v", status, body)
	}
	node, _ := body["node"].(map[string]any)
	explicit, _ := node["id"].(float64)
	got = listNodeIDs(t, h)
	if len(got) == 0 || got[0] != int64(explicit) {
		t.Fatalf("显式 sort_order = 0 应当排在最前，实际顺序 %v", got)
	}

	// 未知字段（拼错的字段名）仍然被拒 —— 加了新字段不该顺手放宽解析。
	status, _ = h.post(t, "/api/v1/nodes", map[string]any{"name": "typo-2", "ids": []int64{1}}, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("未知字段应当 400，实际 %d", status)
	}
}
