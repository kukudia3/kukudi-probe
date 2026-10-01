package server

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
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

// bucketTSOf 读出 bucket_ts / bucket_sec / buckets 三个字段（形状不对直接失败）。
func bucketTSOf(t *testing.T, body map[string]any) ([]int64, int64, int) {
	t.Helper()
	raw, ok := body["bucket_ts"].([]any)
	if !ok {
		t.Fatalf("响应里的 bucket_ts 不是数组: %v", body["bucket_ts"])
	}
	ts := make([]int64, len(raw))
	for i, v := range raw {
		f, ok := v.(float64)
		if !ok {
			t.Fatalf("bucket_ts[%d] 不是数字: %v", i, v)
		}
		ts[i] = int64(f)
	}
	return ts, int64(floatField(t, body, "bucket_sec")), int(floatField(t, body, "buckets"))
}

// bucket_ts 是"每格的起始 Unix 秒"，前端拿它显示悬停浮层上的 "HH:MM – HH:MM"。
//
// 为什么不许前端自己推：桶边界由服务端算（桶号 = (ts-start)/bucketSec，且 end 向下
// 对齐到桶宽）。前端用"现在 − 窗口 + i×桶宽"推出来的边界会与真实桶错开最多一整格，
// 显示出来的时间段就不是那一格数据的实际区间 —— 而且这种错位在页面上看不出来。
// 所以这里钉死四条：长度 == buckets、严格升序、相邻差 == bucket_sec、
// 最后一格的结束时间 ≈ 现在（右界对齐到桶宽，所以误差必然小于一格）。
func TestOverviewBucketTimestamps(t *testing.T) {
	h := newAuthHarness(t)
	h.cancel()

	cases := []struct {
		path    string
		sec     int64
		buckets int
	}{
		{"/api/v1/overview?window=1h&buckets=10", 360, 10},
		// 换一组参数：桶宽与段数都不是默认值时，边界同样要跟着后端算。
		{"/api/v1/overview?window=2h&buckets=8", 900, 8},
	}
	for _, c := range cases {
		body := overviewOf(t, h, c.path)
		ts, sec, buckets := bucketTSOf(t, body)

		if buckets != c.buckets || sec != c.sec {
			t.Fatalf("%s: buckets=%d bucket_sec=%d，期望 %d/%d", c.path, buckets, sec, c.buckets, c.sec)
		}
		if len(ts) != buckets {
			t.Fatalf("%s: bucket_ts 的长度 = %d，期望 buckets=%d", c.path, len(ts), buckets)
		}
		for i := 1; i < len(ts); i++ {
			if ts[i] <= ts[i-1] {
				t.Fatalf("%s: bucket_ts 必须严格升序，实际第 %d 格 %d <= 第 %d 格 %d",
					c.path, i, ts[i], i-1, ts[i-1])
			}
			if got := ts[i] - ts[i-1]; got != sec {
				t.Errorf("%s: 第 %d/%d 格的间隔 = %d 秒，期望 bucket_sec=%d", c.path, i-1, i, got, sec)
			}
		}
		// 最后一格的结束时间就是窗口右界：不能比现在还晚，也不能早出一整格。
		now := time.Now().Unix()
		end := ts[len(ts)-1] + sec
		if end > now {
			t.Errorf("%s: 最后一格的结束时间 %d 比现在还晚 %d 秒", c.path, end, end-now)
		}
		if now-end >= sec {
			t.Errorf("%s: 最后一格结束于 %d，比现在早 %d 秒（≥ 一格 %d 秒）", c.path, end, now-end, sec)
		}
		// 第一格起点 = 末格起点 − (buckets-1)×桶宽：整个窗口正好铺满 buckets 格，
		// 中间没有空洞（有空洞的话某些格子会永远拿不到数据）。
		if got, want := ts[0], ts[len(ts)-1]-int64(buckets-1)*sec; got != want {
			t.Errorf("%s: 第一格的起点 = %d，期望 %d", c.path, got, want)
		}
	}
}

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

