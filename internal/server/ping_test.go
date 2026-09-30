package server

import (
	"context"
	"math"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"probe/internal/protocol"
	"probe/internal/store"
)

// targetsOf 从响应体里取出 targets 数组（形状断言用）。
func targetsOf(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	raw, ok := body["targets"].([]any)
	if !ok {
		t.Fatalf("响应里没有 targets 数组: %v", body)
	}
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		target, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("targets 里出现了非对象: %v", item)
		}
		out = append(out, target)
	}
	return out
}

// 握手之后：welcome 之后必须紧跟一帧 config，带上该节点的间隔与探测设置。
func TestAgentReceivesConfigAfterHandshake(t *testing.T) {
	ts, s, node, token := newAgentTestServer(t)
	ctx := context.Background()

	// 预先配好目标：一个启用的 TCP、一个启用的 ICMP、一个被停用的。
	saved, err := s.db.SetPingSettings(ctx, []store.PingTarget{
		{Label: "Cloudflare", Type: protocol.PingTypeTCP, Host: "1.1.1.1", Port: 443, Enabled: true},
		{Label: "Google", Type: protocol.PingTypeICMP, Host: "8.8.8.8", Enabled: true},
		{Label: "停用的", Type: protocol.PingTypeTCP, Host: "9.9.9.9", Port: 53, Enabled: false},
	}, 120)
	if err != nil {
		t.Fatalf("保存探测设置: %v", err)
	}

	conn := mustDialAgent(t, ts, token)
	sendFrame(t, conn, helloFrame(t, testHello()))

	welcomeEnv, err := readFrame(t, conn, 3*time.Second)
	if err != nil {
		t.Fatalf("读取 welcome: %v", err)
	}
	if welcomeEnv.T != protocol.TypeWelcome {
		t.Fatalf("首帧应当是 welcome，实际 %q", welcomeEnv.T)
	}
	var welcome protocol.Welcome
	if err := welcomeEnv.Bind(&welcome); err != nil {
		t.Fatalf("解析 welcome: %v", err)
	}

	cfgEnv, err := readFrame(t, conn, 3*time.Second)
	if err != nil {
		t.Fatalf("welcome 之后应当收到 config 帧: %v", err)
	}
	if cfgEnv.T != protocol.TypeConfig {
		t.Fatalf("第二帧应当是 config，实际 %q", cfgEnv.T)
	}
	var cfg protocol.Config
	if err := cfgEnv.Bind(&cfg); err != nil {
		t.Fatalf("解析 config: %v", err)
	}
	if err := protocol.ValidateConfig(cfg); err != nil {
		t.Fatalf("下发的 config 自己不合法: %v", err)
	}
	if cfg.ConfigVersion != welcome.ConfigVersion || cfg.ConfigVersion <= 0 {
		t.Fatalf("config 版本号 %d 与 welcome %d 不一致（应当同号且为正）",
			cfg.ConfigVersion, welcome.ConfigVersion)
	}
	if cfg.IntervalSec != node.IntervalSec {
		t.Fatalf("config.IntervalSec = %d，期望节点配置的 %d", cfg.IntervalSec, node.IntervalSec)
	}
	if cfg.PingIntervalSec != 120 {
		t.Fatalf("config.PingIntervalSec = %d，期望 120", cfg.PingIntervalSec)
	}
	// 停用的目标不下发：Agent 不该为它浪费任何流量。
	if len(cfg.PingTargets) != 2 {
		t.Fatalf("应当只下发 2 个启用的目标，实际 %+v", cfg.PingTargets)
	}
	if cfg.PingTargets[0].ID != saved.Targets[0].ID || cfg.PingTargets[0].Host != "1.1.1.1" ||
		cfg.PingTargets[0].Port != 443 || cfg.PingTargets[0].Type != protocol.PingTypeTCP {
		t.Fatalf("第一个目标不对: %+v", cfg.PingTargets[0])
	}
	if cfg.PingTargets[1].Type != protocol.PingTypeICMP || cfg.PingTargets[1].Port != 0 {
		t.Fatalf("第二个目标不对: %+v", cfg.PingTargets[1])
	}

	// 版本号必须真的递增：同一个节点重连时拿到的版本号要更大。
	conn2 := mustDialAgent(t, ts, token)
	sendFrame(t, conn2, helloFrame(t, testHello()))
	if _, err := readFrame(t, conn2, 3*time.Second); err != nil {
		t.Fatalf("读取第二个连接的 welcome: %v", err)
	}
	cfgEnv2, err := readFrame(t, conn2, 3*time.Second)
	if err != nil {
		t.Fatalf("读取第二个连接的 config: %v", err)
	}
	var cfg2 protocol.Config
	if err := cfgEnv2.Bind(&cfg2); err != nil {
		t.Fatalf("解析 config: %v", err)
	}
	if cfg2.ConfigVersion <= cfg.ConfigVersion {
		t.Fatalf("config 版本号没有递增：%d → %d", cfg.ConfigVersion, cfg2.ConfigVersion)
	}
}

