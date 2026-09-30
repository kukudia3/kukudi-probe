package e2e

import (
	"context"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"probe/internal/config"
)

// TestHistoryReachesSeriesAPI 验证"上报 → 内存聚合 → 落盘 → 历史接口"这条完整链路。
//
// 唯一为了让测试跑得快而改的参数是 --flush-interval（默认 10 秒，这里 1 秒）；
// 桶宽、档位、查询逻辑全部走生产代码。
func TestHistoryReachesSeriesAPI(t *testing.T) {
	logs := &captureHandler{}
	h := startServerFull(t, "127.0.0.1:0", filepath.Join(t.TempDir(), "probe.db"),
		slog.New(logs), func(c *config.Server) { c.FlushInterval = time.Second })

	br := newBrowser(t, "http://"+h.addr)
	setupAdmin(t, br, waitSetupCode(t, logs))
	nodeID, token := createNodeViaAPI(t, br, "history-01")

	client, _ := newClient(t, "http://"+h.addr, token)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = client.Run(ctx) }()

	path := "/api/v1/nodes/" + strconv.FormatInt(nodeID, 10) + "/series?metric=mem&range=1h"
	waitFor(t, 40*time.Second, "历史曲线出现数据", func() bool {
		status, body := br.do(http.MethodGet, path, nil, false)
		if status != http.StatusOK {
			return false
		}
		points, ok := body["points"].([]any)
		return ok && len(points) >= 1
	})

	status, body := br.do(http.MethodGet, path, nil, false)
	if status != http.StatusOK {
		t.Fatalf("曲线接口状态码 = %d", status)
	}
	meta, _ := body["meta"].(map[string]any)
	if meta["key"] != "1h" || meta["bucket_sec"] != float64(10) || meta["tick_base_sec"] != float64(600) {
		t.Fatalf("meta 不对: %v", meta)
	}

	points, _ := body["points"].([]any)
	if len(points) == 0 {
		t.Fatal("没有返回任何历史点")
	}
	for _, raw := range points {
		p, _ := raw.([]any)
		if len(p) != 3 {
			t.Fatalf("点结构 = %v，期望 [ts, avg, max]", p)
		}
		avg, _ := p[1].(float64)
		max, _ := p[2].(float64)
		// fixture 快照的内存使用率是 51.77%，聚合后仍应在这个范围附近。
		if avg < 50 || avg > 53 {
			t.Fatalf("内存平均值 = %v，与 /proc 快照不符", avg)
		}
		// 不能精确比较：桶里装的是同一个内存值（fixture 是静态快照），
		// avg = 累加和 / 样本数 会累积浮点舍入，可能比 max 大最后一位。
		// 曾经写成 max < avg 的精确比较，样本数一变就偶发失败（在 CI 上真实发生过）。
		if max < avg-1e-9 {
			t.Fatalf("最大值 %v 小于平均值 %v", max, avg)
		}
	}

	// 六档都必须能查（哪怕后面几档还没数据）。
	for _, key := range []string{"1h", "6h", "12h", "1d", "3d", "7d"} {
		status, body := br.do(http.MethodGet,
			"/api/v1/nodes/"+strconv.FormatInt(nodeID, 10)+"/series?metric=cpu&range="+key, nil, false)
		if status != http.StatusOK {
			t.Fatalf("%s 档查询失败: %d %v", key, status, body)
		}
		meta, _ := body["meta"].(map[string]any)
		if meta["key"] != key {
			t.Fatalf("%s 档返回的 meta.key = %v", key, meta["key"])
		}
	}

	// 详情接口也要能拿到可用率与档位参数。
	status, body = br.do(http.MethodGet, "/api/v1/nodes/"+strconv.FormatInt(nodeID, 10), nil, false)
	if status != http.StatusOK {
		t.Fatalf("详情接口状态码 = %d", status)
	}
	if ranges, _ := body["ranges"].([]any); len(ranges) != 6 {
		t.Fatalf("详情里的档位数量 = %d", len(ranges))
	}
	uptime, _ := body["uptime"].(map[string]any)
	day, _ := uptime["1d"].(map[string]any)
	if day == nil {
		t.Fatalf("详情里应当包含 24h 可用率字段: %v", uptime)
	}
	// 1d/7d 档的源表是 samples_1m，它由每分钟的 rollup 填充；
	// 这条用例只跑十几秒，所以这里只要求"有字段、有数据时数值合理"。
	if day["has_data"] == true {
		if pct, _ := day["pct"].(float64); pct < 95 {
			t.Fatalf("Agent 一直在跑，24h 可用率不该这么低: %v", pct)
		}
	}
}
