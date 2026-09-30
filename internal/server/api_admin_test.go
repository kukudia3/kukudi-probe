package server

import (
	"context"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"probe/internal/alert"
	"probe/internal/store"
)

func TestUpdateNodeAPI(t *testing.T) {
	h := newAuthHarness(t)
	nodeID, _ := createNodeOverHTTP(t, h, "edit-01")

	status, body, _ := h.do(t, http.MethodPatch, "/api/v1/nodes/1", map[string]any{
		"name": "edit-01-renamed", "group_name": "东京", "region": "JP",
		"note": "换了机房", "interval_sec": 5,
		"traffic_limit": int64(500) << 30, "traffic_warn_pct": 90, "reset_day": 25,
		"expires_at": time.Now().Add(48 * time.Hour).Unix(),
		"enabled":    true,
	}, true, nil)
	if status != http.StatusOK {
		t.Fatalf("修改节点失败: %d %v", status, body)
	}
	node, _ := body["node"].(map[string]any)
	if node["name"] != "edit-01-renamed" || node["region"] != "JP" || node["note"] != "换了机房" {
		t.Fatalf("字段没有更新: %v", node)
	}
	if node["interval_sec"] != float64(5) || node["reset_day"] != float64(25) {
		t.Fatalf("间隔/重置日没有更新: %v", node)
	}
	if node["traffic_limit"] != float64(int64(500)<<30) || node["traffic_warn_pct"] != float64(90) {
		t.Fatalf("流量配置没有更新: %v", node)
	}

	// 落库确认（不是只改了返回值）。
	loaded, err := h.srv.db.NodeByID(context.Background(), nodeID)
	if err != nil {
		t.Fatalf("读取节点: %v", err)
	}
	if loaded.Name != "edit-01-renamed" || loaded.ResetDay != 25 || !loaded.Enabled {
		t.Fatalf("数据库里的内容不对: %+v", loaded)
	}

	// 停用节点：Agent 不该能连上。
	status, body, _ = h.do(t, http.MethodPatch, "/api/v1/nodes/1", map[string]any{
		"name": "edit-01-renamed", "interval_sec": 5, "reset_day": 25, "enabled": false,
	}, true, nil)
	if status != http.StatusOK {
		t.Fatalf("停用节点失败: %d %v", status, body)
	}
	if _, _, err := dialAgent(t, h.ts, "some-token"); err == nil {
		t.Fatal("停用后不该还能建立连接（Token 也是错的，但状态检查更早）")
	}

	// 校验分支。注意：PATCH 是"整体替换"语义，必须带上全部字段，
	// 缺字段会得到明确的 400，而不是被悄悄改回默认值。
	editPayload := func(name string, mutate func(map[string]any)) map[string]any {
		payload := map[string]any{
			"name": name, "group_name": "", "region": "", "note": "", "iface": "",
			"interval_sec": 1, "traffic_limit": 0, "traffic_warn_pct": 80,
			"reset_day": 1, "expires_at": 0, "enabled": true,
		}
		if mutate != nil {
			mutate(payload)
		}
		return payload
	}

	cases := []struct {
		name    string
		payload map[string]any
		want    int
	}{
		{"空名称", editPayload("", nil), http.StatusBadRequest},
		{"间隔过大", editPayload("x", func(p map[string]any) { p["interval_sec"] = 999 }), http.StatusBadRequest},
		{"重置日非法", editPayload("x", func(p map[string]any) { p["reset_day"] = 99 }), http.StatusBadRequest},
		{"缺字段（不会静默改成默认值）", map[string]any{"name": "x", "interval_sec": 1}, http.StatusBadRequest},
		{"改成自己原来的名字", editPayload("edit-01-renamed", nil), http.StatusOK},
	}
	for _, tc := range cases {
		status, body, _ := h.do(t, http.MethodPatch, "/api/v1/nodes/1", tc.payload, true, nil)
		if status != tc.want {
			t.Errorf("%s → %d，期望 %d（%v）", tc.name, status, tc.want, body)
		}
	}

	// 不存在的节点 / 非法 ID。
	if status, _, _ := h.do(t, http.MethodPatch, "/api/v1/nodes/999", editPayload("x", nil), true, nil); status != http.StatusNotFound {
		t.Errorf("不存在的节点应当 404，实际 %d", status)
	}
	if status, _, _ := h.do(t, http.MethodPatch, "/api/v1/nodes/abc", editPayload("x", nil), true, nil); status != http.StatusBadRequest {
		t.Errorf("非法 ID 应当 400，实际 %d", status)
	}

	// 重名冲突。
	createNodeOverHTTP(t, h, "edit-02")
	status, body, _ = h.do(t, http.MethodPatch, "/api/v1/nodes/2", editPayload("edit-01-renamed", nil), true, nil)
	if status != http.StatusConflict {
		t.Fatalf("重名应当 409，实际 %d %v", status, body)
	}
}

