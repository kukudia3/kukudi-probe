package state

import (
	"sync"
	"testing"
	"time"

	"probe/internal/protocol"
)

func TestStatusFor(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	stale := 10 * time.Second
	offline := 30 * time.Second

	cases := []struct {
		name     string
		lastSeen time.Time
		want     Status
	}{
		{"从未通信", time.Time{}, StatusUnknown},
		{"刚刚通信", now.Add(-time.Second), StatusOnline},
		{"边界：恰好等于抖动阈值", now.Add(-stale), StatusStale},
		{"抖动区间", now.Add(-20 * time.Second), StatusStale},
		{"边界：恰好等于离线阈值", now.Add(-offline), StatusOffline},
		{"长时间静默", now.Add(-time.Hour), StatusOffline},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := StatusFor(tc.lastSeen, now, stale, offline); got != tc.want {
				t.Fatalf("StatusFor = %q，期望 %q", got, tc.want)
			}
		})
	}
}

func TestUpdateAndGet(t *testing.T) {
	s := New()
	now := time.Unix(1_700_000_000, 0)

	if _, ok := s.Get(7); ok {
		t.Fatal("还没有数据时不应命中")
	}

	metrics := protocol.Metrics{CPUPct: 42, Mem: protocol.Mem{Total: 100, Used: 50, Pct: 50}}
	seq := s.Update(7, 1, metrics, 0, now)
	if seq != 1 {
		t.Fatalf("首次更新的 seq = %d，期望 1", seq)
	}
	seq = s.Update(7, 1, metrics, 0, now.Add(time.Second))
	if seq != 2 {
		t.Fatalf("第二次更新的 seq = %d，期望 2", seq)
	}

	got, ok := s.Get(7)
	if !ok {
		t.Fatal("应当命中节点 7")
	}
	if got.Seq != 2 || got.Metrics.CPUPct != 42 || !got.Connected {
		t.Fatalf("节点状态不对: %+v", got)
	}
	if !got.LastSeen.Equal(now.Add(time.Second)) {
		t.Fatalf("LastSeen = %v", got.LastSeen)
	}
	if s.Len() != 1 {
		t.Fatalf("Len = %d", s.Len())
	}
}

func TestAttachDetachOnlyAffectsCurrentConn(t *testing.T) {
	s := New()
	now := time.Unix(1_700_000_000, 0)

	s.Attach(3, 100, protocol.Info{Hostname: "hk-01"}, "203.0.113.7", "10.0.0.5", "2001:db8::5", now)
	if got, _ := s.Get(3); !got.Connected || got.ObservedIP != "203.0.113.7" || got.Info.Hostname != "hk-01" {
		t.Fatalf("Attach 未生效: %+v", got)
	}
	if got, _ := s.Get(3); got.LocalIP != "10.0.0.5" || got.LocalIP6 != "2001:db8::5" {
		t.Fatalf("Attach 未写入本机地址: %+v", got)
	}

	// 旧连接的 Detach 不能把新连接标记为断开。
	s.Attach(3, 200, protocol.Info{Hostname: "hk-01"}, "203.0.113.7", "", "", now.Add(time.Second))
	s.Detach(3, 100)
	if got, _ := s.Get(3); !got.Connected || got.ConnID != 200 {
		t.Fatalf("旧连接不应影响新连接: %+v", got)
	}

	s.Detach(3, 200)
	if got, _ := s.Get(3); got.Connected {
		t.Fatalf("当前连接应当被标记为断开: %+v", got)
	}

	// 断线不清空最后通信时间——离线判定依赖它。
	got, _ := s.Get(3)
	if got.LastSeen.IsZero() {
		t.Fatal("Detach 不应清空 LastSeen")
	}

	// 不存在的节点：不应 panic。
	s.Detach(999, 1)
}

func TestSnapshotAndDelete(t *testing.T) {
	s := New()
	now := time.Unix(1_700_000_000, 0)
	for id := int64(1); id <= 3; id++ {
		s.Attach(id, uint64(id), protocol.Info{}, "", "", "", now)
		s.Update(id, uint64(id), protocol.Metrics{CPUPct: float64(id)}, 0, now)
	}
	if len(s.Snapshot()) != 3 {
		t.Fatalf("快照数量 = %d", len(s.Snapshot()))
	}
	s.Delete(2)
	if len(s.Snapshot()) != 2 {
		t.Fatalf("删除后快照数量 = %d", len(s.Snapshot()))
	}
	if _, ok := s.Get(2); ok {
		t.Fatal("删除后不应命中")
	}
}

func TestGapIsReported(t *testing.T) {
	s := New()
	now := time.Unix(1_700_000_000, 0)
	s.Update(1, 1, protocol.Metrics{CPUPct: 1}, 5, now)
	got, _ := s.Get(1)
	if got.Gap != 5 {
		t.Fatalf("Gap = %d，期望 5", got.Gap)
	}
}

// 并发读写：配合 -race 使用（本机没有 64 位 gcc，这条用例在 Linux 上更有价值）。
func TestConcurrentAccess(t *testing.T) {
	s := New()
	now := time.Unix(1_700_000_000, 0)
	var wg sync.WaitGroup

	for writer := 0; writer < 4; writer++ {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				s.Attach(id, uint64(id), protocol.Info{}, "", "", "", now)
				s.Update(id, uint64(id), protocol.Metrics{CPUPct: float64(i)}, 0, now)
			}
		}(int64(writer + 1))
	}
	for reader := 0; reader < 4; reader++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				_ = s.Snapshot()
				_, _ = s.Get(1)
			}
		}()
	}
	wg.Wait()

	if s.Len() != 4 {
		t.Fatalf("节点数 = %d，期望 4", s.Len())
	}
}
