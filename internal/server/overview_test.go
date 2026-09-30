package server

import (
	"context"
	"math"
	"net/http"
	"testing"
	"time"

	"probe/internal/protocol"
	"probe/internal/store"
)

// createPricedNode 建一个有价格的节点（createNodeOverHTTP 不带价格三件套）。
//
// 到期日固定成"现在 + 365 天 + 1 小时"：剩余天数按整天向下取整，多出来的那一小时
// 让它在整个用例期间都停在 365 天 —— 否则跨过一次秒/零点，剩余价值就会变一个数，
// 断言随之变成偶发失败。
func createPricedNode(t *testing.T, h *authHarness, name string, priceCents int64, currency string, months int) int64 {
	t.Helper()
	status, body := h.post(t, "/api/v1/nodes", map[string]any{
		"name": name, "interval_sec": 1, "reset_day": 19,
		"price_cents": priceCents, "currency": currency, "billing_months": months,
		"expires_at": time.Now().Unix() + 365*86400 + 3600,
	}, nil)
	if status != http.StatusCreated {
		t.Fatalf("创建节点 %s 失败: %d %v", name, status, body)
	}
	node, _ := body["node"].(map[string]any)
	id, _ := node["id"].(float64)
	return int64(id)
}

// onlineState 把一个节点的状态写成"刚刚上报过"（默认 stale-after 是 10 秒）。
func onlineState(t *testing.T, s *Server, nodeID int64, at time.Time, m protocol.Metrics) {
	t.Helper()
	s.State().Update(nodeID, 1, m, 0, at)
}

// overviewOf 取一次总览并断言 200。
func overviewOf(t *testing.T, h *authHarness, path string) map[string]any {
	t.Helper()
	status, body, _ := h.do(t, http.MethodGet, path, nil, false, nil)
	if status != http.StatusOK {
		t.Fatalf("GET %s = %d %v", path, status, body)
	}
	return body
}

func totalsOf(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	totals, ok := body["totals"].(map[string]any)
	if !ok {
		t.Fatalf("响应里没有 totals 对象: %v", body)
	}
	return totals
}

func overviewNodesOf(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	// nodes 必须是对象（哪怕是空的）：前端拿到 null 会直接读它的属性。
	nodes, ok := body["nodes"].(map[string]any)
	if !ok {
		t.Fatalf("响应里的 nodes 不是对象: %v", body["nodes"])
	}
	return nodes
}

// floatField 取一个 JSON 数字字段（缺失或类型不对时直接失败）。
func floatField(t *testing.T, obj map[string]any, key string) float64 {
	t.Helper()
	v, ok := obj[key].(float64)
	if !ok {
		t.Fatalf("字段 %q 不是数字: %v", key, obj[key])
	}
	return v
}

func closeTo(got, want float64) bool { return math.Abs(got-want) < 0.01 }

