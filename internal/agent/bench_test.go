package agent

import (
	"runtime"
	"testing"
	"time"
)

// memTraffic 造一个"不落盘"的流量状态（path 为空 = 纯内存）。
func memTraffic(fatal func(string, ...any)) *Traffic {
	tr, warn, err := LoadTraffic("")
	if err != nil || warn != "" {
		fatal("LoadTraffic(\"\") = %v, %q", err, warn)
	}
	return tr
}

// 这些基准用来回答"Agent 每秒一次采样到底花多少 CPU / 分配多少内存"。
//
// 采样全是读 /proc 快照 + 整数运算，没有 goroutine、没有缓存增长，
// 所以稳态内存 = 固定开销 + 峰值临时分配（见 TestSampleSteadyStateMemory）。
func BenchmarkCollectorSample(b *testing.B) {
	c := New("testdata/root", "eth0", "/", memTraffic(b.Fatalf))

	now := time.Unix(1_800_000_000, 0)
	// 先跑一次建立基线（第一次采样不算速率）。
	if _, _, err := c.Sample(now); err != nil {
		b.Fatalf("首次采样: %v", err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := c.Sample(now.Add(time.Duration(i) * time.Second)); err != nil {
			b.Fatalf("采样失败: %v", err)
		}
	}
}

func BenchmarkCollectorInfo(b *testing.B) {
	c := New("testdata/root", "eth0", "/", memTraffic(b.Fatalf))

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.Info(); err != nil {
			b.Fatalf("Info 失败: %v", err)
		}
	}
}

// TestSampleSteadyStateMemory 验证"采样不会让内存增长"：
// 跑 10000 次之后堆占用不应当比开始时可观测地更大。
//
// 这条用例替代了"在 Linux 上盯着 RSS 看半小时"——它证明的是同一件事里
// 最容易被忽略的部分：没有累积状态。
func TestSampleSteadyStateMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("短模式跳过")
	}
	c := New("testdata/root", "eth0", "/", memTraffic(t.Fatalf))

	now := time.Unix(1_800_000_000, 0)
	if _, _, err := c.Sample(now); err != nil {
		t.Fatalf("首次采样: %v", err)
	}
	// 预热：让一次性分配（缓冲、字符串表）先发生。
	for i := 0; i < 200; i++ {
		if _, _, err := c.Sample(now.Add(time.Duration(i) * time.Second)); err != nil {
			t.Fatalf("预热采样: %v", err)
		}
	}
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	const rounds = 10000
	for i := 0; i < rounds; i++ {
		if _, _, err := c.Sample(now.Add(time.Duration(i+200) * time.Second)); err != nil {
			t.Fatalf("采样失败: %v", err)
		}
	}
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	const limit = 2 << 20 // 2 MiB 余量：足以吸收 map/slice 的容量抖动
	growth := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	if growth > limit {
		t.Fatalf("%d 次采样后堆增长 %d 字节，超过 %d 字节的余量（说明存在累积状态）",
			rounds, growth, limit)
	}
	t.Logf("%d 次采样后堆变化 %+d 字节（HeapAlloc %.2f MiB → %.2f MiB）",
		rounds, growth, float64(before.HeapAlloc)/(1<<20), float64(after.HeapAlloc)/(1<<20))
}