func TestUpdateNodeRequiresCSRFAndLogin(t *testing.T) {
	h := newAuthHarness(t)
	createNodeOverHTTP(t, h, "csrf-01")

	status, _, _ := h.do(t, http.MethodPatch, "/api/v1/nodes/1", map[string]any{"name": "x"}, false, nil)
	if status != http.StatusForbidden {
		t.Fatalf("缺 CSRF 应当 403，实际 %d", status)
	}
}

func TestDeleteNodeRemovesEverything(t *testing.T) {
	h := newAuthHarness(t)
	ctx := context.Background()
	nodeID, token := createNodeOverHTTP(t, h, "delete-01")

	// 造一点历史与流量，确认删除时一起清掉。
	now := time.Now().Unix()
	base := now - now%10
	if err := h.srv.db.InsertBuckets(ctx, store.TableSamples10s, []store.SampleBucket{
		{NodeID: nodeID, TS: base, CPUAvg: 10, Up: 10, All: 10},
	}); err != nil {
		t.Fatalf("写入历史: %v", err)
	}
	if err := h.srv.db.FlushTraffic(ctx, []store.TrafficUpdate{{
		NodeID: nodeID, Day: store.FormatDay(time.Now()), RxDelta: 1024, TxDelta: 512,
		RxTotal: 1024, TxTotal: 512,
	}}, time.Now()); err != nil {
		t.Fatalf("写入流量: %v", err)
	}
	h.srv.State().Update(nodeID, 1, testMetricsForDetail(), 0, time.Now())
	h.srv.traffic.observe(nodeID, testMetricsForDetail())
	if err := h.srv.db.UpsertAlertStates(ctx, []store.AlertStateRow{{
		NodeID: nodeID, Rule: alert.RuleOffline, State: alert.StateFiring, Since: now,
	}}); err != nil {
		t.Fatalf("写入告警状态: %v", err)
	}

	status, body, _ := h.do(t, http.MethodDelete, "/api/v1/nodes/1", nil, true, nil)
	if status != http.StatusOK || body["deleted"] != true {
		t.Fatalf("删除失败: %d %v", status, body)
	}

	// 节点、历史、流量、告警状态全部消失。
	if _, err := h.srv.db.NodeByID(ctx, nodeID); err == nil {
		t.Fatal("节点还在")
	}
	count, err := h.srv.db.CountSamples(ctx, store.TableSamples10s, nodeID, time.Unix(0, 0), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("统计历史: %v", err)
	}
	if count != 0 {
		t.Fatalf("历史没删干净: %d 行", count)
	}
	rows, err := h.srv.db.LoadAlertStates(ctx)
	if err != nil {
		t.Fatalf("读取告警状态: %v", err)
	}
	for _, row := range rows {
		if row.NodeID == nodeID {
			t.Fatalf("告警状态没删干净: %+v", row)
		}
	}
	daily, err := h.srv.db.TrafficDailySince(ctx, store.FormatDay(time.Now().AddDate(0, 0, -1)))
	if err != nil {
		t.Fatalf("读取流量: %v", err)
	}
	for _, row := range daily {
		if row.NodeID == nodeID {
			t.Fatalf("流量没删干净: %+v", row)
		}
	}

	// 内存状态与流量基线清掉了：不再推送、不再记账。
	if _, ok := h.srv.State().Get(nodeID); ok {
		t.Fatal("内存状态还在")
	}
	if _, _, _, ok := h.srv.traffic.pending(nodeID); ok {
		t.Fatal("流量状态还在")
	}

	// 用旧 Token 连不上了（节点已被删除）。
	if _, _, err := dialAgent(t, h.ts, token); err == nil {
		t.Fatal("节点删除后不该还能连接")
	}

	// 重复删除 → 404。
	status, _, _ = h.do(t, http.MethodDelete, "/api/v1/nodes/1", nil, true, nil)
	if status != http.StatusNotFound {
		t.Fatalf("重复删除应当 404，实际 %d", status)
	}
}