// 设置变更后，在线的 Agent 必须立刻收到新 config（不必等重连）。
func TestSettingsChangePushesConfigToOnlineAgents(t *testing.T) {
	h := newAuthHarness(t)
	_, token := createNodeOverHTTP(t, h, "ping-push")

	conn := mustDialAgent(t, h.ts, token)
	sendFrame(t, conn, helloFrame(t, testHello()))
	if _, err := readFrame(t, conn, 3*time.Second); err != nil {
		t.Fatalf("读取 welcome: %v", err)
	}
	first, err := readFrame(t, conn, 3*time.Second)
	if err != nil {
		t.Fatalf("读取握手时的 config: %v", err)
	}
	var handshake protocol.Config
	if err := first.Bind(&handshake); err != nil {
		t.Fatalf("解析 config: %v", err)
	}
	if len(handshake.PingTargets) != 0 {
		t.Fatalf("还没配置目标时不该下发目标: %+v", handshake.PingTargets)
	}

	status, body, _ := h.do(t, http.MethodPut, "/api/v1/settings/ping", map[string]any{
		"interval_sec": 30,
		"targets": []map[string]any{
			{"label": "Cloudflare", "type": "tcp", "host": "1.1.1.1", "port": 443, "enabled": true},
			{"label": "内网", "type": "icmp", "host": "10.0.0.1", "enabled": true},
		},
	}, true, nil)
	if status != http.StatusOK {
		t.Fatalf("保存探测设置失败: %d %v", status, body)
	}

	// 推送是异步的（不能拖住设置接口的响应），这里等它到。
	env, err := readFrame(t, conn, 5*time.Second)
	if err != nil {
		t.Fatalf("设置变更后应当收到新 config: %v", err)
	}
	if env.T != protocol.TypeConfig {
		t.Fatalf("期望 config 帧，实际 %q", env.T)
	}
	var pushed protocol.Config
	if err := env.Bind(&pushed); err != nil {
		t.Fatalf("解析 config: %v", err)
	}
	if pushed.ConfigVersion <= handshake.ConfigVersion {
		t.Fatalf("推送的版本号 %d 没有超过握手时的 %d", pushed.ConfigVersion, handshake.ConfigVersion)
	}
	if pushed.PingIntervalSec != 30 || len(pushed.PingTargets) != 2 {
		t.Fatalf("推送的配置内容不对: %+v", pushed)
	}
	if pushed.PingTargets[0].Host != "1.1.1.1" || pushed.PingTargets[1].Type != protocol.PingTypeICMP {
		t.Fatalf("推送的目标不对: %+v", pushed.PingTargets)
	}

	// Agent 回 ack，服务端不该因此断开（也不能把 ack 当上报帧限流）。
	ack, err := protocol.New(protocol.TypeAck, protocol.Ack{ConfigVersion: pushed.ConfigVersion})
	if err != nil {
		t.Fatalf("构造 ack: %v", err)
	}
	sendFrame(t, conn, ack)
	time.Sleep(100 * time.Millisecond)

	// 落库确认：再拉一次设置，看到的是同一份。
	status, body, _ = h.do(t, http.MethodGet, "/api/v1/settings", nil, false, nil)
	if status != http.StatusOK {
		t.Fatalf("读取设置失败: %d", status)
	}
	ping, _ := body["ping"].(map[string]any)
	if ping == nil {
		t.Fatalf("GET /settings 里缺少 ping：%v", body)
	}
	if ping["interval_sec"] != float64(30) || ping["max_targets"] != float64(protocol.MaxPingTargets) {
		t.Fatalf("ping 设置内容不对: %v", ping)
	}
	targets, _ := ping["targets"].([]any)
	if len(targets) != 2 {
		t.Fatalf("ping.targets 数量 = %d，期望 2", len(targets))
	}
	firstTarget, _ := targets[0].(map[string]any)
	if firstTarget["id"] == nil || firstTarget["label"] != "Cloudflare" || firstTarget["enabled"] != true {
		t.Fatalf("目标形状不对: %v", firstTarget)
	}
}

// 未登录：设置接口与曲线接口都必须 401。
func TestPingEndpointsRequireLogin(t *testing.T) {
	h := newAuthHarness(t)
	createNodeOverHTTP(t, h, "ping-auth")
	h.anonymousClient(t)

	if status, _, _ := h.do(t, http.MethodPut, "/api/v1/settings/ping", map[string]any{
		"interval_sec": 60, "targets": []any{},
	}, false, nil); status != http.StatusUnauthorized {
		t.Errorf("未登录保存探测设置应当 401，实际 %d", status)
	}
	if status, _, _ := h.do(t, http.MethodGet, "/api/v1/nodes/1/ping", nil, false, nil); status != http.StatusUnauthorized {
		t.Errorf("未登录读取延迟曲线应当 401，实际 %d", status)
	}
}

