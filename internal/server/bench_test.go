package server

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"probe/internal/config"
	"probe/internal/protocol"
	"probe/internal/store"
)

// 服务端侧的性能基准：回答"50 个节点时，一次列表/详情/曲线查询要多久"。
//
// 与 e2e 的 TestFiftyAgentsLoad 配合看：那边测"能不能跟上"，这里测"单次查询多贵"。

// benchServer 直接构造一个服务端（基准里用不到会话，所以不走 auth harness）。
func benchServer(tb testing.TB) *Server {
	tb.Helper()
	db, err := store.Open(context.Background(), filepath.Join(tb.TempDir(), "probe.db"))
	if err != nil {
		tb.Fatalf("打开数据库: %v", err)
	}
	tb.Cleanup(func() { _ = db.Close() })
	return New(config.Default(), db, slog.New(slog.DiscardHandler), time.UTC)
}

func seedNodes(tb testing.TB, s *Server, count int) []int64 {
	tb.Helper()
	ids := make([]int64, 0, count)
	for i := 0; i < count; i++ {
		node, _, err := s.db.CreateNode(context.Background(), store.NewNode{
			Name: fmt.Sprintf("bench-%03d", i), GroupName: "基准", Region: "HK",
			IntervalSec: 1, TrafficWarnPct: 80, ResetDay: 19, TrafficLimit: 1 << 40,
		}, time.Now())
		if err != nil {
			tb.Fatalf("创建节点: %v", err)
		}
		ids = append(ids, node.ID)
	}
	return ids
}

// seedHistory 造一段真实形状的历史：每个节点最近 1 小时、每 10 秒一个桶。
func seedHistory(tb testing.TB, s *Server, ids []int64) {
	tb.Helper()
	ctx := context.Background()
	now := time.Now().Unix()
	base := now - now%10
	for _, id := range ids {
		buckets := make([]store.SampleBucket, 0, 360)
		for i := int64(360); i > 0; i-- {
			buckets = append(buckets, store.SampleBucket{
				NodeID: id, TS: base - i*10, CPUAvg: 12.5, CPUMax: 30, MemAvg: 50, MemMax: 60,
				DiskAvg: 40, DiskMax: 41, RxRate: 1024, TxRate: 512, LatAvg: 20, LatMin: 18, LatMax: 40,
				Up: 10, All: 10,
			})
		}
		if err := s.db.InsertBuckets(ctx, store.TableSamples10s, buckets); err != nil {
			tb.Fatalf("写入历史: %v", err)
		}
	}
}

func seedLiveState(s *Server, ids []int64) {
	for _, id := range ids {
		s.state.Update(id, 1, protocol.Metrics{
			CPUPct: 12.5,
			Mem:    protocol.Mem{Total: 1 << 30, Used: 1 << 29, Pct: 50},
			Disk:   []protocol.Disk{{Mount: "/", FS: "ext4", Total: 1 << 40, Used: 1 << 39, Pct: 50}},
			Net:    protocol.Net{Iface: "eth0", RxRate: 1024, TxRate: 512},
			LatMS:  20,
		}, 0, time.Now())
	}
}

func BenchmarkCurrentNodes50(b *testing.B) {
	s := benchServer(b)
	ids := seedNodes(b, s, 50)
	seedLiveState(s, ids)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.currentNodes(context.Background()); err != nil {
			b.Fatalf("currentNodes: %v", err)
		}
	}
}

func BenchmarkSeriesQuery1h(b *testing.B) {
	s := benchServer(b)
	ids := seedNodes(b, s, 1)
	seedHistory(b, s, ids)
	rg, _ := store.RangeByKey("1h")

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.db.QuerySeries(context.Background(), ids[0], "cpu", rg, time.Now()); err != nil {
			b.Fatalf("QuerySeries: %v", err)
		}
	}
}

func BenchmarkTrafficAggregates50(b *testing.B) {
	s := benchServer(b)
	ids := seedNodes(b, s, 50)
	ctx := context.Background()

	// 每个节点 30 天的日流量。
	updates := make([]store.TrafficUpdate, 0, 50*30)
	for _, id := range ids {
		for day := 0; day < 30; day++ {
			updates = append(updates, store.TrafficUpdate{
				NodeID:  id,
				Day:     store.FormatDay(time.Now().AddDate(0, 0, -day)),
				RxDelta: 1 << 30, TxDelta: 1 << 29,
				RxTotal: uint64(day+1) << 30, TxTotal: uint64(day+1) << 29,
			})
		}
	}
	if err := s.db.FlushTraffic(ctx, updates, time.Now()); err != nil {
		b.Fatalf("写入流量: %v", err)
	}

	nodes, err := s.db.ListNodes(ctx)
	if err != nil {
		b.Fatalf("读取节点: %v", err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.trafficAggregates(ctx, nodes, time.Now()); err != nil {
			b.Fatalf("trafficAggregates: %v", err)
		}
	}
}

func BenchmarkEncodePayload50(b *testing.B) {
	s := benchServer(b)
	ids := seedNodes(b, s, 50)
	seedLiveState(s, ids)

	nodes, err := s.currentNodes(context.Background())
	if err != nil {
		b.Fatalf("currentNodes: %v", err)
	}
	payload, err := encodePayload(nodes, summarize(nodes))
	if err != nil {
		b.Fatalf("encodePayload: %v", err)
	}
	b.Logf("50 节点变更集大小：%d 字节（约 %.1f KB）", len(payload), float64(len(payload))/1024)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := encodePayload(nodes, summarize(nodes)); err != nil {
			b.Fatalf("encodePayload: %v", err)
		}
	}
}
