package server

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"probe/internal/config"
	"probe/internal/protocol"
	"probe/internal/store"
)

func TestSeriesAPIReturnsDesignShape(t *testing.T) {
	h := newAuthHarness(t)
	ctx := context.Background()

	status, body := h.post(t, "/api/v1/nodes", map[string]any{"name": "series-01", "interval_sec": 1}, nil)
	if status != http.StatusCreated {
		t.Fatalf("创建节点失败: %d %v", status, body)
	}
	node, _ := body["node"].(map[string]any)
	id := int64(node["id"].(float64))

	// 往 10 秒表里放 6 个点（1h 档的源表）。
	now := time.Now()
	base := now.Add(-10 * time.Minute).Unix()
	base -= base % 10
	buckets := make([]store.SampleBucket, 0, 6)
	for i := int64(0); i < 6; i++ {
		buckets = append(buckets, store.SampleBucket{
			NodeID: id, TS: base + i*10,
			CPUAvg: float64(10 + i), CPUMax: float64(20 + i),
			MemAvg: 50, MemMax: 55, Up: 10, All: 10,
		})
	}
	if err := h.srv.db.InsertBuckets(ctx, store.TableSamples10s, buckets); err != nil {
		t.Fatalf("写入桶: %v", err)
	}

	status, body = h.get(t, "/api/v1/nodes/1/series?metric=cpu&range=1h")
	if status != http.StatusOK {
		t.Fatalf("查询曲线失败: %d %v", status, body)
	}
	meta, _ := body["meta"].(map[string]any)
	if meta["key"] != "1h" || meta["bucket_sec"] != float64(10) || meta["tick_base_sec"] != float64(600) {
		t.Fatalf("meta 不对: %v", meta)
	}
	if meta["tick_label_sec"] != float64(600) || meta["mobile_agg_sec"] != float64(0) {
		t.Fatalf("刻度/手机聚合参数不对: %v", meta)
	}
	points, _ := body["points"].([]any)
	if len(points) != 6 {
		t.Fatalf("点数 = %d，期望 6", len(points))
	}
	first, _ := points[0].([]any)
	if len(first) != 3 {
		t.Fatalf("点结构 = %v，期望 [ts, avg, max]", first)
	}
	if first[0].(float64) != float64(base) || first[1].(float64) != 10 || first[2].(float64) != 20 {
		t.Fatalf("第一个点 = %v", first)
	}
	if body["metric"] != "cpu" {
		t.Fatalf("metric 字段 = %v", body["metric"])
	}

	// 六个档位都必须能查（哪怕没有数据）。
	for _, key := range []string{"1h", "6h", "12h", "1d", "3d", "7d"} {
		status, body = h.get(t, "/api/v1/nodes/1/series?metric=cpu&range="+key)
		if status != http.StatusOK {
			t.Fatalf("%s 查询失败: %d %v", key, status, body)
		}
		meta, _ := body["meta"].(map[string]any)
		if meta["key"] != key {
			t.Fatalf("%s 返回的 meta.key = %v", key, meta["key"])
		}
		if _, ok := body["points"].([]any); !ok && body["points"] != nil {
			t.Fatalf("%s 的 points 不是数组: %v", key, body["points"])
		}
	}
}

func TestSeriesAPIRejectsBadInput(t *testing.T) {
	h := newAuthHarness(t)

	status, body := h.post(t, "/api/v1/nodes", map[string]any{"name": "series-02", "interval_sec": 1}, nil)
	if status != http.StatusCreated {
		t.Fatalf("创建节点失败: %d %v", status, body)
	}

	cases := []struct {
		path string
		want int
		code string
	}{
		{"/api/v1/nodes/1/series?range=1y", http.StatusBadRequest, "bad_range"},
		{"/api/v1/nodes/1/series?range=", http.StatusOK, ""},
		{"/api/v1/nodes/1/series?metric=evil", http.StatusBadRequest, "bad_metric"},
		{"/api/v1/nodes/1/series?metric=cpu_avg", http.StatusBadRequest, "bad_metric"},
		{"/api/v1/nodes/999/series?metric=cpu&range=1h", http.StatusNotFound, "not_found"},
		{"/api/v1/nodes/abc/series", http.StatusBadRequest, "bad_request"},
	}
	for _, tc := range cases {
		status, body := h.get(t, tc.path)
		if status != tc.want {
			t.Errorf("%s 状态码 = %d，期望 %d（%v）", tc.path, status, tc.want, body)
			continue
		}
		if tc.code != "" {
			errObj, _ := body["error"].(map[string]any)
			if errObj == nil || errObj["code"] != tc.code {
				t.Errorf("%s 错误码 = %v，期望 %s", tc.path, errObj, tc.code)
			}
		}
	}
}

