package server

import (
	"net/http"
	"strconv"
	"testing"
	"time"

	"probe/internal/protocol"
)

// 这一对用例盯的是 SSE 变更集里新增的那一段：**删除名单**（streamPayload.Deleted）。
//
// 为什么必须单独有它：SSE 推的是变更集（只含这一拍变了的节点），而"这一拍里没有
// 某个 id"同时意味着"它没变"和"它没了"两件事 —— 浏览器分不开。少了删除名单，
// 节点删掉之后前端没有任何依据去收那张卡片，它会一直留到整页刷新（用户报的
// 就是"详情页点了删除，主页上还留存，刷新之后才没有"）。
//
// 真浏览器那一条在 internal/e2e/deletenode_browser_test.go（"别人删的节点，
// 卡片自己消失"那一幕）；这里只钉协议本身：服务端到底发了什么。

// deletedIDs 把一帧里的 deleted 读成 []int64（JSON 解出来是 []any + float64）。
func deletedIDs(t *testing.T, payload map[string]any) []int64 {
	t.Helper()
	raw, ok := payload["deleted"]
	if !ok || raw == nil {
		return nil
	}
	list, ok := raw.([]any)
	if !ok {
		t.Fatalf("deleted 不是数组: %#v", raw)
	}
	out := make([]int64, 0, len(list))
	for _, item := range list {
		n, ok := item.(float64)
		if !ok {
			t.Fatalf("deleted 里有非数字: %#v", item)
		}
		out = append(out, int64(n))
	}
	return out
}

func containsID(ids []int64, want int64) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// TestStreamNamesDeletedNodes 钉住"节点被删掉的那一拍，服务端点名说它没了"。
func TestStreamNamesDeletedNodes(t *testing.T) {
	h := newAuthHarness(t)
	goneID, _ := createNodeOverHTTP(t, h, "gone-01")
	keepID, _ := createNodeOverHTTP(t, h, "keep-01")

	reader, stop := startSSE(t, h)
	defer stop()

	// 连上先拿全量快照：两台都在，而且这一帧必须自称**全量**（前端要靠它做差集：
	// 断线期间被删掉的节点不会出现在任何一帧变更集里，只有全量帧收得掉）。
	snapshot := reader.next(t, 5*time.Second)
	if nodes, _ := snapshot["nodes"].([]any); len(nodes) != 2 {
		t.Fatalf("快照里节点数 = %d，期望 2", len(nodes))
	}
	if snapshot["full"] != true {
		t.Errorf("快照帧的 full = %v，期望 true（它必须自称全量列表，见 streamPayload.Full）",
			snapshot["full"])
	}
	if ids := deletedIDs(t, snapshot); len(ids) != 0 {
		t.Errorf("全量快照里不该有删除名单: %v", ids)
	}

	// 等服务端**见过**这两台机器。删除名单是拿"上一拍见过的 id"做出来的，
	// 所以必须先跨过一拍，否则删除无从判定（1 Hz 的循环，1.2 秒必然跨过一拍）。
	time.Sleep(1200 * time.Millisecond)

	status, body, _ := h.do(t, http.MethodDelete, "/api/v1/nodes/"+strconv.FormatInt(goneID, 10), nil, true, nil)
	if status != http.StatusOK && status != http.StatusNoContent {
		t.Fatalf("删除节点失败: %d %v", status, body)
	}

	frame := reader.nextMatching(t, 5*time.Second, "点名删除了 gone-01 的推送", func(p map[string]any) bool {
		return containsID(deletedIDs(t, p), goneID)
	})
	ids := deletedIDs(t, frame)
	// 变更集不是全量帧：标错了前端就会照它做差集，把所有没变过的卡片删掉。
	if _, ok := frame["full"]; ok {
		t.Errorf("变更集帧不该带 full（那是全量帧的标记）: %v", frame["full"])
	}
	if containsID(ids, keepID) {
		t.Errorf("被删的只有 %d，删除名单里却还带着 %d: %v", goneID, keepID, ids)
	}
	// 删掉的那台不许再出现在变更集里（它是被删名单点名的，不是"变了"）。
	for _, raw := range frame["nodes"].([]any) {
		if n, _ := raw.(map[string]any); int64(n["id"].(float64)) == goneID {
			t.Errorf("删掉的节点不该还留在 nodes 里: %v", n)
		}
	}
	// 汇总始终是全量的：现在只剩一台。
	if summary, _ := frame["summary"].(map[string]any); summary["total"] != float64(1) {
		t.Errorf("删除后的汇总 total = %v，期望 1", summary["total"])
	}
	t.Logf("删除 %d 之后收到的帧：deleted=%v，nodes 里剩 %d 台", goneID, ids, len(frame["nodes"].([]any)))

	// 删除名单只发一次：下一拍不该再重复点名（否则前端会一遍遍去删同一张卡片）。
	// 这一拍未必有帧（没有任何变化时循环不发），所以只断言"再收到的帧里没有它"。
	if next, ok := reader.tryNext(1500 * time.Millisecond); ok {
		if containsID(deletedIDs(t, next), goneID) {
			t.Errorf("同一个节点被重复点名删除: %v", deletedIDs(t, next))
		}
	}
}