// /overview 的 nodes[].targets：每个**配置过的**探测目标一项，含 label/host、
// 当前延迟（最近一个有效桶）、整段平均（按成功次数加权）、丢包与 has_data。
//
// 为什么放在 /overview 而不是节点 DTO：探测结果 60 秒才变一次，塞进每秒推送的
// SSE 是纯浪费。这里顺带钉住"加了目标维度之后每个节点仍然只有一次查询"的
// 可观测结果：一条 GROUP BY 同时喂饱分桶（跨目标）与分目标（跨桶）。
func TestOverviewTargetsShape(t *testing.T) {
	h := newAuthHarness(t)
	h.cancel() // 停掉 1 Hz 循环：它会每秒重建一次视图，与这里的时序抢跑
	ctx := context.Background()
	now := time.Now()

	nodeA, _ := createNodeOverHTTP(t, h, "targets-a")
	onlineState(t, h.srv, nodeA, now, protocol.Metrics{})

	// 三个目标：1 有数据、2 有数据但最近一段全丢、3 一个点都没有。
	if status, body := h.put(t, "/api/v1/settings/ping", map[string]any{
		"interval_sec": 60,
		"targets": []map[string]any{
			{"label": "浙江电信", "type": "tcp", "host": "1.1.1.1", "port": 443, "enabled": true},
			{"label": "香港", "type": "tcp", "host": "8.8.8.8", "port": 443, "enabled": true},
			// label 故意留空：服务端存的是它归一化后的值（回落成 host），
			// 前端也有一次回落（见 app.js 的 pingTargetLabel）。
			{"label": "", "type": "icmp", "host": "9.9.9.9", "enabled": true},
		},
	}); status != http.StatusOK {
		t.Fatalf("保存探测目标失败: %d %v", status, body)
	}

	// 样本的时刻按**桶网格**算：end 会被服务端向下对齐到 bucket_sec（360 秒），
	// 直接拿"现在 − 400 秒"会在跨桶边界时落到窗口外。这里自己算一遍 end，
	// 再把两个样本放进两个不同的桶（新桶 200ms、旧桶 100ms）。
	windowSec, buckets := int64(3600), 10
	bucketSec := windowSec / int64(buckets)
	end := now.Unix() - now.Unix()%bucketSec
	newTS, oldTS := end-bucketSec-10, end-3*bucketSec-10
	if err := h.srv.db.UpsertPingBuckets(ctx, []store.PingBucket{
		store.NewPingBucket(nodeA, 1, oldTS, 100, 90, 110, 0),
		store.NewPingBucket(nodeA, 1, newTS, 200, 190, 210, 0),
		// 目标 2：最近一段整段全丢（up=0）——它的"当前延迟"没有样本。
		store.NewPingBucket(nodeA, 2, newTS, 0, 0, 0, 100),
	}); err != nil {
		t.Fatalf("写入探测桶: %v", err)
	}

	nodes := overviewNodesOf(t, overviewOf(t, h, "/api/v1/overview?window=1h&buckets=10"))
	mini, ok := nodes["1"].(map[string]any)
	if !ok {
		t.Fatalf("nodes 里缺少节点 1: %v", nodes)
	}
	rawTargets, ok := mini["targets"].([]any)
	if !ok {
		t.Fatalf("nodes[1].targets 不是数组（null 会让前端多一条判断）: %v", mini["targets"])
	}
	if len(rawTargets) != 3 {
		t.Fatalf("targets 应当把**每个配置过的目标**都列出来（含没有数据的），实际 %d 项: %v",
			len(rawTargets), rawTargets)
	}
	targets := make([]map[string]any, len(rawTargets))
	for i, raw := range rawTargets {
		item, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("targets[%d] 不是对象: %v", i, raw)
		}
		targets[i] = item
	}

	// 顺序 = 配置顺序（前端按顺序取色与显示，与详情页延迟图的图例一致）。
	if got := targets[0]["label"]; got != "浙江电信" {
		t.Errorf("targets[0].label = %v，期望「浙江电信」（按配置顺序）", got)
	}
	if got := targets[0]["host"]; got != "1.1.1.1" {
		t.Errorf("targets[0].host = %v，期望 1.1.1.1（label 留空时前端要靠它回落）", got)
	}
	// 目标 1：当前值取**最近一个有效桶**（200ms），整段均值按成功次数加权
	// （两个桶各一次成功探测 → (100+200)/2 = 150ms）。
	if got := floatField(t, targets[0], "lat_ms"); !closeTo(got, 200) {
		t.Errorf("targets[0].lat_ms = %v，期望 200（最近一段的延迟，不是整段均值）", got)
	}
	if got := floatField(t, targets[0], "avg_ms"); !closeTo(got, 150) {
		t.Errorf("targets[0].avg_ms = %v，期望 150（整段按成功次数加权）", got)
	}
	if got := floatField(t, targets[0], "loss_pct"); !closeTo(got, 0) {
		t.Errorf("targets[0].loss_pct = %v，期望 0", got)
	}
	if got := targets[0]["has_data"]; got != true {
		t.Errorf("targets[0].has_data = %v，期望 true", got)
	}

	// 目标 2：整段全丢 —— 有数据（has_data=true）、丢包 100%、但没有延迟样本，
	// 所以当前值与均值都是 0（前端画成 —，而不是 0 ms）。
	if got := targets[1]["has_data"]; got != true {
		t.Errorf("targets[1].has_data = %v，期望 true（它有探测记录，只是全丢了）", got)
	}
	if got := floatField(t, targets[1], "loss_pct"); !closeTo(got, 100) {
		t.Errorf("targets[1].loss_pct = %v，期望 100", got)
	}
	if got := floatField(t, targets[1], "avg_ms"); got != 0 {
		t.Errorf("targets[1].avg_ms = %v，期望 0（全丢没有延迟样本，不能把 0ms 当成很快）", got)
	}
	if got := floatField(t, targets[1], "lat_ms"); got != 0 {
		t.Errorf("targets[1].lat_ms = %v，期望 0", got)
	}

	// 目标 3：这一小时一个点都没有，但照样在数组里（"这个目标没数据"本身就是信息）。
	if got := targets[2]["has_data"]; got != false {
		t.Errorf("targets[2].has_data = %v，期望 false", got)
	}
	if got := targets[2]["label"]; got != "9.9.9.9" {
		t.Errorf("targets[2].label = %v，期望落回 host（9.9.9.9）", got)
	}
	if got := floatField(t, targets[2], "lat_ms"); got != 0 {
		t.Errorf("targets[2].lat_ms = %v，期望 0", got)
	}
}

