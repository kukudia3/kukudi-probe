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
