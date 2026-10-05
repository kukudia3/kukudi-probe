package harness

import (
	"context"
	"testing"
	"time"

	"probe/internal/store"
)

// CreatePingTarget 走真接口配一个探测目标（顺带把探测间隔设成 intervalSec），
// 返回目标的 ID。
//
// 第二次调用时目标按 (type, host, port) 复用同一个 id（服务端就是这么匹配的），
// 所以只改间隔、不会把曲线身份换掉。
func CreatePingTarget(t *testing.T, br *Browser, label, host string, port, intervalSec int) int64 {
	t.Helper()
	status, body := br.Do("PUT", "/api/v1/settings/ping", map[string]any{
		"interval_sec": intervalSec,
		"targets": []map[string]any{
			{"label": label, "type": "tcp", "host": host, "port": port, "enabled": true},
		},
	}, true)
	if status != 200 {
		t.Fatalf("保存探测目标失败: %d %v", status, body)
	}
	targets, _ := body["targets"].([]any)
	if len(targets) != 1 {
		t.Fatalf("探测目标数量 = %d，期望 1", len(targets))
	}
	first, _ := targets[0].(map[string]any)
	id, _ := first["id"].(float64)
	if id <= 0 {
		t.Fatalf("探测目标 ID 不合法: %v", first)
	}
	if got, _ := body["interval_sec"].(float64); int(got) != intervalSec {
		t.Fatalf("探测间隔 = %v，期望 %d", body["interval_sec"], intervalSec)
	}
	return int64(id)
}

// SeedPingBuckets 按 step 秒的间距喂一段延迟曲线（默认场景就是**真实的 1 分钟桶**）。
//
// 平均值在 20~39ms 之间小幅起伏，峰值给到平均值的 6 倍以上 ——
// 这样"轴要不要算峰值"在画面上一眼可辨（默认关掉之后轴会矮一大截）。
//
// step 就是"探测间隔"：300 秒一行时，60 秒宽的桶里每 5 个才有 1 个有数据 ——
// 这正是"探测间隔 > 桶宽"要复现的情形。
func SeedPingBuckets(t *testing.T, h *Harness, nodeID, targetID int64, step int, span time.Duration) {
	t.Helper()
	now := time.Now()
	end := now.Unix() - now.Unix()%int64(step) - int64(step)
	start := end - int64(span.Seconds())
	var buckets []store.PingBucket
	for ts := start; ts <= end; ts += int64(step) {
		i := (ts / int64(step)) % 20
		avg := float64(20 + i) // 20…39 ms
		peak := avg*6 + 30     // 150…264 ms
		buckets = append(buckets, store.NewPingBucket(nodeID, targetID, ts, avg, avg-5, peak, 0))
	}
	if len(buckets) < 20 {
		t.Fatalf("播撒的探测桶只有 %d 个，太少", len(buckets))
	}
	if err := h.DB.UpsertPingBuckets(context.Background(), buckets); err != nil {
		t.Fatalf("写入探测桶: %v", err)
	}
	t.Logf("播撒探测桶 %d 个（节点 %d，ts %d … %d，步长 %d 秒）", len(buckets), nodeID, start, end, step)
}

// SeedCPUSamples 写一段 CPU 曲线（10 秒桶）。返回这些点的 ts（升序）。
//
// 只写窗口**中间**的一段（25 分钟前到 10 分钟前）：1h 档的查询窗口两端会对齐到
// 桶网格，写满整小时的话首尾点会随着 now 往前走而变，断言就飘了。
func SeedCPUSamples(t *testing.T, h *Harness, nodeID int64) []int64 {
	t.Helper()
	end := time.Now().Add(-10 * time.Minute).Truncate(10 * time.Second)
	start := time.Now().Add(-25 * time.Minute).Truncate(10 * time.Second)
	buckets := make([]store.SampleBucket, 0, 90)
	ticks := make([]int64, 0, 90)
	for ts := start; !ts.After(end); ts = ts.Add(10 * time.Second) {
		i := len(buckets)
		buckets = append(buckets, store.SampleBucket{
			NodeID: nodeID, TS: ts.Unix(),
			CPUAvg: float64(10 + i%20), CPUMax: float64(12 + i%20),
			MemAvg: 20, MemMax: 22, Up: 10, All: 10,
		})
		ticks = append(ticks, ts.Unix())
	}
	if len(buckets) < 30 {
		t.Fatalf("播撒的 CPU 桶只有 %d 个，太少", len(buckets))
	}
	if err := h.DB.InsertBuckets(context.Background(), store.TableSamples10s, buckets); err != nil {
		t.Fatalf("写入 CPU 桶: %v", err)
	}
	t.Logf("播撒 CPU 桶 %d 个（ts %d … %d，步长 10 秒）", len(buckets), ticks[0], ticks[len(ticks)-1])
	return ticks
}

// CreateNodeWithGroup 建一台带分组的节点（group 传空串就是"没填分组"）。
func CreateNodeWithGroup(t *testing.T, br *Browser, name, group string) int64 {
	t.Helper()
	status, body := br.Do("POST", "/api/v1/nodes", map[string]any{
		"name": name, "group_name": group, "region": "HK", "interval_sec": 1,
	}, true)
	if status != 201 {
		t.Fatalf("创建节点 %s 失败: %d %v", name, status, body)
	}
	node, _ := body["node"].(map[string]any)
	id, _ := node["id"].(float64)
	if id <= 0 {
		t.Fatalf("创建节点 %s 没有返回 id: %v", name, body)
	}
	return int64(id)
}

func ContainsString(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}