// 总览接口的形状：合计各字段、币种分组、分桶长度恒为 buckets 且"没有数据的段是 null"。
func TestOverviewAPIShape(t *testing.T) {
	h := newAuthHarness(t)
	// 停掉 1 Hz 的实时循环：它会每秒把流量汇总写进缓存（TTL 30 秒），
	// 与"先落盘流量、再断言合计"的时序抢跑。
	h.cancel()

	ctx := context.Background()
	now := time.Now()

	// 节点 1：在线、有价格（CNY）。
	nodeA := createPricedNode(t, h, "ov-a", 12000, "CNY", 1)
	// 节点 2：从没上报过（unknown）、另一个币种（USD）。
	nodeB := createPricedNode(t, h, "ov-b", 9900, "USD", 1)
	// 节点 3：一分钟前上报过 —— 按默认判定（stale 10s / offline 30s）是**离线**。
	nodeC := createPricedNode(t, h, "ov-c", 5000, "CNY", 1)

	onlineState(t, h.srv, nodeA, now, protocol.Metrics{
		Mem:  protocol.Mem{Total: 16 << 30, Used: 8 << 30, Pct: 50},
		Disk: []protocol.Disk{{Mount: "/", Total: 400 << 30, Used: 100 << 30}},
		Net:  protocol.Net{RxRate: 1024, TxRate: 2048},
	})
	onlineState(t, h.srv, nodeC, now.Add(-time.Minute), protocol.Metrics{
		Mem:  protocol.Mem{Total: 64 << 30, Used: 63 << 30},
		Disk: []protocol.Disk{{Mount: "/", Total: 4000 << 30, Used: 3999 << 30}},
		Net:  protocol.Net{RxRate: 1 << 20, TxRate: 1 << 20},
	})

	// 累计流量取数据库里的历史账（traffic_daily 求和），三个节点都算，离线的也算。
	day := store.FormatDay(now.In(time.UTC))
	if err := h.srv.db.FlushTraffic(ctx, []store.TrafficUpdate{
		{NodeID: nodeA, Day: day, RxDelta: 1 << 30, TxDelta: 2 << 30},
		{NodeID: nodeB, Day: day, RxDelta: 4 << 30, TxDelta: 8 << 30},
		{NodeID: nodeC, Day: day, RxDelta: 16 << 20, TxDelta: 32 << 20},
	}, now); err != nil {
		t.Fatalf("写入流量: %v", err)
	}
	// 缓存里可能还留着"落盘之前"的那一份汇总。
	h.srv.trafficCache.invalidate()

	status, body := h.put(t, "/api/v1/settings/ping", map[string]any{
		"interval_sec": 60,
		"targets": []map[string]any{
			{"label": "Cloudflare", "type": "tcp", "host": "1.1.1.1", "port": 443, "enabled": true},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("保存探测目标失败: %d %v", status, body)
	}

	// 三分钟的探测结果（都在窗口内、都在 360 秒网格的左边界附近，见下面的 base）。
	// 加权平均 = (100×100 + 200×100 + 300×50) / (100+100+50) = 180ms；
	// 丢包 = (300-250)/300 = 16.67%。
	//
	// ts 一律取"至少 400 秒前"：桶网格对齐到 360 秒，end 最大可以比 now 早 359 秒，
	// 太新的样本会落在窗口右边界之外。
	base := now.Unix() - 400
	if err := h.srv.db.UpsertPingBuckets(ctx, []store.PingBucket{
		store.NewPingBucket(nodeA, 1, base, 100, 90, 110, 0),
		store.NewPingBucket(nodeA, 1, base+10, 200, 190, 210, 0),
		store.NewPingBucket(nodeA, 1, base+20, 300, 290, 310, 50),
	}); err != nil {
		t.Fatalf("写入探测桶: %v", err)
	}

	body = overviewOf(t, h, "/api/v1/overview?window=1h&buckets=10")

	if got := floatField(t, body, "window_sec"); got != 3600 {
		t.Errorf("window_sec = %v，期望 3600", got)
	}
	if got := floatField(t, body, "buckets"); got != 10 {
		t.Errorf("buckets = %v，期望 10", got)
	}
	if got := floatField(t, body, "bucket_sec"); got != 360 {
		t.Errorf("bucket_sec = %v，期望 360", got)
	}

	totals := totalsOf(t, body)
	if got := floatField(t, totals, "nodes_total"); got != 3 {
		t.Errorf("nodes_total = %v，期望 3", got)
	}
	// 只有节点 1 在 stale-after 之内：离线的节点不该被算进"实时用量"。
	if got := floatField(t, totals, "nodes_online"); got != 1 {
		t.Errorf("nodes_online = %v，期望 1", got)
	}
	if got := floatField(t, totals, "mem_used"); got != float64(8<<30) {
		t.Errorf("mem_used = %v，期望 %d", got, int64(8<<30))
	}
	if got := floatField(t, totals, "mem_total"); got != float64(16<<30) {
		t.Errorf("mem_total = %v，期望 %d", got, int64(16<<30))
	}
	if got := floatField(t, totals, "mem_pct"); !closeTo(got, 50) {
		t.Errorf("mem_pct = %v，期望 50", got)
	}
	if got := floatField(t, totals, "disk_used"); got != float64(100<<30) {
		t.Errorf("disk_used = %v，期望 %d", got, int64(100<<30))
	}
	if got := floatField(t, totals, "disk_total"); got != float64(400<<30) {
		t.Errorf("disk_total = %v，期望 %d", got, int64(400<<30))
	}
	if got := floatField(t, totals, "disk_pct"); !closeTo(got, 25) {
		t.Errorf("disk_pct = %v，期望 25", got)
	}
	// rx 是下行、tx 是上行（与卡片上的 ↑tx / ↓rx 同一套口径）。
	if got := floatField(t, totals, "rx_rate"); got != 1024 {
		t.Errorf("rx_rate = %v，期望 1024（离线节点的速率不该计入）", got)
	}
	if got := floatField(t, totals, "tx_rate"); got != 2048 {
		t.Errorf("tx_rate = %v，期望 2048", got)
	}
	wantRx := int64(1<<30) + int64(4<<30) + int64(16<<20)
	wantTx := int64(2<<30) + int64(8<<30) + int64(32<<20)
	if got := floatField(t, totals, "traffic_rx_total"); got != float64(wantRx) {
		t.Errorf("traffic_rx_total = %v，期望 %d", got, wantRx)
	}
	if got := floatField(t, totals, "traffic_tx_total"); got != float64(wantTx) {
		t.Errorf("traffic_tx_total = %v，期望 %d", got, wantTx)
	}

	// 币种分组：CNY 一组（两个节点）、USD 一组（一个节点），绝不相加。
	// 每组的金额与 /api/v1/nodes 里各节点的 remaining_value_cents 对得上
	// （不在这里重写一遍摊销公式：那会把测试变成 applyPricing 的复读机）。
	status, listBody, _ := h.do(t, http.MethodGet, "/api/v1/nodes", nil, false, nil)
	if status != http.StatusOK {
		t.Fatalf("读取节点列表失败: %d", status)
	}
	wantByCurrency := map[string]int64{}
	rawNodes, _ := listBody["nodes"].([]any)
	for _, raw := range rawNodes {
		n, _ := raw.(map[string]any)
		code, _ := n["currency"].(string)
		cents, _ := n["remaining_value_cents"].(float64)
		wantByCurrency[code] += int64(cents)
	}
	groups, ok := totals["remaining_value"].([]any)
	if !ok {
		t.Fatalf("remaining_value 不是数组: %v", totals["remaining_value"])
	}
	if len(groups) != len(wantByCurrency) || len(groups) != 2 {
		t.Fatalf("remaining_value 应当有 2 个币种（CNY/USD），实际 %v", groups)
	}
	for _, raw := range groups {
		g, _ := raw.(map[string]any)
		code, _ := g["currency"].(string)
		cents, _ := g["cents"].(float64)
		want, ok := wantByCurrency[code]
		if !ok {
			t.Fatalf("出现了没配过的币种 %q: %v", code, groups)
		}
		if int64(cents) != want {
			t.Errorf("%s 的剩余价值 = %d，期望 %d", code, int64(cents), want)
		}
	}
	// 跨币种不相加：总额只可能是其中一组，不可能是两组之和。
	cny := int64(0)
	for _, raw := range groups {
		g, _ := raw.(map[string]any)
		if g["currency"] == "CNY" {
			cny = int64(floatField(t, g, "cents"))
		}
	}
	all := int64(0)
	for _, v := range wantByCurrency {
		all += v
	}
	if cny == all {
		t.Errorf("CNY 分组的金额 %d 等于所有币种之和：跨币种被加在一起了", cny)
	}

	// nodes：只有节点 1（唯一有探测数据的节点）。
	nodes := overviewNodesOf(t, body)
	if len(nodes) != 1 {
		t.Fatalf("nodes 应当只包含有探测数据的节点，实际 %v", nodes)
	}
	mini, ok := nodes["1"].(map[string]any)
	if !ok {
		t.Fatalf("nodes 里缺少节点 1: %v", nodes)
	}
	if got := floatField(t, mini, "lat_ms"); !closeTo(got, 180) {
		t.Errorf("lat_ms = %v，期望 180（按成功探测次数加权）", got)
	}
	if got := floatField(t, mini, "loss_pct"); !closeTo(got, 100.0/6) {
		t.Errorf("loss_pct = %v，期望 16.67", got)
	}
	lat, ok := mini["lat"].([]any)
	if !ok {
		t.Fatalf("lat 不是数组: %v", mini["lat"])
	}
	loss, ok := mini["loss"].([]any)
	if !ok {
		t.Fatalf("loss 不是数组: %v", mini["loss"])
	}
	if len(lat) != 10 || len(loss) != 10 {
		t.Fatalf("lat/loss 的长度必须恒为 buckets=10，实际 %d/%d", len(lat), len(loss))
	}
	// 数据只覆盖 3 分钟的样本，最多落在两个桶里；其余各段必须是 null（不是 0）。
	nulls := 0
	for i := range lat {
		if lat[i] == nil {
			nulls++
		}
		if lat[i] == nil && loss[i] != nil {
			t.Errorf("第 %d 段：延迟是 null 但丢包有值 —— 两段应当同源", i)
		}
	}
	if nulls < 8 {
		t.Errorf("没有数据的段应当是 null，实际只有 %d 段为 null: %v", nulls, lat)
	}
	if nulls == len(lat) {
		t.Errorf("三段样本应当至少落进一个桶里: %v", lat)
	}
}

// 硬盘只取根挂载点：同一块盘挂到多个路径时不能重复计入。
func TestOverviewDiskCountsRootMountOnly(t *testing.T) {
	h := newAuthHarness(t)
	now := time.Now()

	nodeA, _ := createNodeOverHTTP(t, h, "disk-multi")
	nodeB, _ := createNodeOverHTTP(t, h, "disk-no-root")

	// 同一块 100GiB 的盘被挂了三个路径（Agent 会把它们全都上报）。
	onlineState(t, h.srv, nodeA, now, protocol.Metrics{
		Disk: []protocol.Disk{
			{Mount: "/", Total: 100 << 30, Used: 40 << 30},
			{Mount: "/var/lib/probe-agent", Total: 100 << 30, Used: 40 << 30},
			{Mount: "/var/tmp", Total: 100 << 30, Used: 40 << 30},
		},
	})
	// 没有 / 的节点（容器/老 Agent）：退回列表第一项，同样只算一次。
	onlineState(t, h.srv, nodeB, now, protocol.Metrics{
		Disk: []protocol.Disk{
			{Mount: "/data", Total: 50 << 30, Used: 10 << 30},
			{Mount: "/data/logs", Total: 50 << 30, Used: 10 << 30},
		},
	})

	totals := totalsOf(t, overviewOf(t, h, "/api/v1/overview"))
	if got := floatField(t, totals, "disk_used"); got != float64(50<<30) {
		t.Errorf("disk_used = %v，期望 %d（40+10，而不是三个挂载点全加起来）", got, int64(50<<30))
	}
	if got := floatField(t, totals, "disk_total"); got != float64(150<<30) {
		t.Errorf("disk_total = %v，期望 %d", got, int64(150<<30))
	}
}

// 没配探测目标（或这个节点这一小时压根没有探测结果）时，该节点不能出现在 nodes 里。
func TestOverviewOmitsNodesWithoutPingData(t *testing.T) {
	h := newAuthHarness(t)
	ctx := context.Background()
	now := time.Now()

	nodeA, _ := createNodeOverHTTP(t, h, "ping-has-data")
	nodeB, _ := createNodeOverHTTP(t, h, "ping-no-data")
	onlineState(t, h.srv, nodeA, now, protocol.Metrics{})
	onlineState(t, h.srv, nodeB, now, protocol.Metrics{})

	if status, body := h.put(t, "/api/v1/settings/ping", map[string]any{
		"interval_sec": 60,
		"targets": []map[string]any{
			{"label": "CF", "type": "tcp", "host": "1.1.1.1", "port": 443, "enabled": true},
		},
	}); status != http.StatusOK {
		t.Fatalf("保存探测目标失败: %d %v", status, body)
	}
	ts := now.Unix() - 400
	if err := h.srv.db.UpsertPingBuckets(ctx, []store.PingBucket{
		store.NewPingBucket(nodeA, 1, ts, 20, 18, 25, 0),
	}); err != nil {
		t.Fatalf("写入探测桶: %v", err)
	}

	nodes := overviewNodesOf(t, overviewOf(t, h, "/api/v1/overview"))
	if len(nodes) != 1 {
		t.Fatalf("只有节点 %d 有探测数据，nodes 里不该出现别的节点: %v", nodeA, nodes)
	}
	if _, ok := nodes["1"]; !ok {
		t.Fatalf("nodes 里缺少有数据的节点 1: %v", nodes)
	}

	// 目标被全部删掉：库里还残留着这一小时的历史，但整块迷你条都不该再画
	// —— 用户刚删完，首页还挂着格子会让人以为删除没生效。
	if status, body := h.put(t, "/api/v1/settings/ping", map[string]any{
		"interval_sec": 60, "targets": []any{},
	}); status != http.StatusOK {
		t.Fatalf("清空探测目标失败: %d %v", status, body)
	}
	nodes = overviewNodesOf(t, overviewOf(t, h, "/api/v1/overview"))
	if len(nodes) != 0 {
		t.Errorf("一个探测目标都没配时 nodes 应当是空对象，实际 %v", nodes)
	}
}

// 未登录一律 401（总览里有内存/硬盘/金额，比曲线更该守住）。
func TestOverviewRequiresLogin(t *testing.T) {
	h := newAuthHarness(t)
	h.anonymousClient(t)

	if status, _, _ := h.do(t, http.MethodGet, "/api/v1/overview", nil, false, nil); status != http.StatusUnauthorized {
		t.Errorf("未登录读取总览应当 401，实际 %d", status)
	}
}

// window / buckets 的边界：非法值 400，合法值按参数生效。
func TestOverviewParamBoundaries(t *testing.T) {
	h := newAuthHarness(t)
	h.cancel()

	for _, path := range []string{
		"/api/v1/overview?window=abc",
		"/api/v1/overview?window=10s",  // 窗口太小：一段不足一秒没有意义
		"/api/v1/overview?window=200h", // 超过探测数据的保留期
		"/api/v1/overview?buckets=0",
		"/api/v1/overview?buckets=abc",
		"/api/v1/overview?buckets=100000",         // 超过段数上限
		"/api/v1/overview?window=1m&buckets=3600", // 60 秒切 3600 段：桶宽会变成 0
	} {
		status, body, _ := h.do(t, http.MethodGet, path, nil, false, nil)
		if status != http.StatusBadRequest {
			t.Errorf("GET %s = %d，期望 400（%v）", path, status, body)
		}
	}

	// 不传参数时用默认值；窗口不能整除段数时，window_sec 对齐成 bucket_sec × buckets。
	body := overviewOf(t, h, "/api/v1/overview")
	if floatField(t, body, "window_sec") != 3600 || floatField(t, body, "buckets") != 10 {
		t.Errorf("默认参数应当是 1h/10 段: %v", body)
	}

	body = overviewOf(t, h, "/api/v1/overview?window=2h&buckets=8")
	if got := floatField(t, body, "window_sec"); got != 7200 {
		t.Errorf("window_sec = %v，期望 7200", got)
	}
	if got := floatField(t, body, "bucket_sec"); got != 900 {
		t.Errorf("bucket_sec = %v，期望 900", got)
	}

	body = overviewOf(t, h, "/api/v1/overview?window=1h&buckets=7")
	windowSec := floatField(t, body, "window_sec")
	bucketSec := floatField(t, body, "bucket_sec")
	buckets := floatField(t, body, "buckets")
	// 桶号是 (ts-start)/bucket_sec，只有窗口正好是整数倍，桶号才保证落在 [0, buckets)。
	if bucketSec*buckets != windowSec {
		t.Errorf("window_sec(%v) 应当是 bucket_sec(%v) × buckets(%v)", windowSec, bucketSec, buckets)
	}
}