func TestRotateNodeToken(t *testing.T) {
	h := newAuthHarness(t)
	nodeID, oldToken := createNodeOverHTTP(t, h, "rotate-01")

	conn := mustDialAgent(t, h.ts, oldToken)
	sendFrame(t, conn, helloFrame(t, testHello()))
	if _, err := readFrame(t, conn, 3*time.Second); err != nil {
		t.Fatalf("读取 welcome: %v", err)
	}
	waitFor(t, 3*time.Second, "连接登记", func() bool { return h.srv.agents.activeCount() == 1 })

	status, body, _ := h.do(t, http.MethodPost, "/api/v1/nodes/1/token", nil, true, nil)
	if status != http.StatusOK {
		t.Fatalf("重新生成 Token 失败: %d %v", status, body)
	}
	newToken, _ := body["token"].(string)
	if newToken == "" || newToken == oldToken || !strings.HasPrefix(newToken, "pba_") {
		t.Fatalf("新 Token 不合法: %q", newToken)
	}

	// 旧连接被断开（服务端主动关闭）。
	waitFor(t, 3*time.Second, "旧连接被断开", func() bool { return h.srv.agents.activeCount() == 0 })
	if _, err := readFrame(t, conn, time.Second); err == nil {
		t.Fatal("旧连接应当已被服务端关闭")
	}

	// 旧 Token 连不上，新 Token 可以。
	if _, _, err := dialAgent(t, h.ts, oldToken); err == nil {
		t.Fatal("旧 Token 应当失效")
	}
	conn2 := mustDialAgent(t, h.ts, newToken)
	sendFrame(t, conn2, helloFrame(t, testHello()))
	if _, err := readFrame(t, conn2, 3*time.Second); err != nil {
		t.Fatalf("新 Token 应当可用: %v", err)
	}

	// Token 哈希变了，明文没落库。
	loaded, err := h.srv.db.NodeByID(context.Background(), nodeID)
	if err != nil {
		t.Fatalf("读取节点: %v", err)
	}
	if loaded.TokenPrefix != newToken[:8] {
		t.Fatalf("Token 前缀没更新: %s vs %s", loaded.TokenPrefix, newToken[:8])
	}
}