// 已登录但缺 CSRF：写接口必须 403（避免被跨站表单改配置）。
func TestPutPingSettingsRequiresCSRF(t *testing.T) {
	h := newAuthHarness(t)
	status, _, _ := h.do(t, http.MethodPut, "/api/v1/settings/ping", map[string]any{
		"interval_sec": 60, "targets": []any{},
	}, false, nil)
	if status != http.StatusForbidden {
		t.Fatalf("缺 CSRF 应当 403，实际 %d", status)
	}
}

func TestPutPingSettingsValidation(t *testing.T) {
	h := newAuthHarness(t)

	tooMany := make([]map[string]any, 0, protocol.MaxPingTargets+1)
	for i := 0; i <= protocol.MaxPingTargets; i++ {
		tooMany = append(tooMany, map[string]any{
			"type": "tcp", "host": "10.0.0." + strconv.Itoa(i), "port": 80, "enabled": true,
		})
	}
	longLabel := ""
	for i := 0; i < protocol.MaxPingLabelLen+1; i++ {
		longLabel += "字"
	}

	cases := []struct {
		name    string
		payload map[string]any
		want    int
		code    string
	}{
		{"正常", map[string]any{"interval_sec": 60, "targets": []map[string]any{
			{"label": "CF", "type": "tcp", "host": "1.1.1.1", "port": 443, "enabled": true},
		}}, http.StatusOK, ""},
		{"类型非法", map[string]any{"interval_sec": 60, "targets": []map[string]any{
			{"type": "udp", "host": "1.1.1.1", "port": 53},
		}}, http.StatusBadRequest, "bad_request"},
		{"tcp 缺端口", map[string]any{"interval_sec": 60, "targets": []map[string]any{
			{"type": "tcp", "host": "1.1.1.1"},
		}}, http.StatusBadRequest, "bad_request"},
		{"端口越界", map[string]any{"interval_sec": 60, "targets": []map[string]any{
			{"type": "tcp", "host": "1.1.1.1", "port": 70000},
		}}, http.StatusBadRequest, "bad_request"},
		{"端口为负", map[string]any{"interval_sec": 60, "targets": []map[string]any{
			{"type": "tcp", "host": "1.1.1.1", "port": -1},
		}}, http.StatusBadRequest, "bad_request"},
		{"主机为空", map[string]any{"interval_sec": 60, "targets": []map[string]any{
			{"type": "tcp", "host": "", "port": 80},
		}}, http.StatusBadRequest, "bad_request"},
		{"名称过长", map[string]any{"interval_sec": 60, "targets": []map[string]any{
			{"label": longLabel, "type": "tcp", "host": "1.1.1.1", "port": 80},
		}}, http.StatusBadRequest, "bad_request"},
		{"目标过多", map[string]any{"interval_sec": 60, "targets": tooMany},
			http.StatusBadRequest, "bad_request"},
		{"间隔过小", map[string]any{"interval_sec": 9, "targets": []any{}},
			http.StatusBadRequest, "bad_request"},
		{"间隔过大", map[string]any{"interval_sec": 3601, "targets": []any{}},
			http.StatusBadRequest, "bad_request"},
		{"间隔为 0", map[string]any{"interval_sec": 0, "targets": []any{}},
			http.StatusBadRequest, "bad_request"},
		{"间隔类型不对", map[string]any{"interval_sec": "60", "targets": []any{}},
			http.StatusBadRequest, ""}, // decodeJSON 的报错，码是 bad_request
		{"缺少 targets 字段", map[string]any{"interval_sec": 60},
			http.StatusBadRequest, "bad_request"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body, _ := h.do(t, http.MethodPut, "/api/v1/settings/ping", tc.payload, true, nil)
			if status != tc.want {
				t.Fatalf("状态码 = %d，期望 %d（%v）", status, tc.want, body)
			}
			if tc.code != "" {
				e, _ := body["error"].(map[string]any)
				if e["code"] != tc.code {
					t.Fatalf("错误码 = %v，期望 %q（%v）", e["code"], tc.code, body)
				}
			}
		})
	}
}

// 不带 interval_sec 时保持现值（前端只改目标时不该被强制重填间隔）。
func TestPutPingSettingsKeepsIntervalWhenOmitted(t *testing.T) {
	h := newAuthHarness(t)

	status, body, _ := h.do(t, http.MethodPut, "/api/v1/settings/ping", map[string]any{
		"interval_sec": 600, "targets": []any{},
	}, true, nil)
	if status != http.StatusOK || body["interval_sec"] != float64(600) {
		t.Fatalf("保存间隔失败: %d %v", status, body)
	}
	status, body, _ = h.do(t, http.MethodPut, "/api/v1/settings/ping", map[string]any{
		"targets": []map[string]any{{"type": "icmp", "host": "1.1.1.1", "enabled": true}},
	}, true, nil)
	if status != http.StatusOK {
		t.Fatalf("只改目标失败: %d %v", status, body)
	}
	if body["interval_sec"] != float64(600) {
		t.Fatalf("没传 interval_sec 时应当保持 600，实际 %v", body["interval_sec"])
	}
}

