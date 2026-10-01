package server

import (
	"context"
	"time"

	"probe/internal/protocol"
	"probe/internal/state"
	"probe/internal/store"
)

// 后台流水线的周期。
const (
	rollupEvery    = time.Minute
	runtimeEvery   = time.Minute
	retentionEvery = time.Hour
	// rollupCatchUp 是启动时的回补窗口：异常退出后把这段空洞一次补上。
	rollupCatchUp = 2 * time.Hour
	// rollupSteady 是稳态每分钟的重算窗口。
	//
	// 只重算最近几分钟：10 秒桶最迟在 60 秒内落盘，稳态下更早的桶早已定稿，
	// 每分钟把 2 小时重算一遍纯属重复读写（每个 1 分钟桶会被重写上百次）。
	// 取 10 分钟是为了容纳"落盘失败后重试"的情形（见 flushSamples）。
	rollupSteady = 10 * time.Minute
)

// pipelineLoop 负责把内存里的数据按周期落到 SQLite。
//
// 只用 1 个 goroutine + 4 个 ticker：写库的总频率是个位数每分钟，
// 拆成多个 goroutine 只会增加调度与竞争，没有任何收益。
func (s *Server) pipelineLoop(ctx context.Context) {
	flush := time.NewTicker(s.cfg.FlushInterval)
	defer flush.Stop()
	rollup := time.NewTicker(rollupEvery)
	defer rollup.Stop()
	runtime := time.NewTicker(runtimeEvery)
	defer runtime.Stop()
	retention := time.NewTicker(retentionEvery)
	defer retention.Stop()

	// 启动时先做一次：先补算 2 小时空洞，再把"刚刚重启"前的内存数据落盘。
	s.rollupSamplesSince(ctx, rollupCatchUp)
	s.flushSamples(ctx)
	s.flushRuntime(ctx)
	s.flushTraffic(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-flush.C:
			s.flushSamples(ctx)
		case <-rollup.C:
			s.rollupSamplesSince(ctx, rollupSteady)
		case <-runtime.C:
			s.flushRuntime(ctx)
			s.flushTraffic(ctx)
		case <-retention.C:
			s.purge(ctx)
		}
	}
}

// flushTraffic 把内存里的流量增量落盘。
//
// 增量与"新基线"在同一个事务里提交（store.FlushTraffic），只有写成功才推进内存基线：
// 失败时 pending 原样保留，下一分钟继续尝试，绝不丢也不重复。
func (s *Server) flushTraffic(ctx context.Context) {
	now := time.Now()
	day := store.FormatDay(now.In(s.loc))
	updates := s.traffic.snapshot(day)
	if len(updates) == 0 {
		return
	}
	if err := s.db.FlushTraffic(ctx, updates, now); err != nil {
		s.log.Error("流量落盘失败（数据保留在内存，下次重试）", "err", err, "nodes", len(updates))
		return
	}
	s.traffic.commit(updates)
	s.trafficCache.invalidate()
	s.log.Debug("流量已落盘", "nodes", len(updates))
}

// flushSamples 把已结束的 10 秒桶写库。
func (s *Server) flushSamples(ctx context.Context) {
	buckets := s.agg.flushClosed(time.Now())
	if len(buckets) == 0 {
		return
	}
	if err := s.db.InsertBuckets(ctx, store.TableSamples10s, buckets); err != nil {
		// 写失败不要把桶丢掉：InsertBuckets 是幂等 upsert，放回队列下次重试。
		// 但服务端正在退出时重试没有意义（下一次 tick 永远不会来）。
		if ctx.Err() != nil {
			s.log.Error("写入 10 秒聚合失败（正在退出，这批数据丢弃）", "err", err, "buckets", len(buckets))
			return
		}
		dropped := s.agg.requeue(buckets)
		s.log.Error("写入 10 秒聚合失败，已放回队列稍后重试",
			"err", err, "buckets", len(buckets), "dropped", dropped)
		return
	}
	s.log.Debug("已写入 10 秒聚合", "buckets", len(buckets))
}

// rollupSamplesSince 把最近 lookback 内的 10 秒桶聚合到 1 分钟桶（幂等，可重复执行）。
func (s *Server) rollupSamplesSince(ctx context.Context, lookback time.Duration) {
	nodes, err := s.db.ListNodes(ctx)
	if err != nil {
		s.log.Error("读取节点列表失败", "err", err)
		return
	}
	ids := make([]int64, 0, len(nodes))
	for _, n := range nodes {
		ids = append(ids, n.ID)
	}
	if len(ids) == 0 {
		return
	}
	now := time.Now()
	if _, err := s.db.RollupSamples(ctx, store.TableSamples10s, store.TableSamples1m, 60, ids, now.Add(-lookback), now); err != nil {
		s.log.Error("聚合到 1 分钟桶失败", "err", err, "lookback", lookback.String())
	}
}

