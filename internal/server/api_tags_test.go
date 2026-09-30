package server

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// allowedTagLen 与存储层的 maxTagLen 对齐（16 个字符）。
//
// 这里写死数字而不是 import 存储层的常量：那两个常量没有导出，而且测试要钉的是
// "接口拒绝超长标签"这个行为，数字本身是契约的一部分。
const allowedTagLen = 16

// 标签从创建 → 列表/详情 → 修改（PUT）→ 落库 → 审计 → 非法输入 400 的完整链路。
//
// 这条用例同时钉住一件事：改标签走的就是**现有的节点更新接口**（没有新开接口），
// 而且 PUT 与 PATCH 是同一个处理函数（设置页的「编辑标签」用 PUT）。
func TestNodeTagsAPI(t *testing.T) {
	h := newAuthHarness(t)

	// 创建：空白、重复、空串都要被清掉。
	status, body := h.post(t, "/api/v1/nodes", map[string]any{
		"name": "tag-01", "interval_sec": 1,
		"tags": []string{" 探针 ", "搜索", "探针", "   "},
	}, nil)
	if status != http.StatusCreated {
		t.Fatalf("创建节点失败: %d %v", status, body)
	}
	node, _ := body["node"].(map[string]any)
	if got := tagList(t, node); len(got) != 2 || got[0] != "探针" || got[1] != "搜索" {
		t.Fatalf("创建接口返回的标签没有归一化: %v", got)
	}

	// 列表与详情：两个接口读的是同一个 DTO，都要带上标签。
	status, list := h.get(t, "/api/v1/nodes")
	if status != http.StatusOK {
		t.Fatalf("查询节点失败: %d", status)
	}
	if got := tagList(t, firstNodeFromList(t, list)); len(got) != 2 || got[0] != "探针" {
		t.Fatalf("列表里的标签不对: %v", got)
	}
	status, detail := h.get(t, "/api/v1/nodes/1")
	if status != http.StatusOK {
		t.Fatalf("查询详情失败: %d", status)
	}
	detailNode, _ := detail["node"].(map[string]any)
	if got := tagList(t, detailNode); len(got) != 2 {
		t.Fatalf("详情里的标签不对: %v", got)
	}

	// 改标签走 PUT /api/v1/nodes/{id}（整体替换语义：必须带上全部字段，
	// 只发 tags 会把名称等字段冲成空值，被存储层拒掉 —— 见下面那条断言）。
	before, err := h.srv.db.NodeByID(context.Background(), 1)
	if err != nil {
		t.Fatalf("读取节点: %v", err)
	}
	status, body, _ = h.do(t, http.MethodPut, "/api/v1/nodes/1", map[string]any{
		"name": before.Name, "group_name": "小业务", "region": "HK", "note": "备注",
		"interval_sec": before.IntervalSec, "traffic_warn_pct": 80, "reset_day": 1,
		"traffic_limit": int64(1) << 40, "expires_at": 0,
		"tags": []string{"搜索", "探针", " 新标签 "},
	}, true, nil)
	if status != http.StatusOK {
		t.Fatalf("PUT 改标签失败: %d %v", status, body)
	}
	node, _ = body["node"].(map[string]any)
	got := tagList(t, node)
	if len(got) != 3 || got[0] != "搜索" || got[2] != "新标签" {
		t.Fatalf("PUT 之后的标签不对: %v", got)
	}

	// 落库了，而且没冲掉别的字段。
	saved, err := h.srv.db.NodeByID(context.Background(), 1)
	if err != nil {
		t.Fatalf("读取节点: %v", err)
	}
	if len(saved.Tags) != 3 || saved.Tags[0] != "搜索" {
		t.Fatalf("库里的标签不对: %v", saved.Tags)
	}
	if saved.Name != before.Name || saved.GroupName != "小业务" || saved.Region != "HK" ||
		saved.Note != "备注" || saved.TrafficLimit != int64(1)<<40 {
		t.Fatalf("改标签把其它字段冲掉了: %+v", saved)
	}

	// 审计：照现有 node_update 的写法，并且写清标签变成了什么。
	var action, auditDetail string
	err = h.srv.db.Reader().QueryRowContext(context.Background(),
		`SELECT action, detail FROM audit_log ORDER BY id DESC LIMIT 1`).Scan(&action, &auditDetail)
	if err != nil {
		t.Fatalf("读取审计: %v", err)
	}
	if action != "node_update" {
		t.Fatalf("审计动作 = %q，期望 node_update", action)
	}
	if !strings.Contains(auditDetail, "新标签") {
		t.Fatalf("审计详情里应当写清改成了哪些标签，实际 %q", auditDetail)
	}

	// 只发 tags（不带别的字段）会被整体替换语义拒掉，而不是把名称等字段悄悄冲掉。
	status, body, _ = h.do(t, http.MethodPut, "/api/v1/nodes/1",
		map[string]any{"tags": []string{"只发标签"}}, true, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("只带 tags 的 PUT 应当 400（整体替换语义），实际 %d %v", status, body)
	}
	if again, err := h.srv.db.NodeByID(context.Background(), 1); err != nil || again.Name != before.Name {
		t.Fatalf("被拒的请求不该改动数据: %+v %v", again, err)
	}

	// 非法标签 → 400，且消息里点明是哪个标签。
	long := strings.Repeat("汉", allowedTagLen+1)
	status, body, _ = h.do(t, http.MethodPut, "/api/v1/nodes/1", map[string]any{
		"name": before.Name, "interval_sec": 1, "traffic_warn_pct": 80, "reset_day": 1,
		"tags": []string{"正常", long},
	}, true, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("超长标签应当 400，实际 %d %v", status, body)
	}
	if msg := errMessage(body); !strings.Contains(msg, long) {
		t.Fatalf("400 的消息里应当点明是哪个标签，实际 %q", msg)
	}

	// 超过 8 个 → 400。
	status, body, _ = h.do(t, http.MethodPut, "/api/v1/nodes/1", map[string]any{
		"name": before.Name, "interval_sec": 1, "traffic_warn_pct": 80, "reset_day": 1,
		"tags": []string{"1", "2", "3", "4", "5", "6", "7", "8", "9"},
	}, true, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("超过 8 个标签应当 400，实际 %d %v", status, body)
	}

	// 创建接口同样挡（前端拦一道，服务端是真正生效的那一道）。
	status, _ = h.post(t, "/api/v1/nodes", map[string]any{
		"name": "tag-bad", "interval_sec": 1, "tags": []string{long},
	}, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("创建时的超长标签应当 400，实际 %d", status)
	}
}

