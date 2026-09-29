package server

import (
	"context"
	"net/http"
	"testing"
	"time"

	"probe/internal/config"
	"probe/internal/protocol"
	"probe/internal/store"
)

func TestPipelineWritesAndRollsUpSamples(t *testing.T) {
	cfg := config.Default()
	cfg.FlushInterval = time.Second
	h := newAuthHarnessWithConfig(t, cfg)
	ctx := context.Background()

	status, body := h.post(t, "/api/v1/nodes", map[string]any{"name": "pipe-01", "interval_sec": 1}, nil)
	if status != http.StatusCreated {
		t.Fatalf("创建节点失败: %d %v", status, body)
	}
	node, _ := body["node"].(map[string]any)
	id := int64(node["id"].(float64))

	// 造 3 分钟前的数据：这样 10 秒桶与 1 分钟桶都已经结束。
	base := time.Now().Add(-3 * time.Minute)
	base = time.Unix(base.Unix()-(base.Unix()%10), 0)
	for i := 0; i < 18; i++ {
		at := base.Add(time.Duration(i) * 10 * time.Second)
		h.srv.agg.add(id, 1, protocol.Metrics{
			CPUPct: float64(i),
			Mem:    protocol.Mem{Pct: 50},
			Disk:   []protocol.Disk{{Mount: "/", Pct: 60}},
			Net:    protocol.Net{Iface: "eth0", RxRate: float64(100 * (i + 1))},
			LatMS:  float64(20 + i),
		}, at)
	}

	h.srv.flushSamples(ctx)
	count, err := h.srv.db.CountSamples(ctx, store.TableSamples10s, id, base.Add(-time.Minute), time.Now())
	if err != nil {
		t.Fatalf("统计 10 秒桶: %v", err)
	}
	if count != 18 {
		t.Fatalf("10 秒桶 = %d，期望 18", count)
	}

	h.srv.rollupSamplesSince(ctx, rollupCatchUp)
	count1m, err := h.srv.db.CountSamples(ctx, store.TableSamples1m, id, base.Add(-time.Minute), time.Now())
	if err != nil {
		t.Fatalf("统计 1 分钟桶: %v", err)
	}
	// 18 个 10 秒桶覆盖 180 秒；按自然分钟切分可能是 3~4 个桶
	// （首尾可能是残桶，且最后一个未封闭的分钟桶不会聚合）。
	if count1m < 2 || count1m > 4 {
		t.Fatalf("1 分钟桶 = %d，期望 2-4 个", count1m)
	}
	// 每个 1 分钟桶都必须落在 60 秒网格上，并且 up_cnt 不超过 all_cnt。
	rows1m, err := h.srv.db.Reader().QueryContext(ctx,
		`SELECT ts, up_cnt, all_cnt FROM samples_1m WHERE node_id = ? ORDER BY ts`, id)
	if err != nil {
		t.Fatalf("读取 1 分钟桶: %v", err)
	}
	defer func() { _ = rows1m.Close() }()
	for rows1m.Next() {
		var ts, up, all int64
		if err := rows1m.Scan(&ts, &up, &all); err != nil {
			t.Fatalf("扫描: %v", err)
		}
		if ts%60 != 0 {
			t.Fatalf("1 分钟桶起点 %d 不是 60 的倍数", ts)
		}
		// 测试数据是"每个 10 秒桶里恰好 1 个样本"（间隔 1 秒 → 每桶应有 10 帧），
		// 所以 all 必须是 up 的 10 倍；一个 1 分钟桶最多聚合 6 个 10 秒桶。
		maxAll := int64(6) * uptimeScale * bucketWidth
		if up <= 0 || all != up*10 || all > maxAll {
			t.Fatalf("1 分钟桶 up/all = %d/%d（上限 %d）", up, all, maxAll)
		}
	}

	// 运行态也应当被写出去。
	h.srv.state.Update(id, 1, protocol.Metrics{
		CPUPct: 12.5, Mem: protocol.Mem{Pct: 50}, Net: protocol.Net{Iface: "eth0"},
	}, 0, time.Now())
	h.srv.flushRuntime(ctx)
	rows, err := h.srv.db.LoadRuntime(ctx)
	if err != nil {
		t.Fatalf("读取运行态: %v", err)
	}
	if len(rows) != 1 || rows[0].CPUPct != 12.5 || rows[0].Status == "" {
		t.Fatalf("运行态内容不对: %+v", rows)
	}

	// 清理：把保留期设成 1 分钟，验证"只删旧的、留下新的"。
	cfg2 := config.Default()
	cfg2.Retention10s = time.Minute
	h.srv.cfg = cfg2
	h.srv.purge(ctx)

	count, err = h.srv.db.CountSamples(ctx, store.TableSamples10s, id, base.Add(-time.Minute), time.Now())
	if err != nil {
		t.Fatalf("统计: %v", err)
	}
	if count == 0 || count >= 18 {
		t.Fatalf("清理后 10 秒桶 = %d，期望删掉一部分、留下最近 1 分钟内的", count)
	}
	var oldest int64
	if err := h.srv.db.Reader().QueryRowContext(ctx,
		`SELECT MIN(ts) FROM samples_10s WHERE node_id = ?`, id).Scan(&oldest); err != nil {
		t.Fatalf("读取最早桶: %v", err)
	}
	if cutoff := time.Now().Add(-time.Minute).Unix(); oldest < cutoff {
		t.Fatalf("清理后仍有过期桶：最早 %d < 截止 %d", oldest, cutoff)
	}
}