// ID 的处理：回传既有 ID 保留、不带 ID 分配新号（这是"改名字不能让历史断掉"的关键）。
func TestPutPingSettingsKeepsAndAssignsIDs(t *testing.T) {
	h := newAuthHarness(t)

	status, body, _ := h.do(t, http.MethodPut, "/api/v1/settings/ping", map[string]any{
		"interval_sec": 60,
		"targets": []map[string]any{
			{"label": "CF", "type": "tcp", "host": "1.1.1.1", "port": 443, "enabled": true},
			{"label": "Google", "type": "icmp", "host": "8.8.8.8", "enabled": true},
		},
	}, true, nil)
	if status != http.StatusOK {
		t.Fatalf("保存失败: %d %v", status, body)
	}
	targets := targetsOf(t, body)
	if len(targets) != 2 || targets[0]["id"] != float64(1) || targets[1]["id"] != float64(2) {
		t.Fatalf("ID 分配不对: %v", targets)
	}

	// 改名 + 加一个新目标：老 ID 必须保留下来。
	status, body, _ = h.do(t, http.MethodPut, "/api/v1/settings/ping", map[string]any{
		"interval_sec": 60,
		"targets": []map[string]any{
			{"id": 2, "label": "Google DNS", "type": "icmp", "host": "8.8.8.8", "enabled": true},
			{"label": "Quad9", "type": "tcp", "host": "9.9.9.9", "port": 853, "enabled": true},
		},
	}, true, nil)
	if status != http.StatusOK {
		t.Fatalf("保存失败: %d %v", status, body)
	}
	targets = targetsOf(t, body)
	if targets[0]["id"] != float64(2) || targets[0]["label"] != "Google DNS" {
		t.Fatalf("改名字不该换 ID: %v", targets[0])
	}
	if targets[1]["id"] != float64(3) {
		t.Fatalf("新目标应当拿到 3: %v", targets[1])
	}

	// 重复目标：自动去重（返回体里就是去重后的结果）。
	status, body, _ = h.do(t, http.MethodPut, "/api/v1/settings/ping", map[string]any{
		"interval_sec": 60,
		"targets": []map[string]any{
			{"type": "icmp", "host": "8.8.8.8", "enabled": true},
			{"type": "icmp", "host": "8.8.8.8", "enabled": true},
		},
	}, true, nil)
	if status != http.StatusOK {
		t.Fatalf("保存失败: %d %v", status, body)
	}
	if targets = targetsOf(t, body); len(targets) != 1 {
		t.Fatalf("重复目标应当自动去重: %v", targets)
	}

	// 写审计日志（设置类操作的 action 固定为 settings_update）。
	status, body, _ = h.do(t, http.MethodGet, "/api/v1/audit?limit=5", nil, false, nil)
	if status != http.StatusOK {
		t.Fatalf("读取审计日志失败: %d", status)
	}
	entries, _ := body["entries"].([]any)
	found := false
	for _, raw := range entries {
		entry, _ := raw.(map[string]any)
		if entry["action"] == "settings_update" {
			if detail, _ := entry["detail"].(string); strings.Contains(detail, "修改延迟探测目标") {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("审计日志里没有 settings_update/修改延迟探测目标：%v", entries)
	}
}

// 延迟曲线接口的形状：所有配置了的目标都要出现，没数据的 has_data=false。
func TestNodePingAPIShape(t *testing.T) {
	h := newAuthHarness(t)
	nodeID, _ := createNodeOverHTTP(t, h, "ping-api")
	ctx := context.Background()

	status, body, _ := h.do(t, http.MethodPut, "/api/v1/settings/ping", map[string]any{
		"interval_sec": 60,
		"targets": []map[string]any{
			{"label": "Cloudflare", "type": "tcp", "host": "1.1.1.1", "port": 443, "enabled": true},
			{"label": "停用的", "type": "icmp", "host": "10.0.0.1", "enabled": false},
		},
	}, true, nil)
	if status != http.StatusOK {
		t.Fatalf("保存设置失败: %d %v", status, body)
	}

	// 只有第一个目标有历史数据；最后一分钟整段丢包（逐点丢包率要能带出来）。
	now := time.Now()
	base := now.Unix() - now.Unix()%60 - 5*60
	for i := int64(0); i < 5; i++ {
		loss := 0.0
		if i == 4 {
			loss = 100
		}
		if err := h.srv.db.UpsertPingBuckets(ctx, []store.PingBucket{
			store.NewPingBucket(nodeID, 1, base+i*60, float64(20+i), float64(18+i), float64(25+i), loss),
		}); err != nil {
			t.Fatalf("写入探测桶: %v", err)
		}
	}

	status, body, _ = h.do(t, http.MethodGet, "/api/v1/nodes/1/ping?range=1h", nil, false, nil)
	if status != http.StatusOK {
		t.Fatalf("读取延迟曲线失败: %d %v", status, body)
	}
	meta, _ := body["meta"].(map[string]any)
	if meta["key"] != "1h" || meta["seconds"] != float64(3600) ||
		meta["bucket_sec"] != float64(60) || meta["points"] != float64(60) {
		t.Fatalf("meta 不对: %v", meta)
	}
	targets, _ := body["targets"].([]any)
	if len(targets) != 2 {
		t.Fatalf("所有配置了的目标都要出现（含停用的），实际 %v", targets)
	}

	first, _ := targets[0].(map[string]any)
	if first["id"] != float64(1) || first["label"] != "Cloudflare" || first["type"] != "tcp" ||
		first["host"] != "1.1.1.1" || first["port"] != float64(443) || first["enabled"] != true {
		t.Fatalf("目标字段不对: %v", first)
	}
	if first["has_data"] != true {
		t.Fatalf("有数据的目标 has_data 应当为 true: %v", first)
	}
	points, _ := first["points"].([]any)
	if len(points) != 5 {
		t.Fatalf("点数 = %d，期望 5", len(points))
	}
	point, _ := points[0].([]any)
	if len(point) != 4 {
		t.Fatalf("点结构应当是 [ts, avg, max, loss]: %v", point)
	}
	if point[1] != float64(20) || point[2] != float64(25) {
		t.Fatalf("第一个点不对: %v", point)
	}
	if ts, _ := point[0].(float64); int64(ts) != base {
		t.Fatalf("第一个点的时间戳 = %v，期望 %d", point[0], base)
	}
	// 前三个元素的含义与顺序不许变（前端读 p[1]/p[2]），第 4 个是桶丢包率。
	if point[3] != float64(0) {
		t.Fatalf("第一个桶没有丢包，逐点丢包率 = %v，期望 0", point[3])
	}
	last, _ := points[4].([]any)
	if len(last) != 4 || last[3] != float64(100) {
		t.Fatalf("最后一个桶整段丢包，点应当是 [ts, avg, max, 100]: %v", last)
	}
	// 区间聚合丢包率与逐点口径一致：5 个桶里丢了 1 个。
	if loss, _ := first["loss_pct"].(float64); loss < 19.9 || loss > 20.1 {
		t.Fatalf("区间丢包率 = %v，期望约 20", first["loss_pct"])
	}
	// 慢的三件套也在：20/21/22/23/24 五个有读数的点，中位数 22 →
	// 阈值被 100ms 的下限兜住（22×3 = 66 < 100），所以一个点都不算慢。
	if got := floatField(t, first, "baseline_ms"); !closeTo(got, 22) {
		t.Errorf("baseline_ms = %v，期望 22", got)
	}
	if got := floatField(t, first, "threshold_ms"); !closeTo(got, store.SlowFloorMS) {
		t.Errorf("threshold_ms = %v，期望下限 %v", got, store.SlowFloorMS)
	}
	if got := floatField(t, first, "slow_pct"); got != 0 {
		t.Errorf("slow_pct = %v，期望 0（全部远低于阈值下限）", got)
	}

	// 没有数据的目标：has_data=false、points 是空数组（不是 null）。
	second, _ := targets[1].(map[string]any)
	if second["has_data"] != false || second["enabled"] != false {
		t.Fatalf("停用目标的状态不对: %v", second)
	}
	if pts, ok := second["points"].([]any); !ok || len(pts) != 0 {
		t.Fatalf("没有数据时 points 应当是 []，实际 %v", second["points"])
	}
	if _, ok := second["loss_pct"].(float64); !ok {
		t.Fatalf("缺少 loss_pct: %v", second)
	}
}

// 延迟曲线接口必须给出**整段**的平均延迟（avg_ms），而且按成功探测次数加权。
//
// 图例上要显示"这个目标这一小时平均多少毫秒"，前端手里只有画曲线用的分桶点：
// 让它自己把桶平均一遍，就等于把服务端的加权规则再实现一次（见 store.QueryPingSeries）。
func TestNodePingAvgMSIsWeighted(t *testing.T) {
	h := newAuthHarness(t)
	nodeID, _ := createNodeOverHTTP(t, h, "ping-avg")
	ctx := context.Background()

	status, body := h.put(t, "/api/v1/settings/ping", map[string]any{
		"interval_sec": 60,
		"targets": []map[string]any{
			{"label": "Cloudflare", "type": "tcp", "host": "1.1.1.1", "port": 443, "enabled": true},
			{"label": "Google DNS", "type": "icmp", "host": "8.8.8.8", "enabled": true},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("保存设置失败: %d %v", status, body)
	}

	now := time.Now()
	base := now.Unix() - now.Unix()%60 - 10*60
	if err := h.srv.db.UpsertPingBuckets(ctx, []store.PingBucket{
		// 探通 100 次，平均 100ms
		store.NewPingBucket(nodeID, 1, base, 100, 90, 110, 0),
		// 探通 100 次，平均 200ms
		store.NewPingBucket(nodeID, 1, base+60, 200, 190, 210, 0),
		// 只探通 10 次（90% 丢包），平均 800ms
		store.NewPingBucket(nodeID, 1, base+120, 800, 700, 900, 90),
	}); err != nil {
		t.Fatalf("写入探测桶: %v", err)
	}

	status, body, _ = h.do(t, http.MethodGet, "/api/v1/nodes/1/ping?range=1h", nil, false, nil)
	if status != http.StatusOK {
		t.Fatalf("读取延迟曲线失败: %d %v", status, body)
	}
	targets := targetsOf(t, body)
	if len(targets) != 2 {
		t.Fatalf("目标数量 = %d，期望 2", len(targets))
	}

	// 加权平均 = (100×100 + 200×100 + 800×10) / (100+100+10) = 180.95…
	// 若把三个桶的均值直接平均会得到 366.67 —— 差距足够大，断言不会含糊。
	first := targets[0]
	avg, ok := first["avg_ms"].(float64)
	if !ok {
		t.Fatalf("targets[0] 缺少 avg_ms: %v", first)
	}
	if math.Abs(avg-38000.0/210.0) > 0.01 {
		t.Errorf("avg_ms = %v，期望 %v（按成功探测次数加权）", avg, 38000.0/210.0)
	}
	// 与整段丢包率同一份数据（丢了 90 次 / 300 次探测）。
	if loss, _ := first["loss_pct"].(float64); math.Abs(loss-30) > 0.01 {
		t.Errorf("loss_pct = %v，期望 30", loss)
	}

	// 没有数据的目标：has_data=false，avg_ms 是 0（而不是缺字段或 null）。
	second := targets[1]
	if second["has_data"] != false {
		t.Fatalf("第二个目标不该有数据: %v", second)
	}
	if avg, ok := second["avg_ms"].(float64); !ok || avg != 0 {
		t.Errorf("没有数据时 avg_ms 应当是 0，实际 %v", second["avg_ms"])
	}
}

// 延迟曲线接口的"慢"三件套：中位数基线、max(基线×3, 100ms) 阈值、
// 以及"慢"占**有读数**探测的百分比。三个数全部由服务端算 ——
// 前端只拿 threshold_ms 与它自己画出来的那些点比大小。
func TestNodePingSlowStatsShape(t *testing.T) {
	h := newAuthHarness(t)
	nodeID, _ := createNodeOverHTTP(t, h, "ping-slow")
	ctx := context.Background()

	status, body := h.put(t, "/api/v1/settings/ping", map[string]any{
		"interval_sec": 60,
		"targets": []map[string]any{
			{"label": "Cloudflare", "type": "tcp", "host": "1.1.1.1", "port": 443, "enabled": true},
			{"label": "没有数据的", "type": "icmp", "host": "10.0.0.9", "enabled": true},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("保存设置失败: %d %v", status, body)
	}

	// 一个小时的桶：58 个 200ms 的正常探测 + 1 根 2203ms 的尖峰（用户看到的那一根）
	// + 1 个整段全丢（没有延迟样本）。
	now := time.Now()
	end := now.Unix() - now.Unix()%60
	base := end - 60*60 // 正好是 1h 窗口的左边界（ts >= start）
	buckets := make([]store.PingBucket, 0, 60)
	for i := int64(0); i < 58; i++ {
		buckets = append(buckets, store.NewPingBucket(nodeID, 1, base+i*60, 200, 195, 205, 0))
	}
	buckets = append(buckets,
		store.NewPingBucket(nodeID, 1, base+58*60, 2203, 2203, 2203, 0),
		store.NewPingBucket(nodeID, 1, base+59*60, 0, 0, 0, 100),
	)
	if err := h.srv.db.UpsertPingBuckets(ctx, buckets); err != nil {
		t.Fatalf("写入探测桶: %v", err)
	}

	status, body, _ = h.do(t, http.MethodGet, "/api/v1/nodes/1/ping?range=1h", nil, false, nil)
	if status != http.StatusOK {
		t.Fatalf("读取延迟曲线失败: %d %v", status, body)
	}
	targets := targetsOf(t, body)
	if len(targets) != 2 {
		t.Fatalf("目标数量 = %d，期望 2", len(targets))
	}
	first := targets[0]

	// 基线 = 中位数(58 个 200 + 一个 2203) = 200（平均值会是 233.9 —— 被尖峰自己拉高）。
	if got := floatField(t, first, "baseline_ms"); !closeTo(got, 200) {
		t.Errorf("baseline_ms = %v，期望 200（中位数，不是平均值）", got)
	}
	// 阈值 = max(200×3, 100) = 600。
	if got := floatField(t, first, "threshold_ms"); !closeTo(got, 600) {
		t.Errorf("threshold_ms = %v，期望 600", got)
	}
	// 慢占比 = 1/59：分母是**有读数**的 59 个桶（那个全丢的桶没有延迟样本），
	// 不是全部的 60 个（后者会算成 1.67%）。
	if got := floatField(t, first, "slow_pct"); !closeTo(got, 100.0/59) {
		t.Errorf("slow_pct = %v，期望 %v（分母是有读数的探测）", got, 100.0/59)
	}

	// 没有数据的目标：三个新字段都是 0（数字，不是 null）—— 前端据此不标红、
	// 图例里也不写慢占比。floatField 在字段缺失或为 null 时会直接失败。
	second := targets[1]
	if second["has_data"] != false {
		t.Fatalf("第二个目标不该有数据: %v", second)
	}
	for _, key := range []string{"baseline_ms", "threshold_ms", "slow_pct"} {
		if got := floatField(t, second, key); got != 0 {
			t.Errorf("没有数据时 %s 应当是 0，实际 %v", key, got)
		}
	}
}

// 回归：loss_pct 的含义一个字都不许改 —— 它永远只统计"真丢包"。
//
// 用户的场景：一根 2203ms 的尖峰（基线约 200ms）、丢包 0% —— 因为丢包的判定规则是
// "3 秒内有没有回应"，它 2.2 秒就回来了。所以"慢"必须单独算：这里构造一批
// **慢但一次都没丢**的探测，断言 loss_pct == 0 而 slow_pct > 0。
func TestNodePingSlowIsNotLoss(t *testing.T) {
	h := newAuthHarness(t)
	nodeID, _ := createNodeOverHTTP(t, h, "ping-slow-not-loss")
	ctx := context.Background()

	status, body := h.put(t, "/api/v1/settings/ping", map[string]any{
		"interval_sec": 60,
		"targets": []map[string]any{
			{"label": "Cloudflare", "type": "tcp", "host": "1.1.1.1", "port": 443, "enabled": true},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("保存设置失败: %d %v", status, body)
	}

	// 58 个 100ms + 2 个 500ms，**全部 loss = 0**（包都回来了，只是有两个特别慢）。
	// 基线 = 100 → 阈值 = max(300, 100) = 300 → 慢的是那两个 500 → 2/60 = 3.33%。
	now := time.Now()
	end := now.Unix() - now.Unix()%60
	base := end - 60*60
	buckets := make([]store.PingBucket, 0, 60)
	for i := int64(0); i < 58; i++ {
		buckets = append(buckets, store.NewPingBucket(nodeID, 1, base+i*60, 100, 98, 103, 0))
	}
	buckets = append(buckets,
		store.NewPingBucket(nodeID, 1, base+58*60, 500, 480, 520, 0),
		store.NewPingBucket(nodeID, 1, base+59*60, 500, 480, 520, 0),
	)
	if err := h.srv.db.UpsertPingBuckets(ctx, buckets); err != nil {
		t.Fatalf("写入探测桶: %v", err)
	}

	status, body, _ = h.do(t, http.MethodGet, "/api/v1/nodes/1/ping?range=1h", nil, false, nil)
	if status != http.StatusOK {
		t.Fatalf("读取延迟曲线失败: %d %v", status, body)
	}
	first := targetsOf(t, body)[0]
	if got := floatField(t, first, "loss_pct"); got != 0 {
		t.Fatalf("一个包都没丢，loss_pct = %v，期望 0（慢不等于丢）", got)
	}
	if got := floatField(t, first, "slow_pct"); !closeTo(got, 100.0/30) {
		t.Fatalf("slow_pct = %v，期望 %v（50ms 的探测慢，但一个都没丢）", got, 100.0/30)
	}
	// 逐点丢包率同理：全是 0 —— 底部竖条一根都不该画出来（前端用例覆盖绘制）。
	points, _ := first["points"].([]any)
	for i, raw := range points {
		p, _ := raw.([]any)
		if len(p) != 4 {
			t.Fatalf("第 %d 个点结构不对: %v", i, p)
		}
		if p[3] != float64(0) {
			t.Fatalf("第 %d 个点的丢包率 = %v，期望 0", i, p[3])
		}
	}
}

// 各种非法/边界输入。
func TestNodePingAPIBoundaries(t *testing.T) {
	h := newAuthHarness(t)
	createNodeOverHTTP(t, h, "ping-edge")

	// 非法档位 → 400 bad_range。
	status, body, _ := h.do(t, http.MethodGet, "/api/v1/nodes/1/ping?range=2h", nil, false, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("非法档位应当 400，实际 %d", status)
	}
	e, _ := body["error"].(map[string]any)
	if e["code"] != "bad_range" {
		t.Fatalf("错误码 = %v，期望 bad_range", e["code"])
	}

	// 不传 range 时默认 1h。
	for _, key := range []string{"1h", "6h", "12h", "1d", "3d", "7d"} {
		status, body, _ := h.do(t, http.MethodGet, "/api/v1/nodes/1/ping?range="+key, nil, false, nil)
		if status != http.StatusOK {
			t.Fatalf("%s 档位应当 200，实际 %d", key, status)
		}
		meta, _ := body["meta"].(map[string]any)
		if meta["key"] != key {
			t.Fatalf("%s 档位的 meta.key = %v", key, meta["key"])
		}
	}

	// 不存在的节点 → 404；非法 ID → 400。
	if status, _, _ := h.do(t, http.MethodGet, "/api/v1/nodes/999/ping", nil, false, nil); status != http.StatusNotFound {
		t.Fatalf("不存在的节点应当 404，实际 %d", status)
	}
	if status, _, _ := h.do(t, http.MethodGet, "/api/v1/nodes/abc/ping", nil, false, nil); status != http.StatusBadRequest {
		t.Fatalf("非法节点 ID 应当 400，实际 %d", status)
	}

	// 一个目标都没配：targets 是空数组，不是 null。
	status, body, _ = h.do(t, http.MethodGet, "/api/v1/nodes/1/ping", nil, false, nil)
	if status != http.StatusOK {
		t.Fatalf("没配目标时也应当 200，实际 %d", status)
	}
	if targets, ok := body["targets"].([]any); !ok || len(targets) != 0 {
		t.Fatalf("没配目标时 targets 应当是 []，实际 %v", body["targets"])
	}
}

// 探测结果从 metrics 帧进内存、每分钟落一行。
func TestPingTrackerFlushWritesRows(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	now := time.Now()

	node, _, err := s.db.CreateNode(ctx, store.NewNode{
		Name: "flush-01", IntervalSec: 1, TrafficWarnPct: 80, ResetDay: 1,
	}, now)
	if err != nil {
		t.Fatalf("创建节点: %v", err)
	}

	// 每秒一帧、每帧带同一份"最近一次结果" —— 落盘只应当有一行。
	for i := 0; i < 60; i++ {
		s.ping.observe(node.ID, []protocol.PingResult{
			{TargetID: 1, AvgMS: 23.4, MinMS: 20, MaxMS: 31, LossPct: 0},
			{TargetID: 2, AvgMS: 0, MinMS: 0, MaxMS: 0, LossPct: 100},
		}, now)
	}
	s.flushPings(ctx)

	var rows int
	if err := s.db.Reader().QueryRowContext(ctx,
		`SELECT count(*) FROM ping_samples_1m WHERE node_id = ?`, node.ID).Scan(&rows); err != nil {
		t.Fatalf("统计: %v", err)
	}
	if rows != 2 {
		t.Fatalf("落盘行数 = %d，期望 2（每个目标一行）", rows)
	}
	var avg, loss float64
	if err := s.db.Reader().QueryRowContext(ctx,
		`SELECT avg_ms, loss_pct FROM ping_samples_1m WHERE node_id = ? AND target_id = 2`,
		node.ID).Scan(&avg, &loss); err != nil {
		t.Fatalf("读取: %v", err)
	}
	if avg != 0 || loss != 100 {
		t.Fatalf("全丢的那一行不对: avg=%v loss=%v", avg, loss)
	}

	// 取走之后就清空：没有新的观测就不会再写第二遍（节点掉线时图表应当出现空洞）。
	s.flushPings(ctx)
	if err := s.db.Reader().QueryRowContext(ctx,
		`SELECT count(*) FROM ping_samples_1m WHERE node_id = ?`, node.ID).Scan(&rows); err != nil {
		t.Fatalf("统计: %v", err)
	}
	if rows != 2 {
		t.Fatalf("重复 flush 不该产生新行，实际 %d 行", rows)
	}

	// 删除节点时必须一并清掉探测数据。
	if err := s.db.DeleteNode(ctx, node.ID); err != nil {
		t.Fatalf("删除节点: %v", err)
	}
	if err := s.db.Reader().QueryRowContext(ctx,
		`SELECT count(*) FROM ping_samples_1m WHERE node_id = ?`, node.ID).Scan(&rows); err != nil {
		t.Fatalf("统计: %v", err)
	}
	if rows != 0 {
		t.Fatalf("删除节点后仍残留 %d 行", rows)
	}
}

// purge 按 1 分钟桶的保留期清理探测数据。
func TestPurgeRemovesOldPingSamples(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	now := time.Now()

	node, _, err := s.db.CreateNode(ctx, store.NewNode{
		Name: "purge-01", IntervalSec: 1, TrafficWarnPct: 80, ResetDay: 1,
	}, now)
	if err != nil {
		t.Fatalf("创建节点: %v", err)
	}
	old := now.Add(-2 * s.cfg.Retention1m).Unix()
	old -= old % 60
	fresh := now.Unix() - now.Unix()%60
	if err := s.db.UpsertPingBuckets(ctx, []store.PingBucket{
		store.NewPingBucket(node.ID, 1, old, 20, 20, 20, 0),
		store.NewPingBucket(node.ID, 1, fresh, 20, 20, 20, 0),
	}); err != nil {
		t.Fatalf("写入: %v", err)
	}

	s.purge(ctx)

	var rows int
	if err := s.db.Reader().QueryRowContext(ctx,
		`SELECT count(*) FROM ping_samples_1m WHERE node_id = ?`, node.ID).Scan(&rows); err != nil {
		t.Fatalf("统计: %v", err)
	}
	if rows != 1 {
		t.Fatalf("清理后剩余 %d 行，期望 1（只删过期的）", rows)
	}
}