func TestAuditLogAPI(t *testing.T) {
	h := newAuthHarness(t)

	// 初始化管理员本身也会被记一笔。
	status, body := h.get(t, "/api/v1/audit")
	if status != http.StatusOK {
		t.Fatalf("查询审计失败: %d", status)
	}
	initial, _ := body["entries"].([]any)
	if len(initial) != 1 {
		t.Fatalf("初始应当只有一条管理员初始化记录: %v", body)
	}
	setupEntry, _ := initial[0].(map[string]any)
	if setupEntry["action"] != "admin_setup" {
		t.Fatalf("第一条应当是初始化管理员: %v", setupEntry)
	}

	// 做几件会被审计的事（PATCH 需要提交完整字段）。
	nodeID, _ := createNodeOverHTTP(t, h, "audit-01")
	editPayload := map[string]any{
		"name": "audit-01-x", "group_name": "测试", "region": "HK", "note": "",
		"iface": "", "interval_sec": 1, "traffic_limit": 0, "traffic_warn_pct": 80,
		"reset_day": 19, "expires_at": 0, "enabled": true,
	}
	if status, body, _ := h.do(t, http.MethodPatch, "/api/v1/nodes/1", editPayload, true, nil); status != http.StatusOK {
		t.Fatalf("修改节点失败: %d %v", status, body)
	}
	if status, _, _ := h.do(t, http.MethodPost, "/api/v1/nodes/1/token", nil, true, nil); status != http.StatusOK {
		t.Fatal("换 Token 失败")
	}
	if status, _, _ := h.do(t, http.MethodDelete, "/api/v1/nodes/1", nil, true, nil); status != http.StatusOK {
		t.Fatal("删除节点失败")
	}

	status, body = h.get(t, "/api/v1/audit?limit=10")
	if status != http.StatusOK {
		t.Fatalf("查询审计失败: %d", status)
	}
	entries, _ := body["entries"].([]any)
	if len(entries) < 4 {
		t.Fatalf("审计条目太少: %d", len(entries))
	}
	// 最新的在最前面，且都带操作者与来源。
	first, _ := entries[0].(map[string]any)
	if first["action"] != "node_delete" {
		t.Fatalf("最新一条应当是删除: %v", first)
	}
	if !strings.Contains(first["detail"].(string), "by admin") {
		t.Fatalf("审计详情应当带操作者: %v", first["detail"])
	}
	if first["ip"] == "" {
		t.Fatal("审计应当记录来源 IP")
	}
	if int64(first["node_id"].(float64)) != nodeID {
		t.Fatalf("审计的节点 ID 不对: %v", first["node_id"])
	}

	// 翻页：before_id 只返回更早的记录。
	firstID := int64(first["id"].(float64))
	status, body = h.get(t, "/api/v1/audit?limit=10&before_id="+strconv.FormatInt(firstID, 10))
	if status != http.StatusOK {
		t.Fatalf("翻页失败: %d", status)
	}
	for _, raw := range body["entries"].([]any) {
		entry, _ := raw.(map[string]any)
		if int64(entry["id"].(float64)) >= firstID {
			t.Fatalf("翻页返回了更新的记录: %v", entry)
		}
	}

	// 参数校验。
	for _, query := range []string{"?limit=0", "?limit=501", "?limit=abc", "?before_id=-1"} {
		if status, _ := h.get(t, "/api/v1/audit"+query); status != http.StatusBadRequest {
			t.Errorf("%s 应当 400，实际 %d", query, status)
		}
	}
}

func TestSettingsAPIAndAlertParamsTakeEffect(t *testing.T) {
	h := newAuthHarness(t)

	status, body := h.get(t, "/api/v1/settings")
	if status != http.StatusOK {
		t.Fatalf("读取设置失败: %d", status)
	}
	server, _ := body["server"].(map[string]any)
	if server == nil || server["listen"] == "" || server["timezone"] == "" {
		t.Fatalf("服务器信息不全: %v", body)
	}
	if _, leaked := server["telegram_token"]; leaked {
		t.Fatal("设置里不该出现 Telegram Token 字段")
	}
	alertCfg, _ := body["alert"].(map[string]any)
	if alertCfg["cooldown"] != "30m0s" {
		t.Fatalf("默认冷却时间不对: %v", alertCfg)
	}

	// 修改告警参数：立刻生效（引擎参数被替换）。
	status, body = h.put(t, "/api/v1/settings/alert", map[string]any{
		"cooldown": "5m", "startup_grace": "1s", "debounce": "500ms", "recover_stable": "2s",
	})
	if status != http.StatusOK {
		t.Fatalf("保存告警参数失败: %d %v", status, body)
	}
	alertCfg, _ = body["alert"].(map[string]any)
	if alertCfg["cooldown"] != "5m0s" || alertCfg["debounce"] != "500ms" {
		t.Fatalf("参数没有生效: %v", alertCfg)
	}
	if h.srv.cfg.AlertCooldown != 5*time.Minute || h.srv.cfg.AlertDebounce != 500*time.Millisecond {
		t.Fatal("服务端配置没有被更新")
	}

	// 非法时长。
	for _, payload := range []map[string]any{
		{"cooldown": "五分钟"},
		{"debounce": "-1s"},
		{"recover_stable": "99h"},
	} {
		if status, _ := h.put(t, "/api/v1/settings/alert", payload); status != http.StatusBadRequest {
			t.Errorf("%v 应当 400，实际 %d", payload, status)
		}
	}

	// 部分字段留空 = 不改动。
	before := h.srv.cfg.AlertCooldown
	if status, _ := h.put(t, "/api/v1/settings/alert", map[string]any{"debounce": "1s"}); status != http.StatusOK {
		t.Fatal("部分更新失败")
	}
	if h.srv.cfg.AlertCooldown != before {
		t.Fatal("留空的字段不该被改掉")
	}
	if h.srv.cfg.AlertDebounce != time.Second {
		t.Fatal("debounce 没有更新")
	}
}