// 空标签必须是 []，不能是 null：前端拿到 null 会多一条"这里可能是空的"判断，
// 而标签行的显隐只看长度。
func TestNodeDTOAlwaysSerializesTagsAsArray(t *testing.T) {
	h := newAuthHarness(t)
	if status, body := h.post(t, "/api/v1/nodes", map[string]any{"name": "no-tags", "interval_sec": 1}, nil); status != http.StatusCreated {
		t.Fatalf("创建节点失败: %d %v", status, body)
	}

	for _, path := range []string{"/api/v1/nodes", "/api/v1/nodes/1"} {
		raw := h.rawBody(t, path)
		if strings.Contains(raw, `"tags":null`) {
			t.Errorf("%s 的响应里出现了 \"tags\":null，空列表必须是 []", path)
		}
		if !strings.Contains(raw, `"tags":[]`) {
			t.Errorf("%s 的响应里没有 \"tags\":[]：%s", path, raw)
		}
	}
}

// 改标签也是写操作：未登录必须 401（PATCH 与 PUT 都要）。
func TestNodeTagsUpdateRequiresLogin(t *testing.T) {
	h := newAuthHarness(t)
	if status, body := h.post(t, "/api/v1/nodes", map[string]any{"name": "tag-401", "interval_sec": 1}, nil); status != http.StatusCreated {
		t.Fatalf("创建节点失败: %d %v", status, body)
	}
	h.anonymousClient(t)

	for _, method := range []string{http.MethodPut, http.MethodPatch} {
		status, _, _ := h.do(t, method, "/api/v1/nodes/1",
			map[string]any{"name": "tag-401", "interval_sec": 1, "tags": []string{"x"}}, true, nil)
		if status != http.StatusUnauthorized {
			t.Fatalf("%s 未登录应当 401，实际 %d", method, status)
		}
	}
}

// tagList 把响应里的 tags 收成 []string；缺失或类型不对都算失败（返回 nil）。
func tagList(t *testing.T, node map[string]any) []string {
	t.Helper()
	raw, ok := node["tags"]
	if !ok {
		t.Fatalf("节点 DTO 里没有 tags 字段: %v", node)
	}
	items, ok := raw.([]any)
	if !ok {
		t.Fatalf("tags 不是数组: %#v", raw)
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		s, ok := item.(string)
		if !ok {
			t.Fatalf("tags 里出现了非字符串元素: %#v", item)
		}
		out = append(out, s)
	}
	return out
}

// errMessage 取错误信封里的消息。
func errMessage(body map[string]any) string {
	if apiErr, ok := body["error"].(map[string]any); ok {
		if msg, ok := apiErr["message"].(string); ok {
			return msg
		}
	}
	return ""
}