// flushRuntime 写出每个节点的最后状态（服务端重启后界面不至于一片空白）。
func (s *Server) flushRuntime(ctx context.Context) {
	nodes, err := s.db.ListNodes(ctx)
	if err != nil {
		s.log.Error("读取节点列表失败", "err", err)
		return
	}
	// 顺手把"现在有哪些节点"告诉聚合器与流量记账：删除节点后残留的帧会被直接
	// 忽略，不会因为外键约束让整批落盘失败。
	ids := make([]int64, 0, len(nodes))
	for _, n := range nodes {
		ids = append(ids, n.ID)
	}
	s.agg.setKnown(ids)
	s.traffic.setKnown(ids)

	now := time.Now()
	rows := make([]store.RuntimeRow, 0, len(nodes))
	for _, n := range nodes {
		st, ok := s.state.Get(n.ID)
		if !ok || st.LastSeen.IsZero() {
			continue
		}
		diskPct := 0.0
		if len(st.Metrics.Disk) > 0 {
			diskPct = st.Metrics.Disk[0].Pct
		}
		rows = append(rows, store.RuntimeRow{
			NodeID:       n.ID,
			LastSeen:     st.LastSeen.Unix(),
			Status:       string(state.StatusFor(st.LastSeen, now, s.cfg.StaleAfter, s.cfg.OfflineAfter)),
			CPUPct:       st.Metrics.CPUPct,
			MemPct:       st.Metrics.Mem.Pct,
			SwapPct:      st.Metrics.Swap.Pct,
			DiskPct:      diskPct,
			Load1:        st.Metrics.Load.L1,
			LatMS:        st.Metrics.LatMS,
			UptimeSec:    int64(st.Metrics.UptimeSec),
			BootID:       st.Metrics.Net.BootID,
			Iface:        st.Metrics.Net.Iface,
			RxRaw:        st.Metrics.Net.RxRaw,
			TxRaw:        st.Metrics.Net.TxRaw,
			AgentVersion: st.Info.AgentVersion,
			Kernel:       st.Info.OS.Kernel,
			OSName:       st.Info.OS.Name,
			CPUModel:     st.Info.CPU.Model,
			// 在线起点由 tracker 给（它才是这份记账的唯一来源，见 online.go）。
			// 落盘周期是一分钟：崩溃时最多丢掉"这一分钟内新起算的那一段起点"，
			// 而正常重启（Run 的优雅退出里也有一次 flushRuntime）不会丢。
			OnlineSince: s.online.sinceOf(n.ID),
		})
	}
	if len(rows) == 0 {
		return
	}
	if err := s.db.UpsertRuntime(ctx, rows, now); err != nil {
		s.log.Error("写入节点运行态失败", "err", err)
	}
}

// purge 按保留策略清理历史桶与过期会话。
func (s *Server) purge(ctx context.Context) {
	nodes, err := s.db.ListNodes(ctx)
	if err != nil {
		s.log.Error("读取节点列表失败", "err", err)
		return
	}
	ids := make([]int64, 0, len(nodes))
	for _, n := range nodes {
		ids = append(ids, n.ID)
	}

	now := time.Now()
	if s.cfg.Retention10s > 0 {
		if n, err := s.db.DeleteOldSamples(ctx, store.TableSamples10s, now.Add(-s.cfg.Retention10s), ids); err != nil {
			s.log.Error("清理 10 秒桶失败", "err", err)
		} else if n > 0 {
			s.log.Info("已清理过期的 10 秒桶", "rows", n)
		}
	}
	if s.cfg.Retention1m > 0 {
		if n, err := s.db.DeleteOldSamples(ctx, store.TableSamples1m, now.Add(-s.cfg.Retention1m), ids); err != nil {
			s.log.Error("清理 1 分钟桶失败", "err", err)
		} else if n > 0 {
			s.log.Info("已清理过期的 1 分钟桶", "rows", n)
		}
	}
	if n, err := s.db.DeleteExpiredSessions(ctx, now); err != nil {
		s.log.Error("清理过期会话失败", "err", err)
	} else if n > 0 {
		s.log.Info("已清理过期会话", "rows", n)
	}
}

// seedFromRuntime 把上次退出前的最后状态读回内存。
//
// 恢复出来的节点一律是"未连接"：它们的状态由 last_seen 决定，
// 因此重启后界面会如实显示"离线/抖动"，而不是假装在线。
//
// 「连续在线时长」的起点也在这里恢复（node_runtime.online_since，迁移 0005）：
// 重启只花了几秒、那个节点按同一套判定**仍然在线**时，时长接着原来的起点累加，
// 而不是从零开始（见 online.go）。已经掉线的节点不继承起点 —— 停机那段时间
// 不是"在线"。
func (s *Server) seedFromRuntime(ctx context.Context) (int, error) {
	rows, err := s.db.LoadRuntime(ctx)
	if err != nil {
		return 0, err
	}
	now := time.Now()
	for _, r := range rows {
		s.state.Seed(state.Seed{
			NodeID:   r.NodeID,
			LastSeen: time.Unix(r.LastSeen, 0),
			Metrics: protocol.Metrics{
				CPUPct:    r.CPUPct,
				Mem:       protocol.Mem{Pct: r.MemPct},
				Swap:      protocol.Mem{Pct: r.SwapPct},
				Disk:      []protocol.Disk{{Mount: "/", Pct: r.DiskPct}},
				Load:      protocol.Load{L1: r.Load1},
				LatMS:     r.LatMS,
				UptimeSec: uint64(max64(r.UptimeSec, 0)),
				Net: protocol.Net{
					Iface: r.Iface, BootID: r.BootID, RxRaw: r.RxRaw, TxRaw: r.TxRaw,
				},
			},
			Info: protocol.Info{
				AgentVersion: r.AgentVersion,
				OS:           protocol.OSInfo{Name: r.OSName, Kernel: r.Kernel},
				CPU:          protocol.CPUInfo{Model: r.CPUModel},
			},
		})
		s.online.seed(r.NodeID,
			statusOf(time.Unix(r.LastSeen, 0), true, now, s.cfg.StaleAfter, s.cfg.OfflineAfter),
			r.OnlineSince)
	}
	return len(rows), nil
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