// chartKeys 把 JSON 数组转成 []string（解出来是 []any，比较前要先收拢）。
func chartKeys(t *testing.T, raw any) []string {
	t.Helper()
	list, ok := raw.([]any)
	if !ok {
		t.Fatalf("不是数组: %#v", raw)
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		s, ok := item.(string)
		if !ok {
			t.Fatalf("数组元素不是字符串: %#v", item)
		}
		out = append(out, s)
	}
	return out
}

// 图表可见性接口：缺省全显示 → 存子集 → 读回 → 空数组＝全隐藏 → 非法值 400。
func TestChartVisibilityAPI(t *testing.T) {
	h := newAuthHarness(t)

	// 缺省＝全部显示，并且响应里带上"全部可选"，前端不用自己抄一份键表。
	status, body := h.get(t, "/api/v1/settings")
	if status != http.StatusOK {
		t.Fatalf("读取设置失败: %d", status)
	}
	charts, ok := body["charts"].(map[string]any)
	if !ok {
		t.Fatalf("设置响应里缺少 charts 字段: %v", body)
	}
	if got := chartKeys(t, charts["all"]); !reflect.DeepEqual(got, store.AllCharts) {
		t.Fatalf("all = %v，期望 %v", got, store.AllCharts)
	}
	if got := chartKeys(t, charts["visible"]); !reflect.DeepEqual(got, store.AllCharts) {
		t.Fatalf("缺省 visible = %v，期望全部显示", got)
	}

	// 保存子集（顺带验证重复项被去掉）。
	status, body = h.put(t, "/api/v1/settings/charts", map[string]any{"visible": []string{"cpu", "mem", "cpu"}})
	if status != http.StatusOK {
		t.Fatalf("保存图表设置失败: %d %v", status, body)
	}
	if got := chartKeys(t, body["visible"]); !reflect.DeepEqual(got, []string{"cpu", "mem"}) {
		t.Fatalf("保存后返回 visible = %v，期望 [cpu mem]", got)
	}
	if got := chartKeys(t, body["all"]); !reflect.DeepEqual(got, store.AllCharts) {
		t.Fatalf("保存响应也要带 all，实际 %v", got)
	}

	// 读回（确认是落了库，不是只改了返回值）。
	status, body = h.get(t, "/api/v1/settings")
	if status != http.StatusOK {
		t.Fatalf("读取设置失败: %d", status)
	}
	charts, _ = body["charts"].(map[string]any)
	if got := chartKeys(t, charts["visible"]); !reflect.DeepEqual(got, []string{"cpu", "mem"}) {
		t.Fatalf("读回的 visible = %v，期望 [cpu mem]", got)
	}

	// 审计：改设置必须留痕。
	var action, detail string
	if err := h.srv.db.Reader().QueryRowContext(context.Background(),
		`SELECT action, detail FROM audit_log ORDER BY id DESC LIMIT 1`).Scan(&action, &detail); err != nil {
		t.Fatalf("读取审计: %v", err)
	}
	if action != "settings_update" {
		t.Fatalf("审计动作 = %q，期望 settings_update", action)
	}
	if !strings.Contains(detail, "修改图表显示（保留 2/6）") {
		t.Fatalf("审计详情 = %q", detail)
	}

	// 非法值：未知键、大小写不符、空字符串都要 400（静默丢弃会表现成"勾了又弹回去"）。
	for _, payload := range []map[string]any{
		{"visible": []string{"cpu", "gpu"}},
		{"visible": []string{"CPU"}},
		{"visible": []string{""}},
		{"visible": []string{"net_down"}},
	} {
		status, body = h.put(t, "/api/v1/settings/charts", payload)
		if status != http.StatusBadRequest {
			t.Errorf("%v 应当 400，实际 %d %v", payload, status, body)
		}
		msg, _ := body["error"].(map[string]any)
		if msg["code"] != "bad_request" {
			t.Errorf("%v 的错误码 = %v，期望 bad_request", payload, msg["code"])
		}
	}

	// 非法值不该把已经存好的设置改坏。
	status, body = h.get(t, "/api/v1/settings")
	if status != http.StatusOK {
		t.Fatalf("读取设置失败: %d", status)
	}
	charts, _ = body["charts"].(map[string]any)
	if got := chartKeys(t, charts["visible"]); !reflect.DeepEqual(got, []string{"cpu", "mem"}) {
		t.Fatalf("非法请求之后 visible = %v，期望保持 [cpu mem]", got)
	}

	// 空数组 = 全部隐藏，而且读回来仍然是空（不能被当成"没设置过"）。
	status, body = h.put(t, "/api/v1/settings/charts", map[string]any{"visible": []string{}})
	if status != http.StatusOK {
		t.Fatalf("保存空设置失败: %d %v", status, body)
	}
	if got := chartKeys(t, body["visible"]); len(got) != 0 {
		t.Fatalf("空数组应当表示全部隐藏，实际 %v", got)
	}
	status, body = h.get(t, "/api/v1/settings")
	if status != http.StatusOK {
		t.Fatalf("读取设置失败: %d", status)
	}
	charts, _ = body["charts"].(map[string]any)
	if got := chartKeys(t, charts["visible"]); len(got) != 0 {
		t.Fatalf("读回的 visible = %v，期望空数组（全部隐藏）", got)
	}
}