func TestSeedFromRuntimeRestoresLastState(t *testing.T) {
	h := newAuthHarness(t)
	ctx := context.Background()

	status, body := h.post(t, "/api/v1/nodes", map[string]any{"name": "seed-01", "interval_sec": 1}, nil)
	if status != http.StatusCreated {
		t.Fatalf("创建节点失败: %d %v", status, body)
	}
	node, _ := body["node"].(map[string]any)
	id := int64(node["id"].(float64))

	if err := h.srv.db.UpsertRuntime(ctx, []store.RuntimeRow{{
		NodeID: id, LastSeen: time.Now().Add(-time.Hour).Unix(), Status: "offline",
		CPUPct: 33, MemPct: 44, DiskPct: 55, UptimeSec: 3600, Iface: "eth0",
		OSName: "Debian", Kernel: "6.1.0", AgentVersion: "0.1.0",
	}}, time.Now()); err != nil {
		t.Fatalf("写入运行态: %v", err)
	}

	// 模拟"服务端重启后恢复"：内存状态是空的。
	h.srv.State().Delete(id)
	if n, err := h.srv.seedFromRuntime(ctx); err != nil || n != 1 {
		t.Fatalf("恢复运行态 = %d, err=%v", n, err)
	}

	st, ok := h.srv.State().Get(id)
	if !ok {
		t.Fatal("恢复后应当有内存状态")
	}
	if st.Metrics.CPUPct != 33 || st.Metrics.Mem.Pct != 44 {
		t.Fatalf("恢复的指标不对: %+v", st.Metrics)
	}
	if st.Connected {
		t.Fatal("恢复出来的节点不能被标记成已连接")
	}
	if st.Info.OS.Name != "Debian" {
		t.Fatalf("恢复的静态信息不对: %+v", st.Info)
	}

	// 恢复后界面看到的是"离线"（因为 last_seen 是一小时前）。
	status, list := h.get(t, "/api/v1/nodes")
	if status != http.StatusOK {
		t.Fatalf("查询节点失败: %d", status)
	}
	got := firstNodeFromList(t, list)
	if got["status"] != "offline" {
		t.Fatalf("恢复后的状态 = %v，期望 offline", got["status"])
	}
}