// TestStreamKeepsDisabledNodesOutOfDeleted 钉住"停用 ≠ 删除"。
//
// 停用（enabled=false）只是断开 Agent、不再显示在线：节点本身还在列表里、卡片也
// 还在页面上（那张卡片照样要能点开、能编辑）。如果哪天有人把"没在上报的节点"
// 顺手也算进删除名单，这一条会红 —— 表现会是"停用一台机器，首页上那张卡片直接
// 没了"，而用户以为它被删了。
func TestStreamKeepsDisabledNodesOutOfDeleted(t *testing.T) {
	h := newAuthHarness(t)
	liveID, _ := createNodeOverHTTP(t, h, "live-01")
	offID, _ := createNodeOverHTTP(t, h, "off-01")

	reader, stop := startSSE(t, h)
	defer stop()
	_ = reader.next(t, 5*time.Second) // 快照
	time.Sleep(1200 * time.Millisecond)

	// 先让 live-01 在线（这样下面的"催一帧"有东西可推）。
	h.srv.State().Update(liveID, 1, protocol.Metrics{
		CPUPct: 42,
		Net:    protocol.Net{Iface: "eth0"},
	}, 0, time.Now())

	// 停用另一台：走真实接口（它会断开 Agent、写库）。
	status, body, _ := h.do(t, http.MethodPatch, "/api/v1/nodes/"+strconv.FormatInt(offID, 10), map[string]any{
		"name": "off-01", "interval_sec": 1, "reset_day": 1, "enabled": false,
	}, true, nil)
	if status != http.StatusOK {
		t.Fatalf("停用节点失败: %d %v", status, body)
	}

	// 催一帧出来（live-01 的 cpu 变了），再检查这一帧的删除名单。
	frame := reader.nextMatching(t, 6*time.Second, "live-01 的变更", func(p map[string]any) bool {
		n := firstNode(t, p)
		return n != nil && n["cpu_pct"] == 42.0
	})
	if ids := deletedIDs(t, frame); containsID(ids, offID) {
		t.Errorf("停用的节点 %d 被当成了删除: %v（停用只是不再上报，卡片必须留着）", offID, ids)
	}
	if summary, _ := frame["summary"].(map[string]any); summary["total"] != float64(2) {
		t.Errorf("停用之后 total = %v，期望 2（停用不减少台数）", summary["total"])
	}

	// 列表接口里它也还在（前端刷新之后卡片照旧在）。
	_, list, _ := h.do(t, http.MethodGet, "/api/v1/nodes", nil, false, nil)
	if nodes, _ := list["nodes"].([]any); len(nodes) != 2 {
		t.Errorf("停用后列表里节点数 = %d，期望 2", len(nodes))
	}
}
