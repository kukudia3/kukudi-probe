package server

import (
	"context"
	"net/http"
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