// /overview 里每个节点的延迟聚合口径：**跨目标**按成功探测次数加权
// （(200×3+20)/4 = 155），不是"各目标的平均再平均"（那会得到 110）。
//
// 这条用例原来叫 TestOverviewNodeThresholdMS —— 那时候它顺带钉住节点级的
// "慢阈值"（threshold_ms）。用户把"慢"整个概念删掉之后那个字段没有了，
// 但**加权口径**这一半照样要守：首页迷你条左边那个数、格子分级用的基准
// （前端拿它当窗口均值）全靠它。所以留着夹具、去掉慢那部分断言。
func TestOverviewNodeLatencyAggregation(t *testing.T) {
	h := newAuthHarness(t)
	h.cancel() // 停掉 1 Hz 循环：它会每秒重建一次视图，与这里的时序抢跑
	ctx := context.Background()
	now := time.Now()

	nodeA, _ := createNodeOverHTTP(t, h, "lat-agg-a")
	// 节点 2：有探测记录但整段全丢（没有延迟样本）。
	nodeB, _ := createNodeOverHTTP(t, h, "lat-agg-none")
	onlineState(t, h.srv, nodeA, now, protocol.Metrics{})
	onlineState(t, h.srv, nodeB, now, protocol.Metrics{})

	if status, body := h.put(t, "/api/v1/settings/ping", map[string]any{
		"interval_sec": 60,
		"targets": []map[string]any{
			{"label": "美西", "type": "tcp", "host": "1.1.1.1", "port": 443, "enabled": true},
			{"label": "香港", "type": "tcp", "host": "8.8.8.8", "port": 443, "enabled": true},
		},
	}); status != http.StatusOK {
		t.Fatalf("保存探测目标失败: %d %v", status, body)
	}

	// 窗口边界与 TestOverviewTargetsShape 同一套算法（end 向下对齐到桶宽 360 秒）。
	windowSec, buckets := int64(3600), 10
	bucketSec := windowSec / int64(buckets)
	end := now.Unix() - now.Unix()%bucketSec
	ts := end - 10
	if err := h.srv.db.UpsertPingBuckets(ctx, []store.PingBucket{
		// 目标 1（美西）：三个 200ms 的桶。
		store.NewPingBucket(nodeA, 1, ts, 200, 195, 205, 0),
		store.NewPingBucket(nodeA, 1, ts-bucketSec, 200, 195, 205, 0),
		store.NewPingBucket(nodeA, 1, ts-2*bucketSec, 200, 195, 205, 0),
		// 目标 2（香港）：一个 20ms 的桶。
		store.NewPingBucket(nodeA, 2, ts, 20, 18, 25, 0),
		// 节点 2：整段全丢。
		store.NewPingBucket(nodeB, 1, ts, 0, 0, 0, 100),
	}); err != nil {
		t.Fatalf("写入探测桶: %v", err)
	}

	nodes := overviewNodesOf(t, overviewOf(t, h, "/api/v1/overview?window=1h&buckets=10"))

	miniA, ok := nodes[strconv.FormatInt(nodeA, 10)].(map[string]any)
	if !ok {
		t.Fatalf("nodes 里缺少节点 %d: %v", nodeA, nodes)
	}
	// 迷你条那两个数字的口径：延迟按成功次数加权 = (200×3+20)/4 = 155 ——
	// 不是"各目标的平均再平均"（那会得到 (200+20)/2 = 110）。
	if got := floatField(t, miniA, "lat_ms"); !closeTo(got, 155) {
		t.Errorf("lat_ms = %v，期望 155（按成功探测次数加权）", got)
	}
	if got := floatField(t, miniA, "loss_pct"); got != 0 {
		t.Errorf("loss_pct = %v，期望 0", got)
	}

	// 整段全丢的节点：loss 是 100（真丢包照样统计），延迟分桶是 null。
	miniB, ok := nodes[strconv.FormatInt(nodeB, 10)].(map[string]any)
	if !ok {
		t.Fatalf("nodes 里缺少节点 %d: %v", nodeB, nodes)
	}
	if got := floatField(t, miniB, "lat_ms"); got != 0 {
		t.Errorf("整段全丢时 lat_ms = %v，期望 0（没有延迟样本）", got)
	}
	if got := floatField(t, miniB, "loss_pct"); !closeTo(got, 100) {
		t.Errorf("整段全丢时 loss_pct = %v，期望 100", got)
	}
	// 全丢时延迟分桶是 null（没有样本），不是 0 —— 前端画成浅灰，不是绿格。
	lat, _ := miniB["lat"].([]any)
	for i, v := range lat {
		if v != nil {
			t.Errorf("第 %d 段全丢，延迟应当是 null，实际 %v", i, v)
		}
	}
}

// 一个探测目标都没配（配置被清空）时 targets 必须是**空数组**而不是 null：
// 前端拿到 null 会去读它的属性。
func TestOverviewTargetsEmptyIsArrayNotNull(t *testing.T) {
	got := orderOverviewTargets(store.OverviewPing{
		Targets: []store.OverviewPingTarget{{ID: 1, LatMS: 10}},
	}, nil)
	if got.Targets == nil {
		t.Fatal("没有配置目标时 Targets 不该是 nil")
	}
	if len(got.Targets) != 0 {
		t.Fatalf("没有配置目标时不该留下任何项（已被删除的目标连同历史一起丢掉），实际 %v", got.Targets)
	}
	data, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("序列化: %v", err)
	}
	if !strings.Contains(string(data), `"targets":[]`) {
		t.Fatalf("targets 应当序列化成 []，实际 %s", data)
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