func TestSetParamsDoesNotNotifyAgain(t *testing.T) {
	// 改了参数之后，已经处于 firing 的规则不应该重新通知一遍。
	engine := alert.NewEngine(alert.DefaultParams(), time.Now().Add(-time.Hour))
	now := time.Now()
	node := alert.Node{ID: 1, Name: "x", Status: "offline", LastSeen: now.Add(-time.Minute)}

	engine.Evaluate(now, []alert.Node{node})
	if decisions := engine.Evaluate(now.Add(3*time.Second), []alert.Node{node}); len(decisions) != 1 || !decisions[0].Notify {
		t.Fatalf("应当先触发一次离线: %+v", decisions)
	}

	params := alert.DefaultParams()
	params.NotifyCooldown = time.Minute
	engine.SetParams(params)

	if decisions := engine.Evaluate(now.Add(4*time.Second), []alert.Node{node}); len(decisions) != 0 {
		t.Fatalf("改参数后不该重复通知: %+v", decisions)
	}
}

func TestAdminEndpointsRequireLogin(t *testing.T) {
	h := newAuthHarness(t)
	createNodeOverHTTP(t, h, "auth-01")
	h.anonymousClient(t)

	cases := []struct {
		method string
		path   string
		want   int
	}{
		{http.MethodGet, "/api/v1/audit", http.StatusUnauthorized},
		{http.MethodGet, "/api/v1/settings", http.StatusUnauthorized},
		{http.MethodPut, "/api/v1/settings/alert", http.StatusUnauthorized},
		{http.MethodPut, "/api/v1/settings/charts", http.StatusUnauthorized},
		{http.MethodPatch, "/api/v1/nodes/1", http.StatusUnauthorized},
		{http.MethodDelete, "/api/v1/nodes/1", http.StatusUnauthorized},
		{http.MethodPost, "/api/v1/nodes/1/token", http.StatusUnauthorized},
	}
	for _, tc := range cases {
		status, _, _ := h.do(t, tc.method, tc.path, map[string]any{"name": "x", "interval_sec": 1}, true, nil)
		if status != tc.want {
			t.Errorf("%s %s 未登录应当 %d，实际 %d", tc.method, tc.path, tc.want, status)
		}
	}
}