func TestSeriesRequiresLogin(t *testing.T) {
	// 不带 Cookie 的裸客户端（未登录状态由 auth 中间件挡下）。
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir()+"/probe.db")
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := New(config.Default(), db, slog.New(slog.DiscardHandler), time.UTC)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	client := &http.Client{Timeout: 5 * time.Second}
	for _, path := range []string{"/api/v1/nodes/1", "/api/v1/nodes/1/series"} {
		resp, err := client.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("请求 %s: %v", path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s 未登录应当 401，实际 %d", path, resp.StatusCode)
		}
	}
}

func TestNodeDetailAPI(t *testing.T) {
	h := newAuthHarness(t)
	ctx := context.Background()

	status, body := h.post(t, "/api/v1/nodes", map[string]any{
		"name": "detail-01", "group_name": "香港", "region": "HK", "interval_sec": 1,
	}, nil)
	if status != http.StatusCreated {
		t.Fatalf("创建节点失败: %d %v", status, body)
	}
	node, _ := body["node"].(map[string]any)
	id := int64(node["id"].(float64))

	// 放进一些历史数据，让 24h 可用率有值。
	// 注意 1d/7d 档的源表是 samples_1m（见 docs/DESIGN.md §10.3）。
	now := time.Now()
	base := now.Add(-20 * time.Minute).Unix()
	base -= base % 60
	buckets := make([]store.SampleBucket, 0, 10)
	for i := int64(0); i < 10; i++ {
		buckets = append(buckets, store.SampleBucket{
			NodeID: id, TS: base + i*60, CPUAvg: 30, CPUMax: 35,
			MemAvg: 50, MemMax: 55, Up: 9, All: 10,
		})
	}
	if err := h.srv.db.InsertBuckets(ctx, store.TableSamples1m, buckets); err != nil {
		t.Fatalf("写入桶: %v", err)
	}

	h.srv.State().Update(id, 1, testMetricsForDetail(), 0, time.Now())

	status, body = h.get(t, "/api/v1/nodes/1")
	if status != http.StatusOK {
		t.Fatalf("详情接口失败: %d %v", status, body)
	}
	got, _ := body["node"].(map[string]any)
	if got["name"] != "detail-01" || got["region"] != "HK" {
		t.Fatalf("详情内容不对: %v", got)
	}
	if got["cpu_pct"] != 12.5 || got["status"] != "online" {
		t.Fatalf("详情实时字段不对: %v", got)
	}

	uptime, _ := body["uptime"].(map[string]any)
	day, _ := uptime["1d"].(map[string]any)
	if day == nil || day["has_data"] != true {
		t.Fatalf("24h 可用率缺失: %v", uptime)
	}
	if pct, _ := day["pct"].(float64); pct < 89.9 || pct > 90.1 {
		t.Fatalf("24h 可用率 = %v，期望 90", pct)
	}

	ranges, _ := body["ranges"].([]any)
	if len(ranges) != 6 {
		t.Fatalf("档位数量 = %d，期望 6", len(ranges))
	}
	first, _ := ranges[0].(map[string]any)
	if first["key"] != "1h" || first["bucket_sec"] != float64(10) {
		t.Fatalf("第一个档位 = %v", first)
	}

	status, _ = h.get(t, "/api/v1/nodes/999")
	if status != http.StatusNotFound {
		t.Fatalf("不存在的节点应当 404，实际 %d", status)
	}
}

func testMetricsForDetail() protocol.Metrics {
	return protocol.Metrics{
		CPUPct:    12.5,
		Mem:       protocol.Mem{Total: 1 << 30, Used: 1 << 29, Pct: 50},
		Disk:      []protocol.Disk{{Mount: "/", FS: "ext4", Pct: 50}},
		Net:       protocol.Net{Iface: "eth0", RxRate: 1024},
		LatMS:     10,
		UptimeSec: 100,
	}
}
